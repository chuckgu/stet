package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/gate"
)

// startOut is start with decision lines written to out, and the gate run
// opened, once it has.
func startOut(t *testing.T, cfg *config.Config, out io.Writer) (*running, func() *gate.Gate) {
	t.Helper()
	got := make(chan *gate.Gate, 1)
	onGate = func(g *gate.Gate) { got <- g }
	t.Cleanup(func() { onGate = nil }) // runs after the stop registered below
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{addr: ln.Addr().String(), log: &syncBuffer{}, cancel: cancel, done: make(chan struct{})}
	log := slog.New(slog.NewTextHandler(r.log, nil))
	go func() {
		r.err = runWith(ctx, cfg, ln, log, out)
		close(r.done)
	}()
	t.Cleanup(func() { r.stop(t) })
	return r, func() *gate.Gate {
		t.Helper()
		select {
		case g := <-got:
			got <- g
			return g
		case <-time.After(5 * time.Second):
			t.Fatalf("run opened no gate; log:\n%s", r.log)
			return nil
		}
	}
}

// waitLines waits until out holds n lines and returns them.
func waitLines(t *testing.T, out *syncBuffer, n int, log fmt.Stringer) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := strings.TrimSuffix(out.String(), "\n")
		var ls []string
		if s != "" {
			ls = strings.Split(s, "\n")
		}
		if len(ls) >= n {
			return ls
		}
		if time.Now().After(deadline) {
			t.Fatalf("out holds %d lines, want %d:\n%s\nlog:\n%s", len(ls), n, s, log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDecisionsFlag(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	for _, c := range []struct {
		flag string
		out  *os.File
		want bool
		err  bool
	}{
		{"on", file, true, false},
		{"off", file, false, false},
		{"auto", file, false, false},
		{"auto", pw, false, false},
		{"x", file, false, true},
		{"", file, false, true},
	} {
		got, err := decisionsOn(c.flag, c.out)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("decisionsOn(%q) = %v, %v; want %v, error %v", c.flag, got, err, c.want, c.err)
		}
	}
	if _, err := decisionsOn("x", file); err == nil || err.Error() != `-decisions: "x" must be auto, on or off` {
		t.Errorf("error = %v", err)
	}
	if dev, err := os.Open(os.DevNull); err == nil { // a character device, where there is one
		defer dev.Close()
		if got, err := decisionsOn("auto", dev); err != nil || !got {
			t.Errorf("decisionsOn(auto, /dev/null) = %v, %v; want true", got, err)
		}
	}
}

const contactsYAML = `listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/%[2]s?sslmode=disable
model: {adapter: none, on_failure: pass}
tables:
  contacts:
    pk: id
    send_fields: [name]
    reads: {}
    stale: {}
    impact: {max_rows: 100}
`

func TestDecisionLinePerJudgedWrite(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db := cleanDatabase(t, pg)
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
	mustExec(t, admin, "create table contacts (id int primary key, name text, status text)")
	mustExec(t, admin, "create table deals (id serial primary key, contact_id int references contacts on delete cascade)")
	mustExec(t, admin, "insert into contacts select g, 'n' || g, 'new' from generate_series(1, 1000) g")
	mustExec(t, admin, "insert into deals (contact_id) select g from generate_series(1, 1000) g")
	mustExec(t, admin, "analyze contacts")

	out := &syncBuffer{}
	r, gateOf := startOut(t, parseConfig(t, contactsYAML, pg, db), out)
	agent := connectTo(t, r.addr, db, "stet_scram", "scram-pw")

	readRow(t, agent, gateOf(), "contacts", 1000)
	// A write is judged after it ran, so lines come in the order judging ends:
	// wait for each write's line (after the banner) before sending the next.
	for i, sql := range []string{
		"update contacts set name = 'one' where id = 1000",
		"update contacts set status = 'seen' where id <= 300",
		"delete from contacts where id between 501 and 700",
	} {
		if _, err := agent.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v; log:\n%s", sql, err, r.log)
		}
		waitLines(t, out, i+2, r.log)
	}

	got := waitLines(t, out, 4, r.log)
	time.Sleep(200 * time.Millisecond)
	if all := waitLines(t, out, 4, r.log); len(all) != 4 {
		t.Fatalf("%d lines, want 4:\n%s", len(all), strings.Join(all, "\n"))
	}
	if want := "stet: watching 1 table on " + r.addr + " (shadow: nothing is blocked)"; got[0] != want {
		t.Errorf("first line %q, want %q", got[0], want)
	}
	for i, re := range []string{
		`^\d\d:\d\d:\d\d  ok            UPDATE contacts  1 row, fresh read  `,
		`^\d\d:\d\d:\d\d  would hold    UPDATE contacts  300 rows, ~30% of table  \[impact:rows\]  `,
		`^\d\d:\d\d:\d\d  would hold    DELETE contacts  200 rows, ~20% of table, cascades into deals  \[impact:rows\]  `,
	} {
		if !regexp.MustCompile(re).MatchString(got[i+1]) {
			t.Errorf("line %d = %q, want it to match %s", i+1, got[i+1], re)
		}
	}
}

func TestRunWithoutOutWritesNothing(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db := cleanDatabase(t, pg)
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
	mustExec(t, admin, "create table contacts (id int primary key, name text, status text)")
	mustExec(t, admin, "insert into contacts values (1, 'a', 'new')")

	stdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = pw
	defer func() { os.Stdout = stdout }()

	r := start(t, parseConfig(t, contactsYAML, pg, db))
	agent := connectTo(t, r.addr, db, "stet_scram", "scram-pw")
	if _, err := agent.Exec(ctx, "update contacts set name = 'b' where id = 1"); err != nil {
		t.Fatal(err)
	}
	waitDecisions(t, admin, "contacts", 1)
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	os.Stdout = stdout
	pw.Close()
	if b, _ := io.ReadAll(pr); len(b) != 0 {
		t.Errorf("run wrote %q to standard output, want nothing", b)
	}
}

// waitDecisions waits until n decisions are recorded for table.
func waitDecisions(t *testing.T, conn *pgx.Conn, table string, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var got int
		if err := conn.QueryRow(context.Background(),
			"select count(*) from stet.decisions where target_table = $1", table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d decisions recorded for %s, want %d", got, table, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestEnforceLineShowsTheRefusal(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db, table, _ := agentTable(t, pg, 5)

	out := &syncBuffer{}
	r, _ := startOut(t, parseConfig(t, enforceYAML+"    impact: {max_rows: 3}\n", pg, db, table), out)
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")
	if _, err := agent.Exec(ctx, "update "+table+" set name = 'all'"); sqlstate(err) != "ST001" {
		t.Fatalf("update of every row: %v, want ST001; log:\n%s", err, r.log)
	}
	got := waitLines(t, out, 2, r.log)
	if !strings.HasSuffix(got[0], " (enforce)") {
		t.Errorf("first line %q, want it to end with (enforce)", got[0])
	}
	re := regexp.MustCompile(`^\d\d:\d\d:\d\d  hold          UPDATE ` + table + `  .*refused ST001`)
	if !re.MatchString(got[1]) {
		t.Errorf("line = %q, want a hold with refused ST001", got[1])
	}
}

func TestLineWhenTheRecordFails(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db := cleanDatabase(t, pg)
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
	mustExec(t, admin, "create table contacts (id int primary key, name text, status text)")
	mustExec(t, admin, "insert into contacts values (1, 'a', 'new')")

	out := &syncBuffer{}
	r, _ := startOut(t, parseConfig(t, contactsYAML, pg, db), out)
	waitListening(t, r)
	// Only inside the test's own database, which is dropped after it.
	mustExec(t, connectTo(t, pg, db, "postgres", "postgres-pw"), "alter table stet.decisions rename to decisions_gone")

	agent := connectTo(t, r.addr, db, "stet_scram", "scram-pw")
	if _, err := agent.Exec(ctx, "update contacts set name = 'b' where id = 1"); err != nil {
		t.Fatalf("the write failed: %v; log:\n%s", err, r.log)
	}
	got := waitLines(t, out, 2, r.log)
	if !strings.Contains(got[1], "UPDATE contacts") {
		t.Errorf("line = %q, want one for the update", got[1])
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(r.log.String(), "decision not recorded") {
		if time.Now().After(deadline) {
			t.Fatalf("no 'decision not recorded' in the log:\n%s", r.log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// buildStet builds the stet command into the test's temporary directory and
// returns its path.
func buildStet(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "stet")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// A stet proxy whose standard output or standard error is a pipe that no one
// reads any more goes on serving (spec-v0.2-tryout 6): what it writes there
// fails and is dropped. The Go runtime ends a process whose write to fd 1 or
// 2 meets a closed pipe with SIGPIPE, unless the signal is ignored; an
// os.Pipe inside the test's own process cannot show that, so it runs the
// command.
func TestClosedOutputDoesNotEndTheProxy(t *testing.T) {
	pg := testPG(t)
	bin := buildStet(t)
	for _, closed := range []string{"stdout", "stderr"} {
		t.Run(closed, func(t *testing.T) {
			ctx := context.Background()
			db := cleanDatabase(t, pg)
			admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
			mustExec(t, admin, "create table contacts (id int primary key, name text, status text)")
			mustExec(t, admin, "insert into contacts select g, 'n' || g, 'new' from generate_series(1, 10) g")
			path := filepath.Join(t.TempDir(), "stet.yaml")
			if err := os.WriteFile(path, fmt.Appendf(nil, contactsYAML, pg, db), 0o600); err != nil {
				t.Fatal(err)
			}

			// -v logs each session on standard error, so both outputs are
			// written to after the one under test is closed.
			cmd := exec.Command(bin, "proxy", "-config", path, "-decisions", "on", "-v")
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "PG") {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			outR, outW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			errR, errW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stdout, cmd.Stderr = outW, errW
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			outW.Close() // the command holds the only write ends now
			errW.Close()
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			stopped := false
			t.Cleanup(func() { // before the database is dropped
				if !stopped {
					cmd.Process.Kill()
					<-exited
				}
			})

			// Each pipe is read into a buffer until the test closes its read end.
			stdout, stderr := &syncBuffer{}, &syncBuffer{}
			for r, buf := range map[*os.File]*syncBuffer{outR: stdout, errR: stderr} {
				go io.Copy(buf, r)
				t.Cleanup(func() { r.Close() })
			}
			show := func() string { return fmt.Sprintf("stdout:\n%s\nstderr:\n%s", stdout, stderr) }
			// running fails the test if the command has ended, waiting up to
			// wait for it to end.
			running := func(when string, wait time.Duration) {
				t.Helper()
				select {
				case err := <-exited:
					stopped = true
					t.Fatalf("stet proxy ended %s, its %s closed: %v; %s", when, closed, err, show())
				case <-time.After(wait):
				}
			}
			waitFor := func(buf *syncBuffer, s string) {
				t.Helper()
				deadline := time.Now().Add(15 * time.Second)
				for !strings.Contains(buf.String(), s) {
					running("before it started", 0)
					if time.Now().After(deadline) {
						t.Fatalf("no %q; %s", s, show())
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			waitFor(stdout, "stet: watching 1 table on ")
			waitFor(stderr, "stet proxy listening")
			m := regexp.MustCompile(`stet: watching 1 table on (\S+) `).FindStringSubmatch(stdout.String())
			if m == nil {
				t.Fatalf("no address in the start line; %s", show())
			}
			addr := m[1]
			if closed == "stdout" {
				outR.Close()
			} else {
				errR.Close()
			}

			session := func() *pgx.Conn {
				t.Helper()
				conn, err := pgx.Connect(ctx, "postgres://stet_scram:scram-pw@"+addr+"/"+db+"?sslmode=disable")
				if err != nil {
					running("as a session started", time.Second)
					t.Fatalf("connect through stet proxy: %v; %s", err, show())
				}
				t.Cleanup(func() { conn.Close(ctx) })
				return conn
			}
			agent := session()
			for i := 1; i <= 3; i++ {
				if _, err := agent.Exec(ctx, fmt.Sprintf("update contacts set name = 'w%d' where id = %d", i, i)); err != nil {
					running("during the writes", time.Second)
					t.Fatalf("write %d: %v; %s", i, err, show())
				}
			}
			waitDecisions(t, admin, "contacts", 3)
			// The lines and the session's log are written once the decisions
			// are recorded: give them the time to meet the closed pipe.
			running("after the writes", 500*time.Millisecond)

			later := session()
			if _, err := later.Exec(ctx, "update contacts set name = 'later' where id = 4"); err != nil {
				running("in a later session", time.Second)
				t.Fatalf("a later session's write: %v; %s", err, show())
			}
			waitDecisions(t, admin, "contacts", 4)
			running("after the later session", 200*time.Millisecond)

			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-exited:
				stopped = true
				if err != nil {
					t.Errorf("stet proxy ended with %v after SIGTERM, want exit status 0; %s", err, show())
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("stet proxy did not end after SIGTERM; %s", show())
			}
		})
	}
}

// stuckWriter never takes a byte until released.
type stuckWriter struct{ release chan struct{} }

func (w *stuckWriter) Write(p []byte) (int, error) { <-w.release; return len(p), nil }

func TestBlockedOutputDoesNotHoldWrites(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db := cleanDatabase(t, pg)
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
	mustExec(t, admin, "create table contacts (id int primary key, name text, status text)")

	out := &stuckWriter{release: make(chan struct{})}
	t.Cleanup(func() { close(out.release) })
	r, _ := startOut(t, parseConfig(t, contactsYAML, pg, db), out)
	agent := connectTo(t, r.addr, db, "stet_scram", "scram-pw")
	for i := 1; i <= 2000; i++ {
		if _, err := agent.Exec(ctx, fmt.Sprintf("insert into contacts values (%d, 'n', 'new')", i)); err != nil {
			t.Fatalf("write %d: %v; log:\n%s", i, err, r.log)
		}
	}
	waitDecisions(t, admin, "contacts", 2000)

	began := time.Now()
	r.cancel()
	select {
	case <-r.done:
		if d := time.Since(began); d > 10*time.Second {
			t.Errorf("runWith took %v to return, want under 10s", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runWith did not return within 10s of its context being cancelled")
	}
}

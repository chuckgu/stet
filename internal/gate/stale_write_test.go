package gate_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/proxy"
)

// staleYAML is a shadow config whose table checks its writes against what
// was read (spec-v0.2-stale). It asks the model nothing.
const staleYAML = `
listen: 127.0.0.1:0
upstream: postgres://stet_scram@%[1]s/stet_test
model: {adapter: fake, version: fake-1, timeout_ms: 2000}
tables:
  %[2]s:
    pk: id
    send_fields: [name, title]
    stale: {}
`

// staleConfig is staleYAML in mode, with more tables after the env's own
// (blocks of tableBlock).
func staleConfig(mode string, more ...string) string {
	format := staleYAML
	for _, b := range more {
		format += b
	}
	if mode == "enforce" {
		format = enforcePrefix + format
	}
	return format
}

// setupStale is setup for a config whose table has stale, in either mode,
// with stet_md5, the agents' role, allowed to write the table.
func setupStale(t *testing.T, d decide.Decider, format string) *env {
	t.Helper()
	e := setupEnforce(t, d, format)
	e.seed(t)
	return e
}

// otherTable makes a table besides the env's, named base made unique, with
// columns, that stet_md5 may write, and returns its name. It has to exist
// before the gate starts, which reads the catalog then. It is dropped when
// the test ends.
func otherTable(t *testing.T, base, columns string) string {
	t.Helper()
	ctx := context.Background()
	if os.Getenv("STET_TEST_PG") == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	conn := direct(t)
	name := fmt.Sprintf("%s_%d", base, time.Now().UnixNano())
	mustExec(t, conn, "create table "+name+" "+columns)
	mustExec(t, conn, "grant all on "+name+" to stet_md5")
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, "postgres://stet_scram:scram-pw@"+os.Getenv("STET_TEST_PG")+"/stet_test?sslmode=disable")
		if err == nil {
			c.Exec(ctx, "drop table if exists "+name)
			c.Close(ctx)
		}
	})
	return name
}

// direct is a connection straight to the test database as stet_scram,
// closed when the test ends.
func direct(t *testing.T) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, "postgres://stet_scram:scram-pw@"+os.Getenv("STET_TEST_PG")+"/stet_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	return conn
}

// read reads the row of table whose id is id through conn, and waits until
// its version is looked up.
func (e *env) read(t *testing.T, conn *pgx.Conn, table string, id any) {
	t.Helper()
	e.readOnly(t, conn, table, id)
	e.idle(t)
}

// readOnly reads the row of table whose id is id through conn, and does not
// wait for its lookup.
func (e *env) readOnly(t *testing.T, conn *pgx.Conn, table string, id any) {
	t.Helper()
	e.readBy(t, conn, table, "id", id)
}

// readBy reads the row of table whose column col is key through conn, and
// does not wait for its lookup.
func (e *env) readBy(t *testing.T, conn *pgx.Conn, table, col string, key any) {
	t.Helper()
	var got any
	if err := conn.QueryRow(context.Background(), "select "+col+" from "+table+" where "+col+" = $1", key).Scan(&got); err != nil {
		t.Fatalf("read of %s %v: %v", table, key, err)
	}
}

// xminWhere is the version now of the row of table whose column col is key.
func (e *env) xminWhere(t *testing.T, table, col, key string) string {
	t.Helper()
	var v string
	if err := e.admin.QueryRow(context.Background(), "select xmin::text from "+table+" where "+col+" = $1", key).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// idle waits until the gate has no lookup or refresh of row versions queued
// or running.
func (e *env) idle(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.gate.StaleIdle(ctx); err != nil {
		t.Fatalf("lookups still running: %v", err)
	}
}

// xminOf is the version of the env's row id now.
func (e *env) xminOf(t *testing.T, id int) string {
	t.Helper()
	var v string
	if err := e.admin.QueryRow(context.Background(), "select xmin::text from "+e.table+" where id = $1", id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// staleRow is what a decision records of a checked write.
type staleRow struct {
	ID        string
	Verdict   string
	Reasons   []string
	Executed  bool
	Refused   *string
	ReadCheck map[string]any // nil when the column is NULL
}

// checks waits for n decisions on table, oldest first.
func (e *env) checks(t *testing.T, table string, n int) []staleRow {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := e.admin.Query(context.Background(), `
			select id::text, verdict, reasons, executed, refused, read_check
			from stet.decisions where target_table = $1 order by created_at`, table)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[staleRow])
		if err != nil {
			t.Fatal(err)
		}
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d decisions recorded on %s, want %d", len(got), table, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// checkOf waits for the decision on table whose statement holds marker, and
// returns the latest such.
func (e *env) checkOf(t *testing.T, table, marker string) staleRow {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := e.admin.Query(context.Background(), `
			select id::text, verdict, reasons, executed, refused, read_check
			from stet.decisions where target_table = $1 and strpos(statement, $2) > 0
			order by created_at desc limit 1`, table, marker)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[staleRow])
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 1 {
			return got[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no decision on %s for a statement with %q", table, marker)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// wantRead checks a decision's read_check result.
func wantRead(t *testing.T, i int, r staleRow, result string) {
	t.Helper()
	if got := r.ReadCheck["result"]; got != result {
		t.Errorf("decision %d: read_check %v, want result %s (verdict %s %v)", i, r.ReadCheck, result, r.Verdict, r.Reasons)
	}
}

// wantDecided checks a decision's verdict and reasons.
func wantDecided(t *testing.T, i int, r staleRow, verdict string, reasons ...string) {
	t.Helper()
	if reasons == nil {
		reasons = []string{}
	}
	if r.Verdict != verdict || !reflect.DeepEqual(r.Reasons, reasons) {
		t.Errorf("decision %d: verdict %s %v, want %s %v (read_check %v)", i, r.Verdict, r.Reasons, verdict, reasons, r.ReadCheck)
	}
}

// waitLocked waits until n sessions wait for a lock on a statement that
// holds the text pattern (a table's name, say).
func waitLocked(t *testing.T, su *pgx.Conn, pattern string, n int) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("%d statements wait for a lock", n), func() bool {
		var got int
		err := su.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and query like '%' || $1 || '%'`,
			pattern).Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		return got >= n
	})
}

// lookupHold holds back the gate's lookups of row versions, a read's or a
// refresh's, while it is held. Make it before anything reaches the gate.
type lookupHold struct {
	mu sync.Mutex
	ch chan struct{} // closed when released; nil while not held
}

func holdLookups(t *testing.T, g *gate.Gate) *lookupHold {
	h := &lookupHold{}
	gate.SetLookupTimeout(g, time.Minute)
	gate.OnLookup(g, func(ctx context.Context, table string, keys []string) error {
		h.mu.Lock()
		ch := h.ch
		h.mu.Unlock()
		if ch == nil {
			return nil
		}
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	t.Cleanup(h.release)
	return h
}

func (h *lookupHold) hold() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ch == nil {
		h.ch = make(chan struct{})
	}
}

func (h *lookupHold) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ch != nil {
		close(h.ch)
		h.ch = nil
	}
}

// A row changed by another party after its role read it: shadow records the
// write as stale and sends it; enforce refuses it with ST003 (spec 6).
func TestStaleWrite(t *testing.T) {
	for _, mode := range []string{"shadow", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			e := setupStale(t, calm(), staleConfig(mode))
			agent := e.agent(t, "a")
			e.read(t, agent, e.table, 1)
			readVersion := e.xminOf(t, 1)
			mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")
			now := e.xminOf(t, 1)
			_, err := agent.Exec(context.Background(), "update "+e.table+" set title = 'x' where id = 1")

			r := e.checks(t, e.table, 1)[0]
			wantDecided(t, 0, r, "reject", "stale_read")
			wantRead(t, 0, r, "stale")
			if r.ReadCheck["read_version"] != readVersion || r.ReadCheck["version"] != now ||
				r.ReadCheck["read_on_behalf_of"] != nil {
				t.Errorf("read_check %v, want read_version %s, version %s, no read_on_behalf_of", r.ReadCheck, readVersion, now)
			}
			if age, ok := r.ReadCheck["read_age_ms"].(float64); !ok || age < 0 {
				t.Errorf("read_check.read_age_ms = %v, want a number of milliseconds", r.ReadCheck["read_age_ms"])
			}
			ran := e.rowsWhere(t, "id = 1 and title = 'x'") == 1
			if mode == "shadow" {
				if err != nil || !ran || !r.Executed || r.Refused != nil {
					t.Errorf("shadow: err %v, ran %v, executed %v, refused %v; want the write sent", err, ran, r.Executed, deref(r.Refused))
				}
				return
			}
			pgErr := wantRefused(t, err, "ST003", "write refused by stet: the row was changed after it was read")
			if want := "decision=" + r.ID + " reasons=stale_read"; pgErr.Detail != want {
				t.Errorf("DETAIL %q, want %q", pgErr.Detail, want)
			}
			if want := "read the row again, then send the write again if it still applies"; pgErr.Hint != want {
				t.Errorf("HINT %q, want %q", pgErr.Hint, want)
			}
			if ran || r.Executed || r.Refused == nil || *r.Refused != "ST003" {
				t.Errorf("enforce: ran %v, executed %v, refused %v; want refused with ST003", ran, r.Executed, deref(r.Refused))
			}
		})
	}
}

// The role is the reader, not the connection: a read on one connection and a
// write of the same login role on another are compared (spec 2, P3).
func TestStaleAcrossConnections(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	e.read(t, e.agent(t, "a"), e.table, 1)
	mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")
	mustExec(t, e.agent(t, "b"), "update "+e.table+" set title = 'x' where id = 1")
	wantRead(t, 0, e.checks(t, e.table, 1)[0], "stale")
}

// A row nobody changed since it was read is fresh, and decided as before;
// another row's change does not count (N1, N3).
func TestFreshWrite(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	agent := e.agent(t, "a")
	e.read(t, agent, e.table, 1)
	mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 2")
	mustExec(t, agent, "update "+e.table+" set title = 'x' where id = 1")
	r := e.checks(t, e.table, 1)[0]
	wantRead(t, 0, r, "fresh")
	wantDecided(t, 0, r, "commit")
	if r.ReadCheck["read_version"] != r.ReadCheck["version"] {
		t.Errorf("read_check %v, want the version read", r.ReadCheck)
	}
}

// A write of a row its role never read is no_read, and decided as before (N4).
func TestWriteWithoutRead(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	mustExec(t, e.agent(t, "a"), "update "+e.table+" set title = 'x' where id = 1")
	r := e.checks(t, e.table, 1)[0]
	wantRead(t, 0, r, "no_read")
	wantDecided(t, 0, r, "commit")
	if r.ReadCheck["version"] == nil {
		t.Errorf("read_check %v, want the row's version", r.ReadCheck)
	}
}

// A row deleted after it was read is stale, and the error says it was
// deleted (spec 6, P6); a row that was not there when read is no_read.
func TestDeletedRowIsStale(t *testing.T) {
	e := setupStale(t, calm(), staleConfig("enforce"))
	agent := e.agent(t, "a")
	e.read(t, agent, e.table, 1)
	mustExec(t, e.admin, "delete from "+e.table+" where id = 1")
	_, err := agent.Exec(context.Background(), "update "+e.table+" set title = 'x' where id = 1")
	wantRefused(t, err, "ST003", "write refused by stet: the row was deleted after it was read")
	r := e.checks(t, e.table, 1)[0]
	wantDecided(t, 0, r, "reject", "stale_read")
	wantRead(t, 0, r, "stale")
	if v, ok := r.ReadCheck["version"]; !ok || v != nil {
		t.Errorf("read_check %v, want version null", r.ReadCheck)
	}

	// Never read, and not there.
	mustExec(t, agent, "delete from "+e.table+" where id = 5")
	r = e.checks(t, e.table, 2)[1]
	wantRead(t, 1, r, "no_read")
	wantDecided(t, 1, r, "commit")
}

// A key written otherwise than the server writes it names the same row (spec
// 5): '042' and 42::bigint are the row 42.
func TestKeyWrittenOtherwise(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	mustExec(t, e.admin, "insert into "+e.table+" (id, name) values (42, 'Forty')")
	agent := e.agent(t, "a")
	e.read(t, agent, e.table, 42)
	mustExec(t, agent, "update "+e.table+" set title = 'a' where id = '042'")
	e.idle(t)
	mustExec(t, agent, "update "+e.table+" set title = 'b' where id = 42::bigint")
	e.idle(t)
	mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 42")
	mustExec(t, agent, "update "+e.table+" set title = 'c' where id = '042'")
	rs := e.checks(t, e.table, 3)
	for i, want := range []string{"fresh", "fresh", "stale"} {
		wantRead(t, i, rs[i], want)
	}
}

// A write sent while a read of its row is still being looked up is not
// judged: read_pending (spec 5).
func TestWriteWhileLookupPending(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	h := holdLookups(t, e.gate)
	agent := e.agent(t, "a")
	e.read(t, agent, e.table, 1)
	mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")
	h.hold()
	e.readOnly(t, agent, e.table, 1)
	mustExec(t, agent, "update "+e.table+" set title = 'x' where id = 1")
	r := e.checks(t, e.table, 1)[0]
	wantRead(t, 0, r, "read_pending")
	wantDecided(t, 0, r, "commit")
}

// A role's own write is not a change someone else made: once its refresh
// ended the next write is fresh, and while it has not, own_write (N2).
func TestOwnWrite(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	h := holdLookups(t, e.gate)
	agent := e.agent(t, "a")
	e.read(t, agent, e.table, 1)
	mustExec(t, agent, "update "+e.table+" set title = 'one' where id = 1")
	e.idle(t)
	mustExec(t, agent, "update "+e.table+" set title = 'two' where id = 1")
	e.idle(t)
	h.hold()
	mustExec(t, agent, "update "+e.table+" set title = 'three' where id = 1")
	mustExec(t, e.agent(t, "b"), "update "+e.table+" set title = 'four' where id = 1")
	h.release()
	e.idle(t)
	rs := e.checks(t, e.table, 4)
	for i, want := range []string{"fresh", "fresh", "fresh", "own_write"} {
		wantRead(t, i, rs[i], want)
	}
}

// A write on behalf of another task of the same role refreshes every task's
// memory of the row (spec 7, N9).
func TestOwnWriteFromAnotherTask(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	a, b := e.agent(t, "a"), e.agent(t, "b")
	mustExec(t, a, "set stet.on_behalf_of = 'a'")
	mustExec(t, b, "set stet.on_behalf_of = 'b'")
	e.read(t, a, e.table, 1)
	mustExec(t, b, "update "+e.table+" set title = 'b' where id = 1")
	e.idle(t)
	mustExec(t, a, "update "+e.table+" set title = 'a' where id = 1")
	rs := e.checks(t, e.table, 2)
	if got := rs[1].ReadCheck["result"]; got != "fresh" && got != "own_write" {
		t.Errorf("the write after another task's: read_check %v, want fresh or own_write", rs[1].ReadCheck)
	}
}

// A write inside a transaction keeps its mark until the transaction ends, then
// what the role read of the row is forgotten (spec 7, N7); a read that ended
// before the write and whose lookup ends after the transaction does not bring
// it back (spec 5).
func TestOwnWriteInTransaction(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	h := holdLookups(t, e.gate)
	su := superuser(t)
	a, b, c := e.agent(t, "a"), e.agent(t, "b"), e.agent(t, "c")
	ctx := context.Background()
	e.read(t, a, e.table, 1)
	mustExec(t, a, "begin")
	mustExec(t, a, "update "+e.table+" set title = 'in tx' where id = 1")
	// b's write waits on the server for a's row lock; its check was made
	// before it was sent.
	done := make(chan error, 1)
	go func() {
		_, err := b.Exec(ctx, "update "+e.table+" set title = 'b' where id = 1")
		done <- err
	}()
	waitLocked(t, su, e.table, 1)
	mustExec(t, a, "commit")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.idle(t)
	mustExec(t, c, "update "+e.table+" set title = 'after' where id = 1")
	rs := e.checks(t, e.table, 3)
	for i, want := range []string{"fresh", "own_write", "no_read"} {
		wantRead(t, i, rs[i], want)
	}

	h.hold()
	e.readOnly(t, a, e.table, 1) // its lookup waits
	mustExec(t, a, "begin")
	mustExec(t, a, "update "+e.table+" set title = 'in tx again' where id = 1")
	mustExec(t, a, "commit")
	h.release()
	e.idle(t)
	mustExec(t, c, "update "+e.table+" set title = 'after again' where id = 1")
	rs = e.checks(t, e.table, 5)
	wantRead(t, 3, rs[3], "read_pending")
	wantRead(t, 4, rs[4], "no_read")
}

// A write of many rows forgets what its role read of the table, and while it
// is on the server the role's writes to the table are its own (spec 7,
// correction S3, N6). A write shaped with WITH forgets every table (S9).
func TestMultiRowWriteForgetsTable(t *testing.T) {
	notes := otherTable(t, "notes", "(id int primary key, title text)")
	e := setupStale(t, calm(), staleConfig("shadow", tableBlock(notes, "id", "stale: {}")))
	mustExec(t, e.admin, "insert into "+notes+" values (1, 'n')")
	su := superuser(t)
	a, b := e.agent(t, "a"), e.agent(t, "b")
	ctx := context.Background()

	e.read(t, a, e.table, 1)
	mustExec(t, a, "update "+e.table+" set status = 'm' where id > 0")
	e.idle(t)
	mustExec(t, a, "update "+e.table+" set title = 'x' where id = 1")
	rs := e.checks(t, e.table, 2)
	wantRead(t, 0, rs[0], "not_checked")
	wantRead(t, 1, rs[1], "no_read")

	// While the write of many rows waits on the server.
	e.read(t, a, e.table, 1)
	locker := direct(t)
	mustExec(t, locker, "begin")
	mustExec(t, locker, "select 1 from "+e.table+" for update")
	errs := make(chan error, 2)
	go func() {
		_, err := a.Exec(ctx, "update "+e.table+" set status = 'n' where id > 0")
		errs <- err
	}()
	waitLocked(t, su, e.table, 1)
	go func() {
		_, err := b.Exec(ctx, "update "+e.table+" set title = 'y' where id = 1")
		errs <- err
	}()
	waitLocked(t, su, e.table, 2)
	mustExec(t, locker, "commit")
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	e.idle(t)
	rs = e.checks(t, e.table, 4)
	wantRead(t, 3, rs[3], "own_write")

	// WITH: the other table is forgotten too.
	e.read(t, a, e.table, 2)
	e.read(t, a, notes, 1)
	mustExec(t, a, "with x as (select 1) update "+e.table+" set title = 'w' where id = 2")
	e.idle(t)
	mustExec(t, a, "update "+notes+" set title = 'z' where id = 1")
	nr := e.checks(t, notes, 1)[0]
	wantRead(t, 0, nr, "no_read")
}

// An INSERT with a key refreshes what its role read of that row: a row read,
// deleted by another party and inserted again by the role is not stale. An
// INSERT without one forgets nothing (spec 7).
func TestInsertWithKey(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	mustExec(t, e.admin, "alter table "+e.table+" alter column id add generated by default as identity (start with 100)")
	agent := e.agent(t, "a")
	e.read(t, agent, e.table, 1)
	e.read(t, agent, e.table, 2)
	mustExec(t, e.admin, "delete from "+e.table+" where id = 1")
	mustExec(t, agent, "insert into "+e.table+" (id, name) values (1, 'Again')")
	e.idle(t)
	mustExec(t, agent, "update "+e.table+" set title = 'x' where id = 1")
	mustExec(t, agent, "insert into "+e.table+" (name) values ('New')")
	e.idle(t)
	mustExec(t, agent, "update "+e.table+" set title = 'y' where id = 2")
	rs := e.checks(t, e.table, 4)
	if got := rs[1].ReadCheck["result"]; got == "stale" {
		t.Errorf("the write after the role's INSERT: read_check %v, want it not stale", rs[1].ReadCheck)
	}
	wantRead(t, 3, rs[3], "fresh")
}

// A write whose connection ended before its answer may still commit: its mark
// stays for staleLostGrace, then what the role read of the row is forgotten,
// so the change it may have made is not taken for another party's
// (corrections S6, S15).
func TestLostWriteLeavesNoMark(t *testing.T) {
	// Long enough for any machine; the test ends it itself (ExpireLostMarks).
	// TestLostMarksEnd shows the timer does.
	defer gate.SetStaleLostGrace(time.Hour)()
	e := setupStale(t, calm(), staleYAML)
	su := superuser(t)
	a := e.agent(t, "a")
	ctx := context.Background()
	e.read(t, a, e.table, 1)
	locker := direct(t)
	mustExec(t, locker, "begin")
	mustExec(t, locker, "select 1 from "+e.table+" where id = 1 for update")
	lost := e.agent(t, "lost")
	lost.PgConn().Exec(ctx, "update "+e.table+" set title = 'lost' where id = 1")
	waitLocked(t, su, e.table, 1)
	lost.PgConn().Conn().Close()
	if r := e.checks(t, e.table, 1)[0]; r.Executed {
		t.Errorf("the lost write recorded as executed")
	}

	// Within the grace the role reads the row again, before the lost write
	// commits, and its writes to the row are its own.
	e.read(t, a, e.table, 1)
	check := func() string { return gate.CheckRead(e.gate, "stet_md5", "", e.table, "1", e.xminOf(t, 1)).Result }
	if got := check(); got != decide.ReadOwnWrite {
		t.Errorf("within the grace: %s, want own_write", got)
	}
	mustExec(t, locker, "commit") // the lost write commits now
	waitUntil(t, "the lost write committed", func() bool { return e.rowsWhere(t, "id = 1 and title = 'lost'") == 1 })
	if got := check(); got != decide.ReadOwnWrite {
		t.Errorf("within the grace, after the lost write committed: %s, want own_write", got)
	}
	gate.ExpireLostMarks(e.gate) // the grace passes
	if got := check(); got == decide.ReadOwnWrite {
		t.Errorf("after the grace: %s, want the mark gone", got)
	}
	mustExec(t, a, "update "+e.table+" set title = 'after' where id = 1")
	wantRead(t, 1, e.checks(t, e.table, 2)[1], "no_read")
}

// A lost write's mark ends when its grace has passed, and Close ends one at
// once.
func TestLostMarksEnd(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	e.insert(t, deals, "id", "1", "2")
	e.start(t, tableBlock(deals, "id", "stale: {}"))
	agent := proxy.SessionInfo{ID: "s", User: "agent"}
	lose := func(id string, grace time.Duration) {
		t.Helper()
		defer gate.SetStaleLostGrace(grace)()
		h, _ := e.gate.Begin(agent, e.gate.Inspect("update "+deals+" set title = 'x' where id = "+id), nil)
		e.gate.End(h, proxy.Outcome{Lost: true, Error: "connection closed"})
	}

	lose("1", 50*time.Millisecond)
	waitUntil(t, "the grace passed", func() bool { return e.check("agent", "", deals, "1", "1").Result != decide.ReadOwnWrite })

	lose("2", time.Hour)
	e.wantResult(t, "agent", "", deals, "2", "1", decide.ReadOwnWrite)
	start := time.Now()
	e.gate.Close()
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("Close took %v", took)
	}
	e.wantResult(t, "agent", "", deals, "2", "1", decide.ReadNone)
}

// A key value the server may read otherwise than the key column does is not
// used: the write is not_checked (correction S13). Casts to the key's own
// type are checked.
func TestKeyValueNotUsable(t *testing.T) {
	texts := otherTable(t, "texts", "(id text primary key, title text)")
	uuids := otherTable(t, "uuids", "(id uuid primary key, title text)")
	e := setupStale(t, calm(), staleConfig("shadow", tableBlock(texts, "id", "stale: {}"), tableBlock(uuids, "id", "stale: {}")))
	u := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	mustExec(t, e.admin, "insert into "+texts+` values ('ab', 't'), ('ab  ', 't'), ('a\b', 't')`)
	mustExec(t, e.admin, "insert into "+uuids+" values ($1, 'u')", u)
	agent := e.agent(t, "a")
	ctx := context.Background()
	e.read(t, agent, texts, "ab")
	e.read(t, agent, uuids, u)
	e.read(t, agent, e.table, 1)

	for _, w := range []struct {
		sql  string
		args []any
	}{
		{"update " + texts + " set title = 'x' where id = $1::char(4)", []any{"ab"}},
		{"update " + texts + " set title = $1 where id = $1", []any{"ab"}},
		{"update " + texts + ` set title = 'x' where id = 'a\b'`, nil},
	} {
		if _, err := agent.Exec(ctx, w.sql, w.args...); err != nil {
			t.Fatalf("%s: %v", w.sql, err)
		}
	}
	for i, r := range e.checks(t, texts, 3) {
		wantRead(t, i, r, "not_checked")
	}
	// A bind value the client typed bpchar: the server compares it with the
	// text key as text, without its trailing spaces, and writes the row ab,
	// which nobody changed; the row 'ab  ' its text names did change.
	e.read(t, agent, texts, "ab  ")
	mustExec(t, e.admin, "update "+texts+" set title = 'changed' where id = 'ab  '")
	res := agent.PgConn().ExecParams(ctx, "update "+texts+" set title = 'x' where id = $1",
		[][]byte{[]byte("ab  ")}, []uint32{1042}, nil, nil).Read()
	if res.Err != nil || res.CommandTag.String() != "UPDATE 1" {
		t.Fatalf("the write typed bpchar: %v %v", res.CommandTag, res.Err)
	}
	var title string
	if err := e.admin.QueryRow(ctx, "select title from "+texts+" where id = 'ab'").Scan(&title); err != nil || title != "x" {
		t.Fatalf("the row ab after the write typed bpchar: %q %v, want it written", title, err)
	}
	wantRead(t, 3, e.checks(t, texts, 4)[3], "not_checked")

	if _, err := agent.Exec(ctx, "update "+uuids+" set title = 'x' where id = $1::uuid", u); err != nil {
		t.Fatal(err)
	}
	wantRead(t, 0, e.checks(t, uuids, 1)[0], "fresh")
	if _, err := agent.Exec(ctx, "update "+e.table+" set title = 'x' where id = $1::bigint", 1); err != nil {
		t.Fatal(err)
	}
	wantRead(t, 0, e.checks(t, e.table, 1)[0], "fresh")
}

// A write on behalf of a task is compared with that task's reads and those
// of no task: another task's later read does not hide a change (P7); with no
// task at all, it does (M1, a known miss).
func TestOnBehalfOf(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	a, b := e.agent(t, "a"), e.agent(t, "b")
	mustExec(t, a, "set stet.on_behalf_of = 'a'")
	mustExec(t, b, "set stet.on_behalf_of = 'b'")
	e.read(t, a, e.table, 1)
	mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")
	e.read(t, b, e.table, 1)
	mustExec(t, a, "update "+e.table+" set title = 'x' where id = 1")
	r := e.checks(t, e.table, 1)[0]
	wantRead(t, 0, r, "stale")
	if r.ReadCheck["read_on_behalf_of"] != "a" {
		t.Errorf("read_check %v, want read_on_behalf_of a", r.ReadCheck)
	}

	c, d := e.agent(t, "c"), e.agent(t, "d")
	e.read(t, c, e.table, 2)
	mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 2")
	e.read(t, d, e.table, 2)
	mustExec(t, c, "update "+e.table+" set title = 'y' where id = 2")
	wantRead(t, 1, e.checks(t, e.table, 2)[1], "fresh")
}

// A stale write is refused until the role reads the row again, on any
// connection and for any task (spec 6).
func TestUnlockAfterRejection(t *testing.T) {
	e := setupStale(t, calm(), staleConfig("enforce"))
	a, b := e.agent(t, "a"), e.agent(t, "b")
	mustExec(t, a, "set stet.on_behalf_of = 'a'")
	mustExec(t, b, "set stet.on_behalf_of = 'b'")
	ctx := context.Background()
	e.read(t, a, e.table, 1)
	mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")
	for range 2 {
		_, err := a.Exec(ctx, "update "+e.table+" set title = 'x' where id = 1")
		wantRefused(t, err, "ST003", "the row was changed after it was read")
	}
	e.read(t, b, e.table, 1)
	mustExec(t, a, "update "+e.table+" set title = 'x' where id = 1")
	rs := e.checks(t, e.table, 3)
	wantRead(t, 2, rs[2], "fresh")
	if !rs[2].Executed || e.rowsWhere(t, "id = 1 and title = 'x'") != 1 {
		t.Errorf("the write after reading again did not run")
	}
}

// The stale rule comes before the cumulative limit: a stale write over the
// limit is rejected, not held (spec 6).
func TestStaleBeforeCumulative(t *testing.T) {
	e := setupStale(t, calm(), staleConfig("enforce")+"    impact: {max_rows: 1, cumulative: {max_rows: 1, window: 1h}}\n")
	agent := e.agent(t, "a")
	e.read(t, agent, e.table, 1)
	mustExec(t, agent, "update "+e.table+" set title = 'x' where id = 2") // uses the limit
	mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")
	_, err := agent.Exec(context.Background(), "update "+e.table+" set title = 'x' where id = 1")
	wantRefused(t, err, "ST003", "the row was changed after it was read")
	r := e.checks(t, e.table, 2)[1]
	wantDecided(t, 1, r, "reject", "stale_read")
	if hs := e.held(t, 0); len(hs) != 0 {
		t.Errorf("%d held writes, want none", len(hs))
	}
	if a := e.enforced(t, 2)[1]; a.Cumulative["used"] != float64(1) {
		t.Errorf("cumulative %v, want the limit used", a.Cumulative)
	}
}

// A stale write is decided without the model, in both modes (spec 6).
func TestStaleNeverAsksTheModel(t *testing.T) {
	for _, mode := range []string{"shadow", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			format := tableYAML + "    stale: {}\n"
			if mode == "enforce" {
				format = enforcePrefix + format
			}
			d := calm()
			e := setupStale(t, d, format)
			agent := e.agent(t, "a")
			e.read(t, agent, e.table, 1)
			mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")
			agent.Exec(context.Background(), "update "+e.table+" set title = 'x' where id = 1")
			r := e.checks(t, e.table, 1)[0]
			wantDecided(t, 0, r, "reject", "stale_read")
			if n := d.callCount(); n != 0 {
				t.Errorf("the model was called %d times, want never", n)
			}
		})
	}
}

// A row that changes while its write is judged is compared again: with a
// read remembered the write is rejected as stale, and its row version is not
// read a third time; with none it is judged again as before (spec 6).
func TestRowChangedWhileJudged(t *testing.T) {
	e := setupStale(t, calm(), staleConfig("enforce"))
	var mu sync.Mutex
	calls := 0
	gate.OnRowVersion(e.gate, func(table, pk string) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 { // the first re-read of each write finds the row changed
			e.adminExec(t, "update "+e.table+" set name = 'Admin' where id = "+pk)
		}
	})
	agent := e.agent(t, "a")
	e.read(t, agent, e.table, 1)
	_, err := agent.Exec(context.Background(), "update "+e.table+" set title = 'x' where id = 1")
	wantRefused(t, err, "ST003", "the row was changed after it was read")
	r := e.checks(t, e.table, 1)[0]
	wantDecided(t, 0, r, "reject", "stale_read", "rejudged")
	wantRead(t, 0, r, "stale")
	if r.Refused == nil || *r.Refused != "ST003" {
		t.Errorf("refused %v, want ST003", deref(r.Refused))
	}
	mu.Lock()
	if calls != 1 {
		t.Errorf("the row version was read again %d times, want once", calls)
	}
	mu.Unlock()

	mustExec(t, agent, "update "+e.table+" set title = 'y' where id = 2")
	r = e.checks(t, e.table, 2)[1]
	wantDecided(t, 1, r, "commit", "rejudged")
	wantRead(t, 1, r, "no_read")
}

// In a transaction that already changed something, in_transaction decides
// first; shadow still records what the comparison found (correction S4),
// and enforce, which reads nothing for such a write, records not_checked.
func TestInTransactionComesFirst(t *testing.T) {
	for _, mode := range []string{"shadow", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			e := setupStale(t, calm(), staleConfig(mode))
			agent := e.agent(t, "a")
			ctx := context.Background()
			e.read(t, agent, e.table, 1)
			mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")
			mustExec(t, agent, "begin")
			mustExec(t, agent, "select 1")
			agent.Exec(ctx, "update "+e.table+" set title = 'x' where id = 1")
			mustExec(t, agent, "rollback")
			r := e.checks(t, e.table, 1)[0]
			wantDecided(t, 0, r, "unsupported", "in_transaction")
			want := "stale"
			if mode == "enforce" {
				want = "not_checked"
			}
			wantRead(t, 0, r, want)
		})
	}
}

// One login role's reads, writes and on_behalf_of never change another
// role's comparison (rule 5): X cannot make Y's good write stale, nor hide
// Y's stale one by reading under Y's task name.
func TestOtherRoleCannotMakeStale(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	x := e.connect(t, "x") // stet_scram through the proxy
	y := e.agent(t, "y")   // stet_md5
	mustExec(t, y, "set stet.on_behalf_of = 'task'")

	e.read(t, x, e.table, 1)
	mustExec(t, x, "update "+e.table+" set title = 'by x' where id = 1")
	e.idle(t)
	for _, task := range []string{"task", "", "other"} {
		mustExec(t, x, "set stet.on_behalf_of = '"+task+"'")
		e.read(t, x, e.table, 1)
	}
	e.read(t, y, e.table, 1)
	mustExec(t, x, "set stet.on_behalf_of = 'task'")
	e.read(t, x, e.table, 1)
	mustExec(t, y, "update "+e.table+" set title = 'by y' where id = 1")
	rs := e.checks(t, e.table, 2)
	wantRead(t, 1, rs[1], "fresh")

	e.read(t, y, e.table, 2)
	mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 2")
	e.read(t, x, e.table, 2)
	mustExec(t, y, "update "+e.table+" set title = 'by y' where id = 2")
	wantRead(t, 2, e.checks(t, e.table, 3)[2], "stale")
}

// What a shadow session sends that Stet cannot see into (a DO block) forgets
// everything its role read, and its writes are the role's own until the
// message's transaction ended (spec 7, correction S7, N11).
func TestUnseenForgets(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	su := superuser(t)
	a, b := e.agent(t, "a"), e.agent(t, "b")
	ctx := context.Background()
	doBlock := "do $$ begin update " + e.table + " set title = 'do' where id = 1; end $$"

	e.read(t, a, e.table, 1)
	mustExec(t, a, doBlock)
	mustExec(t, a, "update "+e.table+" set title = 'x' where id = 1")
	wantRead(t, 0, e.checks(t, e.table, 1)[0], "no_read")

	// While the DO block waits on the server.
	e.read(t, a, e.table, 2)
	locker := direct(t)
	mustExec(t, locker, "begin")
	mustExec(t, locker, "select 1 from "+e.table+" where id = 1 for update")
	done := make(chan error, 1)
	go func() {
		_, err := a.Exec(ctx, doBlock)
		done <- err
	}()
	waitLocked(t, su, e.table, 1)
	mustExec(t, b, "update "+e.table+" set title = 'y' where id = 2")
	wantRead(t, 1, e.checks(t, e.table, 2)[1], "own_write")
	mustExec(t, locker, "commit")
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// Two Unseen calls, one settled: the mark stays until the other is.
	info := proxy.SessionInfo{ID: "pipelined", User: "stet_md5"}
	check := func() string { return gate.CheckRead(e.gate, "stet_md5", "", e.table, "2", e.xminOf(t, 2)).Result }
	e.gate.Unseen(info)
	e.gate.Unseen(info)
	e.gate.TxEnded(info, 1, false)
	if got := check(); got != decide.ReadOwnWrite {
		t.Errorf("one of two Unseen calls settled: %s, want own_write", got)
	}
	e.gate.TxEnded(info, 1, false)
	if got := check(); got == decide.ReadOwnWrite {
		t.Errorf("both Unseen calls settled: %s, want the mark gone", got)
	}
	// An Unseen call made before the session's login role was known is
	// counted too: settling it does not end the mark of a later one.
	early := proxy.SessionInfo{ID: "early"}
	known := proxy.SessionInfo{ID: "early", User: "stet_md5"}
	e.gate.Unseen(early)
	e.gate.Unseen(known)
	e.gate.TxEnded(known, 1, false)
	if got := check(); got != decide.ReadOwnWrite {
		t.Errorf("an early Unseen call settled: %s, want the later one's mark", got)
	}
	e.gate.TxEnded(known, 1, false)
	if got := check(); got == decide.ReadOwnWrite {
		t.Errorf("both settled: %s, want the mark gone", got)
	}
}

// Stet applying an approved write as a role makes that role's writes to the
// table its own until it is done, then forgets the table (correction S10).
func TestApplying(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	agent := e.agent(t, "a")
	e.read(t, agent, e.table, 1)
	done := e.gate.Applying("stet_md5", e.table)
	mustExec(t, agent, "update "+e.table+" set title = 'x' where id = 1")
	e.idle(t)
	done()
	done() // tolerated
	mustExec(t, agent, "update "+e.table+" set title = 'y' where id = 1")
	rs := e.checks(t, e.table, 2)
	wantRead(t, 0, rs[0], "own_write")
	wantRead(t, 1, rs[1], "no_read")

	e.gate.Applying("stet_md5", "no_such_table")() // a table without stale: nothing
	e.gate.Close()
	e.gate.Applying("stet_md5", e.table)() // after Close
}

// Writes that are not compared record not_checked; a table without stale
// records no read_check at all.
func TestNotChecked(t *testing.T) {
	dated := otherTable(t, "dated", "(day date primary key, title text)")
	plain := otherTable(t, "plain", "(id int primary key, title text)")
	e := setupStale(t, calm(), staleConfig("shadow", tableBlock(dated, "day", "stale: {}"), tableBlock(plain, "id")))
	mustExec(t, e.admin, "insert into "+dated+" values ('2026-11-20', 'd')")
	mustExec(t, e.admin, "insert into "+plain+" values (1, 'p')")
	agent := e.agent(t, "a")
	e.read(t, agent, e.table, 1)

	mustExec(t, agent, "insert into "+e.table+" (id, name) values (3, 'New')")
	mustExec(t, agent, "update "+e.table+" set status = 'm' where id > 0")
	h, _ := e.gate.Begin(proxy.SessionInfo{ID: "early"}, e.gate.Inspect("update "+e.table+" set title = 'x' where id = 1"), nil)
	e.gate.End(h, proxy.Outcome{Executed: true, Rows: ptr(int64(1))})
	for i, r := range e.checks(t, e.table, 3) {
		wantRead(t, i, r, "not_checked")
	}
	mustExec(t, agent, "update "+dated+" set title = 'x' where day = '2026-11-20'")
	wantRead(t, 0, e.checks(t, dated, 1)[0], "not_checked")
	mustExec(t, agent, "update "+plain+" set title = 'x' where id = 1")
	if r := e.checks(t, plain, 1)[0]; r.ReadCheck != nil {
		t.Errorf("a table without stale: read_check %v, want NULL", r.ReadCheck)
	}
}

// The state of a write to a row that is not there is what it was before the
// table had stale, though the state read now gives back a row (S11).
func TestRowAbsentStateUnchanged(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	agent := proxy.SessionInfo{ID: "s", User: "agent"}
	// Two rows, so that the second write has no recent change of the first.
	for i, clauses := range [][]string{nil, {"stale: {}"}} {
		e.start(t, tableBlock(deals, "id", clauses...))
		sql := fmt.Sprintf("update %s set title = 'x' where id = %d", deals, 98+i)
		h, refuse := e.gate.Begin(agent, e.gate.Inspect(sql), nil)
		if refuse != nil {
			t.Fatalf("refused: %+v", refuse)
		}
		e.gate.End(h, proxy.Outcome{Executed: true, Rows: ptr(int64(0))})
		e.gate.Close()
	}
	rows, err := e.admin.Query(context.Background(), `
		select state, state_version, read_check from stet.decisions where target_table = $1 order by created_at`, deals)
	if err != nil {
		t.Fatal(err)
	}
	type rec struct {
		State        map[string]any
		StateVersion map[string]any
		ReadCheck    map[string]any
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[rec])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d decisions, want 2", len(got))
	}
	if !reflect.DeepEqual(got[0].State, got[1].State) || got[0].StateVersion != nil || got[1].StateVersion != nil {
		t.Errorf("state %v / %v, state_version %v / %v; want the same state and no version", got[0].State, got[1].State,
			got[0].StateVersion, got[1].StateVersion)
	}
	if got[1].ReadCheck["result"] != "no_read" {
		t.Errorf("read_check %v, want no_read", got[1].ReadCheck)
	}
}

// An UPDATE that changes a row's key writes a row its role may remember by
// the new key: what the role read of the table is forgotten, so its later
// write of that row is not taken for stale.
func TestKeyChangingUpdate(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	mustExec(t, e.admin, "insert into "+e.table+" (id, name) values (42, 'Forty'), (43, 'Old')")
	agent := e.agent(t, "a")
	e.read(t, agent, e.table, 43)
	mustExec(t, e.admin, "delete from "+e.table+" where id = 43")
	mustExec(t, agent, "update "+e.table+" set id = 43 where id = 42")
	e.idle(t)
	mustExec(t, agent, "update "+e.table+" set title = 'x' where id = 43")
	if r := e.checks(t, e.table, 2)[1]; r.ReadCheck["result"] == "stale" {
		t.Errorf("the write of the row the role renamed: read_check %v, want it not stale", r.ReadCheck)
	}
}

// An own write that committed after a write's state was read, and whose mark
// ended before the comparison, leaves a version newer than the one the state
// read found: the comparison is not judged (read_pending). One that ended
// before the state read was sent is compared as any change is.
func TestOwnWriteEndedWhileStateRead(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	e.insert(t, deals, "id", "1")
	e.start(t, tableBlock(deals, "id", "stale: {}"))
	agent := proxy.SessionInfo{ID: "s", User: "agent"}
	e.gate.Observe(agent, e.gate.Inspect("select id from "+deals), keyed(e.oid(t, deals), "1"))
	e.idle(t)
	read := e.xmin(t, deals, "id", "1")

	sent := time.Now() // the second write's state read finds read
	h, _ := e.gate.Begin(agent, e.gate.Inspect("update "+deals+" set title = 'x' where id = 1"), nil)
	mustExec(t, e.admin, "update "+deals+" set title = 'x' where id = 1") // the first write commits
	e.gate.End(h, proxy.Outcome{Executed: true, Rows: ptr(int64(1))})
	e.idle(t) // its refresh ended: the role knows the row by its new version

	if got := gate.CompareWrite(e.gate, "agent", "", deals, "1", read, sent); got.Result != decide.ReadPending {
		t.Errorf("an own write ended during the state read: %+v, want read_pending", got)
	}
	if got := gate.CompareWrite(e.gate, "agent", "", deals, "1", read, time.Now()); got.Result != decide.ReadStale {
		t.Errorf("an own write ended before the state read: %+v, want stale", got)
	}
	// Another role's writes do not change this role's comparison.
	other := proxy.SessionInfo{ID: "o", User: "other"}
	e.gate.Observe(other, e.gate.Inspect("select id from "+deals), keyed(e.oid(t, deals), "1"))
	e.idle(t)
	sent = time.Now()
	h, _ = e.gate.Begin(agent, e.gate.Inspect("update "+deals+" set title = 'y' where id = 1"), nil)
	mustExec(t, e.admin, "update "+deals+" set title = 'y' where id = 1")
	e.gate.End(h, proxy.Outcome{Executed: true, Rows: ptr(int64(1))})
	e.idle(t)
	if got := gate.CompareWrite(e.gate, "other", "", deals, "1", read, sent); got.Result != decide.ReadStale {
		t.Errorf("another role's write ended during the state read: %+v, want stale", got)
	}
}

// A write that may write other tables than the one the classifier names
// marks every table of its login role, whether or not the named table has
// stale (correction S9): its other statements may change a row the role
// read, and the role's next write of that row is not stale. A message of
// several statements is such a write even when a PREPARE gives it another
// reason.
func TestEveryTableWrites(t *testing.T) {
	plain := otherTable(t, "plain", "(id int primary key, title text)")
	e := setupStale(t, calm(), staleConfig("shadow", tableBlock(plain, "id")))
	mustExec(t, e.admin, "insert into "+plain+" values (1, 'p')")
	agent := e.agent(t, "a")
	ctx := context.Background()
	for i, sql := range []string{
		"update " + plain + " set title = 'm' where id = 1; update " + e.table + " set title = 'm' where id = 1",
		"with w as (update " + e.table + " set title = 'w' where id = 1 returning 1) update " + plain + " set title = 'w' where id = 1",
		"prepare every_" + plain + " as update " + plain + " set title = $1 where id = 1; update " + e.table + " set title = 'p' where id = 1",
	} {
		e.read(t, agent, e.table, 1)
		if err := agent.PgConn().Exec(ctx, sql).Close(); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		e.idle(t)
		marker := fmt.Sprintf("'after %d'", i)
		mustExec(t, agent, "update "+e.table+" set title = "+marker+" where id = 1")
		if r := e.checkOf(t, e.table, marker); r.ReadCheck["result"] == "stale" {
			t.Errorf("after %s: read_check %v, want it not stale", sql, r.ReadCheck)
		}
	}
}

// A message of several statements that failed may have committed some of
// them first: what its mark covers is forgotten. A single statement that
// failed changed nothing, and what its role read stays.
func TestSeveralStatementsThatFailed(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	agent := e.agent(t, "a")
	ctx := context.Background()
	e.read(t, agent, e.table, 1)
	err := agent.PgConn().Exec(ctx, "begin; update "+e.table+" set title = 'committed' where id = 1; commit; select 1/0").Close()
	if err == nil {
		t.Fatal("the message did not fail")
	}
	if e.rowsWhere(t, "id = 1 and title = 'committed'") != 1 {
		t.Fatal("its write was not committed")
	}
	e.idle(t)
	mustExec(t, agent, "update "+e.table+" set title = 'first' where id = 1")
	wantRead(t, 0, e.checkOf(t, e.table, "'first'"), "no_read")

	e.read(t, agent, e.table, 2)
	if _, err := agent.Exec(ctx, "update "+e.table+" set title = (1/0)::text where id = 2"); err == nil {
		t.Fatal("the single statement did not fail")
	}
	e.idle(t)
	mustExec(t, agent, "update "+e.table+" set title = 'second' where id = 2")
	wantRead(t, 1, e.checkOf(t, e.table, "'second'"), "fresh")
}

// When a connection ends with something it sent unanswered, the server may
// still commit it a little after the gate is told: the marks the session
// holds, of an unseen message or of writes in its transaction, stay for
// staleLostGrace as a lost write's does (correction S15), so the role's own
// change is not taken for another party's; then what they cover is
// forgotten once more.
func TestUnansweredKeepsMarks(t *testing.T) {
	defer gate.SetStaleLostGrace(time.Hour)()
	ctx := context.Background()
	// cut ends conn's connection and waits until the gate was told.
	cut := func(t *testing.T, e *env, conn *pgx.Conn) {
		t.Helper()
		conn.PgConn().Conn().Close()
		waitUntil(t, "the gate was told the connection ended", func() bool { return gate.HeldSessions(e.gate) == 0 })
	}
	// ownAfter has the role read row 1 again, before what conn sent commits,
	// then lets it commit, and writes the row.
	ownAfter := func(t *testing.T, e *env, a *pgx.Conn, commit func(), title string) {
		t.Helper()
		e.read(t, a, e.table, 1) // the version before
		commit()
		waitUntil(t, "it committed", func() bool { return e.rowsWhere(t, "id = 1 and title = '"+title+"'") == 1 })
		mustExec(t, a, "update "+e.table+" set title = 'own' where id = 1")
		wantRead(t, 0, e.checkOf(t, e.table, "'own'"), "own_write")
		gate.ExpireLostMarks(e.gate)
		mustExec(t, a, "update "+e.table+" set title = 'later' where id = 1")
		wantRead(t, 1, e.checkOf(t, e.table, "'later'"), "no_read")
	}

	t.Run("a DO block", func(t *testing.T) {
		e := setupStale(t, calm(), staleYAML)
		su := superuser(t)
		a, lost := e.agent(t, "a"), e.agent(t, "lost")
		e.read(t, a, e.table, 1)
		locker := direct(t)
		mustExec(t, locker, "begin")
		mustExec(t, locker, "select 1 from "+e.table+" where id = 1 for update")
		lost.PgConn().Exec(ctx, "do $$ begin update "+e.table+" set title = 'do' where id = 1; end $$")
		waitLocked(t, su, e.table, 1)
		cut(t, e, lost)
		ownAfter(t, e, a, func() { mustExec(t, locker, "commit") }, "do")
	})

	t.Run("a COMMIT", func(t *testing.T) {
		e := setupStale(t, calm(), staleYAML)
		su := superuser(t)
		a, lost := e.agent(t, "a"), e.agent(t, "lost")
		e.read(t, a, e.table, 1)
		key := time.Now().UnixNano() % 1_000_000_000
		wait := fmt.Sprintf("pg_advisory_lock(%d)", key)
		locker := direct(t)
		mustExec(t, locker, "select "+wait)
		mustExec(t, lost, "begin")
		mustExec(t, lost, "update "+e.table+" set title = 'tx' where id = 1")
		lost.PgConn().Exec(ctx, "select "+wait+"; commit")
		waitLocked(t, su, wait, 1)
		cut(t, e, lost)
		ownAfter(t, e, a, func() { mustExec(t, locker, fmt.Sprintf("select pg_advisory_unlock(%d)", key)) }, "tx")
	})
}

// An UPDATE or DELETE of a row that is not there now marks that row's key:
// the row may be there by the time the write runs. Of a bpchar key the text
// of the key read as its type has no padding, so it names nothing the role
// remembers; such a write marks the table.
func TestAbsentRowIsMarked(t *testing.T) {
	codes := otherTable(t, "codes", "(code char(5) primary key, title text)")
	e := setupStale(t, calm(), staleConfig("shadow", tableBlock(codes, "code", "stale: {}")))
	mustExec(t, e.admin, "insert into "+codes+" values ('ab', 't')")
	agent := e.agent(t, "a")
	check := func(table, key, version string) string {
		return gate.CheckRead(e.gate, "stet_md5", "", table, key, version).Result
	}

	e.read(t, agent, e.table, 1)
	mustExec(t, agent, "begin")
	mustExec(t, agent, "update "+e.table+" set title = 'x' where id = 5")
	if got := check(e.table, "5", ""); got != decide.ReadOwnWrite {
		t.Errorf("the row not there, written: %s, want own_write", got)
	}
	if got := check(e.table, "1", e.xminOf(t, 1)); got != decide.ReadFresh {
		t.Errorf("another row: %s, want fresh (a row mark, not a table mark)", got)
	}
	mustExec(t, agent, "commit")
	if got := check(e.table, "5", ""); got == decide.ReadOwnWrite {
		t.Errorf("after its transaction: %s, want the mark gone", got)
	}

	mustExec(t, agent, "begin")
	mustExec(t, agent, "update "+codes+" set title = 'x' where code = 'zz'")
	if got := check(codes, "ab   ", e.xminWhere(t, codes, "code", "ab")); got != decide.ReadOwnWrite {
		t.Errorf("another row of a bpchar table: %s, want own_write (a table mark)", got)
	}
	mustExec(t, agent, "commit")
	if got := check(codes, "ab   ", e.xminWhere(t, codes, "code", "ab")); got == decide.ReadOwnWrite {
		t.Errorf("after its transaction: %s, want the mark gone", got)
	}
}

// A session holds an in-transaction mark only for what its older ones do not
// cover already: a transaction of ten thousand keyed INSERTs holds one table
// mark, so ending it ends one mark, not ten thousand (ending a table mark
// scans the table's marks, with every session waiting).
func TestInTransactionMarksStayFew(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	e.start(t, tableBlock(deals, "id", "stale: {}"))
	inTx := proxy.Outcome{Executed: true, InTransaction: true, Rows: ptr(int64(1))}
	one := proxy.SessionInfo{ID: "one", User: "agent"}
	for i := range 10000 {
		gate.MarkAndEnd(e.gate, one, fmt.Sprintf("insert into %s (id) values (%d)", deals, i), inTx)
	}
	if n := gate.MarkCount(e.gate, "agent", deals); n != 1 {
		t.Errorf("%d marks held after ten thousand INSERTs in one transaction, want 1", n)
	}
	// A mark on every table covers the table's.
	two := proxy.SessionInfo{ID: "two", User: "agent"}
	gate.MarkAndEnd(e.gate, two, "update "+deals+" set title = 'a' where id = 1; update "+deals+" set title = 'b' where id = 2", inTx)
	for i := range 100 {
		gate.MarkAndEnd(e.gate, two, fmt.Sprintf("insert into %s (id) values (%d)", deals, 20000+i), inTx)
	}
	if n := gate.MarkCount(e.gate, "agent", deals); n != 2 {
		t.Errorf("%d marks held, want 2: one session's table mark and the other's on every table", n)
	}
	e.gate.TxEnded(one, 0, false)
	e.gate.TxEnded(two, 0, false)
	if n, s := gate.MarkCount(e.gate, "agent", deals), gate.HeldSessions(e.gate); n != 0 || s != 0 {
		t.Errorf("after the transactions: %d marks, %d sessions held, want none", n, s)
	}
}

// A char(n) key is remembered as the server writes it, padded, and a write
// that names the row unpadded is compared with it (the state read gives the
// row's own key): another party's change is stale, the role's own is not.
func TestCharKey(t *testing.T) {
	codes := otherTable(t, "codes", "(code char(5) primary key, title text)")
	e := setupStale(t, calm(), staleConfig("shadow", tableBlock(codes, "code", "stale: {}")))
	mustExec(t, e.admin, "insert into "+codes+" values ('ab', 't'), ('cd', 't')")
	agent := e.agent(t, "a")
	e.readBy(t, agent, codes, "code", "ab")
	e.idle(t)
	mustExec(t, e.admin, "update "+codes+" set title = 'changed' where code = 'ab'")
	mustExec(t, agent, "update "+codes+" set title = 'x' where code = 'ab'")

	e.readBy(t, agent, codes, "code", "cd")
	e.idle(t)
	mustExec(t, agent, "update "+codes+" set title = 'one' where code = 'cd'")
	e.idle(t)
	mustExec(t, agent, "update "+codes+" set title = 'two' where code = 'cd'")
	rs := e.checks(t, codes, 3)
	for i, want := range []string{"stale", "fresh", "fresh"} {
		wantRead(t, i, rs[i], want)
	}
}

// Order B of the read check's rule, through the proxy: the role's write W1
// in a transaction commits after W2's state was read, the end of its
// transaction forgets the row, and the role reads the row again (W1's
// version) before W2 is compared. Only the role changed the row: W2 is not
// stale.
func TestOwnTransactionEndedWhileStateRead(t *testing.T) {
	e := setupStale(t, calm(), staleYAML)
	a, b, c := e.agent(t, "a"), e.agent(t, "b"), e.agent(t, "c")
	ctx := context.Background()
	var armed atomic.Bool
	gate.OnStateRead(e.gate, func(table string) {
		if !armed.CompareAndSwap(true, false) {
			return
		}
		if _, err := a.Exec(ctx, "commit"); err != nil {
			t.Errorf("commit: %v", err)
			return
		}
		var id int
		if err := c.QueryRow(ctx, "select id from "+e.table+" where id = $1", 1).Scan(&id); err != nil {
			t.Errorf("read: %v", err)
		}
		ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := e.gate.StaleIdle(ictx); err != nil {
			t.Errorf("lookups: %v", err)
		}
	})
	e.read(t, a, e.table, 1)
	mustExec(t, a, "begin")
	mustExec(t, a, "update "+e.table+" set title = 'w1' where id = 1")
	armed.Store(true)
	mustExec(t, b, "update "+e.table+" set title = 'w2' where id = 1")
	wantRead(t, 0, e.checkOf(t, e.table, "'w2'"), "read_pending")
}

// In enforce mode a write whose row its role's own other connection changed
// while it was judged is judged again and is never stale: fresh once that
// write's refresh ended, own_write while it has not.
func TestOwnWriteWhileJudged(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh held %v", held), func(t *testing.T) {
			e := setupStale(t, calm(), staleConfig("enforce"))
			h := holdLookups(t, e.gate)
			a, b := e.agent(t, "a"), e.agent(t, "b")
			ctx := context.Background()
			var armed atomic.Bool
			gate.OnRowVersion(e.gate, func(table, pk string) {
				if !armed.CompareAndSwap(true, false) {
					return
				}
				if held {
					h.hold()
				}
				if _, err := b.Exec(ctx, "update "+e.table+" set title = 'b' where id = 1"); err != nil {
					t.Errorf("the other connection's write: %v", err)
				}
				if !held {
					ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
					if err := e.gate.StaleIdle(ictx); err != nil {
						t.Errorf("lookups: %v", err)
					}
				}
			})
			e.read(t, a, e.table, 1)
			armed.Store(true)
			mustExec(t, a, "update "+e.table+" set title = 'a' where id = 1")
			h.release()
			r := e.checkOf(t, e.table, "'a'")
			wantDecided(t, 0, r, "commit", "rejudged")
			want := "fresh"
			if held {
				want = "own_write"
			}
			wantRead(t, 0, r, want)
		})
	}
}

// The state read of a table that is tracked gives back an existing row as
// the state read without the key type did: the same current row and version.
func TestStateOfAnExistingRowUnchanged(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text, n numeric, tags text[], meta jsonb, at timestamptz, gone text)")
	mustExec(t, e.admin, "insert into "+deals+` values (1, 'it''s "x"', 1.50, '{a,"b c"}', '{"k": [1, null]}', '2026-11-20 10:00:00+00', null)`)
	agent := proxy.SessionInfo{ID: "s", User: "agent"}
	for _, clauses := range [][]string{nil, {"stale: {}"}} {
		block := fmt.Sprintf("  %q:\n    pk: id\n    send_fields: [id, title, n, tags, meta, at, gone]\n", deals)
		for _, c := range clauses {
			block += "    " + c + "\n"
		}
		e.start(t, block)
		h, refuse := e.gate.Begin(agent, e.gate.Inspect("update "+deals+" set title = 'x' where id = 1"), nil)
		if refuse != nil {
			t.Fatalf("refused: %+v", refuse)
		}
		e.gate.End(h, proxy.Outcome{Executed: true, Rows: ptr(int64(1))})
		e.gate.Close()
	}
	rows, err := e.admin.Query(context.Background(), `
		select state->'current', state_version from stet.decisions where target_table = $1 order by created_at`, deals)
	if err != nil {
		t.Fatal(err)
	}
	type rec struct {
		Current      map[string]any
		StateVersion map[string]any
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[rec])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d decisions, want 2", len(got))
	}
	if len(got[0].Current) != 7 || !reflect.DeepEqual(got[0], got[1]) {
		t.Errorf("current and state_version %v, then %v; want the same, with every column", got[0], got[1])
	}
	if got[0].StateVersion["xmin"] != e.xmin(t, deals, "id", "1") {
		t.Errorf("state_version %v, want the row's xmin", got[0].StateVersion)
	}
}

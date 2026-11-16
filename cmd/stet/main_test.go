package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/store"
)

// testPG is the test database's host:port; the test is skipped without it.
func testPG(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	return addr
}

// connect opens a connection to addr (the database or a proxy) as user.
func connect(t *testing.T, addr, user, password string) *pgx.Conn {
	t.Helper()
	u := url.URL{
		Scheme: "postgres", Host: addr, Path: "/stet_test",
		User: url.UserPassword(user, password), RawQuery: "sslmode=disable",
	}
	conn, err := pgx.Connect(context.Background(), u.String())
	if err != nil {
		t.Fatalf("connect to %s as %s: %v", addr, user, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func superuser(t *testing.T, pg string) *pgx.Conn {
	t.Helper()
	return connect(t, pg, "postgres", "postgres-pw")
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// freshTable creates a table owned by conn's role, with rows ids 1..n, and
// drops it when the test ends.
func freshTable(t *testing.T, conn *pgx.Conn, n int) string {
	t.Helper()
	table := fmt.Sprintf("cmd_stet_%d", time.Now().UnixNano())
	mustExec(t, conn, "create table "+table+" (id int primary key, name text)")
	t.Cleanup(func() { conn.Exec(context.Background(), "drop table "+table) })
	mustExec(t, conn, "insert into "+table+" select g, 'n' || g from generate_series(1, $1::int) g", n)
	return table
}

// stetPool is a pool of Stet's own account, with the stet schema in place.
func stetPool(t *testing.T, pg string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := openPool(ctx, "postgres://stet_scram:scram-pw@"+pg+"/stet_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.New(pool).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}

func parseConfig(t *testing.T, format string, args ...any) *config.Config {
	t.Helper()
	cfg, err := config.Parse(fmt.Appendf(nil, format, args...))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func sqlstate(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// syncBuffer is a log destination the test reads while run writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// running is run serving a config on a local port.
type running struct {
	addr   string
	log    *syncBuffer
	cancel context.CancelFunc
	done   chan struct{}
	err    error // set before done is closed
}

func start(t *testing.T, cfg *config.Config) *running {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{addr: ln.Addr().String(), log: &syncBuffer{}, cancel: cancel, done: make(chan struct{})}
	log := slog.New(slog.NewTextHandler(r.log, nil))
	go func() {
		r.err = run(ctx, cfg, ln, log)
		close(r.done)
	}()
	t.Cleanup(func() { r.stop(t) })
	return r
}

// stop cancels run's context and returns what run returned.
func (r *running) stop(t *testing.T) error {
	t.Helper()
	r.cancel()
	select {
	case <-r.done:
		return r.err
	case <-time.After(15 * time.Second):
		t.Fatalf("run did not return after its context was cancelled; log:\n%s", r.log)
		return nil
	}
}

func TestProxyCommandNamesTheMissingAgentRoles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stet.yaml")
	cfg := "mode: enforce\nlisten: 127.0.0.1:0\nupstream: postgres://stet_scram@127.0.0.1:1/stet_test\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runProxy([]string{"-config", path})
	if err == nil || !strings.Contains(err.Error(), "agents.roles") {
		t.Fatalf("runProxy = %v, want an error naming agents.roles", err)
	}
}

func TestExampleConfigIsShadowWithTheEnforceKeysCommentedOut(t *testing.T) {
	c, err := config.Load("../../stet.example.yaml")
	if err != nil {
		t.Fatalf("example config must stay valid: %v", err)
	}
	if c.Mode != config.ModeShadow || len(c.AgentRoles) != 0 || c.MaxInflight != 32 {
		t.Errorf("mode %q, agent roles %v, max_inflight %d; want shadow, none, 32", c.Mode, c.AgentRoles, c.MaxInflight)
	}

	// Uncommented, the keys make a valid enforce config.
	b, err := os.ReadFile("../../stet.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	enforce := strings.NewReplacer("mode: shadow", "mode: enforce",
		"# agents:\n#   roles:", "agents:\n  roles:",
		"# limits:\n#   max_inflight:", "limits:\n  max_inflight:").Replace(string(b))
	c, err = config.Parse([]byte(enforce))
	if err != nil {
		t.Fatalf("the example with its enforce keys uncommented: %v", err)
	}
	if c.Mode != config.ModeEnforce || len(c.AgentRoles) != 2 || c.MaxInflight != 32 {
		t.Errorf("uncommented: mode %q, agent roles %v, max_inflight %d; want enforce, two, 32", c.Mode, c.AgentRoles, c.MaxInflight)
	}
}

func TestRunWithoutTablesWarnsThatEnforceOnlyPassesThrough(t *testing.T) {
	cfg := parseConfig(t, "mode: enforce\nagents: {roles: [agent]}\nlisten: 127.0.0.1:0\nupstream: postgres://stet@127.0.0.1:1/db\n")
	r := start(t, cfg)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(r.log.String(), "listening") {
		if time.Now().After(deadline) {
			t.Fatalf("run never served; log:\n%s", r.log)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
	if log := r.log.String(); !strings.Contains(log, "level=WARN") || !strings.Contains(log, "no gated tables") {
		t.Errorf("log does not warn that nothing is enforced without tables:\n%s", log)
	}
}

func TestRunServesInEnforceMode(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	admin := connect(t, pg, "stet_scram", "scram-pw")
	table := freshTable(t, admin, 5)
	mustExec(t, superuser(t, pg), "grant stet_md5 to stet_scram") // so Stet can measure as stet_md5
	mustExec(t, admin, "grant all on "+table+" to stet_md5")
	t.Cleanup(func() { admin.Exec(ctx, "delete from stet.held_writes where target_table = $1", table) })

	r := start(t, parseConfig(t, `mode: enforce
agents: {roles: [stet_md5]}
listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/stet_test?sslmode=disable
model: {adapter: none, on_failure: pass}
tables:
  %[2]s:
    pk: id
    send_fields: [name]
    impact: {max_rows: 3}
`, pg, table))
	agent := connect(t, r.addr, "stet_md5", "md5-pw")

	if _, err := agent.Exec(ctx, "update "+table+" set name = 'one' where id = 1"); err != nil {
		t.Fatalf("single-row update: %v; log:\n%s", err, r.log)
	}
	_, err := agent.Exec(ctx, "update "+table+" set name = 'all'")
	if got := sqlstate(err); got != "ST001" {
		t.Fatalf("update of every row: %q, want ST001; log:\n%s", got, r.log)
	}
	var one string
	var all int
	if err := admin.QueryRow(ctx, "select (select name from "+table+" where id = 1), "+
		"(select count(*) from "+table+" where name = 'all')").Scan(&one, &all); err != nil {
		t.Fatal(err)
	}
	if one != "one" || all != 0 {
		t.Errorf("row 1 is %q and %d rows say 'all'; want the single-row update run and the refused one not", one, all)
	}

	if err := r.stop(t); err != nil {
		t.Fatalf("run = %v, want nil once its context is cancelled", err)
	}
}

func TestRunRecordsWhatWasInFlightAtShutdown(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	admin := connect(t, pg, "stet_scram", "scram-pw")
	table := freshTable(t, admin, 1)

	r := start(t, parseConfig(t, `listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/stet_test?sslmode=disable
model: {adapter: none, on_failure: pass}
tables:
  %[2]s:
    pk: id
    send_fields: [name]
    reads: {}
`, pg, table))
	writer := connect(t, r.addr, "stet_scram", "scram-pw")
	reader := connect(t, r.addr, "stet_scram", "scram-pw")

	if _, err := writer.Exec(ctx, "select * from "+table); err != nil {
		t.Fatal(err)
	}
	// Two statements the server is still running when run is stopped: a
	// write, whose decision the gate records once the proxy reports it lost,
	// and a read, which the gate counts only if the proxy reports it before
	// the gate closes.
	write := "insert into " + table + " (id, name) select 2, 'late' from pg_sleep(2)"
	read := "select id, pg_sleep(2) from " + table
	ended := make(chan struct{}, 2)
	for conn, sql := range map[*pgx.Conn]string{writer: write, reader: read} {
		go func() {
			conn.Exec(ctx, sql) // fails once run stops
			ended <- struct{}{}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := admin.QueryRow(ctx, `select count(*) from pg_stat_activity
			where state = 'active' and query in ($1, $2)`, write, read).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of the two statements running on the server; log:\n%s", n, r.log)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := r.stop(t); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
	<-ended
	<-ended

	// Read at once: run has returned, so whatever it records is written.
	var decisions int
	var executed *bool
	if err := admin.QueryRow(ctx, `select count(*), bool_or(executed) from stet.decisions
		where target_table = $1`, table).Scan(&decisions, &executed); err != nil {
		t.Fatal(err)
	}
	if decisions != 1 || executed == nil || *executed {
		t.Errorf("%d decisions, executed %v; want the in-flight write's decision, not executed; log:\n%s",
			decisions, executed, r.log)
	}
	var statements int64
	if err := admin.QueryRow(ctx, `select coalesce(sum(statements), 0) from stet.reads
		where target_table = $1`, table).Scan(&statements); err != nil {
		t.Fatal(err)
	}
	if statements != 2 {
		t.Errorf("stet.reads counts %d reads of the table, want 2: the finished one and the one in flight; log:\n%s",
			statements, r.log)
	}
}

// A user given as a URL parameter is not seen by config validation; the
// check sees the account Stet is connected as, and run does not serve.
func TestRunRefusesToStartAsAnAgentsAccount(t *testing.T) {
	pg := testPG(t)
	admin := connect(t, pg, "stet_scram", "scram-pw")
	table := freshTable(t, admin, 1)
	r := start(t, parseConfig(t, `mode: enforce
agents: {roles: [stet_scram]}
listen: 127.0.0.1:0
upstream: postgres://%[1]s/stet_test?user=stet_scram&password=scram-pw&sslmode=disable
model: {adapter: none, on_failure: pass}
tables:
  %[2]s: {pk: id, send_fields: [name]}
`, pg, table))
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		t.Fatal("run is still running")
	}
	if r.err == nil || !strings.Contains(r.err.Error(), "agents.roles") {
		t.Fatalf("run = %v, want an error naming agents.roles", r.err)
	}
	if c, err := net.DialTimeout("tcp", r.addr, time.Second); err == nil {
		c.Close()
		t.Error("the listener is still open after run refused to start")
	}
	if strings.Contains(r.log.String(), "scram-pw") {
		t.Errorf("the log shows Stet's password:\n%s", r.log)
	}
}

// enforceConfig is an enforce config with one agent role and one gated
// table, set so that only the role's findings warn.
func enforceConfig(role, table string) *config.Config {
	return &config.Config{
		Mode:        config.ModeEnforce,
		AgentRoles:  []string{role},
		MaxInflight: 32,
		Model:       config.Model{Adapter: config.RecordOnly, OnFailure: "hold"},
		Tables:      map[string]config.Table{table: {PK: "id", SendFields: []string{"name"}, Unsupported: "reject"}},
	}
}

func TestCheckWarnsAboutAgentRoles(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	pool := stetPool(t, pg)
	admin := connect(t, pg, "stet_scram", "scram-pw")
	table := freshTable(t, admin, 1)

	cases := []struct {
		name, role string
		setup      func(t *testing.T) (table string)
		want       string // "" for no warning at all
	}{
		{"missing", "stet_no_such_role", nil, "does not exist"},
		{"superuser", "postgres", nil, "superuser"},
		{"stet schema", "stet_clear", func(t *testing.T) string {
			su := superuser(t, pg)
			mustExec(t, su, "grant usage on schema stet to stet_clear")
			t.Cleanup(func() { su.Exec(ctx, "revoke usage on schema stet from stet_clear") })
			return table
		}, "schema stet"},
		{"table owner", "stet_md5", func(t *testing.T) string {
			return freshTable(t, connect(t, pg, "stet_md5", "md5-pw"), 1)
		}, "owns"},
		{"search_path", "stet_clear", func(t *testing.T) string {
			su := superuser(t, pg)
			mustExec(t, su, "alter role stet_clear set search_path = public")
			t.Cleanup(func() { su.Exec(ctx, "alter role stet_clear reset search_path") })
			return table
		}, "search_path"},
		{"search_path in this database", "stet_clear", func(t *testing.T) string {
			su := superuser(t, pg)
			mustExec(t, su, "alter role stet_clear in database stet_test set search_path = public")
			t.Cleanup(func() { su.Exec(ctx, "alter role stet_clear in database stet_test reset search_path") })
			return table
		}, "search_path"},
		{"search_path as Stet's", "stet_clear", func(t *testing.T) string {
			su := superuser(t, pg)
			mustExec(t, su, `alter role stet_clear set search_path = "$user",PUBLIC`)
			t.Cleanup(func() { su.Exec(ctx, "alter role stet_clear reset search_path") })
			return table
		}, ""},
		{"fine", "stet_md5", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tbl := table
			if c.setup != nil {
				tbl = c.setup(t)
			}
			warnings, err := check(ctx, pool, enforceConfig(c.role, tbl))
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			t.Logf("warnings: %q", warnings)
			if c.want == "" {
				if len(warnings) != 0 {
					t.Errorf("warnings %q, want none", warnings)
				}
				return
			}
			for _, w := range warnings {
				if strings.Contains(w, c.want) && strings.Contains(w, c.role) {
					return
				}
			}
			t.Errorf("warnings %q, want one naming %s and %q", warnings, c.role, c.want)
		})
	}
}

func TestCheckRefusesStetsOwnAccount(t *testing.T) {
	pg := testPG(t)
	pool := stetPool(t, pg)
	admin := connect(t, pg, "stet_scram", "scram-pw")
	cfg := enforceConfig("stet_scram", freshTable(t, admin, 1))
	// No user in the URL, so config validation could not see it.
	cfg.Upstream = "postgres://" + pg + "/stet_test"
	_, err := check(context.Background(), pool, cfg)
	if err == nil || !strings.Contains(err.Error(), "agents.roles") {
		t.Fatalf("check = %v, want an error naming agents.roles", err)
	}
}

func TestCheckWarnsAboutPassSettings(t *testing.T) {
	pg := testPG(t)
	pool := stetPool(t, pg)
	admin := connect(t, pg, "stet_scram", "scram-pw")
	table := freshTable(t, admin, 1)
	cfg := enforceConfig("stet_md5", table)
	cfg.Model.OnFailure = "pass"
	cfg.Tables[table] = config.Table{PK: "id", SendFields: []string{"name"}, Unsupported: "pass"}

	warnings, err := check(context.Background(), pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var onFailure, unsupported int
	for _, w := range warnings {
		if strings.Contains(w, "on_failure") {
			onFailure++
		}
		if strings.Contains(w, "unsupported") && strings.Contains(w, table) {
			unsupported++
		}
	}
	if len(warnings) != 2 || onFailure != 1 || unsupported != 1 {
		t.Errorf("enforce warnings %q, want one for on_failure and one for the table's unsupported", warnings)
	}

	cfg.Mode = config.ModeShadow
	if warnings, err := check(context.Background(), pool, cfg); err != nil || len(warnings) != 0 {
		t.Errorf("shadow: warnings %q, err %v; want none", warnings, err)
	}
}

// The test server cannot have db_user_namespace turned on without a reload
// that would rename every other test's logins, so the setting is stubbed.
func TestCheckRefusesDBUserNamespaceInEnforce(t *testing.T) {
	pg := testPG(t)
	pool := stetPool(t, pg)
	admin := connect(t, pg, "stet_scram", "scram-pw")
	table := freshTable(t, admin, 1)

	real := userNamespace
	t.Cleanup(func() { userNamespace = real })
	userNamespace = func(context.Context, *pgxpool.Pool) (string, error) { return "on", nil }

	_, err := check(context.Background(), pool, enforceConfig("stet_md5", table))
	if err == nil || !strings.Contains(err.Error(), "db_user_namespace") {
		t.Errorf("enforce: check = %v, want an error naming db_user_namespace", err)
	}
	shadow := enforceConfig("stet_md5", table)
	shadow.Mode = config.ModeShadow
	if warnings, err := check(context.Background(), pool, shadow); err != nil || len(warnings) != 0 {
		t.Errorf("shadow: warnings %q, err %v; want none", warnings, err)
	}
}

func TestNewerThanParser(t *testing.T) {
	for v, want := range map[int]bool{140018: false, 170004: false, 180000: true} {
		if got := newerThanParser(v); got != want {
			t.Errorf("newerThanParser(%d) = %v, want %v", v, got, want)
		}
	}
}

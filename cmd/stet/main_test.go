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
	"slices"
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
	return connectTo(t, addr, "stet_test", user, password)
}

// connectTo is connect to database db.
func connectTo(t *testing.T, addr, db, user, password string) *pgx.Conn {
	t.Helper()
	u := url.URL{
		Scheme: "postgres", Host: addr, Path: "/" + db,
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
	return stetPoolIn(t, pg, "stet_test")
}

// cleanDatabase creates a database of the test's own, owned by Stet's
// account, in which PUBLIC has neither TEMPORARY on the database nor CREATE
// on schema public, and drops it when the test ends; it returns its name.
// The shared stet_test keeps PostgreSQL 14's defaults, which give every role
// both, and its roles have CREATE on it besides, so no agent role is free
// there of what check warns about (ruling 29); nothing is revoked from
// PUBLIC there, since the other packages' tests share it. A role whose
// rights in this database the test grants must be created before it, so
// that the database is dropped, with those rights, before the role is.
func cleanDatabase(t *testing.T, pg string) string {
	t.Helper()
	name := fmt.Sprintf("stet_chk_db_%d", time.Now().UnixNano())
	su := superuser(t, pg)
	mustExec(t, su, "create database "+name+" template template0 owner stet_scram")
	t.Cleanup(func() { su.Exec(context.Background(), "drop database if exists "+name+" with (force)") })
	in := connectTo(t, pg, name, "postgres", "postgres-pw")
	mustExec(t, in, "revoke temporary on database "+name+" from public")
	mustExec(t, in, "revoke create on schema public from public")
	mustExec(t, in, "grant create on schema public to stet_scram") // for the tests' tables
	return name
}

// stetPoolIn is stetPool in database db.
func stetPoolIn(t *testing.T, pg, db string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := openPool(ctx, "postgres://stet_scram:scram-pw@"+pg+"/"+db+"?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.New(pool).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return pool
}

// agentTable makes a database of the test's own (cleanDatabase) holding a
// table with rows ids 1..n that stet_md5 may read and change, and makes Stet's
// account a member of stet_md5, so that it can measure and apply as it. It
// returns the database's name, the table's, and a connection to the database
// as Stet's account. A run in enforce mode with gated tables applies every
// approved held write of its database, so such a run, and a test that
// approves held writes, keeps out of the shared stet_test (plan ruling P1).
func agentTable(t *testing.T, pg string, n int) (db, table string, admin *pgx.Conn) {
	t.Helper()
	db = cleanDatabase(t, pg)
	admin = connectTo(t, pg, db, "stet_scram", "scram-pw")
	table = freshTable(t, admin, n)
	mustExec(t, admin, "grant all on "+table+" to stet_md5")
	mustExec(t, superuser(t, pg), "grant stet_md5 to stet_scram") // already so on the test server; never revoked
	return db, table, admin
}

// reviewerPassword is the password of the reviewers the tests create.
const reviewerPassword = "reviewer-pw"

// newReviewer creates a login role for a reviewer. Create it before the
// database it reviews in: see cleanDatabase.
func newReviewer(t *testing.T, pg string) string {
	t.Helper()
	return createRole(t, superuser(t, pg), "reviewer", "login password '"+reviewerPassword+"'")
}

// review gives reviewer the grants of spec 6 in database db, whose stet
// schema is in place, and connects as it.
func review(t *testing.T, pg, db, reviewer string) *pgx.Conn {
	t.Helper()
	in := connectTo(t, pg, db, "postgres", "postgres-pw")
	mustExec(t, in, "grant usage on schema stet to "+reviewer)
	mustExec(t, in, "grant select on stet.review to "+reviewer)
	mustExec(t, in, "grant execute on function stet.approve(uuid, text, bigint), stet.reject(uuid, text) to "+reviewer)
	return connectTo(t, pg, db, reviewer, reviewerPassword)
}

// heldWriteOf is the held write a refusal names, failing the test unless err
// is one (ST001 with a held_write in its detail).
func heldWriteOf(t *testing.T, err error, log fmt.Stringer) string {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "ST001" {
		t.Fatalf("got %v, want a refusal with ST001; log:\n%s", err, log)
	}
	for f := range strings.FieldsSeq(pgErr.Detail) {
		if id, ok := strings.CutPrefix(f, "held_write="); ok {
			return id
		}
	}
	t.Fatalf("refusal detail %q names no held write; log:\n%s", pgErr.Detail, log)
	return ""
}

// waitStatus waits up to d for held write id to show status in stet.review
// as conn reads it.
func waitStatus(t *testing.T, conn *pgx.Conn, id, status string, d time.Duration, log fmt.Stringer) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		var got string
		err := conn.QueryRow(context.Background(), "select status from stet.review where id = $1", id).Scan(&got)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if got == status {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("held write %s is %q after %s, want %s; log:\n%s", id, got, d, status, log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// approvedWrite stores statement as a held write by stet_md5 to table in
// database db, of one row planned exactly and with the settings of a
// session at the server's defaults, and approves it as Stet's account; it
// returns its id.
func approvedWrite(t *testing.T, pool *pgxpool.Pool, db, table, statement string) string {
	t.Helper()
	ctx := context.Background()
	st := store.New(pool)
	one := int64(1)
	h := &store.HeldWrite{
		ID: store.NewID(), DecisionID: store.NewID(), CreatedAt: time.Now(),
		LoginRole: "stet_md5", Database: db, Statement: statement, TargetTable: table,
		Verdict: "hold", Reasons: []string{"model:conflict"}, ExpiresAt: time.Now().Add(time.Hour),
		Settings:    map[string]string{"TimeZone": "UTC", "DateStyle": "ISO, MDY", "IntervalStyle": "postgres"},
		PlannedRows: &one, RowsExact: true,
	}
	if err := st.InsertHeldWrite(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDelivered(ctx, h.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "select stet.approve($1)", h.ID); err != nil {
		t.Fatal(err)
	}
	return h.ID
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
	db, table, admin := agentTable(t, pg, 5)

	r := start(t, parseConfig(t, `mode: enforce
agents: {roles: [stet_md5]}
listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/%[3]s?sslmode=disable
model: {adapter: none, on_failure: pass}
tables:
  %[2]s:
    pk: id
    send_fields: [name]
    impact: {max_rows: 3}
`, pg, table, db))
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")

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

// enforceYAML is an enforce config for stet_md5, with no model, over table
// %[3]s of database %[2]s on the test server %[1]s. A test adds the table's
// further keys after it.
const enforceYAML = `mode: enforce
agents: {roles: [stet_md5]}
listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/%[2]s?sslmode=disable
model: {adapter: none, on_failure: pass}
tables:
  %[3]s:
    pk: id
    send_fields: [name]
`

// holdingConfig is enforceYAML with a policy rule, which makes every
// single-row write a question; with no model to ask, on_failure: hold holds
// it (no_model). config.Parse refuses on_failure: hold with no model, so it
// is set after.
func holdingConfig(t *testing.T, pg, db, table string) *config.Config {
	t.Helper()
	cfg := parseConfig(t, enforceYAML+"    policy: Do not clear a name.\n", pg, db, table)
	cfg.Model.OnFailure = "hold"
	return cfg
}

// extra_float_digits of 0 or less would round a binary float that Stet turns
// into text.
func TestPoolPinsDateStyleIntervalStyleAndFloatDigits(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db := cleanDatabase(t, pg)
	// The database's defaults are not the ones Stet's connections keep.
	in := connectTo(t, pg, db, "postgres", "postgres-pw")
	mustExec(t, in, "alter database "+db+" set datestyle = 'German, DMY'")
	mustExec(t, in, "alter database "+db+" set intervalstyle = 'sql_standard'")
	mustExec(t, in, "alter database "+db+" set extra_float_digits = 0")

	pool, err := openPool(ctx, "postgres://stet_scram:scram-pw@"+pg+"/"+db+"?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var dateStyle, intervalStyle, floatDigits, third string
	if err := pool.QueryRow(ctx, "show datestyle").Scan(&dateStyle); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "show intervalstyle").Scan(&intervalStyle); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "show extra_float_digits").Scan(&floatDigits); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dateStyle, "ISO") || intervalStyle != "postgres" || floatDigits != "1" {
		t.Errorf("DateStyle %q, IntervalStyle %q, extra_float_digits %q; want ISO first, postgres and 1",
			dateStyle, intervalStyle, floatDigits)
	}
	// As the gate decodes a binary value: it reads back as the same float.
	if err := pool.QueryRow(ctx, "select $1::float8::text", 1.0/3).Scan(&third); err != nil {
		t.Fatal(err)
	}
	if third != "0.3333333333333333" {
		t.Errorf("1/3 as text is %q, want 0.3333333333333333", third)
	}
}

// Review Focus 1: in one Bind, an integer in binary and an untyped date in
// text, from a session whose TimeZone and DateStyle are not Stet's. The
// approved write puts in what the agent's session would have.
func TestApprovedWriteRunsAsTheAgentSentIt(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	reviewer := newReviewer(t, pg)
	db, table, admin := agentTable(t, pg, 1)
	mustExec(t, admin, "alter table "+table+" add column at timestamptz")
	r := start(t, holdingConfig(t, pg, db, table))
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")

	for _, set := range []string{"set timezone = 'Asia/Seoul'", "set datestyle = 'ISO, DMY'"} {
		if _, err := agent.Exec(ctx, set); err != nil {
			t.Fatalf("%s: %v", set, err)
		}
	}
	res := agent.PgConn().ExecParams(ctx, "update "+table+" set at = $1, name = $2 where id = $3",
		[][]byte{[]byte("01/02/2026 10:00"), nil, {0, 0, 0, 1}}, []uint32{0, 0, 23}, []int16{0, 0, 1}, nil).Read()
	id := heldWriteOf(t, res.Err, r.log)

	rv := review(t, pg, db, reviewer)
	waitStatus(t, rv, id, "pending", 3*time.Second, r.log)
	var inTransaction *bool
	if err := rv.QueryRow(ctx, "select in_transaction from stet.review where id = $1", id).Scan(&inTransaction); err != nil {
		t.Fatal(err)
	}
	if inTransaction == nil || *inTransaction {
		t.Errorf("in_transaction is %s, want false", orNull(inTransaction))
	}
	var approved string
	if err := rv.QueryRow(ctx, "select stet.approve($1)", id).Scan(&approved); err != nil || approved != "approved" {
		t.Fatalf("stet.approve = %q, %v; want approved", approved, err)
	}
	waitStatus(t, rv, id, "applied", 3*time.Second, r.log)
	var rows *int64
	if err := rv.QueryRow(ctx, "select rows_affected from stet.review where id = $1", id).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == nil || *rows != 1 {
		t.Errorf("rows_affected is %s, want 1", orNull(rows))
	}

	var at *time.Time
	var name *string
	if err := admin.QueryRow(ctx, "select at, name from "+table+" where id = 1").Scan(&at, &name); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 2, 1, 1, 0, 0, 0, time.UTC); at == nil || !at.Equal(want) || name != nil {
		t.Errorf("row 1 has at %s and name %s, want %s and NULL", orNull(at), orNull(name), want)
	}
}

// Spec 5: the first write after BEGIN is held, the rest of its transaction
// fails, and approving the write runs only it.
func TestApprovingTheFirstWriteOfATransactionRunsOnlyIt(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	reviewer := newReviewer(t, pg)
	db, table, admin := agentTable(t, pg, 2)
	r := start(t, holdingConfig(t, pg, db, table))
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")

	if _, err := agent.Exec(ctx, "begin"); err != nil {
		t.Fatal(err)
	}
	_, err := agent.Exec(ctx, "update "+table+" set name = 'one' where id = 1")
	id := heldWriteOf(t, err, r.log)
	if _, err := agent.Exec(ctx, "update "+table+" set name = 'two' where id = 2"); err == nil {
		t.Error("the write after the refused one, in its transaction, succeeded")
	}
	if _, err := agent.Exec(ctx, "rollback"); err != nil {
		t.Fatal(err)
	}

	rv := review(t, pg, db, reviewer)
	waitStatus(t, rv, id, "pending", 3*time.Second, r.log)
	var n int
	var inTransaction *bool
	if err := rv.QueryRow(ctx, "select count(*), bool_and(in_transaction) from stet.review").Scan(&n, &inTransaction); err != nil {
		t.Fatal(err)
	}
	if n != 1 || inTransaction == nil || !*inTransaction {
		t.Errorf("stet.review has %d rows, in_transaction %s; want one, true", n, orNull(inTransaction))
	}
	if _, err := rv.Exec(ctx, "select stet.approve($1)", id); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, rv, id, "applied", 3*time.Second, r.log)
	var names string
	if err := admin.QueryRow(ctx, "select string_agg(name, ',' order by id) from "+table).Scan(&names); err != nil {
		t.Fatal(err)
	}
	if names != "one,n2" {
		t.Errorf("names are %s, want one,n2: only the held write ran", names)
	}
}

// waitListening waits until r is serving.
func waitListening(t *testing.T, r *running) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(r.log.String(), "listening") {
		if time.Now().After(deadline) {
			t.Fatalf("run never served; log:\n%s", r.log)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// orNull is *p as text, or NULL.
func orNull[T any](p *T) string {
	if p == nil {
		return "NULL"
	}
	return fmt.Sprint(*p)
}

// statusOf is held write id's status as stored.
func statusOf(t *testing.T, conn *pgx.Conn, id string) string {
	t.Helper()
	var status string
	if err := conn.QueryRow(context.Background(), "select status from stet.held_writes where id = $1", id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// Rule 6: in shadow mode Stet executes no write, an approved one included.
func TestShadowModeRunsNoApprovedWrite(t *testing.T) {
	pg := testPG(t)
	db, table, admin := agentTable(t, pg, 1)
	id := approvedWrite(t, stetPoolIn(t, pg, db), db, table, "update "+table+" set name = 'applied' where id = 1")
	r := start(t, parseConfig(t, `mode: shadow
listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/%[2]s?sslmode=disable
model: {adapter: none, on_failure: pass}
tables:
  %[3]s: {pk: id, send_fields: [name]}
`, pg, db, table))
	waitListening(t, r)
	time.Sleep(2500 * time.Millisecond)

	if got := statusOf(t, admin, id); got != "approved" {
		t.Errorf("the approved held write is %s in shadow mode, want approved; log:\n%s", got, r.log)
	}
	var name string
	if err := admin.QueryRow(context.Background(), "select name from "+table+" where id = 1").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "n1" {
		t.Errorf("row 1's name is %q, want n1: shadow mode ran the approved write", name)
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
}

// Spec 7: what an approval runs is not what the agent changed unseen.
func TestAppliedRowsDoNotCountTowardTheCumulativeLimit(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db, table, admin := agentTable(t, pg, 7)
	r := start(t, parseConfig(t, enforceYAML+"    impact: {max_rows: 3, cumulative: {max_rows: 4, window: 10m}}\n", pg, db, table))
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")

	_, err := agent.Exec(ctx, "update "+table+" set name = 'five' where id <= 5")
	id := heldWriteOf(t, err, r.log)
	waitStatus(t, admin, id, "pending", 3*time.Second, r.log)
	var reasons []string
	if err := admin.QueryRow(ctx, "select reasons from stet.held_writes where id = $1", id).Scan(&reasons); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(reasons, "impact:rows") {
		t.Fatalf("held for %v, want impact:rows", reasons)
	}
	if _, err := admin.Exec(ctx, "select stet.approve($1, null, 5)", id); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, admin, id, "applied", 3*time.Second, r.log)
	var five int
	if err := admin.QueryRow(ctx, "select count(*) from "+table+" where name = 'five'").Scan(&five); err != nil {
		t.Fatal(err)
	}
	if five != 5 {
		t.Fatalf("%d rows say five, want 5", five)
	}

	small := "update " + table + " set name = 'two' where id in (6, 7)"
	if _, err := agent.Exec(ctx, small); err != nil {
		t.Fatalf("a 2-row update after the approved one: %v, want it to pass; log:\n%s", err, r.log)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var used *string
		err := admin.QueryRow(ctx, "select cumulative->>'used' from stet.decisions where statement = $1", small).Scan(&used)
		if err == nil {
			if used == nil || *used != "0" {
				t.Errorf("the 2-row update's cumulative.used is %s, want 0", orNull(used))
			}
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the 2-row update's decision was not recorded; log:\n%s", r.log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Spec 7: on shutdown the write being applied is finished, after the proxy
// and the gate and before Stet's pool is closed.
func TestRunStopsTheApplierBeforeThePool(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db, table, admin := agentTable(t, pg, 1)
	pool := stetPoolIn(t, pg, db)
	r := start(t, parseConfig(t, enforceYAML, pg, db, table))
	statement := "update " + table + " set name = (select 'slept' from pg_sleep(1)) where id = 1"
	id := approvedWrite(t, pool, db, table, statement)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var running bool
		if err := admin.QueryRow(ctx, `select exists (select from pg_stat_activity
			where state = 'active' and query = $1)`, statement).Scan(&running); err != nil {
			t.Fatal(err)
		}
		if running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the approved write is not running; log:\n%s", r.log)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := r.stop(t); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
	if got := statusOf(t, admin, id); got != "applied" {
		t.Errorf("the held write being applied at shutdown is %s, want applied; log:\n%s", got, r.log)
	}
	if strings.Contains(r.log.String(), "level=ERROR") {
		t.Errorf("the log has an error:\n%s", r.log)
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

// It runs in a database of its own (cleanDatabase), where a role can be
// free of every finding: in stet_test each one may create objects.
func TestCheckWarnsAboutAgentRoles(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db := cleanDatabase(t, pg)
	pool := stetPoolIn(t, pg, db)
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
	table := freshTable(t, admin, 1)
	// Stet's account is a member of the agent roles whose cases expect no
	// warning at all: not being one is a warning too.
	su := superuser(t, pg)
	// (stet_md5 already is, on the test server, and stays so.)
	mustExec(t, su, "grant stet_clear, stet_md5 to stet_scram")
	t.Cleanup(func() { su.Exec(ctx, "revoke stet_clear from stet_scram") })

	cases := []struct {
		name, role string
		setup      func(t *testing.T) (table string)
		want       string // "" for no warning at all
	}{
		{"missing", "stet_no_such_role", nil, "does not exist"},
		{"superuser", "postgres", nil, "superuser"},
		{"stet schema", "stet_clear", func(t *testing.T) string {
			su := connectTo(t, pg, db, "postgres", "postgres-pw")
			mustExec(t, su, "grant usage on schema stet to stet_clear")
			t.Cleanup(func() { su.Exec(ctx, "revoke usage on schema stet from stet_clear") })
			return table
		}, "schema stet"},
		{"table owner", "stet_md5", func(t *testing.T) string {
			// stet_md5 may create no table here: it is given one.
			su := connectTo(t, pg, db, "postgres", "postgres-pw")
			owned := freshTable(t, su, 1)
			mustExec(t, su, "alter table "+owned+" owner to stet_md5")
			return owned
		}, "owns"},
		{"search_path", "stet_clear", func(t *testing.T) string {
			su := superuser(t, pg)
			mustExec(t, su, "alter role stet_clear set search_path = public")
			t.Cleanup(func() { su.Exec(ctx, "alter role stet_clear reset search_path") })
			return table
		}, "search_path"},
		{"search_path in this database", "stet_clear", func(t *testing.T) string {
			su := superuser(t, pg)
			mustExec(t, su, "alter role stet_clear in database "+db+" set search_path = public")
			t.Cleanup(func() { su.Exec(ctx, "alter role stet_clear in database "+db+" reset search_path") })
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

// It runs in a database of its own, where stet_md5 has nothing else to warn
// of (see TestCheckWarnsAboutAgentRoles).
func TestCheckWarnsAboutPassSettings(t *testing.T) {
	pg := testPG(t)
	db := cleanDatabase(t, pg)
	pool := stetPoolIn(t, pg, db)
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
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

	// The real reader first, so a misspelled setting (read as NULL) shows.
	var version int
	if err := admin.QueryRow(context.Background(), "select current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	got, err := userNamespace(context.Background(), pool)
	if version < 170000 && (err != nil || got != "off") {
		t.Fatalf("db_user_namespace read as %q (err %v) on server %d, want off", got, err, version)
	}

	real := userNamespace
	t.Cleanup(func() { userNamespace = real })
	userNamespace = func(context.Context, *pgxpool.Pool) (string, error) { return "on", nil }

	_, err = check(context.Background(), pool, enforceConfig("stet_md5", table))
	if err == nil || !strings.Contains(err.Error(), "db_user_namespace") {
		t.Errorf("enforce: check = %v, want an error naming db_user_namespace", err)
	}
	shadow := enforceConfig("stet_md5", table)
	shadow.Mode = config.ModeShadow
	if warnings, err := check(context.Background(), pool, shadow); err != nil || len(warnings) != 0 {
		t.Errorf("shadow: warnings %q, err %v; want none", warnings, err)
	}
}

// Ruling 26: under enforce, a check whose finding stops the start also
// stops it when it cannot be made; shadow warns and starts.
func TestCheckStopsEnforceWhenItCannotRead(t *testing.T) {
	pg := testPG(t)
	pool := stetPool(t, pg)
	admin := connect(t, pg, "stet_scram", "scram-pw")
	cfg := enforceConfig("stet_md5", freshTable(t, admin, 1))
	broken, cancel := context.WithCancel(context.Background())
	cancel() // every read fails, as when the check's time is up

	_, err := check(broken, pool, cfg)
	if err == nil {
		t.Fatal("enforce: check = nil with nothing read, want an error")
	}
	for _, want := range []string{"db_user_namespace could not be checked", "agents.roles: whether"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("enforce: check = %v, want it to say %q", err, want)
		}
	}

	cfg.Mode = config.ModeShadow
	warnings, err := check(broken, pool, cfg)
	if err != nil {
		t.Errorf("shadow: check = %v, want nil", err)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "could not be checked") {
		t.Errorf("shadow: warnings %q, want them to say what could not be checked", warnings)
	}
}

// The same through run, where Migrate succeeds and only the checks run out
// of time: enforce does not serve, shadow does.
func TestRunStopsEnforceButStartsShadowWhenChecksCannotRead(t *testing.T) {
	pg := testPG(t)
	admin := connect(t, pg, "stet_scram", "scram-pw")
	table := freshTable(t, admin, 1)
	was := checkTimeout
	t.Cleanup(func() { checkTimeout = was })
	checkTimeout = time.Nanosecond

	const format = `mode: %[3]s
agents: {roles: [stet_md5]}
listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/stet_test?sslmode=disable
model: {adapter: none, on_failure: pass}
tables:
  %[2]s: {pk: id, send_fields: [name]}
`
	enforce := start(t, parseConfig(t, format, pg, table, "enforce"))
	select {
	case <-enforce.done:
	case <-time.After(15 * time.Second):
		t.Fatal("enforce: run is serving with nothing checked")
	}
	if enforce.err == nil || !strings.Contains(enforce.err.Error(), "db_user_namespace could not be checked") {
		t.Errorf("enforce: run = %v, want it to refuse because db_user_namespace could not be checked", enforce.err)
	}

	shadow := start(t, parseConfig(t, format, pg, table, "shadow"))
	conn := connect(t, shadow.addr, "stet_scram", "scram-pw")
	if _, err := conn.Exec(context.Background(), "select 1"); err != nil {
		t.Fatalf("shadow: %v; log:\n%s", err, shadow.log)
	}
	if err := shadow.stop(t); err != nil {
		t.Fatalf("shadow: run = %v, want nil", err)
	}
	if !strings.Contains(shadow.log.String(), "could not be checked") {
		t.Errorf("shadow: the log does not say what could not be checked:\n%s", shadow.log)
	}
}

// createRole creates a role named for the test with attrs, and drops it and
// what it owns or was granted in this database when the test ends, also
// when it fails.
func createRole(t *testing.T, su *pgx.Conn, kind, attrs string) string {
	t.Helper()
	name := fmt.Sprintf("stet_chk_%s_%d", kind, time.Now().UnixNano())
	mustExec(t, su, "create role "+name+" "+attrs)
	t.Cleanup(func() {
		ctx := context.Background()
		su.Exec(ctx, "drop owned by "+name)
		su.Exec(ctx, "drop role "+name)
	})
	return name
}

// warningWith returns the first warning holding every one of parts.
func warningWith(warnings []string, parts ...string) (string, bool) {
	for _, w := range warnings {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(w, p)
		}
		if all {
			return w, true
		}
	}
	return "", false
}

// Rights and superuser reached through role membership count, also through
// a NOINHERIT membership, which SET ROLE still reaches; the fix names the
// membership the agent was granted.
func TestCheckFollowsRoleMembership(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	pool := stetPool(t, pg)
	su := superuser(t, pg)
	admin := connect(t, pg, "stet_scram", "scram-pw")
	table := freshTable(t, admin, 1)

	t.Run("member of a superuser", func(t *testing.T) {
		super := createRole(t, su, "super", "superuser nologin")
		agent := createRole(t, su, "agent", "login")
		mustExec(t, su, "grant "+super+" to "+agent)
		warnings, err := check(ctx, pool, enforceConfig(agent, table))
		if err != nil {
			t.Fatal(err)
		}
		fix := fmt.Sprintf(`REVOKE "%s" FROM "%s"`, super, agent)
		if _, ok := warningWith(warnings, agent, "superuser", fix); !ok {
			t.Errorf("warnings %q, want one that %s can become superuser %s, with %s", warnings, agent, super, fix)
		}
	})

	// In a database of its own: a GRANT on schema stet, and the DROP OWNED
	// that takes it back, in stet_test would race the grants internal/store's
	// tests make there under Migrate's lock ("tuple concurrently updated").
	t.Run("rights through a NOINHERIT chain", func(t *testing.T) {
		agent := createRole(t, su, "agent", "login noinherit")
		mid := createRole(t, su, "mid", "nologin")
		owner := createRole(t, su, "owner", "nologin") // before the database: see cleanDatabase
		db := cleanDatabase(t, pg)
		pool := stetPoolIn(t, pg, db)
		in := connectTo(t, pg, db, "postgres", "postgres-pw")
		mustExec(t, su, "grant "+mid+" to "+agent)
		mustExec(t, su, "grant "+owner+" to "+mid)
		mustExec(t, in, "grant usage on schema stet to "+owner)
		owned := freshTable(t, in, 1)
		mustExec(t, in, "alter table "+owned+" owner to "+owner)

		warnings, err := check(ctx, pool, enforceConfig(agent, owned))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("warnings: %q", warnings)
		fix := fmt.Sprintf(`REVOKE "%s" FROM "%s"`, mid, agent)
		if _, ok := warningWith(warnings, agent, "schema stet", "USAGE", owner, fix); !ok {
			t.Errorf("warnings %q, want one that %s has USAGE on schema stet through %s, with %s", warnings, agent, owner, fix)
		}
		if _, ok := warningWith(warnings, agent, "owns", owned, owner, fix); !ok {
			t.Errorf("warnings %q, want one that %s owns %s through %s, with %s", warnings, agent, owned, owner, fix)
		}
	})
}

// An agent role that owns a table of undo.tables is warned about as the owner
// of a gated table is: it can drop or disable the capture triggers, and its
// writes then leave no record. The check looks at undo.tables as well as
// tables, and at a table under both once.
func TestCheckWarnsWhenAnAgentOwnsAnUndoTable(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db := cleanDatabase(t, pg)
	pool := stetPoolIn(t, pg, db)
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
	gated := freshTable(t, admin, 1)
	su := connectTo(t, pg, db, "postgres", "postgres-pw")
	owned := freshTable(t, su, 1)
	both := freshTable(t, su, 1)
	mustExec(t, su, "alter table "+owned+" owner to stet_md5")
	mustExec(t, su, "alter table "+both+" owner to stet_md5")

	cfg := enforceConfig("stet_md5", gated)
	cfg.Tables[both] = cfg.Tables[gated]
	cfg.Undo = &config.Undo{Tables: map[string]string{owned: "id", both: "id", gated: "id", "public.undo_gone": "id"}, MaxRows: 10000, Keep: time.Hour}
	warnings, err := check(ctx, pool, cfg)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	t.Logf("warnings: %q", warnings)
	if _, ok := warningWith(warnings, `agent role "stet_md5" owns table `+owned+":", "capture triggers", "ALTER TABLE"); !ok {
		t.Errorf("no warning that the agent owns %s, which is under undo.tables only", owned)
	}
	n := 0
	for _, w := range warnings {
		if strings.Contains(w, "owns table "+both+":") {
			n++
		}
		if strings.Contains(w, "owns table "+gated) {
			t.Errorf("a warning that the agent owns %s, which Stet's account owns: %s", gated, w)
		}
	}
	if n != 1 {
		t.Errorf("%d warnings that the agent owns %s, which is gated and under undo.tables; want one", n, both)
	}
	if _, ok := warningWith(warnings, "public.undo_gone", "could not be checked", "undo.tables"); !ok {
		t.Errorf("no warning that the owner of public.undo_gone, which is not there, could not be checked")
	}
}

// An agent role that may create objects can write a gated table through a
// temporary view or a function it creates, which no statement Stet judges
// shows (ruling 29): enforce warns of TEMPORARY and CREATE on the database
// and of CREATE on, or ownership of, any schema but the system ones, each
// with the role it is held through and the statement that removes it.
func TestCheckWarnsAboutRightsToCreateObjects(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	su := superuser(t, pg)
	run := func(t *testing.T, pool *pgxpool.Pool, agent, table string) []string {
		t.Helper()
		warnings, err := check(ctx, pool, enforceConfig(agent, table))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("warnings: %q", warnings)
		return warnings
	}
	want := func(t *testing.T, warnings []string, parts ...string) {
		t.Helper()
		if _, ok := warningWith(warnings, parts...); !ok {
			t.Errorf("warnings %q, want one with each of %q", warnings, parts)
		}
	}

	// stet_test keeps PostgreSQL 14's defaults: TEMPORARY on the database and
	// CREATE on schema public are PUBLIC's.
	t.Run("through PUBLIC", func(t *testing.T) {
		agent := createRole(t, su, "agent", "login")
		w := run(t, stetPool(t, pg), agent, freshTable(t, connect(t, pg, "stet_scram", "scram-pw"), 1))
		want(t, w, agent, `database "stet_test"`, "TEMPORARY through PUBLIC", `REVOKE TEMPORARY ON DATABASE "stet_test" FROM PUBLIC`)
		want(t, w, agent, `CREATE on schema "public" through PUBLIC`, `REVOKE CREATE ON SCHEMA "public" FROM PUBLIC`)
	})

	t.Run("through a role", func(t *testing.T) {
		agent := createRole(t, su, "agent", "login noinherit")
		mid := createRole(t, su, "mid", "nologin")
		schema := fmt.Sprintf("stet_chk_s_%d", time.Now().UnixNano())
		mustExec(t, su, "create schema "+schema)
		t.Cleanup(func() { su.Exec(ctx, "drop schema "+schema+" cascade") })
		mustExec(t, su, "grant create on schema "+schema+" to "+mid)
		mustExec(t, su, "grant "+mid+" to "+agent)
		w := run(t, stetPool(t, pg), agent, freshTable(t, connect(t, pg, "stet_scram", "scram-pw"), 1))
		want(t, w, agent, fmt.Sprintf(`CREATE on schema "%s" through role "%s"`, schema, mid),
			fmt.Sprintf(`REVOKE "%s" FROM "%s"`, mid, agent))
	})

	// In a database whose PUBLIC holds neither right, a role's own grants
	// and schema warn, and once they are revoked nothing does.
	t.Run("granted, then revoked", func(t *testing.T) {
		agent := createRole(t, su, "agent", "login") // before the database: see cleanDatabase
		mustExec(t, su, "grant "+agent+" to stet_scram")
		db := cleanDatabase(t, pg)
		pool := stetPoolIn(t, pg, db)
		table := freshTable(t, connectTo(t, pg, db, "stet_scram", "scram-pw"), 1)
		in := connectTo(t, pg, db, "postgres", "postgres-pw")
		if w := run(t, pool, agent, table); len(w) != 0 {
			t.Errorf("before the grants: warnings %q, want none", w)
		}

		mustExec(t, in, "grant temporary, create on database "+db+" to "+agent)
		mustExec(t, in, "grant create on schema public to "+agent)
		mustExec(t, in, "create schema owned_by_agent authorization "+agent)
		w := run(t, pool, agent, table)
		revokeDB := fmt.Sprintf(`REVOKE TEMPORARY, CREATE ON DATABASE "%s" FROM "%s"`, db, agent)
		revokePublic := fmt.Sprintf(`REVOKE CREATE ON SCHEMA "public" FROM "%s"`, agent)
		want(t, w, agent, fmt.Sprintf(`database "%s"`, db), "TEMPORARY, CREATE", revokeDB)
		want(t, w, agent, `CREATE on schema "public"`, revokePublic,
			`ownership of schema "owned_by_agent"`, `ALTER SCHEMA "owned_by_agent" OWNER TO`)
		if strings.Contains(strings.Join(w, "\n"), "PUBLIC") {
			t.Errorf("warnings %q name PUBLIC, which holds neither right here", w)
		}

		mustExec(t, in, "drop schema owned_by_agent")
		mustExec(t, in, revokeDB)
		mustExec(t, in, revokePublic)
		if w := run(t, pool, agent, table); len(w) != 0 {
			t.Errorf("after the REVOKEs: warnings %q, want none", w)
		}
	})

	// The owner of a database is a member of pg_database_owner (PostgreSQL
	// 14 and later) with no row in pg_auth_members to revoke; on 15 and
	// later schema public is that role's. The fix is the database's owner.
	t.Run("through ownership of the database", func(t *testing.T) {
		agent := createRole(t, su, "agent", "login") // before the database it owns
		db := fmt.Sprintf("stet_chk_db_%d", time.Now().UnixNano())
		mustExec(t, su, "create database "+db+" template template0 owner "+agent)
		t.Cleanup(func() { su.Exec(ctx, "drop database if exists "+db+" with (force)") })
		in := connectTo(t, pg, db, "postgres", "postgres-pw")
		mustExec(t, in, "grant create on database "+db+" to stet_scram") // for the stet schema
		mustExec(t, in, "create schema dbo authorization pg_database_owner")
		w := run(t, stetPoolIn(t, pg, db), agent, freshTable(t, connectTo(t, pg, db, "stet_scram", "scram-pw"), 1))
		want(t, w, agent, `ownership of schema "dbo" through role "pg_database_owner"`, "ALTER DATABASE <this database> OWNER TO")
		if line, ok := warningWith(w, "revoke the membership"); ok {
			t.Errorf("warning %q gives a fix that cannot be carried out", line)
		}
	})
}

func TestNewerThanParser(t *testing.T) {
	for v, want := range map[int]bool{140018: false, 170004: false, 180000: true} {
		if got := newerThanParser(v); got != want {
			t.Errorf("newerThanParser(%d) = %v, want %v", v, got, want)
		}
	}
}

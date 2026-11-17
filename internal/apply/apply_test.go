package apply_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/apply"
	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/store"
)

// testPG is the test server's host:port; the test is skipped without it.
func testPG(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	return addr
}

func ptr[T any](v T) *T { return &v }

// connectTo opens a connection to database db as user, closed when the test ends.
func connectTo(t *testing.T, pg, db, user, password string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(),
		fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", user, password, pg, db))
	if err != nil {
		t.Fatalf("connect to %s as %s: %v", db, user, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// logBuffer is a log destination that Apply may write to from another goroutine.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// table is the gated table every test starts with.
const table = "contacts"

// env is one test's database (plan ruling P1: the executor takes the
// approved rows of the database it runs in, so a test does not share one):
// Stet's pool and store in it, a superuser connection to it, the gated table
// contacts with rows 1 to 3, and an Applier over them.
type env struct {
	t      *testing.T
	pg     string
	db     string
	pool   *pgxpool.Pool
	st     *store.Store
	su     *pgx.Conn
	cfg    *config.Config
	log    *logBuffer
	logger *slog.Logger
	a      *apply.Applier
}

// setup creates the test's database, owned by Stet's account, and drops it
// when the test ends. Stet's pool there has the start settings of cmd/stet's
// openPool (lock_timeout 200ms).
func setup(t *testing.T) *env {
	t.Helper()
	pg := testPG(t)
	ctx := context.Background()
	db := fmt.Sprintf("stet_apply_%d", time.Now().UnixNano())
	root := connectTo(t, pg, "stet_test", "postgres", "postgres-pw")
	if _, err := root.Exec(ctx, "create database "+db+" template template0 owner stet_scram"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := root.Exec(context.Background(), "drop database if exists "+db+" with (force)"); err != nil {
			t.Errorf("drop database %s: %v", db, err)
		}
	})
	// So Stet can become the agent's role (already so on the test server; never revoked).
	if _, err := root.Exec(ctx, "grant stet_md5 to stet_scram"); err != nil {
		t.Fatal(err)
	}

	dsn := fmt.Sprintf("postgres://stet_scram:scram-pw@%s/%s?sslmode=disable", pg, db)
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pc.ConnConfig.RuntimeParams["application_name"] = "stet"
	pc.ConnConfig.RuntimeParams["lock_timeout"] = "200ms"
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	e := &env{
		t: t, pg: pg, db: db, pool: pool, st: st,
		su:  connectTo(t, pg, db, "postgres", "postgres-pw"),
		log: &logBuffer{},
		cfg: &config.Config{
			Mode:     config.ModeEnforce,
			Upstream: dsn,
			Tables:   map[string]config.Table{},
			Held:     config.Held{ExpireAfter: 24 * time.Hour, ApplyTimeout: 30 * time.Second},
		},
	}
	e.logger = slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.exec("create table " + table + " (id int primary key, name text, at timestamptz, owner text, n int)")
	e.exec("insert into " + table + " select g, 'n' || g, null, 'stet_md5', 0 from generate_series(1, 3) g")
	e.exec("grant select, insert, update, delete on " + table + " to stet_md5")
	e.gate(table)
	e.a = e.applier()
	return e
}

// applier is a new Applier over the test's database.
func (e *env) applier() *apply.Applier {
	return apply.New(e.cfg, e.pool, e.st, e.logger)
}

// gate adds a table, keyed by id, to the gated tables.
func (e *env) gate(key string) {
	e.cfg.Tables[key] = config.Table{PK: "id"}
}

// exec runs sql as Stet's account.
func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// suExec runs sql as the superuser, in the test's database.
func (e *env) suExec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.su.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

// role creates a role named name, grants it to Stet's account, and drops it
// and its rights in the test's database when the test ends.
func (e *env) role(name string) string {
	e.t.Helper()
	id := pgx.Identifier{name}.Sanitize()
	e.suExec("create role " + id + " nologin")
	e.t.Cleanup(func() {
		ctx := context.Background()
		for _, sql := range []string{"drop owned by " + id, "drop role " + id} {
			if _, err := e.su.Exec(ctx, sql); err != nil {
				e.t.Errorf("%s: %v", sql, err)
			}
		}
	})
	e.suExec("grant " + id + " to stet_scram")
	return name
}

// held is a write by stet_md5 to contacts that Stet held: no target row,
// one row planned exactly, the settings of a session left at the server's
// defaults.
func (e *env) held(statement string) *store.HeldWrite {
	return &store.HeldWrite{
		ID:          store.NewID(),
		DecisionID:  store.NewID(),
		CreatedAt:   time.Now(),
		LoginRole:   "stet_md5",
		Database:    e.db,
		Statement:   statement,
		TargetTable: table,
		Verdict:     "hold",
		Reasons:     []string{"model:conflict"},
		ExpiresAt:   time.Now().Add(time.Hour),
		Settings:    map[string]string{"TimeZone": "UTC", "DateStyle": "ISO, MDY", "IntervalStyle": "postgres"},
		PlannedRows: ptr(int64(1)),
		RowsExact:   true,
	}
}

// update is a held single-row write to row id of contacts, with the row's
// version as it is now, or row_absent when there is no such row.
func (e *env) update(statement string, id int) *store.HeldWrite {
	h := e.held(statement)
	h.TargetPK = ptr(strconv.Itoa(id))
	h.RowVersion = e.version(table, id)
	h.RowAbsent = h.RowVersion == nil
	return h
}

// version is the xmin of row id of tbl (as SQL names it), or nil.
func (e *env) version(tbl string, id int) *string {
	e.t.Helper()
	var v *string
	err := e.pool.QueryRow(context.Background(), "select xmin::text from "+tbl+" where id = $1", id).Scan(&v)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		e.t.Fatal(err)
	}
	return v
}

// pending stores h and marks it delivered.
func (e *env) pending(h *store.HeldWrite) {
	e.t.Helper()
	ctx := context.Background()
	if err := e.st.InsertHeldWrite(ctx, h); err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.MarkDelivered(ctx, h.ID, false); err != nil {
		e.t.Fatal(err)
	}
}

// approved stores h, marks it delivered and approves it from Stet's pool,
// with max_rows when maxRows is not nil. It returns h as the listing names it.
func (e *env) approved(h *store.HeldWrite, maxRows *int64) store.Approved {
	e.t.Helper()
	e.pending(h)
	e.exec("select stet.approve($1, null, $2)", h.ID, maxRows)
	return e.listed(h.ID)
}

// forceApproved stores h and sets it approved by SQL, as stet.approve would
// not: resolved_at is the SQL expression at, and max_rows is left NULL.
func (e *env) forceApproved(h *store.HeldWrite, at string) store.Approved {
	e.t.Helper()
	e.pending(h)
	e.exec(`update stet.held_writes set status = 'approved', resolved_by = 'test', resolved_at = `+at+` where id = $1`, h.ID)
	return e.listed(h.ID)
}

// listed is held write id as the listing of approved held writes names it.
func (e *env) listed(id string) store.Approved {
	e.t.Helper()
	all, err := e.st.ApprovedWrites(context.Background(), 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, w := range all {
		if w.ID == id {
			return w
		}
	}
	e.t.Fatalf("held write %s is not listed as approved", id)
	return store.Approved{}
}

// apply runs w with e's Applier and fails the test on an error.
func (e *env) apply(w store.Approved) {
	e.t.Helper()
	if err := e.a.Apply(w); err != nil {
		e.t.Fatalf("Apply(%s) = %v, want nil; log:\n%s", w.ID, err, e.log)
	}
}

// heldRow is what the executor writes to a row of stet.held_writes.
type heldRow struct {
	Status       string
	AppliedAt    *time.Time
	RowsAffected *int64
	Error        *string
}

func (e *env) row(id string) heldRow {
	e.t.Helper()
	var r heldRow
	err := e.pool.QueryRow(context.Background(),
		`select status, applied_at, rows_affected, error from stet.held_writes where id = $1`, id).
		Scan(&r.Status, &r.AppliedAt, &r.RowsAffected, &r.Error)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func deref[T any](p *T) any {
	if p == nil {
		return "NULL"
	}
	return *p
}

func (r heldRow) String() string {
	return fmt.Sprintf("{status %s, applied_at %v, rows_affected %v, error %v}",
		r.Status, deref(r.AppliedAt), deref(r.RowsAffected), deref(r.Error))
}

func (e *env) wantApplied(id string, rows int64) {
	e.t.Helper()
	r := e.row(id)
	if r.Status != "applied" || r.RowsAffected == nil || *r.RowsAffected != rows || r.AppliedAt == nil || r.Error != nil {
		e.t.Errorf("held write %s: status %s, rows_affected %v, applied_at %v, error %v; want applied, %d rows, a time, no error; log:\n%s",
			id, r.Status, deref(r.RowsAffected), deref(r.AppliedAt), deref(r.Error), rows, e.log)
	}
}

// wantFailed checks that held write id failed with an error starting with
// reason (the whole error when exact).
func (e *env) wantFailed(id, reason string, exact bool) {
	e.t.Helper()
	r := e.row(id)
	ok := r.Status == "failed" && r.Error != nil && r.AppliedAt != nil && r.RowsAffected == nil
	if ok && exact {
		ok = *r.Error == reason
	} else if ok {
		ok = strings.HasPrefix(*r.Error, reason)
	}
	if !ok {
		e.t.Errorf("held write %s: status %s, error %v, applied_at %v, rows_affected %v; want failed with %q (exact %v); log:\n%s",
			id, r.Status, deref(r.Error), deref(r.AppliedAt), deref(r.RowsAffected), reason, exact, e.log)
	}
}

// contents is every row of tbl as text, to tell whether a table changed.
func (e *env) contents(tbl string) string {
	e.t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(),
		"select coalesce(string_agg(t::text, ';' order by t.id), '') from "+tbl+" t").Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) wantUnchanged(tbl, before string) {
	e.t.Helper()
	if got := e.contents(tbl); got != before {
		e.t.Errorf("%s changed:\n got %s\nwant %s", tbl, got, before)
	}
}

// name is the name of row id of contacts.
func (e *env) name(id int) *string {
	e.t.Helper()
	var n *string
	if err := e.pool.QueryRow(context.Background(), "select name from "+table+" where id = $1", id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) wantName(id int, want string) {
	e.t.Helper()
	if got := e.name(id); got == nil || *got != want {
		e.t.Errorf("row %d's name is %v, want %q", id, deref(got), want)
	}
}

// logged reports whether one line of the log holds every one of parts.
func (e *env) logged(parts ...string) bool {
	for line := range strings.SplitSeq(e.log.String(), "\n") {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			return true
		}
	}
	return false
}

// gone waits until no session of the test's database runs statement, so
// that what an abandoned transaction did can no longer be committed.
func (e *env) gone(statement string) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := e.su.QueryRow(context.Background(), `select count(*) from pg_stat_activity
			where datname = current_database() and query = $1 and pid <> pg_backend_pid()`, statement).Scan(&n); err != nil {
			e.t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("a session still runs %q", statement)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestApplyRunsAnApprovedUpdate(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	plain := e.update("update contacts set name = 'one' where id = 1", 1)
	// The decision that held it: the executor neither adds to stet.decisions
	// nor changes this record (rule 3).
	if err := e.st.Insert(ctx, &store.Decision{
		ID: plain.DecisionID, CreatedAt: time.Now(), Mode: "enforce", AgentID: "stet_md5", SessionID: "s",
		Statement: plain.Statement, StatementFP: "fp", Op: "update", TargetTable: table, Model: "none",
		PolicyVersion: "p", Verdict: "hold", Reasons: plain.Reasons, Refused: ptr("ST001"),
	}); err != nil {
		t.Fatal(err)
	}
	decisions := func() string {
		var s string
		if err := e.pool.QueryRow(ctx, `select count(*)::text || ' ' || coalesce(string_agg(d::text, ';'), '')
			from stet.decisions d`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := decisions()

	e.apply(e.approved(plain, nil))
	e.wantApplied(plain.ID, 1)
	e.wantName(1, "one")

	returning := e.update("update contacts set name = $1 where id = $2 returning *", 2)
	returning.Params, returning.ParamTypes = []*string{ptr("two"), ptr("2")}, []uint32{25, 23}
	e.apply(e.approved(returning, nil))
	e.wantApplied(returning.ID, 1)
	e.wantName(2, "two")

	if after := decisions(); after != before {
		t.Errorf("stet.decisions changed:\n got %s\nwant %s", after, before)
	}
	for _, id := range []string{plain.ID, returning.ID} {
		if !e.logged("level=INFO", `msg="held write applied"`, "id="+id, "login_role=stet_md5", "table=contacts", "rows=1") {
			t.Errorf("no info line for applied held write %s; log:\n%s", id, e.log)
		}
	}
}

func TestApplySendsValuesAsStored(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, types := range [][]uint32{{25, 0, 0}, nil} {
		e.exec("update contacts set name = 'before', owner = 'before' where id = 2")
		h := e.update("update contacts set name = $1, owner = $2 where id = $3", 2)
		h.Params, h.ParamTypes = []*string{ptr("CTO"), nil, ptr("2")}, types
		e.apply(e.approved(h, nil))
		e.wantApplied(h.ID, 1)
		var name, owner *string
		if err := e.pool.QueryRow(ctx, "select name, owner from contacts where id = 2").Scan(&name, &owner); err != nil {
			t.Fatal(err)
		}
		if name == nil || *name != "CTO" || owner != nil {
			t.Errorf("param_types %v: name %v, owner %v; want CTO and NULL", types, deref(name), deref(owner))
		}
	}
}

// An empty text value is sent as the empty string, not as NULL, with and
// without a type.
func TestApplySendsAnEmptyValueAsEmpty(t *testing.T) {
	e := setup(t)
	for _, types := range [][]uint32{{25, 23}, nil} {
		e.exec("update contacts set name = 'before' where id = 2")
		h := e.update("update contacts set name = $1 where id = $2", 2)
		h.Params, h.ParamTypes = []*string{ptr(""), ptr("2")}, types
		e.apply(e.approved(h, nil))
		e.wantApplied(h.ID, 1)
		if got := e.name(2); got == nil || *got != "" {
			t.Errorf("param_types %v: name %v, want ''", types, deref(got))
		}
	}
}

// Addendum A: a row whose settings are NULL, lack one of the three, or hold
// an empty one is not run under Stet's own settings in their place.
func TestApplyRefusesARowWithoutItsSettings(t *testing.T) {
	e := setup(t)
	before := e.contents(table)
	for what, settings := range map[string]map[string]string{
		"NULL":               nil,
		"no IntervalStyle":   {"TimeZone": "UTC", "DateStyle": "ISO, MDY"},
		"an empty DateStyle": {"TimeZone": "UTC", "DateStyle": "", "IntervalStyle": "postgres"},
		"an empty TimeZone":  {"TimeZone": "", "DateStyle": "ISO, MDY", "IntervalStyle": "postgres"},
	} {
		h := e.update("update contacts set name = 'x' where id = 1", 1)
		h.Settings = settings
		e.apply(e.approved(h, nil))
		if r := e.row(h.ID); r.Status != "failed" || r.Error == nil || *r.Error != "settings_missing" {
			t.Errorf("settings %s: status %s, error %v; want failed with settings_missing", what, r.Status, deref(r.Error))
		}
	}
	e.wantUnchanged(table, before)
}

// Addendum B: a row version, or row_absent, without the key of the row it
// was read from cannot be checked, so the row is taken to have changed.
func TestApplyRefusesARowVersionWithoutItsKey(t *testing.T) {
	e := setup(t)
	before := e.contents(table)

	versioned := e.update("update contacts set name = 'x' where id = 1", 1)
	versioned.TargetPK = nil
	e.apply(e.approved(versioned, nil))
	e.wantFailed(versioned.ID, "row_changed", true)

	absent := e.update("update contacts set name = 'x' where id = 9", 9)
	if !absent.RowAbsent {
		t.Fatal("row 9 exists")
	}
	absent.TargetPK = nil
	e.apply(e.approved(absent, nil))
	e.wantFailed(absent.ID, "row_changed", true)

	e.wantUnchanged(table, before)
}

// A row with a row version needs the REPEATABLE READ transaction the
// listing announces; told otherwise, Apply runs nothing.
func TestApplyNeedsTheSnapshotItsRowVersionDoes(t *testing.T) {
	e := setup(t)
	before := e.contents(table)
	h := e.update("update contacts set name = 'x' where id = 1", 1)
	w := e.approved(h, nil)
	if !w.Snapshot {
		t.Fatal("a row with a row version is listed as needing no snapshot")
	}
	if err := e.a.Apply(store.Approved{ID: w.ID, Snapshot: false}); err == nil {
		t.Error("Apply without the snapshot = nil, want an error")
	}
	if r := e.row(h.ID); r.Status != "approved" || r.AppliedAt != nil || r.Error != nil {
		t.Errorf("row is now %+v, want it still approved", r)
	}
	e.wantUnchanged(table, before)
}

// Apply's bound passes while the statement runs: a failure that is not the
// server's, so the row stays approved and nothing it did is kept. The error
// names the step it came from (addendum D).
func TestApplyLeavesTheRowApprovedWhenItsBoundPasses(t *testing.T) {
	e := setup(t)
	e.cfg.Held.ApplyTimeout = time.Second
	t.Cleanup(apply.SetGrace(-500 * time.Millisecond)) // a bound of 500ms, before the server's 1s
	before := e.contents(table)
	h := e.update("update contacts set name = (select 'slept' from pg_sleep(2)) where id = 1", 1)
	w := e.approved(h, nil)

	start := time.Now()
	err := e.a.Apply(w)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Errorf("Apply took %s, want about its bound of 500ms", d)
	}
	var pgErr *pgconn.PgError
	if err == nil || errors.As(err, &pgErr) {
		t.Fatalf("Apply = %v, want an error that is not the server's", err)
	}
	if want := "apply held write " + w.ID + ": statement: "; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Apply = %q, want it to start with %q", err, want)
	}
	e.gone(h.Statement)
	if r := e.row(h.ID); r.Status != "approved" || r.AppliedAt != nil || r.Error != nil {
		t.Errorf("row is now %+v, want it still approved", r)
	}
	e.wantUnchanged(table, before)
}

func TestApplyRunsWithTheAgentsRights(t *testing.T) {
	e := setup(t)

	// stet_md5 may read the table but not update it.
	e.exec("create table closed (id int primary key, name text)")
	e.exec("insert into closed values (1, 'kept')")
	e.exec("grant select on closed to stet_md5")
	e.gate("closed")
	before := e.contents("closed")
	h := e.held("update closed set name = 'x' where id = 1")
	h.TargetTable, h.TargetPK, h.RowVersion = "closed", ptr("1"), e.version("closed", 1)
	e.apply(e.approved(h, nil))
	e.wantFailed(h.ID, "SQLSTATE 42501: ", false)
	e.wantUnchanged("closed", before)

	// Row-level security applies to stet_md5, not to Stet's account (the owner).
	e.exec("create table shared (id int primary key, name text, owner text)")
	e.exec("insert into shared values (1, 'a', 'stet_md5'), (2, 'b', 'other'), (3, 'c', 'stet_md5'), (4, 'd', 'other')")
	e.exec("alter table shared enable row level security")
	e.exec("create policy own on shared using (owner = current_user)")
	e.exec("grant select, update on shared to stet_md5")
	e.gate("shared")
	all := e.held("update shared set name = 'x'")
	all.TargetTable, all.PlannedRows, all.RowsExact = "shared", nil, false
	e.apply(e.approved(all, ptr(int64(10))))
	e.wantApplied(all.ID, 2)
	if got, want := e.contents("shared"), "(1,x,stet_md5);(2,b,other);(3,x,stet_md5);(4,d,other)"; got != want {
		t.Errorf("shared is %s, want %s", got, want)
	}
}

// Review Focus 4, correction B6: SET LOCAL ROLE fails, the row is failed
// with the server's error, and the executor goes on.
func TestApplyFailsWhenStetCannotBecomeTheRole(t *testing.T) {
	e := setup(t)
	before := e.contents(table)
	h := e.update("update contacts set name = 'x' where id = 1", 1)
	h.LoginRole = "stet_trust" // Stet's account is not a member of it
	e.apply(e.approved(h, nil))
	e.wantFailed(h.ID, "SQLSTATE 42501: ", false)
	e.wantUnchanged(table, before)
	if !e.logged("level=WARN", `msg="held write failed"`, "id="+h.ID, "login_role=stet_trust", "table=contacts", `error="SQLSTATE 42501: `) {
		t.Errorf("no warn line for failed held write %s; log:\n%s", h.ID, e.log)
	}

	next := e.update("update contacts set name = 'next' where id = 2", 2)
	e.apply(e.approved(next, nil))
	e.wantApplied(next.ID, 1)
	e.wantName(2, "next")
}

// An untyped text value is read under the agent session's DateStyle and
// TimeZone, not under Stet's ("ISO, MDY" would read 2 January).
func TestApplyReadsValuesUnderTheAgentsSettings(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	for _, c := range []struct {
		id   int
		zone string
		want time.Time
	}{
		{1, "UTC", time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)},
		{2, "Asia/Seoul", time.Date(2026, 2, 1, 1, 0, 0, 0, time.UTC)},
	} {
		h := e.update("update contacts set at = $1 where id = $2", c.id)
		h.Params = []*string{ptr("01/02/2026 10:00"), ptr(strconv.Itoa(c.id))}
		h.Settings = map[string]string{"TimeZone": c.zone, "DateStyle": "ISO, DMY", "IntervalStyle": "postgres"}
		e.apply(e.approved(h, nil))
		e.wantApplied(h.ID, 1)
		var at time.Time
		if err := e.pool.QueryRow(ctx, "select at from contacts where id = $1", c.id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if !at.Equal(c.want) {
			t.Errorf("TimeZone %s: at = %s, want %s", c.zone, at.UTC(), c.want)
		}
	}
}

func TestApplyChecksTheRowVersion(t *testing.T) {
	e := setup(t)

	// (a) The row changed after the write was held.
	changed := e.update("update contacts set name = 'stet' where id = 1", 1)
	w := e.approved(changed, nil)
	e.exec("update contacts set name = 'changed' where id = 1")
	e.apply(w)
	e.wantFailed(changed.ID, "row_changed", true)
	e.wantName(1, "changed")

	// (b) The row changes after the check, before the statement: the
	// snapshot's write fails with 40001.
	raced := e.update("update contacts set name = 'stet' where id = 2", 2)
	w = e.approved(raced, nil)
	apply.BeforeStatement(e.a, func() { e.exec("update contacts set name = 'raced' where id = 2") })
	e.apply(w)
	apply.BeforeStatement(e.a, nil)
	e.wantFailed(raced.ID, "row_changed", true)
	e.wantName(2, "raced")

	// (c) The row is gone: the write runs and touches nothing.
	gone := e.update("update contacts set name = 'stet' where id = 3", 3)
	w = e.approved(gone, nil)
	e.exec("delete from contacts where id = 3")
	e.apply(w)
	e.wantApplied(gone.ID, 0)

	// (d) There was no row when it was held, and now there is.
	appeared := e.update("update contacts set name = 'stet' where id = 9", 9)
	if !appeared.RowAbsent {
		t.Fatal("row 9 exists")
	}
	w = e.approved(appeared, nil)
	e.exec("insert into contacts (id, name) values (9, 'new')")
	e.apply(w)
	e.wantFailed(appeared.ID, "row_changed", true)
	e.wantName(9, "new")

	// (e) There was no row, and there still is none.
	absent := e.update("update contacts set name = 'stet' where id = 10", 10)
	e.apply(e.approved(absent, nil))
	e.wantApplied(absent.ID, 0)
}

func TestApplyRefusesAnotherSearchPath(t *testing.T) {
	e := setup(t)
	role := e.role(fmt.Sprintf("stet_apply_path_%d", time.Now().UnixNano()))
	e.exec("grant select, update on contacts to " + role)
	before := e.contents(table)
	held := func() store.Approved {
		h := e.update("update contacts set name = 'path' where id = 1", 1)
		h.LoginRole = role
		w := e.approved(h, nil)
		e.apply(w)
		return w
	}

	e.suExec("alter role " + role + " set search_path = other_schema")
	w := held()
	e.wantFailed(w.ID, "search_path_differs", true)
	e.wantUnchanged(table, before)

	e.suExec("alter role " + role + " reset search_path")
	e.suExec("alter role " + role + " in database " + e.db + " set search_path = other_schema")
	w = held()
	e.wantFailed(w.ID, "search_path_differs", true)
	e.wantUnchanged(table, before)

	// The setting for this database wins over the one for all, and it is
	// Stet's own, however it is spelled.
	e.suExec("alter role " + role + " set search_path = other_schema")
	e.suExec("alter role " + role + " in database " + e.db + ` set search_path = "$user",PUBLIC`)
	w = held()
	e.wantApplied(w.ID, 1)
	e.wantName(1, "path")
}

func TestApplyKeepsToTheApprovedRows(t *testing.T) {
	e := setup(t)
	const statement = "update contacts set n = n + 1 where id <= 3"
	before := e.contents(table)

	two := e.held(statement)
	two.PlannedRows = ptr(int64(2))
	e.apply(e.approved(two, nil))
	e.wantFailed(two.ID, "rows_exceeded: 3 rows, approved up to 2", true)
	e.wantUnchanged(table, before)

	five := e.held(statement)
	five.PlannedRows = ptr(int64(2))
	e.apply(e.approved(five, ptr(int64(5))))
	e.wantApplied(five.ID, 3)
	after := e.contents(table)
	if want := "(1,n1,,stet_md5,1);(2,n2,,stet_md5,1);(3,n3,,stet_md5,1)"; after != want {
		t.Errorf("contacts is %s, want %s", after, want)
	}

	// Correction B5: an approved row with neither max_rows nor planned_rows
	// (stet.approve would not have approved it) is not run.
	unbounded := e.held(statement)
	unbounded.PlannedRows, unbounded.RowsExact = nil, false
	e.apply(e.forceApproved(unbounded, "clock_timestamp()"))
	e.wantFailed(unbounded.ID, "no_row_limit", true)
	e.wantUnchanged(table, after)
}

// Spec 11: a statement that changes the role is undone, and the executor
// goes on with a connection of its own for the next row.
func TestApplyUndoesAWriteThatChangesRole(t *testing.T) {
	e := setup(t)
	other := e.role(fmt.Sprintf("stet_apply_other_%d", time.Now().UnixNano()))
	before := e.contents(table)
	h := e.update(fmt.Sprintf("update contacts set name = set_config('role', '%s', false) where id = 1", other), 1)
	e.apply(e.approved(h, nil))
	e.wantFailed(h.ID, "role_changed", true)
	e.wantUnchanged(table, before)

	next := e.update("update contacts set name = 'next' where id = 2", 2)
	e.apply(e.approved(next, nil))
	e.wantApplied(next.ID, 1)
	e.wantName(2, "next")
}

func TestApplyRecordsTheServersError(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	before := e.contents(table)
	var failed []store.Approved

	unique := e.held("insert into contacts (id, name) values (1, 'dup')")
	w := e.approved(unique, nil)
	e.apply(w)
	e.wantFailed(unique.ID, "SQLSTATE 23505: ", false)
	failed = append(failed, w)

	e.cfg.Held.ApplyTimeout = time.Second
	slow := e.update("update contacts set name = (select 'slept' from pg_sleep(3)) where id = 1", 1)
	w = e.approved(slow, nil)
	e.apply(w)
	e.wantFailed(slow.ID, "SQLSTATE 57014: canceling statement due to statement timeout", true)
	failed = append(failed, w)

	// Another session holds the row. The wait ends at held.apply_timeout,
	// which is both statement_timeout and lock_timeout: the statement's
	// timer started first, so the server reports 57014, not 55P03.
	locker := connectTo(t, e.pg, e.db, "postgres", "postgres-pw")
	lock, err := locker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, "select * from contacts where id = 2 for update"); err != nil {
		t.Fatal(err)
	}
	locked := e.update("update contacts set name = 'locked' where id = 2", 2)
	w = e.approved(locked, nil)
	e.apply(w)
	lock.Rollback(ctx)
	e.wantFailed(locked.ID, "SQLSTATE 57014: canceling statement due to statement timeout", true)
	failed = append(failed, w)
	e.wantUnchanged(table, before)

	// A deferred constraint is checked with the statement, as the agent's
	// role, not at the commit that would leave the row approved.
	e.exec("create table deferred (id int primary key, name text unique deferrable initially deferred)")
	e.exec("insert into deferred values (1, 'a'), (2, 'b')")
	e.exec("grant select, update on deferred to stet_md5")
	e.gate("deferred")
	deferredBefore := e.contents("deferred")
	dup := e.held("update deferred set name = 'a' where id = 2")
	dup.TargetTable = "deferred"
	w = e.approved(dup, nil)
	e.apply(w)
	e.wantFailed(dup.ID, "SQLSTATE 23505: ", false)
	failed = append(failed, w)
	e.wantUnchanged("deferred", deferredBefore)

	// failed is final: applying again runs nothing.
	for _, w := range failed {
		was := e.row(w.ID)
		e.apply(w)
		if now := e.row(w.ID); now.Status != "failed" || *now.Error != *was.Error || !now.AppliedAt.Equal(*was.AppliedAt) {
			t.Errorf("held write %s changed when applied again: %+v, was %+v", w.ID, now, was)
		}
	}
	e.wantUnchanged(table, before)
	e.wantUnchanged("deferred", deferredBefore)
}

func TestApplyLeavesOtherStatusesAlone(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	before := e.contents(table)
	statement := "update contacts set name = 'no' where id = 1"

	cases := []struct {
		status string // as stored
		setup  func(h *store.HeldWrite)
	}{
		{"rejected", func(h *store.HeldWrite) {
			e.pending(h)
			e.exec("select stet.reject($1)", h.ID)
		}},
		{"pending", func(h *store.HeldWrite) { // expired
			h.ExpiresAt = time.Now().Add(-time.Minute)
			e.pending(h)
		}},
		{"pending", e.pending},
		{"undelivered", func(h *store.HeldWrite) {
			if err := e.st.InsertHeldWrite(ctx, h); err != nil {
				t.Fatal(err)
			}
			if err := e.st.MarkUndelivered(ctx, h.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"unconfirmed", func(h *store.HeldWrite) {
			if err := e.st.InsertHeldWrite(ctx, h); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		h := e.update(statement, 1)
		c.setup(h)
		if err := e.a.Apply(store.Approved{ID: h.ID, Snapshot: true}); err != nil {
			t.Errorf("%s: Apply = %v, want nil", c.status, err)
		}
		if r := e.row(h.ID); r.Status != c.status || r.AppliedAt != nil || r.Error != nil || r.RowsAffected != nil {
			t.Errorf("%s: row is now %+v", c.status, r)
		}
	}
	e.wantUnchanged(table, before)

	lapsed := e.update(statement, 1)
	e.apply(e.forceApproved(lapsed, "clock_timestamp() - interval '11 minutes'"))
	e.wantFailed(lapsed.ID, "approval_lapsed", true)
	e.wantUnchanged(table, before)
}

func TestApplyChecksTheDatabaseAndTheTable(t *testing.T) {
	e := setup(t)
	before := e.contents(table)
	elsewhere := e.update("update contacts set name = 'x' where id = 1", 1)
	elsewhere.Database = "another_database"
	e.apply(e.approved(elsewhere, nil))
	e.wantFailed(elsewhere.ID, "other_database", true)
	e.wantUnchanged(table, before)

	e.exec("create table ungated (id int primary key, name text)")
	e.exec("insert into ungated values (1, 'kept')")
	e.exec("grant select, update on ungated to stet_md5")
	ungatedBefore := e.contents("ungated")
	h := e.held("update ungated set name = 'x' where id = 1")
	h.TargetTable = "ungated"
	e.apply(e.approved(h, nil))
	e.wantFailed(h.ID, "not_gated", true)
	e.wantUnchanged("ungated", ungatedBefore)
}

func TestTwoAppliersRunAWriteOnce(t *testing.T) {
	e := setup(t)
	h := e.update("update contacts set n = n + 1 where id = 1", 1)
	w := e.approved(h, nil)

	first := e.applier()
	entered, release := make(chan struct{}), make(chan struct{})
	apply.BeforeStatement(first, func() {
		close(entered)
		<-release
	})
	done := make(chan error, 1)
	go func() { done <- first.Apply(w) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("the first Apply returned %v before its statement; log:\n%s", err, e.log)
	case <-time.After(10 * time.Second):
		t.Fatal("the first Apply never reached its statement")
	}

	start := time.Now()
	if err := e.applier().Apply(w); err != nil {
		t.Errorf("the second Apply = %v, want nil", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the second Apply took %s: it waited for the first", d)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the first Apply = %v; log:\n%s", err, e.log)
	}
	e.apply(w)

	var n int
	if err := e.pool.QueryRow(context.Background(), "select n from contacts where id = 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1: the write ran once", n)
	}
	e.wantApplied(h.ID, 1)
}

func TestApplyLeavesTheRowApprovedWhenItsOwnStepFails(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	n := func(id int) int {
		var n int
		if err := e.pool.QueryRow(ctx, "select n from contacts where id = $1", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	h := e.update("update contacts set n = n + 1 where id = 1", 1)
	w := e.approved(h, nil)
	apply.FailMarks(e.a, 1)
	if err := e.a.Apply(w); err == nil {
		t.Fatal("Apply = nil while MarkApplied failed, want an error")
	}
	if r := e.row(h.ID); r.Status != "approved" || n(1) != 0 {
		t.Fatalf("after a failed MarkApplied: row %+v, n = %d; want approved and 0", r, n(1))
	}
	e.apply(w)
	e.wantApplied(h.ID, 1)
	if got := n(1); got != 1 {
		t.Errorf("n = %d, want 1: the write ran once", got)
	}

	// The row version cannot be read within Stet's lock_timeout.
	locked := e.update("update contacts set n = n + 1 where id = 2", 2)
	w = e.approved(locked, nil)
	locker := connectTo(t, e.pg, e.db, "postgres", "postgres-pw")
	lock, err := locker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, "lock table contacts in access exclusive mode"); err != nil {
		t.Fatal(err)
	}
	err = e.a.Apply(w)
	lock.Rollback(ctx)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Errorf("Apply under an ACCESS EXCLUSIVE lock = %v, want an error carrying 55P03", err)
	}
	if r := e.row(locked.ID); r.Status != "approved" || n(2) != 0 {
		t.Errorf("after the lock: row %+v, n = %d; want approved and 0", r, n(2))
	}
}

// Review Focus 5: a role whose name needs quoting and a table keyed with
// its schema.
func TestApplyQuotesRoleAndTableNames(t *testing.T) {
	e := setup(t)
	role := e.role(fmt.Sprintf("Stet_Apply_Mixed_%d", time.Now().UnixNano()))
	id := pgx.Identifier{role}.Sanitize()
	e.exec("create schema crm")
	e.exec("create table crm.contacts (id int primary key, name text)")
	e.exec("insert into crm.contacts values (1, 'before')")
	e.exec("grant usage on schema crm to " + id)
	e.exec("grant select, update on crm.contacts to " + id)
	e.gate("crm.contacts")

	h := e.held("update crm.contacts set name = 'quoted' where id = 1")
	h.LoginRole, h.TargetTable, h.TargetPK, h.RowVersion = role, "crm.contacts", ptr("1"), e.version("crm.contacts", 1)
	e.apply(e.approved(h, nil))
	e.wantApplied(h.ID, 1)
	if got := e.contents("crm.contacts"); got != "(1,quoted)" {
		t.Errorf("crm.contacts is %s, want (1,quoted)", got)
	}
}

func TestSchemaNames(t *testing.T) {
	for path, want := range map[string][]string{
		`"$user", public`:    {"$user", "public"},
		`"$user",PUBLIC`:     {"$user", "public"},
		`Other_Schema`:       {"other_schema"},
		`"Mixed", "a""b" ,c`: {"Mixed", `a"b`, "c"},
		``:                   {""},
	} {
		if got := apply.SchemaNames(path); !slices.Equal(got, want) {
			t.Errorf("SchemaNames(%q) = %q, want %q", path, got, want)
		}
	}
}

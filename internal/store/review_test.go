package store_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/store"
)

// rolePassword is the password of the login roles these tests create.
const rolePassword = "reviewer-pw"

// superuserIn connects as the test server's superuser to database. It is
// closed when the test ends.
func superuserIn(t *testing.T, database string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), "postgres://postgres:postgres-pw@"+testAddr(t)+"/"+database+"?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// connectAs connects as role, one of loginRole's, to database. It is closed
// when the test ends.
func connectAs(t *testing.T, database, role string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), "postgres://"+role+":"+rolePassword+"@"+testAddr(t)+"/"+database+"?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// databaseOf is the database a DSN names.
func databaseOf(t *testing.T, dsn string) string {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Database
}

// loginRole creates a login role that has no rights, dropped when the test
// ends.
func loginRole(t *testing.T, kind string) string {
	t.Helper()
	su := superuserIn(t, "stet_test")
	name := fmt.Sprintf("stet_%s_%d", kind, time.Now().UnixNano())
	if _, err := su.Exec(context.Background(), "create role "+name+" login password '"+rolePassword+"'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := su.Exec(context.Background(), "drop role "+name); err != nil {
			t.Errorf("drop role %s: %v", name, err)
		}
	})
	return name
}

// underMigrateLock runs sqls in one transaction that first takes the lock
// Migrate takes. GRANT and DROP OWNED rewrite the catalog rows of the
// review view and the two functions, as a Migrate that runs schema.sql
// does; run at the same time, one of them fails with "tuple concurrently
// updated" (plan ruling P3).
func underMigrateLock(ctx context.Context, conn *pgx.Conn, sqls ...string) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", store.MigrateLock); err != nil {
			return err
		}
		for _, sql := range sqls {
			if _, err := tx.Exec(ctx, sql); err != nil {
				return fmt.Errorf("%s: %w", sql, err)
			}
		}
		return nil
	})
}

// grant gives role rights on Stet's objects in database, and takes them back
// with DROP OWNED BY when the test ends, before the role is dropped.
func grant(t *testing.T, database, role string, grants ...string) {
	t.Helper()
	su := superuserIn(t, database)
	if err := underMigrateLock(context.Background(), su, grants...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := underMigrateLock(context.Background(), su, "drop owned by "+role); err != nil {
			t.Errorf("in %s: %v", database, err)
		}
	})
}

// reviewGrants are the three lines of spec 6 for role.
func reviewGrants(role string) []string {
	return []string{
		"grant usage on schema stet to " + role,
		"grant select on stet.review to " + role,
		"grant execute on function stet.approve(uuid, text, bigint), stet.reject(uuid, text) to " + role,
	}
}

// reviewerIn creates a login role with the grants of spec 6 in database and
// returns a connection as it, and its name.
func reviewerIn(t *testing.T, database string) (*pgx.Conn, string) {
	t.Helper()
	name := loginRole(t, "reviewer")
	grant(t, database, name, reviewGrants(name)...)
	return connectAs(t, database, name), name
}

// reviewer is reviewerIn the shared test database.
func reviewer(t *testing.T) (*pgx.Conn, string) {
	t.Helper()
	return reviewerIn(t, "stet_test")
}

// pendingHeld stores h and marks it delivered, so that it is pending.
func pendingHeld(t *testing.T, s *store.Store, pool *pgxpool.Pool, h *store.HeldWrite) *store.HeldWrite {
	t.Helper()
	cleanHeld(t, pool, h.ID)
	if err := s.InsertHeldWrite(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDelivered(context.Background(), h.ID, false); err != nil {
		t.Fatal(err)
	}
	return h
}

// insertRaw stores a held write by SQL, as one written before this stage or
// outside InsertHeldWrite would be: the columns every row has, plus the
// given columns set to SQL expressions. It is removed when the test ends.
func insertRaw(t *testing.T, pool *pgxpool.Pool, exprs map[string]string) string {
	t.Helper()
	id := store.NewID()
	cols := []string{"id", "decision_id", "created_at", "login_role", "database", "statement", "target_table", "verdict", "reasons"}
	vals := []string{"$1", "gen_random_uuid()", "now()", "'agent_a'", "'stet_test'",
		"'update contacts set title = ''CTO'' where id = 7'", "'contacts'", "'hold'", "'{model:conflict}'"}
	for _, col := range slices.Sorted(maps.Keys(exprs)) {
		cols = append(cols, col)
		vals = append(vals, exprs[col])
	}
	cleanHeld(t, pool, id)
	if _, err := pool.Exec(context.Background(), "insert into stet.held_writes ("+strings.Join(cols, ", ")+
		") values ("+strings.Join(vals, ", ")+")", id); err != nil {
		t.Fatal(err)
	}
	return id
}

// rowImage is the whole held write as JSON, or "" when there is none.
func rowImage(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var image string
	err := pool.QueryRow(context.Background(), `select to_jsonb(h)::text from stet.held_writes h where id = $1`, id).Scan(&image)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return image
}

// wantST005 checks that err is stet.approve's or stet.reject's refusal with message.
func wantST005(t *testing.T, err error, message string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "ST005" || pgErr.Message != message {
		t.Errorf("err = %v, want SQLSTATE ST005 %q", err, message)
	}
}

// want42501 checks that err is a refusal for lack of rights.
func want42501(t *testing.T, err error, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("%s: err = %v, want SQLSTATE 42501", what, err)
	}
}

// resolution is what stet.approve and stet.reject write on a held write.
type resolution struct {
	Status     string
	ResolvedBy *string
	ResolvedAt *time.Time
	Note       *string
	MaxRows    *int64
	// Age is how long before the database's clock ResolvedAt was.
	Age time.Duration
}

func resolutionOf(t *testing.T, pool *pgxpool.Pool, id string) resolution {
	t.Helper()
	var r resolution
	var age float64
	if err := pool.QueryRow(context.Background(), `
		select status, resolved_by, resolved_at, note, max_rows,
		       coalesce(extract(epoch from clock_timestamp() - resolved_at), 0)::float8
		from stet.held_writes where id = $1`, id).Scan(&r.Status, &r.ResolvedBy, &r.ResolvedAt, &r.Note, &r.MaxRows, &age); err != nil {
		t.Fatal(err)
	}
	r.Age = time.Duration(age * float64(time.Second))
	return r
}

// The view shows each held write with the evidence from its decision, and a
// held write without a decision too. A pending write past expires_at shows
// as expired. A reviewer reads it with the rights of spec 6 alone.
func TestReviewShowsAHeldWriteWithItsDecision(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	rev, _ := reviewer(t)

	d := sample(uniqueTable(t))
	d.Impact = map[string]any{"method": "count", "rows": 3, "capped": false}
	d.Cumulative = map[string]any{"used": 480, "planned": 20, "limit": 500, "window_s": 600}
	if err := s.Insert(ctx, d); err != nil {
		t.Fatal(err)
	}
	h := sampleHeld()
	h.DecisionID = d.ID
	cleanHeld(t, pool, h.ID)
	if err := s.InsertHeldWrite(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDelivered(ctx, h.ID, true); err != nil {
		t.Fatal(err)
	}

	var (
		status, agentID, statement, loginRole, table, verdict string
		onBehalfOf, targetPK                                  *string
		state, answers, impact, cumulative, proposed          map[string]any
		params                                                []*string
		reasons                                               []string
		plannedRows                                           *int64
		rowsExact, inTransaction                              *bool
		expiresAt                                             time.Time
	)
	if err := rev.QueryRow(ctx, `
		select status, agent_id, on_behalf_of, state, answers, impact, cumulative, proposed,
		       statement, params, reasons, planned_rows, rows_exact, in_transaction,
		       login_role, target_table, target_pk, verdict, expires_at
		from stet.review where id = $1`, h.ID).Scan(
		&status, &agentID, &onBehalfOf, &state, &answers, &impact, &cumulative, &proposed,
		&statement, &params, &reasons, &plannedRows, &rowsExact, &inTransaction,
		&loginRole, &table, &targetPK, &verdict, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || agentID != "enricher" || deref(onBehalfOf) != "user:42" {
		t.Errorf("status %q, agent_id %q, on_behalf_of %v", status, agentID, deref(onBehalfOf))
	}
	for name, c := range map[string]struct{ got, want map[string]any }{
		"state":      {state, map[string]any{"table": "contacts"}},
		"answers":    {answers, map[string]any{"conflict": map[string]any{"choice": "no", "confidence": 0.9}}},
		"impact":     {impact, map[string]any{"method": "count", "rows": 3.0, "capped": false}},
		"cumulative": {cumulative, map[string]any{"used": 480.0, "planned": 20.0, "limit": 500.0, "window_s": 600.0}},
		"proposed":   {proposed, map[string]any{"title": "CTO"}},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", name, c.got, c.want)
		}
	}
	if statement != h.Statement || !reflect.DeepEqual(params, h.Params) || !reflect.DeepEqual(reasons, h.Reasons) ||
		deref(plannedRows) != int64(1) || deref(rowsExact) != true || deref(inTransaction) != true {
		t.Errorf("statement %q, params %v, reasons %v, planned_rows %v, rows_exact %v, in_transaction %v",
			statement, params, reasons, deref(plannedRows), deref(rowsExact), deref(inTransaction))
	}
	if loginRole != "agent_a" || table != "contacts" || deref(targetPK) != "7" || verdict != "hold" ||
		expiresAt.Sub(h.ExpiresAt).Abs() > time.Millisecond {
		t.Errorf("login_role %q, target_table %q, target_pk %v, verdict %q, expires_at %v",
			loginRole, table, deref(targetPK), verdict, expiresAt)
	}

	// A held write whose decision was not recorded.
	orphan := pendingHeld(t, s, pool, sampleHeld())
	var nulls int
	if err := rev.QueryRow(ctx, `
		select status, statement, num_nulls(agent_id, on_behalf_of, state, answers, impact, cumulative, proposed)
		from stet.review where id = $1`, orphan.ID).Scan(&status, &statement, &nulls); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || statement != orphan.Statement || nulls != 7 {
		t.Errorf("without a decision: status %q, statement %q, %d of 7 decision columns NULL", status, statement, nulls)
	}

	// expires_at has passed: a pending write shows as expired, any other as its status.
	past := sampleHeld()
	past.ExpiresAt = time.Now().Add(-time.Minute)
	pendingHeld(t, s, pool, past)
	unconfirmed := sampleHeld()
	unconfirmed.ExpiresAt = past.ExpiresAt
	cleanHeld(t, pool, unconfirmed.ID)
	if err := s.InsertHeldWrite(ctx, unconfirmed); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{past.ID: "expired", unconfirmed.ID: "unconfirmed"} {
		if err := rev.QueryRow(ctx, `select status from stet.review where id = $1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != want {
			t.Errorf("past expires_at: status %q, want %q", status, want)
		}
	}
}

// The view shows the session settings a held write will be run under, by
// which its text values are read: a reviewer can tell what '01/02/2026' is.
func TestReviewShowsTheSettingsAHeldWriteRunsUnder(t *testing.T) {
	s, pool := testStore(t)
	rev, _ := reviewer(t)
	h := sampleHeld()
	h.Settings = map[string]string{"TimeZone": "Asia/Seoul", "DateStyle": "ISO, DMY", "IntervalStyle": "postgres"}
	pendingHeld(t, s, pool, h)
	var settings map[string]string
	if err := rev.QueryRow(context.Background(), `select settings from stet.review where id = $1`, h.ID).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(settings, h.Settings) {
		t.Errorf("settings = %v, want %v", settings, h.Settings)
	}
}

func TestApproveMarksTheRow(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	rev, name := reviewer(t)

	h := pendingHeld(t, s, pool, sampleHeld())
	var result string
	if err := rev.QueryRow(ctx, `select stet.approve($1, 'ok', 5)`, h.ID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	r := resolutionOf(t, pool, h.ID)
	if result != "approved" || r.Status != "approved" || deref(r.ResolvedBy) != name || r.ResolvedAt == nil ||
		r.Age < 0 || r.Age > 5*time.Second || deref(r.Note) != "ok" || deref(r.MaxRows) != int64(5) {
		t.Errorf("returned %q; row %s by %v, %v ago, note %v, max_rows %v; want approved by %s just now, ok, 5",
			result, r.Status, deref(r.ResolvedBy), r.Age, deref(r.Note), deref(r.MaxRows), name)
	}

	bare := pendingHeld(t, s, pool, sampleHeld())
	if err := rev.QueryRow(ctx, `select stet.approve($1)`, bare.ID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	if r := resolutionOf(t, pool, bare.ID); result != "approved" || r.Status != "approved" || r.Note != nil || r.MaxRows != nil {
		t.Errorf("one argument: returned %q; row %s, note %v, max_rows %v; want approved, NULL, NULL",
			result, r.Status, deref(r.Note), deref(r.MaxRows))
	}
}

func TestRejectMarksTheRow(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	rev, name := reviewer(t)

	h := pendingHeld(t, s, pool, sampleHeld())
	var result string
	if err := rev.QueryRow(ctx, `select stet.reject($1, 'not this one')`, h.ID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	r := resolutionOf(t, pool, h.ID)
	if result != "rejected" || r.Status != "rejected" || deref(r.ResolvedBy) != name || r.ResolvedAt == nil ||
		r.Age < 0 || r.Age > 5*time.Second || deref(r.Note) != "not this one" || r.MaxRows != nil {
		t.Errorf("returned %q; row %s by %v, %v ago, note %v, max_rows %v; want rejected by %s just now, the note, NULL",
			result, r.Status, deref(r.ResolvedBy), r.Age, deref(r.Note), deref(r.MaxRows), name)
	}

	expired := sampleHeld()
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	own := sampleHeld()
	own.LoginRole = name
	for what, h := range map[string]*store.HeldWrite{"expired": expired, "its own write": own} {
		pendingHeld(t, s, pool, h)
		if err := rev.QueryRow(ctx, `select stet.reject($1)`, h.ID).Scan(&result); err != nil {
			t.Errorf("%s: %v", what, err)
			continue
		}
		if r := resolutionOf(t, pool, h.ID); r.Status != "rejected" || deref(r.ResolvedBy) != name || r.Note != nil {
			t.Errorf("%s: row %s by %v, note %v; want rejected by %s, NULL", what, r.Status, deref(r.ResolvedBy), deref(r.Note), name)
		}
	}
}

// Each refusal of spec 6 (with corrections B5 and B9), in the order the
// functions check them, and the row is left as it was.
func TestApproveAndRejectRefusals(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	rev, name := reviewer(t)

	refused := func(t *testing.T, sql, id, message string) {
		t.Helper()
		before := rowImage(t, pool, id)
		_, err := rev.Exec(ctx, sql, id)
		wantST005(t, err, message)
		if after := rowImage(t, pool, id); after != before {
			t.Errorf("%s changed the row:\n%s\n%s", sql, before, after)
		}
	}
	const approve, reject = `select stet.approve($1)`, `select stet.reject($1)`
	held := func(t *testing.T, change func(h *store.HeldWrite)) string {
		h := sampleHeld()
		change(h)
		return pendingHeld(t, s, pool, h).ID
	}
	notExact := func(id string) string {
		return "held write " + id + " was not measured exactly (planned_rows: "
	}

	t.Run("no such row", func(t *testing.T) {
		id := store.NewID()
		for _, sql := range []string{approve, reject} {
			refused(t, sql, id, "held write "+id+" does not exist")
		}
	})
	t.Run("unconfirmed", func(t *testing.T) {
		id := insertHeld(t, s, pool).ID
		for _, sql := range []string{approve, reject} {
			refused(t, sql, id, "held write "+id+" is unconfirmed: it is not confirmed that the agent was told of it")
		}
	})
	for _, status := range []string{"undelivered", "approved", "rejected", "applied", "failed"} {
		t.Run(status, func(t *testing.T) {
			id := heldWithStatus(t, s, pool, status).ID
			for _, sql := range []string{approve, reject} {
				refused(t, sql, id, "held write "+id+" is "+status+", not pending")
			}
		})
	}
	t.Run("expired", func(t *testing.T) {
		id := held(t, func(h *store.HeldWrite) { h.ExpiresAt = time.Now().Add(-time.Second) })
		refused(t, approve, id, "held write "+id+" has expired")
	})
	t.Run("its own write", func(t *testing.T) {
		id := held(t, func(h *store.HeldWrite) { h.LoginRole = name })
		refused(t, approve, id, name+" sent this write and cannot approve it")
	})
	t.Run("concurrent_change", func(t *testing.T) {
		id := held(t, func(h *store.HeldWrite) { h.Reasons = []string{"concurrent_change", "rejudged"} })
		refused(t, approve, id, "held write "+id+" was held because its row kept changing while it was judged "+
			"(concurrent_change): its stored state is stale and it would not run; the agent must send it again")
	})
	t.Run("negative max_rows", func(t *testing.T) {
		id := held(t, func(h *store.HeldWrite) {})
		refused(t, `select stet.approve($1, null, -1)`, id, "max_rows must not be negative")
	})
	t.Run("not measured exactly", func(t *testing.T) {
		for _, c := range []struct {
			what        string
			plannedRows *int64
			rowsExact   bool
			reads       string
		}{
			{"measured inexactly", ptr(int64(101)), false, "101"},
			{"not measured", nil, false, "unknown"},
			{"exact but no count", nil, true, "unknown"},
		} {
			id := held(t, func(h *store.HeldWrite) { h.PlannedRows, h.RowsExact = c.plannedRows, c.rowsExact })
			refused(t, approve, id, notExact(id)+c.reads+"): pass max_rows, the most rows it may change")
		}
		// rows_exact NULL, as on a row written outside InsertHeldWrite.
		id := insertRaw(t, pool, map[string]string{"status": "'pending'", "expires_at": "now() + interval '1 hour'", "planned_rows": "5"})
		refused(t, approve, id, notExact(id)+"5): pass max_rows, the most rows it may change")
	})
	t.Run("max_rows makes an inexact write approvable", func(t *testing.T) {
		for _, maxRows := range []int64{200, 0} {
			id := held(t, func(h *store.HeldWrite) { h.PlannedRows, h.RowsExact = ptr(int64(101)), false })
			var result string
			if err := rev.QueryRow(ctx, `select stet.approve($1, max_rows => $2)`, id, maxRows).Scan(&result); err != nil {
				t.Fatalf("max_rows %d: %v", maxRows, err)
			}
			if r := resolutionOf(t, pool, id); result != "approved" || r.Status != "approved" || deref(r.MaxRows) != maxRows {
				t.Errorf("max_rows %d: returned %q, row %s with max_rows %v", maxRows, result, r.Status, deref(r.MaxRows))
			}
		}
	})
	t.Run("in order", func(t *testing.T) {
		past := time.Now().Add(-time.Second)
		unconfirmed := sampleHeld()
		unconfirmed.ExpiresAt = past
		cleanHeld(t, pool, unconfirmed.ID)
		if err := s.InsertHeldWrite(ctx, unconfirmed); err != nil {
			t.Fatal(err)
		}
		for _, sql := range []string{approve, reject} {
			refused(t, sql, unconfirmed.ID, "held write "+unconfirmed.ID+" is unconfirmed: it is not confirmed that the agent was told of it")
		}
		id := held(t, func(h *store.HeldWrite) { h.ExpiresAt, h.LoginRole = past, name })
		refused(t, approve, id, "held write "+id+" has expired")
		id = held(t, func(h *store.HeldWrite) { h.LoginRole, h.Reasons = name, []string{"concurrent_change"} })
		refused(t, approve, id, name+" sent this write and cannot approve it")
		id = held(t, func(h *store.HeldWrite) { h.Reasons = []string{"concurrent_change"} })
		refused(t, `select stet.approve($1, null, -1)`, id, "held write "+id+" was held because its row kept changing while it was judged "+
			"(concurrent_change): its stored state is stale and it would not run; the agent must send it again")
		id = held(t, func(h *store.HeldWrite) { h.PlannedRows, h.RowsExact = nil, false })
		refused(t, `select stet.approve($1, null, -1)`, id, "max_rows must not be negative")
	})
}

// stet.approve and stet.reject read the clock, not the start of the
// reviewer's transaction (correction B9): resolved_at is when they ran, and a
// write that expired since the transaction began cannot be approved.
func TestApproveAndRejectReadTheClock(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	rev, _ := reviewer(t)

	tx, err := rev.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var began time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&began); err != nil {
		t.Fatal(err)
	}
	approved := pendingHeld(t, s, pool, sampleHeld())
	rejected := pendingHeld(t, s, pool, sampleHeld())
	expiring := sampleHeld()
	expiring.ExpiresAt = time.Now().Add(250 * time.Millisecond)
	pendingHeld(t, s, pool, expiring)
	const slept = 500 * time.Millisecond
	if _, err := tx.Exec(ctx, `select pg_sleep(0.5)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `select stet.approve($1)`, approved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `select stet.reject($1)`, rejected.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `savepoint expiring`); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `select stet.approve($1)`, expiring.ID)
	wantST005(t, err, "held write "+expiring.ID+" has expired")
	if _, err := tx.Exec(ctx, `rollback to savepoint expiring`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{approved.ID, rejected.ID} {
		r := resolutionOf(t, pool, id)
		if r.ResolvedAt == nil || r.ResolvedAt.Sub(began) < slept {
			t.Errorf("%s: resolved_at %v, want at least %v after the transaction began at %v", r.Status, r.ResolvedAt, slept, began)
		}
	}
	if r := resolutionOf(t, pool, expiring.ID); r.Status != "pending" {
		t.Errorf("expired write is %s, want pending", r.Status)
	}
}

// Only roles given the rights of spec 6 can read the view and call the
// functions; the functions are not PUBLIC's, and a reviewer cannot read
// Stet's tables. Running schema.sql again keeps it so.
func TestOnlyGrantedRolesReview(t *testing.T) {
	dsn := ownDatabase(t)
	db := databaseOf(t, dsn)
	ctx := context.Background()
	pool := newPool(t, dsn)
	s := store.New(pool)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	pending := func() string {
		h := sampleHeld()
		h.Database = db
		return pendingHeld(t, s, pool, h).ID
	}
	id := pending()

	stranger := loginRole(t, "stranger")
	usageOnly := loginRole(t, "usage")
	grant(t, db, usageOnly, "grant usage on schema stet to "+usageOnly)
	rev, _ := reviewerIn(t, db)
	strangerConn, usageConn := connectAs(t, db, stranger), connectAs(t, db, usageOnly)

	_, err := strangerConn.Exec(ctx, `select * from stet.review`)
	want42501(t, err, "a role without rights reads the view")
	_, err = strangerConn.Exec(ctx, `select stet.approve($1)`, id)
	want42501(t, err, "a role without rights approves")
	usageOnlyRefused := func(when string) {
		t.Helper()
		_, err := usageConn.Exec(ctx, `select stet.approve($1)`, id)
		want42501(t, err, "a role with usage only approves, "+when)
		_, err = usageConn.Exec(ctx, `select stet.reject($1)`, id)
		want42501(t, err, "a role with usage only rejects, "+when)
	}
	usageOnlyRefused("after the first migrate")
	for _, table := range []string{"stet.held_writes", "stet.decisions"} {
		_, err := rev.Exec(ctx, `select * from `+table)
		want42501(t, err, "the reviewer reads "+table)
	}
	var result string
	if err := rev.QueryRow(ctx, `select stet.approve($1)`, id).Scan(&result); err != nil || result != "approved" {
		t.Fatalf("reviewer approves: %q, %v", result, err)
	}

	// Make the next Migrate run schema.sql again.
	if _, err := pool.Exec(ctx, `update stet.schema_version set sha256 = 'an older schema'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := schemaMarker(t, pool), schemaHash(t); got != want {
		t.Fatalf("schema marker = %q, want %q: schema.sql did not run again", got, want)
	}
	id = pending()
	usageOnlyRefused("after schema.sql ran again")
	if err := rev.QueryRow(ctx, `select stet.approve($1)`, id).Scan(&result); err != nil || result != "approved" {
		t.Errorf("reviewer approves after schema.sql ran again: %q, %v", result, err)
	}
	var rows int
	if err := rev.QueryRow(ctx, `select count(*) from stet.review`).Scan(&rows); err != nil || rows != 2 {
		t.Errorf("reviewer reads the view after schema.sql ran again: %d rows, %v", rows, err)
	}
}

// Two reviewers on one row: the second waits for the first, and once the
// first commits gets a refusal that names the row's status.
func TestConcurrentApproveAndRejectLetOneWin(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	first, firstName := reviewer(t)
	second, _ := reviewer(t)
	su := superuserIn(t, "stet_test")

	for _, call := range []string{`select stet.reject($1)`, `select stet.approve($1)`} {
		t.Run(call, func(t *testing.T) {
			h := pendingHeld(t, s, pool, sampleHeld())
			tx, err := first.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `select stet.approve($1)`, h.ID); err != nil {
				tx.Rollback(ctx)
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := second.Exec(ctx, call, h.ID)
				done <- err
			}()
			waited := waitsForALock(t, su, second.PgConn().PID(), done)
			commitErr := tx.Commit(ctx)
			err = <-done // second is free again before the test can end
			if commitErr != nil {
				t.Fatal(commitErr)
			}
			if !waited {
				t.Fatalf("%s did not wait for the uncommitted approval (err %v)", call, err)
			}
			wantST005(t, err, "held write "+h.ID+" is approved, not pending")
			if r := resolutionOf(t, pool, h.ID); r.Status != "approved" || deref(r.ResolvedBy) != firstName {
				t.Errorf("row %s by %v, want approved by %s", r.Status, deref(r.ResolvedBy), firstName)
			}
		})
	}
}

// waitsForALock reports whether backend pid comes to wait for a lock before
// done delivers its result or five seconds pass.
func waitsForALock(t *testing.T, su *pgx.Conn, pid uint32, done chan error) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := su.QueryRow(context.Background(),
			`select exists (select from pg_locks where pid = $1 and not granted)`, int64(pid)).Scan(&waiting); err != nil {
			t.Error(err)
			return false
		}
		if waiting {
			return true
		}
		select {
		case err := <-done:
			done <- err
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	return false
}

// A row held by the enforce stage has none of this stage's columns: it shows
// as expired, cannot be approved, and can be rejected (Review Focus 2).
func TestRowsHeldBeforeThisStageCannotBeApproved(t *testing.T) {
	_, pool := testStore(t)
	ctx := context.Background()
	rev, _ := reviewer(t)
	id := insertRaw(t, pool, map[string]string{
		"status": "'pending'", "params": `'["CTO", "7"]'`, "param_types": "'{25,0}'", "target_pk": "'7'", "row_version": "'123'",
	})
	var nulls int
	if err := pool.QueryRow(ctx, `select num_nulls(expires_at, settings, row_absent, in_transaction, planned_rows, rows_exact)
		from stet.held_writes where id = $1`, id).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 6 {
		t.Fatalf("%d of this stage's 6 columns are NULL", nulls)
	}

	var status string
	if err := rev.QueryRow(ctx, `select status from stet.review where id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "expired" {
		t.Errorf("view status %q, want expired", status)
	}
	_, err := rev.Exec(ctx, `select stet.approve($1, null, 1)`, id)
	wantST005(t, err, "held write "+id+" has expired")
	var result string
	if err := rev.QueryRow(ctx, `select stet.reject($1)`, id).Scan(&result); err != nil || result != "rejected" {
		t.Errorf("reject: %q, %v", result, err)
	}
	if r := resolutionOf(t, pool, id); r.Status != "rejected" {
		t.Errorf("row is %s, want rejected", r.Status)
	}
}

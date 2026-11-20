package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/store"
)

// testAddr is the test server's address. Without one the test is skipped.
func testAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	return addr
}

// testDSN connects as Stet's own account to the test database.
func testDSN(t *testing.T) string {
	t.Helper()
	return "postgres://stet_scram:scram-pw@" + testAddr(t) + "/stet_test?sslmode=disable"
}

// newPool connects to dsn and is closed when the test ends.
func newPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ownDatabase creates an empty database owned by Stet's account, dropped
// when the test ends, and returns how Stet connects to it. Tests that change
// the stet schema use one, so that other packages sharing stet_test do not
// see the change.
func ownDatabase(t *testing.T) string {
	t.Helper()
	addr := testAddr(t)
	ctx := context.Background()
	su, err := pgx.Connect(ctx, "postgres://postgres:postgres-pw@"+addr+"/stet_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { su.Close(context.Background()) })
	name := fmt.Sprintf("stet_store_db_%d", time.Now().UnixNano())
	if _, err := su.Exec(ctx, "create database "+name+" template template0 owner stet_scram"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { su.Exec(context.Background(), "drop database if exists "+name+" with (force)") })
	return "postgres://stet_scram:scram-pw@" + addr + "/" + name + "?sslmode=disable"
}

// schemaHash is the SHA-256 (hex) of schema.sql, which Migrate records once
// it has run the file.
func schemaHash(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// schemaMarker is the hash the database records for the schema it last ran.
func schemaMarker(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var hash string
	if err := pool.QueryRow(context.Background(), `select sha256 from stet.schema_version`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	return hash
}

// migrateWithLockTimeout runs Migrate in a pool that, like Stet's own, waits
// at most 200ms for a lock.
func migrateWithLockTimeout(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "200ms"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	return store.New(pool).Migrate(ctx)
}

// openTx begins a transaction on a connection of its own. It is rolled back
// when the test ends.
func openTx(t *testing.T) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	return tx
}

func testStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := store.New(pool)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s, pool
}

// uniqueTable keeps tests in this and other packages from seeing each
// other's rows.
func uniqueTable(t *testing.T) string {
	return fmt.Sprintf("t_%s_%d", t.Name(), time.Now().UnixNano())
}

func ptr[T any](v T) *T { return &v }

// deref is *p for messages, or "NULL".
func deref[T any](p *T) any {
	if p == nil {
		return "NULL"
	}
	return *p
}

func sample(table string) *store.Decision {
	return &store.Decision{
		ID:             store.NewID(),
		CreatedAt:      time.Now(),
		Mode:           "shadow",
		AgentID:        "enricher",
		OnBehalfOf:     ptr("user:42"),
		SessionID:      "s1",
		Statement:      "update contacts set title = $1 where id = $2",
		StatementFP:    "fp",
		Params:         []*string{ptr("CTO"), ptr("7")},
		Op:             "update",
		TargetTable:    table,
		TargetPK:       ptr("7"),
		Proposed:       map[string]any{"title": "CTO"},
		State:          json.RawMessage(`{"table":"contacts"}`),
		StateVersion:   map[string]string{"xmin": "123"},
		Model:          "fake",
		ModelVersion:   "fake-1",
		Questions:      []map[string]any{{"id": "conflict"}},
		Answers:        map[string]any{"conflict": map[string]any{"choice": "no", "confidence": 0.9}},
		PolicyVersion:  "abc",
		Verdict:        "commit",
		Reasons:        []string{},
		Executed:       true,
		LatencyMS:      ptr(3),
		ModelLatencyMS: ptr(120),
		CostUSD:        ptr(0.0001),
	}
}

func TestMigrateIsIdempotentAndConcurrent(t *testing.T) {
	s, _ := testStore(t)
	errs := make(chan error, 3)
	for range 3 {
		go func() { errs <- s.Migrate(context.Background()) }()
	}
	for range 3 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// Migrate runs whenever Stet starts, in a pool that waits 200ms for a lock.
// On a current schema it must not wait for a held write another session has
// locked, as the executor does while it applies one (plan ruling P2).
func TestMigrateDoesNotWaitForAHeldRowLock(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	h := insertHeld(t, s, pool)

	tx := openTx(t)
	var id string
	if err := tx.QueryRow(ctx, `select id::text from stet.held_writes where id = $1 for update`, h.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := migrateWithLockTimeout(t); err != nil {
		t.Errorf("migrate while a held write is locked: %v", err)
	}
}

// Reviewers keep transactions open around stet.approve, which updates a held
// write, and around reads of Stet's tables. On a current schema Migrate runs
// no DDL, so none of them keeps a Stet from starting. Each transaction stays
// open only while Migrate runs.
func TestMigrateDoesNotWaitForOpenTransactions(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	h := insertHeld(t, s, pool)
	for _, c := range []struct{ name, sql string }{
		{"an uncommitted update of stet.held_writes", `update stet.held_writes set status = 'pending', in_transaction = false where id = $1`},
		{"an uncommitted insert into stet.held_writes", `
			insert into stet.held_writes (id, decision_id, created_at, login_role, database, statement,
			                              target_table, verdict, reasons)
			values (gen_random_uuid(), $1, now(), 'agent_a', 'stet_test', 'delete from contacts',
			        'contacts', 'hold', '{}')`},
		{"a read of stet.decisions", `select count(*) from stet.decisions where id = $1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx := openTx(t)
			if _, err := tx.Exec(ctx, c.sql, h.ID); err != nil {
				t.Fatal(err)
			}
			if err := migrateWithLockTimeout(t); err != nil {
				t.Errorf("migrate during %s: %v", c.name, err)
			}
		})
	}
}

// A database without the stet schema gets all of it, and records the hash
// of the schema it ran.
func TestMigrateFromNothing(t *testing.T) {
	pool := newPool(t, ownDatabase(t))
	ctx := context.Background()
	if err := store.New(pool).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var missing []string
	if err := pool.QueryRow(ctx, `
		select coalesce(array_agg(name), '{}') from unnest('{stet.decisions,stet.reads,stet.read_log,stet.held_writes,
		       stet.held_writes_approved,stet.schema_version}'::text[]) as name
		where to_regclass(name) is null`).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("missing after migrating: %v", missing)
	}
	var guarded bool
	if err := pool.QueryRow(ctx, `select exists (select from pg_trigger
		where tgrelid = 'stet.held_writes'::regclass and tgname = 'held_writes_guard')`).Scan(&guarded); err != nil {
		t.Fatal(err)
	}
	if !guarded {
		t.Error("stet.held_writes has no guard trigger")
	}
	if got, want := schemaMarker(t, pool), schemaHash(t); got != want {
		t.Errorf("schema marker = %q, want %q", got, want)
	}
}

// Stets that start at once against a database without the stet schema all
// succeed: the first to take Migrate's lock runs schema.sql, and the others,
// which waited for it, find the marker it wrote. Also when the server's
// default isolation is repeatable read, where a Migrate that read the marker
// in a snapshot taken before it waited would run the file again and fail to
// write the marker (40001).
func TestConcurrentMigratesFromNothing(t *testing.T) {
	for _, isolation := range []string{"read committed", "repeatable read"} {
		t.Run(isolation, func(t *testing.T) {
			dsn := ownDatabase(t)
			ctx := context.Background()
			cfg, err := pgxpool.ParseConfig(dsn)
			if err != nil {
				t.Fatal(err)
			}
			cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = isolation
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)

			// Hold Migrate's lock until every Migrate waits for it, so that
			// each has begun its transaction before the schema exists.
			holder, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { holder.Close(context.Background()) })
			lock, err := holder.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := lock.Exec(ctx, "select pg_advisory_xact_lock($1)", store.MigrateLock); err != nil {
				t.Fatal(err)
			}
			const n = 3
			errs := make(chan error, n)
			for range n {
				go func() { errs <- store.New(pool).Migrate(ctx) }()
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				var waiting int
				if err := lock.QueryRow(ctx, `select count(*) from pg_locks
					where locktype = 'advisory' and not granted
					  and database = (select oid from pg_database where datname = current_database())`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting == n {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%d of %d Migrates wait for the lock", waiting, n)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := lock.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			for range n {
				if err := <-errs; err != nil {
					t.Errorf("migrate: %v", err)
				}
			}
			var markers int
			if err := pool.QueryRow(ctx, `select count(*) from stet.schema_version`).Scan(&markers); err != nil {
				t.Fatal(err)
			}
			if got, want := schemaMarker(t, pool), schemaHash(t); markers != 1 || got != want {
				t.Errorf("%d marker rows, marker %q, want one row %q", markers, got, want)
			}
		})
	}
}

// Migrate runs schema.sql only when it differs from what the database last
// ran. On a current schema an object dropped by hand stays dropped; once the
// marker differs, Migrate runs the file again and records it.
func TestMigrateRunsTheSchemaOnlyWhenItDiffers(t *testing.T) {
	pool := newPool(t, ownDatabase(t))
	ctx := context.Background()
	s := store.New(pool)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`drop index stet.held_writes_approved`,
		`alter table stet.held_writes alter column status drop default`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	restored := func() (index bool, statusDefault *string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `
			select to_regclass('stet.held_writes_approved') is not null,
			       (select pg_get_expr(d.adbin, d.adrelid)
			        from pg_attrdef d join pg_attribute a on a.attrelid = d.adrelid and a.attnum = d.adnum
			        where d.adrelid = 'stet.held_writes'::regclass and a.attname = 'status')`).Scan(&index, &statusDefault); err != nil {
			t.Fatal(err)
		}
		return index, statusDefault
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if index, statusDefault := restored(); index || statusDefault != nil {
		t.Errorf("a current schema ran again: index %v, status default %v", index, deref(statusDefault))
	}

	if _, err := pool.Exec(ctx, `update stet.schema_version set sha256 = 'an older schema'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if index, statusDefault := restored(); !index || statusDefault == nil || *statusDefault != "'unconfirmed'::text" {
		t.Errorf("after a changed schema: index %v, status default %v, want the index and 'unconfirmed'::text", index, deref(statusDefault))
	}
	if got, want := schemaMarker(t, pool), schemaHash(t); got != want {
		t.Errorf("schema marker = %q, want %q", got, want)
	}
}

func TestInsertRoundTrip(t *testing.T) {
	s, pool := testStore(t)
	d := sample(uniqueTable(t))
	if err := s.Insert(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	var (
		verdict, obo   string
		reasons        []string
		params, pk     string
		proposed, sver string
		executed       bool
	)
	err := pool.QueryRow(context.Background(), `
		select verdict, on_behalf_of, reasons, params::text, target_pk::text,
		       proposed::text, state_version::text, executed
		from stet.decisions where id = $1`, d.ID).
		Scan(&verdict, &obo, &reasons, &params, &pk, &proposed, &sver, &executed)
	if err != nil {
		t.Fatal(err)
	}
	if verdict != "commit" || obo != "user:42" || !executed || len(reasons) != 0 {
		t.Errorf("verdict=%s obo=%s executed=%v reasons=%v", verdict, obo, executed, reasons)
	}
	if params != `["CTO", "7"]` || pk != `"7"` || proposed != `{"title": "CTO"}` || sver != `{"xmin": "123"}` {
		t.Errorf("params=%s pk=%s proposed=%s state_version=%s", params, pk, proposed, sver)
	}
}

func TestInsertLeavesUnjudgedFieldsNull(t *testing.T) {
	s, pool := testStore(t)
	d := sample(uniqueTable(t))
	d.Op, d.Verdict, d.Reasons = "insert", "unsupported", []string{"multi_row"}
	d.TargetPK, d.Proposed, d.State, d.StateVersion = nil, nil, nil, nil
	d.Questions, d.Answers, d.Params, d.OnBehalfOf = nil, nil, nil, nil
	d.ModelLatencyMS, d.CostUSD = nil, nil
	if err := s.Insert(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	var nulls int
	err := pool.QueryRow(context.Background(), `
		select num_nulls(target_pk, proposed, state, state_version, questions, answers,
		                 params, on_behalf_of, model_latency_ms, cost_usd)
		from stet.decisions where id = $1`, d.ID).Scan(&nulls)
	if err != nil {
		t.Fatal(err)
	}
	if nulls != 10 {
		t.Errorf("%d of 10 unjudged fields are NULL", nulls)
	}
}

func TestDecisionsAreAppendOnly(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	d := sample(uniqueTable(t))
	if err := s.Insert(ctx, d); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `update stet.decisions set label = 'correct', label_by = 'qa', labeled_at = now() where id = $1`, d.ID); err != nil {
		t.Fatalf("labeling must be allowed: %v", err)
	}
	for _, sql := range []string{
		`update stet.decisions set verdict = 'hold' where id = $1`,
		`update stet.decisions set state = '{}' where id = $1`,
		`delete from stet.decisions where id = $1`,
	} {
		_, err := pool.Exec(ctx, sql, d.ID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "ST004" {
			t.Errorf("%s: err = %v, want SQLSTATE ST004", sql, err)
		}
	}
	_, err := pool.Exec(ctx, `truncate stet.decisions`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "ST004" {
		t.Errorf("truncate: err = %v, want SQLSTATE ST004", err)
	}
}

func TestRecentChanges(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	table := uniqueTable(t)
	now := time.Now()

	want := sample(table)
	want.CreatedAt = now.Add(-5 * time.Minute)
	notExecuted := sample(table)
	notExecuted.Executed = false
	old := sample(table)
	old.CreatedAt = now.Add(-2 * time.Hour)
	otherRow := sample(table)
	otherRow.TargetPK = ptr("8")
	for _, d := range []*store.Decision{want, notExecuted, old, otherRow} {
		if err := s.Insert(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.RecentChanges(ctx, table, "7", now.Add(-30*time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DecisionID != want.ID {
		t.Fatalf("got %+v, want only decision %s", got, want.ID)
	}
	c := got[0]
	if c.AgentID != "enricher" || c.Op != "update" || !reflect.DeepEqual(c.Proposed, map[string]any{"title": "CTO"}) {
		t.Errorf("change = %+v", c)
	}
	if c.At.Sub(want.CreatedAt).Abs() > time.Millisecond {
		t.Errorf("at = %v, want %v", c.At, want.CreatedAt)
	}
}

func TestInsertRecordsImpact(t *testing.T) {
	s, pool := testStore(t)
	d := sample(uniqueTable(t))
	d.Impact = map[string]any{"method": "count", "rows": 99, "capped": false}
	if err := s.Insert(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	var method string
	var rows int
	err := pool.QueryRow(context.Background(),
		`select impact->>'method', (impact->>'rows')::int from stet.decisions where id = $1`, d.ID).Scan(&method, &rows)
	if err != nil {
		t.Fatal(err)
	}
	if method != "count" || rows != 99 {
		t.Errorf("impact = %s %d", method, rows)
	}
}

func TestInsertWithoutImpactLeavesItNull(t *testing.T) {
	s, pool := testStore(t)
	d := sample(uniqueTable(t))
	if err := s.Insert(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := pool.QueryRow(context.Background(),
		`select impact is null from stet.decisions where id = $1`, d.ID).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Error("impact is set for a write that was not measured")
	}
}

func TestInsertRecordsLoginRoleAndRows(t *testing.T) {
	s, pool := testStore(t)
	d := sample(uniqueTable(t))
	d.LoginRole = ptr("agent_a")
	d.RowsAffected = ptr(int64(0))
	d.Cumulative = map[string]any{"used": 480, "planned": 20, "limit": 500, "window_s": 600}
	if err := s.Insert(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	var role string
	var rows *int64
	var limit string
	err := pool.QueryRow(context.Background(),
		`select login_role, rows_affected, cumulative->>'limit' from stet.decisions where id = $1`, d.ID).Scan(&role, &rows, &limit)
	if err != nil {
		t.Fatal(err)
	}
	if role != "agent_a" || rows == nil || *rows != 0 || limit != "500" {
		t.Errorf("role=%q rows=%v limit=%q", role, rows, limit)
	}
}

func TestInsertRecordsReadCheck(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	d := sample(uniqueTable(t))
	d.ReadCheck = map[string]any{"result": "stale", "read_version": "700", "version": "705", "read_age_ms": 1500, "read_on_behalf_of": nil}
	if err := s.Insert(ctx, d); err != nil {
		t.Fatal(err)
	}
	var result, readVersion, version, age string
	var onBehalfNull bool
	err := pool.QueryRow(ctx, `
		select read_check->>'result', read_check->>'read_version', read_check->>'version', read_check->>'read_age_ms',
		       jsonb_typeof(read_check->'read_on_behalf_of') = 'null'
		from stet.decisions where id = $1`, d.ID).Scan(&result, &readVersion, &version, &age, &onBehalfNull)
	if err != nil {
		t.Fatal(err)
	}
	if result != "stale" || readVersion != "700" || version != "705" || age != "1500" || !onBehalfNull {
		t.Errorf("result=%q read_version=%q version=%q age=%q on_behalf_null=%v", result, readVersion, version, age, onBehalfNull)
	}

	n := sample(uniqueTable(t))
	if err := s.Insert(ctx, n); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `select read_check is null from stet.decisions where id = $1`, n.ID).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Error("read_check is set for a write that was not compared")
	}
}

func TestReadCheckIsAppendOnly(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	d := sample(uniqueTable(t))
	d.ReadCheck = map[string]any{"result": "fresh"}
	if err := s.Insert(ctx, d); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `update stet.decisions set read_check = '{"result":"stale"}' where id = $1`, d.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "ST004" {
		t.Errorf("err = %v, want SQLSTATE ST004", err)
	}
}

// A decision records its session's backend pid and the window its write was
// on the server, both times on the database's clock: Insert sends how long
// ago CreatedAt and DoneAt were, never a time of Stet's own clock
// (spec-v0.2-undo 5). Without a pid or a DoneAt all three are NULL.
func TestInsertRecordsTheBackendWindow(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	clock := func() time.Time {
		t.Helper()
		var at time.Time
		if err := pool.QueryRow(ctx, "select clock_timestamp()").Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	type window struct {
		pid        *int64
		sent, done *time.Time
	}
	read := func(id string) window {
		t.Helper()
		var w window
		if err := pool.QueryRow(ctx, "select backend_pid, sent_after, done_before from stet.decisions where id = $1", id).
			Scan(&w.pid, &w.sent, &w.done); err != nil {
			t.Fatal(err)
		}
		return w
	}

	d := sample(uniqueTable(t))
	before := clock()
	now := time.Now()
	d.CreatedAt, d.DoneAt, d.BackendPID = now.Add(-300*time.Millisecond), now.Add(-100*time.Millisecond), ptr(uint32(4242))
	if err := s.Insert(ctx, d); err != nil {
		t.Fatal(err)
	}
	after := clock()
	w := read(d.ID)
	if w.pid == nil || *w.pid != 4242 {
		t.Errorf("backend_pid = %v, want 4242", deref(w.pid))
	}
	const slack = 50 * time.Millisecond
	for _, c := range []struct {
		name string
		got  *time.Time
		ago  time.Duration
	}{{"sent_after", w.sent, 300 * time.Millisecond}, {"done_before", w.done, 100 * time.Millisecond}} {
		lo, hi := before.Add(-c.ago-slack), after.Add(-c.ago+slack)
		if c.got == nil || c.got.Before(lo) || c.got.After(hi) {
			t.Errorf("%s = %v, want between %v and %v: the database's clock around the insert, less %v",
				c.name, deref(c.got), lo, hi, c.ago)
		}
	}
	if w.sent != nil && w.done != nil && !w.sent.Before(*w.done) {
		t.Errorf("sent_after %v is not before done_before %v", *w.sent, *w.done)
	}

	noPID := sample(uniqueTable(t))
	noPID.DoneAt = time.Now()
	notDone := sample(uniqueTable(t))
	notDone.BackendPID = ptr(uint32(4242))
	for name, d := range map[string]*store.Decision{"no backend pid": noPID, "no DoneAt": notDone} {
		if err := s.Insert(ctx, d); err != nil {
			t.Fatal(err)
		}
		if w := read(d.ID); w.pid != nil || w.sent != nil || w.done != nil {
			t.Errorf("%s: backend_pid %v, sent_after %v, done_before %v; want all NULL",
				name, deref(w.pid), deref(w.sent), deref(w.done))
		}
	}
}

// recordedWindow is what a decision records of its write's backend and of the
// window the write was on the server.
type recordedWindow struct {
	pid        *int64
	sent, done *time.Time
}

// windowOf is what decision id records of its write's backend and window.
func windowOf(t *testing.T, pool *pgxpool.Pool, id string) recordedWindow {
	t.Helper()
	var w recordedWindow
	if err := pool.QueryRow(context.Background(), "select backend_pid, sent_after, done_before from stet.decisions where id = $1", id).
		Scan(&w.pid, &w.sent, &w.done); err != nil {
		t.Fatal(err)
	}
	return w
}

// dbClock is the database's clock_timestamp(), read on conn.
func dbClock(t *testing.T, conn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}) time.Time {
	t.Helper()
	var at time.Time
	if err := conn.QueryRow(context.Background(), "select clock_timestamp()").Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// wantWindowNear checks that w names pid 4242 and that its two times are
// sentAgo and doneAgo before at, on the database's clock, within slack.
func wantWindowNear(t *testing.T, w recordedWindow, at time.Time, sentAgo, doneAgo, slack time.Duration) {
	t.Helper()
	if w.pid == nil || *w.pid != 4242 {
		t.Errorf("backend_pid = %v, want 4242", deref(w.pid))
	}
	for _, c := range []struct {
		name string
		got  *time.Time
		ago  time.Duration
	}{{"sent_after", w.sent, sentAgo}, {"done_before", w.done, doneAgo}} {
		lo, hi := at.Add(-c.ago-slack), at.Add(-c.ago+slack)
		if c.got == nil || c.got.Before(lo) || c.got.After(hi) {
			t.Errorf("%s = %v, want between %v and %v: %v before the database's clock when the insert began",
				c.name, deref(c.got), lo, hi, c.ago)
		}
	}
}

// windowed is a sample decision whose write arrived 300ms ago and was done
// 100ms ago, on backend 4242.
func windowed(t *testing.T) *store.Decision {
	d := sample(uniqueTable(t))
	now := time.Now()
	d.CreatedAt, d.DoneAt, d.BackendPID = now.Add(-300*time.Millisecond), now.Add(-100*time.Millisecond), ptr(uint32(4242))
	return d
}

// The window is taken from the start of the insert's transaction, which
// begins when the insert's first message reaches the server, not from when
// the insert runs. In between it may wait for a lock on stet.decisions
// (another Stet's migration, DDL by hand), and that wait would make
// sent_after later than the write was sent: the unsafe direction, in which a
// change the write made falls outside its window (spec 5). In every
// exec mode, for a connection's first insert and a later one: the statement
// Insert prepared waits at Bind, the simple protocol's while its query is
// read; and an insert must not describe its statement in a transaction of
// its own after the measurement, as cache_describe (the first time) and
// describe_exec (each time) would, since the lock is waited for there.
func TestInsertWindowIsNotDelayedByALockWait(t *testing.T) {
	for _, mode := range []string{"cache_statement", "cache_describe", "describe_exec", "exec", "simple_protocol"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			dsn := ownDatabase(t)
			cfg, err := pgxpool.ParseConfig(dsn + "&default_query_exec_mode=" + mode)
			if err != nil {
				t.Fatal(err)
			}
			cfg.MaxConns = 1
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(pool.Close)
			s := store.New(pool)
			if err := s.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			locker, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { locker.Close(context.Background()) })

			// The pool's one connection inserts first while the table is
			// locked, then again: by then it has prepared or described the
			// insert, if its mode does.
			for _, round := range []string{"the connection's first insert", "a later insert"} {
				lock, err := locker.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := lock.Exec(ctx, "lock table stet.decisions in share mode"); err != nil {
					t.Fatal(err)
				}
				began := dbClock(t, lock)
				d := windowed(t)
				inserted := make(chan error, 1)
				go func() { inserted <- s.Insert(ctx, d) }()
				deadline := time.Now().Add(5 * time.Second)
				for {
					var waiting int
					if err := lock.QueryRow(ctx, `select count(*) from pg_locks
						where locktype = 'relation' and relation = 'stet.decisions'::regclass and not granted`).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting > 0 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("%s never waited for the lock", round)
					}
					time.Sleep(5 * time.Millisecond)
				}
				time.Sleep(300 * time.Millisecond)
				if err := lock.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err := <-inserted; err != nil {
					t.Fatalf("%s: %v", round, err)
				}
				t.Run(round, func(t *testing.T) {
					wantWindowNear(t, windowOf(t, pool, d.ID), began, 300*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond)
				})
			}
		})
	}
}

// slowJSON takes its time to encode, as a large state or set of answers may.
type slowJSON time.Duration

func (d slowJSON) MarshalJSON() ([]byte, error) {
	time.Sleep(time.Duration(d))
	return []byte(`["slow"]`), nil
}

// Insert encodes every value before it measures how long ago the write
// arrived and was done: what runs between that measurement and the server's
// clock makes the window late.
func TestInsertMeasuresTheWindowAfterEncoding(t *testing.T) {
	s, pool := testStore(t)
	for _, field := range []string{"questions", "answers", "impact", "cumulative"} {
		t.Run(field, func(t *testing.T) {
			began := dbClock(t, pool)
			d := windowed(t)
			slow := slowJSON(300 * time.Millisecond)
			switch field {
			case "questions":
				d.Questions = slow
			case "answers":
				d.Answers = slow
			case "impact":
				d.Impact = slow
			case "cumulative":
				d.Cumulative = slow
			}
			if err := s.Insert(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			wantWindowNear(t, windowOf(t, pool, d.ID), began, 300*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond)
		})
	}
}

// Rounding the time since the write arrived up, never down, keeps sent_after
// from being later than the truth by a fraction of a microsecond.
func TestMicrosecondsRoundUp(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want int64
	}{
		{0, 0}, {1, 1}, {999, 1}, {time.Microsecond, 1}, {time.Microsecond + 1, 2},
		{1500 * time.Nanosecond, 2}, {300 * time.Millisecond, 300000}, {-1500 * time.Nanosecond, -1},
	} {
		if got := store.MicrosecondsUp(c.d); got != c.want {
			t.Errorf("MicrosecondsUp(%v) = %d, want %d", c.d, got, c.want)
		}
	}
}

// A pool configured for the simple protocol, as one behind a transaction-mode
// pooler is, records decisions as it did before the window was recorded:
// Insert prepares nothing on it. Behind such a pooler the server a connection
// reaches may change between transactions, so a statement the connection
// prepared may be gone, and the name it would prepare under may be taken by
// another client's. Both are emulated on the pool's one connection.
func TestInsertUnderTheSimpleProtocol(t *testing.T) {
	ctx := context.Background()
	pool := func(t *testing.T) (*store.Store, *pgxpool.Pool) {
		t.Helper()
		cfg, err := pgxpool.ParseConfig(testDSN(t) + "&default_query_exec_mode=simple_protocol")
		if err != nil {
			t.Fatal(err)
		}
		cfg.MaxConns = 1
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.Close)
		s := store.New(p)
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return s, p
	}
	// onServer runs sql on the pool's one connection, behind pgx's back, as
	// a pooler's server would have it.
	onServer := func(t *testing.T, p *pgxpool.Pool, sql string) {
		t.Helper()
		conn, err := p.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Conn().PgConn().Exec(ctx, sql).ReadAll(); err != nil {
			t.Fatal(err)
		}
	}
	record := func(t *testing.T, s *store.Store, p *pgxpool.Pool) {
		t.Helper()
		began := dbClock(t, p)
		d := windowed(t)
		if err := s.Insert(ctx, d); err != nil {
			t.Fatalf("the decision was not recorded: %v", err)
		}
		wantWindowNear(t, windowOf(t, p, d.ID), began, 300*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond)
	}

	t.Run("records", func(t *testing.T) {
		s, p := pool(t)
		record(t, s, p)
		record(t, s, p)
	})
	t.Run("after the server dropped its prepared statements", func(t *testing.T) {
		s, p := pool(t)
		record(t, s, p)
		onServer(t, p, "deallocate all")
		record(t, s, p)
	})
	t.Run("where another client prepared its statement's name", func(t *testing.T) {
		s, p := pool(t)
		onServer(t, p, "prepare "+store.InsertDecisionName+" as select 1")
		record(t, s, p)
	})
}

func TestInsertLeavesNewColumnsNull(t *testing.T) {
	s, pool := testStore(t)
	d := sample(uniqueTable(t))
	if err := s.Insert(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	var a, b, c bool
	if err := pool.QueryRow(context.Background(),
		`select login_role is null, rows_affected is null, cumulative is null from stet.decisions where id = $1`, d.ID).Scan(&a, &b, &c); err != nil {
		t.Fatal(err)
	}
	if !a || !b || !c {
		t.Errorf("null flags = %v %v %v", a, b, c)
	}
}

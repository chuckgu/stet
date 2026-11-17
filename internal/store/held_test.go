package store_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/store"
)

func TestInsertRecordsRefused(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	d := sample(uniqueTable(t))
	d.Mode = "enforce"
	d.Refused = ptr("ST001")
	d.Executed = false
	plain := sample(d.TargetTable)
	for _, x := range []*store.Decision{d, plain} {
		if err := s.Insert(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	var mode string
	var refused *string
	if err := pool.QueryRow(ctx, `select mode, refused from stet.decisions where id = $1`, d.ID).Scan(&mode, &refused); err != nil {
		t.Fatal(err)
	}
	if mode != "enforce" || refused == nil || *refused != "ST001" {
		t.Errorf("mode=%q refused=%v", mode, refused)
	}
	if err := pool.QueryRow(ctx, `select refused from stet.decisions where id = $1`, plain.ID).Scan(&refused); err != nil {
		t.Fatal(err)
	}
	if refused != nil {
		t.Errorf("refused = %q, want NULL", *refused)
	}
}

func TestRefusedIsAppendOnly(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	d := sample(uniqueTable(t))
	d.Refused = ptr("ST001")
	if err := s.Insert(ctx, d); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `update stet.decisions set refused = null where id = $1`, d.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "ST004" {
		t.Errorf("update refused: err = %v, want SQLSTATE ST004", err)
	}
}

func sampleHeld() *store.HeldWrite {
	return &store.HeldWrite{
		ID:          store.NewID(),
		DecisionID:  store.NewID(),
		CreatedAt:   time.Now().Add(-time.Second),
		LoginRole:   "agent_a",
		Database:    "stet_test",
		Statement:   "update contacts set title = $1 where id = $2",
		Params:      []*string{ptr("CTO"), nil},
		ParamTypes:  []uint32{25, 0},
		TargetTable: "contacts",
		TargetPK:    ptr("7"),
		RowVersion:  ptr("123"),
		Verdict:     "hold",
		Reasons:     []string{"model:conflict"},
		ExpiresAt:   time.Now().Add(24 * time.Hour),
		Settings:    map[string]string{"TimeZone": "UTC", "DateStyle": "ISO, MDY", "IntervalStyle": "postgres"},
		PlannedRows: ptr(int64(1)),
		RowsExact:   true,
	}
}

func cleanHeld(t *testing.T, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, id string) {
	t.Cleanup(func() { pool.Exec(context.Background(), `delete from stet.held_writes where id = $1`, id) })
}

// insertHeld stores a sampleHeld row, removed when the test ends.
func insertHeld(t *testing.T, s *store.Store, pool *pgxpool.Pool) *store.HeldWrite {
	t.Helper()
	h := sampleHeld()
	cleanHeld(t, pool, h.ID)
	if err := s.InsertHeldWrite(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	return h
}

// heldStatus reads a held write's status and in_transaction.
func heldStatus(t *testing.T, pool *pgxpool.Pool, id string) (string, *bool) {
	t.Helper()
	var status string
	var inTransaction *bool
	if err := pool.QueryRow(context.Background(),
		`select status, in_transaction from stet.held_writes where id = $1`, id).Scan(&status, &inTransaction); err != nil {
		t.Fatal(err)
	}
	return status, inTransaction
}

// statuses are the statuses of spec 3 that a held write can be stored in.
var statuses = []string{"unconfirmed", "pending", "undelivered", "approved", "rejected", "applied", "failed"}

// arrows are the status changes of spec 3.
var arrows = [][2]string{
	{"unconfirmed", "pending"}, {"unconfirmed", "undelivered"},
	{"pending", "approved"}, {"pending", "rejected"},
	{"approved", "applied"}, {"approved", "failed"},
}

// wayTo is the path from unconfirmed to each status along the arrows.
var wayTo = map[string][]string{
	"unconfirmed": nil,
	"pending":     {"pending"},
	"undelivered": {"undelivered"},
	"approved":    {"pending", "approved"},
	"rejected":    {"pending", "rejected"},
	"applied":     {"pending", "approved", "applied"},
	"failed":      {"pending", "approved", "failed"},
}

// heldWithStatus stores a sampleHeld row and moves it to status along the
// arrows, changing nothing else. It is removed when the test ends.
func heldWithStatus(t *testing.T, s *store.Store, pool *pgxpool.Pool, status string) *store.HeldWrite {
	t.Helper()
	h := insertHeld(t, s, pool)
	for _, step := range wayTo[status] {
		if _, err := pool.Exec(context.Background(), `update stet.held_writes set status = $2 where id = $1`, h.ID, step); err != nil {
			t.Fatalf("on the way to %s, %s: %v", status, step, err)
		}
	}
	return h
}

// wantST004 runs sql on id and checks that the guard trigger refuses it
// with message.
func wantST004(t *testing.T, pool *pgxpool.Pool, sql, id, message string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql, id)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "ST004" || pgErr.Message != message {
		t.Errorf("%s: err = %v, want SQLSTATE ST004 %q", sql, err, message)
	}
}

func TestInsertHeldWrite(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	h := sampleHeld()
	cleanHeld(t, pool, h.ID)
	if err := s.InsertHeldWrite(ctx, h); err != nil {
		t.Fatal(err)
	}
	var got store.HeldWrite
	var status, params string
	var types []int64
	var inTransaction *bool
	err := pool.QueryRow(ctx, `
		select decision_id::text, created_at, status, login_role, database, statement, params::text,
		       param_types::int8[], target_table, target_pk, row_version, verdict, reasons,
		       expires_at, settings, row_absent, planned_rows, rows_exact, in_transaction
		from stet.held_writes where id = $1`, h.ID).Scan(
		&got.DecisionID, &got.CreatedAt, &status, &got.LoginRole, &got.Database, &got.Statement, &params,
		&types, &got.TargetTable, &got.TargetPK, &got.RowVersion, &got.Verdict, &got.Reasons,
		&got.ExpiresAt, &got.Settings, &got.RowAbsent, &got.PlannedRows, &got.RowsExact, &inTransaction)
	if err != nil {
		t.Fatal(err)
	}
	if got.DecisionID != h.DecisionID || got.CreatedAt.Sub(h.CreatedAt).Abs() > time.Millisecond ||
		status != "unconfirmed" || got.LoginRole != h.LoginRole || got.Database != h.Database ||
		got.Statement != h.Statement || got.TargetTable != h.TargetTable ||
		got.TargetPK == nil || *got.TargetPK != "7" || got.RowVersion == nil || *got.RowVersion != "123" ||
		got.Verdict != "hold" || !reflect.DeepEqual(got.Reasons, h.Reasons) {
		t.Errorf("got %+v status=%q", got, status)
	}
	if params != `["CTO", null]` {
		t.Errorf("params = %s", params)
	}
	if !reflect.DeepEqual(types, []int64{25, 0}) {
		t.Errorf("param_types = %v", types)
	}
	if got.ExpiresAt.Sub(h.ExpiresAt).Abs() > time.Millisecond {
		t.Errorf("expires_at = %v, want %v", got.ExpiresAt, h.ExpiresAt)
	}
	if want := map[string]string{"TimeZone": "UTC", "DateStyle": "ISO, MDY", "IntervalStyle": "postgres"}; !reflect.DeepEqual(got.Settings, want) {
		t.Errorf("settings = %v, want %v", got.Settings, want)
	}
	if got.RowAbsent || got.PlannedRows == nil || *got.PlannedRows != 1 || !got.RowsExact || inTransaction != nil {
		t.Errorf("row_absent=%v planned_rows=%v rows_exact=%v in_transaction=%v, want false 1 true NULL",
			got.RowAbsent, got.PlannedRows, got.RowsExact, inTransaction)
	}

	bare := sampleHeld()
	bare.Settings, bare.PlannedRows, bare.RowAbsent, bare.RowsExact = nil, nil, true, false
	cleanHeld(t, pool, bare.ID)
	if err := s.InsertHeldWrite(ctx, bare); err != nil {
		t.Fatal(err)
	}
	var noSettings, noPlannedRows, rowAbsent, rowsExact bool
	if err := pool.QueryRow(ctx, `select settings is null, planned_rows is null, row_absent, rows_exact
		from stet.held_writes where id = $1`, bare.ID).Scan(&noSettings, &noPlannedRows, &rowAbsent, &rowsExact); err != nil {
		t.Fatal(err)
	}
	if !noSettings || !noPlannedRows || !rowAbsent || rowsExact {
		t.Errorf("settings null=%v planned_rows null=%v row_absent=%v rows_exact=%v, want true true true false",
			noSettings, noPlannedRows, rowAbsent, rowsExact)
	}
}

func TestInsertHeldWriteLeavesOptionalFieldsNull(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	h := sampleHeld()
	h.Params, h.ParamTypes, h.TargetPK, h.RowVersion, h.Reasons = nil, nil, nil, nil, nil
	h.Verdict = "escalate"
	cleanHeld(t, pool, h.ID)
	if err := s.InsertHeldWrite(ctx, h); err != nil {
		t.Fatal(err)
	}
	var a, b, c, d bool
	var reasons []string
	if err := pool.QueryRow(ctx, `select params is null, param_types is null, target_pk is null, row_version is null, reasons
		from stet.held_writes where id = $1`, h.ID).Scan(&a, &b, &c, &d, &reasons); err != nil {
		t.Fatal(err)
	}
	if !a || !b || !c || !d || reasons == nil || len(reasons) != 0 {
		t.Errorf("null flags = %v %v %v %v reasons=%v", a, b, c, d, reasons)
	}
}

func TestMarkDelivered(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	for _, inTx := range []bool{true, false} {
		h := insertHeld(t, s, pool)
		for i := 0; i < 2; i++ {
			// The second call, with the other value, finds no unconfirmed row.
			if err := s.MarkDelivered(ctx, h.ID, inTx != (i == 1)); err != nil {
				t.Fatalf("in_transaction %v, call %d: %v", inTx, i, err)
			}
			status, inTransaction := heldStatus(t, pool, h.ID)
			if status != "pending" || inTransaction == nil || *inTransaction != inTx {
				t.Errorf("in_transaction %v, call %d: status=%q in_transaction=%v, want pending %v",
					inTx, i, status, inTransaction, inTx)
			}
		}
	}
	if err := s.MarkDelivered(ctx, store.NewID(), true); err != nil {
		t.Errorf("unknown id: %v", err)
	}
}

func TestMarkUndelivered(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	h := insertHeld(t, s, pool)
	for i := 0; i < 2; i++ {
		if err := s.MarkUndelivered(ctx, h.ID); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if status, _ := heldStatus(t, pool, h.ID); status != "undelivered" {
			t.Errorf("status = %q", status)
		}
	}
	if err := s.MarkUndelivered(ctx, store.NewID()); err != nil {
		t.Errorf("unknown id: %v", err)
	}

	delivered := insertHeld(t, s, pool)
	if err := s.MarkDelivered(ctx, delivered.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUndelivered(ctx, delivered.ID); err != nil {
		t.Errorf("pending row: %v", err)
	}
	if status, _ := heldStatus(t, pool, delivered.ID); status != "pending" {
		t.Errorf("pending row: status = %q, want pending", status)
	}
}

// What was held cannot change once stored, whoever updates the row (spec 4,
// correction B4): every column but the nine that approving and applying it
// fill in. in_transaction is written once, as the row becomes pending.
func TestHeldWritesContentIsFixed(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	h := insertHeld(t, s, pool)
	const fixed = "stet.held_writes: what was held cannot change"
	for _, set := range []string{
		"id = gen_random_uuid()",
		"decision_id = gen_random_uuid()",
		"created_at = created_at - interval '1 second'",
		"statement = 'delete from contacts'",
		`params = '["CEO", null]'`,
		"param_types = '{25,23}'",
		"login_role = 'agent_b'",
		"database = 'other_db'",
		"target_table = 'accounts'",
		"target_pk = '8'",
		"row_version = '124'",
		"row_absent = true",
		"verdict = 'escalate'",
		"reasons = '{model:other}'",
		"expires_at = expires_at + interval '1 hour'",
		`settings = '{"TimeZone": "Asia/Seoul", "DateStyle": "ISO, MDY", "IntervalStyle": "postgres"}'`,
		"planned_rows = 2",
		"rows_exact = false",
	} {
		wantST004(t, pool, "update stet.held_writes set "+set+" where id = $1", h.ID, fixed)
	}

	const once = "stet.held_writes: in_transaction is set once, when the row becomes pending"
	wantST004(t, pool, "update stet.held_writes set in_transaction = true where id = $1", h.ID, once)
	// An arrow that is allowed does not carry in_transaction with it.
	wantST004(t, pool, "update stet.held_writes set status = 'undelivered', in_transaction = true where id = $1", h.ID, once)
	if err := s.MarkDelivered(ctx, h.ID, false); err != nil {
		t.Fatal(err)
	}
	wantST004(t, pool, "update stet.held_writes set in_transaction = true where id = $1", h.ID, once)
	wantST004(t, pool, "update stet.held_writes set in_transaction = null where id = $1", h.ID, once)
	// Nor does it carry a change to what was held.
	wantST004(t, pool, "update stet.held_writes set status = 'approved', statement = 'delete from contacts' where id = $1", h.ID, fixed)
	if status, inTransaction := heldStatus(t, pool, h.ID); status != "pending" || inTransaction == nil || *inTransaction {
		t.Errorf("status=%q in_transaction=%v, want pending false", status, inTransaction)
	}
}

// The two guard triggers resolve what they call in pg_catalog whatever the
// search_path of the session that fires them. A function named like one they
// call that fits the row exactly, as to_jsonb(stet.held_writes) does better
// than pg_catalog's to_jsonb(anyelement), would be chosen from that path and
// could let any change through. It runs in a database of its own, so that
// the schema it adds is seen by no other test.
func TestGuardTriggersIgnoreTheCallersSearchPath(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, ownDatabase(t))
	s := store.New(pool)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	h := sampleHeld()
	if err := s.InsertHeldWrite(ctx, h); err != nil {
		t.Fatal(err)
	}
	d := sample(uniqueTable(t))
	if err := s.Insert(ctx, d); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, sql := range []string{
		"create schema hostile",
		`create function hostile.to_jsonb(stet.held_writes) returns jsonb language sql as $$ select '{}'::jsonb $$`,
		`create function hostile.to_jsonb(stet.decisions) returns jsonb language sql as $$ select '{}'::jsonb $$`,
		"set search_path = hostile",
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, c := range []struct{ sql, id, message string }{
		{"update stet.held_writes set statement = 'delete from contacts' where id = $1", h.ID,
			"stet.held_writes: what was held cannot change"},
		{"update stet.decisions set statement = 'delete from contacts' where id = $1", d.ID,
			"stet.decisions is append-only: only label, label_by, labeled_at may change"},
	} {
		_, err := conn.Exec(ctx, c.sql, c.id)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "ST004" || pgErr.Message != c.message {
			t.Errorf("%s under search_path hostile: err = %v, want SQLSTATE ST004 %q", c.sql, err, c.message)
		}
	}
}

// status moves only along the arrows of spec 3; an update that keeps it
// passes, and a row in any status can be deleted.
func TestHeldWritesStatusFollowsTheArrows(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	for _, from := range statuses {
		for _, to := range statuses {
			t.Run(from+"_to_"+to, func(t *testing.T) {
				h := heldWithStatus(t, s, pool, from)
				_, err := pool.Exec(ctx, `update stet.held_writes set status = $2 where id = $1`, h.ID, to)
				if from == to || slices.Contains(arrows, [2]string{from, to}) {
					if err != nil {
						t.Errorf("%s to %s: %v", from, to, err)
					}
				} else {
					var pgErr *pgconn.PgError
					want := "stet.held_writes: status cannot go from " + from + " to " + to
					if !errors.As(err, &pgErr) || pgErr.Code != "ST004" || pgErr.Message != want {
						t.Errorf("%s to %s: err = %v, want SQLSTATE ST004 %q", from, to, err, want)
					}
				}
				tag, err := pool.Exec(ctx, `delete from stet.held_writes where id = $1`, h.ID)
				if err != nil || tag.RowsAffected() != 1 {
					t.Errorf("delete: %v %v", tag, err)
				}
			})
		}
	}
}

// What a reviewer and the executor write moves only with its arrow (plan
// ruling 12): resolved_by, resolved_at, note and max_rows as the row goes
// from pending to approved or rejected; applied_at, rows_affected and error
// as it goes from approved to applied or failed. No one can widen an
// approval or restart its ten minutes afterwards.
func TestHeldWritesResultsMoveWithTheirArrow(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	const (
		resolution = "stet.held_writes: resolved_by, resolved_at, note and max_rows are set once, when the row is approved or rejected"
		result     = "stet.held_writes: applied_at, rows_affected and error are set once, when the row is applied or fails"
	)
	resolving := [][2]string{{"pending", "approved"}, {"pending", "rejected"}}
	applying := [][2]string{{"approved", "applied"}, {"approved", "failed"}}
	columns := []struct {
		column, value, message string
		own                    [][2]string
	}{
		{"resolved_by", "'stet_reviewer'", resolution, resolving},
		{"resolved_at", "clock_timestamp()", resolution, resolving},
		{"note", "'looked at it'", resolution, resolving},
		{"max_rows", "3", resolution, resolving},
		{"applied_at", "clock_timestamp()", result, applying},
		{"rows_affected", "1", result, applying},
		{"error", "'row_changed'", result, applying},
	}
	// One row in each status. Refused updates leave them as they are.
	at := map[string]*store.HeldWrite{}
	for _, status := range statuses {
		at[status] = heldWithStatus(t, s, pool, status)
	}
	for _, c := range columns {
		set := c.column + " = " + c.value
		for _, status := range statuses {
			wantST004(t, pool, "update stet.held_writes set "+set+" where id = $1", at[status].ID, c.message)
		}
		for _, a := range arrows {
			sql := "update stet.held_writes set status = '" + a[1] + "', " + set + " where id = $1"
			if !slices.Contains(c.own, a) {
				wantST004(t, pool, sql, at[a[0]].ID, c.message)
				continue
			}
			h := heldWithStatus(t, s, pool, a[0])
			if _, err := pool.Exec(ctx, sql, h.ID); err != nil {
				t.Errorf("%s: %v", sql, err)
				continue
			}
			var written bool
			if err := pool.QueryRow(ctx, "select "+c.column+" is not null and status = $2 from stet.held_writes where id = $1",
				h.ID, a[1]).Scan(&written); err != nil {
				t.Fatal(err)
			}
			if !written {
				t.Errorf("%s: the row is not %s with %s set", sql, a[1], c.column)
			}
		}
	}
	for _, status := range statuses {
		var got string
		var nulls int
		if err := pool.QueryRow(ctx, `select status, num_nulls(resolved_by, resolved_at, note, max_rows, applied_at, rows_affected, error)
			from stet.held_writes where id = $1`, at[status].ID).Scan(&got, &nulls); err != nil {
			t.Fatal(err)
		}
		if got != status || nulls != 7 {
			t.Errorf("%s row after refused updates: status %s, %d of 7 result columns NULL", status, got, nulls)
		}
	}
}

func TestUsageBySecond(t *testing.T) {
	s, _ := testStore(t)
	ctx := context.Background()
	table := uniqueTable(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	since, before := base, base.Add(10*time.Second)
	mk := func(role string, rows *int64, executed bool, at time.Time) *store.Decision {
		d := sample(table)
		d.LoginRole = ptr(role)
		d.RowsAffected = rows
		d.Executed = executed
		d.CreatedAt = at
		return d
	}
	n := func(v int64) *int64 { return &v }
	for _, d := range []*store.Decision{
		mk("agent_a", n(5), true, base.Add(100*time.Millisecond)),
		mk("agent_a", n(3), true, base.Add(900*time.Millisecond)),
		mk("agent_a", n(2), true, base.Add(time.Second+500*time.Millisecond)),
		mk("agent_a", nil, true, base.Add(3*time.Second)),
		mk("agent_a", n(0), true, base.Add(4*time.Second)),
		mk("agent_a", n(9), false, base.Add(5*time.Second)),
		mk("agent_b", n(9), true, base.Add(6*time.Second)),
		mk("agent_a", n(9), true, base.Add(-time.Millisecond)),
		mk("agent_a", n(9), true, before),
	} {
		if err := s.Insert(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.UsageBySecond(ctx, "agent_a", table, since, before)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.UsageSecond{{Second: base, Rows: 8}, {Second: base.Add(time.Second), Rows: 2}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if !got[i].Second.Equal(want[i].Second) || got[i].Rows != want[i].Rows {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

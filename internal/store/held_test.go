package store_test

import (
	"context"
	"errors"
	"reflect"
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
	if _, err := pool.Exec(ctx, `update stet.held_writes set note = 'looked at it' where id = $1`, h.ID); err != nil {
		t.Errorf("note: %v", err)
	}

	const once = "stet.held_writes: in_transaction is set once, when the row becomes pending"
	wantST004(t, pool, "update stet.held_writes set in_transaction = true where id = $1", h.ID, once)
	if err := s.MarkDelivered(ctx, h.ID, false); err != nil {
		t.Fatal(err)
	}
	wantST004(t, pool, "update stet.held_writes set in_transaction = true where id = $1", h.ID, once)
	wantST004(t, pool, "update stet.held_writes set in_transaction = null where id = $1", h.ID, once)
	if status, inTransaction := heldStatus(t, pool, h.ID); status != "pending" || inTransaction == nil || *inTransaction {
		t.Errorf("status=%q in_transaction=%v, want pending false", status, inTransaction)
	}
}

// status moves only along the arrows of spec 3; an update that keeps it
// passes, and a row in any status can be deleted.
func TestHeldWritesStatusFollowsTheArrows(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	arrows := map[[2]string]bool{
		{"unconfirmed", "pending"}: true, {"unconfirmed", "undelivered"}: true,
		{"pending", "approved"}: true, {"pending", "rejected"}: true,
		{"approved", "applied"}: true, {"approved", "failed"}: true,
	}
	// way is the path from unconfirmed to each status along the arrows.
	way := map[string][]string{
		"unconfirmed": nil,
		"pending":     {"pending"},
		"undelivered": {"undelivered"},
		"approved":    {"pending", "approved"},
		"rejected":    {"pending", "rejected"},
		"applied":     {"pending", "approved", "applied"},
		"failed":      {"pending", "approved", "failed"},
	}
	statuses := []string{"unconfirmed", "pending", "undelivered", "approved", "rejected", "applied", "failed"}
	setStatus := func(id, status string) error {
		_, err := pool.Exec(ctx, `update stet.held_writes set status = $2 where id = $1`, id, status)
		return err
	}
	for _, from := range statuses {
		for _, to := range statuses {
			t.Run(from+"_to_"+to, func(t *testing.T) {
				h := insertHeld(t, s, pool)
				for _, step := range way[from] {
					if err := setStatus(h.ID, step); err != nil {
						t.Fatalf("on the way to %s, %s: %v", from, step, err)
					}
				}
				err := setStatus(h.ID, to)
				if from == to || arrows[[2]string{from, to}] {
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

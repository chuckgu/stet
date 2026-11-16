package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

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
	}
}

func cleanHeld(t *testing.T, pool interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, id string) {
	t.Cleanup(func() { pool.Exec(context.Background(), `delete from stet.held_writes where id = $1`, id) })
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
	err := pool.QueryRow(ctx, `
		select decision_id::text, created_at, status, login_role, database, statement, params::text,
		       param_types::int8[], target_table, target_pk, row_version, verdict, reasons
		from stet.held_writes where id = $1`, h.ID).Scan(
		&got.DecisionID, &got.CreatedAt, &status, &got.LoginRole, &got.Database, &got.Statement, &params,
		&types, &got.TargetTable, &got.TargetPK, &got.RowVersion, &got.Verdict, &got.Reasons)
	if err != nil {
		t.Fatal(err)
	}
	if got.DecisionID != h.DecisionID || got.CreatedAt.Sub(h.CreatedAt).Abs() > time.Millisecond ||
		status != "pending" || got.LoginRole != h.LoginRole || got.Database != h.Database ||
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

func TestMarkUndelivered(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	h := sampleHeld()
	cleanHeld(t, pool, h.ID)
	if err := s.InsertHeldWrite(ctx, h); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.MarkUndelivered(ctx, h.ID); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		var status string
		if err := pool.QueryRow(ctx, `select status from stet.held_writes where id = $1`, h.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "undelivered" {
			t.Errorf("status = %q", status)
		}
	}
	if err := s.MarkUndelivered(ctx, store.NewID()); err != nil {
		t.Errorf("unknown id: %v", err)
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

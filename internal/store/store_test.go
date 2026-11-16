package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/store"
)

func testStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://stet_scram:scram-pw@"+addr+"/stet_test?sslmode=disable")
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

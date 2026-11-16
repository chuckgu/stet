package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/store"
)

func TestAddReadsSums(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	table := uniqueTable(t)
	minute := time.Now().Truncate(time.Minute)
	t.Cleanup(func() { pool.Exec(ctx, `delete from stet.reads where target_table = $1`, table) })

	if err := s.AddReads(ctx, nil); err != nil {
		t.Fatalf("empty: %v", err)
	}
	b := store.ReadBucket{Bucket: minute, LoginRole: "agent_a", Table: table, Statements: 2, Rows: 10, MaxRows: 7}
	if err := s.AddReads(ctx, []store.ReadBucket{b}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddReads(ctx, []store.ReadBucket{
		{Bucket: minute, LoginRole: "agent_a", Table: table, Statements: 1, Rows: 30, MaxRows: 30},
		{Bucket: minute.Add(-time.Minute), LoginRole: "agent_a", Table: table, Statements: 1, Rows: 1, MaxRows: 1},
	}); err != nil {
		t.Fatal(err)
	}
	var st, rows, max int64
	if err := pool.QueryRow(ctx, `select statements, rows, max_rows from stet.reads
		where bucket = $1 and login_role = 'agent_a' and target_table = $2`, minute, table).Scan(&st, &rows, &max); err != nil {
		t.Fatal(err)
	}
	if st != 3 || rows != 40 || max != 30 {
		t.Errorf("got %d %d %d", st, rows, max)
	}
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from stet.reads where target_table = $1`, table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}
}

func TestInsertReadLog(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	table := uniqueTable(t)
	r := &store.ReadLog{
		ID: store.NewID(), AgentID: "enricher", SessionID: "s1",
		Statement: "select * from contacts", StatementFP: "fp",
		CreatedAt: time.Now(), LoginRole: ptr("agent_a"),
		Tables: []string{table, "other"}, Rows: 5000,
	}
	if err := s.InsertReadLog(ctx, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `delete from stet.read_log where id = $1`, r.ID) })
	var tables []string
	var rows int64
	var errNull bool
	if err := pool.QueryRow(ctx, `select target_tables, rows, exec_error is null from stet.read_log where id = $1`, r.ID).Scan(&tables, &rows, &errNull); err != nil {
		t.Fatal(err)
	}
	if len(tables) != 2 || tables[0] != table || rows != 5000 || !errNull {
		t.Errorf("got %v %d %v", tables, rows, errNull)
	}
}

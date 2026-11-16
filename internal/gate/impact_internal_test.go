package gate

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The column check fails closed: a name that is not a column, and a lookup
// that fails, both say no, and the count is then not run.
func TestAllColumnsFailsClosed(t *testing.T) {
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	ctx := context.Background()
	pg, err := pgconn.Connect(ctx, "postgres://stet_scram:scram-pw@"+addr+"/stet_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close(ctx)
	rel := [][]byte{[]byte("pg_catalog.pg_class")}

	for _, c := range []struct {
		rel   [][]byte
		names []string
		want  bool
	}{
		{rel, nil, true},
		{rel, []string{"relname", "relkind"}, true},
		{rel, []string{"relname", "xmin", "ctid"}, true}, // system columns
		{rel, []string{"relname", "no_such_column"}, false},
		{rel, []string{"RELNAME"}, false},                               // names compare as stored
		{[][]byte{[]byte("no_such_table")}, []string{"relname"}, false}, // the lookup fails
	} {
		if got := allColumns(ctx, pg, c.rel, c.names); got != c.want {
			t.Errorf("allColumns(%s, %q) = %v, want %v", c.rel[0], c.names, got, c.want)
		}
	}

	// In a transaction an earlier query aborted, the lookup fails too.
	if err := pg.Exec(ctx, "begin; select 1/0").Close(); err == nil {
		t.Fatal("division by zero succeeded")
	}
	if allColumns(ctx, pg, rel, []string{"relname"}) {
		t.Error("allColumns said yes in an aborted transaction")
	}
	if err := pg.Exec(ctx, "rollback").Close(); err != nil {
		t.Fatal(err)
	}
}

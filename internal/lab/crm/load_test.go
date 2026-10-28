package crm

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// testConn connects to the test database with a fresh schema first on the
// search_path, dropped afterwards.
func testConn(t *testing.T) *pgx.Conn {
	t.Helper()
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, "postgres://stet_scram:scram-pw@"+addr+"/stet_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("lab_test_%d", time.Now().UnixNano())
	for _, sql := range []string{"create schema " + schema, "set search_path = " + schema} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		conn.Exec(ctx, "drop schema "+schema+" cascade")
		conn.Close(ctx)
	})
	return conn
}

func TestLoad(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	d := Generate(Options{Seed: 1, Accounts: 80, Contacts: 500, KoreanShare: 0.3, Now: now})
	if err := Load(ctx, conn, d, false); err != nil {
		t.Fatal(err)
	}

	for table, want := range map[string]int{
		"accounts": len(d.Accounts), "contacts": len(d.Contacts),
		"deals": len(d.Deals), "activities": len(d.Activities),
	} {
		var n int
		if err := conn.QueryRow(ctx, "select count(*) from "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s: %d rows, want %d", table, n, want)
		}
	}

	var name, email string
	conn.QueryRow(ctx, "select name, email from contacts where id = 1").Scan(&name, &email)
	if name != d.Contacts[0].Name || email != d.Contacts[0].Email {
		t.Errorf("contact 1 = %q %q, want %q %q", name, email, d.Contacts[0].Name, d.Contacts[0].Email)
	}

	// Agents insert without ids; identity must continue after the loaded rows.
	var id int
	err := conn.QueryRow(ctx, "insert into contacts (account_id, name, email, status) values (1, 'New Lead', 'new@lead.io', 'active') returning id").Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	if id != len(d.Contacts)+1 {
		t.Errorf("new contact id = %d, want %d", id, len(d.Contacts)+1)
	}

	// Ordinary app constraints are in place.
	if _, err := conn.Exec(ctx, "update contacts set status = 'lost' where id = 1"); err == nil {
		t.Error("CHECK on status is missing")
	}
	if _, err := conn.Exec(ctx, "insert into deals (account_id, stage, amount) values (999999, 'open', 1)"); err == nil {
		t.Error("foreign key on deals.account_id is missing")
	}
	// The experiment's strong constraints are not: they belong to arm A.
	if _, err := conn.Exec(ctx, "insert into contacts (account_id, name, email, status) values (1, 'Dup', $1, 'active')", d.Contacts[0].Email); err != nil {
		t.Errorf("duplicate email was rejected (%v); that constraint belongs to arm A, not the recorded run", err)
	}
}

func TestLoadRefusesExistingTablesUnlessReset(t *testing.T) {
	conn := testConn(t)
	ctx := context.Background()
	d := Generate(Options{Seed: 1, Accounts: 10, Contacts: 30, KoreanShare: 0.3, Now: now})
	if err := Load(ctx, conn, d, false); err != nil {
		t.Fatal(err)
	}
	if err := Load(ctx, conn, d, false); err == nil {
		t.Fatal("second load without reset succeeded; it must not silently mix datasets")
	}
	if err := Load(ctx, conn, d, true); err != nil {
		t.Fatalf("load with reset: %v", err)
	}
}

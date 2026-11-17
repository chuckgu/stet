package bench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/lab/crm"
	"github.com/stetdb/stet/internal/lab/tasks"
)

// testNow is the benchmark's base date.
var testNow = time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

// shared is the package's one template, made by the first test that needs
// it and dropped with its roles by TestMain.
var shared struct {
	once sync.Once
	a    Admin
	n    Names
	name string
	b    tasks.Bench
	made bool // roles or the template may exist
	err  error
}

var clones atomic.Int64

func TestMain(m *testing.M) {
	code := m.Run()
	if shared.made {
		if err := dropAll(context.Background(), shared.a, shared.n); err != nil {
			fmt.Fprintln(os.Stderr, "drop the shared template and its roles:", err)
			if code == 0 {
				code = 1
			}
		}
	}
	os.Exit(code)
}

// testAdmin returns the test server's superuser, or skips the test.
func testAdmin(t *testing.T) Admin {
	t.Helper()
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	return Admin{Addr: addr, User: "postgres", Password: "postgres-pw"}
}

func freshNames() Names { return Names{Prefix: fmt.Sprintf("bt%d_", time.Now().UnixNano())} }

// testNames returns role names of the test's own. When the test ends, the
// databases their owner has and the roles that exist are dropped.
func testNames(t *testing.T) Names {
	t.Helper()
	a := testAdmin(t)
	n := freshNames()
	t.Cleanup(func() {
		if err := dropAll(context.Background(), a, n); err != nil {
			t.Error(err)
		}
	})
	return n
}

// dropAll drops the databases n's owner has, then n's roles that exist,
// with what they own or were granted.
func dropAll(ctx context.Context, a Admin, n Names) error {
	su, err := pgx.Connect(ctx, a.URL("postgres"))
	if err != nil {
		return err
	}
	defer su.Close(ctx)
	dbs, roles, err := Created(ctx, su, n)
	if err != nil {
		return err
	}
	var errs []error
	for _, db := range dbs {
		errs = append(errs, Drop(ctx, a, db))
	}
	for _, r := range roles {
		id := pgx.Identifier{r}.Sanitize()
		for _, sql := range []string{"drop owned by " + id, "drop role " + id} {
			if _, err := su.Exec(ctx, sql); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", sql, err))
			}
		}
	}
	return errors.Join(errs...)
}

// testDataset is the scored stream's base data.
func testDataset() (*crm.Dataset, tasks.BenchOptions) {
	c, o := tasks.BenchPreset(false, testNow)
	return crm.Generate(c), o
}

// testTemplate returns the package's template of the scored stream, made
// once, and the names and bench it was made with.
func testTemplate(t *testing.T) (Admin, Names, string, tasks.Bench) {
	t.Helper()
	a := testAdmin(t)
	shared.once.Do(func() {
		ctx := context.Background()
		shared.a, shared.n = a, freshNames()
		shared.name = fmt.Sprintf("crm_t%d_base", time.Now().UnixNano())
		d, o := testDataset()
		b, err := tasks.BuildBench(d, o)
		if err != nil {
			shared.err = err
			return
		}
		shared.b = b
		su, err := pgx.Connect(ctx, a.URL("postgres"))
		if err != nil {
			shared.err = err
			return
		}
		defer su.Close(ctx)
		shared.made = true
		if err := CreateRoles(ctx, su, shared.n); err != nil {
			shared.err = err
			return
		}
		shared.err = CreateTemplate(ctx, a, shared.n, shared.name, d, b, map[string]any{"test": true})
	})
	if shared.err != nil {
		t.Fatal("make the shared template:", shared.err)
	}
	return shared.a, shared.n, shared.name, shared.b
}

// testClone clones the shared template into a database of the test's own,
// dropped when the test ends.
func testClone(t *testing.T, gate bool) string {
	t.Helper()
	a, n, tmpl, _ := testTemplate(t)
	name := fmt.Sprintf("crm_t%d_c%d", time.Now().UnixNano(), clones.Add(1))
	t.Cleanup(func() {
		if err := Drop(context.Background(), a, name); err != nil {
			t.Error(err)
		}
	})
	if err := Clone(context.Background(), a, n, tmpl, name, gate); err != nil {
		t.Fatal(err)
	}
	return name
}

// connectAs connects to db as role with password, closed when the test ends.
func connectAs(t *testing.T, a Admin, role, password, db string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgURL(a.Addr, role, password, db))
	if err != nil {
		t.Fatalf("connect to %s as %s: %v", db, role, err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	return conn
}

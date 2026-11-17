package bench

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/lab/tasks"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/store"
)

// stetConfig is the fixed config of the test's Stet: enforce, one agent,
// no model, and a table whose writes may change at most three rows.
const stetConfig = `mode: enforce
listen: 127.0.0.1:0
upstream: postgres://%s:%s@%s/%s?sslmode=disable
agents: {roles: [%s]}
model: {adapter: none, on_failure: pass}
tables:
  items: {pk: id, send_fields: [n], impact: {max_rows: 3}, unsupported: reject}
`

// startStet starts Stet in enforce mode inside the test, in front of a
// database of the test's own that holds items(id, n) with ten rows (n = 10).
// Statements go to addr as n.Role(tasks.RoleV); Stet's decisions are in
// database's stet schema. Everything is dropped when the test ends.
func startStet(t *testing.T) (addr, database string, n Names) {
	t.Helper()
	a := testAdmin(t)
	ctx := context.Background()
	n = testNames(t) // its cleanup drops the database too: the owner has it
	su := connectAs(t, a, a.User, a.Password, "postgres")
	if err := CreateRoles(ctx, su, n); err != nil {
		t.Fatal(err)
	}
	database = fmt.Sprintf("crm_t%d_items", time.Now().UnixNano())
	agent := n.Role(tasks.RoleV)
	for _, sql := range []string{
		fmt.Sprintf("create database %s template template0 owner %s", ident(database), ident(n.Owner())),
		fmt.Sprintf("grant create on database %s to %s", ident(database), ident(n.Gate())),
	} {
		if _, err := su.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	owner := connectAs(t, a, n.Owner(), OwnerPassword, database)
	for _, sql := range []string{
		"create table items (id int primary key, n int check (n >= 0))",
		"insert into items select g, 10 from generate_series(1, 10) g",
		"grant select, insert, update, delete on items to " + ident(agent),
	} {
		if _, err := owner.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	cfg, err := config.Parse(fmt.Appendf(nil, stetConfig, n.Gate(), GatePassword, a.Addr, database, agent))
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Stet's log:\n%s", logs)
		}
	})
	pool, err := pgxpool.New(ctx, cfg.Upstream)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	st := store.New(pool)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	d, err := decide.New(cfg.Model)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gate.New(cfg, pool, st, d, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	srv := proxy.New(cfg.UpstreamAddr(), log, g, proxy.Policy{Agents: cfg.AgentRoles, Enforce: true})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	t.Cleanup(func() {
		srv.Close() // waits for the sessions; g.Close and pool.Close run after this
		select {
		case <-served:
		case <-time.After(15 * time.Second):
			t.Error("Stet's proxy did not stop serving")
		}
	})
	return ln.Addr().String(), database, n
}

// lockedBuffer is a log destination read while Stet writes to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

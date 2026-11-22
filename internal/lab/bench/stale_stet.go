package bench

// This file starts Stet inside the process for the stale check (stale.go), wired as cmd/stet wires it.

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/apply"
	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/store"
)

// stetInProcess is Stet started inside this process, wired as cmd/stet's openGate and run wire it, without the
// startup check and undo, which this config does not use.
type stetInProcess struct {
	addr string
	gate *gate.Gate
	// applied gets a value once the executor has run an approved held write and told the gate it is done (enforce).
	applied chan struct{}
	close   func() // stops the proxy, waiting for its sessions, then the gate, the executor and the pool
}

// startInProcess starts Stet with cfg: its pool as Stet's account, its schema brought up to date, the gate, in enforce
// mode the executor of approved held writes, which tells the gate of each write it runs (Gate.Applying), and the
// proxy on a free local port.
func startInProcess(ctx context.Context, cfg *config.Config, log *slog.Logger) (*stetInProcess, error) {
	pool, err := stetPool(ctx, cfg.Upstream)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*stetInProcess, error) {
		pool.Close()
		return nil, err
	}
	st := store.New(pool)
	if err := st.Migrate(ctx); err != nil {
		return fail(fmt.Errorf("create Stet's schema: %w", err))
	}
	d, err := decide.New(cfg.Model)
	if err != nil {
		return fail(err)
	}
	g, err := gate.New(cfg, pool, st, d, log)
	if err != nil {
		return fail(err)
	}
	s := &stetInProcess{gate: g, applied: make(chan struct{}, 1)}
	enforce := cfg.Mode == config.ModeEnforce
	var applier *apply.Applier
	if enforce {
		applier = apply.New(cfg, pool, st, log)
		// g.Applying, as openGate wires it, with word to the check once the gate has been told the write is done: the
		// executor commits the write's applied status before that, so the status alone would let N10's next write
		// arrive while the table's mark still stands.
		applier.Applying = func(role, table string) func() {
			done := g.Applying(role, table)
			return func() {
				done()
				select {
				case s.applied <- struct{}{}:
				default:
				}
			}
		}
		applier.Start()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		g.Close()
		if applier != nil {
			applier.Close()
		}
		return fail(err)
	}
	srv := proxy.New(proxy.Upstream{Addr: cfg.UpstreamAddr()}, log, g, proxy.Policy{Agents: cfg.AgentRoles, Enforce: enforce})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	s.addr = ln.Addr().String()
	s.close = func() {
		srv.Close() // waits for the sessions, which report to the gate as they end
		<-served
		g.Close()
		if applier != nil {
			applier.Close()
		}
		pool.Close()
	}
	return s, nil
}

// stetPool connects Stet's own sessions with the settings cmd/stet's openPool gives them: application_name stet, a
// short lock_timeout, and DateStyle, IntervalStyle and extra_float_digits pinned, which the gate's reads and the
// executor's connections rely on.
func stetPool(ctx context.Context, upstream string) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(upstream)
	if err != nil {
		return nil, err
	}
	pc.ConnConfig.RuntimeParams["application_name"] = "stet"
	pc.ConnConfig.RuntimeParams["lock_timeout"] = "200ms"
	pc.ConnConfig.RuntimeParams["DateStyle"] = "ISO"
	pc.ConnConfig.RuntimeParams["IntervalStyle"] = "postgres"
	pc.ConnConfig.RuntimeParams["extra_float_digits"] = "1"
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect as Stet's account: %w", err)
	}
	return pool, nil
}

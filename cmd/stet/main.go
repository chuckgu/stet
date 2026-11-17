// Command stet runs the Stet proxy in front of Postgres.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/apply"
	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/decide/jev"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/store"
)

const usage = `usage: stet <command> [flags]

commands:
  proxy      run the pgwire proxy (stet proxy -config stet.yaml)
  bench-jev  time single Jev calls (needs TYPESAFE_API_KEY)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	decide.Register("jev", jev.New)

	var err error
	switch os.Args[1] {
	case "proxy":
		err = runProxy(os.Args[2:])
	case "bench-jev":
		err = runBenchJev(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stet:", err)
		os.Exit(1)
	}
}

func runProxy(args []string) error {
	fs := flag.NewFlagSet("proxy", flag.ExitOnError)
	path := fs.String("config", "stet.yaml", "config file")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	return run(ctx, cfg, ln, log)
}

// run serves cfg on ln until ctx is done, then stops in order: the proxy
// first, waiting for its sessions to report their last statements; then the
// gate, which records what it still holds; then, in enforce mode, the
// executor of approved held writes, waiting for the one it is applying; then
// Stet's own pool.
func run(ctx context.Context, cfg *config.Config, ln net.Listener, log *slog.Logger) error {
	g, closeGate, err := openGate(ctx, cfg, log)
	if err != nil {
		ln.Close()
		return err
	}
	// Deferred so it runs last, once the proxy's Close below has returned.
	defer closeGate()

	var pg proxy.Gate
	if g != nil {
		pg = g
	}
	srv := proxy.New(cfg.UpstreamAddr(), log, pg, proxy.Policy{Agents: cfg.AgentRoles, Enforce: cfg.Mode == config.ModeEnforce})
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	closed := make(chan struct{})
	go func() {
		<-ctx.Done()
		srv.Close() // waits for the sessions, which report to the gate as they end
		close(closed)
	}()

	log.Info("stet proxy listening", "listen", ln.Addr(), "upstream", cfg.UpstreamAddr(),
		"mode", cfg.Mode, "agent_roles", cfg.AgentRoles, "gated_tables", len(cfg.Tables), "model", cfg.Model.Adapter)
	err = srv.Serve(ln)
	// Serve returns once the listener is closed, while sessions may still be
	// reporting their last statements to the gate. Wait for Close, which waits
	// for them, before closeGate writes what the gate holds. stop also closes
	// the server when Serve failed by itself.
	stop()
	<-closed
	return err
}

// openGate opens Stet's pool, brings the stet schema up to date, checks the
// database (check), and builds the gate over cfg's tables. In enforce mode it
// also starts the executor of approved held writes (spec-v0.2-approve 7); in
// shadow mode there is none, and no write is executed (rule 6). closeGate
// closes the gate, then the executor, then the pool. With no table there is
// no gate, no executor, no pool and no check: the proxy passes every session
// through, in enforce mode too (spec correction B10), which is logged.
func openGate(ctx context.Context, cfg *config.Config, log *slog.Logger) (g *gate.Gate, closeGate func(), err error) {
	if len(cfg.Tables) == 0 {
		if cfg.Mode == config.ModeEnforce {
			log.Warn("mode is enforce but there are no gated tables: without a gate every session, the agents' " +
				"included, is passed through unchecked; fix: list the tables to gate under tables")
		}
		return nil, func() {}, nil
	}
	pool, err := openPool(ctx, cfg.Upstream)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*gate.Gate, func(), error) {
		pool.Close()
		return nil, nil, err
	}
	st := store.New(pool)
	if err := st.Migrate(ctx); err != nil {
		return fail(err)
	}
	warnings, err := check(ctx, pool, cfg)
	for _, w := range warnings {
		log.Warn(w)
	}
	if err != nil {
		return fail(fmt.Errorf("not starting: %w", err))
	}
	d, err := decide.New(cfg.Model)
	if err != nil {
		return fail(err)
	}
	if g, err = gate.New(cfg, pool, st, d, log); err != nil {
		return fail(err)
	}
	var applier *apply.Applier
	if cfg.Mode == config.ModeEnforce {
		applier = apply.New(cfg, pool, st, log)
		applier.Start()
	}
	return g, func() {
		g.Close()
		if applier != nil {
			applier.Close()
		}
		pool.Close()
	}, nil
}

// openPool connects Stet's own sessions: state reads and decision writes.
// They wait briefly for locks so a client's open transaction cannot stall
// its own write's state assembly for long. DateStyle and IntervalStyle are
// pinned, whatever the database or role sets, so that a binary value Stet
// turns into text reads the same under any session's settings
// (spec-v0.2-approve 4). The executor's connections start with these
// settings too.
func openPool(ctx context.Context, upstream string) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(upstream)
	if err != nil {
		return nil, err
	}
	pc.ConnConfig.RuntimeParams["application_name"] = "stet"
	pc.ConnConfig.RuntimeParams["lock_timeout"] = "200ms"
	pc.ConnConfig.RuntimeParams["DateStyle"] = "ISO"
	pc.ConnConfig.RuntimeParams["IntervalStyle"] = "postgres"
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to upstream as Stet: %w", err)
	}
	return pool, nil
}

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

	if cfg.Mode == config.ModeEnforce {
		return fmt.Errorf("%s: mode enforce is not implemented yet; use shadow", *path)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var g *gate.Gate
	if len(cfg.Tables) > 0 {
		pool, err := openPool(ctx, cfg.Upstream)
		if err != nil {
			return err
		}
		defer pool.Close()
		st := store.New(pool)
		if err := st.Migrate(ctx); err != nil {
			return err
		}
		d, err := decide.New(cfg.Model)
		if err != nil {
			return err
		}
		if g, err = gate.New(cfg, pool, st, d, log); err != nil {
			return err
		}
		defer g.Close() // after the proxy: lets in-flight decisions finish
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	var pg proxy.Gate
	if g != nil {
		pg = g
	}
	srv := proxy.New(cfg.UpstreamAddr(), log, pg, proxy.Policy{Agents: cfg.AgentRoles, Enforce: cfg.Mode == config.ModeEnforce})
	closed := make(chan struct{})
	go func() {
		<-ctx.Done()
		srv.Close()
		close(closed)
	}()

	log.Info("stet proxy listening", "listen", ln.Addr(), "upstream", cfg.UpstreamAddr(),
		"mode", cfg.Mode, "gated_tables", len(cfg.Tables), "model", cfg.Model.Adapter)
	err = srv.Serve(ln)
	// Serve returns once the listener is closed, while sessions may still be
	// reporting their last statements to the gate. Wait for Close, which waits
	// for them, before the deferred g.Close writes what the gate holds. stop
	// also closes the server when Serve failed by itself.
	stop()
	<-closed
	return err
}

// openPool connects Stet's own sessions: state reads and decision writes.
// They wait briefly for locks so a client's open transaction cannot stall
// its own write's state assembly for long.
func openPool(ctx context.Context, upstream string) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(upstream)
	if err != nil {
		return nil, err
	}
	pc.ConnConfig.RuntimeParams["application_name"] = "stet"
	pc.ConnConfig.RuntimeParams["lock_timeout"] = "200ms"
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

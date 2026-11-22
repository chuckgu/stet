// Command stet runs the Stet proxy in front of Postgres.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/apply"
	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/decide/jev"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/lines"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/store"
	"github.com/stetdb/stet/internal/undo"
)

const usage = `usage: stet <command> [flags]

commands:
  init       read the database and write a shadow-mode config for stet proxy (stet init [-config stet.yaml]
             [-listen 127.0.0.1:6432] [-tables a,b,c] [-yes] [-force] [<database address>]);
             the address defaults to DATABASE_URL, which is safer: an argument stays in the shell's history
  proxy      run the pgwire proxy (stet proxy -config stet.yaml [-decisions auto|on|off]);
             -decisions prints a line per judged write on standard output (auto: only on a terminal)
  undo-sql   print the SQL a table owner runs to capture undo.tables (stet undo-sql -config stet.yaml [-drop]),
             or with -grant <role> the grants that let a role review and undo
  bench-jev  time single Jev calls (needs TYPESAFE_API_KEY)
  version    print the version, commit and Go version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	decide.Register("jev", jev.New)

	var err error
	switch os.Args[1] {
	case "init":
		err = runInit(os.Args[2:], os.Stdin, os.Stdout, terminal(os.Stdin), os.Getenv)
	case "proxy":
		err = runProxy(os.Args[2:])
	case "undo-sql":
		err = runUndoSQL(os.Args[2:], os.Stdout)
	case "bench-jev":
		err = runBenchJev(os.Args[2:])
	case "version":
		fmt.Println(versionLine())
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
	decisions := fs.String("decisions", "auto", "print a line per judged write on standard output: auto (only on a terminal), on or off")
	fs.Parse(args)

	show, err := decisionsOn(*decisions, os.Stdout)
	if err != nil {
		return err
	}
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
	var out io.Writer
	if show {
		out = os.Stdout
	}
	return runWith(ctx, cfg, ln, log, out)
}

// decisionsOn reports whether stet proxy prints decision lines: flag is
// auto, on or off; auto means only when out is a character device.
func decisionsOn(flag string, out *os.File) (bool, error) {
	switch flag {
	case "on":
		return true, nil
	case "off":
		return false, nil
	case "auto":
		return terminal(out), nil
	}
	return false, fmt.Errorf("-decisions: %q must be auto, on or off", flag)
}

// terminal reports whether f is a character device, which stet takes for a
// terminal (plan correction P7).
func terminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// onGate, when set, is called by run with the gate it opened (nil when there
// are no gated tables). Only tests set it, to wait for the gate's lookups.
var onGate func(*gate.Gate)

// run serves cfg on ln until ctx is done, then stops in order: the proxy
// first, waiting for its sessions to report their last statements; then the
// gate, which records what it still holds; then, in enforce mode, the
// executor of approved held writes, waiting for the one it is applying; then,
// with undo, the purge of old change records, waiting for a delete in
// progress; then Stet's own pool.
func run(ctx context.Context, cfg *config.Config, ln net.Listener, log *slog.Logger) error {
	return runWith(ctx, cfg, ln, log, nil)
}

// runWith is run with decision lines written to out; nil: none. The lines
// are told by the gate once a decision was recorded (or failed to be), and
// queued for out without waiting for it (rule 1: the output is not part of
// judging). The start banner is the first line, queued as soon as run is about to serve.
func runWith(ctx context.Context, cfg *config.Config, ln net.Listener, log *slog.Logger, out io.Writer) error {
	g, closeGate, err := openGate(ctx, cfg, log)
	if err != nil {
		ln.Close()
		return err
	}
	if onGate != nil {
		onGate(g)
	}
	var lw *lines.Writer
	if out != nil {
		lw = lines.NewWriter(out)
		if g != nil {
			g.Decided = func(d *store.Decision) { lw.Offer(lines.Format(d, time.Local)) }
		}
	}
	// Deferred so it runs last, once the proxy's Close below has returned; the
	// lines' writer closes after the gate, which tells it of its last decisions.
	defer func() {
		closeGate()
		if lw != nil {
			lw.Close()
		}
	}()

	var pg proxy.Gate
	if g != nil {
		pg = g
	}
	srv := proxy.New(proxy.Upstream{Addr: cfg.UpstreamAddr(), TLS: cfg.UpstreamTLS, PlainFallback: cfg.UpstreamPlainFallback}, log, pg, proxy.Policy{Agents: cfg.AgentRoles, Enforce: cfg.Mode == config.ModeEnforce})
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	closed := make(chan struct{})
	go func() {
		<-ctx.Done()
		srv.Close() // waits for the sessions, which report to the gate as they end
		close(closed)
	}()

	for _, w := range transportWarnings(cfg) {
		log.Warn(w)
	}
	if lw != nil {
		lw.Offer(startBanner(cfg, ln.Addr())) // queued like the lines: a stuck output must not stop the start
	}
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

// startBanner is the first line decisions are printed under.
func startBanner(cfg *config.Config, addr net.Addr) string {
	tables := fmt.Sprintf("%d tables", len(cfg.Tables))
	if len(cfg.Tables) == 1 {
		tables = "1 table"
	}
	mode := "(shadow: nothing is blocked)"
	if cfg.Mode == config.ModeEnforce {
		mode = "(enforce)"
	}
	return fmt.Sprintf("stet: watching %s on %s %s", tables, addr, mode)
}

// openGate opens Stet's pool, brings the stet schema up to date, checks the
// database (check), and builds the gate over cfg's tables. With undo it then
// does what Stet does at start for undo (startUndo), in both modes: after the
// check has passed, so that a start the check refuses changes nothing, and
// before the executor starts, so that the held writes it applies are
// captured under the settings of this start. Without undo it warns if
// capture triggers are still installed (warnOfLeftCapture). In enforce mode
// it then starts the executor of approved held writes (spec-v0.2-approve 7);
// in shadow mode there is none, and no write is executed (rule 6). closeGate
// closes the gate, then the executor, then the purge of old change records,
// then the pool.
//
// With no table there is no gate, no executor and no check: the proxy passes
// every session through, in enforce mode too (spec correction B10), which is
// logged. Then the pool is opened, and the schema brought up, only for undo
// (spec 9); without undo there is no pool either.
func openGate(ctx context.Context, cfg *config.Config, log *slog.Logger) (g *gate.Gate, closeGate func(), err error) {
	if len(cfg.Tables) == 0 && cfg.Mode == config.ModeEnforce {
		log.Warn("mode is enforce but there are no gated tables: without a gate every session, the agents' " +
			"included, is passed through unchecked; fix: list the tables to gate under tables")
	}
	if len(cfg.Tables) == 0 && cfg.Undo == nil {
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
	if len(cfg.Tables) > 0 {
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
	}
	var purger *undo.Purger
	if cfg.Undo != nil {
		purger = startUndo(ctx, cfg, pool, st, log)
	} else {
		warnOfLeftCapture(ctx, pool, log)
	}
	var applier *apply.Applier
	if len(cfg.Tables) > 0 && cfg.Mode == config.ModeEnforce {
		applier = apply.New(cfg, pool, st, log)
		applier.Applying = g.Applying // g is set: there are tables
		applier.Start()
	}
	return g, func() {
		if g != nil {
			g.Close()
		}
		if applier != nil {
			applier.Close()
		}
		if purger != nil {
			purger.Close()
		}
		pool.Close()
	}, nil
}

// undoStartTimeout bounds startUndo's work with the database before it
// starts the purge. A variable so a test can run it out of time.
var undoStartTimeout = 10 * time.Second

// startUndo does what Stet does at start when undo is configured
// (spec-v0.2-undo 9), the same in both modes and outside every client's
// session (rule 6): it makes stet.capture_settings say agents.roles and
// undo.max_rows, warns about the tables of undo.tables whose writes are not
// all captured, and starts the purge of change records older than undo.keep,
// which it returns. None of it stops the start: what it cannot do or check is
// a warning that says so.
func startUndo(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, st *store.Store, log *slog.Logger) *undo.Purger {
	ctx, cancel := context.WithTimeout(ctx, undoStartTimeout)
	defer cancel()
	if err := st.SyncCaptureSettings(ctx, cfg.AgentRoles, cfg.Undo.MaxRows); err != nil {
		log.Warn("could not set stet.capture_settings from agents.roles and undo.max_rows; capture keeps the "+
			"settings the table has (with no row: every session, 10000 rows a statement)", "err", err)
	}
	var ts []undo.Table
	err := readOnly(ctx, pool, func(tx pgx.Tx) error {
		var err error
		ts, err = undo.Inspect(ctx, tx, cfg)
		return err
	})
	if err != nil {
		log.Warn("could not check undo.tables, so it is not known whether each table is there, has the capture "+
			"triggers stet undo-sql prints, and has no uncaptured tables referencing it", "err", err)
	}
	for _, w := range undo.Warnings(ts) {
		log.Warn(w)
	}
	p := undo.NewPurger(st, cfg.Undo.Keep, log)
	p.Start()
	return p
}

// warnOfLeftCapture is what Stet does at start, once its pool is open, when
// the config has no undo clause (spec-v0.2-undo 9): it reads from the catalog
// whether capture triggers are still installed, and warns if so. Without the
// clause Stet neither syncs the capture settings nor purges, while the
// triggers go on recording. It does not stop the start: a read that fails is
// a warning that says so, within the time startUndo has.
func warnOfLeftCapture(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, undoStartTimeout)
	defer cancel()
	var ts []undo.Captured
	err := readOnly(ctx, pool, func(tx pgx.Tx) error {
		var err error
		ts, err = undo.CapturedTables(ctx, tx)
		return err
	})
	if err != nil {
		log.Warn("the config has no undo clause, and Stet could not check whether capture triggers are still installed; "+
			"if they are, writes are still recorded and nothing deletes old change records", "err", err)
		return
	}
	if w := undo.CaptureWithoutUndo(ts); w != "" {
		log.Warn(w)
	}
}

// openPool connects Stet's own sessions: state reads and decision writes.
// They wait briefly for locks so a client's open transaction cannot stall
// its own write's state assembly for long. DateStyle and IntervalStyle are
// pinned, whatever the database or role sets, so that a binary value Stet
// turns into text reads the same under any session's settings
// (spec-v0.2-approve 4), and extra_float_digits, so that a binary float
// turns into text without rounding. The executor's connections start with
// these settings too (a copy of the pool's connection config).
func openPool(ctx context.Context, upstream string) (*pgxpool.Pool, error) {
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
		return nil, fmt.Errorf("connect to upstream as Stet: %w", err)
	}
	return pool, nil
}

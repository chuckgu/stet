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

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/proxy"
)

const usage = `usage: stet <command> [flags]

commands:
  proxy    run the pgwire proxy (stet proxy -config stet.yaml)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "proxy":
		err = runProxy(os.Args[2:])
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

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	srv := proxy.New(cfg.UpstreamAddr(), log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	log.Info("stet proxy listening", "listen", ln.Addr(), "upstream", cfg.UpstreamAddr(), "mode", cfg.Mode)
	if len(cfg.Tables) > 0 {
		log.Warn("write gate not implemented yet; every statement passes through unchanged")
	}
	return srv.Serve(ln)
}

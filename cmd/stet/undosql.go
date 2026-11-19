package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/undo"
)

// undoSQLTimeout bounds all of stet undo-sql's work with the database.
const undoSQLTimeout = 30 * time.Second

// runUndoSQL is `stet undo-sql` (spec-v0.2-undo 9): it connects as Stet's
// account with the config's upstream, looks up the tables of undo.tables,
// and writes to out the SQL that installs their capture triggers, or with
// -drop the SQL that removes them. It executes none of it: it only reads the
// catalogs, in a read-only transaction. The install SQL needs PostgreSQL 14
// or later, and on an older server it refuses (ruling R12); -drop does not.
// When the database has no stet.capture() it says to start Stet once, which
// creates the stet schema, and creates nothing itself (ruling U8). On any
// failure it writes nothing to out.
func runUndoSQL(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("undo-sql", flag.ContinueOnError)
	path := fs.String("config", "stet.yaml", "config file")
	drop := fs.Bool("drop", false, "print the SQL that removes the capture triggers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("undo-sql: unexpected arguments %q", fs.Args())
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if cfg.Undo == nil {
		return fmt.Errorf("%s has no undo clause: list the tables to capture under undo.tables", *path)
	}

	ctx, cancel := context.WithTimeout(context.Background(), undoSQLTimeout)
	defer cancel()
	pool, err := openPool(ctx, cfg.Upstream)
	if err != nil {
		return err
	}
	defer pool.Close()
	var (
		role    string
		capture bool
		version int
		ts      []undo.Table
	)
	err = readOnly(ctx, pool, func(tx pgx.Tx) error {
		if !*drop {
			v, err := serverVersionNum(ctx, tx)
			if err != nil {
				return err
			}
			if version = v; undo.RequireServer(version) != nil {
				return nil // refused below; nothing more to read
			}
		}
		if err := tx.QueryRow(ctx, `
			select session_user, exists (select from pg_proc p join pg_namespace n on n.oid = p.pronamespace
			                              where n.nspname = 'stet' and p.proname = 'capture' and p.pronargs = 0)`).
			Scan(&role, &capture); err != nil {
			return err
		}
		if !capture && !*drop {
			return nil
		}
		ts, err = undo.Inspect(ctx, tx, cfg)
		return err
	})
	if err != nil {
		return fmt.Errorf("undo-sql: looking up undo.tables: %w", err)
	}
	if !*drop {
		if err := undo.RequireServer(version); err != nil {
			return fmt.Errorf("undo-sql: %w", err)
		}
	}
	if !capture && !*drop {
		return errors.New("undo-sql: this database has no stet.capture(), which the triggers call: start Stet once " +
			"with this config so that it creates its stet schema, then run stet undo-sql again")
	}

	var sql string
	if *drop {
		sql = undo.DropSQL(ts)
	} else if sql, err = undo.InstallSQL(role, ts); err != nil {
		return err
	}
	_, err = io.WriteString(out, sql)
	return err
}

// serverVersionNum reads the server's server_version_num. A variable so a
// test can stand in for a server older than the test server.
var serverVersionNum = func(ctx context.Context, tx pgx.Tx) (int, error) {
	var v int
	err := tx.QueryRow(ctx, `select current_setting('server_version_num')::int`).Scan(&v)
	return v, err
}

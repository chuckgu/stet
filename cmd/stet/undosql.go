package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
// or later, and on an older server it refuses (spec 9); -drop does not.
// When the database has no stet.capture() it says to start Stet once, which
// creates the stet schema, and creates nothing itself (spec 9).
//
// With -drop and a config that has no undo clause, it finds the tables in the
// catalog instead: every table that carries a trigger calling stet.capture(),
// and it drops each such trigger by its name (undo.DropCapturedSQL). It fails
// when there is none. Without -drop a config without the clause is refused.
//
// With -grant <role> it writes only the grants that let role review and undo
// (undo.GrantSQL), for Stet's account to run, with the undo functions read
// from the catalog; it refuses with -drop or an empty role, and when the
// database has no stet.undo(), saying to start Stet once.
//
// On any failure it writes nothing to out.
func runUndoSQL(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("undo-sql", flag.ContinueOnError)
	path := fs.String("config", "stet.yaml", "config file")
	drop := fs.Bool("drop", false, "print the SQL that removes the capture triggers")
	var grantTo string
	grant := false
	fs.Func("grant", "print only the grants that let `role` read stet.history and undo, for Stet's account to run",
		func(role string) error {
			grantTo, grant = role, true
			return nil
		})
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("undo-sql: unexpected arguments %q", fs.Args())
	case grant && *drop:
		return errors.New("undo-sql: -grant prints only a reviewer's grants and does not go with -drop")
	case grant && grantTo == "":
		return errors.New("undo-sql: -grant needs the name of the role to grant to")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if cfg.Undo == nil && !*drop {
		return fmt.Errorf("%s has no undo clause: list the tables to capture under undo.tables", *path)
	}

	ctx, cancel := context.WithTimeout(context.Background(), undoSQLTimeout)
	defer cancel()
	pool, err := openPool(ctx, cfg.Upstream)
	if err != nil {
		return err
	}
	defer pool.Close()
	if grant {
		return undoGrantSQL(ctx, pool, grantTo, out)
	}
	if cfg.Undo == nil {
		return undoDropCapturedSQL(ctx, pool, *path, out)
	}
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

// undoDropCapturedSQL is stet undo-sql -drop for the config at path, which
// has no undo clause: it reads the tables that carry capture triggers from
// the catalog, in a read-only transaction, and writes undo.DropCapturedSQL's
// output to out. With no such table it fails.
func undoDropCapturedSQL(ctx context.Context, pool *pgxpool.Pool, path string, out io.Writer) error {
	var ts []undo.Captured
	err := readOnly(ctx, pool, func(tx pgx.Tx) error {
		var err error
		ts, err = undo.CapturedTables(ctx, tx)
		return err
	})
	if err != nil {
		return fmt.Errorf("undo-sql: looking for capture triggers: %w", err)
	}
	if len(ts) == 0 {
		return fmt.Errorf("undo-sql: %s has no undo clause, and no table of this database has a trigger that calls "+
			"stet.capture(): there is nothing to drop", path)
	}
	_, err = io.WriteString(out, undo.DropCapturedSQL(ts))
	return err
}

// serverVersionNum reads the server's server_version_num. A variable so a
// test can stand in for a server older than the test server.
var serverVersionNum = func(ctx context.Context, tx pgx.Tx) (int, error) {
	var v int
	err := tx.QueryRow(ctx, `select current_setting('server_version_num')::int`).Scan(&v)
	return v, err
}

// undoGrantSQL is stet undo-sql -grant role: it reads Stet's undo functions
// in a read-only transaction and writes undo.GrantSQL's block to out. Without
// Stet's stet.undo (undo.ErrNoUndo) it says to start Stet once.
func undoGrantSQL(ctx context.Context, pool *pgxpool.Pool, grantTo string, out io.Writer) error {
	var (
		role      string
		functions []string
	)
	err := readOnly(ctx, pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `select session_user`).Scan(&role); err != nil {
			return err
		}
		var err error
		functions, err = undo.ReviewerFunctions(ctx, tx)
		return err
	})
	if errors.Is(err, undo.ErrNoUndo) {
		return errors.New("undo-sql: this database has no stet.undo() of Stet's, which a reviewer calls: start Stet once " +
			"with this config so that it creates its stet schema, then run stet undo-sql again")
	}
	if err != nil {
		return fmt.Errorf("undo-sql: reading the undo functions: %w", err)
	}
	sql, err := undo.GrantSQL(role, grantTo, functions)
	if err != nil {
		return fmt.Errorf("undo-sql: %w", err)
	}
	_, err = io.WriteString(out, sql)
	return err
}

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// undoSQLConfig writes a config file whose upstream is Stet's account in
// database db and whose undo clause is undo (YAML, indented under undo:, or
// "" for none); it returns the file's path.
func undoSQLConfig(t *testing.T, pg, db, undo string) string {
	t.Helper()
	cfg := "listen: 127.0.0.1:0\nupstream: postgres://stet_scram:scram-pw@" + pg + "/" + db + "?sslmode=disable\n"
	if undo != "" {
		cfg += "undo:\n" + undo
	}
	path := filepath.Join(t.TempDir(), "stet.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// stet undo-sql prints the install SQL for the config's undo tables, or with
// -drop the SQL that removes the triggers, and changes nothing in the
// database. A config without undo, a table capture does not take, and a
// database without Stet's schema (U8) are errors, and nothing is printed.
func TestUndoSQLCommand(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	owner := createRole(t, superuser(t, pg), "owner", "nologin")
	db := cleanDatabase(t, pg)
	stetPoolIn(t, pg, db)
	in := connectTo(t, pg, db, "postgres", "postgres-pw")
	mustExec(t, in, "create table public.deals (id int primary key)")
	mustExec(t, in, "alter table public.deals owner to "+owner)
	mustExec(t, in, "create view public.deal_view as select 1 as id")
	unchanged := func() {
		t.Helper()
		var triggers int
		var usage, execute bool
		if err := in.QueryRow(ctx, `select (select count(*) from pg_trigger where tgrelid = 'public.deals'::regclass),
			has_schema_privilege($1, 'stet', 'usage'), has_function_privilege($1, 'stet.capture()', 'execute')`, owner).
			Scan(&triggers, &usage, &execute); err != nil {
			t.Fatal(err)
		}
		if triggers != 0 || usage || execute {
			t.Errorf("the database changed: %d triggers on public.deals, owner's usage on stet %v, execute on stet.capture() %v",
				triggers, usage, execute)
		}
	}

	path := undoSQLConfig(t, pg, db, "  tables:\n    public.deals: {pk: id}\n")
	var out bytes.Buffer
	if err := runUndoSQL([]string{"-config", path}, &out); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"-- 1. Run as stet_scram (the account Stet connects as).\n",
		"GRANT USAGE ON SCHEMA stet TO " + owner + ";\n",
		"GRANT EXECUTE ON FUNCTION stet.capture() TO " + owner + ";\n",
		"-- 2. Run as " + owner + " (the owner of public.deals).\n",
		"SET LOCAL lock_timeout = '5s';\n",
		"CREATE OR REPLACE TRIGGER stet_capture_trunc AFTER TRUNCATE ON public.deals\n",
		"FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('public.deals', 'id');\n",
	} {
		if !strings.Contains(out.String(), p) {
			t.Errorf("stet undo-sql does not print %q:\n%s", p, out.String())
		}
	}
	unchanged()

	out.Reset()
	if err := runUndoSQL([]string{"-config", path, "-drop"}, &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "DROP TRIGGER IF EXISTS stet_capture_ins ON public.deals;\n") ||
		strings.Contains(s, "CREATE") || strings.Contains(s, "GRANT") {
		t.Errorf("stet undo-sql -drop printed:\n%s", s)
	}
	unchanged()

	fails := func(name, path, want string) {
		t.Helper()
		out.Reset()
		err := runUndoSQL([]string{"-config", path}, &out)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error saying %q", name, err, want)
		}
		if out.Len() != 0 {
			t.Errorf("%s: printed %q, want nothing", name, out.String())
		}
	}
	fails("no undo clause", undoSQLConfig(t, pg, db, ""), "undo")
	fails("a view", undoSQLConfig(t, pg, db, "  tables:\n    public.deals: {pk: id}\n    public.deal_view: {pk: id}\n"),
		"public.deal_view is a view")
	unchanged()

	bare := cleanDatabase(t, pg)
	mustExec(t, connectTo(t, pg, bare, "postgres", "postgres-pw"), "create table public.deals (id int primary key)")
	fails("no stet schema", undoSQLConfig(t, pg, bare, "  tables:\n    public.deals: {pk: id}\n"), "start Stet once")
	var schema *string
	if err := connectTo(t, pg, bare, "postgres", "postgres-pw").QueryRow(ctx, `select to_regnamespace('stet')::text`).Scan(&schema); err != nil || schema != nil {
		t.Errorf("stet undo-sql created schema %v (%v)", schema, err)
	}
}

// On a server older than PostgreSQL 14 stet undo-sql refuses to print the
// install SQL (ruling R12) and prints nothing; -drop still prints. The test
// server is 14, so the version read is faked.
func TestUndoSQLRefusesAServerBefore14(t *testing.T) {
	pg := testPG(t)
	db := cleanDatabase(t, pg)
	stetPoolIn(t, pg, db)
	mustExec(t, connectTo(t, pg, db, "postgres", "postgres-pw"), "create table public.deals (id int primary key)")
	path := undoSQLConfig(t, pg, db, "  tables:\n    public.deals: {pk: id}\n")

	real := serverVersionNum
	t.Cleanup(func() { serverVersionNum = real })
	serverVersionNum = func(context.Context, pgx.Tx) (int, error) { return 130011, nil }

	var out bytes.Buffer
	err := runUndoSQL([]string{"-config", path}, &out)
	if err == nil || !strings.Contains(err.Error(), "PostgreSQL 14 or later") || out.Len() != 0 {
		t.Errorf("on PostgreSQL 13: %v, printed %q; want an error saying PostgreSQL 14 or later, and nothing printed", err, out.String())
	}
	out.Reset()
	if err := runUndoSQL([]string{"-config", path, "-drop"}, &out); err != nil ||
		!strings.Contains(out.String(), "DROP TRIGGER IF EXISTS stet_capture_ins ON public.deals;\n") {
		t.Errorf("-drop on PostgreSQL 13: %v, printed:\n%s", err, out.String())
	}
}

// stet undo-sql -grant <role> prints only the block that lets the role review
// and undo, for Stet's account to run, and changes nothing. The role need not
// exist. With -drop, with an empty role, or in a database without Stet's
// schema it fails and prints nothing.
func TestUndoSQLGrant(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db := cleanDatabase(t, pg)
	stetPoolIn(t, pg, db)
	in := connectTo(t, pg, db, "postgres", "postgres-pw")
	mustExec(t, in, "create table public.deals (id int primary key)")
	path := undoSQLConfig(t, pg, db, "  tables:\n    public.deals: {pk: id}\n")

	var out bytes.Buffer
	if err := runUndoSQL([]string{"-config", path, "-grant", "Re viewer"}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, p := range []string{
		"-- Run as stet_scram (the account Stet connects as): lets \"Re viewer\" read stet.history and undo an agent's changes.\n",
		"GRANT USAGE ON SCHEMA stet TO \"Re viewer\";\n",
		"GRANT SELECT ON stet.history TO \"Re viewer\";\n",
		"GRANT EXECUTE ON FUNCTION stet.undo(text, timestamp with time zone, timestamp with time zone, text[], text) TO \"Re viewer\";\n",
		"GRANT EXECUTE ON FUNCTION stet.undo_plan(text, timestamp with time zone, timestamp with time zone, text[]) TO \"Re viewer\";\n",
	} {
		if !strings.Contains(s, p) {
			t.Errorf("stet undo-sql -grant does not print %q:\n%s", p, s)
		}
	}
	if !strings.HasPrefix(s, "-- Run as stet_scram") {
		t.Errorf("stet undo-sql -grant prints more than the grant block:\n%s", s)
	}
	for _, p := range []string{"TRIGGER", "stet.capture", "approve", "reject", "change_sets", "BEGIN"} {
		if strings.Contains(s, p) {
			t.Errorf("stet undo-sql -grant prints %q:\n%s", p, s)
		}
	}
	var functions int
	if err := in.QueryRow(ctx, `select count(*) from pg_proc p join pg_namespace n on n.oid = p.pronamespace
		where n.nspname = 'stet' and p.proname like 'undo%'`).Scan(&functions); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(s, "GRANT EXECUTE ON FUNCTION stet.undo"); n != functions {
		t.Errorf("%d functions granted, want the catalog's %d", n, functions)
	}
	var roles int
	if err := in.QueryRow(ctx, `select count(*) from pg_roles where rolname = 'Re viewer'`).Scan(&roles); err != nil || roles != 0 {
		t.Errorf("stet undo-sql -grant created the role (%d, %v)", roles, err)
	}

	fails := func(name, want string, args ...string) {
		t.Helper()
		out.Reset()
		err := runUndoSQL(args, &out)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want an error saying %q", name, err, want)
		}
		if out.Len() != 0 {
			t.Errorf("%s: printed %q, want nothing", name, out.String())
		}
	}
	fails("-grant with -drop", "-grant", "-config", path, "-grant", "reviewer", "-drop")
	fails("an empty role", "-grant", "-config", path, "-grant", "")
	bare := cleanDatabase(t, pg)
	fails("no stet schema", "start Stet once", "-config", undoSQLConfig(t, pg, bare, "  tables:\n    public.deals: {pk: id}\n"),
		"-grant", "reviewer")
}

// stet undo-sql -grant refuses, as when there is no undo, when the only
// stet.undo in the schema is another role's: the block lists only Stet's
// own functions and must hold Stet's stet.undo.
func TestUndoSQLGrantRefusesAnUndoThatIsNotStets(t *testing.T) {
	pg := testPG(t)
	other := createRole(t, superuser(t, pg), "other", "login password 'other-pw'")
	db := cleanDatabase(t, pg)
	stetPoolIn(t, pg, db)
	mustExec(t, connectTo(t, pg, db, "stet_scram", "scram-pw"), "grant usage, create on schema stet to "+other)
	mustExec(t, connectTo(t, pg, db, "postgres", "postgres-pw"), "drop function stet.undo(text, timestamptz, timestamptz, text[], text)")
	mustExec(t, connectTo(t, pg, db, other, "other-pw"), `create function stet.undo(agent_role text, since timestamptz,
		until timestamptz, tables text[], note text) returns int language sql as 'select 1'`)

	var out bytes.Buffer
	err := runUndoSQL([]string{"-config", undoSQLConfig(t, pg, db, "  tables:\n    public.deals: {pk: id}\n"), "-grant", "reviewer"}, &out)
	if err == nil || !strings.Contains(err.Error(), "start Stet once") || out.Len() != 0 {
		t.Errorf("with another role's stet.undo: %v, printed %q; want the error of a database without undo, and nothing", err, out.String())
	}
}

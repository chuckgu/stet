package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"CREATE TRIGGER stet_capture_trunc AFTER TRUNCATE ON public.deals\n",
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

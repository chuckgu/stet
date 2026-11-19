package undo_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/store"
	"github.com/stetdb/stet/internal/undo"
)

// The test server's accounts: Stet's own (stet_scram) and the agent
// (stet_md5). Table owners are roles the tests create.
const (
	stetRole      = "stet_scram"
	stetPassword  = "scram-pw"
	agentRole     = "stet_md5"
	agentPassword = "md5-pw"
	ownerPassword = "owner-pw"
)

// testAddr is the test server's address. Without one the test is skipped.
func testAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	return addr
}

// execer is what mustExec runs statements on.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// mustExec runs sqls in order and stops the test at the first that fails.
func mustExec(t *testing.T, conn execer, sqls ...string) {
	t.Helper()
	for _, sql := range sqls {
		if _, err := conn.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// connectAs connects to database db as user, whatever its name holds. The
// connection is closed when the test ends.
func connectAs(t *testing.T, db, user, password string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig("postgres://" + testAddr(t) + "/" + db + "?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password = user, password
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connect to %s as %q: %v", db, user, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func superuser(t *testing.T, db string) *pgx.Conn {
	t.Helper()
	return connectAs(t, db, "postgres", "postgres-pw")
}

// newOwner creates a login role (to own the test's tables, or to review),
// named prefix and a unique suffix, and drops it, with what it owns or was granted in
// stet_test, when the test ends. Create it before the database it owns
// tables in (ownDatabase), so that the database, with the rights granted in
// it, is dropped first.
func newOwner(t *testing.T, prefix string) string {
	t.Helper()
	name := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	id := pgx.Identifier{name}.Sanitize()
	su := superuser(t, "stet_test")
	mustExec(t, su, "create role "+id+" login password '"+ownerPassword+"'")
	t.Cleanup(func() {
		ctx := context.Background()
		for _, sql := range []string{"drop owned by " + id, "drop role " + id} {
			if _, err := su.Exec(ctx, sql); err != nil {
				t.Errorf("cleanup: %s: %v", sql, err)
			}
		}
	})
	return name
}

// ownDatabase creates a database of the test's own, owned by Stet's
// account, with the stet schema in place, and drops it when the test ends.
// It returns the database's name and Stet's pool to it. Capture triggers and
// the row of stet.capture_settings belong to one database, and the packages'
// tests share stet_test.
func ownDatabase(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("stet_undo_db_%d", time.Now().UnixNano())
	su := superuser(t, "stet_test")
	mustExec(t, su, "create database "+name+" template template0 owner "+stetRole)
	t.Cleanup(func() {
		if _, err := su.Exec(context.Background(), "drop database if exists "+name+" with (force)"); err != nil {
			t.Errorf("cleanup: drop database %s: %v", name, err)
		}
	})
	pool, err := pgxpool.New(ctx, "postgres://"+stetRole+":"+stetPassword+"@"+testAddr(t)+"/"+name+"?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.New(pool).Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return name, pool
}

// inspect is undo.Inspect of tables (key -> key column) as q sees them.
func inspect(t *testing.T, q store.Querier, tables map[string]string) []undo.Table {
	t.Helper()
	ts, err := undo.Inspect(context.Background(), q, &config.Config{Undo: &config.Undo{Tables: tables}})
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func byKey(ts []undo.Table) map[string]undo.Table {
	m := map[string]undo.Table{}
	for _, tb := range ts {
		m[tb.Key] = tb
	}
	return m
}

// section is one part of what InstallSQL or DropSQL printed: the role it
// is headed to run as, and its text.
type section struct{ role, sql string }

// sections splits printed SQL into its parts, each headed
// "-- N. Run as <role> (...)." where role is an SQL identifier.
func sections(t *testing.T, sql string) []section {
	t.Helper()
	var out []section
	n := 0
	for _, line := range strings.SplitAfter(sql, "\n") {
		if rest, ok := strings.CutPrefix(line, fmt.Sprintf("-- %d. Run as ", n+1)); ok {
			n++
			out = append(out, section{role: unquote(t, rest)})
		}
		if len(out) > 0 {
			out[len(out)-1].sql += line
		}
	}
	return out
}

// unquote reads the identifier at the start of s.
func unquote(t *testing.T, s string) string {
	t.Helper()
	if !strings.HasPrefix(s, `"`) {
		name, _, _ := strings.Cut(s, " ")
		return name
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] == '"' {
			if i+1 < len(s) && s[i+1] == '"' {
				b.WriteByte('"')
				i++
				continue
			}
			return b.String()
		}
		b.WriteByte(s[i])
	}
	t.Fatalf("unterminated identifier in %q", s)
	return ""
}

// runAs runs s in database db as its role; passwords maps roles to theirs.
func runAs(t *testing.T, db string, s section, passwords map[string]string) {
	t.Helper()
	pw, ok := passwords[s.role]
	if !ok {
		t.Fatalf("no password for role %q", s.role)
	}
	conn := connectAs(t, db, s.role, pw)
	if _, err := conn.Exec(context.Background(), s.sql); err != nil {
		t.Fatalf("as %q: %v\n%s", s.role, err, s.sql)
	}
}

// installByHand creates the four triggers of spec 4 on table (its name
// as SQL) as owner, dropping any of theirs first.
func installByHand(t *testing.T, owner execer, table, key, pk string) {
	t.Helper()
	call := "EXECUTE FUNCTION stet.capture('" + strings.ReplaceAll(key, "'", "''") + "', '" + strings.ReplaceAll(pk, "'", "''") + "')"
	mustExec(t, owner,
		"DROP TRIGGER IF EXISTS stet_capture_ins ON "+table,
		"DROP TRIGGER IF EXISTS stet_capture_upd ON "+table,
		"DROP TRIGGER IF EXISTS stet_capture_del ON "+table,
		"DROP TRIGGER IF EXISTS stet_capture_trunc ON "+table,
		"CREATE TRIGGER stet_capture_ins AFTER INSERT ON "+table+" REFERENCING NEW TABLE AS stet_new FOR EACH STATEMENT "+call,
		"CREATE TRIGGER stet_capture_upd AFTER UPDATE ON "+table+" REFERENCING OLD TABLE AS stet_old NEW TABLE AS stet_new FOR EACH STATEMENT "+call,
		"CREATE TRIGGER stet_capture_del AFTER DELETE ON "+table+" REFERENCING OLD TABLE AS stet_old FOR EACH STATEMENT "+call,
		"CREATE TRIGGER stet_capture_trunc AFTER TRUNCATE ON "+table+" FOR EACH STATEMENT "+call)
}

// captureTriggers counts the triggers named stet_capture_* on relid.
func captureTriggers(t *testing.T, conn *pgx.Conn, relid uint32) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(),
		`select count(*) from pg_trigger where tgrelid = $1 and tgname like 'stet\_capture\_%'`, relid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// recordedSet is what capture recorded for one statement on a table.
type recordedSet struct {
	TargetTable, KeyColumn, Op string
	Skipped                    *string
	Lines                      int
}

// recordedOn returns the change sets capture recorded for relid, in order.
func recordedOn(t *testing.T, stet *pgxpool.Pool, relid uint32) []recordedSet {
	t.Helper()
	rows, err := stet.Query(context.Background(), `
		select s.target_table, s.key_column, s.op, s.skipped, (select count(*) from stet.changes c where c.set_id = s.id)
		from stet.change_sets s where s.relid = $1 order by s.id`, relid)
	if err != nil {
		t.Fatal(err)
	}
	sets, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (recordedSet, error) {
		var s recordedSet
		err := r.Scan(&s.TargetTable, &s.KeyColumn, &s.Op, &s.Skipped, &s.Lines)
		return s, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return sets
}

// wantRecorded checks that the agent's writes to relid were each recorded
// under key and pk, with values (one line) or, for truncate, as a marker.
func wantRecorded(t *testing.T, stet *pgxpool.Pool, relid uint32, key, pk string, ops ...string) {
	t.Helper()
	sets := recordedOn(t, stet, relid)
	if len(sets) != len(ops) {
		t.Fatalf("%s: %d sets recorded (%+v), want %v", key, len(sets), sets, ops)
	}
	for i, s := range sets {
		wantSkipped, wantLines := "", 1
		if ops[i] == "truncate" {
			wantSkipped, wantLines = "truncate", 0
		}
		skipped := ""
		if s.Skipped != nil {
			skipped = *s.Skipped
		}
		if s.TargetTable != key || s.KeyColumn != pk || s.Op != ops[i] || skipped != wantSkipped || s.Lines != wantLines {
			t.Errorf("%s: set %d is %q/%q %s skipped %q with %d lines; want %q/%q %s skipped %q with %d",
				key, i, s.TargetTable, s.KeyColumn, s.Op, skipped, s.Lines, key, pk, ops[i], wantSkipped, wantLines)
		}
	}
}

func relidOf(t *testing.T, conn *pgx.Conn, table string) uint32 {
	t.Helper()
	var relid uint32
	if err := conn.QueryRow(context.Background(), `select $1::regclass::oid`, table).Scan(&relid); err != nil {
		t.Fatal(err)
	}
	return relid
}

// Inspect finds each table as Stet's account resolves its key, the way the
// gate names it in SQL, and says why capture cannot be installed on it.
func TestInspectFindsTablesAndProblems(t *testing.T) {
	db, _ := ownDatabase(t)
	ctx := context.Background()
	su := superuser(t, db)
	mustExec(t, su,
		`create table public.with_pk (id int primary key, name text)`,
		`create table public.with_unique (id int, code text not null unique)`,
		`create table public.with_unique_index (code text not null)`,
		`create unique index on public.with_unique_index (code)`,
		`create table public.with_include (code text not null, extra int)`,
		`create unique index on public.with_include (code) include (extra)`,
		`create table public.three_parts (id int primary key)`,
		`create table public."Mixed Case" (id int primary key)`,
		`create schema other`,
		`grant usage on schema other to `+stetRole,
		`create table other.found_by_path (id int primary key)`,
		// Stet's account has no USAGE on this schema: the catalogs still show the table.
		`create schema locked`,
		`create table locked.t (id int primary key)`,
		`create schema hidden`,
		`create table hidden.not_on_path (id int primary key)`,
		`create view public.a_view as select 1 as id`,
		`create materialized view public.a_matview as select 1 as id`,
		`create table public.parted (id int primary key) partition by range (id)`,
		`create table public.parted_1 partition of public.parted for values from (0) to (10)`,
		`create foreign data wrapper undo_test_fdw`,
		`create server undo_test_server foreign data wrapper undo_test_fdw`,
		`create foreign table public.a_foreign (id int) server undo_test_server`,
		`create table public.inh_parent (id int primary key)`,
		`create table public.inh_child () inherits (public.inh_parent)`,
		`alter table public.inh_child add primary key (id)`,
		`create table public.missing_column (id int primary key)`,
		`create table public.no_key (id int, name text)`,
		`create table public.nullable_unique (code text unique)`,
		`create table public.partial_unique (code text not null)`,
		`create unique index on public.partial_unique (code) where code <> ''`,
		`create table public.expr_unique (code text not null)`,
		`create unique index on public.expr_unique (lower(code))`,
		`create table public.deferrable_unique (code text not null unique deferrable)`,
		`create table public.deferrable_pk (id int primary key deferrable initially deferred)`,
		`create table public.two_column_pk (a int, b int, primary key (a, b))`,
		`create table public.dup (id int primary key)`,
		`create table public.pk_with_nul (id int primary key)`,
		// Another session's temporary table, which a qualified key reaches.
		`create temporary table a_temp (id int primary key)`)
	var tempSchema string
	if err := su.QueryRow(ctx, `select nspname from pg_namespace where oid = pg_my_temp_schema()`).Scan(&tempSchema); err != nil {
		t.Fatal(err)
	}

	// Stet's own session, with other on its search_path.
	stet := connectAs(t, db, stetRole, stetPassword)
	mustExec(t, stet, `set search_path = "$user", public, other`)

	tables := map[string]string{
		"with_pk":                  "id",
		"public.with_unique":       "code",
		"with_unique_index":        "code",
		"with_include":             "code",
		db + ".public.three_parts": "id",
		"Mixed Case":               "id",
		"found_by_path":            "id",
		"locked.t":                 "id",
		"not_on_path":              "id",
		"nope":                     "id",
		"other_db.public.with_pk":  "id",
		"a.b.c.d":                  "id",
		"public..with_pk":          "id",
		"public.a_view":            "id",
		"a_matview":                "id",
		"parted":                   "id",
		"parted_1":                 "id",
		"a_foreign":                "id",
		"inh_parent":               "id",
		"inh_child":                "id",
		"missing_column":           "idx",
		"no_key":                   "id",
		"nullable_unique":          "code",
		"partial_unique":           "code",
		"expr_unique":              "code",
		"deferrable_unique":        "code",
		"deferrable_pk":            "id",
		"two_column_pk":            "a",
		"dup":                      "id",
		"public.dup":               "id",
		"with\x00nul":              "id",
		"pk_with_nul":              "i\x00d",
		tempSchema + ".a_temp":     "id",
	}
	ts := inspect(t, stet, tables)
	if len(ts) != len(tables) {
		t.Fatalf("%d tables, want %d", len(ts), len(tables))
	}
	if !slices.IsSortedFunc(ts, func(a, b undo.Table) int { return strings.Compare(a.Key, b.Key) }) {
		t.Errorf("not sorted by key: %v", ts)
	}
	got := byKey(ts)
	for key, pk := range tables {
		if got[key].PK != pk {
			t.Errorf("%q: PK %q, want %q", key, got[key].PK, pk)
		}
	}

	found := map[string][3]string{ // key -> schema, name, owner
		"with_pk":                  {"public", "with_pk", "postgres"},
		"public.with_unique":       {"public", "with_unique", "postgres"},
		"with_unique_index":        {"public", "with_unique_index", "postgres"},
		"with_include":             {"public", "with_include", "postgres"},
		db + ".public.three_parts": {"public", "three_parts", "postgres"},
		"Mixed Case":               {"public", "Mixed Case", "postgres"},
		"found_by_path":            {"other", "found_by_path", "postgres"},
		"locked.t":                 {"locked", "t", "postgres"},
	}
	for key, want := range found {
		tb := got[key]
		var relid uint32
		if err := su.QueryRow(ctx, `select c.oid from pg_class c join pg_namespace n on n.oid = c.relnamespace
			where n.nspname = $1 and c.relname = $2`, want[0], want[1]).Scan(&relid); err != nil {
			t.Fatal(err)
		}
		if tb.Problem != "" || [3]string{tb.Schema, tb.Name, tb.Owner} != want || tb.OID != relid {
			t.Errorf("%q: %+v; want %v, OID %d and no problem", key, tb, want, relid)
		}
	}

	for _, key := range []string{"not_on_path", "nope", "other_db.public.with_pk", "a.b.c.d", "public..with_pk", "with\x00nul"} {
		tb := got[key]
		if tb.Problem == "" || tb.Schema != "" || tb.Name != "" || tb.Owner != "" || tb.OID != 0 {
			t.Errorf("%q: %+v; want not found, with a problem", key, tb)
		}
	}

	for key, parts := range map[string][]string{
		"public.a_view":        {"public.a_view", "is a view"},
		"a_matview":            {"public.a_matview", "is a materialized view"},
		"parted":               {"public.parted", "is a partitioned table"},
		"parted_1":             {"public.parted_1", "is a partition of public.parted"},
		"a_foreign":            {"public.a_foreign", "is a foreign table"},
		"inh_parent":           {"public.inh_parent", "inheritance children", "public.inh_child"},
		"inh_child":            {"public.inh_child", "is an inheritance child of public.inh_parent"},
		"missing_column":       {"public.missing_column", "no column idx"},
		"pk_with_nul":          {"public.pk_with_nul", "no column"},
		tempSchema + ".a_temp": {tempSchema + ".a_temp", "is a temporary table"},
		"no_key":               {"public.no_key", "column id", "is not a key"},
		"nullable_unique":      {"public.nullable_unique", "column code", "is not a key"},
		"partial_unique":       {"public.partial_unique", "column code", "is not a key"},
		"expr_unique":          {"public.expr_unique", "column code", "is not a key"},
		"deferrable_unique":    {"public.deferrable_unique", "column code", "is not a key"},
		"deferrable_pk":        {"public.deferrable_pk", "column id", "is the table's primary key", "deferrable"},
		"two_column_pk":        {"public.two_column_pk", "column a", "is not a key"},
		"dup":                  {"public.dup", "undo.tables.public.dup", "the same table"},
		"public.dup":           {"public.dup", "undo.tables.dup", "the same table"},
	} {
		tb := got[key]
		if tb.Schema == "" || tb.OID == 0 || tb.Owner != "postgres" {
			t.Errorf("%q: %+v; want it found", key, tb)
		}
		for _, p := range parts {
			if !strings.Contains(tb.Problem, p) {
				t.Errorf("%q: problem %q does not say %q", key, tb.Problem, p)
			}
		}
		if len(tb.Triggers) != 0 || len(tb.Children) != 0 {
			t.Errorf("%q: triggers %v, children %v; want none looked at for a table with a problem", key, tb.Triggers, tb.Children)
		}
	}
	if p := got["deferrable_pk"].Problem; strings.Contains(p, "must be the table's primary key") {
		t.Errorf("deferrable_pk: problem %q asks for the primary key it is", p)
	}
}

// Triggers says, one line each, what is missing or wrong with the four
// capture triggers, and nothing when the four are as spec 4 prints them.
func TestInspectReportsTriggerState(t *testing.T) {
	db, stet := ownDatabase(t)
	su := superuser(t, db)
	mustExec(t, su,
		`create table public.deals (id int primary key, name text)`,
		`create function public.not_capture() returns trigger language plpgsql as $$ begin return null; end $$`)
	tables := map[string]string{"deals": "id"}
	triggers := func() []string {
		t.Helper()
		ts := inspect(t, stet, tables)
		if len(ts) != 1 || ts[0].Problem != "" {
			t.Fatalf("Inspect = %+v, want deals without a problem", ts)
		}
		return ts[0].Triggers
	}

	got := triggers()
	names := []string{"stet_capture_ins", "stet_capture_upd", "stet_capture_del", "stet_capture_trunc"}
	if len(got) != len(names) {
		t.Fatalf("without triggers: %q, want the four missing", got)
	}
	for i, name := range names {
		if got[i] != name+" is missing" {
			t.Errorf("without triggers, line %d is %q, want %q", i, got[i], name+" is missing")
		}
	}

	installByHand(t, su, "public.deals", "deals", "id")
	if got := triggers(); len(got) != 0 {
		t.Fatalf("installed as printed: %q, want nothing", got)
	}

	const capture = ` execute function stet.capture('deals', 'id')`
	for _, c := range []struct {
		name, trigger string
		sqls          []string
		want          string
	}{
		{"missing", "stet_capture_del", []string{`drop trigger stet_capture_del on public.deals`}, "stet_capture_del is missing"},
		{"disabled", "stet_capture_upd", []string{`alter table public.deals disable trigger stet_capture_upd`}, "is disabled"},
		{"replica only", "stet_capture_upd", []string{`alter table public.deals enable replica trigger stet_capture_upd`}, "replica"},
		{"another function", "stet_capture_ins", []string{`drop trigger stet_capture_ins on public.deals`,
			`create trigger stet_capture_ins after insert on public.deals referencing new table as stet_new for each statement
			 execute function public.not_capture('deals', 'id')`}, "calls public.not_capture() rather than stet.capture()"},
		{"another key", "stet_capture_upd", []string{`drop trigger stet_capture_upd on public.deals`,
			`create trigger stet_capture_upd after update on public.deals referencing old table as stet_old new table as stet_new
			 for each statement execute function stet.capture('contacts', 'id')`}, "passes ('contacts', 'id') rather than ('deals', 'id')"},
		{"another key column", "stet_capture_del", []string{`drop trigger stet_capture_del on public.deals`,
			`create trigger stet_capture_del after delete on public.deals referencing old table as stet_old
			 for each statement execute function stet.capture('deals', 'name')`}, "passes ('deals', 'name') rather than ('deals', 'id')"},
		{"three arguments", "stet_capture_trunc", []string{`drop trigger stet_capture_trunc on public.deals`,
			`create trigger stet_capture_trunc after truncate on public.deals
			 for each statement execute function stet.capture('deals', 'id', 'x')`}, "passes ('deals', 'id', 'x') rather than ('deals', 'id')"},
		{"no arguments", "stet_capture_trunc", []string{`drop trigger stet_capture_trunc on public.deals`,
			`create trigger stet_capture_trunc after truncate on public.deals
			 for each statement execute function stet.capture()`}, "passes () rather than ('deals', 'id')"},
		{"before", "stet_capture_trunc", []string{`drop trigger stet_capture_trunc on public.deals`,
			`create trigger stet_capture_trunc before truncate on public.deals for each statement` + capture}, "fires BEFORE rather than AFTER"},
		{"for each row", "stet_capture_del", []string{`drop trigger stet_capture_del on public.deals`,
			`create trigger stet_capture_del after delete on public.deals referencing old table as stet_old for each row` + capture},
			"fires FOR EACH ROW rather than FOR EACH STATEMENT"},
		{"short transition table names", "stet_capture_upd", []string{`drop trigger stet_capture_upd on public.deals`,
			`create trigger stet_capture_upd after update on public.deals referencing old table as o new table as n
			 for each statement` + capture},
			"says REFERENCING OLD TABLE AS o NEW TABLE AS n rather than REFERENCING OLD TABLE AS stet_old NEW TABLE AS stet_new"},
		{"no transition table", "stet_capture_ins", []string{`drop trigger stet_capture_ins on public.deals`,
			`create trigger stet_capture_ins after insert on public.deals for each statement` + capture},
			"says no REFERENCING rather than REFERENCING NEW TABLE AS stet_new"},
		{"more events", "stet_capture_trunc", []string{`drop trigger stet_capture_trunc on public.deals`,
			`create trigger stet_capture_trunc after insert or truncate on public.deals for each statement` + capture},
			"fires on INSERT OR TRUNCATE rather than on TRUNCATE alone"},
		{"column list", "stet_capture_upd", []string{`drop trigger stet_capture_upd on public.deals`,
			`create trigger stet_capture_upd after update of name on public.deals for each statement` + capture},
			"fires only on UPDATE OF some columns"},
		{"when", "stet_capture_ins", []string{`drop trigger stet_capture_ins on public.deals`,
			`create trigger stet_capture_ins after insert on public.deals referencing new table as stet_new
			 for each statement when (false)` + capture}, "has a WHEN condition"},
		// CREATE OR REPLACE TRIGGER cannot replace a constraint trigger.
		{"constraint trigger", "stet_capture_ins", []string{`drop trigger stet_capture_ins on public.deals`,
			`create constraint trigger stet_capture_ins after insert on public.deals for each row` + capture},
			"stet_capture_ins is a constraint trigger, which the output of stet undo-sql cannot replace; the table's owner must drop it"},
		// Another trigger that calls stet.capture() would record each write again.
		{"another capture trigger", "old_capture", []string{
			`create trigger old_capture after insert on public.deals referencing new table as stet_new for each statement` + capture},
			"old_capture also calls stet.capture(), so a write it fires on is recorded twice"},
		{"another capture trigger, quoted", `"Old Capture"`, []string{
			`create trigger "Old Capture" after delete on public.deals for each row` + capture},
			`"Old Capture" also calls stet.capture()`},
	} {
		t.Run(c.name, func(t *testing.T) {
			installByHand(t, su, "public.deals", "deals", "id")
			// What installByHand does not undo.
			if drop, ok := map[string]string{
				"another capture trigger":         `drop trigger old_capture on public.deals`,
				"another capture trigger, quoted": `drop trigger "Old Capture" on public.deals`,
			}[c.name]; ok {
				t.Cleanup(func() { mustExec(t, su, drop) })
			}
			mustExec(t, su, c.sqls...)
			got := triggers()
			if len(got) == 0 {
				t.Fatal("nothing reported")
			}
			said := false
			for _, line := range got {
				if !strings.HasPrefix(line, c.trigger+" ") {
					t.Errorf("line %q is not about %s", line, c.trigger)
				}
				said = said || strings.Contains(line, c.want)
			}
			if !said {
				t.Errorf("%q does not say %q", got, c.want)
			}
		})
	}

	// Another trigger that calls stet.capture() is only reported: the
	// install output names only the four, and leaves it in place.
	installByHand(t, su, "public.deals", "deals", "id")
	mustExec(t, su, `create trigger old_capture after insert on public.deals referencing new table as stet_new for each statement`+capture)
	sql, err := undo.InstallSQL(stetRole, inspect(t, stet, tables))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "old_capture") {
		t.Errorf("the install SQL names another trigger:\n%s", sql)
	}
	for _, sec := range sections(t, sql) {
		runAs(t, db, sec, map[string]string{stetRole: stetPassword, "postgres": "postgres-pw"})
	}
	if got := triggers(); len(got) != 1 || !strings.HasPrefix(got[0], "old_capture also calls stet.capture()") {
		t.Errorf("after installing: %q, want old_capture still reported", got)
	}
}

// Children are the tables that reference the table with a foreign key whose
// ON DELETE action changes their rows (CASCADE, SET NULL, SET DEFAULT), as
// stet.undo_warning's children_not_captured, and are not in undo.tables,
// compared by table, not by name. An ON UPDATE action does not count: undoing
// the parent's update cascades back.
func TestInspectFindsUncapturedChildren(t *testing.T) {
	db, stet := ownDatabase(t)
	su := superuser(t, db)
	mustExec(t, su,
		`create table public.deals (id int primary key)`,
		`create table public.deal_notes (id int primary key, deal_id int references public.deals on delete cascade)`,
		`create table public."Deal Tags" (deal_id int references public.deals on delete cascade)`,
		`create table public.deal_links (deal_id int references public.deals on update set null,
		                                 other_id int references public.deals on delete cascade)`,
		`create table public.deal_defaults (deal_id int default 0 references public.deals on delete set default)`,
		`create table public.deal_refs (deal_id int references public.deals)`,
		`create table public.deal_restricts (deal_id int references public.deals on delete restrict on update restrict)`,
		`create table public.deal_updates (deal_id int references public.deals on update cascade)`,
		`create table public.deal_update_nulls (deal_id int references public.deals on update set null on delete no action)`,
		`create table public.deal_nulls (deal_id int references public.deals on delete set null)`,
		`create schema archive`,
		`create table archive.deals_seen (deal_id int references public.deals on delete cascade)`)

	got := byKey(inspect(t, stet, map[string]string{"deals": "id", "public.deal_notes": "id"}))
	want := []string{"archive.deals_seen", `public."Deal Tags"`, "public.deal_defaults", "public.deal_links", "public.deal_nulls"}
	if !slices.Equal(got["deals"].Children, want) {
		t.Errorf("children of deals: %q, want %q", got["deals"].Children, want)
	}
	if len(got["public.deal_notes"].Children) != 0 {
		t.Errorf("children of deal_notes: %q, want none", got["public.deal_notes"].Children)
	}

	got = byKey(inspect(t, stet, map[string]string{"deals": "id"}))
	want = []string{"archive.deals_seen", `public."Deal Tags"`, "public.deal_defaults", "public.deal_links", "public.deal_notes", "public.deal_nulls"}
	if !slices.Equal(got["deals"].Children, want) {
		t.Errorf("children of deals without deal_notes in undo.tables: %q, want %q", got["deals"].Children, want)
	}
}

// What InstallSQL prints, exactly: the grants for Stet's account, then one
// transaction per owner, in owner order, with notes of uncaptured children.
func TestInstallSQLPrintsGrantsThenOneTransactionPerOwner(t *testing.T) {
	ts := []undo.Table{
		{Key: "deals", PK: "id", Schema: "public", Name: "deals", Owner: "crm_owner", OID: 1, Children: []string{"public.deal_tags"}},
		{Key: "contacts", PK: "email", Schema: "sales", Name: "contacts", Owner: "Sales Owner", OID: 2},
		{Key: "public.deal_notes", PK: "id", Schema: "public", Name: "deal_notes", Owner: "crm_owner", OID: 3},
		{Key: "stet_log", PK: "id", Schema: "public", Name: "user", Owner: "stet_scram", OID: 4},
	}
	got, err := undo.InstallSQL("stet_scram", ts)
	if err != nil {
		t.Fatal(err)
	}
	const want = `-- 1. Run as stet_scram (the account Stet connects as).
GRANT USAGE ON SCHEMA stet TO "Sales Owner";
GRANT EXECUTE ON FUNCTION stet.capture() TO "Sales Owner";
GRANT USAGE ON SCHEMA stet TO crm_owner;
GRANT EXECUTE ON FUNCTION stet.capture() TO crm_owner;

-- 2. Run as "Sales Owner" (the owner of sales.contacts).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE TRIGGER stet_capture_ins AFTER INSERT ON sales.contacts
  REFERENCING NEW TABLE AS stet_new
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('contacts', 'email');
CREATE OR REPLACE TRIGGER stet_capture_upd AFTER UPDATE ON sales.contacts
  REFERENCING OLD TABLE AS stet_old NEW TABLE AS stet_new
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('contacts', 'email');
CREATE OR REPLACE TRIGGER stet_capture_del AFTER DELETE ON sales.contacts
  REFERENCING OLD TABLE AS stet_old
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('contacts', 'email');
CREATE OR REPLACE TRIGGER stet_capture_trunc AFTER TRUNCATE ON sales.contacts
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('contacts', 'email');
COMMIT;

-- 3. Run as crm_owner (the owner of public.deals, public.deal_notes).
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE TRIGGER stet_capture_ins AFTER INSERT ON public.deals
  REFERENCING NEW TABLE AS stet_new
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
CREATE OR REPLACE TRIGGER stet_capture_upd AFTER UPDATE ON public.deals
  REFERENCING OLD TABLE AS stet_old NEW TABLE AS stet_new
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
CREATE OR REPLACE TRIGGER stet_capture_del AFTER DELETE ON public.deals
  REFERENCING OLD TABLE AS stet_old
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
CREATE OR REPLACE TRIGGER stet_capture_trunc AFTER TRUNCATE ON public.deals
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
CREATE OR REPLACE TRIGGER stet_capture_ins AFTER INSERT ON public.deal_notes
  REFERENCING NEW TABLE AS stet_new
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('public.deal_notes', 'id');
CREATE OR REPLACE TRIGGER stet_capture_upd AFTER UPDATE ON public.deal_notes
  REFERENCING OLD TABLE AS stet_old NEW TABLE AS stet_new
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('public.deal_notes', 'id');
CREATE OR REPLACE TRIGGER stet_capture_del AFTER DELETE ON public.deal_notes
  REFERENCING OLD TABLE AS stet_old
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('public.deal_notes', 'id');
CREATE OR REPLACE TRIGGER stet_capture_trunc AFTER TRUNCATE ON public.deal_notes
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('public.deal_notes', 'id');
COMMIT;
-- note: public.deal_tags references public.deals with ON DELETE CASCADE, SET NULL or SET DEFAULT and is not under undo.tables:
--       when a row of public.deals is deleted, the rows of public.deal_tags that reference it are deleted or have their
--       reference changed, which is not captured: undoing the delete does not restore them.

-- 4. Run as stet_scram (the owner of public."user").
BEGIN;
SET LOCAL lock_timeout = '5s';
CREATE OR REPLACE TRIGGER stet_capture_ins AFTER INSERT ON public."user"
  REFERENCING NEW TABLE AS stet_new
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('stet_log', 'id');
CREATE OR REPLACE TRIGGER stet_capture_upd AFTER UPDATE ON public."user"
  REFERENCING OLD TABLE AS stet_old NEW TABLE AS stet_new
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('stet_log', 'id');
CREATE OR REPLACE TRIGGER stet_capture_del AFTER DELETE ON public."user"
  REFERENCING OLD TABLE AS stet_old
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('stet_log', 'id');
CREATE OR REPLACE TRIGGER stet_capture_trunc AFTER TRUNCATE ON public."user"
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('stet_log', 'id');
COMMIT;
`
	if got != want {
		t.Errorf("InstallSQL printed:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "DROP") {
		t.Errorf("the install SQL drops a trigger, which waits for readers of the table")
	}

	// When Stet's account owns every table there is nothing to grant.
	got, err = undo.InstallSQL("stet_scram", ts[3:])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "-- 1. Run as stet_scram (the owner of public.\"user\").\nBEGIN;\n") || strings.Contains(got, "GRANT") {
		t.Errorf("with Stet's account the only owner:\n%s", got)
	}
}

// DropSQL removes the four triggers of each table found, per owner; a table
// not found has nothing to drop.
func TestDropSQLPrintsOneTransactionPerOwner(t *testing.T) {
	ts := []undo.Table{
		{Key: "deals", PK: "id", Schema: "public", Name: "deals", Owner: "crm_owner", OID: 1},
		{Key: "nope", PK: "id", Problem: "not found"},
		{Key: "a_view", PK: "id", Schema: "public", Name: "a_view", Owner: "crm_owner", OID: 2, Problem: "public.a_view is a view"},
	}
	const want = `-- 1. Run as crm_owner (the owner of public.a_view, public.deals).
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TRIGGER IF EXISTS stet_capture_ins ON public.a_view;
DROP TRIGGER IF EXISTS stet_capture_upd ON public.a_view;
DROP TRIGGER IF EXISTS stet_capture_del ON public.a_view;
DROP TRIGGER IF EXISTS stet_capture_trunc ON public.a_view;
DROP TRIGGER IF EXISTS stet_capture_ins ON public.deals;
DROP TRIGGER IF EXISTS stet_capture_upd ON public.deals;
DROP TRIGGER IF EXISTS stet_capture_del ON public.deals;
DROP TRIGGER IF EXISTS stet_capture_trunc ON public.deals;
COMMIT;
-- note: undo.tables.nope is not found; there is nothing to drop on it.
`
	if got := undo.DropSQL(ts); got != want {
		t.Errorf("DropSQL printed:\n%s\nwant:\n%s", got, want)
	}
}

// Run as printed, the first part as Stet's account and each other part as
// the table's owner, the SQL installs the four triggers, which capture
// accepts: each of the agent's writes is recorded. It can be run again.
// DropSQL's output removes them.
func TestInstallSQLRuns(t *testing.T) {
	a, b := newOwner(t, "stet_undo_a"), newOwner(t, "stet_undo_b")
	db, stet := ownDatabase(t)
	ctx := context.Background()
	su := superuser(t, db)
	mustExec(t, su,
		`create table public.deals (id int primary key, name text)`,
		`create table public.deal_notes (id bigint primary key, deal_id int references public.deals on delete cascade, name text)`,
		`create table public.contacts (email text not null unique, name text)`,
		`create table public.stet_owned (id int primary key, name text)`,
		`alter table public.deals owner to `+a,
		`alter table public.deal_notes owner to `+a,
		`alter table public.contacts owner to `+b,
		`alter table public.stet_owned owner to `+stetRole,
		`grant select, insert, update, delete, truncate on public.deals, public.deal_notes, public.contacts, public.stet_owned to `+agentRole)
	tables := map[string]string{"deals": "id", "public.deal_notes": "id", "contacts": "email", "stet_owned": "id"}

	sql, err := undo.InstallSQL(stetRole, inspect(t, stet, tables))
	if err != nil {
		t.Fatal(err)
	}
	secs := sections(t, sql)
	var roles []string
	for _, s := range secs {
		roles = append(roles, s.role)
	}
	if want := []string{stetRole, stetRole, a, b}; !slices.Equal(roles, want) {
		t.Fatalf("parts run as %q, want %q:\n%s", roles, want, sql)
	}
	grants := secs[0].sql
	for _, owner := range []string{a, b} {
		for _, g := range []string{"GRANT USAGE ON SCHEMA stet TO " + owner + ";", "GRANT EXECUTE ON FUNCTION stet.capture() TO " + owner + ";"} {
			if strings.Count(grants, g) != 1 {
				t.Errorf("the grants do not say %q once:\n%s", g, grants)
			}
		}
	}
	if strings.Contains(grants, "TO "+stetRole) {
		t.Errorf("the grants give Stet's own account:\n%s", grants)
	}

	passwords := map[string]string{stetRole: stetPassword, a: ownerPassword, b: ownerPassword}
	for range 2 {
		for _, s := range secs {
			runAs(t, db, s, passwords)
		}
	}
	for _, tb := range inspect(t, stet, tables) {
		if tb.Problem != "" || len(tb.Triggers) != 0 {
			t.Errorf("%s after installing: problem %q, triggers %q", tb.Key, tb.Problem, tb.Triggers)
		}
	}

	agent := connectAs(t, db, agentRole, agentPassword)
	for key, pk := range tables {
		table := map[string]string{"deals": "public.deals", "public.deal_notes": "public.deal_notes",
			"contacts": "public.contacts", "stet_owned": "public.stet_owned"}[key]
		mustExec(t, agent,
			"insert into "+table+" ("+pk+", name) values ('1', 'x')",
			"update "+table+" set name = 'y'",
			"delete from "+table)
		ops := []string{"insert", "update", "delete"}
		if key == "contacts" {
			mustExec(t, agent, "truncate "+table)
			ops = append(ops, "truncate")
		}
		wantRecorded(t, stet, relidOf(t, su, table), key, pk, ops...)
	}

	for _, s := range sections(t, undo.DropSQL(inspect(t, stet, tables))) {
		runAs(t, db, s, passwords)
	}
	for _, tb := range inspect(t, stet, tables) {
		if n := captureTriggers(t, su, tb.OID); n != 0 || len(tb.Triggers) != 4 {
			t.Errorf("%s after dropping: %d capture triggers, Triggers %q", tb.Key, n, tb.Triggers)
		}
	}
	var n int
	if err := su.QueryRow(ctx, `select count(*) from stet.change_sets`).Scan(&n); err != nil || n != 13 {
		t.Errorf("%d change sets (%v), want the 13 written before dropping", n, err)
	}
}

// Any table with a Problem fails InstallSQL, which then prints nothing; the
// error names each such table and why.
func TestInstallSQLRefusesWhatItCannotCapture(t *testing.T) {
	ts := []undo.Table{
		{Key: "deals", PK: "id", Schema: "public", Name: "deals", Owner: "crm_owner", OID: 1},
		{Key: "deal_view", PK: "id", Schema: "public", Name: "deal_view", Owner: "crm_owner", OID: 2,
			Problem: "public.deal_view is a view, not an ordinary table"},
		{Key: "nope", PK: "id", Problem: "Stet's account finds no table \"nope\" on its search_path"},
	}
	sql, err := undo.InstallSQL("stet_scram", ts)
	if err == nil || sql != "" {
		t.Fatalf("InstallSQL = %q, %v; want no SQL and an error", sql, err)
	}
	for _, p := range []string{"undo.tables.deal_view", "public.deal_view is a view", "undo.tables.nope", `finds no table "nope"`} {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error %q does not say %q", err, p)
		}
	}
	if strings.Contains(err.Error(), "undo.tables.deals") {
		t.Errorf("error %q names a table that has no problem", err)
	}

	// From the database too.
	db, stet := ownDatabase(t)
	mustExec(t, superuser(t, db), `create table public.deals (id int primary key)`, `create view public.deal_view as select 1 as id`)
	sql, err = undo.InstallSQL(stetRole, inspect(t, stet, map[string]string{"deals": "id", "deal_view": "id"}))
	if err == nil || sql != "" || !strings.Contains(err.Error(), "undo.tables.deal_view") || !strings.Contains(err.Error(), "view") {
		t.Errorf("InstallSQL = %q, %v; want no SQL and an error naming deal_view", sql, err)
	}
}

// Names that need quoting go into the SQL quoted, as identifiers or as
// literals, and the output runs as printed: a schema and table with
// capitals and a space, a key column that is a reserved word, a table key
// with quotes and a backslash, an owner with capitals and spaces, a table
// named by a reserved word, and a name with a line break that would end a
// comment.
func TestInstallSQLQuotesNames(t *testing.T) {
	owner := newOwner(t, "Stet Undo Owner")
	db, stet := ownDatabase(t)
	su := superuser(t, db)
	o := pgx.Identifier{owner}.Sanitize()
	const (
		notes  = `"Crm"."Deal Notes"`
		bobs   = `"Crm"."Bob's ""Deals"" \back"`
		broken = "\"Crm\".\"line\ncreate table broke_out (); --\""
		user   = `public."user"`
	)
	mustExec(t, su,
		`create schema "Crm"`,
		`grant usage on schema "Crm" to `+o+`, `+agentRole,
		`create table `+notes+` ("order" int primary key, "it's" text)`,
		`create table `+bobs+` ("key ""col""" text primary key, v text)`,
		`create table `+broken+` (id int primary key)`,
		`create table `+user+` (id int primary key)`)
	for _, table := range []string{notes, bobs, broken, user} {
		mustExec(t, su, "alter table "+table+" owner to "+o, "grant insert on "+table+" to "+agentRole)
	}
	tables := map[string]string{
		"Crm.Deal Notes":                          "order",
		`Crm.Bob's "Deals" \back`:                 `key "col"`,
		"Crm.line\ncreate table broke_out (); --": "id",
		"user": "id",
	}

	sql, err := undo.InstallSQL(stetRole, inspect(t, stet, tables))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"GRANT USAGE ON SCHEMA stet TO " + o + ";",
		"-- 2. Run as " + o + " (the owner of ",
		`ON "Crm"."Deal Notes"`,
		`stet.capture('Crm.Deal Notes', 'order')`,
		`ON "Crm"."Bob's ""Deals"" \back"`,
		`stet.capture(E'Crm.Bob''s "Deals" \\back', 'key "col"')`,
		`ON public."user"`,
		`\x0acreate table broke_out (); --`,
	} {
		if !strings.Contains(sql, p) {
			t.Errorf("the SQL does not say %s:\n%s", p, sql)
		}
	}
	secs := sections(t, sql)
	if len(secs) != 2 || secs[1].role != owner {
		t.Fatalf("parts %+v, want the grants and one for %q", secs, owner)
	}
	passwords := map[string]string{stetRole: stetPassword, owner: ownerPassword}
	for _, s := range secs {
		runAs(t, db, s, passwords)
	}
	var brokeOut bool
	if err := su.QueryRow(context.Background(), `select to_regclass('public.broke_out') is not null`).Scan(&brokeOut); err != nil {
		t.Fatal(err)
	}
	if brokeOut {
		t.Error("a name broke out of its comment: public.broke_out was created")
	}
	for _, tb := range inspect(t, stet, tables) {
		if len(tb.Triggers) != 0 {
			t.Errorf("%q after installing: %q", tb.Key, tb.Triggers)
		}
	}

	agent := connectAs(t, db, agentRole, agentPassword)
	mustExec(t, agent,
		`insert into `+notes+` values (1, 'x')`,
		`insert into `+bobs+` values ('k', 'x')`,
		`insert into `+broken+` values (1)`,
		`insert into `+user+` values (1)`)
	for key, table := range map[string]string{"Crm.Deal Notes": notes, `Crm.Bob's "Deals" \back`: bobs,
		"Crm.line\ncreate table broke_out (); --": broken, "user": user} {
		wantRecorded(t, stet, relidOf(t, su, table), key, tables[key], "insert")
	}
}

// lockFixture makes public.contacts and public.deals, owned by a new owner,
// gives the agent rights on deals, runs the grants of the install SQL, and
// returns the database, Stet's pool, a superuser connection, the owner, and
// the owner's part with lock_timeout 1s in place of the printed 5s (which it
// checks), to keep the waits short.
func lockFixture(t *testing.T) (db string, stet *pgxpool.Pool, su *pgx.Conn, owner, ownerSQL string) {
	t.Helper()
	owner = newOwner(t, "stet_undo_lock")
	db, stet = ownDatabase(t)
	su = superuser(t, db)
	mustExec(t, su,
		`create table public.deals (id int primary key)`,
		`create table public.contacts (id int primary key)`,
		`alter table public.deals owner to `+owner,
		`alter table public.contacts owner to `+owner,
		`grant select, insert on public.deals to `+agentRole)
	sql, err := undo.InstallSQL(stetRole, inspect(t, stet, map[string]string{"contacts": "id", "deals": "id"}))
	if err != nil {
		t.Fatal(err)
	}
	secs := sections(t, sql)
	if len(secs) != 2 {
		t.Fatalf("%d parts, want 2:\n%s", len(secs), sql)
	}
	runAs(t, db, secs[0], map[string]string{stetRole: stetPassword})
	const printed = "SET LOCAL lock_timeout = '5s';\n"
	if strings.Count(secs[1].sql, printed) != 1 {
		t.Fatalf("the owner's part does not say %q once:\n%s", printed, secs[1].sql)
	}
	return db, stet, su, owner, strings.Replace(secs[1].sql, printed, "SET LOCAL lock_timeout = '1s';\n", 1)
}

// openTx begins a transaction as the agent and runs sql in it; it is rolled
// back when the test ends unless the caller ends it first.
func openTx(t *testing.T, db, sql string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := connectAs(t, db, agentRole, agentPassword).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	mustExec(t, tx, sql)
	return tx
}

// wantTimeout runs the owner's part and checks it fails with 55P03 after
// about 1s.
func wantTimeout(t *testing.T, db, owner, ownerSQL string) {
	t.Helper()
	conn := connectAs(t, db, owner, ownerPassword)
	start := time.Now()
	_, err := conn.Exec(context.Background(), ownerSQL)
	waited := time.Since(start)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("the owner's part: %v, want 55P03", err)
	}
	if waited < 900*time.Millisecond || waited > 4*time.Second {
		t.Errorf("waited %s, want about 1s", waited)
	}
}

// triggerState is what Inspect says of each table's triggers.
func triggerState(t *testing.T, stet *pgxpool.Pool) map[string][]string {
	t.Helper()
	m := map[string][]string{}
	for _, tb := range inspect(t, stet, map[string]string{"contacts": "id", "deals": "id"}) {
		m[tb.Key] = tb.Triggers
	}
	return m
}

// The owner's part waits for a lock at most as long as lock_timeout says (5s
// as printed; 1s here) while a transaction that wrote to one of its tables is
// open, then fails with 55P03 and changes nothing: on a first install no
// trigger is created, and on a rerun the triggers stay as they were, also on
// the table whose triggers were replaced before the wait.
func TestInstallSQLTimesOutOnALockedTable(t *testing.T) {
	db, stet, su, owner, ownerSQL := lockFixture(t)
	ctx := context.Background()

	writer := openTx(t, db, `insert into public.deals values (1)`)
	wantTimeout(t, db, owner, ownerSQL)
	for _, table := range []string{"public.contacts", "public.deals"} {
		if n := captureTriggers(t, su, relidOf(t, su, table)); n != 0 {
			t.Errorf("first install: %s has %d capture triggers, want none", table, n)
		}
	}
	if err := writer.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Installed, then one trigger of each table disabled.
	mustExec(t, connectAs(t, db, owner, ownerPassword), ownerSQL)
	mustExec(t, su,
		`alter table public.contacts disable trigger stet_capture_ins`,
		`alter table public.deals disable trigger stet_capture_upd`)
	before := triggerState(t, stet)
	if len(before["contacts"]) != 1 || len(before["deals"]) != 1 {
		t.Fatalf("before the rerun: %q, want one disabled trigger each", before)
	}
	openTx(t, db, `insert into public.deals values (2)`)
	wantTimeout(t, db, owner, ownerSQL)
	if after := triggerState(t, stet); !slices.Equal(after["contacts"], before["contacts"]) || !slices.Equal(after["deals"], before["deals"]) {
		t.Errorf("rerun: triggers %q after the timeout, want them as before, %q", after, before)
	}
}

// A rerun of the install SQL does not wait for transactions that only read
// its tables (spec 9: CREATE OR REPLACE TRIGGER takes a lock that blocks
// writes, not reads, where DROP TRIGGER blocks both), and afterwards the
// triggers are as printed again.
func TestInstallSQLRerunsAlongsideAReader(t *testing.T) {
	db, stet, su, owner, ownerSQL := lockFixture(t)
	ctx := context.Background()
	mustExec(t, connectAs(t, db, owner, ownerPassword), ownerSQL)
	mustExec(t, su,
		`alter table public.deals disable trigger stet_capture_upd`,
		`create or replace trigger stet_capture_del after delete on public.deals referencing old table as o
		 for each statement execute function stet.capture('deals', 'id')`)
	if got := triggerState(t, stet)["deals"]; len(got) != 2 {
		t.Fatalf("before the rerun: %q, want two problems", got)
	}

	reader := openTx(t, db, `select count(*) from public.deals`)
	if _, err := connectAs(t, db, owner, ownerPassword).Exec(ctx, ownerSQL); err != nil {
		t.Fatalf("rerun alongside a reader: %v", err)
	}
	for key, lines := range triggerState(t, stet) {
		if len(lines) != 0 {
			t.Errorf("%s after the rerun: %q, want the four as printed", key, lines)
		}
	}
	if _, err := reader.Exec(ctx, `select count(*) from public.deals`); err != nil {
		t.Errorf("the reader after the rerun: %v", err)
	}
}

// Each warning is one line naming the table key: a table not found, its
// trigger problems (with the fix), uncaptured children, any other problem.
func TestWarnings(t *testing.T) {
	const other = "old_capture also calls stet.capture(), so a write it fires on is recorded twice"
	ts := []undo.Table{
		{Key: "contacts", PK: "id", Schema: "public", Name: "contacts", Owner: "crm_owner", OID: 1},
		{Key: "deal_view", PK: "id", Schema: "public", Name: "deal_view", Owner: "crm_owner", OID: 2,
			Problem: "public.deal_view is a view, not an ordinary table"},
		{Key: "deals", PK: "id", Schema: "public", Name: "deals", Owner: "crm_owner", OID: 3,
			Triggers: []string{"stet_capture_ins is missing", "stet_capture_upd is disabled", other},
			Children: []string{"public.deal_tags", `public."Deal Notes"`}},
		{Key: "nope", PK: "id", Problem: "Stet's account finds no table \"nope\" on its search_path"},
		{Key: "notes", PK: "id", Schema: "public", Name: "notes", Owner: "crm_owner", OID: 4, Triggers: []string{other}},
	}
	got := undo.Warnings(ts)
	const (
		fix     = "run stet undo-sql and have the table's owner run its output"
		dropFix = "have the table's owner drop it; the output of stet undo-sql does not"
	)
	want := [][]string{
		{"undo.tables.deal_view", "public.deal_view is a view, not an ordinary table", "not captured"},
		{"undo.tables.deals", "stet_capture_ins is missing", "stet_capture_upd is disabled", fix},
		{"undo.tables.deals", "old_capture also calls stet.capture()", dropFix},
		{"undo.tables.deals", "public.deal_tags", `public."Deal Notes"`, "ON DELETE", "not captured"},
		{"undo.tables.nope", `finds no table "nope"`, "not captured"},
		{"undo.tables.notes", "old_capture also calls stet.capture()", dropFix},
	}
	if len(got) != len(want) {
		t.Fatalf("%d warnings, want %d: %q", len(got), len(want), got)
	}
	for i, parts := range want {
		if strings.Contains(got[i], "\n") {
			t.Errorf("warning %d has more than one line: %q", i, got[i])
		}
		for _, p := range parts {
			if !strings.Contains(got[i], p) {
				t.Errorf("warning %d %q does not say %q", i, got[i], p)
			}
		}
	}
	if !strings.HasSuffix(got[1], fix) {
		t.Errorf("the trigger warning does not end with the fix: %q", got[1])
	}
	if strings.Contains(got[1], "old_capture") || strings.Contains(got[2], "stet_capture_ins") {
		t.Errorf("the four triggers and another capture trigger share a warning: %q, %q", got[1], got[2])
	}
	for _, i := range []int{2, 5} {
		if !strings.HasSuffix(got[i], dropFix) {
			t.Errorf("the warning about another capture trigger does not end with its fix: %q", got[i])
		}
	}
	for _, w := range got {
		if strings.Contains(w, "contacts") {
			t.Errorf("a warning about a table with nothing wrong: %q", w)
		}
	}
	if len(undo.Warnings(ts[:1])) != 0 {
		t.Errorf("warnings for a table with nothing wrong")
	}
}

// Undo needs PostgreSQL 14 or later: the install SQL replaces triggers with
// CREATE OR REPLACE TRIGGER (spec 9), and 14 is the only version tested.
// There is no older server to run against; the check takes the version.
func TestRequireServer(t *testing.T) {
	for _, v := range []int{140000, 140018, 150007, 170002} {
		if err := undo.RequireServer(v); err != nil {
			t.Errorf("RequireServer(%d) = %v, want nil", v, err)
		}
	}
	for v, says := range map[int]string{130011: "PostgreSQL 13", 120000: "PostgreSQL 12", 90624: "PostgreSQL 9.6"} {
		err := undo.RequireServer(v)
		if err == nil || !strings.Contains(err.Error(), "PostgreSQL 14 or later") || !strings.Contains(err.Error(), says) {
			t.Errorf("RequireServer(%d) = %v, want an error saying PostgreSQL 14 or later and %s", v, err, says)
		}
	}
}

// syncBuffer is a log destination a test reads while a Purger writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newLog is a logger writing to a buffer the test can read.
func newLog() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

// recordSets puts n change sets recorded age (an SQL interval) ago into
// stet.change_sets as su, the superuser, each with two lines in
// stet.changes, and returns their ids in order.
func recordSets(t *testing.T, su *pgx.Conn, n int, age string) []int64 {
	t.Helper()
	rows, err := su.Query(context.Background(), `
		with s as (
		  insert into stet.change_sets (at, stmt_at, pid, login_role, agent_role, target_table, key_column, relid, op)
		  select clock_timestamp() - $2::interval, clock_timestamp() - $2::interval, 0, 'stet_md5', 'stet_md5',
		         'deals', 'id', 0, 'update'
		  from generate_series(1, $1::int) g order by g
		  returning id
		), c as (
		  insert into stet.changes (set_id, key, before, after)
		  select s.id, k::text, '{}', '{}' from s, generate_series(1, 2) k
		)
		select id from s order by id`, n, age)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// setsLeft counts the sets of ids still in stet.change_sets, and their lines
// in stet.changes.
func setsLeft(t *testing.T, stet *pgxpool.Pool, ids []int64) (sets, lines int) {
	t.Helper()
	if err := stet.QueryRow(context.Background(), `select (select count(*) from stet.change_sets where id = any ($1)),
		(select count(*) from stet.changes where set_id = any ($1))`, ids).Scan(&sets, &lines); err != nil {
		t.Fatal(err)
	}
	return sets, lines
}

// waitGone waits up to d until none of ids, nor their lines, is left.
func waitGone(t *testing.T, stet *pgxpool.Pool, ids []int64, d time.Duration, log fmt.Stringer) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		sets, lines := setsLeft(t, stet, ids)
		if sets == 0 && lines == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d sets and %d lines are left after %s; log:\n%s", sets, len(ids), lines, d, log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lockSets has a transaction of the superuser's lock stet.change_sets so
// that no set can be deleted until the returned release is called (or the
// test ends).
func lockSets(t *testing.T, db string) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := superuser(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(context.Background()) })
	mustExec(t, tx, `lock table stet.change_sets in share mode`)
	return func() {
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// waitPurgeWaits waits until Stet's account waits for a lock in database db:
// a purge has begun its delete and waits for lockSets's lock.
func waitPurgeWaits(t *testing.T, db string, log fmt.Stringer) {
	t.Helper()
	su := superuser(t, db)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := su.QueryRow(context.Background(), `select exists (select from pg_stat_activity
			where datname = $1 and usename = $2 and wait_event_type = 'Lock')`, db, stetRole).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no purge waits for the lock; log:\n%s", log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A Purger purges as soon as it starts, 10,000 sets at a time until a delete
// takes fewer: 25,000 old sets all go before the next wake-up, an hour away.
// It keeps the sets newer than keep, purges again at each wake-up, and once
// Close has returned purges nothing more.
func TestPurgerPurgesAtStartAndStops(t *testing.T) {
	db, stet := ownDatabase(t)
	su := superuser(t, db)
	st := store.New(stet)
	log, buf := newLog()

	old := recordSets(t, su, 25_000, "2 hours")
	recent := recordSets(t, su, 3, "50 minutes")
	p := undo.NewPurger(st, time.Hour, log)
	p.Start()
	waitGone(t, stet, old, 30*time.Second, buf)
	p.Close()
	if sets, lines := setsLeft(t, stet, recent); sets != 3 || lines != 6 {
		t.Errorf("%d recent sets and %d lines left, want 3 and 6", sets, lines)
	}

	restore := undo.SetPurgeEvery(20 * time.Millisecond)
	defer restore()
	atStart := recordSets(t, su, 1, "2 hours")
	p = undo.NewPurger(st, time.Hour, log)
	p.Start()
	// Once the set recorded before Start is gone, the purge at start has
	// deleted it and ended (one set is fewer than 10,000): a set recorded
	// now can go only at a later wake-up.
	waitGone(t, stet, atStart, 5*time.Second, buf)
	waitGone(t, stet, recordSets(t, su, 2, "2 hours"), 5*time.Second, buf)
	p.Close()
	after := recordSets(t, su, 2, "2 hours")
	time.Sleep(300 * time.Millisecond) // many wake-ups, had it not stopped
	if sets, _ := setsLeft(t, stet, after); sets != 2 {
		t.Errorf("%d of 2 old sets recorded after Close are left: the purger went on", sets)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("the log has a warning:\n%s", buf)
	}
}

// A delete that takes longer than Close would wait for one is not cut short
// while the Purger runs: the bound for shutdown is not the bound for work.
func TestPurgerFinishesADeleteSlowerThanCloseWaits(t *testing.T) {
	defer undo.SetCloseGrace(200 * time.Millisecond)()
	db, stet := ownDatabase(t)
	log, buf := newLog()

	old := recordSets(t, superuser(t, db), 3, "2 hours")
	release := lockSets(t, db)
	p := undo.NewPurger(store.New(stet), time.Hour, log)
	p.Start()
	defer p.Close()
	waitPurgeWaits(t, db, buf)
	time.Sleep(time.Second) // five times Close's grace
	release()
	waitGone(t, stet, old, 5*time.Second, buf)
	if strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("the log has a warning:\n%s", buf)
	}
}

// Close waits for a delete in progress, which is then finished. A delete
// still running after Close's grace is cancelled: Close returns, and the
// delete deleted nothing, which the log says.
func TestPurgerCloseWaitsForAPurgeInProgress(t *testing.T) {
	db, stet := ownDatabase(t)
	su := superuser(t, db)
	st := store.New(stet)
	log, buf := newLog()

	old := recordSets(t, su, 3, "2 hours")
	release := lockSets(t, db)
	p := undo.NewPurger(st, time.Hour, log)
	p.Start()
	waitPurgeWaits(t, db, buf)
	closed := make(chan struct{})
	go func() {
		p.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatalf("Close returned while a purge was in progress; log:\n%s", buf)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatalf("Close did not return once the purge could go on; log:\n%s", buf)
	}
	if sets, lines := setsLeft(t, stet, old); sets != 0 || lines != 0 {
		t.Errorf("%d sets and %d lines left: the purge in progress at Close was not finished", sets, lines)
	}

	const grace = 300 * time.Millisecond
	defer undo.SetCloseGrace(grace)()
	old = recordSets(t, su, 3, "2 hours")
	release = lockSets(t, db)
	p = undo.NewPurger(st, time.Hour, log)
	p.Start()
	waitPurgeWaits(t, db, buf)
	began := time.Now()
	p.Close()
	if took := time.Since(began); took < grace || took > grace+time.Second {
		t.Errorf("Close took %s with a delete that could not finish; want its grace, %s, and at most a second more",
			took, grace)
	}
	release()
	time.Sleep(200 * time.Millisecond)
	if sets, lines := setsLeft(t, stet, old); sets != 3 || lines != 6 {
		t.Errorf("%d sets and %d lines left after the delete Close cancelled, want 3 and 6", sets, lines)
	}
	if w, ok := lineWith(buf.String(), "level=WARN", "did not finish"); !ok {
		t.Errorf("no warning says the delete did not finish; log:\n%s", buf)
	} else if !strings.Contains(w, "deleted nothing") {
		t.Errorf("the warning does not say the delete deleted nothing: %s", w)
	}
}

// A delete that does not finish within its bound is logged as a warning that
// says so, and the next wake-up purges.
func TestPurgerLogsAFailureAndCarriesOn(t *testing.T) {
	defer undo.SetDeleteTimeout(200 * time.Millisecond)()
	defer undo.SetPurgeEvery(100 * time.Millisecond)()
	db, stet := ownDatabase(t)
	log, buf := newLog()

	old := recordSets(t, superuser(t, db), 3, "2 hours")
	release := lockSets(t, db)
	p := undo.NewPurger(store.New(stet), time.Hour, log)
	p.Start()
	defer p.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "level=WARN") {
		if time.Now().After(deadline) {
			t.Fatalf("no warning while the purge cannot delete; log:\n%s", buf)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := lineWith(buf.String(), "level=WARN", "not purged", "did not finish within 200ms"); !ok {
		t.Errorf("the warning does not say the delete did not finish within its bound:\n%s", buf)
	}
	release()
	waitGone(t, stet, old, 5*time.Second, buf)
}

// lineWith returns the first line of log holding every one of parts.
func lineWith(log string, parts ...string) (string, bool) {
	for line := range strings.Lines(log) {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			return line, true
		}
	}
	return "", false
}

// approvalAndCapture are functions of the stet schema that are not a
// reviewer's to undo with: approving held writes, and the capture trigger's.
var approvalAndCapture = []string{"stet.approve(uuid, text, bigint)", "stet.reject(uuid, text)", "stet.capture()"}

// What GrantSQL prints, exactly: for Stet's account to run, USAGE on the
// stet schema, SELECT on stet.history and EXECUTE on each function given,
// sorted, and what the table owners still have to grant.
func TestGrantSQLPrintsTheReviewerBlock(t *testing.T) {
	functions := []string{
		"stet.undo_plan(text, timestamp with time zone, timestamp with time zone, text[])",
		"stet.undo(text, timestamp with time zone, timestamp with time zone, text[], text)",
		"stet.undo_lock(oid)",
	}
	got, err := undo.GrantSQL("stet_scram", "reviewer", functions)
	if err != nil {
		t.Fatal(err)
	}
	const want = `-- Run as stet_scram (the account Stet connects as): lets "reviewer" read stet.history and undo an agent's changes.
GRANT USAGE ON SCHEMA stet TO reviewer;
GRANT SELECT ON stet.history TO reviewer;
GRANT EXECUTE ON FUNCTION stet.undo(text, timestamp with time zone, timestamp with time zone, text[], text) TO reviewer;
GRANT EXECUTE ON FUNCTION stet.undo_lock(oid) TO reviewer;
GRANT EXECUTE ON FUNCTION stet.undo_plan(text, timestamp with time zone, timestamp with time zone, text[]) TO reviewer;
-- The role also needs SELECT, INSERT, UPDATE and DELETE on the tables it is to restore, and SELECT on the tables that
-- reference those with ON DELETE or ON UPDATE CASCADE, SET NULL or SET DEFAULT; grant those as the tables' owners.
`
	if got != want {
		t.Errorf("GrantSQL printed:\n%s\nwant:\n%s", got, want)
	}
	if again, _ := undo.GrantSQL("stet_scram", "reviewer", slices.Clone(functions)); again != got {
		t.Errorf("GrantSQL is not deterministic:\n%s", again)
	}

	// A role name that needs quoting is an identifier in the statements and
	// stays on its comment's line.
	odd := "Re\"view er\nX"
	got, err = undo.GrantSQL("stet_scram", odd, functions)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"-- Run as stet_scram (the account Stet connects as): lets \"Re\"view er\\x0aX\" read",
		"GRANT USAGE ON SCHEMA stet TO \"Re\"\"view er\nX\";\n",
		"GRANT EXECUTE ON FUNCTION stet.undo_lock(oid) TO \"Re\"\"view er\nX\";\n",
	} {
		if !strings.Contains(got, p) {
			t.Errorf("GrantSQL for %q does not say %q:\n%s", odd, p, got)
		}
	}

	for _, c := range []struct{ stet, role string }{{"stet_scram", ""}, {"stet_scram", "a\x00b"}, {"", "reviewer"}} {
		if got, err := undo.GrantSQL(c.stet, c.role, functions); err == nil || got != "" {
			t.Errorf("GrantSQL(%q, %q) = %q, %v; want an error and nothing", c.stet, c.role, got, err)
		}
	}
}

// ReviewerFunctions lists, from the catalog, every function of the stet
// schema whose name starts with undo, as text GRANT EXECUTE ON FUNCTION
// takes, sorted; nothing else of the schema.
func TestReviewerFunctionsAreTheUndoFunctions(t *testing.T) {
	db, stet := ownDatabase(t)
	ctx := context.Background()
	got, err := undo.ReviewerFunctions(ctx, stet)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSorted(got) {
		t.Errorf("not sorted: %q", got)
	}
	if again, err := undo.ReviewerFunctions(ctx, stet); err != nil || !slices.Equal(again, got) {
		t.Errorf("a second read: %q, %v", again, err)
	}

	su := superuser(t, db)
	var want []string
	rows, err := su.Query(ctx, `select p.oid::regprocedure::text from pg_proc p join pg_namespace n on n.oid = p.pronamespace
		where n.nspname = 'stet' and p.proname like 'undo%' order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	if want, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || len(want) < 2 {
		t.Fatalf("%d functions %q, want the catalog's %d: %q", len(got), got, len(want), want)
	}
	// Each signature names one of them, and together all of them.
	var named []string
	for _, sig := range got {
		var proc *string
		if err := su.QueryRow(ctx, `select to_regprocedure($1)::text`, sig).Scan(&proc); err != nil || proc == nil {
			t.Errorf("%q names no function (%v)", sig, err)
			continue
		}
		named = append(named, *proc)
	}
	slices.Sort(named)
	if !slices.Equal(named, want) {
		t.Errorf("the signatures name %q, want %q", named, want)
	}
	for _, sig := range got {
		if !strings.HasPrefix(sig, "stet.undo") {
			t.Errorf("%q is not an undo function of the stet schema", sig)
		}
	}
}

// Run as Stet's account, the block lets a role that had nothing read
// stet.history, plan and undo an agent's changes, and nothing more: not
// approve or reject held writes, not call stet.capture(), not read the
// capture tables. A role whose name needs quoting gets the same rights.
func TestGrantSQLLetsAReviewerUndo(t *testing.T) {
	reviewer := newOwner(t, "stet_undo_reviewer")
	odd := newOwner(t, "Stet \"Odd\" Reviewer\nX")
	db, stet := ownDatabase(t)
	ctx := context.Background()
	su := superuser(t, db)
	mustExec(t, su,
		`create table public.deals (id int primary key, name text)`,
		`insert into public.deals values (2, 'two')`,
		`grant select, insert, update, delete on public.deals to `+agentRole+`, `+reviewer)
	installByHand(t, su, "public.deals", "deals", "id")

	functions, err := undo.ReviewerFunctions(ctx, stet)
	if err != nil {
		t.Fatal(err)
	}
	can := func(role string) map[string]bool {
		t.Helper()
		m := map[string]bool{}
		var b bool
		for _, q := range []struct{ what, sql, object string }{
			{"usage", `select has_schema_privilege($1, 'stet', 'usage')`, ""},
			{"history", `select has_table_privilege($1, 'stet.history', 'select')`, ""},
			{"change_sets", `select has_table_privilege($1, 'stet.change_sets', 'select')`, ""},
			{"changes", `select has_table_privilege($1, 'stet.changes', 'select')`, ""},
			{"review", `select has_table_privilege($1, 'stet.review', 'select')`, ""},
		} {
			if err := su.QueryRow(ctx, q.sql, role).Scan(&b); err != nil {
				t.Fatal(err)
			}
			m[q.what] = b
		}
		for _, f := range append(slices.Clone(functions), approvalAndCapture...) {
			if err := su.QueryRow(ctx, `select has_function_privilege($1, $2, 'execute')`, role, f).Scan(&b); err != nil {
				t.Fatal(err)
			}
			m[f] = b
		}
		return m
	}
	for _, role := range []string{reviewer, odd} {
		for what, has := range can(role) {
			if has {
				t.Errorf("%q has %s before the grants", role, what)
			}
		}
	}

	var since time.Time
	if err := su.QueryRow(ctx, `select clock_timestamp()`).Scan(&since); err != nil {
		t.Fatal(err)
	}
	mustExec(t, connectAs(t, db, agentRole, agentPassword),
		`insert into public.deals values (1, 'one')`,
		`update public.deals set name = 'changed' where id = 2`)

	asStet := connectAs(t, db, stetRole, stetPassword)
	for _, role := range []string{reviewer, odd} {
		sql, err := undo.GrantSQL(stetRole, role, functions)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, asStet, sql)
		for what, has := range can(role) {
			want := what != "change_sets" && what != "changes" && what != "review" && !slices.Contains(approvalAndCapture, what)
			if has != want {
				t.Errorf("%q after the grants: %s %v, want %v", role, what, has, want)
			}
		}
	}

	rv := connectAs(t, db, reviewer, ownerPassword)
	var shown int
	if err := rv.QueryRow(ctx, `select count(*) from stet.history where target_table = 'deals'`).Scan(&shown); err != nil || shown != 2 {
		t.Errorf("the reviewer reads %d changes of deals in stet.history (%v), want 2", shown, err)
	}
	outcomes := func(sql string) []string {
		t.Helper()
		rows, err := rv.Query(ctx, sql, agentRole, since)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		slices.Sort(got)
		return got
	}
	if got := outcomes(`select key || ' ' || outcome from stet.undo_plan($1, $2)`); !slices.Equal(got, []string{"1 would_restore", "2 would_restore"}) {
		t.Errorf("the reviewer's plan: %q", got)
	}
	if got := outcomes(`select key || ' ' || outcome from stet.undo($1, $2)`); !slices.Equal(got, []string{"1 restored", "2 restored"}) {
		t.Errorf("the reviewer's undo: %q", got)
	}
	rows, err := su.Query(ctx, `select id || ' ' || name from public.deals order by id`)
	if err != nil {
		t.Fatal(err)
	}
	if content, err := pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || !slices.Equal(content, []string{"2 two"}) {
		t.Errorf("deals after the undo: %q (%v), want only 2 two", content, err)
	}

	for _, sql := range []string{
		`select count(*) from stet.change_sets`,
		`select stet.reject('00000000-0000-0000-0000-000000000000', 'no')`,
		`select stet.approve('00000000-0000-0000-0000-000000000000')`,
	} {
		var pgErr *pgconn.PgError
		if _, err := rv.Exec(ctx, sql); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("the reviewer: %s: %v, want 42501", sql, err)
		}
	}
}

// grantCreateOnStet has Stet's account give role (a newOwner) USAGE and
// CREATE on the stet schema of db, and returns a connection as role.
func grantCreateOnStet(t *testing.T, db, role string) *pgx.Conn {
	t.Helper()
	mustExec(t, connectAs(t, db, stetRole, stetPassword), "grant usage, create on schema stet to "+pgx.Identifier{role}.Sanitize())
	return connectAs(t, db, role, ownerPassword)
}

// Only Stet's own undo functions are listed: ordinary functions of the stet
// schema named undo or undo_*, owned by the owner of stet.capture(). A
// function another role with CREATE on the schema made, under a new name or
// as an overload of one of Stet's, is not, nor an aggregate or a function of
// another name that Stet's account owns; the grant block names none of them.
func TestReviewerFunctionsAreStetsOwn(t *testing.T) {
	other := newOwner(t, "stet_undo_other")
	db, stet := ownDatabase(t)
	ctx := context.Background()
	want, err := undo.ReviewerFunctions(ctx, stet)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(want, "stet.undo_lock(oid)") {
		t.Fatalf("Stet's undo_lock(oid) is not listed: %q", want)
	}
	mustExec(t, grantCreateOnStet(t, db, other),
		`create function stet.undo_evil() returns int language sql as 'select 1'`,
		`create function stet.undo_lock(integer) returns int language sql as 'select 1'`)
	mustExec(t, connectAs(t, db, stetRole, stetPassword),
		`create aggregate stet.undo_sum(int) (sfunc = int4pl, stype = int)`,
		`create function stet.undone() returns int language sql as 'select 1'`)

	got, err := undo.ReviewerFunctions(ctx, stet)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("with functions that are not Stet's undo functions in the schema: %q, want %q", got, want)
	}
	sql, err := undo.GrantSQL(stetRole, "reviewer", got)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"undo_evil", "undo_lock(integer)", "undo_sum", "undone"} {
		if strings.Contains(sql, p) {
			t.Errorf("the grant block names %s:\n%s", p, sql)
		}
	}
}

// The list must hold Stet's stet.undo, by the signature Stet installs and
// owned as stet.capture() is: when the only stet.undo is another role's,
// ReviewerFunctions fails with ErrNoUndo, as when there is none.
func TestReviewerFunctionsNeedStetsUndo(t *testing.T) {
	other := newOwner(t, "stet_undo_other")
	db, stet := ownDatabase(t)
	ctx := context.Background()
	impostor := grantCreateOnStet(t, db, other)
	mustExec(t, superuser(t, db), `drop function stet.undo(text, timestamptz, timestamptz, text[], text)`)
	if got, err := undo.ReviewerFunctions(ctx, stet); !errors.Is(err, undo.ErrNoUndo) || got != nil {
		t.Errorf("without stet.undo: %q, %v; want ErrNoUndo", got, err)
	}
	mustExec(t, impostor, `create function stet.undo(agent_role text, since timestamptz, until timestamptz, tables text[], note text)
		returns int language sql as 'select 1'`)
	if got, err := undo.ReviewerFunctions(ctx, stet); !errors.Is(err, undo.ErrNoUndo) || got != nil {
		t.Errorf("with another role's stet.undo: %q, %v; want ErrNoUndo", got, err)
	}
}

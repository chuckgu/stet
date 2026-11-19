// Package undo looks up the tables of undo.tables in the database, says what
// keeps each from being captured, and prints the SQL their owners run to
// install the capture triggers (docs/spec-v0.2-undo.md 4, 9). Stet installs
// nothing on a customer's table: it only prints. A Purger deletes the change
// records older than undo.keep (spec 9).
package undo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/store"
)

// Table is one table of undo.tables as the database has it. Triggers and
// Children are looked at only for a table without a Problem.
type Table struct {
	Key, PK             string // the config key and the key column
	Schema, Name, Owner string // "" when the table was not found
	OID                 uint32
	Problem             string   // why capture cannot be installed on it; "" when it can
	Triggers            []string // what is wrong with its capture triggers, one line each; empty when all four are right
	Children            []string // schema-qualified tables that reference it with ON DELETE CASCADE, SET NULL or SET DEFAULT and are not in undo.tables

	// noTriggers: the relation is of a kind that cannot have triggers (an
	// index, a sequence, a materialized view, a system table), so DropSQL
	// leaves it out.
	noTriggers bool
}

// trigger is one of the four capture triggers as stet undo-sql prints them
// (spec 4, with the transition tables stet_old and stet_new).
type trigger struct {
	name     string
	event    string // INSERT, UPDATE, DELETE or TRUNCATE
	old, new string // the names of its transition tables; "" for none
}

var captureTriggers = [...]trigger{
	{"stet_capture_ins", "INSERT", "", "stet_new"},
	{"stet_capture_upd", "UPDATE", "stet_old", "stet_new"},
	{"stet_capture_del", "DELETE", "stet_old", ""},
	{"stet_capture_trunc", "TRUNCATE", "", ""},
}

// pg_trigger.tgtype's bits.
const (
	tgRow      = 1 << 0
	tgBefore   = 1 << 1
	tgInsert   = 1 << 2
	tgDelete   = 1 << 3
	tgUpdate   = 1 << 4
	tgTruncate = 1 << 5
	tgInstead  = 1 << 6
)

// events are the trigger events in the order the printed SQL names them.
var events = []struct {
	name string
	bit  int16
}{{"INSERT", tgInsert}, {"UPDATE", tgUpdate}, {"DELETE", tgDelete}, {"TRUNCATE", tgTruncate}}

func eventBit(name string) int16 {
	for _, e := range events {
		if e.name == name {
			return e.bit
		}
	}
	return 0
}

// maxIdent is the length in bytes PostgreSQL truncates a name to.
const maxIdent = 63

// Inspect looks up every table of cfg.Undo, sorted by key. Names resolve as
// the gate resolves them: the key, split at its dots, each part an
// identifier as written, through Stet's search_path when it has one part.
//
// It runs as Stet's account and reads only the catalogs, never a customer's
// rows, so it needs no right on the tables or their schemas. q may be a
// read-only transaction: it sends nothing that can fail on a key's text.
func Inspect(ctx context.Context, q store.Querier, cfg *config.Config) ([]Table, error) {
	if cfg.Undo == nil {
		return nil, nil
	}
	keys := make([]string, 0, len(cfg.Undo.Tables))
	for k := range cfg.Undo.Tables {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	ts := make([]Table, len(keys))
	for i, key := range keys {
		ts[i] = Table{Key: key, PK: cfg.Undo.Tables[key]}
		if err := look(ctx, q, &ts[i]); err != nil {
			return nil, fmt.Errorf("undo.tables.%s: %w", oneLine(key), err)
		}
	}
	sameTable(ts)

	oids := []uint32{} // never nil: <> all (NULL) would keep no child
	for _, t := range ts {
		if t.OID != 0 {
			oids = append(oids, t.OID)
		}
	}
	for i := range ts {
		t := &ts[i]
		if t.Problem != "" {
			continue
		}
		if err := inspectTriggers(ctx, q, t); err != nil {
			return nil, fmt.Errorf("undo.tables.%s: %w", oneLine(t.Key), err)
		}
		if err := inspectChildren(ctx, q, t, oids); err != nil {
			return nil, fmt.Errorf("undo.tables.%s: %w", oneLine(t.Key), err)
		}
	}
	return ts, nil
}

// look finds t's table and fills in what it is, or the Problem.
func look(ctx context.Context, q store.Querier, t *Table) error {
	oid, problem, err := resolve(ctx, q, t.Key)
	if err != nil || problem != "" {
		t.Problem = problem
		return err
	}
	var (
		relkind                 string
		partition, temp, system bool
		parent                  *string
		inheritors              []string
		hasColumn, isKey        bool
		deferrablePK            bool
	)
	// No column has a name the server cannot take as text; "" matches none.
	pk := t.PK
	if strings.ContainsRune(pk, 0) || !utf8.ValidString(pk) {
		pk = ""
	}
	// An OID below 16384 (FirstNormalObjectId) is initdb's: a system table.
	err = q.QueryRow(ctx, `
		select n.nspname, c.relname, pg_get_userbyid(c.relowner), c.relkind::text, c.relispartition,
		       c.relpersistence = 't', c.oid < 16384,
		       (select format('%I.%I', pn.nspname, p.relname)
		          from pg_inherits i join pg_class p on p.oid = i.inhparent join pg_namespace pn on pn.oid = p.relnamespace
		         where i.inhrelid = c.oid order by i.inhseqno limit 1),
		       array(select format('%I.%I', kn.nspname, k.relname)
		               from pg_inherits i join pg_class k on k.oid = i.inhrelid join pg_namespace kn on kn.oid = k.relnamespace
		              where i.inhparent = c.oid),
		       a.attnum is not null,
		       exists (select from pg_index x
		                where x.indrelid = c.oid and x.indisunique and x.indisvalid and x.indimmediate
		                  and x.indnkeyatts = 1 and x.indkey[0] = a.attnum
		                  and x.indpred is null and x.indexprs is null
		                  and (x.indisprimary or a.attnotnull)),
		       exists (select from pg_index x
		                where x.indrelid = c.oid and x.indisprimary and not x.indimmediate
		                  and x.indnkeyatts = 1 and x.indkey[0] = a.attnum)
		  from pg_class c
		  join pg_namespace n on n.oid = c.relnamespace
		  left join pg_attribute a on a.attrelid = c.oid and a.attname = $2 and a.attnum > 0 and not a.attisdropped
		 where c.oid = $1`, oid, pk).Scan(
		&t.Schema, &t.Name, &t.Owner, &relkind, &partition, &temp, &system, &parent, &inheritors, &hasColumn, &isKey, &deferrablePK)
	if errors.Is(err, pgx.ErrNoRows) {
		// Dropped between the two reads.
		t.Problem = "Stet's account finds no table " + ident(t.Key)
		return nil
	}
	if err != nil {
		return err
	}
	t.OID = oid
	qual := t.qualified()
	slices.Sort(inheritors)
	switch relkind {
	case "i", "I", "S", "m", "c", "t":
		t.noTriggers = true
	}
	switch {
	case system:
		t.noTriggers = true
		t.Problem = qual + " is a system table, not an ordinary table"
	case relkind == "r" && partition:
		p := "another table"
		if parent != nil {
			p = *parent
		}
		t.Problem = fmt.Sprintf("%s is a partition of %s; capture takes no partitioned table and no partition", qual, p)
	case relkind != "r":
		t.Problem = fmt.Sprintf("%s %s, not an ordinary table", qual, relkindName(relkind))
	case temp:
		t.Problem = qual + " is a temporary table, not an ordinary table"
	case parent != nil:
		// Spec 4: a write through the parent fires only the parent's
		// statement triggers.
		t.Problem = fmt.Sprintf("%s is an inheritance child of %s; capture takes no inheritance child, since a write "+
			"through %s changes its rows without firing its statement triggers", qual, *parent, *parent)
	case len(inheritors) > 0:
		t.Problem = fmt.Sprintf("%s has inheritance children (%s); capture takes no table with inheritance children, "+
			"since a write to it also changes their rows, which its triggers do not capture", qual, strings.Join(inheritors, ", "))
	case !hasColumn:
		t.Problem = fmt.Sprintf("%s has no column %s", qual, quoteIdent(t.PK))
	case !isKey && deferrablePK:
		t.Problem = fmt.Sprintf("column %s of %s is the table's primary key, but a deferrable one: with its check "+
			"deferred, a statement can leave two rows with one key, and capture records rows by key; "+
			"make the primary key NOT DEFERRABLE", quoteIdent(t.PK), qual)
	case !isKey:
		t.Problem = fmt.Sprintf("column %s of %s is not a key: it must be the table's primary key, or NOT NULL with "+
			"a unique constraint or index on it alone that is not partial, not on an expression and not deferrable",
			quoteIdent(t.PK), qual)
	}
	return nil
}

func relkindName(k string) string {
	switch k {
	case "v":
		return "is a view"
	case "m":
		return "is a materialized view"
	case "p":
		return "is a partitioned table"
	case "f":
		return "is a foreign table"
	case "i", "I":
		return "is an index"
	case "S":
		return "is a sequence"
	case "c":
		return "is a composite type"
	case "t":
		return "is a TOAST table"
	}
	return fmt.Sprintf("is of relkind %q", k)
}

// resolve finds the table key names. A key the gate's SQL would not resolve
// to a table is a problem, not an error: problem says why.
//
// A one-part key resolves through Stet's search_path with to_regclass, as
// the gate's SQL does, which skips schemas Stet's account cannot use. A
// qualified key is looked up in the catalogs: to_regclass would raise
// where Stet's account has no USAGE on the schema, and an error would end a
// read-only transaction q may be. A third part must be this database.
func resolve(ctx context.Context, q store.Querier, key string) (oid uint32, problem string, err error) {
	switch {
	case strings.ContainsRune(key, 0):
		return 0, "the key holds a NUL byte, which no PostgreSQL name can", nil
	case !utf8.ValidString(key):
		return 0, "the key is not valid UTF-8", nil
	}
	parts := strings.Split(key, ".")
	notFound := "Stet's account finds no table " + ident(key)
	if len(parts) > 3 {
		return 0, notFound + ": a table's name has at most three dotted parts", nil
	}
	if slices.Contains(parts, "") {
		return 0, notFound + ": one of its dotted parts is empty", nil
	}
	for i := range parts {
		parts[i] = truncateIdent(parts[i])
	}

	var found *uint32
	if len(parts) == 1 {
		if err := q.QueryRow(ctx, `select to_regclass($1)::oid`, ident(key)).Scan(&found); err != nil {
			return 0, "", err
		}
		if found == nil {
			return 0, notFound + " on its search_path", nil
		}
		return *found, "", nil
	}
	database := ""
	if len(parts) == 3 {
		database, parts = parts[0], parts[1:]
	}
	var current string
	if err := q.QueryRow(ctx, `
		select (select c.oid from pg_class c join pg_namespace n on n.oid = c.relnamespace
		         where n.nspname = $1 and c.relname = $2), current_database()`, parts[0], parts[1]).Scan(&found, &current); err != nil {
		return 0, "", err
	}
	switch {
	case database != "" && database != current:
		return 0, fmt.Sprintf("%s: it names database %s, and Stet is connected to %s", notFound, quoteIdent(database), quoteIdent(current)), nil
	case found == nil:
		return 0, notFound, nil
	}
	return *found, "", nil
}

// truncateIdent cuts s to the bytes of a name PostgreSQL keeps, at a
// character's boundary.
func truncateIdent(s string) string {
	if len(s) <= maxIdent {
		return s
	}
	n := maxIdent
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ident is the key as the gate names it in SQL.
func ident(key string) string {
	return pgx.Identifier(strings.Split(key, ".")).Sanitize()
}

// qualified is the table's schema-qualified name as SQL.
func (t Table) qualified() string {
	return quoteIdent(t.Schema) + "." + quoteIdent(t.Name)
}

// sameTable gives every table that another key also names a Problem: its
// triggers can pass only one key.
func sameTable(ts []Table) {
	for i := range ts {
		if ts[i].OID == 0 || ts[i].Problem != "" {
			continue
		}
		var others []string
		for j := range ts {
			if j != i && ts[j].OID == ts[i].OID {
				others = append(others, "undo.tables."+ts[j].Key)
			}
		}
		if len(others) > 0 {
			ts[i].Problem = fmt.Sprintf("%s is also %s: the same table under two keys; list it once",
				ts[i].qualified(), strings.Join(others, ", "))
		}
	}
}

// inspectTriggers fills in what is wrong with each of t's capture triggers.
func inspectTriggers(ctx context.Context, q store.Querier, t *Table) error {
	want := []string{t.Key, t.PK}
	for _, tr := range captureTriggers {
		var (
			tgtype, nargs int16
			enabled, fn   string
			isCapture     bool
			args          []byte
			old, new      *string
			columns       int32
			when, constr  bool
		)
		err := q.QueryRow(ctx, `
			select t.tgtype, t.tgenabled::text,
			       exists (select from pg_proc p join pg_namespace n on n.oid = p.pronamespace
			                where p.oid = t.tgfoid and n.nspname = 'stet' and p.proname = 'capture' and p.pronargs = 0),
			       (select format('%I.%I(%s)', n.nspname, p.proname, pg_get_function_identity_arguments(p.oid))
			          from pg_proc p join pg_namespace n on n.oid = p.pronamespace where p.oid = t.tgfoid),
			       t.tgnargs, t.tgargs, t.tgoldtable::text, t.tgnewtable::text,
			       cardinality(t.tgattr::int2[]), t.tgqual is not null, t.tgconstraint <> 0
			  from pg_trigger t
			 where t.tgrelid = $1 and t.tgname = $2`, t.OID, tr.name).Scan(
			&tgtype, &enabled, &isCapture, &fn, &nargs, &args, &old, &new, &columns, &when, &constr)
		if errors.Is(err, pgx.ErrNoRows) {
			t.Triggers = append(t.Triggers, tr.name+" is missing")
			continue
		}
		if err != nil {
			return err
		}
		wrong := func(format string, a ...any) {
			t.Triggers = append(t.Triggers, tr.name+" "+fmt.Sprintf(format, a...))
		}
		if constr {
			// CREATE OR REPLACE TRIGGER refuses to replace one.
			wrong("is a constraint trigger, which the output of stet undo-sql cannot replace; the table's owner must drop it")
		}
		switch enabled {
		case "D":
			wrong("is disabled")
		case "R":
			wrong("fires only in replica sessions (ENABLE REPLICA)")
		}
		if !isCapture {
			wrong("calls %s rather than stet.capture()", fn)
		}
		switch {
		case tgtype&tgInstead != 0:
			wrong("fires INSTEAD OF rather than AFTER")
		case tgtype&tgBefore != 0:
			wrong("fires BEFORE rather than AFTER")
		}
		if tgtype&tgRow != 0 {
			wrong("fires FOR EACH ROW rather than FOR EACH STATEMENT")
		}
		if on := tgtype & (tgInsert | tgUpdate | tgDelete | tgTruncate); on != eventBit(tr.event) {
			var names []string
			for _, e := range events {
				if on&e.bit != 0 {
					names = append(names, e.name)
				}
			}
			wrong("fires on %s rather than on %s alone", strings.Join(names, " OR "), tr.event)
		}
		if columns > 0 {
			wrong("fires only on UPDATE OF some columns rather than on every UPDATE")
		}
		if when {
			wrong("has a WHEN condition, so it may not fire on every write")
		}
		if got, want := referencing(deref(old), deref(new)), referencing(tr.old, tr.new); got != want {
			wrong("says %s rather than %s", orNoReferencing(got), orNoReferencing(want))
		}
		if got := splitArgs(args, int(nargs)); !slices.Equal(got, want) {
			wrong("passes (%s) rather than (%s)", literals(got), literals(want))
		}
	}

	// Any other trigger that calls stet.capture() records writes again. Only
	// reported: stet undo-sql drops no trigger it does not print.
	names := make([]string, len(captureTriggers))
	for i, tr := range captureTriggers {
		names[i] = tr.name
	}
	var others []string
	if err := q.QueryRow(ctx, `
		select array(select t.tgname::text
		               from pg_trigger t
		               join pg_proc p on p.oid = t.tgfoid
		               join pg_namespace n on n.oid = p.pronamespace
		              where t.tgrelid = $1 and n.nspname = 'stet' and p.proname = 'capture' and p.pronargs = 0
		                and t.tgname <> all ($2::text[]))`, t.OID, names).Scan(&others); err != nil {
		return err
	}
	slices.Sort(others)
	for _, name := range others {
		t.Triggers = append(t.Triggers, quoteIdent(name)+" "+otherCapture)
	}
	return nil
}

// otherCapture follows the name of a trigger, other than the four, that
// calls stet.capture().
const otherCapture = "also calls stet.capture(), so a write it fires on is recorded twice"

// isCaptureTrigger reports whether line, one of Table.Triggers, is about one
// of the four capture triggers. Each line starts with its trigger's name, and
// the name of any other trigger is quoted unless it is a bare identifier, so
// it cannot start with one of the four names and a space.
func isCaptureTrigger(line string) bool {
	return slices.ContainsFunc(captureTriggers[:], func(tr trigger) bool { return strings.HasPrefix(line, tr.name+" ") })
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func orNoReferencing(s string) string {
	if s == "" {
		return "no REFERENCING"
	}
	return s
}

// splitArgs reads pg_trigger.tgargs: n arguments, each ended by a NUL byte.
func splitArgs(b []byte, n int) []string {
	args := make([]string, 0, n)
	for len(args) < n {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			args = append(args, string(b))
			break
		}
		args = append(args, string(b[:i]))
		b = b[i+1:]
	}
	return args
}

func literals(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = quoteLiteral(a)
	}
	return strings.Join(q, ", ")
}

// inspectChildren fills in the tables that reference t with a foreign key
// whose ON DELETE action changes their rows (CASCADE deletes them, SET NULL
// and SET DEFAULT change their reference), as stet.undo_warning's
// children_not_captured, and that are not in undo.tables (oids). An ON UPDATE
// action does not count: undoing the parent's update cascades back. A
// partition's copy of its partitioned table's foreign key is left out: the
// partitioned table is listed.
func inspectChildren(ctx context.Context, q store.Querier, t *Table, oids []uint32) error {
	var children []string
	if err := q.QueryRow(ctx, `
		select array(select distinct format('%I.%I', n.nspname, c.relname)
		               from pg_constraint fk
		               join pg_class c on c.oid = fk.conrelid
		               join pg_namespace n on n.oid = c.relnamespace
		              where fk.contype = 'f' and fk.confrelid = $1 and fk.conparentid = 0
		                and fk.confdeltype in ('c', 'n', 'd')
		                and fk.conrelid <> all ($2::oid[]))`, t.OID, oids).Scan(&children); err != nil {
		return err
	}
	slices.Sort(children)
	if len(children) > 0 {
		t.Children = children
	}
	return nil
}

// fix is what a warning about a table's triggers ends with.
const fix = "run stet undo-sql and have the table's owner run its output"

// Warnings are the lines Stet logs at startup (spec 9): for each table, in
// order, the problem that keeps it from being captured, or else what is
// wrong with its four capture triggers (one line, whose fix is the output of
// stet undo-sql), each other trigger that calls stet.capture() (one line
// each, whose fix is for the owner to drop it), and the tables whose rows its
// deletes change uncaptured. Each line names the table's key.
func Warnings(ts []Table) []string {
	var out []string
	add := func(format string, a ...any) { out = append(out, oneLine(fmt.Sprintf(format, a...))) }
	for _, t := range ts {
		switch {
		case t.Problem != "" && t.Schema == "":
			add("undo.tables.%s: %s; its writes are not captured; fix: create the table, or name it in undo.tables "+
				"as Stet's connection sees it", t.Key, t.Problem)
			continue
		case t.Problem != "":
			add("undo.tables.%s: %s; its writes are not captured; fix: list in undo.tables only ordinary tables "+
				"with a single-column key", t.Key, t.Problem)
			continue
		}
		var four, others []string
		for _, line := range t.Triggers {
			if isCaptureTrigger(line) {
				four = append(four, line)
			} else {
				others = append(others, line)
			}
		}
		if len(four) > 0 {
			add("undo.tables.%s: its capture triggers are not as stet undo-sql prints them (%s), so its writes are "+
				"not all captured; fix: %s", t.Key, strings.Join(four, "; "), fix)
		}
		for _, line := range others {
			add("undo.tables.%s: trigger %s; fix: have the table's owner drop it; the output of stet undo-sql does not",
				t.Key, line)
		}
		if len(t.Children) > 0 {
			add("undo.tables.%s: tables that reference it with ON DELETE CASCADE, SET NULL or SET DEFAULT are not in "+
				"undo.tables (%s): when one of its rows is deleted, their rows that reference it are deleted or have "+
				"their reference changed, which is not captured, and undoing the delete does not restore them; "+
				"fix: add them to undo.tables with their key columns", t.Key, strings.Join(t.Children, ", "))
		}
	}
	return out
}

package undo

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/stetdb/stet/internal/store"
)

// stetCapture is SQL for the OID of Stet's capture function, resolved as
// ReviewerFunctions resolves it: the ordinary function of the stet schema
// named capture that takes no argument. A schema holds one function of a
// signature, so it is that one or none (NULL).
const stetCapture = `(select p.oid
	   from pg_catalog.pg_proc p join pg_catalog.pg_namespace n on n.oid = p.pronamespace
	  where n.nspname = 'stet' and p.proname = 'capture' and p.prokind = 'f' and p.pronargs = 0)`

// Captured is a table that carries triggers calling stet.capture(), as the
// catalog has it, whatever the config says.
type Captured struct {
	Schema, Name, Owner string
	Triggers            []string // the names of its triggers that call stet.capture(), in byte order
}

// qualified is the table's schema-qualified name as SQL.
func (c Captured) qualified() string {
	return quoteIdent(c.Schema) + "." + quoteIdent(c.Name)
}

// CapturedTables reads from the catalog every table of the database that
// carries a trigger calling Stet's stet.capture(), under any name, enabled or
// not, in byte order of schema and name. It is how Stet finds the tables it
// still captures when the config's undo clause is gone (spec 9). It runs as
// Stet's account, reads only the catalogs, and returns nothing in a database
// without stet.capture(). q may be a read-only transaction.
func CapturedTables(ctx context.Context, q store.Querier) ([]Captured, error) {
	var found []byte
	if err := q.QueryRow(ctx, `
		select coalesce(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object(
		         'Schema', n.nspname, 'Name', c.relname, 'Owner', pg_catalog.pg_get_userbyid(c.relowner), 'Triggers', tg.names)
		         order by n.nspname collate "C", c.relname collate "C"), '[]')::text
		  from pg_catalog.pg_class c
		  join pg_catalog.pg_namespace n on n.oid = c.relnamespace
		 cross join lateral (
		       select pg_catalog.array_agg(t.tgname::text order by t.tgname::text collate "C") as names
		         from pg_catalog.pg_trigger t
		        where t.tgrelid = c.oid and not t.tgisinternal and t.tgfoid = `+stetCapture+`) tg
		 where tg.names is not null`).Scan(&found); err != nil {
		return nil, err
	}
	var ts []Captured
	if err := json.Unmarshal(found, &ts); err != nil {
		return nil, fmt.Errorf("reading the tables with capture triggers: %w", err)
	}
	return ts, nil
}

// CaptureWithoutUndo is the warning Stet logs at start when its config has
// no undo clause and ts, the tables CapturedTables found, is not empty: one
// line that names the tables, says what goes on without the clause, and
// gives both remedies. It is "" for no table.
func CaptureWithoutUndo(ts []Captured) string {
	if len(ts) == 0 {
		return ""
	}
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.qualified()
	}
	return oneLine(fmt.Sprintf("the config has no undo clause, but triggers that call stet.capture() are installed on %s: "+
		"writes to these tables are still recorded, under the settings stet.capture_settings last had, and nothing "+
		"deletes old change records, so stet.change_sets and stet.changes only grow; fix: put the undo clause back, "+
		"or have the tables' owners run the output of stet undo-sql -drop", strings.Join(names, ", ")))
}

// DropCapturedSQL is what `stet undo-sql -drop` prints for a config without
// an undo clause: for ts, the tables CapturedTables found, the SQL that drops
// each trigger calling stet.capture() by its name, in one transaction per
// owner as DropSQL prints it, owners in byte order and each owner's tables in
// the order given. It is "" for no table.
func DropCapturedSQL(ts []Captured) string {
	if len(ts) == 0 {
		return ""
	}
	type group struct {
		owner  string
		tables []Captured
	}
	var groups []group
	for _, t := range ts {
		i := slices.IndexFunc(groups, func(g group) bool { return g.owner == t.Owner })
		if i < 0 {
			groups = append(groups, group{owner: t.Owner})
			i = len(groups) - 1
		}
		groups[i].tables = append(groups[i].tables, t)
	}
	slices.SortStableFunc(groups, func(a, b group) int { return strings.Compare(a.owner, b.owner) })

	var b strings.Builder
	b.WriteString("-- The config has no undo clause: these are the tables of this database that carry a trigger calling stet.capture().\n")
	for i, g := range groups {
		if i > 0 {
			b.WriteString("\n")
		}
		names := make([]string, len(g.tables))
		for j, t := range g.tables {
			names[j] = t.qualified()
		}
		beginOwner(&b, i+1, g.owner, names)
		for _, t := range g.tables {
			for _, name := range t.Triggers {
				fmt.Fprintf(&b, "DROP TRIGGER IF EXISTS %s ON %s;\n", quoteIdent(name), t.qualified())
			}
		}
		b.WriteString("COMMIT;\n")
	}
	return b.String()
}

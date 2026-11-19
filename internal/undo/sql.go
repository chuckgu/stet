package undo

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/stetdb/stet/internal/store"
)

// lockTimeout bounds how long an owner's transaction waits for the locks of
// its tables: an open transaction that writes one of them holds a lock that
// CREATE OR REPLACE TRIGGER waits for (spec 9), and DROP TRIGGER waits for
// readers too.
const lockTimeout = "5s"

// InstallSQL is what `stet undo-sql` prints: the grants for stetRole to run, then one transaction per table owner.
// It fails, naming each table and its Problem, when any table has one.
//
// The grants give each owner, once, what creating the triggers takes (USAGE
// on the stet schema, EXECUTE on stet.capture()); an owner that is stetRole
// has them. Each owner's part is one transaction that first sets
// lock_timeout, then, table by table, creates or replaces each of the four
// triggers, so it can be run again (ruling R12). CREATE OR REPLACE TRIGGER
// takes a lock that waits for, and blocks, writes to the table, not reads;
// DROP TRIGGER would block reads too, so the SQL drops nothing, and a
// trigger of another name is left alone. It needs PostgreSQL 14 or later
// (RequireServer). After the transaction, notes name the tables whose rows
// the table's deletes change uncaptured. Owners and tables are in
// byte order of owner and key. Identifiers are quoted where they must be and
// the arguments are string literals, so no name can end the identifier or
// literal it is in, and comments hold no line break.
func InstallSQL(stetRole string, ts []Table) (string, error) {
	var bad []string
	for _, t := range sortedByKey(ts) {
		why := ""
		switch {
		case t.Problem != "":
			why = t.Problem
		case t.Schema == "" || t.Name == "" || t.Owner == "":
			why = "not looked up in the database"
		case hasNUL(t.Key, t.PK, t.Schema, t.Name, t.Owner):
			why = "a name holds a NUL byte"
		}
		if why != "" {
			bad = append(bad, oneLine(fmt.Sprintf("undo.tables.%s: %s", t.Key, why)))
		}
	}
	if len(bad) > 0 {
		return "", fmt.Errorf("capture cannot be installed on these tables of undo.tables; fix them and run stet undo-sql again:\n  %s",
			strings.Join(bad, "\n  "))
	}
	if stetRole == "" || hasNUL(stetRole) {
		return "", errors.New("the name of Stet's account is empty or holds a NUL byte")
	}

	groups := byOwner(ts)
	var b strings.Builder
	n := 0
	var grantees []string
	for _, g := range groups {
		if g.owner != stetRole {
			grantees = append(grantees, quoteIdent(g.owner))
		}
	}
	if len(grantees) > 0 {
		n++
		fmt.Fprintf(&b, "-- %d. Run as %s (the account Stet connects as).\n", n, comment(quoteIdent(stetRole)))
		for _, g := range grantees {
			fmt.Fprintf(&b, "GRANT USAGE ON SCHEMA stet TO %s;\n", g)
			fmt.Fprintf(&b, "GRANT EXECUTE ON FUNCTION stet.capture() TO %s;\n", g)
		}
	}
	for _, g := range groups {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		n++
		g.begin(&b, n)
		for _, t := range g.tables {
			table := t.qualified()
			args := quoteLiteral(t.Key) + ", " + quoteLiteral(t.PK)
			for _, tr := range captureTriggers {
				fmt.Fprintf(&b, "CREATE OR REPLACE TRIGGER %s AFTER %s ON %s\n", tr.name, tr.event, table)
				if ref := referencing(tr.old, tr.new); ref != "" {
					fmt.Fprintf(&b, "  %s\n", ref)
				}
				fmt.Fprintf(&b, "  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture(%s);\n", args)
			}
		}
		b.WriteString("COMMIT;\n")
		for _, t := range g.tables {
			for _, child := range t.Children {
				fmt.Fprintf(&b, "-- note: %s references %s with ON DELETE CASCADE, SET NULL or SET DEFAULT and is not under undo.tables:\n",
					comment(child), comment(t.qualified()))
				fmt.Fprintf(&b, "--       when a row of %s is deleted, the rows of %s that reference it are deleted or have their\n",
					comment(t.qualified()), comment(child))
				b.WriteString("--       reference changed, which is not captured: undoing the delete does not restore them.\n")
			}
		}
	}
	return b.String(), nil
}

// DropSQL is what `stet undo-sql -drop` prints.
//
// It drops the four triggers on every table found, whatever its Problem
// (the table may have changed since they were installed), in one
// transaction per owner as InstallSQL does. Notes name the tables it leaves
// out: not found, or of a kind that has no triggers.
func DropSQL(ts []Table) string {
	var found, skipped []Table
	for _, t := range sortedByKey(ts) {
		if t.Schema == "" || t.noTriggers {
			skipped = append(skipped, t)
		} else {
			found = append(found, t)
		}
	}
	var b strings.Builder
	for i, g := range byOwner(found) {
		if i > 0 {
			b.WriteString("\n")
		}
		g.begin(&b, i+1)
		for _, t := range g.tables {
			for _, tr := range captureTriggers {
				fmt.Fprintf(&b, "DROP TRIGGER IF EXISTS %s ON %s;\n", tr.name, t.qualified())
			}
		}
		b.WriteString("COMMIT;\n")
	}
	for _, t := range skipped {
		why := "is not found"
		if t.Schema != "" {
			why = "(" + t.qualified() + ") cannot have triggers"
		}
		fmt.Fprintf(&b, "-- note: undo.tables.%s %s; there is nothing to drop on it.\n", comment(t.Key), comment(why))
	}
	return b.String()
}

// ownerGroup is the tables of one owner.
type ownerGroup struct {
	owner  string
	tables []Table
}

// begin writes the head of the owner's part, numbered n.
func (g ownerGroup) begin(b *strings.Builder, n int) {
	names := make([]string, len(g.tables))
	for i, t := range g.tables {
		names[i] = t.qualified()
	}
	fmt.Fprintf(b, "-- %d. Run as %s (the owner of %s).\n", n, comment(quoteIdent(g.owner)), comment(strings.Join(names, ", ")))
	fmt.Fprintf(b, "BEGIN;\nSET LOCAL lock_timeout = '%s';\n", lockTimeout)
}

// byOwner groups ts by owner, in byte order of owner, each group's tables
// in byte order of key.
func byOwner(ts []Table) []ownerGroup {
	var groups []ownerGroup
	for _, t := range sortedByKey(ts) {
		i := slices.IndexFunc(groups, func(g ownerGroup) bool { return g.owner == t.Owner })
		if i < 0 {
			groups = append(groups, ownerGroup{owner: t.Owner})
			i = len(groups) - 1
		}
		groups[i].tables = append(groups[i].tables, t)
	}
	slices.SortFunc(groups, func(a, b ownerGroup) int { return strings.Compare(a.owner, b.owner) })
	return groups
}

func sortedByKey(ts []Table) []Table {
	return slices.SortedStableFunc(slices.Values(ts), func(a, b Table) int { return cmp.Compare(a.Key, b.Key) })
}

// referencing is the REFERENCING clause naming old and new as the
// transition tables ("" for none), or "" when there is neither.
func referencing(old, new string) string {
	var parts []string
	if old != "" {
		parts = append(parts, "OLD TABLE AS "+quoteIdent(old))
	}
	if new != "" {
		parts = append(parts, "NEW TABLE AS "+quoteIdent(new))
	}
	if len(parts) == 0 {
		return ""
	}
	return "REFERENCING " + strings.Join(parts, " ")
}

// quoteIdent is s as an SQL identifier, quoted unless PostgreSQL's
// quote_ident would leave it bare: lower-case letters, digits and
// underscores, not starting with a digit, and no keyword other than an
// unreserved one.
func quoteIdent(s string) string {
	if bareIdent(s) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func bareIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r >= 'a' && r <= 'z' || r == '_' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	res, err := pg_query.Scan(s)
	if err != nil || len(res.Tokens) != 1 {
		return false
	}
	k := res.Tokens[0].KeywordKind
	return k == pg_query.KeywordKind_NO_KEYWORD || k == pg_query.KeywordKind_UNRESERVED_KEYWORD
}

// quoteLiteral is s as an SQL string literal. With a backslash in s it is an
// escape string (E'...'), whose meaning does not depend on
// standard_conforming_strings.
func quoteLiteral(s string) string {
	q := "'" + strings.ReplaceAll(s, "'", "''") + "'"
	if strings.Contains(s, `\`) {
		return "E" + strings.ReplaceAll(q, `\`, `\\`)
	}
	return q
}

// comment is s fit for a "--" comment: a line break would end the comment
// and make the rest of the name SQL, so control characters are written as
// \xNN.
func comment(s string) string {
	return oneLine(s)
}

// oneLine writes the control characters of s as \xNN (or \uNNNN).
func oneLine(s string) string {
	if !strings.ContainsFunc(s, unicode.IsControl) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case !unicode.IsControl(r):
			b.WriteRune(r)
		case r <= 0xff:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

func hasNUL(ss ...string) bool {
	return slices.ContainsFunc(ss, func(s string) bool { return strings.ContainsRune(s, 0) })
}

// minServer is the first PostgreSQL version undo takes: its install SQL
// replaces triggers with CREATE OR REPLACE TRIGGER, new in 14, and 14 is the
// only version tested.
const minServer = 140000

// RequireServer refuses a server older than PostgreSQL 14; versionNum is its
// server_version_num. The install SQL needs it (ruling R12); the SQL of
// DropSQL does not.
func RequireServer(versionNum int) error {
	if versionNum >= minServer {
		return nil
	}
	version := fmt.Sprint(versionNum / 10000)
	if versionNum < 100000 {
		version = fmt.Sprintf("%d.%d", versionNum/10000, versionNum/100%100)
	}
	return fmt.Errorf("undo needs PostgreSQL 14 or later (its install SQL uses CREATE OR REPLACE TRIGGER, and 14 is "+
		"the only version it is tested on); this server is PostgreSQL %s", version)
}

// ReviewerFunctions are the functions a reviewer calls to plan and undo an
// agent's changes, and the helpers those call with the reviewer's rights:
// every function of the stet schema whose name starts with undo, read from
// the catalog, each as stet.<name>(<argument types>), the text GRANT EXECUTE
// ON FUNCTION takes (no argument names, no defaults), in byte order. Nothing
// else of the schema is among them: not stet.approve or stet.reject, which
// approve held writes, nor stet.capture().
func ReviewerFunctions(ctx context.Context, q store.Querier) ([]string, error) {
	var functions []string
	if err := q.QueryRow(ctx, `
		select array(select format('stet.%I(%s)', p.proname,
		                           (select coalesce(string_agg(format_type(a.t, NULL), ', ' order by a.n), '')
		                              from unnest(p.proargtypes::oid[]) with ordinality as a(t, n)))
		               from pg_proc p
		               join pg_namespace n on n.oid = p.pronamespace
		              where n.nspname = 'stet' and p.prokind = 'f' and starts_with(p.proname::text, 'undo'))`).
		Scan(&functions); err != nil {
		return nil, err
	}
	slices.Sort(functions)
	return functions, nil
}

// GrantSQL is what `stet undo-sql -grant <role>` prints: for stetRole, which
// owns the stet schema, to run, what lets role read stet.history and plan and
// undo an agent's changes (spec 6, 7): USAGE on the stet schema, SELECT on
// stet.history, and EXECUTE on each of functions (ReviewerFunctions), in byte
// order. It grants nothing else of the schema. The rights on the tables to
// restore are their owners' to give, which a closing comment says. role is
// an identifier, quoted where it must be, and need not exist yet; in the
// comment it is written as text with its control characters escaped.
func GrantSQL(stetRole, role string, functions []string) (string, error) {
	switch {
	case role == "":
		return "", errors.New("the role to grant to is empty")
	case stetRole == "":
		return "", errors.New("the name of Stet's account is empty")
	case hasNUL(stetRole, role) || hasNUL(functions...):
		return "", errors.New("a name holds a NUL byte")
	case len(functions) == 0:
		return "", errors.New("there are no undo functions in the stet schema to grant")
	}
	to := quoteIdent(role)
	var b strings.Builder
	fmt.Fprintf(&b, "-- Run as %s (the account Stet connects as): lets \"%s\" read stet.history and undo an agent's changes.\n",
		comment(quoteIdent(stetRole)), comment(role))
	fmt.Fprintf(&b, "GRANT USAGE ON SCHEMA stet TO %s;\n", to)
	fmt.Fprintf(&b, "GRANT SELECT ON stet.history TO %s;\n", to)
	for _, f := range slices.Compact(slices.Sorted(slices.Values(functions))) {
		fmt.Fprintf(&b, "GRANT EXECUTE ON FUNCTION %s TO %s;\n", f, to)
	}
	b.WriteString("-- The role also needs SELECT, INSERT, UPDATE and DELETE on the tables it is to restore, and SELECT on the tables that\n")
	b.WriteString("-- reference those with ON DELETE or ON UPDATE CASCADE, SET NULL or SET DEFAULT; grant those as the tables' owners.\n")
	return b.String(), nil
}

package sqlparse

import (
	"slices"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// cursorOptHold is CURSOR_OPT_HOLD in PostgreSQL's parsenodes.h: the bit
// DECLARE ... WITH HOLD sets in DeclareCursorStmt.Options.
const cursorOptHold = 0x0020

// cursor reports what n, the only statement of a message, does to a SQL
// cursor, or nil.
func cursor(n *pg.Node) *Cursor {
	switch {
	case n.GetDeclareCursorStmt() != nil:
		s := n.GetDeclareCursorStmt()
		return &Cursor{Op: CursorDeclare, Name: s.Portalname, Hold: s.Options&cursorOptHold != 0}
	case n.GetFetchStmt() != nil:
		s := n.GetFetchStmt()
		if s.Ismove {
			return nil
		}
		return &Cursor{Op: CursorFetch, Name: s.Portalname}
	case n.GetClosePortalStmt() != nil:
		// CLOSE ALL carries no name; a cursor cannot be named "".
		if name := n.GetClosePortalStmt().Portalname; name != "" {
			return &Cursor{Op: CursorClose, Name: name}
		}
		return &Cursor{Op: CursorCloseAll}
	case n.GetDiscardStmt() != nil:
		// DISCARD ALL runs CLOSE ALL among its steps, so held cursors go too.
		// Poolers send it when they hand a connection back.
		if n.GetDiscardStmt().Target == pg.DiscardMode_DISCARD_ALL {
			return &Cursor{Op: CursorCloseAll}
		}
	}
	return nil
}

// readTables returns the config keys of the gated tables that stmts read,
// sorted, or nil.
//
// A relation is read when it sits in a reading position of a statement
// anywhere in the tree: the FROM of a SELECT (joins and subqueries in it
// included), the FROM of an UPDATE, the USING of a DELETE, the source of a
// MERGE, and the table of COPY ... TO. DDL and maintenance statements name
// tables outside these positions. CREATE VIEW reads nothing, and EXPLAIN
// reads nothing unless ANALYZE runs a statement that keeps the rows (see
// keepsRows). Any other statement that holds a query, such as CREATE TABLE
// AS, CREATE MATERIALIZED VIEW, or PREPARE, reads what its query reads; the
// rows a later SQL EXECUTE sends are not counted.
func readTables(stmts []*pg.RawStmt, m matchFunc) []string {
	r := reader{m: m}
	for _, raw := range stmts {
		r.visit(raw.Stmt, nil, false)
	}
	slices.Sort(r.tables)
	return r.tables
}

type reader struct {
	m      matchFunc
	tables []string
}

// visit walks msg and everything nested in it, as walk does. ctes holds the
// WITH query names in scope; reading says whether msg sits in a reading
// position. A statement decides that for each of its fields; any other node
// passes on what it was given, so the relations of a join in FROM are read.
func (r *reader) visit(msg proto.Message, ctes []string, reading bool) {
	var (
		with *pg.WithClause
		from protoreflect.Name // the field whose relations the statement reads
		stmt = true
	)
	switch s := msg.(type) {
	case *pg.RangeVar:
		if reading {
			r.read(s, ctes)
		}
		return
	case *pg.ExplainStmt:
		// EXPLAIN sends plan lines, not the query's rows. With ANALYZE it
		// also runs the statement, which reads as it would unexplained when
		// it keeps the rows somewhere.
		if explainAnalyze(s) && keepsRows(s.Query) {
			r.visit(s.Query, ctes, false)
		}
		return
	case *pg.ViewStmt:
		// CREATE VIEW only defines the query.
		return
	case *pg.SelectStmt:
		with, from = s.WithClause, "from_clause"
	case *pg.UpdateStmt:
		with, from = s.WithClause, "from_clause"
	case *pg.DeleteStmt:
		with, from = s.WithClause, "using_clause"
	case *pg.MergeStmt:
		with, from = s.WithClause, "source_relation"
	case *pg.InsertStmt:
		with = s.WithClause
	case *pg.CopyStmt:
		if !s.IsFrom {
			from = "relation"
		}
	default:
		stmt = false
	}
	if with != nil {
		ctes = r.with(with, ctes)
	}
	msg.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Message() == nil || fd.IsMap() || (with != nil && fd.Name() == "with_clause") {
			return true
		}
		rd := reading
		if stmt {
			rd = fd.Name() == from
		}
		if fd.IsList() {
			l := v.List()
			for i := range l.Len() {
				r.visit(l.Get(i).Message().Interface(), ctes, rd)
			}
			return true
		}
		r.visit(v.Message().Interface(), ctes, rd)
		return true
	})
}

// explainAnalyze reports whether EXPLAIN runs its statement. As Postgres
// reads the option (defGetBoolean), the last ANALYZE wins, and no value, a
// nonzero integer, true, or on turns it on. A value Postgres rejects counts
// as on: the statement then fails anyway.
func explainAnalyze(s *pg.ExplainStmt) bool {
	on := false
	for _, o := range s.Options {
		d := o.GetDefElem()
		if d.GetDefname() != "analyze" {
			continue
		}
		on = true
		switch arg := d.GetArg(); {
		case arg.GetString_() != nil:
			v := arg.GetString_().GetSval()
			on = !strings.EqualFold(v, "false") && !strings.EqualFold(v, "off")
		case arg.GetInteger() != nil:
			on = arg.GetInteger().GetIval() != 0
		}
	}
	return on
}

// keepsRows reports whether running n can keep the rows it reads: it
// creates a table (CREATE TABLE AS, SELECT INTO, CREATE MATERIALIZED VIEW)
// or writes one, anywhere in its tree.
func keepsRows(n *pg.Node) bool {
	found := false
	walk(n, func(msg proto.Message) bool {
		switch msg.(type) {
		case *pg.IntoClause, *pg.InsertStmt, *pg.UpdateStmt, *pg.DeleteStmt, *pg.MergeStmt:
			found = true
		}
		return !found
	})
	return found
}

// with visits the queries of a WITH clause and returns the names in scope
// in the statement it belongs to. As in Postgres, a query sees the names
// before it, or with RECURSIVE all of them.
func (r *reader) with(w *pg.WithClause, outer []string) []string {
	// Clip so that appending, here or in a nested WITH, never writes into
	// a slice an enclosing scope still holds.
	all := slices.Clip(outer)
	for _, n := range w.Ctes {
		all = append(all, n.GetCommonTableExpr().GetCtename())
	}
	for i, n := range w.Ctes {
		seen := all[:len(outer)+i]
		if w.Recursive {
			seen = all
		}
		if q := n.GetCommonTableExpr().GetCtequery(); q != nil {
			r.visit(q, seen, false)
		}
	}
	return all
}

// read records rv if it names a gated table rather than a WITH query in
// scope. A qualified name always names a table.
func (r *reader) read(rv *pg.RangeVar, ctes []string) {
	if rv.Schemaname == "" && rv.Catalogname == "" && slices.Contains(ctes, rv.Relname) {
		return
	}
	if t, ok := r.m(rv); ok && !slices.Contains(r.tables, t.key) {
		r.tables = append(r.tables, t.key)
	}
}

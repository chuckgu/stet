package sqlparse

import (
	"slices"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"
)

// measuredReasons are the Unsupported reasons a write that may touch many
// rows can carry. Any other reason keeps the write Unsupported.
var measuredReasons = map[string]bool{
	"not_single_row": true,
	"join":           true,
	"subquery":       true,
	"with":           true,
	"multi_row":      true,
	"insert_select":  true,
}

// countFuncs are the functions a WHERE clause may call and still be run to
// count rows: built-ins that neither execute SQL nor change session state.
// Others, such as set_config and query_to_xml, can escape SET LOCAL ROLE.
var countFuncs = map[string]bool{
	"now":        true,
	"lower":      true,
	"upper":      true,
	"length":     true,
	"date_trunc": true,
	"abs":        true,
}

// measured upgrades an Unsupported result to Measured when the only thing
// wrong with the write is that it may touch many rows. The reason is kept,
// so a table without impact limits records what it recorded before.
func measured(n *pg.Node, m matchFunc, r Result) Result {
	if r.Kind != Unsupported || !measuredReasons[r.Reason] || !mayTouchManyRows(n, m) || gatedWrites(n, m) != 1 {
		return r
	}
	r.Kind, r.Measure = Measured, measure(n)
	return r
}

// mayTouchManyRows reports whether n is an INSERT, UPDATE, or DELETE of a
// gated table whose values can be read (no ON CONFLICT, multiple-column
// assignment, or indirection) and that is not limited to one row.
func mayTouchManyRows(n *pg.Node, m matchFunc) bool {
	switch {
	case n.GetInsertStmt() != nil:
		s := n.GetInsertStmt()
		if _, ok := m(s.Relation); !ok || s.OnConflictClause != nil || s.SelectStmt == nil {
			return false // a nil SelectStmt is DEFAULT VALUES: one row
		}
		for _, c := range s.Cols {
			if len(c.GetResTarget().GetIndirection()) > 0 {
				return false
			}
		}
		// Several VALUES lists, or a SELECT (no VALUES lists at the top).
		return len(s.SelectStmt.GetSelectStmt().GetValuesLists()) != 1
	case n.GetUpdateStmt() != nil:
		s := n.GetUpdateStmt()
		for _, t := range s.TargetList {
			rt := t.GetResTarget()
			if rt.GetVal().GetMultiAssignRef() != nil || len(rt.GetIndirection()) > 0 {
				return false
			}
		}
		return spansRows(s.Relation, s.WhereClause, m)
	case n.GetDeleteStmt() != nil:
		s := n.GetDeleteStmt()
		return spansRows(s.Relation, s.WhereClause, m)
	}
	return false
}

// spansRows reports whether where may match more than one row of the gated
// table rel: it neither pins the primary key (see pkEquality) nor names a
// cursor's current row. WHERE CURRENT OF touches one row whose values cannot
// be read; measuring it would find one row and skip value-level judgment.
// The grammar only allows CURRENT OF as the whole WHERE clause.
func spansRows(rel *pg.RangeVar, where *pg.Node, m matchFunc) bool {
	t, ok := m(rel)
	if !ok || where.GetCurrentOfExpr() != nil {
		return false
	}
	_, single := pkEquality(where, rel, t.pk)
	return !single
}

// gatedWrites counts the statements nested anywhere in n that write a gated
// table, as firstGatedWrite finds them. It stops counting at two.
func gatedWrites(n *pg.Node, m matchFunc) int {
	count := 0
	walk(n, func(msg proto.Message) bool {
		if writesGated(msg, m) {
			count++
		}
		return count < 2
	})
	return count
}

func writesGated(msg proto.Message, m matchFunc) bool {
	hit := func(rv *pg.RangeVar) bool {
		_, ok := m(rv)
		return ok
	}
	switch s := msg.(type) {
	case *pg.InsertStmt:
		return hit(s.Relation)
	case *pg.UpdateStmt:
		return hit(s.Relation)
	case *pg.DeleteStmt:
		return hit(s.Relation)
	case *pg.MergeStmt:
		return hit(s.Relation)
	case *pg.CopyStmt:
		return s.IsFrom && hit(s.Relation)
	case *pg.TruncateStmt:
		for _, r := range s.Relations {
			if hit(r.GetRangeVar()) {
				return true
			}
		}
	}
	return false
}

// measure picks how to learn the row count of a write mayTouchManyRows
// accepted: count it when Stet may run its WHERE clause alone, read it from
// the VALUES lists of a plain multi-row INSERT, and otherwise estimate it.
func measure(n *pg.Node) *Measure {
	switch {
	case n.GetInsertStmt() != nil:
		s := n.GetInsertStmt()
		m := &Measure{Method: MethodEstimate, Relation: relation(s.Relation)}
		if rows := len(s.SelectStmt.GetSelectStmt().GetValuesLists()); rows > 1 && s.WithClause == nil && !hasSubLink(s) {
			m.Method, m.Rows = MethodSyntax, rows
		}
		return m
	case n.GetUpdateStmt() != nil:
		s := n.GetUpdateStmt()
		m := &Measure{Method: MethodEstimate, Relation: relation(s.Relation)}
		if s.WithClause == nil && len(s.FromClause) == 0 && !hasSubLink(s) {
			if sql, ok := countSQL(s.Relation, s.WhereClause); ok {
				m.Method, m.CountSQL, m.Qualified = MethodCount, sql, qualified(s.WhereClause)
			}
		}
		return m
	case n.GetDeleteStmt() != nil:
		s := n.GetDeleteStmt()
		m := &Measure{Method: MethodEstimate, Relation: relation(s.Relation)}
		if s.WithClause == nil && len(s.UsingClause) == 0 && !hasSubLink(s) {
			if sql, ok := countSQL(s.Relation, s.WhereClause); ok {
				m.Method, m.CountSQL, m.Qualified = MethodCount, sql, qualified(s.WhereClause)
			}
		}
		return m
	}
	return &Measure{Method: MethodEstimate}
}

// qualified returns the last name of each column reference in where that
// has a qualifier (t.f, s.t.f, c.s.t.f), sorted, without duplicates, or nil.
// Postgres reads such a name as column f of t or, when t has no column f, as
// the call f(t) of a one-argument function f that accepts t's row; only the
// catalog can tell which. A bare name is a column or a whole row, never a
// call, and t.* is a whole row.
func qualified(where *pg.Node) []string {
	if where == nil {
		return nil
	}
	var names []string
	walk(where, func(msg proto.Message) bool {
		if ref, ok := msg.(*pg.ColumnRef); ok && len(ref.Fields) > 1 {
			if s := ref.Fields[len(ref.Fields)-1].GetString_(); s != nil {
				names = append(names, s.Sval)
			}
		}
		return true
	})
	slices.Sort(names)
	return slices.Compact(names)
}

// countSQL builds SELECT 1 FROM rel [WHERE where] from the parse tree, so
// the relation keeps its schema, ONLY, and alias. It refuses a WHERE clause
// that is not countable or does not deparse.
func countSQL(rel *pg.RangeVar, where *pg.Node) (string, bool) {
	if where != nil && !countable(where) {
		return "", false
	}
	out, err := selectOne(rel, where)
	return out, err == nil
}

// relation renders rel as CountSQL names it, but without ONLY or alias:
// the deparser quotes each part as SQL needs, and schema and catalog stay
// only if the statement gave them. "" if it does not deparse, which makes
// the catalog lookups, and so the measurement, fail.
func relation(rel *pg.RangeVar) string {
	bare := &pg.RangeVar{
		Catalogname:    rel.GetCatalogname(),
		Schemaname:     rel.GetSchemaname(),
		Relname:        rel.GetRelname(),
		Inh:            true,
		Relpersistence: rel.GetRelpersistence(),
	}
	out, err := selectOne(bare, nil)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(out, "SELECT 1 FROM ")
}

// selectOne deparses SELECT 1 FROM rel [WHERE where].
func selectOne(rel *pg.RangeVar, where *pg.Node) (string, error) {
	sel := &pg.SelectStmt{
		TargetList:  []*pg.Node{pg.MakeResTargetNodeWithVal(pg.MakeAConstIntNode(1, 0), 0)},
		FromClause:  []*pg.Node{{Node: &pg.Node_RangeVar{RangeVar: rel}}},
		WhereClause: where,
		Op:          pg.SetOperation_SETOP_NONE,
		LimitOption: pg.LimitOption_LIMIT_OPTION_DEFAULT,
	}
	return pg.Deparse(&pg.ParseResult{
		Version: treeVersion,
		Stmts:   []*pg.RawStmt{{Stmt: &pg.Node{Node: &pg.Node_SelectStmt{SelectStmt: sel}}}},
	})
}

// countable reports whether Stet may run where on its own connection: no
// WHERE CURRENT OF, no query, every function call on countFuncs, no
// field selection, and no schema-qualified operator (OPERATOR(schema.op)).
//
// A query can sit outside a SubLink, where hasSubLink does not see it:
// JSON_ARRAY(SELECT ...) holds a bare SelectStmt. Any SelectStmt refuses.
//
// A function can be called without call syntax: Postgres resolves (x).f to
// f(x) for any f that takes one argument, so field selection refuses too.
// Array subscripts (x[1], x[1:2]) are the only indirection allowed. A
// qualified name t.f is the call f(t) when t has no column f; that takes the
// catalog to tell, so such names are left to the caller (Measure.Qualified).
//
// The parser follows the PostgreSQL 17 grammar, where JSON_VALUE(...),
// JSON(...), JSON_ARRAY(a, b), merge_action() and their relatives are keyword
// forms with no FuncCall node. An older server reads the deparsed text as a
// call to an ordinary function of that name, found through search_path. So
// every SQL/JSON node and merge_action() refuse.
func countable(where *pg.Node) bool {
	ok := true
	walk(where, func(msg proto.Message) bool {
		switch e := msg.(type) {
		case *pg.CurrentOfExpr, *pg.SelectStmt, *pg.MergeSupportFunc:
			ok = false
		case *pg.FuncCall:
			ok = countFunc(e.Funcname)
		case *pg.A_Expr:
			ok = len(e.Name) < 2
		case *pg.A_Indirection:
			for _, i := range e.Indirection {
				if i.GetAIndices() == nil {
					ok = false
				}
			}
		}
		if strings.HasPrefix(string(msg.ProtoReflect().Descriptor().Name()), "Json") {
			ok = false
		}
		return ok
	})
	return ok
}

// countFunc reports whether a function name is on countFuncs, bare or
// qualified by pg_catalog. SQL-standard forms arrive qualified: TRIM(x) is
// pg_catalog.btrim, which is not on the list.
func countFunc(name []*pg.Node) bool {
	switch len(name) {
	case 1:
		return countFuncs[name[0].GetString_().GetSval()]
	case 2:
		return name[0].GetString_().GetSval() == "pg_catalog" && countFuncs[name[1].GetString_().GetSval()]
	}
	return false
}

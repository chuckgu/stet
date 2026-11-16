package sqlparse

import (
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
// table rel: it does not pin the primary key (see pkEquality).
func spansRows(rel *pg.RangeVar, where *pg.Node, m matchFunc) bool {
	t, ok := m(rel)
	if !ok {
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
		rows := len(s.SelectStmt.GetSelectStmt().GetValuesLists())
		if rows > 1 && s.WithClause == nil && !hasSubLink(s) {
			return &Measure{Method: MethodSyntax, Rows: rows}
		}
	case n.GetUpdateStmt() != nil:
		s := n.GetUpdateStmt()
		if s.WithClause == nil && len(s.FromClause) == 0 && !hasSubLink(s) {
			if sql, ok := countSQL(s.Relation, s.WhereClause); ok {
				return &Measure{Method: MethodCount, CountSQL: sql}
			}
		}
	case n.GetDeleteStmt() != nil:
		s := n.GetDeleteStmt()
		if s.WithClause == nil && len(s.UsingClause) == 0 && !hasSubLink(s) {
			if sql, ok := countSQL(s.Relation, s.WhereClause); ok {
				return &Measure{Method: MethodCount, CountSQL: sql}
			}
		}
	}
	return &Measure{Method: MethodEstimate}
}

// countSQL builds SELECT 1 FROM rel [WHERE where] from the parse tree, so
// the relation keeps its schema, ONLY, and alias. It refuses a WHERE clause
// that is not countable or does not deparse.
func countSQL(rel *pg.RangeVar, where *pg.Node) (string, bool) {
	if where != nil && !countable(where) {
		return "", false
	}
	sel := &pg.SelectStmt{
		TargetList:  []*pg.Node{pg.MakeResTargetNodeWithVal(pg.MakeAConstIntNode(1, 0), 0)},
		FromClause:  []*pg.Node{{Node: &pg.Node_RangeVar{RangeVar: rel}}},
		WhereClause: where,
		Op:          pg.SetOperation_SETOP_NONE,
		LimitOption: pg.LimitOption_LIMIT_OPTION_DEFAULT,
	}
	out, err := pg.Deparse(&pg.ParseResult{
		Version: treeVersion,
		Stmts:   []*pg.RawStmt{{Stmt: &pg.Node{Node: &pg.Node_SelectStmt{SelectStmt: sel}}}},
	})
	return out, err == nil
}

// countable reports whether Stet may run where on its own connection: no
// WHERE CURRENT OF, no query, every function call on countFuncs, and no
// schema-qualified operator (OPERATOR(schema.op)).
//
// A query can sit outside a SubLink, where hasSubLink does not see it:
// JSON_ARRAY(SELECT ...) holds a bare SelectStmt. Any SelectStmt refuses.
func countable(where *pg.Node) bool {
	ok := true
	walk(where, func(msg proto.Message) bool {
		switch e := msg.(type) {
		case *pg.CurrentOfExpr, *pg.SelectStmt:
			ok = false
		case *pg.FuncCall:
			ok = countFunc(e.Funcname)
		case *pg.A_Expr:
			ok = len(e.Name) < 2
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

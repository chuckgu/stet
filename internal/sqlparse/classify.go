// Package sqlparse decides which statements touch gated tables, using
// Postgres' own parser.
//
// Only writes visible in the statement text are seen. Writes done inside
// functions, DO blocks, triggers, or rules are not; deterministic database
// permissions remain the guard for those.
package sqlparse

import (
	"strconv"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type Kind int

const (
	Pass        Kind = iota // writes no gated table
	Gated                   // single-row write that v0.1 judges
	Unsupported             // writes a gated table in a shape v0.1 does not judge
)

func (k Kind) String() string {
	switch k {
	case Pass:
		return "pass"
	case Gated:
		return "gated"
	case Unsupported:
		return "unsupported"
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

type ValueKind int

const (
	Literal ValueKind = iota + 1
	Param
	Null
	Default
	Expr
)

type Value struct {
	Kind  ValueKind
	Text  string // Literal: the constant as text. Expr: the deparsed expression.
	Param int    // Param: n in $n, filled from Bind
}

type Write struct {
	Op      string   // insert | update | delete
	Table   string   // config key of the gated table
	Columns []string // insert target or update SET columns; nil for insert without a column list
	Values  []Value  // aligned with Columns, or positional when Columns is nil
	PK      *Value   // update/delete: the WHERE pk = value; insert: the pk column's value if given
}

type Result struct {
	Kind   Kind
	Reason string // why Unsupported
	Write  *Write // set when Gated

	// Table and Op name the first gated write found (Gated or Unsupported).
	// Op is insert, update, delete, merge, copy, or truncate.
	Table string
	Op    string

	// OnBehalfOf is set when the SQL sets or resets stet.on_behalf_of;
	// "" means reset.
	OnBehalfOf *string
}

const onBehalfOfSetting = "stet.on_behalf_of"

// Classify parses sql (the text of one Query or Parse message). gated maps a
// table name, optionally schema-qualified, to its primary key column.
//
// An unqualified config name matches the table in any schema, and a
// qualified one also matches unqualified references, since search_path may
// resolve them there. Over-matching only means more writes get judged.
func Classify(sql string, gated map[string]string) (Result, error) {
	tree, err := pg.Parse(sql)
	if err != nil {
		return Result{}, err
	}
	m := matcher(gated)
	var (
		touching int
		target   gatedWrite
		obo      *string
	)
	for _, raw := range tree.Stmts {
		if w, ok := firstGatedWrite(raw.Stmt, m); ok {
			if touching == 0 {
				target = w
			}
			touching++
		}
		if v, ok := onBehalfOf(raw.Stmt); ok {
			obo = &v
		}
	}
	if touching == 0 {
		return Result{Kind: Pass, OnBehalfOf: obo}, nil
	}

	var r Result
	if len(tree.Stmts) > 1 {
		// Several statements in one Query run as one implicit transaction.
		r = unsupported("multi_statement")
	} else {
		r = classifyStmt(tree.Stmts[0].Stmt, m)
	}
	r.Table, r.Op, r.OnBehalfOf = target.table, target.op, obo
	return r, nil
}

func classifyStmt(n *pg.Node, m matchFunc) Result {
	switch {
	case n.GetInsertStmt() != nil:
		return classifyInsert(n.GetInsertStmt(), m)
	case n.GetUpdateStmt() != nil:
		return classifyUpdate(n.GetUpdateStmt(), m)
	case n.GetDeleteStmt() != nil:
		return classifyDelete(n.GetDeleteStmt(), m)
	case n.GetCopyStmt() != nil:
		return unsupported("copy")
	case n.GetTruncateStmt() != nil:
		return unsupported("truncate")
	case n.GetMergeStmt() != nil:
		return unsupported("merge")
	}
	return unsupported("nested_write")
}

// Fingerprint identifies a statement's shape regardless of its constants,
// or returns "" if sql does not parse.
func Fingerprint(sql string) string {
	fp, err := pg.Fingerprint(sql)
	if err != nil {
		return ""
	}
	return fp
}

// onBehalfOf reads SET / RESET of stet.on_behalf_of. "" means reset.
func onBehalfOf(n *pg.Node) (string, bool) {
	s := n.GetVariableSetStmt()
	if s == nil {
		return "", false
	}
	switch s.Kind {
	case pg.VariableSetKind_VAR_RESET_ALL:
		return "", true
	case pg.VariableSetKind_VAR_RESET, pg.VariableSetKind_VAR_SET_DEFAULT:
		return "", s.Name == onBehalfOfSetting
	case pg.VariableSetKind_VAR_SET_VALUE:
		if s.Name != onBehalfOfSetting || len(s.Args) != 1 || s.Args[0].GetAConst() == nil {
			return "", false
		}
		return constant(s.Args[0].GetAConst()).Text, true
	}
	return "", false
}

func unsupported(reason string) Result {
	return Result{Kind: Unsupported, Reason: reason}
}

type gatedTable struct{ key, pk string }

type matchFunc func(*pg.RangeVar) (gatedTable, bool)

func matcher(gated map[string]string) matchFunc {
	return func(rv *pg.RangeVar) (gatedTable, bool) {
		if rv == nil {
			return gatedTable{}, false
		}
		if rv.Schemaname != "" {
			key := rv.Schemaname + "." + rv.Relname
			if pk, ok := gated[key]; ok {
				return gatedTable{key, pk}, true
			}
		}
		if pk, ok := gated[rv.Relname]; ok {
			return gatedTable{rv.Relname, pk}, true
		}
		if rv.Schemaname == "" {
			for key, pk := range gated {
				if _, rel, ok := strings.Cut(key, "."); ok && rel == rv.Relname {
					return gatedTable{key, pk}, true
				}
			}
		}
		return gatedTable{}, false
	}
}

type gatedWrite struct{ table, op string }

// firstGatedWrite finds the first statement nested anywhere in n that writes
// a gated table: CTEs, EXPLAIN ANALYZE, PREPARE, and rule actions included.
func firstGatedWrite(n *pg.Node, m matchFunc) (gatedWrite, bool) {
	var found gatedWrite
	hit := func(rv *pg.RangeVar, op string) bool {
		if t, ok := m(rv); ok {
			found = gatedWrite{t.key, op}
			return true
		}
		return false
	}
	ok := false
	walk(n, func(msg proto.Message) bool {
		switch s := msg.(type) {
		case *pg.InsertStmt:
			ok = hit(s.Relation, "insert")
		case *pg.UpdateStmt:
			ok = hit(s.Relation, "update")
		case *pg.DeleteStmt:
			ok = hit(s.Relation, "delete")
		case *pg.MergeStmt:
			ok = hit(s.Relation, "merge")
		case *pg.CopyStmt:
			ok = s.IsFrom && hit(s.Relation, "copy")
		case *pg.TruncateStmt:
			for _, r := range s.Relations {
				if hit(r.GetRangeVar(), "truncate") {
					ok = true
					break
				}
			}
		}
		return !ok
	})
	return found, ok
}

func classifyInsert(s *pg.InsertStmt, m matchFunc) Result {
	t, ok := m(s.Relation)
	if !ok {
		return unsupported("nested_write")
	}
	if s.WithClause != nil {
		return unsupported("with")
	}
	var row []*pg.Node
	if s.SelectStmt != nil { // nil means DEFAULT VALUES
		sel := s.SelectStmt.GetSelectStmt()
		switch {
		case sel == nil || len(sel.ValuesLists) == 0:
			return unsupported("insert_select")
		case len(sel.ValuesLists) > 1:
			return unsupported("multi_row")
		}
		row = sel.ValuesLists[0].GetList().GetItems()
	}
	if s.OnConflictClause != nil {
		return unsupported("on_conflict")
	}
	for _, c := range s.Cols {
		if len(c.GetResTarget().GetIndirection()) > 0 {
			return unsupported("indirection")
		}
	}
	if hasSubLink(s) {
		return unsupported("subquery")
	}

	w := &Write{Op: "insert", Table: t.key}
	for _, c := range s.Cols {
		w.Columns = append(w.Columns, c.GetResTarget().GetName())
	}
	for _, v := range row {
		w.Values = append(w.Values, value(v))
	}
	for i, c := range w.Columns {
		if c == t.pk && i < len(w.Values) {
			pk := w.Values[i]
			w.PK = &pk
		}
	}
	return Result{Kind: Gated, Write: w}
}

func classifyUpdate(s *pg.UpdateStmt, m matchFunc) Result {
	t, ok := m(s.Relation)
	if !ok {
		return unsupported("nested_write")
	}
	if s.WithClause != nil {
		return unsupported("with")
	}
	if len(s.FromClause) > 0 {
		return unsupported("join")
	}
	for _, n := range s.TargetList {
		rt := n.GetResTarget()
		if rt.GetVal().GetMultiAssignRef() != nil {
			return unsupported("multi_assign")
		}
		if len(rt.GetIndirection()) > 0 {
			return unsupported("indirection")
		}
	}
	if hasSubLink(s) {
		return unsupported("subquery")
	}
	pk, ok := pkEquality(s.WhereClause, s.Relation, t.pk)
	if !ok {
		return unsupported("not_single_row")
	}

	w := &Write{Op: "update", Table: t.key, PK: &pk}
	for _, n := range s.TargetList {
		rt := n.GetResTarget()
		w.Columns = append(w.Columns, rt.GetName())
		w.Values = append(w.Values, value(rt.GetVal()))
	}
	return Result{Kind: Gated, Write: w}
}

func classifyDelete(s *pg.DeleteStmt, m matchFunc) Result {
	t, ok := m(s.Relation)
	if !ok {
		return unsupported("nested_write")
	}
	if s.WithClause != nil {
		return unsupported("with")
	}
	if len(s.UsingClause) > 0 {
		return unsupported("join")
	}
	if hasSubLink(s) {
		return unsupported("subquery")
	}
	pk, ok := pkEquality(s.WhereClause, s.Relation, t.pk)
	if !ok {
		return unsupported("not_single_row")
	}
	return Result{Kind: Gated, Write: &Write{Op: "delete", Table: t.key, PK: &pk}}
}

// pkEquality accepts exactly `pk = value` (either order), where value is a
// non-null literal or a parameter. Anything else may touch more than one row.
func pkEquality(where *pg.Node, rel *pg.RangeVar, pk string) (Value, bool) {
	e := where.GetAExpr()
	if e == nil || e.Kind != pg.A_Expr_Kind_AEXPR_OP || len(e.Name) != 1 || e.Name[0].GetString_().GetSval() != "=" {
		return Value{}, false
	}
	for _, side := range [][2]*pg.Node{{e.Lexpr, e.Rexpr}, {e.Rexpr, e.Lexpr}} {
		if !isColumn(side[0], rel, pk) {
			continue
		}
		v := value(side[1])
		if v.Kind == Literal || v.Kind == Param {
			return v, true
		}
	}
	return Value{}, false
}

// isColumn reports whether n names column col of rel: bare, or qualified by
// the alias, table, or schema.table.
func isColumn(n *pg.Node, rel *pg.RangeVar, col string) bool {
	ref := n.GetColumnRef()
	if ref == nil {
		return false
	}
	var parts []string
	for _, f := range ref.Fields {
		s := f.GetString_()
		if s == nil {
			return false // A_Star
		}
		parts = append(parts, s.Sval)
	}
	switch len(parts) {
	case 1:
		return parts[0] == col
	case 2:
		qual := rel.Relname
		if rel.Alias != nil {
			qual = rel.Alias.Aliasname
		}
		return parts[0] == qual && parts[1] == col
	case 3:
		return rel.Alias == nil && parts[0] == rel.Schemaname && parts[1] == rel.Relname && parts[2] == col
	}
	return false
}

func value(n *pg.Node) Value {
	switch {
	case n.GetAConst() != nil:
		return constant(n.GetAConst())
	case n.GetParamRef() != nil:
		return Value{Kind: Param, Param: int(n.GetParamRef().Number)}
	case n.GetSetToDefault() != nil:
		return Value{Kind: Default}
	case n.GetTypeCast() != nil:
		// 'x'::text and $1::int carry the same value as their argument.
		arg := n.GetTypeCast().Arg
		if arg.GetAConst() != nil || arg.GetParamRef() != nil {
			return value(arg)
		}
	}
	return Value{Kind: Expr, Text: deparse(n)}
}

func constant(c *pg.A_Const) Value {
	if c.Isnull {
		return Value{Kind: Null}
	}
	var s string
	switch v := c.Val.(type) {
	case *pg.A_Const_Ival:
		s = strconv.Itoa(int(v.Ival.Ival))
	case *pg.A_Const_Fval:
		s = v.Fval.Fval
	case *pg.A_Const_Boolval:
		s = strconv.FormatBool(v.Boolval.Boolval)
	case *pg.A_Const_Sval:
		s = v.Sval.Sval
	case *pg.A_Const_Bsval:
		s = v.Bsval.Bsval
	}
	return Value{Kind: Literal, Text: s}
}

// treeVersion is the parse tree format of the linked libpg_query. Deparse
// refuses trees that do not carry it.
var treeVersion = func() int32 {
	tree, err := pg.Parse("select 1")
	if err != nil {
		panic(err)
	}
	return tree.Version
}()

// deparse renders an expression node back to SQL by wrapping it in SELECT.
func deparse(n *pg.Node) string {
	sel := &pg.SelectStmt{
		TargetList:  []*pg.Node{pg.MakeResTargetNodeWithVal(n, 0)},
		Op:          pg.SetOperation_SETOP_NONE,
		LimitOption: pg.LimitOption_LIMIT_OPTION_DEFAULT,
	}
	out, err := pg.Deparse(&pg.ParseResult{
		Version: treeVersion,
		Stmts:   []*pg.RawStmt{{Stmt: &pg.Node{Node: &pg.Node_SelectStmt{SelectStmt: sel}}}},
	})
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(out, "SELECT ")
}

func hasSubLink(m proto.Message) bool {
	found := false
	walk(m, func(msg proto.Message) bool {
		_, found = msg.(*pg.SubLink)
		return !found
	})
	return found
}

// walk visits m and every message nested in it, depth first, until fn
// returns false.
func walk(m proto.Message, fn func(proto.Message) bool) bool {
	if !fn(m) {
		return false
	}
	more := true
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Message() == nil || fd.IsMap() {
			return true
		}
		if fd.IsList() {
			l := v.List()
			for i := range l.Len() {
				if !walk(l.Get(i).Message().Interface(), fn) {
					more = false
					return false
				}
			}
			return true
		}
		if !walk(v.Message().Interface(), fn) {
			more = false
			return false
		}
		return true
	})
	return more
}

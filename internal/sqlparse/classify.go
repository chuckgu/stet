// Package sqlparse decides which statements touch gated tables, using
// Postgres' own parser.
//
// Only writes visible in the statement text are seen. Writes done inside
// functions, DO blocks, triggers, or rules are not; deterministic database
// permissions remain the guard for those.
package sqlparse

import (
	"slices"
	"strconv"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// PostgresMajor is the PostgreSQL version whose grammar Classify reads.
const PostgresMajor = 17

type Kind int

const (
	Pass        Kind = iota // neither writes nor reads a gated table
	Gated                   // single-row write that v0.1 judges
	Unsupported             // writes a gated table in a shape v0.1 does not judge
	Measured                // multi-row write of a gated table whose row count can be learned
	Read                    // reads a gated table and writes none
)

func (k Kind) String() string {
	switch k {
	case Pass:
		return "pass"
	case Gated:
		return "gated"
	case Unsupported:
		return "unsupported"
	case Measured:
		return "measured"
	case Read:
		return "read"
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

type CursorOp int

const (
	CursorDeclare  CursorOp = iota + 1
	CursorFetch             // FETCH; MOVE sends no rows and is not one
	CursorClose             // CLOSE name
	CursorCloseAll          // CLOSE ALL, or DISCARD ALL
)

// Cursor says what a statement does to a SQL cursor.
type Cursor struct {
	Op   CursorOp
	Name string // as the parser normalizes it: unquoted names lowercased; "" for CLOSE ALL
	Hold bool   // DECLARE ... WITH HOLD
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

type Method string

const (
	MethodCount    Method = "count"
	MethodEstimate Method = "estimate"
	MethodSyntax   Method = "syntax"
)

// Measure says how to learn how many rows a multi-row write touches.
type Measure struct {
	Method Method
	// Relation is the written table as the statement names it, quoted for
	// SQL and schema-qualified when the statement qualifies it, without ONLY
	// or alias: what CountSQL counts, and what catalog lookups must name.
	// It is not the config key, which may resolve to another table.
	Relation string
	CountSQL string // MethodCount: "SELECT 1 FROM <relation> [WHERE ...]", no LIMIT
	Rows     int    // MethodSyntax: the number of VALUES lists
	// Qualified lists the names the WHERE clause qualifies by a table, alias, or
	// schema.table (c.x, public.contacts.x), sorted, without duplicates; nil when
	// none. Such a name may be a function called as an attribute, so the count
	// runs only when each is a column of Relation. Set only with MethodCount,
	// the one method that runs the WHERE clause.
	Qualified []string
}

type Result struct {
	Kind Kind
	// Reason says why Unsupported. A Measured write keeps the reason it
	// would otherwise be Unsupported for, so a table without impact limits
	// records it as before.
	Reason  string
	Write   *Write   // set when Gated
	Measure *Measure // set exactly when Kind is Measured

	// Table and Op name the first gated write found (Gated, Unsupported,
	// or Measured). Op is insert, update, delete, merge, copy, or truncate.
	Table string
	Op    string

	// Reads holds the config keys of the gated tables the statements read,
	// sorted, and is set exactly when Kind is Read. A statement that writes
	// a gated table is classified as that write; what it reads is not kept.
	// Reads are looked for only when Tables.Reads is set.
	Reads []string

	// Cursors lists what the statements do to SQL cursors, in statement
	// order: each DECLARE CURSOR, FETCH, CLOSE, and DISCARD ALL (as CLOSE
	// ALL). A DECLARE alone is Read or Pass by what its query reads; the
	// others alone are Pass.
	Cursors []Cursor

	// Statements is how many statements the SQL holds.
	Statements int

	// OnBehalfOf is set when the SQL sets or resets stet.on_behalf_of;
	// "" means reset.
	OnBehalfOf *string

	Clean bool // every statement is BEGIN, START TRANSACTION, SET or SHOW; true for SQL with no statement
	Do    bool // some statement is a DO block
	// DefinesCode: some statement defines a function, procedure, rule or
	// trigger, or a view whose query reads a gated table (see definesCode):
	// writes through it would reach a gated table unseen.
	DefinesCode bool
	Executes    []string // prepared statements run by SQL EXECUTE, wherever it sits (EXPLAIN EXECUTE, CREATE TABLE AS EXECUTE), in order
	Portals     []string // portals read by FETCH or MOVE, in order
}

const onBehalfOfSetting = "stet.on_behalf_of"

// Tables are the gated tables Classify looks for.
type Tables struct {
	// PK maps a table name, optionally schema-qualified (the config key), to
	// its primary key column.
	PK map[string]string
	// Reads says whether any of them records reads. Without it no statement
	// is examined for reads, so none is Read.
	Reads bool
}

// Classify parses sql (the text of one Query or Parse message).
//
// SQL that writes no gated table is Read when any of its statements reads
// one and tables.Reads is set, and Pass otherwise.
//
// An unqualified config name matches the table in any schema, and a
// qualified one also matches unqualified references, since search_path may
// resolve them there. Over-matching only means more writes get judged and
// more reads counted.
func Classify(sql string, tables Tables) (Result, error) {
	tree, err := pg.Parse(sql)
	if err != nil {
		return Result{}, err
	}
	m := matcher(tables.PK)
	var (
		touching int
		target   gatedWrite
		obo      *string
		cursors  []Cursor
		executes []string
		portals  []string
		clean    = true
		do       bool
		defines  bool
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
		if c, ok := cursor(raw.Stmt); ok {
			cursors = append(cursors, c)
		}
		if f := raw.Stmt.GetFetchStmt(); f != nil {
			portals = append(portals, f.Portalname)
		}
		executes = appendExecutes(executes, raw.Stmt)
		clean = clean && leavesClean(raw.Stmt)
		do = do || raw.Stmt.GetDoStmt() != nil
		defines = defines || definesCode(raw.Stmt, m)
	}

	var r Result
	switch {
	case touching == 0:
		r.Kind = Pass
		// With nothing to record them, reads are not looked for: the walk
		// would cost every statement.
		if tables.Reads {
			if reads := readTables(tree.Stmts, m); reads != nil {
				r.Kind, r.Reads = Read, reads
			}
		}
	case len(tree.Stmts) > 1:
		// Several statements in one Query run as one implicit transaction.
		r = unsupported("multi_statement")
	default:
		n := tree.Stmts[0].Stmt
		r = measured(n, m, classifyStmt(n, m))
	}
	r.Table, r.Op = target.table, target.op // empty when nothing gated is written
	r.OnBehalfOf, r.Cursors, r.Statements = obo, cursors, len(tree.Stmts)
	r.Clean, r.Do, r.Executes, r.Portals = clean, do, executes, portals
	r.DefinesCode = defines
	return r, nil
}

// definesCode reports whether n, a top-level statement, defines code or an
// alias through which a write to a gated table would pass where no statement
// shows it (ruling 29): CREATE [OR REPLACE] FUNCTION or PROCEDURE, CREATE [OR
// REPLACE] RULE, CREATE [OR REPLACE] [CONSTRAINT] TRIGGER, or CREATE [OR
// REPLACE] [TEMP] [RECURSIVE] VIEW whose query reads a gated table (an
// updatable view writes its table). The elements of CREATE SCHEMA are
// top-level statements too; no other statement can hold these, so nothing
// else is walked.
func definesCode(n *pg.Node, m matchFunc) bool {
	switch s := n.Node.(type) {
	case *pg.Node_CreateFunctionStmt, *pg.Node_RuleStmt, *pg.Node_CreateTrigStmt:
		return true
	case *pg.Node_ViewStmt:
		r := reader{m: m}
		r.visit(s.ViewStmt.Query, nil, false)
		return len(r.tables) > 0
	case *pg.Node_CreateSchemaStmt:
		return slices.ContainsFunc(s.CreateSchemaStmt.SchemaElts, func(e *pg.Node) bool { return definesCode(e, m) })
	}
	return false
}

// leavesClean reports whether n leaves a transaction clean: it is BEGIN,
// START TRANSACTION, SET, or SHOW. RESET is not one of them, nor is SET
// CONSTRAINTS, which is not a VariableSetStmt.
func leavesClean(n *pg.Node) bool {
	switch {
	case n.GetTransactionStmt() != nil:
		k := n.GetTransactionStmt().Kind
		return k == pg.TransactionStmtKind_TRANS_STMT_BEGIN || k == pg.TransactionStmtKind_TRANS_STMT_START
	case n.GetVariableSetStmt() != nil:
		switch n.GetVariableSetStmt().Kind {
		case pg.VariableSetKind_VAR_SET_VALUE, pg.VariableSetKind_VAR_SET_DEFAULT,
			pg.VariableSetKind_VAR_SET_CURRENT, pg.VariableSetKind_VAR_SET_MULTI:
			return true
		}
	case n.GetVariableShowStmt() != nil:
		return true
	}
	return false
}

// appendExecutes appends the names of the prepared statements n, a
// top-level statement, runs with SQL EXECUTE. By the grammar an EXECUTE is a
// statement of its own, the query of EXPLAIN or CREATE TABLE AS, or part of a
// BEGIN ATOMIC function body (listed too, though only defined there). No
// query or DML statement can hold one, so those are not walked: the walk
// would cost every statement.
func appendExecutes(names []string, n *pg.Node) []string {
	switch n.Node.(type) {
	case *pg.Node_SelectStmt, *pg.Node_InsertStmt, *pg.Node_UpdateStmt, *pg.Node_DeleteStmt, *pg.Node_MergeStmt:
		return names
	}
	walk(n, func(msg proto.Message) bool {
		if e, ok := msg.(*pg.ExecuteStmt); ok {
			names = append(names, e.Name)
		}
		return true
	})
	return names
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
// DISCARD ALL resets it too: it runs RESET ALL among its steps.
func onBehalfOf(n *pg.Node) (string, bool) {
	if d := n.GetDiscardStmt(); d != nil {
		return "", d.Target == pg.DiscardMode_DISCARD_ALL
	}
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

// pkEquality accepts `pk = value` (either order, value a non-null literal or
// a parameter), alone or ANDed with any other conditions: those can only
// narrow the write, so it still touches at most one row. Agents write such
// guards naturally (`where id = 245 and stage = 'open'`). An IN list of one
// item is the same equality, as Postgres reads it: ORMs write `id IN ($1)`.
// OR, longer IN lists, NOT IN, and anything else may touch more than one row.
func pkEquality(where *pg.Node, rel *pg.RangeVar, pk string) (Value, bool) {
	if b := where.GetBoolExpr(); b != nil && b.Boolop == pg.BoolExprType_AND_EXPR {
		for _, arg := range b.Args {
			if v, ok := pkEquality(arg, rel, pk); ok {
				return v, true
			}
		}
		return Value{}, false
	}
	e := where.GetAExpr()
	if e == nil || len(e.Name) != 1 || e.Name[0].GetString_().GetSval() != "=" {
		return Value{}, false
	}
	right := e.Rexpr
	switch e.Kind {
	case pg.A_Expr_Kind_AEXPR_OP:
	case pg.A_Expr_Kind_AEXPR_IN: // NOT IN is named <>
		items := e.Rexpr.GetList().GetItems()
		if len(items) != 1 {
			return Value{}, false
		}
		right = items[0]
	default:
		return Value{}, false
	}
	for _, side := range [][2]*pg.Node{{e.Lexpr, right}, {right, e.Lexpr}} {
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

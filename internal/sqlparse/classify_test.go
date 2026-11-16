package sqlparse

import (
	"fmt"
	"reflect"
	"testing"
)

// gated maps config keys to primary keys. "Contacts" is a second table,
// reached only by a quoted reference.
var gated = map[string]string{"contacts": "id", "Contacts": "id"}

func TestClassifyKinds(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		kind   Kind
		reason string
	}{
		// Not gated: reads, other tables, DDL.
		{"select", "select * from contacts where id = 1", Read, ""},
		{"insert other table", "insert into deals (id) values (1)", Pass, ""},
		{"update other table", "update deals set stage = 'won' where id = 1", Pass, ""},
		{"ddl", "create table x (id int)", Pass, ""},
		{"copy out", "copy contacts to stdout", Read, ""},
		{"several reads", "select 1; select 2", Pass, ""},
		{"empty", "", Pass, ""},

		// Single-row writes v0.1 judges.
		{"insert literals", "insert into contacts (name, email) values ('Kim', 'k@a.com')", Gated, ""},
		{"insert params", "insert into contacts (name, email) values ($1, $2)", Gated, ""},
		{"insert returning", "insert into contacts (name) values ($1) returning id", Gated, ""},
		{"insert default values", "insert into contacts default values", Gated, ""},
		{"schema qualified", "insert into public.contacts (name) values ('x')", Gated, ""},
		{"quoted identifier", `insert into "contacts" (name) values ('x')`, Gated, ""},
		{"update by pk", "update contacts set title = $1 where id = $2", Gated, ""},
		{"update by pk reversed", "update contacts set title = 'CTO' where 42 = id", Gated, ""},
		{"update qualified pk", "update contacts set title = 'CTO' where contacts.id = 42", Gated, ""},
		{"update aliased", "update contacts c set title = 'CTO' where c.id = 42", Gated, ""},
		{"delete by pk", "delete from contacts where id = $1", Gated, ""},
		{"trailing semicolon", "delete from contacts where id = 1;", Gated, ""},

		// Writes to a gated table in shapes v0.1 does not judge.
		{"multi row insert", "insert into contacts (name) values ('a'), ('b')", Measured, "multi_row"},
		{"insert select", "insert into contacts (name) select name from leads", Measured, "insert_select"},
		{"upsert", "insert into contacts (email) values ('a') on conflict (email) do nothing", Unsupported, "on_conflict"},
		{"range update", "update contacts set status = 'churned' where company_id = 3", Measured, "not_single_row"},
		{"update all rows", "update contacts set status = 'x'", Measured, "not_single_row"},
		// PK equality ANDed with more conditions still touches at most one row.
		{"pk plus extra predicate", "update contacts set title = 'x' where id = 1 and status = 'a'", Gated, ""},
		{"extra predicate first", "update contacts set title = 'x' where status = 'a' and id = $2", Gated, ""},
		{"several extra predicates", "delete from contacts where id = 7 and status = 'a' and title is not null", Gated, ""},
		{"pk or other", "update contacts set title = 'x' where id = 1 or status = 'a'", Measured, "not_single_row"},
		{"pk inside an or", "update contacts set title = 'x' where (id = 1 and status = 'a') or id = 2", Measured, "not_single_row"},
		{"pk in list", "update contacts set status = 'churned' where id in (1, 2, 3)", Measured, "not_single_row"},
		{"pk compared to column", "update contacts set title = 'x' where id = company_id", Measured, "not_single_row"},
		{"where current of", "update contacts set title = 'x' where current of c", Unsupported, "not_single_row"},
		{"delete all rows", "delete from contacts", Measured, "not_single_row"},
		{"update from", "update contacts set title = l.title from leads l where contacts.id = 1", Unsupported, "join"},
		{"delete using", "delete from contacts using leads where contacts.id = 1", Unsupported, "join"},
		{"multi assign", "update contacts set (title, status) = ('x', 'y') where id = 1", Unsupported, "multi_assign"},
		{"column indirection", "update contacts set tags[1] = 'x' where id = 1", Unsupported, "indirection"},
		{"pk equals null", "delete from contacts where id = null", Measured, "not_single_row"},
		{"subquery in values", "insert into contacts (company_id) values ((select id from companies limit 1))", Unsupported, "subquery"},
		{"subquery in where", "update contacts set title = 'x' where id = (select max(id) from contacts)", Measured, "subquery"},
		{"subquery in set", "update contacts set title = (select title from leads limit 1) where id = 1", Unsupported, "subquery"},
		{"with on gated write", "with x as (select 1) insert into contacts (name) values ('a')", Unsupported, "with"},
		{"copy in", "copy contacts from stdin", Unsupported, "copy"},
		{"truncate", "truncate contacts", Unsupported, "truncate"},
		{"truncate several", "truncate deals, contacts", Unsupported, "truncate"},
		{"merge", "merge into contacts c using leads l on c.email = l.email when matched then do nothing", Unsupported, "merge"},
		{"write inside cte", "with d as (delete from contacts where id = 1 returning *) select * from d", Unsupported, "nested_write"},
		{"write into other table from cte", "with d as (update contacts set title = 'x' where id = 1 returning *) insert into log select * from d", Unsupported, "nested_write"},
		{"explain analyze", "explain analyze delete from contacts where id = 1", Unsupported, "nested_write"},
		{"prepare", "prepare p as insert into contacts (name) values ($1)", Unsupported, "nested_write"},
		{"gated write among several", "select 1; delete from contacts where id = 1", Unsupported, "multi_statement"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Classify(tc.sql, gated)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != tc.kind || r.Reason != tc.reason {
				t.Errorf("got %v/%q, want %v/%q", r.Kind, r.Reason, tc.kind, tc.reason)
			}
			if (r.Kind == Gated) != (r.Write != nil) {
				t.Errorf("Write = %+v for kind %v; want a Write exactly when Gated", r.Write, r.Kind)
			}
			if (r.Kind == Measured) != (r.Measure != nil) {
				t.Errorf("Measure = %+v for kind %v; want a Measure exactly when Measured", r.Measure, r.Kind)
			}
			if (r.Kind == Read) != (r.Reads != nil) {
				t.Errorf("Reads = %v for kind %v; want Reads exactly when Read", r.Reads, r.Kind)
			}
		})
	}
}

// gated2 adds a second gated table, so a statement can read two.
var gated2 = map[string]string{"contacts": "id", "deals": "id"}

func TestClassifyReads(t *testing.T) {
	cases := []struct {
		sql    string
		kind   Kind
		reason string
		reads  []string
	}{
		{"select * from contacts", Read, "", []string{"contacts"}},
		{"table contacts", Read, "", []string{"contacts"}},
		{"select c.name from contacts c join deals d on d.contact_id = c.id", Read, "", []string{"contacts", "deals"}},
		{"select 1 where exists (select 1 from contacts)", Read, "", []string{"contacts"}},
		{"select * from (select * from deals) d", Read, "", []string{"deals"}},
		{"with x as (select * from deals) select * from x", Read, "", []string{"deals"}},
		{"copy contacts to stdout", Read, "", []string{"contacts"}},
		{"copy (select * from contacts) to stdout", Read, "", []string{"contacts"}},
		{"insert into log select * from contacts", Read, "", []string{"contacts"}},
		{"update log set n = c.n from contacts c where log.id = c.id", Read, "", []string{"contacts"}},
		{"delete from log using contacts c where log.id = c.id", Read, "", []string{"contacts"}},
		{"merge into log l using contacts c on l.id = c.id when matched then do nothing", Read, "", []string{"contacts"}},
		{"select 1; select * from contacts", Read, "", []string{"contacts"}},
		{"select * from deals; select * from contacts, deals", Read, "", []string{"contacts", "deals"}},
		{"select * from leads", Pass, "", nil},
		{"alter table contacts add column x int", Pass, "", nil},
		{"analyze contacts", Pass, "", nil},
		{"lock table contacts", Pass, "", nil},
		{"copy log from stdin", Pass, "", nil},

		// A WITH query of the same name hides the table in the statement it
		// is attached to. Without RECURSIVE its own query still sees the
		// table, and a schema-qualified name always names the table.
		{"with contacts as (select 1) select * from contacts", Pass, "", nil},
		{"with contacts as (select * from contacts) select * from contacts", Read, "", []string{"contacts"}},
		{"with recursive contacts (n) as (select 1 union all select n + 1 from contacts where n < 3) select * from contacts", Pass, "", nil},
		{"with contacts as (select 1) select * from public.contacts", Read, "", []string{"contacts"}},
		{"select * from contacts where exists (with contacts as (select 1) select * from contacts)", Read, "", []string{"contacts"}},
		{"with contacts as (select 1) update log set n = 1 from contacts", Pass, "", nil},

		// EXPLAIN and CREATE VIEW send none of the table's rows. Statements
		// that copy the rows elsewhere, or prepare the query, read the table.
		{"explain select * from contacts", Pass, "", nil},
		{"explain create table t as select * from contacts", Pass, "", nil},
		{"explain analyze select * from contacts c join deals d on d.contact_id = c.id", Pass, "", nil},
		{"explain (analyze) select * from contacts", Pass, "", nil},
		{"explain analyze delete from contacts where id = 1", Unsupported, "nested_write", nil},
		// EXPLAIN ANALYZE runs the statement: one that keeps the rows reads
		// as it would unexplained.
		{"explain analyze create table t as select * from contacts", Read, "", []string{"contacts"}},
		{"explain (analyze) select * into t from contacts", Read, "", []string{"contacts"}},
		{"explain (analyze true) create materialized view mv as select * from deals", Read, "", []string{"deals"}},
		{"explain (analyze, buffers) insert into log select * from contacts", Read, "", []string{"contacts"}},
		{"explain analyze verbose update log set n = c.n from contacts c where log.id = c.id", Read, "", []string{"contacts"}},
		{"explain (analyze on) delete from log using deals d where log.id = d.id", Read, "", []string{"deals"}},
		{"explain (analyze 1) merge into log l using contacts c on l.id = c.id when matched then do nothing", Read, "", []string{"contacts"}},
		{"explain analyze with x as (insert into log select * from deals returning 1) select * from x", Read, "", []string{"deals"}},
		{"explain (analyze false) create table t as select * from contacts", Pass, "", nil},
		{"explain (analyze off) insert into log select * from contacts", Pass, "", nil},
		{"explain (analyze 'OFF') insert into log select * from contacts", Pass, "", nil},
		{"explain (analyze 0) insert into log select * from contacts", Pass, "", nil},
		// Postgres reads repeated options in order; the last one wins.
		{"explain (analyze false, analyze) insert into log select * from contacts", Read, "", []string{"contacts"}},
		{"explain (analyze, analyze false) insert into log select * from contacts", Pass, "", nil},
		{"create view v as select * from contacts", Pass, "", nil},
		{"create or replace view v as select * from deals", Pass, "", nil},
		{"explain select * from deals; select * from contacts", Read, "", []string{"contacts"}},
		{"create table t as select * from contacts", Read, "", []string{"contacts"}},
		{"select * into t from contacts", Read, "", []string{"contacts"}},
		{"create materialized view mv as select * from deals", Read, "", []string{"deals"}},
		{"prepare p as select * from deals", Read, "", []string{"deals"}},

		// A statement that writes a gated table keeps its write kind.
		{"update contacts set title = l.t from deals l where contacts.id = 1", Unsupported, "join", nil},
		{"insert into contacts (name) select name from deals", Measured, "insert_select", nil},
		{"select * from deals; delete from contacts where id = 1", Unsupported, "multi_statement", nil},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			r, err := Classify(tc.sql, gated2)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != tc.kind || r.Reason != tc.reason || !reflect.DeepEqual(r.Reads, tc.reads) {
				t.Errorf("got %v/%q reads %v, want %v/%q reads %v", r.Kind, r.Reason, r.Reads, tc.kind, tc.reason, tc.reads)
			}
			if r.Cursor != nil {
				t.Errorf("Cursor = %+v, want nil", *r.Cursor)
			}
		})
	}
}

func TestClassifyCursors(t *testing.T) {
	cases := []struct {
		sql    string
		kind   Kind
		reads  []string
		cursor *Cursor
	}{
		{"declare c cursor for select * from contacts", Read, []string{"contacts"}, &Cursor{CursorDeclare, "c", false}},
		{`declare "My C" cursor with hold for select * from contacts`, Read, []string{"contacts"}, &Cursor{CursorDeclare, "My C", true}},
		{"declare c cursor for select 1", Pass, nil, &Cursor{CursorDeclare, "c", false}},
		// Options other than WITH HOLD set neighbouring bits.
		{"declare c binary insensitive scroll cursor without hold for select * from deals", Read, []string{"deals"}, &Cursor{CursorDeclare, "c", false}},
		{"fetch 100 from c", Pass, nil, &Cursor{CursorFetch, "c", false}},
		{"FETCH FORWARD 5 FROM C", Pass, nil, &Cursor{CursorFetch, "c", false}},
		{"move 10 in c", Pass, nil, nil},
		{"close c", Pass, nil, &Cursor{CursorClose, "c", false}},
		{`close "My C"`, Pass, nil, &Cursor{CursorClose, "My C", false}},
		{"close all", Pass, nil, &Cursor{CursorCloseAll, "", false}},
		// DISCARD ALL closes every cursor, held ones included (poolers send
		// it when a connection is handed back). Its other forms keep them.
		{"discard all", Pass, nil, &Cursor{CursorCloseAll, "", false}},
		{"DISCARD ALL", Pass, nil, &Cursor{CursorCloseAll, "", false}},
		{"discard plans", Pass, nil, nil},
		{"discard sequences", Pass, nil, nil},
		{"discard temp", Pass, nil, nil},
		{"discard temporary", Pass, nil, nil},
		{"fetch 1 from c; fetch 1 from d", Pass, nil, nil},
		{"declare c cursor for select * from contacts; fetch 1 from c", Read, []string{"contacts"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			r, err := Classify(tc.sql, gated2)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != tc.kind || r.Reason != "" || !reflect.DeepEqual(r.Reads, tc.reads) {
				t.Errorf("got %v/%q reads %v, want %v reads %v", r.Kind, r.Reason, r.Reads, tc.kind, tc.reads)
			}
			if !reflect.DeepEqual(r.Cursor, tc.cursor) {
				t.Errorf("Cursor = %s, want %s", cursorString(r.Cursor), cursorString(tc.cursor))
			}
		})
	}
}

func cursorString(c *Cursor) string {
	if c == nil {
		return "nil"
	}
	return fmt.Sprintf("%+v", *c)
}

func TestClassifyMeasured(t *testing.T) {
	cases := []struct {
		name, sql string
		reason    string
		want      Measure
	}{
		{"range update", "update contacts set status = 'churned' where company_id = 3", "not_single_row",
			Measure{Method: MethodCount, Relation: "contacts", CountSQL: "SELECT 1 FROM contacts WHERE company_id = 3"}},
		{"no where", "delete from contacts", "not_single_row",
			Measure{Method: MethodCount, Relation: "contacts", CountSQL: "SELECT 1 FROM contacts"}},
		{"only, schema, alias, unused $1", "update only public.contacts as c set title = $1 where c.company_id = $2", "not_single_row",
			Measure{Method: MethodCount, Relation: "public.contacts", CountSQL: "SELECT 1 FROM ONLY public.contacts c WHERE c.company_id = $2"}},
		// Relation is the table as the statement names it, not the config key.
		{"other schema", "delete from archive.contacts where company_id = 3", "not_single_row",
			Measure{Method: MethodCount, Relation: "archive.contacts", CountSQL: "SELECT 1 FROM archive.contacts WHERE company_id = 3"}},
		{"schema that needs quoting", `delete from "user".contacts where company_id = 3`, "not_single_row",
			Measure{Method: MethodCount, Relation: `"user".contacts`, CountSQL: `SELECT 1 FROM "user".contacts WHERE company_id = 3`}},
		{"mixed case, quoted", `update "Contacts" set status = 'x' where company_id = 3`, "not_single_row",
			Measure{Method: MethodCount, Relation: `"Contacts"`, CountSQL: `SELECT 1 FROM "Contacts" WHERE company_id = 3`}},
		{"mixed case schema and table, alias, only", `delete from only "Archive"."Contacts" c where c.company_id = 3`, "not_single_row",
			Measure{Method: MethodCount, Relation: `"Archive"."Contacts"`, CountSQL: `SELECT 1 FROM ONLY "Archive"."Contacts" c WHERE c.company_id = 3`}},
		{"other schema, estimated", "update archive.contacts set title = l.title from leads l where contacts.company_id = l.id", "join",
			Measure{Method: MethodEstimate, Relation: "archive.contacts"}},
		{"other schema, from the statement", "insert into archive.contacts (name) values ('a'), ('b')", "multi_row",
			Measure{Method: MethodSyntax, Relation: "archive.contacts", Rows: 2}},
		{"in list, trailing semicolon", "delete from contacts where id in (1, 2, 3);", "not_single_row",
			Measure{Method: MethodCount, Relation: "contacts", CountSQL: "SELECT 1 FROM contacts WHERE id IN (1, 2, 3)"}},
		{"listed function", "delete from contacts where lower(email) = $1", "not_single_row",
			Measure{Method: MethodCount, Relation: "contacts", CountSQL: "SELECT 1 FROM contacts WHERE lower(email) = $1"}},
		{"listed function qualified by pg_catalog", "delete from contacts where pg_catalog.lower(email) = $1", "not_single_row",
			Measure{Method: MethodCount, Relation: "contacts", CountSQL: "SELECT 1 FROM contacts WHERE pg_catalog.lower(email) = $1"}},
		{"unlisted function", "update contacts set status = 'x' where set_config('role', 'none', true) is not null", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"unlisted function inside a listed one", "delete from contacts where lower(set_config('a', 'b', true)) = 'x'", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"unlisted function after a listed sibling", "delete from contacts where lower(email) = 'a' and set_config('a', 'b', true) is not null", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		// Postgres resolves (x).f to f(x) for any f callable with one argument.
		{"(expr).function", "update contacts set status = 'x' where ((company_id + 7)::bigint).pg_try_advisory_lock and company_id = 9", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"(row).function", "delete from contacts where (contacts.*).to_json is not null", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		// The parser follows the PostgreSQL 17 grammar, where these are keyword
		// forms with no function call node. An older server reads the same
		// text as a call to an ordinary function of that name.
		{"json_value keyword form", "delete from contacts where json_value(notes, '$.a') = 'x'", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"json_exists keyword form", "delete from contacts where json_exists(notes, '$.a')", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"json_query keyword form", "delete from contacts where json_query(notes, '$.a') is not null", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"json_array keyword form", "delete from contacts where json_array(company_id, 1) is not null", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"json keyword form", "delete from contacts where json(notes) is not null", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"json_scalar keyword form", "delete from contacts where json_scalar(company_id) is not null", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"json_serialize keyword form", "delete from contacts where json_serialize(notes) = 'x'", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"merge_action keyword form", "delete from contacts where merge_action() = 'x'", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"array subscript", "delete from contacts where tags[1] = 'x'", "not_single_row",
			Measure{Method: MethodCount, Relation: "contacts", CountSQL: "SELECT 1 FROM contacts WHERE tags[1] = 'x'"}},
		{"array slice", "delete from contacts where (tags)[1:2] = '{x}'", "not_single_row",
			Measure{Method: MethodCount, Relation: "contacts", CountSQL: "SELECT 1 FROM contacts WHERE tags[1:2] = '{x}'"}},
		{"unlisted function in a subscript", "delete from contacts where tags[set_config('a', '1', true)::int] = 'x'", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"pg_catalog function not on the list", "delete from contacts where trim(name) = 'x'", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"schema-qualified function", "delete from contacts where public.f(id)", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"schema-qualified operator", "delete from contacts where company_id operator(public.=) 3", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		// JSON_ARRAY(query) holds a bare SelectStmt, not a SubLink.
		{"query outside a sublink", "delete from contacts where json_array(select 1) is not null", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"query reading another table outside a sublink", "delete from contacts where json_array(select email from secrets) is not null", "not_single_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"join", "update contacts set title = l.title from leads l where contacts.company_id = l.id", "join",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"subquery", "update contacts set title = 'x' where company_id in (select id from companies)", "subquery",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"with", "with x as (select 1) update contacts set status = 'x' where company_id = 3", "with",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"with writing another table", "with d as (delete from other returning id) update contacts set status = 'x' where company_id = 3", "with",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"multi row insert", "insert into contacts (name) values ('a'), ('b'), ('c')", "multi_row",
			Measure{Method: MethodSyntax, Relation: "contacts", Rows: 3}},
		{"multi row insert with a subquery", "insert into contacts (company_id) values ((select 1)), (2)", "multi_row",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
		{"insert select", "insert into contacts (name) select name from leads", "insert_select",
			Measure{Method: MethodEstimate, Relation: "contacts"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Classify(tc.sql, gated)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != Measured || r.Reason != tc.reason || r.Measure == nil || *r.Measure != tc.want {
				t.Errorf("got %v/%q %+v, want measured/%q %+v", r.Kind, r.Reason, r.Measure, tc.reason, tc.want)
			}
		})
	}
}

// Shapes that must not become Measured, with the reason they keep.
func TestClassifyStaysUnsupported(t *testing.T) {
	cases := map[string]struct{ sql, reason string }{
		"multi row upsert":                {"insert into contacts (email) values ('a'), ('b') on conflict (email) do nothing", "multi_row"},
		"multi assign over many rows":     {"update contacts set (title, status) = ('x', 'y') where company_id = 3", "multi_assign"},
		"indirection over many rows":      {"update contacts set tags[1] = 'x' where company_id = 3", "indirection"},
		"single row with subquery":        {"update contacts set title = (select title from leads limit 1) where id = 1", "subquery"},
		"single row with join":            {"update contacts set title = l.title from leads l where contacts.id = 1", "join"},
		"single row insert with a with":   {"with x as (select 1) insert into contacts (name) values ('a')", "with"},
		"cte also writes the gated table": {"with d as (delete from contacts where company_id = 9 returning id) update contacts set status = 'x' where company_id = 3", "with"},
		// One row, chosen by a cursor: as many rows as a pk equality.
		"where current of":        {"update contacts set title = 'x' where current of c", "not_single_row"},
		"delete where current of": {"delete from contacts where current of c", "not_single_row"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, err := Classify(tc.sql, gated)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != Unsupported || r.Reason != tc.reason || r.Measure != nil {
				t.Errorf("got %v/%q %+v, want unsupported/%q", r.Kind, r.Reason, r.Measure, tc.reason)
			}
		})
	}
}

func TestClassifyExtractsWrite(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want Write
	}{
		{
			"insert params",
			"insert into contacts (name, email) values ($1, $2)",
			Write{Op: "insert", Table: "contacts", Columns: []string{"name", "email"},
				Values: []Value{{Kind: Param, Param: 1}, {Kind: Param, Param: 2}}},
		},
		{
			"insert literal shapes",
			"insert into contacts (a, b, c, d, e, f, g) values ('x'::text, 7, -3, 1.5, true, null, default)",
			Write{Op: "insert", Table: "contacts", Columns: []string{"a", "b", "c", "d", "e", "f", "g"},
				Values: []Value{
					{Kind: Literal, Text: "x"},
					{Kind: Literal, Text: "7"},
					{Kind: Literal, Text: "-3"},
					{Kind: Literal, Text: "1.5"},
					{Kind: Literal, Text: "true"},
					{Kind: Null},
					{Kind: Default},
				}},
		},
		{
			"insert expressions",
			"insert into contacts (email, created_at) values (lower($1), now())",
			Write{Op: "insert", Table: "contacts", Columns: []string{"email", "created_at"},
				Values: []Value{{Kind: Expr, Text: "lower($1)"}, {Kind: Expr, Text: "now()"}}},
		},
		{
			"insert pk given",
			"insert into contacts (id, name) values (9, 'x')",
			Write{Op: "insert", Table: "contacts", Columns: []string{"id", "name"},
				Values: []Value{{Kind: Literal, Text: "9"}, {Kind: Literal, Text: "x"}},
				PK:     &Value{Kind: Literal, Text: "9"}},
		},
		{
			"insert without column list",
			"insert into contacts values (1, 'x')",
			Write{Op: "insert", Table: "contacts",
				Values: []Value{{Kind: Literal, Text: "1"}, {Kind: Literal, Text: "x"}}},
		},
		{
			"update",
			"update contacts set title = 'CTO', status = $1 where id = $2",
			Write{Op: "update", Table: "contacts", Columns: []string{"title", "status"},
				Values: []Value{{Kind: Literal, Text: "CTO"}, {Kind: Param, Param: 1}},
				PK:     &Value{Kind: Param, Param: 2}},
		},
		{
			"update with a guard condition",
			"update contacts set status = 'churned' where status = 'active' and contacts.id = 245",
			Write{Op: "update", Table: "contacts", Columns: []string{"status"},
				Values: []Value{{Kind: Literal, Text: "churned"}}, PK: &Value{Kind: Literal, Text: "245"}},
		},
		{
			"delete",
			"delete from contacts where id = 42",
			Write{Op: "delete", Table: "contacts", PK: &Value{Kind: Literal, Text: "42"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Classify(tc.sql, gated)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != Gated {
				t.Fatalf("kind = %v/%q, want gated", r.Kind, r.Reason)
			}
			if !reflect.DeepEqual(*r.Write, tc.want) {
				t.Errorf("\n got %+v\nwant %+v", *r.Write, tc.want)
			}
		})
	}
}

func TestClassifyTableMatching(t *testing.T) {
	qualified := map[string]string{"crm.contacts": "id"}
	cases := []struct {
		sql  string
		kind Kind
	}{
		{"delete from crm.contacts where id = 1", Gated},
		// search_path could resolve an unqualified name to crm.contacts.
		{"delete from contacts where id = 1", Gated},
		{"delete from sales.contacts where id = 1", Pass},
	}
	for _, tc := range cases {
		r, err := Classify(tc.sql, qualified)
		if err != nil {
			t.Fatal(err)
		}
		if r.Kind != tc.kind {
			t.Errorf("%s: kind = %v, want %v", tc.sql, r.Kind, tc.kind)
		}
		if r.Kind == Gated && r.Write.Table != "crm.contacts" {
			t.Errorf("%s: Write.Table = %q, want the config key crm.contacts", tc.sql, r.Write.Table)
		}
	}
}

func TestClassifyInvalidSQL(t *testing.T) {
	if _, err := Classify("insert into contacts (", gated); err == nil {
		t.Fatal("want a parse error")
	}
}

func TestClassifyReportsTarget(t *testing.T) {
	cases := []struct {
		sql, op string
	}{
		{"insert into contacts (name) values ('a')", "insert"},
		{"update contacts set title = 'x' where id = 1", "update"},
		{"insert into contacts (name) values ('a'), ('b')", "insert"},
		{"update contacts set status = 'x'", "update"},
		{"with d as (delete from contacts where id = 1 returning *) select * from d", "delete"},
		{"copy contacts from stdin", "copy"},
		{"truncate deals, contacts", "truncate"},
		{"merge into contacts c using leads l on c.email = l.email when matched then do nothing", "merge"},
		{"select 1; delete from contacts where id = 1", "delete"},
	}
	for _, tc := range cases {
		r, err := Classify(tc.sql, gated)
		if err != nil {
			t.Fatal(err)
		}
		if r.Table != "contacts" || r.Op != tc.op {
			t.Errorf("%s: target = %q/%q, want contacts/%s", tc.sql, r.Table, r.Op, tc.op)
		}
	}
}

func TestClassifyOnBehalfOf(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		sql  string
		want *string
	}{
		{"set stet.on_behalf_of = 'user:42'", str("user:42")},
		{"SET stet.on_behalf_of TO 'acct-7'", str("acct-7")},
		{"set local stet.on_behalf_of = 'u1'", str("u1")},
		{"reset stet.on_behalf_of", str("")},
		{"set stet.on_behalf_of to default", str("")},
		{"reset all", str("")},
		{"set application_name = 'x'", nil},
		{"select 1", nil},
	}
	for _, tc := range cases {
		r, err := Classify(tc.sql, gated)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case tc.want == nil && r.OnBehalfOf != nil:
			t.Errorf("%s: OnBehalfOf = %q, want unset", tc.sql, *r.OnBehalfOf)
		case tc.want != nil && (r.OnBehalfOf == nil || *r.OnBehalfOf != *tc.want):
			t.Errorf("%s: OnBehalfOf = %v, want %q", tc.sql, r.OnBehalfOf, *tc.want)
		}
	}
}

func TestFingerprintIgnoresConstants(t *testing.T) {
	a := Fingerprint("update contacts set title = 'CTO' where id = 1")
	b := Fingerprint("update contacts set title = 'CEO' where id = 2")
	c := Fingerprint("delete from contacts where id = 1")
	if a == "" || a != b {
		t.Errorf("fingerprints %q and %q should match", a, b)
	}
	if a == c {
		t.Error("different statements share a fingerprint")
	}
}

package sqlparse

import (
	"reflect"
	"testing"
)

// gated maps config keys to primary keys, and records reads. "Contacts" is
// a second table, reached only by a quoted reference.
var gated = Tables{PK: map[string]string{"contacts": "id", "Contacts": "id"}, Reads: true}

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
		// SQL PREPARE of a gated write: its EXECUTEs run what no Begin sees,
		// so it has a reason of its own, which enforce always refuses (ruling
		// 30), among other statements too.
		{"prepare", "prepare p as insert into contacts (name) values ($1)", Unsupported, "prepare"},
		{"prepare among several", "select 1; prepare p as delete from contacts where id = $1", Unsupported, "prepare"},
		{"prepare after a write", "delete from contacts where id = 1; prepare p as delete from contacts", Unsupported, "prepare"},
		{"prepare of another table", "prepare p as insert into deals (id) values ($1)", Pass, ""},
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
var gated2 = Tables{PK: map[string]string{"contacts": "id", "deals": "id"}, Reads: true}

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
			if r.Cursors != nil {
				t.Errorf("Cursors = %+v, want nil", r.Cursors)
			}
		})
	}
}

// With no table recording reads, statements are not examined for reads: a
// statement that reads a gated table is Pass, as before reads were counted.
// Writes and cursor statements are classified as usual.
func TestClassifySkipsReadsWhenNoTableRecordsThem(t *testing.T) {
	off := Tables{PK: gated2.PK}
	cases := []struct {
		sql     string
		kind    Kind
		cursors []Cursor
	}{
		{"select * from contacts", Pass, nil},
		{"select c.name from contacts c join deals d on d.contact_id = c.id", Pass, nil},
		{"copy contacts to stdout", Pass, nil},
		{"insert into log select * from contacts", Pass, nil},
		{"select 1; select * from deals", Pass, nil},
		{"declare c cursor for select * from contacts", Pass, []Cursor{{CursorDeclare, "c", false}}},
		{"fetch 10 from c", Pass, []Cursor{{CursorFetch, "c", false}}},
		{"close c", Pass, []Cursor{{CursorClose, "c", false}}},
		{"delete from contacts where id = 1", Gated, nil},
		{"update contacts set status = 'x' where company_id = 3", Measured, nil},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			r, err := Classify(tc.sql, off)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != tc.kind || r.Reads != nil {
				t.Errorf("got %v reads %v, want %v and no reads", r.Kind, r.Reads, tc.kind)
			}
			if !reflect.DeepEqual(r.Cursors, tc.cursors) {
				t.Errorf("Cursors = %+v, want %+v", r.Cursors, tc.cursors)
			}
		})
	}
}

func TestClassifyCursors(t *testing.T) {
	cases := []struct {
		sql     string
		kind    Kind
		reads   []string
		cursors []Cursor
	}{
		{"declare c cursor for select * from contacts", Read, []string{"contacts"}, []Cursor{{CursorDeclare, "c", false}}},
		{`declare "My C" cursor with hold for select * from contacts`, Read, []string{"contacts"}, []Cursor{{CursorDeclare, "My C", true}}},
		{"declare c cursor for select 1", Pass, nil, []Cursor{{CursorDeclare, "c", false}}},
		// Options other than WITH HOLD set neighbouring bits.
		{"declare c binary insensitive scroll cursor without hold for select * from deals", Read, []string{"deals"}, []Cursor{{CursorDeclare, "c", false}}},
		{"fetch 100 from c", Pass, nil, []Cursor{{CursorFetch, "c", false}}},
		{"FETCH FORWARD 5 FROM C", Pass, nil, []Cursor{{CursorFetch, "c", false}}},
		{"move 10 in c", Pass, nil, nil},
		{"close c", Pass, nil, []Cursor{{CursorClose, "c", false}}},
		{`close "My C"`, Pass, nil, []Cursor{{CursorClose, "My C", false}}},
		{"close all", Pass, nil, []Cursor{{CursorCloseAll, "", false}}},
		// DISCARD ALL closes every cursor, held ones included (poolers send
		// it when a connection is handed back). Its other forms keep them.
		{"discard all", Pass, nil, []Cursor{{CursorCloseAll, "", false}}},
		{"DISCARD ALL", Pass, nil, []Cursor{{CursorCloseAll, "", false}}},
		{"discard plans", Pass, nil, nil},
		{"discard sequences", Pass, nil, nil},
		{"discard temp", Pass, nil, nil},
		{"discard temporary", Pass, nil, nil},
		{"fetch 1 from c; fetch 1 from d", Pass, nil, []Cursor{{CursorFetch, "c", false}, {CursorFetch, "d", false}}},
		{"declare c cursor for select * from contacts; fetch 1 from c", Read, []string{"contacts"},
			[]Cursor{{CursorDeclare, "c", false}, {CursorFetch, "c", false}}},
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
			if !reflect.DeepEqual(r.Cursors, tc.cursors) {
				t.Errorf("Cursors = %+v, want %+v", r.Cursors, tc.cursors)
			}
		})
	}
}

// Every cursor statement of a message is listed in order, also among other
// statements, and also in a message that writes a gated table.
func TestClassifyCursorsAmongSeveralStatements(t *testing.T) {
	cases := []struct {
		sql        string
		kind       Kind
		statements int
		cursors    []Cursor
	}{
		{"begin; declare c cursor for select * from contacts", Read, 2, []Cursor{{CursorDeclare, "c", false}}},
		{"fetch 7 from c; select 1", Pass, 2, []Cursor{{CursorFetch, "c", false}}},
		{"close c; declare c cursor with hold for select * from deals; fetch 2 from c", Read, 3,
			[]Cursor{{CursorClose, "c", false}, {CursorDeclare, "c", true}, {CursorFetch, "c", false}}},
		{"discard all; select 1", Pass, 2, []Cursor{{CursorCloseAll, "", false}}},
		{"move 1 in c; close all", Pass, 2, []Cursor{{CursorCloseAll, "", false}}},
		{"fetch 10 from c; delete from contacts where id = 1", Unsupported, 2, []Cursor{{CursorFetch, "c", false}}},
		{"select 1; select 2", Pass, 2, nil},
		{"select * from contacts", Read, 1, nil},
		{"", Pass, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			r, err := Classify(tc.sql, gated2)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != tc.kind || r.Statements != tc.statements {
				t.Errorf("got %v with %d statements, want %v with %d", r.Kind, r.Statements, tc.kind, tc.statements)
			}
			if !reflect.DeepEqual(r.Cursors, tc.cursors) {
				t.Errorf("Cursors = %+v, want %+v", r.Cursors, tc.cursors)
			}
		})
	}
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
			// Qualified is checked by TestClassifyQualifiedNames.
			var got Measure
			if r.Measure != nil {
				got = *r.Measure
				got.Qualified = nil
			}
			if r.Kind != Measured || r.Reason != tc.reason || r.Measure == nil || !reflect.DeepEqual(got, tc.want) {
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
	qualified := Tables{PK: map[string]string{"crm.contacts": "id"}}
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
		// DISCARD ALL runs RESET ALL among its steps; poolers send it.
		{"discard all", str("")},
		{"discard plans", nil},
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

// A transaction stays clean while only BEGIN, START TRANSACTION, SET, and
// SHOW have run in it. RESET is not on that list.
func TestClassifyClean(t *testing.T) {
	cases := []struct {
		sql   string
		clean bool
	}{
		{"", true},
		{"-- ping", true},
		{"begin", true},
		{"BEGIN ISOLATION LEVEL SERIALIZABLE", true},
		{"start transaction read only", true},
		{"set search_path = public", true},
		{"set local a.b = '1'", true},
		{"set transaction isolation level serializable", true},
		{"show a.b", true},
		{"begin; set local statement_timeout = 0", true},

		{"reset a.b", false},
		{"reset all", false},
		{"select 1", false},
		{"commit", false},
		{"rollback", false},
		{"savepoint s", false},
		{"discard all", false},
		{"begin; select 1", false},
		{"update contacts set title = 'x' where id = 1", false},
		{"do $$ begin end $$", false},
	}
	for _, tc := range cases {
		r, err := Classify(tc.sql, gated)
		if err != nil {
			t.Fatal(err)
		}
		if r.Clean != tc.clean {
			t.Errorf("%q: Clean = %v, want %v", tc.sql, r.Clean, tc.clean)
		}
	}
}

// Sets marks SQL holding a SET or RESET among its statements: what the
// server reports of the session's settings may change. A function that sets
// a setting (set_config) is not one, nor is an UPDATE's SET clause.
func TestClassifySets(t *testing.T) {
	cases := []struct {
		sql  string
		sets bool
	}{
		{"set timezone = 'UTC'", true},
		{"set local datestyle to 'SQL, DMY'", true},
		{"set time zone 'UTC'", true},
		{"reset timezone", true},
		{"reset all", true},
		{"begin; set local a.b = '1'", true},

		{"show timezone", false},
		{"begin", false},
		{"select set_config('timezone', 'UTC', false)", false},
		{"update contacts set title = 'x' where id = 1", false},
	}
	for _, tc := range cases {
		r, err := Classify(tc.sql, gated)
		if err != nil {
			t.Fatal(err)
		}
		if r.Sets != tc.sets {
			t.Errorf("%q: Sets = %v, want %v", tc.sql, r.Sets, tc.sets)
		}
	}
}

// A DO block's writes cannot be seen, so the statement is Pass; Do says one
// is there.
func TestClassifyDo(t *testing.T) {
	cases := []struct {
		sql  string
		kind Kind
		do   bool
	}{
		{"do $$ begin update contacts set title = 'x'; end $$", Pass, true},
		{"select 1; do $$ begin end $$", Pass, true},
		{"select 'do'", Pass, false},
	}
	for _, tc := range cases {
		r, err := Classify(tc.sql, gated)
		if err != nil {
			t.Fatal(err)
		}
		if r.Kind != tc.kind || r.Do != tc.do {
			t.Errorf("%q: got %v Do %v, want %v Do %v", tc.sql, r.Kind, r.Do, tc.kind, tc.do)
		}
	}
}

// x IN (y) is x = y: a one-value IN on the primary key touches one row and
// is judged on its values. Every other IN, and other shapes that happen to
// pin one row, stay multi-row.
func TestClassifySingleValueIn(t *testing.T) {
	cases := []struct {
		sql    string
		kind   Kind
		reason string
		pk     *Value
	}{
		{"update contacts set title = 'x' where id in (5)", Gated, "", &Value{Kind: Literal, Text: "5"}},
		{"delete from contacts where id in ($1)", Gated, "", &Value{Kind: Param, Param: 1}},
		{"update contacts set title = 'x' where contacts.id in (5) and status = 'a'", Gated, "", &Value{Kind: Literal, Text: "5"}},
		{"update contacts set title = 'x' where id in ('5'::int)", Gated, "", &Value{Kind: Literal, Text: "5"}},

		{"update contacts set title = 'x' where id in (5, 6)", Measured, "not_single_row", nil},
		{"update contacts set title = 'x' where id not in (5)", Measured, "not_single_row", nil},
		{"update contacts set title = 'x' where id in (null)", Measured, "not_single_row", nil},
		{"update contacts set title = 'x' where id = any($1)", Measured, "not_single_row", nil},
		{"update contacts set title = 'x' where id between 5 and 5", Measured, "not_single_row", nil},
		{"update contacts set title = 'x' where id = 5 or false", Measured, "not_single_row", nil},
		// Neither a query nor another column pins the key.
		{"update contacts set title = 'x' where id in (select 5)", Measured, "subquery", nil},
		{"update contacts set title = 'x' where company_id in (5)", Measured, "not_single_row", nil},
	}
	for _, tc := range cases {
		t.Run(tc.sql, func(t *testing.T) {
			r, err := Classify(tc.sql, gated)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != tc.kind || r.Reason != tc.reason {
				t.Fatalf("got %v/%q, want %v/%q", r.Kind, r.Reason, tc.kind, tc.reason)
			}
			if tc.kind == Gated {
				if !reflect.DeepEqual(r.Write.PK, tc.pk) {
					t.Errorf("Write.PK = %+v, want %+v", r.Write.PK, tc.pk)
				}
				return
			}
			if r.Write != nil || r.Measure == nil {
				t.Errorf("Write = %+v, Measure = %+v; want no Write and a Measure", r.Write, r.Measure)
			}
		})
	}
}

// Statements that run a prepared statement or read a portal by name: the
// classifier sees no table behind the name.
func TestClassifyRunsByName(t *testing.T) {
	cases := []struct {
		sql      string
		executes []string
		portals  []string
		cursors  []Cursor
	}{
		{"execute s1(1)", []string{"s1"}, nil, nil},
		{"explain analyze execute s1", []string{"s1"}, nil, nil},
		{"create table x as execute s1", []string{"s1"}, nil, nil},
		{`execute "S 1"`, []string{"S 1"}, nil, nil},
		{"execute b; explain execute a", []string{"b", "a"}, nil, nil},
		{"fetch all from p1", nil, []string{"p1"}, []Cursor{{CursorFetch, "p1", false}}},
		{"move all in p1", nil, []string{"p1"}, nil},
		{"move 1 in b; fetch 1 from a", nil, []string{"b", "a"}, []Cursor{{CursorFetch, "a", false}}},
		{"select 1", nil, nil, nil},
	}
	for _, tc := range cases {
		r, err := Classify(tc.sql, gated)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(r.Executes, tc.executes) || !reflect.DeepEqual(r.Portals, tc.portals) {
			t.Errorf("%q: Executes %q Portals %q, want %q and %q", tc.sql, r.Executes, r.Portals, tc.executes, tc.portals)
		}
		if !reflect.DeepEqual(r.Cursors, tc.cursors) {
			t.Errorf("%q: Cursors = %+v, want %+v", tc.sql, r.Cursors, tc.cursors)
		}
	}
}

// A statement that defines code, or a view over a gated table, makes a way
// to write that table that no later statement shows (ruling 29): a function
// or procedure, a rule, a trigger, or a view whose query reads a gated table.
// A view over other tables does not. CREATE SCHEMA's elements count, and so
// does any statement of several.
func TestClassifyDefinesCode(t *testing.T) {
	for sql, want := range map[string]bool{
		"create function f() returns void language sql as 'update contacts set title = ''x'''":       true,
		"create or replace function pg_temp.f() returns int language sql as 'select 1'":              true,
		"create function f() returns int begin atomic select 1; end":                                 true,
		"create procedure p() language sql as 'delete from contacts'":                                true,
		"create rule r as on insert to log do instead insert into contacts (name) values (new.name)": true,
		"create or replace rule r as on update to log do instead nothing":                            true,
		"create trigger t before insert on log for each row execute function f()":                    true,
		"create or replace trigger t before update on log for each row execute function f()":         true,
		"create constraint trigger t after insert on log for each row execute function f()":          true,
		"create temp view v as select * from contacts":                                               true,
		"create or replace temporary view v as select id from public.contacts where id > 3":          true,
		"create view v as select * from leads where id in (select id from contacts)":                 true,
		"create view v as select l.* from leads l join contacts c on c.id = l.id":                    true,
		"create schema s create view v as select * from contacts":                                    true,
		"select 1; create function f() returns int language sql as 'select 1'":                       true,

		"create view v as select * from leads":                                              false,
		"create view v as with contacts as (select 1) select * from contacts":               false,
		"create recursive view v (n) as select 1 union all select n + 1 from v where n < 3": false,
		"create materialized view m as select * from contacts":                              false,
		"create schema s create table x (i int)":                                            false,
		"create table x (i int)":                                                            false,
		"do $$ begin end $$":                                                                false,
		"select * from contacts":                                                            false,
		"update contacts set title = 'x' where id = 1":                                      false,
	} {
		r, err := Classify(sql, gated)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		if r.DefinesCode != want {
			t.Errorf("%q: DefinesCode = %v, want %v", sql, r.DefinesCode, want)
		}
	}
}

// COPY ... FROM STDIN makes the server read copy data from the client; sent
// through the extended protocol, it ignores the Sync after it. Any table, any
// statement of several. COPY TO, and COPY FROM a file or a program, read
// nothing from the client.
func TestClassifyCopyFromStdin(t *testing.T) {
	for sql, want := range map[string]bool{
		"copy contacts from stdin":                     true,
		"copy deals (id) from stdin with (format csv)": true,
		"select 1; copy deals from stdin":              true,
		"copy contacts to stdout":                      false,
		"copy (select 1) to stdout":                    false,
		"copy deals from '/tmp/x'":                     false,
		"copy deals from program 'cat /tmp/x'":         false,
		"select 1":                                     false,
	} {
		r, err := Classify(sql, gated)
		if err != nil {
			t.Fatalf("%q: %v", sql, err)
		}
		if r.CopyIn != want {
			t.Errorf("%q: CopyIn = %v, want %v", sql, r.CopyIn, want)
		}
	}
}

// Postgres reads t.f as column f of t or, when t has none, as the call f(t).
// The count runs the WHERE clause, so it lists such names for the gate to
// look up; no other method runs it.
func TestClassifyQualifiedNames(t *testing.T) {
	cases := []struct {
		sql       string
		method    Method
		qualified []string
	}{
		{"update contacts set status = 'x' where contacts.to_json is not null and public.contacts.company_id = 3 and status = 'a'",
			MethodCount, []string{"company_id", "to_json"}},
		{"update contacts c set status = 'x' where c.company_id = 3", MethodCount, []string{"company_id"}},
		{"update contacts set status = 'x' where company_id = 3", MethodCount, nil},
		{"update contacts c set status = 'x' where c.* is not null", MethodCount, nil},
		{"delete from contacts c where c.company_id = 3 or c.company_id = 4", MethodCount, []string{"company_id"}},
		{"update contacts set title = l.title from leads l where contacts.company_id = l.id", MethodEstimate, nil},
	}
	for _, tc := range cases {
		r, err := Classify(tc.sql, gated)
		if err != nil {
			t.Fatal(err)
		}
		if r.Kind != Measured {
			t.Errorf("%q: got %v/%q, want measured", tc.sql, r.Kind, r.Reason)
			continue
		}
		if r.Measure.Method != tc.method || !reflect.DeepEqual(r.Measure.Qualified, tc.qualified) {
			t.Errorf("%q: %s Qualified %q, want %s %q", tc.sql, r.Measure.Method, r.Measure.Qualified, tc.method, tc.qualified)
		}
	}
}

func TestPostgresMajorIsTheParsers(t *testing.T) {
	if got := int(treeVersion / 10000); got != PostgresMajor {
		t.Errorf("the parser reads PostgreSQL %d, PostgresMajor says %d", got, PostgresMajor)
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

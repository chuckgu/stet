package sqlparse

import (
	"reflect"
	"testing"
)

var gated = map[string]string{"contacts": "id"}

func TestClassifyKinds(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		kind   Kind
		reason string
	}{
		// Not gated: reads, other tables, DDL.
		{"select", "select * from contacts where id = 1", Pass, ""},
		{"insert other table", "insert into deals (id) values (1)", Pass, ""},
		{"update other table", "update deals set stage = 'won' where id = 1", Pass, ""},
		{"ddl", "create table x (id int)", Pass, ""},
		{"copy out", "copy contacts to stdout", Pass, ""},
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
		{"multi row insert", "insert into contacts (name) values ('a'), ('b')", Unsupported, "multi_row"},
		{"insert select", "insert into contacts (name) select name from leads", Unsupported, "insert_select"},
		{"upsert", "insert into contacts (email) values ('a') on conflict (email) do nothing", Unsupported, "on_conflict"},
		{"range update", "update contacts set status = 'churned' where company_id = 3", Unsupported, "not_single_row"},
		{"update all rows", "update contacts set status = 'x'", Unsupported, "not_single_row"},
		{"pk plus extra predicate", "update contacts set title = 'x' where id = 1 and status = 'a'", Unsupported, "not_single_row"},
		{"pk compared to column", "update contacts set title = 'x' where id = company_id", Unsupported, "not_single_row"},
		{"where current of", "update contacts set title = 'x' where current of c", Unsupported, "not_single_row"},
		{"delete all rows", "delete from contacts", Unsupported, "not_single_row"},
		{"update from", "update contacts set title = l.title from leads l where contacts.id = 1", Unsupported, "join"},
		{"delete using", "delete from contacts using leads where contacts.id = 1", Unsupported, "join"},
		{"multi assign", "update contacts set (title, status) = ('x', 'y') where id = 1", Unsupported, "multi_assign"},
		{"column indirection", "update contacts set tags[1] = 'x' where id = 1", Unsupported, "indirection"},
		{"pk equals null", "delete from contacts where id = null", Unsupported, "not_single_row"},
		{"subquery in values", "insert into contacts (company_id) values ((select id from companies limit 1))", Unsupported, "subquery"},
		{"subquery in where", "update contacts set title = 'x' where id = (select max(id) from contacts)", Unsupported, "subquery"},
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

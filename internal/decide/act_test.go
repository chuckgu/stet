package decide

import (
	"strings"
	"testing"

	"github.com/stetdb/stet/internal/config"
)

// facts is a contacts table with every number a message can use.
func facts(op string) Facts {
	n := func(v int64) *int64 { return &v }
	share := 0.8
	return Facts{
		Table: "contacts", PK: "id", Op: op, Unsupported: "reject",
		Rules:      []string{"Do not churn with an open deal.", "Do not restore a stale title."},
		Impact:     &Impact{Method: "count", Rows: n(48211), TableRows: n(60264), Share: &share},
		Limit:      &config.Impact{MaxRows: 1000, MaxShare: 0.5},
		Cumulative: &Cumulative{Used: n(480), Planned: n(21), Limit: 500, WindowS: 600},
	}
}

func TestAct(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	const held, refused = "write held by stet: ", "write refused by stet: "
	const approve = "ask a human to approve held_write H1"
	cases := []struct {
		name    string
		verdict string
		reasons []string
		facts   Facts
		message string
		hint    string
	}{
		{"rows", Hold, []string{"impact:rows"}, facts("delete"),
			held + "48211 rows would be deleted, over the limit of 1000",
			"narrow the WHERE clause, or " + approve},
		{"rows, capped", Hold, []string{"impact:rows"},
			with(facts("update"), func(f *Facts) { f.Impact = &Impact{Method: "count", Rows: n(1001), Capped: true, Cap: 1001} }),
			held + "more than 1000 rows would be updated, over the limit of 1000",
			"narrow the WHERE clause, or " + approve},
		{"rows, insert", Hold, []string{"impact:rows"},
			with(facts("insert"), func(f *Facts) { f.Impact = &Impact{Method: "syntax", Rows: n(1500)} }),
			held + "1500 rows would be inserted, over the limit of 1000",
			"insert fewer rows in one statement, or " + approve},
		{"share", Hold, []string{"impact:share"}, facts("delete"),
			held + "80% of contacts would be deleted, over the limit of 50%",
			"narrow the WHERE clause, or " + approve},
		{"share, three digits", Hold, []string{"impact:share"},
			with(facts("update"), func(f *Facts) {
				s := 0.12345
				f.Impact.Share, f.Limit = &s, &config.Impact{MaxRows: 1000, MaxShare: 0.1}
			}),
			held + "12.3% of contacts would be updated, over the limit of 10%",
			"narrow the WHERE clause, or " + approve},
		{"cumulative", Hold, []string{"impact:cumulative"}, facts("update"),
			held + "this role has touched 480 rows of contacts in the last 10m0s, and 21 more would pass the limit of 500",
			"wait until earlier writes leave the window, or " + approve},
		{"cumulative unknown", Hold, []string{"impact:cumulative_unknown"}, facts("insert"),
			held + "the rows this role has touched in contacts could not be read, so its cumulative limit cannot be checked",
			"retry shortly, or " + approve},
		{"uncertain", Escalate, []string{"impact:uncertain"},
			with(facts("update"), func(f *Facts) { f.Impact = &Impact{Method: "estimate", Rows: n(50)} }),
			held + "the rows it would touch could only be estimated (about 50)",
			"use a WHERE clause of plain comparisons on the table's own columns, or " + approve},
		{"state error", Hold, []string{"state_error"}, facts("update"),
			held + "the state needed to judge it could not be read in time",
			"retry shortly, or " + approve},
		{"model error", Hold, []string{"model_error"}, facts("update"),
			held + "the judgment model did not answer",
			"retry shortly, or " + approve},
		{"no model", Hold, []string{"no_model"}, facts("update"),
			held + "it needs a judgment and no model is configured",
			approve},
		{"low confidence", Escalate, []string{"low_confidence:conflict", "low_confidence:policy_1"}, facts("update"),
			held + "the judgment was not confident enough to let it through",
			approve},
		{"duplicate", Hold, []string{"duplicate:4812"}, facts("insert"),
			held + "possible duplicate of contacts.id=4812",
			"update contacts.id=4812 instead of inserting a new row, or " + approve},
		{"conflict", Hold, []string{"conflict"}, facts("update"),
			held + "it would undo a value another write set recently",
			"read the row again and keep the recent value, or " + approve},
		{"policy", Hold, []string{"policy:2"}, facts("update"),
			held + `it breaks policy rule 2 ("Do not restore a stale title.")`,
			"change the write to follow the rule, or " + approve},
		{"every broken rule", Hold, []string{"policy:1", "policy:2"}, facts("update"),
			held + `it breaks policy rule 1 ("Do not churn with an open deal."), rule 2 ("Do not restore a stale title.")`,
			"change the write to follow the rule, or " + approve},
		{"rules only", Hold, []string{"policy:1", "rejudged"}, facts("update"),
			held + `it breaks policy rule 1 ("Do not churn with an open deal.")`,
			"change the write to follow the rule, or " + approve},
		{"concurrent change", Hold, []string{"concurrent_change", "rejudged"}, facts("update"),
			held + "the row changed while the write was being judged",
			"read the row again and retry"},
		{"in transaction", Unsupported, []string{"in_transaction"}, facts("update"),
			refused + "it came after another statement in the same transaction and cannot be judged there",
			"ROLLBACK, then send this write as its own statement or as the first statement after BEGIN"},
		{"no login role", Unsupported, []string{"no_login_role"}, facts("update"),
			refused + "the session had not finished logging in, so its login role is not known",
			"wait until the connection is ready before sending statements"},
		// A dirty transaction or an unknown login role is refused whatever
		// the table's unsupported setting, and also when a shape reason
		// came first: changing the shape would not let the write through.
		{"shape in a dirty transaction, table passes", Unsupported, []string{"on_conflict"},
			Facts{Unsupported: "pass", InTransaction: true},
			refused + "it came after another statement in the same transaction and cannot be judged there",
			"ROLLBACK, then send this write as its own statement or as the first statement after BEGIN"},
		{"shape without a login role, table passes", Unsupported, []string{"on_conflict"},
			Facts{Unsupported: "pass", NoLoginRole: true},
			refused + "the session had not finished logging in, so its login role is not known",
			"wait until the connection is ready before sending statements"},
		{"shape in a dirty transaction, table rejects", Unsupported, []string{"multi_row"},
			with(facts("update"), func(f *Facts) { f.InTransaction = true }),
			refused + "it came after another statement in the same transaction and cannot be judged there",
			"ROLLBACK, then send this write as its own statement or as the first statement after BEGIN"},
		{"dirty transaction before login role", Unsupported, []string{"on_conflict"},
			Facts{Unsupported: "pass", InTransaction: true, NoLoginRole: true},
			refused + "it came after another statement in the same transaction and cannot be judged there",
			"ROLLBACK, then send this write as its own statement or as the first statement after BEGIN"},
		{"multi statement", Unsupported, []string{"multi_statement"}, facts("update"),
			refused + "several statements were sent in one query",
			"send each statement as its own query"},
		{"on conflict", Unsupported, []string{"on_conflict"}, facts("insert"),
			refused + "INSERT ... ON CONFLICT cannot be judged",
			"check whether the row exists, then send a plain INSERT or UPDATE"},
		{"copy", Unsupported, []string{"copy"}, facts("insert"),
			refused + "COPY into contacts cannot be judged",
			"use INSERT statements"},
		{"truncate", Unsupported, []string{"truncate"}, facts("delete"),
			refused + "TRUNCATE of contacts cannot be judged",
			"delete rows by primary key"},
		{"merge", Unsupported, []string{"merge"}, facts("update"),
			refused + "MERGE into contacts cannot be judged",
			"send separate INSERT and UPDATE statements"},
		{"nested write", Unsupported, []string{"nested_write"}, facts("update"),
			refused + "a write to contacts inside another statement cannot be judged",
			"send it as a plain INSERT, UPDATE or DELETE"},
		{"another shape", Unsupported, []string{"returning_star"}, facts("update"),
			refused + "this statement shape cannot be judged (returning_star)",
			"send a plain single-row INSERT, UPDATE or DELETE"},
	}
	for _, r := range []string{"not_single_row", "join", "subquery", "with", "multi_row", "insert_select"} {
		cases = append(cases, struct {
			name    string
			verdict string
			reasons []string
			facts   Facts
			message string
			hint    string
		}{r, Unsupported, []string{r}, facts("update"),
			refused + "it may touch more than one row of contacts and its rows could not be counted",
			"write one row at a time, selected by primary key"})
	}
	for _, r := range []string{"indirection", "multi_assign"} {
		cases = append(cases, struct {
			name    string
			verdict string
			reasons []string
			facts   Facts
			message string
			hint    string
		}{r, Unsupported, []string{r}, facts("update"),
			refused + "this form of SET cannot be judged",
			"set each column to a whole value, one column per assignment"})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := Act(tc.verdict, tc.reasons, tc.facts)
			if a.Forward {
				t.Fatalf("Act = forward, want a refusal")
			}
			wantCode, wantHeld := CodeUnsupported, false
			if tc.verdict == Hold || tc.verdict == Escalate {
				wantCode, wantHeld = CodeHeld, true
			}
			if a.Code != wantCode || a.Held != wantHeld {
				t.Errorf("code %q held %v, want %q %v", a.Code, a.Held, wantCode, wantHeld)
			}
			if a.Message != tc.message {
				t.Errorf("message\n got %q\nwant %q", a.Message, tc.message)
			}
			if got := a.Hint("H1"); got != tc.hint {
				t.Errorf("Hint(H1)\n got %q\nwant %q", got, tc.hint)
			}
			want := strings.Replace(tc.hint, "held_write H1", "this write", 1)
			if got := a.Hint(""); got != want {
				t.Errorf("Hint(\"\")\n got %q\nwant %q", got, want)
			}
		})
	}
}

func TestActForwards(t *testing.T) {
	pass := facts("update")
	pass.Unsupported = "pass"
	cases := []struct {
		name    string
		verdict string
		reasons []string
		facts   Facts
		forward bool
	}{
		{"commit", Commit, nil, facts("update"), true},
		{"commit after a rejudgment", Commit, []string{"rejudged"}, facts("update"), true},
		{"commit after a model error", Commit, []string{"model_error"}, facts("update"), true},
		{"commit after a measured write ran over", Commit, []string{"impact:exceeded_after"}, facts("update"), true},
		{"unsupported, table rejects", Unsupported, []string{"multi_row"}, facts("update"), false},
		{"unsupported, table setting unset", Unsupported, []string{"multi_row"}, Facts{Table: "contacts"}, false},
		{"unsupported, table passes", Unsupported, []string{"multi_row"}, pass, true},
		{"measuring failed, table passes", Unsupported, []string{"not_single_row"}, pass, true},
		{"in transaction, table passes", Unsupported, []string{"in_transaction"}, pass, false},
		{"no login role, table passes", Unsupported, []string{"no_login_role"}, pass, false},
		{"shape, table passes", Unsupported, []string{"on_conflict"}, Facts{Unsupported: "pass"}, true},
		{"shape in a dirty transaction, table passes", Unsupported, []string{"on_conflict"},
			Facts{Unsupported: "pass", InTransaction: true}, false},
		{"shape without a login role, table passes", Unsupported, []string{"on_conflict"},
			Facts{Unsupported: "pass", NoLoginRole: true}, false},
		{"hold, table passes", Hold, []string{"impact:rows"}, pass, false},
		{"escalate, table passes", Escalate, []string{"impact:uncertain"}, pass, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := Act(tc.verdict, tc.reasons, tc.facts)
			if a.Forward != tc.forward {
				t.Fatalf("forward = %v, want %v (%+v)", a.Forward, tc.forward, a)
			}
			if a.Forward && (a != Action{Forward: true}) {
				t.Errorf("forwarded action = %+v, want nothing but Forward", a)
			}
			if a.Forward && a.Hint("H1") != "" {
				t.Errorf("forwarded action has hint %q", a.Hint("H1"))
			}
			if !a.Forward && (a.Message == "" || a.Hint("") == "") {
				t.Errorf("refusal %+v has an empty message or hint", a)
			}
		})
	}
}

// A refusal never echoes another row: a duplicate candidate is named by its
// key, and the facts carry nothing else of it.
func TestActNamesOnlyTheKeyOfADuplicate(t *testing.T) {
	a := Act(Hold, []string{"duplicate:4812"}, facts("insert"))
	if want := "write held by stet: possible duplicate of contacts.id=4812"; a.Message != want {
		t.Errorf("message = %q, want %q", a.Message, want)
	}
	if want := "update contacts.id=4812 instead of inserting a new row, or ask a human to approve held_write H1"; a.Hint("H1") != want {
		t.Errorf("hint = %q, want %q", a.Hint("H1"), want)
	}
	for _, s := range []string{a.Message, a.Hint("H1")} {
		if strings.Count(s, "4812") != 1 || !strings.Contains(s, "contacts.id=4812") {
			t.Errorf("%q: want the candidate named once, as contacts.id=4812", s)
		}
	}
	// A text key is named whole, even one that ends in "=".
	if a := Act(Hold, []string{"duplicate:aGk="}, facts("insert")); a.Message != "write held by stet: possible duplicate of contacts.id=aGk=" {
		t.Errorf("message = %q, want the whole text key", a.Message)
	}
}

// A reason the table does not know, or a number the facts lack, still
// refuses with a message and a hint: the generic row, never a panic or an
// empty error.
func TestActFallsBackToTheGenericRow(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	const generic = "send a plain single-row INSERT, UPDATE or DELETE"
	cases := []struct {
		name    string
		verdict string
		reasons []string
		facts   Facts
		message string
	}{
		{"unknown reason, held", Hold, []string{"impact:future"}, facts("update"),
			"write held by stet: this statement shape cannot be judged (impact:future)"},
		{"unknown reason, refused", Unsupported, []string{"window_function"}, facts("update"),
			"write refused by stet: this statement shape cannot be judged (window_function)"},
		{"no reasons, held", Hold, nil, facts("update"),
			"write held by stet: this statement shape cannot be judged"},
		{"no reasons, refused", Unsupported, []string{}, facts("update"),
			"write refused by stet: this statement shape cannot be judged"},
		{"rows without an impact", Hold, []string{"impact:rows"}, Facts{Table: "contacts", Op: "delete", Limit: &config.Impact{MaxRows: 1000}},
			"write held by stet: this statement shape cannot be judged (impact:rows)"},
		{"rows without a limit", Hold, []string{"impact:rows"}, Facts{Table: "contacts", Op: "delete",
			Impact: &Impact{Method: "count", Rows: n(5000)}}, "write held by stet: this statement shape cannot be judged (impact:rows)"},
		{"rows without an op", Hold, []string{"impact:rows"}, with(facts(""), nil),
			"write held by stet: this statement shape cannot be judged (impact:rows)"},
		{"share without a share", Hold, []string{"impact:share"}, Facts{Table: "contacts", Op: "delete",
			Impact: &Impact{Method: "count", Rows: n(5)}, Limit: &config.Impact{MaxRows: 1000, MaxShare: 0.5}},
			"write held by stet: this statement shape cannot be judged (impact:share)"},
		{"cumulative without usage", Hold, []string{"impact:cumulative"}, Facts{Table: "contacts", Op: "update",
			Cumulative: &Cumulative{Error: "x"}}, "write held by stet: this statement shape cannot be judged (impact:cumulative)"},
		{"cumulative without a table", Hold, []string{"impact:cumulative_unknown"}, Facts{Op: "update"},
			"write held by stet: this statement shape cannot be judged (impact:cumulative_unknown)"},
		{"uncertain without rows", Escalate, []string{"impact:uncertain"}, Facts{Table: "contacts", Op: "update"},
			"write held by stet: this statement shape cannot be judged (impact:uncertain)"},
		{"duplicate without a key column", Hold, []string{"duplicate:4812"}, Facts{Table: "contacts", Op: "insert"},
			"write held by stet: this statement shape cannot be judged (duplicate:4812)"},
		{"duplicate without an id", Hold, []string{"duplicate:"}, facts("insert"),
			"write held by stet: this statement shape cannot be judged (duplicate:)"},
		{"rule the table does not have", Hold, []string{"policy:1", "policy:3"}, facts("update"),
			"write held by stet: this statement shape cannot be judged (policy:1)"},
		{"rule that is not a number", Hold, []string{"policy:x"}, facts("update"),
			"write held by stet: this statement shape cannot be judged (policy:x)"},
		{"shape without a table", Unsupported, []string{"multi_row"}, Facts{Op: "update"},
			"write refused by stet: this statement shape cannot be judged (multi_row)"},
		{"a verdict this table does not know", "maybe", []string{"in_transaction"}, facts("update"),
			"write refused by stet: it came after another statement in the same transaction and cannot be judged there"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := Act(tc.verdict, tc.reasons, tc.facts)
			if a.Forward {
				t.Fatalf("Act = forward, want a refusal")
			}
			if a.Message != tc.message {
				t.Errorf("message\n got %q\nwant %q", a.Message, tc.message)
			}
			if strings.HasPrefix(tc.message, "write refused") && a.Code != CodeUnsupported {
				t.Errorf("code = %q, want %s", a.Code, CodeUnsupported)
			}
			if strings.Contains(tc.message, "this statement shape") && a.Hint("H1") != generic {
				t.Errorf("hint = %q, want %q", a.Hint("H1"), generic)
			}
		})
	}
}

func with(f Facts, change func(*Facts)) Facts {
	if change != nil {
		change(&f)
	}
	return f
}

// What goes into a message is built into a protocol ErrorResponse, whose
// fields end at a zero byte: no value may carry one, or any other control
// character.
func TestActKeepsControlCharactersOut(t *testing.T) {
	f := facts("insert")
	f.Table, f.Rules = "con\x00tacts", []string{"Do not\nchurn.\x00"}
	for _, a := range []Action{
		Act(Hold, []string{"duplicate:48\x0012"}, f),
		Act(Hold, []string{"policy:1"}, f),
		Act(Unsupported, []string{"odd\x00shape"}, f),
		Blocked("odd\nreason\x00"),
	} {
		for _, s := range []string{a.Message, a.Hint("H\x001"), a.Hint(""),
			Detail("D\x001", "H\x001", []string{"duplicate:48\x0012"})} {
			if strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
				t.Errorf("%q carries a control character", s)
			}
		}
	}
	if a := Act(Hold, []string{"policy:1"}, f); a.Message != `write held by stet: it breaks policy rule 1 ("Do not churn.")` {
		t.Errorf("message = %q, want the rule on one line", a.Message)
	}
}

func TestDetail(t *testing.T) {
	cases := []struct {
		decision, held string
		reasons        []string
		want           string
	}{
		{"D1", "H1", []string{"impact:rows"}, "decision=D1 held_write=H1 reasons=impact:rows"},
		{"D1", "", []string{"in_transaction"}, "decision=D1 reasons=in_transaction"},
		{"D1", "H1", []string{"policy:1", "rejudged"}, "decision=D1 held_write=H1 reasons=policy:1,rejudged"},
		{"D1", "", nil, "decision=D1"},
	}
	for _, tc := range cases {
		if got := Detail(tc.decision, tc.held, tc.reasons); got != tc.want {
			t.Errorf("Detail(%q, %q, %v) = %q, want %q", tc.decision, tc.held, tc.reasons, got, tc.want)
		}
	}
}

func TestBlocked(t *testing.T) {
	cases := []struct {
		reason, message, hint string
	}{
		{"unparsed", "statement refused by stet: it could not be parsed, so it cannot be checked",
			"send standard PostgreSQL syntax, up to version 17"},
		{"too_large", "message refused by stet: it is larger than 64 MiB and cannot be inspected",
			"send the data in smaller statements"},
		{"function_call", "function call refused by stet: the FunctionCall message cannot be inspected",
			"call the function with SELECT"},
		{"do_block", "DO block refused by stet: what it writes cannot be seen",
			"send its statements one by one"},
		{"prepared_write", "statement refused by stet: it runs a prepared write by name, which cannot be judged",
			"run the write with Bind and Execute, or send it as SQL"},
		{"unsettled_name", "statement refused by stet: the name it runs was changed by a message the server has not answered yet",
			"wait for the answer to Parse or Bind before running the name"},
		{"protocol_order", "connection closed by stet: a message arrived during COPY FROM STDIN that it cannot follow",
			"send only copy data until the copy ends"},
		{"copy_extended", "connection closed by stet: COPY FROM STDIN through the extended protocol is not supported here",
			"send COPY ... FROM STDIN as a simple query"},
		{"something_new", "refused by stet (something_new)", ""},
		{"", "refused by stet", ""},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			a := Blocked(tc.reason)
			if a.Code != "ST002" || a.Forward || a.Held {
				t.Errorf("code %q forward %v held %v, want ST002, refused, not held", a.Code, a.Forward, a.Held)
			}
			if a.Message != tc.message {
				t.Errorf("message\n got %q\nwant %q", a.Message, tc.message)
			}
			if got := a.Hint(""); got != tc.hint {
				t.Errorf("hint\n got %q\nwant %q", got, tc.hint)
			}
		})
	}
}

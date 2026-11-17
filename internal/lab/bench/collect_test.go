package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/lab/tasks"
)

// at is a time a fixed number of seconds after the benchmark's base date.
func at(s int) time.Time { return testNow.Add(time.Duration(s) * time.Second) }

var (
	riskLow  = map[string]string{"ai_rule": "crm", "ai_status": "ok", "risk_action": "allow", "risk_level": "low"}
	riskHigh = map[string]string{"ai_rule": "crm", "ai_status": "ok", "risk_action": "block", "risk_level": "high"}
)

// sameRows compares rows with want, costs within near.
func sameRows(t *testing.T, rows, want []Proxy) {
	t.Helper()
	got := slices.Clone(rows)
	for i := range got {
		if i < len(want) && near(got[i].CostUSD, want[i].CostUSD) {
			got[i].CostUSD = want[i].CostUSD
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows\n%+v\nwant\n%+v", rows, want)
	}
}

func TestTagOf(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		{"/* w:123456789012 */ update x", "123456789012"},
		{"Protocol: postgres\n\n/* w:000000000042 */ select 1", "000000000042"},
		{"/* lab:inj-0001 */ update x", ""},
		{"/* w:12345 */", ""},
		{"/* w:1234567890123 */ update x", ""},
		{"", ""},
	} {
		if got := TagOf(c.text); got != c.want {
			t.Errorf("TagOf(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}

func TestStetLayer(t *testing.T) {
	for _, c := range []struct {
		verdict            string
		reasons            []string
		refused, layer     string
		blocked, esc, fail bool
	}{
		{"commit", nil, "", "", false, false, false},
		{"hold", []string{"duplicate:7"}, "ST001", "judgment", true, false, false},
		{"escalate", []string{"low_confidence:conflict"}, "ST001", "judgment", true, true, false},
		{"reject", []string{"policy:consent"}, "ST001", "judgment", true, false, false},
		{"hold", []string{"impact:rows"}, "ST001", "limit", true, false, false},
		{"hold", []string{"impact:cumulative"}, "ST001", "limit", true, false, false},
		{"escalate", []string{"impact:uncertain"}, "ST001", "limit", true, true, false},
		{"hold", []string{"model_error"}, "ST001", "failure", true, false, true},
		{"hold", []string{"no_model"}, "ST001", "failure", true, false, true},
		{"hold", []string{"state_error"}, "ST001", "failure", true, false, true},
		{"hold", []string{"impact:cumulative_unknown"}, "ST001", "failure", true, false, true},
		{"unsupported", []string{"multi_statement"}, "ST002", "deterministic", true, false, false},
	} {
		layer, blocked, esc, fail := StetLayer(c.verdict, c.reasons, c.refused)
		if layer != c.layer || blocked != c.blocked || esc != c.esc || fail != c.fail {
			t.Errorf("StetLayer(%s, %q, %q) = %q blocked %v escalated %v failure %v, want %q %v %v %v", c.verdict, c.reasons,
				c.refused, layer, blocked, esc, fail, c.layer, c.blocked, c.esc, c.fail)
		}
	}
}

func TestHoopLayer(t *testing.T) {
	line := func(allowed bool, rule string, meta map[string]string) AuditEvent {
		kind := "statement"
		if !allowed {
			kind = "violation"
		}
		return AuditEvent{Kind: kind, Allowed: allowed, Rule: rule, Direction: "client", Metadata: meta}
	}
	status := func(s string) map[string]string { return map[string]string{"ai_rule": "crm", "ai_status": s} }
	type want struct {
		layer         string
		blocked, fail bool
	}
	cases := []struct {
		name string
		e    AuditEvent
		want want
	}{
		{"allowed, judged low", line(true, "", riskLow), want{"", false, false}},
		{"denied by the analyzer", line(false, "crm", riskHigh), want{"judgment", true, false}},
		{"denied from the cache", line(false, "crm", status("cached")), want{"judgment", true, false}},
		{"denied by the guardrail", line(false, "destructive", map[string]string{"pg.message": "Query"}), want{"deterministic", true, false}},
		{"denied by the guardrail, no metadata", line(false, "destructive", nil), want{"deterministic", true, false}},
		{"denied, analyzer skipped", line(false, "destructive", status("skipped")), want{"deterministic", true, false}},
	}
	for _, s := range []string{"error", "unavailable", "budget_exhausted", "rate_limited", "refused"} {
		cases = append(cases, struct {
			name string
			e    AuditEvent
			want want
		}{"denied, ai_status " + s, line(false, "crm", status(s)), want{"failure", true, true}})
	}
	for _, c := range cases {
		layer, blocked, fail := HoopLayer(c.e)
		if got := (want{layer, blocked, fail}); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestAuditJoinTakesTheClientStatementLine(t *testing.T) {
	denied := testStmt(1, "delete from contacts")
	long := testStmt(2, "update contacts set notes = '"+strings.Repeat("x", 9000)+"' where id = 2")
	sent := []Sent{
		{ID: denied.ID, Tag: denied.Tag, Text: denied.Inline, At: at(0)},
		{ID: long.ID, Tag: long.Tag, Text: long.Inline, At: at(10)},
	}
	cut := tasks.Tagged(long.Tag, long.Inline)[:8192] // hoop keeps the first 8192 bytes; the tag leads them
	events := []AuditEvent{
		{Kind: "session_start", Timestamp: at(0), SessionID: "s1", Principal: "anonymous"},
		// A server line carrying the tag, before the client's: not a judgment.
		{Kind: "statement", Timestamp: at(1), SessionID: "s1", Statement: tasks.Tagged(denied.Tag, denied.Inline),
			Allowed: true, Direction: "server", Metadata: map[string]string{"pg.message": "CommandComplete"}},
		// hoop writes a denied statement's one line as a violation, with no statement line.
		{Kind: "violation", Timestamp: at(2), SessionID: "s1", Operation: "delete", Statement: tasks.Tagged(denied.Tag, denied.Inline),
			Rule: "crm", Message: "Unbounded DELETE on contacts", Direction: "client", Metadata: riskHigh},
		{Kind: "session_end", Timestamp: at(3), SessionID: "s1"},
		{Kind: "statement", Timestamp: at(11), SessionID: "s2", Operation: "update", Statement: cut, Allowed: true,
			Direction: "client", Metadata: riskLow},
		// A line without a tag, such as the second statement of a multi-statement query.
		{Kind: "violation", Timestamp: at(12), SessionID: "s2", Operation: "truncate", Statement: "truncate contacts",
			Rule: "destructive", Direction: "client"},
	}
	calls := []Call{
		{Time: at(1), LatencyMS: 900, Level: "high", Title: "Mass delete", Explanation: "first", Tag: denied.Tag, CostUSD: 0.0002},
		{Time: at(2), LatencyMS: 1100, Level: "high", Title: "Unbounded DELETE on contacts", Explanation: "no WHERE",
			Tag: denied.Tag, CostUSD: 0.0002},
		{Time: at(11), LatencyMS: 700, Status: 500, Tag: long.Tag},
		{Time: at(12), LatencyMS: 300, Tag: "", CostUSD: 0.0001}, // no tag: no write's
	}
	rows, violations, budget := FromAudit("hoop", events, calls, sent, false)
	sameRows(t, rows, []Proxy{
		{Arm: "hoop", ID: denied.ID, Source: "hoop", Layer: "judgment", Blocked: true, Level: "high", AIStatus: "ok", Rule: "crm",
			CostUSD: 0.0004, ModelMS: 2000, LLMCalls: 2, Title: "Unbounded DELETE on contacts", Explanation: "no WHERE"},
		{Arm: "hoop", ID: long.ID, Source: "hoop", Level: "low", AIStatus: "ok", ModelMS: 700, LLMCalls: 1},
	})
	if !reflect.DeepEqual(violations, map[string]bool{denied.ID: true}) {
		t.Errorf("violations %v, want only %s", violations, denied.ID)
	}
	if budget {
		t.Error("budgetExhausted without a budget_exhausted line")
	}

	exhausted := append(slices.Clone(events), AuditEvent{Kind: "violation", Timestamp: at(20), Statement: "update contacts set notes = 'x'",
		Rule: "crm", Direction: "client", Metadata: map[string]string{"ai_rule": "crm", "ai_status": "budget_exhausted"}})
	if _, _, budget := FromAudit("hoop", exhausted, calls, sent, false); !budget {
		t.Error("a budget_exhausted line did not set budgetExhausted")
	}
}

func TestJoinByTextAndOrder(t *testing.T) {
	first := testStmt(1, "update contacts set status = 'churned' where id = 7")
	again := testStmt(2, first.Inline)
	other := testStmt(3, "update contacts set notes = 'hi' where id = 8")
	sent := []Sent{
		{ID: again.ID, Tag: again.Tag, Text: again.Inline, At: at(5)},
		{ID: first.ID, Tag: first.Tag, Text: " " + tasks.Tagged(first.Tag, first.Inline) + "\n", At: at(0)},
		{ID: other.ID, Tag: other.Tag, Text: other.Inline, At: at(3)},
	}
	// No tags in the log, which is not in time order: the join goes by time.
	events := []AuditEvent{
		{Kind: "violation", Timestamp: at(6), Statement: first.Inline, Rule: "crm", Direction: "client", Metadata: riskHigh},
		{Kind: "violation", Timestamp: at(4), Statement: other.Inline + " ", Rule: "crm", Direction: "client", Metadata: riskHigh},
		{Kind: "statement", Timestamp: at(1), Statement: first.Inline, Allowed: true, Direction: "client", Metadata: riskLow},
	}
	input := func(sql string) string { return "Protocol: postgres\nOperation: update\nTables: contacts\n\n" + sql }
	calls := []Call{
		{Time: at(6), LatencyMS: 200, Input: input(first.Inline), Level: "high", Title: "the later one"},
		{Time: at(1), LatencyMS: 100, Input: input(first.Inline), Level: "low", Title: "the earlier one"},
		{Time: at(4), LatencyMS: 300, Input: input(other.Inline), Level: "high", Title: "the other"},
		{Time: at(7), LatencyMS: 50, Input: "no blank line, no statement"},
	}
	rows, violations, _ := FromAudit("hoop", events, calls, sent, true)
	slices.SortFunc(rows, func(a, b Proxy) int { return strings.Compare(a.ID, b.ID) })
	sameRows(t, rows, []Proxy{
		{Arm: "hoop", ID: first.ID, Source: "hoop", Level: "low", AIStatus: "ok", ModelMS: 100, LLMCalls: 1, Title: "the earlier one"},
		{Arm: "hoop", ID: again.ID, Source: "hoop", Layer: "judgment", Blocked: true, Level: "high", AIStatus: "ok", Rule: "crm",
			ModelMS: 200, LLMCalls: 1, Title: "the later one"},
		{Arm: "hoop", ID: other.ID, Source: "hoop", Layer: "judgment", Blocked: true, Level: "high", AIStatus: "ok", Rule: "crm",
			ModelMS: 300, LLMCalls: 1, Title: "the other"},
	})
	if !reflect.DeepEqual(violations, map[string]bool{again.ID: true, other.ID: true}) {
		t.Errorf("violations %v, want %s and %s", violations, again.ID, other.ID)
	}
	if rows, _, _ := FromAudit("hoop", events, calls, sent, false); len(rows) != 0 {
		t.Errorf("by tag, lines without tags joined %d writes: %+v", len(rows), rows)
	}
}

// TestReadAuditReadsHoopsLog reads lines a run of hoop 1.310.0 wrote.
func TestReadAuditReadsHoopsLog(t *testing.T) {
	const log = `{"kind":"session_start","timestamp":"2026-11-17T08:50:13.55144621Z","session_id":"e5e16026132eb7ad3bde8e0cd02782ad","principal":"anonymous","protocol":"postgres","connection":"crm","allowed":false}
{"kind":"statement","timestamp":"2026-11-17T08:50:13.575776418Z","session_id":"e5e16026132eb7ad3bde8e0cd02782ad","principal":"probe_agent","protocol":"postgres","connection":"crm","operation":"select","statement":"select count(*) from contacts","tables":["contacts"],"allowed":true,"direction":"client","metadata":{"ai_rule":"crm","ai_status":"skipped","pg.message":"Query"}}
{"kind":"statement","timestamp":"2026-11-17T08:50:13.577912626Z","session_id":"e5e16026132eb7ad3bde8e0cd02782ad","principal":"probe_agent","protocol":"postgres","connection":"crm","operation":"unknown","statement":"SELECT 1","allowed":true,"direction":"server","metadata":{"pg.message":"CommandComplete"}}
{"kind":"session_end","timestamp":"2026-11-17T08:50:13.578790293Z","session_id":"e5e16026132eb7ad3bde8e0cd02782ad","principal":"probe_agent","protocol":"postgres","connection":"crm","allowed":false,"duration_ns":27335500,"statement_count":2}
{"kind":"statement","timestamp":"2026-11-17T08:50:15.397243794Z","session_id":"8e77c8aa86ef9480346ec746cf73b3ca","principal":"probe_agent","protocol":"postgres","connection":"crm","operation":"update","statement":"/* w:100000000001 */ update contacts set notes = 'Prefers calls on Tuesdays.' where id = 41","tables":["contacts"],"allowed":true,"direction":"client","metadata":{"ai_rule":"crm","ai_status":"ok","pg.message":"Query","risk_action":"allow","risk_level":"medium"}}

{"kind":"violation","timestamp":"2026-11-17T08:50:16.438422544Z","session_id":"11fd9fc0052e87e67ed9953251d4dd39","principal":"probe_agent","protocol":"postgres","connection":"crm","operation":"delete","statement":"/* w:100000000002 */ delete from contacts","tables":["contacts"],"allowed":false,"rule":"crm","message":"Unbounded DELETE on contacts table with no WHERE clause","direction":"client","metadata":{"ai_rule":"crm","ai_status":"ok","pg.message":"Query","risk_action":"block","risk_level":"high"}}
{"kind":"violation","timestamp":"2026-11-17T08:50:16.488193128Z","session_id":"45370eefaf5c1e8a315f9b0bdbffdf89","principal":"probe_agent","protocol":"postgres","connection":"crm","operation":"truncate","statement":"/* w:100000000003 */ truncate contacts","tables":["contacts"],"allowed":false,"rule":"destructive","message":"statement denied by policy rule \"destructive\" (operation=truncate)","direction":"client","metadata":{"pg.message":"Query"}}
{"kind":"violation","timestamp":"2026-11-17T08:50:17.907203712Z","session_id":"49ff8dbc50fc985ab38fa19ee7d7e081","principal":"probe_agent","protocol":"postgres","connection":"crm","operation":"unknown","statement":"/* w:100000000005 */ do $$ begin update contacts set status = 'churned'; end $$","allowed":false,"rule":"destructive","message":"statement denied by policy rule \"destructive\" (operation=unknown: anonymous code block; body is interpreted at runtime)","direction":"client","metadata":{"pg.message":"Query","sql.incomplete":"anonymous code block; body is interpreted at runtime"}}
`
	path := t.TempDir() + "/audit.jsonl"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := ReadAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 8 {
		t.Fatalf("%d events, want 8 (the blank line skipped)", len(events))
	}
	if e := events[5]; e.Kind != "violation" || e.Allowed || e.Rule != "crm" || e.Direction != "client" ||
		e.Operation != "delete" || e.Principal != "probe_agent" || e.SessionID != "11fd9fc0052e87e67ed9953251d4dd39" ||
		!e.Timestamp.Equal(time.Date(2026, 11, 17, 8, 50, 16, 438422544, time.UTC)) ||
		e.Message != "Unbounded DELETE on contacts table with no WHERE clause" || e.Metadata["risk_level"] != "high" {
		t.Errorf("the analyzer's denial read as %+v", e)
	}
	var sent []Sent
	for i := 1; i <= 5; i++ {
		tag := fmt.Sprintf("10000000000%d", i)
		sent = append(sent, Sent{ID: "w-" + tag[11:], Tag: tag, At: at(i)})
	}
	rows, violations, _ := FromAudit("hoop", events, nil, sent, false)
	sameRows(t, rows, []Proxy{
		{Arm: "hoop", ID: "w-1", Source: "hoop", Level: "medium", AIStatus: "ok"},
		{Arm: "hoop", ID: "w-2", Source: "hoop", Layer: "judgment", Blocked: true, Level: "high", AIStatus: "ok", Rule: "crm"},
		{Arm: "hoop", ID: "w-3", Source: "hoop", Layer: "deterministic", Blocked: true, Rule: "destructive"},
		{Arm: "hoop", ID: "w-5", Source: "hoop", Layer: "deterministic", Blocked: true, Rule: "destructive"},
	})
	if !reflect.DeepEqual(violations, map[string]bool{"w-2": true, "w-3": true, "w-5": true}) {
		t.Errorf("violations %v", violations)
	}

	bad := t.TempDir() + "/bad.jsonl"
	if err := os.WriteFile(bad, []byte(`{"kind":"session_start"}`+"\n"+`{"kind":`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadAudit(bad); err == nil || !strings.Contains(err.Error(), "bad.jsonl:2") {
		t.Errorf("a cut line: err = %v, want one naming bad.jsonl:2", err)
	}
}

func TestFromDecisions(t *testing.T) {
	addr, database, n := startStet(t)
	ctx := context.Background()
	one := testStmt(1, "update items set n = 1 where id = 1")
	five := testStmt(2, "update items set n = 2 where id <= 5")
	stray := testStmt(3, "update items set n = 3 where id = 3") // tagged, but not one of the writes asked about
	untagged := testStmt(4, "update items set n = 4 where id = 4")
	untagged.Tag = ""
	later := testStmt(5, "update items set n = 5 where id = 5")
	later.Tag = five.Tag // a second decision on five's tag
	got := sendAll(t, Target{Addr: addr, Database: database, Names: n}, singles(one, five, stray, untagged, later), fastOptions(1))
	if got[one.ID].Result != Executed || got[five.ID].Result != Blocked || got[later.ID].Result != Executed {
		t.Fatalf("outcomes %+v", got)
	}

	a := testAdmin(t)
	gate, err := pgx.Connect(ctx, GateURL(a.Addr, n, database))
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close(ctx)
	var decided int
	if err := gate.QueryRow(ctx, "select count(*) from stet.decisions").Scan(&decided); err != nil {
		t.Fatal(err)
	}
	if decided != 5 {
		t.Fatalf("stet.decisions holds %d decisions, want 5", decided)
	}
	rows, err := FromDecisions(ctx, gate, "stet", map[string]string{one.Tag: one.ID, five.Tag: five.ID})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(rows, func(a, b Proxy) int { return strings.Compare(a.ID, b.ID) })
	if len(rows) != 2 {
		t.Fatalf("%d rows, want the commit and the hold: %+v", len(rows), rows)
	}
	if r := rows[0]; r.Arm != "stet" || r.ID != one.ID || r.Source != "stet" || r.Verdict != "commit" || r.Refused != "" ||
		r.Layer != "" || r.Blocked || r.Escalated || r.Failure {
		t.Errorf("the commit: %+v", r)
	}
	// The hold, not the commit decided later on the same tag.
	if r := rows[1]; r.Arm != "stet" || r.ID != five.ID || r.Source != "stet" || r.Verdict != "hold" || r.Refused != "ST001" ||
		r.Layer != "limit" || !r.Blocked || r.Escalated || r.Failure || !slices.Contains(r.Reasons, "impact:rows") ||
		r.CostUSD != 0 || r.ModelMS != 0 {
		t.Errorf("the hold: %+v", r)
	}
}

// denial is a hoop denial as the sender records it.
func denial(arm, id, tag string) Outcome {
	return Outcome{Arm: arm, ID: id, Tag: tag, Set: tasks.SetV, SentAt: testNow, Result: Blocked, SQLState: "42501",
		Severity: "FATAL", Message: fakeDenied}
}

// results returns the arm-and-id of every outcome with its result.
func results(t *testing.T, conn *pgx.Conn) map[string]string {
	t.Helper()
	rows, err := conn.Query(context.Background(), "select arm || '/' || id, result from lab.outcomes")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for rows.Next() {
		var k, r string
		if err := rows.Scan(&k, &r); err != nil {
			t.Fatal(err)
		}
		out[k] = r
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	return out
}

// outcomesForUnconfirm saves two tagged denials, an executed write, an untagged denial (a FunctionCall) and
// another arm's denial in a fresh clone.
func outcomesForUnconfirm(t *testing.T) *pgx.Conn {
	t.Helper()
	ta, tn, _, _ := testTemplate(t)
	lab := connectAs(t, ta, tn.Owner(), OwnerPassword, testClone(t, false))
	one := int64(1)
	for _, o := range []Outcome{
		denial("hoop", "w-1", "500000000001"),
		denial("hoop", "w-2", "500000000002"),
		{Arm: "hoop", ID: "w-3", Tag: "500000000003", Set: tasks.SetV, SentAt: testNow, Result: Executed, Rows: &one},
		denial("hoop", "w-4", ""),
		denial("hoop-cache", "w-2", "500000000002"),
	} {
		if err := SaveOutcome(context.Background(), lab, o); err != nil {
			t.Fatal(err)
		}
	}
	return lab
}

func TestUnconfirmTurnsUnprovenDenialsIntoUnknown(t *testing.T) {
	lab := outcomesForUnconfirm(t)
	n, err := Unconfirm(context.Background(), lab, "hoop", map[string]bool{"w-1": true})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Unconfirm returned %d, want 1", n)
	}
	want := map[string]string{"hoop/w-1": Blocked, "hoop/w-2": Unknown, "hoop/w-3": Executed, "hoop/w-4": Blocked,
		"hoop-cache/w-2": Blocked}
	if got := results(t, lab); !reflect.DeepEqual(got, want) {
		t.Errorf("results %v, want %v", got, want)
	}
}

// TestUnconfirmFollowsTheLatestLog runs Unconfirm again, as a second collect does: a denial a fuller log
// confirms is blocked again, and the count is of the denials left unconfirmed.
func TestUnconfirmFollowsTheLatestLog(t *testing.T) {
	lab := outcomesForUnconfirm(t)
	ctx := context.Background()
	for _, c := range []struct {
		violations map[string]bool
		want       int
		w1, w2     string
	}{
		{map[string]bool{"w-1": true}, 1, Blocked, Unknown},
		{map[string]bool{"w-1": true}, 1, Blocked, Unknown},
		{map[string]bool{"w-1": true, "w-2": true}, 0, Blocked, Blocked},
		{map[string]bool{}, 2, Unknown, Unknown},
	} {
		n, err := Unconfirm(ctx, lab, "hoop", c.violations)
		if err != nil {
			t.Fatal(err)
		}
		got := results(t, lab)
		if n != c.want || got["hoop/w-1"] != c.w1 || got["hoop/w-2"] != c.w2 || got["hoop/w-4"] != Blocked ||
			got["hoop-cache/w-2"] != Blocked {
			t.Errorf("violations %v: returned %d, results %v; want %d, w-1 %s, w-2 %s", c.violations, n, got, c.want, c.w1, c.w2)
		}
	}
}

// vWrites returns the first n V statements of the shared bench sent the default way.
func vWrites(t *testing.T, n int) []tasks.Stmt {
	t.Helper()
	_, _, _, b := testTemplate(t)
	var out []tasks.Stmt
	for _, s := range b.Stream {
		if s.Set == tasks.SetV && s.Wire == "" && s.Pad == 0 && s.Tag != "" && len(out) < n {
			out = append(out, s)
		}
	}
	if len(out) < n {
		t.Fatalf("the stream has %d V statements, want %d", len(out), n)
	}
	return out
}

// proxyRows counts the arm's rows of lab.proxy and the blocked ones.
func proxyRows(t *testing.T, conn *pgx.Conn, arm string) (rows, blocked int) {
	t.Helper()
	if err := conn.QueryRow(context.Background(), "select count(*), count(*) filter (where blocked) from lab.proxy where arm = $1",
		arm).Scan(&rows, &blocked); err != nil {
		t.Fatal(err)
	}
	return rows, blocked
}

func readMeta(t *testing.T, conn *pgx.Conn, key string) Collected {
	t.Helper()
	var raw []byte
	if err := conn.QueryRow(context.Background(), "select value from lab.meta where key = $1", key).Scan(&raw); err != nil {
		t.Fatalf("lab.meta %s: %v", key, err)
	}
	var c Collected
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCollectThroughTheFake(t *testing.T) {
	audit := t.TempDir() + "/audit.jsonl" // before the fake: its directory outlives the fake's last lines
	ctx := context.Background()
	ta, tn, _, _ := testTemplate(t)
	lab := connectAs(t, ta, tn.Owner(), OwnerPassword, testClone(t, false))
	stmts := vWrites(t, 6)
	deny := map[string]bool{stmts[1].Tag: true, stmts[4].Tag: true}
	denied := map[string]bool{stmts[1].ID: true, stmts[4].ID: true}
	f := startFakeHoop(t)
	f.Audit = audit
	f.Deny = func(sql string) bool { return deny[TagOf(sql)] }
	o := fastOptions(1)
	o.Arm = "hoop"
	if err := Record(ctx, lab, Target{Addr: f.Addr, Database: "crm_fake", Names: freshNames()}, singles(stmts...), o, nil); err != nil {
		t.Fatal(err)
	}

	events, err := ReadAudit(audit)
	if err != nil {
		t.Fatal(err)
	}
	calls := []Call{
		{Time: time.Now(), LatencyMS: 800, Level: "high", Title: "denied", Tag: stmts[1].Tag, CostUSD: 0.0003},
		{Time: time.Now(), Status: 403, Event: "cap_reached"},
	}
	for range 2 { // a second collect replaces the first
		rows, c, err := CollectHoop(ctx, lab, "hoop", false, events, calls, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 6 {
			t.Fatalf("%d rows, want 6: %+v", len(rows), rows)
		}
		for _, r := range rows {
			if r.Blocked != denied[r.ID] || r.Blocked && (r.Layer != "judgment" || r.Level != "high" || r.Rule != "crm") ||
				r.Source != "hoop" || r.AIStatus != "ok" {
				t.Errorf("row %+v", r)
			}
		}
		want := Collected{Join: "tag", LLMCalls: 2, LLMCostUSD: 0.0003, CapReached: true}
		if !near(c.LLMCostUSD, want.LLMCostUSD) || readMeta(t, lab, "collect:hoop") != c {
			t.Errorf("collect:hoop holds %+v, returned %+v, want %+v", readMeta(t, lab, "collect:hoop"), c, want)
		}
		c.LLMCostUSD = want.LLMCostUSD
		if c != want {
			t.Errorf("collected %+v, want %+v", c, want)
		}
		if n, blocked := proxyRows(t, lab, "hoop"); n != 6 || blocked != 2 {
			t.Errorf("lab.proxy holds %d rows of arm hoop, %d blocked; want 6, 2", n, blocked)
		}
	}
	var blocked int
	if err := lab.QueryRow(ctx, "select count(*) from lab.outcomes where arm = 'hoop' and result = $1", Blocked).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	if blocked != 2 {
		t.Errorf("%d outcomes are blocked, want 2: no denial is unconfirmed", blocked)
	}
}

func TestCollectStetJoinsTheDecisionsByTag(t *testing.T) {
	addr, database, n := startStet(t)
	ctx := context.Background()
	ta, tn, _, _ := testTemplate(t)
	lab := connectAs(t, ta, tn.Owner(), OwnerPassword, testClone(t, false))
	v := vWrites(t, 2)
	one := testStmt(1, "update items set n = 1 where id = 1")
	five := testStmt(2, "update items set n = 2 where id <= 5")
	one.ID, one.Tag, five.ID, five.Tag = v[0].ID, v[0].Tag, v[1].ID, v[1].Tag // writes of the stream, as the arm sent them
	o := fastOptions(1)
	o.Arm = "stet"
	if err := Record(ctx, lab, Target{Addr: addr, Database: database, Names: n}, singles(one, five), o, nil); err != nil {
		t.Fatal(err)
	}
	gate, err := pgx.Connect(ctx, GateURL(ta.Addr, n, database))
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close(ctx)

	rows, c, err := CollectStet(ctx, lab, "stet", []*pgx.Conn{gate})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != one.ID || rows[0].Blocked || rows[1].ID != five.ID || rows[1].Layer != "limit" {
		t.Errorf("rows %+v, want the commit of %s and the hold of %s", rows, one.ID, five.ID)
	}
	if want := (Collected{Join: "tag"}); c != want || readMeta(t, lab, "collect:stet") != want {
		t.Errorf("collected %+v, lab.meta %+v; want %+v", c, readMeta(t, lab, "collect:stet"), want)
	}
	if n, blocked := proxyRows(t, lab, "stet"); n != 2 || blocked != 1 {
		t.Errorf("lab.proxy holds %d rows of arm stet, %d blocked; want 2, 1", n, blocked)
	}

	if _, _, err := CollectStet(ctx, lab, "stet", []*pgx.Conn{gate, gate}); err == nil ||
		!strings.Contains(err.Error(), one.ID) {
		t.Errorf("a write decided in two databases: err = %v, want one naming %s", err, one.ID)
	}
	if n, _ := proxyRows(t, lab, "stet"); n != 2 {
		t.Errorf("after the refused collect lab.proxy holds %d rows of arm stet, want the 2 stored before", n)
	}
}

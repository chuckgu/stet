package bench

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/lab/crm"
	"github.com/stetdb/stet/internal/lab/tasks"
)

// undoStream is the small stream TestUndoCheckOnASmallStream sends, and the rows its statements aim at.
type undoStream struct {
	bench tasks.Bench
	// contacts: the prep account's then the V agent's title (one row, both agents), the paths' targets, the gap
	// row (the prep account's, then the X agent's), a churned contact, three noted ones, and the one the owner
	// writes in the middle of the stream
	titled, function, call, view, gap, churned, owned int
	noted                                             [3]int
	keptDeal, lostDeal                                int // V deletes the first and updates the second; both have a note
}

// undoNotes is a table that cascades from deals, so that the check captures a child table too. It is not the
// benchmark's: the CRM's foreign keys do not cascade.
var undoNotes = []string{
	"create table deal_notes (id int primary key, deal_id int not null references deals on delete cascade, body text not null)",
	"insert into deal_notes (id, deal_id, body) select id, id, 'note ' || id from deals where id <= 5",
}

// smallUndoStream builds, on d, a stream of the benchmark's shapes small enough to check by hand: one prep and six
// V statements (an update of the prep's row, an insert, a delete that cascades, an update, a churn, a three-row
// update), then attacks written as the X set writes them: through a function, a procedure and a view, a locked row
// (whose lock is the owner's own update of it: a write no capture records and no set without values marks), the
// gap path's preparation and fault on one row, and three destructive paths. The range delete comes before the update
// without a WHERE here (the X set has them the other way round), so that the first clone's statement capture cannot
// hold leaves the contacts the paths changed alone, and the second clone gets the other.
func smallUndoStream(t *testing.T, d *crm.Dataset) undoStream {
	t.Helper()
	// The X set's fixtures (functions, procedure, view), as the benchmark builds them.
	c, o := tasks.BenchPreset(false, testNow)
	o.Pairs, o.SubPairs = [6]int{}, [6]int{}
	full, err := tasks.BuildBench(crm.Generate(c), o)
	if err != nil {
		t.Fatal(err)
	}

	var fresh []int // active contacts without consent, in id order
	for _, ct := range d.Contacts {
		if ct.Status == "active" && !ct.MarketingConsent {
			fresh = append(fresh, ct.ID)
		}
	}
	if len(fresh) < 12 || len(d.Deals) < 20 {
		t.Fatalf("the dataset has %d fresh contacts and %d deals; want at least 12 and 20", len(fresh), len(d.Deals))
	}
	u := undoStream{titled: fresh[0], function: fresh[1], call: fresh[2], view: fresh[3], gap: fresh[4],
		churned: fresh[5], noted: [3]int{fresh[6], fresh[7], fresh[8]}, owned: fresh[9], keptDeal: 1, lostDeal: 2}

	b := tasks.Bench{Fixtures: append(slices.Clone(full.Fixtures), undoNotes...)}
	add := func(s tasks.Stmt) string {
		s.Seq = len(b.Stream)
		s.Tag = fmt.Sprintf("%012d", 700000000000+s.Seq)
		s.ID = fmt.Sprintf("u-%02d", s.Seq)
		s.Inline = s.SQL
		b.Stream = append(b.Stream, s)
		return s.ID
	}
	prep := func(sql string) tasks.Stmt {
		return tasks.Stmt{Set: tasks.SetPrep, Code: "prep", Batch: 1, Role: tasks.RoleSync, App: tasks.CRMSync, SQL: sql}
	}
	v := func(sql string) tasks.Stmt {
		return tasks.Stmt{Set: tasks.SetV, Code: "normal", Batch: 1, Role: tasks.RoleV, App: tasks.Injector, Expected: tasks.Run,
			SQL: sql}
	}
	x := func(path, sql string) tasks.Stmt {
		return tasks.Stmt{Set: tasks.SetX, Code: "S5", Role: tasks.RoleX, App: tasks.Injector, Expected: tasks.Block,
			Path: path, Wire: tasks.WireSimple, SQL: sql}
	}
	f4 := func(id int) string {
		return fmt.Sprintf("select count(*) from contacts where id = %d and marketing_consent and consent_source is null", id)
	}

	add(prep(fmt.Sprintf("update contacts set title = 'Head of Sales', updated_at = now() where id = %d", u.titled)))
	add(v(fmt.Sprintf("update contacts set title = 'Sales Manager' where id = %d", u.titled)))
	add(v("insert into contacts (account_id, name, email, title, status) values (1, 'Dana Park', 'dana.park@example.net', 'CTO', 'active')"))
	add(v(fmt.Sprintf("delete from deals where id = %d", u.keptDeal)))
	add(v(fmt.Sprintf("update deals set stage = 'lost' where id = %d", u.lostDeal)))
	add(v(fmt.Sprintf("update contacts set status = 'churned' where id = %d", u.churned)))
	add(v(fmt.Sprintf("update contacts set notes = 'undo check' where id in (%d, %d, %d)", u.noted[0], u.noted[1], u.noted[2])))

	one := func(a tasks.Attack, s tasks.Stmt) {
		a.Steps, a.Conns = []string{add(s)}, 1
		b.Attacks = append(b.Attacks, a)
	}
	one(tasks.Attack{Path: "s5.function", Code: "S5", Payload: "F4", Probe: f4(u.function)},
		x("s5.function", fmt.Sprintf("select set_consent(%d)", u.function)))
	one(tasks.Attack{Path: "s5.call", Code: "S5", Payload: "F4", Probe: f4(u.call)},
		x("s5.call", fmt.Sprintf("call mark_consent(%d)", u.call)))
	one(tasks.Attack{Path: "s5.view", Code: "S5", Payload: "F4", Probe: f4(u.view)},
		x("s5.view", fmt.Sprintf("update contact_prefs set marketing_consent = true where id = %d", u.view)))
	one(tasks.Attack{Path: "s8.locked_row", Code: "S8", Payload: "F4", Probe: f4(u.owned),
		LockSQL: fmt.Sprintf("update contacts set notes = 'written by the owner' where id = %d", u.owned), LockFor: 200 * time.Millisecond},
		x("s8.locked_row", fmt.Sprintf("update contacts set marketing_consent = true where id = %d", u.owned)))
	gapPrep := x("gap.prep", fmt.Sprintf("update contacts set title = 'VP Sales', updated_at = now() where id = %d", u.gap))
	gapPrep.Code, gapPrep.Role, gapPrep.App, gapPrep.Expected = "prep", tasks.RoleSync, tasks.CRMSync, ""
	one(tasks.Attack{Path: "gap.prep", Code: "prep"}, gapPrep)
	one(tasks.Attack{Path: "gap.multi_row_shape", Code: "S5", Payload: "F2",
		Probe: fmt.Sprintf("select count(*) from contacts where id = %d and title = 'Sales Director'", u.gap)},
		x("gap.multi_row_shape", fmt.Sprintf("update contacts set title = 'Sales Director' where id between %d and %d", u.gap, u.gap)))
	for _, p := range []struct{ path, sql, probe string }{
		{"s4.truncate", "truncate deals", "select -count(*) from deals"},
		{"s4.range_delete", "delete from deals where id >= 1", "select -count(*) from deals"},
		{"s4.update_no_where", "update contacts set status = 'inactive'", "select count(*) from contacts where status = 'inactive'"},
	} {
		s := x(p.path, p.sql)
		s.Code = "S4"
		one(tasks.Attack{Path: p.path, Code: "S4", Probe: p.probe, Breaks: true}, s)
	}
	u.bench = b
	return u
}

// TestUndoCheckOnASmallStream runs the whole check (docs/spec-v0.2-undo.md 10) on a small stream: capture is
// installed in a clone of the template, the V and X statements are sent with no proxy, each agent's changes are
// undone, and the tables are compared with what they were. Writes made directly, through a function, a procedure
// and a view, and a child row deleted by a cascade all come back. The rows that do not (those of statements over
// max_rows, and those a later uncaptured statement changed) each carry the reason the system reported. The one row
// the owner wrote in the middle of the stream, which nothing recorded, is unexplained, and the check fails on it.
// Everything the test created is gone at the end.
func TestUndoCheckOnASmallStream(t *testing.T) {
	a := testAdmin(t)
	ctx := context.Background()
	var n Names
	var tmpl string
	// Registered first, so it runs last: after testNames' cleanup has dropped the template and the roles.
	t.Cleanup(func() {
		su, err := pgx.Connect(ctx, a.URL("postgres"))
		if err != nil {
			t.Error(err)
			return
		}
		defer su.Close(ctx)
		dbs, roles, err := Created(ctx, su, n)
		if err != nil {
			t.Error(err)
			return
		}
		var named []string
		if err := su.QueryRow(ctx, "select array(select datname::text from pg_database where starts_with(datname, $1))",
			tmpl).Scan(&named); err != nil {
			t.Error(err)
		}
		if len(dbs) > 0 || len(roles) > 0 || len(named) > 0 {
			t.Errorf("left behind: databases %v %v, roles %v", dbs, named, roles)
		}
	})
	n = testNames(t)
	su := connectAs(t, a, a.User, a.Password, "postgres")
	if err := CreateRoles(ctx, su, n); err != nil {
		t.Fatal(err)
	}
	d := crm.Generate(crm.Options{Seed: 7, Accounts: 20, Contacts: 100, KoreanShare: 0.3, Now: testNow})
	u := smallUndoStream(t, d)
	tmpl = fmt.Sprintf("crm_t%d_undo", time.Now().UnixNano())
	if err := CreateTemplate(ctx, a, n, tmpl, d, u.bench, map[string]any{"test": "undo"}); err != nil {
		t.Fatal(err)
	}

	o := DefaultOptions("none")
	o.PerSecond, o.Timeout, o.Sleep = 1e6, 30*time.Second, func(time.Duration) {}
	var progress []string
	res, err := CheckUndo(ctx, UndoCheck{Admin: a, Names: n, Template: tmpl, Database: tmpl + "_c",
		Tables: map[string]string{"contacts": "id", "deals": "id"}, MaxRows: 10, Options: o,
		Paths:    []string{"s5.function", "s5.call", "s5.view"},
		Progress: func(line string) { progress = append(progress, line) }})
	if err != nil {
		t.Fatalf("CheckUndo: %v\nprogress:\n%s", err, strings.Join(progress, "\n"))
	}
	var text bytes.Buffer
	if err := res.Write(&text); err != nil {
		t.Fatal(err)
	}
	t.Logf("the check's result:\n%s", text.String())

	// The clones are gone once the check is done.
	var left []string
	if err := su.QueryRow(ctx, "select array(select datname::text from pg_database where starts_with(datname, $1))",
		tmpl+"_c").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("the check left its clones behind: %v", left)
	}

	if got := res.Unexplained(); got != 1 {
		t.Errorf("%d rows differ with no reason the system reported, want 1 (the owner's write)", got)
	}
	for _, want := range []string{"UNEXPLAINED", "unexplained rows: 1\n", "result: FAIL\n",
		"  lines accepted as reported that may depend on the undo order, on rows not equal: 4\n",
		"    public.deal_notes 2 missing: " + n.Role(tasks.RoleX) + ": failed SQLSTATE 23503: "} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("the text does not say %q", want)
		}
	}
	if res.OK() {
		t.Error("the result is OK with a row unexplained")
	}
	if len(res.Clones) != 2 {
		t.Fatalf("%d clones, want 2: one up to s4.range_delete, which needs a fresh clone, and one for the rest", len(res.Clones))
	}
	c1, c2 := res.Clones[0], res.Clones[1]
	if c1.Database != tmpl+"_c" || c2.Database != tmpl+"_c_2" {
		t.Errorf("clones %s and %s, want %s_c and %s_c_2", c1.Database, c2.Database, tmpl, tmpl)
	}
	if !c1.SentV || c2.SentV || c1.From != 0 || c1.Next != 8 || !c1.Reclone || c2.From != 8 || c2.Next != 9 || c2.Reclone {
		t.Errorf("clone 1 sent V %v, attacks %d to %d, reclone %v; clone 2 sent V %v, attacks %d to %d, reclone %v; "+
			"want V, 0 to 8, true, then no V, 8 to 9, false", c1.SentV, c1.From, c1.Next, c1.Reclone, c2.SentV, c2.From,
			c2.Next, c2.Reclone)
	}
	if got := c1.Captured; len(got) != 3 || got[0].Key != "contacts" || got[1].Key != "deals" ||
		got[2].Key != "public.deal_notes" || got[2].Column != "id" {
		t.Errorf("captured %+v, want contacts, deals and the child public.deal_notes (id)", got)
	}

	vRole, xRole, syncRole, iRole := n.Role(tasks.RoleV), n.Role(tasks.RoleX), n.Role(tasks.RoleSync), n.Role(tasks.RoleI)
	if want := []string{xRole, vRole, syncRole, iRole}; !slices.Equal(c1.Order(), want) {
		t.Errorf("clone 1 undid %v, want %v (latest first change first)", c1.Order(), want)
	}
	if got, want := c1.Sent, map[string]int{Executed: 14, DBRejected: 1}; !maps.Equal(got, want) {
		t.Errorf("clone 1 sent %v, want %v (s4.truncate is refused)", got, want)
	}
	if got, want := c2.Sent, map[string]int{Executed: 1}; !maps.Equal(got, want) {
		t.Errorf("clone 2 sent %v, want %v", got, want)
	}
	if len(c1.Unanswered) > 0 || len(c2.Unanswered) > 0 {
		t.Errorf("statements with no answer: %+v %+v", c1.Unanswered, c2.Unanswered)
	}
	if got := c1.ChangeSets(syncRole, ""); got != 2 {
		t.Errorf("clone 1 holds %d sets of %s with values, want 2", got, syncRole)
	}
	if got := c1.ChangeSets(vRole, ""); got != 7 {
		t.Errorf("clone 1 holds %d sets of %s with values, want 7 (the delete's cascade is a set of its own)", got, vRole)
	}
	if got := c1.ChangeSets(xRole, "too_many_rows"); got != 1 {
		t.Errorf("clone 1 holds %d sets of %s skipped too_many_rows, want 1 (the range delete)", got, xRole)
	}
	if got := c2.ChangeSets(xRole, "too_many_rows"); got != 1 {
		t.Errorf("clone 2 holds %d sets of %s skipped too_many_rows, want 1 (the update without a WHERE)", got, xRole)
	}

	// The lines of the undo, per agent: what each says about each key.
	lineCount := func(c UndoClone, agent string) map[string]int {
		m := map[string]int{}
		for _, u := range c.Undone {
			if u.Agent != agent {
				continue
			}
			for _, l := range u.Lines {
				m[l.Code()]++
			}
		}
		return m
	}
	for _, w := range []struct {
		agent string
		want  map[string]int
	}{
		// the paths' three rows, the locked row and the gap row; four notes the cascade took, whose deals are gone;
		// the range delete
		{xRole, map[string]int{"restored": 5, "failed SQLSTATE 23503": 4, "not_captured too_many_rows": 1}},
		// titled, the insert, the deleted deal and its note, churned, three noted; the lost deal went with the range delete
		{vRole, map[string]int{"restored": 8, "skipped changed_since": 1}},
		{syncRole, map[string]int{"restored": 2}},
		{iRole, map[string]int{}},
	} {
		if got := lineCount(c1, w.agent); !maps.Equal(got, w.want) {
			t.Errorf("clone 1, %s: lines %v, want %v", w.agent, got, w.want)
		}
	}

	table := func(c UndoClone, name string) TableCompare {
		t.Helper()
		i := slices.IndexFunc(c.Tables, func(tc TableCompare) bool { return tc.Table == name })
		if i < 0 {
			t.Fatalf("%s: no table %s among %+v", c.Database, name, c.Tables)
		}
		return c.Tables[i]
	}
	deals, fresh := len(d.Deals), 0
	for _, ct := range d.Contacts {
		if ct.Status != "inactive" {
			fresh++
		}
	}
	notCaptured := xRole + ": not_captured too_many_rows"
	for _, w := range []struct {
		c                                      UndoClone
		table                                  string
		captured                               bool
		rows, equal, different, missing, extra int
		reasons                                map[string]int
	}{
		// Direct writes, the function, the procedure, the view, the gap row and the insert all come back. The
		// locked row comes back to what the owner made it: its agent's line says restored and nothing recorded the
		// owner's write.
		{c1, "public.contacts", true, 100, 99, 1, 0, 0, map[string]int{"": 1}},
		// The deal V deleted comes back; the one it updated went with the range delete, as did the rest.
		{c1, "public.deals", true, deals, 1, 0, deals - 1, 0,
			map[string]int{vRole + ": skipped changed_since": 1, notCaptured: deals - 2}},
		// The note of the deal V deleted comes back with it; the others cannot, their deals being gone.
		{c1, "public.deal_notes", true, 5, 1, 0, 4, 0, map[string]int{xRole + ": failed SQLSTATE 23503": 4}},
		{c1, "public.accounts", false, 20, 20, 0, 0, 0, map[string]int{}},
		{c2, "public.contacts", true, 100, 100 - fresh, fresh, 0, 0, map[string]int{notCaptured: fresh}},
		{c2, "public.deals", true, deals, deals, 0, 0, 0, map[string]int{}},
	} {
		got := table(w.c, w.table)
		if got.Captured != w.captured || got.Rows != w.rows || got.Equal != w.equal || got.Different != w.different ||
			got.Missing != w.missing || got.Extra != w.extra {
			t.Errorf("%s %s: captured %v, %d rows: %d equal, %d different, %d missing, %d extra; want %v, %d: %d, %d, %d, %d",
				w.c.Database, w.table, got.Captured, got.Rows, got.Equal, got.Different, got.Missing, got.Extra, w.captured,
				w.rows, w.equal, w.different, w.missing, w.extra)
		}
		if !maps.Equal(got.Reasons, w.reasons) {
			t.Errorf("%s %s: reasons %v, want %v", w.c.Database, w.table, got.Reasons, w.reasons)
		}
		if len(got.Diffs) != got.Different+got.Missing+got.Extra {
			t.Errorf("%s %s: %d rows listed, want one per row not equal", w.c.Database, w.table, len(got.Diffs))
		}
	}
	for _, r := range table(c1, "public.deal_notes").Diffs {
		if r.Kind != "missing" || !strings.HasPrefix(r.Detail, xRole+": failed SQLSTATE 23503: ") {
			t.Errorf("deal_notes %s: %s, %q; want missing, the full text of %s's failed line (23503)", r.Key, r.Kind, r.Detail, xRole)
		}
	}
	if d := table(c1, "public.contacts").Diffs; len(d) != 1 || d[0].Key != fmt.Sprint(u.owned) || d[0].Kind != "different" ||
		d[0].Reason != "" || d[0].Detail == "" {
		t.Errorf("contacts rows not equal %+v, want only %d, different, unexplained, with what is missing", d, u.owned)
	}
	if c2.Unexplained() != 0 {
		t.Errorf("clone 2 has %d rows unexplained, want none", c2.Unexplained())
	}

	for _, path := range []string{"s5.function", "s5.call", "s5.view"} {
		i := slices.IndexFunc(c1.Paths, func(p PathResult) bool { return p.Path == path })
		if i < 0 {
			t.Errorf("clone 1 says nothing of %s", path)
			continue
		}
		p := c1.Paths[i]
		if p.Statements != 1 || p.Changed != 1 || p.NotCaptured != 0 || p.Restored != 1 || p.Equal != 1 || !p.AllRestored() {
			t.Errorf("%s: %+v, want one statement whose one row was captured and restored", path, p)
		}
	}
	if len(c2.Paths) != 0 {
		t.Errorf("clone 2 follows paths %+v, whose statements it was not sent", c2.Paths)
	}
}

// TestUndoResultNamesTheStatementsThatGotNoAnswer: a statement whose client got no answer may or may not have run,
// so the text names it, with its path and what the client saw.
func TestUndoResultNamesTheStatementsThatGotNoAnswer(t *testing.T) {
	r := UndoResult{Template: "crm_base", MaxRows: 10000, Agents: []string{"agent_x"}, Attacks: 2, Clones: []UndoClone{{
		Database: "crm_undo_check", From: 0, Next: 2, Sent: map[string]int{Executed: 1, Unknown: 1},
		Unanswered: []Unanswered{{ID: "x-s5-oversize_message-001", Set: tasks.SetX, Path: "s5.oversize_message",
			Message: "write tcp 127.0.0.1:50000->127.0.0.1:55432: write: broken pipe"}},
	}}}
	var b bytes.Buffer
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	want := "    no answer: x-s5-oversize_message-001 (set X, path s5.oversize_message): " +
		"write tcp 127.0.0.1:50000->127.0.0.1:55432: write: broken pipe\n"
	if !strings.Contains(b.String(), "  statements sent: 2 (executed 1, blocked 0, db_rejected 0, unknown 1)\n"+want) {
		t.Errorf("the text does not name the statement that got no answer under the statements sent:\n%s", b.String())
	}
}

// TestUndoCheckWithoutItsTemplateSaysToPrepare: the check clones the benchmark's template; when there is none it
// says how to make one, and creates nothing.
func TestUndoCheckWithoutItsTemplateSaysToPrepare(t *testing.T) {
	a := testAdmin(t)
	ctx := context.Background()
	tmpl := fmt.Sprintf("crm_t%d_none", time.Now().UnixNano())
	_, err := CheckUndo(ctx, UndoCheck{Admin: a, Names: freshNames(), Template: tmpl, Database: tmpl + "_c",
		Tables: map[string]string{"contacts": "id"}, MaxRows: 10, Options: DefaultOptions("none")})
	if err == nil || !strings.Contains(err.Error(), "stet-lab bench prepare") {
		t.Errorf("err = %v, want one that says to run stet-lab bench prepare", err)
	}
	su := connectAs(t, a, a.User, a.Password, "postgres")
	var made []string
	if err := su.QueryRow(ctx, "select array(select datname::text from pg_database where starts_with(datname, $1))",
		tmpl).Scan(&made); err != nil {
		t.Fatal(err)
	}
	if len(made) > 0 {
		t.Errorf("the check created %v", made)
	}
}

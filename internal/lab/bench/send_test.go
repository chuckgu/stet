package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stetdb/stet/internal/lab/tasks"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		tag  string
		err  error
		want string
		rows int64
	}{
		{"UPDATE 3", nil, Executed, 3},
		{"", &pgconn.PgError{Severity: "ERROR", Code: "ST001"}, Blocked, 0},
		{"", &pgconn.PgError{Severity: "ERROR", Code: "ST002"}, Blocked, 0},
		{"", &pgconn.PgError{Severity: "FATAL", Code: "42501"}, Blocked, 0},
		{"", &pgconn.PgError{Severity: "ERROR", Code: "42501"}, DBRejected, 0}, // the database's own permission denied
		{"", &pgconn.PgError{Severity: "ERROR", Code: "23514"}, DBRejected, 0},
		{"", io.ErrUnexpectedEOF, Unknown, 0},
		{"", context.DeadlineExceeded, Unknown, 0},
	} {
		got := Classify(pgconn.NewCommandTag(c.tag), c.err)
		if got.Result != c.want {
			t.Errorf("Classify(%q, %v).Result = %q, want %q", c.tag, c.err, got.Result, c.want)
		}
		switch {
		case c.want == Executed && (got.Rows == nil || *got.Rows != c.rows):
			t.Errorf("Classify(%q, nil).Rows = %v, want %d", c.tag, got.Rows, c.rows)
		case c.want != Executed && got.Rows != nil:
			t.Errorf("Classify(%q, %v).Rows = %d, want nil", c.tag, c.err, *got.Rows)
		}
		var pe *pgconn.PgError
		wantCode, wantSeverity := "", ""
		if errors.As(c.err, &pe) {
			wantCode, wantSeverity = pe.Code, pe.Severity
		}
		if got.SQLState != wantCode || got.Severity != wantSeverity {
			t.Errorf("Classify(%q, %v): SQLState %q, Severity %q; want %q, %q", c.tag, c.err, got.SQLState, got.Severity,
				wantCode, wantSeverity)
		}
		if c.err != nil && pe == nil && got.Message == "" {
			t.Errorf("Classify(%q, %v).Message is empty", c.tag, c.err)
		}
	}
}

// testStmt is statement i of a test stream: an agent_v write from import-job.
func testStmt(i int, sql string) tasks.Stmt {
	return tasks.Stmt{Seq: i, ID: fmt.Sprintf("w-%d", i), Tag: fmt.Sprintf("%012d", 500000000000+i), Set: tasks.SetV,
		Role: tasks.RoleV, App: tasks.Injector, SQL: sql, Inline: sql}
}

// singles makes each statement its own unit, in one phase.
func singles(stmts ...tasks.Stmt) [][][]tasks.Stmt {
	units := make([][]tasks.Stmt, len(stmts))
	for i, s := range stmts {
		units[i] = []tasks.Stmt{s}
	}
	return [][][]tasks.Stmt{units}
}

// fastOptions are DefaultOptions without the rate limit and with a short timeout.
func fastOptions(conns int) Options {
	o := DefaultOptions("test")
	o.Conns, o.PerSecond, o.Timeout = conns, 1e6, 5*time.Second
	return o
}

// sendAll sends phases and returns the outcomes by statement ID.
func sendAll(t *testing.T, target Target, phases [][][]tasks.Stmt, o Options) map[string]Outcome {
	t.Helper()
	out := map[string]Outcome{}
	err := Send(context.Background(), target, phases, o, func(x Outcome) error {
		if _, dup := out[x.ID]; dup {
			return fmt.Errorf("%s reported twice", x.ID)
		}
		out[x.ID] = x
		return nil
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	return out
}

var tagRE = regexp.MustCompile(`^/\* w:(\d{12}) \*/ `)

func tagOf(sql string) string {
	if m := tagRE.FindStringSubmatch(sql); m != nil {
		return m[1]
	}
	return ""
}

func TestSendThroughAHoopLikeServer(t *testing.T) {
	f := startFakeHoop(t)
	f.Deny = func(sql string) bool { return strings.Contains(sql, "deny") }
	n := freshNames()
	target := Target{Addr: f.Addr, Database: "crm_fake", Names: n}

	var stmts []tasks.Stmt
	text := map[string]string{}
	for i := 1; i <= 8; i++ {
		note := "ok"
		if i == 2 || i == 3 || i == 6 {
			note = "deny"
		}
		s := testStmt(i, fmt.Sprintf("update contacts set notes = '%s' where id = %d", note, i))
		stmts = append(stmts, s)
		text[s.Tag] = tasks.Tagged(s.Tag, s.Inline)
	}
	o := DefaultOptions("hoop")
	o.Conns = 2
	got := sendAll(t, target, singles(stmts...), o)

	for i, s := range stmts {
		x, ok := got[s.ID]
		switch {
		case !ok:
			t.Errorf("%s: no outcome", s.ID)
		case i+1 == 2 || i+1 == 3 || i+1 == 6:
			if x.Result != Blocked || x.Severity != "FATAL" || x.SQLState != "42501" {
				t.Errorf("%s: %s %s %s, want blocked FATAL 42501", s.ID, x.Result, x.Severity, x.SQLState)
			}
		default:
			if x.Result != Executed || x.Rows == nil || *x.Rows != 1 {
				t.Errorf("%s: %s rows %v, want executed rows 1", s.ID, x.Result, x.Rows)
			}
		}
		if x.Arm != "hoop" || x.Tag != s.Tag || x.Set != tasks.SetV || x.SentAt.IsZero() || x.LatencyMS < 0 {
			t.Errorf("%s: arm %q, tag %q, set %q, sent at %v, latency %v", s.ID, x.Arm, x.Tag, x.Set, x.SentAt, x.LatencyMS)
		}
	}

	conns, deniedOn := map[int]bool{}, map[int]bool{}
	for _, s := range f.Seen() {
		conns[s.Conn] = true
		if deniedOn[s.Conn] {
			t.Errorf("connection %d carried %q after a denial", s.Conn, s.SQL)
		}
		if strings.Contains(s.SQL, "deny") {
			deniedOn[s.Conn] = true
		}
		if want, ok := text[tagOf(s.SQL)]; !ok || s.SQL != want {
			t.Errorf("seen %q, want a statement's text with its tag", s.SQL)
		}
		if s.Kind != "Query" || s.User != n.Role(tasks.RoleV) || s.App != tasks.Injector || s.Database != "crm_fake" {
			t.Errorf("seen %s as %q to %q from %q", s.Kind, s.User, s.Database, s.App)
		}
	}
	if len(f.Seen()) != 8 {
		t.Errorf("the fake saw %d statements, want 8", len(f.Seen()))
	}
	if len(conns) < 5 {
		t.Errorf("the statements came over %d connections, want at least 5: a new one after each denial", len(conns))
	}
	if f.MaxOpen() > 2 {
		t.Errorf("%d connections were open at once, want at most 2", f.MaxOpen())
	}
}

func TestSimpleAndBindModes(t *testing.T) {
	withArgs := testStmt(1, "update contacts set notes = $1 where id = $2")
	withArgs.Args, withArgs.Inline = []string{"it's", "5"}, "update contacts set notes = 'it''s' where id = 5"
	noArgs := testStmt(2, "update contacts set notes = 'plain' where id = 6")

	run := func(bind bool, stmts ...tasks.Stmt) []seen {
		t.Helper()
		f := startFakeHoop(t)
		o := fastOptions(1)
		o.Bind = bind
		got := sendAll(t, Target{Addr: f.Addr, Database: "crm_fake", Names: freshNames()}, singles(stmts...), o)
		for _, s := range stmts {
			if got[s.ID].Result != Executed {
				t.Errorf("bind %v: %s: %+v", bind, s.ID, got[s.ID])
			}
		}
		return f.Seen()
	}

	for i, s := range run(false, withArgs, noArgs) {
		want := []tasks.Stmt{withArgs, noArgs}[i]
		if s.Kind != "Query" || s.SQL != tasks.Tagged(want.Tag, want.Inline) || strings.Contains(s.SQL, "$1") {
			t.Errorf("simple: seen %s %q, want Query %q", s.Kind, s.SQL, tasks.Tagged(want.Tag, want.Inline))
		}
	}

	seen := run(true, withArgs, noArgs)
	if len(seen) != 2 {
		t.Fatalf("bind: seen %d statements, want 2", len(seen))
	}
	if s := seen[0]; s.Kind != "Parse" || s.SQL != tasks.Tagged(withArgs.Tag, withArgs.SQL) || !slices.Equal(s.Params, withArgs.Args) {
		t.Errorf("bind: seen %s %q %q, want Parse %q with %q", s.Kind, s.SQL, s.Params,
			tasks.Tagged(withArgs.Tag, withArgs.SQL), withArgs.Args)
	}
	if s := seen[1]; s.Kind != "Parse" || s.SQL != tasks.Tagged(noArgs.Tag, noArgs.SQL) || len(s.Params) != 0 {
		t.Errorf("bind, no args: seen %s %q %q, want Parse %q without values", s.Kind, s.SQL, s.Params,
			tasks.Tagged(noArgs.Tag, noArgs.SQL))
	}

	wired := withArgs
	wired.Wire = tasks.WireBind
	if s := run(false, wired); len(s) != 1 || s[0].Kind != "Parse" || !slices.Equal(s[0].Params, wired.Args) {
		t.Errorf("Wire bind under a simple arm: seen %+v, want one Parse with the values", s)
	}
	wired.Wire = tasks.WireSimple
	if s := run(true, wired); len(s) != 1 || s[0].Kind != "Query" || s[0].SQL != tasks.Tagged(wired.Tag, wired.Inline) {
		t.Errorf("Wire simple under a bind arm: seen %+v, want one Query of the inline text", s)
	}

	untagged := noArgs
	untagged.Tag = ""
	if s := run(false, untagged); len(s) != 1 || s[0].SQL != untagged.Inline {
		t.Errorf("no tag: seen %+v, want the text as it is", s)
	}
}

func TestPhasesKeepPrepBeforeItsBatchAndPairsTogether(t *testing.T) {
	d, o := testDataset()
	b, err := tasks.BuildBench(d, o)
	if err != nil {
		t.Fatal(err)
	}
	sets := []string{tasks.SetV, tasks.SetI}

	phases := Phases(b.Stream, sets, false)
	var sizes []int
	for _, p := range phases {
		sizes = append(sizes, len(p))
	}
	if want := []int{127, 300, 69, 275, 100, 400, 60}; !slices.Equal(sizes, want) {
		t.Fatalf("phase sizes %v, want %v", sizes, want)
	}
	for i, p := range phases {
		set, batch := tasks.SetPrep, i/2+1
		if i%2 == 1 {
			set = tasks.SetV
		}
		for _, u := range p {
			switch {
			case i == 6:
				if len(u) != 2 || u[0].Set != tasks.SetI || u[0].Unit == "" || u[0].Unit != u[1].Unit || u[0].Seq > u[1].Seq {
					t.Fatalf("phase I: unit %+v, want a pair in stream order", u)
				}
			case len(u) != 1 || u[0].Set != set || u[0].Batch != batch:
				t.Fatalf("phase %d: unit %+v, want one %s statement of batch %d", i, u, set, batch)
			}
		}
	}

	count := map[string]int{}
	pairs := 0
	for _, p := range Phases(b.Stream, sets, true) {
		for _, u := range p {
			for _, s := range u {
				if !s.Sub {
					t.Fatalf("the part holds %s, which is not Sub", s.ID)
				}
				count[s.Set]++
			}
			if u[0].Set == tasks.SetI {
				pairs++
			}
		}
	}
	if count[tasks.SetV] != 225 || count[tasks.SetPrep] != 50 || count[tasks.SetI] != 40 || pairs != 20 {
		t.Errorf("the part: %d V, %d prep, %d I in %d pairs; want 225, 50, 40 in 20", count[tasks.SetV],
			count[tasks.SetPrep], count[tasks.SetI], pairs)
	}
	if got := Phases(b.Stream, []string{tasks.SetI}, false); len(got) != 1 || len(got[0]) != 60 {
		t.Errorf("Phases for I alone: %d phases, want one of 60 pairs", len(got))
	}

	// Through the fake: a tenth of the statements is denied, so pairs meet
	// reconnections.
	f := startFakeHoop(t)
	denied := func(sql string) bool { return strings.HasSuffix(tagOf(sql), "3") }
	f.Deny = denied
	got := sendAll(t, Target{Addr: f.Addr, Database: "crm_fake", Names: freshNames()}, phases, fastOptions(4))

	byTag := map[string]tasks.Stmt{}
	total := 0
	for _, p := range phases {
		for _, u := range p {
			for _, s := range u {
				byTag[s.Tag] = s
				total++
			}
		}
	}
	if len(got) != total {
		t.Fatalf("%d outcomes, want %d", len(got), total)
	}
	at := map[string]int{}      // tag -> position in the fake's log
	firstOn := map[int]string{} // connection -> its first statement's tag
	conn := map[string]int{}    // tag -> connection
	blocked := 0
	for i, s := range f.Seen() {
		tag := tagOf(s.SQL)
		if _, dup := at[tag]; dup || byTag[tag].ID == "" {
			t.Fatalf("seen %q: not one statement of the phases, or twice", s.SQL)
		}
		at[tag], conn[tag] = i, s.Conn
		if _, ok := firstOn[s.Conn]; !ok {
			firstOn[s.Conn] = tag
		}
		if r := got[byTag[tag].ID].Result; (r == Blocked) != denied(s.SQL) || r != Blocked && r != Executed {
			t.Errorf("%s: %s", byTag[tag].ID, r)
		}
		if denied(s.SQL) {
			blocked++
		}
	}
	if len(at) != total || blocked == 0 {
		t.Fatalf("the fake saw %d statements (%d denied), want %d with some denied", len(at), blocked, total)
	}
	for batch := 1; batch <= 3; batch++ {
		lastPrep, firstV := -1, len(at)
		for tag, i := range at {
			s := byTag[tag]
			if s.Batch != batch {
				continue
			}
			switch s.Set {
			case tasks.SetPrep:
				lastPrep = max(lastPrep, i)
			case tasks.SetV:
				firstV = min(firstV, i)
			}
		}
		if lastPrep > firstV {
			t.Errorf("batch %d: a prep statement arrived after one of its V statements", batch)
		}
	}
	moved := 0 // pairs whose first statement was denied
	for _, u := range phases[6] {
		first, second := u[0].Tag, u[1].Tag
		if at[first] > at[second] {
			t.Errorf("pair %s: the second statement arrived first", u[0].Unit)
		}
		if conn[first] != conn[second] && (!denied(tasks.Tagged(first, "")) || firstOn[conn[second]] != second) {
			t.Errorf("pair %s: its statements came over connections %d and %d, and the second is not the first "+
				"statement of a connection that followed a denial", u[0].Unit, conn[first], conn[second])
		}
		if denied(tasks.Tagged(first, "")) {
			moved++
		}
	}
	if moved == 0 {
		t.Error("no pair's first statement was denied: the test does not see a pair cross a reconnection")
	}
}

func TestRateIsAtMostFivePerSecond(t *testing.T) {
	f := startFakeHoop(t)
	var stmts []tasks.Stmt
	for i := 1; i <= 16; i++ {
		stmts = append(stmts, testStmt(i, fmt.Sprintf("update contacts set notes = 'r' where id = %d", i)))
	}
	o := DefaultOptions("hoop")
	if o.PerSecond != 5 || o.Conns != 4 {
		t.Fatalf("DefaultOptions: %d connections, %v per second; want 4 and 5", o.Conns, o.PerSecond)
	}
	sendAll(t, Target{Addr: f.Addr, Database: "crm_fake", Names: freshNames()}, singles(stmts...), o)
	var at []time.Time
	for _, s := range f.Seen() {
		at = append(at, s.At)
	}
	if len(at) != 16 {
		t.Fatalf("the fake saw %d statements, want 16", len(at))
	}
	sort.Slice(at, func(i, j int) bool { return at[i].Before(at[j]) })
	for i := 0; i+5 < len(at); i++ {
		if span := at[i+5].Sub(at[i]); span < 950*time.Millisecond {
			t.Errorf("arrivals %d to %d span %v, want at least 950ms", i, i+5, span)
		}
	}
}

func TestSilentServerIsUnknown(t *testing.T) {
	f := startFakeHoop(t)
	f.Mode = "hang"
	o := fastOptions(1)
	o.Timeout = 300 * time.Millisecond
	a, b := testStmt(1, "update contacts set notes = 'a' where id = 1"), testStmt(2, "update contacts set notes = 'b' where id = 2")
	got := sendAll(t, Target{Addr: f.Addr, Database: "crm_fake", Names: freshNames()}, singles(a, b), o)
	for _, s := range []tasks.Stmt{a, b} {
		if x := got[s.ID]; x.Result != Unknown || x.Rows != nil || x.Message == "" {
			t.Errorf("%s: %+v, want unknown with a message", s.ID, x)
		}
	}
	if s := f.Seen(); len(s) != 2 || s[0].Conn == s[1].Conn {
		t.Errorf("seen %+v, want the second statement on a new connection", s)
	}
}

func TestDroppedAnswerIsUnknown(t *testing.T) {
	f := startFakeHoop(t)
	f.Mode = "drop"
	var ran atomic.Int32
	f.Exec = func(string) { ran.Add(1) }
	s := testStmt(1, "update contacts set notes = 'a' where id = 1")
	got := sendAll(t, Target{Addr: f.Addr, Database: "crm_fake", Names: freshNames()}, singles(s), fastOptions(1))
	if x := got[s.ID]; x.Result != Unknown || x.Message == "" {
		t.Errorf("%+v, want unknown with a message", x)
	}
	if n := ran.Load(); n != 1 {
		t.Errorf("Exec ran %d times, want 1: the statement reached the database, its answer did not come back", n)
	}
}

func TestUnreachableServerIsUnknown(t *testing.T) {
	window, every := connectWindow, connectEvery
	connectWindow, connectEvery = 300*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { connectWindow, connectEvery = window, every })

	f := startFakeHoop(t)
	addr := f.Addr
	f.ln.Close() // nothing listens there now

	s := testStmt(1, "update contacts set notes = 'a' where id = 1")
	start := time.Now()
	got := sendAll(t, Target{Addr: addr, Database: "crm_fake", Names: freshNames()}, singles(s), fastOptions(1))
	if x := got[s.ID]; x.Result != Unknown || !strings.HasPrefix(x.Message, "connect: ") {
		t.Errorf("%+v, want unknown with a message starting %q", x, "connect: ")
	}
	if took := time.Since(start); took < 250*time.Millisecond || took > 5*time.Second {
		t.Errorf("gave up after %v, want about the 300ms window", took)
	}
}

func TestSendThroughStet(t *testing.T) {
	addr, database, n := startStet(t)
	ctx := context.Background()
	one := testStmt(1, "update items set n = 1 where id = 1")
	five := testStmt(2, "update items set n = 2 where id <= 5")
	negative := testStmt(3, "update items set n = -1 where id = 2")
	got := sendAll(t, Target{Addr: addr, Database: database, Names: n}, singles(one, five, negative), fastOptions(1))

	if x := got[one.ID]; x.Result != Executed || x.Rows == nil || *x.Rows != 1 {
		t.Errorf("one row: %+v, want executed, rows 1", x)
	}
	if x := got[five.ID]; x.Result != Blocked || x.SQLState != "ST001" {
		t.Errorf("five rows: %+v, want blocked with ST001", x)
	}
	if x := got[negative.ID]; x.Result != DBRejected || x.SQLState != "23514" {
		t.Errorf("negative n: %+v, want db_rejected with 23514", x)
	}

	a := testAdmin(t)
	db := connectAs(t, a, a.User, a.Password, database)
	var first, twos, rest int
	if err := db.QueryRow(ctx, `select (select n from items where id = 1), (select count(*) from items where n = 2),
		(select count(*) from items where id > 1 and n = 10)`).Scan(&first, &twos, &rest); err != nil {
		t.Fatal(err)
	}
	if first != 1 || twos != 0 || rest != 9 {
		t.Errorf("row 1 holds %d, %d rows hold 2, %d other rows hold 10; want 1, 0, 9", first, twos, rest)
	}
	var recorded int
	if err := db.QueryRow(ctx, "select count(*) from stet.decisions where starts_with(statement, $1)",
		tasks.Tagged(five.Tag, "")).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 1 {
		t.Errorf("stet.decisions holds %d decisions on the refused write's tag, want 1", recorded)
	}

	ta, tn, _, _ := testTemplate(t)
	lab := connectAs(t, ta, tn.Owner(), OwnerPassword, testClone(t, false))
	for _, s := range []tasks.Stmt{one, five, negative} {
		x := got[s.ID]
		x.Arm = "stet"
		if err := SaveOutcome(ctx, lab, x); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := lab.Query(ctx, "select id, tag, set, result, rows, sqlstate, severity from lab.outcomes where arm = 'stet' order by id")
	if err != nil {
		t.Fatal(err)
	}
	var stored []string
	for rows.Next() {
		var id, tag, set, result, sqlstate, severity string
		var n *int64
		if err := rows.Scan(&id, &tag, &set, &result, &n, &sqlstate, &severity); err != nil {
			t.Fatal(err)
		}
		r := "-"
		if n != nil {
			r = fmt.Sprint(*n)
		}
		stored = append(stored, strings.Join([]string{id, tag, set, result, r, sqlstate, severity}, " "))
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	want := []string{
		"w-1 " + one.Tag + " V executed 1  ",
		"w-2 " + five.Tag + " V blocked - ST001 ERROR",
		"w-3 " + negative.Tag + " V db_rejected - 23514 ERROR",
	}
	if !slices.Equal(stored, want) {
		t.Errorf("lab.outcomes holds\n%q\nwant\n%q", stored, want)
	}
	for _, c := range []struct {
		arm, set string
		want     bool
	}{{"stet", tasks.SetV, true}, {"hoop", tasks.SetV, false}, {"stet", tasks.SetI, false}} {
		got, err := AlreadySent(ctx, lab, c.arm, c.set)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("AlreadySent(%s, %s) = %v, want %v", c.arm, c.set, got, c.want)
		}
	}
}

func TestArmPlan(t *testing.T) {
	for _, c := range []struct {
		arm        string
		practice   bool
		bind, part bool
		sets       string
	}{
		{"none", false, false, false, "V I X"}, {"stet", false, false, false, "V I X"}, {"hoop", false, false, false, "V I X"},
		{"hoop-cache", false, false, false, "V I"}, {"hoop-bind", false, true, true, "V I"}, {"stet-bind", false, true, true, "V I"},
		{"hoop-opus", false, false, true, "V I"},
		{"none", true, false, false, "V I"}, {"stet", true, false, false, "V I"}, {"hoop-a", true, false, false, "V I"},
		{"hoop-b", true, false, false, "V I"}, {"hoop-c", true, false, false, "V I"},
	} {
		bind, part, sets, err := ArmPlan(c.arm, c.practice)
		if err != nil {
			t.Errorf("ArmPlan(%s, %v): %v", c.arm, c.practice, err)
			continue
		}
		if bind != c.bind || part != c.part || strings.Join(sets, " ") != c.sets {
			t.Errorf("ArmPlan(%s, %v) = %v, %v, %v; want %v, %v, %s", c.arm, c.practice, bind, part, sets, c.bind, c.part, c.sets)
		}
	}
	for _, c := range []struct {
		arm      string
		practice bool
	}{{"hoop-a", false}, {"hoop-cache", true}, {"stet-bind", true}, {"other", false}} {
		if _, _, _, err := ArmPlan(c.arm, c.practice); err == nil {
			t.Errorf("ArmPlan(%s, %v) = nil error, want one", c.arm, c.practice)
		}
	}
	if o := DefaultOptions("stet-bind"); !o.Bind || o.Arm != "stet-bind" || o.Timeout != 120*time.Second || o.Sleep == nil {
		t.Errorf("DefaultOptions(stet-bind) = %+v, want Bind, the arm, 120s and a Sleep", o)
	}
}

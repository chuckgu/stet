package bench

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/lab/tasks"
)

// pauses notes what RunAttacks asks Sleep for, and takes no time.
type pauses struct {
	mu  sync.Mutex
	got []time.Duration
}

func (p *pauses) sleep(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, d)
}

func (p *pauses) all() []time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.got)
}

// xOptions are the options the X tests send with: no rate to speak of, a 30s
// timeout, the pad capped at 1 MiB, and sleep for the pauses.
func xOptions(arm string, sleep func(time.Duration)) Options {
	o := DefaultOptions(arm)
	o.PerSecond, o.Timeout, o.MaxPad, o.Sleep = 1e6, 30*time.Second, 1<<20, sleep
	return o
}

// xStmts are the X statements of b by ID.
func xStmts(b tasks.Bench) map[string]tasks.Stmt {
	m := map[string]tasks.Stmt{}
	for _, s := range b.Stream {
		if s.Set == tasks.SetX {
			m[s.ID] = s
		}
	}
	return m
}

// indexOf is the position of the first attack of path.
func indexOf(t *testing.T, attacks []tasks.Attack, path string) int {
	t.Helper()
	i := slices.IndexFunc(attacks, func(a tasks.Attack) bool { return a.Path == path })
	if i < 0 {
		t.Fatalf("no attack of path %s", path)
	}
	return i
}

// xRun is what one RunAttacks call reported.
type xRun struct {
	out     map[string]Outcome // by statement ID
	results []AttackResult
	next    int
	reclone bool
}

func runX(t *testing.T, target Target, owner *pgx.Conn, attacks []tasks.Attack, stmts map[string]tasks.Stmt, from int,
	o Options) xRun {
	t.Helper()
	r := xRun{out: map[string]Outcome{}}
	var err error
	r.next, r.reclone, err = RunAttacks(context.Background(), target, owner, attacks, stmts, from, o,
		func(x Outcome) error {
			if _, dup := r.out[x.ID]; dup {
				return fmt.Errorf("%s reported twice", x.ID)
			}
			r.out[x.ID] = x
			return nil
		},
		func(a AttackResult) error {
			r.results = append(r.results, a)
			return nil
		})
	if err != nil {
		t.Fatalf("RunAttacks: %v", err)
	}
	return r
}

func isGap(a tasks.Attack) bool { return strings.HasPrefix(a.Path, "gap.") && a.Path != "gap.prep" }

func TestAttacksExecuteWhenNothingStandsInFront(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	var p pauses
	got := runX(t, Target{Addr: a.Addr, Database: clone, Names: n}, owner, b.Attacks, stmts, 0, xOptions("none", p.sleep))

	last := indexOf(t, b.Attacks, "s4.range_delete")
	if !got.reclone || got.next != last {
		t.Fatalf("RunAttacks returned next %d, reclone %v; want %d (s4.range_delete), true", got.next, got.reclone, last)
	}
	if len(got.results) != last || got.results[last-1].Path != "s4.update_no_where" {
		t.Fatalf("%d attack results, want %d ending with s4.update_no_where", len(got.results), last)
	}
	gaps, s5 := 0, map[string]bool{}
	for i, r := range got.results {
		x := b.Attacks[i]
		if r.Index != i || r.Path != x.Path || r.Code != x.Code || r.Payload != x.Payload || r.Database != clone ||
			r.Arm != "none" || r.Undetermined {
			t.Errorf("attack %d: %+v does not describe %s %s %s in %s", i, r, x.Path, x.Code, x.Payload, clone)
		}
		for _, id := range x.Steps {
			if o, ok := got.out[id]; !ok || o.Set != tasks.SetX || o.Arm != "none" || o.Tag != stmts[id].Tag {
				t.Errorf("%s: step %s: outcome %+v, want one of arm none, set X, its tag", x.Path, id, o)
			}
		}
		rise := r.After - r.Before
		switch x.Path {
		case "gap.prep":
			if r.Executed || len(x.Steps) != 20 {
				t.Errorf("gap.prep: executed %v with %d steps, want not judged, 20 steps", r.Executed, len(x.Steps))
			}
			for _, id := range x.Steps {
				if o := got.out[id]; o.Result != Executed {
					t.Errorf("gap.prep: step %s %s %s", id, o.Result, o.Message)
				}
			}
		case "s4.truncate", "s4.drop_table", "s4.disable_trigger":
			o := got.out[x.Steps[0]]
			if r.Executed || o.Result != DBRejected || o.SQLState != "42501" {
				t.Errorf("%s: executed %v, step %s %s; want not executed, db_rejected 42501", x.Path, r.Executed, o.Result, o.SQLState)
			}
		default:
			if !r.Executed {
				var steps []string
				for _, id := range x.Steps {
					steps = append(steps, got.out[id].Result+" "+got.out[id].Message)
				}
				t.Errorf("%s (%d): not executed; the probe went from %d to %d; steps %q", x.Path, i, r.Before, r.After, steps)
			}
		}
		switch x.Path {
		case "s5.copy":
			if o := got.out[x.Steps[0]]; rise != 300 || o.Rows == nil || *o.Rows != 300 {
				t.Errorf("s5.copy: the probe rose by %d, the answer said %v rows; want 300", rise, o.Rows)
			}
		case "s8.burst":
			if rise != 200 {
				t.Errorf("s8.burst: the probe rose by %d, want 200", rise)
			}
		case "split":
			if rise != 600 {
				t.Errorf("split bundle %d: the probe rose by %d, want 600", i, rise)
			}
		case "s8.locked_row":
			if ms := got.out[x.Steps[0]].LatencyMS; ms < 1900 {
				t.Errorf("s8.locked_row: the step took %.0fms, want at least 1900 (it waits for the lock)", ms)
			}
		}
		if isGap(x) {
			gaps++
			if x.Payload == "F2" && r.Before != 0 {
				t.Errorf("%s F2 (%d): the probe read %d before, want 0 (its preparation ran)", x.Path, i, r.Before)
			}
		}
		if strings.HasPrefix(x.Path, "s5.") {
			s5[x.Path] = r.Executed
		}
	}
	if gaps != 80 {
		t.Errorf("%d gap attacks, want 80", gaps)
	}
	for _, path := range []string{"s5.function_call_message", "s5.copy", "s5.oversize_message", "s5.unknown_grammar"} {
		if !s5[path] {
			t.Errorf("%s did not execute", path)
		}
	}

	// A fresh clone, and on from where the first run stopped.
	clone2 := testClone(t, false)
	owner2 := connectAs(t, a, n.Owner(), OwnerPassword, clone2)
	more := runX(t, Target{Addr: a.Addr, Database: clone2, Names: n}, owner2, b.Attacks, stmts, got.next, xOptions("none", p.sleep))
	if more.reclone || more.next != len(b.Attacks) || len(more.results) != 1 {
		t.Fatalf("from %d: next %d, reclone %v, %d results; want %d, false, 1", got.next, more.next, more.reclone,
			len(more.results), len(b.Attacks))
	}
	if r := more.results[0]; r.Path != "s4.range_delete" || r.Index != last || !r.Executed || r.Database != clone2 {
		t.Errorf("from %d: %+v, want s4.range_delete executed in %s", got.next, r, clone2)
	}
}

func TestNothingRunsBehindAServerThatDeniesEverything(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	run := func(attacks []tasks.Attack, p *pauses) (xRun, *fakeHoop) {
		t.Helper()
		f := startFakeHoop(t)
		f.Deny = func(string) bool { return true }
		return runX(t, Target{Addr: f.Addr, Database: clone, Names: n}, owner, attacks, stmts, 0, xOptions("hoop", p.sleep)), f
	}

	var p pauses
	got, f := run(b.Attacks, &p)
	if got.reclone || got.next != len(b.Attacks) || len(got.results) != len(b.Attacks) {
		t.Fatalf("next %d, reclone %v, %d results; want %d, false, %d", got.next, got.reclone, len(got.results),
			len(b.Attacks), len(b.Attacks))
	}
	wires := map[string]int{}
	for i, r := range got.results {
		x := b.Attacks[i]
		if r.Executed || r.Undetermined || r.After != r.Before {
			t.Errorf("%s (%d): %+v, want not executed with the probe unchanged", x.Path, i, r)
		}
		for _, id := range x.Steps {
			if o := got.out[id]; o.Result != Blocked {
				t.Errorf("%s: step %s (%s): %s %s %s, want blocked", x.Path, id, stmts[id].Wire, o.Result, o.SQLState, o.Message)
			} else {
				wires[stmts[id].Wire]++
			}
		}
	}
	if wires[tasks.WireFunctionCall] != 1 || wires[tasks.WireCopy] != 1 || wires[tasks.WireBind] != 40 {
		t.Errorf("blocked steps by wire: %v; want the FunctionCall, the copy and the 40 bound ones among them", wires)
	}
	if want := slices.Repeat([]time.Duration{70 * time.Second}, 10); !slices.Equal(p.all(), want) {
		t.Errorf("Sleep was called with %v, want 70s ten times", p.all())
	}
	if m := f.MaxOpen(); m <= 4 || m > 32 {
		t.Errorf("%d connections were open at once, want more than 4 (the burst) and at most 32", m)
	}

	// What reached the fake: the FunctionCall by the function's OID and the
	// contact's id, and the padded texts in full.
	ctx := context.Background()
	var oid uint32
	if err := owner.QueryRow(ctx, "select 'set_consent(integer)'::regprocedure::oid").Scan(&oid); err != nil {
		t.Fatal(err)
	}
	fc := stmts[b.Attacks[indexOf(t, b.Attacks, "s5.function_call_message")].Steps[0]]
	padded := map[string]string{}
	for _, path := range []string{"s5.oversize_message", "s8.large_value"} {
		s := stmts[b.Attacks[indexOf(t, b.Attacks, path)].Steps[0]]
		padded[path] = tasks.Tagged(s.Tag, strings.Replace(s.Inline, "@PAD@", strings.Repeat("x", min(s.Pad, 1<<20)), 1))
	}
	calls := 0
	for _, s := range f.Seen() {
		switch {
		case s.Kind == "FunctionCall":
			calls++
			if s.SQL != fmt.Sprint(oid) || !slices.Equal(s.Params, fc.Args) || s.User != n.Role(tasks.RoleX) {
				t.Errorf("FunctionCall of %q with %q as %s, want %d with %q as %s", s.SQL, s.Params, s.User, oid, fc.Args,
					n.Role(tasks.RoleX))
			}
		case strings.Contains(s.SQL, "@PAD@"):
			t.Errorf("a text reached the fake with its @PAD@ token: %.80q", s.SQL)
		}
		for path, want := range padded {
			if tagOf(s.SQL) == tagOf(want) && s.SQL != want {
				t.Errorf("%s: the fake saw %d bytes, want %d with the pad written out", path, len(s.SQL), len(want))
			}
		}
	}
	if calls != 1 {
		t.Errorf("the fake saw %d FunctionCall messages, want 1", calls)
	}

	rest := slices.DeleteFunc(slices.Clone(b.Attacks), func(a tasks.Attack) bool { return a.Path == "s8.burst" })
	if _, f := run(rest, &pauses{}); f.MaxOpen() > 2 {
		t.Errorf("without s8.burst %d connections were open at once, want at most 2", f.MaxOpen())
	}
}

// execOn returns a fake's Exec that runs each statement on db as the owner.
// The fake calls it on its own goroutines, so the connection is closed under
// the same lock.
func execOn(t *testing.T, a Admin, n Names, db string) func(string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, OwnerURL(a.Addr, n, db))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		conn.Close(ctx)
	})
	return func(sql string) {
		mu.Lock()
		defer mu.Unlock()
		if conn.IsClosed() {
			return
		}
		if err := conn.PgConn().Exec(ctx, sql).Close(); err != nil {
			t.Errorf("run %.80q on the database: %v", sql, err)
		}
	}
}

func TestAttackIsJudgedByTheProbeNotTheAnswer(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	f := startFakeHoop(t)
	f.Mode = "drop"
	f.Exec = execOn(t, a, n, clone)

	attack := b.Attacks[indexOf(t, b.Attacks, "s5.do")]
	got := runX(t, Target{Addr: f.Addr, Database: clone, Names: n}, owner, []tasks.Attack{attack}, stmts, 0,
		xOptions("hoop", (&pauses{}).sleep))
	if o := got.out[attack.Steps[0]]; o.Result != Unknown {
		t.Errorf("s5.do's step: %s, want unknown (the fake hung up without answering)", o.Result)
	}
	if len(got.results) != 1 || !got.results[0].Executed {
		t.Errorf("s5.do: %+v, want executed: the probe saw the change", got.results)
	}
}

func TestSplitWaitsSeventySecondsBeforeEachBundle(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	f := startFakeHoop(t)

	var bundles []tasks.Attack
	for _, x := range b.Attacks {
		if x.Path == "split" {
			bundles = append(bundles, x)
		}
	}
	var (
		mu     sync.Mutex
		events []string
	)
	note := func(e string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}
	o := xOptions("hoop", func(d time.Duration) { note("sleep " + d.String()) })
	_, _, err := RunAttacks(context.Background(), Target{Addr: f.Addr, Database: clone, Names: n}, owner, bundles, stmts, 0, o,
		func(Outcome) error { note("step"); return nil }, func(AttackResult) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for range bundles {
		want = append(want, "sleep 1m10s")
		want = append(want, slices.Repeat([]string{"step"}, 12)...)
	}
	if len(bundles) != 10 || !slices.Equal(events, want) {
		t.Errorf("%d bundles; events %q, want a 70s sleep before each bundle's 12 steps, ten times", len(bundles), events)
	}
}

func TestPrepareThatIsDeniedLeavesExecuteToTheDatabase(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	attack := b.Attacks[indexOf(t, b.Attacks, "s5.prepare_execute")]
	if len(attack.Steps) != 2 {
		t.Fatalf("s5.prepare_execute has %d steps, want 2", len(attack.Steps))
	}
	prepare, execute := stmts[attack.Steps[0]], stmts[attack.Steps[1]]
	run := func(deny func(string) bool) (xRun, []seen) {
		t.Helper()
		f := startFakeHoop(t)
		f.Deny = deny
		got := runX(t, Target{Addr: f.Addr, Database: clone, Names: n}, owner, []tasks.Attack{attack}, stmts, 0,
			xOptions("hoop", (&pauses{}).sleep))
		return got, f.Seen()
	}

	// Allowed, both steps go over one session, in order.
	got, seen := run(nil)
	if len(seen) != 2 || tagOf(seen[0].SQL) != prepare.Tag || tagOf(seen[1].SQL) != execute.Tag || seen[0].Conn != seen[1].Conn {
		t.Errorf("allowed: seen %+v, want PREPARE then EXECUTE over one connection", seen)
	}
	if got.out[prepare.ID].Result != Executed || got.out[execute.ID].Result != Executed {
		t.Errorf("allowed: %+v", got.out)
	}

	got, seen = run(func(sql string) bool { return strings.Contains(sql, "prepare ") })
	if o := got.out[prepare.ID]; o.Result != Blocked {
		t.Errorf("denied PREPARE: %s, want blocked", o.Result)
	}
	if len(seen) != 2 || tagOf(seen[1].SQL) != execute.Tag || seen[0].Conn == seen[1].Conn {
		t.Errorf("denied PREPARE: seen %+v, want EXECUTE on a new connection", seen)
	}
	if _, ok := got.out[execute.ID]; !ok {
		t.Error("denied PREPARE: EXECUTE has no outcome")
	}
}

func TestAttackWithAFailingProbeIsUndetermined(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	ctx := context.Background()

	bad := b.Attacks[indexOf(t, b.Attacks, "s5.function")]
	bad.Probe = "select nope"
	good := b.Attacks[indexOf(t, b.Attacks, "s5.call")]
	o := xOptions("none", (&pauses{}).sleep)
	sent := map[string]Outcome{}
	next, reclone, err := RunAttacks(ctx, Target{Addr: a.Addr, Database: clone, Names: n}, owner, []tasks.Attack{bad, good},
		stmts, 0, o, func(x Outcome) error { sent[x.ID] = x; return nil },
		func(r AttackResult) error { return SaveAttack(ctx, owner, r) })
	if err != nil || next != 2 || reclone {
		t.Fatalf("RunAttacks: next %d, reclone %v, %v; want 2, false, nil", next, reclone, err)
	}
	if _, ok := sent[bad.Steps[0]]; !ok {
		t.Error("the step of the attack whose probe fails was not sent")
	}
	rows, err := owner.Query(ctx, "select idx, path, database, before, after, executed from lab.attacks where arm = 'none' order by idx")
	if err != nil {
		t.Fatal(err)
	}
	var stored []string
	for rows.Next() {
		var (
			idx           int
			path, db      string
			before, after *int64
			executed      bool
		)
		if err := rows.Scan(&idx, &path, &db, &before, &after, &executed); err != nil {
			t.Fatal(err)
		}
		show := func(v *int64) string {
			if v == nil {
				return "null"
			}
			return fmt.Sprint(*v)
		}
		stored = append(stored, fmt.Sprintf("%d %s %v %s %s %v", idx, path, db == clone, show(before), show(after), executed))
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if want := []string{"0 s5.function true null null false", "1 s5.call true 0 1 true"}; !slices.Equal(stored, want) {
		t.Errorf("lab.attacks holds %q, want %q", stored, want)
	}

	// With the owner's connection gone, nothing more is sent.
	gone := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	gone.Close(ctx)
	saved := 0
	_, _, err = RunAttacks(ctx, Target{Addr: a.Addr, Database: clone, Names: n}, gone, []tasks.Attack{good}, stmts, 0, o,
		func(Outcome) error { saved++; return nil }, func(AttackResult) error { saved++; return nil })
	if err == nil || saved != 0 {
		t.Errorf("a closed owner connection: err %v, %d saves; want an error and nothing sent", err, saved)
	}
}

// TestF2WithoutItsPreparationShowsInBefore pins down how lab.attacks tells a
// gap F2 attempt whose preparation did not run: its probe reads 1 before the
// attempt (the old title is still there), where it reads 0 when the
// preparation ran.
func TestF2WithoutItsPreparationShowsInBefore(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	f := startFakeHoop(t)
	f.Deny = func(sql string) bool { return strings.Contains(sql, "updated_at = now()") } // the preparation writes
	f.Exec = execOn(t, a, n, clone)

	var attacks []tasks.Attack
	for _, x := range b.Attacks {
		if x.Path == "gap.prep" || isGap(x) && x.Payload == "F2" {
			attacks = append(attacks, x)
		}
	}
	got := runX(t, Target{Addr: f.Addr, Database: clone, Names: n}, owner, attacks, stmts, 0, xOptions("hoop", (&pauses{}).sleep))
	if len(got.results) != 21 {
		t.Fatalf("%d results, want gap.prep and 20 F2 attempts", len(got.results))
	}
	for _, id := range attacks[0].Steps {
		if o := got.out[id]; o.Result != Blocked {
			t.Errorf("preparation %s: %s, want blocked", id, o.Result)
		}
	}
	for _, r := range got.results[1:] {
		if r.Before != 1 || r.After != 1 || r.Executed {
			t.Errorf("%s %s (%d): before %d, after %d, executed %v; want 1, 1, false", r.Path, r.Payload, r.Index, r.Before,
				r.After, r.Executed)
		}
	}
}

// TestBurstOpensItsConnectionsFirstAndIgnoresTheRate: the steps of an attack
// of one connection wait for the rate; those of the burst open their 32
// connections first and go at once.
func TestBurstOpensItsConnectionsFirstAndIgnoresTheRate(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	o := xOptions("hoop", (&pauses{}).sleep)
	o.PerSecond = 10

	span := func(path string) (time.Duration, *fakeHoop) {
		t.Helper()
		f := startFakeHoop(t)
		runX(t, Target{Addr: f.Addr, Database: clone, Names: n}, owner, []tasks.Attack{b.Attacks[indexOf(t, b.Attacks, path)]},
			stmts, 0, o)
		seen := f.Seen()
		if len(seen) == 0 {
			t.Fatalf("%s: the fake saw nothing", path)
		}
		first, last := seen[0].At, seen[0].At
		for _, s := range seen {
			if s.At.Before(first) {
				first = s.At
			}
			if s.At.After(last) {
				last = s.At
			}
		}
		return last.Sub(first), f
	}
	if d, _ := span("gap.prep"); d < 1800*time.Millisecond {
		t.Errorf("gap.prep's 20 steps came within %v, want at least 1.8s at 10 a second", d)
	}
	d, f := span("s8.burst")
	if d > 5*time.Second {
		t.Errorf("s8.burst's 200 steps took %v, want them at once (20s at the rate)", d)
	}
	if f.MaxOpen() != 32 || len(f.Seen()) != 200 {
		t.Errorf("s8.burst: %d connections open at once, %d statements; want 32 and 200", f.MaxOpen(), len(f.Seen()))
	}
}

func TestPadIsWrittenOutOnce(t *testing.T) {
	for _, c := range []struct {
		tag, sql string
		n        int
		want     string
	}{
		{"000000000001", "update t set v = '@PAD@' where id = 1", 3, "/* w:000000000001 */ update t set v = 'xxx' where id = 1"},
		{"", "select '@PAD@', '@PAD@'", 2, "select 'xx', 'xx'"},
		{"000000000002", "select 1", 4, "/* w:000000000002 */ select 1"},
	} {
		if got := padded(c.tag, c.sql, c.n); got != c.want {
			t.Errorf("padded(%q, %q, %d) = %q, want %q", c.tag, c.sql, c.n, got, c.want)
		}
	}
	if got := padded("", "'@PAD@'", 200000); len(got) != 200002 || strings.Count(got, "x") != 200000 {
		t.Errorf("padded with 200000: %d bytes, want 200002", len(got))
	}
}

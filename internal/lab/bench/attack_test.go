package bench

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	// The burst's address check may still count at the fake when its 32 connections open: the client hung up on it.
	if m := f.MaxOpen(); m <= 4 || m > 33 {
		t.Errorf("%d connections were open at once, want more than 4 (the burst) and at most 33 (32 and the address check)", m)
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

// TestDenialLostInTheRaceIsTheProxysByItsAuditRecord reproduces the race of a client still writing when hoop
// denies and hangs up at once. An Abrupt fake judges s5.oversize_message by its head and closes while the client
// is still writing the message: the client's write fails before it reads the denial, and the step is recorded
// unknown. collect joins the audit log's violation to it, lab.outcomes keeps what the client saw, and the X
// table gives the attack to the proxy's layer. s5.copy, whose data goes out behind its statement, is recorded
// blocked or unknown, whichever the client meets first; either way it is the proxy's.
func TestDenialLostInTheRaceIsTheProxysByItsAuditRecord(t *testing.T) {
	audit := t.TempDir() + "/audit.jsonl" // before the fake: its directory outlives the fake's last lines
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	lab := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	ctx := context.Background()
	f := startFakeHoop(t)
	f.Audit, f.Abrupt = audit, true
	f.Deny = func(string) bool { return true }

	var attacks []tasks.Attack
	for _, p := range []string{"s5.oversize_message", "s5.copy"} {
		attacks = append(attacks, b.Attacks[indexOf(t, b.Attacks, p)])
	}
	big, copied := attacks[0].Steps[0], attacks[1].Steps[0]
	o := xOptions("hoop", (&pauses{}).sleep)
	o.MaxPad = 24 << 20 // more than the sockets of both sides hold: the client is still writing when the fake closes
	_, _, err := RunAttacks(ctx, Target{Addr: f.Addr, Database: clone, Names: n}, owner, attacks, stmts, 0, o,
		func(x Outcome) error { return SaveOutcome(ctx, lab, x) },
		func(r AttackResult) error {
			r.Index = indexOf(t, b.Attacks, r.Path) // its place in the stream's attack list
			return SaveAttack(ctx, lab, r)
		})
	if err != nil {
		t.Fatal(err)
	}
	sent := results(t, lab)
	if got := sent["hoop/"+big]; got != Unknown {
		t.Fatalf("s5.oversize_message's step: %s, want unknown: its write failed before it read the denial", got)
	}
	if got := sent["hoop/"+copied]; got != Blocked && got != Unknown {
		t.Errorf("s5.copy's step: %s, want blocked or unknown", got)
	}

	events, err := ReadAudit(audit)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := CollectHoop(ctx, lab, "hoop", false, events, nil, false); err != nil {
		t.Fatal(err)
	}
	if got := results(t, lab); got["hoop/"+big] != Unknown || got["hoop/"+copied] != sent["hoop/"+copied] {
		t.Errorf("after collect: %v, want what the client saw kept: %v", got, sent)
	}
	r, err := LoadRun(ctx, lab, "hoop", 1)
	if err != nil {
		t.Fatal(err)
	}
	if p := r.Proxy[big]; !p.Blocked || p.Layer != "judgment" {
		t.Errorf("lab.proxy of s5.oversize_message's step: %+v, want blocked by judgment from the audit log's violation", p)
	}
	by := xByPath(r)
	for path, want := range map[string]string{"s5.oversize_message": "judgment", "s5.copy": "deterministic"} {
		if got := by[path]; got.Executed != 0 || !maps.Equal(got.Layers, map[string]int{want: 1}) {
			t.Errorf("%s: %+v, want not executed, stopped by %s", path, got, want)
		}
	}
	fates := attackFates(r)
	for _, x := range attacks {
		if f := fates[indexOf(t, b.Attacks, x.Path)]; f != fateProxy {
			t.Errorf("%s: fate %q, want the proxy's", x.Path, f)
		}
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
	// The attack's first connection, which only checks that the address
	// answers, may still count at the fake: the client hung up on it.
	if m := f.MaxOpen(); m < 32 || m > 33 || len(f.Seen()) != 200 {
		t.Errorf("s8.burst: %d connections open at once, %d statements; want 32 (33 with the address check) and 200",
			m, len(f.Seen()))
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

// execLeftRunning returns a fake's Exec that sends each statement to db as
// the owner, after pause seconds of pg_sleep in the same transaction, and
// returns once the database has started it, without waiting for it to end:
// the fake then hangs up on a client whose statement is still running.
func execLeftRunning(t *testing.T, a Admin, n Names, db string, pause float64) func(string) {
	t.Helper()
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	return func(sql string) {
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, OwnerURL(a.Addr, n, db))
		if err != nil {
			t.Errorf("connect to run %.80q: %v", sql, err)
			return
		}
		if pause > 0 {
			sql = fmt.Sprintf("select pg_sleep(%g); %s", pause, sql)
		}
		running := conn.PgConn().Exec(ctx, sql) // sent here, read below
		time.Sleep(50 * time.Millisecond)
		wg.Go(func() {
			if err := running.Close(); err != nil {
				t.Errorf("run %.80q on the database: %v", sql, err)
			}
			conn.Close(ctx)
		})
	}
}

func TestAfterProbeWaitsForAStatementTheArmLeftRunning(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	f := startFakeHoop(t)
	f.Mode = "drop"
	f.Exec = execLeftRunning(t, a, n, clone, 0.5)

	attack := b.Attacks[indexOf(t, b.Attacks, "s5.do")]
	got := runX(t, Target{Addr: f.Addr, Database: clone, Names: n}, owner, []tasks.Attack{attack}, stmts, 0,
		xOptions("hoop", (&pauses{}).sleep))
	if o := got.out[attack.Steps[0]]; o.Result != Unknown {
		t.Errorf("s5.do's step: %s, want unknown", o.Result)
	}
	if r := got.results[0]; !r.Executed || r.Undetermined {
		t.Errorf("s5.do: %+v, want executed: the statement committed half a second after the fake hung up", r)
	}
}

func TestLockedRowWhoseAnswerIsLostIsJudgedAfterItCommits(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	f := startFakeHoop(t)
	f.Mode = "drop"
	f.Exec = execLeftRunning(t, a, n, clone, 0) // the write waits for the lock after the fake has hung up

	attack := b.Attacks[indexOf(t, b.Attacks, "s8.locked_row")]
	got := runX(t, Target{Addr: f.Addr, Database: clone, Names: n}, owner, []tasks.Attack{attack}, stmts, 0,
		xOptions("hoop", (&pauses{}).sleep))
	if o := got.out[attack.Steps[0]]; o.Result != Unknown || o.LatencyMS > 1000 {
		t.Errorf("s8.locked_row's step: %s after %.0fms, want unknown at once", o.Result, o.LatencyMS)
	}
	if r := got.results[0]; !r.Executed || r.Undetermined {
		t.Errorf("s8.locked_row: %+v, want executed: the write ran once the lock was released", r)
	}
}

func TestDatabaseThatNeverSettlesIsUndetermined(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	ctx := context.Background()
	busy := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	tx, err := busy.Begin(ctx) // a session that stays inside a transaction
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "select 1"); err != nil {
		t.Fatal(err)
	}

	attack := b.Attacks[indexOf(t, b.Attacks, "s5.call")]
	o := xOptions("none", (&pauses{}).sleep)
	o.Timeout = 500 * time.Millisecond
	start := time.Now()
	got := runX(t, Target{Addr: a.Addr, Database: clone, Names: n}, owner, []tasks.Attack{attack}, stmts, 0, o)
	if r := got.results[0]; !r.Undetermined || r.Executed {
		t.Errorf("s5.call beside a session that never settles: %+v, want undetermined", r)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("gave up after %v, want about the 500ms timeout", took)
	}
}

func TestBurstWhoseSaveFailsKeepsInFlightStepsOutOfUnknown(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	burst := b.Attacks[indexOf(t, b.Attacks, "s8.burst")]
	denied := stmts[burst.Steps[0]]

	// The failing save is the denied step's: a denial never reaches Exec,
	// which the fake runs one at a time, so the other connections' steps
	// wait behind the slow one, in flight.
	f := startFakeHoop(t)
	f.Deny = func(sql string) bool { return tagOf(sql) == denied.Tag }
	inflight := make(chan struct{})
	var once sync.Once
	f.Exec = func(string) {
		once.Do(func() {
			close(inflight)
			time.Sleep(300 * time.Millisecond)
		})
	}
	full := errors.New("disk full")
	var got []Outcome
	save := func(o Outcome) error {
		got = append(got, o)
		if o.ID != denied.ID {
			return nil
		}
		select {
		case <-inflight:
		case <-time.After(5 * time.Second):
		}
		return full
	}
	_, _, err := RunAttacks(context.Background(), Target{Addr: f.Addr, Database: clone, Names: n}, owner,
		[]tasks.Attack{burst}, stmts, 0, xOptions("hoop", (&pauses{}).sleep), save, func(AttackResult) error { return nil })
	if !errors.Is(err, full) {
		t.Errorf("RunAttacks = %v, want the save's error", err)
	}
	for _, o := range got {
		if o.Result == Unknown {
			t.Errorf("%s recorded as unknown (%s): the far side answered it", o.ID, o.Message)
		}
	}
	if seen := len(f.Seen()); seen != len(got) || seen >= len(burst.Steps) {
		t.Errorf("the fake saw %d statements, %d were recorded; want every one sent recorded, and fewer than %d",
			seen, len(got), len(burst.Steps))
	}
}

func TestResumeGoesOnExactlyWhereTheResultsStop(t *testing.T) {
	a, n, _, b := testTemplate(t)
	ctx := context.Background()
	conn := connectAs(t, a, n.Owner(), OwnerPassword, testClone(t, false))
	at := b.Attacks
	result := func(arm string, i int) {
		t.Helper()
		if err := SaveAttack(ctx, conn, AttackResult{Arm: arm, Index: i, Path: at[i].Path, Code: at[i].Code,
			Payload: at[i].Payload, Database: "crm_x"}); err != nil {
			t.Fatal(err)
		}
	}
	outcome := func(arm, id string) {
		t.Helper()
		if err := SaveOutcome(ctx, conn, Outcome{Arm: arm, ID: id, Set: tasks.SetX, SentAt: time.Now(), Result: Executed}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(arm string, from int, want ...string) {
		t.Helper()
		err := CheckResume(ctx, conn, arm, at, from)
		switch {
		case len(want) == 0 && err != nil:
			t.Errorf("%s -from %d: %v, want no error", arm, from, err)
		case len(want) > 0 && err == nil:
			t.Errorf("%s -from %d: no error, want one saying %q", arm, from, want)
		case len(want) > 0:
			for _, w := range want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%s -from %d: %q does not say %q", arm, from, err, w)
				}
			}
		}
	}

	check("a", 0)
	check("a", 3, "-from 0")
	for i := range 3 {
		result("a", i)
		for _, id := range at[i].Steps {
			outcome("a", id)
		}
	}
	check("a", 3)
	check("a", 2, "-from 3")
	check("a", 4, "-from 3")
	check("b", 0) // another arm's rows are not this arm's

	// Attack 3 was cut off: one of its steps is recorded, its result is not.
	outcome("a", at[3].Steps[0])
	check("a", 3, "attack 3", at[3].Steps[0], "re-clone")
	check("a", 4, "-from 3")

	// A gap: attack 1 has no result, attack 2 has one.
	result("c", 0)
	result("c", 2)
	check("c", 3, "attack 1", "-from 1")
}

func TestUndeterminedDestructiveAttackStopsForAFreshClone(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	truncate := b.Attacks[indexOf(t, b.Attacks, "s4.truncate")]
	truncate.Probe = "select nope"
	call := b.Attacks[indexOf(t, b.Attacks, "s5.call")]
	target := Target{Addr: a.Addr, Database: clone, Names: n}

	got := runX(t, target, owner, []tasks.Attack{truncate, call}, stmts, 0, xOptions("none", (&pauses{}).sleep))
	if !got.reclone || got.next != 1 || len(got.results) != 1 || !got.results[0].Undetermined {
		t.Errorf("an undetermined s4.truncate before s5.call: next %d, reclone %v, results %+v; want 1, true, "+
			"one undetermined", got.next, got.reclone, got.results)
	}
	last := runX(t, target, owner, []tasks.Attack{call, truncate}, stmts, 0, xOptions("none", (&pauses{}).sleep))
	if last.reclone || last.next != 2 {
		t.Errorf("an undetermined s4.truncate last: next %d, reclone %v; want 2, false", last.next, last.reclone)
	}
}

func TestAttacksToADeadAddressRecordNothing(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	window, every := connectWindow, connectEvery
	connectWindow, connectEvery = 300*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { connectWindow, connectEvery = window, every })
	f := startFakeHoop(t)
	dead := f.Addr
	f.ln.Close() // nothing listens there now

	saved := 0
	start := time.Now()
	next, _, err := RunAttacks(context.Background(), Target{Addr: dead, Database: clone, Names: n}, owner, b.Attacks[:3], stmts, 0,
		xOptions("hoop", (&pauses{}).sleep), func(Outcome) error { saved++; return nil },
		func(AttackResult) error { saved++; return nil })
	if err == nil || !strings.Contains(err.Error(), dead) || next != 0 || saved != 0 {
		t.Errorf("attacks to a dead address: next %d, %d saves, err %v; want 0, 0 and an error naming %s", next, saved, err, dead)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %v to find the address dead; want one short attempt, not the connect window", took)
	}
}

// shortConnect shortens the connect window for the test.
func shortConnect(t *testing.T) {
	t.Helper()
	window, every := connectWindow, connectEvery
	connectWindow, connectEvery = 300*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { connectWindow, connectEvery = window, every })
}

// labIDs returns the first column of query over the arm's rows, as text, in order.
func labIDs(t *testing.T, conn *pgx.Conn, query string) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestAttacksStopWhenTheArmGoesAway: the arm's proxy is gone after attack 0. Attack 1 is neither sent nor
// recorded as not executed: the run stops at it with an error, lab.attacks holds attack 0 only, and it goes on
// from attack 1 once a proxy answers again.
func TestAttacksStopWhenTheArmGoesAway(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	lab := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	ctx := context.Background()
	shortConnect(t)
	attacks := b.Attacks[:6]
	o := xOptions("hoop", (&pauses{}).sleep)
	save := func(x Outcome) error { return SaveOutcome(ctx, lab, x) }

	f := startFakeHoop(t)
	next, _, err := RunAttacks(ctx, Target{Addr: f.Addr, Database: clone, Names: n}, owner, attacks, stmts, 0, o, save,
		func(r AttackResult) error {
			if err := SaveAttack(ctx, lab, r); err != nil {
				return err
			}
			if r.Index == 0 {
				f.close() // the proxy dies
			}
			return nil
		})
	if err == nil || next != 1 || !strings.Contains(err.Error(), "attack 1 ("+attacks[1].Path+")") ||
		!strings.Contains(err.Error(), f.Addr) {
		t.Fatalf("RunAttacks with the proxy gone after attack 0: next %d, err %v; want 1 and an error naming attack 1 and %s",
			next, err, f.Addr)
	}
	if got := labIDs(t, lab, "select idx::text from lab.attacks where arm = 'hoop' order by idx"); !slices.Equal(got, []string{"0"}) {
		t.Errorf("lab.attacks holds attacks %q, want only 0: attack 1 and later were never sent", got)
	}
	want := slices.Sorted(slices.Values(attacks[0].Steps))
	if got := labIDs(t, lab, "select id from lab.outcomes where arm = 'hoop' order by id"); !slices.Equal(got, want) {
		t.Errorf("lab.outcomes holds %q, want only attack 0's steps %q", got, want)
	}

	// The guard lets the run go on from attack 1, and it does once a proxy answers.
	if err := CheckResume(ctx, lab, "hoop", attacks, 1); err != nil {
		t.Fatalf("CheckResume -from 1: %v", err)
	}
	g := startFakeHoop(t)
	next, _, err = RunAttacks(ctx, Target{Addr: g.Addr, Database: clone, Names: n}, owner, attacks, stmts, 1, o, save,
		func(r AttackResult) error { return SaveAttack(ctx, lab, r) })
	if err != nil || next != len(attacks) {
		t.Fatalf("resumed from 1: next %d, err %v; want %d, nil", next, err, len(attacks))
	}
	if got := labIDs(t, lab, "select idx::text from lab.attacks where arm = 'hoop' order by idx"); len(got) != len(attacks) {
		t.Errorf("after the resume lab.attacks holds %q, want all %d attacks", got, len(attacks))
	}
}

// TestStepThatCannotConnectStopsItsAttack: s5.prepare_execute's PREPARE is denied, and the proxy is gone before
// EXECUTE reconnects. The attack is cut there: no outcome for EXECUTE, no result for the attack, and the resume
// guard names the step that was recorded.
func TestStepThatCannotConnectStopsItsAttack(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	lab := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	ctx := context.Background()
	shortConnect(t)
	attack := b.Attacks[indexOf(t, b.Attacks, "s5.prepare_execute")]
	prepare, execute := attack.Steps[0], attack.Steps[1]

	f := startFakeHoop(t)
	f.Deny = func(sql string) bool { return strings.Contains(sql, "prepare ") }
	var saved []string
	results := 0
	next, _, err := RunAttacks(ctx, Target{Addr: f.Addr, Database: clone, Names: n}, owner, []tasks.Attack{attack}, stmts, 0,
		xOptions("hoop", (&pauses{}).sleep),
		func(x Outcome) error {
			saved = append(saved, x.ID)
			if x.ID == prepare {
				f.ln.Close() // the proxy stops taking connections after the denial
			}
			return SaveOutcome(ctx, lab, x)
		},
		func(AttackResult) error { results++; return nil })
	if err == nil || next != 0 || !strings.Contains(err.Error(), execute) || !strings.Contains(err.Error(), "connect: ") {
		t.Errorf("RunAttacks: next %d, err %v; want 0 and an error naming step %s and its failed connect", next, err, execute)
	}
	if !slices.Equal(saved, []string{prepare}) || results != 0 {
		t.Errorf("saved outcomes %q and %d results; want only %s's outcome and no result", saved, results, prepare)
	}
	if err := CheckResume(ctx, lab, "hoop", []tasks.Attack{attack}, 0); err == nil || !strings.Contains(err.Error(), "cut off") ||
		!strings.Contains(err.Error(), prepare) {
		t.Errorf("CheckResume -from 0: %v, want the cut-off attack named with its recorded step %s", err, prepare)
	}
}

// TestBurstStopsOnlyWhenNoStepCouldConnect: a worker of the burst that cannot reconnect is part of the attack,
// and its steps are recorded as unknown; when not one step could connect, the attack is cut and not recorded.
func TestBurstStopsOnlyWhenNoStepCouldConnect(t *testing.T) {
	a, n, _, b := testTemplate(t)
	stmts := xStmts(b)
	clone := testClone(t, false)
	owner := connectAs(t, a, n.Owner(), OwnerPassword, clone)
	shortConnect(t)
	burst := b.Attacks[indexOf(t, b.Attacks, "s8.burst")]
	for _, c := range []struct {
		name     string
		accepted int // connections let in: the address check's, then the burst's
		cut      bool
	}{
		{"no step connects", 1, true},
		{"half the workers connect", 17, false},
	} {
		f := startFakeHoop(t)
		f.Refuse = func(conn int) bool { return conn > c.accepted }
		got := map[string]int{}
		results := 0
		_, _, err := RunAttacks(context.Background(), Target{Addr: f.Addr, Database: clone, Names: n}, owner,
			[]tasks.Attack{burst}, stmts, 0, xOptions("hoop", (&pauses{}).sleep),
			func(x Outcome) error {
				k := x.Result
				if strings.HasPrefix(x.Message, "connect: ") {
					k = "connect failed"
				}
				got[k]++
				return nil
			},
			func(AttackResult) error { results++; return nil })
		switch {
		case c.cut && (err == nil || !strings.Contains(err.Error(), "s8.burst") || results != 0):
			t.Errorf("%s: err %v, %d results; want an error naming s8.burst and no result", c.name, err, results)
		case !c.cut && (err != nil || results != 1):
			t.Errorf("%s: err %v, %d results; want the attack recorded", c.name, err, results)
		}
		if c.cut && got["connect failed"] != len(burst.Steps) {
			t.Errorf("%s: outcomes %v, want all %d steps failed to connect", c.name, got, len(burst.Steps))
		}
		if !c.cut && (got["connect failed"] == 0 || got[Executed] == 0 || got["connect failed"]+got[Executed] != len(burst.Steps)) {
			t.Errorf("%s: outcomes %v, want some executed and some failed to connect, %d in all", c.name, got, len(burst.Steps))
		}
	}
}

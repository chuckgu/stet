package gate

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/store"
)

// usageBase is a whole second, so offsets from it name their seconds.
var usageBase = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func at(offset time.Duration) time.Time { return usageBase.Add(offset) }

// loadCall is one call of a usage's load function.
type loadCall struct{ since, before time.Time }

// fakeLoad answers loads with secs, or with err while err is set, and keeps
// every call.
type fakeLoad struct {
	secs  []store.UsageSecond
	err   error
	calls []loadCall
}

func (f *fakeLoad) load(_ context.Context, _, _ string, since, before time.Time) ([]store.UsageSecond, error) {
	f.calls = append(f.calls, loadCall{since, before})
	if f.err != nil {
		return nil, f.err
	}
	return f.secs, nil
}

// testUsage is a usage over f that started at start.
func testUsage(f *fakeLoad, start time.Time) *usage {
	u := newUsage(f.load)
	u.start = start
	return u
}

// mustReserve reserves for role agent in table t and returns what was used.
func mustReserve(t *testing.T, u *usage, id string, when time.Time, planned int64, window time.Duration) int64 {
	t.Helper()
	used, err := u.reserve(context.Background(), "agent", "t", id, when, planned, window)
	if err != nil {
		t.Fatalf("reserve %s: %v", id, err)
	}
	return used
}

// seconds is the pair's rows by second, offset from usageBase.
func (u *usage) seconds(role, table string) map[time.Duration]int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := map[time.Duration]int64{}
	if p := u.pairs[usageKey{role, table}]; p != nil {
		for _, s := range p.secs {
			out[time.Unix(s.sec, 0).Sub(usageBase)] = s.rows
		}
	}
	return out
}

func TestUsageCountsPerSecond(t *testing.T) {
	u := testUsage(&fakeLoad{}, usageBase)
	const window = time.Hour
	n := func(v int64) *int64 { return &v }

	if used := mustReserve(t, u, "a", at(0), 2, window); used != 0 {
		t.Errorf("first write: used %d, want 0", used)
	}
	// A reservation counts from the next write on, not for its own.
	if used := mustReserve(t, u, "b", at(100*time.Millisecond), 3, window); used != 2 {
		t.Errorf("second write: used %d, want 2", used)
	}
	u.settle("agent", "t", "a", n(1), true, false) // ran: its count replaces the reservation
	u.settle("agent", "t", "b", nil, false, false) // the server refused it: dropped
	if used := mustReserve(t, u, "c", at(200*time.Millisecond), 4, window); used != 1 {
		t.Errorf("third write: used %d, want 1", used)
	}
	u.settle("agent", "t", "c", nil, false, true) // lost with no completion: may have committed, kept

	if used := mustReserve(t, u, "d", at(1500*time.Millisecond), 5, window); used != 5 {
		t.Errorf("fourth write: used %d, want 5", used)
	}
	u.settle("agent", "t", "d", n(2), true, true) // lost after its completion: its count is known
	if used := mustReserve(t, u, "e", at(2*time.Second), 7, window); used != 7 {
		t.Errorf("fifth write: used %d, want 7", used)
	}
	u.settle("agent", "t", "e", nil, true, false) // ran, no count: the reservation stays
	if used := mustReserve(t, u, "f", at(2500*time.Millisecond), 0, window); used != 14 {
		t.Errorf("sixth write: used %d, want 14", used)
	}

	want := map[time.Duration]int64{0: 5, time.Second: 2, 2 * time.Second: 7}
	if got := u.seconds("agent", "t"); !reflect.DeepEqual(got, want) {
		t.Errorf("seconds = %v, want %v", got, want)
	}
	// Settling twice, or a write never reserved, changes nothing.
	u.settle("agent", "t", "a", n(100), true, false)
	u.settle("agent", "t", "zz", n(100), true, false)
	u.settle("other", "t", "a", n(100), true, false)
	if got := u.seconds("agent", "t"); !reflect.DeepEqual(got, want) {
		t.Errorf("after stray settles: seconds = %v, want %v", got, want)
	}
}

func TestUsageKeepsASecondUntilItLeftTheWindow(t *testing.T) {
	u := testUsage(&fakeLoad{}, usageBase)
	const window = 2 * time.Second
	mustReserve(t, u, "early", at(900*time.Millisecond), 10, window)

	// The window at 2.5s starts at 0.5s, inside the early write's second.
	if used := mustReserve(t, u, "p1", at(2500*time.Millisecond), 0, window); used != 10 {
		t.Errorf("at 2.5s: used %d, want 10", used)
	}
	if used := mustReserve(t, u, "p2", at(2999*time.Millisecond), 0, window); used != 10 {
		t.Errorf("at 2.999s: used %d, want 10", used)
	}
	// At 3.0s the window starts at 1.0s: all of second 0 has left it.
	if used := mustReserve(t, u, "p3", at(3*time.Second), 0, window); used != 0 {
		t.Errorf("at 3.0s: used %d, want 0", used)
	}
	// A write that settles after its second left the window changes nothing.
	u.settle("agent", "t", "early", nil, false, false)
	if used := mustReserve(t, u, "p4", at(3100*time.Millisecond), 0, window); used != 0 {
		t.Errorf("after a late settle: used %d, want 0", used)
	}
	// Nor does a reservation whose second already left the window as a later
	// write saw it: it began long before it reserved.
	mustReserve(t, u, "late", at(900*time.Millisecond), 10, window)
	if used := mustReserve(t, u, "p5", at(3200*time.Millisecond), 0, window); used != 0 {
		t.Errorf("after a late reservation: used %d, want 0", used)
	}
}

func TestUsageLoadsOnlyWhatCameBeforeItStarted(t *testing.T) {
	f := &fakeLoad{secs: []store.UsageSecond{
		{Second: at(-30 * time.Second), Rows: 4},
		{Second: at(-10 * time.Second), Rows: 2},
	}}
	start := at(500 * time.Millisecond)
	u := testUsage(f, start)
	const window = time.Minute

	// The window of a write at 1.5s starts at -58.5s, in second -59.
	if used := mustReserve(t, u, "a", at(1500*time.Millisecond), 1, window); used != 6 {
		t.Errorf("first write: used %d, want the 6 loaded rows", used)
	}
	if used := mustReserve(t, u, "b", at(2*time.Second), 1, window); used != 7 {
		t.Errorf("second write: used %d, want 7", used)
	}
	if want := []loadCall{{at(-59 * time.Second), start}}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("loads = %v, want one over [window start, gate start) = %v", f.calls, want)
	}
	// Loaded seconds leave the window as any other: second -30 has left it
	// once the window starts at -29s.
	if used := mustReserve(t, u, "c", at(31*time.Second), 0, window); used != 4 {
		t.Errorf("at 31s: used %d, want 4", used)
	}
}

func TestUsageRetriesAfterAnInterval(t *testing.T) {
	defer func(d time.Duration) { usageRetry = d }(usageRetry)
	usageRetry = 5 * time.Second
	f := &fakeLoad{err: errors.New("down")}
	u := testUsage(f, usageBase)
	const window = time.Hour
	reserve := func(id string, when time.Time) (int64, error) {
		return u.reserve(context.Background(), "agent", "t", id, when, 1, window)
	}

	if _, err := reserve("a", at(time.Second)); err == nil || err.Error() != "down" {
		t.Fatalf("first write: err %v, want the failed load's", err)
	}
	// Within the interval no load is tried; the failure is the answer.
	used, err := reserve("b", at(5900*time.Millisecond))
	if err == nil || err.Error() != "down" {
		t.Errorf("within the interval: err %v, want the failed load's", err)
	}
	if used != 1 {
		t.Errorf("within the interval: used %d, want 1: the first write is reserved", used)
	}
	if len(f.calls) != 1 {
		t.Errorf("%d loads within the interval, want 1", len(f.calls))
	}

	f.err, f.secs = nil, []store.UsageSecond{{Second: at(-10 * time.Second), Rows: 3}}
	used, err = reserve("c", at(6*time.Second))
	if err != nil {
		t.Fatalf("after the interval: %v", err)
	}
	if len(f.calls) != 2 {
		t.Errorf("%d loads after the interval, want 2", len(f.calls))
	}
	if used != 5 {
		t.Errorf("after the interval: used %d, want 5: three loaded rows and the two writes reserved meanwhile", used)
	}
}

// A write whose time to read state is already up does not start a load, and
// its want of one is not a failure that makes others wait.
func TestUsageDoesNotLoadOnADoneContext(t *testing.T) {
	f := &fakeLoad{secs: []store.UsageSecond{{Second: at(-10 * time.Second), Rows: 3}}}
	u := testUsage(f, usageBase)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := u.reserve(ctx, "agent", "t", "a", at(time.Second), 1, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err %v, want the context's", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("%d loads on a done context, want 0", len(f.calls))
	}
	if used := mustReserve(t, u, "b", at(1100*time.Millisecond), 1, time.Hour); used != 4 {
		t.Errorf("next write: used %d, want 4: it loads at once", used)
	}
}

func TestUsageDropsIdlePairs(t *testing.T) {
	u := testUsage(&fakeLoad{}, usageBase)
	const window = 10 * time.Second
	reserve := func(role, id string, when time.Time) {
		t.Helper()
		if _, err := u.reserve(context.Background(), role, "t", id, when, 1, window); err != nil {
			t.Fatal(err)
		}
	}
	has := func(role string) bool {
		u.mu.Lock()
		defer u.mu.Unlock()
		_, ok := u.pairs[usageKey{role, "t"}]
		return ok
	}

	reserve("idle", "a", at(0))
	u.settle("idle", "t", "a", nil, true, false)
	reserve("busy", "b", at(0)) // not settled: still running
	reserve("recent", "c", at(55*time.Second))
	u.settle("recent", "t", "c", nil, true, false)

	reserve("other", "d", at(30*time.Second)) // within a minute of the last sweep
	if !has("idle") {
		t.Error("the idle pair was dropped before a minute passed")
	}
	reserve("other", "e", at(61*time.Second))
	if has("idle") {
		t.Error("the idle pair is kept after its last write left the window")
	}
	if !has("busy") || !has("recent") || !has("other") {
		t.Errorf("busy %v recent %v other %v, want all kept: one has a write running, two have writes in the window",
			has("busy"), has("recent"), has("other"))
	}

	u.settle("busy", "t", "b", nil, true, false)
	reserve("other", "f", at(122*time.Second))
	if has("busy") || has("recent") {
		t.Errorf("busy %v recent %v, want both dropped once idle", has("busy"), has("recent"))
	}
}

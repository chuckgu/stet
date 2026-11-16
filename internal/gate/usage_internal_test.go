package gate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/store"
)

// usageBase is a whole second, so offsets from it name their seconds.
var usageBase = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func at(offset time.Duration) time.Time { return usageBase.Add(offset) }

// loadCall is one call of a usage's load function.
type loadCall struct{ since, before time.Time }

// fakeLoad answers loads with secs, or with err while err is set, and keeps
// every call. While block is set, a load first waits for it to be closed or
// for its context to end.
type fakeLoad struct {
	mu    sync.Mutex
	secs  []store.UsageSecond
	err   error
	calls []loadCall
	block chan struct{}
}

func (f *fakeLoad) load(ctx context.Context, _, _ string, since, before time.Time) ([]store.UsageSecond, error) {
	f.mu.Lock()
	f.calls = append(f.calls, loadCall{since, before})
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.secs, nil
}

// answer makes later loads return secs and err.
func (f *fakeLoad) answer(secs []store.UsageSecond, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secs, f.err = secs, err
}

func (f *fakeLoad) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// waitLoaded waits until the pair of role agent in table t has loaded.
func (u *usage) waitLoaded(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		u.mu.Lock()
		p := u.pairs[usageKey{"agent", "t"}]
		loaded := p != nil && p.loaded
		u.mu.Unlock()
		if loaded {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the pair never loaded")
		}
	}
}

// testUsage is a usage over f that started at start. Its loads are waited
// for when the test ends.
func testUsage(t *testing.T, f *fakeLoad, start time.Time) *usage {
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	u := newUsage(f.load, &wg)
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
	u := testUsage(t, &fakeLoad{}, usageBase)
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
	u := testUsage(t, &fakeLoad{}, usageBase)
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
	u := testUsage(t, f, start)
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
	u := testUsage(t, f, usageBase)
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
	u := testUsage(t, f, usageBase)
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
	u := testUsage(t, &fakeLoad{}, usageBase)
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

// Seconds reserved while the pair could not load pile up; the load that
// finally succeeds merges its seconds with them, where they interleave and
// where they fall in the same second, and skips what already left the window.
func TestUsageMergesLoadedAndReservedSeconds(t *testing.T) {
	defer func(d time.Duration) { usageRetry = d }(usageRetry)
	usageRetry = time.Hour
	f := &fakeLoad{err: errors.New("down")}
	u := testUsage(t, f, usageBase)
	const window = time.Hour
	for _, s := range []int{2, 5, 9} {
		u.reserve(context.Background(), "agent", "t", fmt.Sprint(s), at(time.Duration(s)*time.Second), 1, window)
	}
	f.answer([]store.UsageSecond{
		{Second: at(-4000 * time.Second), Rows: 1000}, // before every window seen: skipped
		{Second: at(1 * time.Second), Rows: 10},
		{Second: at(2 * time.Second), Rows: 20},
		{Second: at(6 * time.Second), Rows: 30},
		{Second: at(9 * time.Second), Rows: 40},
		{Second: at(12 * time.Second), Rows: 50},
	}, nil)
	usageRetry = 0
	if used := mustReserve(t, u, "last", at(13*time.Second), 1, window); used != 153 {
		t.Errorf("used %d, want 153: 150 loaded rows and 3 reserved", used)
	}
	want := map[time.Duration]int64{
		1 * time.Second: 10, 2 * time.Second: 21, 5 * time.Second: 1, 6 * time.Second: 30,
		9 * time.Second: 41, 12 * time.Second: 50, 13 * time.Second: 1,
	}
	if got := u.seconds("agent", "t"); !reflect.DeepEqual(got, want) {
		t.Errorf("seconds = %v, want %v", got, want)
	}
	u.mu.Lock()
	secs := u.pairs[usageKey{"agent", "t"}].secs
	u.mu.Unlock()
	for i := 1; i < len(secs); i++ {
		if secs[i-1].sec >= secs[i].sec {
			t.Fatalf("seconds out of order at %d: %v", i, secs)
		}
	}
}

// The merge runs under the lock every pair shares, so it must be one pass:
// a full day of loaded seconds against an hour of reserved ones, reserved
// through a run of failed loads. One pass takes under a millisecond (about
// 5ms with -race); one insert per loaded second took over 100ms. The fastest
// of three trials is timed, so a busy machine does not fail the test.
func TestUsageMergesALoadInOnePass(t *testing.T) {
	defer func(d time.Duration) { usageRetry = d }(usageRetry)
	const window = 24 * time.Hour
	const reserved = 3600
	last := at(reserved * time.Second)
	var secs []store.UsageSecond
	for s := last.Add(-window); s.Before(usageBase); s = s.Add(time.Second) {
		secs = append(secs, store.UsageSecond{Second: s, Rows: 1})
	}

	fastest := time.Hour
	for range 3 {
		usageRetry = 24 * time.Hour
		f := &fakeLoad{err: errors.New("down")}
		u := testUsage(t, f, usageBase)
		for i := range reserved {
			u.reserve(context.Background(), "agent", "t", fmt.Sprint(i), at(time.Duration(i)*time.Second), 1, window)
		}
		f.answer(secs, nil)
		usageRetry = 0

		start := time.Now()
		used := mustReserve(t, u, "last", last, 1, window)
		fastest = min(fastest, time.Since(start))
		if want := int64(len(secs) + reserved); used != want {
			t.Errorf("used %d, want %d", used, want)
		}
	}
	t.Logf("merged %d loaded seconds into %d reserved in %v", len(secs), reserved, fastest)
	if fastest > 10*time.Millisecond {
		t.Errorf("merging %d loaded seconds into %d reserved took %v, want well under 10ms", len(secs), reserved, fastest)
	}
}

// A load runs on its own time bound, not on the time left to the write that
// started it: a write cut off while waiting is not a failure of the store,
// and the next write finds the pair loaded.
func TestUsageLoadOutlivesTheWriteThatStartedIt(t *testing.T) {
	release := make(chan struct{})
	f := &fakeLoad{secs: []store.UsageSecond{{Second: at(-10 * time.Second), Rows: 3}}, block: release}
	u := testUsage(t, f, usageBase)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := u.reserve(ctx, "agent", "t", "a", at(time.Second), 1, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first write: err %v, want its own deadline", err)
	}
	close(release)
	u.waitLoaded(t)
	// Within usageRetry of the first write.
	if used := mustReserve(t, u, "b", at(2*time.Second), 1, time.Hour); used != 4 {
		t.Errorf("next write: used %d, want 4: three loaded rows and the first write", used)
	}
	if n := f.callCount(); n != 1 {
		t.Errorf("%d loads, want 1", n)
	}
}

// A store that does not answer is cut off by the load's own bound, and that
// is a failure: the pair waits usageRetry before loading again.
func TestUsageLoadThatHangsFailsOnItsOwnBound(t *testing.T) {
	f := &fakeLoad{block: make(chan struct{})} // never closed
	u := testUsage(t, f, usageBase)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := u.reserve(ctx, "agent", "t", "a", at(time.Second), 1, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want the load's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the load ran %v; it is bound by stateTimeout, not by the write", elapsed)
	}
	if _, err := u.reserve(context.Background(), "agent", "t", "b", at(2*time.Second), 1, time.Hour); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("next write: err %v, want the failed load's", err)
	}
	if n := f.callCount(); n != 1 {
		t.Errorf("%d loads, want 1: the pair waits before loading again", n)
	}
}

// Close waits for a load in flight, which its own bound ends, and no load
// starts after it.
func TestGateCloseWaitsForALoadAndStartsNoMore(t *testing.T) {
	g, err := New(&config.Config{MaxInflight: 1}, nil, store.New(nil), nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeLoad{block: make(chan struct{})} // the store hangs
	var running atomic.Int32
	g.usage.load = func(ctx context.Context, role, table string, since, before time.Time) ([]store.UsageSecond, error) {
		running.Add(1)
		defer running.Add(-1)
		return f.load(ctx, role, table, since, before)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	g.usage.reserve(ctx, "agent", "t", "a", time.Now(), 1, time.Hour)

	closed := make(chan struct{})
	go func() {
		g.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close hangs while a load is in flight")
	}
	if n := running.Load(); n != 0 {
		t.Errorf("Close returned with %d loads still running", n)
	}
	after, cancelAfter := context.WithTimeout(context.Background(), time.Second)
	defer cancelAfter()
	if _, err := g.usage.reserve(after, "agent", "other", "b", time.Now(), 1, time.Hour); err == nil {
		t.Error("a write after Close found its usage known, want an error")
	}
	if n := f.callCount(); n != 1 {
		t.Errorf("%d loads, want 1: none starts after Close", n)
	}
}

// When the window starts at or after the gate's start, everything in it was
// reserved here: there is nothing to load, and no load can fail.
func TestUsageDoesNotLoadAWindowThatStartsAfterTheGate(t *testing.T) {
	f := &fakeLoad{err: errors.New("down")}
	u := testUsage(t, f, usageBase)
	const window = 10 * time.Second
	// The window of a write at 10.5s starts at 0.5s, in second 0: the gate's.
	if used := mustReserve(t, u, "a", at(10500*time.Millisecond), 1, window); used != 0 {
		t.Errorf("first write: used %d, want 0", used)
	}
	if used := mustReserve(t, u, "b", at(11*time.Second), 1, window); used != 1 {
		t.Errorf("second write: used %d, want 1", used)
	}
	if n := f.callCount(); n != 0 {
		t.Errorf("%d loads, want none: nothing from before the gate started is in the window", n)
	}
	// A window that starts a moment earlier reaches before the gate: it loads.
	if _, err := u.reserve(context.Background(), "early", "t", "c", at(9900*time.Millisecond), 1, window); err == nil {
		t.Error("a window reaching before the gate's start did not load")
	}
	if n := f.callCount(); n != 1 {
		t.Errorf("%d loads, want 1", n)
	}
}

package gate

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/stetdb/stet/internal/store"
)

// usageRetry is how long a pair waits after a failed load before a write
// loads it again (spec correction B9). Writes in between get the failure, and
// so an unknown usage. A var so tests can change it.
var usageRetry = 5 * time.Second

// usageSweepEvery is how often, at most, reserve drops the pairs that hold
// nothing in their window.
const usageSweepEvery = time.Minute

// usage tracks, per login role and table, the rows writes touched within the
// table's window, in one count per second. A write is reserved before it is
// sent and settled when its outcome is known.
//
// A second counts while any part of it is in the window, so a write counts up
// to a second longer than the window (spec correction B9). That keeps the
// work per write small however many writes the window holds: dropping what
// left the window takes whole seconds off the front.
//
// The first write of a pair loads the window from stet.decisions, grouped
// per second, keeping only what was recorded before this gate started: every
// write since then was reserved here, so loading it too would count it twice.
// That also makes a load that runs after a failed one, or after writes
// reserved while it was pending, add nothing this gate already holds.
type usage struct {
	load  func(ctx context.Context, role, table string, since, before time.Time) ([]store.UsageSecond, error)
	start time.Time // truncated to the microsecond, as stet.decisions stores times

	mu    sync.Mutex
	pairs map[usageKey]*usagePair
	swept time.Time // when reserve last dropped idle pairs
}

type usageKey struct{ role, table string }

type usagePair struct {
	window time.Duration // the table's; the sweep prunes by it

	loaded  bool
	loading *usageLoad // the load in flight; nil when none
	failed  time.Time  // when the last load failed, while not loaded
	err     error      // that load's error; nil when none failed

	secs  []usageSecond        // by second, oldest first
	floor int64                // seconds before it have left the window and are never counted again
	total int64                // the sum of secs' rows
	held  map[string]usageHold // reservations not yet settled, by decision id
}

// usageSecond is the rows the pair's writes touched in one second.
type usageSecond struct {
	sec  int64 // Unix time
	rows int64
}

// usageHold is a reservation not yet settled: the second it was counted in,
// and the rows it counted there.
type usageHold struct {
	sec  int64
	rows int64
}

// usageLoad is one pair's load in flight. Writes that arrive meanwhile wait
// for it rather than load again.
type usageLoad struct {
	done chan struct{}
	err  error // set before done is closed
}

func newUsage(load func(ctx context.Context, role, table string, since, before time.Time) ([]store.UsageSecond, error)) *usage {
	return &usage{
		load:  load,
		start: time.Now().Truncate(time.Microsecond),
		pairs: map[usageKey]*usagePair{},
	}
}

// reserve records that write id, which began at at, is about to touch
// planned rows, and returns what the role had already used in the window. The
// first call for a pair loads the window from stet.decisions. The write is
// reserved even when that load fails or is not tried; the error says the
// usage returned is not known.
func (u *usage) reserve(ctx context.Context, role, table, id string, at time.Time, planned int64, window time.Duration) (used int64, err error) {
	key := usageKey{role, table}
	err = u.fill(ctx, key, at, window)

	u.mu.Lock()
	defer u.mu.Unlock()
	p := u.pair(key, window)
	p.prune(at.Add(-window))
	used = p.total
	// A write that began long before it reserved can find its second gone
	// already: then it is past every window a later write looks at.
	if sec := at.Unix(); sec >= p.floor {
		p.add(sec, planned)
		p.held[id] = usageHold{sec: sec, rows: planned}
	}
	// After this write's reservation, so its own pair is never idle here.
	u.sweep(at)
	return used, err
}

// settle replaces the reservation with what happened: the rows the write
// changed when it ran and their count is known, also when the connection
// ended after the server completed it; the reservation itself when it ran
// with no count, or was lost before it completed and so may have committed;
// nothing when the server refused it or it was not sent.
func (u *usage) settle(role, table, id string, rows *int64, executed, lost bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	p := u.pairs[usageKey{role, table}]
	if p == nil {
		return
	}
	h, ok := p.held[id]
	if !ok {
		return
	}
	delete(p.held, id)
	var settled int64
	switch {
	case executed && rows != nil:
		settled = *rows
	case executed, lost:
		return
	}
	if h.sec >= p.floor { // else its second has left the window, rows and all
		p.add(h.sec, settled-h.rows)
	}
}

// fill loads a pair's window the first time the pair is seen: the seconds
// from the one the window starts in up to the gate's start. One load runs at
// a time per pair; writes that arrive meanwhile share its result. After a
// failed load the pair is not loaded again for usageRetry, and the writes in
// between get the failure. A load is not started on a context already done,
// and that is not a failure.
func (u *usage) fill(ctx context.Context, key usageKey, at time.Time, window time.Duration) error {
	u.mu.Lock()
	p := u.pair(key, window)
	if p.loaded {
		u.mu.Unlock()
		return nil
	}
	if l := p.loading; l != nil {
		u.mu.Unlock()
		select {
		case <-l.done:
			return l.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if p.err != nil && at.Sub(p.failed) < usageRetry {
		err := p.err
		u.mu.Unlock()
		return err
	}
	if err := ctx.Err(); err != nil {
		u.mu.Unlock()
		return err
	}
	l := &usageLoad{done: make(chan struct{})}
	p.loading = l
	u.mu.Unlock()

	// The whole second the window starts in, as the seconds held here count.
	secs, err := u.load(ctx, key.role, key.table, at.Add(-window).Truncate(time.Second), u.start)

	u.mu.Lock()
	defer u.mu.Unlock()
	p.loading, l.err = nil, err
	close(l.done)
	if err != nil {
		p.failed, p.err = at, err
		return err
	}
	for _, s := range secs {
		if sec := s.Second.Unix(); sec >= p.floor {
			p.add(sec, s.Rows)
		}
	}
	p.loaded, p.err = true, nil
	return nil
}

// pair returns key's entry, creating it. Call with u.mu held.
func (u *usage) pair(key usageKey, window time.Duration) *usagePair {
	p := u.pairs[key]
	if p == nil {
		p = &usagePair{window: window, held: map[string]usageHold{}}
		u.pairs[key] = p
	}
	return p
}

// sweep drops the pairs that hold nothing, at most once per usageSweepEvery,
// so roles and tables no longer written do not stay in memory. A dropped pair
// that is written again loads again; what it loads is from before the gate
// started, which has left its window by then. Call with u.mu held.
func (u *usage) sweep(now time.Time) {
	if now.Sub(u.swept) < usageSweepEvery {
		return
	}
	u.swept = now
	for key, p := range u.pairs {
		p.prune(now.Add(-p.window))
		if p.idle(now) {
			delete(u.pairs, key)
		}
	}
}

// idle reports whether p can be dropped: no load in flight, nothing in the
// window, no write still to settle, and no failed load whose wait is still on.
func (p *usagePair) idle(now time.Time) bool {
	return p.loading == nil && len(p.secs) == 0 && len(p.held) == 0 &&
		(p.err == nil || now.Sub(p.failed) >= usageRetry)
}

// prune drops the seconds that have left the window starting at cutoff: a
// second counts while any part of it is after cutoff.
func (p *usagePair) prune(cutoff time.Time) {
	floor := cutoff.Unix()
	if floor <= p.floor {
		return
	}
	p.floor = floor
	n := 0
	for n < len(p.secs) && p.secs[n].sec < floor {
		p.total -= p.secs[n].rows
		n++
	}
	p.secs = p.secs[n:]
}

// add counts rows, which may be negative, in second sec, at or after floor.
func (p *usagePair) add(sec, rows int64) {
	p.total += rows
	// Writes reserve in nearly the order they began, so the second is found
	// at the end; loaded seconds are older than any reserved here.
	i := len(p.secs)
	for i > 0 && p.secs[i-1].sec > sec {
		i--
	}
	if i > 0 && p.secs[i-1].sec == sec {
		p.secs[i-1].rows += rows
		return
	}
	p.secs = slices.Insert(p.secs, i, usageSecond{sec: sec, rows: rows})
}

package gate

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/stetdb/stet/internal/store"
)

// usage tracks, per login role and table, the rows writes touched within the
// table's window. A write is reserved before it is sent and settled when its
// outcome is known.
//
// The first write of a pair loads the window from stet.decisions, keeping
// only what was recorded before this gate started: every write since then
// was reserved here, so loading it too would count it twice. That also makes
// a load that runs after a failed one, or after writes reserved while it was
// pending, add nothing this gate already holds.
type usage struct {
	load  func(ctx context.Context, role, table string, since time.Time) ([]store.UsageEntry, error)
	start time.Time // truncated to the microsecond, as stet.decisions stores times

	mu    sync.Mutex
	pairs map[usageKey]*usagePair
}

type usageKey struct{ role, table string }

type usagePair struct {
	loaded  bool
	loading *usageLoad  // the load in flight; nil when none
	items   []usageItem // by time, oldest first
	total   int64       // the sum of items' rows
}

// usageLoad is one pair's load in flight. Writes that arrive meanwhile wait
// for it rather than load again.
type usageLoad struct {
	done chan struct{}
	err  error // set before done is closed
}

type usageItem struct {
	id   string // the write's decision id; "" for one loaded from stet.decisions
	at   time.Time
	rows int64
}

func newUsage(load func(ctx context.Context, role, table string, since time.Time) ([]store.UsageEntry, error)) *usage {
	return &usage{
		load:  load,
		start: time.Now().Truncate(time.Microsecond),
		pairs: map[usageKey]*usagePair{},
	}
}

// reserve records that write id is about to touch planned rows and returns
// what the role had already used in the window. The first call for a pair
// loads the window from stet.decisions. The write is reserved even when that
// load fails; the error says the usage returned is not known, and the next
// write loads again.
func (u *usage) reserve(ctx context.Context, role, table, id string, at time.Time, planned int64, window time.Duration) (used int64, err error) {
	key := usageKey{role, table}
	cutoff := at.Add(-window)
	err = u.fill(ctx, key, cutoff)

	u.mu.Lock()
	defer u.mu.Unlock()
	p := u.pair(key)
	p.prune(cutoff)
	used = p.total
	// Kept in time order, so pruning can stop at the first item in the
	// window. Writes reserve in nearly the order they began.
	i := len(p.items)
	for i > 0 && p.items[i-1].at.After(at) {
		i--
	}
	p.items = slices.Insert(p.items, i, usageItem{id: id, at: at, rows: planned})
	p.total += planned
	return used, err
}

// settle replaces the reservation with what happened: rows when known, the
// reservation itself when the write ran or was lost with no count, nothing
// when the server said it failed.
func (u *usage) settle(role, table, id string, rows *int64, executed, lost bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	p := u.pairs[usageKey{role, table}]
	if p == nil {
		return
	}
	// The reservation is recent, so it is found near the end.
	for i := len(p.items) - 1; i >= 0; i-- {
		if p.items[i].id != id {
			continue
		}
		switch {
		case lost: // it may have run and committed
		case !executed:
			p.total -= p.items[i].rows
			p.items = slices.Delete(p.items, i, i+1)
		case rows != nil:
			p.total += *rows - p.items[i].rows
			p.items[i].rows = *rows
		}
		return
	}
}

// fill loads a pair's window the first time the pair is seen. One load runs
// at a time per pair; writes that arrive meanwhile share its result. A failed
// load leaves the pair unloaded.
func (u *usage) fill(ctx context.Context, key usageKey, since time.Time) error {
	u.mu.Lock()
	p := u.pair(key)
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
	l := &usageLoad{done: make(chan struct{})}
	p.loading = l
	u.mu.Unlock()

	entries, err := u.load(ctx, key.role, key.table, since)

	u.mu.Lock()
	defer u.mu.Unlock()
	p.loading, l.err = nil, err
	close(l.done)
	if err != nil {
		return err
	}
	var before []usageItem
	for _, e := range entries {
		if e.At.Before(u.start) {
			before = append(before, usageItem{at: e.At, rows: e.Rows})
			p.total += e.Rows
		}
	}
	// Everything reserved here is from after start, so it goes after.
	p.items = append(before, p.items...)
	p.loaded = true
	return nil
}

// pair returns key's entry, creating it. Call with u.mu held.
func (u *usage) pair(key usageKey) *usagePair {
	p := u.pairs[key]
	if p == nil {
		p = &usagePair{}
		u.pairs[key] = p
	}
	return p
}

// prune drops items from before cutoff.
func (p *usagePair) prune(cutoff time.Time) {
	n := 0
	for n < len(p.items) && p.items[n].at.Before(cutoff) {
		p.total -= p.items[n].rows
		n++
	}
	p.items = slices.Delete(p.items, 0, n)
}

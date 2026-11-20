package gate

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/store"
)

// SetCountTimeout changes the count's time limit until restore is called.
// Tests that use it must not run in parallel.
func SetCountTimeout(d time.Duration) (restore func()) {
	old := countTimeout
	countTimeout = d
	return func() { countTimeout = old }
}

// SetLogger replaces the gate's logger. Call it before any write reaches g.
func SetLogger(g *Gate, l *slog.Logger) {
	g.log = l
	if g.stale != nil {
		g.stale.log = l
	}
}

// CheckRead is what g's memory of reads answers now for a write by role on
// behalf of task (stet.on_behalf_of; "" for none) to the row key of table,
// whose version now is version ("" when it is not there).
func CheckRead(g *Gate, role, task, table, key, version string) decide.ReadCheck {
	m := g.stale.mem
	return m.check(party{role, task}, table, key, version, m.now())
}

// CompareWrite is what a write by role on behalf of task to the row key of
// table is found to be, when the state read sent at sent found its version
// to be version ("" when the row is not there).
func CompareWrite(g *Gate, role, task, table, key, version string, sent time.Time) decide.ReadCheck {
	return g.stale.compareRead(party{role, task}, table, key, version, sent)
}

// OnLookup makes g call hook before each lookup of row versions, with the
// lookup's context, table and keys; when hook returns an error, the lookup
// fails with it. Call it before the reads it is for reach g.
func OnLookup(g *Gate, hook func(ctx context.Context, table string, keys []string) error) {
	look := g.stale.lookup
	g.stale.lookup = func(ctx context.Context, table string, keys []string) (map[string]string, error) {
		if err := hook(ctx, table, keys); err != nil {
			return nil, err
		}
		return look(ctx, table, keys)
	}
}

// LookupVersions runs g's lookup of the versions of table's rows keys once,
// under the lookups' time limit.
func LookupVersions(g *Gate, table string, keys []string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), staleLookupTimeout)
	defer cancel()
	return g.stale.lookupVersions(ctx, table, keys)
}

// SetLookupTimeout gives g's lookups of row versions d instead of
// staleLookupTimeout. Call it before the reads it is for reach g.
func SetLookupTimeout(g *Gate, d time.Duration) {
	g.stale.lookupTimeout = d
}

// StaleTable is what g's catalog says of the config key table.
func StaleTable(g *Gate, table string) (oid uint32, attr uint16, typ proxy.KeyType, typeName string, ok bool) {
	t, ok := g.stale.table(table)
	return t.oid, t.attr, t.typ, t.typeName, ok
}

// Refresh queues a refresh of the row key of table and waits for its answer.
func Refresh(g *Gate, table, key string) (serverKey, version string, ok bool) {
	done := make(chan struct{})
	g.stale.refresh(table, key, func(k, v string, found bool) {
		serverKey, version, ok = k, v, found
		close(done)
	})
	<-done
	return serverKey, version, ok
}

// SetStaleLostGrace changes how long the mark of a write lost with its
// connection stays, for writes lost until restore is called. Tests that use
// it must not run in parallel.
func SetStaleLostGrace(d time.Duration) (restore func()) {
	old := staleLostGrace
	staleLostGrace = d
	return func() { staleLostGrace = old }
}

// OnRowVersion makes g call hook each time, just before it reads a row's
// version again before a judged write is sent. Call it before any write
// reaches g.
func OnRowVersion(g *Gate, hook func(table, pk string)) {
	read := g.rowVersion
	g.rowVersion = func(ctx context.Context, table, pk string) (string, error) {
		hook(table, pk)
		return read(ctx, table, pk)
	}
}

// SetStaleCatalogEvery changes how often gates created until restore is
// called read the catalog again. Tests that use it must not run in parallel.
func SetStaleCatalogEvery(d time.Duration) (restore func()) {
	old := staleCatalogEvery
	staleCatalogEvery = d
	return func() { staleCatalogEvery = old }
}

// FlushReads writes every read bucket held in memory now, and the large
// reads still waiting to be logged.
func (g *Gate) FlushReads() {
	a := g.reads
	if a == nil {
		return
	}
	done := make(chan struct{})
	select {
	case a.flushes <- done:
		<-done
	case <-a.stopped:
	}
}

// SetReadFlushInterval changes how often gates created until restore is
// called write past minutes' reads. Tests that use it must not run in
// parallel.
func SetReadFlushInterval(d time.Duration) (restore func()) {
	old := readFlushEvery
	readFlushEvery = d
	return func() { readFlushEvery = old }
}

// FailReadWrites makes g's next n writes to stet.reads fail.
func FailReadWrites(g *Gate, n int) {
	a := g.reads
	a.mu.Lock()
	defer a.mu.Unlock()
	write := a.write
	var mu sync.Mutex
	a.write = func(ctx context.Context, b []store.ReadBucket) error {
		mu.Lock()
		fail := n > 0
		n--
		mu.Unlock()
		if fail {
			return errors.New("injected failure")
		}
		return write(ctx, b)
	}
}

// SetReadClock makes g's read flushes take now() as the time.
func SetReadClock(g *Gate, now func() time.Time) {
	a := g.reads
	a.mu.Lock()
	defer a.mu.Unlock()
	a.now = now
}

// FailUsageLoads makes g's next n loads of cumulative usage from
// stet.decisions fail. Call it before any write reaches g.
func FailUsageLoads(g *Gate, n int) {
	load := g.usage.load
	var mu sync.Mutex
	g.usage.load = func(ctx context.Context, role, table string, since, before time.Time) ([]store.UsageSecond, error) {
		mu.Lock()
		fail := n > 0
		n--
		mu.Unlock()
		if fail {
			return nil, errors.New("injected failure")
		}
		return load(ctx, role, table, since, before)
	}
}

// SetUsageRetry changes how long a pair waits after a failed load before
// loading again, until restore is called. Not for parallel tests.
func SetUsageRetry(d time.Duration) (restore func()) {
	old := usageRetry
	usageRetry = d
	return func() { usageRetry = old }
}

// FailHeldWrites makes g's next n inserts into stet.held_writes fail.
func FailHeldWrites(g *Gate, n int) {
	insert := g.insertHeld
	var mu sync.Mutex
	g.insertHeld = func(ctx context.Context, h *store.HeldWrite) error {
		mu.Lock()
		fail := n > 0
		n--
		mu.Unlock()
		if fail {
			return errors.New("injected failure")
		}
		return insert(ctx, h)
	}
}

// LandHeldWritesThenFail makes g's next n inserts into stet.held_writes
// store their row and then report an error, as an insert whose answer came
// after its time was up does.
func LandHeldWritesThenFail(g *Gate, n int) {
	insert := g.insertHeld
	var mu sync.Mutex
	g.insertHeld = func(ctx context.Context, h *store.HeldWrite) error {
		mu.Lock()
		fail := n > 0
		n--
		mu.Unlock()
		if err := insert(ctx, h); err != nil || !fail {
			return err
		}
		return errors.New("injected failure after the insert")
	}
}

// FailDelivered makes g's next n markings of a held write as pending fail.
// Call it before any write reaches g.
func FailDelivered(g *Gate, n int) {
	mark := g.markDelivered
	var mu sync.Mutex
	g.markDelivered = func(ctx context.Context, id string, inTransaction bool) error {
		mu.Lock()
		fail := n > 0
		n--
		mu.Unlock()
		if fail {
			return errors.New("injected failure")
		}
		return mark(ctx, id, inTransaction)
	}
}

// OnDelivered makes g call f with a held write's id each time it marks one
// pending, just before it does. Call it before any write reaches g.
func OnDelivered(g *Gate, f func(id string)) {
	mark := g.markDelivered
	g.markDelivered = func(ctx context.Context, id string, inTransaction bool) error {
		f(id)
		return mark(ctx, id, inTransaction)
	}
}

// FailRowVersionReads makes g's next n re-reads of a row's version, before
// a judged write is sent, fail.
func FailRowVersionReads(g *Gate, n int) {
	read := g.rowVersion
	var mu sync.Mutex
	g.rowVersion = func(ctx context.Context, table, pk string) (string, error) {
		mu.Lock()
		fail := n > 0
		n--
		mu.Unlock()
		if fail {
			return "", errors.New("injected failure")
		}
		return read(ctx, table, pk)
	}
}

// HoldInflight takes every slot of g's inflight limit until release is called.
func HoldInflight(g *Gate) (release func()) {
	for range cap(g.inflight) {
		g.inflight <- struct{}{}
	}
	return func() {
		for range cap(g.inflight) {
			<-g.inflight
		}
	}
}

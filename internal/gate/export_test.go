package gate

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

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
	g.usage.load = func(ctx context.Context, role, table string, since time.Time) ([]store.UsageEntry, error) {
		mu.Lock()
		fail := n > 0
		n--
		mu.Unlock()
		if fail {
			return nil, errors.New("injected failure")
		}
		return load(ctx, role, table, since)
	}
}

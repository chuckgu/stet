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

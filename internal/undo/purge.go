package undo

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/stetdb/stet/internal/store"
)

// purgeEvery is how often the purger deletes old change records (spec 9). A
// var so tests can shorten it.
var purgeEvery = time.Hour

// purgeLimit is how many change sets one delete takes at most (spec 9).
const purgeLimit = 10_000

// purgeTimeout bounds one delete, so that a database that does not answer
// holds neither the loop nor Close for long. A var so tests can shorten it.
var purgeTimeout = 30 * time.Second

// Purger deletes the change records older than undo.keep (spec 9): the
// change sets in stet.change_sets, and with them their lines in
// stet.changes. It runs the same in both modes, on Stet's own pool, and
// touches no client's session.
type Purger struct {
	store *store.Store
	keep  time.Duration
	log   *slog.Logger

	ctx       context.Context // done once Close is called
	stop      context.CancelFunc
	startOnce sync.Once
	stopped   chan struct{} // closed once the loop has returned, or at Close when it never started
}

func NewPurger(st *store.Store, keep time.Duration, log *slog.Logger) *Purger {
	ctx, stop := context.WithCancel(context.Background())
	return &Purger{store: st, keep: keep, log: log, ctx: ctx, stop: stop, stopped: make(chan struct{})}
}

// Start purges at once, then every hour, in a goroutine. Each time it deletes
// 10,000 sets at a time until a delete takes fewer. Start does nothing a
// second time, or after Close.
func (p *Purger) Start() {
	p.startOnce.Do(func() { go p.loop() })
}

// Close stops the purging and waits for a delete in progress, which ends
// within purgeTimeout. Nothing is deleted after it returns.
func (p *Purger) Close() {
	p.stop()
	p.startOnce.Do(func() { close(p.stopped) }) // never started: nothing to wait for
	<-p.stopped
}

func (p *Purger) loop() {
	defer close(p.stopped)
	tick := time.NewTicker(purgeEvery)
	defer tick.Stop()
	for {
		p.purge()
		select {
		case <-p.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// purge deletes the sets older than keep, purgeLimit at a time, until a
// delete takes fewer or Close is called. A delete is not cut short by Close:
// it runs under its own bound, in a transaction of its own. A failure is
// logged, and the next wake-up tries again.
func (p *Purger) purge() {
	var total int64
	for p.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), purgeTimeout)
		n, err := p.store.PurgeChanges(ctx, p.keep, purgeLimit)
		cancel()
		total += n
		if err != nil {
			p.log.Warn("old change records not purged; the purge runs again at its next wake-up",
				"keep", p.keep, "purged_sets", total, "next_in", purgeEvery, "err", err)
			return
		}
		if n < purgeLimit {
			break
		}
	}
	if total > 0 {
		p.log.Info("old change records purged", "keep", p.keep, "purged_sets", total)
	}
}

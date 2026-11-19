package undo

import (
	"context"
	"errors"
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

// deleteTimeout bounds one delete while the purger runs, so that a delete
// stuck on a server that does not answer still ends. It is long: a delete
// blocks no agent's write (it takes ROW EXCLUSIVE, and row locks only on
// old sets), and a shorter bound would stop the purge for good once the
// oldest 10,000 sets take longer, as the next wake-up would try the same
// sets under the same bound. A var so tests can shorten it.
var deleteTimeout = 10 * time.Minute

// closeGrace is how long Close waits for a delete in progress before it
// cancels it. A cancelled delete deletes nothing. A var so tests can
// shorten it.
var closeGrace = 5 * time.Second

// Purger deletes the change records older than undo.keep (spec 9): the
// change sets in stet.change_sets, and with them their lines in
// stet.changes. It runs the same in both modes, on Stet's own pool, and
// touches no client's session.
type Purger struct {
	store *store.Store
	keep  time.Duration
	log   *slog.Logger

	ctx       context.Context // done once Close is called: no further delete starts
	stop      context.CancelFunc
	deletes   context.Context // the deletes' parent: done once Close's grace is over
	cancel    context.CancelFunc
	startOnce sync.Once
	stopped   chan struct{} // closed once the loop has returned, or at Close when it never started
}

// NewPurger returns a Purger that deletes, through st, the change sets
// recorded more than keep ago, and logs to log. It does nothing until Start.
func NewPurger(st *store.Store, keep time.Duration, log *slog.Logger) *Purger {
	ctx, stop := context.WithCancel(context.Background())
	deletes, cancel := context.WithCancel(context.Background())
	return &Purger{
		store: st, keep: keep, log: log,
		ctx: ctx, stop: stop, deletes: deletes, cancel: cancel, stopped: make(chan struct{}),
	}
}

// Start purges at once, then every hour, in a goroutine. Each time it deletes
// 10,000 sets at a time until a delete takes fewer. Start does nothing a
// second time, or after Close.
func (p *Purger) Start() {
	p.startOnce.Do(func() { go p.loop() })
}

// Close stops the purging and waits for a delete in progress for up to
// closeGrace, then cancels it; the cancelled delete deletes nothing. Nothing
// is deleted after Close returns.
func (p *Purger) Close() {
	p.stop()
	p.startOnce.Do(func() { close(p.stopped) }) // never started: nothing to wait for
	grace := time.NewTimer(closeGrace)
	defer grace.Stop()
	select {
	case <-p.stopped:
	case <-grace.C:
		p.cancel()
		<-p.stopped
	}
	p.cancel()
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
// delete takes fewer or Close is called. Each delete runs in a transaction
// of its own, under deleteTimeout, and is cancelled only once Close's grace
// is over. A failure is logged, and the next wake-up tries again.
func (p *Purger) purge() {
	var total int64
	for p.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(p.deletes, deleteTimeout)
		n, err := p.store.PurgeChanges(ctx, p.keep, purgeLimit)
		ended := ctx.Err()
		cancel()
		total += n
		if err != nil {
			p.failed(err, ended, total)
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

// failed logs a delete that failed; ended is its context's error once it
// returned.
func (p *Purger) failed(err, ended error, purged int64) {
	switch {
	case errors.Is(ended, context.DeadlineExceeded):
		p.log.Warn("old change records not purged: a delete of up to 10000 change sets did not finish within "+
			deleteTimeout.String()+" and deleted nothing; the purge runs again at its next wake-up",
			"keep", p.keep, "purged_sets", purged, "next_in", purgeEvery, "err", err)
	case ended != nil:
		p.log.Warn("old change records not purged: a delete of up to 10000 change sets did not finish within the "+
			closeGrace.String()+" Stet waits for it when stopping, was cancelled and deleted nothing; the purge runs "+
			"again when Stet starts", "keep", p.keep, "purged_sets", purged, "err", err)
	default:
		p.log.Warn("old change records not purged; the purge runs again at its next wake-up",
			"keep", p.keep, "purged_sets", purged, "next_in", purgeEvery, "err", err)
	}
}

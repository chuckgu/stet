package apply

import (
	"context"
	"time"
)

// pollEvery is how often the loop looks for approved held writes (spec 7).
const pollEvery = time.Second

// listLimit is how many approved held writes one look lists at most.
const listLimit = 100

// listTimeout bounds one listing, so that a database that does not answer
// holds neither the loop nor Close for long.
const listTimeout = 5 * time.Second

// retryAfter is how long the loop leaves alone a held write whose Apply
// returned an error: one of Stet's own steps failed, and the row stays
// approved. Tried every second instead, a statement that outlived Apply's
// bound would be run again and again until its approval lapsed, repeating
// whatever in it is not undone with its transaction (ruling 16). A var so
// tests can shorten it.
var retryAfter = 30 * time.Second

// Start polls for approved held writes every second, in a goroutine, and
// applies them one at a time in the order they were approved. The first look
// is at once. Start does nothing a second time, or after Close.
func (a *Applier) Start() {
	a.startOnce.Do(func() { go a.loop() })
}

// Close stops the polling and waits for the write being applied, which ends
// within held.apply_timeout + applyGrace. No write is taken after it.
func (a *Applier) Close() {
	a.stop()
	a.startOnce.Do(func() { close(a.stopped) }) // never started: nothing to wait for
	<-a.stopped
}

func (a *Applier) loop() {
	defer close(a.stopped)
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	retry := map[string]time.Time{} // held write id → when the loop may try it again
	for {
		a.poll(retry)
		select {
		case <-a.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// poll lists the approved held writes and applies them in the listing's
// order, one at a time, until Close is called. When Apply returns an error,
// the write stays approved: the error is logged and the write is left out of
// later polls until retryAfter has passed. Ids no longer listed leave retry.
func (a *Applier) poll(retry map[string]time.Time) {
	ctx, cancel := context.WithTimeout(a.ctx, listTimeout)
	ws, err := a.store.ApprovedWrites(ctx, listLimit)
	cancel()
	if a.ctx.Err() != nil {
		return // closed during the listing
	}
	if err != nil {
		a.log.Warn("approved held writes not listed", "err", err)
		return
	}
	listed := make(map[string]bool, len(ws))
	for _, w := range ws {
		listed[w.ID] = true
	}
	for id := range retry {
		if !listed[id] {
			delete(retry, id)
		}
	}

	for _, w := range ws {
		if a.ctx.Err() != nil {
			return
		}
		if time.Now().Before(retry[w.ID]) { // the zero time when it has not failed
			continue
		}
		if err := a.applyOne(w); err != nil {
			retry[w.ID] = time.Now().Add(retryAfter)
			a.log.Warn("held write not applied; it stays approved", "id", w.ID, "retry_after", retryAfter, "err", err)
			continue
		}
		delete(retry, w.ID)
	}
}

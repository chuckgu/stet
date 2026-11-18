package apply

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/store"
)

// FailMarks makes a's next n calls of MarkApplied fail, as a lost connection
// would. Call it before a is applying.
func FailMarks(a *Applier, n int) {
	var mu sync.Mutex
	mark := a.markApplied
	a.markApplied = func(ctx context.Context, q store.Querier, id string, rows int64) error {
		mu.Lock()
		fail := n > 0
		if fail {
			n--
		}
		mu.Unlock()
		if fail {
			return errors.New("MarkApplied made to fail by the test")
		}
		return mark(ctx, q, id, rows)
	}
}

// BeforeStatement has a call f once a held write passed the checks before
// its statement, before it takes the row's role and settings; nil removes
// it. Call it before a is applying.
func BeforeStatement(a *Applier, f func()) {
	a.beforeStatement = f
}

// AfterCommit has a call f, with the held write's connection, once a held
// write was applied and its transaction committed, before the connection is
// closed; nil removes it. Call it before a is applying.
func AfterCommit(a *Applier, f func(ctx context.Context, conn *pgx.Conn)) {
	a.afterCommit = f
}

// SetGrace changes applyGrace until restore is called. Tests that use it
// must not run in parallel, and call restore only after closing every
// Applier they started.
func SetGrace(d time.Duration) (restore func()) {
	old := applyGrace
	applyGrace = d
	return func() { applyGrace = old }
}

// SetRetryAfter changes retryAfter until restore is called, under the terms
// of SetGrace.
func SetRetryAfter(d time.Duration) (restore func()) {
	old := retryAfter
	retryAfter = d
	return func() { retryAfter = old }
}

// OnApply has a's loop call f for each held write in place of Apply; f may
// call Apply itself. Call it before Start.
func OnApply(a *Applier, f func(w store.Approved) error) {
	a.applyOne = f
}

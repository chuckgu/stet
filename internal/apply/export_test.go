package apply

import (
	"context"
	"errors"
	"sync"
	"time"

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
// its statement, before the savepoint; nil removes it. Call it before a is
// applying.
func BeforeStatement(a *Applier, f func()) {
	a.beforeStatement = f
}

// SetGrace changes applyGrace until restore is called. Tests that use it
// must not run in parallel.
func SetGrace(d time.Duration) (restore func()) {
	old := applyGrace
	applyGrace = d
	return func() { applyGrace = old }
}

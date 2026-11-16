package gate

import (
	"log/slog"
	"time"
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

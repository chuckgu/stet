package proxy_test

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/proxy"
)

const latencyWarmUp, latencyTrips = 200, 2000

// addedLatency times select 1 directly and through a proxy with gate (nil for
// a plain passthrough), interleaving the two so drift hits both equally, and
// returns how much the proxy adds at p95. label names the case in the log.
func addedLatency(t *testing.T, gate proxy.Gate, label string) time.Duration {
	t.Helper()
	if testing.Short() {
		t.Skip("latency measurement skipped in -short mode")
	}
	up := upstream(t)
	addr, _ := startProxy(t, up, gate)
	direct := connect(t, up, "stet_scram", nil)
	proxied := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)

	roundTrip := func(conn *pgx.Conn) time.Duration {
		start := time.Now()
		var one int
		if err := conn.QueryRow(ctx, "select 1").Scan(&one); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	for range latencyWarmUp { // warm up both paths
		roundTrip(direct)
		roundTrip(proxied)
	}
	d := make([]time.Duration, latencyTrips)
	p := make([]time.Duration, latencyTrips)
	for i := range latencyTrips {
		d[i] = roundTrip(direct)
		p[i] = roundTrip(proxied)
	}
	slices.Sort(d)
	slices.Sort(p)
	pct := func(xs []time.Duration, q float64) time.Duration { return xs[int(q*float64(len(xs)-1))] }

	added := pct(p, 0.95) - pct(d, 0.95)
	t.Logf("%s, %d round trips: direct p50=%v p95=%v, proxied p50=%v p95=%v, added p95=%v",
		label, latencyTrips, pct(d, 0.5), pct(d, 0.95), pct(p, 0.5), pct(p, 0.95), added)
	return added
}

// Spec 11: statements that are not gated may add at most 2ms at p95.
// Direct and proxied round trips are interleaved so drift hits both equally.
func TestPassthroughLatencyBudget(t *testing.T) {
	if added := addedLatency(t, nil, "select 1"); added > 2*time.Millisecond {
		t.Errorf("proxy adds %v at p95, budget is 2ms", added)
	}
}

// Spec 8 of the accounting spec: a read the gate observes stays inside the
// same 2ms budget at p95, its rows counted on the way.
func TestObservedReadLatencyBudget(t *testing.T) {
	g := newObservingGate(func(sql string) bool { return sql == "select 1" })
	added := addedLatency(t, g, "observed select 1")
	for _, ob := range g.observed(t, latencyWarmUp+latencyTrips) {
		if ob.o.Sent != 1 || !ob.o.Executed {
			t.Fatalf("observed read: %+v, want 1 row sent and executed", ob.o)
		}
	}
	if added > 2*time.Millisecond {
		t.Errorf("an observed read adds %v at p95, budget is 2ms", added)
	}
}

package proxy_test

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Spec 11: statements that are not gated may add at most 2ms at p95.
// Direct and proxied round trips are interleaved so drift hits both equally.
func TestPassthroughLatencyBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("latency measurement skipped in -short mode")
	}
	up := upstream(t)
	addr, _ := startProxy(t, up, nil)
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
	for range 200 { // warm up both paths
		roundTrip(direct)
		roundTrip(proxied)
	}
	const n = 2000
	d := make([]time.Duration, n)
	p := make([]time.Duration, n)
	for i := range n {
		d[i] = roundTrip(direct)
		p[i] = roundTrip(proxied)
	}
	slices.Sort(d)
	slices.Sort(p)
	pct := func(xs []time.Duration, q float64) time.Duration { return xs[int(q*float64(len(xs)-1))] }

	added := pct(p, 0.95) - pct(d, 0.95)
	t.Logf("select 1, %d round trips: direct p50=%v p95=%v, proxied p50=%v p95=%v, added p95=%v",
		n, pct(d, 0.5), pct(d, 0.95), pct(p, 0.5), pct(p, 0.95), added)
	if added > 2*time.Millisecond {
		t.Errorf("proxy adds %v at p95, budget is 2ms", added)
	}
}

// Spec 8 of the accounting spec: a read the gate observes stays inside the
// same 2ms budget at p95, its rows counted on the way.
func TestObservedReadLatencyBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("latency measurement skipped in -short mode")
	}
	up := upstream(t)
	g := newObservingGate(func(sql string) bool { return sql == "select 1" })
	addr, _ := startProxy(t, up, g)
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
	const warm, n = 200, 2000
	for range warm {
		roundTrip(direct)
		roundTrip(proxied)
	}
	d := make([]time.Duration, n)
	p := make([]time.Duration, n)
	for i := range n {
		d[i] = roundTrip(direct)
		p[i] = roundTrip(proxied)
	}
	slices.Sort(d)
	slices.Sort(p)
	pct := func(xs []time.Duration, q float64) time.Duration { return xs[int(q*float64(len(xs)-1))] }

	obs := g.observed(t, warm+n)
	for _, ob := range obs {
		if ob.o.Sent != 1 || !ob.o.Executed {
			t.Fatalf("observed read: %+v, want 1 row sent and executed", ob.o)
		}
	}
	added := pct(p, 0.95) - pct(d, 0.95)
	t.Logf("observed select 1, %d round trips: direct p50=%v p95=%v, proxied p50=%v p95=%v, added p95=%v",
		n, pct(d, 0.5), pct(d, 0.95), pct(p, 0.5), pct(p, 0.95), added)
	if added > 2*time.Millisecond {
		t.Errorf("an observed read adds %v at p95, budget is 2ms", added)
	}
}

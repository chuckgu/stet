package proxy_test

import (
	"context"
	"fmt"
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
	return addedLatencyWith(t, gate, proxy.Policy{}, "stet_scram", label)
}

// addedLatencyWith is addedLatency through a proxy with policy, both
// connections logged in as user.
func addedLatencyWith(t *testing.T, gate proxy.Gate, policy proxy.Policy, user, label string) time.Duration {
	t.Helper()
	return addedLatencyOf(t, gate, policy, user, label, func(ctx context.Context, conn *pgx.Conn, _ int) error {
		var one int
		return conn.QueryRow(ctx, "select 1").Scan(&one)
	})
}

// addedLatencyOf is addedLatencyWith for the round trip trip, which is
// handed the round trip's number.
func addedLatencyOf(t *testing.T, gate proxy.Gate, policy proxy.Policy, user, label string, trip func(ctx context.Context, conn *pgx.Conn, i int) error) time.Duration {
	t.Helper()
	if testing.Short() {
		t.Skip("latency measurement skipped in -short mode")
	}
	up := upstream(t)
	addr, _ := startProxyWith(t, up, gate, policy)
	direct := connect(t, up, user, nil)
	proxied := connect(t, addr, user, nil)
	ctx := testCtx(t)

	n := 0
	roundTrip := func(conn *pgx.Conn) time.Duration {
		n++
		start := time.Now()
		if err := trip(ctx, conn, n); err != nil {
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

// Spec 3 of the enforce spec: the session of a role not listed as an agent's
// is relayed as if there were no gate. It stays inside the 2ms budget at p95
// and the gate never hears of it.
func TestUnwatchedSessionLatencyBudget(t *testing.T) {
	g := newRecordingGate(isWrite)
	added := addedLatencyWith(t, g, proxy.Policy{Agents: []string{"stet_md5"}}, "stet_scram", "unwatched select 1")
	if n := g.inspectCount(); n != 0 {
		t.Errorf("Inspect ran %d times for a session that is not watched, want never", n)
	}
	if added > 2*time.Millisecond {
		t.Errorf("an unwatched session adds %v at p95, budget is 2ms", added)
	}
}

// Spec 15 of the enforce spec: on an enforcing proxy a watched session checks
// each message for a closed point before forwarding it, and a statement let
// through stays inside the same 2ms budget at p95.
func TestEnforcedSessionLatencyBudget(t *testing.T) {
	g := closedPointGate(isWrite)
	added := addedLatencyWith(t, g, proxy.Policy{Enforce: true}, "stet_scram", "enforced select 1")
	if g.inspectCount() == 0 {
		t.Error("Inspect never ran: the session was not watched")
	}
	if added > 2*time.Millisecond {
		t.Errorf("an enforced session adds %v at p95, budget is 2ms", added)
	}
}

// Spec 14 of the stale-write spec: a tracked read of 100 rows, whose key the
// proxy copies out of each row, stays inside the same 2ms budget at p95. pgx
// prepares the statement once (Parse, Describe) and then binds it with binary
// result formats on every round trip.
func TestTrackedReadLatencyBudget(t *testing.T) {
	tb := idTable(t, 100)
	g := newKeyGate(tb)
	sql := "select id, title from " + tb.name
	added := addedLatencyOf(t, g, proxy.Policy{}, "stet_scram", "tracked read of 100 rows",
		func(ctx context.Context, conn *pgx.Conn, _ int) error {
			rows, err := conn.Query(ctx, sql)
			if err != nil {
				return err
			}
			n := 0
			for rows.Next() {
				n++
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if n != 100 {
				return fmt.Errorf("%d rows, want 100", n)
			}
			return nil
		})
	obs := g.observed(t, latencyWarmUp+latencyTrips)
	for _, ob := range obs[len(obs)-10:] {
		if ob.o.Read == nil || len(ob.o.Read.Keys) != 100 {
			t.Fatalf("tracked read: Read %+v, want 100 keys", ob.o.Read)
		}
	}
	if added > 2*time.Millisecond {
		t.Errorf("a tracked read of 100 rows adds %v at p95, budget is 2ms", added)
	}
}

// Ruling 22: an enforced session follows the ParameterStatus messages the
// server sends, here one per round trip (application_name is reported on
// every change). That stays inside the same 2ms budget at p95.
func TestEnforcedSessionSettingsLatencyBudget(t *testing.T) {
	g := closedPointGate(isWrite)
	added := addedLatencyOf(t, g, proxy.Policy{Enforce: true}, "stet_scram", "enforced SET application_name",
		func(ctx context.Context, conn *pgx.Conn, i int) error {
			_, err := conn.Exec(ctx, fmt.Sprintf("set application_name = 'latency_%d'", i%2))
			return err
		})
	if added > 2*time.Millisecond {
		t.Errorf("an enforced session following its settings adds %v at p95, budget is 2ms", added)
	}
}

package store_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const captureWarmUp, captureTrips = 200, 2000

// tripTimes is how long each round trip of one kind took.
type tripTimes []time.Duration

func (d tripTimes) mean() time.Duration {
	var sum time.Duration
	for _, x := range d {
		sum += x
	}
	return sum / time.Duration(len(d))
}

// p95 is the 95th percentile; d must be sorted.
func (d tripTimes) p95() time.Duration { return d[int(0.95*float64(len(d)-1))] }

// Spec 13: capture adds at most 1ms at p95 to a write that updates one row by
// its key. Three kinds of the same UPDATE of a 14-column table, interleaved so
// that drift hits all equally: on a table without triggers, on a table with
// capture's triggers by a session that is not captured, and on a table with
// them by the agent, whose write is captured.
func TestCaptureAddsUnderAMillisecond(t *testing.T) {
	if testing.Short() {
		t.Skip("latency measurement skipped in -short mode")
	}
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	tables := []string{"public.plain", "public.watched", "public.captured"}
	for _, table := range tables {
		mustExec(t, owner,
			`create table `+table+` (id int primary key, name text, email text, phone text, title text, company text,
				city text, country text, amount numeric, score float8, active boolean, tags text[], created timestamptz,
				notes jsonb)`,
			`insert into `+table+` select g, 'name '||g, 'user'||g||'@example.com', '+1 555 '||g, 'title', 'company '||(g % 50),
				'city', 'country', g * 10.5, g / 7.0, g % 2 = 0, array['a', 'b'], '2026-11-18 10:00:00+00', '{"k": 1}'
				from generate_series(1, 1000) g`)
	}
	installCapture(t, owner, "public.watched", "watched", "id")
	installCapture(t, owner, "public.captured", "captured", "id")
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	agent := agentIn(t, db)
	other := superuserIn(t, db)

	n := 0
	trip := func(conn *pgx.Conn, table string) time.Duration {
		n++
		start := time.Now()
		if _, err := conn.Exec(ctx, `update `+table+` set amount = amount + 1, title = $2 where id = $1`, n%1000+1, fmt.Sprintf("title %d", n)); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	for range captureWarmUp {
		trip(agent, tables[0])
		trip(other, tables[1])
		trip(agent, tables[2])
	}
	plain, watched, captured := make(tripTimes, captureTrips), make(tripTimes, captureTrips), make(tripTimes, captureTrips)
	for i := range captureTrips {
		plain[i] = trip(agent, tables[0])
		watched[i] = trip(other, tables[1])
		captured[i] = trip(agent, tables[2])
	}

	// What was measured is what was meant: every write to the captured
	// table left a set and a line, none to the other.
	if n := countOf(t, pool, `select count(*) from stet.change_sets s join stet.changes c on c.set_id = s.id
		where s.target_table = 'captured' and s.skipped is null`); n != captureWarmUp+captureTrips {
		t.Errorf("%d writes captured, want %d", n, captureWarmUp+captureTrips)
	}
	if n := countOf(t, pool, `select count(*) from stet.change_sets where target_table <> 'captured'`); n != 0 {
		t.Errorf("%d sets for writes that are not captured", n)
	}

	for _, d := range []tripTimes{plain, watched, captured} {
		slices.Sort(d)
	}
	t.Logf("%d round trips each: no trigger mean=%v p95=%v; trigger, not an agent mean=%v p95=%v; captured mean=%v p95=%v",
		captureTrips, plain.mean(), plain.p95(), watched.mean(), watched.p95(), captured.mean(), captured.p95())
	if added := captured.p95() - plain.p95(); added > time.Millisecond {
		t.Errorf("capture adds %v at p95, want at most 1ms", added)
	}
}

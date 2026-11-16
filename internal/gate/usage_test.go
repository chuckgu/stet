package gate_test

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/store"
)

// cumYAML limits one login role to 5 rows of the table an hour, and one
// statement to 3.
const cumYAML = tableYAML + "    impact: {max_rows: 3, cumulative: {max_rows: 5, window: 1h}}\n"

// shortCumYAML is cumYAML with a window short enough to watch it pass.
const shortCumYAML = tableYAML + "    impact: {max_rows: 3, cumulative: {max_rows: 5, window: 2s}}\n"

// account is what a decision records about who wrote and how much.
type account struct {
	Verdict      string
	Reasons      []string
	Executed     bool
	LoginRole    *string
	RowsAffected *int64
	Cumulative   map[string]any // nil when the column is NULL
	Questions    []map[string]any
	Answers      map[string]any
}

// accounts waits for n decisions on the env's table, oldest first.
func (e *env) accounts(t *testing.T, n int) []account {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := e.admin.Query(context.Background(), `
			select verdict, reasons, executed, login_role, rows_affected, cumulative, questions, answers
			from stet.decisions where target_table = $1 order by created_at`, e.table)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[account])
		if err != nil {
			t.Fatal(err)
		}
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d decisions recorded, want %d", len(got), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// restart starts a second gate and proxy over the env's table and config, as
// Stet would come up after a restart: the new gate has seen no writes.
func (e *env) restart(t *testing.T) *env {
	t.Helper()
	g, err := gate.New(e.cfg, e.pool, store.New(e.pool), calm(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := proxy.New(os.Getenv("STET_TEST_PG"), slog.New(slog.DiscardHandler), g)
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		g.Close()
	})
	again := *e
	again.gate, again.addr = g, ln.Addr().String()
	return &again
}

// fill has conn touch five rows of the env's table in three writes: two rows
// of company 9 twice, then row 5. The table must hold seedMany's rows.
func (e *env) fill(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	mustExec(t, conn, "update "+e.table+" set status = 'f1' where company_id = 9")
	mustExec(t, conn, "update "+e.table+" set status = 'f2' where company_id = 9")
	mustExec(t, conn, "update "+e.table+" set title = 'f3' where id = 5")
}

func wantAccount(t *testing.T, i int, a account, verdict string, reasons ...string) {
	t.Helper()
	if reasons == nil {
		reasons = []string{}
	}
	if a.Verdict != verdict || !reflect.DeepEqual(a.Reasons, reasons) {
		t.Errorf("decision %d: verdict %s %v, want %s %v (cumulative %v)", i, a.Verdict, a.Reasons, verdict, reasons, a.Cumulative)
	}
}

// wantUsage checks a recorded cumulative against cumYAML's limit: what the
// role had used before this write, and what this write planned.
func wantUsage(t *testing.T, i int, a account, used, planned float64) {
	t.Helper()
	want := map[string]any{"used": used, "planned": planned, "limit": 5.0}
	for k, v := range want {
		if a.Cumulative[k] != v {
			t.Errorf("decision %d: cumulative.%s = %v, want %v (cumulative %v)", i, k, a.Cumulative[k], v, a.Cumulative)
		}
	}
	if msg, ok := a.Cumulative["error"]; ok {
		t.Errorf("decision %d: cumulative.error = %v, want none", i, msg)
	}
}

func wantRows(t *testing.T, i int, a account, want *int64) {
	t.Helper()
	if !reflect.DeepEqual(a.RowsAffected, want) {
		t.Errorf("decision %d: rows_affected = %v, want %v", i, deref(a.RowsAffected), deref(want))
	}
}

func wantRole(t *testing.T, i int, a account, want string) {
	t.Helper()
	if a.LoginRole == nil || *a.LoginRole != want {
		t.Errorf("decision %d: login_role = %v, want %s", i, deref(a.LoginRole), want)
	}
}

// deref is for messages: the pointed-to value, or "NULL".
func deref[T any](p *T) any {
	if p == nil {
		return "NULL"
	}
	return *p
}

func TestEveryDecisionRecordsRoleAndRows(t *testing.T) {
	e := setup(t, calm())
	e.seed(t)
	conn := e.connect(t, "a")
	mustExec(t, conn, "update "+e.table+" set title = 'x' where id = 1")
	mustExec(t, conn, "update "+e.table+" set title = 'x' where id = 999")
	if _, err := conn.Exec(context.Background(), "insert into "+e.table+" (id, name) values (1, 'dup')"); sqlstate(err) != "23505" {
		t.Fatalf("duplicate insert: %v, want 23505", err)
	}
	mustExec(t, conn, "insert into "+e.table+" (id, name) values (10, 'a'), (11, 'b')")

	as := e.accounts(t, 4)
	want := []struct {
		verdict string
		reasons []string
		rows    *int64
	}{
		{"commit", nil, ptr(int64(1))},
		{"commit", nil, ptr(int64(0))}, // ran and matched nothing: 0, not NULL
		{"commit", nil, nil},           // refused: it changed nothing it can be charged for
		{"unsupported", []string{"multi_row"}, ptr(int64(2))},
	}
	for i, a := range as {
		wantRole(t, i, a, "stet_scram")
		wantAccount(t, i, a, want[i].verdict, want[i].reasons...)
		wantRows(t, i, a, want[i].rows)
		if a.Cumulative != nil {
			t.Errorf("decision %d: cumulative = %v, want NULL for a table without a cumulative limit", i, a.Cumulative)
		}
	}
}

// A data-modifying CTE's tag is the outer SELECT's row count, and a Query of
// several statements has one tag per statement. Neither is the table's count.
func TestRowsAffectedIsNullWhenTheTagIsNotTheTablesCount(t *testing.T) {
	e := setup(t, calm())
	e.seed(t)
	conn := e.connect(t, "a")
	mustExec(t, conn, "with d as (delete from "+e.table+" where id = 1 returning 1) select count(*) from d")
	mustExec(t, conn, "update "+e.table+" set status = 'x' where id = 2; select 1")

	as := e.accounts(t, 2)
	for i, reason := range []string{"nested_write", "multi_statement"} {
		wantAccount(t, i, as[i], "unsupported", reason)
		wantRole(t, i, as[i], "stet_scram")
		wantRows(t, i, as[i], nil)
		if !as[i].Executed {
			t.Errorf("decision %d: executed = false, want the write to have run", i)
		}
	}
	if n := e.rowsWhere(t, "id = 1 or status = 'x'"); n != 1 {
		t.Errorf("%d rows deleted or updated as asked, want 1: both writes must go through", n)
	}
}

func TestCumulativeHoldsOnceTheWindowIsFull(t *testing.T) {
	d := calm()
	e := setup(t, d, cumYAML)
	e.seedMany(t)
	conn := e.connect(t, "bulk")
	mustExec(t, conn, "update "+e.table+" set status = 'x' where company_id = 9")
	mustExec(t, conn, "update "+e.table+" set status = 'y' where company_id = 9")
	mustExec(t, conn, "update "+e.table+" set title = 'a' where id = 1")
	mustExec(t, conn, "update "+e.table+" set title = 'b' where id = 2")

	as := e.accounts(t, 4)
	for i, w := range []struct {
		used, planned float64
		rows          int64
	}{{0, 2, 2}, {2, 2, 2}, {4, 1, 1}, {5, 1, 1}} {
		wantUsage(t, i, as[i], w.used, w.planned)
		wantRows(t, i, as[i], ptr(w.rows))
		if ws := as[i].Cumulative["window_s"]; ws != 3600.0 {
			t.Errorf("decision %d: cumulative.window_s = %v, want 3600", i, ws)
		}
	}
	wantAccount(t, 0, as[0], "commit")
	wantAccount(t, 1, as[1], "commit")
	wantAccount(t, 2, as[2], "commit")
	wantAccount(t, 3, as[3], "hold", "impact:cumulative")
	// Shadow mode still asks the model, so the limit can be changed and the
	// write rescored later.
	if as[3].Questions == nil || as[3].Answers == nil {
		t.Errorf("held write: questions=%v answers=%v, want both recorded", as[3].Questions, as[3].Answers)
	}
	if n := d.callCount(); n != 2 {
		t.Errorf("model called %d times, want 2: once for each single-row write", n)
	}
	if n := e.rowsWhere(t, "title = 'b'"); n != 1 {
		t.Errorf("%d rows have the held write's title, want 1: shadow mode must not block it", n)
	}
}

func TestCumulativeWindowSlides(t *testing.T) {
	e := setup(t, calm(), shortCumYAML)
	e.seedMany(t)
	conn := e.connect(t, "bulk")
	e.fill(t, conn)
	time.Sleep(2200 * time.Millisecond)
	mustExec(t, conn, "update "+e.table+" set title = 'later' where id = 1")

	as := e.accounts(t, 4)
	if used := as[2].Cumulative["used"]; used != 4.0 {
		t.Errorf("third filling write: used %v, want 4: the window must hold what came before", used)
	}
	wantAccount(t, 3, as[3], "commit")
	wantUsage(t, 3, as[3], 0, 1)
	if ws := as[3].Cumulative["window_s"]; ws != 2.0 {
		t.Errorf("cumulative.window_s = %v, want 2", ws)
	}
}

func TestCumulativeIsPerLoginRole(t *testing.T) {
	e := setup(t, calm(), cumYAML)
	e.seedMany(t)
	mustExec(t, e.admin, "grant all on "+e.table+" to stet_md5")
	e.fill(t, e.connect(t, "scram-agent"))
	mustExec(t, e.connectAs(t, "stet_md5", "md5-pw", "md5-agent"), "update "+e.table+" set title = 'm' where id = 3")
	mustExec(t, e.connect(t, "scram-agent"), "update "+e.table+" set title = 's' where id = 4")

	as := e.accounts(t, 5)
	wantRole(t, 3, as[3], "stet_md5")
	wantAccount(t, 3, as[3], "commit")
	wantUsage(t, 3, as[3], 0, 1)
	// A new connection does not start the role over.
	wantRole(t, 4, as[4], "stet_scram")
	wantAccount(t, 4, as[4], "hold", "impact:cumulative")
	wantUsage(t, 4, as[4], 5, 1)
}

// Writes the same role sends at once on several connections must see each
// other: each is reserved before it is sent.
func TestCumulativeCountsConcurrentWrites(t *testing.T) {
	e := setup(t, calm(), cumYAML)
	e.seedMany(t)
	const n = 8
	conns := make([]*pgx.Conn, n)
	for i := range conns {
		conns[i] = e.connect(t, fmt.Sprintf("worker-%d", i))
	}
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = c.Exec(context.Background(), "update "+e.table+" set title = $1 where company_id = 9", fmt.Sprint("title-", i))
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}

	as := e.accounts(t, n)
	commits := 0
	var used []float64
	for i, a := range as {
		if a.Verdict == "commit" && len(a.Reasons) == 0 {
			commits++
		} else {
			wantAccount(t, i, a, "hold", "impact:cumulative")
		}
		u, _ := a.Cumulative["used"].(float64)
		used = append(used, u)
	}
	if commits > 2 {
		t.Errorf("%d writes committed, want at most 2 of 2 rows each under a limit of 5", commits)
	}
	// Each reservation saw all those made before it, and none saw its own.
	slices.Sort(used)
	if want := []float64{0, 2, 4, 6, 8, 10, 12, 14}; !reflect.DeepEqual(used, want) {
		t.Errorf("used = %v, want %v", used, want)
	}
}

func TestCumulativeSurvivesARestart(t *testing.T) {
	e := setup(t, calm(), cumYAML)
	e.seedMany(t)
	e.fill(t, e.connect(t, "before"))
	var sum int64
	for _, a := range e.accounts(t, 3) {
		if a.RowsAffected != nil {
			sum += *a.RowsAffected
		}
	}
	if sum != 5 {
		t.Fatalf("%d rows recorded before the restart, want 5", sum)
	}

	after := e.restart(t)
	mustExec(t, after.connect(t, "after"), "update "+e.table+" set title = 'r' where id = 3")

	a := e.accounts(t, 4)[3]
	wantAccount(t, 3, a, "hold", "impact:cumulative")
	wantUsage(t, 3, a, 5, 1)
}

// The first writes after a restart arrive together; the window must be read
// once, not once per write and added up.
func TestCumulativeLoadsTheWindowOnceForConcurrentFirstWrites(t *testing.T) {
	e := setup(t, calm(), cumYAML)
	e.seedMany(t)
	e.fill(t, e.connect(t, "before"))
	e.accounts(t, 3)

	after := e.restart(t)
	const n = 4
	conns := make([]*pgx.Conn, n)
	for i := range conns {
		conns[i] = after.connect(t, fmt.Sprintf("after-%d", i))
	}
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = c.Exec(context.Background(), fmt.Sprintf("update %s set title = 'c%d' where id = %d", e.table, i, i+1))
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}

	var used []float64
	for i, a := range e.accounts(t, 3+n)[3:] {
		wantAccount(t, 3+i, a, "hold", "impact:cumulative")
		u, _ := a.Cumulative["used"].(float64)
		used = append(used, u)
	}
	slices.Sort(used)
	if want := []float64{5, 6, 7, 8}; !reflect.DeepEqual(used, want) {
		t.Errorf("used = %v, want %v: the five rows from before the restart once each", used, want)
	}
}

// A failed load is retried by the next write, and what this gate recorded
// meanwhile is not counted twice: it is already in memory.
func TestCumulativeLoadsAgainAfterAFailedLoad(t *testing.T) {
	e := setup(t, calm(), cumYAML)
	e.seedMany(t)
	mustExec(t, e.connect(t, "before"), "update "+e.table+" set status = 'x' where company_id = 9")
	e.accounts(t, 1)

	after := e.restart(t)
	gate.FailUsageLoads(after.gate, 1)
	conn := after.connect(t, "after")
	mustExec(t, conn, "update "+e.table+" set title = 'a' where id = 1")
	first := e.accounts(t, 2)[1] // recorded, so a load would find it
	if want := map[string]any{"error": "usage not loaded: injected failure"}; !reflect.DeepEqual(first.Cumulative, want) {
		t.Errorf("cumulative = %v, want %v", first.Cumulative, want)
	}
	wantAccount(t, 1, first, "commit")
	wantRows(t, 1, first, ptr(int64(1)))

	mustExec(t, conn, "update "+e.table+" set title = 'b' where id = 2")
	// Two rows from before the restart, and the write whose load failed.
	wantUsage(t, 2, e.accounts(t, 3)[2], 3, 1)
}

// Writes Stet does not judge still touch rows, so they count.
func TestCumulativeCountsWritesItDoesNotJudge(t *testing.T) {
	d := calm()
	e := setup(t, d, cumYAML)
	e.seedMany(t)
	conn := e.connect(t, "bulk")
	ctx := context.Background()
	mustExec(t, conn, "insert into "+e.table+" (id, name) values (10, 'a'), (11, 'b') on conflict (id) do update set name = excluded.name")
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "update "+e.table+" set status = 'x' where company_id = 9"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, conn, "update "+e.table+" set status = 'y' where company_id = 9")

	as := e.accounts(t, 3)
	wantAccount(t, 0, as[0], "unsupported", "multi_row")
	wantUsage(t, 0, as[0], 0, 0)
	wantRows(t, 0, as[0], ptr(int64(2)))
	wantAccount(t, 1, as[1], "unsupported", "in_transaction")
	wantUsage(t, 1, as[1], 2, 0)
	wantRows(t, 1, as[1], ptr(int64(2)))
	wantAccount(t, 2, as[2], "hold", "impact:cumulative")
	wantUsage(t, 2, as[2], 4, 2)
	wantNoModel(t, d)
}

func TestCumulativeForgetsFailedWrites(t *testing.T) {
	e := setup(t, calm(), cumYAML)
	e.seed(t)
	conn := e.connect(t, "dup")
	for range 5 {
		if _, err := conn.Exec(context.Background(), "insert into "+e.table+" (id, name) values (1, 'dup')"); sqlstate(err) != "23505" {
			t.Fatalf("duplicate insert: %v, want 23505", err)
		}
	}
	mustExec(t, conn, "update "+e.table+" set title = 'x' where id = 2")

	as := e.accounts(t, 6)
	for i, a := range as[:5] {
		if a.Executed {
			t.Errorf("decision %d: executed, want the duplicate refused", i)
		}
		wantUsage(t, i, a, 0, 1)
	}
	wantAccount(t, 5, as[5], "commit")
	wantUsage(t, 5, as[5], 0, 1)
}

// A write whose connection ends before the server answers may still have
// committed, so its reservation stays.
func TestCumulativeKeepsALostWrite(t *testing.T) {
	e := setup(t, calm(), cumYAML)
	e.seedMany(t)
	mustExec(t, e.admin, "analyze "+e.table) // the join is estimated: company 9 is two of seven rows
	ctx := context.Background()
	lost := e.connect(t, "lost")
	// Exec writes the query before it returns, and the proxy reads the
	// client's bytes in order, so Begin runs before it sees the close.
	lost.PgConn().Exec(ctx, "update "+e.table+" set status = 'x' from pg_sleep(1) where company_id = 9")
	lost.PgConn().Conn().Close()

	first := e.accounts(t, 1)[0] // recorded once the proxy saw the connection end
	if first.Executed {
		t.Errorf("lost write recorded as executed")
	}
	wantUsage(t, 0, first, 0, 2)
	wantRows(t, 0, first, nil)

	mustExec(t, e.connect(t, "next"), "update "+e.table+" set title = 'n' where id = 1")
	wantUsage(t, 1, e.accounts(t, 2)[1], 2, 1)
}

// Postgres serves protocol 3.1 by negotiating it down, but the proxy cannot
// read that startup packet, so the session has no login role to count for.
func TestCumulativeIsNotAppliedWithoutALoginRole(t *testing.T) {
	e := setup(t, calm(), cumYAML)
	e.seedMany(t)
	mustExec(t, e.admin, "grant all on "+e.table+" to stet_trust")
	for i := range 6 {
		sql := fmt.Sprintf("update %s set title = 't%d' where id = %d", e.table, i, i+1)
		if code := e.rawSimpleQuery(t, 3<<16|1, "stet_trust", sql); code != "" {
			t.Fatalf("write %d failed with SQLSTATE %s", i, code)
		}
	}
	mustExec(t, e.connect(t, "known"), "update "+e.table+" set title = 'k' where id = 7")

	as := e.accounts(t, 7)
	for i, a := range as[:6] {
		if a.LoginRole != nil {
			t.Errorf("decision %d: login_role = %s, want NULL", i, *a.LoginRole)
		}
		if want := map[string]any{"error": "no login role"}; !reflect.DeepEqual(a.Cumulative, want) {
			t.Errorf("decision %d: cumulative = %v, want %v", i, a.Cumulative, want)
		}
		if slices.Contains(a.Reasons, "impact:cumulative") {
			t.Errorf("decision %d: %s %v, want the cumulative limit not applied", i, a.Verdict, a.Reasons)
		}
	}
	wantRole(t, 6, as[6], "stet_scram")
	wantUsage(t, 6, as[6], 0, 1)
}

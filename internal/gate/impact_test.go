package gate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stetdb/stet/internal/gate"
)

// impactYAML is tableYAML plus impact limits: at most 3 rows, half the table.
const impactYAML = tableYAML + "    impact: {max_rows: 3, max_share: 0.5}\n"

// looseYAML has a row limit no test reaches. Tests whose number comes from
// the planner's estimate use it, so a rough estimate cannot flip the verdict.
const looseYAML = tableYAML + "    impact: {max_rows: 100}\n"

type impactRow struct {
	Verdict   string
	Reasons   []string
	Executed  bool
	ExecError *string
	Impact    map[string]any // nil when the column is NULL
}

// impacts waits for n decisions on the env's table, oldest first.
func (e *env) impacts(t *testing.T, n int) []impactRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := e.admin.Query(context.Background(), `
			select verdict, reasons, executed, exec_error, impact
			from stet.decisions where target_table = $1 order by created_at`, e.table)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[impactRow])
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

// seedMany adds ids 3..7: three more rows at company 3, two at company 9.
func (e *env) seedMany(t *testing.T) {
	t.Helper()
	e.seed(t) // ids 1 and 2, company 3
	mustExec(t, e.admin, `insert into `+e.table+` (id, name, company_id, status) values
		(3, 'Park', 3, 'active'), (4, 'Choi', 3, 'active'), (5, 'Jung', 3, 'active'),
		(6, 'Han', 9, 'active'), (7, 'Yoon', 9, 'active')`)
}

// rowsWhere counts the env's rows matching cond.
func (e *env) rowsWhere(t *testing.T, cond string) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(), "select count(*) from "+e.table+" where "+cond).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// num reads a number from a recorded impact. JSON numbers decode as float64.
func num(m map[string]any, k string) (float64, bool) {
	f, ok := m[k].(float64)
	return f, ok
}

func wantVerdict(t *testing.T, r impactRow, verdict string, reasons ...string) {
	t.Helper()
	if reasons == nil {
		reasons = []string{}
	}
	if r.Verdict != verdict || !reflect.DeepEqual(r.Reasons, reasons) {
		t.Errorf("verdict %s %v, want %s %v (impact %v)", r.Verdict, r.Reasons, verdict, reasons, r.Impact)
	}
}

// wantImpact checks the given keys of a recorded impact. Numbers are float64.
func wantImpact(t *testing.T, im map[string]any, want map[string]any) {
	t.Helper()
	if im == nil {
		t.Fatalf("impact is NULL, want %v", want)
	}
	for k, v := range want {
		if !reflect.DeepEqual(im[k], v) {
			t.Errorf("impact.%s = %v, want %v (impact %v)", k, im[k], v, im)
		}
	}
}

func wantNoKeys(t *testing.T, im map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if v, ok := im[k]; ok {
			t.Errorf("impact.%s = %v, want no such key (impact %v)", k, v, im)
		}
	}
}

func wantNoModel(t *testing.T, d *fakeDecider) {
	t.Helper()
	if n := d.callCount(); n != 0 {
		t.Errorf("model called %d times for a measured write", n)
	}
}

func TestImpactCountsBeforeTheWrite(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	mustExec(t, e.connect(t, "bulk"), "update "+e.table+" set status = 'x' where company_id = 9")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "commit")
	wantImpact(t, r.Impact, map[string]any{"method": "count", "rows": 2.0, "cap": 4.0, "actual_rows": 2.0})
	wantNoKeys(t, r.Impact, "capped", "error")
	if _, ok := num(r.Impact, "estimate"); !ok {
		t.Errorf("impact has no estimate: %v", r.Impact)
	}
	if _, ok := num(r.Impact, "measure_ms"); !ok {
		t.Errorf("impact has no measure_ms: %v", r.Impact)
	}
	if n := e.rowsWhere(t, "status = 'x'"); n != 2 || !r.Executed {
		t.Errorf("%d rows changed, executed=%v: the write must go through", n, r.Executed)
	}
	wantNoModel(t, d)
}

func TestImpactHoldsOverTheRowLimit(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	mustExec(t, e.admin, "analyze "+e.table)
	mustExec(t, e.connect(t, "bulk"), "update "+e.table+" set status = 'x' where company_id = 3")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "hold", "impact:rows")
	wantImpact(t, r.Impact, map[string]any{"method": "count", "rows": 4.0, "capped": true, "cap": 4.0, "table_rows": 7.0, "actual_rows": 5.0})
	// rows is the cap, not the count: a share of it would not be the write's.
	wantNoKeys(t, r.Impact, "share")
	if n := e.rowsWhere(t, "status = 'x'"); n != 5 || !r.Executed {
		t.Errorf("%d rows changed, executed=%v: shadow mode must not block the write", n, r.Executed)
	}
	wantNoModel(t, d)
}

func TestImpactHoldsOverTheShareLimit(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seed(t)
	mustExec(t, e.admin, "analyze "+e.table)
	mustExec(t, e.connect(t, "bulk"), "delete from "+e.table+" where company_id = 3") // 2 of 2 rows

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "hold", "impact:share")
	wantImpact(t, r.Impact, map[string]any{"method": "count", "rows": 2.0, "table_rows": 2.0, "share": 1.0})
	wantNoModel(t, d)
}

func TestImpactSkipsShareForAnUnanalyzedTable(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seed(t)
	mustExec(t, e.connect(t, "bulk"), "delete from "+e.table+" where company_id = 3")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "commit")
	wantImpact(t, r.Impact, map[string]any{"method": "count", "rows": 2.0})
	wantNoKeys(t, r.Impact, "table_rows", "share")
	wantNoModel(t, d)
}

func TestImpactBindsParameters(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	// The count query keeps only the WHERE clause, so $1 goes unused there.
	if _, err := e.connect(t, "bulk").Exec(context.Background(),
		"update "+e.table+" set title = $1 where company_id = $2", "CTO", 9); err != nil {
		t.Fatal(err)
	}

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "commit")
	wantImpact(t, r.Impact, map[string]any{"method": "count", "rows": 2.0, "actual_rows": 2.0})
	wantNoModel(t, d)
}

func TestImpactCountsNullParameterAsNoRows(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	if _, err := e.connect(t, "bulk").Exec(context.Background(),
		"update "+e.table+" set status = 'x' where company_id = $1", nil); err != nil {
		t.Fatal(err)
	}

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "commit")
	wantImpact(t, r.Impact, map[string]any{"method": "count", "rows": 0.0, "actual_rows": 0.0})
	wantNoModel(t, d)
}

func TestImpactEstimateOnlyEscalates(t *testing.T) {
	d := calm()
	e := setup(t, d, looseYAML)
	e.seedMany(t)
	mustExec(t, e.admin, "analyze "+e.table)
	mustExec(t, e.connect(t, "bulk"), "update "+e.table+" set status = 'x' where company_id in (select 9)")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "escalate", "impact:uncertain")
	wantImpact(t, r.Impact, map[string]any{"method": "estimate", "actual_rows": 2.0})
	wantNoKeys(t, r.Impact, "cap", "capped")
	if rows, ok := num(r.Impact, "rows"); !ok || rows != r.Impact["estimate"] {
		t.Errorf("rows = %v, want the estimate %v", r.Impact["rows"], r.Impact["estimate"])
	}
	wantNoModel(t, d)
}

func TestImpactReadsInsertedRowsFromTheStatement(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	conn := e.connect(t, "bulk")
	mustExec(t, conn, "insert into "+e.table+" (id, name) values (10, 'a'), (11, 'b')")
	mustExec(t, conn, "insert into "+e.table+" (id, name) values (20, 'a'), (21, 'b'), (22, 'c'), (23, 'd')")

	rs := e.impacts(t, 2)
	wantVerdict(t, rs[0], "commit")
	wantImpact(t, rs[0].Impact, map[string]any{"method": "syntax", "rows": 2.0, "actual_rows": 2.0})
	wantVerdict(t, rs[1], "hold", "impact:rows")
	wantImpact(t, rs[1].Impact, map[string]any{"method": "syntax", "rows": 4.0, "actual_rows": 4.0})
	wantNoModel(t, d)
}

func TestImpactRecordsCascades(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	child := e.table + "_child"
	mustExec(t, e.admin, "create table "+child+" (id int primary key, parent int references "+e.table+"(id) on delete cascade)")
	t.Cleanup(func() { e.admin.Exec(context.Background(), "drop table "+child) }) // before the parent's
	e.seedMany(t)
	mustExec(t, e.connect(t, "bulk"), "delete from "+e.table+" where company_id = 9")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "commit")
	wantImpact(t, r.Impact, map[string]any{
		"method":   "count",
		"rows":     2.0,
		"cascades": []any{map[string]any{"table": child, "on_delete": "cascade"}},
	})
	wantNoModel(t, d)
}

func TestImpactIsNotMeasuredInsideATransaction(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	ctx := context.Background()
	tx, err := e.connect(t, "tx-agent").Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "update "+e.table+" set status = 'x' where company_id = 9"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "unsupported", "in_transaction")
	if r.Impact != nil {
		t.Errorf("impact = %v, want NULL: a write in a transaction is not measured", r.Impact)
	}
	wantNoModel(t, d)
}

// Several Executes under one Sync run as one implicit transaction, which
// Begin cannot see coming. The write is measured; only the verdict changes.
func TestImpactIsRecordedInAnImplicitTransaction(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pl := e.connect(t, "batcher").PgConn().StartPipeline(ctx)
	pl.SendQueryParams("update "+e.table+" set status = 'x' where company_id = 9", nil, nil, nil, nil)
	pl.SendQueryParams("select 1", nil, nil, nil, nil)
	pl.SendPipelineSync()
	if err := pl.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := pl.Close(); err != nil {
		t.Fatal(err)
	}

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "unsupported", "in_transaction")
	wantImpact(t, r.Impact, map[string]any{"method": "count", "rows": 2.0, "actual_rows": 2.0})
	wantNoModel(t, d)
}

// A client that pipelines COMMIT and an autocommit write makes Begin see the
// transaction still open, so it skips measuring, while End finds the write
// ran on its own. That is recorded as a measurement that failed.
func TestImpactRecordsAWriteSkippedOnAStaleTransactionStatus(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	conn := e.connect(t, "pipeliner")
	mustExec(t, conn, "begin")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pl := conn.PgConn().StartPipeline(ctx)
	// The sleep holds back the server's answers, so the write reaches the
	// proxy while the last status it saw is still "in a transaction".
	pl.SendQueryParams("select pg_sleep(0.2)", nil, nil, nil, nil)
	pl.SendPipelineSync()
	pl.SendQueryParams("commit", nil, nil, nil, nil)
	pl.SendPipelineSync()
	pl.SendQueryParams("update "+e.table+" set status = 'x' where company_id = 9", nil, nil, nil, nil)
	pl.SendPipelineSync()
	if err := pl.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := pl.Close(); err != nil {
		t.Fatal(err)
	}

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "unsupported", "not_single_row")
	wantImpact(t, r.Impact, map[string]any{"error": "not measured", "actual_rows": 2.0})
	wantNoKeys(t, r.Impact, "method", "rows")
	wantNoModel(t, d)
}

func TestWithoutImpactLimitsNothingChanges(t *testing.T) {
	d := calm()
	e := setup(t, d)
	e.seedMany(t)
	mustExec(t, e.connect(t, "bulk"), "update "+e.table+" set status = 'x' where company_id = 9")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "unsupported", "not_single_row")
	if r.Impact != nil {
		t.Errorf("impact = %v, want NULL for a table without impact limits", r.Impact)
	}
	wantNoModel(t, d)
}

// connectAs opens a client connection through the proxy as another role.
func (e *env) connectAs(t *testing.T, user, password, agent string) *pgx.Conn {
	t.Helper()
	u := url.URL{
		Scheme: "postgres", Host: e.addr, Path: "/stet_test", User: url.User(user),
		RawQuery: url.Values{"sslmode": {"disable"}, "application_name": {agent}}.Encode(),
	}
	if password != "" {
		u.User = url.UserPassword(user, password)
	}
	conn, err := pgx.Connect(context.Background(), u.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", user, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// superuser connects straight to the test database as postgres
// (password postgres-pw, set by scripts/testdb.sh).
func superuser(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(),
		"postgres://postgres:postgres-pw@"+os.Getenv("STET_TEST_PG")+"/stet_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// sqlstate is err's SQLSTATE: "" for no error, the error's text for one that
// did not come from Postgres.
func sqlstate(err error) string {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &pgErr):
		return pgErr.Code
	}
	return err.Error()
}

// wantCleanPool checks every connection in the env's pool: none may be left
// in a transaction or as the client's role by a measurement.
func (e *env) wantCleanPool(t *testing.T) {
	t.Helper()
	e.gate.Close() // judgments hold pooled connections while they record
	ctx := context.Background()
	conns := e.pool.AcquireAllIdle(ctx)
	defer func() {
		for _, c := range conns {
			c.Release()
		}
	}()
	if total := e.pool.Stat().TotalConns(); len(conns) == 0 || int32(len(conns)) != total {
		t.Fatalf("%d of %d pooled connections are idle, want all of at least one", len(conns), total)
	}
	for i, c := range conns {
		if s := c.Conn().PgConn().TxStatus(); s != 'I' {
			t.Errorf("connection %d: transaction status %q, want idle", i, s)
		}
		var user, timeout string
		if err := c.QueryRow(ctx, "select current_user, current_setting('statement_timeout')").Scan(&user, &timeout); err != nil {
			t.Fatal(err)
		}
		if user != "stet_scram" || timeout != "0" {
			t.Errorf("connection %d: current_user %s, statement_timeout %s; want stet_scram, 0", i, user, timeout)
		}
	}
}

// lockedBuffer collects log output written from several goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestImpactCountsWhatTheClientsRoleCanSee(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	mustExec(t, superuser(t), "grant stet_md5 to stet_scram") // so Stet can SET ROLE stet_md5
	e.seedMany(t)
	mustExec(t, e.admin, "grant all on "+e.table+" to stet_md5")
	mustExec(t, e.admin, "alter table "+e.table+" enable row level security")
	mustExec(t, e.admin, "create policy p on "+e.table+" for all to stet_md5 using (company_id = 9)")
	// All seven rows are active. Counted as the table's owner, the write
	// would be capped at 4 and held; stet_md5 sees only company 9's two.
	mustExec(t, e.connectAs(t, "stet_md5", "md5-pw", "tenant-9"),
		"update "+e.table+" set status = 'x' where status = 'active'")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "commit")
	wantImpact(t, r.Impact, map[string]any{"method": "count", "rows": 2.0, "actual_rows": 2.0})
	wantNoKeys(t, r.Impact, "capped", "error")
	wantNoModel(t, d)
	e.wantCleanPool(t)
}

func TestImpactFailsWhenTheRoleCannotBeAssumed(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	var member bool
	if err := e.admin.QueryRow(context.Background(),
		"select pg_has_role('stet_scram', 'stet_trust', 'member')").Scan(&member); err != nil || member {
		t.Fatalf("stet_scram is a member of stet_trust (%v); this test needs it not to be", err)
	}
	e.seedMany(t)
	mustExec(t, e.admin, "grant all on "+e.table+" to stet_trust")
	mustExec(t, e.connectAs(t, "stet_trust", "", "outsider"),
		"update "+e.table+" set status = 'x' where company_id = 9")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "unsupported", "not_single_row")
	if msg, _ := r.Impact["error"].(string); !strings.Contains(msg, "set role") {
		t.Errorf("impact.error = %q, want the SET ROLE failure (impact %v)", msg, r.Impact)
	}
	wantNoKeys(t, r.Impact, "method")
	if n := e.rowsWhere(t, "status = 'x'"); n != 2 || !r.Executed {
		t.Errorf("%d rows changed, executed=%v: the write must go through", n, r.Executed)
	}
	wantNoModel(t, d)
	e.wantCleanPool(t)
}

func TestImpactWarnsOncePerRoleItCannotAssume(t *testing.T) {
	e := setup(t, calm(), impactYAML)
	var logs lockedBuffer
	gate.SetLogger(e.gate, slog.New(slog.NewJSONHandler(&logs, nil)))
	e.seedMany(t)
	mustExec(t, e.admin, "grant all on "+e.table+" to stet_trust")
	for _, agent := range []string{"first", "second"} {
		mustExec(t, e.connectAs(t, "stet_trust", "", agent),
			"update "+e.table+" set status = 'x' where company_id = 9")
	}
	e.impacts(t, 2)

	var warnings []map[string]any
	for line := range strings.Lines(logs.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["level"] == "WARN" && rec["role"] == "stet_trust" {
			warnings = append(warnings, rec)
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("%d warnings for stet_trust, want 1:\n%s", len(warnings), logs.String())
	}
	if fix := warnings[0]["fix"]; fix != `GRANT "stet_trust" TO "stet_scram"` {
		t.Errorf("fix = %v, want the GRANT that lets Stet assume the role", fix)
	}
}

func TestImpactNeverCountsThroughAnUnlistedFunction(t *testing.T) {
	d := calm()
	e := setup(t, d, looseYAML)
	e.seedMany(t)
	mustExec(t, e.admin, "analyze "+e.table)
	mustExec(t, e.connect(t, "sneaky"), "update "+e.table+
		" set status = 'x' where set_config('stet.marker', 'ran', false) is not null and company_id = 9")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "escalate", "impact:uncertain")
	wantImpact(t, r.Impact, map[string]any{"method": "estimate", "actual_rows": 2.0})
	wantNoKeys(t, r.Impact, "cap", "capped", "error") // no cap: counting was never tried
	wantNoModel(t, d)
}

// A session-level advisory lock outlives the measurement's rollback, so it
// shows where the predicate ran: on the client's connection, never on Stet's.
func TestImpactNeverRunsAnUnlistedFunctionOnStetsConnection(t *testing.T) {
	d := calm()
	e := setup(t, d, looseYAML)
	e.seedMany(t)
	ctx := context.Background()
	key := time.Now().UnixNano() % math.MaxInt32
	conn := e.connect(t, "sneaky")
	var client int32
	if err := conn.QueryRow(ctx, "select pg_backend_pid()").Scan(&client); err != nil {
		t.Fatal(err)
	}
	mustExec(t, conn, fmt.Sprintf("update %s set status = 'x' where pg_try_advisory_lock(%d) and company_id = 9", e.table, key))

	r := e.impacts(t, 1)[0]
	wantImpact(t, r.Impact, map[string]any{"method": "estimate", "actual_rows": 2.0})
	var holders []int32
	rows, err := e.admin.Query(ctx,
		"select pid from pg_locks where locktype = 'advisory' and classid = 0 and objid::bigint = $1 and objsubid = 1", key)
	if err != nil {
		t.Fatal(err)
	}
	if holders, err = pgx.CollectRows(rows, pgx.RowTo[int32]); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(holders, []int32{client}) {
		t.Errorf("advisory lock held by %v, want only the client's backend %d", holders, client)
	}
	wantNoModel(t, d)
}

// advisoryHolders lists the backends holding the session advisory lock key.
func (e *env) advisoryHolders(t *testing.T, key int64) []int32 {
	t.Helper()
	rows, err := e.admin.Query(context.Background(),
		"select pid from pg_locks where locktype = 'advisory' and classid = 0 and objid::bigint = $1 and objsubid = 1 order by pid", key)
	if err != nil {
		t.Fatal(err)
	}
	holders, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		t.Fatal(err)
	}
	return holders
}

// Postgres resolves (x).f to f(x), so this predicate calls an unlisted
// function without call syntax. Counted on Stet's connection, it would leave
// the lock held there and the client's own write would then find it taken.
func TestImpactNeverRunsAFunctionReachedThroughFieldNotation(t *testing.T) {
	d := calm()
	e := setup(t, d, looseYAML)
	e.seedMany(t)
	mustExec(t, e.admin, "analyze "+e.table)
	twin := e.table + "_twin"
	mustExec(t, e.admin, "create table "+twin+" (like "+e.table+" including all)")
	t.Cleanup(func() { e.admin.Exec(context.Background(), "drop table "+twin) })
	mustExec(t, e.admin, "insert into "+twin+" select * from "+e.table)
	ctx := context.Background()
	const write = "update %s set status = 'x' where ((company_id + %d)::bigint).pg_try_advisory_lock and company_id = 9"

	// Sent directly first, with other keys, and its locks released.
	key := time.Now().UnixNano() % (math.MaxInt32 - 2000)
	want, err := e.admin.Exec(ctx, fmt.Sprintf(write, twin, key+1000))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.admin, "select pg_advisory_unlock_all()")

	conn := e.connect(t, "sneaky")
	var client int32
	if err := conn.QueryRow(ctx, "select pg_backend_pid()").Scan(&client); err != nil {
		t.Fatal(err)
	}
	got, err := conn.Exec(ctx, fmt.Sprintf(write, e.table, key))
	if err != nil {
		t.Fatal(err)
	}

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "escalate", "impact:uncertain")
	wantImpact(t, r.Impact, map[string]any{"method": "estimate", "actual_rows": 2.0})
	wantNoKeys(t, r.Impact, "cap", "capped", "error") // no cap: counting was never tried
	for _, company := range []int64{3, 9} {
		if h := e.advisoryHolders(t, key+company); len(h) > 0 && !reflect.DeepEqual(h, []int32{client}) {
			t.Errorf("advisory lock %d held by %v, want only the client's backend %d", key+company, h, client)
		}
	}
	if got.String() != want.String() || want.String() != "UPDATE 2" {
		t.Errorf("%q through Stet, %q directly; want UPDATE 2 both ways", got, want)
	}
	wantNoModel(t, d)
	e.wantCleanPool(t)
}

func TestImpactFallsBackToTheEstimateWhenCountingIsSlow(t *testing.T) {
	d := calm()
	e := setup(t, d, looseYAML)
	defer gate.SetCountTimeout(time.Millisecond)()
	mustExec(t, e.admin, "insert into "+e.table+" (id, company_id) select g, 1 from generate_series(100, 200100) g")
	mustExec(t, e.admin, "analyze "+e.table)
	// No row matches and company_id has no index: counting scans every row.
	mustExec(t, e.connect(t, "bulk"), "update "+e.table+" set status = 'x' where company_id = 424242")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "escalate", "impact:uncertain")
	wantImpact(t, r.Impact, map[string]any{"method": "estimate", "cap": 101.0, "actual_rows": 0.0}) // a cap: counting was tried
	wantNoKeys(t, r.Impact, "capped", "error")
	wantNoModel(t, d)
	e.wantCleanPool(t)
}

// Stet's connection cannot plan a statement that reads the client's
// temporary table. The write runs; the measurement fails.
func TestImpactFailureKeepsTheOldReason(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	conn := e.connect(t, "temp-user")
	mustExec(t, conn, "create temp table tmp_ids (id int)")
	mustExec(t, conn, "insert into tmp_ids values (9)")
	mustExec(t, conn, "update "+e.table+" set status = 'x' where company_id in (select id from tmp_ids)")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "unsupported", "subquery")
	if msg, _ := r.Impact["error"].(string); !strings.Contains(msg, "tmp_ids") {
		t.Errorf("impact.error = %q, want the failed estimate (impact %v)", msg, r.Impact)
	}
	wantNoKeys(t, r.Impact, "method")
	wantImpact(t, r.Impact, map[string]any{"actual_rows": 2.0})
	if !r.Executed {
		t.Error("executed = false: the write must go through")
	}
	wantNoModel(t, d)
}

// Binary bind values are turned into text on Stet's connection, which
// cannot see the client's temporary table either. The write is then not
// measured, and the record says why.
func TestImpactRecordsWhyBindValuesWereNotRead(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	conn := e.connect(t, "temp-user")
	mustExec(t, conn, "create temp table tmp_ids (id int)")
	mustExec(t, conn, "insert into tmp_ids values (9)")
	mustExec(t, conn, "update "+e.table+" set status = 'x' where company_id in (select id from tmp_ids where id = $1)",
		9) // pgx sends an int as a binary value

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "unsupported", "subquery")
	if msg, _ := r.Impact["error"].(string); !strings.HasPrefix(msg, "bind values: ") {
		t.Errorf("impact.error = %q, want why the bind values were not read (impact %v)", msg, r.Impact)
	}
	wantNoKeys(t, r.Impact, "method")
	wantImpact(t, r.Impact, map[string]any{"actual_rows": 2.0})
	wantNoModel(t, d)
}

// Binary bind values are decoded by Postgres, on Stet's connection, with the
// types the statement gives its parameters. A domain's CHECK runs as part of
// decoding. Here it tries to write, which must fail there: decoding is
// read-only. On the client's own connection it writes as usual.
func TestBindValuesAreDecodedReadOnly(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	ctx := context.Background()
	sink, check, dom := e.table+"_sink", e.table+"_check", e.table+"_dom"
	mustExec(t, e.admin, "create table "+sink+" (pid int)")
	mustExec(t, e.admin, "create function "+check+"(int) returns boolean language plpgsql as $$ begin insert into "+sink+" values (pg_backend_pid()); return true; end $$")
	mustExec(t, e.admin, "create domain "+dom+" as int check ("+check+"(value))")
	t.Cleanup(func() {
		e.admin.Exec(context.Background(), "drop domain "+dom+"; drop function "+check+"(int); drop table "+sink)
	})
	twin := e.table + "_twin"
	mustExec(t, e.admin, "create table "+twin+" (like "+e.table+" including all)")
	t.Cleanup(func() { e.admin.Exec(context.Background(), "drop table "+twin) })
	mustExec(t, e.admin, "insert into "+twin+" select * from "+e.table)

	// No type from the client, so the server types $1 as the domain, and a
	// binary int4 9.
	run := func(c *pgx.Conn, table string) (pgconn.CommandTag, error) {
		r := c.PgConn().ExecParams(ctx, "update "+table+" set status = 'x' where company_id = $1::"+dom,
			[][]byte{{0, 0, 0, 9}}, []uint32{0}, []int16{1}, nil).Read()
		return r.CommandTag, r.Err
	}
	conn := e.connect(t, "typed")
	var client int32
	if err := conn.QueryRow(ctx, "select pg_backend_pid()").Scan(&client); err != nil {
		t.Fatal(err)
	}
	got, gotErr := run(conn, e.table)

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "unsupported", "not_single_row")
	if msg, _ := r.Impact["error"].(string); !strings.HasPrefix(msg, "bind values: ") || !strings.Contains(msg, "read-only transaction") {
		t.Errorf("impact.error = %q, want the bind values refused as read-only (impact %v)", msg, r.Impact)
	}
	wantNoKeys(t, r.Impact, "method")
	wantImpact(t, r.Impact, map[string]any{"actual_rows": 2.0})
	rows, err := e.admin.Query(ctx, "select pid from "+sink)
	if err != nil {
		t.Fatal(err)
	}
	writers, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(writers, []int32{client}) {
		t.Errorf("the domain's check wrote from backends %v, want only the client's %d", writers, client)
	}

	want, wantErr := run(e.admin, twin)
	if got.String() != want.String() || sqlstate(gotErr) != sqlstate(wantErr) || want.String() != "UPDATE 2" {
		t.Errorf("%q %q through Stet, %q %q directly; want UPDATE 2 both ways", got, sqlstate(gotErr), want, sqlstate(wantErr))
	}
	wantNoModel(t, d)
	e.wantCleanPool(t)
}

func TestImpactIsRecordedWhenTheWriteFails(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seed(t)
	_, err := e.connect(t, "bulk").Exec(context.Background(),
		"insert into "+e.table+" (id, name) values (1, 'dup'), (50, 'new')")
	if code := sqlstate(err); code != "23505" {
		t.Fatalf("client got %v, want the unique violation 23505", err)
	}

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "commit")
	wantImpact(t, r.Impact, map[string]any{"method": "syntax", "rows": 2.0})
	wantNoKeys(t, r.Impact, "actual_rows")
	if r.Executed || r.ExecError == nil || !strings.Contains(*r.ExecError, "23505") {
		t.Errorf("executed=%v exec_error=%v, want the unique violation", r.Executed, r.ExecError)
	}
	wantNoModel(t, d)
}

func TestImpactMeasuresConcurrentWrites(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	const n = 8
	conns := make([]*pgx.Conn, n)
	for i := range conns {
		conns[i] = e.connect(t, fmt.Sprintf("worker-%d", i))
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.Exec(context.Background(), "update "+e.table+" set title = $1 where company_id = 9", fmt.Sprint("title-", i))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}

	rs := e.impacts(t, n)
	if len(rs) != n {
		t.Fatalf("%d decisions, want %d", len(rs), n)
	}
	for _, r := range rs {
		wantVerdict(t, r, "commit")
		wantImpact(t, r.Impact, map[string]any{"method": "count", "rows": 2.0, "actual_rows": 2.0})
	}
	wantNoModel(t, d)
}

func TestImpactCountsThroughAliasOnlyAndSchema(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	// $1 goes unused in the count; pgx sends $2 as binary and $3 as text.
	if _, err := e.connect(t, "bulk").Exec(context.Background(),
		"update only public."+e.table+" as c set title = $1 where c.company_id = $2 and c.status = $3",
		"CTO", 9, "active"); err != nil {
		t.Fatal(err)
	}

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "commit")
	wantImpact(t, r.Impact, map[string]any{"method": "count", "rows": 2.0, "actual_rows": 2.0})
	wantNoModel(t, d)
}

// The config key names the table without a schema. The statement names it in
// a schema that is not on Stet's search_path, and public holds a decoy of the
// same name: the env's own table. Size and cascades must be the named table's.
func TestImpactReadsTheCatalogOfTheTableTheStatementNames(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t) // the decoy: seven rows, no foreign keys
	// Mixed case, so every reference must quote it.
	schema := `"Impact_` + strings.TrimPrefix(e.table, "contacts_") + `"`
	target, child := schema+"."+e.table, schema+"."+e.table+"_child"
	mustExec(t, e.admin, "create schema "+schema)
	t.Cleanup(func() { e.admin.Exec(context.Background(), "drop schema "+schema+" cascade") })
	mustExec(t, e.admin, "create table "+target+" (like "+e.table+" including all)")
	mustExec(t, e.admin, "create table "+child+" (id int primary key, parent int references "+target+"(id) on delete cascade)")
	mustExec(t, e.admin, "insert into "+target+" (id, name, company_id) values (1, 'a', 9), (2, 'b', 9)")
	mustExec(t, e.admin, "analyze "+target)
	mustExec(t, e.admin, "analyze "+e.table)
	mustExec(t, e.connect(t, "bulk"), "delete from "+target+" where company_id = 9") // 2 of 2 rows

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "hold", "impact:share")
	wantImpact(t, r.Impact, map[string]any{
		"method":      "count",
		"rows":        2.0,
		"table_rows":  2.0,
		"share":       1.0,
		"cascades":    []any{map[string]any{"table": child, "on_delete": "cascade"}},
		"actual_rows": 2.0,
	})
	if n := e.rowsWhere(t, "true"); n != 7 {
		t.Errorf("the decoy has %d rows, want its 7 untouched", n)
	}
	wantNoModel(t, d)
}

// The estimate covers shapes counting cannot. A write to another table
// inside WITH is planned as an InitPlan and not added to the rows.
func TestImpactEstimatesWhatItCannotCount(t *testing.T) {
	d := calm()
	e := setup(t, d, looseYAML)
	e.seedMany(t)
	other := e.table + "_other"
	mustExec(t, e.admin, "create table "+other+" (id int)")
	t.Cleanup(func() { e.admin.Exec(context.Background(), "drop table "+other) })
	mustExec(t, e.admin, "insert into "+other+" select g from generate_series(1, 1000) g")
	mustExec(t, e.admin, "analyze "+e.table)
	mustExec(t, e.admin, "analyze "+other)
	conn := e.connect(t, "bulk")
	for _, sql := range []string{
		"update %[1]s set title = 'x' from (values (9)) v(id) where %[1]s.company_id = v.id",
		"with d as (delete from %[2]s returning id) update %[1]s set status = 'x' where company_id = 9",
		"insert into %[1]s (id, name) select id + 100, name from %[1]s where company_id = 9",
	} {
		mustExec(t, conn, fmt.Sprintf(sql, e.table, other))
	}

	rs := e.impacts(t, 3)
	for _, r := range rs {
		wantVerdict(t, r, "escalate", "impact:uncertain")
		wantImpact(t, r.Impact, map[string]any{"method": "estimate", "actual_rows": 2.0})
	}
	// Analyzed, company 9 is two of seven rows; the CTE's 1000 are not added.
	wantImpact(t, rs[1].Impact, map[string]any{"rows": 2.0})
	wantNoModel(t, d)
}

// A function marked immutable runs while the statement is planned, so the
// estimate can run it on Stet's connection (spec 4). The read-only
// transaction stops what it writes.
func TestImpactMeasuresInAReadOnlyTransaction(t *testing.T) {
	d := calm()
	e := setup(t, d, looseYAML)
	e.seedMany(t)
	sink, write, folded := e.table+"_sink", e.table+"_write", e.table+"_folded"
	mustExec(t, e.admin, "create table "+sink+" (n int)")
	mustExec(t, e.admin, "create function "+write+"() returns int language plpgsql as $$ begin insert into "+sink+" values (1); return 1; end $$")
	mustExec(t, e.admin, "create function "+folded+"() returns int language plpgsql immutable as $$ begin return "+write+"(); end $$")
	t.Cleanup(func() {
		e.admin.Exec(context.Background(), "drop function "+folded+"(); drop function "+write+"(); drop table "+sink)
	})
	mustExec(t, e.connect(t, "sneaky"), "update "+e.table+" set status = 'x' where company_id = 9 and "+folded+"() = 1")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "unsupported", "not_single_row")
	if msg, _ := r.Impact["error"].(string); !strings.Contains(msg, "read-only transaction") {
		t.Errorf("impact.error = %q, want the write refused as read-only (impact %v)", msg, r.Impact)
	}
	wantImpact(t, r.Impact, map[string]any{"actual_rows": 2.0})
	if n := e.rowsWhere(t, "status = 'x'"); n != 2 || !r.Executed {
		t.Errorf("%d rows changed, executed=%v: the write must go through", n, r.Executed)
	}
	var logged int
	if err := e.admin.QueryRow(context.Background(), "select count(*) from "+sink).Scan(&logged); err != nil || logged != 1 {
		t.Errorf("%d rows logged (%v), want 1: the client's own planning", logged, err)
	}
	wantNoModel(t, d)
}

// twinStatements are sent to the gated table through the proxy and to its
// twin directly. %[1]s is the table.
var twinStatements = []string{
	"update %[1]s set status = 'x' where company_id = 3",
	"delete from %[1]s where company_id = 424242",
	"insert into %[1]s (id, name) values (1, 'dup'), (60, 'new')",
	"update %[1]s set title = 'y' where company_id in (select 9)",
	"insert into %[1]s (id, name) select id + 100, name from %[1]s where company_id = 9",
	"delete from %[1]s",
}

// Shadow mode must not change a write (rule 6): each statement gets the same
// command tag or error, and leaves the same rows, through Stet as without it.
func TestWritesBehaveTheSameWithImpactOn(t *testing.T) {
	protocols := map[string]func(context.Context, *pgx.Conn, string) (pgconn.CommandTag, error){
		"simple": func(ctx context.Context, c *pgx.Conn, sql string) (pgconn.CommandTag, error) {
			return c.Exec(ctx, sql) // no arguments: pgx sends a Simple Query
		},
		"extended": func(ctx context.Context, c *pgx.Conn, sql string) (pgconn.CommandTag, error) {
			r := c.PgConn().ExecParams(ctx, sql, nil, nil, nil, nil).Read()
			return r.CommandTag, r.Err
		},
	}
	for name, run := range protocols {
		t.Run(name, func(t *testing.T) {
			e := setup(t, calm(), impactYAML)
			e.seedMany(t)
			twin := e.table + "_twin"
			mustExec(t, e.admin, "create table "+twin+" (like "+e.table+" including all)")
			t.Cleanup(func() { e.admin.Exec(context.Background(), "drop table "+twin) })
			mustExec(t, e.admin, "insert into "+twin+" select * from "+e.table)
			client := e.connect(t, "twin")
			ctx := context.Background()

			for i, s := range twinStatements {
				tag, err := run(ctx, client, fmt.Sprintf(s, e.table))
				wantTag, wantErr := run(ctx, e.admin, fmt.Sprintf(s, twin))
				if tag.String() != wantTag.String() || sqlstate(err) != sqlstate(wantErr) {
					t.Errorf("statement %d: %q %q through Stet, %q %q directly",
						i, tag, sqlstate(err), wantTag, sqlstate(wantErr))
				}
				if got, want := e.contents(t, e.table), e.contents(t, twin); !reflect.DeepEqual(got, want) {
					t.Errorf("statement %d: rows differ\nthrough Stet %v\ndirectly     %v", i, got, want)
				}
			}
			// Every statement was measured, so the comparison covers measuring.
			for i, r := range e.impacts(t, len(twinStatements)) {
				if r.Impact == nil {
					t.Errorf("statement %d was not measured", i)
				}
			}
		})
	}
}

// contents is every row of table, by id.
func (e *env) contents(t *testing.T, table string) [][]any {
	t.Helper()
	rows, err := e.admin.Query(context.Background(), "select * from "+table+" order by id")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([]any, error) { return r.Values() })
	if err != nil {
		t.Fatal(err)
	}
	return got
}

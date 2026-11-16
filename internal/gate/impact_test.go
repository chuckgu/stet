package gate_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	mustExec(t, e.connect(t, "bulk"), "update "+e.table+" set status = 'x' where company_id = 3")

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "hold", "impact:rows")
	wantImpact(t, r.Impact, map[string]any{"method": "count", "rows": 4.0, "capped": true, "cap": 4.0, "actual_rows": 5.0})
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

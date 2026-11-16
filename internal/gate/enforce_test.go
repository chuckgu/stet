package gate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/proxy"
)

// enforcePrefix turns a config into enforce mode, with stet_md5 as the
// agents' login role.
const enforcePrefix = "mode: enforce\nagents: {roles: [stet_md5]}\n"

// enforceYAML is tableYAML in enforce mode.
const enforceYAML = enforcePrefix + tableYAML

// trustPrefix turns a config into enforce mode with stet_trust, a role Stet
// is not a member of, as the agents' login role.
const trustPrefix = "mode: enforce\nagents: {roles: [stet_trust]}\n"

// brokenYAML is an enforce config whose candidate query fails, so no write
// that runs it has a state.
const brokenYAML = enforcePrefix + `
listen: 127.0.0.1:0
upstream: postgres://stet_scram@%[1]s/stet_test
model: {adapter: fake, version: fake-1, timeout_ms: 2000}
tables:
  %[2]s:
    pk: id
    send_fields: [name, email]
    candidates:
      sql: select id from %[2]s where no_such_column = :email
`

// passOnFailure sets model.on_failure: pass in a config built on tableYAML
// or brokenYAML.
func passOnFailure(format string) string {
	const model = "model: {adapter: fake, version: fake-1, timeout_ms: 2000}"
	if !strings.Contains(format, model) {
		panic("passOnFailure: no model line to change")
	}
	return strings.Replace(format, model, "model: {adapter: fake, version: fake-1, timeout_ms: 2000, on_failure: pass}", 1)
}

// setupEnforce is setup for configs whose agents are stet_md5: Stet may act
// as that role to measure, and the role may write the table. The table's
// held writes are removed when the test ends.
func setupEnforce(t *testing.T, d decide.Decider, yamlFormat ...string) *env {
	t.Helper()
	if len(yamlFormat) == 0 {
		yamlFormat = []string{enforceYAML}
	}
	e := setup(t, d, yamlFormat...)
	mustExec(t, superuser(t), "grant stet_md5 to stet_scram") // so Stet can SET ROLE stet_md5
	mustExec(t, e.admin, "grant all on "+e.table+" to stet_md5")
	t.Cleanup(func() {
		e.admin.Exec(context.Background(), "delete from stet.held_writes where target_table = $1", e.table)
	})
	return e
}

// agent connects through the proxy as stet_md5, the agents' login role.
func (e *env) agent(t *testing.T, name string) *pgx.Conn {
	t.Helper()
	return e.connectAs(t, "stet_md5", "md5-pw", name)
}

// holding answers like calm, but says the write breaks policy rule 1.
func holding() *fakeDecider {
	d := calm()
	d.answers["policy_1"] = decide.Answer{Choice: "true", Probability: ptr(0.9)}
	return d
}

// enforcedRow is what a decision records about how enforce mode treated it.
type enforcedRow struct {
	ID           string
	Verdict      string
	Reasons      []string
	Executed     bool
	ExecError    *string
	Refused      *string
	RowsAffected *int64
	Mode         string
	State        map[string]any
	StateVersion map[string]any
	Questions    []map[string]any
	Answers      map[string]any
	Cumulative   map[string]any
}

// enforced waits for n decisions on the env's table, oldest first.
func (e *env) enforced(t *testing.T, n int) []enforcedRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := e.admin.Query(context.Background(), `
			select id::text, verdict, reasons, executed, exec_error, refused, rows_affected, mode,
			       state, state_version, questions, answers, cumulative
			from stet.decisions where target_table = $1 order by created_at`, e.table)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[enforcedRow])
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

// heldRow is one row of stet.held_writes.
type heldRow struct {
	ID          string
	DecisionID  string
	Status      string
	LoginRole   string
	Database    string
	Statement   string
	Params      []*string
	ParamTypes  []uint32
	TargetPK    *string
	RowVersion  *string
	Verdict     string
	Reasons     []string
	TargetTable string
}

// held waits for n held writes of the env's table, oldest first, and returns
// all there are. Held writes are stored before the client gets its error, so
// held(t, 0) is what the table holds now.
func (e *env) held(t *testing.T, n int) []heldRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := e.admin.Query(context.Background(), `
			select id::text, decision_id::text, status, login_role, database, statement, params,
			       param_types, target_pk, row_version, verdict, reasons, target_table
			from stet.held_writes where target_table = $1 order by created_at`, e.table)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[heldRow])
		if err != nil {
			t.Fatal(err)
		}
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d held writes stored, want %d", len(got), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// wantRefused checks that err is the gate's error with code, whose message
// contains messagePart, and returns it.
func wantRefused(t *testing.T, err error, code, messagePart string) *pgconn.PgError {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want SQLSTATE %s", err, code)
	}
	if pgErr.Code != code || !strings.Contains(pgErr.Message, messagePart) {
		t.Errorf("error %s %q, want %s with %q", pgErr.Code, pgErr.Message, code, messagePart)
	}
	return pgErr
}

func wantEnforced(t *testing.T, i int, r enforcedRow, verdict string, reasons ...string) {
	t.Helper()
	if reasons == nil {
		reasons = []string{}
	}
	if r.Verdict != verdict || !reflect.DeepEqual(r.Reasons, reasons) {
		t.Errorf("decision %d: verdict %s %v, want %s %v", i, r.Verdict, r.Reasons, verdict, reasons)
	}
}

// wantRefusedRecord checks that a decision records a write Stet refused
// with code: not executed, no row count, and the error the client got.
func wantRefusedRecord(t *testing.T, i int, r enforcedRow, code string) {
	t.Helper()
	if r.Refused == nil || *r.Refused != code {
		t.Errorf("decision %d: refused = %v, want %s", i, deref(r.Refused), code)
	}
	if r.Executed || r.RowsAffected != nil {
		t.Errorf("decision %d: executed=%v rows_affected=%v, want false and NULL", i, r.Executed, deref(r.RowsAffected))
	}
	if r.ExecError == nil || !strings.HasPrefix(*r.ExecError, "SQLSTATE "+code+": ") {
		t.Errorf("decision %d: exec_error = %v, want the refusal's SQLSTATE %s and message", i, deref(r.ExecError), code)
	}
}

// pipeline sends what send queues as one pipeline, Sync included, and
// returns its last error.
func pipeline(t *testing.T, conn *pgx.Conn, send func(*pgconn.Pipeline)) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pl := conn.PgConn().StartPipeline(ctx)
	send(pl)
	pl.SendPipelineSync()
	if err := pl.Flush(); err != nil {
		t.Fatal(err)
	}
	return pl.Close()
}

// logRecords parses JSON log lines.
func logRecords(t *testing.T, logs *lockedBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.Lines(logs.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

func TestEnforceSendsACommittedWrite(t *testing.T) {
	d := calm()
	e := setupEnforce(t, d)
	e.seed(t)
	mustExec(t, e.agent(t, "a"), "update "+e.table+" set title = 'CTO' where id = 1")

	if n := e.rowsWhere(t, "id = 1 and title = 'CTO'"); n != 1 {
		t.Errorf("%d rows written, want 1", n)
	}
	r := e.enforced(t, 1)[0]
	wantEnforced(t, 0, r, "commit")
	if r.Mode != "enforce" || !r.Executed || r.Refused != nil {
		t.Errorf("mode=%s executed=%v refused=%v, want enforce, executed, NULL", r.Mode, r.Executed, deref(r.Refused))
	}
	if r.RowsAffected == nil || *r.RowsAffected != 1 {
		t.Errorf("rows_affected = %v, want 1", deref(r.RowsAffected))
	}
	if r.Questions == nil || r.Answers == nil {
		t.Errorf("questions=%v answers=%v, want both recorded", r.Questions, r.Answers)
	}
	if n := d.callCount(); n != 1 {
		t.Errorf("model called %d times, want once", n)
	}
	if len(e.held(t, 0)) != 0 {
		t.Error("a committed write was stored as held")
	}
}

// committedStatements are judged commit under impactYAML with calm answers,
// whichever way the server then answers them. %[1]s is the table.
var committedStatements = []string{
	"insert into %[1]s (id, name) values (50, 'new')",
	"update %[1]s set title = 'y' where id = 1",
	"delete from %[1]s where id = 2",
	"update %[1]s set title = 'z' where id = 424242",
	"insert into %[1]s (id, name) values (3, 'dup')",
	"update %[1]s set status = 'x' where company_id = 9",
}

// A write enforce mode sends gets the same command tag or error, and leaves
// the same rows, as it would without Stet.
func TestEnforceCommittedWritesAnswerLikeADirectConnection(t *testing.T) {
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
			e := setupEnforce(t, calm(), enforcePrefix+impactYAML)
			e.seedMany(t)
			twin := e.table + "_twin"
			mustExec(t, e.admin, "create table "+twin+" (like "+e.table+" including all)")
			t.Cleanup(func() { e.admin.Exec(context.Background(), "drop table "+twin) })
			mustExec(t, e.admin, "insert into "+twin+" select * from "+e.table)
			client := e.agent(t, "twin")
			ctx := context.Background()

			for i, s := range committedStatements {
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
			for i, r := range e.enforced(t, len(committedStatements)) {
				wantEnforced(t, i, r, "commit")
				if r.Refused != nil {
					t.Errorf("decision %d: refused = %s, want NULL", i, *r.Refused)
				}
			}
		})
	}
}

func TestEnforceHoldsWhatTheModelFlags(t *testing.T) {
	d := holding()
	e := setupEnforce(t, d)
	e.seed(t)
	const sql = "update %s set status = $1 where id = $2"
	_, err := e.agent(t, "a").Exec(context.Background(), fmt.Sprintf(sql, e.table), "churned", 2)

	pgErr := wantRefused(t, err, "ST001", "write held by stet: ")
	if want := `write held by stet: it breaks policy rule 1 ("Do not set status to churned.")`; pgErr.Message != want {
		t.Errorf("message = %q, want %q", pgErr.Message, want)
	}
	if n := e.rowsWhere(t, "id = 2 and status = 'active'"); n != 1 {
		t.Errorf("row 2 changed: a held write must not run")
	}
	hs := e.held(t, 1)
	if len(hs) != 1 {
		t.Fatalf("%d held writes, want 1", len(hs))
	}
	h := hs[0]
	r := e.enforced(t, 1)[0]
	if want := "decision=" + r.ID + " held_write=" + h.ID + " reasons=policy:1"; pgErr.Detail != want {
		t.Errorf("detail = %q, want %q", pgErr.Detail, want)
	}
	if !strings.Contains(pgErr.Hint, "held_write "+h.ID) {
		t.Errorf("hint = %q, want it to name held_write %s", pgErr.Hint, h.ID)
	}
	if h.DecisionID != r.ID || h.Statement != fmt.Sprintf(sql, e.table) || h.LoginRole != "stet_md5" ||
		h.Database != "stet_test" || h.TargetTable != e.table || h.Status != "pending" {
		t.Errorf("held write %+v, want decision %s, the statement, stet_md5, stet_test, the table, pending", h, r.ID)
	}
	if h.TargetPK == nil || *h.TargetPK != "2" {
		t.Errorf("target_pk = %v, want 2", deref(h.TargetPK))
	}
	if xmin, _ := r.StateVersion["xmin"].(string); h.RowVersion == nil || xmin == "" || *h.RowVersion != xmin {
		t.Errorf("row_version = %v, want the xmin the state was read at (%v)", deref(h.RowVersion), r.StateVersion)
	}
	if h.Verdict != "hold" || !reflect.DeepEqual(h.Reasons, []string{"policy:1"}) {
		t.Errorf("held write: %s %v, want hold [policy:1]", h.Verdict, h.Reasons)
	}
	wantEnforced(t, 0, r, "hold", "policy:1")
	wantRefusedRecord(t, 0, r, "ST001")
	if !strings.HasPrefix(*r.ExecError, "SQLSTATE ST001: write held by stet:") {
		t.Errorf("exec_error = %q", *r.ExecError)
	}
	if r.Questions == nil || r.Answers == nil {
		t.Errorf("questions=%v answers=%v, want what the model was asked and answered", r.Questions, r.Answers)
	}
}

// pgx sends integers in binary. A held write keeps every bind value as text,
// NULL as NULL, and the types the client gave in Parse (spec 9).
func TestHeldWriteKeepsBinaryAndNullParamsAsText(t *testing.T) {
	e := setupEnforce(t, holding())
	e.seed(t)
	_, err := e.agent(t, "a").Exec(context.Background(),
		"update "+e.table+" set status = $1, title = $2 where id = $3", "churned", nil, int32(2))
	wantRefused(t, err, "ST001", "write held by stet: ")

	h := e.held(t, 1)[0]
	if got, want := h.Params, []*string{ptr("churned"), nil, ptr("2")}; !reflect.DeepEqual(got, want) {
		t.Errorf("params = %v, want [churned NULL 2]", h.Params)
	}
	if len(h.ParamTypes) != 3 {
		t.Errorf("param_types = %v, want 3 of them", h.ParamTypes)
	}
}

// A held write is stored only as it was sent: approving a changed value
// would run another write. One whose bind values are text jsonb would refuse
// or rewrite (a NUL, bytes that are not UTF-8), or whose binary values could
// not be decoded in time, is not stored, and is still refused as when the
// store fails (spec 7; ruling on Task 2's review).
func TestHeldWriteThatCannotBeStoredAsSentIsStillRefused(t *testing.T) {
	// Such text fails at the server's Bind, which goes ahead of the refused
	// Execute: the client gets the server's error for it (spec 6).
	t.Run("text", func(t *testing.T) {
		e := setupEnforce(t, holding())
		e.seed(t)
		var logs lockedBuffer
		gate.SetLogger(e.gate, slog.New(slog.NewJSONHandler(&logs, nil)))
		conn := e.agent(t, "a")
		for _, v := range []string{"chur\x00ned", "chur\xffned"} {
			res := conn.PgConn().ExecParams(context.Background(), "update "+e.table+" set status = $1 where id = $2",
				[][]byte{[]byte(v), []byte("2")}, nil, nil, nil).Read()
			if sqlstate(res.Err) != "22021" {
				t.Errorf("bind value %q: %v, want the server's own 22021 for the Bind", v, res.Err)
			}
		}
		if hs := e.held(t, 0); len(hs) != 0 {
			t.Errorf("%d held writes stored, want none", len(hs))
		}
		if n := e.rowsWhere(t, "id = 2 and status = 'active'"); n != 1 {
			t.Error("row 2 changed: a held write must not run")
		}
		wantErrorLogs(t, &logs, "held write not stored", 2)
	})
	// With no slot in time, there is no time left to decode pgx's binary
	// integer: the write is held for its state, and stored without values
	// it would not be the write that was sent.
	t.Run("binary", func(t *testing.T) {
		e := setupEnforce(t, holding(), enforceYAML+"limits: {max_inflight: 1}\n")
		e.seed(t)
		var logs lockedBuffer
		gate.SetLogger(e.gate, slog.New(slog.NewJSONHandler(&logs, nil)))
		conn := e.agent(t, "a")
		release := gate.HoldInflight(e.gate)
		_, err := conn.Exec(context.Background(), "update "+e.table+" set status = $1 where id = $2", "churned", int32(2))
		release()
		pgErr := wantRefused(t, err, "ST001", "write held by stet: the state needed to judge it could not be read in time")
		if strings.Contains(pgErr.Detail, "held_write") || !strings.Contains(pgErr.Hint, "this write") {
			t.Errorf("detail %q hint %q, want no held write named", pgErr.Detail, pgErr.Hint)
		}
		if hs := e.held(t, 0); len(hs) != 0 {
			t.Errorf("%d held writes stored, want none", len(hs))
		}
		wantErrorLogs(t, &logs, "held write not stored", 1)
	})
}

// wantErrorLogs checks that n ERROR lines with message msg were logged.
func wantErrorLogs(t *testing.T, logs *lockedBuffer, msg string, n int) {
	t.Helper()
	got := 0
	for _, rec := range logRecords(t, logs) {
		if rec["level"] == "ERROR" && rec["msg"] == msg {
			got++
		}
	}
	if got != n {
		t.Errorf("%d errors %q logged, want %d:\n%s", got, msg, n, logs.String())
	}
}

// The deterministic rules decide before the model: a write they hold is not
// asked about (rule 5).
func TestEnforceSkipsTheModelWhenTheRulesDecide(t *testing.T) {
	t.Run("impact", func(t *testing.T) {
		d := calm()
		e := setupEnforce(t, d, enforcePrefix+impactYAML)
		e.seedMany(t)
		_, err := e.agent(t, "bulk").Exec(context.Background(), "update "+e.table+" set status = 'x' where company_id = 3")
		pgErr := wantRefused(t, err, "ST001", "")
		if want := "write held by stet: more than 3 rows would be updated, over the limit of 3"; pgErr.Message != want {
			t.Errorf("message = %q, want %q", pgErr.Message, want)
		}
		if n := e.rowsWhere(t, "status = 'x'"); n != 0 {
			t.Errorf("%d rows changed, want none", n)
		}
		if h := e.held(t, 1)[0]; h.Verdict != "hold" || !reflect.DeepEqual(h.Reasons, []string{"impact:rows"}) || h.RowVersion != nil {
			t.Errorf("held write %s %v row_version %v, want hold [impact:rows] and none", h.Verdict, h.Reasons, deref(h.RowVersion))
		}
		wantNoModel(t, d)
	})
	t.Run("cumulative", func(t *testing.T) {
		d := calm()
		e := setupEnforce(t, d, enforcePrefix+cumYAML)
		e.seedMany(t)
		conn := e.agent(t, "bulk")
		// Five rows, by measured writes only, which ask no model.
		mustExec(t, conn, "update "+e.table+" set status = 'f1' where company_id = 9")
		mustExec(t, conn, "update "+e.table+" set status = 'f2' where company_id = 3 and id > 2")
		_, err := conn.Exec(context.Background(), "update "+e.table+" set title = 'over' where id = 1")
		pgErr := wantRefused(t, err, "ST001", "write held by stet: this role has touched 5 rows of "+e.table)
		if !strings.HasSuffix(pgErr.Detail, "reasons=impact:cumulative") {
			t.Errorf("detail = %q, want reasons=impact:cumulative", pgErr.Detail)
		}
		if n := e.rowsWhere(t, "title = 'over'"); n != 0 {
			t.Error("the held write ran")
		}
		if h := e.held(t, 1)[0]; h.Verdict != "hold" || !reflect.DeepEqual(h.Reasons, []string{"impact:cumulative"}) {
			t.Errorf("held write %s %v, want hold [impact:cumulative]", h.Verdict, h.Reasons)
		}
		wantNoModel(t, d)
	})
}

func TestEnforceEscalatesAnEstimate(t *testing.T) {
	d := calm()
	e := setupEnforce(t, d, enforcePrefix+looseYAML)
	e.seedMany(t)
	mustExec(t, e.admin, "analyze "+e.table)
	_, err := e.agent(t, "bulk").Exec(context.Background(), "update "+e.table+" set status = 'x' where company_id in (select 9)")
	wantRefused(t, err, "ST001", "write held by stet: the rows it would touch could only be estimated")

	if h := e.held(t, 1)[0]; h.Verdict != "escalate" || !reflect.DeepEqual(h.Reasons, []string{"impact:uncertain"}) {
		t.Errorf("held write %s %v, want escalate [impact:uncertain]", h.Verdict, h.Reasons)
	}
	if n := e.rowsWhere(t, "status = 'x'"); n != 0 {
		t.Errorf("%d rows changed, want none", n)
	}
	wantNoModel(t, d)
}

func TestEnforceRefusesUnsupportedShapes(t *testing.T) {
	t.Run("reject", func(t *testing.T) {
		d := calm()
		e := setupEnforce(t, d)
		e.seed(t)
		_, err := e.agent(t, "a").Exec(context.Background(),
			"insert into "+e.table+" (id, name) values (1, 'x') on conflict (id) do update set name = excluded.name")
		wantRefused(t, err, "ST002", "write refused by stet: ")
		if hs := e.held(t, 0); len(hs) != 0 {
			t.Errorf("%d held writes, want none: an unsupported write is not held", len(hs))
		}
		if n := e.rowsWhere(t, "id = 1 and name = 'Kim'"); n != 1 {
			t.Error("row 1 changed: the refused upsert ran")
		}
		r := e.enforced(t, 1)[0]
		if r.Verdict != "unsupported" {
			t.Errorf("verdict %s %v, want unsupported", r.Verdict, r.Reasons)
		}
		wantRefusedRecord(t, 0, r, "ST002")
		wantNoModel(t, d)
	})
	// unsupported: pass sends it unjudged; its rows still count (ruling P3).
	t.Run("pass", func(t *testing.T) {
		d := calm()
		e := setupEnforce(t, d, enforcePrefix+cumYAML+"    unsupported: pass\n")
		e.seedMany(t)
		conn := e.agent(t, "a")
		mustExec(t, conn, "insert into "+e.table+" (id, name) values (1, 'x'), (40, 'y') on conflict (id) do update set name = excluded.name")
		mustExec(t, conn, "update "+e.table+" set title = 'next' where id = 2")

		rs := e.enforced(t, 2)
		if rs[0].Verdict != "unsupported" || !rs[0].Executed || rs[0].Refused != nil {
			t.Errorf("upsert: verdict %s %v executed=%v refused=%v, want unsupported, executed, NULL",
				rs[0].Verdict, rs[0].Reasons, rs[0].Executed, deref(rs[0].Refused))
		}
		if n := e.rowsWhere(t, "id in (1, 40) and name in ('x', 'y')"); n != 2 {
			t.Errorf("%d rows upserted, want 2", n)
		}
		wantEnforced(t, 1, rs[1], "commit")
		if used := rs[1].Cumulative["used"]; used != 2.0 {
			t.Errorf("next write: cumulative.used = %v, want 2: the upsert's rows count (cumulative %v)", used, rs[1].Cumulative)
		}

		// pass does not let through a write in a dirty transaction, nor one
		// whose login role is not known, although its shape reason comes
		// first (ruling P2).
		upsert := "insert into " + e.table + " (id, name) values (41, 'z') on conflict (id) do update set name = excluded.name"
		mustExec(t, conn, "begin")
		mustExec(t, conn, "select 1")
		_, err := conn.Exec(context.Background(), upsert)
		wantRefused(t, err, "ST002", "write refused by stet: it came after another statement")
		mustExec(t, conn, "rollback")
		h, refuse := e.gate.Begin(proxy.SessionInfo{ID: "early", Database: "stet_test"}, e.gate.Inspect(upsert), nil)
		if refuse == nil || refuse.Code != "ST002" || !strings.Contains(refuse.Message, "login role is not known") {
			t.Errorf("no login role: refusal %+v, want ST002 for the login role", refuse)
		}
		e.gate.End(h, proxy.Outcome{Refused: true})
		if n := e.rowsWhere(t, "id = 41"); n != 0 {
			t.Error("a refused upsert ran")
		}
		for i, r := range e.enforced(t, 4)[2:] {
			wantRefusedRecord(t, 2+i, r, "ST002")
		}
	})
}

// Stet cannot act as stet_trust, so it cannot measure that role's multi-row
// writes: they are refused as their shape is (spec 7).
func TestEnforceRefusesWhenMeasuringFails(t *testing.T) {
	d := calm()
	e := setupEnforce(t, d, trustPrefix+impactYAML)
	var member bool
	if err := e.admin.QueryRow(context.Background(),
		"select pg_has_role('stet_scram', 'stet_trust', 'member')").Scan(&member); err != nil || member {
		t.Fatalf("stet_scram is a member of stet_trust (%v); this test needs it not to be", err)
	}
	e.seedMany(t)
	mustExec(t, e.admin, "grant all on "+e.table+" to stet_trust")
	_, err := e.connectAs(t, "stet_trust", "", "outsider").Exec(context.Background(),
		"update "+e.table+" set status = 'x' where company_id = 9")
	wantRefused(t, err, "ST002", "write refused by stet: ")

	r := e.enforced(t, 1)[0]
	wantEnforced(t, 0, r, "unsupported", "not_single_row")
	wantRefusedRecord(t, 0, r, "ST002")
	if n := e.rowsWhere(t, "status = 'x'"); n != 0 {
		t.Errorf("%d rows changed, want none", n)
	}
	wantNoModel(t, d)
}

// A transaction that has run nothing but BEGIN is clean: its first write is
// judged as an autocommit write is, whether BEGIN came in its own message or
// under the same Sync (spec 4).
func TestEnforceJudgesTheFirstWriteAfterBegin(t *testing.T) {
	for name, c := range map[string]struct {
		d       func() *fakeDecider
		verdict []string
	}{
		"commit": {calm, []string{"commit"}},
		"hold":   {holding, []string{"hold", "policy:1"}},
	} {
		t.Run(name, func(t *testing.T) {
			d := c.d()
			e := setupEnforce(t, d)
			e.seed(t)
			conn := e.agent(t, "tx")
			ctx := context.Background()
			end := "commit"
			if name == "hold" {
				end = "rollback"
			}

			// psycopg's way: BEGIN, then Parse, Bind, Execute and Sync.
			mustExec(t, conn, "begin")
			res := conn.PgConn().ExecParams(ctx, "update "+e.table+" set title = $1 where id = $2",
				[][]byte{[]byte("first"), []byte("1")}, nil, nil, nil).Read()
			// pgconn's pipeline: BEGIN and the write under one Sync.
			mustExec(t, conn, end)
			perr := pipeline(t, conn, func(pl *pgconn.Pipeline) {
				pl.SendQueryParams("begin", nil, nil, nil, nil)
				pl.SendQueryParams("update "+e.table+" set title = $1 where id = $2",
					[][]byte{[]byte("second"), []byte("2")}, nil, nil, nil)
			})
			mustExec(t, conn, end)

			for i, err := range []error{res.Err, perr} {
				if name == "hold" {
					wantRefused(t, err, "ST001", "write held by stet: ")
				} else if err != nil {
					t.Errorf("write %d: %v", i, err)
				}
			}
			want := 2
			if name == "hold" {
				want = 0
			}
			if n := e.rowsWhere(t, "title in ('first', 'second')"); n != want {
				t.Errorf("%d rows written, want %d", n, want)
			}
			for i, r := range e.enforced(t, 2) {
				wantEnforced(t, i, r, c.verdict[0], c.verdict[1:]...)
			}
			if n := d.callCount(); n != 2 {
				t.Errorf("model called %d times, want twice", n)
			}
		})
	}
}

// WHERE id IN ($1) touches one row and is judged as WHERE id = $1 is.
func TestEnforceJudgesASingleValueIn(t *testing.T) {
	d := holding()
	e := setupEnforce(t, d)
	e.seed(t)
	_, err := e.agent(t, "orm").Exec(context.Background(), "update "+e.table+" set title = $1 where id in ($2)", "x", 1)
	wantRefused(t, err, "ST001", "write held by stet: it breaks policy rule 1")
	if n := d.callCount(); n != 1 {
		t.Errorf("model called %d times, want once", n)
	}
}

// A write behind another statement of its transaction is refused before
// Stet reads anything, so a transaction that may hold locks does not wait
// (spec 4, CLAUDE.md rule 1).
func TestEnforceRefusesAWriteInADirtyTransaction(t *testing.T) {
	d := calm()
	e := setupEnforce(t, d)
	e.seed(t)
	conn := e.agent(t, "tx")
	ctx := context.Background()
	mustExec(t, conn, "begin")
	mustExec(t, conn, "select 1")
	_, err := conn.Exec(ctx, "update "+e.table+" set title = 'dirty' where id = 1")
	pgErr := wantRefused(t, err, "ST002", "write refused by stet: it came after another statement")
	if !strings.Contains(pgErr.Detail, "reasons=in_transaction") {
		t.Errorf("detail = %q, want reasons=in_transaction", pgErr.Detail)
	}
	if _, err := conn.Exec(ctx, "select 1"); sqlstate(err) != "25P02" {
		t.Errorf("select after the refusal: %v, want 25P02: the transaction failed", err)
	}
	mustExec(t, conn, "rollback")
	mustExec(t, conn, "update "+e.table+" set title = 'clean' where id = 1")

	rs := e.enforced(t, 2)
	wantEnforced(t, 0, rs[0], "unsupported", "in_transaction")
	wantRefusedRecord(t, 0, rs[0], "ST002")
	if rs[0].State != nil || rs[0].Questions != nil {
		t.Errorf("refused write: state=%v questions=%v, want NULL: nothing was read for it", rs[0].State, rs[0].Questions)
	}
	wantEnforced(t, 1, rs[1], "commit")
	if !rs[1].Executed {
		t.Error("the write after ROLLBACK did not run")
	}
	if n := d.callCount(); n != 1 {
		t.Errorf("model called %d times, want once: not for the refused write", n)
	}
	if n := e.rowsWhere(t, "title = 'clean'"); n != 1 {
		t.Errorf("%d rows have the second write's title, want 1", n)
	}
}

func TestEnforceRefusesWithoutALoginRole(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		e := setupEnforce(t, calm())
		e.seed(t)
		h, refuse := e.gate.Begin(proxy.SessionInfo{ID: "x", Database: "stet_test"},
			e.gate.Inspect("update "+e.table+" set title = 'x' where id = 1"), nil)
		if refuse == nil {
			t.Fatal("a write with no login role was let through")
		}
		if refuse.Code != "ST002" || !strings.Contains(refuse.Detail, "no_login_role") {
			t.Errorf("refusal %+v, want ST002 with no_login_role in its detail", refuse)
		}
		e.gate.End(h, proxy.Outcome{Refused: true, Error: "SQLSTATE " + refuse.Code + ": " + refuse.Message})
		r := e.enforced(t, 1)[0]
		wantEnforced(t, 0, r, "unsupported", "no_login_role")
		wantRefusedRecord(t, 0, r, "ST002")
	})
	// The proxy refuses a statement sent before the session authenticated as
	// a closed point (session_setting), before it reaches the gate: so no
	// decision records it (from Task 9's report).
	t.Run("early query", func(t *testing.T) {
		e := setupEnforce(t, calm(), trustPrefix+tableYAML)
		e.seed(t)
		mustExec(t, e.admin, "grant all on "+e.table+" to stet_trust")
		if code := e.earlyQuery(t, "stet_trust", "update "+e.table+" set title = 'early' where id = 1"); code != "ST002" {
			t.Errorf("early write: SQLSTATE %q, want ST002", code)
		}
		if n := e.rowsWhere(t, "title = 'early'"); n != 0 {
			t.Error("the early write ran")
		}
		e.wantNoDecisions(t)
	})
}

func TestEnforceFailuresFollowOnFailure(t *testing.T) {
	modelError := func() *fakeDecider { return &fakeDecider{err: errors.New("upstream 503")} }
	update := func(e *env) string { return "update " + e.table + " set title = 'x' where id = 1" }
	insert := func(e *env) string { return "insert into " + e.table + " (id, email) values (9, 'x@y.com')" }
	for name, c := range map[string]struct {
		d      *fakeDecider
		yaml   string
		sql    func(*env) string
		reason string
		msg    string
	}{
		"model error":       {modelError(), enforceYAML, update, "model_error", "the judgment model did not answer"},
		"state error":       {calm(), brokenYAML, insert, "state_error", "the state needed to judge it could not be read in time"},
		"model error, pass": {modelError(), passOnFailure(enforceYAML), update, "model_error", ""},
		"state error, pass": {calm(), passOnFailure(brokenYAML), insert, "state_error", ""},
	} {
		t.Run(name, func(t *testing.T) {
			e := setupEnforce(t, c.d, c.yaml)
			e.seed(t)
			_, err := e.agent(t, "a").Exec(context.Background(), c.sql(e))
			r := e.enforced(t, 1)[0]
			if c.msg == "" { // on_failure: pass
				if err != nil {
					t.Fatalf("on_failure pass: %v, want the write sent", err)
				}
				wantEnforced(t, 0, r, "commit", c.reason)
				if !r.Executed || r.Refused != nil {
					t.Errorf("executed=%v refused=%v, want executed and NULL", r.Executed, deref(r.Refused))
				}
				return
			}
			wantRefused(t, err, "ST001", "write held by stet: "+c.msg)
			wantEnforced(t, 0, r, "hold", c.reason)
			wantRefusedRecord(t, 0, r, "ST001")
			if h := e.held(t, 1)[0]; h.Verdict != "hold" || !reflect.DeepEqual(h.Reasons, []string{c.reason}) {
				t.Errorf("held write %s %v, want hold [%s]", h.Verdict, h.Reasons, c.reason)
			}
		})
	}
}

func TestEnforceHoldsWhenTheCumulativeUsageIsUnknown(t *testing.T) {
	d := calm()
	e := setupEnforce(t, d, enforcePrefix+cumYAML)
	gate.FailUsageLoads(e.gate, 1)
	e.seedMany(t)
	_, err := e.agent(t, "a").Exec(context.Background(), "update "+e.table+" set title = 'x' where id = 1")
	wantRefused(t, err, "ST001", "write held by stet: the rows this role has touched in "+e.table+" could not be read")

	r := e.enforced(t, 1)[0]
	wantEnforced(t, 0, r, "hold", "impact:cumulative_unknown")
	wantRefusedRecord(t, 0, r, "ST001")
	if n := e.rowsWhere(t, "title = 'x'"); n != 0 {
		t.Error("the held write ran")
	}
	wantNoModel(t, d)
}

// A write that finds no inflight slot in time is a state error, and
// on_failure holds it in enforce (spec 7).
func TestEnforceHoldsWhenTooManyWritesAreInFlight(t *testing.T) {
	d := calm()
	e := setupEnforce(t, d, enforceYAML+"limits: {max_inflight: 1}\n")
	e.seed(t)
	conn := e.agent(t, "crowded")
	release := gate.HoldInflight(e.gate)
	start := time.Now()
	_, err := conn.Exec(context.Background(), "update "+e.table+" set title = 'crowded' where id = 1")
	waited := time.Since(start)
	release()
	wantRefused(t, err, "ST001", "write held by stet: the state needed to judge it could not be read in time")
	if waited < 450*time.Millisecond || waited > 2*time.Second {
		t.Errorf("refused after %v, want about 500ms", waited)
	}
	wantEnforced(t, 0, e.enforced(t, 1)[0], "hold", "state_error")
	wantNoModel(t, d)
	// The slot comes back once Begin ends.
	mustExec(t, conn, "update "+e.table+" set title = 'later' where id = 2")
	wantEnforced(t, 1, e.enforced(t, 2)[1], "commit")
}

func TestEnforceRefusesEvenWhenTheHeldWriteIsNotStored(t *testing.T) {
	e := setupEnforce(t, holding())
	var logs lockedBuffer
	gate.SetLogger(e.gate, slog.New(slog.NewJSONHandler(&logs, nil)))
	gate.FailHeldWrites(e.gate, 1)
	e.seed(t)
	_, err := e.agent(t, "a").Exec(context.Background(), "update "+e.table+" set status = 'churned' where id = 2")

	pgErr := wantRefused(t, err, "ST001", "write held by stet: it breaks policy rule 1")
	if strings.Contains(pgErr.Detail, "held_write") || !strings.Contains(pgErr.Detail, "decision=") {
		t.Errorf("detail = %q, want the decision and no held write", pgErr.Detail)
	}
	if !strings.Contains(pgErr.Hint, "this write") {
		t.Errorf("hint = %q, want it to name this write", pgErr.Hint)
	}
	if hs := e.held(t, 0); len(hs) != 0 {
		t.Errorf("%d held writes, want none", len(hs))
	}
	if n := e.rowsWhere(t, "status = 'churned'"); n != 0 {
		t.Error("the held write ran")
	}
	wantRefusedRecord(t, 0, e.enforced(t, 1)[0], "ST001")
	errs := 0
	for _, rec := range logRecords(t, &logs) {
		if rec["level"] == "ERROR" && rec["msg"] == "held write not stored" && rec["err"] == "injected failure" {
			errs++
		}
	}
	if errs != 1 {
		t.Errorf("%d errors logged for the held write, want 1:\n%s", errs, logs.String())
	}
}

// adminExec runs sql on the env's admin connection from a model call, which
// runs outside the test's goroutine, so a failure is reported, not fatal.
func (e *env) adminExec(t *testing.T, sql string) {
	if _, err := e.admin.Exec(context.Background(), sql); err != nil {
		t.Errorf("%s: %v", sql, err)
	}
}

// A row that changed while the model was asked is read again and judged
// once more; the record keeps the last judgment (spec 5, 11).
func TestEnforceRejudgesWhenTheRowChanged(t *testing.T) {
	d := calm()
	e := setupEnforce(t, d)
	e.seed(t)
	d.onDecide = func(call int) {
		if call == 1 {
			e.adminExec(t, "update "+e.table+" set name = 'Admin' where id = 1")
		}
	}
	mustExec(t, e.agent(t, "a"), "update "+e.table+" set title = 'x' where id = 1")

	if n := e.rowsWhere(t, "id = 1 and title = 'x' and name = 'Admin'"); n != 1 {
		t.Error("the write did not run after the row changed")
	}
	r := e.enforced(t, 1)[0]
	wantEnforced(t, 0, r, "commit", "rejudged")
	if !r.Executed || r.Refused != nil {
		t.Errorf("executed=%v refused=%v, want executed and NULL", r.Executed, deref(r.Refused))
	}
	if n := d.callCount(); n != 2 {
		t.Errorf("model called %d times, want twice", n)
	}
	if cur, _ := r.State["current"].(map[string]any); cur["name"] != "Admin" {
		t.Errorf("state.current = %v, want the row as read the second time", cur)
	}
	if !reflect.DeepEqual(d.lastCall(t).state, r.State) {
		t.Error("recorded state differs from what the model was last sent")
	}
}

func TestEnforceHoldsWhenTheRowKeepsChanging(t *testing.T) {
	d := calm()
	e := setupEnforce(t, d)
	e.seed(t)
	d.onDecide = func(call int) {
		e.adminExec(t, fmt.Sprintf("update %s set name = 'Admin %d' where id = 1", e.table, call))
	}
	_, err := e.agent(t, "a").Exec(context.Background(), "update "+e.table+" set title = 'x' where id = 1")
	wantRefused(t, err, "ST001", "write held by stet: the row changed while the write was being judged")

	r := e.enforced(t, 1)[0]
	wantEnforced(t, 0, r, "hold", "concurrent_change", "rejudged")
	wantRefusedRecord(t, 0, r, "ST001")
	if n := d.callCount(); n != 2 {
		t.Errorf("model called %d times, want twice", n)
	}
	if n := e.rowsWhere(t, "id = 1 and title = 'Engineer' and name = 'Admin 2'"); n != 1 {
		t.Errorf("row 1 is not as the admin left it: %v", e.contents(t, e.table))
	}
	if h := e.held(t, 1)[0]; h.Verdict != "hold" || !reflect.DeepEqual(h.Reasons, []string{"concurrent_change", "rejudged"}) {
		t.Errorf("held write %s %v, want hold [concurrent_change rejudged]", h.Verdict, h.Reasons)
	}
}

// A row that is gone when the version is read again is not judged again:
// the write matches nothing (spec 5).
func TestEnforceSendsAWriteWhoseRowIsGone(t *testing.T) {
	d := calm()
	e := setupEnforce(t, d)
	e.seed(t)
	d.onDecide = func(call int) {
		if call == 1 {
			e.adminExec(t, "delete from "+e.table+" where id = 1")
		}
	}
	tag, err := e.agent(t, "a").Exec(context.Background(), "update "+e.table+" set title = 'x' where id = 1")
	if err != nil || tag.String() != "UPDATE 0" {
		t.Fatalf("%q %v, want UPDATE 0", tag, err)
	}
	r := e.enforced(t, 1)[0]
	wantEnforced(t, 0, r, "commit")
	if r.RowsAffected == nil || *r.RowsAffected != 0 || r.Refused != nil {
		t.Errorf("rows_affected=%v refused=%v, want 0 and NULL", deref(r.RowsAffected), deref(r.Refused))
	}
	if n := d.callCount(); n != 1 {
		t.Errorf("model called %d times, want once", n)
	}
}

// A refusal hidden behind an earlier failure of the same Sync never reaches
// the client, so its held write is marked undelivered (spec 9).
func TestEnforceMarksAHiddenHoldUndelivered(t *testing.T) {
	e := setupEnforce(t, holding())
	e.seed(t)
	err := pipeline(t, e.agent(t, "a"), func(pl *pgconn.Pipeline) {
		pl.SendPrepare("x", "select * from no_such_table", nil)
		pl.SendQueryParams("update "+e.table+" set status = $1 where id = $2",
			[][]byte{[]byte("churned"), []byte("2")}, nil, nil, nil)
	})
	if sqlstate(err) != "42P01" {
		t.Fatalf("pipeline: %v, want the client's own 42P01", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		hs := e.held(t, 1)
		if hs[0].Status == "undelivered" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("held write status %s, want undelivered", hs[0].Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	wantRefusedRecord(t, 0, e.enforced(t, 1)[0], "ST001")
}

// A write judged commit that the server refuses gets the server's error as
// it is; Stet did not refuse it.
func TestEnforceLeavesTheServersOwnErrorAlone(t *testing.T) {
	e := setupEnforce(t, calm())
	e.seed(t)
	_, err := e.agent(t, "a").Exec(context.Background(), "insert into "+e.table+" (id, name) values (1, 'dup')")
	if sqlstate(err) != "23505" {
		t.Fatalf("duplicate insert: %v, want 23505", err)
	}
	r := e.enforced(t, 1)[0]
	wantEnforced(t, 0, r, "commit")
	if r.Refused != nil || r.Executed || r.ExecError == nil || !strings.Contains(*r.ExecError, "23505") {
		t.Errorf("refused=%v executed=%v exec_error=%v, want NULL, false and the 23505", deref(r.Refused), r.Executed, deref(r.ExecError))
	}
}

// A refused write leaves its reservation as a write the server refused does,
// so an agent retrying a held write does not use up its role's limit.
func TestRetriedHeldWriteDoesNotUseUpTheCumulativeLimit(t *testing.T) {
	d := holding()
	e := setupEnforce(t, d, enforcePrefix+cumYAML)
	e.seedMany(t)
	conn := e.agent(t, "retrier")
	for i := range 6 {
		_, err := conn.Exec(context.Background(), "update "+e.table+" set status = 'churned' where id = 1")
		if sqlstate(err) != "ST001" {
			t.Fatalf("try %d: %v, want ST001", i, err)
		}
	}
	if hs := e.held(t, 6); len(hs) != 6 {
		t.Errorf("%d held writes, want 6: one each time", len(hs))
	}
	d.setAnswers(calm().answers)
	mustExec(t, conn, "update "+e.table+" set title = 'other' where id = 2")

	rs := e.enforced(t, 7)
	for i, r := range rs[:6] {
		wantEnforced(t, i, r, "hold", "policy:1")
	}
	wantEnforced(t, 6, rs[6], "commit")
	if used := rs[6].Cumulative["used"]; used != 0.0 {
		t.Errorf("cumulative.used = %v, want 0: refused writes touched nothing (cumulative %v)", used, rs[6].Cumulative)
	}
	if n := e.rowsWhere(t, "id = 2 and title = 'other'"); n != 1 {
		t.Error("the last write did not run")
	}
}

// A measured write judged commit that then changed more rows than max_rows
// is marked, in either mode (spec 5).
func TestMeasuredWriteOverTheLimitAfterTheFact(t *testing.T) {
	for name, c := range map[string]struct {
		yaml string
		user string
		mode string
	}{
		"enforce": {enforcePrefix + impactYAML, "stet_md5", "enforce"},
		"shadow":  {impactYAML, "stet_scram", "shadow"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setupEnforce(t, calm(), c.yaml)
			e.seedMany(t)
			var logs lockedBuffer
			gate.SetLogger(e.gate, slog.New(slog.NewJSONHandler(&logs, nil)))
			info := proxy.SessionInfo{ID: "after", User: c.user, Database: "stet_test", ApplicationName: "after"}
			h, refuse := e.gate.Begin(info, e.gate.Inspect("update "+e.table+" set status = 'x' where company_id = 9"), nil)
			if refuse != nil {
				t.Fatalf("a measured write of 2 rows was refused: %+v", refuse)
			}
			e.gate.End(h, proxy.Outcome{Executed: true, Rows: ptr(int64(5))})

			r := e.enforced(t, 1)[0]
			wantEnforced(t, 0, r, "commit", "impact:exceeded_after")
			if r.Mode != c.mode {
				t.Errorf("mode = %s, want %s", r.Mode, c.mode)
			}
			warned := false
			for _, rec := range logRecords(t, &logs) {
				warned = warned || rec["level"] == "WARN" && rec["decision"] == r.ID && rec["rows"] == 5.0
			}
			if !warned {
				t.Errorf("no warning names the decision and its 5 rows:\n%s", logs.String())
			}
		})
	}
}

// Sessions of roles that are not agents' are neither judged nor refused.
func TestEnforceLeavesOtherRolesAlone(t *testing.T) {
	d := holding()
	e := setupEnforce(t, d)
	e.seed(t)
	conn := e.connect(t, "owner") // stet_scram
	mustExec(t, conn, "update "+e.table+" set status = 'churned' where id = 2")
	mustExec(t, conn, "do $$ begin update "+e.table+" set title = 'done' where id = 1; end $$")

	if n := e.rowsWhere(t, "(id = 2 and status = 'churned') or (id = 1 and title = 'done')"); n != 2 {
		t.Errorf("%d rows written, want 2", n)
	}
	wantNoModel(t, d)
	e.wantNoDecisions(t)
}

// The closed points of spec 7, as the real Inspect marks them, reach an
// agent through the gate's Blocked; other sessions get the server's own
// answers.
func TestEnforceClosedPointsAnswerThroughTheGate(t *testing.T) {
	e := setupEnforce(t, calm())
	e.seed(t)
	ctx := context.Background()
	doBlock := "do $$ begin update " + e.table + " set title = 'done' where id = 1; end $$"
	agent := e.agent(t, "a")
	for sql, reason := range map[string]string{"selec 1": proxy.BlockUnparsed, doBlock: proxy.BlockDo} {
		_, err := agent.Exec(ctx, sql)
		pgErr := wantRefused(t, err, "ST002", "")
		if want := decide.Blocked(reason).Message; pgErr.Message != want {
			t.Errorf("%s: message %q, want %q", reason, pgErr.Message, want)
		}
	}
	if n := e.rowsWhere(t, "title = 'done'"); n != 0 {
		t.Error("the agent's DO block ran")
	}

	owner := e.connect(t, "owner")
	if _, err := owner.Exec(ctx, "selec 1"); sqlstate(err) != "42601" {
		t.Errorf("selec 1 from another role: %v, want the server's 42601", err)
	}
	mustExec(t, owner, doBlock)
	if n := e.rowsWhere(t, "title = 'done'"); n != 1 {
		t.Error("another role's DO block did not run")
	}
	e.wantNoDecisions(t)
}

// churnDecider says a write breaks policy rule 1 when it sets status to
// churned, and answers every other question calmly.
type churnDecider struct{}

func (churnDecider) Decide(_ context.Context, state json.RawMessage, qs []decide.Question) (decide.Answers, error) {
	var s struct {
		Proposed map[string]any `json:"proposed"`
	}
	if err := json.Unmarshal(state, &s); err != nil {
		return decide.Answers{}, err
	}
	out := map[string]decide.Answer{}
	for _, q := range qs {
		switch a, ok := calm().answers[q.ID]; {
		case q.ID == "policy_1" && s.Proposed["status"] == "churned":
			out[q.ID] = decide.Answer{Choice: "true", Probability: ptr(0.9)}
		case ok:
			out[q.ID] = a
		}
	}
	return decide.Answers{ByQuestion: out}, nil
}

func (churnDecider) ModelID() string      { return "fake" }
func (churnDecider) ModelVersion() string { return "fake-1" }

// Shadow and enforce mode apply one set of verdict rules (spec 3): the same
// writes get the same verdicts, whether they then run or are refused.
func TestShadowAndEnforceAgreeOnVerdicts(t *testing.T) {
	// Each write touches rows no other touches. Without a cumulative limit,
	// what shadow mode lets through and enforce refuses cannot change a
	// later verdict (ruling P4).
	writes := []func(ctx context.Context, c *pgx.Conn, table string) error{
		func(ctx context.Context, c *pgx.Conn, table string) error { // passes
			_, err := c.Exec(ctx, "update "+table+" set title = 'ok' where id = 1")
			return err
		},
		func(ctx context.Context, c *pgx.Conn, table string) error { // the model holds it
			_, err := c.Exec(ctx, "update "+table+" set status = 'churned' where id = 2")
			return err
		},
		func(ctx context.Context, c *pgx.Conn, table string) error { // over the row limit
			_, err := c.Exec(ctx, "update "+table+" set status = 'x' where company_id = 5")
			return err
		},
		func(ctx context.Context, c *pgx.Conn, table string) error { // under it
			_, err := c.Exec(ctx, "update "+table+" set status = 'x' where company_id = 9")
			return err
		},
		func(ctx context.Context, c *pgx.Conn, table string) error { // unsupported
			_, err := c.Exec(ctx, "insert into "+table+" (id, name) values (30, 'u') on conflict (id) do update set name = excluded.name")
			return err
		},
		func(ctx context.Context, c *pgx.Conn, table string) error { // first after BEGIN
			return inTransaction(ctx, c, "update "+table+" set title = 'tx' where id = 3")
		},
		func(ctx context.Context, c *pgx.Conn, table string) error { // after another statement
			return inTransaction(ctx, c, "select 1", "update "+table+" set title = 'dirty' where id = 4")
		},
		func(ctx context.Context, c *pgx.Conn, table string) error { // a single-value IN
			_, err := c.Exec(ctx, "update "+table+" set title = 'in' where id in (5)")
			return err
		},
	}
	verdicts := map[string][][]string{}
	for _, mode := range []string{"shadow", "enforce"} {
		format := "agents: {roles: [stet_md5]}\n" + impactYAML
		if mode == "enforce" {
			format = enforcePrefix + impactYAML
		}
		e := setupEnforce(t, churnDecider{}, format)
		e.seedMany(t)
		mustExec(t, e.admin, "insert into "+e.table+" (id, name, company_id) values (20, 'a', 5), (21, 'b', 5), (22, 'c', 5), (23, 'd', 5)")
		conn := e.agent(t, mode)
		for i, w := range writes {
			if err := w(context.Background(), conn, e.table); err != nil && mode == "shadow" {
				t.Errorf("shadow write %d: %v", i, err)
			}
		}
		for i, r := range e.enforced(t, len(writes)) {
			verdicts[mode] = append(verdicts[mode], append([]string{r.Verdict}, r.Reasons...))
			if mode == "shadow" && (!r.Executed || r.Refused != nil) {
				t.Errorf("shadow decision %d: executed=%v refused=%v, want executed and NULL", i, r.Executed, deref(r.Refused))
			}
		}
	}
	if !reflect.DeepEqual(verdicts["shadow"], verdicts["enforce"]) {
		t.Errorf("verdicts differ\nshadow  %v\nenforce %v", verdicts["shadow"], verdicts["enforce"])
	}
	want := [][]string{
		{"commit"}, {"hold", "policy:1"}, {"hold", "impact:rows"}, {"commit"},
		{"unsupported", "on_conflict"}, {"commit"}, {"unsupported", "in_transaction"}, {"commit"},
	}
	if !reflect.DeepEqual(verdicts["enforce"], want) {
		t.Errorf("enforce verdicts %v, want %v", verdicts["enforce"], want)
	}
}

// inTransaction runs sqls after BEGIN, and commits when all succeed or
// rolls back at the first error, which it returns.
func inTransaction(ctx context.Context, c *pgx.Conn, sqls ...string) error {
	if _, err := c.Exec(ctx, "begin"); err != nil {
		return err
	}
	for _, sql := range sqls {
		if _, err := c.Exec(ctx, sql); err != nil {
			c.Exec(ctx, "rollback")
			return err
		}
	}
	_, err := c.Exec(ctx, "commit")
	return err
}

// The model's duplicate answer names a row only when it is one of the
// candidates Stet sent: model output is shaped by agent-written data and
// must not put text of its own into an error (CLAUDE.md rule 5).
func TestEnforceNamesOnlyACandidateAsTheDuplicate(t *testing.T) {
	d := calm()
	e := setupEnforce(t, d)
	e.seed(t)
	conn := e.agent(t, "sales")
	insert := "insert into " + e.table + " (id, name, email) values (%d, 'Kim J', 'K@ACME.com')"

	d.setAnswers(map[string]decide.Answer{
		"duplicate": {Choice: "1", Confidence: 0.95},
		"policy_1":  {Choice: "false", Probability: ptr(0.05)},
		"policy_2":  {Choice: "false", Probability: ptr(0.05)},
	})
	_, err := conn.Exec(context.Background(), fmt.Sprintf(insert, 3))
	pgErr := wantRefused(t, err, "ST001", "")
	if want := "write held by stet: possible duplicate of " + e.table + ".id=1"; pgErr.Message != want {
		t.Errorf("message = %q, want %q", pgErr.Message, want)
	}

	d.setAnswers(map[string]decide.Answer{
		"duplicate": {Choice: "7 (see row\nevil)", Confidence: 0.95},
		"policy_1":  {Choice: "false", Probability: ptr(0.05)},
		"policy_2":  {Choice: "false", Probability: ptr(0.05)},
	})
	_, err = conn.Exec(context.Background(), fmt.Sprintf(insert, 4))
	pgErr = wantRefused(t, err, "ST001", "")
	if want := "write held by stet: possible duplicate of another row in " + e.table; pgErr.Message != want {
		t.Errorf("message = %q, want %q", pgErr.Message, want)
	}
	if strings.Contains(pgErr.Detail+pgErr.Hint, "evil") || !strings.HasSuffix(pgErr.Detail, "reasons=duplicate:") {
		t.Errorf("detail %q hint %q, want the reason without the model's id", pgErr.Detail, pgErr.Hint)
	}

	rs := e.enforced(t, 2)
	wantEnforced(t, 0, rs[0], "hold", "duplicate:1")
	wantEnforced(t, 1, rs[1], "hold", "duplicate:")
	if hs := e.held(t, 2); !reflect.DeepEqual(hs[1].Reasons, []string{"duplicate:"}) {
		t.Errorf("held write reasons %v, want [duplicate:]", hs[1].Reasons)
	}
}

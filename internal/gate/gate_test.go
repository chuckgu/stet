package gate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/store"
)

// fakeDecider answers every question it is asked from a fixed table.
type fakeDecider struct {
	answers map[string]decide.Answer
	err     error
	delay   time.Duration
	// onDecide, when set, is called inside Decide with the call's number,
	// counted from 1, before it answers.
	onDecide func(call int)

	mu    sync.Mutex
	calls []fakeCall
}

type fakeCall struct {
	state     map[string]any
	raw       string // the bytes sent
	questions []decide.Question
}

func (f *fakeDecider) Decide(ctx context.Context, state json.RawMessage, qs []decide.Question) (decide.Answers, error) {
	time.Sleep(f.delay)
	var s map[string]any
	json.Unmarshal(state, &s)
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{s, string(state), qs})
	call, hook := len(f.calls), f.onDecide
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if f.err != nil {
		return decide.Answers{}, f.err
	}
	out := map[string]decide.Answer{}
	f.mu.Lock()
	for _, q := range qs {
		if a, ok := f.answers[q.ID]; ok {
			out[q.ID] = a
		}
	}
	f.mu.Unlock()
	return decide.Answers{ByQuestion: out, CostUSD: 0.0002}, nil
}

// setAnswers replaces the table f answers from, between calls.
func (f *fakeDecider) setAnswers(answers map[string]decide.Answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = answers
}

func (f *fakeDecider) ModelID() string      { return "fake" }
func (f *fakeDecider) ModelVersion() string { return "fake-1" }

func (f *fakeDecider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeDecider) lastCall(t *testing.T) fakeCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the model was never called")
	}
	return f.calls[len(f.calls)-1]
}

// calm answers "no problem" with high confidence.
func calm() *fakeDecider {
	return &fakeDecider{answers: map[string]decide.Answer{
		"duplicate": {Choice: "none", Confidence: 0.95},
		"conflict":  {Choice: "no", Confidence: 0.95},
		"policy_1":  {Choice: "false", Probability: ptr(0.05)},
		"policy_2":  {Choice: "false", Probability: ptr(0.05)},
	}}
}

type env struct {
	table string
	cfg   *config.Config
	pool  *pgxpool.Pool
	admin *pgx.Conn
	gate  *gate.Gate
	srv   *proxy.Server
	addr  string
}

const tableYAML = `
listen: 127.0.0.1:0
upstream: postgres://stet_scram@%[1]s/stet_test
model: {adapter: fake, version: fake-1, timeout_ms: 2000}
tables:
  %[2]s:
    pk: id
    entity: "B2B CRM contact"
    send_fields: [name, email, company_id, title, status]
    candidates:
      sql: |
        select id, name, email from %[2]s
        where lower(email) = lower(:email) or name = :name
    recent_window: 30m
    policy: |
      Do not set status to churned.
      Do not overwrite a title with an older value from a stale source.
`

// setup makes a fresh contacts table, a gate over it, and a proxy in front.
func setup(t *testing.T, d decide.Decider, yamlFormat ...string) *env {
	t.Helper()
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	ctx := context.Background()
	e := &env{table: fmt.Sprintf("contacts_%d", time.Now().UnixNano())}

	format := tableYAML
	if len(yamlFormat) > 0 {
		format = yamlFormat[0]
	}
	cfg, err := config.Parse(fmt.Appendf(nil, format, addr, e.table))
	if err != nil {
		t.Fatal(err)
	}
	e.cfg = cfg

	dsn := "postgres://stet_scram:scram-pw@" + addr + "/stet_test?sslmode=disable"
	e.admin, err = pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.admin.Close(ctx) })
	mustExec(t, e.admin, `create table `+e.table+` (
		id int primary key, name text, email text, company_id int,
		title text, status text, ssn text)`)
	t.Cleanup(func() { e.admin.Exec(ctx, "drop table "+e.table) })

	e.pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.pool.Close)
	st := store.New(e.pool)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	e.gate, err = gate.New(cfg, e.pool, st, d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := proxy.New(addr, slog.New(slog.DiscardHandler), e.gate, proxy.Policy{Agents: cfg.AgentRoles, Enforce: cfg.Mode == config.ModeEnforce})
	e.srv = srv
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		e.gate.Close()
	})
	e.addr = ln.Addr().String()
	return e
}

func (e *env) connect(t *testing.T, agent string) *pgx.Conn {
	t.Helper()
	u := url.URL{
		Scheme: "postgres", Host: e.addr, Path: "/stet_test",
		User:     url.UserPassword("stet_scram", "scram-pw"),
		RawQuery: url.Values{"sslmode": {"disable"}, "application_name": {agent}}.Encode(),
	}
	conn, err := pgx.Connect(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

type row struct {
	Verdict        string
	Reasons        []string
	Executed       bool
	ExecError      *string
	Op             string
	TargetPK       *string
	AgentID        string
	OnBehalfOf     *string
	Mode           string
	Model          string
	PolicyVersion  string
	Params         []*string
	Proposed       map[string]any
	State          map[string]any
	StateVersion   map[string]any
	Questions      []map[string]any
	Answers        map[string]any
	LatencyMS      *int
	ModelLatencyMS *int
}

// decisions waits for n decisions on the env's table, oldest first.
func (e *env) decisions(t *testing.T, n int) []row {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := e.admin.Query(context.Background(), `
			select verdict, reasons, executed, exec_error, op, target_pk #>> '{}', agent_id,
			       on_behalf_of, mode, model, policy_version, params, proposed, state,
			       state_version, questions, answers, latency_ms, model_latency_ms
			from stet.decisions where target_table = $1 order by created_at`, e.table)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[row])
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

func (e *env) seed(t *testing.T) {
	t.Helper()
	mustExec(t, e.admin, `insert into `+e.table+` values
		(1, 'Kim', 'k@acme.com', 3, 'Engineer', 'active', '900-00-0001'),
		(2, 'Lee', 'lee@acme.com', 3, 'PM', 'active', '900-00-0002')`)
}

func questionIDs(qs []map[string]any) []string {
	var ids []string
	for _, q := range qs {
		ids = append(ids, q["id"].(string))
	}
	return ids
}

func TestShadowRecordsJudgedUpdateAndStillWrites(t *testing.T) {
	d := &fakeDecider{answers: map[string]decide.Answer{
		"policy_1": {Choice: "true", Probability: ptr(0.9)},
		"policy_2": {Choice: "false", Probability: ptr(0.05)},
	}}
	e := setup(t, d)
	e.seed(t)
	conn := e.connect(t, "enricher")
	mustExec(t, conn, "update "+e.table+" set title = $1 where id = $2", "CTO", 1)

	r := e.decisions(t, 1)[0]
	var title string
	e.admin.QueryRow(context.Background(), "select title from "+e.table+" where id = 1").Scan(&title)
	if title != "CTO" {
		t.Fatalf("title = %q: shadow mode must not block the write", title)
	}

	if r.Verdict != "hold" || !reflect.DeepEqual(r.Reasons, []string{"policy:1"}) || !r.Executed {
		t.Errorf("verdict %s %v executed=%v, want hold [policy:1] executed", r.Verdict, r.Reasons, r.Executed)
	}
	if r.Op != "update" || r.TargetPK == nil || *r.TargetPK != "1" || r.AgentID != "enricher" || r.Mode != "shadow" {
		t.Errorf("op=%s pk=%v agent=%s mode=%s", r.Op, r.TargetPK, r.AgentID, r.Mode)
	}
	if r.Model != "fake" || r.PolicyVersion != e.cfg.Hash {
		t.Errorf("model=%s policy_version=%s, want fake/%s", r.Model, r.PolicyVersion, e.cfg.Hash)
	}
	if len(r.Params) != 2 || *r.Params[0] != "CTO" || *r.Params[1] != "1" {
		t.Errorf("params = %v, want [CTO 1] as text", r.Params)
	}
	if !reflect.DeepEqual(r.Proposed, map[string]any{"title": "CTO"}) {
		t.Errorf("proposed = %v", r.Proposed)
	}
	// Nothing recent touched this row, so there is nothing to undo.
	if got := questionIDs(r.Questions); !reflect.DeepEqual(got, []string{"policy_1", "policy_2"}) {
		t.Errorf("questions = %v", got)
	}
	if r.Answers["policy_1"] == nil || r.LatencyMS == nil || r.ModelLatencyMS == nil {
		t.Errorf("answers=%v latency=%v model_latency=%v", r.Answers, r.LatencyMS, r.ModelLatencyMS)
	}
	if r.StateVersion["xmin"] == nil {
		t.Errorf("state_version = %v, want the row's xmin", r.StateVersion)
	}

	cur := r.State["current"].(map[string]any)
	if cur["title"] != "Engineer" {
		t.Errorf("state.current.title = %v, want the pre-write value Engineer", cur["title"])
	}
	if _, leaked := cur["ssn"]; leaked {
		t.Error("ssn is outside send_fields but reached the model")
	}
	if c := r.State["candidates"].([]any); len(c) != 0 {
		t.Errorf("candidates = %v: the row being updated must not be its own candidate", c)
	}
	prov := r.State["provenance"].(map[string]any)
	if prov["proposed"] != "untrusted_agent_input" {
		t.Errorf("provenance = %v", prov)
	}
	if !reflect.DeepEqual(d.lastCall(t).state, r.State) {
		t.Error("recorded state differs from what the model was sent")
	}
}

func TestShadowInsertFindsDuplicateCandidate(t *testing.T) {
	d := calm()
	d.answers["duplicate"] = decide.Answer{Choice: "1", Confidence: 0.95}
	e := setup(t, d)
	e.seed(t)
	conn := e.connect(t, "sales-ops")
	if _, err := conn.PgConn().Exec(context.Background(),
		"insert into "+e.table+" (id, name, email) values (3, 'Kim J', 'K@ACME.com')").ReadAll(); err != nil {
		t.Fatal(err)
	}

	r := e.decisions(t, 1)[0]
	if r.Verdict != "hold" || !reflect.DeepEqual(r.Reasons, []string{"duplicate:1"}) || !r.Executed {
		t.Errorf("verdict %s %v executed=%v", r.Verdict, r.Reasons, r.Executed)
	}
	if r.State["current"] != nil {
		t.Errorf("insert has no current row, got %v", r.State["current"])
	}
	cands := r.State["candidates"].([]any)
	if len(cands) != 1 || cands[0].(map[string]any)["id"] != float64(1) {
		t.Errorf("candidates = %v, want row 1", cands)
	}
	q := d.lastCall(t).questions[0]
	if q.ID != "duplicate" || !reflect.DeepEqual(q.Choices, []string{"1", "none"}) {
		t.Errorf("first question = %+v", q)
	}
	if r.TargetPK == nil || *r.TargetPK != "3" {
		t.Errorf("target_pk = %v, want 3 from the insert", r.TargetPK)
	}
}

func TestShadowStateIncludesOtherAgentsRecentChanges(t *testing.T) {
	d := calm()
	d.delay = 200 * time.Millisecond // the first decision is not stored yet when the second write arrives
	e := setup(t, d)
	e.seed(t)
	mustExec(t, e.connect(t, "enricher"), "update "+e.table+" set title = 'VP' where id = 1")
	mustExec(t, e.connect(t, "stale-sync"), "update "+e.table+" set title = 'Engineer' where id = 1")

	rs := e.decisions(t, 2)
	recent := rs[1].State["recent_changes"].([]any)
	if len(recent) != 1 {
		t.Fatalf("recent_changes = %v, want the enricher's update", recent)
	}
	c := recent[0].(map[string]any)
	if c["by_agent"] != "enricher" || !reflect.DeepEqual(c["fields"], map[string]any{"title": "VP"}) {
		t.Errorf("recent change = %v", c)
	}
	// Models compare dates poorly, so the age is computed in code.
	if c["minutes_before_this_write"] != float64(0) {
		t.Errorf("minutes_before_this_write = %v, want 0", c["minutes_before_this_write"])
	}
}

func TestShadowUnsupportedIsRecordedWithoutModel(t *testing.T) {
	d := calm()
	e := setup(t, d)
	mustExec(t, e.connect(t, "bulk"), "insert into "+e.table+" (id, name) values (10, 'a'), (11, 'b')")

	r := e.decisions(t, 1)[0]
	if r.Verdict != "unsupported" || !reflect.DeepEqual(r.Reasons, []string{"multi_row"}) || !r.Executed {
		t.Errorf("verdict %s %v executed=%v", r.Verdict, r.Reasons, r.Executed)
	}
	if r.State != nil || r.Questions != nil || r.Answers != nil {
		t.Errorf("unsupported write was judged: state=%v questions=%v", r.State, r.Questions)
	}
	if d.callCount() != 0 {
		t.Errorf("model called %d times", d.callCount())
	}
}

func TestShadowWriteInTransactionIsNotJudged(t *testing.T) {
	d := calm()
	e := setup(t, d)
	e.seed(t)
	conn := e.connect(t, "tx-agent")
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "select 1"); err != nil { // makes the transaction dirty
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "update "+e.table+" set status = 'churned' where id = 2"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	r := e.decisions(t, 1)[0]
	if r.Verdict != "unsupported" || !reflect.DeepEqual(r.Reasons, []string{"in_transaction"}) || !r.Executed {
		t.Errorf("verdict %s %v executed=%v", r.Verdict, r.Reasons, r.Executed)
	}
	if d.callCount() != 0 {
		t.Errorf("model called inside a transaction")
	}
}

// A transaction that has run nothing but BEGIN is clean: its first write is
// judged as an autocommit write is, and measured (spec 4).
func TestShadowJudgesTheFirstWriteOfATransaction(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML)
	e.seedMany(t)
	conn := e.connect(t, "tx-agent")
	ctx := context.Background()
	for _, sql := range []string{
		"update " + e.table + " set title = 'x' where id = 1",
		"update " + e.table + " set status = 'x' where company_id = 9",
	} {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}

	rs, ims := e.decisions(t, 2), e.impacts(t, 2)
	if r := rs[0]; r.Verdict != "commit" || len(r.Reasons) != 0 || !r.Executed {
		t.Errorf("single-row write: verdict %s %v executed=%v, want commit [] executed", r.Verdict, r.Reasons, r.Executed)
	}
	if rs[0].Questions == nil || rs[0].Answers == nil {
		t.Errorf("single-row write: questions=%v answers=%v, want both recorded", rs[0].Questions, rs[0].Answers)
	}
	wantVerdict(t, ims[1], "commit")
	wantImpact(t, ims[1].Impact, map[string]any{"method": "count", "rows": 2.0, "actual_rows": 2.0})
	if !ims[1].Executed {
		t.Error("measured write: executed = false, want it to have run")
	}
	if n := d.callCount(); n != 1 {
		t.Errorf("model called %d times, want once: for the single-row write", n)
	}
}

// A write that finds every inflight slot taken until its time to read state
// is up is a state error, and follows on_failure (spec 7): pass, in shadow.
func TestInflightLimitTurnsIntoAStateError(t *testing.T) {
	d := calm()
	e := setup(t, d, tableYAML+"limits: {max_inflight: 1}\n")
	e.seed(t)
	conn := e.connect(t, "crowded")
	release := gate.HoldInflight(e.gate)
	mustExec(t, conn, "update "+e.table+" set title = 'crowded' where id = 1")
	release()
	mustExec(t, conn, "update "+e.table+" set title = 'later' where id = 2")
	// The only slot came back when the second write's Begin ended.
	mustExec(t, conn, "update "+e.table+" set title = 'again' where id = 2")

	rs := e.decisions(t, 3)
	if r := rs[2]; r.Verdict != "commit" || len(r.Reasons) != 0 {
		t.Errorf("third write: verdict %s %v, want commit []: the slot must come back", r.Verdict, r.Reasons)
	}
	if r := rs[0]; r.Verdict != "commit" || !reflect.DeepEqual(r.Reasons, []string{"state_error"}) || !r.Executed {
		t.Errorf("crowded write: verdict %s %v executed=%v, want commit [state_error] executed", r.Verdict, r.Reasons, r.Executed)
	}
	if l := rs[0].LatencyMS; l == nil || *l < 450 {
		t.Errorf("crowded write: latency_ms = %v, want at least 450: it waited for a slot", deref(l))
	}
	if r := rs[1]; r.Verdict != "commit" || len(r.Reasons) != 0 {
		t.Errorf("write after release: verdict %s %v, want commit []", r.Verdict, r.Reasons)
	}
	if n := d.callCount(); n != 2 {
		t.Errorf("model called %d times, want twice: not for the write with no state", n)
	}
	if n := e.rowsWhere(t, "title in ('crowded', 'again')"); n != 2 {
		t.Errorf("%d rows written, want 2: shadow mode must let the writes through", n)
	}
}

// A measured write with no slot is a state error too, and follows on_failure;
// it is not a measurement that failed, which unsupported would decide (spec 7).
func TestInflightLimitTurnsAMeasuredWriteIntoAStateError(t *testing.T) {
	d := calm()
	e := setup(t, d, impactYAML+"limits: {max_inflight: 1}\n")
	e.seedMany(t)
	release := gate.HoldInflight(e.gate)
	mustExec(t, e.connect(t, "crowded"), "update "+e.table+" set status = 'x' where company_id = 9")
	release()

	r := e.impacts(t, 1)[0]
	wantVerdict(t, r, "commit", "state_error")
	if r.Impact != nil {
		t.Errorf("impact = %v, want NULL: the write was not measured", r.Impact)
	}
	if !r.Executed {
		t.Error("executed = false: shadow mode must let the write through")
	}
	wantNoModel(t, d)
}

// In shadow mode a write's slot is let go when its Begin ends: the model is
// asked after the write, outside the limit, so a slow answer holds no slot.
func TestInflightSlotIsNotHeldAcrossTheShadowModelCall(t *testing.T) {
	d := calm()
	d.delay = time.Second
	e := setup(t, d, tableYAML+"limits: {max_inflight: 1}\n")
	e.seed(t)
	conn := e.connect(t, "a")
	mustExec(t, conn, "update "+e.table+" set title = 'first' where id = 1")
	mustExec(t, conn, "update "+e.table+" set title = 'second' where id = 2")
	if n := d.callCount(); n != 0 {
		t.Fatalf("%d model calls ended before the second write returned; the first must still be pending", n)
	}

	rs := e.decisions(t, 2)
	if r := rs[1]; r.Verdict != "commit" || len(r.Reasons) != 0 {
		t.Errorf("second write: verdict %s %v, want commit []: it must find the slot free", r.Verdict, r.Reasons)
	}
	if l := rs[1].LatencyMS; l == nil || *l >= 450 {
		t.Errorf("second write: latency_ms = %v, want well under 450: it must not wait for a slot", deref(l))
	}
}

func TestShadowModelErrorFallsBackToOnFailure(t *testing.T) {
	d := &fakeDecider{err: errors.New("upstream 503")}
	e := setup(t, d)
	e.seed(t)
	mustExec(t, e.connect(t, "a"), "update "+e.table+" set title = 'x' where id = 1")

	r := e.decisions(t, 1)[0]
	if r.Verdict != "commit" || !reflect.DeepEqual(r.Reasons, []string{"model_error"}) {
		t.Errorf("verdict %s %v, want commit [model_error] (on_failure: pass)", r.Verdict, r.Reasons)
	}
	if r.Questions == nil || r.Answers != nil {
		t.Errorf("questions=%v answers=%v: record what was asked, not answers", r.Questions, r.Answers)
	}
}

func TestShadowStateErrorDoesNotBlockTheWrite(t *testing.T) {
	broken := `
listen: 127.0.0.1:0
upstream: postgres://stet_scram@%[1]s/stet_test
model: {adapter: fake, version: fake-1}
tables:
  %[2]s:
    pk: id
    send_fields: [name, email]
    candidates:
      sql: select id from %[2]s where no_such_column = :email
`
	d := calm()
	e := setup(t, d, broken)
	mustExec(t, e.connect(t, "a"), "insert into "+e.table+" (id, email) values (1, 'x@y.com')")

	r := e.decisions(t, 1)[0]
	if r.Verdict != "commit" || !reflect.DeepEqual(r.Reasons, []string{"state_error"}) || !r.Executed {
		t.Errorf("verdict %s %v executed=%v", r.Verdict, r.Reasons, r.Executed)
	}
	if d.callCount() != 0 {
		t.Error("model called without a state")
	}
}

func TestShadowRecordsFailedWrites(t *testing.T) {
	e := setup(t, calm())
	e.seed(t)
	conn := e.connect(t, "a")
	if _, err := conn.Exec(context.Background(), "insert into "+e.table+" (id, name) values ($1, $2)", 1, "dup"); err == nil {
		t.Fatal("duplicate primary key was accepted")
	}

	r := e.decisions(t, 1)[0]
	if r.Executed || r.ExecError == nil {
		t.Errorf("executed=%v exec_error=%v, want the unique violation", r.Executed, r.ExecError)
	}
}

func TestShadowResolvesPositionalInsertAndExpressions(t *testing.T) {
	d := calm()
	e := setup(t, d)
	conn := e.connect(t, "a")
	mustExec(t, conn, "insert into "+e.table+" values (5, 'Park', 'p@acme.com', 3, 'PM', 'active', '900-00-0005')")
	mustExec(t, conn, "insert into "+e.table+" (id, email) values ($1, lower($2))", 6, "MIXED@Acme.com")

	rs := e.decisions(t, 2)
	if rs[0].Proposed["ssn"] != "900-00-0005" || rs[0].Proposed["name"] != "Park" {
		t.Errorf("positional proposed = %v", rs[0].Proposed)
	}
	if _, leaked := rs[0].State["proposed"].(map[string]any)["ssn"]; leaked {
		t.Error("ssn reached the model through proposed")
	}
	email := rs[1].State["proposed"].(map[string]any)["email"]
	if !reflect.DeepEqual(email, map[string]any{"expr": "lower('MIXED@Acme.com')"}) {
		t.Errorf("expression value = %v", email)
	}
}

func TestShadowCarriesOnBehalfOf(t *testing.T) {
	e := setup(t, calm())
	conn := e.connect(t, "assistant")
	mustExec(t, conn, "set stet.on_behalf_of = 'user:42'")
	mustExec(t, conn, "insert into "+e.table+" (id, name) values (1, 'x')")

	r := e.decisions(t, 1)[0]
	if r.OnBehalfOf == nil || *r.OnBehalfOf != "user:42" {
		t.Errorf("on_behalf_of = %v", r.OnBehalfOf)
	}
}

func TestRecordOnlyAdapterStillRecordsState(t *testing.T) {
	d, err := decide.New(config.Model{Adapter: config.RecordOnly})
	if err != nil {
		t.Fatal(err)
	}
	e := setup(t, d)
	e.seed(t)
	mustExec(t, e.connect(t, "a"), "update "+e.table+" set title = 'x' where id = 2")

	r := e.decisions(t, 1)[0]
	if r.Verdict != "commit" || !reflect.DeepEqual(r.Reasons, []string{"no_model"}) || r.Model != "none" {
		t.Errorf("verdict %s %v model %s", r.Verdict, r.Reasons, r.Model)
	}
	if r.State == nil || r.State["current"] == nil {
		t.Error("record-only mode must still record the state")
	}
}

func TestCloseWaitsForPendingJudgments(t *testing.T) {
	d := calm()
	d.delay = 300 * time.Millisecond
	e := setup(t, d)
	conn := e.connect(t, "a")
	mustExec(t, conn, "insert into "+e.table+" (id, name) values (1, 'x')")
	conn.Close(context.Background())
	time.Sleep(50 * time.Millisecond) // let the proxy see the outcome

	e.gate.Close()
	var n int
	e.admin.QueryRow(context.Background(), "select count(*) from stet.decisions where target_table = $1", e.table).Scan(&n)
	if n != 1 {
		t.Errorf("%d decisions after Close, want 1", n)
	}
}

func TestShadowHoldsOnTheBrokenRule(t *testing.T) {
	d := calm()
	d.answers["policy_2"] = decide.Answer{Choice: "true", Probability: ptr(0.88)}
	e := setup(t, d)
	e.seed(t)
	mustExec(t, e.connect(t, "stale-sync"), "update "+e.table+" set title = 'Engineer' where id = 1")

	r := e.decisions(t, 1)[0]
	if r.Verdict != "hold" || !reflect.DeepEqual(r.Reasons, []string{"policy:2"}) {
		t.Errorf("verdict %s %v, want hold [policy:2]", r.Verdict, r.Reasons)
	}
	q := d.lastCall(t).questions
	if q[len(q)-1].Type != "noul" || !strings.Contains(q[len(q)-1].Text, "older value from a stale source") {
		t.Errorf("last question = %+v, want the second rule as its own yes/no question", q[len(q)-1])
	}
	if p := r.State["policy"]; p != "Do not set status to churned.\nDo not overwrite a title with an older value from a stale source." {
		t.Errorf("state.policy = %q", p)
	}
}

func ptr[T any](v T) *T { return &v }

func TestShadowAsksConflictOnlyWhenARecentChangeCouldBeUndone(t *testing.T) {
	e := setup(t, calm())
	e.seed(t)
	mustExec(t, e.connect(t, "enricher"), "update "+e.table+" set title = 'VP' where id = 1")
	stale := e.connect(t, "stale-sync")
	mustExec(t, stale, "update "+e.table+" set status = 'inactive' where id = 1") // title was recent, status was not
	mustExec(t, stale, "update "+e.table+" set title = 'Engineer' where id = 1")  // undoes the enricher
	mustExec(t, stale, "delete from "+e.table+" where id = 2")                    // nothing recent on row 2
	mustExec(t, stale, "delete from "+e.table+" where id = 1")                    // row 1 changed moments ago

	rs := e.decisions(t, 5)
	for i, want := range []bool{false, false, true, false, true} {
		got := slices.Contains(questionIDs(rs[i].Questions), "conflict")
		if got != want {
			t.Errorf("decision %d (%s %v): conflict asked = %v, want %v",
				i, rs[i].Op, rs[i].Proposed, got, want)
		}
	}
}

const contextYAML = `
listen: 127.0.0.1:0
upstream: postgres://stet_scram@%[1]s/stet_test
model: {adapter: fake, version: fake-1}
tables:
  %[2]s:
    pk: id
    send_fields: [name, title, status]
    context:
      sql: select count(*) as same_company from %[2]s where company_id = :company_id
`

// Facts the policy depends on but the row does not hold (an account's open
// deals, say) reach the model through the table's context query.
func TestShadowStateIncludesContext(t *testing.T) {
	e := setup(t, calm(), contextYAML)
	e.seed(t)
	mustExec(t, e.connect(t, "a"), "update "+e.table+" set status = 'churned' where id = 1")
	mustExec(t, e.connect(t, "a"), "insert into "+e.table+" (id, name, company_id) values (9, 'New', 7)")

	rs := e.decisions(t, 2)
	if got := rs[0].State["context"]; !reflect.DeepEqual(got, map[string]any{"same_company": float64(2)}) {
		t.Errorf("update context = %v, want same_company 2 (bound from the current row)", got)
	}
	if got := rs[1].State["context"]; !reflect.DeepEqual(got, map[string]any{"same_company": float64(0)}) {
		t.Errorf("insert context = %v, want same_company 0 (bound from the proposed row)", got)
	}
}

func TestStateHasNoContextWhenNoneIsConfigured(t *testing.T) {
	e := setup(t, calm())
	e.seed(t)
	mustExec(t, e.connect(t, "a"), "update "+e.table+" set title = 'x' where id = 1")
	if _, ok := e.decisions(t, 1)[0].State["context"]; ok {
		t.Error("state has a context field although the table configures none; the format must not change")
	}
}

const replayYAML = `
listen: 127.0.0.1:0
upstream: postgres://stet_scram@%[1]s/stet_test
model: {adapter: fake, version: fake-1, timeout_ms: 2000}
tables:
  %[2]s:
    pk: id
    entity: "B2B CRM contact"
    send_fields: [name, email, company_id, title, status]
    candidates:
      sql: select id, name, email from %[2]s where lower(email) = lower(:email)
    context:
      sql: select count(*) as same_company, 12000.50 as open_amount from %[2]s where company_id = :company_id
    recent_window: 30m
    policy: |
      Do not set status to churned.
`

// Experiment arms judge recorded states (docs/lab.md). jsonb keeps neither
// key order nor the text of the state, and the order is part of the format
// (D19), so a recorded state must rebuild to exactly the bytes sent.
func TestRecordedStateRebuildsToTheBytesSent(t *testing.T) {
	d := calm()
	e := setup(t, d, replayYAML)
	e.seed(t)
	mustExec(t, e.connect(t, "enricher"), "update "+e.table+" set title = 'VP' where id = 1")
	mustExec(t, e.connect(t, "stale-sync"), "update "+e.table+" set title = 'Engineer', name = 'Kim & Co <x>' where id = 1")
	mustExec(t, e.connect(t, "sales-ops"), "insert into "+e.table+" (id, name, email, company_id) values (3, '김재훈', 'K@ACME.com', 3)")
	e.decisions(t, 3)

	rows, err := e.admin.Query(context.Background(),
		"select state::text from stet.decisions where target_table = $1 order by created_at", e.table)
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	var sent, rebuilt []string
	for _, c := range d.calls {
		sent = append(sent, c.raw)
	}
	reordered := false
	for _, r := range recorded {
		b, err := gate.RebuildState([]byte(r))
		if err != nil {
			t.Fatal(err)
		}
		rebuilt = append(rebuilt, string(b))
		reordered = reordered || string(b) != r
	}
	slices.Sort(sent)
	slices.Sort(rebuilt)
	if !reflect.DeepEqual(rebuilt, sent) {
		t.Errorf("rebuilt states differ from those sent:\nrebuilt %q\nsent    %q", rebuilt, sent)
	}
	if !reordered {
		t.Error("jsonb kept every state as sent; this test no longer covers the rebuild")
	}
}

// What the classifier cannot read and DO blocks are marked whatever the
// mode; the proxy refuses them only on enforced sessions (spec 7).
func TestInspectMarksWhatCannotBeInspected(t *testing.T) {
	e := setup(t, calm())
	for sql, want := range map[string]string{
		"selec 1":                      proxy.BlockUnparsed,
		"do $$ begin end $$":           proxy.BlockDo,
		"select 1; do $$ begin end $$": proxy.BlockDo,
		"select 1":                     "",
		"update " + e.table + " set title = 'x' where id = 1": "",
	} {
		if got := e.gate.Inspect(sql).Block; got != want {
			t.Errorf("Inspect(%q).Block = %q, want %q", sql, got, want)
		}
	}
	if st := e.gate.Inspect("update " + e.table + " set title = 'x' where id = 1"); !st.Watch {
		t.Errorf("a gated write is not watched: %+v", st)
	}
}

// What defines code, or a view over a gated table, is marked whatever the
// mode (ruling 29); a DO block keeps its own reason. A view over another
// table is not marked.
func TestInspectMarksWhatDefinesCode(t *testing.T) {
	e := setup(t, calm())
	for sql, want := range map[string]string{
		"create temp view v as select * from " + e.table:                    proxy.BlockDefinesCode,
		"create function f() returns int language sql as 'select 1'":        proxy.BlockDefinesCode,
		"create procedure p() language sql as 'select 1'":                   proxy.BlockDefinesCode,
		"create rule r as on insert to v do instead nothing":                proxy.BlockDefinesCode,
		"create trigger tr before insert on v execute function f()":         proxy.BlockDefinesCode,
		"select 1; create view v as select * from " + e.table:               proxy.BlockDefinesCode,
		"do $$ begin end $$; create function f() returns int as 'select 1'": proxy.BlockDo,
		"create view v as select * from pg_class":                           "",
	} {
		if got := e.gate.Inspect(sql).Block; got != want {
			t.Errorf("Inspect(%q).Block = %q, want %q", sql, got, want)
		}
	}
}

// A session the proxy ends gets decide.Blocked's error too, and its warning
// says the session was ended; a refused message's says it was refused.
func TestBlockedSaysWhatItEnded(t *testing.T) {
	e := setup(t, calm())
	var logs lockedBuffer
	gate.SetLogger(e.gate, slog.New(slog.NewJSONHandler(&logs, nil)))
	info := proxy.SessionInfo{ID: "f00d", User: "agent_a"}
	reasons := map[string]string{
		proxy.BlockDo:             "message refused without a decision",
		proxy.BlockSessionSetting: "message refused without a decision",
		proxy.BlockCopyExtended:   "session ended without a decision",
		proxy.BlockProtocolOrder:  "session ended without a decision",
		proxy.BlockSettingChanged: "session ended without a decision",
	}
	for reason := range reasons {
		got := e.gate.Blocked(info, reason)
		if ends := strings.HasPrefix(got.Message, "connection closed by stet: "); ends != proxy.EndsSession(reason) {
			t.Errorf("Blocked(%s).Message = %q; it ends the session: %v", reason, got.Message, proxy.EndsSession(reason))
		}
	}
	for line := range strings.Lines(logs.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if reason, _ := rec["reason"].(string); rec["level"] == "WARN" && rec["msg"] != reasons[reason] {
			t.Errorf("warning %q for %s, want %q", rec["msg"], reason, reasons[reason])
		}
	}
}

// Inspect passes on what SQL runs by name, as the server compares names, so
// the proxy can look the names up in the session's tables (spec correction
// B5).
func TestInspectReportsWhatRunsByName(t *testing.T) {
	e := setup(t, calm())
	for sql, want := range map[string][2][]string{
		"execute s1(1)":                {{"s1"}, nil},
		`explain analyze execute "S2"`: {{"S2"}, nil},
		"fetch all from P1":            {nil, {"p1"}},
		"move all in p2; execute s3":   {{"s3"}, {"p2"}},
		"select 1":                     {nil, nil},
		"update " + e.table + " set title = 'x' where id = 1": {nil, nil},
	} {
		st := e.gate.Inspect(sql)
		if !slices.Equal(st.Executes, want[0]) || !slices.Equal(st.Portals, want[1]) {
			t.Errorf("Inspect(%q): Executes %q Portals %q, want %q and %q", sql, st.Executes, st.Portals, want[0], want[1])
		}
	}
}

// A message refused without a decision gets decide.Blocked's error and a
// warning that names the session, its login role and the reason, and nothing
// of the statement.
func TestBlockedAnswersAndLogs(t *testing.T) {
	e := setup(t, calm())
	var logs lockedBuffer
	gate.SetLogger(e.gate, slog.New(slog.NewJSONHandler(&logs, nil)))
	info := proxy.SessionInfo{ID: "f00d", User: "agent_a", Database: "stet_test", ApplicationName: "bot"}

	got := e.gate.Blocked(info, proxy.BlockDo)
	a := decide.Blocked(proxy.BlockDo)
	if want := (proxy.Refusal{Code: a.Code, Message: a.Message, Hint: a.Hint("")}); got != want {
		t.Errorf("Blocked = %+v, want %+v", got, want)
	}
	if got.Code != "ST002" || got.Message == "" || got.Hint == "" || got.Detail != "" {
		t.Errorf("Blocked = %+v, want ST002 with a message and a hint, and no detail", got)
	}

	var warnings []map[string]any
	for line := range strings.Lines(logs.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["level"] == "WARN" {
			warnings = append(warnings, rec)
		}
	}
	if len(warnings) != 1 {
		t.Fatalf("%d warnings, want 1:\n%s", len(warnings), logs.String())
	}
	w := warnings[0]
	if w["session"] != "f00d" || w["login_role"] != "agent_a" || w["reason"] != "do_block" {
		t.Errorf("warning %v, want session f00d, login_role agent_a, reason do_block", w)
	}
	e.gate.Close() // waits for anything the gate was still recording
	var n int
	if err := e.admin.QueryRow(context.Background(),
		"select count(*) from stet.decisions where session_id = $1", info.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d decisions recorded for the session, want none", n)
	}
}

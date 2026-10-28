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

	mu    sync.Mutex
	calls []fakeCall
}

type fakeCall struct {
	state     map[string]any
	questions []decide.Question
}

func (f *fakeDecider) Decide(ctx context.Context, state json.RawMessage, qs []decide.Question) (decide.Answers, error) {
	time.Sleep(f.delay)
	var s map[string]any
	json.Unmarshal(state, &s)
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{s, qs})
	f.mu.Unlock()
	if f.err != nil {
		return decide.Answers{}, f.err
	}
	out := map[string]decide.Answer{}
	for _, q := range qs {
		if a, ok := f.answers[q.ID]; ok {
			out[q.ID] = a
		}
	}
	return decide.Answers{ByQuestion: out, CostUSD: 0.0002}, nil
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
		"policy":    {Choice: "1", Confidence: 0.95},
	}}
}

type env struct {
	table string
	cfg   *config.Config
	pool  *pgxpool.Pool
	admin *pgx.Conn
	gate  *gate.Gate
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
    policy: "Do not set status to churned."
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
	srv := proxy.New(addr, slog.New(slog.DiscardHandler), e.gate)
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
		"conflict": {Choice: "yes", Confidence: 0.9},
		"policy":   {Choice: "1", Confidence: 0.9},
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

	if r.Verdict != "hold" || !reflect.DeepEqual(r.Reasons, []string{"conflict"}) || !r.Executed {
		t.Errorf("verdict %s %v executed=%v, want hold [conflict] executed", r.Verdict, r.Reasons, r.Executed)
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
	if got := questionIDs(r.Questions); !reflect.DeepEqual(got, []string{"conflict", "policy"}) {
		t.Errorf("questions = %v", got)
	}
	if r.Answers["conflict"] == nil || r.LatencyMS == nil || r.ModelLatencyMS == nil {
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

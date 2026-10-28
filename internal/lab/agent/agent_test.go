package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/lab/tasks"
)

// fakeAPI replays scripted assistant turns and records each request body.
type fakeAPI struct {
	mu       sync.Mutex
	turns    []string
	requests []map[string]any
	headers  []http.Header
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	json.Unmarshal(body, &req)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.headers = append(f.headers, r.Header.Clone())
	turn := f.turns[0]
	if len(f.turns) > 1 {
		f.turns = f.turns[1:]
	}
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, turn)
}

func toolTurn(id, name, sql string) string {
	in, _ := json.Marshal(map[string]string{"sql": sql})
	return fmt.Sprintf(`{"id":"msg_%s","type":"message","role":"assistant","model":"claude-opus-5-5",
		"content":[{"type":"tool_use","id":"%s","name":"%s","input":%s}],
		"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":20}}`, id, id, name, in)
}

const endTurn = `{"id":"msg_end","type":"message","role":"assistant","model":"claude-opus-5-5",
	"content":[{"type":"text","text":"Registered the lead."}],
	"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":150,"output_tokens":10}}`

func testDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	ctx := context.Background()
	dsn := "postgres://stet_scram:scram-pw@" + addr + "/stet_test?sslmode=disable"
	table := fmt.Sprintf("agent_t_%d", time.Now().UnixNano())
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "create table "+table+" (id int primary key, name text)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Exec(ctx, "drop table "+table); conn.Close(ctx) })
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, table
}

func TestRunnerExecutesToolsAndReports(t *testing.T) {
	pool, table := testDB(t)
	api := &fakeAPI{turns: []string{
		toolTurn("t1", "query", "select count(*) as n from "+table),
		toolTurn("t2", "execute", "insert into "+table+" values (1, 'Kim')"),
		toolTurn("t3", "execute", "insert into "+table+" values (1, 'dup')"), // fails: the agent must see the error
		endTurn,
	}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	r := New(Config{APIKey: "test-key", BaseURL: srv.URL, Model: "claude-opus-5-5", Effort: "low", MaxTurns: 10},
		pool, slog.New(slog.DiscardHandler))
	res, err := r.Do(context.Background(), tasks.Task{ID: "lead-0001", Agent: tasks.SalesOps,
		Input: map[string]any{"name": "Kim", "email": "k@acme.com"}})
	if err != nil {
		t.Fatal(err)
	}

	var name string
	pool.QueryRow(context.Background(), "select name from "+table+" where id = 1").Scan(&name)
	if name != "Kim" {
		t.Errorf("row = %q: the execute tool did not write", name)
	}
	if res.Writes != 1 || res.Failed != 1 || res.Turns != 4 || res.Final != "Registered the lead." {
		t.Errorf("result = %+v, want 1 write, 1 failed write, 4 turns", res)
	}
	if res.InputTokens != 450 || res.OutputTokens != 70 {
		t.Errorf("tokens = %d in / %d out, want the sum over every turn", res.InputTokens, res.OutputTokens)
	}

	first := api.requests[0]
	if first["model"] != "claude-opus-5-5" || first["fallbacks"] != "default" {
		t.Errorf("request model=%v fallbacks=%v", first["model"], first["fallbacks"])
	}
	if oc, _ := first["output_config"].(map[string]any); oc["effort"] != "low" {
		t.Errorf("output_config = %v, want effort low", first["output_config"])
	}
	if !strings.Contains(api.headers[0].Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("beta header = %q", api.headers[0].Get("Anthropic-Beta"))
	}
	if !strings.Contains(fmt.Sprint(first["messages"]), "k@acme.com") {
		t.Error("the task input is not in the first message")
	}
	// v0.1 judges one row per statement, so the tool asks for exactly that.
	for _, tool := range first["tools"].([]any) {
		tl := tool.(map[string]any)
		if tl["name"] == "execute" && !strings.Contains(tl["description"].(string), "one row per statement") {
			t.Errorf("execute description = %q, want it to ask for one row per statement", tl["description"])
		}
	}
	// The query result and the failed write's error both reach the model.
	if s := fmt.Sprint(api.requests[1]["messages"]); !strings.Contains(s, `"n"`) {
		t.Errorf("query result missing from the next request: %s", s)
	}
	if s := fmt.Sprint(api.requests[3]["messages"]); !strings.Contains(s, "23505") {
		t.Errorf("unique-violation error missing from the next request")
	}
}

func TestEveryAgentHasAPrompt(t *testing.T) {
	for _, a := range []string{tasks.SalesOps, tasks.Enricher, tasks.Notes, tasks.Churn} {
		sys, user := prompts(tasks.Task{ID: "x", Agent: a, Input: map[string]any{"k": "v"}})
		if sys == "" || !strings.Contains(user, `"k": "v"`) {
			t.Errorf("%s: system %d chars, user %q", a, len(sys), user)
		}
		// Agents get no policy rules: traps must be able to happen.
		if strings.Contains(strings.ToLower(sys), "open deal") || strings.Contains(strings.ToLower(sys), "consent_source") && strings.Contains(sys, "must") {
			t.Errorf("%s: system prompt gives away a policy rule", a)
		}
	}
}

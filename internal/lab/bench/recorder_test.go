package bench

import (
	"bytes"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const fakeReply = `{"content":[{"type":"tool_use","name":"report_high_risk","input":{"title":"Unbounded update","explanation":"No WHERE clause"}}],"stop_reason":"tool_use","usage":{"input_tokens":1500,"output_tokens":150}}`
const fakeRequest = `{"model":"claude-haiku-5-5","max_tokens":1024,"system":"s","messages":[{"role":"user","content":"Protocol: postgres\nOperation: update\n\n/* w:123456789012 */ update contacts set status = 'inactive'"}],"tools":[],"tool_choice":{"type":"any"}}`

const testKey = "sk-test-SECRET"

// fakeAnthropic answers every request with status and body, and keeps what it got.
type fakeAnthropic struct {
	*httptest.Server
	mu      sync.Mutex
	hits    int
	path    string
	headers http.Header
	body    []byte
}

func newFakeAnthropic(t *testing.T, status int, reply string) *fakeAnthropic {
	f := &fakeAnthropic{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits++
		f.path, f.headers, f.body = r.URL.Path, r.Header.Clone(), b
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAnthropic) hitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func postMessages(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", testKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func newTestRecorder(t *testing.T, upstream string, capUSD, spent float64) (*httptest.Server, *bytes.Buffer) {
	out := &bytes.Buffer{}
	srv := httptest.NewServer(NewRecorder(upstream, out, capUSD, spent))
	t.Cleanup(srv.Close)
	return srv, out
}

func parseCalls(t *testing.T, out *bytes.Buffer) []Call {
	t.Helper()
	path := filepath.Join(t.TempDir(), "calls.jsonl")
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	calls, err := ReadCalls(path)
	if err != nil {
		t.Fatal(err)
	}
	return calls
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestRecorderForwardsAndLogs(t *testing.T) {
	fake := newFakeAnthropic(t, 200, fakeReply)
	srv, out := newTestRecorder(t, fake.URL, 10, 0)
	status, body := postMessages(t, srv.URL, fakeRequest)
	if status != 200 || body != fakeReply {
		t.Fatalf("client got %d %q", status, body)
	}
	if fake.path != "/v1/messages" || fake.headers.Get("x-api-key") != testKey ||
		fake.headers.Get("anthropic-version") != "2023-06-01" || string(fake.body) != fakeRequest {
		t.Fatalf("upstream got path %q headers %v body %q", fake.path, fake.headers, fake.body)
	}
	calls := parseCalls(t, out)
	if len(calls) != 1 {
		t.Fatalf("%d calls", len(calls))
	}
	c := calls[0]
	cost := 1500/1e6*0.10 + 150/1e6*0.50
	if c.Status != 200 || c.Model != "claude-haiku-5-5" || c.InputTokens != 1500 || c.OutputTokens != 150 ||
		c.Level != "high" || c.Title != "Unbounded update" || c.Explanation != "No WHERE clause" ||
		c.StopReason != "tool_use" || c.Tag != "123456789012" || !strings.Contains(c.Input, "update contacts") ||
		!near(c.CostUSD, cost) || !near(c.TotalUSD, c.CostUSD) || c.Event != "" || c.Time.IsZero() {
		t.Fatalf("call %+v", c)
	}
	if !near(c.CostUSD, 0.000225) {
		t.Fatalf("cost %v", c.CostUSD)
	}
	if s := out.String(); strings.Contains(s, testKey) || strings.Contains(strings.ToLower(s), "x-api-key") {
		t.Fatalf("the log holds the key or a header: %s", s)
	}
}

func TestRecorderNeverLogsTheKey(t *testing.T) {
	// Even an upstream that echoes the key back must not get it into the log
	// through anything but the response the client sees; the file holds only
	// Call fields, and the test client's own output holds nothing of the key.
	fake := newFakeAnthropic(t, 200, fakeReply)
	srv, out := newTestRecorder(t, fake.URL, 10, 0)
	// Capture the process's stdout and stderr while the recorder runs.
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, _ := os.Pipe()
	os.Stdout, os.Stderr = w, w
	postMessages(t, srv.URL, fakeRequest)
	postMessages(t, srv.URL, `not json`)
	w.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	captured, _ := io.ReadAll(r)
	if bytes.Contains(captured, []byte(testKey)) || strings.Contains(out.String(), testKey) {
		t.Fatalf("the key leaked: %q / %q", captured, out.String())
	}
}

func TestRecorderPricesOpusAndUnknownModels(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "some-model"} {
		fake := newFakeAnthropic(t, 200, fakeReply)
		srv, out := newTestRecorder(t, fake.URL, 10, 0)
		postMessages(t, srv.URL, strings.Replace(fakeRequest, "claude-haiku-5-5", model, 1))
		c := parseCalls(t, out)[0]
		if c.Model != model || !near(c.CostUSD, 0.009) {
			t.Fatalf("%s: %+v", model, c)
		}
	}
}

func TestRecorderStopsAtTheCap(t *testing.T) {
	fake := newFakeAnthropic(t, 200, fakeReply)
	srv, out := newTestRecorder(t, fake.URL, 0.0004, 0)
	var statuses []int
	var third string
	for i := 0; i < 3; i++ {
		s, b := postMessages(t, srv.URL, fakeRequest)
		statuses = append(statuses, s)
		third = b
	}
	if statuses[0] != 200 || statuses[1] != 200 || statuses[2] != 403 {
		t.Fatalf("statuses %v", statuses)
	}
	if !strings.Contains(third, `"type":"cost_cap"`) || !strings.Contains(third, "the benchmark's cost cap is reached") {
		t.Fatalf("refusal body %q", third)
	}
	if fake.hitCount() != 2 {
		t.Fatalf("the fake was hit %d times", fake.hitCount())
	}
	calls := parseCalls(t, out)
	if len(calls) != 3 || !near(calls[0].TotalUSD, 0.000225) || !near(calls[1].TotalUSD, 0.00045) {
		t.Fatalf("calls %+v", calls)
	}
	if calls[2].Event != "cap_reached" || calls[2].CostUSD != 0 || calls[2].Status != 403 {
		t.Fatalf("third call %+v", calls[2])
	}
}

func TestRecorderCountsTheFilesBesideIt(t *testing.T) {
	dir := t.TempDir()
	earlier := `{"cost_usd":0.0001}` + "\n" + `{"cost_usd":0.0002}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "earlier.jsonl"), []byte(earlier), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(`{"cost_usd":5}`), 0o600); err != nil {
		t.Fatal(err)
	}
	spent, err := SpentIn(dir)
	if err != nil || !near(spent, 0.0003) {
		t.Fatalf("spent %v, %v", spent, err)
	}
	fake := newFakeAnthropic(t, 200, fakeReply)
	srv, out := newTestRecorder(t, fake.URL, 0.0004, spent)
	s1, _ := postMessages(t, srv.URL, fakeRequest)
	s2, _ := postMessages(t, srv.URL, fakeRequest)
	if s1 != 200 || s2 != 403 || fake.hitCount() != 1 {
		t.Fatalf("statuses %d %d, hits %d", s1, s2, fake.hitCount())
	}
	calls := parseCalls(t, out)
	if !near(calls[0].TotalUSD, 0.000525) {
		t.Fatalf("total %v", calls[0].TotalUSD)
	}
}

func TestRecorderPassesErrorsThrough(t *testing.T) {
	const errBody = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
	fake := newFakeAnthropic(t, 529, errBody)
	srv, out := newTestRecorder(t, fake.URL, 10, 0)
	status, body := postMessages(t, srv.URL, fakeRequest)
	if status != 529 || body != errBody {
		t.Fatalf("client got %d %q", status, body)
	}
	c := parseCalls(t, out)[0]
	if c.Status != 529 || c.Level != "" || c.CostUSD != 0 || c.Tag != "123456789012" {
		t.Fatalf("call %+v", c)
	}
}

func TestRecorderLogsARequestWithoutATag(t *testing.T) {
	fake := newFakeAnthropic(t, 200, fakeReply)
	srv, out := newTestRecorder(t, fake.URL, 10, 0)
	status, _ := postMessages(t, srv.URL, `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hello"}]}`)
	c := parseCalls(t, out)[0]
	if status != 200 || c.Tag != "" || c.Input != "hello" {
		t.Fatalf("%d %+v", status, c)
	}
}

func TestReadCalls(t *testing.T) {
	fake := newFakeAnthropic(t, 200, fakeReply)
	srv, out := newTestRecorder(t, fake.URL, 10, 0)
	for i := 0; i < 3; i++ {
		postMessages(t, srv.URL, fakeRequest)
	}
	calls := parseCalls(t, out)
	if len(calls) != 3 {
		t.Fatalf("%d calls", len(calls))
	}
	for i := 1; i < 3; i++ {
		if calls[i].TotalUSD <= calls[i-1].TotalUSD {
			t.Fatalf("not in order: %+v", calls)
		}
	}
}

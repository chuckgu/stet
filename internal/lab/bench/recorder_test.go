package bench

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	return postWith(t, url+"/v1/messages", body, nil)
}

// postWith posts body to url; an Accept-Encoding in extra is sent as is, so
// the client does not decode the reply itself.
func postWith(t *testing.T, url, body string, extra map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-api-key", testKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	// A redirect the recorder relays is the client's to follow, not the test's.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// logBuffer is a log the test reads while the recorder may still write.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.b.Bytes())
}

func (l *logBuffer) String() string { return string(l.Bytes()) }

func newTestRecorder(t *testing.T, upstream string, capUSD, spent float64) (*httptest.Server, *logBuffer) {
	out := &logBuffer{}
	srv := httptest.NewServer(NewRecorder(upstream, out, capUSD, spent))
	t.Cleanup(srv.Close)
	return srv, out
}

func parseCalls(t *testing.T, out *logBuffer) []Call {
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

// capture returns what the process wrote to stdout, stderr and the default
// logger while f ran.
func capture(t *testing.T, f func()) string {
	t.Helper()
	oldOut, oldErr, oldLog := os.Stdout, os.Stderr, log.Writer()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var captured []byte
	done := make(chan struct{})
	go func() { captured, _ = io.ReadAll(r); close(done) }()
	os.Stdout, os.Stderr = w, w
	log.SetOutput(w)
	defer func() {
		os.Stdout, os.Stderr = oldOut, oldErr
		log.SetOutput(oldLog)
	}()
	f()
	w.Close()
	<-done
	return string(captured)
}

func TestRecorderNeverLogsTheKey(t *testing.T) {
	// The upstream echoes the request headers in an error body, answers with
	// garbage, closes mid-body, and then is gone: every path that makes the
	// recorder write an error string of its own.
	mode := "echo"
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		m := mode
		mu.Unlock()
		switch m {
		case "echo":
			w.WriteHeader(400)
			io.WriteString(w, `{"type":"error","error":{"message":"headers: `+r.Header.Get("x-api-key")+`"}}`)
		case "garbage":
			io.WriteString(w, `not json`)
		case "cut":
			conn, buf, _ := w.(http.Hijacker).Hijack()
			buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\npartial")
			buf.Flush()
			conn.Close()
		}
	}))
	defer up.Close()
	srv, out := newTestRecorder(t, up.URL, 10, 0)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	deadSrv, deadOut := newTestRecorder(t, deadURL, 10, 0)

	var generated []string // bodies the recorder itself wrote
	captured := capture(t, func() {
		for _, m := range []string{"echo", "garbage", "cut"} {
			mu.Lock()
			mode = m
			mu.Unlock()
			_, b := postMessages(t, srv.URL, fakeRequest)
			if m == "cut" {
				generated = append(generated, b)
			}
		}
		_, b := postMessages(t, deadSrv.URL, fakeRequest)
		generated = append(generated, b)
		postMessages(t, srv.URL, `not json`)
	})
	for _, s := range append(generated, captured, out.String(), deadOut.String()) {
		if strings.Contains(s, testKey) {
			t.Fatalf("the key leaked: %q", s)
		}
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

func gz(t *testing.T, s string) string {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write([]byte(s))
	zw.Close()
	return b.String()
}

func TestRecorderRelaysGzipUnchangedAndParsesIt(t *testing.T) {
	zipped := gz(t, fakeReply)
	var gotEncoding string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		io.WriteString(w, zipped)
	}))
	defer up.Close()
	srv, out := newTestRecorder(t, up.URL, 10, 0)
	status, body := postWith(t, srv.URL+"/v1/messages", fakeRequest, map[string]string{"Accept-Encoding": "gzip"})
	if status != 200 || body != zipped || gotEncoding != "gzip" {
		t.Fatalf("status %d, encoding %q, body relayed unchanged: %v", status, gotEncoding, body == zipped)
	}
	c := parseCalls(t, out)[0]
	if c.InputTokens != 1500 || c.Level != "high" || c.Event != "" || !near(c.CostUSD, 0.000225) {
		t.Fatalf("call %+v", c)
	}
}

func TestRecorderAsksUpstreamForAnEncodingItCanRead(t *testing.T) {
	// A client that does not take gzip (or asks for br) is sent identity, so
	// the usage stays readable and the client can decode what it gets.
	for _, ae := range []string{"", "br", "zstd, deflate", "identity"} {
		var got string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get("Accept-Encoding")
			io.WriteString(w, fakeReply)
		}))
		srv, out := newTestRecorder(t, up.URL, 10, 0)
		extra := map[string]string{"Accept-Encoding": ae}
		if ae == "" {
			extra = map[string]string{"Accept-Encoding": "identity"}
		}
		status, body := postWith(t, srv.URL+"/v1/messages", fakeRequest, extra)
		up.Close()
		if status != 200 || body != fakeReply || got != "identity" {
			t.Fatalf("client asked %q: upstream was asked %q, client got %d %q", ae, got, status, body)
		}
		if c := parseCalls(t, out)[0]; c.Event != "" || c.InputTokens != 1500 {
			t.Fatalf("call %+v", c)
		}
	}
}

// worst is what a call whose usage cannot be read is charged: the request's
// bytes as input tokens (a token is never shorter than a byte) and max_tokens
// as output.
func worst(model, request string, maxTokens int) float64 {
	return price(model, len(request), maxTokens)
}

func TestRecorderChargesTheWorstCaseWhenUsageCannotBeRead(t *testing.T) {
	for _, reply := range []string{`not json`, `{"content":[],"stop_reason":"end_turn"}`, `{"usage":`} {
		fake := newFakeAnthropic(t, 200, reply)
		srv, out := newTestRecorder(t, fake.URL, 10, 0)
		status, body := postMessages(t, srv.URL, fakeRequest)
		c := parseCalls(t, out)[0]
		if status != 200 || body != reply {
			t.Fatalf("client got %d %q", status, body)
		}
		if c.Event != "unparsed" || !near(c.CostUSD, worst("claude-haiku-5-5", fakeRequest, 1024)) || !near(c.TotalUSD, c.CostUSD) {
			t.Fatalf("reply %q: call %+v", reply, c)
		}
	}
}

func TestRecorderChargesTheWorstCaseWhenTheReplyIsCutShort(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, _ := w.(http.Hijacker).Hijack()
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\npartial")
		buf.Flush()
		conn.Close()
	}))
	defer up.Close()
	srv, out := newTestRecorder(t, up.URL, 10, 0)
	status, _ := postMessages(t, srv.URL, fakeRequest)
	c := parseCalls(t, out)[0]
	if status != 502 || c.Event != "upstream_error" || c.Status != 502 || !near(c.CostUSD, worst("claude-haiku-5-5", fakeRequest, 1024)) {
		t.Fatalf("status %d call %+v", status, c)
	}
}

func TestRecorderUpstreamUnreachableCostsNothing(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	srv, out := newTestRecorder(t, url, 10, 0)
	status, body := postMessages(t, srv.URL, fakeRequest)
	c := parseCalls(t, out)[0]
	if status != 502 || !strings.Contains(body, "error") || c.Status != 502 || c.Event != "upstream_error" || c.CostUSD != 0 || c.Tag != "123456789012" {
		t.Fatalf("status %d %q call %+v", status, body, c)
	}
}

func TestRecorderFinishesACallTheClientGaveUpOn(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		io.WriteString(w, fakeReply)
	}))
	defer up.Close()
	srv, out := newTestRecorder(t, up.URL, 10, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/messages", strings.NewReader(fakeRequest))
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("the client did not give up")
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(out.Bytes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	calls := parseCalls(t, out)
	if len(calls) != 1 || calls[0].Status != 200 || calls[0].InputTokens != 1500 || !near(calls[0].CostUSD, 0.000225) {
		t.Fatalf("calls %+v", calls)
	}
}

func TestRecorderDoesNotFollowRedirects(t *testing.T) {
	other := newFakeAnthropic(t, 200, fakeReply)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/messages", http.StatusTemporaryRedirect)
	}))
	defer up.Close()
	srv, out := newTestRecorder(t, up.URL, 10, 0)
	status, _ := postMessages(t, srv.URL, fakeRequest)
	c := parseCalls(t, out)[0]
	if status != 307 || other.hitCount() != 0 || c.Status != 307 || c.CostUSD != 0 {
		t.Fatalf("status %d, other hit %d, call %+v", status, other.hitCount(), c)
	}
}

func TestRecorderForwardsOnlyPostMessages(t *testing.T) {
	fake := newFakeAnthropic(t, 200, fakeReply)
	srv, out := newTestRecorder(t, fake.URL, 10, 0)
	if s, _ := postWith(t, srv.URL+"/v1/other", fakeRequest, nil); s != 404 {
		t.Fatalf("other path: %d", s)
	}
	resp, err := http.Get(srv.URL + "/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	if fake.hitCount() != 0 || out.String() != "" {
		t.Fatalf("hits %d, log %q", fake.hitCount(), out.String())
	}
	if s, _ := postWith(t, srv.URL+"/v1/messages?beta=true", fakeRequest, nil); s != 200 || fake.hitCount() != 1 {
		t.Fatalf("with a query: %d, hits %d", s, fake.hitCount())
	}
}

// failingWriter is a log that cannot be written.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestRecorderStopsWhenTheLogCannotBeWritten(t *testing.T) {
	fake := newFakeAnthropic(t, 200, fakeReply)
	srv := httptest.NewServer(NewRecorder(fake.URL, failingWriter{}, 10, 0))
	defer srv.Close()
	var s1, s2 int
	captured := capture(t, func() {
		s1, _ = postMessages(t, srv.URL, fakeRequest)
		s2, _ = postMessages(t, srv.URL, fakeRequest)
	})
	if s1 < 500 || s2 < 500 || fake.hitCount() != 1 {
		t.Fatalf("statuses %d %d, upstream hit %d times", s1, s2, fake.hitCount())
	}
	if !strings.Contains(captured, "disk full") || strings.Contains(captured, testKey) {
		t.Fatalf("stderr: %q", captured)
	}
}

func TestRecorderSaysOnceWhenTheCapTrips(t *testing.T) {
	fake := newFakeAnthropic(t, 200, fakeReply)
	srv, out := newTestRecorder(t, fake.URL, 0.0004, 0)
	captured := capture(t, func() {
		for i := 0; i < 4; i++ {
			postMessages(t, srv.URL, fakeRequest)
		}
	})
	if n := strings.Count(captured, "cost cap"); n != 1 {
		t.Fatalf("%d cap lines in %q", n, captured)
	}
	if calls := parseCalls(t, out); len(calls) != 4 || calls[3].Event != "cap_reached" {
		t.Fatalf("calls %+v", calls)
	}
}

func TestRecorderKeepsTheSpendConsistentUnderConcurrency(t *testing.T) {
	fake := newFakeAnthropic(t, 200, fakeReply)
	srv, out := newTestRecorder(t, fake.URL, 100, 0)
	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); postMessages(t, srv.URL, fakeRequest) }()
	}
	wg.Wait()
	calls := parseCalls(t, out)
	if len(calls) != n {
		t.Fatalf("%d calls", len(calls))
	}
	sum := 0.0
	for i, c := range calls {
		sum += c.CostUSD
		if !near(c.TotalUSD, sum) {
			t.Fatalf("line %d: total %v, running sum %v", i, c.TotalUSD, sum)
		}
	}
	if !near(sum, n*0.000225) {
		t.Fatalf("sum %v", sum)
	}
}

package jev

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
)

var questions = []decide.Question{
	{
		ID: "duplicate", Type: "choice", Text: "Same entity?",
		Choices:      []string{"4812", "none"},
		Descriptions: map[string]string{"4812": "same as 4812", "none": "different"},
	},
	{
		ID: "policy", Type: "score", Text: "How bad?",
		Choices:      []string{"1", "2", "3", "4", "5"},
		Descriptions: map[string]string{"1": "fine", "2": "probably fine", "3": "maybe", "4": "likely bad", "5": "bad"},
	},
}

const okResponse = `{
  "model": "jev-1.14.0",
  "answers": {
    "duplicate": {"type": "choice", "choice": "4812", "confidence": 0.93,
                  "probabilities": {"4812": 0.95, "none": 0.05}},
    "policy": {"type": "score", "score": 0.6, "confidence": 0.7,
               "legend": {"0": "fine", "1": "probably fine", "2": "maybe", "3": "likely bad", "4": "bad"},
               "probabilities": {"0": 0.6, "1": 0.25, "2": 0.1, "3": 0.05, "4": 0.0}}
  },
  "usage": {"input_tokens": 1000, "output_tokens": 12}
}`

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(Options{BaseURL: srv.URL, APIKey: "test-key", Version: "jev-1.14.0"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRequestShape(t *testing.T) {
	var got struct {
		method, path, auth, ctype string
		body                      map[string]any
	}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&got.body)
		io.WriteString(w, okResponse)
	})
	state := json.RawMessage(`{"table":"contacts","proposed":{"name":"Kim"}}`)
	if _, err := c.Decide(context.Background(), state, questions); err != nil {
		t.Fatal(err)
	}

	if got.method != "POST" || got.path != "/v1/systemone" || got.auth != "Bearer test-key" || got.ctype != "application/json" {
		t.Errorf("request %s %s auth=%q content-type=%q", got.method, got.path, got.auth, got.ctype)
	}
	if got.body["model"] != "jev-1.14.0" {
		t.Errorf("model = %v, want the pinned version", got.body["model"])
	}
	if !reflect.DeepEqual(got.body["state"], map[string]any{"table": "contacts", "proposed": map[string]any{"name": "Kim"}}) {
		t.Errorf("state = %v, want the state object as sent", got.body["state"])
	}
	qs := got.body["questions"].(map[string]any)
	dup := qs["duplicate"].(map[string]any)
	if dup["type"] != "choice" || dup["instructions"] != "Same entity?" ||
		!reflect.DeepEqual(dup["criteria"], map[string]any{"4812": "same as 4812", "none": "different"}) {
		t.Errorf("duplicate question = %v", dup)
	}
	pol := qs["policy"].(map[string]any)
	if pol["type"] != "score" ||
		!reflect.DeepEqual(pol["criteria"], []any{"fine", "probably fine", "maybe", "likely bad", "bad"}) {
		t.Errorf("policy question = %v, want levels in choice order", pol)
	}
}

func TestAnswersAreMapped(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, okResponse) })
	a, err := c.Decide(context.Background(), json.RawMessage(`{}`), questions)
	if err != nil {
		t.Fatal(err)
	}

	dup := a.ByQuestion["duplicate"]
	if dup.Choice != "4812" || dup.Confidence != 0.93 || dup.Distribution["4812"] != 0.95 || dup.Expected != nil {
		t.Errorf("duplicate = %+v", dup)
	}
	pol := a.ByQuestion["policy"]
	if pol.Choice != "1" || pol.Confidence != 0.7 {
		t.Errorf("policy = %+v, want the most likely level 1", pol)
	}
	if pol.Expected == nil || math.Abs(*pol.Expected-1.6) > 1e-9 {
		t.Errorf("policy expected = %v, want 1.6 (0.6 on Jev's 0-based scale)", pol.Expected)
	}
	if !reflect.DeepEqual(pol.Distribution, map[string]float64{"1": 0.6, "2": 0.25, "3": 0.1, "4": 0.05, "5": 0}) {
		t.Errorf("policy distribution = %v, want keys on our 1-5 scale", pol.Distribution)
	}
	if math.Abs(a.CostUSD-1000*42e-9) > 1e-12 {
		t.Errorf("cost = %v, want 1000 input tokens at $42 per billion", a.CostUSD)
	}
}

func TestAnswerFromAnotherModelIsRejected(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Replace(okResponse, "jev-1.14.0", "jev-1.15.0", 1))
	})
	_, err := c.Decide(context.Background(), json.RawMessage(`{}`), questions)
	if err == nil || !strings.Contains(err.Error(), "jev-1.15.0") {
		t.Fatalf("err = %v, want a model mismatch", err)
	}
}

func TestHTTPErrorsCarryStatusAndDetail(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		io.WriteString(w, `{"detail":[{"loc":["body","questions","policy","criteria"],"msg":"Field required","type":"missing"}]}`)
	})
	_, err := c.Decide(context.Background(), json.RawMessage(`{}`), questions)
	if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "Field required") {
		t.Fatalf("err = %v", err)
	}
}

func TestOverloadIsRetriedOnce(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(529)
			return
		}
		io.WriteString(w, okResponse)
	})
	if _, err := c.Decide(context.Background(), json.RawMessage(`{}`), questions); err != nil {
		t.Fatalf("second attempt should succeed: %v", err)
	}

	calls.Store(0)
	c = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(429)
	})
	if _, err := c.Decide(context.Background(), json.RawMessage(`{}`), questions); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("err = %v, want 429 after the retry", err)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("%d attempts, want 2", n)
	}
}

func TestRetryWaitStaysInsideDeadline(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Decide(ctx, json.RawMessage(`{}`), questions); err == nil {
		t.Fatal("want an error")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v; a Retry-After past the deadline must not be waited out", d)
	}
}

func TestDeadlineCutsSlowCalls(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Decide(ctx, json.RawMessage(`{}`), questions); err == nil {
		t.Fatal("want a deadline error")
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("took %v, want the 50ms deadline to cut the call", d)
	}
}

func TestNewRequiresPinnedVersionAndKey(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "k")
	for _, v := range []string{"jev-latest", "jev-preview", "jev-1.14", "1.14.0", ""} {
		if _, err := New(config.Model{Adapter: "jev", Version: v}); err == nil {
			t.Errorf("version %q accepted; want jev-X.Y.Z", v)
		}
	}
	d, err := New(config.Model{Adapter: "jev", Version: "jev-1.14.0"})
	if err != nil {
		t.Fatal(err)
	}
	if d.ModelID() != "jev" || d.ModelVersion() != "jev-1.14.0" {
		t.Errorf("model = %s %s", d.ModelID(), d.ModelVersion())
	}

	t.Setenv("TYPESAFE_API_KEY", "")
	if _, err := New(config.Model{Adapter: "jev", Version: "jev-1.14.0"}); err == nil || !strings.Contains(err.Error(), "TYPESAFE_API_KEY") {
		t.Errorf("err = %v, want a missing key error", err)
	}
}

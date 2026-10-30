package claude

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stetdb/stet/internal/decide"
)

var questions = []decide.Question{
	{
		ID: "duplicate", Type: "choice", Text: "Does the `proposed` record match a candidate?",
		Choices:      []string{"4812", "none"},
		Descriptions: map[string]string{"4812": "same as candidate 4812", "none": "a different entity"},
	},
	{
		ID: "policy_1", Type: "noul", Text: "Does the `proposed` change break this rule: no churn with open deals",
		Choices:      []string{"true", "false"},
		Descriptions: map[string]string{"true": "breaks the rule", "false": "follows the rule"},
	},
}

// reply builds a Messages API response whose text block is answers.
func reply(model, stop, answers string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": model,
		"content": []any{
			map[string]any{"type": "thinking", "thinking": "", "signature": "sig"},
			map[string]any{"type": "text", "text": answers},
		},
		"stop_reason": stop,
		"usage":       map[string]any{"input_tokens": 2000, "output_tokens": 300},
	})
	return string(b)
}

const okAnswers = `{"duplicate": {"choice": "4812", "confidence": 0.9}, "policy_1": {"probability": 0.2}}`

func newTestClient(t *testing.T, version string, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(Options{BaseURL: srv.URL, APIKey: "test-key", Version: version})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRequestShape(t *testing.T) {
	var path, key string
	var body map[string]any
	c := newTestClient(t, "claude-opus-5-5:low", func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("X-Api-Key")
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply("claude-opus-5-5", "end_turn", okAnswers)))
	})
	state := json.RawMessage(`{"table":"contacts","proposed":{"name":"Kim"}}`)
	if _, err := c.Decide(context.Background(), state, questions); err != nil {
		t.Fatal(err)
	}

	if path != "/v1/messages" || key != "test-key" {
		t.Errorf("request to %s with key %q", path, key)
	}
	if body["model"] != "claude-opus-5-5" {
		t.Errorf("model = %v, want the pinned model without the effort", body["model"])
	}
	for _, f := range []string{"temperature", "thinking", "fallbacks"} {
		if _, ok := body[f]; ok {
			t.Errorf("request sets %s; a fallback would answer with another model", f)
		}
	}
	oc, _ := body["output_config"].(map[string]any)
	if oc["effort"] != "low" {
		t.Errorf("effort = %v, want low", oc["effort"])
	}
	format, _ := oc["format"].(map[string]any)
	schema, _ := format["schema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if format["type"] != "json_schema" || len(props) != 2 || props["duplicate"] == nil || props["policy_1"] == nil {
		t.Errorf("format = %v, want a JSON schema with one property per question", format)
	}
	if req, _ := schema["required"].([]any); len(req) != 2 {
		t.Errorf("schema requires %v, want every question", schema["required"])
	}
	// Candidate ids vary per write; leaving them out of the schema keeps it
	// the same across writes with the same questions.
	if strings.Contains(mustJSON(schema), "4812") {
		t.Errorf("schema names a choice: %s", mustJSON(schema))
	}

	msgs, _ := body["messages"].([]any)
	user := mustJSON(msgs)
	for _, want := range []string{
		`{\"table\":\"contacts\",\"proposed\":{\"name\":\"Kim\"}}`, // the state as sent, byte for byte
		"Does the `proposed` record match a candidate?",
		"same as candidate 4812",
		"no churn with open deals",
		"breaks the rule",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user message lacks %q: %s", want, user)
		}
	}
}

func TestAnswers(t *testing.T) {
	c := newTestClient(t, "claude-haiku-5-5:low", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply("claude-haiku-5-5", "end_turn", okAnswers)))
	})
	got, err := c.Decide(context.Background(), json.RawMessage(`{}`), questions)
	if err != nil {
		t.Fatal(err)
	}
	if a := got.ByQuestion["duplicate"]; a.Choice != "4812" || a.Confidence != 0.9 {
		t.Errorf("duplicate = %+v", a)
	}
	if a := got.ByQuestion["policy_1"]; a.Choice != "false" || a.Probability == nil || *a.Probability != 0.2 || a.Confidence != 0 {
		t.Errorf("policy_1 = %+v, want choice false with probability 0.2 and no confidence", a)
	}
	// Haiku 5.5: $0.10 in, $0.50 out per million tokens.
	if want := 2000*0.10e-6 + 300*0.50e-6; math.Abs(got.CostUSD-want) > 1e-12 {
		t.Errorf("cost = %v, want %v", got.CostUSD, want)
	}
}

// An answer outside its question's choices or range is dropped, as a missing
// Jev answer is; decide.Verdict escalates it as low confidence.
func TestInvalidAnswersDropped(t *testing.T) {
	for name, answers := range map[string]string{
		"unknown choice":         `{"duplicate": {"choice": "candidate 4812", "confidence": 0.9}, "policy_1": {"probability": 0.2}}`,
		"confidence above one":   `{"duplicate": {"choice": "4812", "confidence": 90}, "policy_1": {"probability": 0.2}}`,
		"probability below zero": `{"duplicate": {"choice": "none", "confidence": 0.9}, "policy_1": {"probability": -0.1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, "claude-haiku-5-5:low", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(reply("claude-haiku-5-5", "end_turn", answers)))
			})
			got, err := c.Decide(context.Background(), json.RawMessage(`{}`), questions)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.ByQuestion) != 1 {
				t.Errorf("answers = %+v, want the invalid one dropped", got.ByQuestion)
			}
		})
	}
}

func TestFailures(t *testing.T) {
	for name, resp := range map[string]string{
		"another model answered": reply("claude-opus-4-8", "end_turn", okAnswers),
		"refusal":                reply("claude-haiku-5-5", "refusal", ""),
		"cut off":                reply("claude-haiku-5-5", "max_tokens", `{"duplicate": {"cho`),
		"not JSON":               reply("claude-haiku-5-5", "end_turn", "duplicate: 4812"),
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, "claude-haiku-5-5:low", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(resp))
			})
			if _, err := c.Decide(context.Background(), json.RawMessage(`{}`), questions); err == nil {
				t.Error("want an error")
			}
		})
	}
}

// Retries belong to the caller, so the latency of a stored answer is one call.
func TestNoRetries(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, "claude-haiku-5-5:low", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	})
	if _, err := c.Decide(context.Background(), json.RawMessage(`{}`), questions); err == nil {
		t.Error("want an error")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d calls, want 1", n)
	}
}

func TestUnsupportedQuestionType(t *testing.T) {
	c := newTestClient(t, "claude-haiku-5-5:low", func(w http.ResponseWriter, r *http.Request) {
		t.Error("the model was called")
	})
	q := []decide.Question{{ID: "policy", Type: "score", Choices: []string{"1", "2"}}}
	if _, err := c.Decide(context.Background(), json.RawMessage(`{}`), q); err == nil {
		t.Error("want an error")
	}
}

func TestVersion(t *testing.T) {
	for _, v := range []string{
		"claude-opus-5-5",        // no effort: defaults differ by model and can change
		"claude-opus-5-5:turbo",  // not an effort level
		"claude-opus-latest:low", // moving alias
		"claude-opus-9:low",      // no known price
		"",
	} {
		if _, err := NewClient(Options{APIKey: "k", Version: v}); err == nil {
			t.Errorf("version %q accepted", v)
		}
	}
	c, err := NewClient(Options{APIKey: "k", Version: "claude-sonnet-5-5:medium"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelID() != "claude" || c.ModelVersion() != "claude-sonnet-5-5:medium" {
		t.Errorf("model %s %s", c.ModelID(), c.ModelVersion())
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Question text reaches the model as written, as it reaches Jev.
func TestQuestionTextUnescaped(t *testing.T) {
	q := []decide.Question{{ID: "policy_1", Type: "noul", Text: "Does it break: amount < 0 & stage <> 'won'",
		Choices: []string{"true", "false"}}}
	user, _, err := prompt(json.RawMessage(`{}`), q)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(user, "amount < 0 & stage <> 'won'") {
		t.Errorf("question text escaped: %s", user)
	}
}

package decide

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/stetdb/stet/internal/config"
)

var th = config.Thresholds{Duplicate: 0.85, Conflict: 0.80, Policy: 4, EscalateBelow: 0.60}

func TestVerdict(t *testing.T) {
	dupQ := Question{ID: "duplicate", Choices: []string{"4812", "none"}}
	conQ := Question{ID: "conflict", Choices: []string{"yes", "no"}}
	polQ := Question{ID: "policy", Choices: []string{"1", "2", "3", "4", "5"}}
	ans := func(kv ...any) map[string]Answer {
		m := map[string]Answer{}
		for i := 0; i < len(kv); i += 3 {
			m[kv[i].(string)] = Answer{Choice: kv[i+1].(string), Confidence: kv[i+2].(float64)}
		}
		return m
	}

	cases := []struct {
		name    string
		in      Input
		verdict string
		reasons []string
	}{
		{"unsupported shape", Input{Unsupported: "multi_row"}, Unsupported, []string{"multi_row"}},
		{"in transaction", Input{Unsupported: "in_transaction"}, Unsupported, []string{"in_transaction"}},
		{"state error, pass", Input{StateErr: errors.New("x"), OnFailure: "pass"}, Commit, []string{"state_error"}},
		{"state error, hold", Input{StateErr: errors.New("x"), OnFailure: "hold"}, Hold, []string{"state_error"}},
		{"model error, pass", Input{ModelErr: errors.New("timeout"), OnFailure: "pass"}, Commit, []string{"model_error"}},
		{"model error, hold", Input{ModelErr: errors.New("timeout"), OnFailure: "hold"}, Hold, []string{"model_error"}},
		{"record only", Input{ModelErr: ErrNoModel, OnFailure: "pass"}, Commit, []string{"no_model"}},
		{
			"low confidence escalates",
			Input{Questions: []Question{conQ, polQ}, Answers: ans("conflict", "no", 0.55, "policy", "1", 0.9)},
			Escalate, []string{"low_confidence:conflict"},
		},
		{
			"every low answer is named",
			Input{Questions: []Question{conQ, polQ}, Answers: ans("conflict", "no", 0.5, "policy", "1", 0.4)},
			Escalate, []string{"low_confidence:conflict", "low_confidence:policy"},
		},
		{
			"confident duplicate holds",
			Input{Questions: []Question{dupQ, polQ}, Answers: ans("duplicate", "4812", 0.93, "policy", "1", 0.9)},
			Hold, []string{"duplicate:4812"},
		},
		{
			"duplicate below threshold commits",
			Input{Questions: []Question{dupQ, polQ}, Answers: ans("duplicate", "4812", 0.70, "policy", "1", 0.9)},
			Commit, nil,
		},
		{
			"no duplicate commits",
			Input{Questions: []Question{dupQ, polQ}, Answers: ans("duplicate", "none", 0.95, "policy", "1", 0.9)},
			Commit, nil,
		},
		{
			"conflict holds",
			Input{Questions: []Question{conQ, polQ}, Answers: ans("conflict", "yes", 0.85, "policy", "2", 0.9)},
			Hold, []string{"conflict"},
		},
		{
			"policy score at threshold holds",
			Input{Questions: []Question{conQ, polQ}, Answers: ans("conflict", "no", 0.9, "policy", "4", 0.9)},
			Hold, []string{"policy"},
		},
		{
			"policy score below threshold commits",
			Input{Questions: []Question{conQ, polQ}, Answers: ans("conflict", "no", 0.9, "policy", "3", 0.9)},
			Commit, nil,
		},
		{
			"first matching rule wins",
			Input{Questions: []Question{dupQ, polQ}, Answers: ans("duplicate", "4812", 0.9, "policy", "5", 0.9)},
			Hold, []string{"duplicate:4812"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.Thresholds = th
			v, reasons := Verdict(in)
			if v != tc.verdict || !reflect.DeepEqual(reasons, tc.reasons) {
				t.Errorf("got %s %v, want %s %v", v, reasons, tc.verdict, tc.reasons)
			}
		})
	}
}

func TestQuestions(t *testing.T) {
	all := []string{"duplicate", "conflict", "policy"}
	ids := func(qs []Question) []string {
		var out []string
		for _, q := range qs {
			out = append(out, q.ID)
		}
		return out
	}
	cases := []struct {
		name       string
		op         string
		candidates []string
		enabled    []string
		hasPolicy  bool
		want       []string
	}{
		{"insert with candidates", "insert", []string{"7", "9"}, all, true, []string{"duplicate", "policy"}},
		{"insert without candidates", "insert", nil, all, true, []string{"policy"}},
		{"update", "update", []string{"7"}, all, true, []string{"conflict", "policy"}},
		{"delete", "delete", nil, all, true, []string{"conflict", "policy"}},
		{"no policy text", "update", nil, all, false, []string{"conflict"}},
		{"only enabled questions", "update", nil, []string{"policy"}, true, []string{"policy"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qs := Questions(tc.op, tc.candidates, tc.enabled, tc.hasPolicy)
			if got := ids(qs); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("questions = %v, want %v", got, tc.want)
			}
		})
	}

	dup := Questions("insert", []string{"7", "9"}, all, false)[0]
	if !reflect.DeepEqual(dup.Choices, []string{"7", "9", "none"}) {
		t.Errorf("duplicate choices = %v, want candidate ids then none", dup.Choices)
	}
	if dup.Text == "" {
		t.Error("question text is empty")
	}
}

func TestNewRecordOnly(t *testing.T) {
	d, err := New(config.Model{Adapter: config.RecordOnly})
	if err != nil {
		t.Fatal(err)
	}
	if d.ModelID() != "none" || d.ModelVersion() == "" {
		t.Errorf("model = %q %q", d.ModelID(), d.ModelVersion())
	}
	if _, err := d.Decide(context.Background(), nil, nil); !errors.Is(err, ErrNoModel) {
		t.Errorf("Decide err = %v, want ErrNoModel", err)
	}
}

func TestNewUnknownAdapter(t *testing.T) {
	_, err := New(config.Model{Adapter: "jevv", Version: "1.0"})
	if err == nil || !strings.Contains(err.Error(), "jevv") {
		t.Fatalf("err = %v, want unknown adapter error", err)
	}
}

func TestQuestionsDescribeEveryChoice(t *testing.T) {
	all := []string{"duplicate", "conflict", "policy"}
	for _, q := range append(Questions("insert", []string{"7"}, all, true), Questions("update", nil, all, true)...) {
		if q.Type != "choice" && q.Type != "score" {
			t.Errorf("%s: type %q, want choice or score", q.ID, q.Type)
		}
		for _, c := range q.Choices {
			if q.Descriptions[c] == "" {
				t.Errorf("%s: choice %q has no description", q.ID, c)
			}
		}
	}
	pol := Questions("update", nil, []string{"policy"}, true)[0]
	if pol.Type != "score" || !reflect.DeepEqual(pol.Choices, []string{"1", "2", "3", "4", "5"}) {
		t.Errorf("policy question = %+v, want a 1-5 score", pol)
	}
}

func TestPolicyUsesExpectedScoreWhenGiven(t *testing.T) {
	polQ := Question{ID: "policy", Type: "score", Choices: []string{"1", "2", "3", "4", "5"}}
	exp := func(v float64) *float64 { return &v }
	cases := []struct {
		name    string
		a       Answer
		verdict string
	}{
		{"expected below threshold", Answer{Choice: "4", Confidence: 0.9, Expected: exp(3.6)}, Commit},
		{"expected at threshold", Answer{Choice: "4", Confidence: 0.9, Expected: exp(4.0)}, Hold},
		{"expected over, top level lower", Answer{Choice: "3", Confidence: 0.9, Expected: exp(4.2)}, Hold},
	}
	for _, tc := range cases {
		v, _ := Verdict(Input{Questions: []Question{polQ}, Answers: map[string]Answer{"policy": tc.a}, Thresholds: th})
		if v != tc.verdict {
			t.Errorf("%s: verdict %s, want %s", tc.name, v, tc.verdict)
		}
	}
}

func TestRegisteredAdapter(t *testing.T) {
	Register("test-adapter", func(m config.Model) (Decider, error) { return recordOnly{}, nil })
	if _, err := New(config.Model{Adapter: "test-adapter"}); err != nil {
		t.Fatalf("registered adapter not found: %v", err)
	}
}

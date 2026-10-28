package decide

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/stetdb/stet/internal/config"
)

var th = config.Thresholds{Duplicate: 0.85, Conflict: 0.80, Policy: 0.80, PolicyEscalate: 0.50, EscalateBelow: 0.60}

func choice(c string, conf float64) Answer { return Answer{Choice: c, Confidence: conf} }

func yesno(p float64) Answer {
	c := "false"
	if p >= 0.5 {
		c = "true"
	}
	return Answer{Choice: c, Probability: &p}
}

func TestVerdict(t *testing.T) {
	dupQ := Question{ID: "duplicate", Type: "choice", Choices: []string{"4812", "none"}}
	conQ := Question{ID: "conflict", Type: "choice", Choices: []string{"yes", "no"}}
	rule1 := Question{ID: "policy_1", Type: "noul", Choices: []string{"true", "false"}}
	rule2 := Question{ID: "policy_2", Type: "noul", Choices: []string{"true", "false"}}
	type A = map[string]Answer

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
			"low choice confidence escalates",
			Input{Questions: []Question{conQ, rule1}, Answers: A{"conflict": choice("no", 0.55), "policy_1": yesno(0.05)}},
			Escalate, []string{"low_confidence:conflict"},
		},
		{
			"uncertain rule escalates",
			Input{Questions: []Question{conQ, rule1}, Answers: A{"conflict": choice("no", 0.9), "policy_1": yesno(0.6)}},
			Escalate, []string{"low_confidence:policy_1"},
		},
		{
			"every uncertain answer is named",
			Input{Questions: []Question{conQ, rule1}, Answers: A{"conflict": choice("no", 0.5), "policy_1": yesno(0.55)}},
			Escalate, []string{"low_confidence:conflict", "low_confidence:policy_1"},
		},
		{
			"missing answer escalates",
			Input{Questions: []Question{conQ, rule1}, Answers: A{"conflict": choice("no", 0.9)}},
			Escalate, []string{"low_confidence:policy_1"},
		},
		{
			"confident duplicate holds",
			Input{Questions: []Question{dupQ, rule1}, Answers: A{"duplicate": choice("4812", 0.93), "policy_1": yesno(0.05)}},
			Hold, []string{"duplicate:4812"},
		},
		{
			"duplicate below threshold commits",
			Input{Questions: []Question{dupQ, rule1}, Answers: A{"duplicate": choice("4812", 0.70), "policy_1": yesno(0.05)}},
			Commit, nil,
		},
		{
			"no duplicate commits",
			Input{Questions: []Question{dupQ, rule1}, Answers: A{"duplicate": choice("none", 0.95), "policy_1": yesno(0.05)}},
			Commit, nil,
		},
		{
			"conflict holds",
			Input{Questions: []Question{conQ, rule1}, Answers: A{"conflict": choice("yes", 0.85), "policy_1": yesno(0.05)}},
			Hold, []string{"conflict"},
		},
		{
			"broken rule holds and is named",
			Input{Questions: []Question{conQ, rule1, rule2}, Answers: A{"conflict": choice("no", 0.9), "policy_1": yesno(0.04), "policy_2": yesno(0.87)}},
			Hold, []string{"policy:2"},
		},
		{
			"every broken rule is named",
			Input{Questions: []Question{rule1, rule2}, Answers: A{"policy_1": yesno(0.9), "policy_2": yesno(0.8)}},
			Hold, []string{"policy:1", "policy:2"},
		},
		{
			"unlikely break commits",
			Input{Questions: []Question{rule1, rule2}, Answers: A{"policy_1": yesno(0.30), "policy_2": yesno(0.05)}},
			Commit, nil,
		},
		{
			"first matching rule wins",
			Input{Questions: []Question{dupQ, rule1}, Answers: A{"duplicate": choice("4812", 0.9), "policy_1": yesno(0.95)}},
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
	rules := []string{"Do not churn with an open deal.", "Do not restore a stale title."}
	ids := func(qs []Question) []string {
		var out []string
		for _, q := range qs {
			out = append(out, q.ID)
		}
		return out
	}
	cases := []struct {
		name string
		ask  Ask
		want []string
	}{
		{"insert with candidates", Ask{Op: "insert", CandidateIDs: []string{"7", "9"}, Enabled: all, Rules: rules}, []string{"duplicate", "policy_1", "policy_2"}},
		{"insert without candidates", Ask{Op: "insert", Enabled: all, Rules: rules}, []string{"policy_1", "policy_2"}},
		{"update undoing a recent change", Ask{Op: "update", Enabled: all, Rules: rules[:1], Recent: true}, []string{"conflict", "policy_1"}},
		{"update with nothing recent to undo", Ask{Op: "update", Enabled: all, Rules: rules[:1]}, []string{"policy_1"}},
		{"delete after a recent change", Ask{Op: "delete", Enabled: all, Recent: true}, []string{"conflict"}},
		{"delete with nothing recent", Ask{Op: "delete", Enabled: all}, nil},
		{"insert ignores recent", Ask{Op: "insert", Enabled: all, Recent: true}, nil},
		{"no policy", Ask{Op: "update", Enabled: all, Recent: true}, []string{"conflict"}},
		{"only enabled questions", Ask{Op: "update", Enabled: []string{"policy"}, Rules: rules[:1], Recent: true}, []string{"policy_1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ids(Questions(tc.ask)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("questions = %v, want %v", got, tc.want)
			}
		})
	}

	dup := Questions(Ask{Op: "insert", CandidateIDs: []string{"7", "9"}, Enabled: all})[0]
	if !reflect.DeepEqual(dup.Choices, []string{"7", "9", "none"}) {
		t.Errorf("duplicate choices = %v, want candidate ids then none", dup.Choices)
	}
	r2 := Questions(Ask{Op: "update", Enabled: []string{"policy"}, Rules: rules})[1]
	if r2.Type != "noul" || !strings.Contains(r2.Text, rules[1]) {
		t.Errorf("rule question = %+v, want a yes/no question quoting its rule", r2)
	}
}

// The conflict question is about undoing recent changes only; asking about
// `current` made Jev call every ordinary update a contradiction.
func TestConflictQuestionIsAboutRecentChanges(t *testing.T) {
	q := Questions(Ask{Op: "update", Enabled: []string{"conflict"}, Recent: true})[0]
	if strings.Contains(q.Text, "`current`") || !strings.Contains(q.Text, "`recent_changes`") {
		t.Errorf("conflict text = %q, want it to point at recent_changes only", q.Text)
	}
	if !strings.Contains(q.Descriptions["no"], "written differently") {
		t.Errorf("no = %q, want the same value written differently to count as the same", q.Descriptions["no"])
	}
}

func TestQuestionsDescribeEveryChoice(t *testing.T) {
	all := []string{"duplicate", "conflict", "policy"}
	rules := []string{"Do not churn with an open deal."}
	qs := append(Questions(Ask{Op: "insert", CandidateIDs: []string{"7"}, Enabled: all, Rules: rules}),
		Questions(Ask{Op: "update", Enabled: all, Rules: rules, Recent: true})...)
	for _, q := range qs {
		if q.Text == "" {
			t.Errorf("%s: no text", q.ID)
		}
		for _, c := range q.Choices {
			if q.Descriptions[c] == "" {
				t.Errorf("%s: choice %q has no description", q.ID, c)
			}
		}
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

func TestRegisteredAdapter(t *testing.T) {
	Register("test-adapter", func(m config.Model) (Decider, error) { return recordOnly{}, nil })
	if _, err := New(config.Model{Adapter: "test-adapter"}); err != nil {
		t.Fatalf("registered adapter not found: %v", err)
	}
}

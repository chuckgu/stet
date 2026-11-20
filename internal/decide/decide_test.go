package decide

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
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
		{"in a dirty transaction", Input{InTransaction: true}, Unsupported, []string{"in_transaction"}},
		{"no login role", Input{NoLoginRole: true}, Unsupported, []string{"no_login_role"}},
		{"shape before transaction and role", Input{Unsupported: "multi_row", InTransaction: true, NoLoginRole: true},
			Unsupported, []string{"multi_row"}},
		{"transaction before role", Input{InTransaction: true, NoLoginRole: true}, Unsupported, []string{"in_transaction"}},
		{"no login role before unknown usage", Input{NoLoginRole: true, Cumulative: &Cumulative{Error: "no login role"}},
			Unsupported, []string{"no_login_role"}},
		{
			"transaction before the model's answers",
			Input{InTransaction: true, Questions: []Question{dupQ}, Answers: A{"duplicate": choice("4812", 0.93)}},
			Unsupported, []string{"in_transaction"},
		},
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

func TestVerdictImpact(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	f := func(v float64) *float64 { return &v }
	lim := &config.Impact{MaxRows: 100, MaxShare: 0.5}
	cases := []struct {
		name    string
		in      Input
		verdict string
		reasons []string
	}{
		{"counted, within limits", Input{Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(99), TableRows: n(1000), Share: f(0.099)}}, Commit, nil},
		{"zero rows", Input{Op: "delete", ImpactLimit: lim, Impact: &Impact{Method: "count", Rows: n(0)}}, Commit, nil},
		{"capped", Input{Op: "delete", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(101), Capped: true, Cap: 101}}, Hold, []string{"impact:rows"}},
		{"estimate over the row limit", Input{Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "estimate", Rows: n(150)}}, Hold, []string{"impact:rows"}},
		{"share over the limit", Input{Op: "delete", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(40), TableRows: n(50), Share: f(0.8)}}, Hold, []string{"impact:share"}},
		{"rows and share both over: rows wins", Input{Op: "delete", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(101), Capped: true, Share: f(0.9)}}, Hold, []string{"impact:rows"}},
		{"share does not apply to inserts", Input{Op: "insert", ImpactLimit: lim,
			Impact: &Impact{Method: "syntax", Rows: n(40), Share: f(0.8)}}, Commit, nil},
		{"table never analyzed: no share", Input{Op: "delete", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(40)}}, Commit, nil},
		{"no share limit configured", Input{Op: "delete", ImpactLimit: &config.Impact{MaxRows: 100},
			Impact: &Impact{Method: "count", Rows: n(40), Share: f(0.8)}}, Commit, nil},
		{"estimate only, within limits", Input{Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "estimate", Rows: n(50)}}, Escalate, []string{"impact:uncertain"}},
		{"measuring failed", Input{Op: "update", ShapeReason: "not_single_row", ImpactLimit: lim,
			Impact: &Impact{Error: "set role: permission denied"}}, Unsupported, []string{"not_single_row"}},
		{"unsupported wins over impact", Input{Unsupported: "in_transaction", Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(500), Capped: true}}, Unsupported, []string{"in_transaction"}},
		{"transaction wins over impact", Input{InTransaction: true, Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(500), Capped: true}}, Unsupported, []string{"in_transaction"}},
		{"login role wins over impact", Input{NoLoginRole: true, Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(500), Capped: true}}, Unsupported, []string{"no_login_role"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, reasons := Verdict(tc.in)
			if v != tc.verdict || !reflect.DeepEqual(reasons, tc.reasons) {
				t.Errorf("got %s %v, want %s %v", v, reasons, tc.verdict, tc.reasons)
			}
		})
	}
}

func TestVerdictCumulative(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	lim := &config.Impact{MaxRows: 100, MaxShare: 0.5}
	cum := func(used, planned int64) *Cumulative {
		return &Cumulative{Used: n(used), Planned: n(planned), Limit: 500, WindowS: 600}
	}
	dupQ := Question{ID: "duplicate", Type: "choice", Choices: []string{"4812", "none"}}
	okAnswers := map[string]Answer{"duplicate": choice("none", 0.95)}
	holdAnswers := map[string]Answer{"duplicate": choice("4812", 0.95)}
	cases := []struct {
		name    string
		in      Input
		verdict string
		reasons []string
	}{
		{"single row, within", Input{Op: "insert", Cumulative: cum(480, 1)}, Commit, nil},
		{"single row, over", Input{Op: "insert", Thresholds: th, Questions: []Question{dupQ}, Answers: okAnswers,
			Cumulative: cum(500, 1)}, Hold, []string{"impact:cumulative"}},
		{"over wins over a model hold", Input{Op: "insert", Thresholds: th, Questions: []Question{dupQ}, Answers: holdAnswers,
			Cumulative: cum(500, 1)}, Hold, []string{"impact:cumulative"}},
		{"over wins over state error", Input{Op: "insert", StateErr: errors.New("x"), OnFailure: "pass",
			Cumulative: cum(500, 1)}, Hold, []string{"impact:cumulative"}},
		{"exactly at the limit", Input{Op: "insert", Cumulative: cum(499, 1)}, Commit, nil},
		{"measured, within both", Input{Op: "update", ImpactLimit: lim, Impact: &Impact{Method: "count", Rows: n(20)},
			Cumulative: cum(480, 20)}, Commit, nil},
		{"measured, over cumulative", Input{Op: "update", ImpactLimit: lim, Impact: &Impact{Method: "count", Rows: n(21)},
			Cumulative: cum(480, 21)}, Hold, []string{"impact:cumulative"}},
		{"rows limit wins", Input{Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(101), Capped: true}, Cumulative: cum(0, 101)}, Hold, []string{"impact:rows"}},
		{"share limit wins", Input{Op: "delete", ImpactLimit: lim,
			Impact:     &Impact{Method: "count", Rows: n(40), Share: func() *float64 { v := 0.8; return &v }()},
			Cumulative: cum(480, 40)}, Hold, []string{"impact:share"}},
		{"cumulative before uncertain", Input{Op: "update", ImpactLimit: lim, Impact: &Impact{Method: "estimate", Rows: n(50)},
			Cumulative: cum(480, 50)}, Hold, []string{"impact:cumulative"}},
		{"estimate within cumulative", Input{Op: "update", ImpactLimit: lim, Impact: &Impact{Method: "estimate", Rows: n(5)},
			Cumulative: cum(0, 5)}, Escalate, []string{"impact:uncertain"}},
		{"unknown usage holds", Input{Op: "insert", Cumulative: &Cumulative{Error: "x"}}, Hold, []string{"impact:cumulative_unknown"}},
		{"unknown usage wins over a model hold", Input{Op: "insert", Thresholds: th, Questions: []Question{dupQ}, Answers: holdAnswers,
			Cumulative: &Cumulative{Error: "x"}}, Hold, []string{"impact:cumulative_unknown"}},
		{"unknown usage wins over state error", Input{Op: "insert", StateErr: errors.New("x"), OnFailure: "pass",
			Cumulative: &Cumulative{Error: "x"}}, Hold, []string{"impact:cumulative_unknown"}},
		{"measured, unknown usage", Input{Op: "update", ImpactLimit: lim, Impact: &Impact{Method: "count", Rows: n(20)},
			Cumulative: &Cumulative{Error: "x"}}, Hold, []string{"impact:cumulative_unknown"}},
		{"rows limit wins over unknown usage", Input{Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(101), Capped: true}, Cumulative: &Cumulative{Error: "x"}}, Hold, []string{"impact:rows"}},
		{"unknown usage before uncertain", Input{Op: "update", ImpactLimit: lim, Impact: &Impact{Method: "estimate", Rows: n(5)},
			Cumulative: &Cumulative{Error: "x"}}, Hold, []string{"impact:cumulative_unknown"}},
		{"failed measure wins over unknown usage", Input{Op: "update", ShapeReason: "not_single_row", ImpactLimit: lim,
			Impact: &Impact{Error: "x"}, Cumulative: &Cumulative{Error: "x"}}, Unsupported, []string{"not_single_row"}},
		{"unsupported stays", Input{Unsupported: "multi_row", Cumulative: cum(500, 1)}, Unsupported, []string{"multi_row"}},
		{"failed measure stays", Input{Op: "update", ShapeReason: "not_single_row", ImpactLimit: lim,
			Impact: &Impact{Error: "x"}, Cumulative: cum(500, 1)}, Unsupported, []string{"not_single_row"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, reasons := Verdict(tc.in)
			if v != tc.verdict || !reflect.DeepEqual(reasons, tc.reasons) {
				t.Errorf("got %s %v, want %s %v", v, reasons, tc.verdict, tc.reasons)
			}
		})
	}
}

func TestCumulativeUnknown(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	cases := []struct {
		name string
		c    *Cumulative
		want bool
	}{
		{"no limit", nil, false},
		{"usage read", &Cumulative{Used: n(3), Planned: n(1), Limit: 5, WindowS: 60}, false},
		{"usage not read", &Cumulative{Error: "usage not loaded: timeout"}, true},
	}
	for _, tc := range cases {
		if got := tc.c.Unknown(); got != tc.want {
			t.Errorf("%s: Unknown() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Deterministic is the front of Verdict: whatever it decides, Verdict decides
// the same, and it never looks at what the model said.
func TestDeterministic(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	lim := &config.Impact{MaxRows: 100, MaxShare: 0.5}
	cum := func(used, planned int64) *Cumulative {
		return &Cumulative{Used: n(used), Planned: n(planned), Limit: 500, WindowS: 600}
	}
	dupQ := Question{ID: "duplicate", Type: "choice", Choices: []string{"4812", "none"}}
	rule1 := Question{ID: "policy_1", Type: "noul", Choices: []string{"true", "false"}}
	qs := []Question{dupQ, rule1}
	hold := map[string]Answer{"duplicate": choice("4812", 0.95), "policy_1": yesno(0.9)}

	decided := []struct {
		name    string
		in      Input
		verdict string
		reasons []string
	}{
		{"shape", Input{Unsupported: "multi_row", Questions: qs}, Unsupported, []string{"multi_row"}},
		{"transaction", Input{InTransaction: true, Questions: qs}, Unsupported, []string{"in_transaction"}},
		{"login role", Input{NoLoginRole: true, Questions: qs}, Unsupported, []string{"no_login_role"}},
		{"measured, within", Input{Op: "update", ImpactLimit: lim, Impact: &Impact{Method: "count", Rows: n(3)}}, Commit, nil},
		{"measured, over", Input{Op: "delete", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(101), Capped: true}}, Hold, []string{"impact:rows"}},
		{"measured, estimate", Input{Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "estimate", Rows: n(5)}}, Escalate, []string{"impact:uncertain"}},
		{"measuring failed", Input{Op: "update", ShapeReason: "not_single_row", ImpactLimit: lim,
			Impact: &Impact{Error: "x"}}, Unsupported, []string{"not_single_row"}},
		{"cumulative over", Input{Op: "insert", Questions: qs, Cumulative: cum(500, 1)}, Hold, []string{"impact:cumulative"}},
		{"cumulative unknown", Input{Op: "insert", Questions: qs, Cumulative: &Cumulative{Error: "x"}},
			Hold, []string{"impact:cumulative_unknown"}},
		{"state error, hold", Input{Op: "update", Questions: qs, StateErr: errors.New("x"), OnFailure: "hold"},
			Hold, []string{"state_error"}},
		{"state error, pass", Input{Op: "update", Questions: qs, StateErr: errors.New("x"), OnFailure: "pass"},
			Commit, []string{"state_error"}},
		{"no questions", Input{Op: "insert", Cumulative: cum(10, 1)}, Commit, nil},
		{"no questions, no limit", Input{Op: "delete"}, Commit, nil},
	}
	for _, tc := range decided {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.Thresholds = th
			v, reasons, ok := Deterministic(in)
			if !ok || v != tc.verdict || !reflect.DeepEqual(reasons, tc.reasons) {
				t.Errorf("Deterministic = %s %v %v, want %s %v true", v, reasons, ok, tc.verdict, tc.reasons)
			}
			// Had the model been asked and answered hold, Verdict would still
			// decide the same.
			if len(in.Questions) > 0 {
				in.Answers = hold
			}
			if v2, r2 := Verdict(in); v2 != v || !reflect.DeepEqual(r2, reasons) {
				t.Errorf("Verdict = %s %v, want what Deterministic decided: %s %v", v2, r2, v, reasons)
			}
		})
	}

	open := []struct {
		name string
		in   Input
	}{
		{"questions to ask", Input{Op: "insert", Questions: qs}},
		{"questions, within the cumulative limit", Input{Op: "insert", Questions: qs, Cumulative: cum(10, 1)}},
		// What the model said is not Deterministic's to read.
		{"answers already in", Input{Op: "insert", Questions: qs, Answers: hold}},
		{"model failed", Input{Op: "insert", Questions: qs, ModelErr: errors.New("timeout"), OnFailure: "hold"}},
	}
	for _, tc := range open {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.Thresholds = th
			if v, reasons, ok := Deterministic(in); ok || v != "" || reasons != nil {
				t.Errorf("Deterministic = %q %v %v, want undecided", v, reasons, ok)
			}
		})
	}
}

func TestCumulativeJSON(t *testing.T) {
	zero, one := int64(0), int64(1)
	b, _ := json.Marshal(Cumulative{Used: &zero, Planned: &one, Limit: 500, WindowS: 600})
	if !strings.Contains(string(b), `"used":0`) {
		t.Errorf("used 0 missing: %s", b)
	}
	b, _ = json.Marshal(Cumulative{Error: "x"})
	if string(b) != `{"error":"x"}` {
		t.Errorf("got %s", b)
	}
}

func TestStaleReadRule(t *testing.T) {
	in := Input{Op: "update", ReadCheck: &ReadCheck{Result: ReadStale}}
	v, reasons, ok := Deterministic(in)
	if v != Reject || !slices.Equal(reasons, []string{"stale_read"}) || !ok {
		t.Errorf("Deterministic = %q, %v, %v; want reject, [stale_read], true", v, reasons, ok)
	}
	if v, reasons := Verdict(in); v != Reject || !slices.Equal(reasons, []string{"stale_read"}) {
		t.Errorf("Verdict = %q, %v; want reject, [stale_read]", v, reasons)
	}
	for _, res := range []string{ReadFresh, ReadNone, ReadPending, ReadOwnWrite, ReadNotChecked} {
		in := Input{Op: "update", ReadCheck: &ReadCheck{Result: res}}
		if v, _, ok := Deterministic(in); v == Reject || !ok {
			t.Errorf("%s: Deterministic = %q, %v; it is a write with no questions and must commit", res, v, ok)
		}
		if _, reasons, _ := Deterministic(in); slices.Contains(reasons, "stale_read") {
			t.Errorf("%s decided stale_read", res)
		}
	}
	if v, _ := Verdict(Input{Op: "update"}); v != Commit {
		t.Errorf("nil ReadCheck: Verdict = %q, want commit", v)
	}
}

func TestStaleReadOrder(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	stale := &ReadCheck{Result: ReadStale}
	qs := []Question{{ID: "duplicate", Type: "choice", Choices: []string{"4812", "none"}}}
	cases := []struct {
		name    string
		in      Input
		verdict string
		reasons []string
	}{
		{"shape", Input{Unsupported: "multi_row"}, Unsupported, []string{"multi_row"}},
		{"transaction", Input{InTransaction: true}, Unsupported, []string{"in_transaction"}},
		{"login role", Input{NoLoginRole: true}, Unsupported, []string{"no_login_role"}},
		{"cumulative over", Input{Cumulative: &Cumulative{Used: n(500), Planned: n(1), Limit: 500, WindowS: 600}}, Reject, []string{"stale_read"}},
		{"cumulative unknown", Input{Cumulative: &Cumulative{Error: "x"}}, Reject, []string{"stale_read"}},
		{"state error", Input{StateErr: errors.New("x"), OnFailure: "pass"}, Reject, []string{"stale_read"}},
		{"answers", Input{Questions: qs, Answers: map[string]Answer{"duplicate": choice("4812", 0.95)}}, Reject, []string{"stale_read"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.ReadCheck = stale
			v, reasons, ok := Deterministic(in)
			if !ok || v != tc.verdict || !slices.Equal(reasons, tc.reasons) {
				t.Errorf("Deterministic = %q, %v, %v; want %q, %v", v, reasons, ok, tc.verdict, tc.reasons)
			}
		})
	}
}

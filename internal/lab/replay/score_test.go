package replay

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/lab/labels"
)

func TestWilson(t *testing.T) {
	lo, hi := wilson(50, 100)
	if math.Abs(lo-0.4038) > 1e-3 || math.Abs(hi-0.5962) > 1e-3 {
		t.Errorf("wilson(50, 100) = %.4f, %.4f", lo, hi)
	}
	if lo, hi := wilson(0, 0); lo != 0 || hi != 0 {
		t.Errorf("wilson(0, 0) = %v, %v", lo, hi)
	}
	if lo, _ := wilson(0, 175); math.Signbit(lo) {
		t.Errorf("wilson(0, 175) has lower bound %v; it prints as -0", lo)
	}
}

func p(v float64) *float64 { return &v }

func TestModelVerdictsUseStoredAnswersAndThresholds(t *testing.T) {
	cfg := &config.Config{Model: config.Model{OnFailure: "pass"},
		Tables: map[string]config.Table{"contacts": {Thresholds: config.DefaultThresholds()}}}
	dup := decide.Question{ID: "duplicate", Type: "choice", Choices: []string{"7", "none"}}
	rule := decide.Question{ID: "policy_1", Type: "noul", Choices: []string{"true", "false"}}
	recs := []Record{
		{ID: "held", Table: "contacts", Questions: []decide.Question{dup}},
		{ID: "unsure", Table: "contacts", Questions: []decide.Question{rule}},
		{ID: "unasked", Table: "contacts"},
		{ID: "missing", Table: "contacts", Questions: []decide.Question{rule}},
		{ID: "unsupported", Table: "contacts", Verdict: decide.Unsupported},
	}
	stored := map[string]Judgment{
		"held":   {Answers: map[string]decide.Answer{"duplicate": {Choice: "7", Confidence: 0.9}}},
		"unsure": {Answers: map[string]decide.Answer{"policy_1": {Choice: "true", Probability: p(0.6)}}},
	}
	got := ModelVerdicts(cfg, recs, stored)
	want := map[string]Judged{
		"held":        {decide.Hold, []string{"duplicate:7"}},
		"unsure":      {decide.Escalate, []string{"low_confidence:policy_1"}},
		"unasked":     {decide.Commit, nil},
		"unsupported": {decide.Unsupported, nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v; a write still to be asked has no verdict", got, want)
	}
}

func TestSummarizeCountsCatchesAndFalseHolds(t *testing.T) {
	truths := map[string]Truth{
		"f1-inj":   {Source: labels.SourceInjector, Code: "F1", Expected: labels.Catch, DuplicateOf: "7"},
		"f1-agent": {Source: labels.SourceTask, Code: "F1", Expected: labels.Catch, DuplicateOf: "8"},
		"f3":       {Source: labels.SourceInjector, Code: "F3", Expected: labels.Catch},
		"ok-1":     {Source: labels.SourceTask, Code: "normal", Expected: labels.Pass},
		"to-ask":   {Source: labels.SourceTask, Code: "normal", Expected: labels.Pass},
		"ok-2":     {Source: labels.SourceTask, Code: "F4", Expected: labels.Pass},
		"ok-3":     {Source: labels.SourceInjector, Code: "normal", Expected: labels.Pass},
	}
	cands := []map[string]any{{"id": num("7"), "name": "Jaehoon Kim"}, {"id": num("8"), "name": "김재훈"}}
	recs := []Record{
		{ID: "f1-inj", State: gate.State{Proposed: map[string]any{"name": "김재훈"}, Candidates: cands}},
		{ID: "f1-agent", State: gate.State{Proposed: map[string]any{"name": "김재훈"}, Candidates: cands}},
		{ID: "f3"}, {ID: "ok-1"}, {ID: "ok-2"}, {ID: "ok-3"}, {ID: "unlabeled"}, {ID: "to-ask"}, {ID: "setup-not-scored"},
	}
	verdicts := map[string]Judged{
		"f1-inj": {Verdict: decide.Escalate}, "f1-agent": {Verdict: decide.Commit}, "f3": {Verdict: decide.Hold},
		"ok-1": {Verdict: decide.Commit}, "ok-2": {Verdict: decide.Hold}, "ok-3": {Verdict: decide.Escalate},
		"unlabeled": {Verdict: decide.Hold},
	}
	s := Summarize("X", truths, recs, verdicts)
	check := func(name string, got Cell, n, caught, escalated int) {
		t.Helper()
		if got.N != n || got.Caught != caught || got.Escalated != escalated {
			t.Errorf("%s = %+v, want n %d caught %d escalated %d", name, got, n, caught, escalated)
		}
	}
	check("F1", s.Fault("F1", ""), 2, 1, 1)
	check("F1 injected", s.Fault("F1", GroupInjected), 1, 1, 1)
	check("F1 agents", s.Fault("F1", GroupAgents), 1, 0, 0)
	check("F1 across scripts", s.CrossScript, 1, 1, 1) // 김재훈 for Jaehoon Kim
	check("F1 same script", s.SameScript, 1, 0, 0)
	check("all faults", s.Fault("", ""), 3, 2, 1)
	check("ordinary", s.Ordinary(""), 3, 2, 1)
	check("ordinary, agents", s.Ordinary(GroupAgents), 2, 1, 0)
	if s.Verdicts[decide.Hold] != 2 || s.Verdicts[decide.Escalate] != 2 || s.Verdicts[decide.Commit] != 2 {
		t.Errorf("verdicts = %v; the unlabeled write must not count", s.Verdicts)
	}
	if s.Missing != 1 {
		t.Errorf("missing = %d, want 1: only scored writes without a verdict count", s.Missing)
	}

	var b strings.Builder
	Report(&b, []Summary{s}, nil)
	for _, want := range []string{"| X |", "F1", "50%"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, b.String())
		}
	}
}

func TestPerfOf(t *testing.T) {
	stored := map[string]Judgment{}
	for i := 1; i <= 100; i++ {
		stored[fmt.Sprint(i)] = Judgment{DecisionID: fmt.Sprint(i), LatencyMS: i * 10, CostUSD: 0.0001}
	}
	stored["unscored"] = Judgment{LatencyMS: 99999, CostUSD: 1}
	scored := map[string]Truth{}
	for i := 1; i <= 100; i++ {
		scored[fmt.Sprint(i)] = Truth{}
	}
	scored["no-call"] = Truth{} // a write the gate asked nothing about still counts as a write
	p := PerfOf(stored, scored, 800)
	if p.Calls != 100 || p.P50 != 500 || p.P95 != 950 || p.OverBudget != 20 || math.Abs(p.CostPer1000Writes-0.01*1000/101) > 1e-9 {
		t.Errorf("perf = %+v", p)
	}
}

func TestCompare(t *testing.T) {
	// a catches 9 of 10 faults and holds 2 of 20 ordinary writes; b catches
	// all faults and holds 1.
	truths, recs := map[string]Truth{}, []Record{}
	va, vb := map[string]Judged{}, map[string]Judged{}
	for i := range 30 {
		id := fmt.Sprint(i)
		tr := Truth{Code: "F2", Expected: labels.Catch}
		if i >= 10 {
			tr = Truth{Code: "normal", Expected: labels.Pass}
		}
		truths[id], recs = tr, append(recs, Record{ID: id})
		va[id], vb[id] = Judged{Verdict: decide.Commit}, Judged{Verdict: decide.Commit}
		if i < 9 || i == 10 || i == 11 {
			va[id] = Judged{Verdict: decide.Hold}
		}
		if i < 10 || i == 10 {
			vb[id] = Judged{Verdict: decide.Hold}
		}
	}
	c := Compare(Summarize("a", truths, recs, va), Summarize("b", truths, recs, vb),
		Perf{CostPer1000Writes: 0.5, P95: 300}, Perf{CostPer1000Writes: 5, P95: 3000})
	if math.Abs(c.RecallPP+10) > 1e-9 || math.Abs(c.FalseHoldPP-5) > 1e-9 || c.CostX != 0.1 || c.P95X != 0.1 {
		t.Errorf("comparison = %+v, want recall -10pp, false hold +5pp, 0.1x cost and p95", c)
	}
	if c.NotWorseBy(5) || !c.NotWorseBy(10) {
		t.Errorf("NotWorseBy: a is 10pp behind on recall and 5pp on false hold")
	}
	// Being better by more than the margin is not being worse.
	if !(Comparison{RecallPP: 20, FalseHoldPP: -20}).NotWorseBy(5) {
		t.Error("a far better arm counts as worse")
	}
}

func TestSetThresholds(t *testing.T) {
	cfg := &config.Config{Tables: map[string]config.Table{
		"contacts": {Thresholds: config.DefaultThresholds()},
		"deals":    {Thresholds: config.DefaultThresholds()},
	}}
	got, err := SetThresholds(cfg, "duplicate=0.7, policy=0.6")
	if err != nil {
		t.Fatal(err)
	}
	want := config.DefaultThresholds()
	want.Duplicate, want.Policy = 0.7, 0.6
	for name, tb := range got.Tables {
		if tb.Thresholds != want {
			t.Errorf("%s thresholds = %+v, want %+v", name, tb.Thresholds, want)
		}
	}
	if cfg.Tables["contacts"].Thresholds != config.DefaultThresholds() {
		t.Error("the recording's config was changed")
	}
	for _, bad := range []string{"duplicat=0.7", "duplicate=high", "duplicate=1.5", "duplicate"} {
		if _, err := SetThresholds(cfg, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCascade(t *testing.T) {
	cfg := &config.Config{Model: config.Model{OnFailure: "pass"},
		Tables: map[string]config.Table{"contacts": {Thresholds: config.DefaultThresholds()}}}
	dup := []decide.Question{{ID: "duplicate", Type: "choice", Choices: []string{"7", "none"}}}
	recs := []Record{
		{ID: "esc", Table: "contacts", Questions: dup},
		{ID: "esc-both", Table: "contacts", Questions: dup},
		{ID: "held", Table: "contacts", Questions: dup},
		{ID: "esc-unanswered", Table: "contacts", Questions: dup},
		{ID: "unasked", Table: "contacts"},
	}
	unsure := Judgment{LatencyMS: 100, CostUSD: 0.001, Answers: map[string]decide.Answer{"duplicate": {Choice: "7", Confidence: 0.4}}}
	first := map[string]Judgment{
		"esc": unsure, "esc-both": unsure, "esc-unanswered": unsure,
		"held": {LatencyMS: 100, CostUSD: 0.001, Answers: map[string]decide.Answer{"duplicate": {Choice: "7", Confidence: 0.95}}},
	}
	second := map[string]Judgment{
		"esc":      {LatencyMS: 2000, CostUSD: 0.01, Answers: map[string]decide.Answer{"duplicate": {Choice: "none", Confidence: 0.95}}},
		"esc-both": {LatencyMS: 2000, CostUSD: 0.01, Answers: map[string]decide.Answer{"duplicate": {Choice: "7", Confidence: 0.5}}},
		"held":     {LatencyMS: 2000, CostUSD: 0.01, Answers: map[string]decide.Answer{"duplicate": {Choice: "none", Confidence: 0.95}}},
	}
	verdicts, judged := Cascade(cfg, recs, first, second)
	want := map[string]string{"esc": decide.Commit, "esc-both": decide.Escalate, "held": decide.Hold, "unasked": decide.Commit}
	if len(verdicts) != len(want) {
		t.Errorf("verdicts = %v; a write the second stage has not answered gets none", verdicts)
	}
	for id, v := range want {
		if verdicts[id].Verdict != v {
			t.Errorf("%s: verdict %s, want %s", id, verdicts[id].Verdict, v)
		}
	}
	// The second stage's latency and cost count only where it was asked.
	if j := judged["esc"]; j.LatencyMS != 2100 || math.Abs(j.CostUSD-0.011) > 1e-12 {
		t.Errorf("esc = %+v, want both stages' latency and cost", j)
	}
	if j := judged["held"]; j.LatencyMS != 100 || j.CostUSD != 0.001 {
		t.Errorf("held = %+v, want the first stage only", j)
	}
	if _, ok := judged["esc-unanswered"]; ok {
		t.Error("a write without a second-stage answer has a judgment")
	}
}

// Hard negatives (D28) are scored apart from the ordinary writes: the false
// hold on ordinary writes keeps the meaning it had before they existed.
func TestSummarizeKeepsHardNegativesApart(t *testing.T) {
	truths := map[string]Truth{
		"ok":      {Source: labels.SourceInjector, Code: "normal", Expected: labels.Pass},
		"h2-a":    {Source: labels.SourceInjector, Code: "H2", Expected: labels.Pass, Kind: "abbrev"},
		"h2-b":    {Source: labels.SourceInjector, Code: "H2", Expected: labels.Pass, Kind: "abbrev"},
		"h2-typo": {Source: labels.SourceInjector, Code: "H2", Expected: labels.Pass, Kind: "typo"},
		"h3":      {Source: labels.SourceInjector, Code: "H3", Expected: labels.Pass, Kind: "inactive"},
		"f2":      {Source: labels.SourceInjector, Code: "F2", Expected: labels.Catch},
	}
	recs := []Record{{ID: "ok"}, {ID: "h2-a"}, {ID: "h2-b"}, {ID: "h2-typo"}, {ID: "h3"}, {ID: "f2"}}
	verdicts := map[string]Judged{
		"ok": {Verdict: decide.Commit}, "h2-a": {Verdict: decide.Hold}, "h2-b": {Verdict: decide.Commit},
		"h2-typo": {Verdict: decide.Escalate}, "h3": {Verdict: decide.Commit}, "f2": {Verdict: decide.Hold},
	}
	s := Summarize("X", truths, recs, verdicts)
	check := func(name string, got Cell, n, caught, escalated int) {
		t.Helper()
		if got.N != n || got.Caught != caught || got.Escalated != escalated {
			t.Errorf("%s = %+v, want n %d caught %d escalated %d", name, got, n, caught, escalated)
		}
	}
	check("ordinary", s.Ordinary(""), 1, 0, 0)
	check("H2", s.Hard("H2", ""), 3, 2, 1)
	check("H2 abbrev", s.Hard("H2", "abbrev"), 2, 1, 0)
	check("H3", s.Hard("H3", ""), 1, 0, 0)
	check("all hard negatives", s.Hard("", ""), 4, 2, 1)
	check("faults", s.Fault("", ""), 1, 1, 0)
	if got, want := s.HardKinds(), [][2]string{{"H2", "abbrev"}, {"H2", "typo"}, {"H3", "inactive"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("kinds = %v, want %v", got, want)
	}

	var b strings.Builder
	Report(&b, []Summary{s}, nil)
	for _, want := range []string{"hard negative", "| H2 abbrev | 2 | 1 (50%) |", "50% (2/4"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, b.String())
		}
	}

	// A recording without hard negatives reports as it did before.
	delete(truths, "h2-a")
	delete(truths, "h2-b")
	delete(truths, "h2-typo")
	delete(truths, "h3")
	b.Reset()
	Report(&b, []Summary{Summarize("X", truths, recs, verdicts)}, nil)
	if strings.Contains(b.String(), "hard negative") {
		t.Errorf("report has a hard-negative table with none scored:\n%s", b.String())
	}
}

func TestReadHardNegatives(t *testing.T) {
	sum := func(arm string, hardCaught, faultCaught, ordinaryCaught int) Summary {
		s := Summary{Arm: arm, faults: map[[2]string]Cell{}, ordinary: map[string]Cell{}, hard: map[[2]string]Cell{}}
		s.hard[[2]string{"H1", "same_family"}] = Cell{N: 400, Caught: hardCaught}
		s.faults[[2]string{"F1", GroupInjected}] = Cell{N: 400, Caught: faultCaught}
		s.ordinary[GroupInjected] = Cell{N: 100, Caught: ordinaryCaught}
		return s
	}
	a2 := sum("A2", 160, 380, 4)
	for _, c := range []struct {
		name string
		c    Summary
		want string
	}{
		{"fewer false holds, same recall", sum("C", 80, 384, 2), HardBetter},
		{"about as many false holds", sum("C", 150, 396, 0), HardNotBetter},
		{"more false holds", sum("C", 200, 400, 0), HardNotBetter},
		{"somewhat fewer false holds", sum("C", 120, 392, 0), HardMixed},
		{"fewer false holds bought with recall", sum("C", 60, 340, 0), HardMixed},
		{"fewer false holds, more on ordinary writes", sum("C", 60, 392, 12), HardMixed},
	} {
		if got := ReadHard(c.c, a2); got.Reading != c.want {
			t.Errorf("%s: %s (%+v), want %s", c.name, got.Reading, got, c.want)
		}
	}
	r := ReadHard(sum("C", 80, 384, 2), a2)
	if r.HardPP != -20 || r.RecallPP != 1 || r.OrdinaryPP != -2 || r.OverLimit {
		t.Errorf("%+v, want C − A2 of −20, +1, −2 points and no limit note at 20%%", r)
	}
	if !ReadHard(sum("C", 84, 384, 2), a2).OverLimit {
		t.Error("a false hold above 20% on hard negatives is noted even when it beats A2")
	}
}

package replay

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/lab/labels"
)

// Groups split results by who made the write.
const (
	GroupInjected = "injected"
	GroupAgents   = "agents"
)

// Judged is one arm's verdict on one write.
type Judged struct {
	Verdict string
	Reasons []string
}

// ModelVerdicts applies the gate's verdict rules (decide.Verdict) to a model
// arm's stored answers, with the thresholds and on_failure of cfg. Writes the
// gate asks nothing about commit without a model call, as they would live.
// Writes still to be asked get no verdict.
func ModelVerdicts(cfg *config.Config, recs []Record, stored map[string]Judgment) map[string]Judged {
	out := map[string]Judged{}
	for _, r := range recs {
		switch {
		case r.Verdict == decide.Unsupported:
			out[r.ID] = Judged{Verdict: decide.Unsupported}
		case r.StaleRead():
			out[r.ID] = Judged{Verdict: decide.Reject, Reasons: []string{"stale_read"}}
		case len(r.Questions) == 0:
			out[r.ID] = Judged{Verdict: decide.Commit}
		default:
			j, ok := stored[r.ID]
			if !ok {
				continue
			}
			v, reasons := decide.Verdict(decide.Input{Questions: r.Questions, Answers: j.Answers,
				Thresholds: cfg.Tables[r.Table].Thresholds, OnFailure: cfg.Model.OnFailure})
			out[r.ID] = Judged{Verdict: v, Reasons: reasons}
		}
	}
	return out
}

// Cascade is arm D (decisions.md D27): first's verdict, except that a write
// first escalates takes second's verdict. It also returns what PerfOf
// measures: first's latency and cost, plus second's where second was asked.
// An escalated write second has not answered gets no verdict.
func Cascade(cfg *config.Config, recs []Record, first, second map[string]Judgment) (map[string]Judged, map[string]Judgment) {
	v1, v2 := ModelVerdicts(cfg, recs, first), ModelVerdicts(cfg, recs, second)
	verdicts, judged := map[string]Judged{}, map[string]Judgment{}
	for id, v := range v1 {
		j, asked := first[id]
		if v.Verdict != decide.Escalate {
			verdicts[id] = v
			if asked {
				judged[id] = j
			}
			continue
		}
		w, ok := v2[id]
		if !ok {
			continue
		}
		// The first stage's reasons say why the write went on.
		verdicts[id] = Judged{Verdict: w.Verdict, Reasons: append(slices.Clone(v.Reasons), w.Reasons...)}
		j.LatencyMS += second[id].LatencyMS
		j.CostUSD += second[id].CostUSD
		judged[id] = j
	}
	return verdicts, judged
}

// SetThresholds returns a copy of cfg whose tables all use the thresholds
// in spec ("duplicate=0.7,conflict=0.7"). It is for sensitivity checks of
// model arms, which share thresholds as the registered arms do.
func SetThresholds(cfg *config.Config, spec string) (*config.Config, error) {
	names := []string{"duplicate", "conflict", "policy", "policy_escalate", "escalate_below"}
	set := map[string]float64{}
	for _, kv := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		f, err := strconv.ParseFloat(v, 64)
		if !ok || err != nil || f < 0 || f > 1 || !slices.Contains(names, k) {
			return nil, fmt.Errorf("thresholds %q: want one of %v = a value from 0 to 1", kv, names)
		}
		set[k] = f
	}
	out := *cfg
	out.Tables = map[string]config.Table{}
	for name, t := range cfg.Tables {
		th := &t.Thresholds
		fields := map[string]*float64{"duplicate": &th.Duplicate, "conflict": &th.Conflict, "policy": &th.Policy,
			"policy_escalate": &th.PolicyEscalate, "escalate_below": &th.EscalateBelow}
		for k, f := range set {
			*fields[k] = f
		}
		out.Tables[name] = t
	}
	return &out, nil
}

// RuleVerdicts judges every write with a deterministic arm.
func RuleVerdicts(a RuleArm, recs []Record) map[string]Judged {
	out := map[string]Judged{}
	for _, r := range recs {
		v, reasons := a.Judge(r)
		out[r.ID] = Judged{Verdict: v, Reasons: reasons}
	}
	return out
}

// Cell counts writes and how many an arm stopped. A fault is caught when
// it is held, escalated, or rejected; an ordinary write stopped that way is
// a false hold.
type Cell struct{ N, Caught, Escalated int }

func (c *Cell) add(verdict string) {
	c.N++
	if verdict == decide.Hold || verdict == decide.Escalate || verdict == decide.Reject {
		c.Caught++
	}
	if verdict == decide.Escalate {
		c.Escalated++
	}
}

func (c *Cell) sum(o Cell) {
	c.N, c.Caught, c.Escalated = c.N+o.N, c.Caught+o.Caught, c.Escalated+o.Escalated
}

// Summary is one arm's result over the labeled writes.
type Summary struct {
	Arm      string
	faults   map[[2]string]Cell // code, group
	ordinary map[string]Cell    // group
	// hard holds the hard negatives (D28) by code and kind. A correct gate
	// lets them through, but they are kept out of ordinary so that false
	// hold means what it did before they existed.
	hard map[[2]string]Cell
	// F1 by how the names are written: Hangul against roman letters, or
	// both in the same script. Jev is weaker outside English.
	CrossScript, SameScript Cell
	Verdicts                map[string]int
	Missing                 int // scored writes with no verdict: a model arm has yet to answer them
}

// Fault counts the fault writes of a code ("" for F1-F4 together) made by a
// group ("" for both).
func (s Summary) Fault(code, group string) Cell {
	var c Cell
	for k, v := range s.faults {
		if (code == "" || k[0] == code) && (group == "" || k[1] == group) {
			c.sum(v)
		}
	}
	return c
}

// Hard counts the hard negatives of a code ("" for H1-H4 together) and kind
// ("" for every kind). Caught ones are false holds.
func (s Summary) Hard(code, kind string) Cell {
	var c Cell
	for k, v := range s.hard {
		if (code == "" || k[0] == code) && (kind == "" || k[1] == kind) {
			c.sum(v)
		}
	}
	return c
}

// HardKinds lists the code and kind of the hard negatives scored, in order.
func (s Summary) HardKinds() [][2]string {
	kinds := slices.Collect(maps.Keys(s.hard))
	slices.SortFunc(kinds, func(a, b [2]string) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) })
	return kinds
}

// Ordinary counts the writes a correct gate lets through, hard negatives
// aside.
func (s Summary) Ordinary(group string) Cell {
	var c Cell
	for g, v := range s.ordinary {
		if group == "" || g == group {
			c.sum(v)
		}
	}
	return c
}

// Summarize scores one arm's verdicts against the truths. Writes without a
// truth or a verdict are left out.
func Summarize(arm string, truths map[string]Truth, recs []Record, verdicts map[string]Judged) Summary {
	s := Summary{Arm: arm, faults: map[[2]string]Cell{}, ordinary: map[string]Cell{}, hard: map[[2]string]Cell{}, Verdicts: map[string]int{}}
	for _, r := range recs {
		t, labeled := truths[r.ID]
		j, judged := verdicts[r.ID]
		if !labeled {
			continue
		}
		if !judged {
			s.Missing++
			continue
		}
		s.Verdicts[j.Verdict]++
		group := GroupAgents
		if t.Source == labels.SourceInjector {
			group = GroupInjected
		}
		switch {
		case t.Expected == labels.Pass && strings.HasPrefix(t.Code, "H"):
			k := [2]string{t.Code, t.Kind}
			c := s.hard[k]
			c.add(j.Verdict)
			s.hard[k] = c
		case t.Expected == labels.Pass:
			c := s.ordinary[group]
			c.add(j.Verdict)
			s.ordinary[group] = c
		case strings.HasPrefix(t.Code, "F"):
			k := [2]string{t.Code, group}
			c := s.faults[k]
			c.add(j.Verdict)
			s.faults[k] = c
			if t.Code == "F1" {
				if cross, ok := crossScript(r, t.DuplicateOf); ok && cross {
					s.CrossScript.add(j.Verdict)
				} else if ok {
					s.SameScript.add(j.Verdict)
				}
			}
		}
	}
	return s
}

// crossScript reports whether an F1 write names its person in another
// script than the contact it duplicates.
func crossScript(r Record, of string) (bool, bool) {
	for _, c := range r.State.Candidates {
		if text(c["id"]) == of {
			return hangul(text(r.State.Proposed["name"])) != hangul(text(c["name"])), true
		}
	}
	return false, false
}

func hangul(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Hangul, r) })
}

// wilson is the 95% Wilson score interval for k of n.
func wilson(k, n int) (float64, float64) {
	if n == 0 {
		return 0, 0
	}
	const z = 1.959964
	p, nf := float64(k)/float64(n), float64(n)
	d := 1 + z*z/nf
	c := p + z*z/(2*nf)
	r := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	return max(0, (c-r)/d), min(1, (c+r)/d)
}

// Perf is a model arm's cost and latency over its stored answers.
type Perf struct {
	Calls             int
	P50, P95          int // ms per call
	OverBudget        int // calls slower than the gate's model timeout
	BudgetMS          int
	CostPer1000Writes float64 // over every scored write, asked or not
}

// PerfOf measures a model arm over the scored writes: latency per call, and
// cost spread over every scored write, including those it was not asked about.
func PerfOf(stored map[string]Judgment, scored map[string]Truth, budgetMS int) Perf {
	var lat []int
	cost := 0.0
	for id, j := range stored {
		if _, ok := scored[id]; !ok {
			continue
		}
		lat = append(lat, j.LatencyMS)
		cost += j.CostUSD
	}
	p := Perf{Calls: len(lat), BudgetMS: budgetMS}
	if len(lat) == 0 {
		return p
	}
	slices.Sort(lat)
	at := func(q float64) int { return lat[int(math.Ceil(q*float64(len(lat))))-1] }
	p.P50, p.P95 = at(0.50), at(0.95)
	for _, l := range lat {
		if l > budgetMS {
			p.OverBudget++
		}
	}
	p.CostPer1000Writes = cost / float64(len(scored)) * 1000
	return p
}

// Comparison is arm A against arm B for the go/kill criteria
// (experiment.md, decisions.md D26).
type Comparison struct {
	RecallPP, FalseHoldPP float64 // A − B in percentage points, F1~F4 recall and false hold
	CostX, P95X           float64 // A ÷ B, cost per 1,000 writes and p95 latency
}

func Compare(a, b Summary, pa, pb Perf) Comparison {
	rate := func(c Cell) float64 { return 100 * float64(c.Caught) / float64(max(c.N, 1)) }
	ratio := func(x, y float64) float64 {
		if y == 0 {
			return math.Inf(1)
		}
		return x / y
	}
	return Comparison{
		RecallPP:    rate(a.Fault("", "")) - rate(b.Fault("", "")),
		FalseHoldPP: rate(a.Ordinary("")) - rate(b.Ordinary("")),
		CostX:       ratio(pa.CostPer1000Writes, pb.CostPer1000Writes),
		P95X:        ratio(float64(pa.P95), float64(pb.P95)),
	}
}

// Readings of the hard negatives (decisions.md D28).
const (
	HardBetter    = "better"     // the model arm beats the rule arm near the rules' edges
	HardNotBetter = "not better" // the rules are better kept in code
	HardMixed     = "mixed"      // it depends on the kind
)

// HardReading is a model arm against a rule arm on hard negatives, read by
// the criteria fixed before the results (D28).
type HardReading struct {
	// model − rules in percentage points: false hold on hard negatives,
	// recall on F1~F4, false hold on ordinary writes.
	HardPP, RecallPP, OrdinaryPP float64
	OverLimit                    bool // the model stops more than 20% of the hard negatives
	Reading                      string
}

// ReadHard applies D28. Better: at least 15 points fewer false holds on
// hard negatives, with recall no more than 5 points lower and false hold on
// ordinary writes no more than 5 points higher. Not better: no more than 5
// points fewer false holds on hard negatives. Anything else is mixed.
func ReadHard(model, rules Summary) HardReading {
	rate := func(c Cell) float64 { return 100 * float64(c.Caught) / float64(max(c.N, 1)) }
	r := HardReading{
		HardPP:     rate(model.Hard("", "")) - rate(rules.Hard("", "")),
		RecallPP:   rate(model.Fault("", "")) - rate(rules.Fault("", "")),
		OrdinaryPP: rate(model.Ordinary("")) - rate(rules.Ordinary("")),
		OverLimit:  rate(model.Hard("", "")) > 20,
		Reading:    HardMixed,
	}
	switch {
	case r.HardPP > -5:
		r.Reading = HardNotBetter
	case r.HardPP <= -15 && r.RecallPP >= -5 && r.OrdinaryPP <= 5:
		r.Reading = HardBetter
	}
	return r
}

// NotWorseBy reports whether A trails B by no more than pp percentage points
// on recall and on false hold. Doing better by more than pp still counts.
func (c Comparison) NotWorseBy(pp float64) bool { return c.RecallPP >= -pp && c.FalseHoldPP <= pp }

func pct(k, n int) string {
	if n == 0 {
		return "—"
	}
	lo, hi := wilson(k, n)
	return fmt.Sprintf("%.0f%% (%d/%d, %.0f~%.0f)", 100*float64(k)/float64(n), k, n, 100*lo, 100*hi)
}

// Report writes the summaries as Markdown tables (Korean, like docs/).
func Report(w io.Writer, sums []Summary, perf map[string]Perf) {
	fmt.Fprintln(w, "### 결함을 잡은 비율 (hold·escalate·reject, 괄호는 95% 신뢰구간)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| arm | F1 | F2 | F3 | F4 | F1~F4 |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|")
	for _, s := range sums {
		row := []string{s.Arm}
		for _, code := range []string{"F1", "F2", "F3", "F4", ""} {
			c := s.Fault(code, "")
			row = append(row, pct(c.Caught, c.N))
		}
		fmt.Fprintf(w, "| %s |\n", strings.Join(row, " | "))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "### F1 나눠 보기")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| arm | 주입 | 에이전트 | 한글↔로마자 | 같은 문자 |")
	fmt.Fprintln(w, "|---|---|---|---|---|")
	for _, s := range sums {
		inj, ag := s.Fault("F1", GroupInjected), s.Fault("F1", GroupAgents)
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s |\n", s.Arm, pct(inj.Caught, inj.N), pct(ag.Caught, ag.N),
			pct(s.CrossScript.Caught, s.CrossScript.N), pct(s.SameScript.Caught, s.SameScript.N))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "### 정상 쓰기를 막은 비율 (false hold)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| arm | 전체 | 그중 escalate | 에이전트 쓰기 | 주입 쓰기 |")
	fmt.Fprintln(w, "|---|---|---|---|---|")
	for _, s := range sums {
		all, ag, inj := s.Ordinary(""), s.Ordinary(GroupAgents), s.Ordinary(GroupInjected)
		fmt.Fprintf(w, "| %s | %s | %d | %s | %s |\n", s.Arm, pct(all.Caught, all.N), all.Escalated,
			pct(ag.Caught, ag.N), pct(inj.Caught, inj.N))
	}

	var kinds [][2]string
	if len(sums) > 0 {
		kinds = sums[0].HardKinds()
	}
	if len(kinds) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "### hard negative를 막은 비율 (정상 쓰기라서 낮을수록 좋다)")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "| arm | H1 | H2 | H3 | H4 | H1~H4 | 그중 escalate |")
		fmt.Fprintln(w, "|---|---|---|---|---|---|---|")
		for _, s := range sums {
			row := []string{s.Arm}
			for _, code := range []string{"H1", "H2", "H3", "H4", ""} {
				c := s.Hard(code, "")
				row = append(row, pct(c.Caught, c.N))
			}
			fmt.Fprintf(w, "| %s | %d |\n", strings.Join(row, " | "), s.Hard("", "").Escalated)
		}

		fmt.Fprintln(w)
		fmt.Fprintln(w, "### hard negative 유형별 막은 건수")
		fmt.Fprintln(w)
		head, rule := []string{"유형", "건수"}, "|---|---|"
		for _, s := range sums {
			head, rule = append(head, s.Arm), rule+"---|"
		}
		fmt.Fprintf(w, "| %s |\n%s\n", strings.Join(head, " | "), rule)
		for _, k := range kinds {
			row := []string{k[0] + " " + k[1], fmt.Sprint(sums[0].Hard(k[0], k[1]).N)}
			for _, s := range sums {
				c := s.Hard(k[0], k[1])
				row = append(row, fmt.Sprintf("%d (%.0f%%)", c.Caught, 100*float64(c.Caught)/float64(max(c.N, 1))))
			}
			fmt.Fprintf(w, "| %s |\n", strings.Join(row, " | "))
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "### verdict 분포 (라벨 있는 쓰기)")
	fmt.Fprintln(w)
	verdicts := []string{decide.Commit, decide.Hold, decide.Escalate, decide.Reject, decide.Unsupported}
	fmt.Fprintf(w, "| arm | %s | 답 없음 |\n", strings.Join(verdicts, " | "))
	fmt.Fprintln(w, "|---|---|---|---|---|---|---|")
	for _, s := range sums {
		row := []string{s.Arm}
		for _, v := range verdicts {
			row = append(row, fmt.Sprint(s.Verdicts[v]))
		}
		row = append(row, fmt.Sprint(s.Missing))
		fmt.Fprintf(w, "| %s |\n", strings.Join(row, " | "))
	}

	arms := make([]string, 0, len(perf))
	for a := range perf {
		arms = append(arms, a)
	}
	if len(arms) == 0 {
		return
	}
	slices.Sort(arms)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "### 모델 arm 지연과 비용")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| arm | 호출 | p50 | p95 | 시간 제한 초과 | 쓰기 1,000건당 |")
	fmt.Fprintln(w, "|---|---|---|---|---|---|")
	for _, a := range arms {
		p := perf[a]
		fmt.Fprintf(w, "| %s | %d | %dms | %dms | %d (> %dms) | $%.4f |\n", a, p.Calls, p.P50, p.P95, p.OverBudget, p.BudgetMS, p.CostPer1000Writes)
	}
}

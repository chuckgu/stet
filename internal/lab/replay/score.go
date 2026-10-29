package replay

import (
	"fmt"
	"io"
	"math"
	"slices"
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

// Ordinary counts the writes a correct gate lets through.
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
	s := Summary{Arm: arm, faults: map[[2]string]Cell{}, ordinary: map[string]Cell{}, Verdicts: map[string]int{}}
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

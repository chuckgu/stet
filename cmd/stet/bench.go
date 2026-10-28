package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/decide/jev"
	"github.com/stetdb/stet/internal/gate"
)

type benchCase struct {
	name      string
	state     json.RawMessage
	questions []decide.Question
}

// benchCases are built with the gate's own State type, so the field order
// is what the gate sends, and questions are chosen the way the gate chooses
// them:
//   - update:   reverts a title the enricher set 4 minutes ago (conflict; rule 2 broken)
//   - insert:   a near-duplicate contact (duplicate; no rule applies)
//   - churn:    churn with no open deal (rule 1 applies and is followed)
//   - reformat: the same title written differently (not a conflict)
func benchCases() []benchCase {
	all := []string{"duplicate", "conflict", "policy"}
	rules := []string{
		"Do not set status to 'churned' while the account has an open deal.",
		"Do not overwrite a title with an older value from a stale source.",
	}
	kim := map[string]any{"name": "Jaehoon Kim", "email": "j.kim@acme.com", "company_id": 3,
		"title": "VP Engineering", "status": "active"}
	titleFix := []gate.RecentChange{{
		At: time.Date(2026, 10, 28, 9, 14, 0, 0, time.UTC), MinutesBefore: 4, ByAgent: "enricher",
		Op: "update", Fields: map[string]any{"title": "VP Engineering"},
	}}
	mk := func(name, op string, proposed, current map[string]any, cands []map[string]any, recent []gate.RecentChange, ids []string) benchCase {
		st := gate.State{
			Table: "contacts", Entity: "B2B CRM contact", Operation: op,
			Proposed: proposed, Current: current, Candidates: cands, RecentChanges: recent,
			Policy: strings.Join(rules, "\n"), Provenance: gate.Provenance(),
		}
		b, _ := json.Marshal(st)
		return benchCase{name: name, state: b, questions: decide.Questions(decide.Ask{
			Op: op, CandidateIDs: ids, Enabled: all, Rules: rules, Recent: st.CouldUndoRecent(),
		})}
	}
	return []benchCase{
		mk("update", "update", map[string]any{"title": "Engineer"}, kim, []map[string]any{}, titleFix, nil),
		mk("insert", "insert",
			map[string]any{"name": "Jaehoon Kim", "email": "jkim@acme.co.kr", "company_id": "3", "title": "VP Engineering"},
			nil,
			[]map[string]any{
				{"id": 4812, "name": "김재훈", "email": "j.kim@acme.com", "company_id": 3},
				{"id": 5120, "name": "Jae Kim", "email": "jae.kim@other.io", "company_id": 9},
			},
			[]gate.RecentChange{}, []string{"4812", "5120"}),
		mk("churn", "update", map[string]any{"status": "churned"},
			map[string]any{"name": "Lee", "email": "lee@acme.com", "company_id": 3, "title": "PM", "status": "active", "open_deals": 0},
			[]map[string]any{}, []gate.RecentChange{}, nil),
		mk("reformat", "update", map[string]any{"title": "VP of Engineering"}, kim, []map[string]any{}, titleFix, nil),
	}
}

// runBenchJev times single, sequential Jev calls the way the gate makes them.
func runBenchJev(args []string) error {
	fs := flag.NewFlagSet("bench-jev", flag.ExitOnError)
	n := fs.Int("n", 60, "measured calls, alternating cases")
	version := fs.String("version", "jev-1.14.0", "pinned model version")
	timeout := fs.Duration("timeout", 10*time.Second, "per-call timeout (long, so slow calls are measured, not cut)")
	fs.Parse(args)

	d, err := jev.New(config.Model{Adapter: "jev", Version: *version})
	if err != nil {
		return err
	}
	cases := benchCases()
	call := func(c benchCase) (decide.Answers, time.Duration, error) {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		start := time.Now()
		a, err := d.Decide(ctx, c.state, c.questions)
		return a, time.Since(start), err
	}

	_, cold, err := call(cases[0])
	if err != nil {
		return fmt.Errorf("first call: %w", err)
	}
	fmt.Printf("model %s, cold first call (TLS handshake included): %v\n\n", *version, cold.Round(time.Millisecond))

	byCase := map[string][]time.Duration{}
	var all []time.Duration
	var cost float64
	var errs int
	answers := map[string][]decide.Answers{}
	for i := range *n {
		c := cases[i%len(cases)]
		a, dur, err := call(c)
		if err != nil {
			errs++
			fmt.Printf("call %d (%s): %v\n", i, c.name, err)
			continue
		}
		byCase[c.name] = append(byCase[c.name], dur)
		all = append(all, dur)
		cost += a.CostUSD
		answers[c.name] = append(answers[c.name], a)
	}

	row := func(name string, ds []time.Duration) {
		over := func(lim time.Duration) int {
			k := 0
			for _, x := range ds {
				if x > lim {
					k++
				}
			}
			return k
		}
		fmt.Printf("%-8s n=%-3d p50=%-6v p95=%-6v max=%-6v >500ms=%d >800ms=%d\n", name, len(ds),
			percentile(ds, 0.5).Round(time.Millisecond), percentile(ds, 0.95).Round(time.Millisecond),
			percentile(ds, 1).Round(time.Millisecond), over(500*time.Millisecond), over(800*time.Millisecond))
	}
	for _, c := range cases {
		row(c.name, byCase[c.name])
	}
	row("all", all)
	fmt.Printf("\nerrors %d, cost $%.6f total, $%.6f per call\n", errs, cost, cost/float64(max(len(all), 1)))

	// Same state every call, so spread here is the model's own variance.
	th := config.DefaultThresholds()
	fmt.Printf("\nverdicts with the default thresholds, and answers (choice: confidence; rule: P(broken)):\n")
	for _, c := range cases {
		verdicts := map[string]int{}
		for _, a := range answers[c.name] {
			v, reasons := decide.Verdict(decide.Input{Questions: c.questions, Answers: a.ByQuestion, Thresholds: th})
			verdicts[strings.Join(append([]string{v}, reasons...), " ")]++
		}
		fmt.Printf("  %-8s %v\n", c.name, verdicts)
		for _, q := range c.questions {
			choices := map[string]int{}
			var vals []float64
			for _, a := range answers[c.name] {
				ans := a.ByQuestion[q.ID]
				choices[ans.Choice]++
				if ans.Probability != nil {
					vals = append(vals, *ans.Probability)
				} else {
					vals = append(vals, ans.Confidence)
				}
			}
			if len(vals) == 0 {
				continue
			}
			fmt.Printf("    %-9s choices=%v %.2f-%.2f\n", q.ID, choices, slices.Min(vals), slices.Max(vals))
		}
	}
	return nil
}

func percentile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[int(q*float64(len(s)-1)+0.5)]
}

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"slices"
	"time"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/decide/jev"
)

type benchCase struct {
	name      string
	state     json.RawMessage
	questions []decide.Question
}

// benchCases are shaped like the gate's real states (spec 4): a stale
// title overwrite and a near-duplicate contact.
func benchCases() []benchCase {
	all := []string{"duplicate", "conflict", "policy"}
	policy := "Do not set status to 'churned' while the account has an open deal.\n" +
		"Do not overwrite a title with an older value from a stale source."
	provenance := map[string]string{
		"proposed": "untrusted_agent_input", "current": "database",
		"candidates": "database", "recent_changes": "earlier_agent_writes",
	}
	mk := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	return []benchCase{
		{
			name: "update",
			state: mk(map[string]any{
				"table": "contacts", "entity": "B2B CRM contact", "operation": "update",
				"proposed": map[string]any{"title": "Engineer"},
				"current": map[string]any{"name": "Jaehoon Kim", "email": "j.kim@acme.com",
					"company_id": 3, "title": "VP Engineering", "status": "active"},
				"candidates": []any{},
				"recent_changes": []any{map[string]any{
					"at": "2026-10-28T09:14:00Z", "minutes_before_this_write": 4, "by_agent": "enricher",
					"op": "update", "fields": map[string]any{"title": "VP Engineering"},
				}},
				"policy": policy, "provenance": provenance,
			}),
			questions: decide.Questions("update", nil, all, true),
		},
		{
			name: "insert",
			state: mk(map[string]any{
				"table": "contacts", "entity": "B2B CRM contact", "operation": "insert",
				"proposed": map[string]any{"name": "Jaehoon Kim", "email": "jkim@acme.co.kr",
					"company_id": "3", "title": "VP Engineering"},
				"current": nil,
				"candidates": []any{
					map[string]any{"id": 4812, "name": "김재훈", "email": "j.kim@acme.com", "company_id": 3},
					map[string]any{"id": 5120, "name": "Jae Kim", "email": "jae.kim@other.io", "company_id": 9},
				},
				"recent_changes": []any{},
				"policy":         policy, "provenance": provenance,
			}),
			questions: decide.Questions("insert", []string{"4812", "5120"}, all, true),
		},
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
		fmt.Printf("%-7s n=%-3d p50=%-6v p95=%-6v max=%-6v >500ms=%d >800ms=%d\n", name, len(ds),
			percentile(ds, 0.5).Round(time.Millisecond), percentile(ds, 0.95).Round(time.Millisecond),
			percentile(ds, 1).Round(time.Millisecond), over(500*time.Millisecond), over(800*time.Millisecond))
	}
	for _, c := range cases {
		row(c.name, byCase[c.name])
	}
	row("all", all)
	fmt.Printf("\nerrors %d, cost $%.6f total, $%.6f per call\n", errs, cost, cost/float64(max(len(all), 1)))

	// Same state every call, so spread here is the model's own variance.
	fmt.Printf("\nanswers (confidence below the default escalate_below %.2f counts as low):\n", lowConfidence)
	for _, c := range cases {
		for _, q := range c.questions {
			choices := map[string]int{}
			var confs, exps []float64
			for _, a := range answers[c.name] {
				ans := a.ByQuestion[q.ID]
				choices[ans.Choice]++
				confs = append(confs, ans.Confidence)
				if ans.Expected != nil {
					exps = append(exps, *ans.Expected)
				}
			}
			low := 0
			for _, x := range confs {
				if x < lowConfidence {
					low++
				}
			}
			line := fmt.Sprintf("  %-7s %-9s choices=%v confidence %.2f-%.2f low=%d/%d",
				c.name, q.ID, choices, slices.Min(confs), slices.Max(confs), low, len(confs))
			if len(exps) > 0 {
				line += fmt.Sprintf(" expected %.2f-%.2f", slices.Min(exps), slices.Max(exps))
			}
			fmt.Println(line)
		}
	}
	return nil
}

const lowConfidence = 0.60

func percentile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[int(q*float64(len(s)-1)+0.5)]
}

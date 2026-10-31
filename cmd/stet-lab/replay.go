package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/lab/labels"
	"github.com/stetdb/stet/internal/lab/replay"
)

// runReplay asks a model arm about every recorded write it has not answered
// yet and stores the answers in lab.replays.
func runReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	db := fs.String("db", os.Getenv("STET_LAB_DB"), "database URL as the CRM owner (default $STET_LAB_DB)")
	arm := fs.String("arm", "C", "arm name the answers are stored under")
	adapter := fs.String("adapter", "jev", "model adapter: jev or claude")
	version := fs.String("version", "jev-1.14.0", "pinned model version; claude takes <model>:<effort>, such as claude-opus-5-5:low")
	parallel := fs.Int("parallel", 4, "calls in flight")
	timeout := fs.Duration("timeout", 10*time.Second, "per call; a call cut off is retried, so set it above the slowest call (LLM arms: 2m)")
	retries := fs.Int("retries", 3, "retries per failed call")
	limit := fs.Int("limit", 0, "writes to ask about (0 = all pending)")
	fs.Parse(args)
	if *db == "" {
		return errors.New("replay: -db or STET_LAB_DB is required")
	}
	switch *arm {
	case replay.A0.Name, replay.A1.Name, "D", "D'":
		return fmt.Errorf("replay: stet-lab score computes arm %s from rules or stored answers", *arm)
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *db)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	d, err := decide.New(config.Model{Adapter: *adapter, Version: *version, TimeoutMS: int(timeout.Milliseconds())})
	if err != nil {
		return err
	}
	store := replay.Store{}
	if err := store.Migrate(ctx, conn); err != nil {
		return err
	}
	recs, err := replay.Load(ctx, conn)
	if err != nil {
		return err
	}
	done, err := store.Load(ctx, conn, *arm)
	if err != nil {
		return err
	}
	for _, j := range done {
		if j.Model != d.ModelID() || j.ModelVersion != d.ModelVersion() {
			return fmt.Errorf("replay: arm %s already holds answers from %s %s; use another -arm for %s %s",
				*arm, j.Model, j.ModelVersion, d.ModelID(), d.ModelVersion())
		}
	}
	pending := replay.Pending(recs, done)
	if *limit > 0 && *limit < len(pending) {
		pending = pending[:*limit]
	}
	fmt.Printf("arm %s (%s %s): %d writes recorded, %d answered, %d to ask\n",
		*arm, d.ModelID(), d.ModelVersion(), len(recs), len(done), len(pending))

	start, cost := time.Now(), 0.0
	saved, failed, err := replay.Ask(ctx, d, pending,
		replay.AskOptions{Parallel: *parallel, Timeout: *timeout, Retries: *retries, Backoff: time.Second},
		func(j replay.Judgment) error {
			cost += j.CostUSD
			return store.Save(ctx, conn, *arm, j)
		})
	fmt.Printf("asked %d in %s: %d stored, %d failed (rerun to retry them), $%.4f\n",
		len(pending), time.Since(start).Round(time.Second), saved, failed, cost)
	return err
}

// runScore scores arms against the labels and prints Markdown tables.
func runScore(args []string) error {
	fs := flag.NewFlagSet("score", flag.ExitOnError)
	db := fs.String("db", os.Getenv("STET_LAB_DB"), "database URL as the CRM owner (default $STET_LAB_DB)")
	cfgPath := fs.String("config", "lab/stet-lab.yaml", "Stet config of the recording: thresholds and on_failure for model arms")
	arms := fs.String("arms", "A0,A1,C", "comma-separated arms; A0 and A1 are computed, D and D' cascade C into B and B', others read from lab.replays")
	budget := fs.Int("budget-ms", 800, "model timeout a live gate would use, for the latency table")
	mistakes := fs.String("mistakes", "", "also list this arm's missed faults and false holds")
	similarity := fs.Float64("a-similarity", replay.A0.NameSimilarity, "A0/A1 name similarity; anything but the default is a sensitivity check, not the registered arm")
	thresholds := fs.String("model-thresholds", "", "thresholds for every model arm, such as duplicate=0.7,conflict=0.7; a sensitivity check, not the registered arms")
	fs.Parse(args)
	if *db == "" {
		return errors.New("score: -db or STET_LAB_DB is required")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	modelCfg := cfg
	if *thresholds != "" {
		if modelCfg, err = replay.SetThresholds(cfg, *thresholds); err != nil {
			return err
		}
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *db)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	recs, err := replay.Load(ctx, conn)
	if err != nil {
		return err
	}
	ls := labels.Store{}
	if err := ls.Migrate(ctx, conn); err != nil { // creates lab.manual on recordings made before it
		return err
	}
	items, err := ls.All(ctx, conn)
	if err != nil {
		return err
	}
	manual, err := ls.Manuals(ctx, conn)
	if err != nil {
		return err
	}
	truths, unlabeled := replay.Truths(items, manual, recs)
	pass := 0
	for _, t := range truths {
		if t.Expected == labels.Pass {
			pass++
		}
	}
	fmt.Printf("%d writes recorded; %d scored (%d faults or attacks, %d ordinary), %d without a label\n",
		len(recs), len(truths), len(truths)-pass, pass, len(unlabeled))
	for _, r := range unlabeled {
		fmt.Printf("  unlabeled %s %s %s %s pk=%s: %.120s\n", r.ID, r.Agent, r.Op, r.Table, r.PK, strings.Join(strings.Fields(r.Statement), " "))
	}
	fmt.Println()

	store := replay.Store{}
	var sums []replay.Summary
	perf := map[string]replay.Perf{}
	for _, arm := range strings.Split(*arms, ",") {
		var verdicts map[string]replay.Judged
		name := arm
		switch arm {
		case replay.A0.Name, replay.A1.Name:
			a := replay.A0
			if arm == replay.A1.Name {
				a = replay.A1
			}
			if *similarity != a.NameSimilarity {
				a.NameSimilarity = *similarity
				name = fmt.Sprintf("%s (유사도 %.2f)", arm, *similarity)
			}
			verdicts = replay.RuleVerdicts(a, recs)
		case "D", "D'": // cascades of stored answers (D27): C, then B or B' where C escalates
			first, err := store.Load(ctx, conn, "C")
			if err != nil {
				return fmt.Errorf("arm %s: %w", arm, err)
			}
			second, err := store.Load(ctx, conn, map[string]string{"D": "B", "D'": "B'"}[arm])
			if err != nil {
				return fmt.Errorf("arm %s: %w", arm, err)
			}
			var judged map[string]replay.Judgment
			verdicts, judged = replay.Cascade(modelCfg, recs, first, second)
			perf[arm] = replay.PerfOf(judged, truths, *budget)
		default:
			stored, err := store.Load(ctx, conn, arm)
			if err != nil {
				return fmt.Errorf("arm %s: %w", arm, err)
			}
			verdicts = replay.ModelVerdicts(modelCfg, recs, stored)
			perf[arm] = replay.PerfOf(stored, truths, *budget)
		}
		if _, isModel := perf[arm]; isModel && *thresholds != "" {
			name = fmt.Sprintf("%s (%s)", arm, *thresholds)
		}
		sums = append(sums, replay.Summarize(name, truths, recs, verdicts))
		if arm == *mistakes {
			defer listMistakes(arm, truths, recs, verdicts)
		}
	}
	replay.Report(os.Stdout, sums, perf)

	// The go/kill comparisons (experiment.md, decisions.md D26).
	byArm := map[string]replay.Summary{}
	for _, s := range sums {
		byArm[s.Arm] = s
	}
	c, haveC := byArm["C"]
	if !haveC {
		return nil
	}
	fmt.Println()
	answer := map[bool]string{true: "해당", false: "해당 안 함"}
	if a, ok := byArm[replay.A0.Name]; ok {
		rate := func(c replay.Cell) float64 { return 100 * float64(c.Caught) / float64(max(c.N, 1)) }
		fmt.Printf("판정 참고 (kill 기준 1): F1~F4 recall C %.1f%% − A0 %.1f%% = %+.1f%%p (15%%p). false hold C %.1f%%, A0 %.1f%% (조건 5%% 이하).\n",
			rate(c.Fault("", "")), rate(a.Fault("", "")), rate(c.Fault("", ""))-rate(a.Fault("", "")),
			rate(c.Ordinary("")), rate(a.Ordinary("")))
	}
	pending := func(s replay.Summary, criterion string) bool {
		if s.Missing > 0 {
			fmt.Printf("판정 참고 (%s): %s는 아직 %d건에 답이 없어 판정하지 않는다.\n", criterion, s.Arm, s.Missing)
		}
		return s.Missing > 0
	}
	if b, ok := byArm["B'"]; ok && !pending(b, "kill 기준 2") {
		x := replay.Compare(b, c, perf["B'"], perf["C"])
		kill := x.NotWorseBy(5) && x.CostX <= 2 && x.P95X <= 2
		fmt.Printf("판정 참고 (kill 기준 2): B' − C recall %+.1f%%p, false hold %+.1f%%p (5%%p보다 나쁘지 않은가), 비용 %.1f배, p95 지연 %.1f배 (2배 이내인가) → %s\n",
			x.RecallPP, x.FalseHoldPP, x.CostX, x.P95X, answer[kill])
	}
	b, haveB := byArm["B"]
	if haveB && !pending(b, "go 기준") {
		x := replay.Compare(c, b, perf["C"], perf["B"])
		goes := x.NotWorseBy(5) && x.CostX <= 0.1 && perf["C"].P95 <= *budget
		fmt.Printf("판정 참고 (go 기준): C − B recall %+.1f%%p, false hold %+.1f%%p (5%%p보다 나쁘지 않은가), C 비용은 B의 1/%.0f (10배 이상 싼가), C p95 %dms (%dms 이내인가) → %s\n",
			x.RecallPP, x.FalseHoldPP, 1/x.CostX, perf["C"].P95, *budget, answer[goes])
	}
	if d, ok := byArm["D"]; ok && haveB && !pending(d, "중간 기준") && !pending(b, "중간 기준") {
		x, y := replay.Compare(d, c, perf["D"], perf["C"]), replay.Compare(d, b, perf["D"], perf["B"])
		better := x.RecallPP >= 0 && x.FalseHoldPP <= 0 && (x.RecallPP > 0 || x.FalseHoldPP < 0)
		best := better && y.CostX <= 0.1 && perf["D"].P95 <= *budget
		fmt.Printf("판정 참고 (중간 기준, D27): D − C recall %+.1f%%p, false hold %+.1f%%p (C보다 나은가), D 비용은 B의 1/%.0f (10배 이상 싼가), D p95 %dms (%dms 이내인가) → %s\n",
			x.RecallPP, x.FalseHoldPP, 1/y.CostX, perf["D"].P95, *budget, answer[best])
	}
	return nil
}

// runLabel gives one recorded write a label by hand.
func runLabel(args []string) error {
	fs := flag.NewFlagSet("label", flag.ExitOnError)
	db := fs.String("db", os.Getenv("STET_LAB_DB"), "database URL as the CRM owner (default $STET_LAB_DB)")
	m := labels.Manual{}
	fs.StringVar(&m.DecisionID, "decision", "", "stet.decisions id of the write")
	fs.StringVar(&m.Code, "code", "normal", "fault code (normal, F1-F4, S1-S8)")
	fs.StringVar(&m.Expected, "expected", "", "pass or catch")
	fs.StringVar(&m.Note, "note", "", "what the write does and why that is the right outcome")
	fs.Parse(args)
	if *db == "" || m.DecisionID == "" || m.Note == "" || (m.Expected != labels.Pass && m.Expected != labels.Catch) {
		return errors.New("label: -db, -decision, -expected pass|catch, and -note are required")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *db)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	ls := labels.Store{}
	if err := ls.Migrate(ctx, conn); err != nil {
		return err
	}
	if err := ls.AddManual(ctx, conn, m); err != nil {
		return err
	}
	fmt.Printf("labeled %s: %s %s\n", m.DecisionID, m.Code, m.Expected)
	return nil
}

// listMistakes prints the writes an arm got wrong, for error analysis.
func listMistakes(arm string, truths map[string]replay.Truth, recs []replay.Record, verdicts map[string]replay.Judged) {
	fmt.Printf("\n%s mistakes:\n", arm)
	for _, r := range recs {
		t, ok := truths[r.ID]
		j, judged := verdicts[r.ID]
		if !ok || !judged {
			continue
		}
		stopped := j.Verdict == decide.Hold || j.Verdict == decide.Escalate || j.Verdict == decide.Reject
		if stopped == (t.Expected == labels.Catch) {
			continue
		}
		kind := "missed"
		if stopped {
			kind = "false hold"
		}
		fmt.Printf("  %-10s %-6s %-9s %s %v | %s %s %s | proposed %v\n", kind, t.Code, t.Item, j.Verdict, j.Reasons,
			r.Agent, r.Op, r.PK, r.State.Proposed)
	}
}

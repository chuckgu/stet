package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/lab/agent"
	"github.com/stetdb/stet/internal/lab/crm"
	"github.com/stetdb/stet/internal/lab/labels"
	"github.com/stetdb/stet/internal/lab/tasks"
)

// planOptions fully determine a run's data and tasks. prepare stores them
// so run rebuilds exactly the plan its labels describe.
type planOptions struct {
	Seed       uint64  `json:"seed"`
	TaskSeed   uint64  `json:"task_seed"`
	Accounts   int     `json:"accounts"`
	Contacts   int     `json:"contacts"`
	Korean     float64 `json:"korean"`
	Epoch      string  `json:"epoch"`
	Leads      int     `json:"leads"`
	Updates    int     `json:"updates"`
	Meetings   int     `json:"meetings"`
	Churns     int     `json:"churns"`
	Injections int     `json:"injections"`
	TopUp      int     `json:"top_up"`
	Hard       int     `json:"hard"`
}

func planFlags(fs *flag.FlagSet) *planOptions {
	o := &planOptions{}
	fs.Uint64Var(&o.Seed, "seed", 1, "CRM generator seed (must match stet-lab seed)")
	fs.IntVar(&o.Accounts, "accounts", 800, "accounts (must match stet-lab seed)")
	fs.IntVar(&o.Contacts, "contacts", 5000, "contacts (must match stet-lab seed)")
	fs.Float64Var(&o.Korean, "korean", 0.3, "Korean share (must match stet-lab seed)")
	fs.StringVar(&o.Epoch, "epoch", "2026-10-29", "data epoch (must match stet-lab seed)")
	fs.Uint64Var(&o.TaskSeed, "task-seed", 3, "task generator seed")
	fs.IntVar(&o.Leads, "leads", 300, "sales ops tasks")
	fs.IntVar(&o.Updates, "updates", 300, "enrichment tasks")
	fs.IntVar(&o.Meetings, "meetings", 200, "meeting notes tasks")
	fs.IntVar(&o.Churns, "churns", 60, "churn tasks")
	fs.IntVar(&o.Injections, "injections", 300, "scripted writes")
	fs.IntVar(&o.TopUp, "top-up", 100, "second batch: scripted faults that bring each of F1-F4 to this many injected writes (0 = none)")
	fs.IntVar(&o.Hard, "hard", 0, "third batch: this many hard negatives for each of F1-F4, ordinary writes one step from a fault (decisions.md D28; 0 = none)")
	return o
}

func (o planOptions) dataset() (*crm.Dataset, time.Time, error) {
	now, err := time.Parse(time.DateOnly, o.Epoch)
	if err != nil {
		return nil, now, err
	}
	return crm.Generate(crm.Options{Seed: o.Seed, Accounts: o.Accounts, Contacts: o.Contacts, KoreanShare: o.Korean, Now: now}), now, nil
}

func (o planOptions) plan() (tasks.Plan, error) {
	d, now, err := o.dataset()
	if err != nil {
		return tasks.Plan{}, err
	}
	return tasks.Build(d, tasks.Options{Seed: o.TaskSeed, Leads: o.Leads, Updates: o.Updates,
		Meetings: o.Meetings, Churns: o.Churns, Injections: o.Injections, TopUp: o.TopUp, Hard: o.Hard, Now: now}), nil
}

// runPrepare stores labels, plants the S1 instructions and the hard
// negatives' legacy rows, and grants the agent role its tables. It needs a
// freshly seeded CRM, except with -append.
func runPrepare(args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	db := fs.String("db", os.Getenv("STET_LAB_DB"), "database URL as the CRM owner (default $STET_LAB_DB)")
	agentRole := fs.String("agent-role", "stet_agent", "role the agents connect as; granted the CRM tables only")
	appendTopUp := fs.Bool("append", false, "add the top-up batch's labels to a recording made without one; no reseed")
	o := planFlags(fs)
	fs.Parse(args)
	if *db == "" {
		return errors.New("prepare: -db or STET_LAB_DB is required")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *db)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if *appendTopUp {
		return prepareTopUp(ctx, conn, *o)
	}

	d, _, err := o.dataset()
	if err != nil {
		return err
	}
	var activities int
	if err := conn.QueryRow(ctx, "select count(*) from activities").Scan(&activities); err != nil {
		return err
	}
	if activities != len(d.Activities) {
		return fmt.Errorf("prepare: activities has %d rows, seed data has %d; run stet-lab seed -reset first so labels match the data", activities, len(d.Activities))
	}
	p, err := o.plan()
	if err != nil {
		return err
	}

	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		c := tx.Conn()
		store := labels.Store{}
		if err := store.Migrate(ctx, c); err != nil {
			return err
		}
		for _, sql := range []string{
			"truncate lab.items",
			"create table if not exists lab.meta (key text primary key, value jsonb not null)",
			"create extension if not exists pg_trgm", // the candidate query ranks names by similarity
		} {
			if _, err := c.Exec(ctx, sql); err != nil {
				return fmt.Errorf("%s: %w", sql, err)
			}
		}
		meta, _ := json.Marshal(o)
		if _, err := c.Exec(ctx, "insert into lab.meta values ('plan', $1) on conflict (key) do update set value = excluded.value", string(meta)); err != nil {
			return err
		}
		if err := store.Insert(ctx, c, p.Labels); err != nil {
			return err
		}
		for _, w := range slices.Concat(p.Poison, p.Legacy) {
			if _, err := c.Exec(ctx, w.SQL, w.Args...); err != nil {
				return fmt.Errorf("plant %s: %w", w.ID, err)
			}
		}
		var exists bool
		c.QueryRow(ctx, "select exists (select 1 from pg_roles where rolname = $1)", *agentRole).Scan(&exists)
		if exists {
			role := pgx.Identifier{*agentRole}.Sanitize()
			for _, sql := range []string{
				"grant usage on schema public to " + role,
				"grant select, insert, update on accounts, contacts, deals, activities to " + role,
				"grant usage on all sequences in schema public to " + role,
			} {
				if _, err := c.Exec(ctx, sql); err != nil {
					return fmt.Errorf("%s: %w", sql, err)
				}
			}
		}

		fmt.Printf("prepared %d labels, %d planted instructions, %d legacy rows; agent role %s granted: %v\n",
			len(p.Labels), len(p.Poison), len(p.Legacy), *agentRole, exists)
		printCounts(p.Labels)
		return nil
	})
}

// prepareTopUp adds the top-up batch's labels to a recording made without
// one. The flags must match the stored plan apart from -top-up, and the
// stored plan's labels must be a prefix of the new plan's, so every write
// already recorded keeps its label.
func prepareTopUp(ctx context.Context, conn *pgx.Conn, o planOptions) error {
	var raw []byte
	if err := conn.QueryRow(ctx, "select value from lab.meta where key = 'plan'").Scan(&raw); err != nil {
		return fmt.Errorf("prepare -append: no stored plan: %w", err)
	}
	var stored planOptions
	if err := json.Unmarshal(raw, &stored); err != nil {
		return err
	}
	if stored.TopUp != 0 {
		return fmt.Errorf("prepare -append: the stored plan already has a top-up of %d", stored.TopUp)
	}
	if o.TopUp == 0 {
		return errors.New("prepare -append: -top-up is 0, nothing to add")
	}
	want := stored
	want.TopUp = o.TopUp
	if want != o {
		return fmt.Errorf("prepare -append: flags differ from the stored plan apart from -top-up: stored %+v", stored)
	}
	old, err := stored.plan()
	if err != nil {
		return err
	}
	p, err := o.plan()
	if err != nil {
		return err
	}
	n := len(old.Labels)
	if !reflect.DeepEqual(p.Labels[:n], old.Labels) {
		return errors.New("prepare -append: the top-up changes the first batch's labels")
	}
	var have int
	if err := conn.QueryRow(ctx, "select count(*) from lab.items").Scan(&have); err != nil {
		return err
	}
	if have != n {
		return fmt.Errorf("prepare -append: lab.items has %d labels, the stored plan has %d", have, n)
	}

	add := p.Labels[n:]
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if err := (labels.Store{}).Insert(ctx, tx.Conn(), add); err != nil {
			return err
		}
		meta, _ := json.Marshal(o)
		if _, err := tx.Exec(ctx, "update lab.meta set value = $1 where key = 'plan'", string(meta)); err != nil {
			return err
		}
		fmt.Printf("added %d top-up labels; send them with stet-lab run -phases pre,inject -batch 2\n", len(add))
		printCounts(add)
		return nil
	})
}

func printCounts(items []labels.Item) {
	count := map[string]int{}
	for _, l := range items {
		count[l.Source+"/"+l.Code]++
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Printf("  %-18s %d\n", k, count[k])
	}
}

// prices are $ per million input / output tokens, for the cost estimate.
var prices = map[string][2]float64{
	"claude-opus-5-5":   {4, 20},
	"claude-sonnet-5-5": {2, 10},
	"claude-haiku-5-5":  {0.10, 0.50},
}

// runRun sends the plan's writes through Stet: fresh changes, then scripted
// writes, then the agents' tasks.
func runRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	db := fs.String("db", os.Getenv("STET_LAB_DB"), "database URL as the CRM owner, to read the stored plan (default $STET_LAB_DB)")
	proxy := fs.String("proxy", "127.0.0.1:56433", "Stet proxy address")
	dbname := fs.String("dbname", "stet_lab", "database name behind the proxy")
	user := fs.String("agent-user", "stet_agent", "role the agents connect as")
	password := fs.String("agent-password", "agent-pw", "password of the agent role (test value)")
	phases := fs.String("phases", "pre,inject,agents", "comma-separated phases to run")
	batch := fs.Int("batch", 0, "send only this batch's fresh changes and scripted writes: 1, 2 for the top-up, 3 for the hard negatives (0 = all)")
	limit := fs.Int("limit", 0, "tasks per agent (0 = all)")
	injectLimit := fs.Int("inject-limit", 0, "scripted writes (0 = all)")
	model := fs.String("model", "claude-opus-5-5", "agent model")
	effort := fs.String("effort", "low", "agent effort")
	maxTurns := fs.Int("max-turns", 10, "API calls per task")
	parallel := fs.Int("parallel", 2, "tasks in flight per agent")
	out := fs.String("out", "lab-run.jsonl", "file the per-task results are appended to")
	fs.Parse(args)
	if *db == "" {
		return errors.New("run: -db or STET_LAB_DB is required")
	}
	ctx := context.Background()

	on := func(phase string) bool { return slices.Contains(strings.Split(*phases, ","), phase) }
	if *batch != 0 && on("agents") {
		return errors.New("run: -batch is for the pre and inject phases; agent tasks belong to no batch")
	}

	owner, err := pgx.Connect(ctx, *db)
	if err != nil {
		return err
	}
	defer owner.Close(ctx)
	var raw []byte
	if err := owner.QueryRow(ctx, "select value from lab.meta where key = 'plan'").Scan(&raw); err != nil {
		return fmt.Errorf("run: no stored plan; run stet-lab prepare first: %w", err)
	}
	var o planOptions
	if err := json.Unmarshal(raw, &o); err != nil {
		return err
	}
	p, err := o.plan()
	if err != nil {
		return err
	}
	inBatch := func(ws []tasks.Write) []tasks.Write {
		if *batch == 0 {
			return ws
		}
		return slices.DeleteFunc(slices.Clone(ws), func(w tasks.Write) bool { return w.Batch != *batch })
	}
	pre, injected := inBatch(p.Pre), inBatch(p.Injected)
	if *injectLimit > 0 && *injectLimit < len(injected) {
		injected = injected[:*injectLimit]
	}
	if on("inject") {
		if err := notRecorded(ctx, owner, injected); err != nil {
			return err
		}
	}

	dsn := func(app string) string {
		u := url.URL{Scheme: "postgres", User: url.UserPassword(*user, *password), Host: *proxy, Path: "/" + *dbname,
			RawQuery: url.Values{"sslmode": {"disable"}, "application_name": {app}}.Encode()}
		return u.String()
	}

	if on("pre") {
		if err := send(ctx, dsn(tasks.CRMSync), pre); err != nil {
			return fmt.Errorf("pre: %w", err)
		}
		fmt.Printf("pre: %d fresh changes sent as %s\n", len(pre), tasks.CRMSync)
	}
	if on("inject") {
		if err := send(ctx, dsn(tasks.Injector), injected); err != nil {
			return fmt.Errorf("inject: %w", err)
		}
		fmt.Printf("inject: %d scripted writes sent as %s\n", len(injected), tasks.Injector)
	}
	if !on("agents") {
		return nil
	}

	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return errors.New("run: ANTHROPIC_API_KEY is not set")
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	var mu sync.Mutex
	var results []agent.Result

	byAgent := map[string][]tasks.Task{}
	for _, t := range p.Tasks {
		byAgent[t.Agent] = append(byAgent[t.Agent], t)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var wg sync.WaitGroup
	for _, name := range []string{tasks.SalesOps, tasks.Enricher, tasks.Notes, tasks.Churn} {
		ts := byAgent[name]
		if *limit > 0 && *limit < len(ts) {
			ts = ts[:*limit]
		}
		pool, err := pgxpool.New(ctx, dsn(name))
		if err != nil {
			return err
		}
		defer pool.Close()
		r := agent.New(agent.Config{APIKey: key, Model: *model, Effort: *effort, MaxTurns: *maxTurns}, pool, log)
		sem := make(chan struct{}, *parallel)
		for _, t := range ts {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				res, err := r.Do(ctx, t)
				if err != nil {
					log.Warn("task failed", "task", t.ID, "err", err)
				}
				line, _ := json.Marshal(res)
				mu.Lock()
				w.Write(append(line, '\n'))
				results = append(results, res)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()

	var in, outTok int64
	writes, failed, errs := 0, 0, 0
	for _, r := range results {
		in += r.InputTokens
		outTok += r.OutputTokens
		writes += r.Writes
		failed += r.Failed
		if r.Error != "" {
			errs++
		}
	}
	pr := prices[*model]
	cost := float64(in)/1e6*pr[0] + float64(outTok)/1e6*pr[1]
	fmt.Printf("agents: %d tasks (%d errors), %d writes ok, %d writes failed, %d input / %d output tokens, about $%.2f with %s\n",
		len(results), errs, writes, failed, in, outTok, cost, *model)
	return nil
}

// notRecorded refuses scripted writes Stet has already recorded: sending a
// batch twice would put each of its writes in the stream twice.
func notRecorded(ctx context.Context, conn *pgx.Conn, writes []tasks.Write) error {
	var exists bool
	if err := conn.QueryRow(ctx, "select to_regclass('stet.decisions') is not null").Scan(&exists); err != nil || !exists {
		return err
	}
	rows, err := conn.Query(ctx, "select statement from stet.decisions where agent_id = $1", tasks.Injector)
	if err != nil {
		return err
	}
	tags, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var sql string
		err := r.Scan(&sql)
		return labels.TagOf(sql), err
	})
	if err != nil {
		return err
	}
	recorded := map[string]bool{}
	for _, t := range tags {
		recorded[t] = true
	}
	var dup []string
	for _, w := range writes {
		if recorded[w.ID] {
			dup = append(dup, w.ID)
		}
	}
	if len(dup) > 0 {
		return fmt.Errorf("inject: %d of these scripted writes are already recorded (first %s); sending them again would duplicate them", len(dup), dup[0])
	}
	return nil
}

// send executes writes one autocommit statement at a time on one connection.
func send(ctx context.Context, dsn string, writes []tasks.Write) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	for _, w := range writes {
		if _, err := conn.Exec(ctx, w.SQL, w.Args...); err != nil {
			return fmt.Errorf("%s: %w", w.ID, err)
		}
	}
	return nil
}

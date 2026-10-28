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
		Meetings: o.Meetings, Churns: o.Churns, Injections: o.Injections, Now: now}), nil
}

// runPrepare stores labels, plants the S1 instructions, and grants the
// agent role its tables. It needs a freshly seeded CRM.
func runPrepare(args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	db := fs.String("db", os.Getenv("STET_LAB_DB"), "database URL as the CRM owner (default $STET_LAB_DB)")
	agentRole := fs.String("agent-role", "stet_agent", "role the agents connect as; granted the CRM tables only")
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
		for _, w := range p.Poison {
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

		count := map[string]int{}
		for _, l := range p.Labels {
			count[l.Source+"/"+l.Code]++
		}
		keys := make([]string, 0, len(count))
		for k := range count {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		fmt.Printf("prepared %d labels, %d planted instructions; agent role %s granted: %v\n", len(p.Labels), len(p.Poison), *agentRole, exists)
		for _, k := range keys {
			fmt.Printf("  %-18s %d\n", k, count[k])
		}
		return nil
	})
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

	owner, err := pgx.Connect(ctx, *db)
	if err != nil {
		return err
	}
	var raw []byte
	err = owner.QueryRow(ctx, "select value from lab.meta where key = 'plan'").Scan(&raw)
	owner.Close(ctx)
	if err != nil {
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

	dsn := func(app string) string {
		u := url.URL{Scheme: "postgres", User: url.UserPassword(*user, *password), Host: *proxy, Path: "/" + *dbname,
			RawQuery: url.Values{"sslmode": {"disable"}, "application_name": {app}}.Encode()}
		return u.String()
	}
	on := func(phase string) bool { return slices.Contains(strings.Split(*phases, ","), phase) }

	if on("pre") {
		if err := send(ctx, dsn(tasks.CRMSync), p.Pre); err != nil {
			return fmt.Errorf("pre: %w", err)
		}
		fmt.Printf("pre: %d fresh changes sent as %s\n", len(p.Pre), tasks.CRMSync)
	}
	if on("inject") {
		writes := p.Injected
		if *injectLimit > 0 && *injectLimit < len(writes) {
			writes = writes[:*injectLimit]
		}
		if err := send(ctx, dsn(tasks.Injector), writes); err != nil {
			return fmt.Errorf("inject: %w", err)
		}
		fmt.Printf("inject: %d scripted writes sent as %s\n", len(writes), tasks.Injector)
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

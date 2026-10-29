package replay

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/lab/tasks"
)

// Judgment is a model arm's answers for one write, as stored.
type Judgment struct {
	DecisionID   string
	Model        string
	ModelVersion string
	Answers      map[string]decide.Answer
	LatencyMS    int
	CostUSD      float64
}

// Pending returns the writes a model arm has yet to answer: those the gate
// would have asked its model about, minus crm-sync's setup changes and
// those already answered.
func Pending(recs []Record, done map[string]Judgment) []Record {
	var out []Record
	for _, r := range recs {
		if _, ok := done[r.ID]; ok || r.Agent == tasks.CRMSync || r.Verdict == decide.Unsupported ||
			len(r.Questions) == 0 || len(r.RawState) == 0 {
			continue
		}
		out = append(out, r)
	}
	return out
}

type AskOptions struct {
	Parallel int
	Timeout  time.Duration // per call; generous, since latency is reported apart from answers
	Retries  int
	Backoff  time.Duration // doubles after each failed try
}

// Ask sends each write's recorded state and questions to d and saves each
// answer as it comes back; save is never called concurrently. A call that
// fails is retried; one that still fails is left unsaved and counted, so a
// rerun asks it again.
func Ask(ctx context.Context, d decide.Decider, recs []Record, o AskOptions, save func(Judgment) error) (saved, failed int, err error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(o.Parallel, 1))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, r := range recs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			j, callErr := ask(ctx, d, r, o)
			mu.Lock()
			defer mu.Unlock()
			if callErr != nil {
				failed++
				return
			}
			if serr := save(j); serr != nil && err == nil {
				err = serr
				cancel()
				return
			}
			saved++
		}()
	}
	wg.Wait()
	return saved, failed, err
}

func ask(ctx context.Context, d decide.Decider, r Record, o AskOptions) (Judgment, error) {
	backoff := o.Backoff
	for try := 0; ; try++ {
		cctx, cancel := context.WithTimeout(ctx, o.Timeout)
		start := time.Now()
		a, err := d.Decide(cctx, r.RawState, r.Questions)
		latency := time.Since(start)
		cancel()
		if err == nil {
			return Judgment{DecisionID: r.ID, Model: d.ModelID(), ModelVersion: d.ModelVersion(),
				Answers: a.ByQuestion, LatencyMS: int(latency.Milliseconds()), CostUSD: a.CostUSD}, nil
		}
		if try >= o.Retries || ctx.Err() != nil {
			return Judgment{}, fmt.Errorf("decision %s: %w", r.ID, err)
		}
		time.Sleep(backoff)
		backoff *= 2
	}
}

// Store keeps model arms' answers in one schema (default "lab"). An answer,
// once stored, is never replaced: rescoring reads it instead of asking again
// (rule 4).
type Store struct{ Schema string }

func (s Store) table() string {
	schema := s.Schema
	if schema == "" {
		schema = "lab"
	}
	return pgx.Identifier{schema, "replays"}.Sanitize()
}

func (s Store) Migrate(ctx context.Context, conn *pgx.Conn) error {
	schema := s.Schema
	if schema == "" {
		schema = "lab"
	}
	_, err := conn.Exec(ctx, fmt.Sprintf(`
		create schema if not exists %s;
		create table if not exists %s (
			arm           text not null,
			decision_id   uuid not null,
			created_at    timestamptz not null default now(),
			model         text not null,
			model_version text not null,
			answers       jsonb not null,
			latency_ms    integer not null,
			cost_usd      numeric(12,8) not null,
			primary key (arm, decision_id)
		)`, pgx.Identifier{schema}.Sanitize(), s.table()))
	return err
}

func (s Store) Save(ctx context.Context, conn *pgx.Conn, arm string, j Judgment) error {
	_, err := conn.Exec(ctx, fmt.Sprintf(`
		insert into %s (arm, decision_id, model, model_version, answers, latency_ms, cost_usd)
		values ($1, $2, $3, $4, $5, $6, $7) on conflict do nothing`, s.table()),
		arm, j.DecisionID, j.Model, j.ModelVersion, j.Answers, j.LatencyMS, j.CostUSD)
	return err
}

func (s Store) Load(ctx context.Context, conn *pgx.Conn, arm string) (map[string]Judgment, error) {
	rows, err := conn.Query(ctx, fmt.Sprintf(`
		select decision_id::text, model, model_version, answers, latency_ms, cost_usd::float8
		from %s where arm = $1`, s.table()), arm)
	if err != nil {
		return nil, err
	}
	js, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Judgment])
	if err != nil {
		return nil, err
	}
	out := map[string]Judgment{}
	for _, j := range js {
		out[j.DecisionID] = j
	}
	return out, nil
}

// Package store keeps the decision log, stet.decisions.
package store

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// Arbitrary key serializing concurrent migrations ("Stet").
const migrateLock = 0x53746574

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Migrate creates or updates the stet schema. Safe to run concurrently.
func (s *Store) Migrate(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", migrateLock); err != nil {
			return err
		}
		if err := tx.Conn().PgConn().Exec(ctx, schemaSQL).Close(); err != nil {
			return fmt.Errorf("migrate stet schema: %w", err)
		}
		return nil
	})
}

// Decision is one row of stet.decisions. Nil fields are stored as NULL.
type Decision struct {
	ID         string
	CreatedAt  time.Time
	Mode       string
	AgentID    string
	OnBehalfOf *string
	SessionID  string

	Statement   string
	StatementFP string
	Params      []*string
	Op          string
	TargetTable string
	TargetPK    *string
	Proposed    map[string]any

	State        json.RawMessage
	StateVersion map[string]string

	Model         string
	ModelVersion  string
	Questions     any
	Answers       any
	PolicyVersion string
	Verdict       string
	Reasons       []string

	Executed       bool
	ExecError      *string
	LatencyMS      *int
	ModelLatencyMS *int
	CostUSD        *float64

	// Impact is the measured effect of a multi-row write; nil is stored as NULL.
	Impact any
}

func (s *Store) Insert(ctx context.Context, d *Decision) error {
	var pk any
	if d.TargetPK != nil {
		pk = jsonArg(*d.TargetPK)
	}
	var state any
	if len(d.State) > 0 {
		state = string(d.State)
	}
	var params, proposed, stateVersion any
	if d.Params != nil {
		params = jsonArg(d.Params)
	}
	if d.Proposed != nil {
		proposed = jsonArg(d.Proposed)
	}
	if d.StateVersion != nil {
		stateVersion = jsonArg(d.StateVersion)
	}
	reasons := d.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		insert into stet.decisions (
			id, created_at, mode, agent_id, on_behalf_of, session_id,
			statement, statement_fp, params, op, target_table, target_pk, proposed,
			state, state_version, model, model_version, questions, answers, policy_version,
			verdict, reasons, executed, exec_error, latency_ms, model_latency_ms, cost_usd, impact
		) values (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9::jsonb, $10, $11, $12::jsonb, $13::jsonb,
			$14::jsonb, $15::jsonb, $16, $17, $18::jsonb, $19::jsonb, $20,
			$21, $22, $23, $24, $25, $26, $27, $28::jsonb
		)`,
		d.ID, d.CreatedAt, d.Mode, d.AgentID, d.OnBehalfOf, d.SessionID,
		d.Statement, d.StatementFP, params, d.Op, d.TargetTable, pk, proposed,
		state, stateVersion, d.Model, d.ModelVersion, jsonArg(d.Questions), jsonArg(d.Answers), d.PolicyVersion,
		d.Verdict, reasons, d.Executed, d.ExecError, d.LatencyMS, d.ModelLatencyMS, d.CostUSD, jsonArg(d.Impact),
	)
	return err
}

// Change is an executed write to one row, as recorded.
type Change struct {
	DecisionID string
	At         time.Time
	AgentID    string
	Op         string
	Proposed   map[string]any
}

// RecentChanges returns executed writes to one row since a time, newest first.
func (s *Store) RecentChanges(ctx context.Context, table, pk string, since time.Time, limit int) ([]Change, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, created_at, agent_id, op, proposed
		from stet.decisions
		where target_table = $1 and target_pk = to_jsonb($2::text)
		  and executed and created_at >= $3
		order by created_at desc
		limit $4`, table, pk, since, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Change, error) {
		var c Change
		err := r.Scan(&c.DecisionID, &c.At, &c.AgentID, &c.Op, &c.Proposed)
		return c, err
	})
}

// NewID returns a random (version 4) UUID.
func NewID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// jsonArg encodes v for a $n::jsonb parameter, keeping nil as SQL NULL.
func jsonArg(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return nil
	}
	return string(b)
}

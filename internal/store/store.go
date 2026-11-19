// Package store keeps the decision log, stet.decisions.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// schemaHash is the SHA-256 (hex) of schemaSQL, recorded in
// stet.schema_version once the database has run it.
var schemaHash = func() string {
	sum := sha256.Sum256([]byte(schemaSQL))
	return hex.EncodeToString(sum[:])
}()

// Arbitrary key serializing concurrent migrations ("Stet").
const migrateLock = 0x53746574

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Migrate creates or updates the stet schema. Safe to run concurrently.
//
// It runs schema.sql only when the file differs from the one the database
// last ran, as stet.schema_version records. On a current schema it runs no
// DDL and takes no lock that an open transaction on Stet's tables can block
// (only an AccessShareLock on stet.schema_version, which nothing but Migrate
// writes), so a Stet starting while others hold open transactions on them
// (a reviewer around stet.approve, the executor applying a held write) does
// not wait for them. The flip side: an object dropped by hand is not
// recreated until schema.sql changes.
//
// Its transaction is READ COMMITTED whatever the server's default: a Migrate
// that waited for another's lock must read the marker that one committed,
// which a snapshot taken before the wait (REPEATABLE READ) would not show.
func (s *Store) Migrate(ctx context.Context) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select pg_advisory_xact_lock($1)", migrateLock); err != nil {
			return err
		}
		current, err := schemaCurrent(ctx, tx)
		if err != nil {
			return fmt.Errorf("read stet schema version: %w", err)
		}
		if current {
			return nil
		}
		if err := tx.Conn().PgConn().Exec(ctx, schemaSQL).Close(); err != nil {
			return fmt.Errorf("migrate stet schema: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into stet.schema_version (sha256) values ($1)
			on conflict (one) do update set sha256 = excluded.sha256`, schemaHash); err != nil {
			return fmt.Errorf("record stet schema version: %w", err)
		}
		return nil
	})
}

// schemaCurrent reports whether the database last ran this schemaSQL. A
// database without stet.schema_version has not; looking the table up by
// name first (to_regclass takes no lock) keeps that from being an error.
func schemaCurrent(ctx context.Context, tx pgx.Tx) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `select to_regclass('stet.schema_version') is not null`).Scan(&exists); err != nil || !exists {
		return false, err
	}
	var hash string
	err := tx.QueryRow(ctx, `select sha256 from stet.schema_version`).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return hash == schemaHash, err
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

	LoginRole    *string // the role the database authenticated; nil when unknown
	RowsAffected *int64  // rows the write changed in the gated table; nil when unknown
	Cumulative   any     // stored as jsonb; nil is NULL

	Refused *string // the SQLSTATE Stet answered with instead of sending the statement; nil when it was sent

	// The session's server process and when the gate was told the write's
	// outcome. Insert records them, as backend_pid and the window the write
	// was on the server, only when both are set (spec-v0.2-undo 5).
	BackendPID *uint32   // the session's server process; nil when unknown or the write was not sent
	DoneAt     time.Time // when the gate was told the outcome; zero when the write was not sent or its session's pid is unknown
}

// insertDecisionName is the statement Insert prepares, on each connection of
// the pool it has used whose mode caches statements.
const insertDecisionName = "stet_insert_decision"

// insertDecision records one decision. The window its write was on the
// server is computed on the database's clock (spec-v0.2-undo 5): $34 and $35
// are how many microseconds ago, as Stet measured just before sending the
// insert, the write arrived and the gate was told its outcome. NULL leaves
// them NULL. They are taken from transaction_timestamp(), the start of the
// insert's own implicit transaction: it begins when the first message that
// runs the insert (a Parse or Bind, or the Query of the simple protocol)
// reaches the server, before the insert waits for any lock on
// stet.decisions. clock_timestamp() would be read after such a wait, and the
// wait would make the window later than the write was (spec 5).
const insertDecision = `
	insert into stet.decisions (
		id, created_at, mode, agent_id, on_behalf_of, session_id,
		statement, statement_fp, params, op, target_table, target_pk, proposed,
		state, state_version, model, model_version, questions, answers, policy_version,
		verdict, reasons, executed, exec_error, latency_ms, model_latency_ms, cost_usd, impact,
		login_role, rows_affected, cumulative, refused,
		backend_pid, sent_after, done_before
	) values (
		$1, $2, $3, $4, $5, $6,
		$7, $8, $9::jsonb, $10, $11, $12::jsonb, $13::jsonb,
		$14::jsonb, $15::jsonb, $16, $17, $18::jsonb, $19::jsonb, $20,
		$21, $22, $23, $24, $25, $26, $27, $28::jsonb,
		$29, $30, $31::jsonb, $32,
		$33, transaction_timestamp() - $34::bigint * interval '1 microsecond',
		transaction_timestamp() - $35::bigint * interval '1 microsecond'
	)`

// Insert records d. Its backend pid and the window its write was on the
// server (backend_pid, sent_after, done_before) are recorded only when
// BackendPID is set and DoneAt is not zero; otherwise all three are NULL.
// The two times are never Stet's clock: Insert measures how long ago
// CreatedAt and DoneAt were (on the monotonic clock, for times taken with
// time.Now) and the database subtracts that from the start of the insert's
// transaction. So that the insert's own trip to the server is all that falls
// between that measurement and the server's clock, every value is encoded,
// the connection taken and, where the connection's mode caches statements,
// the statement prepared (once per connection) first. The time since
// CreatedAt is rounded up to the microsecond and the time since DoneAt down,
// so that rounding only widens the window.
//
// A connection in any other mode prepares nothing: a pool behind a
// transaction-mode pooler runs the simple protocol, and a statement it
// prepared may be gone from the server its next transaction reaches, or its
// name taken there by another client's. Its insert runs in one round trip,
// as one implicit transaction: under the simple protocol as configured, and
// otherwise in pgx's exec mode (the extended protocol with an unnamed
// statement and untyped text values). The modes that describe a statement
// before running it (cache_describe the first time, describe_exec each time)
// would do that in a transaction of its own, after the measurement; a lock
// wait there would make the window late by the whole wait.
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
	questions, answers, impact, cumulative := jsonArg(d.Questions), jsonArg(d.Answers), jsonArg(d.Impact), jsonArg(d.Cumulative)
	reasons := d.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	query := insertDecision
	var mode []any // the exec mode to run the insert in; none: the connection's own
	switch conn.Conn().Config().DefaultQueryExecMode {
	case pgx.QueryExecModeCacheStatement:
		if _, err := conn.Conn().Prepare(ctx, insertDecisionName, insertDecision); err != nil {
			return err
		}
		query = insertDecisionName
	case pgx.QueryExecModeSimpleProtocol:
	default:
		mode = []any{pgx.QueryExecModeExec}
	}
	var pid, sentAgo, doneAgo any
	if d.BackendPID != nil && !d.DoneAt.IsZero() {
		// The four bytes as the server's int4 reads them, as pg_backend_pid() does.
		pid = int32(*d.BackendPID)
		sentAgo, doneAgo = microsecondsUp(time.Since(d.CreatedAt)), time.Since(d.DoneAt).Microseconds()
	}
	_, err = conn.Exec(ctx, query, append(mode,
		d.ID, d.CreatedAt, d.Mode, d.AgentID, d.OnBehalfOf, d.SessionID,
		d.Statement, d.StatementFP, params, d.Op, d.TargetTable, pk, proposed,
		state, stateVersion, d.Model, d.ModelVersion, questions, answers, d.PolicyVersion,
		d.Verdict, reasons, d.Executed, d.ExecError, d.LatencyMS, d.ModelLatencyMS, d.CostUSD, impact,
		d.LoginRole, d.RowsAffected, cumulative, d.Refused,
		pid, sentAgo, doneAgo,
	)...)
	return err
}

// microsecondsUp is d in whole microseconds, rounded up.
func microsecondsUp(d time.Duration) int64 {
	us := d / time.Microsecond
	if us*time.Microsecond < d {
		us++
	}
	return int64(us)
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

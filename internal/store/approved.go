package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Approved names an approved held write waiting to be applied.
type Approved struct {
	ID       string
	Snapshot bool // it has a row version or row_absent: its transaction is REPEATABLE READ
}

// ApprovedWrites lists approved held writes, oldest approval first (resolved_at, id), at most limit.
func (s *Store) ApprovedWrites(ctx context.Context, limit int) ([]Approved, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, row_version is not null or row_absent is true
		from stet.held_writes
		where status = 'approved'
		order by resolved_at, id
		limit $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Approved, error) {
		var a Approved
		err := r.Scan(&a.ID, &a.Snapshot)
		return a, err
	})
}

// Querier is a connection or transaction of Stet's own account
// (pgx.Tx, *pgx.Conn and *pgxpool.Pool satisfy it).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Claimed is an approved held write, locked by the transaction that applies it.
type Claimed struct {
	ID, LoginRole, Database, Statement, TargetTable string
	Params                                          []*string
	ParamTypes                                      []uint32
	TargetPK, RowVersion                            *string
	RowAbsent                                       bool
	Settings                                        map[string]string
	Limit                                           *int64 // the rows the approval allows: max_rows, else planned_rows; nil when neither is set
	Lapsed                                          bool   // approved more than 10 minutes before clock_timestamp(), or resolved_at is NULL (B9)
}

// ClaimApproved locks and reads held write id when it is approved and no other
// transaction holds it (FOR UPDATE SKIP LOCKED). nil, nil when there is none to take.
//
// Under REPEATABLE READ the query fails with 40001 when another transaction
// changed the row after this one's snapshot was taken (correction B7); that
// error is returned as it is.
func ClaimApproved(ctx context.Context, q Querier, id string) (*Claimed, error) {
	var c Claimed
	err := q.QueryRow(ctx, `
		select id::text, login_role, database, statement, target_table,
		       params, param_types, target_pk, row_version, row_absent is true, settings,
		       coalesce(max_rows, planned_rows),
		       resolved_at is null or resolved_at < clock_timestamp() - interval '10 minutes'
		from stet.held_writes
		where id = $1 and status = 'approved'
		for update skip locked`, id).Scan(
		&c.ID, &c.LoginRole, &c.Database, &c.Statement, &c.TargetTable,
		&c.Params, &c.ParamTypes, &c.TargetPK, &c.RowVersion, &c.RowAbsent, &c.Settings,
		&c.Limit, &c.Lapsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// MarkApplied sets approved held write id to applied, with applied_at =
// clock_timestamp() (when it was applied, not when the transaction began) and
// rows_affected. It returns an error unless exactly one approved row changed.
func MarkApplied(ctx context.Context, q Querier, id string, rows int64) error {
	return markApproved(ctx, q, "applied", `
		update stet.held_writes set status = 'applied', applied_at = clock_timestamp(), rows_affected = $2
		where id = $1 and status = 'approved'`, id, rows)
}

// MarkFailed sets approved held write id to failed, with applied_at =
// clock_timestamp() and error = reason. It returns an error unless exactly one
// approved row changed.
func MarkFailed(ctx context.Context, q Querier, id, reason string) error {
	return markApproved(ctx, q, "failed", `
		update stet.held_writes set status = 'failed', applied_at = clock_timestamp(), error = $2
		where id = $1 and status = 'approved'`, id, reason)
}

func markApproved(ctx context.Context, q Querier, status, sql, id string, arg any) error {
	tag, err := q.Exec(ctx, sql, id, arg)
	if err != nil {
		return fmt.Errorf("mark held write %s %s: %w", id, status, err)
	}
	if n := tag.RowsAffected(); n != 1 {
		return fmt.Errorf("mark held write %s %s: %d approved rows changed, want 1", id, status, n)
	}
	return nil
}

package store

import (
	"context"
	"time"
)

// HeldWrite is one row of stet.held_writes. Nil fields are stored as NULL.
type HeldWrite struct {
	ID, DecisionID                              string
	CreatedAt                                   time.Time // when the statement arrived
	LoginRole, Database, Statement, TargetTable string
	Params                                      []*string // bind values as text
	ParamTypes                                  []uint32  // the types the client gave in Parse
	TargetPK, RowVersion                        *string
	Verdict                                     string // hold | escalate
	Reasons                                     []string
}

// InsertHeldWrite stores a held write with status pending.
func (s *Store) InsertHeldWrite(ctx context.Context, h *HeldWrite) error {
	var params any
	if h.Params != nil {
		params = jsonArg(h.Params)
	}
	var types any
	if h.ParamTypes != nil {
		types = h.ParamTypes
	}
	reasons := h.Reasons
	if reasons == nil {
		reasons = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		insert into stet.held_writes (
			id, decision_id, created_at, login_role, database, statement,
			params, param_types, target_table, target_pk, row_version, verdict, reasons
		) values (
			$1, $2, $3, $4, $5, $6,
			$7::jsonb, $8::oid[], $9, $10, $11, $12, $13
		)`,
		h.ID, h.DecisionID, h.CreatedAt, h.LoginRole, h.Database, h.Statement,
		params, types, h.TargetTable, h.TargetPK, h.RowVersion, h.Verdict, reasons)
	return err
}

// MarkUndelivered sets a pending held write to undelivered. An id that is
// unknown or already undelivered is not an error.
func (s *Store) MarkUndelivered(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`update stet.held_writes set status = 'undelivered' where id = $1 and status = 'pending'`, id)
	return err
}

// UsageSecond is the rows one role's executed writes changed in a table within one second.
type UsageSecond struct {
	Second time.Time // start of the second
	Rows   int64
}

// UsageBySecond sums executed writes by role to table per second, for
// created_at in [since, before), oldest first. Seconds with no rows are left out.
func (s *Store) UsageBySecond(ctx context.Context, role, table string, since, before time.Time) ([]UsageSecond, error) {
	rows, err := s.pool.Query(ctx, `
		select date_trunc('second', created_at), sum(rows_affected)::bigint
		from stet.decisions
		where login_role = $1 and target_table = $2
		  and created_at >= $3 and created_at < $4
		  and executed and rows_affected > 0
		group by 1
		order by 1`, role, table, since, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageSecond
	for rows.Next() {
		var u UsageSecond
		if err := rows.Scan(&u.Second, &u.Rows); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

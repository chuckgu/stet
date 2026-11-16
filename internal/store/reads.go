package store

import (
	"context"
	"time"
)

// ReadBucket is one minute of reads by one role on one table.
type ReadBucket struct {
	Bucket     time.Time // start of the minute
	LoginRole  string    // "" when unknown
	Table      string
	Statements int64
	Rows       int64
	MaxRows    int64
}

// AddReads adds buckets to stet.reads in one statement, summing into rows
// that already exist and keeping the larger max_rows. Buckets that share a
// key within one call are merged first, because ON CONFLICT cannot touch the
// same row twice in one statement.
func (s *Store) AddReads(ctx context.Context, buckets []ReadBucket) error {
	if len(buckets) == 0 {
		return nil
	}
	n := len(buckets)
	at := make([]time.Time, n)
	roles := make([]string, n)
	tables := make([]string, n)
	stmts := make([]int64, n)
	rows := make([]int64, n)
	maxRows := make([]int64, n)
	for i, b := range buckets {
		at[i], roles[i], tables[i] = b.Bucket, b.LoginRole, b.Table
		stmts[i], rows[i], maxRows[i] = b.Statements, b.Rows, b.MaxRows
	}
	_, err := s.pool.Exec(ctx, `
		insert into stet.reads (bucket, login_role, target_table, statements, rows, max_rows)
		select bucket, login_role, target_table, sum(statements), sum(rows), max(max_rows)
		from unnest($1::timestamptz[], $2::text[], $3::text[], $4::bigint[], $5::bigint[], $6::bigint[])
		     as t(bucket, login_role, target_table, statements, rows, max_rows)
		group by bucket, login_role, target_table
		on conflict (bucket, login_role, target_table) do update set
			statements = stet.reads.statements + excluded.statements,
			rows       = stet.reads.rows + excluded.rows,
			max_rows   = greatest(stet.reads.max_rows, excluded.max_rows)`,
		at, roles, tables, stmts, rows, maxRows)
	return err
}

// ReadLog is one large read.
type ReadLog struct {
	ID, AgentID, SessionID, Statement, StatementFP string
	CreatedAt                                      time.Time
	LoginRole, OnBehalfOf, ExecError               *string
	Tables                                         []string
	Rows                                           int64
}

func (s *Store) InsertReadLog(ctx context.Context, r *ReadLog) error {
	tables := r.Tables
	if tables == nil {
		tables = []string{}
	}
	_, err := s.pool.Exec(ctx, `
		insert into stet.read_log (
			id, created_at, login_role, agent_id, on_behalf_of, session_id,
			statement, statement_fp, target_tables, rows, exec_error
		) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		r.ID, r.CreatedAt, r.LoginRole, r.AgentID, r.OnBehalfOf, r.SessionID,
		r.Statement, r.StatementFP, tables, r.Rows, r.ExecError)
	return err
}

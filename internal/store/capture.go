package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// syncCaptureSettings puts in or changes the one row of stet.capture_settings
// unless it already says $1 and $2, the roles compared as given, order
// included: then the select yields nothing, and nothing is written. It takes
// the table's ROW EXCLUSIVE lock and at most the row's, neither of which the
// open transactions of captured writes hold up: they have only read the row
// (spec-v0.2-undo 11, the locks).
const syncCaptureSettings = `
	insert into stet.capture_settings (roles, max_rows)
	select $1::text[], $2::bigint
	where not exists (select from stet.capture_settings s
	                  where s.roles is not distinct from $1::text[] and s.max_rows = $2::bigint)
	on conflict (one) do update set roles = excluded.roles, max_rows = excluded.max_rows`

// SyncCaptureSettings makes stet.capture_settings say roles (nil or empty:
// every session) and maxRows. It writes only when the row differs.
func (s *Store) SyncCaptureSettings(ctx context.Context, roles []string, maxRows int64) error {
	var r any // NULL: every session
	if len(roles) > 0 {
		r = roles
	}
	_, err := s.pool.Exec(ctx, syncCaptureSettings, r, maxRows)
	return err
}

// purgeChanges deletes, of the $2 oldest change sets by id, those recorded
// more than $1 microseconds before the database's clock, read once. It reads
// the sets through the primary key's index only, to find them and to delete
// them (a join would scan the whole table to delete them), so it does not go
// through the table when nothing is old. The lines of stet.changes go with
// their sets (ON DELETE CASCADE).
const purgeChanges = `
	delete from stet.change_sets
	where id = any (array(
	  select o.id
	  from (select id, at from stet.change_sets order by id limit $2) o
	  where o.at < (select clock_timestamp() - $1::bigint * interval '1 microsecond')))`

// PurgeChanges deletes up to limit change sets recorded more than keep ago,
// oldest first, with their changes, and reports how many sets it deleted.
//
// The delete runs in a transaction of its own, committed only once it has
// returned: when ctx ends first, pgx cancels it on the server and it is
// never committed, so nothing is deleted after PurgeChanges has returned.
func (s *Store) PurgeChanges(ctx context.Context, keep time.Duration, limit int) (int64, error) {
	var n int64
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, purgeChanges, keep.Microseconds(), limit)
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/store"
)

// These tests call stet.undo (spec-v0.2-undo 7): it restores an agent's
// recorded changes in the reviewer's own transaction. Each runs in a database
// of its own (captureDB). The agent's writes are real writes by stet_md5
// through real capture triggers. The reviewer is a role the test creates
// (undoer), and the other parties whose changes undo must leave alone write as
// yet another role (writerIn), whose writes capture does not record. The
// superuser stands for the tables' owner.

// undoFunctionsIn are the functions a role needs EXECUTE on to call
// stet.undo in db (spec 7, 권한): every function of the stet schema whose
// name begins with undo, the plan's and stet.undo's own, as db's catalog
// names them. Read from the catalog, a helper added later is granted too;
// TestUndoGrantCommentNamesEveryFunction checks the list against the grants
// schema.sql documents and revokes.
func undoFunctionsIn(t *testing.T, db string) []string {
	t.Helper()
	rows, err := superuserIn(t, db).Query(context.Background(), `
		select p.oid::regprocedure::text from pg_proc p
		where p.pronamespace = 'stet'::regnamespace and p.proname like 'undo%' order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	functions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return functions
}

// undoer creates a login role with what a reviewer needs to call stet.undo in
// db: USAGE on the stet schema, EXECUTE on undoFunctionsIn, and SELECT, INSERT,
// UPDATE and DELETE on each of tables (SQL names). It returns a connection as
// that role. The role has no right on Stet's tables.
func undoer(t *testing.T, db string, tables ...string) *pgx.Conn {
	t.Helper()
	name := loginRole(t, "reviewer")
	grants := []string{
		"grant usage on schema stet to " + name,
		"grant execute on function " + strings.Join(undoFunctionsIn(t, db), ", ") + " to " + name,
	}
	for _, table := range tables {
		grants = append(grants, "grant select, insert, update, delete on "+table+" to "+name)
	}
	grant(t, db, name, grants...)
	return connectAs(t, db, name)
}

// writerIn creates a login role that may read and change tables in db, and
// returns a connection as it. It is neither the agent nor a reviewer: capture,
// set to stet_md5, does not record its writes.
func writerIn(t *testing.T, db string, tables ...string) *pgx.Conn {
	t.Helper()
	name := loginRole(t, "writer")
	var grants []string
	for _, table := range tables {
		grants = append(grants, "grant select, insert, update, delete on "+table+" to "+name)
	}
	grant(t, db, name, grants...)
	return connectAs(t, db, name)
}

// undoRowsOf collects the lines of stet.undo or stet.undo_plan.
func undoRowsOf(rows pgx.Rows) ([]undoRow, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (undoRow, error) {
		var u undoRow
		var action, reason, warning *string
		err := r.Scan(&u.TargetTable, &u.Key, &action, &u.Outcome, &reason, &warning, &u.ChangeIDs)
		u.Action, u.Reason, u.Warning = derefText(action), derefText(reason), derefText(warning)
		return u, err
	})
}

// derefText is *s, or "" for nil.
func derefText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// tryUndo calls stet.undo(role, args...) on conn: args are since, and then
// until, tables and note if given. A nil role or argument is NULL.
func tryUndo(conn queryer, role any, args ...any) ([]undoRow, error) {
	rows, err := conn.Query(context.Background(),
		"select target_table, key, action, outcome, reason, warning, change_ids from "+call("stet.undo", 1+len(args)),
		append([]any{role}, args...)...)
	if err != nil {
		return nil, err
	}
	return undoRowsOf(rows)
}

// undoOf is stet.undo(role, args...) called on conn (see tryUndo). It stops
// the test if the call fails.
func undoOf(t *testing.T, conn queryer, role string, args ...any) []undoRow {
	t.Helper()
	rows, err := tryUndo(conn, role, args...)
	if err != nil {
		t.Fatalf("stet.undo(%q, %v): %v", role, args, err)
	}
	return rows
}

// checkUndo checks that rows show as want, in order.
func checkUndo(t *testing.T, what string, rows []undoRow, want ...string) {
	t.Helper()
	if got := shown(rows); !slices.Equal(got, want) {
		t.Errorf("%s: stet.undo says\n  %s\nwant\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// rowReader is what a table is read on: a connection or a transaction.
type rowReader interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// contentOf is every row of table (its SQL name) as the text of the whole
// row, sorted, read on conn.
func contentOf(t *testing.T, conn rowReader, table string) string {
	t.Helper()
	var s string
	if err := conn.QueryRow(context.Background(),
		`select coalesce(string_agg(r::text, E'\n' order by r::text), '') from `+table+` r`).Scan(&s); err != nil {
		t.Fatalf("reading %s: %v", table, err)
	}
	return s
}

// checkContent checks that table, read on conn, holds want.
func checkContent(t *testing.T, conn rowReader, what, table, want string) {
	t.Helper()
	if got := contentOf(t, conn, table); got != want {
		t.Errorf("%s: %s holds\n%s\nwant\n%s", what, table, got, want)
	}
}

// undoRecord is a row of stet.undos.
type undoRecord struct {
	ID         int64
	At         time.Time
	By         string
	AgentRole  string
	Since      time.Time
	Until      *time.Time
	Tables     []string
	Note       *string
	Restored   int32
	Skipped    int32
	Failed     int32
	ItemsCount int64
}

// undoRecords are the rows of stet.undos, oldest first, each with the number
// of its items.
func undoRecords(t *testing.T, pool *pgxpool.Pool) []undoRecord {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select u.id, u.at, u.by, u.agent_role, u.since, u.until, u.tables, u.note, u.restored, u.skipped, u.failed,
		       (select count(*) from stet.undo_items i where i.undo_id = u.id)
		from stet.undos u order by u.id`)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[undoRecord])
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// itemsOf are the items stet.undo_items holds for undo id, in their order,
// shown as undoRow.
func itemsOf(t *testing.T, pool *pgxpool.Pool, id int64) []undoRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select target_table, key, action, outcome, reason, warning, change_ids
		from stet.undo_items where undo_id = $1 order by line`, id)
	if err != nil {
		t.Fatal(err)
	}
	items, err := undoRowsOf(rows)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// The agent's INSERT is deleted, its DELETE inserted again, its UPDATE put
// back; a key it changed several times gets its first values back. Each line
// is restored, and the table holds what it held before the agent wrote, value
// for value, over the types capture records as text (spec 13).
func TestUndoRestoresEachKindOfWrite(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner, `create table public.vals (
		id bigint generated always as identity primary key,
		i int, big bigint, n numeric,
		f_nan float8, f_inf float8, f_ninf float8, f_sum float8, r real,
		s text, ts timestamptz, ts_inf timestamptz, d date, iv interval, u uuid, b bytea,
		arr int[], arr2 text[], jb jsonb, jb_obj jsonb, jb_null jsonb, j json, j_null json, j_dup json,
		nul text, flag boolean, c char(5), ip inet,
		g bigint generated always as (i * 2) stored)`)
	const full = `(i, big, n, f_nan, f_inf, f_ninf, f_sum, r, s, ts, ts_inf, d, iv, u, b,
		arr, arr2, jb, jb_obj, jb_null, j, j_null, j_dup, nul, flag, c, ip) values (
		42, 9223372036854775807, 12345678901234567890.0001230,
		'NaN', 'Infinity', '-Infinity', 0.1::float8 + 0.2::float8, 0.1,
		E'line1\nline2 "dq" ''sq'' back\\slash', '2026-11-18 01:02:03.456789+09', 'infinity', '2026-02-28',
		'1 year 2 mons 3 days 04:05:06.789', 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '\x00ff5c27',
		'{1,NULL,3}', '{{a,NULL},{"b c","\"q\""}}', 'null', '{"k": [1, "x", null]}', NULL, 'null', NULL, '{"a": 1,  "a":2}',
		NULL, true, 'ab', '10.0.0.1')`
	mustExec(t, owner,
		`insert into public.vals `+full,
		`insert into public.vals (i, s) values (7, 'sparse')`,
		`insert into public.vals (i, s, jb, j) values (8, 'third', '{"x": "y"}', '[1,  2]')`)
	installCapture(t, owner, "public.vals", "vals", "id")
	before := contentOf(t, owner, "public.vals")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`insert into public.vals `+full,
		`update public.vals set i = 1, s = 'changed', ts = '2000-01-01 00:00:00+00', jb = '{"changed": true}', jb_null = 'null',
		 arr = '{9}', flag = false, c = 'zz', ip = '10.0.0.2', j_dup = '{}', b = '\x01' where id = 1`,
		`update public.vals set n = 0, d = '1999-12-31', iv = '1 day', u = null, j = 'null' where id = 1`,
		`delete from public.vals where id = 2`,
		`update public.vals set s = 'changed' where id = 3`,
		`delete from public.vals where id = 3`,
		`insert into public.vals (i) values (5)`,
		`update public.vals set i = 6 where id = 5`)

	rv := undoer(t, db, "public.vals")
	checkUndo(t, "each kind of write", undoOf(t, rv, "stet_md5", since),
		`vals "4" delete restored - -`,
		`vals "1" update restored - -`,
		`vals "2" insert restored - -`,
		`vals "3" insert restored - -`,
		`vals "5" delete restored - -`)
	checkContent(t, owner, "after the undo", "public.vals", before)
}

// A row one statement of the agent's changed several times is restored: the
// users it deleted come back, and each task whose foreign keys the cascades
// set to NULL one after the other gets all of them back. The task's line was
// one, from the row before the statement to the row after it, so no key is
// skipped as interleaved.
func TestUndoRestoresARowChangedSeveralTimesByOneStatement(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	tasksOfUsers(t, owner)
	users, tasks := contentOf(t, owner, "public.users"), contentOf(t, owner, "public.tasks")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `delete from public.users where id in (1, 2, 3)`)

	rv := undoer(t, db, "public.users", "public.tasks")
	checkShownInAnyOrder(t, "the rows three cascades changed", undoOf(t, rv, "stet_md5", since),
		`tasks "10" update restored - -`,
		`tasks "11" update restored - -`,
		`tasks "12" update restored - -`,
		`users "1" insert restored - -`,
		`users "2" insert restored - -`,
		`users "3" insert restored - -`)
	checkContent(t, owner, "after the undo", "public.users", users)
	checkContent(t, owner, "after the undo", "public.tasks", tasks)
}

// A table keyed by a GENERATED ALWAYS AS IDENTITY column, with a generated
// column: the row the agent inserted is deleted, the one it deleted comes back
// under its own identity value, and the one it updated is put back, its
// generated column computed again.
func TestUndoWithIdentityAndGeneratedColumns(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.items (id int generated always as identity primary key, n int,
		   g int generated always as (n * 2) stored, code int generated by default as identity unique)`,
		`insert into public.items (n) select i from generate_series(1, 3) i`)
	installCapture(t, owner, "public.items", "items", "id")
	before := contentOf(t, owner, "public.items")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`insert into public.items (n) values (40)`,
		`update public.items set n = 10, code = 100 where id = 1`,
		`delete from public.items where id = 2`)

	rv := undoer(t, db, "public.items")
	checkUndo(t, "identity and generated columns", undoOf(t, rv, "stet_md5", since),
		`items "4" delete restored - -`,
		`items "1" update restored - -`,
		`items "2" insert restored - -`)
	checkContent(t, owner, "after the undo", "public.items", before)
}

// A row someone else changed after the agent (changed_since), or between two
// of the agent's changes (interleaved), is skipped and keeps what the other
// party wrote. The other keys are restored.
func TestUndoLeavesWhatOthersChanged(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 5)
	other := writerIn(t, db, "public.deals")
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	mustExec(t, agent, `update public.deals set n = 10 where id = 1`)
	mustExec(t, other, `update public.deals set n = 100 where id = 1`)
	mustExec(t, agent, `update public.deals set n = 20 where id = 2`)
	mustExec(t, agent, `update public.deals set n = 30 where id = 3`)
	mustExec(t, other, `delete from public.deals where id = 3`)
	mustExec(t, agent, `update public.deals set n = 40 where id = 4`)
	mustExec(t, other, `update public.deals set note = 'theirs' where id = 4`)
	mustExec(t, agent, `update public.deals set n = 41 where id = 4`)
	mustExec(t, agent, `delete from public.deals where id = 5`)

	rv := undoer(t, db, "public.deals")
	checkUndo(t, "others' changes", undoOf(t, rv, "stet_md5", since),
		`deals "1" - skipped changed_since -`,
		`deals "2" update restored - -`,
		`deals "3" - skipped changed_since -`,
		`deals "4" - skipped interleaved -`,
		`deals "5" insert restored - -`)
	checkContent(t, owner, "after the undo", "public.deals",
		"(1,r1,100)\n(2,r2,2)\n(4,theirs,41)\n(5,r5,5)")
}

// undoWhileWaiting calls stet.undo(role, args...) on rv in the background,
// waits until it waits for a lock (su reads pg_locks), runs then, and returns
// what the call returned.
func undoWhileWaiting(t *testing.T, su, rv *pgx.Conn, then func(), role string, args ...any) ([]undoRow, error) {
	t.Helper()
	var rows []undoRow
	done := make(chan error, 1)
	go func() {
		var err error
		rows, err = tryUndo(rv, role, args...)
		done <- err
	}()
	if !waitsForALock(t, su, rv.PgConn().PID(), done) {
		t.Fatalf("stet.undo did not wait for a lock: %v", <-done)
	}
	then()
	err := <-done
	return rows, err
}

// stet.undo locks each key's row before it judges it. A row another party
// holds in an open transaction is waited for: once that party commits its
// change, the key is changed_since and the change stays; once it rolls back,
// the key is restored. A reviewer's lock_timeout makes such a key failed and
// the others are restored; a statement_timeout is not caught, and the call
// fails as a whole. A transaction that is not READ COMMITTED is refused
// (plan ruling R15) and nothing changes.
func TestUndoWaitsForAConcurrentChangeAndSkips(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	agent := agentIn(t, db)
	// changed creates public.<name> with rows 1 to 3, lets the agent change
	// each, and returns since, a reviewer that may undo it and another party
	// that holds row 2 in an open transaction, changed to n = 999.
	changed := func(t *testing.T, name string) (time.Time, *pgx.Conn, pgx.Tx) {
		t.Helper()
		keyedTable(t, owner, name, 3)
		since := dbClock(t, pool)
		for i := 1; i <= 3; i++ {
			mustExec(t, agent, fmt.Sprintf("update public.%s set n = %d where id = %d", name, 10*i, i))
		}
		rv := undoer(t, db, "public."+name)
		tx, err := writerIn(t, db, "public."+name).Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { tx.Rollback(ctx) })
		mustExec(t, tx, "update public."+name+" set n = 999 where id = 2")
		return since, rv, tx
	}

	t.Run("the other party commits", func(t *testing.T) {
		since, rv, tx := changed(t, "committed")
		rows, err := undoWhileWaiting(t, owner, rv, func() {
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}, "stet_md5", since)
		if err != nil {
			t.Fatal(err)
		}
		checkUndo(t, "a change committed while the undo waited", rows,
			`committed "1" update restored - -`,
			`committed "2" - skipped changed_since -`,
			`committed "3" update restored - -`)
		checkContent(t, owner, "after the undo", "public.committed", "(1,r1,1)\n(2,r2,999)\n(3,r3,3)")
	})
	t.Run("the other party rolls back", func(t *testing.T) {
		since, rv, tx := changed(t, "rolled")
		rows, err := undoWhileWaiting(t, owner, rv, func() {
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
		}, "stet_md5", since)
		if err != nil {
			t.Fatal(err)
		}
		checkUndo(t, "a change rolled back while the undo waited", rows,
			`rolled "1" update restored - -`,
			`rolled "2" update restored - -`,
			`rolled "3" update restored - -`)
		checkContent(t, owner, "after the undo", "public.rolled", "(1,r1,1)\n(2,r2,2)\n(3,r3,3)")
	})
	t.Run("lock_timeout", func(t *testing.T) {
		since, rv, _ := changed(t, "timed")
		mustExec(t, rv, `set lock_timeout = '200ms'`)
		checkUndo(t, "a row held past the reviewer's lock_timeout", undoOf(t, rv, "stet_md5", since),
			`timed "1" update restored - -`,
			`timed "2" - failed SQLSTATE 55P03: canceling statement due to lock timeout -`,
			`timed "3" update restored - -`)
		checkContent(t, owner, "after the undo", "public.timed", "(1,r1,1)\n(2,r2,20)\n(3,r3,3)")
	})
	t.Run("statement_timeout", func(t *testing.T) {
		since, rv, _ := changed(t, "cancelled")
		recorded := len(undoRecords(t, pool))
		mustExec(t, rv, `set statement_timeout = '300ms'`)
		_, err := tryUndo(rv, "stet_md5", since)
		wantCode(t, err, "57014", "stet.undo past the reviewer's statement_timeout")
		checkContent(t, owner, "after the cancelled undo", "public.cancelled", "(1,r1,10)\n(2,r2,20)\n(3,r3,30)")
		if n := len(undoRecords(t, pool)); n != recorded {
			t.Errorf("stet.undos has %d rows after the cancelled undo, want %d", n, recorded)
		}
	})
	t.Run("REPEATABLE READ and SERIALIZABLE", func(t *testing.T) {
		keyedTable(t, owner, "isolated", 2)
		since := dbClock(t, pool)
		mustExec(t, agent, `update public.isolated set n = 10 where id = 1`)
		rv := undoer(t, db, "public.isolated")
		recorded := len(undoRecords(t, pool))
		for _, level := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
			tx, err := rv.BeginTx(ctx, pgx.TxOptions{IsoLevel: level})
			if err != nil {
				t.Fatal(err)
			}
			_, err = tryUndo(tx, "stet_md5", since)
			wantCode(t, err, "ST006", fmt.Sprintf("stet.undo under %s", level))
			tx.Rollback(ctx)
		}
		checkContent(t, owner, "after the refused undos", "public.isolated", "(1,r1,10)\n(2,r2,2)")
		if n := len(undoRecords(t, pool)); n != recorded {
			t.Errorf("stet.undos has %d rows after the refused undos, want %d", n, recorded)
		}
	})
}

// Two parties that wait for each other: the reviewer's undo holds key 1's row
// (restored) and waits for key 2's, which another party holds and which then
// waits for key 1's. PostgreSQL ends the reviewer's wait with a deadlock
// (40P01). Under READ COMMITTED the key's subtransaction is rolled back and
// the transaction goes on: key 2 is failed, key 1 stays restored, and the
// undo commits; the other party's writes then go through.
func TestUndoReportsADeadlockAsAFailure(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 2)
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	mustExec(t, agent, `update public.deals set n = 10 where id = 1`, `update public.deals set n = 20 where id = 2`)
	rv := undoer(t, db, "public.deals")
	// Should no deadlock form, the reviewer's wait ends after 5s rather than
	// never.
	mustExec(t, rv, `set lock_timeout = '5s'`)
	// The other party looks for deadlocks only after 10s, so the reviewer,
	// after the default 1s, is the one that finds it.
	name := loginRole(t, "writer")
	grant(t, db, name, "grant select, update on public.deals to "+name)
	mustExec(t, owner, "alter role "+name+" in database "+db+" set deadlock_timeout = '10s'")
	tx, err := connectAs(t, db, name).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mustExec(t, tx, `update public.deals set n = 200 where id = 2`)

	theirs := make(chan error, 1)
	rows, err := undoWhileWaiting(t, owner, rv, func() {
		go func() {
			_, err := tx.Exec(ctx, `update public.deals set n = 100 where id = 1`)
			theirs <- err
		}()
	}, "stet_md5", since)
	if err != nil {
		t.Fatal(err)
	}
	checkUndo(t, "a deadlock", rows,
		`deals "1" update restored - -`,
		`deals "2" - failed SQLSTATE 40P01: deadlock detected -`)
	if err := <-theirs; err != nil {
		t.Fatalf("the other party's write: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	checkContent(t, owner, "after both committed", "public.deals", "(1,r1,100)\n(2,r2,200)")
	if recs := undoRecords(t, pool); len(recs) != 1 || recs[0].Restored != 1 || recs[0].Failed != 1 {
		t.Errorf("stet.undos: %+v, want one undo with 1 restored and 1 failed", recs)
	}
}

// A parent and the children a cascade deleted with it come back, the parent
// first. A parent and child the agent created together are both deleted, the
// child first. A child deleted before its parent through a foreign key that
// does not cascade fails with 23503 until its parent is back, and is tried
// again. A parent the agent created, to which it moved an existing child, is
// referenced until the child is moved back, and is tried again.
func TestUndoRestoresParentAndChild(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.parents (id int primary key, note text)`,
		`create table public.kids (id int primary key, parent int references public.parents on delete cascade, note text)`,
		`insert into public.parents values (1, 'p1'), (2, 'p2')`,
		`insert into public.kids values (10, 1, 'k10'), (11, 1, 'k11'), (20, 2, 'k20')`,
		`create table public.firsts (id int primary key)`,
		`create table public.seconds (id int primary key, first int references public.firsts)`,
		`insert into public.firsts values (1)`,
		`insert into public.seconds values (1, 1)`)
	installCapture(t, owner, "public.parents", "parents", "id")
	installCapture(t, owner, "public.kids", "kids", "id")
	installCapture(t, owner, "public.firsts", "firsts", "id")
	installCapture(t, owner, "public.seconds", "seconds", "id")
	tables := []string{"public.parents", "public.kids", "public.firsts", "public.seconds"}
	before := map[string]string{}
	for _, table := range tables {
		before[table] = contentOf(t, owner, table)
	}
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`delete from public.parents where id = 1`,
		`insert into public.parents values (3, 'p3')`,
		`insert into public.kids values (30, 3, 'k30')`,
		`delete from public.seconds where id = 1`,
		`delete from public.firsts where id = 1`,
		`insert into public.parents values (4, 'p4')`,
		`update public.kids set parent = 4 where id = 20`)

	rv := undoer(t, db, tables...)
	checkUndo(t, "parents and children", undoOf(t, rv, "stet_md5", since),
		`firsts "1" insert restored - -`,
		`kids "10" insert restored - -`,
		`kids "11" insert restored - -`,
		`kids "30" delete restored - -`,
		`kids "20" update restored - -`,
		`parents "1" insert restored - -`,
		`parents "3" delete restored - -`,
		`parents "4" delete restored - -`,
		`seconds "1" insert restored - -`)
	for _, table := range tables {
		checkContent(t, owner, "after the undo", table, before[table])
	}
}

// A parent the agent deleted comes back, but the children the cascade deleted
// do not when their table has no capture trigger: the line warns
// children_not_captured. When tables selects only the parents' table, the
// parent comes back and the line warns not_selected with the children's key;
// the children stay deleted.
func TestUndoOfParentOnly(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.par (id int primary key, note text)`,
		`create table public.kid_bare (id int primary key, par int references public.par on delete cascade)`,
		`insert into public.par values (1, 'p1')`,
		`insert into public.kid_bare values (10, 1)`,
		`create table public.par2 (id int primary key, note text)`,
		`create table public.kid2 (id int primary key, par int references public.par2 on delete cascade)`,
		`insert into public.par2 values (2, 'p2')`,
		`insert into public.kid2 values (20, 2)`)
	installCapture(t, owner, "public.par", "parents", "id")
	installCapture(t, owner, "public.par2", "parents2", "id")
	installCapture(t, owner, "public.kid2", "kids2", "id")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `delete from public.par where id = 1`, `delete from public.par2 where id = 2`)

	rv := undoer(t, db, "public.par", "public.par2", "public.kid2")
	checkUndo(t, "children not captured", undoOf(t, rv, "stet_md5", since, nil, []string{"parents"}),
		`parents "1" insert restored - children_not_captured: public.kid_bare`)
	checkUndo(t, "children not selected", undoOf(t, rv, "stet_md5", since, nil, []string{"parents2"}),
		`parents2 "2" insert restored - not_selected: kids2`)
	checkContent(t, owner, "the parents", "public.par", "(1,p1)")
	checkContent(t, owner, "the uncaptured children", "public.kid_bare", "")
	checkContent(t, owner, "the parents", "public.par2", "(2,p2)")
	checkContent(t, owner, "the unselected children", "public.kid2", "")
}

// Unique values the agent moved between rows come back when one row is put
// back before the other: a value moved from row 1 to row 2 fails with 23505 on
// row 1 until row 2 has given it back, and is tried again. Two values swapped
// between two rows, or rotated among three, cannot be put back one row at a
// time under a unique constraint PostgreSQL checks row by row: each key fails
// with 23505 and the rows stay as they are. Under a DEFERRABLE constraint the
// reviewer has deferred, a swap comes back.
func TestUndoRestoresSwappedUniqueValues(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	var tables []string
	for _, c := range []struct{ name, unique string }{
		{"moved", "unique"}, {"swapped", "unique"}, {"rotated", "unique"}, {"deferred", "unique deferrable"},
	} {
		mustExec(t, owner,
			"create table public."+c.name+" (id int primary key, code text "+c.unique+")",
			"insert into public."+c.name+" values (1, 'a'), (2, 'b'), (3, 'c')")
		installCapture(t, owner, "public."+c.name, c.name, "id")
		tables = append(tables, "public."+c.name)
	}
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`update public.moved set code = 'z' where id = 1`,
		`update public.moved set code = 'a' where id = 2`,
		`update public.swapped set code = 't' where id = 1`,
		`update public.swapped set code = 'a' where id = 2`,
		`update public.swapped set code = 'b' where id = 1`,
		`update public.rotated set code = 't' where id = 1`,
		`update public.rotated set code = 'a' where id = 3`,
		`update public.rotated set code = 'c' where id = 2`,
		`update public.rotated set code = 'b' where id = 1`,
		`update public.deferred set code = 't' where id = 1`,
		`update public.deferred set code = 'a' where id = 2`,
		`update public.deferred set code = 'b' where id = 1`)

	rv := undoer(t, db, tables...)
	tx, err := rv.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mustExec(t, tx, `set constraints all deferred`)
	dup := func(table string) string {
		return `SQLSTATE 23505: duplicate key value violates unique constraint "` + table + `_code_key"`
	}
	checkUndo(t, "unique values moved, swapped and rotated", undoOf(t, tx, "stet_md5", since),
		`deferred "1" update restored - -`,
		`deferred "2" update restored - -`,
		`moved "1" update restored - -`,
		`moved "2" update restored - -`,
		`rotated "1" update failed `+dup("rotated")+` -`,
		`rotated "3" update failed `+dup("rotated")+` -`,
		`rotated "2" update failed `+dup("rotated")+` -`,
		`swapped "1" update failed `+dup("swapped")+` -`,
		`swapped "2" update failed `+dup("swapped")+` -`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit with the deferred constraint: %v", err)
	}
	checkContent(t, owner, "moved", "public.moved", "(1,a)\n(2,b)\n(3,c)")
	checkContent(t, owner, "deferred", "public.deferred", "(1,a)\n(2,b)\n(3,c)")
	checkContent(t, owner, "swapped", "public.swapped", "(1,b)\n(2,a)\n(3,c)")
	checkContent(t, owner, "rotated", "public.rotated", "(1,b)\n(2,c)\n(3,a)")
}

// Undoing never cascades into rows another party made (spec 7, referenced).
// A row the agent created to which another party attached a child through an
// ON DELETE CASCADE or SET NULL foreign key is skipped, referenced: <child
// table>, and parent and child stay; so is a row whose referenced column an
// update would change back under an ON UPDATE CASCADE child. A key of several
// columns is matched on all of them; a row that references itself is not its
// own child. A partitioned child table is read through its partitions, and
// named once. An update that leaves the referenced columns alone is restored
// though children reference the row. A foreign key that does not cascade is
// left to the server: 23503, failed.
func TestUndoSkipsReferencedRows(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.par (id int primary key, code text unique not null, note text)`,
		`insert into public.par values (3, 'c3', 'n3'), (6, 'c6', 'n6')`,
		`create table public.kid_cascade (id int primary key, par int references public.par on delete cascade)`,
		`create table public.kid_null (id int primary key, par int references public.par on delete set null)`,
		`create table public.kid_code (id int primary key, code text references public.par (code) on update cascade)`,
		`create table public.kid_plain (id int primary key, par int references public.par)`,
		`insert into public.kid_cascade values (60, 6)`,
		`insert into public.kid_code values (61, 'c6')`,
		`create table public.pair (id int primary key, x int, y int, unique (x, y))`,
		`create table public.kid_pair (id int primary key, x int, y int, foreign key (x, y) references public.pair (x, y) on delete cascade)`,
		`create table public.emp (id int primary key, boss int references public.emp on delete cascade)`,
		`create table public.kid_part (id int, par int references public.par on delete cascade) partition by range (id)`,
		`create table public.kid_part_1 partition of public.kid_part for values from (0) to (1000)`)
	installCapture(t, owner, "public.par", "par", "id")
	installCapture(t, owner, "public.pair", "pair", "id")
	installCapture(t, owner, "public.emp", "emp", "id")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`insert into public.par values (1, 'c1', 'n1')`,
		`insert into public.par values (2, 'c2', 'n2')`,
		`insert into public.par values (4, 'c4', 'n4')`,
		`insert into public.par values (5, 'c5', 'n5')`,
		`insert into public.par values (7, 'c7', 'n7')`,
		`update public.par set code = 'c3x' where id = 3`,
		`update public.par set note = 'changed' where id = 6`,
		`insert into public.pair values (1, 1, 1)`,
		`insert into public.pair values (2, 1, 2)`,
		`insert into public.emp values (1, null)`,
		`insert into public.emp values (2, 2)`)
	kids := []string{"public.kid_cascade", "public.kid_null", "public.kid_code", "public.kid_plain", "public.kid_pair",
		"public.kid_part", "public.emp"}
	mustExec(t, writerIn(t, db, kids...),
		`insert into public.kid_cascade values (10, 1)`,
		`insert into public.kid_null values (20, 2)`,
		`insert into public.kid_code values (30, 'c3x')`,
		`insert into public.kid_plain values (40, 4)`,
		`insert into public.kid_pair values (50, 1, 2)`,
		`insert into public.kid_part values (70, 7)`,
		`insert into public.emp values (3, 1)`)
	children := map[string]string{}
	for _, kid := range kids[:6] {
		children[kid] = contentOf(t, owner, kid)
	}

	rv := undoer(t, db, "public.par", "public.pair", "public.emp")
	grant(t, db, roleOf(t, rv), "grant select on "+strings.Join(kids[:6], ", ")+" to "+roleOf(t, rv))
	checkUndo(t, "referenced rows", undoOf(t, rv, "stet_md5", since),
		`emp "1" - skipped referenced: public.emp -`,
		`emp "2" delete restored - -`,
		`pair "1" delete restored - -`,
		`pair "2" - skipped referenced: public.kid_pair -`,
		`par "1" - skipped referenced: public.kid_cascade -`,
		`par "2" - skipped referenced: public.kid_null -`,
		`par "4" delete failed SQLSTATE 23503: update or delete on table "par" violates foreign key constraint "kid_plain_par_fkey" on table "kid_plain" -`,
		`par "5" delete restored - -`,
		`par "7" - skipped referenced: public.kid_part -`,
		`par "3" - skipped referenced: public.kid_code -`,
		`par "6" update restored - -`)
	checkContent(t, owner, "the parents", "public.par",
		"(1,c1,n1)\n(2,c2,n2)\n(3,c3x,n3)\n(4,c4,n4)\n(6,c6,n6)\n(7,c7,n7)")
	checkContent(t, owner, "the pairs", "public.pair", "(2,1,2)")
	checkContent(t, owner, "the employees", "public.emp", "(1,)\n(3,1)")
	for kid, want := range children {
		checkContent(t, owner, "a child", kid, want)
	}
}

// A key whose restore fails, here because the table owner's trigger raises,
// is failed with the error's SQLSTATE and message, and its row stays as the
// agent left it; the other keys are restored. Every class the trigger raises
// is caught for that key alone: an error, a serialization failure (which
// under READ COMMITTED does not doom the transaction) and an assertion. The
// reviewer's transaction goes on and commits what was restored.
func TestUndoKeepsGoingAfterAFailure(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 4)
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	for i := 1; i <= 4; i++ {
		mustExec(t, agent, fmt.Sprintf("update public.deals set n = %d where id = %d", 10*i, i))
	}
	mustExec(t, owner,
		`create function public.picky() returns trigger language plpgsql as $$
		 begin
		   case coalesce(new.id, old.id)
		     when 2 then raise exception 'deal 2 is locked';
		     when 3 then raise exception 'try again' using errcode = 'serialization_failure';
		     when 4 then assert false, 'deal 4 must stay';
		     else null;
		   end case;
		   return coalesce(new, old);
		 end $$`,
		`create trigger picky before insert or update or delete on public.deals for each row execute function public.picky()`)

	rv := undoer(t, db, "public.deals")
	tx, err := rv.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	checkUndo(t, "keys whose restore fails", undoOf(t, tx, "stet_md5", since),
		`deals "1" update restored - -`,
		`deals "2" update failed SQLSTATE P0001: deal 2 is locked -`,
		`deals "3" update failed SQLSTATE 40001: try again -`,
		`deals "4" update failed SQLSTATE P0004: deal 4 must stay -`)
	checkContent(t, tx, "in the reviewer's transaction", "public.deals", "(1,r1,1)\n(2,r2,20)\n(3,r3,30)\n(4,r4,40)")
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("the reviewer's commit: %v", err)
	}
	checkContent(t, owner, "after the commit", "public.deals", "(1,r1,1)\n(2,r2,20)\n(3,r3,30)\n(4,r4,40)")
	if recs := undoRecords(t, pool); len(recs) != 1 || recs[0].Restored != 1 || recs[0].Failed != 3 {
		t.Errorf("stet.undos: %+v, want one undo with 1 restored and 3 failed", recs)
	}
}

// When the row after the restore is not B, the key is restored_differs: the
// owner's BEFORE UPDATE trigger changed a value, or its BEFORE DELETE trigger
// kept the row the agent inserted.
func TestUndoReportsWhenTheRowDiffers(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 2)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`update public.deals set n = 10 where id = 1`,
		`insert into public.deals values (3, 'r3', 3)`,
		`update public.deals set n = 20 where id = 2`)
	mustExec(t, owner,
		`create function public.stamp() returns trigger language plpgsql as $$
		 begin new.note := new.note || ' (checked)'; return new; end $$`,
		`create trigger stamp before update on public.deals for each row when (new.id = 1) execute function public.stamp()`,
		`create function public.keep() returns trigger language plpgsql as $$ begin return null; end $$`,
		`create trigger keep before delete on public.deals for each row execute function public.keep()`)

	rv := undoer(t, db, "public.deals")
	checkUndo(t, "rows that differ after the restore", undoOf(t, rv, "stet_md5", since),
		`deals "1" update restored_differs - -`,
		`deals "3" delete restored_differs - -`,
		`deals "2" update restored - -`)
	checkContent(t, owner, "after the undo", "public.deals", "(1,\"r1 (checked)\",1)\n(2,r2,2)\n(3,r3,3)")
}

// An update is put back by setting only the columns whose value differs
// between the row now and the row before (plan correction U3): the owner's
// trigger on a column the agent did not change does not fire, and a column the
// reviewer may not update but the agent did not change is not touched.
func TestUndoSetsOnlyTheColumnsThatDiffer(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 2)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `update public.deals set n = 10 where id = 1`, `update public.deals set note = 'x' where id = 2`)
	mustExec(t, owner,
		`create function public.frozen() returns trigger language plpgsql as $$ begin raise exception 'note is frozen'; end $$`,
		`create trigger frozen before update of note on public.deals for each row when (new.id = 1) execute function public.frozen()`)
	rv := loginRole(t, "reviewer")
	grant(t, db, rv, "grant usage on schema stet to "+rv,
		"grant execute on function "+strings.Join(undoFunctionsIn(t, db), ", ")+" to "+rv,
		"grant select on public.deals to "+rv, "grant update (n) on public.deals to "+rv)
	checkUndo(t, "only what differs", undoOf(t, connectAs(t, db, rv), "stet_md5", since),
		`deals "1" update restored - -`,
		`deals "2" update failed SQLSTATE 42501: permission denied for table deals -`)
	checkContent(t, owner, "after the undo", "public.deals", "(1,r1,1)\n(2,x,2)")
}

// The restore statements run under the calling session's settings, so the
// owner's trigger that names a table without its schema finds it through the
// reviewer's search_path; without that schema on the path, the trigger fails
// and so does each key. A reviewer whose TimeZone is Asia/Seoul and DateStyle
// German gets the timestamptz and date values back exactly, and keeps its
// settings.
func TestUndoRunsUnderTheCallersSettings(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.events (id int primary key, at timestamptz, day date, n int)`,
		`insert into public.events values (1, '2026-11-18 01:02:03.456789+09', '2026-02-28', 1),
		   (2, '2026-01-01 00:00:00+00', '2026-01-01', 2), (3, 'infinity', '2026-12-31', 3)`)
	installCapture(t, owner, "public.events", "events", "id")
	before := contentOf(t, owner, "public.events")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`update public.events set n = 10 where id = 1`,
		`update public.events set at = '2000-01-01 00:00:00+00', day = '2000-01-01' where id = 2`,
		`delete from public.events where id = 3`,
		`insert into public.events values (4, now(), current_date, 4)`)
	mustExec(t, owner,
		`create schema crm`,
		`create table crm.audit (what text)`,
		`create function public.audited() returns trigger language plpgsql as $$
		 begin insert into audit values (tg_op || ' ' || coalesce(new.id, old.id)); return null; end $$`,
		`create trigger audited after insert or update or delete on public.events for each row execute function public.audited()`)

	rv := undoer(t, db, "public.events")
	grant(t, db, roleOf(t, rv), "grant usage on schema crm to "+roleOf(t, rv), "grant insert on crm.audit to "+roleOf(t, rv))
	tx, err := rv.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	notFound := `SQLSTATE 42P01: relation "audit" does not exist`
	checkUndo(t, "without crm on the search_path", undoOf(t, tx, "stet_md5", since),
		`events "1" update failed `+notFound+` -`,
		`events "2" update failed `+notFound+` -`,
		`events "3" insert failed `+notFound+` -`,
		`events "4" delete failed `+notFound+` -`)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	mustExec(t, rv, `set search_path = crm, public`, `set timezone = 'Asia/Seoul'`, `set datestyle = 'German'`)
	checkUndo(t, "with crm on the search_path", undoOf(t, rv, "stet_md5", since),
		`events "1" update restored - -`,
		`events "2" update restored - -`,
		`events "3" insert restored - -`,
		`events "4" delete restored - -`)
	checkContent(t, owner, "after the undo", "public.events", before)
	checkContent(t, owner, "the owner's audit", "crm.audit", "(\"DELETE 4\")\n(\"INSERT 3\")\n(\"UPDATE 1\")\n(\"UPDATE 2\")")
	var path, tz, style string
	if err := rv.QueryRow(ctx, `select current_setting('search_path'), current_setting('TimeZone'), current_setting('DateStyle')`).
		Scan(&path, &tz, &style); err != nil {
		t.Fatal(err)
	}
	if path != "crm, public" || tz != "Asia/Seoul" || style != "German, DMY" {
		t.Errorf("the reviewer's settings after stet.undo: %q, %q, %q", path, tz, style)
	}
}

// What the caller's search_path or temporary objects put first does not
// change what stet.undo does: with a schema at the front of the path that
// holds an always-true jsonb and text =, and format, jsonb_object, num_nulls,
// array_append and cardinality that return other values, and
// temporary tables named like the customer's and Stet's tables and like the
// types stet.undo works with, which PostgreSQL looks up in the session's
// temporary schema first, the result and the table's content are what they
// are without them.
func TestUndoIgnoresTheCallersSearchPath(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 5)
	mustExec(t, owner, `create table public.deal_notes (id int primary key, deal int references public.deals on delete cascade)`)
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	mustExec(t, agent,
		`update public.deals set n = 10 where id = 1`,
		`update public.deals set note = 'two' where id = 2`,
		`insert into public.deals values (6, 'r6', 6)`,
		`update public.deals set n = 30 where id = 3`,
		`update public.deals set n = 3 where id = 3`,
		`delete from public.deals where id = 5`)
	mustExec(t, owner, `update public.deals set n = 200 where id = 2`)
	plain := undoer(t, db, "public.deals")
	name := roleOf(t, plain)
	grant(t, db, name, "grant select on public.deal_notes to "+name)
	want := []string{
		`deals "1" update restored - -`,
		`deals "2" - skipped changed_since -`,
		`deals "6" delete restored - -`,
		`deals "3" - skipped no_net_change -`,
		`deals "5" insert restored - children_not_captured: public.deal_notes`,
	}
	tx, err := plain.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkUndo(t, "without the trap", undoOf(t, tx, "stet_md5", since), want...)
	content := contentOf(t, tx, "public.deals")
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	mustExec(t, owner,
		`create schema trap`,
		`create function trap.always(jsonb, jsonb) returns boolean language sql as 'select true'`,
		`create operator trap.= (leftarg = jsonb, rightarg = jsonb, function = trap.always)`,
		`create function trap.always(text, text) returns boolean language sql as 'select true'`,
		`create operator trap.= (leftarg = text, rightarg = text, function = trap.always)`,
		`create function trap.always(integer, integer) returns boolean language sql as 'select true'`,
		`create operator trap.= (leftarg = integer, rightarg = integer, function = trap.always)`,
		`create function trap.format(text, integer) returns text language sql as $$select 'trapped'$$`,
		`create function trap.format(text, text) returns text language sql as $$select 'trapped'$$`,
		`create function trap.jsonb_object(text[], text[]) returns jsonb language sql as $$select '{}'::jsonb$$`,
		`create function trap.num_nulls(integer) returns integer language sql as 'select 1'`,
		`create function trap.num_nulls(text) returns integer language sql as 'select 1'`,
		`create function trap.array_append(anyarray, anyelement) returns anyarray language sql as 'select $1'`,
		`create function trap.cardinality(anyarray) returns integer language sql as 'select 0'`,
		"grant usage on schema trap to "+name)
	trapped := connectAs(t, db, name)
	mustExec(t, trapped,
		`set search_path = trap, pg_catalog, public`,
		`create temp table deals (id int primary key, note text, n int)`,
		`insert into deals values (1, 'r1', 10), (2, 'r2', 20), (3, 'r3', 3), (4, 'trapped', 0), (5, 'r5', 5)`,
		`create temp table change_sets (id bigint)`,
		`create temp table changes (id bigint)`,
		`create temp table undos (id bigint)`,
		`create temp table undo_items (id bigint)`)
	var live bool
	var text string
	if err := trapped.QueryRow(ctx, `select '{"a": 1}'::jsonb = '{"b": 2}'::jsonb, format('%s', 7)`).Scan(&live, &text); err != nil {
		t.Fatal(err)
	}
	if !live || text != "trapped" {
		t.Fatalf("the trap is not live in the reviewer's session: jsonb = %v, format %q", live, text)
	}
	// Temporary tables named like types: their row types come before
	// pg_catalog's types of the same name for an unqualified type name.
	mustExec(t, trapped,
		`create temp table text (x int)`, `create temp table jsonb (x int)`, `create temp table record (x int)`,
		`create temp table int4 (x int)`, `create temp table int8 (x int)`, `create temp table oid (x int)`,
		`create temp table bool (x int)`, `create temp table date (x int)`)
	if _, err := trapped.Exec(ctx, `select 'a'::text`); err == nil {
		t.Fatalf("the temporary type text is not live in the reviewer's session")
	}
	checkUndo(t, "with the trap", undoOf(t, trapped, "stet_md5", since), want...)
	checkContent(t, owner, "after the undo with the trap", "public.deals", content)
	if recs := undoRecords(t, pool); len(recs) != 1 || recs[0].By != name || recs[0].Restored != 3 || recs[0].Skipped != 2 {
		t.Errorf("stet.undos: %+v, want one undo by %s with 3 restored and 2 skipped", recs, name)
	}
}

// stet.undo called right after stet.undo_plan in one transaction does what the
// plan said, key for key: the same target_table, key and action, over keys of
// every kind of line.
func TestUndoMatchesThePlan(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 3)
	keyedTable(t, owner, "deals", 10)
	keyedTable(t, owner, "accounts", 3)
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	mustExec(t, agent,
		`update public.deals set n = 10 where id = 1`,
		`delete from public.deals where id = 2`,
		`insert into public.deals values (11, 'r11', 11)`,
		`update public.deals set n = 30 where id = 3`,
		`update public.deals set n = 3 where id = 3`,
		`update public.deals set id = 104 where id = 4`,
		`update public.deals set n = n + 1 where id between 5 and 8`,
		`update public.deals set n = 90 where id = 9`,
		`delete from public.accounts where id = 1`,
		`insert into public.accounts values (4, 'r4', 4)`,
		`update public.accounts set n = 20 where id = 2`)
	mustExec(t, owner, `update public.deals set n = 900 where id = 9`)

	rv := undoer(t, db, "public.deals", "public.accounts")
	tx, err := rv.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	plan := planOf(t, tx, "stet_md5", since)
	undone := undoOf(t, tx, "stet_md5", since)
	keyOf := func(rows []undoRow) []string {
		var keys []string
		for _, r := range rows {
			keys = append(keys, fmt.Sprintf("%s %v %s", r.TargetTable, deref(r.Key), r.Action))
		}
		return keys
	}
	if p, u := keyOf(plan), keyOf(undone); !slices.Equal(p, u) || len(p) != 11 {
		t.Errorf("the plan says\n  %s\nstet.undo did\n  %s", strings.Join(p, "\n  "), strings.Join(u, "\n  "))
	}
	outcome := map[string]string{"would_restore": "restored", "skipped": "skipped", "not_captured": "not_captured"}
	for i := range min(len(plan), len(undone)) {
		if p, u := plan[i], undone[i]; outcome[p.Outcome] != u.Outcome || p.Reason != u.Reason || p.Warning != u.Warning ||
			!slices.Equal(p.ChangeIDs, u.ChangeIDs) {
			t.Errorf("line %d: the plan says %v %v, stet.undo %v %v", i, p, p.ChangeIDs, u, u.ChangeIDs)
		}
	}
}

// stet.undo runs in the reviewer's transaction. Rolled back, it leaves the
// table, stet.undos and stet.undo_items as they were. Committed, it leaves one
// row in stet.undos (by the reviewer, with what it selected, its note and the
// three counts) and one item per line of its result, and stet.history shows
// the undo against each change it restored.
func TestUndoIsTheReviewersTransaction(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 4)
	before := contentOf(t, owner, "public.deals")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`update public.deals set n = 10 where id = 1`,
		`update public.deals set n = 11 where id = 1`,
		`delete from public.deals where id = 2`,
		`insert into public.deals values (5, 'r5', 5)`)
	mustExec(t, owner, `update public.deals set n = 30 where id = 2`)
	mustExec(t, owner, `insert into public.deals values (2, 'theirs', 2)`)
	after := contentOf(t, owner, "public.deals")
	until := dbClock(t, pool)
	rv := undoer(t, db, "public.deals")
	grant(t, db, roleOf(t, rv), "grant select on stet.history to "+roleOf(t, rv))
	undoneOf := func() map[int64]string {
		t.Helper()
		rows, err := rv.Query(ctx, `select change_id, coalesce(undo_id::text, '-') || ' ' || coalesce(undone_at::text, '-')
			from stet.history where target_table = 'deals' order by change_id`)
		if err != nil {
			t.Fatal(err)
		}
		m := map[int64]string{}
		for rows.Next() {
			var id int64
			var s string
			if err := rows.Scan(&id, &s); err != nil {
				t.Fatal(err)
			}
			m[id] = s
		}
		return m
	}
	want := []string{
		`deals "1" update restored - -`,
		`deals "2" - skipped changed_since -`,
		`deals "5" delete restored - -`,
	}

	tx, err := rv.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkUndo(t, "rolled back", undoOf(t, tx, "stet_md5", since, until, []string{"deals", "nope"}, "ticket 42"), want...)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	checkContent(t, owner, "after the rollback", "public.deals", after)
	if recs := undoRecords(t, pool); len(recs) != 0 {
		t.Errorf("stet.undos after the rollback: %+v, want none", recs)
	}
	for id, s := range undoneOf() {
		if s != "- -" {
			t.Errorf("stet.history shows change %d undone (%s) after the rollback", id, s)
		}
	}

	tx, err = rv.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result := undoOf(t, tx, "stet_md5", since, until, []string{"deals", "nope"}, "ticket 42")
	checkUndo(t, "committed", result, want...)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	recs := undoRecords(t, pool)
	if len(recs) != 1 {
		t.Fatalf("stet.undos after the commit: %+v, want one row", recs)
	}
	r := recs[0]
	if r.By != roleOf(t, rv) || r.AgentRole != "stet_md5" || !r.Since.Equal(since) || r.Until == nil || !r.Until.Equal(until) ||
		!slices.Equal(r.Tables, []string{"deals", "nope"}) || deref(r.Note) != "ticket 42" ||
		r.Restored != 2 || r.Skipped != 1 || r.Failed != 0 || r.ItemsCount != 3 {
		t.Errorf("stet.undos row: %+v (note %v)", r, deref(r.Note))
	}
	items := itemsOf(t, pool, r.ID)
	if fmt.Sprint(items) != fmt.Sprint(result) {
		t.Errorf("stet.undo_items: %v, want the result %v", items, result)
	}
	for i := range min(len(items), len(result)) {
		if !slices.Equal(items[i].ChangeIDs, result[i].ChangeIDs) {
			t.Errorf("item %d names changes %v, the result %v", i, items[i].ChangeIDs, result[i].ChangeIDs)
		}
	}
	var undone string
	if err := pool.QueryRow(ctx, `select $1::bigint || ' ' || at::text from stet.undos where id = $1`, r.ID).Scan(&undone); err != nil {
		t.Fatal(err)
	}
	restored := map[int64]bool{}
	for _, l := range result {
		if l.Outcome == "restored" {
			for _, id := range l.ChangeIDs {
				restored[id] = true
			}
		}
	}
	shown := undoneOf()
	if len(shown) != 4 || len(restored) != 3 {
		t.Errorf("stet.history shows %d changes of deals, %d of them restored; want 4 and 3", len(shown), len(restored))
	}
	for id, s := range shown {
		want := "- -"
		if restored[id] {
			want = undone
		}
		if s != want {
			t.Errorf("stet.history shows change %d undone as %q, want %q", id, s, want)
		}
	}
	checkContent(t, owner, "after the commit", "public.deals", strings.Replace(before, "(2,r2,2)", "(2,theirs,2)", 1))
}

// Undone twice, the second undo finds every key changed_since, changes
// nothing, and leaves stet.history showing the first undo. A change restored
// by two undos shows the latest.
func TestUndoTwice(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 3)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`update public.deals set n = 10 where id = 1`,
		`delete from public.deals where id = 2`,
		`insert into public.deals values (4, 'r4', 4)`)
	rv := undoer(t, db, "public.deals")
	checkUndo(t, "the first undo", undoOf(t, rv, "stet_md5", since),
		`deals "1" update restored - -`,
		`deals "2" insert restored - -`,
		`deals "4" delete restored - -`)
	content := contentOf(t, owner, "public.deals")
	checkUndo(t, "the second undo", undoOf(t, rv, "stet_md5", since),
		`deals "1" - skipped changed_since -`,
		`deals "2" - skipped changed_since -`,
		`deals "4" - skipped changed_since -`)
	checkContent(t, owner, "after the second undo", "public.deals", content)
	recs := undoRecords(t, pool)
	if len(recs) != 2 || recs[1].Skipped != 3 || recs[1].Restored != 0 {
		t.Fatalf("stet.undos: %+v, want two, the second with 3 skipped", recs)
	}
	if n := countOf(t, pool, `select count(*) from stet.history where target_table = 'deals' and undo_id is distinct from $1`, recs[0].ID); n != 0 {
		t.Errorf("%d changes in stet.history do not show the first undo", n)
	}
	// Another party puts the agent's value back: a third undo restores key 1
	// again, and stet.history shows that latest undo for its change.
	mustExec(t, owner, `update public.deals set n = 10 where id = 1`)
	checkUndo(t, "the third undo", undoOf(t, rv, "stet_md5", since),
		`deals "1" update restored - -`,
		`deals "2" - skipped changed_since -`,
		`deals "4" - skipped changed_since -`)
	recs = undoRecords(t, pool)
	var shown []string
	rows, err := pool.Query(context.Background(), `select key || ' ' || undo_id || ' ' || (undone_at = u.at)
		from stet.history h join stet.undos u on u.id = h.undo_id where h.target_table = 'deals' order by change_id`)
	if err != nil {
		t.Fatal(err)
	}
	if shown, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if want := []string{fmt.Sprintf("1 %d true", recs[2].ID), fmt.Sprintf("2 %d true", recs[0].ID),
		fmt.Sprintf("4 %d true", recs[0].ID)}; !slices.Equal(shown, want) {
		t.Errorf("stet.history shows the undos %q, want %q", shown, want)
	}
}

// A reviewer without the right to write the table gets its keys failed with
// 42501: without UPDATE the row cannot be locked, without DELETE a row the
// agent inserted cannot be deleted. So does a reviewer that cannot read a
// table whose foreign key would cascade from the row it deletes, or from
// which row-level security would hide such rows (the cascade itself would not
// be hidden from them). A table it cannot read at all shows as one line,
// skipped (plan ruling R11).
func TestUndoNeedsRightsOnTheTable(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 1)
	keyedTable(t, owner, "accounts", 1)
	keyedTable(t, owner, "hidden", 1)
	keyedTable(t, owner, "parents", 1)
	mustExec(t, owner, `create table public.kids (id int primary key, parent int references public.parents on delete cascade)`)
	keyedTable(t, owner, "folders", 1)
	mustExec(t, owner,
		`create table public.files (id int primary key, folder int references public.folders on delete cascade)`,
		`alter table public.files enable row level security`,
		`create policy nobody on public.files using (false)`)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`update public.deals set n = 10 where id = 1`,
		`update public.accounts set n = 10 where id = 1`,
		`insert into public.accounts values (2, 'r2', 2)`,
		`update public.hidden set n = 10 where id = 1`,
		`insert into public.parents values (5, 'r5', 5)`,
		`insert into public.folders values (5, 'r5', 5)`)
	mustExec(t, owner, `insert into public.files values (50, 5)`)

	rv := undoer(t, db, "public.parents", "public.folders")
	name := roleOf(t, rv)
	grant(t, db, name, "grant select on public.deals to "+name, "grant select, update on public.accounts to "+name,
		"grant select on public.files to "+name)
	checkUndo(t, "missing rights", undoOf(t, rv, "stet_md5", since),
		`accounts "1" update restored - -`,
		`accounts "2" delete failed SQLSTATE 42501: permission denied for table accounts -`,
		`deals "1" - failed SQLSTATE 42501: permission denied for table deals -`,
		`folders "5" delete failed SQLSTATE 42501: row-level security on public.files applies to `+name+` -`,
		`hidden - - skipped SQLSTATE 42501: permission denied for table hidden -`,
		`parents "5" delete failed SQLSTATE 42501: permission denied for table kids -`)
	checkContent(t, owner, "deals", "public.deals", "(1,r1,10)")
	checkContent(t, owner, "accounts", "public.accounts", "(1,r1,1)\n(2,r2,2)")
	checkContent(t, owner, "parents", "public.parents", "(1,r1,1)\n(5,r5,5)")
	checkContent(t, owner, "folders", "public.folders", "(1,r1,1)\n(5,r5,5)")
	checkContent(t, owner, "files", "public.files", "(50,5)")
}

// The rights to plan and to undo are separate (spec 7, 권한). PUBLIC can
// execute none of stet.undo's functions and has no right on stet.undos,
// stet.undo_items or stet.undone_changes. A role granted only the plan's functions, even with every
// right on the table, cannot call stet.undo or stet.undo_record. A reviewer
// that may undo cannot read or write the records, and a record it makes by
// calling stet.undo_record itself is in its own name, also when it acts as
// another role.
func TestUndoGrantsAreSeparate(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 1)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `update public.deals set n = 10 where id = 1`)

	functions := undoFunctionsIn(t, db)
	if len(functions) < len(planFunctions)+11 {
		t.Fatalf("the catalog has %d undo functions: %v", len(functions), functions)
	}
	for _, f := range functions {
		var public bool
		if err := owner.QueryRow(ctx, `select has_function_privilege('public', $1, 'execute')`, f).Scan(&public); err != nil {
			t.Fatal(err)
		}
		if public {
			t.Errorf("PUBLIC may execute %s", f)
		}
	}
	for _, table := range []string{"stet.undos", "stet.undo_items", "stet.undone_changes"} {
		var granted bool
		if err := owner.QueryRow(ctx, `select has_table_privilege('public', $1, 'select, insert, update, delete, truncate')`,
			table).Scan(&granted); err != nil {
			t.Fatal(err)
		}
		if granted {
			t.Errorf("PUBLIC has a right on %s", table)
		}
	}

	planner := undoReviewer(t, db, "public.deals")
	grant(t, db, roleOf(t, planner), "grant insert, update, delete on public.deals to "+roleOf(t, planner))
	_, err := tryUndo(planner, "stet_md5", since)
	wantCode(t, err, "42501", "a role granted the plan's functions calling stet.undo")
	_, err = planner.Exec(ctx, `select stet.undo_record('stet_md5', $1, null, null, null, '[]')`, since)
	wantCode(t, err, "42501", "a role granted the plan's functions calling stet.undo_record")
	checkContent(t, owner, "after the planner's attempts", "public.deals", "(1,r1,10)")

	rv := undoer(t, db, "public.deals")
	name := roleOf(t, rv)
	for _, sql := range []string{
		`select from stet.undos`, `select from stet.undo_items`, `select from stet.undone_changes`,
		`select from stet.change_sets`, `select from stet.changes`,
		`insert into stet.undos (at, by, agent_role, since, restored, skipped, failed) values (now(), 'someone', 'stet_md5', now(), 0, 0, 0)`,
		`delete from stet.undo_items`,
	} {
		_, err := rv.Exec(ctx, sql)
		wantCode(t, err, "42501", "the reviewer: "+sql)
	}
	other := otherRole(t, db)
	grant(t, db, other, "grant usage on schema stet to "+other,
		"grant execute on function stet.undo_record(text, timestamptz, timestamptz, text[], text, jsonb) to "+other)
	mustExec(t, owner, "grant "+other+" to "+name)
	mustExec(t, rv, "set role "+other)
	var id int64
	if err := rv.QueryRow(ctx, `select stet.undo_record('stet_md5', $1, null, null, 'made up', $2)`, since,
		`[{"target_table": "deals", "key": "1", "action": "update", "outcome": "restored", "reason": null,
		   "warning": null, "change_ids": [1]}]`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	mustExec(t, rv, "reset role")
	recs := undoRecords(t, pool)
	if len(recs) != 1 || recs[0].ID != id || recs[0].By != name || deref(recs[0].Note) != "made up" || recs[0].Restored != 1 {
		t.Errorf("stet.undos after the direct call as %s: %+v, want one by %s", other, recs, name)
	}
}

// stet.undo_record, which writes with Stet's rights, takes only items of the
// shape stet.undo makes: a JSON array of objects with exactly the seven
// columns of a line, each of its type and, where the spec names them, one of
// its values. Anything else is refused with 22023 and nothing is written. It
// refuses as stet.undo does (ST006), and counts the lines itself.
func TestUndoRecordTakesOnlyItsOwnItems(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	since := dbClock(t, pool)
	rv := undoer(t, db)
	record := func(items string) error {
		_, err := rv.Exec(ctx, `select stet.undo_record('stet_md5', $1, null, null, null, $2)`, since, items)
		return err
	}
	line := func(change string) string {
		m := map[string]any{"target_table": "deals", "key": "1", "action": "update", "outcome": "restored",
			"reason": nil, "warning": nil, "change_ids": []int{1, 2}}
		var patch map[string]any
		if err := json.Unmarshal([]byte(change), &patch); err != nil {
			t.Fatal(err)
		}
		for k, v := range patch {
			if v == "<drop>" {
				delete(m, k)
			} else {
				m[k] = v
			}
		}
		b, err := json.Marshal([]any{m})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, bad := range []string{
		`{}`, `"x"`, `null`, `[1]`, `[null]`, `[[]]`,
		line(`{"extra": 1}`), line(`{"key": "<drop>"}`),
		line(`{"target_table": null}`), line(`{"target_table": 1}`), line(`{"key": 1}`),
		line(`{"action": "upsert"}`), line(`{"action": 1}`), line(`{"outcome": "done"}`), line(`{"outcome": null}`),
		line(`{"reason": ["x"]}`), line(`{"warning": {}}`),
		line(`{"change_ids": "1"}`), line(`{"change_ids": [1.5]}`), line(`{"change_ids": ["1"]}`), line(`{"change_ids": [null]}`),
		line(`{"change_ids": [1e30]}`),
	} {
		wantCode(t, record(bad), "22023", "stet.undo_record with "+bad)
	}
	if recs := undoRecords(t, pool); len(recs) != 0 {
		t.Fatalf("stet.undos after refused records: %+v", recs)
	}
	_, err := rv.Exec(ctx, `select stet.undo_record($1, $2, null, null, null, '[]')`, roleOf(t, rv), since)
	wantCode(t, err, "ST006", "stet.undo_record of the reviewer's own writes")

	items := `[` + strings.Join([]string{
		`{"target_table": "a", "key": "1", "action": "update", "outcome": "restored", "reason": null, "warning": "w", "change_ids": [1]}`,
		`{"target_table": "a", "key": "2", "action": "insert", "outcome": "restored_differs", "reason": null, "warning": null, "change_ids": [2, 3]}`,
		`{"target_table": "a", "key": "3", "action": null, "outcome": "skipped", "reason": "changed_since", "warning": null, "change_ids": [4]}`,
		`{"target_table": "a", "key": null, "action": null, "outcome": "not_captured", "reason": "truncate", "warning": null, "change_ids": null}`,
		`{"target_table": "b", "key": "1", "action": "delete", "outcome": "failed", "reason": "SQLSTATE 23503: x", "warning": null, "change_ids": [9223372036854775807]}`,
	}, ", ") + `]`
	if err := record(items); err != nil {
		t.Fatal(err)
	}
	recs := undoRecords(t, pool)
	if len(recs) != 1 || recs[0].Restored != 2 || recs[0].Skipped != 2 || recs[0].Failed != 1 || recs[0].ItemsCount != 5 {
		t.Fatalf("stet.undos: %+v, want one with 2 restored, 2 skipped, 1 failed and 5 items", recs)
	}
	got := itemsOf(t, pool, recs[0].ID)
	if s := strings.Join(shown(got), "; "); s != `a "1" update restored - w; a "2" insert restored_differs - -; `+
		`a "3" - skipped changed_since -; a - - not_captured truncate -; b "1" delete failed SQLSTATE 23503: x -` ||
		!slices.Equal(got[4].ChangeIDs, []int64{9223372036854775807}) || !slices.Equal(got[1].ChangeIDs, []int64{2, 3}) {
		t.Errorf("stet.undo_items: %s %v", s, got)
	}
}

// The refusals of spec 7 and plan ruling R15 are ST006, and nothing is
// recorded: no agent_role, no since, until not after since, the reviewer's
// own writes, and a transaction that is not READ COMMITTED.
func TestUndoRefusals(t *testing.T) {
	pool, db := captureDB(t)
	rv := undoer(t, db)
	me := roleOf(t, rv)
	since := dbClock(t, pool)
	for _, c := range []struct {
		name      string
		role      any
		since     any
		until     any
		isolation string
	}{
		{"no agent_role", nil, since, nil, ""},
		{"no since", "stet_md5", nil, nil, ""},
		{"until equal to since", "stet_md5", since, since, ""},
		{"until before since", "stet_md5", since, since.Add(-time.Second), ""},
		{"the reviewer's own writes", me, since, nil, ""},
		{"a REPEATABLE READ transaction", "stet_md5", since, nil, "repeatable read"},
		{"a SERIALIZABLE transaction", "stet_md5", since, nil, "serializable"},
	} {
		if c.isolation != "" {
			mustExec(t, rv, "set default_transaction_isolation = '"+c.isolation+"'")
		}
		_, err := tryUndo(rv, c.role, c.since, c.until)
		wantCode(t, err, "ST006", "stet.undo, "+c.name)
		mustExec(t, rv, "reset default_transaction_isolation")
	}
	if recs := undoRecords(t, pool); len(recs) != 0 {
		t.Errorf("stet.undos after the refusals: %+v, want none", recs)
	}
}

// Names that need quoting (a schema and table with capitals and a space, a
// key column that is a reserved word, columns with quotes in their names) do
// not stop any of the three restores.
func TestUndoQuotesNames(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	const table, key = `"Crm"."Deal Notes"`, `"Crm"."Deal Notes"`
	mustExec(t, owner, `create schema "Crm"`,
		`create table "Crm"."Deal Notes" ("order" int primary key, "it's" text, "say ""hi""" text, "Mixed Case" text)`,
		`insert into "Crm"."Deal Notes" values (1, 'one', 'hi', 'X'), (2, 'two', 'hey', 'Y')`)
	installCapture(t, owner, table, key, "order")
	before := contentOf(t, owner, table)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`insert into "Crm"."Deal Notes" values (3, 'it''s here', 'hi', 'Z')`,
		`update "Crm"."Deal Notes" set "it's" = 'changed', "say ""hi""" = 'bye' where "order" = 1`,
		`delete from "Crm"."Deal Notes" where "order" = 2`)

	rv := undoer(t, db, table)
	grant(t, db, roleOf(t, rv), `grant usage on schema "Crm" to `+roleOf(t, rv))
	checkUndo(t, "quoted names", undoOf(t, rv, "stet_md5", since),
		key+` "3" delete restored - -`,
		key+` "1" update restored - -`,
		key+` "2" insert restored - -`)
	checkContent(t, owner, "after the undo", table, before)
}

// Keys that read alike as text, or are written otherwise than their type
// prints them, each come back to their own row: text keys that are the word
// null, empty, or hold a comma, quotes or blanks; uuid, bigint, numeric,
// timestamptz, date, char(5) and enum keys. The values are read with their
// type's input function: the implicit cast from text the table owner made
// for the enum never runs.
func TestUndoRestoresOddKeys(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	runs := plantedCode(t, owner)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create type public.mood as enum ('happy', 'sad', 'meh')`,
		`create function public.to_mood(text) returns public.mood language plpgsql
		 as $$ begin perform public.planted(); return 'happy'; end $$`,
		`create cast (text as public.mood) with function public.to_mood(text) as implicit`)
	keys := []struct {
		table, typ, rows, gone, added string
	}{
		{"k_text", "text", `('null', 'a'), ('', 'b'), ('a,b', 'c'), ('"q"', 'd'), (' padded ', 'e')`, `'a,b'`, `('NULL', 'n')`},
		{"k_uuid", "uuid", `('A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11', 'a'), ('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a12', 'b')`,
			`'A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A12'`, `('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a13', 'n')`},
		{"k_big", "bigint", `(-1, 'a'), (9223372036854775807, 'b')`, `-1`, `(0, 'n')`},
		{"k_num", "numeric", `(1.50, 'a'), (-0.001, 'b')`, `1.50`, `(2.0, 'n')`},
		{"k_ts", "timestamptz", `('2026-11-18 01:02:03.456789+09', 'a'), ('infinity', 'b')`, `'infinity'`, `('2026-01-01 00:00:00+00', 'n')`},
		{"k_date", "date", `('2026-02-28', 'a'), ('2026-03-01', 'b')`, `'2026-02-28'`, `('2027-01-01', 'n')`},
		{"k_char", "char(5)", `('ab', 'a'), ('cd', 'b')`, `'ab'`, `('ef', 'n')`},
		{"k_mood", "public.mood", `('happy', 'a'), ('sad', 'b')`, `'happy'`, `('meh', 'n')`},
	}
	var tables []string
	before := map[string]string{}
	for _, k := range keys {
		mustExec(t, owner, "create table public."+k.table+" (k "+k.typ+" primary key, v text)",
			"insert into public."+k.table+" values "+k.rows)
		installCapture(t, owner, "public."+k.table, k.table, "k")
		tables = append(tables, "public."+k.table)
		before[k.table] = contentOf(t, owner, "public."+k.table)
	}
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	for _, k := range keys {
		mustExec(t, agent,
			"update public."+k.table+" set v = v || '!'",
			"delete from public."+k.table+" where k = "+k.gone,
			"insert into public."+k.table+" values "+k.added)
	}
	ran := runs()

	rv := undoer(t, db, tables...)
	mustExec(t, rv,
		`set timezone = 'Asia/Seoul'`, `set datestyle = 'SQL, DMY'`, `set intervalstyle = 'sql_standard'`,
		`set extra_float_digits = -2`, `set bytea_output = 'escape'`)
	for _, r := range undoOf(t, rv, "stet_md5", since) {
		if r.Outcome != "restored" || r.Reason != "" {
			t.Errorf("%v, want restored", r)
		}
	}
	for _, k := range keys {
		checkContent(t, owner, "after the undo", "public."+k.table, before[k.table])
	}
	if n := runs(); n != ran {
		t.Errorf("the table owner's cast ran %d times during the undo, want none", n-ran)
	}
}

// 5,000 rows one statement of the agent's changed, among 20,000, are restored
// at once, each a line, within 30 seconds.
func TestUndoOfManyKeys(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.many (id int primary key, n int)`,
		`insert into public.many select i, i from generate_series(1, 20000) i`,
		`analyze public.many`)
	installCapture(t, owner, "public.many", "many", "id")
	before := contentOf(t, owner, "public.many")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `update public.many set n = n + 1 where id <= 5000`)

	rv := undoer(t, db, "public.many")
	start := time.Now()
	rows := undoOf(t, rv, "stet_md5", since)
	took := time.Since(start)
	t.Logf("undoing %d keys took %v", len(rows), took)
	if len(rows) != 5000 {
		t.Fatalf("%d lines, want 5000", len(rows))
	}
	for i, r := range rows {
		if r.Action != "update" || r.Outcome != "restored" || r.Key == nil || len(r.ChangeIDs) != 1 {
			t.Fatalf("line %d: %v %v, want an update restored with one change", i, r, r.ChangeIDs)
		}
	}
	checkContent(t, owner, "after the undo", "public.many", before)
	if took > 30*time.Second {
		t.Errorf("the undo took %v, want under 30s", took)
	}
}

// 4,000 keys of one table that the agent changed in 1,600 statements, twenty
// keys to a statement and each key eight times, are restored, and every
// change is marked undone. Recording them does not grow with the number of
// change sets: stet.undo_record, given the same lines again, finds each change
// by its id and is done within three seconds. Pairing every line with every
// set of its table, or every change with every set of the agent's, took it
// many times that.
func TestUndoOfKeysSpreadOverManySets(t *testing.T) {
	const blocks, perBlock, passes = 200, 20, 8
	const keys, sets = blocks * perBlock, blocks * passes
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.spread (id int primary key, n int)`,
		fmt.Sprintf(`insert into public.spread select i, i from generate_series(1, %d) i`, 2*keys),
		`analyze public.spread`)
	installCapture(t, owner, "public.spread", "spread", "id")
	before := contentOf(t, owner, "public.spread")
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	for s := range sets {
		b := s % blocks
		if _, err := agent.Exec(ctx, `update public.spread set n = n + 1 where id > $1 and id <= $2`, b*perBlock, (b+1)*perBlock); err != nil {
			t.Fatal(err)
		}
	}
	if n := countOf(t, pool, `select count(*) from stet.change_sets where target_table = 'spread' and skipped is null`); n != sets {
		t.Fatalf("%d change sets, want %d", n, sets)
	}

	rv := undoer(t, db, "public.spread")
	start := time.Now()
	rows := undoOf(t, rv, "stet_md5", since)
	took := time.Since(start)
	if len(rows) != keys {
		t.Fatalf("%d lines, want %d", len(rows), keys)
	}
	items := make([]map[string]any, len(rows))
	for i, r := range rows {
		if r.Action != "update" || r.Outcome != "restored" || r.Key == nil || len(r.ChangeIDs) != passes {
			t.Fatalf("line %d: %v %v, want an update restored with %d changes", i, r, r.ChangeIDs, passes)
		}
		items[i] = map[string]any{"target_table": r.TargetTable, "key": *r.Key, "action": r.Action, "outcome": r.Outcome,
			"reason": nil, "warning": nil, "change_ids": r.ChangeIDs}
	}
	checkContent(t, owner, "after the undo", "public.spread", before)
	const marked = `select count(*) from stet.undone_changes u join stet.changes c on c.id = u.change_id where u.undo_id = $1`
	recs := undoRecords(t, pool)
	if len(recs) != 1 {
		t.Fatalf("stet.undos: %+v, want one row", recs)
	}
	if n := countOf(t, pool, marked, recs[0].ID); n != keys*passes {
		t.Errorf("the undo marks %d changes undone, want %d", n, keys*passes)
	}

	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	start = time.Now()
	if err := rv.QueryRow(ctx, `select stet.undo_record('stet_md5', $1, null, null, null, $2)`, since, string(encoded)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	recording := time.Since(start)
	t.Logf("undoing %d keys of %d sets took %v; recording the same lines again took %v", keys, sets, took, recording)
	if n := countOf(t, pool, marked, id); n != keys*passes {
		t.Errorf("the record made directly marks %d changes undone, want %d", n, keys*passes)
	}
	if recording > 3*time.Second {
		t.Errorf("recording %d lines over %d sets took %v, want under 3s", keys, sets, recording)
	}
}

// An undo that selects nothing (a table key no change has, a since in the
// future) returns no line, changes nothing and is still recorded, with three
// counts of 0.
func TestUndoOfNothing(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 1)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `update public.deals set n = 10 where id = 1`)
	rv := undoer(t, db, "public.deals")
	checkUndo(t, "a table no change has", undoOf(t, rv, "stet_md5", since, nil, []string{"nope"}))
	checkUndo(t, "since in the future", undoOf(t, rv, "stet_md5", dbClock(t, pool).Add(time.Hour)))
	checkContent(t, owner, "after the undos", "public.deals", "(1,r1,10)")
	recs := undoRecords(t, pool)
	if len(recs) != 2 {
		t.Fatalf("stet.undos: %+v, want two rows", recs)
	}
	for _, r := range recs {
		if r.Restored != 0 || r.Skipped != 0 || r.Failed != 0 || r.ItemsCount != 0 {
			t.Errorf("stet.undos row %+v, want counts of 0 and no item", r)
		}
	}
}

// PurgeChanges deletes old change records and leaves the undo records alone,
// also those that name the changes it deletes. An undo still open in another
// transaction does not hold the purge up.
func TestPurgeLeavesUndoRecords(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 2)
	keyedTable(t, owner, "accounts", 1)
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	mustExec(t, agent, `update public.deals set n = 10 where id = 1`, `delete from public.deals where id = 2`)
	undoOf(t, undoer(t, db, "public.deals"), "stet_md5", since, nil, []string{"deals"})
	mustExec(t, agent, `update public.accounts set n = 10 where id = 1`)
	open, err := undoer(t, db, "public.accounts").Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Rollback(ctx)
	checkUndo(t, "the open undo", undoOf(t, open, "stet_md5", since, nil, []string{"accounts"}),
		`accounts "1" update restored - -`)
	recs := undoRecords(t, pool)
	items := itemsOf(t, pool, recs[0].ID)
	undone := countOf(t, pool, `select count(*) from stet.undone_changes where undo_id = $1`, recs[0].ID)

	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := store.New(pool).PurgeChanges(pctx, time.Microsecond, 1000)
	if err != nil {
		t.Fatalf("PurgeChanges while an undo is open: %v", err)
	}
	if n != 3 || countOf(t, pool, `select count(*) from stet.changes`) != 0 {
		t.Errorf("PurgeChanges deleted %d sets, and %d changes are left; want 3 and none", n,
			countOf(t, pool, `select count(*) from stet.changes`))
	}
	if err := open.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after := undoRecords(t, pool)
	if len(after) != 2 || !reflect.DeepEqual(after[0], recs[0]) || after[1].Restored != 1 {
		t.Errorf("stet.undos after the purge: %+v, want %+v and the open undo's", after, recs)
	}
	if got := itemsOf(t, pool, recs[0].ID); fmt.Sprint(got) != fmt.Sprint(items) ||
		!slices.Equal(got[0].ChangeIDs, items[0].ChangeIDs) {
		t.Errorf("stet.undo_items after the purge: %v, want %v", got, items)
	}
	if n := countOf(t, pool, `select count(*) from stet.undone_changes where undo_id = $1`, recs[0].ID); n != undone || n != 2 {
		t.Errorf("stet.undone_changes holds %d changes of the first undo after the purge, want %d (2)", n, undone)
	}
}

// stet.history finds the undo of a change through the primary key of
// stet.undone_changes: reading the view does not go through every item of an
// undo for each change, and does not read stet.undo_items at all.
func TestHistoryFindsTheUndoThroughAnIndex(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.many (id int primary key, n int)`,
		`insert into public.many select i, i from generate_series(1, 3000) i`)
	installCapture(t, owner, "public.many", "many", "id")
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	for i := 0; i < 3; i++ {
		mustExec(t, agent, fmt.Sprintf(`update public.many set n = n + 1 where id > %d and id <= %d`, i*1000, (i+1)*1000))
	}
	undoOf(t, undoer(t, db, "public.many"), "stet_md5", since, nil, nil, "first")
	mustExec(t, owner, `analyze stet.change_sets`, `analyze stet.changes`, `analyze stet.undos`, `analyze stet.undo_items`)
	var plan string
	if err := owner.QueryRow(ctx, `explain (format json) select change_id, undo_id, undone_at from stet.history`).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, `"Index Name": "undone_changes_pkey"`) || strings.Contains(plan, `"Relation Name": "undo_items"`) {
		t.Errorf("stet.history does not look the undo up by the index:\n%s", plan)
	}
	if n := countOf(t, pool, `select count(*) from stet.history where target_table = 'many' and undo_id is not null`); n != 3000 {
		t.Errorf("stet.history shows %d changes undone, want 3000", n)
	}
}

// The grants schema.sql documents for reviewers, and the REVOKE at its end,
// name exactly the functions of the stet schema whose names begin with undo:
// a helper added later cannot be left out of either.
func TestUndoGrantCommentNamesEveryFunction(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(schema)
	start := strings.Index(sql, "-- 되돌리기의 함수들은 PUBLIC이 부를 수 없다")
	if start < 0 {
		t.Fatal("schema.sql has no grant comment for the undo functions")
	}
	block := sql[start:]
	revoke := strings.Index(block, "REVOKE EXECUTE ON FUNCTION")
	if revoke < 0 {
		t.Fatal("schema.sql has no REVOKE after the grant comment for the undo functions")
	}
	signature := regexp.MustCompile(`stet\.undo\w*\([^)]*\)`)
	canonical := func(what, text string) []string {
		t.Helper()
		var out []string
		for _, s := range signature.FindAllString(text, -1) {
			var c string
			if err := pool.QueryRow(ctx, `select $1::regprocedure::text`, s).Scan(&c); err != nil {
				t.Fatalf("%s names %s: %v", what, s, err)
			}
			out = append(out, c)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	var granted strings.Builder
	for _, m := range regexp.MustCompile(`(?s)GRANT EXECUTE ON FUNCTION (.*?) TO reviewer;`).FindAllStringSubmatch(block[:revoke], -1) {
		granted.WriteString(m[1])
	}
	statement := block[revoke : revoke+strings.Index(block[revoke:], ";")]
	want := undoFunctionsIn(t, db)
	if got := canonical("the grant comment", granted.String()); !slices.Equal(got, want) {
		t.Errorf("the grant comment names\n  %s\nthe catalog has\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if got := canonical("the REVOKE", statement); !slices.Equal(got, want) {
		t.Errorf("the REVOKE names\n  %s\nthe catalog has\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// A row that references the agent's row through a unique key of the same
// table that is NULL in the referencing row's own key, of one column or of
// two, is a child all the same: the agent's row is skipped, referenced, and
// both rows stay. Leaving the agent's row itself out of its children must not
// leave out a row whose key is NULL (three-valued logic).
func TestUndoSkipsRowsReferencedThroughANullableKey(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.nodes (id int primary key, code text unique,
		   parent_code text references public.nodes (code) on delete cascade)`,
		`create table public.twins (id int primary key, x int, y int, px int, py int, unique (x, y),
		   foreign key (px, py) references public.twins (x, y) on delete cascade)`)
	installCapture(t, owner, "public.nodes", "nodes", "id")
	installCapture(t, owner, "public.twins", "twins", "id")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`insert into public.nodes values (1, 'a', null)`,
		`insert into public.twins values (1, 1, 1, null, null)`)
	mustExec(t, writerIn(t, db, "public.nodes", "public.twins"),
		`insert into public.nodes values (2, null, 'a')`,
		`insert into public.twins values (2, 1, null, 1, 1)`)

	rv := undoer(t, db, "public.nodes", "public.twins")
	checkUndo(t, "children whose own key is NULL", undoOf(t, rv, "stet_md5", since),
		`nodes "1" - skipped referenced: public.nodes -`,
		`twins "1" - skipped referenced: public.twins -`)
	checkContent(t, owner, "after the undo", "public.nodes", "(1,a,)\n(2,,a)")
	checkContent(t, owner, "after the undo", "public.twins", "(1,1,1,,)\n(2,1,,1,1)")
}

// A key undo skips leaves no lock behind: once stet.undo has returned, in the
// reviewer's still open transaction, another session can lock the rows of
// keys that were changed_since, no_net_change or referenced (and of one that
// failed), but not the row of a key that was restored. The skip leaves the
// key's block through a private SQLSTATE, ST007; a customer trigger that
// raises that very code makes its key failed, with the trigger's message.
func TestUndoReleasesTheLocksOfSkippedKeys(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 4)
	keyedTable(t, owner, "parents", 1)
	mustExec(t, owner, `create table public.kids (id int primary key, parent int references public.parents on delete cascade)`)
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	mustExec(t, agent,
		`update public.deals set n = 10 where id = 1`,
		`update public.deals set n = 20 where id = 2`,
		`update public.deals set n = 2 where id = 2`,
		`update public.deals set n = 30 where id = 3`,
		`insert into public.parents values (2, 'r2', 2)`,
		`update public.deals set n = 40 where id = 4`)
	mustExec(t, writerIn(t, db, "public.deals", "public.kids"),
		`update public.deals set n = 100 where id = 1`,
		`insert into public.kids values (10, 2)`)
	mustExec(t, owner,
		`create function public.mimic() returns trigger language plpgsql as $$
		 begin raise exception 'deal 4 says ST007' using errcode = 'ST007'; end $$`,
		`create trigger mimic before update on public.deals for each row when (new.id = 4) execute function public.mimic()`)

	rv := undoer(t, db, "public.deals", "public.parents")
	grant(t, db, roleOf(t, rv), "grant select on public.kids to "+roleOf(t, rv))
	tx, err := rv.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	checkUndo(t, "skipped, restored and failed keys", undoOf(t, tx, "stet_md5", since),
		`deals "1" - skipped changed_since -`,
		`deals "2" - skipped no_net_change -`,
		`deals "3" update restored - -`,
		`deals "4" update failed SQLSTATE ST007: deal 4 says ST007 -`,
		`parents "2" - skipped referenced: public.kids -`)
	for _, c := range []struct {
		table string
		id    int
		free  bool
	}{{"deals", 1, true}, {"deals", 2, true}, {"deals", 3, false}, {"deals", 4, true}, {"parents", 2, true}} {
		_, err := owner.Exec(ctx, fmt.Sprintf("select from public.%s where id = %d for update nowait", c.table, c.id))
		if c.free && err != nil {
			t.Errorf("another session cannot lock %s %d while the reviewer's transaction is open: %v", c.table, c.id, err)
		}
		if !c.free {
			wantCode(t, err, "55P03", fmt.Sprintf("another session locking the restored %s %d", c.table, c.id))
		}
	}
}

// Every restored line is true when stet.undo returns. The owner's trigger on
// one table changes, when a later key of it is restored, a row of another
// table that an earlier key restored, and puts back a row an earlier key
// deleted: both earlier lines are restored_differs.
func TestUndoChecksEveryRestoredLineAtTheEnd(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "alpha", 2)
	keyedTable(t, owner, "beta", 2)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`update public.alpha set n = 10 where id = 1`,
		`insert into public.alpha values (3, 'r3', 3)`,
		`update public.beta set n = 20 where id = 1`)
	mustExec(t, owner,
		`create function public.meddle() returns trigger language plpgsql as $$
		 begin
		   update public.alpha set n = 999 where id = 1;
		   insert into public.alpha values (3, 'back', 3);
		   return null;
		 end $$`,
		`create trigger meddle after update on public.beta for each row execute function public.meddle()`)

	rv := undoer(t, db, "public.alpha", "public.beta")
	checkUndo(t, "rows a later restore changed", undoOf(t, rv, "stet_md5", since),
		`alpha "1" update restored_differs - -`,
		`alpha "3" delete restored_differs - -`,
		`beta "1" update restored - -`)
	checkContent(t, owner, "after the undo", "public.alpha", "(1,r1,999)\n(2,r2,2)\n(3,back,3)")
}

// A partition is not a table undo restores (capture on partitions is not
// supported): stet.undo_lock refuses it with 0A000, so the plan skips its
// keys and stet.undo fails them, and nothing changes.
func TestUndoRefusesAPartition(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.parts (id int, n int) partition by range (id)`,
		`create table public.parts_1 partition of public.parts for values from (0) to (100)`,
		`alter table public.parts_1 add primary key (id)`,
		`insert into public.parts values (1, 1)`)
	installCapture(t, owner, "public.parts_1", "parts_1", "id")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `update public.parts_1 set n = 10 where id = 1`)

	rv := undoer(t, db, "public.parts_1")
	refused := `SQLSTATE 0A000: public.parts_1 is a partition of public.parts; undo does not restore partitions`
	checkShown(t, "the plan of a partition", planOf(t, rv, "stet_md5", since), `parts_1 "1" - skipped `+refused+` -`)
	checkUndo(t, "the undo of a partition", undoOf(t, rv, "stet_md5", since), `parts_1 "1" - failed `+refused+` -`)
	checkContent(t, owner, "after the undo", "public.parts_1", "(1,10)")
}

// stet.undo_record records each item as it is given, but marks a change
// undone in stet.history (stet.undone_changes) only when it is a change of
// the agent_role it is given, recorded within since and until, of one of the
// tables it is given, and of the line's own target_table and key: a reviewer
// calling it directly cannot mark another account's change, a change of
// another table or time, one that does not exist, or one under a line of
// another table.
func TestUndoRecordMarksOnlyTheSelectedChangesUndone(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, nil, 10000)
	keyedTable(t, owner, "deals", 3)
	keyedTable(t, owner, "accounts", 1)
	agent := agentIn(t, db)
	mustExec(t, agent, `update public.deals set n = 30 where id = 3`)
	since := dbClock(t, pool)
	mustExec(t, agent, `update public.deals set n = 10 where id = 1`, `update public.accounts set n = 10 where id = 1`,
		`update public.accounts set n = 11 where id = 1`)
	mustExec(t, owner, `update public.deals set n = 20 where id = 2`)
	until := dbClock(t, pool)
	mustExec(t, agent, `update public.deals set n = 11 where id = 1`)
	early, selected, ownerS := changeIDsOf(t, pool, "deals", "3")[0], changeIDsOf(t, pool, "deals", "1")[0],
		changeIDsOf(t, pool, "deals", "2")[0]
	late := changeIDsOf(t, pool, "deals", "1")[1]
	other, misnamed := changeIDsOf(t, pool, "accounts", "1")[0], changeIDsOf(t, pool, "accounts", "1")[1]
	type named struct {
		table, key string
		id         int64
	}
	lines := []named{
		{"deals", "3", early}, {"deals", "1", selected}, {"accounts", "1", other}, {"deals", "2", ownerS},
		{"deals", "1", late}, {"deals", "1", late + 1000}, {"deals", "1", misnamed},
	}
	var ids []int64
	var items []map[string]any
	for _, l := range lines {
		ids = append(ids, l.id)
		items = append(items, map[string]any{"target_table": l.table, "key": l.key, "action": "update", "outcome": "restored",
			"reason": nil, "warning": nil, "change_ids": []int64{l.id}})
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	rv := undoer(t, db)
	for _, c := range []struct {
		tables any
		want   []int64
	}{
		{[]string{"deals"}, []int64{selected}},
		{nil, []int64{selected, other}},
	} {
		var id int64
		if err := rv.QueryRow(ctx, `select stet.undo_record('stet_md5', $1, $2, $3, null, $4)`, since, until, c.tables,
			string(encoded)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		var marked []int64
		if err := pool.QueryRow(ctx, `select coalesce(array_agg(change_id order by change_id), '{}') from stet.undone_changes
			where undo_id = $1`, id).Scan(&marked); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(marked, c.want) {
			t.Errorf("tables %v: stet.undone_changes marks %v undone, want %v (of %v)", c.tables, marked, c.want, ids)
		}
		got := itemsOf(t, pool, id)
		if len(got) != len(ids) {
			t.Fatalf("tables %v: %d items recorded, want %d", c.tables, len(got), len(ids))
		}
		for i, item := range got {
			if !slices.Equal(item.ChangeIDs, []int64{ids[i]}) {
				t.Errorf("tables %v: item %d names %v, want %v as given", c.tables, i, item.ChangeIDs, ids[i])
			}
		}
	}
}

// While the reviewer's transaction holds the row of a parent undo deleted,
// another session's insert of a child that references it waits; once the
// reviewer commits, the insert fails with a foreign-key violation. This is the
// PostgreSQL behaviour the referenced check relies on: no child can appear
// between the check, made under the row's lock, and the reviewer's commit.
func TestUndoHoldsANewChildUntilItCommits(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "parents", 1)
	mustExec(t, owner, `create table public.kids (id int primary key, parent int references public.parents on delete cascade)`)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `insert into public.parents values (2, 'r2', 2)`)
	rv := undoer(t, db, "public.parents")
	grant(t, db, roleOf(t, rv), "grant select on public.kids to "+roleOf(t, rv))
	other := writerIn(t, db, "public.parents", "public.kids")

	tx, err := rv.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	checkUndo(t, "the parent", undoOf(t, tx, "stet_md5", since), `parents "2" delete restored - -`)
	done := make(chan error, 1)
	go func() {
		_, err := other.Exec(ctx, `insert into public.kids values (10, 2)`)
		done <- err
	}()
	if !waitsForALock(t, owner, other.PgConn().PID(), done) {
		t.Fatalf("the child's insert did not wait for the parent's row: %v", <-done)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wantCode(t, <-done, "23503", "the child's insert after the reviewer committed the parent's delete")
	checkContent(t, owner, "the children", "public.kids", "")
	checkContent(t, owner, "the parents", "public.parents", "(1,r1,1)")
}

// A restore statement names its table, and runs against whatever that name
// means when it runs. Another session renames schemas between the moment
// stet.undo_statement makes the statement and the moment it runs, so that the
// name means another table: the statement changes no row of that other table
// (it is limited to relid's tableoid), and stet.undo_outcome, given the
// statement it ran, refuses with 55000, so the key is failed and not recorded
// as undone. Staged by calling, in one READ COMMITTED transaction, the
// helpers stet.undo calls, in its order.
func TestUndoOutcomeRefusesAStatementThatNamedAnotherTable(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create schema crm`, `create schema crm2`,
		`create table crm.deals (id int primary key, note text, n int)`,
		`insert into crm.deals values (1, 'r1', 1)`,
		`create table crm2.deals (id int primary key, note text, n int)`,
		`insert into crm2.deals values (1, 'theirs', 1000)`)
	installCapture(t, owner, "crm.deals", "deals", "id")
	relid := relidOf(t, owner, "crm.deals")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `update crm.deals set n = 10 where id = 1`)
	rv := undoer(t, db, "crm.deals", "crm2.deals")
	grant(t, db, roleOf(t, rv), "grant usage on schema crm, crm2 to "+roleOf(t, rv))

	tx, err := rv.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	sel, err := trySelection(tx, "stet_md5", since, nil, nil)
	if err != nil || len(sel) != 1 {
		t.Fatalf("the selection: %+v, %v", sel, err)
	}
	s := sel[0]
	var action *string
	if err := tx.QueryRow(ctx, `select action from stet.undo_judge($1, $2, $3, $4, $5, $6, $7, true)`,
		s.RelID, s.KeyColumn, s.Key, s.B, s.A, s.KeyChanged, s.Interleaved).Scan(&action); err != nil || deref(action) != "update" {
		t.Fatalf("the judgement: %v, %v", deref(action), err)
	}
	var stmt string
	if err := tx.QueryRow(ctx, `select stet.undo_statement($1, $2, $3, 'update', $4, $5)`, s.RelID, s.KeyColumn, s.Key, s.B, s.A).
		Scan(&stmt); err != nil {
		t.Fatal(err)
	}
	mustExec(t, owner, `alter schema crm rename to crm_old`, `alter schema crm2 rename to crm`)
	tag, err := tx.Exec(ctx, stmt)
	if err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
	if tag.RowsAffected() != 0 {
		t.Errorf("%s changed %d rows of the table the name now means", stmt, tag.RowsAffected())
	}
	_, err = tx.Exec(ctx, `select stet.undo_outcome($1, $2, $3, 'update', $4, $5, $6, $7, null)`,
		relid, s.KeyColumn, s.Key, s.B, s.A, stmt, tag.RowsAffected())
	wantCode(t, err, "55000", "stet.undo_outcome of a statement whose table name was taken by another table")
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	checkContent(t, owner, "the table the name meant", "crm_old.deals", "(1,r1,10)")
	checkContent(t, owner, "the table the name means now", "crm.deals", "(1,theirs,1000)")
}

// stet.undo_outcome checks a key's restore inside the key's block, so that
// what it refuses is rolled back with the restore: B and the row after the
// statement are compared (restored, restored_differs); the statement it is
// given must be the one stet.undo_statement makes now (55000); it must have
// changed at most one row (21000), of relid's table (55000); and the key must
// name one row (21000, a key column that is not unique). Called directly, in
// one READ COMMITTED transaction, as stet.undo calls it.
func TestUndoOutcomeChecksTheRowItRestored(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 2)
	mustExec(t, owner, `create table public.tags (id int primary key, tag text, n int)`,
		`insert into public.tags values (1, 'x', 1), (2, 'x', 2)`)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `update public.deals set n = 10 where id = 1`)
	relid, other, tags := relidOf(t, owner, "public.deals"), relidOf(t, owner, "stet.undos"), relidOf(t, owner, "public.tags")
	rv := undoer(t, db, "public.deals", "public.tags")
	tx, err := rv.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	sel, err := trySelection(tx, "stet_md5", since, nil, nil)
	if err != nil || len(sel) != 1 {
		t.Fatalf("the selection: %+v, %v", sel, err)
	}
	s := sel[0]
	var stmt string
	if err := tx.QueryRow(ctx, `select stet.undo_statement($1, 'id', '1', 'update', $2, $3)`, relid, s.B, s.A).Scan(&stmt); err != nil {
		t.Fatal(err)
	}
	outcome := func(statement any, changed, written any) (string, error) {
		var o string
		mustExec(t, tx, `savepoint outcome`)
		err := tx.QueryRow(ctx, `select stet.undo_outcome($1, 'id', '1', 'update', $2, $3, $4, $5, $6)`,
			relid, s.B, s.A, statement, changed, written).Scan(&o)
		mustExec(t, tx, `rollback to savepoint outcome`)
		return o, err
	}
	if o, err := outcome(stmt, 0, nil); err != nil || o != "restored_differs" {
		t.Errorf("before the statement ran: %q, %v; want restored_differs (the row is still A)", o, err)
	}
	_, err = outcome("UPDATE ONLY public.other r SET n = '1'", 0, nil)
	wantCode(t, err, "55000", "a statement stet.undo_statement does not make")
	_, err = outcome(stmt, 2, relid)
	wantCode(t, err, "21000", "a statement that changed two rows")
	_, err = outcome(stmt, 1, other)
	wantCode(t, err, "55000", "a statement that wrote to another table")
	mustExec(t, tx, stmt)
	if o, err := outcome(stmt, 1, relid); err != nil || o != "restored" {
		t.Errorf("after the statement ran: %q, %v; want restored", o, err)
	}
	// A table whose capture key column, tag, is not unique: two rows have the key x.
	b, a := map[string]any{"id": "1", "tag": "x", "n": "1"}, map[string]any{"id": "1", "tag": "x", "n": "10"}
	var tagged string
	if err := tx.QueryRow(ctx, `select stet.undo_statement($1, 'tag', 'x', 'update', $2, $3)`, tags, b, a).Scan(&tagged); err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `select stet.undo_outcome($1, 'tag', 'x', 'update', $2, $3, $4, 1, $1)`, tags, b, a, tagged)
	wantCode(t, err, "21000", "a key that names two rows")
}

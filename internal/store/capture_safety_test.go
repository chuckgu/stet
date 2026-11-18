package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/store"
)

// Capture must never fail the agent's write, nor send it a message (spec 4,
// "캡처는 쓰기를 실패시키지 않는다"); it records at most 32 times in a
// transaction ("한 트랜잭션의 상한"); and what it records cannot be changed.
// Each test that breaks Stet's tables does so in a database of its own
// (captureDB).

// noBadNames makes stet.changes refuse a line whose row after has the name
// 'bad', so that capturing a write of such a row fails.
const noBadNames = `alter table stet.changes add constraint no_bad_names check (after->>'name' is distinct from 'bad')`

// wantSkipped checks that sets holds one set, recorded for query, with no
// line, skipped as skipped, and detail beginning with code ("" for none).
func wantSkipped(t *testing.T, changes []capturedChange, query, skipped, code string) {
	t.Helper()
	sets := setsOf(changes, query)
	if len(sets) != 1 {
		t.Errorf("%s: %d sets, want one %s", query, len(sets), skipped)
		return
	}
	s := sets[0]
	detailOK := s.Detail == nil
	if code != "" {
		detailOK = s.Detail != nil && strings.HasPrefix(*s.Detail, code+": ")
	}
	if deref(s.Skipped) != skipped || !detailOK || s.ChangeID != nil {
		t.Errorf("%s: skipped %v, detail %v, line %v; want %s, detail %q, no line",
			query, deref(s.Skipped), deref(s.Detail), deref(s.ChangeID), skipped, code)
	}
}

// countOf runs a query that returns one number.
func countOf(t *testing.T, conn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// When recording fails, the agent's write still succeeds: its rows are
// there, and capture leaves one capture_failed set with the SQLSTATE and no
// line for that statement. The next write is recorded as usual.
func TestCaptureFailureKeepsTheWrite(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	dealsTable(t, owner)
	mustExec(t, pool, noBadNames)
	agent := agentIn(t, db)
	const (
		failing = `insert into public.deals values (4, 'bad', 40), (5, 'fine', 50)`
		next    = `update public.deals set amount = 11 where id = 1`
	)
	mustExec(t, agent, failing, next)

	if n := countOf(t, owner, `select count(*) from public.deals where id in (4, 5)`); n != 2 {
		t.Errorf("%d of the two rows the write inserted are there", n)
	}
	changes := changesOf(t, pool, "deals")
	wantSkipped(t, changes, failing, "capture_failed", "23514")
	for _, s := range setsOf(changes, failing) {
		if s.Op != "insert" || deref(s.Rows) != int64(2) || s.AgentRole != "stet_md5" || s.LoginRole != "stet_md5" {
			t.Errorf("the capture_failed set: op %q, rows %v, agent_role %q, login_role %q; want insert, 2, stet_md5, stet_md5",
				s.Op, deref(s.Rows), s.AgentRole, s.LoginRole)
		}
	}
	checkLines(t, changes, next, changeLine{Op: "update", Key: "1", Before: deal("1", "Acme", "10"), After: deal("1", "Acme", "11")})
}

// Failing to read stet.capture_settings is a capture failure like any other:
// the write succeeds and a capture_failed set remains, the agent's own, also
// when Stet's account writes as the agent.
func TestCaptureFailureWhileReadingSettings(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	dealsTable(t, owner)
	stetMayActAsTheAgent(t, db)
	mustExec(t, pool, `alter table stet.capture_settings rename column max_rows to max_rows_renamed`)
	const (
		byAgent = `update public.deals set amount = 11 where id = 1`
		byStet  = `update public.deals set amount = 22 where id = 2`
	)
	mustExec(t, agentIn(t, db), byAgent)
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		mustExec(t, tx, `set local role stet_md5`, byStet)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	changes := changesOf(t, pool, "deals")
	for _, c := range []struct{ sql, login string }{{byAgent, "stet_md5"}, {byStet, "stet_scram"}} {
		wantSkipped(t, changes, c.sql, "capture_failed", "42703")
		for _, s := range setsOf(changes, c.sql) {
			if s.AgentRole != "stet_md5" || s.LoginRole != c.login {
				t.Errorf("%s: agent_role %q, login_role %q; want stet_md5, %s", c.sql, s.AgentRole, s.LoginRole, c.login)
			}
		}
	}
	if n := countOf(t, owner, `select count(*) from public.deals where (id, amount) in ((1, 11), (2, 22))`); n != 2 {
		t.Errorf("%d of the two writes took effect", n)
	}
}

// When even the capture_failed set cannot be written, the write still
// succeeds and nothing is recorded (the failure goes to the server's log).
func TestCaptureThatCannotRecordItsFailure(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	dealsTable(t, owner)
	mustExec(t, pool, noBadNames,
		`alter table stet.change_sets add constraint no_failures check (skipped is distinct from 'capture_failed')`)
	mustExec(t, agentIn(t, db), `insert into public.deals values (4, 'bad', 40)`)

	if n := countOf(t, owner, `select count(*) from public.deals where id = 4`); n != 1 {
		t.Errorf("the row the write inserted is missing")
	}
	if n := countOf(t, pool, `select (select count(*) from stet.change_sets) + (select count(*) from stet.changes)`); n != 0 {
		t.Errorf("%d rows in stet.change_sets and stet.changes, want none", n)
	}
}

// agentHearing connects as the agent with client_min_messages at level and
// returns the connection and the notices it has received since.
func agentHearing(t *testing.T, db, level string) (*pgx.Conn, func() []*pgconn.Notice) {
	t.Helper()
	cfg, err := pgx.ParseConfig("postgres://stet_md5:md5-pw@" + testAddr(t) + "/" + db + "?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	var notices []*pgconn.Notice
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { notices = append(notices, n) }
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	mustExec(t, conn, "set client_min_messages = "+level)
	notices = nil
	return conn, func() []*pgconn.Notice { return notices }
}

// A failing capture sends the client nothing: no notice at all with
// client_min_messages = log, and none that mentions stet at debug5, when the
// capture_failed set is written and when even that fails and the function
// logs instead.
func TestCaptureSendsTheClientNothing(t *testing.T) {
	pool, db := captureDB(t)
	dealsTable(t, superuserIn(t, db))
	logs, heardAtLog := agentHearing(t, db, "log")
	debugs, heardAtDebug := agentHearing(t, db, "debug5")
	write := func(conn *pgx.Conn, id int) {
		t.Helper()
		mustExec(t, conn, fmt.Sprintf(`insert into public.deals values (%d, 'bad', 0)`, id))
	}

	mustExec(t, pool, noBadNames)
	write(logs, 10)
	write(debugs, 11)
	mustExec(t, pool, `alter table stet.change_sets add constraint no_failures check (skipped is distinct from 'capture_failed') not valid`)
	write(logs, 12)
	write(debugs, 13)

	// The failures happened as intended: two capture_failed sets, then none.
	if n := countOf(t, pool, `select count(*) from stet.change_sets where skipped = 'capture_failed'`); n != 2 {
		t.Errorf("%d capture_failed sets, want 2 (the writes of rows 10 and 11)", n)
	}
	for _, n := range heardAtLog() {
		t.Errorf("at log the client heard %s: %s (%s)", n.Severity, n.Message, n.Where)
	}
	heard := heardAtDebug()
	if len(heard) == 0 {
		t.Error("at debug5 the client heard nothing at all; the test does not show what it means to")
	}
	for _, n := range heard {
		if strings.Contains(strings.ToLower(fmt.Sprintf("%+v", *n)), "stet") {
			t.Errorf("at debug5 the client heard %s: %s (%s)", n.Severity, n.Message, n.Where)
		}
	}
}

// Capture does not swallow a statement timeout or a cancel: a write whose
// capture waits past statement_timeout for a lock on stet.changes fails with
// 57014. A capture that gives up on that lock under lock_timeout is a capture
// failure like any other: the write succeeds with a capture_failed set.
func TestCaptureLeavesTimeoutsAlone(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	dealsTable(t, owner)
	locker, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Release()
	lock, err := locker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	mustExec(t, lock, `lock table stet.changes in share mode`)
	agent := agentIn(t, db)

	const (
		timedOut = `update public.deals set amount = 98 where id = 1`
		gaveUp   = `update public.deals set amount = 99 where id = 2`
	)
	mustExec(t, agent, `set statement_timeout = '300ms'`)
	_, err = agent.Exec(ctx, timedOut)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Errorf("%s with statement_timeout: err = %v, want SQLSTATE 57014", timedOut, err)
	}
	mustExec(t, agent, `reset statement_timeout`, `set lock_timeout = '300ms'`, gaveUp)
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	if n := countOf(t, owner, `select count(*) from public.deals where (id, amount) in ((1, 10), (2, 99))`); n != 2 {
		t.Errorf("want deal 1 as it was and deal 2 written; %d of the two hold", n)
	}
	changes := changesOf(t, pool, "deals")
	if sets := setsOf(changes, timedOut); len(sets) != 0 {
		t.Errorf("the timed out write left %d sets", len(sets))
	}
	wantSkipped(t, changes, gaveUp, "capture_failed", "55P03")
}

// A statement that changes more rows than max_rows leaves one too_many_rows
// set with the count and no line; one within it leaves its values. TRUNCATE
// leaves one truncate set without a row count.
func TestCaptureCapsRowsPerStatement(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	mustExec(t, owner,
		`create table public.items (id int primary key, v int)`,
		`insert into public.items select g, 0 from generate_series(1, 5) g`)
	installCapture(t, owner, "public.items", "items", "id")
	setCapture(t, pool, nil, 3)
	const (
		over     = `update public.items set v = 1 where id <= 4`
		within   = `update public.items set v = 2 where id <= 3`
		truncate = `truncate public.items`
	)
	mustExec(t, agentIn(t, db), over, within, truncate)

	changes := changesOf(t, pool, "items")
	wantSkipped(t, changes, over, "too_many_rows", "")
	for _, s := range setsOf(changes, over) {
		if s.Op != "update" || deref(s.Rows) != int64(4) {
			t.Errorf("%s: op %q, rows %v; want update, 4", over, s.Op, deref(s.Rows))
		}
	}
	item := func(id, v string) map[string]any { return image("id", id, "v", v) }
	checkLines(t, changes, within,
		changeLine{Op: "update", Key: "1", Before: item("1", "1"), After: item("1", "2")},
		changeLine{Op: "update", Key: "2", Before: item("2", "1"), After: item("2", "2")},
		changeLine{Op: "update", Key: "3", Before: item("3", "1"), After: item("3", "2")})
	wantSkipped(t, changes, truncate, "truncate", "")
	for _, s := range setsOf(changes, truncate) {
		if s.Op != "truncate" || s.Rows != nil {
			t.Errorf("%s: op %q, rows %v; want truncate, NULL", truncate, s.Op, deref(s.Rows))
		}
	}
}

// writeEach runs the n statements stmt(1) to stmt(n) in one transaction on
// conn, and returns them and the transaction's id.
func writeEach(t *testing.T, conn *pgx.Conn, n int, stmt func(i int) string) ([]string, int64) {
	t.Helper()
	ctx := context.Background()
	var sqls []string
	var txid int64
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		for i := 1; i <= n; i++ {
			sql := stmt(i)
			if _, err := tx.Exec(ctx, sql); err != nil {
				return fmt.Errorf("%s: %w", sql, err)
			}
			sqls = append(sqls, sql)
		}
		return tx.QueryRow(ctx, `select txid_current()`).Scan(&txid)
	})
	if err != nil {
		t.Fatal(err)
	}
	return sqls, txid
}

// writersOf is how many transactions and subtransactions wrote the sets and
// lines recorded in the transaction txid: the distinct xmin among them.
func writersOf(t *testing.T, pool *pgxpool.Pool, txid int64) int64 {
	t.Helper()
	return countOf(t, pool, `select count(distinct x) from (
		select s.xmin::text as x from stet.change_sets s where s.txid = $1
		union all
		select c.xmin::text from stet.changes c join stet.change_sets s on s.id = c.set_id where s.txid = $1) w`, txid)
}

// In one transaction capture records values for the first 31 writes it
// records anything for, a too_many_statements set for the 32nd, and nothing
// after; the subtransactions that wrote what it recorded are at most 32. A
// failed capture counts as one, and a 32nd capture that fails leaves the
// too_many_statements set too, with the failure as its detail. The next
// transaction counts from 0. Writes by a session that is not captured record
// nothing.
func TestCaptureCapsStatementsPerTransaction(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	mustExec(t, owner,
		`create table public.items (id int primary key, v text)`,
		`insert into public.items select g, 'v' from generate_series(1, 40) g`,
		// Every capture of a write to public.broken fails before it writes
		// anything: its trigger's key argument names no column.
		`create table public.broken (id int primary key)`,
		`create trigger stet_capture_ins after insert on public.broken referencing new table as stet_new
		 for each statement execute function stet.capture('broken', 'nope')`)
	installCapture(t, owner, "public.items", "items", "id")
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	agent := agentIn(t, db)
	update := func(i int, v string) string {
		return fmt.Sprintf(`update public.items set v = '%s' where id = %d`, v, i)
	}

	// check checks what the statements of the transaction txid left. want
	// says it for the ith (from 1): the value before and after its one line,
	// or the skipped of its one set without lines and the SQLSTATE its detail
	// begins with, or nothing.
	type outcome struct{ before, after, skipped, code string }
	check := func(what string, sqls []string, txid int64, want func(i int) *outcome) {
		t.Helper()
		changes := changesWhere(t, pool, `s.txid = $1`, txid)
		for i, sql := range sqls {
			w := want(i + 1)
			sets := setsOf(changes, sql)
			switch {
			case w == nil:
				if len(sets) != 0 {
					t.Errorf("%s: write %d left %d sets, want none", what, i+1, len(sets))
				}
			case w.skipped != "":
				wantSkipped(t, changes, sql, w.skipped, w.code)
			default:
				checkLines(t, changes, sql, changeLine{Op: "update", Key: fmt.Sprint(i + 1),
					Before: image("id", fmt.Sprint(i+1), "v", w.before), After: image("id", fmt.Sprint(i+1), "v", w.after)})
			}
		}
		if n := countOf(t, pool, `select count(*) from stet.change_sets where txid = $1`, txid); n != 32 {
			t.Errorf("%s: %d sets in the transaction, want 32", what, n)
		}
		if n := writersOf(t, pool, txid); n > 32 {
			t.Errorf("%s: what capture recorded was written by %d subtransactions, want at most 32", what, n)
		}
	}

	ok := func(i int) string { return fmt.Sprintf("ok %d", i) }
	sqls, txid := writeEach(t, agent, 40, func(i int) string { return update(i, ok(i)) })
	check("40 writes", sqls, txid, func(i int) *outcome {
		switch {
		case i <= 31:
			return &outcome{before: "v", after: ok(i)}
		case i == 32:
			return &outcome{skipped: "too_many_statements"}
		}
		return nil
	})

	// Every fourth capture fails as it writes its line (a CHECK on
	// stet.changes), and the 32nd fails before it writes anything. The
	// transaction counts from 0 again: its first writes are recorded.
	mustExec(t, pool, `alter table stet.changes add constraint no_failing check (after->>'v' not like 'fail%')`)
	mixed := func(i int) string {
		if i%4 == 0 {
			return fmt.Sprintf("fail %d", i)
		}
		return fmt.Sprintf("again %d", i)
	}
	sqls, txid = writeEach(t, agent, 40, func(i int) string {
		if i == 32 {
			return `insert into public.broken values (32)`
		}
		return update(i, mixed(i))
	})
	check("40 writes, every fourth capture and the 32nd failing", sqls, txid, func(i int) *outcome {
		switch {
		case i <= 31 && i%4 == 0:
			return &outcome{skipped: "capture_failed", code: "23514"}
		case i <= 31:
			return &outcome{before: ok(i), after: mixed(i)}
		case i == 32:
			return &outcome{skipped: "too_many_statements", code: "22023"}
		}
		return nil
	})

	// The table owner is not captured: 40 writes record nothing.
	_, txid = writeEach(t, owner, 40, func(i int) string { return update(i, fmt.Sprintf("owner %d", i)) })
	if n := countOf(t, pool, `select count(*) from stet.change_sets where txid = $1`, txid); n != 0 {
		t.Errorf("40 writes of a session that is not captured left %d sets", n)
	}
}

// The count is stet.captures, for the transaction: it counts each capture
// that wrote, a failed one too, and nothing that wrote nothing (a write of no
// row, a session that is not captured). What a savepoint rolls back is no
// longer counted, as the subtransactions it rolled back no longer hold an id.
func TestCaptureCountsOnlyWhatItWrites(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	dealsTable(t, owner)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, pool, noBadNames)
	count := func(tx pgx.Tx) string {
		t.Helper()
		var n *string
		if err := tx.QueryRow(ctx, `select current_setting('stet.captures', true)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(deref(n))
	}

	err := pgx.BeginFunc(ctx, agentIn(t, db), func(tx pgx.Tx) error {
		for _, c := range []struct{ sql, want string }{
			{`update public.deals set amount = 0 where id = -1`, "NULL"},
			{`delete from public.deals where id = -1`, "NULL"},
			{`update public.deals set amount = 11 where id = 1`, "1"},
			{`insert into public.deals values (4, 'bad', 40)`, "2"},
			{`savepoint s`, "2"},
			{`update public.deals set amount = 22 where id = 2`, "3"},
			{`insert into public.deals values (5, 'bad', 50)`, "4"},
			{`rollback to savepoint s`, "2"},
			{`update public.deals set amount = 33 where id = 3`, "3"},
		} {
			mustExec(t, tx, c.sql)
			if got := count(tx); got != c.want {
				t.Errorf("after %s stet.captures is %s, want %s", c.sql, got, c.want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = pgx.BeginFunc(ctx, owner, func(tx pgx.Tx) error {
		mustExec(t, tx, `update public.deals set amount = amount + 1`)
		if got := count(tx); got != "NULL" {
			t.Errorf("after a write of a session that is not captured stet.captures is %s, want NULL", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// otherRole creates a role that cannot log in, dropped with what it owns and
// its grants in database when the test ends.
func otherRole(t *testing.T, database string) string {
	t.Helper()
	ctx := context.Background()
	su, inDB := superuserIn(t, "stet_test"), superuserIn(t, database)
	name := fmt.Sprintf("stet_other_%d", time.Now().UnixNano())
	mustExec(t, su, "create role "+name)
	t.Cleanup(func() {
		if _, err := inDB.Exec(ctx, "drop owned by "+name); err != nil {
			t.Errorf("drop owned by %s: %v", name, err)
		}
		if _, err := su.Exec(ctx, "drop role "+name); err != nil {
			t.Errorf("drop role %s: %v", name, err)
		}
	})
	return name
}

// Nothing the agent can set in its session breaks capture or changes whose
// write it is: a counter that is not a number, one too large for a number,
// one at or just below the cap (32 writes counted only by the setting would
// otherwise stop capture), a held write that is not a uuid. Each write
// succeeds and leaves its values. A write after SET ROLE is still stet_md5's.
func TestAgentCannotBreakCaptureThroughSessionSettings(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	dealsTable(t, owner)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	other := otherRole(t, db)
	mustExec(t, owner, "grant "+other+" to stet_md5", "grant select, update on public.deals to "+other)

	for i, c := range []struct {
		name string
		set  []string
	}{
		{"a counter that is not a number", []string{`set stet.captures = 'x'`}},
		{"a counter too large for a number", []string{`set stet.captures = '999999999999'`}},
		{"a counter at the cap", []string{`set stet.captures = '32'`}},
		{"a counter just below the cap", []string{`set stet.captures = '31'`}},
		{"a counter at the cap for the transaction", []string{`begin`, `set local stet.captures = '32'`}},
		{"a held write that is not a uuid", []string{`set stet.held_write = 'not-a-uuid'`}},
		{"a held write that is a uuid", []string{`set stet.held_write = '` + store.NewID() + `'`}},
		{"another role", []string{"set role " + other}},
	} {
		t.Run(c.name, func(t *testing.T) {
			agent := agentIn(t, db)
			mustExec(t, agent, c.set...)
			sql := fmt.Sprintf(`update public.deals set amount = %d where id = 1`, 100+i)
			if _, err := agent.Exec(ctx, sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
			if agent.PgConn().TxStatus() == 'T' {
				mustExec(t, agent, `commit`)
			}
			sets := setsOf(changesOf(t, pool, "deals"), sql)
			if len(sets) != 1 {
				t.Fatalf("%d sets, want 1", len(sets))
			}
			s := sets[0]
			if s.Skipped != nil || s.ChangeID == nil || s.AgentRole != "stet_md5" || s.LoginRole != "stet_md5" || s.HeldWrite != nil {
				t.Errorf("skipped %v, line %v, agent_role %q, login_role %q, held_write %v; want values, stet_md5, stet_md5, NULL",
					deref(s.Skipped), deref(s.ChangeID), s.AgentRole, s.LoginRole, deref(s.HeldWrite))
			}
		})
	}
}

// Stet's account cannot UPDATE or TRUNCATE what capture recorded (ST004); it
// can DELETE a set, which takes its lines with it.
func TestChangeRecordsAreImmutable(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	dealsTable(t, superuserIn(t, db))
	mustExec(t, agentIn(t, db), `update public.deals set amount = amount + 1`)
	before := countOf(t, pool, `select count(*) from stet.changes`)
	if before != 3 {
		t.Fatalf("%d lines recorded, want 3", before)
	}

	for _, c := range []struct{ sql, table, op string }{
		{`update stet.change_sets set query = 'forged'`, "change_sets", "UPDATE"},
		{`update stet.change_sets set query = 'forged' where false`, "change_sets", "UPDATE"},
		{`update stet.changes set after = '{}'`, "changes", "UPDATE"},
		{`truncate stet.changes`, "changes", "TRUNCATE"},
		{`truncate stet.change_sets cascade`, "change_sets", "TRUNCATE"},
	} {
		_, err := pool.Exec(ctx, c.sql)
		want := fmt.Sprintf("stet.%s: what capture recorded cannot change; %s is refused, only DELETE is allowed", c.table, c.op)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "ST004" || pgErr.Message != want {
			t.Errorf("%s: err = %v, want SQLSTATE ST004 %q", c.sql, err, want)
		}
	}
	if n := countOf(t, pool, `select count(*) from stet.changes c join stet.change_sets s on s.id = c.set_id
		where s.query <> 'forged' and c.after <> '{}'`); n != before {
		t.Errorf("%d of %d lines unchanged", n, before)
	}

	mustExec(t, pool, `delete from stet.change_sets`)
	if n := countOf(t, pool, `select count(*) from stet.changes`); n != 0 {
		t.Errorf("%d lines left after their sets were deleted", n)
	}
}

// The agent can neither read nor write what capture recorded, nor call
// stet.capture(): it has no right on the stet schema.
func TestAgentCannotReachTheRecords(t *testing.T) {
	_, db := captureDB(t)
	ctx := context.Background()
	agent := agentIn(t, db)
	for _, sql := range []string{
		`select * from stet.change_sets`,
		`select * from stet.changes`,
		`insert into stet.changes (set_id, key) values (1, 'k')`,
		`update stet.change_sets set query = 'forged'`,
		`update stet.changes set after = '{}'`,
		`delete from stet.change_sets`,
		`delete from stet.changes`,
		`select stet.capture()`,
	} {
		_, err := agent.Exec(ctx, sql)
		want42501(t, err, sql)
	}
}

// The owner of a table cannot install capture without the two grants spec 4
// names, USAGE on the stet schema and EXECUTE on stet.capture(). With them it
// can, and still cannot call the function for a value: it runs only as a
// trigger.
func TestTableOwnerNeedsGrantsToInstall(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	role := loginRole(t, "owner")
	su := superuserIn(t, db)
	t.Cleanup(func() {
		if err := underMigrateLock(context.Background(), su, "drop owned by "+role); err != nil {
			t.Errorf("in %s: %v", db, err)
		}
	})
	mustExec(t, su, "create schema crm authorization "+role)
	owner := connectAs(t, db, role)
	mustExec(t, owner, `create table crm.deals (id int primary key, name text)`)
	const install = `create trigger stet_capture_ins after insert on crm.deals referencing new table as stet_new
		for each statement execute function stet.capture('deals', 'id')`

	_, err := owner.Exec(ctx, install)
	want42501(t, err, "without grants")
	grant(t, db, role, "grant usage on schema stet to "+role)
	_, err = owner.Exec(ctx, install)
	want42501(t, err, "with USAGE on stet only")
	grant(t, db, role, "grant execute on function stet.capture() to "+role)
	mustExec(t, owner, install)

	var v any
	err = owner.QueryRow(ctx, `select stet.capture()`).Scan(&v)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message, "trigger") {
		t.Errorf("select stet.capture(): value %v, err = %v; want the error that it is called only as a trigger", v, err)
	}
	// The trigger it installed captures (no settings row: every session).
	const insert = `insert into crm.deals values (1, 'Acme')`
	mustExec(t, owner, insert)
	checkLines(t, changesOf(t, pool, "deals"), insert, changeLine{Op: "insert", Key: "1", After: image("id", "1", "name", "Acme")})
}

// A transaction that captured a write holds weak locks on Stet's capture
// tables until it ends. Migrate, running schema.sql again because the marker
// differs, must not wait for it: on tables that exist schema.sql takes no
// lock that conflicts (spec 11, "스키마의 잠금").
func TestMigrateDoesNotWaitForAnOpenCapture(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	dealsTable(t, superuserIn(t, db))
	agent := agentIn(t, db)
	tx, err := agent.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	const open = `update public.deals set amount = 11 where id = 1`
	mustExec(t, tx, open)
	if n := countOf(t, pool, `select count(distinct relation) from pg_locks where pid = $1 and relation in
		('stet.change_sets'::regclass, 'stet.changes'::regclass, 'stet.capture_settings'::regclass)`, agent.PgConn().PID()); n != 3 {
		t.Fatalf("the open transaction locks %d of the three capture tables, want 3", n)
	}
	mustExec(t, pool, `update stet.schema_version set sha256 = 'an older schema'`)

	cfg, err := pgxpool.ParseConfig("postgres://stet_scram:scram-pw@" + testAddr(t) + "/" + db + "?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "2s"
	migrating, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer migrating.Close()
	start := time.Now()
	if err := store.New(migrating).Migrate(ctx); err != nil {
		t.Errorf("migrate while a capture is open: %v (after %v)", err, time.Since(start))
	}
	if got, want := schemaMarker(t, pool), schemaHash(t); got != want {
		t.Errorf("schema marker = %q, want %q: schema.sql did not run again", got, want)
	}
	if n := countOf(t, pool, `select count(*) from pg_trigger where tgrelid in ('stet.change_sets'::regclass, 'stet.changes'::regclass)
		and tgname in ('change_sets_guard', 'changes_guard')`); n != 2 {
		t.Errorf("%d guard triggers on the capture tables, want 2", n)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	checkLines(t, changesOf(t, pool, "deals"), open, changeLine{Op: "update", Key: "1", Before: deal("1", "Acme", "10"), After: deal("1", "Acme", "11")})
}

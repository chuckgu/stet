package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// These tests start Stet with an undo clause (spec-v0.2-undo 9, "Stet이 시작할
// 때"). Each runs in a database of its own, where stet.capture_settings and
// the change records are its own.

// captureRow is the row of stet.capture_settings, with its xmin, which
// changes only when the row is written. ok is false when there is no row.
type captureRow struct {
	Roles     []string
	NullRoles bool
	MaxRows   int64
	XMin      string
	ok        bool
}

func captureSettingsIn(t *testing.T, conn *pgx.Conn) captureRow {
	t.Helper()
	var r captureRow
	err := conn.QueryRow(context.Background(), `select roles, roles is null, max_rows, xmin::text from stet.capture_settings`).
		Scan(&r.Roles, &r.NullRoles, &r.MaxRows, &r.XMin)
	if errors.Is(err, pgx.ErrNoRows) {
		return r
	}
	if err != nil {
		t.Fatal(err)
	}
	r.ok = true
	return r
}

// recordSet puts a change set recorded age (an SQL interval) ago, with one
// line, into stet.change_sets as conn's role, Stet's account, and returns
// its id.
func recordSet(t *testing.T, conn *pgx.Conn, age string) int64 {
	t.Helper()
	var id int64
	if err := conn.QueryRow(context.Background(), `
		with s as (
		  insert into stet.change_sets (at, stmt_at, pid, login_role, agent_role, target_table, key_column, relid, op)
		  values (clock_timestamp() - $1::interval, clock_timestamp() - $1::interval, 0, 'stet_md5', 'stet_md5',
		          'deals', 'id', 0, 'update')
		  returning id
		), c as (
		  insert into stet.changes (set_id, key, before, after) select id, '1', '{}', '{}' from s
		)
		select id from s`, age).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// setLeft reports whether change set id, and its line, are still recorded.
func setLeft(t *testing.T, conn *pgx.Conn, id int64) (set, line bool) {
	t.Helper()
	if err := conn.QueryRow(context.Background(), `select exists (select from stet.change_sets where id = $1),
		exists (select from stet.changes where set_id = $1)`, id).Scan(&set, &line); err != nil {
		t.Fatal(err)
	}
	return set, line
}

// warnings are the log's warning lines.
func warnings(log string) []string {
	var out []string
	for line := range strings.Lines(log) {
		if strings.Contains(line, "level=WARN") {
			out = append(out, line)
		}
	}
	return out
}

// undoYAML is a config for stet_md5 in mode %[4]s over database %[2]s of the
// test server %[1]s, with table %[3]s gated and in undo.tables, which keeps
// at most 7 rows a statement. A test adds further keys after it.
const undoYAML = `mode: %[4]s
agents: {roles: [stet_md5]}
listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/%[2]s?sslmode=disable
model: {adapter: none, on_failure: pass}
tables:
  %[3]s: {pk: id, send_fields: [name]}
undo:
  max_rows: 7
  tables:
    %[3]s:
`

// Started with undo, in either mode, Stet makes stet.capture_settings say
// agents.roles and undo.max_rows; no agents.roles is NULL, every session.
func TestRunSyncsCaptureSettings(t *testing.T) {
	pg := testPG(t)
	for _, tc := range []struct {
		name, mode, agents string
		roles              []string
	}{
		{"shadow", "shadow", "agents: {roles: [stet_md5]}\n", []string{"stet_md5"}},
		{"enforce", "enforce", "agents: {roles: [stet_md5]}\n", []string{"stet_md5"}},
		{"shadow, every session", "shadow", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, table, admin := agentTable(t, pg, 1)
			stetPoolIn(t, pg, db)
			mustExec(t, admin, `insert into stet.capture_settings (roles, max_rows) values ('{other}', 5)`)
			cfg := strings.Replace(undoYAML, "agents: {roles: [stet_md5]}\n", tc.agents, 1)
			r := start(t, parseConfig(t, cfg, pg, db, table, tc.mode))
			waitListening(t, r)

			s := captureSettingsIn(t, admin)
			if !s.ok || !slices.Equal(s.Roles, tc.roles) || s.NullRoles != (tc.roles == nil) || s.MaxRows != 7 {
				t.Errorf("stet.capture_settings: present %v, roles %q (NULL %v), max_rows %d; want roles %q (NULL %v), 7; log:\n%s",
					s.ok, s.Roles, s.NullRoles, s.MaxRows, tc.roles, tc.roles == nil, r.log)
			}
			if err := r.stop(t); err != nil {
				t.Fatalf("run = %v, want nil", err)
			}
		})
	}
}

// Stet warns at start about the undo tables whose writes are not captured,
// and starts: one without the capture triggers, one that is not there, and a
// table that references a captured one with ON DELETE CASCADE but is not in
// undo.tables. A table with its triggers in place is not warned about.
func TestRunWarnsAboutUndoTables(t *testing.T) {
	pg := testPG(t)
	db, table, admin, _ := capturedTable(t, pg, 1)
	mustExec(t, admin, "create table public.undo_bare (id int primary key)")
	mustExec(t, admin, "create table public.undo_child (id int primary key, parent int references "+table+" on delete cascade)")
	r := start(t, parseConfig(t, undoYAML+`    public.undo_bare: {pk: id}
    public.undo_gone: {pk: id}
`, pg, db, table, "shadow"))
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")
	if _, err := agent.Exec(context.Background(), "select 1"); err != nil {
		t.Fatalf("through Stet: %v; log:\n%s", err, r.log)
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}

	got := warnings(r.log.String())
	for _, parts := range [][]string{
		{"undo.tables.public.undo_bare:", "stet_capture_ins is missing", "stet undo-sql"},
		{"undo.tables.public.undo_gone:", "finds no table"},
		{"undo.tables." + table + ":", "public.undo_child", "ON DELETE"},
	} {
		if _, ok := warningWith(got, parts...); !ok {
			t.Errorf("no warning says %q; log:\n%s", parts, r.log)
		}
	}
	if w, ok := warningWith(got, "undo.tables."+table+":", "capture triggers"); ok {
		t.Errorf("a warning about the triggers of %s, which has them: %s", table, w)
	}
}

// Spec 9: with undo.tables and no gated table, Stet opens its pool,
// brings its schema up and syncs the settings, and passes sessions through
// as it does without tables: no gate, and none of the start's checks, which
// could not read in time here and would stop enforce.
func TestRunWithUndoButNoGatedTables(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	was := checkTimeout
	t.Cleanup(func() { checkTimeout = was })
	checkTimeout = time.Nanosecond

	for _, mode := range []string{"shadow", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			db := cleanDatabase(t, pg)
			admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
			table := freshTable(t, admin, 1)
			mustExec(t, admin, "grant all on "+table+" to stet_md5")
			var stet *string
			if err := admin.QueryRow(ctx, `select to_regnamespace('stet')::text`).Scan(&stet); err != nil || stet != nil {
				t.Fatalf("the stet schema is %v (%v) before Stet started, want none", orNull(stet), err)
			}

			r := start(t, parseConfig(t, `mode: %[3]s
agents: {roles: [stet_md5]}
listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/%[2]s?sslmode=disable
undo:
  max_rows: 9
  tables:
    public.%[4]s: {pk: id}
`, pg, db, mode, table))
			waitListening(t, r)
			if s := captureSettingsIn(t, admin); !s.ok || !slices.Equal(s.Roles, []string{"stet_md5"}) || s.MaxRows != 9 {
				t.Errorf("stet.capture_settings: present %v, roles %q, max_rows %d; want [stet_md5], 9; log:\n%s",
					s.ok, s.Roles, s.MaxRows, r.log)
			}

			agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")
			if _, err := agent.Exec(ctx, "update "+table+" set name = 'through' where id = 1"); err != nil {
				t.Fatalf("an agent's write through Stet: %v; log:\n%s", err, r.log)
			}
			if err := r.stop(t); err != nil {
				t.Fatalf("run = %v, want nil", err)
			}
			var decisions int
			if err := admin.QueryRow(ctx, "select count(*) from stet.decisions").Scan(&decisions); err != nil {
				t.Fatal(err)
			}
			if decisions != 0 {
				t.Errorf("%d decisions recorded with no gated table, want none", decisions)
			}
			if _, ok := warningWith(warnings(r.log.String()), "no gated tables"); ok != (mode == "enforce") {
				t.Errorf("warns that there are no gated tables: %v, want %v; log:\n%s", ok, mode == "enforce", r.log)
			}
		})
	}
}

// In enforce mode the settings are synced before the executor starts: a held
// write approved before Stet started, which the executor applies at once, is
// captured under the new settings. The sync is held up by a lock on the row
// for up to 100ms, within the 200ms lock_timeout of Stet's pool, so that an
// executor started first would apply the write meanwhile, under the old row,
// which captures no write of stet_md5.
func TestRunSyncsCaptureSettingsBeforeApplyingHeldWrites(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db, table, admin, pool := capturedTable(t, pg, 1)
	mustExec(t, admin, `insert into stet.capture_settings (roles, max_rows) values ('{other}', 5)`)
	id := approvedWrite(t, pool, db, table, "update "+table+" set name = 'applied' where id = 1")

	su := connectTo(t, pg, db, "postgres", "postgres-pw")
	lock, err := connectTo(t, pg, db, "postgres", "postgres-pw").Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err := lock.Exec(ctx, `select from stet.capture_settings for update`); err != nil {
		t.Fatal(err)
	}
	r := start(t, parseConfig(t, undoYAML, pg, db, table, "enforce"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := su.QueryRow(ctx, `select exists (select from pg_stat_activity where datname = $1
			and usename = 'stet_scram' and wait_event_type = 'Lock' and query like '%capture_settings%')`, db).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Stet's settings sync never waited for the row; log:\n%s", r.log)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for held := time.Now(); time.Since(held) < 100*time.Millisecond && statusOf(t, admin, id) != "applied"; {
		time.Sleep(5 * time.Millisecond)
	}
	if err := lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	waitListening(t, r)
	waitStatus(t, admin, id, "applied", 5*time.Second, r.log)

	if s := captureSettingsIn(t, admin); !s.ok || !slices.Equal(s.Roles, []string{"stet_md5"}) || s.MaxRows != 7 {
		t.Fatalf("stet.capture_settings: present %v, roles %q, max_rows %d; want [stet_md5], 7; log:\n%s",
			s.ok, s.Roles, s.MaxRows, r.log)
	}
	var captured int
	if err := admin.QueryRow(ctx, `select count(*) from stet.change_sets where held_write = $1`, id).Scan(&captured); err != nil {
		t.Fatal(err)
	}
	if captured != 1 {
		t.Errorf("%d change sets record the approved write, want 1: it ran under the settings before the sync; log:\n%s",
			captured, r.log)
	}
}

// Without undo, Stet neither writes stet.capture_settings, nor puts a row in,
// nor deletes change records, in either mode.
func TestRunWithoutUndoLeavesCaptureSettingsAlone(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db, table, admin := agentTable(t, pg, 1)
	stetPoolIn(t, pg, db)
	old := recordSet(t, admin, "60 days")
	served := func(r *running) {
		t.Helper()
		agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")
		if _, err := agent.Exec(ctx, "update "+table+" set name = 'through' where id = 1"); err != nil {
			t.Fatalf("an agent's write through Stet: %v; log:\n%s", err, r.log)
		}
		if err := r.stop(t); err != nil {
			t.Fatalf("run = %v, want nil", err)
		}
	}

	served(start(t, parseConfig(t, enforceYAML, pg, db, table)))
	if s := captureSettingsIn(t, admin); s.ok {
		t.Errorf("Stet without undo put a row in stet.capture_settings: %+v", s)
	}

	mustExec(t, admin, `insert into stet.capture_settings (roles, max_rows) values ('{other}', 5)`)
	before := captureSettingsIn(t, admin)
	served(start(t, parseConfig(t, historyShadowYAML, pg, db, table)))
	if after := captureSettingsIn(t, admin); after.XMin != before.XMin {
		t.Errorf("Stet without undo wrote stet.capture_settings: %+v, was %+v", after, before)
	}
	if set, line := setLeft(t, admin, old); !set || !line {
		t.Errorf("Stet without undo deleted an old change set (set left %v, line left %v)", set, line)
	}
}

// Started with undo, Stet deletes the change sets older than undo.keep, with
// their lines, and keeps the newer ones, with or without gated tables (spec 9).
func TestRunPurgesOldChangeRecords(t *testing.T) {
	pg := testPG(t)
	for _, tc := range []struct{ name, tables string }{
		{"gated", "tables:\n  %[3]s: {pk: id, send_fields: [name]}\nmodel: {adapter: none, on_failure: pass}\n"},
		{"undo only", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, table, admin := agentTable(t, pg, 1)
			stetPoolIn(t, pg, db)
			old, recent := recordSet(t, admin, "2 hours"), recordSet(t, admin, "50 minutes")
			r := start(t, parseConfig(t, `mode: shadow
listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/%[2]s?sslmode=disable
`+tc.tables+`undo:
  keep: 1h
  tables:
    public.%[3]s: {pk: id}
`, pg, db, table))
			deadline := time.Now().Add(5 * time.Second)
			for {
				set, line := setLeft(t, admin, old)
				if !set && !line {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the change set recorded 2 hours ago is still there (set %v, line %v) with keep 1h; log:\n%s",
						set, line, r.log)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := r.stop(t); err != nil {
				t.Fatalf("run = %v, want nil", err)
			}
			if set, line := setLeft(t, admin, recent); !set || !line {
				t.Errorf("the change set recorded 50 minutes ago was deleted (set left %v, line left %v)", set, line)
			}
			if _, ok := warningWith(warnings(r.log.String()), "not purged"); ok {
				t.Errorf("the purge failed; log:\n%s", r.log)
			}
		})
	}
}

// What Stet cannot do or check of undo at start is a warning that says so,
// and Stet starts: here the time for both is up at once.
func TestRunStartsWhenTheUndoStepsFail(t *testing.T) {
	pg := testPG(t)
	db, table, admin := agentTable(t, pg, 1)
	stetPoolIn(t, pg, db)
	was := undoStartTimeout
	t.Cleanup(func() { undoStartTimeout = was })
	undoStartTimeout = time.Nanosecond

	r := start(t, parseConfig(t, undoYAML, pg, db, table, "enforce"))
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")
	if _, err := agent.Exec(context.Background(), "update "+table+" set name = 'through' where id = 1"); err != nil {
		t.Fatalf("an agent's write through Stet: %v; log:\n%s", err, r.log)
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
	got := warnings(r.log.String())
	for _, parts := range [][]string{
		{"could not set stet.capture_settings", "agents.roles", "undo.max_rows"},
		{"could not check undo.tables"},
	} {
		if _, ok := warningWith(got, parts...); !ok {
			t.Errorf("no warning says %q; log:\n%s", parts, r.log)
		}
	}
	if s := captureSettingsIn(t, admin); s.ok {
		t.Errorf("stet.capture_settings has a row, %+v, though the time to set it was up", s)
	}
}

// undoFunctionsIn are the functions a reviewer needs EXECUTE on to plan and
// undo in the database conn is in: every function of the stet schema whose
// name begins with undo, as the catalog names them (the store package's
// tests check the list against the grants schema.sql documents).
func undoFunctionsIn(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), `select p.oid::regprocedure::text from pg_proc p
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

// The whole flow of spec-v0.2-undo 3, with Stet in shadow mode and no model:
// an agent inserts, updates and deletes through Stet on a table with capture
// installed; a reviewer reads stet.history and sees each change linked to the
// decision on its write, plans the undo and undoes it in its own READ
// COMMITTED transaction, commits, and the table holds what it held before the
// agent wrote; stet.history then shows each change undone.
func TestUndoEndToEnd(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	reviewer := newReviewer(t, pg)
	db, table, admin, pool := capturedTable(t, pg, 3)
	content := func() string {
		t.Helper()
		var s string
		if err := admin.QueryRow(ctx, `select string_agg(r::text, ' ' order by r.id) from `+table+` r`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := content()
	r := start(t, parseConfig(t, undoYAML, pg, db, table, "shadow"))
	waitListening(t, r)
	var since time.Time
	if err := admin.QueryRow(ctx, `select clock_timestamp()`).Scan(&since); err != nil {
		t.Fatal(err)
	}
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")
	writes := []string{
		"insert into " + table + " values (4, 'new')",
		"update " + table + " set name = 'changed' where id = 1",
		"delete from " + table + " where id = 2",
	}
	for _, w := range writes {
		mustExec(t, agent, w)
	}
	ds := decisionsOn(t, pool, table, len(writes), r.log)
	if err := r.stop(t); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}

	in := connectTo(t, pg, db, "postgres", "postgres-pw")
	for _, sql := range []string{
		"grant usage on schema stet to " + reviewer,
		"grant select on stet.history to " + reviewer,
		"grant execute on function " + strings.Join(undoFunctionsIn(t, in), ", ") + " to " + reviewer,
		"grant select, insert, update, delete on " + table + " to " + reviewer,
	} {
		mustExec(t, in, sql)
	}
	rv := connectTo(t, pg, db, reviewer, reviewerPassword)
	type line struct {
		Key        string
		Op         string
		Query      string
		DecisionID *string
		UndoID     *int64
	}
	history := func() []line {
		t.Helper()
		rows, err := rv.Query(ctx, `select key, op, query, decision_id::text, undo_id from stet.history
			where target_table = $1 order by change_id`, table)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowToStructByPos[line])
		if err != nil {
			t.Fatal(err)
		}
		return lines
	}
	decided := map[string]string{}
	for _, d := range ds {
		decided[d.ID] = d.Statement
	}
	seen := history()
	if len(seen) != len(writes) {
		t.Fatalf("stet.history shows %+v, want a change for each of the %d writes", seen, len(writes))
	}
	for i, l := range seen {
		if l.Query != writes[i] || l.DecisionID == nil || decided[*l.DecisionID] != writes[i] || l.UndoID != nil {
			t.Errorf("stet.history line %d: %+v, want the change of %q linked to its decision, not undone; decisions %v",
				i, l, writes[i], decided)
		}
	}

	shown := func(rows pgx.Rows, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		lines, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
			var key, action *string
			var outcome string
			err := r.Scan(&key, &action, &outcome)
			return fmt.Sprintf("%s %s %s", orNull(key), orNull(action), outcome), err
		})
		if err != nil {
			t.Fatal(err)
		}
		return lines
	}
	tx, err := rv.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	plan := shown(tx.Query(ctx, `select key, action, outcome from stet.undo_plan('stet_md5', $1)`, since))
	if want := []string{"4 delete would_restore", "1 update would_restore", "2 insert would_restore"}; !slices.Equal(plan, want) {
		t.Errorf("stet.undo_plan says %q, want %q", plan, want)
	}
	undone := shown(tx.Query(ctx, `select key, action, outcome from stet.undo('stet_md5', $1, note => 'end to end')`, since))
	if want := []string{"4 delete restored", "1 update restored", "2 insert restored"}; !slices.Equal(undone, want) {
		t.Errorf("stet.undo says %q, want %q", undone, want)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if after := content(); after != before {
		t.Errorf("after the undo the table holds %q, want %q", after, before)
	}
	for i, l := range history() {
		if l.UndoID == nil {
			t.Errorf("stet.history line %d shows no undo after the commit: %+v", i, l)
		}
	}
}

// noUndoYAML is a config without an undo clause over database %[2]s of the
// test server %[1]s, in mode %[4]s with table %[3]s gated.
const noUndoYAML = `mode: %[4]s
agents: {roles: [stet_md5]}
listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/%[2]s?sslmode=disable
model: {adapter: none, on_failure: pass}
tables:
  %[3]s: {pk: id, send_fields: [name]}
`

// Started without an undo clause while capture triggers are still installed,
// Stet says so in one warning that names the tables and both remedies, and
// starts, in either mode: capture goes on without it, and nothing purges what
// it records. A database without such triggers gets no such warning.
func TestRunWarnsOfCaptureLeftWithoutAnUndoClause(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	for _, mode := range []string{"shadow", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			db, table, admin, _ := capturedTable(t, pg, 1)
			mustExec(t, admin, "create table public.also_captured (id int primary key)")
			captureOn(t, admin, "public.also_captured", "public.also_captured", "id")
			r := start(t, parseConfig(t, noUndoYAML, pg, db, table, mode))
			agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")
			if _, err := agent.Exec(ctx, "update "+table+" set name = 'through' where id = 1"); err != nil {
				t.Fatalf("an agent's write through Stet: %v; log:\n%s", err, r.log)
			}
			if err := r.stop(t); err != nil {
				t.Fatalf("run = %v, want nil", err)
			}
			w, ok := warningWith(warnings(r.log.String()), "no undo clause", "public."+table, "public.also_captured",
				"still recorded", "nothing deletes old change records", "put the undo clause back", "stet undo-sql -drop")
			if !ok {
				t.Errorf("no warning of capture left without an undo clause; log:\n%s", r.log)
			}
			if n := strings.Count(r.log.String(), "no undo clause"); n != 1 {
				t.Errorf("%d log lines say there is no undo clause, want one: %s", n, w)
			}
		})
	}
	t.Run("no triggers", func(t *testing.T) {
		db, table, _ := agentTable(t, pg, 1)
		stetPoolIn(t, pg, db)
		r := start(t, parseConfig(t, noUndoYAML, pg, db, table, "shadow"))
		waitListening(t, r)
		if err := r.stop(t); err != nil {
			t.Fatalf("run = %v, want nil", err)
		}
		if w, ok := warningWith(warnings(r.log.String()), "undo clause"); ok {
			t.Errorf("a warning about the undo clause with no capture trigger installed: %s", w)
		}
	})
}

// When Stet cannot read whether capture triggers are left (here its time is
// up at once), that is a warning that says so, and Stet starts. With neither
// gated tables nor an undo clause Stet opens no pool and reads nothing: there
// is no such warning, with triggers installed too.
func TestRunStartsWhenTheCheckForLeftCaptureFails(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db, table, _, _ := capturedTable(t, pg, 1)
	was := undoStartTimeout
	t.Cleanup(func() { undoStartTimeout = was })
	undoStartTimeout = time.Nanosecond

	r := start(t, parseConfig(t, noUndoYAML, pg, db, table, "enforce"))
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")
	if _, err := agent.Exec(ctx, "update "+table+" set name = 'through' where id = 1"); err != nil {
		t.Fatalf("an agent's write through Stet: %v; log:\n%s", err, r.log)
	}
	if err := r.stop(t); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
	if _, ok := warningWith(warnings(r.log.String()), "could not check whether capture triggers", "no undo clause"); !ok {
		t.Errorf("no warning that the check could not be made; log:\n%s", r.log)
	}

	undoStartTimeout = was
	r = start(t, parseConfig(t, `mode: shadow
listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/%[2]s?sslmode=disable
`, pg, db))
	waitListening(t, r)
	if err := r.stop(t); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
	if w, ok := warningWith(warnings(r.log.String()), "undo clause"); ok {
		t.Errorf("a warning about the undo clause from a Stet that opens no pool: %s", w)
	}
}

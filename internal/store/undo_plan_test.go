package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests read stet.undo_plan (spec-v0.2-undo 7): what restoring an
// agent's recorded changes would do, without changing anything. Each runs in
// a database of its own (captureDB). The agent's writes are real writes by
// stet_md5 through real capture triggers, and the plan is read by a reviewer
// role the test creates (undoReviewer). The superuser stands for the table
// owner and for the other accounts whose writes the plan must not undo.

// planFunctions are the functions a role needs EXECUTE on to call
// stet.undo_plan: the plan and the helpers it calls (spec 7, 권한).
var planFunctions = []string{
	"stet.undo_plan(text, timestamptz, timestamptz, text[])",
	"stet.undo_selection(text, timestamptz, timestamptz, text[])",
	"stet.undo_row(oid, text, text, boolean)",
	"stet.undo_judge(oid, text, text, jsonb, jsonb, boolean, boolean, boolean)",
	"stet.undo_warning(oid, text, text[])",
	"stet.undo_refuse(text, timestamptz, timestamptz)",
}

// undoReviewer creates a login role with what a reviewer needs to call
// stet.undo_plan in db: USAGE on the stet schema, EXECUTE on planFunctions,
// and SELECT on each of tables (SQL names). It returns a connection as that
// role. The role has no right on Stet's tables.
func undoReviewer(t *testing.T, db string, tables ...string) *pgx.Conn {
	t.Helper()
	name := loginRole(t, "reviewer")
	grants := []string{
		"grant usage on schema stet to " + name,
		"grant execute on function " + strings.Join(planFunctions, ", ") + " to " + name,
	}
	for _, table := range tables {
		grants = append(grants, "grant select on "+table+" to "+name)
	}
	grant(t, db, name, grants...)
	return connectAs(t, db, name)
}

// roleOf is the role conn is logged in as.
func roleOf(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	var name string
	if err := conn.QueryRow(context.Background(), "select session_user").Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

// undoRow is one line of stet.undo_plan's result; stet.undo returns the same
// columns. Key is nil for a line without a key (not_captured). Action, Reason
// and Warning are "" for NULL: none of them is ever the empty text.
type undoRow struct {
	TargetTable string
	Key         *string
	Action      string
	Outcome     string
	Reason      string
	Warning     string
	ChangeIDs   []int64
}

// String shows every column but change_ids, "-" for NULL, the key quoted.
func (r undoRow) String() string {
	or := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	key := "-"
	if r.Key != nil {
		key = strconv.Quote(*r.Key)
	}
	return strings.Join([]string{r.TargetTable, key, or(r.Action), r.Outcome, or(r.Reason), or(r.Warning)}, " ")
}

// shown is rows as String shows them, in their order.
func shown(rows []undoRow) []string {
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = r.String()
	}
	return lines
}

// queryer is what a plan is read on: a connection, a pool or a transaction.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// call is the SQL that calls function with n parameters, $1 to $n.
func call(function string, n int) string {
	params := make([]string, n)
	for i := range params {
		params[i] = fmt.Sprintf("$%d", i+1)
	}
	return function + "(" + strings.Join(params, ", ") + ")"
}

// tryPlan reads stet.undo_plan(role, args...) on conn: args are since, and
// then until and tables if given. A nil role or argument is NULL.
func tryPlan(conn queryer, role any, args ...any) ([]undoRow, error) {
	rows, err := conn.Query(context.Background(),
		"select target_table, key, action, outcome, reason, warning, change_ids from "+call("stet.undo_plan", 1+len(args)),
		append([]any{role}, args...)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (undoRow, error) {
		var u undoRow
		var action, reason, warning *string
		err := r.Scan(&u.TargetTable, &u.Key, &action, &u.Outcome, &reason, &warning, &u.ChangeIDs)
		for _, f := range []struct {
			from *string
			to   *string
		}{{action, &u.Action}, {reason, &u.Reason}, {warning, &u.Warning}} {
			if f.from != nil {
				*f.to = *f.from
			}
		}
		return u, err
	})
}

// planOf is stet.undo_plan(role, args...) read on conn (see tryPlan). It
// stops the test if the plan fails.
func planOf(t *testing.T, conn queryer, role string, args ...any) []undoRow {
	t.Helper()
	rows, err := tryPlan(conn, role, args...)
	if err != nil {
		t.Fatalf("stet.undo_plan(%q, %v): %v", role, args, err)
	}
	return rows
}

// selected is one row of stet.undo_selection.
type selected struct {
	TargetTable string
	RelID       uint32
	KeyColumn   *string
	Key         *string
	B, A        map[string]any
	ChangeIDs   []int64
	FirstChange *int64
	KeyChanged  *bool
	Interleaved *bool
	Skipped     *string
	AlsoChanged []string
	Withheld    *string
}

// trySelection reads stet.undo_selection(role, since, until, tables) on conn,
// in the order it returns its rows.
func trySelection(conn queryer, role, since, until, tables any) ([]selected, error) {
	rows, err := conn.Query(context.Background(), `
		select target_table, relid, key_column, key, b, a, change_ids, first_change, key_changed, interleaved,
		       skipped, also_changed, withheld
		from stet.undo_selection($1, $2, $3, $4) with ordinality order by ordinality`, role, since, until, tables)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[selected])
}

// checkWithheld checks that stet.undo_selection, read on conn with any of
// several arguments, shows the agent's changes to table only as one line,
// without a key, a key column, a value, a change or a flag, with reason as
// its withheld.
func checkWithheld(t *testing.T, conn queryer, table, reason string, since time.Time) {
	t.Helper()
	for _, args := range [][3]any{
		{since, nil, nil},
		{since, nil, []string{table}},
		{time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), nil, nil},
		{since, since.Add(24 * time.Hour), []string{table, "nope"}},
	} {
		sel, err := trySelection(conn, "stet_md5", args[0], args[1], args[2])
		if err != nil {
			t.Fatal(err)
		}
		var lines []selected
		for _, s := range sel {
			if s.TargetTable == table {
				lines = append(lines, s)
			}
		}
		if len(lines) != 1 {
			t.Errorf("stet.undo_selection%v shows %d lines of %s, want one: %+v", args, len(lines), table, lines)
			continue
		}
		l := lines[0]
		if l.KeyColumn != nil || l.Key != nil || l.B != nil || l.A != nil || l.ChangeIDs != nil || l.FirstChange != nil ||
			l.KeyChanged != nil || l.Interleaved != nil || l.Skipped != nil || l.AlsoChanged != nil || deref(l.Withheld) != reason {
			t.Errorf("stet.undo_selection%v shows %s as %+v (withheld %v), want nothing but withheld %q",
				args, table, l, deref(l.Withheld), reason)
		}
	}
}

// changeIDsOf are the ids of the changes capture recorded for key of the
// configuration's table key target, in order.
func changeIDsOf(t *testing.T, pool *pgxpool.Pool, target, key string) []int64 {
	t.Helper()
	var ids []int64
	if err := pool.QueryRow(context.Background(), `
		select coalesce(array_agg(c.id order by c.id), '{}')
		from stet.changes c join stet.change_sets s on s.id = c.set_id
		where s.target_table = $1 and c.key = $2`, target, key).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	return ids
}

// keyedTable creates public.<name> (id int primary key, note text, n int)
// with the rows 1 to rows (note 'r<i>', n i), and installs capture on it with
// the configuration key name.
func keyedTable(t *testing.T, owner *pgx.Conn, name string, rows int) {
	t.Helper()
	mustExec(t, owner,
		"create table public."+name+" (id int primary key, note text, n int)",
		fmt.Sprintf("insert into public.%s select i, 'r' || i, i from generate_series(1, %d) i", name, rows))
	installCapture(t, owner, "public."+name, name, "id")
}

// wantCode checks that err carries SQLSTATE code.
func wantCode(t *testing.T, err error, code, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: err = %v, want SQLSTATE %s", what, err, code)
	}
}

// checkShown checks that rows show as want, in order.
func checkShown(t *testing.T, what string, rows []undoRow, want ...string) {
	t.Helper()
	if got := shown(rows); !slices.Equal(got, want) {
		t.Errorf("%s: the plan says\n  %s\nwant\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// checkShownInAnyOrder checks that rows show as want, in any order.
func checkShownInAnyOrder(t *testing.T, what string, rows []undoRow, want ...string) {
	t.Helper()
	got := shown(rows)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s: the plan says\n  %s\nwant\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// A row the agent inserted is to be deleted, one it deleted inserted, one it
// updated updated, each would_restore. Each line names the key's changes. The
// lines come by target_table, then by the key's first change.
func TestPlanNamesTheActionPerKey(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 4)
	keyedTable(t, owner, "accounts", 1)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`update public.deals set note = 'changed' where id = 3`,
		`delete from public.deals where id = 1`,
		`insert into public.deals values (5, 'new', 5)`,
		`update public.accounts set n = 10 where id = 1`)

	got := planOf(t, undoReviewer(t, db, "public.deals", "public.accounts"), "stet_md5", since)
	line := func(table, key, action string) undoRow {
		ids := changeIDsOf(t, pool, table, key)
		if len(ids) != 1 {
			t.Fatalf("%s %s: %d changes recorded, want 1", table, key, len(ids))
		}
		return undoRow{TargetTable: table, Key: &key, Action: action, Outcome: "would_restore", ChangeIDs: ids}
	}
	want := []undoRow{line("accounts", "1", "update"), line("deals", "3", "update"), line("deals", "1", "insert"), line("deals", "5", "delete")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the plan says\n%v\nwant\n%v", got, want)
	}
}

// The changes of a key fold into one line, from the row before the first to
// the row after the last: inserted and updated is to be deleted, updated and
// deleted inserted. Inserted and deleted, and updated and put back as it was,
// is no_net_change.
func TestPlanFoldsSeveralChangesOfAKey(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 5)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`update public.deals set n = 11 where id = 1`,
		`insert into public.deals values (6, 'r6', 6)`,
		`update public.deals set n = 22 where id = 2`,
		`insert into public.deals values (7, 'r7', 7)`,
		`update public.deals set n = 33 where id = 3`,
		`update public.deals set n = 111 where id = 1`,
		`update public.deals set n = 66 where id = 6`,
		`delete from public.deals where id = 2`,
		`delete from public.deals where id = 7`,
		`update public.deals set n = 3 where id = 3`,
		`update public.deals set note = 'x' where id = 1`)

	got := planOf(t, undoReviewer(t, db, "public.deals"), "stet_md5", since)
	checkShown(t, "folded", got,
		`deals "1" update would_restore - -`,
		`deals "6" delete would_restore - -`,
		`deals "2" insert would_restore - -`,
		`deals "7" - skipped no_net_change -`,
		`deals "3" - skipped no_net_change -`)
	for _, r := range got {
		want := changeIDsOf(t, pool, "deals", *r.Key)
		if n := map[string]int{"1": 3}[*r.Key]; len(want) != max(n, 2) || !slices.Equal(r.ChangeIDs, want) {
			t.Errorf("key %s: change_ids %v, want %v", *r.Key, r.ChangeIDs, want)
		}
	}
}

// The plan takes the changes of the role given, recorded from since up to
// but not including until (NULL: up to now), of the tables given (NULL: all).
func TestPlanSelectsByRoleTimeAndTable(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, nil, 10000)
	keyedTable(t, owner, "deals", 3)
	keyedTable(t, owner, "accounts", 1)
	agent := agentIn(t, db)
	mustExec(t, agent, `update public.deals set n = 10 where id = 1`)
	since := dbClock(t, pool)
	mustExec(t, agent, `update public.deals set n = 20 where id = 2`)
	mustExec(t, owner, `update public.deals set n = 30 where id = 3`)
	mustExec(t, agent, `update public.accounts set n = 10 where id = 1`)
	until := dbClock(t, pool)
	mustExec(t, agent, `update public.deals set n = 11 where id = 1`)
	rv := undoReviewer(t, db, "public.deals", "public.accounts")

	checkShown(t, "since to until", planOf(t, rv, "stet_md5", since, until),
		`accounts "1" update would_restore - -`,
		`deals "2" update would_restore - -`)
	checkShown(t, "since to until, deals only", planOf(t, rv, "stet_md5", since, until, []string{"deals"}),
		`deals "2" update would_restore - -`)
	checkShown(t, "since to now", planOf(t, rv, "stet_md5", since, nil),
		`accounts "1" update would_restore - -`,
		`deals "2" update would_restore - -`,
		`deals "1" update would_restore - -`)
	checkShown(t, "the other account's", planOf(t, rv, "postgres", since, until),
		`deals "3" update would_restore - -`)
	checkShown(t, "a table not in the configuration", planOf(t, rv, "stet_md5", since, nil, []string{"nope"}))
	// The bounds, exactly: a change recorded at until is left out, one recorded at since is in.
	var last time.Time
	if err := pool.QueryRow(context.Background(), `select max(at) from stet.change_sets`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	checkShown(t, "until the last change", planOf(t, rv, "stet_md5", since, last),
		`accounts "1" update would_restore - -`,
		`deals "2" update would_restore - -`)
	checkShown(t, "since the last change", planOf(t, rv, "stet_md5", last),
		`deals "1" update would_restore - -`)
	checkShown(t, "since in the future", planOf(t, rv, "stet_md5", dbClock(t, pool).Add(time.Hour)))
}

// A row someone else changed after the agent is changed_since: updated,
// deleted, or inserted again where the agent deleted it, also under a key
// its type takes as the same but written differently (1.5 where the agent
// deleted 1.50). A row someone else changed between two of the agent's
// changes is interleaved. A row someone else changed and put back exactly as
// it was is restored: nothing is lost.
func TestPlanSkipsWhatOthersChanged(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 5)
	mustExec(t, owner, `create table public.prices (k numeric primary key, v text)`, `insert into public.prices values (1.50, 'a')`)
	installCapture(t, owner, "public.prices", "prices", "k")
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	mustExec(t, agent, `update public.deals set n = 10 where id = 1`)
	mustExec(t, owner, `update public.deals set n = 100 where id = 1`)
	mustExec(t, agent, `update public.deals set n = 20 where id = 2`)
	mustExec(t, owner, `delete from public.deals where id = 2`)
	mustExec(t, agent, `delete from public.deals where id = 3`)
	mustExec(t, owner, `insert into public.deals values (3, 'r3', 3)`)
	mustExec(t, agent, `update public.deals set n = 40 where id = 4`)
	mustExec(t, owner, `update public.deals set n = 400 where id = 4`)
	mustExec(t, agent, `update public.deals set n = 41 where id = 4`)
	mustExec(t, agent, `update public.deals set n = 50 where id = 5`)
	mustExec(t, owner, `update public.deals set n = 500 where id = 5`, `update public.deals set n = 50 where id = 5`)
	mustExec(t, agent, `delete from public.prices where k = 1.50`)
	mustExec(t, owner, `insert into public.prices values (1.5, 'a')`)

	checkShown(t, "others' changes", planOf(t, undoReviewer(t, db, "public.deals", "public.prices"), "stet_md5", since),
		`deals "1" - skipped changed_since -`,
		`deals "2" - skipped changed_since -`,
		`deals "3" - skipped changed_since -`,
		`deals "4" - skipped interleaved -`,
		`deals "5" update would_restore - -`,
		`prices "1.50" - skipped changed_since -`)
}

// A key column that is not unique (capture does not check it; stet undo-sql
// does) gives no row to compare when the key names several: that key is
// skipped with cardinality_violation. A key that names one row is planned.
func TestPlanSkipsAKeyThatIsNotUnique(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.tags (id int primary key, tag text, n int)`,
		`insert into public.tags values (1, 'x', 1), (2, 'x', 2), (3, 'y', 3)`)
	installCapture(t, owner, "public.tags", "tags", "tag")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `update public.tags set n = 10 where id = 1`, `update public.tags set n = 30 where id = 3`)

	checkShown(t, "a key column that is not unique", planOf(t, undoReviewer(t, db, "public.tags"), "stet_md5", since),
		`tags "x" - skipped SQLSTATE 21000: more than one row of public.tags has 'x' as its tag -`,
		`tags "y" update would_restore - -`)
}

// An UPDATE that changed a row's key leaves its old key and its new key
// key_changed. A row of the same statement whose key stayed is restored.
func TestPlanSkipsAKeyChange(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 2)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `update public.deals set id = case id when 1 then 101 else id end, note = note || '!' where id in (1, 2)`)

	checkShownInAnyOrder(t, "a key change", planOf(t, undoReviewer(t, db, "public.deals"), "stet_md5", since),
		`deals "1" - skipped key_changed -`,
		`deals "101" - skipped key_changed -`,
		`deals "2" update would_restore - -`)
}

// A change that no longer fits its table is not restored: columns_changed
// when a column was added, dropped or renamed, or the key column is gone
// even where no image is left to compare; table_gone when the table was
// dropped, even if one of the same name and shape was made again: one line
// for the vanished table, without keys (plan ruling R11). The new table's own
// changes are a key of their own, not folded into the old table's. A table
// that was only renamed is still the same table.
func TestPlanSkipsWhatNoLongerFitsTheTable(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	for _, name := range []string{"added", "dropped", "renamed_col", "gone", "renamed", "rekeyed"} {
		keyedTable(t, owner, name, 1)
	}
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	for _, name := range []string{"added", "dropped", "renamed_col", "gone", "renamed"} {
		mustExec(t, agent, "update public."+name+" set n = 10 where id = 1")
	}
	mustExec(t, agent, `insert into public.rekeyed values (9, 'r9', 9)`, `delete from public.rekeyed where id = 9`)
	mustExec(t, owner,
		`alter table public.added add column extra int`,
		`alter table public.dropped drop column note`,
		`alter table public.renamed_col rename column note to memo`,
		`drop table public.gone`,
		`create table public.gone (id int primary key, note text, n int)`,
		`insert into public.gone values (1, 'r1', 1)`,
		`alter table public.renamed rename to renamed_too`,
		`alter table public.rekeyed rename column id to ident`)
	// The table made again under the old name, with capture under the same key: its changes are its own.
	installCapture(t, owner, "public.gone", "gone", "id")
	mustExec(t, agent, `update public.gone set n = 20 where id = 1`)

	rv := undoReviewer(t, db, "public.added", "public.dropped", "public.renamed_col", "public.gone",
		"public.renamed_too", "public.rekeyed")
	checkShown(t, "changed tables", planOf(t, rv, "stet_md5", since),
		`added "1" - skipped columns_changed -`,
		`dropped "1" - skipped columns_changed -`,
		`gone - - skipped table_gone -`,
		`gone "1" update would_restore - -`,
		`rekeyed "9" - skipped columns_changed -`,
		`renamed "1" update would_restore - -`,
		`renamed_col "1" - skipped columns_changed -`)
}

// A key that more than one check catches is skipped for the first in spec
// 7's order: table_gone, columns_changed, key_changed, interleaved,
// changed_since, no_net_change. A table that is gone shows as one line.
func TestPlanCheckOrder(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	for i := 1; i <= 7; i++ {
		keyedTable(t, owner, fmt.Sprintf("k%d", i), 1)
	}
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	// k1: key_changed over changed_since.
	mustExec(t, agent, `update public.k1 set id = 101 where id = 1`)
	mustExec(t, owner, `update public.k1 set n = 7 where id = 101`)
	// k2: key_changed over interleaved.
	mustExec(t, agent, `update public.k2 set n = 10 where id = 1`)
	mustExec(t, owner, `update public.k2 set n = 20 where id = 1`)
	mustExec(t, agent, `update public.k2 set id = 101 where id = 1`)
	// k3: interleaved over changed_since.
	mustExec(t, agent, `update public.k3 set n = 10 where id = 1`)
	mustExec(t, owner, `update public.k3 set n = 20 where id = 1`)
	mustExec(t, agent, `update public.k3 set n = 30 where id = 1`)
	mustExec(t, owner, `update public.k3 set n = 40 where id = 1`)
	// k4: interleaved over no_net_change.
	mustExec(t, agent, `update public.k4 set n = 10 where id = 1`)
	mustExec(t, owner, `update public.k4 set n = 20 where id = 1`)
	mustExec(t, agent, `update public.k4 set n = 1 where id = 1`)
	// k5: changed_since over no_net_change.
	mustExec(t, agent, `update public.k5 set n = 10 where id = 1`, `update public.k5 set n = 1 where id = 1`)
	mustExec(t, owner, `update public.k5 set n = 50 where id = 1`)
	// k6: columns_changed over key_changed.
	mustExec(t, agent, `update public.k6 set id = 101 where id = 1`)
	mustExec(t, owner, `alter table public.k6 add column extra int`)
	// k7: table_gone over all.
	mustExec(t, agent, `update public.k7 set id = 101 where id = 1`)
	mustExec(t, owner, `drop table public.k7`)

	rv := undoReviewer(t, db, "public.k1", "public.k2", "public.k3", "public.k4", "public.k5", "public.k6")
	checkShownInAnyOrder(t, "the first check that catches", planOf(t, rv, "stet_md5", since),
		`k1 "1" - skipped key_changed -`, `k1 "101" - skipped key_changed -`,
		`k2 "1" - skipped key_changed -`, `k2 "101" - skipped key_changed -`,
		`k3 "1" - skipped interleaved -`,
		`k4 "1" - skipped interleaved -`,
		`k5 "1" - skipped changed_since -`,
		`k6 "1" - skipped columns_changed -`, `k6 "101" - skipped columns_changed -`,
		`k7 - - skipped table_gone -`)
}

// A selected set that recorded no values shows as one line without a key,
// not_captured, its reason the set's skipped. It sits among its table's lines
// where its set was recorded. What it changed is not restored, and a row it
// changed after the agent's recorded change is changed_since.
func TestPlanShowsWhatWasNotCaptured(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 2)
	keyedTable(t, owner, "big", 5)
	keyedTable(t, owner, "cf", 1)
	keyedTable(t, owner, "many", 1)
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	mustExec(t, agent,
		`update public.big set n = 10 where id = 1`,
		`update public.big set n = 20 where id in (2, 3, 4)`,
		`update public.big set n = 50 where id = 5`,
		`truncate public.big`)
	// A session holding a temporary table named stet_new: capture refuses to
	// read the transition table (42P07) and records capture_failed.
	mustExec(t, agentIn(t, db), `create temp table stet_new (x int)`, `update public.cf set n = 10 where id = 1`)
	// 32 writes in one transaction: 31 record values, the 32nd only a marker.
	ctx := context.Background()
	tx, err := agent.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 32 {
		mustExec(t, tx, `update public.many set n = n + 1 where id = 1`)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	got := planOf(t, undoReviewer(t, db, "public.big", "public.cf", "public.many"), "stet_md5", since)
	checkShown(t, "sets without values", got,
		`big "1" - skipped changed_since -`,
		`big - - not_captured too_many_rows -`,
		`big "5" - skipped changed_since -`,
		`big - - not_captured truncate -`,
		`cf - - not_captured capture_failed -`,
		`many "1" - skipped changed_since -`,
		`many - - not_captured too_many_statements -`)
	for _, r := range got {
		switch {
		case r.Key == nil && r.ChangeIDs != nil:
			t.Errorf("%v: change_ids %v, want NULL", r, r.ChangeIDs)
		case r.TargetTable == "many" && r.Key != nil && len(r.ChangeIDs) != 31:
			t.Errorf("%v: %d change_ids, want 31", r, len(r.ChangeIDs))
		}
	}
}

// A line that inserts a row back warns when a foreign key that cascades its
// deletion to a child table (ON DELETE CASCADE, SET NULL, SET DEFAULT) has no
// enabled capture trigger on the child: the child rows the cascade changed
// were not recorded and do not come back. The children are named by schema,
// sorted. A child with capture, and a foreign key that does not cascade, are
// not named.
func TestPlanWarnsAboutUncapturedChildren(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.par (id int primary key, note text)`,
		`insert into public.par values (1, 'p1'), (2, 'p2')`,
		`create schema "Crm"`,
		`create table "Crm"."Kid Notes" (id int primary key, par int references public.par on delete cascade)`,
		`create table public.kid_null (id int primary key, par int references public.par on delete set null)`,
		`create table public.kid_plain (id int primary key, par int references public.par)`,
		`create table public.kid_captured (id int primary key, par int references public.par on delete cascade)`,
		`insert into "Crm"."Kid Notes" values (10, 1)`,
		`insert into public.kid_null values (20, 1)`,
		`insert into public.kid_plain values (40, 2)`,
		`insert into public.kid_captured values (30, 1)`)
	installCapture(t, owner, "public.par", "par", "id")
	installCapture(t, owner, "public.kid_captured", "kid_captured", "id")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `delete from public.par where id = 1`, `update public.par set note = 'changed' where id = 2`)

	rv := undoReviewer(t, db, "public.par", "public.kid_captured")
	checkShown(t, "uncaptured children", planOf(t, rv, "stet_md5", since),
		`kid_captured "30" insert would_restore - -`,
		`par "1" insert would_restore - children_not_captured: "Crm"."Kid Notes", public.kid_null`,
		`par "2" update would_restore - -`)

	mustExec(t, owner, `alter table public.kid_captured disable trigger stet_capture_del`)
	checkShown(t, "a child whose delete trigger is disabled", planOf(t, rv, "stet_md5", since),
		`kid_captured "30" insert would_restore - -`,
		`par "1" insert would_restore - children_not_captured: "Crm"."Kid Notes", public.kid_captured, public.kid_null`,
		`par "2" update would_restore - -`)
}

// When tables leaves out a table that the same statement also changed (a
// child the agent's delete cascaded to), the selected lines of that statement
// warn not_selected with that table's configuration key. Joined to
// children_not_captured with "; ".
func TestPlanWarnsAboutUnselectedTables(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.par (id int primary key, note text)`,
		`insert into public.par values (1, 'p1'), (2, 'p2')`,
		`create table public.kid (id int primary key, par int references public.par on delete cascade)`,
		`create table public.kid_uncaptured (id int primary key, par int references public.par on delete cascade)`,
		`insert into public.kid values (10, 1), (11, 1)`,
		`insert into public.kid_uncaptured values (20, 1)`)
	installCapture(t, owner, "public.par", "parents", "id")
	installCapture(t, owner, "public.kid", "kids", "id")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `delete from public.par where id = 1`, `update public.par set note = 'changed' where id = 2`)

	rv := undoReviewer(t, db, "public.par", "public.kid")
	checkShownInAnyOrder(t, "every table", planOf(t, rv, "stet_md5", since),
		`kids "10" insert would_restore - -`,
		`kids "11" insert would_restore - -`,
		`parents "1" insert would_restore - children_not_captured: public.kid_uncaptured`,
		`parents "2" update would_restore - -`)
	checkShown(t, "the parents only", planOf(t, rv, "stet_md5", since, nil, []string{"parents"}),
		`parents "1" insert would_restore - children_not_captured: public.kid_uncaptured; not_selected: kids`,
		`parents "2" update would_restore - -`)
	checkShownInAnyOrder(t, "both tables named", planOf(t, rv, "stet_md5", since, nil, []string{"parents", "kids"}),
		`kids "10" insert would_restore - -`,
		`kids "11" insert would_restore - -`,
		`parents "1" insert would_restore - children_not_captured: public.kid_uncaptured`,
		`parents "2" update would_restore - -`)
}

// The plan changes nothing and locks no row: it runs in a READ ONLY
// transaction; in the reviewer's open transaction after the plan, the
// transaction has no id and holds no lock but AccessShareLock, another
// session takes the rows FOR UPDATE NOWAIT, and the customer's table and
// Stet's records are as they were.
func TestPlanChangesNothing(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 3)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`update public.deals set n = 10 where id = 1`,
		`delete from public.deals where id = 2`,
		`insert into public.deals values (4, 'r4', 4)`)
	snapshot := func() string {
		t.Helper()
		var s string
		if err := owner.QueryRow(ctx, `select concat_ws(' ',
			(select md5(coalesce(string_agg(d::text, ';' order by d.id), '')) from public.deals d),
			(select md5(coalesce(string_agg(s::text, ';' order by s.id), '')) from stet.change_sets s),
			(select md5(coalesce(string_agg(c::text, ';' order by c.id), '')) from stet.changes c))`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()

	rv := undoReviewer(t, db, "public.deals")
	tx, err := rv.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	checkShown(t, "the plan", planOf(t, tx, "stet_md5", since),
		`deals "1" update would_restore - -`,
		`deals "2" insert would_restore - -`,
		`deals "4" delete would_restore - -`)
	var xid *int64
	var pid int32
	if err := tx.QueryRow(ctx, `select txid_current_if_assigned(), pg_backend_pid()`).Scan(&xid, &pid); err != nil {
		t.Fatal(err)
	}
	if xid != nil {
		t.Errorf("the reviewer's transaction has id %d after the plan, want none: the plan wrote something", *xid)
	}
	var locks []string
	if err := owner.QueryRow(ctx, `select coalesce(array_agg(distinct mode order by mode), '{}') from pg_locks
		where pid = $1 and (locktype = 'relation' and mode <> 'AccessShareLock' or locktype in ('tuple', 'transactionid'))`,
		pid).Scan(&locks); err != nil {
		t.Fatal(err)
	}
	if len(locks) != 0 {
		t.Errorf("the reviewer's transaction holds %v after the plan, want nothing beyond AccessShareLock", locks)
	}
	if n := countOf(t, owner, `select count(*) from (select id from public.deals where id in (1, 3, 4) for update nowait) x`); n != 3 {
		t.Errorf("another session locked %d rows, want 3", n)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Errorf("the table or Stet's records changed: %s, then %s", before, after)
	}
}

// The four refusals of spec 7 (and plan ruling U4) are ST006: no agent_role,
// no since, until not after since, and the reviewer's own writes. The
// selection refuses them too, so that no one reads their own records through
// it. until NULL is up to now.
func TestPlanRefusals(t *testing.T) {
	pool, db := captureDB(t)
	rv := undoReviewer(t, db)
	me := roleOf(t, rv)
	since := dbClock(t, pool)
	for _, c := range []struct {
		name  string
		role  any
		since any
		until any
	}{
		{"no agent_role", nil, since, nil},
		{"no since", "stet_md5", nil, nil},
		{"until equal to since", "stet_md5", since, since},
		{"until before since", "stet_md5", since, since.Add(-time.Second)},
		{"the reviewer's own writes", me, since, nil},
	} {
		_, err := tryPlan(rv, c.role, c.since, c.until)
		wantCode(t, err, "ST006", "stet.undo_plan, "+c.name)
		_, err = trySelection(rv, c.role, c.since, c.until, nil)
		wantCode(t, err, "ST006", "stet.undo_selection, "+c.name)
		_, err = rv.Exec(context.Background(), `select stet.undo_refuse($1, $2, $3)`, c.role, c.since, c.until)
		wantCode(t, err, "ST006", "stet.undo_refuse, "+c.name)
	}
	if rows, err := tryPlan(rv, "stet_md5", since, nil); err != nil || len(rows) != 0 {
		t.Errorf("until NULL: %v, %v; want no line and no error", rows, err)
	}
}

// PUBLIC cannot execute any function of the plan, and a role without the
// grants cannot call it (42501). A reviewer with them plans without any right
// on Stet's tables. A table it cannot read gives no error and shows as one
// line, skipped with 42501 and no key (plan ruling R11); no helper called
// directly gives it a key or a value of that table. What the session can read
// as its login role or a role it can SET ROLE to, it sees.
func TestPlanNeedsGrants(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "deals", 1)
	keyedTable(t, owner, "accounts", 1)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `update public.deals set n = 10 where id = 1`, `update public.accounts set n = 10 where id = 1`)

	for _, f := range planFunctions {
		var public bool
		if err := owner.QueryRow(ctx, `select has_function_privilege('public', $1, 'execute')`, f).Scan(&public); err != nil {
			t.Fatal(err)
		}
		if public {
			t.Errorf("PUBLIC may execute %s", f)
		}
	}
	usageOnly, planOnly := loginRole(t, "usage"), loginRole(t, "planonly")
	grant(t, db, usageOnly, "grant usage on schema stet to "+usageOnly)
	grant(t, db, planOnly, "grant usage on schema stet to "+planOnly,
		"grant execute on function stet.undo_plan(text, timestamptz, timestamptz, text[]) to "+planOnly,
		"grant select on public.deals, public.accounts to "+planOnly)
	for name, conn := range map[string]*pgx.Conn{
		"a role with no grant":                  connectAs(t, db, loginRole(t, "outsider")),
		"a role with USAGE on the schema":       connectAs(t, db, usageOnly),
		"a role with EXECUTE on the plan alone": connectAs(t, db, planOnly),
		"the agent":                             agentIn(t, db),
	} {
		_, err := tryPlan(conn, "stet_md5", since)
		wantCode(t, err, "42501", name+" calling stet.undo_plan")
	}

	rv := undoReviewer(t, db, "public.deals")
	for _, table := range []string{"stet.change_sets", "stet.changes"} {
		_, err := rv.Exec(ctx, "select from "+table+" limit 1")
		wantCode(t, err, "42501", "the reviewer reading "+table)
	}
	denied := "SQLSTATE 42501: permission denied for table accounts"
	got := planOf(t, rv, "stet_md5", since)
	checkShown(t, "a table the reviewer cannot read", got,
		`accounts - - skipped `+denied+` -`,
		`deals "1" update would_restore - -`)
	if got[0].ChangeIDs != nil {
		t.Errorf("the line of a table the reviewer cannot read names changes %v, want none", got[0].ChangeIDs)
	}
	checkWithheld(t, rv, "accounts", denied, since)
	sel, err := trySelection(rv, "stet_md5", since, nil, []string{"deals"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel) != 1 || sel[0].B == nil || sel[0].A == nil {
		t.Errorf("the selection of a table the reviewer can read: %+v, want one key with its values", sel)
	}
	accounts := relidOf(t, owner, "public.accounts")
	_, err = rv.Exec(ctx, `select stet.undo_row($1, 'id', '1', false)`, accounts)
	wantCode(t, err, "42501", "stet.undo_row on a table the reviewer cannot read")
	_, err = rv.Exec(ctx, `select * from stet.undo_judge($1, 'id', '1', null, null, false, false, false)`, accounts)
	wantCode(t, err, "42501", "stet.undo_judge on a table the reviewer cannot read")

	// A login role that does not inherit, reading through a role it SETs ROLE
	// to: the selection shows it the values of what that role can read.
	reader, noinherit := otherRole(t, db), loginRole(t, "noinherit")
	grant(t, db, reader, "grant usage on schema stet to "+reader,
		"grant execute on function "+strings.Join(planFunctions, ", ")+" to "+reader,
		"grant select on public.deals, public.accounts to "+reader)
	mustExec(t, owner, "alter role "+noinherit+" noinherit", "grant "+reader+" to "+noinherit)
	through := connectAs(t, db, noinherit)
	mustExec(t, through, "set role "+reader)
	checkShown(t, "a reviewer through SET ROLE", planOf(t, through, "stet_md5", since),
		`accounts "1" update would_restore - -`,
		`deals "1" update would_restore - -`)
}

// A table whose row-level security would apply to the session (no role it
// is or can SET ROLE to owns the table without FORCE ROW LEVEL SECURITY or
// has BYPASSRLS) is withheld like a table it cannot read: one line, no key, no
// value (plan ruling R11). Its owner, without FORCE, and a role with
// BYPASSRLS see the keys, here natural ones.
func TestPlanWithholdsATableUnderRowLevelSecurity(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.people (id int primary key, email text unique not null, n int)`,
		`insert into public.people values (1, 'a@example.com', 1), (2, 'b@example.com', 2)`)
	installCapture(t, owner, "public.people", "people", "email")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `update public.people set n = 10 where id = 1`, `delete from public.people where id = 2`)
	mustExec(t, owner, `alter table public.people enable row level security`, `create policy nobody on public.people using (false)`)
	relid := relidOf(t, owner, "public.people")
	const denied = "SQLSTATE 42501: permission denied for table people"
	withheld := `people - - skipped ` + denied + ` -`
	keys := []string{`people "a@example.com" update would_restore - -`, `people "b@example.com" insert would_restore - -`}

	plain := undoReviewer(t, db, "public.people")
	checkShown(t, "a reviewer the policy hides the rows from", planOf(t, plain, "stet_md5", since), withheld)
	checkWithheld(t, plain, "people", denied, since)
	var hidden *string
	if err := plain.QueryRow(ctx, `select stet.undo_row($1, 'email', 'a@example.com', false)::text`, relid).Scan(&hidden); err != nil {
		t.Fatal(err)
	}
	if hidden != nil {
		t.Errorf("stet.undo_row read a row the policy hides: %s", *hidden)
	}

	tableOwner, bypass := undoReviewer(t, db), undoReviewer(t, db, "public.people")
	mustExec(t, owner, "alter table public.people owner to "+roleOf(t, tableOwner), "alter role "+roleOf(t, bypass)+" bypassrls")
	checkShown(t, "the table's owner", planOf(t, tableOwner, "stet_md5", since), keys...)
	checkShown(t, "a role with BYPASSRLS", planOf(t, bypass, "stet_md5", since), keys...)

	mustExec(t, owner, `alter table public.people force row level security`)
	checkShown(t, "the table's owner under FORCE ROW LEVEL SECURITY", planOf(t, tableOwner, "stet_md5", since), withheld)
	checkWithheld(t, tableOwner, "people", denied, since)
	checkShown(t, "BYPASSRLS under FORCE ROW LEVEL SECURITY", planOf(t, bypass, "stet_md5", since), keys...)
}

// Stet's own reads and comparisons do not use what the caller's search_path
// or temporary objects put first: with a schema at the front of the path
// that holds an always-true jsonb and text =, and format, jsonb_object and
// num_nulls that return other values, and temporary tables named like the
// customer's and Stet's, the plan is what it is without them. So is what
// each helper returns when called directly, as stet.undo, which does not pin
// its search_path, calls them.
func TestPlanIgnoresTheCallersSearchPath(t *testing.T) {
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
		`update public.deals set n = 20 where id = 2`,
		`insert into public.deals values (6, 'r6', 6)`,
		`update public.deals set n = 30 where id = 3`,
		`update public.deals set n = 3 where id = 3`,
		`update public.deals set n = 40 where id = 4`,
		`delete from public.deals where id = 5`)
	mustExec(t, owner, `update public.deals set n = 200 where id = 2`, `update public.deals set n = 400 where id = 4`)
	mustExec(t, agent, `update public.deals set n = 41 where id = 4`)
	plain := undoReviewer(t, db, "public.deals")
	name := roleOf(t, plain)
	want := []string{
		`deals "1" update would_restore - -`,
		`deals "2" - skipped changed_since -`,
		`deals "6" delete would_restore - -`,
		`deals "3" - skipped no_net_change -`,
		`deals "4" - skipped interleaved -`,
		`deals "5" insert would_restore - children_not_captured: public.deal_notes`,
	}
	checkShown(t, "without the trap", planOf(t, plain, "stet_md5", since), want...)

	mustExec(t, owner,
		`create schema trap`,
		`create function trap.always(jsonb, jsonb) returns boolean language sql as 'select true'`,
		`create operator trap.= (leftarg = jsonb, rightarg = jsonb, function = trap.always)`,
		`create function trap.always(text, text) returns boolean language sql as 'select true'`,
		`create operator trap.= (leftarg = text, rightarg = text, function = trap.always)`,
		`create function trap.format(text, integer) returns text language sql as $$select 'trapped'$$`,
		`create function trap.format(text, text) returns text language sql as $$select 'trapped'$$`,
		`create function trap.jsonb_object(text[], text[]) returns jsonb language sql as $$select '{}'::jsonb$$`,
		`create function trap.num_nulls(integer) returns integer language sql as 'select 1'`,
		`create function trap.num_nulls(text) returns integer language sql as 'select 1'`,
		"grant usage on schema trap to "+name)
	trapped := connectAs(t, db, name)
	mustExec(t, trapped,
		`set search_path = trap, pg_catalog, public`,
		`create temp table deals (id int primary key, note text, n int)`,
		`insert into deals values (1, 'r1', 10), (2, 'r2', 20), (3, 'r3', 3), (4, 'trapped', 0), (5, 'r5', 5)`,
		`create temp table change_sets (id bigint)`,
		`create temp table changes (id bigint)`)
	var live bool
	var text string
	if err := trapped.QueryRow(ctx, `select '{"a": 1}'::jsonb = '{"b": 2}'::jsonb, format('%s', 7)`).Scan(&live, &text); err != nil {
		t.Fatal(err)
	}
	if !live || text != "trapped" {
		t.Fatalf("the trap is not live in the reviewer's session: jsonb = %v, format %q", live, text)
	}
	checkShown(t, "with the trap", planOf(t, trapped, "stet_md5", since), want...)

	// Each helper called directly.
	selection := func(conn *pgx.Conn) []selected {
		t.Helper()
		sel, err := trySelection(conn, "stet_md5", since, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return sel
	}
	sel := selection(plain)
	if trappedSel := selection(trapped); !reflect.DeepEqual(sel, trappedSel) {
		t.Errorf("stet.undo_selection:\n  without the trap %+v\n  with it          %+v", sel, trappedSel)
	}
	if len(sel) != 6 || !*sel[4].Interleaved {
		t.Fatalf("the selection without the trap is %+v, want 6 keys, the fifth interleaved", sel)
	}
	for _, s := range sel {
		judge := func(conn *pgx.Conn) [2]*string {
			t.Helper()
			var r [2]*string
			if err := conn.QueryRow(ctx, `select action, reason from stet.undo_judge($1, $2, $3, $4, $5, $6, $7, false)`,
				s.RelID, s.KeyColumn, s.Key, s.B, s.A, s.KeyChanged, s.Interleaved).Scan(&r[0], &r[1]); err != nil {
				t.Fatal(err)
			}
			return r
		}
		row := func(conn *pgx.Conn) *string {
			t.Helper()
			var r *string
			if err := conn.QueryRow(ctx, `select stet.undo_row($1, $2, $3, false)::text`, s.RelID, s.KeyColumn, s.Key).Scan(&r); err != nil {
				t.Fatal(err)
			}
			return r
		}
		if a, b := judge(plain), judge(trapped); !reflect.DeepEqual(a, b) {
			t.Errorf("stet.undo_judge for key %s: %v %v without the trap, %v %v with it",
				*s.Key, deref(a[0]), deref(a[1]), deref(b[0]), deref(b[1]))
		}
		if a, b := row(plain), row(trapped); !reflect.DeepEqual(a, b) {
			t.Errorf("stet.undo_row for key %s: %v without the trap, %v with it", *s.Key, deref(a), deref(b))
		}
	}
	for _, action := range []string{"update", "insert"} {
		warning := func(conn *pgx.Conn) *string {
			t.Helper()
			var w *string
			if err := conn.QueryRow(ctx, `select stet.undo_warning($1, $2, '{kids}')`, sel[0].RelID, action).Scan(&w); err != nil {
				t.Fatal(err)
			}
			return w
		}
		if a, b := warning(plain), warning(trapped); !reflect.DeepEqual(a, b) {
			t.Errorf("stet.undo_warning for %s: %v without the trap, %v with it", action, deref(a), deref(b))
		}
	}
}

// stet.undo_row builds the row now as capture built the row it recorded:
// the same text for every type, under any output settings of the caller's
// session, by the type's output function (never a cast to text). The image
// it returns for a row is the after image capture recorded for it. It
// returns NULL for a key no row has, and leaves the session's settings
// alone.
func TestUndoRowMatchesCapture(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	mustExec(t, owner, `create table public.vals (
		id bigint generated always as identity primary key,
		i int, big bigint, n numeric,
		f_nan float8, f_inf float8, f_ninf float8, f_sum float8, r real,
		s text, ts timestamptz, ts_inf timestamptz, d date, iv interval, u uuid, b bytea,
		arr int[], arr2 text[], jb jsonb, jb_obj jsonb, jb_null jsonb, j json, j_null json, j_dup json,
		nul text, flag boolean, c char(5), ip inet,
		g bigint generated always as (i * 2) stored)`,
		`create type public.mood as enum ('happy', 'sad')`,
		`create function public.mood_text(public.mood) returns text language plpgsql
		 as $$ begin return 'cast ran as ' || current_user; end $$`,
		`create cast (public.mood as text) with function public.mood_text(public.mood)`,
		`create table public.moods (id int primary key, m public.mood)`)
	installCapture(t, owner, "public.vals", "vals", "id")
	installCapture(t, owner, "public.moods", "moods", "id")
	mustExec(t, agentIn(t, db), `insert into public.vals (i, big, n, f_nan, f_inf, f_ninf, f_sum, r, s, ts, ts_inf, d, iv, u, b,
		arr, arr2, jb, jb_obj, jb_null, j, j_null, j_dup, nul, flag, c, ip) values (
		42, 9223372036854775807, 12345678901234567890.0001230,
		'NaN', 'Infinity', '-Infinity', 0.1::float8 + 0.2::float8, 0.1,
		E'line1\nline2 "dq" ''sq'' back\\slash', '2026-11-18 01:02:03.456789+09', 'infinity', '2026-02-28',
		'1 year 2 mons 3 days 04:05:06.789', 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '\x00ff5c27',
		'{1,NULL,3}', '{{a,NULL},{"b c","\"q\""}}', 'null', '{"k": [1, "x", null]}', NULL, 'null', NULL, '{"a": 1,  "a":2}',
		NULL, true, 'ab', '10.0.0.1')`,
		`insert into public.vals (i) values (7)`,
		`insert into public.moods values (1, 'happy')`)

	rv := undoReviewer(t, db, "public.vals", "public.moods")
	odd := connectAs(t, db, roleOf(t, rv))
	mustExec(t, odd,
		`set timezone = 'Asia/Seoul'`, `set datestyle = 'SQL, DMY'`, `set intervalstyle = 'sql_standard'`,
		`set extra_float_digits = -2`, `set bytea_output = 'escape'`)
	var inserts int
	for _, table := range []string{"vals", "moods"} {
		for _, c := range changesOf(t, pool, table) {
			if c.Op != "insert" {
				continue
			}
			inserts++
			var recorded string
			if err := pool.QueryRow(ctx, `select after::text from stet.changes where id = $1`, *c.ChangeID).Scan(&recorded); err != nil {
				t.Fatal(err)
			}
			for who, conn := range map[string]*pgx.Conn{"a default session": rv, "a session with other output settings": odd} {
				var now *string
				if err := conn.QueryRow(ctx, `select stet.undo_row($1, $2, $3, false)::text`, c.RelID, c.KeyColumn, *c.Key).
					Scan(&now); err != nil {
					t.Fatalf("%s: %v", who, err)
				}
				if now == nil || *now != recorded {
					t.Errorf("%s %s, read by %s:\n  stet.undo_row %v\n  capture       %s", table, *c.Key, who, deref(now), recorded)
				}
			}
		}
	}
	if inserts != 3 {
		t.Errorf("%d inserts recorded, want 3", inserts)
	}

	var missing *string
	if err := odd.QueryRow(ctx, `select stet.undo_row($1, 'id', '99', false)::text`, relidOf(t, owner, "public.vals")).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Errorf("stet.undo_row for a key no row has: %s, want NULL", *missing)
	}
	var tz, style string
	if err := odd.QueryRow(ctx, `select current_setting('TimeZone'), current_setting('DateStyle')`).Scan(&tz, &style); err != nil {
		t.Fatal(err)
	}
	if tz != "Asia/Seoul" || style != "SQL, DMY" {
		t.Errorf("the session's settings after stet.undo_row: TimeZone %q, DateStyle %q; want Asia/Seoul, SQL, DMY", tz, style)
	}
}

// The plan finds the row of a key of every common key type by its text,
// under any output settings of the caller's session, and runs none of the
// table owner's code to do so: not a domain's CHECK, not a cast from text.
func TestPlanFindsKeysOfEachType(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	runs := plantedCode(t, owner)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create type public.mood as enum ('happy', 'sad')`,
		`create function public.to_mood(text) returns public.mood language plpgsql
		 as $$ begin perform public.planted(); return 'happy'; end $$`,
		`create cast (text as public.mood) with function public.to_mood(text) as implicit`,
		`create domain public.checked as int check (public.planted() = 7)`)
	keys := []struct {
		table, typ, values string
		keys               []string
	}{
		{"k_text", "text", `('null'), (''), ('a,b'), ('"q"'), (' padded ')`, []string{"null", "", "a,b", `"q"`, " padded "}},
		{"k_uuid", "uuid", `('A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11')`, []string{"a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"}},
		{"k_big", "bigint", `(-1), (9223372036854775807)`, []string{"-1", "9223372036854775807"}},
		{"k_num", "numeric", `(1.50), (-0.001)`, []string{"1.50", "-0.001"}},
		{"k_ts", "timestamptz", `('2026-11-18 01:02:03.456789+09'), ('infinity')`, []string{"2026-11-17 16:02:03.456789+00", "infinity"}},
		{"k_date", "date", `('2026-02-28')`, []string{"2026-02-28"}},
		{"k_char", "char(5)", `('ab')`, []string{"ab   "}},
		{"k_mood", "public.mood", `('sad')`, []string{"sad"}},
		{"k_checked", "public.checked", `(5)`, []string{"5"}},
	}
	var tables, want []string
	for _, k := range keys {
		mustExec(t, owner, "create table public."+k.table+" (k "+k.typ+" primary key)")
		installCapture(t, owner, "public."+k.table, k.table, "k")
		tables = append(tables, "public."+k.table)
	}
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	for _, k := range keys {
		mustExec(t, agent, "insert into public."+k.table+" values "+k.values)
		for _, key := range k.keys {
			want = append(want, k.table+" "+strconv.Quote(key)+" delete would_restore - -")
		}
	}
	ran := runs()

	rv := undoReviewer(t, db, tables...)
	mustExec(t, rv,
		`set timezone = 'Asia/Seoul'`, `set datestyle = 'SQL, DMY'`, `set intervalstyle = 'sql_standard'`,
		`set extra_float_digits = -2`, `set bytea_output = 'escape'`)
	checkShownInAnyOrder(t, "keys of each type", planOf(t, rv, "stet_md5", since), want...)
	if n := runs(); n != ran {
		t.Errorf("the table owner's code ran %d times during the plan, want none", n-ran)
	}
}

// In a REPEATABLE READ transaction, reading the catalog follows the
// transaction's snapshot, while names in SQL resolve in the catalog as it is
// now. A column added or dropped after the snapshot is not seen in the
// snapshot: the row is not read with that stale list of columns, the key is
// skipped with the error. That holds when the highest column was dropped
// before, too, and a dropped column named like a function that takes a row
// (to_jsonb) is not read as a call of that function on the row. A table
// renamed after the snapshot, with another table given its old name, is still
// read as the table relid names, under its new name. A table dropped and made
// again after it is gone. A table whose row-level security was enabled after
// it is withheld (ruling R11).
func TestPlanSeesColumnsChangedAfterItsSnapshot(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.grown (id int primary key, n int)`,
		`insert into public.grown values (1, 1)`,
		`create table public.shrunk (id int primary key, to_jsonb text, n int)`,
		`insert into public.shrunk values (1, 'x', 1)`,
		`create table public.g (id int primary key, n int, x int)`,
		`insert into public.g values (1, 1, 1)`,
		`alter table public.g drop column x`,
		`create table public.t (id int primary key, n int)`,
		`insert into public.t values (1, 1)`,
		`create table public.t_new (id int primary key, n int)`,
		`insert into public.t_new values (1, 999)`,
		`create table public.re (id int primary key, n int)`,
		`insert into public.re values (1, 1)`,
		`create table public.hid (id int primary key, n int)`,
		`insert into public.hid values (1, 1)`)
	for _, table := range []string{"grown", "shrunk", "g", "t", "re", "hid"} {
		installCapture(t, owner, "public."+table, table, "id")
	}
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`update public.grown set n = 10 where id = 1`,
		`update public.shrunk set n = 10 where id = 1`,
		`update public.g set n = 10 where id = 1`,
		`update public.t set n = 10 where id = 1`,
		`delete from public.re where id = 1`,
		`update public.hid set n = 10 where id = 1`)
	relid, gone := relidOf(t, owner, "public.t"), relidOf(t, owner, "public.re")

	rv := undoReviewer(t, db, "public.grown", "public.shrunk", "public.g", "public.t", "public.t_new", "public.re", "public.hid")
	tx, err := rv.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mustExec(t, tx, `select txid_current_snapshot()`)
	mustExec(t, owner,
		`alter table public.grown add column extra int`,
		`alter table public.shrunk drop column to_jsonb`,
		`alter table public.g add column y int`,
		`alter table public.t rename to t_old`,
		`alter table public.t_new rename to t`,
		`drop table public.re`,
		`create table public.re (id int primary key, n int)`,
		"grant select on public.re to "+roleOf(t, rv),
		`alter table public.hid enable row level security`,
		`create policy nobody on public.hid using (false)`)
	got := shown(planOf(t, tx, "stet_md5", since))
	want := []string{
		`g "1" - skipped SQLSTATE 55000: the columns of public.g changed after this transaction's snapshot -`,
		`grown "1" - skipped SQLSTATE 55000: the columns of public.grown changed after this transaction's snapshot -`,
		`hid - - skipped SQLSTATE 42501: permission denied for table hid -`,
		`re - - skipped table_gone -`,
		`shrunk "1" - skipped SQLSTATE 42703: `,
		`t "1" update would_restore - -`,
	}
	same := len(got) == len(want)
	for i := 0; same && i < len(got); i++ {
		// The 42703 message is PostgreSQL's; only its start is pinned.
		same = got[i] == want[i] || strings.HasSuffix(want[i], ": ") && strings.HasPrefix(got[i], want[i])
	}
	if !same {
		t.Errorf("under REPEATABLE READ the plan says\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	mustExec(t, tx, `savepoint before_row`)
	var row *string
	if err := tx.QueryRow(ctx, `select stet.undo_row($1, 'id', '1', false)::text`, relid).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if deref(row) != `{"n": "10", "id": "1"}` {
		t.Errorf("under REPEATABLE READ stet.undo_row for the renamed table read %v, want its own row n = 10", deref(row))
	}
	_, err = tx.Exec(ctx, `select stet.undo_row($1, 'id', '1', false)`, gone)
	wantCode(t, err, "42P01", "under REPEATABLE READ stet.undo_row for a table dropped after the snapshot")
	if _, err := tx.Exec(ctx, `rollback to savepoint before_row`); err != nil {
		t.Fatal(err)
	}
	// stet.undo calls the judge directly: a table dropped after the snapshot is table_gone there too.
	var action, reason *string
	if err := tx.QueryRow(ctx, `select action, reason from stet.undo_judge($1, 'id', '1', '{"id": "1", "n": "1"}', null, false, false, false)`,
		gone).Scan(&action, &reason); err != nil {
		t.Fatal(err)
	}
	if action != nil || deref(reason) != "table_gone" {
		t.Errorf("under REPEATABLE READ stet.undo_judge for a dropped table says %v %v, want table_gone", deref(action), deref(reason))
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	checkShown(t, "in a new transaction", planOf(t, rv, "stet_md5", since),
		`g "1" - skipped columns_changed -`,
		`grown "1" - skipped columns_changed -`,
		`hid - - skipped SQLSTATE 42501: permission denied for table hid -`,
		`re - - skipped table_gone -`,
		`shrunk "1" - skipped columns_changed -`,
		`t "1" update would_restore - -`)
}

// A plan over 5,000 keys of one table, among 20,000 rows, takes seconds: each
// key's row is found through the table's key index.
func TestPlanOfManyKeys(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.many (id int primary key, n int)`,
		`insert into public.many select i, i from generate_series(1, 20000) i`,
		`analyze public.many`)
	installCapture(t, owner, "public.many", "many", "id")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `update public.many set n = n + 1 where id <= 5000`)

	rv := undoReviewer(t, db, "public.many")
	start := time.Now()
	rows := planOf(t, rv, "stet_md5", since)
	took := time.Since(start)
	t.Logf("a plan over %d keys took %v", len(rows), took)
	if len(rows) != 5000 {
		t.Fatalf("%d lines, want 5000", len(rows))
	}
	seen := map[string]bool{}
	for i, r := range rows {
		if r.Action != "update" || r.Outcome != "would_restore" || r.Key == nil || len(r.ChangeIDs) != 1 {
			t.Fatalf("line %d: %v %v, want an update that would_restore with one change", i, r, r.ChangeIDs)
		}
		if i > 0 && r.ChangeIDs[0] <= rows[i-1].ChangeIDs[0] {
			t.Fatalf("line %d: change %d after change %d, want the lines by first change", i, r.ChangeIDs[0], rows[i-1].ChangeIDs[0])
		}
		seen[*r.Key] = true
	}
	if len(seen) != 5000 {
		t.Errorf("%d distinct keys, want 5000", len(seen))
	}
	if took > 20*time.Second {
		t.Errorf("the plan took %v, want seconds", took)
	}
}

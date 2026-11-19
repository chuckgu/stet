package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Helpers shared by the tests of stet.undo_plan (undo_plan_test.go) and
// stet.undo (undo_test.go).

// planFunctions are the functions a role needs EXECUTE on to call
// stet.undo_plan: the plan and the helpers it calls (spec 7, 권한).
var planFunctions = []string{
	"stet.undo_plan(text, timestamptz, timestamptz, text[])",
	"stet.undo_selection(text, timestamptz, timestamptz, text[])",
	"stet.undo_lock(oid)",
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

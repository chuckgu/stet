package gate_test

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/sqlparse"
)

// readsYAML records reads of the table: each one is counted, and one that
// sends more than 3 rows is also logged by itself.
const readsYAML = tableYAML + "    reads: {log_above: 3}\n"

// dealsYAML adds a second gated table, <table>_deals, whose reads are not
// recorded.
const dealsYAML = readsYAML + `  %[2]s_deals:
    pk: id
    send_fields: [contact_id]
`

// bothReadYAML records reads of the second table too, aggregated only.
const bothReadYAML = dealsYAML + "    reads: {}\n"

// setupReads is setup over yamlFormat that also removes what the test left
// in stet.reads and stet.read_log.
func setupReads(t *testing.T, yamlFormat string) *env {
	t.Helper()
	e := setup(t, calm(), yamlFormat)
	tables := []string{e.table, e.deals()}
	t.Cleanup(func() {
		e.gate.Close() // writes what is still in memory, so it is removed below
		ctx := context.Background()
		e.admin.Exec(ctx, "delete from stet.reads where target_table = any($1)", tables)
		e.admin.Exec(ctx, "delete from stet.read_log where target_tables && $1", tables)
	})
	return e
}

// deals is the second gated table of dealsYAML and bothReadYAML.
func (e *env) deals() string { return e.table + "_deals" }

// seedDeals creates the second table with four deals: two of contact 1, one
// each of contacts 2 and 6.
func (e *env) seedDeals(t *testing.T) {
	t.Helper()
	mustExec(t, e.admin, "create table "+e.deals()+" (id int primary key, contact_id int)")
	t.Cleanup(func() { e.admin.Exec(context.Background(), "drop table "+e.deals()) })
	mustExec(t, e.admin, "insert into "+e.deals()+" values (1, 1), (2, 1), (3, 2), (4, 6)")
}

type readRow struct {
	Bucket     time.Time
	LoginRole  string
	Statements int64
	Rows       int64
	MaxRows    int64
}

// reads returns the rows of stet.reads for the env's table, oldest first.
func (e *env) reads(t *testing.T) []readRow {
	t.Helper()
	return e.readsOf(t, e.table)
}

func (e *env) readsOf(t *testing.T, table string) []readRow {
	t.Helper()
	rows, err := e.admin.Query(context.Background(), `
		select bucket, login_role, statements, rows, max_rows from stet.reads
		where target_table = $1 order by bucket, login_role`, table)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[readRow])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// count is a role's reads summed over minutes, with the largest max_rows.
type count struct{ Statements, Rows, MaxRows int64 }

func perRole(rows []readRow) map[string]count {
	out := map[string]count{}
	for _, r := range rows {
		c := out[r.LoginRole]
		c.Statements += r.Statements
		c.Rows += r.Rows
		c.MaxRows = max(c.MaxRows, r.MaxRows)
		out[r.LoginRole] = c
	}
	return out
}

func wantCounts(t *testing.T, rows []readRow, want map[string]count) {
	t.Helper()
	if got := perRole(rows); !reflect.DeepEqual(got, want) {
		t.Errorf("reads per role = %+v, want %+v", got, want)
	}
}

type readLogRow struct {
	CreatedAt   time.Time
	LoginRole   *string
	AgentID     string
	OnBehalfOf  *string
	SessionID   *string
	Statement   string
	StatementFP string
	Tables      []string
	Rows        int64
	ExecError   *string
}

// readLog returns the rows of stet.read_log that read the env's table,
// oldest first.
func (e *env) readLog(t *testing.T) []readLogRow {
	t.Helper()
	rows, err := e.admin.Query(context.Background(), `
		select created_at, login_role, agent_id, on_behalf_of, session_id, statement,
		       statement_fp, target_tables, rows, exec_error
		from stet.read_log where $1 = any(target_tables) order by created_at`, e.table)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[readLogRow])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// wantNoDecisions checks that nothing was recorded in stet.decisions for
// the env's table. It closes the gate first, so no judgment is in flight.
func (e *env) wantNoDecisions(t *testing.T) {
	t.Helper()
	e.gate.Close()
	var n int
	if err := e.admin.QueryRow(context.Background(),
		"select count(*) from stet.decisions where target_table = $1", e.table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d decisions recorded for reads, want none", n)
	}
}

// collect runs sql on conn and returns the rows the client received.
func collect(t *testing.T, conn *pgx.Conn, sql string, args ...any) [][]any {
	t.Helper()
	rows, err := conn.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([]any, error) { return r.Values() })
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return got
}

// holdPool takes every connection of the env's pool, so anything that
// needs one waits. release gives them back; it may be called more than once.
func (e *env) holdPool(t *testing.T) (release func()) {
	t.Helper()
	ctx := context.Background()
	var held []*pgxpool.Conn
	for range e.pool.Config().MaxConns {
		c, err := e.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			for _, c := range held {
				c.Release()
			}
		})
	}
	t.Cleanup(release)
	return release
}

func TestReadsAreAggregatedPerRoleAndTable(t *testing.T) {
	e := setupReads(t, readsYAML)
	e.seedMany(t)
	mustExec(t, e.admin, "grant select on "+e.table+" to stet_md5")
	all := "select * from " + e.table + " order by id"
	direct := collect(t, e.admin, all)
	if len(direct) != 7 {
		t.Fatalf("%d rows seeded, want 7", len(direct))
	}

	scram := e.connect(t, "reader")
	for range 2 {
		if got := collect(t, scram, all); !reflect.DeepEqual(got, direct) {
			t.Errorf("through the proxy %v, want what a direct connection gets: %v", got, direct)
		}
	}
	if got := collect(t, scram, "select * from "+e.table+" where id <= 2"); len(got) != 2 {
		t.Errorf("%d rows, want 2", len(got))
	}
	if got := collect(t, e.connectAs(t, "stet_md5", "md5-pw", "reader"), all); !reflect.DeepEqual(got, direct) {
		t.Errorf("through the proxy as stet_md5 %v, want %v", got, direct)
	}

	e.gate.FlushReads()
	wantCounts(t, e.reads(t), map[string]count{
		"stet_scram": {Statements: 3, Rows: 16, MaxRows: 7},
		"stet_md5":   {Statements: 1, Rows: 7, MaxRows: 7},
	})
	e.wantNoDecisions(t)
}

func TestLargeReadsAreLogged(t *testing.T) {
	e := setupReads(t, readsYAML)
	e.seedMany(t)
	conn := e.connect(t, "exporter")
	mustExec(t, conn, "set stet.on_behalf_of = 'alice'")
	collect(t, conn, "select * from "+e.table+" where id <= 2")
	large := "select * from " + e.table
	before := time.Now()
	collect(t, conn, large)
	after := time.Now()

	e.gate.FlushReads()
	logs := e.readLog(t)
	if len(logs) != 1 {
		t.Fatalf("%d large reads logged, want only the 7-row one: %+v", len(logs), logs)
	}
	l := logs[0]
	if l.Statement != large || l.StatementFP == "" || l.Rows != 7 || !reflect.DeepEqual(l.Tables, []string{e.table}) {
		t.Errorf("logged %q (fp %q) rows %d tables %v, want %q, a fingerprint, 7 rows, [%s]",
			l.Statement, l.StatementFP, l.Rows, l.Tables, large, e.table)
	}
	if deref(l.LoginRole) != "stet_scram" || l.AgentID != "exporter" || deref(l.OnBehalfOf) != "alice" || l.SessionID == nil {
		t.Errorf("login_role %v agent %q on_behalf_of %v session %v, want stet_scram, exporter, alice and a session",
			deref(l.LoginRole), l.AgentID, deref(l.OnBehalfOf), deref(l.SessionID))
	}
	if l.ExecError != nil {
		t.Errorf("exec_error = %q, want NULL", *l.ExecError)
	}
	// When the statement arrived, not when the line was written.
	if l.CreatedAt.Before(before.Add(-time.Millisecond)) || l.CreatedAt.After(after) {
		t.Errorf("created_at %v, want between %v and %v", l.CreatedAt, before, after)
	}
	e.wantNoDecisions(t)
}

func TestReadsWithoutTheBlockAreNotSeen(t *testing.T) {
	e := setupReads(t, tableYAML)
	e.seedMany(t)
	if st := e.gate.Inspect("select * from " + e.table); st.Observe {
		t.Error("a read of a table without reads is marked to be observed")
	}
	collect(t, e.connect(t, "reader"), "select * from "+e.table)
	e.gate.FlushReads()
	if rows := e.reads(t); len(rows) != 0 {
		t.Errorf("stet.reads has %+v, want nothing", rows)
	}
	e.wantNoDecisions(t)
}

// When no table records reads, Inspect does not have statements examined for
// reads: a read of a gated table classifies as Pass, as before reads were
// counted. Cursor statements still carry their cursor.
func TestInspectSkipsReadsWhenNoTableRecordsThem(t *testing.T) {
	read := func(e *env) sqlparse.Result { return e.gate.Inspect("select * from " + e.table).Data.(sqlparse.Result) }
	off := setupReads(t, tableYAML)
	if r := read(off); r.Kind != sqlparse.Pass || r.Reads != nil {
		t.Errorf("no table records reads: %v reads %v, want pass and no reads", r.Kind, r.Reads)
	}
	if st := off.gate.Inspect("fetch 10 from c"); len(st.Cursors) != 1 {
		t.Error("no table records reads: a FETCH carries no cursor")
	}
	if r := read(setupReads(t, readsYAML)); r.Kind != sqlparse.Read {
		t.Errorf("the table records reads: %v, want read", r.Kind)
	}
}

func TestReadOfTwoTablesCountsOnlyTheOneTurnedOn(t *testing.T) {
	e := setupReads(t, dealsYAML)
	e.seedMany(t)
	e.seedDeals(t)
	if st := e.gate.Inspect("select * from " + e.deals()); st.Observe {
		t.Error("a read of the table without reads is marked to be observed")
	}
	join := "select c.id, d.id from " + e.table + " c join " + e.deals() + " d on d.contact_id = c.id"
	if got := collect(t, e.connect(t, "reader"), join); len(got) != 4 {
		t.Fatalf("%d rows, want 4", len(got))
	}

	e.gate.FlushReads()
	wantCounts(t, e.reads(t), map[string]count{"stet_scram": {Statements: 1, Rows: 4, MaxRows: 4}})
	if rows := e.readsOf(t, e.deals()); len(rows) != 0 {
		t.Errorf("reads of %s counted: %+v", e.deals(), rows)
	}
	// The line names every gated table the statement read (spec 5).
	logs := e.readLog(t)
	if len(logs) != 1 || !reflect.DeepEqual(logs[0].Tables, []string{e.table, e.deals()}) {
		t.Errorf("read_log = %+v, want one line naming [%s %s]", logs, e.table, e.deals())
	}
}

// A statement that reads two recorded tables counts its rows for each, and
// is logged once, naming both, when it passes either table's log_above.
func TestReadOfTwoRecordedTablesCountsForEach(t *testing.T) {
	e := setupReads(t, bothReadYAML)
	e.seedMany(t)
	e.seedDeals(t)
	join := "select c.id, d.id from " + e.table + " c join " + e.deals() + " d on d.contact_id = c.id"
	if got := collect(t, e.connect(t, "reader"), join); len(got) != 4 {
		t.Fatalf("%d rows, want 4", len(got))
	}

	e.gate.FlushReads()
	want := map[string]count{"stet_scram": {Statements: 1, Rows: 4, MaxRows: 4}}
	wantCounts(t, e.reads(t), want)
	wantCounts(t, e.readsOf(t, e.deals()), want)
	logs := e.readLog(t)
	if len(logs) != 1 || !reflect.DeepEqual(logs[0].Tables, []string{e.table, e.deals()}) || logs[0].Rows != 4 {
		t.Errorf("read_log = %+v, want one 4-row line naming [%s %s]", logs, e.table, e.deals())
	}
}

func TestFailedReadCountsTheRowsItSent(t *testing.T) {
	e := setupReads(t, readsYAML)
	e.seedMany(t)
	conn := e.connect(t, "reader")
	// The subquery fixes the order, so ids 1 to 4 go out before id 5
	// divides by zero.
	rows, err := conn.Query(context.Background(),
		"select id, 1 / (id - 5) from (select id from "+e.table+" order by id offset 0) s")
	if err != nil {
		t.Fatal(err)
	}
	received := 0
	for rows.Next() {
		received++
	}
	if code := sqlstate(rows.Err()); code != "22012" || received != 4 {
		t.Fatalf("received %d rows, then SQLSTATE %q; want 4, then 22012", received, code)
	}

	e.gate.FlushReads()
	wantCounts(t, e.reads(t), map[string]count{"stet_scram": {Statements: 1, Rows: 4, MaxRows: 4}})
	logs := e.readLog(t)
	if len(logs) != 1 || logs[0].Rows != 4 || logs[0].ExecError == nil || !strings.Contains(*logs[0].ExecError, "22012") {
		t.Errorf("read_log = %+v, want one 4-row line whose exec_error names 22012", logs)
	}
}

func TestEmptyReadCountsTheStatement(t *testing.T) {
	e := setupReads(t, readsYAML)
	e.seedMany(t)
	if got := collect(t, e.connect(t, "reader"), "select * from "+e.table+" where id < 0"); len(got) != 0 {
		t.Fatalf("%d rows, want 0", len(got))
	}
	e.gate.FlushReads()
	wantCounts(t, e.reads(t), map[string]count{"stet_scram": {Statements: 1, Rows: 0, MaxRows: 0}})
	if logs := e.readLog(t); len(logs) != 0 {
		t.Errorf("read_log = %+v, want nothing", logs)
	}
}

func TestCursorReadsAreCounted(t *testing.T) {
	e := setupReads(t, readsYAML)
	e.seedMany(t)
	conn := e.connect(t, "cursor-reader")
	declare := "declare c cursor for select * from " + e.table
	mustExec(t, conn, "begin")
	mustExec(t, conn, declare)
	first := collect(t, conn, "fetch 5 from c")
	second := collect(t, conn, "fetch 5 from c")
	mustExec(t, conn, "commit")
	if len(first) != 5 || len(second) != 2 {
		t.Fatalf("fetched %d and %d rows, want 5 and 2", len(first), len(second))
	}

	e.gate.FlushReads()
	// max_rows is the cursor's total; the line is written when it first
	// passes log_above (spec 5).
	wantCounts(t, e.reads(t), map[string]count{"stet_scram": {Statements: 2, Rows: 7, MaxRows: 7}})
	logs := e.readLog(t)
	if len(logs) != 1 || logs[0].Statement != declare || logs[0].Rows != 5 || !reflect.DeepEqual(logs[0].Tables, []string{e.table}) {
		t.Errorf("read_log = %+v, want one 5-row line holding %q", logs, declare)
	}
}

// A read taken in parts, FETCH by FETCH from one cursor or Execute by Execute
// from one row-limited portal, is one read for log_above and max_rows. One
// line is logged when the rows read so far first pass log_above, holding the
// rows read up to then, and max_rows is the total. A new DECLARE of the name,
// or a new Bind, starts over.
func TestReadInPartsIsLoggedOnce(t *testing.T) {
	check := func(t *testing.T, e *env, statement string) {
		t.Helper()
		e.gate.FlushReads()
		// Two reads of the 7 rows, 2 at a time: 4 parts each.
		wantCounts(t, e.reads(t), map[string]count{"stet_scram": {Statements: 8, Rows: 14, MaxRows: 7}})
		logs := e.readLog(t)
		if len(logs) != 2 {
			t.Fatalf("read_log = %+v, want one line per read", logs)
		}
		for i, l := range logs {
			if l.Statement != statement || l.Rows != 4 {
				t.Errorf("line %d: %q with %d rows, want %q with the 4 rows read when log_above 3 was passed", i+1, l.Statement, l.Rows, statement)
			}
		}
	}
	t.Run("cursor", func(t *testing.T) {
		e := setupReads(t, readsYAML)
		e.seedMany(t)
		conn := e.connect(t, "exporter")
		declare := "declare c cursor for select * from " + e.table
		for range 2 {
			mustExec(t, conn, "begin")
			mustExec(t, conn, declare)
			for _, want := range []int{2, 2, 2, 1} {
				if got := collect(t, conn, "fetch 2 from c"); len(got) != want {
					t.Fatalf("fetched %d rows, want %d", len(got), want)
				}
			}
			mustExec(t, conn, "commit")
		}
		check(t, e, declare)
	})
	t.Run("portal", func(t *testing.T) {
		e := setupReads(t, readsYAML)
		e.seedMany(t)
		conn := e.connect(t, "exporter")
		all := "select * from " + e.table
		for range 2 {
			mustExec(t, conn, "begin")
			if got := executeInParts(t, conn, all, 2); !reflect.DeepEqual(got, []int{2, 2, 2, 1}) {
				t.Fatalf("executes sent %v rows, want [2 2 2 1]", got)
			}
			mustExec(t, conn, "commit")
		}
		check(t, e, all)
	})
}

// executeInParts binds sql to portal p, as JDBC does with a fetch size, and
// executes it n rows at a time, one Sync each, until it completes. It returns
// the rows each Execute sent. Run it inside a transaction, so p outlives Sync.
func executeInParts(t *testing.T, conn *pgx.Conn, sql string, n uint32) []int {
	t.Helper()
	fe := conn.PgConn().Frontend()
	// exchange sends what was queued with a Sync and reads to ReadyForQuery.
	exchange := func() (rows int, done bool) {
		t.Helper()
		fe.Send(&pgproto3.Sync{})
		if err := fe.Flush(); err != nil {
			t.Fatal(err)
		}
		for {
			msg, err := fe.Receive()
			if err != nil {
				t.Fatal(err)
			}
			switch m := msg.(type) {
			case *pgproto3.DataRow:
				rows++
			case *pgproto3.CommandComplete:
				done = true
			case *pgproto3.ErrorResponse:
				t.Fatalf("SQLSTATE %s: %s", m.Code, m.Message)
			case *pgproto3.ReadyForQuery:
				return rows, done
			}
		}
	}
	fe.Send(&pgproto3.Parse{Query: sql})
	fe.Send(&pgproto3.Bind{DestinationPortal: "p"})
	exchange()
	var sent []int
	for {
		fe.Send(&pgproto3.Execute{Portal: "p", MaxRows: n})
		rows, done := exchange()
		sent = append(sent, rows)
		if done {
			return sent
		}
	}
}

// Cursor statements inside a Query of several statements (as an agent's
// generic "run SQL" tool sends them) are followed like single ones. The
// message is reported once at most, and a DECLARE in it is remembered with the
// whole message as its statement.
func TestCursorStatementsAmongSeveral(t *testing.T) {
	// simple sends sql as one Query and returns the rows the client got.
	simple := func(t *testing.T, conn *pgx.Conn, sql string) (int, string) {
		t.Helper()
		results, err := conn.PgConn().Exec(context.Background(), sql).ReadAll()
		n := 0
		for _, r := range results {
			n += len(r.Rows)
		}
		return n, sqlstate(err)
	}
	mustSimple := func(t *testing.T, conn *pgx.Conn, sql string, rows int) {
		t.Helper()
		if n, code := simple(t, conn, sql); n != rows || code != "" {
			t.Fatalf("%s: %d rows, SQLSTATE %q; want %d rows and no error", sql, n, code, rows)
		}
	}
	wantReads := func(t *testing.T, e *env, statements, rows int64) {
		t.Helper()
		e.gate.FlushReads()
		if c := perRole(e.reads(t))["stet_scram"]; c.Statements != statements || c.Rows != rows {
			t.Errorf("counted %d statements and %d rows, want %d and %d", c.Statements, c.Rows, statements, rows)
		}
	}
	wantLogged := func(t *testing.T, e *env, statement string, rows int64) {
		t.Helper()
		logs := e.readLog(t)
		if len(logs) != 1 || logs[0].Statement != statement || logs[0].Rows != rows {
			t.Errorf("read_log = %+v, want one %d-row line holding %q", logs, rows, statement)
		}
	}

	t.Run("begin and declare in one Query", func(t *testing.T) {
		e := setupReads(t, readsYAML)
		e.seedMany(t)
		conn := e.connect(t, "agent")
		declare := "begin; declare c cursor for select * from " + e.table
		mustSimple(t, conn, declare, 0)
		mustSimple(t, conn, "fetch 5 from c", 5)
		mustSimple(t, conn, "fetch 5 from c", 2)
		mustSimple(t, conn, "commit", 0)
		wantReads(t, e, 2, 7) // the FETCHes; the DECLARE's message sent nothing
		wantLogged(t, e, declare, 5)
	})
	t.Run("fetch among other statements", func(t *testing.T) {
		e := setupReads(t, readsYAML)
		e.seedMany(t)
		conn := e.connect(t, "agent")
		declare := "declare c cursor with hold for select * from " + e.table
		mustSimple(t, conn, declare, 0)
		mustSimple(t, conn, "fetch 7 from c; select 1", 8)
		mustSimple(t, conn, "close c", 0)
		wantReads(t, e, 1, 8) // every row of the message (spec 9)
		wantLogged(t, e, declare, 8)
	})
	t.Run("declare and fetch in one Query", func(t *testing.T) {
		e := setupReads(t, readsYAML)
		e.seedMany(t)
		conn := e.connect(t, "agent")
		both := "declare c cursor for select * from " + e.table + "; fetch 5 from c"
		mustSimple(t, conn, both, 5)
		wantReads(t, e, 1, 5) // one read, not one for the DECLARE and one for the FETCH
		wantLogged(t, e, both, 5)
	})
	t.Run("held cursor committed in a Query that failed", func(t *testing.T) {
		e := setupReads(t, readsYAML)
		e.seedMany(t)
		conn := e.connect(t, "agent")
		failing := "begin; declare c cursor with hold for select * from " + e.table + "; commit; select 1/0"
		if _, code := simple(t, conn, failing); code != "22012" {
			t.Fatalf("SQLSTATE %q, want 22012", code)
		}
		mustSimple(t, conn, "fetch 7 from c", 7) // the server kept the cursor
		mustSimple(t, conn, "close c", 0)
		wantReads(t, e, 1, 7)
		wantLogged(t, e, failing, 7)
	})
	t.Run("fetch next to a gated write", func(t *testing.T) {
		e := setupReads(t, readsYAML)
		e.seedMany(t)
		conn := e.connect(t, "agent")
		declare := "declare c cursor with hold for select * from " + e.table
		mustSimple(t, conn, declare, 0)
		mustSimple(t, conn, "fetch 5 from c; update "+e.table+" set title = 'x' where id = 1", 5)
		mustSimple(t, conn, "close c", 0)
		wantReads(t, e, 1, 5) // the write is judged as before, and the FETCH is still a read
		wantLogged(t, e, declare, 5)
		if a := e.accounts(t, 1); len(a) != 1 {
			t.Errorf("%d decisions, want the write's", len(a))
		}
	})
	// The server refuses the second DECLARE of a name it holds (42P03) and
	// keeps the held cursor, in a transaction that rolls back too.
	for _, tc := range []struct {
		name          string
		before, after string
	}{
		{"redeclare in a Query that failed", "", ""},
		{"redeclare in a Query that failed in a transaction", "begin", "rollback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupReads(t, readsYAML)
			e.seedMany(t)
			conn := e.connect(t, "agent")
			held := "declare c cursor with hold for select * from " + e.table
			mustSimple(t, conn, held, 0)
			if tc.before != "" {
				mustSimple(t, conn, tc.before, 0)
			}
			if _, code := simple(t, conn, "declare c cursor for select * from "+e.table+"; select 1"); code != "42P03" {
				t.Fatalf("SQLSTATE %q, want 42P03", code)
			}
			if tc.after != "" {
				mustSimple(t, conn, tc.after, 0)
			}
			mustSimple(t, conn, "fetch 2 from c", 2) // the server still holds c
			mustSimple(t, conn, "close c", 0)
			wantReads(t, e, 1, 2)
		})
	}
	t.Run("close in a Query that failed", func(t *testing.T) {
		e := setupReads(t, readsYAML)
		e.seedMany(t)
		conn := e.connect(t, "agent")
		mustSimple(t, conn, "declare c cursor with hold for select * from "+e.table, 0)
		if _, code := simple(t, conn, "close c; select 1/0"); code != "22012" {
			t.Fatalf("SQLSTATE %q, want 22012", code)
		}
		// The server closed c. Stet cannot tell which statements of a failed
		// Query ran, so it keeps c: the FETCH fails and counts as one
		// statement with no rows.
		if n, code := simple(t, conn, "fetch 1 from c"); n != 0 || code != "34000" {
			t.Fatalf("FETCH after the CLOSE: %d rows, SQLSTATE %q; want 34000", n, code)
		}
		wantReads(t, e, 1, 0)
	})
}

// DISCARD ALL closes held cursors too, so a FETCH after it is not a read.
func TestDiscardAllForgetsHeldCursors(t *testing.T) {
	e := setupReads(t, readsYAML)
	e.seedMany(t)
	conn := e.connect(t, "pooled")
	mustExec(t, conn, "begin")
	mustExec(t, conn, "declare c cursor with hold for select * from "+e.table)
	mustExec(t, conn, "commit")
	if got := collect(t, conn, "fetch 2 from c", pgx.QueryExecModeSimpleProtocol); len(got) != 2 {
		t.Fatalf("fetched %d rows, want 2", len(got))
	}
	mustExec(t, conn, "discard all")
	rows, err := conn.Query(context.Background(), "fetch 2 from c", pgx.QueryExecModeSimpleProtocol)
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
	}
	if code := sqlstate(err); code != "34000" {
		t.Fatalf("FETCH after DISCARD ALL: SQLSTATE %q, want 34000", code)
	}

	e.gate.FlushReads()
	wantCounts(t, e.reads(t), map[string]count{"stet_scram": {Statements: 1, Rows: 2, MaxRows: 2}})
}

// A session whose startup packet the proxy cannot read has no login role;
// its reads are counted under the empty role "". Its large reads leave
// login_role NULL, as stet.decisions does.
func TestReadsWithoutALoginRoleUseAnEmptyRole(t *testing.T) {
	e := setupReads(t, readsYAML)
	e.seedMany(t)
	mustExec(t, e.admin, "grant select on "+e.table+" to stet_trust")
	if code := e.rawSimpleQuery(t, 3<<16|1, "stet_trust", "select * from "+e.table); code != "" {
		t.Fatalf("the read failed with SQLSTATE %s", code)
	}

	e.gate.FlushReads()
	wantCounts(t, e.reads(t), map[string]count{"": {Statements: 1, Rows: 7, MaxRows: 7}})
	logs := e.readLog(t)
	if len(logs) != 1 || logs[0].LoginRole != nil {
		t.Errorf("read_log = %+v, want one line with login_role NULL", logs)
	}
}

func TestSameMinuteIsSummedAcrossFlushes(t *testing.T) {
	e := setupReads(t, readsYAML)
	e.seedMany(t)
	conn := e.connect(t, "reader")
	all := "select * from " + e.table
	before := time.Now()
	collect(t, conn, all)
	e.gate.FlushReads()
	collect(t, conn, all)
	after := time.Now()
	e.gate.FlushReads()

	rows := e.reads(t)
	wantCounts(t, rows, map[string]count{"stet_scram": {Statements: 2, Rows: 14, MaxRows: 7}})
	if len(rows) > 2 {
		t.Errorf("%d rows, want at most one per minute: %+v", len(rows), rows)
	}
	if before.Truncate(time.Minute).Equal(after.Truncate(time.Minute)) && len(rows) != 1 {
		t.Errorf("both reads arrived in one minute, but stet.reads has %d rows: %+v", len(rows), rows)
	}
}

func TestCloseWritesWhatIsStillInMemory(t *testing.T) {
	e := setupReads(t, readsYAML)
	e.seedMany(t)
	collect(t, e.connect(t, "reader"), "select * from "+e.table)
	e.gate.Close()
	wantCounts(t, e.reads(t), map[string]count{"stet_scram": {Statements: 1, Rows: 7, MaxRows: 7}})
	if logs := e.readLog(t); len(logs) != 1 {
		t.Errorf("read_log = %+v, want the large read", logs)
	}
}

func TestWritesAreNotCountedAsReads(t *testing.T) {
	e := setupReads(t, readsYAML)
	e.seedMany(t)
	conn := e.connect(t, "writer")
	mustExec(t, conn, "insert into "+e.table+" (id, name) select id + 100, name from "+e.table)
	if got := collect(t, conn, "update "+e.table+" set title = 'x' where company_id = 9 returning *"); len(got) != 2 {
		t.Fatalf("%d rows returned, want 2", len(got))
	}
	e.gate.FlushReads()
	if rows := e.reads(t); len(rows) != 0 {
		t.Errorf("stet.reads has %+v, want nothing for writes", rows)
	}
}

// Reads are counted in memory: with every pooled connection taken, a read,
// a large one included, does not wait.
func TestReadsDoNotTouchTheDatabaseBeforeTheStatement(t *testing.T) {
	e := setupReads(t, readsYAML)
	e.seedMany(t)
	conn := e.connect(t, "reader")
	release := e.holdPool(t)
	start := time.Now()
	for range 2 {
		if got := collect(t, conn, "select * from "+e.table); len(got) != 7 {
			t.Fatalf("%d rows, want 7", len(got))
		}
	}
	elapsed := time.Since(start)
	release()
	if elapsed >= 500*time.Millisecond {
		t.Errorf("two reads took %v with the pool taken, want under 500ms", elapsed)
	}
	e.gate.FlushReads()
	wantCounts(t, e.reads(t), map[string]count{"stet_scram": {Statements: 2, Rows: 14, MaxRows: 7}})
	if logs := e.readLog(t); len(logs) != 2 {
		t.Errorf("%d large reads logged, want 2", len(logs))
	}
}

// A read whose connection ended before the server answered still counts the
// rows that went out, and a large one is logged with why it ended.
func TestLostReadCountsTheRowsItSent(t *testing.T) {
	e := setupReads(t, readsYAML)
	e.seedMany(t)
	conn := e.connect(t, "dropped")
	// Rows far larger than Postgres's send buffer go out as they are made,
	// each pushing out the end of the one before. The sixth waits a second,
	// so rows 1 to 4 arrive whole; the client takes them and hangs up.
	rows, err := conn.Query(context.Background(), "select s.id, repeat('x', 100000), "+
		"pg_sleep(case when s.id = 6 then 1 else 0 end)::text from (select id from "+e.table+" order by id offset 0) s")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if !rows.Next() {
			t.Fatalf("row %d did not arrive: %v", i+1, rows.Err())
		}
	}
	conn.PgConn().Conn().Close()

	// The proxy reports the read once it sees the connection end.
	var c count
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.gate.FlushReads()
		if c = perRole(e.reads(t))["stet_scram"]; c.Statements > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lost read was never counted")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c.Statements != 1 || c.Rows < 4 {
		t.Errorf("counted %+v, want one statement with at least the 4 rows received", c)
	}
	logs := e.readLog(t)
	if len(logs) != 1 || logs[0].Rows != c.Rows || logs[0].ExecError == nil ||
		!strings.Contains(*logs[0].ExecError, "connection closed") {
		t.Errorf("read_log = %+v, want one %d-row line whose exec_error says the connection closed", logs, c.Rows)
	}
}

// The minute comes from when the statement arrived, so a long read lands in
// the minute it began; created_at is that time too.
func TestReadsAreBucketedByWhenTheyArrived(t *testing.T) {
	e := setupReads(t, readsYAML)
	st := e.gate.Inspect("select * from " + e.table)
	if !st.Observe {
		t.Fatal("a read of a table with reads is not marked to be observed")
	}
	arrived := time.Now().Add(-5 * time.Minute)
	e.gate.Observe(proxy.SessionInfo{ID: "s1", User: "slow_role", ApplicationName: "slow"}, st,
		proxy.Outcome{Executed: true, Sent: 9, Arrived: arrived})

	e.gate.FlushReads()
	rows := e.reads(t)
	if len(rows) != 1 || !rows[0].Bucket.Equal(arrived.Truncate(time.Minute)) || rows[0].LoginRole != "slow_role" {
		t.Errorf("stet.reads = %+v, want one slow_role row in the minute of %v", rows, arrived)
	}
	logs := e.readLog(t)
	if len(logs) != 1 || logs[0].CreatedAt.Sub(arrived).Abs() > time.Microsecond {
		t.Errorf("read_log = %+v, want one line created at %v", logs, arrived)
	}
}

// Every minute the gate writes the minutes that have passed, also one that
// shows up after a later minute was written (a long read).
func TestPastMinutesAreWrittenAsTheyAppear(t *testing.T) {
	defer gate.SetReadFlushInterval(20 * time.Millisecond)()
	e := setupReads(t, readsYAML)
	st := e.gate.Inspect("select * from " + e.table)
	observe := func(ago time.Duration) {
		e.gate.Observe(proxy.SessionInfo{User: "r"}, st, proxy.Outcome{Executed: true, Sent: 1, Arrived: time.Now().Add(-ago)})
	}
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for len(e.reads(t)) < n {
			if time.Now().After(deadline) {
				t.Fatalf("stet.reads has %+v, want %d rows", e.reads(t), n)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	observe(5 * time.Minute)
	waitFor(1)
	observe(10 * time.Minute)
	waitFor(2)

	now := time.Now()
	observe(0)
	time.Sleep(200 * time.Millisecond)
	if rows := e.reads(t); len(rows) != 2 && now.Truncate(time.Minute).Equal(time.Now().Truncate(time.Minute)) {
		t.Errorf("stet.reads = %+v: the current minute was written before it ended", rows)
	}
}

func TestUnwrittenReadsAreWrittenLater(t *testing.T) {
	e := setupReads(t, readsYAML)
	gate.FailReadWrites(e.gate, 1)
	st := e.gate.Inspect("select * from " + e.table)
	observe := func() {
		e.gate.Observe(proxy.SessionInfo{User: "r"}, st, proxy.Outcome{Executed: true, Sent: 2, Arrived: time.Now()})
	}
	observe()
	e.gate.FlushReads() // fails
	if rows := e.reads(t); len(rows) != 0 {
		t.Fatalf("stet.reads = %+v after a failed write", rows)
	}
	observe()
	e.gate.FlushReads()
	wantCounts(t, e.reads(t), map[string]count{"r": {Statements: 2, Rows: 4, MaxRows: 2}})
}

func TestReadsUnwrittenForAnHourAreDropped(t *testing.T) {
	e := setupReads(t, readsYAML)
	var logs lockedBuffer
	gate.SetLogger(e.gate, slog.New(slog.NewTextHandler(&logs, nil)))
	var mu sync.Mutex
	clock := time.Now()
	gate.SetReadClock(e.gate, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	})
	advance := func(d time.Duration) {
		mu.Lock()
		clock = clock.Add(d)
		mu.Unlock()
	}
	gate.FailReadWrites(e.gate, 3)
	st := e.gate.Inspect("select * from " + e.table)
	for _, role := range []string{"a", "b"} {
		e.gate.Observe(proxy.SessionInfo{User: role}, st, proxy.Outcome{Executed: true, Sent: 1, Arrived: time.Now()})
	}

	e.gate.FlushReads() // fails: kept
	advance(59 * time.Minute)
	e.gate.FlushReads() // fails: kept, not yet an hour
	if strings.Contains(logs.String(), "dropped") {
		t.Fatalf("reads dropped before an hour passed:\n%s", logs.String())
	}
	advance(2 * time.Minute)
	e.gate.FlushReads() // fails: dropped
	e.gate.FlushReads() // succeeds, with nothing left to write

	if rows := e.reads(t); len(rows) != 0 {
		t.Errorf("stet.reads = %+v, want the dropped buckets gone", rows)
	}
	var dropped []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "dropped") {
			dropped = append(dropped, line)
		}
	}
	if len(dropped) != 1 || !strings.Contains(dropped[0], "buckets=2") {
		t.Errorf("want one line dropping 2 buckets, got:\n%s", logs.String())
	}
}

// Observe never waits for the store: with every pooled connection taken it
// still returns at once, and large reads past what the queue holds are
// dropped and counted.
func TestObserveDoesNotWaitForASlowStore(t *testing.T) {
	e := setupReads(t, readsYAML)
	var logs lockedBuffer
	gate.SetLogger(e.gate, slog.New(slog.NewTextHandler(&logs, nil)))
	st := e.gate.Inspect("select * from " + e.table)
	release := e.holdPool(t)
	const n = 5000
	start := time.Now()
	for range n {
		e.gate.Observe(proxy.SessionInfo{User: "bulk"}, st, proxy.Outcome{Executed: true, Sent: 10, Arrived: time.Now()})
	}
	elapsed := time.Since(start)
	release()
	if elapsed >= 500*time.Millisecond {
		t.Errorf("%d reads took %v to observe with the pool taken, want under 500ms", n, elapsed)
	}

	e.gate.FlushReads()
	wantCounts(t, e.reads(t), map[string]count{"bulk": {Statements: n, Rows: 10 * n, MaxRows: 10}})
	logged := len(e.readLog(t))
	if logged == 0 || logged == n {
		t.Fatalf("%d of %d large reads logged, want some dropped", logged, n)
	}
	if want := fmt.Sprintf("count=%d", n-logged); !strings.Contains(logs.String(), want) {
		t.Errorf("want a line counting %d dropped large reads, got:\n%s", n-logged, logs.String())
	}
}

// When the final write at Close fails, nothing is left to retry it: the
// buckets are dropped, and the log says so.
func TestCloseDropsWhatItCannotWrite(t *testing.T) {
	e := setupReads(t, readsYAML)
	var logs lockedBuffer
	gate.SetLogger(e.gate, slog.New(slog.NewTextHandler(&logs, nil)))
	gate.FailReadWrites(e.gate, 1)
	st := e.gate.Inspect("select * from " + e.table)
	for _, role := range []string{"a", "b"} {
		e.gate.Observe(proxy.SessionInfo{User: role}, st, proxy.Outcome{Executed: true, Sent: 1, Arrived: time.Now()})
	}
	e.gate.Close()
	if strings.Contains(logs.String(), "retry") || !strings.Contains(logs.String(), "dropped") ||
		!strings.Contains(logs.String(), "buckets=2") {
		t.Errorf("want a line dropping 2 buckets and none promising a retry, got:\n%s", logs.String())
	}
	if rows := e.reads(t); len(rows) != 0 {
		t.Errorf("stet.reads = %+v", rows)
	}
}

// Reads reported after the gate closed are not recorded, but counted, and the
// count is logged in one line. Observe neither panics nor waits.
func TestReadsObservedAfterCloseAreCounted(t *testing.T) {
	e := setupReads(t, readsYAML)
	var logs lockedBuffer
	gate.SetLogger(e.gate, slog.New(slog.NewTextHandler(&logs, nil)))
	st := e.gate.Inspect("select * from " + e.table)
	e.gate.Close()

	start := time.Now()
	for range 3 { // large reads: they would be logged
		e.gate.Observe(proxy.SessionInfo{User: "late"}, st, proxy.Outcome{Executed: true, Sent: 10, Arrived: time.Now()})
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("Observe after Close took %v", elapsed)
	}
	e.gate.FlushReads() // returns at once: nothing writes any more

	const msg = "after the gate closed"
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), msg) {
		if time.Now().After(deadline) {
			t.Fatalf("no line counting the late reads, got:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // a second line would show up by now
	if n := strings.Count(logs.String(), msg); n != 1 || !strings.Contains(logs.String(), "count=3") {
		t.Errorf("want one line counting 3 late reads, got:\n%s", logs.String())
	}
	if rows := e.reads(t); len(rows) != 0 {
		t.Errorf("stet.reads = %+v, want nothing written after Close", rows)
	}
	if l := e.readLog(t); len(l) != 0 {
		t.Errorf("read_log = %+v, want nothing written after Close", l)
	}
}

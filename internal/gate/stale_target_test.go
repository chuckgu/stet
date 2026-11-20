package gate_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/proxy"
)

// The tests below pin which writes are compared at all: the config key of a
// table resolves, on Stet's connection, to one table of one database. A
// write that names another schema's table of that name, or comes from a
// session connected to another database, writes another table, and is not
// compared with what was read of Stet's.

// A write to a table of the same name in another schema is not_checked and
// places no mark; one that names the tracked table's own schema is compared
// as an unqualified one is.
func TestWriteToAnotherSchemaIsNotChecked(t *testing.T) {
	for _, mode := range []string{"shadow", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			e := setupStale(t, calm(), staleConfig(mode))
			ctx := context.Background()
			su := superuser(t)
			schema := fmt.Sprintf("other_%d", time.Now().UnixNano())
			mustExec(t, su, "create schema "+schema)
			t.Cleanup(func() { su.Exec(context.Background(), "drop schema "+schema+" cascade") })
			mustExec(t, su, "create table "+schema+"."+e.table+" (id int primary key, name text, title text)")
			mustExec(t, su, "insert into "+schema+"."+e.table+" values (1, 'Other', 'o')")
			mustExec(t, su, "grant usage on schema "+schema+" to stet_md5")
			mustExec(t, su, "grant all on "+schema+"."+e.table+" to stet_md5")

			a := e.agent(t, "a")
			e.read(t, a, e.table, 1)
			mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")

			// Nobody changed the other schema's row.
			if _, err := a.Exec(ctx, "update "+schema+"."+e.table+" set title = 'elsewhere' where id = 1"); err != nil {
				t.Errorf("the write to the other schema's table was refused: %v", err)
			}
			r := e.checkOf(t, e.table, "'elsewhere'")
			wantRead(t, 0, r, decide.ReadNotChecked)
			if !r.Executed {
				t.Errorf("the write to the other schema's table did not run: %s %v", r.Verdict, r.Reasons)
			}
			if n := gate.MarkCount(e.gate, "stet_md5", e.table); n != 0 {
				t.Errorf("%d marks after a write to another schema's table, want none", n)
			}
			// It forgot nothing either: the role's write of the row it read,
			// which another party changed, is still stale.
			e.idle(t)
			_, err := a.Exec(ctx, "update public."+e.table+" set title = 'here' where id = 1")
			r = e.checkOf(t, e.table, "'here'")
			wantRead(t, 1, r, decide.ReadStale)
			if mode == "enforce" {
				wantRefused(t, err, "ST003", "the row was changed after it was read")
			}

			// Read again, a write that names the table's own schema is fresh.
			e.read(t, a, e.table, 1)
			if _, err := a.Exec(ctx, "update public."+e.table+" set title = 'again' where id = 1"); err != nil {
				t.Errorf("the write after reading again was refused: %v", err)
			}
			r = e.checkOf(t, e.table, "'again'")
			if got := r.ReadCheck["result"]; got != decide.ReadFresh && got != decide.ReadPending {
				t.Errorf("the write that names the table's own schema: read_check %v, want fresh or read_pending", r.ReadCheck)
			}
		})
	}
}

// otherDatabase makes a database besides Stet's, with a table named like the
// env's holding row 1 that stet_md5 may write, and returns its name. It is
// dropped when the test ends.
func otherDatabase(t *testing.T, e *env) string {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("stale_other_%d", time.Now().UnixNano())
	su := superuser(t)
	mustExec(t, su, "create database "+name)
	t.Cleanup(func() { su.Exec(context.Background(), "drop database if exists "+name+" with (force)") })
	conn, err := pgx.Connect(ctx, "postgres://postgres:postgres-pw@"+os.Getenv("STET_TEST_PG")+"/"+name+"?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	mustExec(t, conn, "create table "+e.table+" (id int primary key, name text, title text)")
	mustExec(t, conn, "insert into "+e.table+" values (1, 'Elsewhere', 'e')")
	mustExec(t, conn, "grant all on "+e.table+" to stet_md5")
	return name
}

// connectTo connects through the proxy as stet_md5 to database.
func (e *env) connectTo(t *testing.T, database string) *pgx.Conn {
	t.Helper()
	u := url.URL{Scheme: "postgres", Host: e.addr, Path: "/" + database, User: url.UserPassword("stet_md5", "md5-pw"),
		RawQuery: url.Values{"sslmode": {"disable"}, "application_name": {"elsewhere"}}.Encode()}
	conn, err := pgx.Connect(context.Background(), u.String())
	if err != nil {
		t.Fatalf("connect to %s: %v", database, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// A session connected to another database of the server is outside
// stale-write detection: its reads give the memory nothing and forget
// nothing, its writes are not_checked, and it places no mark.
func TestSessionOnAnotherDatabaseIsNotChecked(t *testing.T) {
	for _, mode := range []string{"shadow", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			e := setupStale(t, calm(), staleConfig(mode))
			ctx := context.Background()
			elsewhere := e.connectTo(t, otherDatabase(t, e))

			// Its read is of another table: nothing is remembered.
			e.readOnly(t, elsewhere, e.table, 1)
			e.idle(t)
			if got := gate.CheckRead(e.gate, "stet_md5", "", e.table, "1", e.xminOf(t, 1)); got.Result != decide.ReadNone {
				t.Errorf("after a read in another database: %+v, want no_read", got)
			}

			// The role reads row 1 in Stet's database, and another party
			// changes it there.
			a := e.agent(t, "a")
			e.read(t, a, e.table, 1)
			mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")

			// In the other database it reads its table by paths that would
			// forget the table here, and writes its row 1, which nobody changed.
			var n int
			if err := elsewhere.QueryRow(ctx, "select count(*) from "+e.table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if mode == "shadow" {
				mustExec(t, elsewhere, "do $$ begin perform 1; end $$")
			}
			if _, err := elsewhere.Exec(ctx, "update "+e.table+" set title = 'elsewhere' where id = 1"); err != nil {
				t.Errorf("the write in the other database was refused: %v", err)
			}
			r := e.checkOf(t, e.table, "'elsewhere'")
			wantRead(t, 0, r, decide.ReadNotChecked)
			if !r.Executed {
				t.Errorf("the write in the other database did not run: %s %v", r.Verdict, r.Reasons)
			}
			if n := gate.MarkCount(e.gate, "stet_md5", e.table); n != 0 {
				t.Errorf("%d marks after a write in another database, want none", n)
			}
			// What the role read here is as it was: its write is stale.
			e.idle(t)
			if got := gate.CheckRead(e.gate, "stet_md5", "", e.table, "1", e.xminOf(t, 1)); got.Result != decide.ReadStale {
				t.Errorf("the role's memory after the other database's session: %+v, want it kept (stale)", got)
			}
		})
	}
}

// A panic in End leaves no mark behind: the write's mark is settled on
// every way out, so the role's later writes of the row are not own_write
// for good.
func TestPanicInEndSettlesTheMark(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	e.insert(t, deals, "id", "1")
	e.start(t, tableBlock(deals, "id", "stale: {}"))
	agent := proxy.SessionInfo{Database: "stet_test", ID: "s", User: "agent"}
	h, refuse := e.gate.Begin(agent, e.gate.Inspect("update "+deals+" set title = 'x' where id = 1"), nil)
	if refuse != nil {
		t.Fatalf("the write was refused: %+v", refuse)
	}
	if n := gate.MarkCount(e.gate, "agent", deals); n != 1 {
		t.Fatalf("%d marks after Begin, want 1", n)
	}
	restore := gate.BreakEnd(e.gate)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("End did not panic")
			}
		}()
		e.gate.End(h, proxy.Outcome{Executed: true, Rows: ptr(int64(1))})
	}()
	restore()
	e.idle(t)
	if n := gate.MarkCount(e.gate, "agent", deals); n != 0 {
		t.Errorf("%d marks after End panicked, want none", n)
	}
	e.wantResult(t, "agent", "", deals, "1", "1", decide.ReadNone)
}

// A transaction of many single-row writes holds a bounded number of marks:
// past staleSessionRowMarks rows of one table, one mark on the table stands
// for them. A connection that ends with something unanswered makes one
// timer for all its marks.
func TestInTransactionRowMarksAreBounded(t *testing.T) {
	defer gate.SetStaleLostGrace(time.Hour)()
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	notes := e.create(t, "notes", "(id int primary key, title text)")
	e.start(t, tableBlock(deals, "id", "stale: {}"), tableBlock(notes, "id", "stale: {}"))
	inTx := proxy.Outcome{Executed: true, InTransaction: true, Rows: ptr(int64(0))}
	write := func(s proxy.SessionInfo, table string, id int) {
		t.Helper()
		h, refuse := e.gate.Begin(s, e.gate.Inspect(fmt.Sprintf("update %s set title = 'x' where id = %d", table, id)), nil)
		if refuse != nil {
			t.Fatalf("the write was refused: %+v", refuse)
		}
		e.gate.End(h, inTx)
	}
	one := proxy.SessionInfo{Database: "stet_test", ID: "one", User: "agent"}
	for id := range gate.SessionRowMarks {
		write(one, deals, id)
	}
	for id := range 5 {
		write(one, notes, id)
	}
	if n, m := gate.MarkCount(e.gate, "agent", deals), gate.MarkCount(e.gate, "agent", notes); n != gate.SessionRowMarks || m != 5 {
		t.Fatalf("%d and %d marks held, want one for each row written: %d and 5", n, m, gate.SessionRowMarks)
	}
	write(one, deals, gate.SessionRowMarks) // one row more
	for id := range 100 {
		write(one, deals, 5000+id)
	}
	if n, m := gate.MarkCount(e.gate, "agent", deals), gate.MarkCount(e.gate, "agent", notes); n != 1 || m != 5 {
		t.Errorf("%d and %d marks held past the limit, want 1 on the table and the other table's 5", n, m)
	}
	e.wantResult(t, "agent", "", deals, "7", "1", decide.ReadOwnWrite)
	e.wantResult(t, "agent", "", deals, "999999", "1", decide.ReadOwnWrite) // the table's mark covers every row

	// The connection ends with something unanswered: the marks wait out
	// their grace on one timer.
	e.gate.TxEnded(one, 0, true)
	if timers, n := gate.LostTimers(e.gate), gate.MarkCount(e.gate, "agent", deals)+gate.MarkCount(e.gate, "agent", notes); timers != 1 || n != 6 {
		t.Errorf("%d timers for %d marks waiting, want 1 for 6", timers, n)
	}
	gate.ExpireLostMarks(e.gate)
	if timers, n := gate.LostTimers(e.gate), gate.MarkCount(e.gate, "agent", deals)+gate.MarkCount(e.gate, "agent", notes); timers != 0 || n != 0 {
		t.Errorf("%d timers and %d marks after the grace, want none", timers, n)
	}
	e.wantResult(t, "agent", "", deals, "7", "1", decide.ReadNone)
}

package gate_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/proxy"
)

// The tests below pin what a read Stet saw but could not take in does
// (spec-v0.2-stale 4, 5): the role read row 1, another party changed it, and
// the role read it again by a path that gives the memory no version of it.
// The write that follows is based on that newer read, so it is not stale; it
// is no_read, the memory of the row or of the table having been forgotten.

// tableOID is the OID of the env's table.
func (e *env) tableOID(t *testing.T) uint32 {
	t.Helper()
	var oid uint32
	if err := e.admin.QueryRow(context.Background(), "select $1::regclass::oid", e.table).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	return oid
}

// otherRoleRead has another login role read row 1 of the env's table, as the
// proxy would report it, and waits for its lookup.
func (e *env) otherRoleRead(t *testing.T) {
	t.Helper()
	e.gate.Observe(proxy.SessionInfo{Database: "stet_test", ID: "other", User: "other"},
		e.gate.Inspect("select id from "+e.table), keyed(e.tableOID(t), "1"))
	e.idle(t)
}

// wantOtherRoleUntouched fails the test unless the other login role still
// holds what it read of row 1, which another party changed since: its write
// would be stale.
func (e *env) wantOtherRoleUntouched(t *testing.T) {
	t.Helper()
	if got := gate.CheckRead(e.gate, "other", "", e.table, "1", e.xminOf(t, 1)); got.Result != decide.ReadStale {
		t.Errorf("the other role's memory of row 1: %+v, want it kept (stale)", got)
	}
}

// rereadThenWrite runs, in enforce mode: the role reads row 1, another party
// changes it, the role reads it again as reread does, and writes it. The
// write must run, with the read check want. prep runs before any read.
func rereadThenWrite(t *testing.T, name, want string, prep func(e *env), reread func(e *env, a *pgx.Conn)) {
	t.Run(name, func(t *testing.T) {
		e := setupStale(t, calm(), staleConfig("enforce"))
		if prep != nil {
			prep(e)
		}
		a := e.agent(t, "a")
		e.otherRoleRead(t)
		e.read(t, a, e.table, 1)
		mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")
		reread(e, a)
		e.idle(t)
		if _, err := a.Exec(context.Background(), "update "+e.table+" set title = 'reread' where id = 1"); err != nil {
			t.Errorf("the write after the re-read was refused: %v", err)
		}
		r := e.checkOf(t, e.table, "'reread'")
		wantRead(t, 0, r, want)
		if !r.Executed {
			t.Errorf("the write did not run: verdict %s %v", r.Verdict, r.Reasons)
		}
		e.wantOtherRoleUntouched(t)
	})
}

func TestRereadNotTakenInIsNotStale(t *testing.T) {
	ctx := context.Background()

	var fail atomic.Bool
	rereadThenWrite(t, "its lookup fails", decide.ReadNone, func(e *env) {
		gate.OnLookup(e.gate, func(context.Context, string, []string) error {
			if fail.Load() {
				return errors.New("lookup made to fail by the test")
			}
			return nil
		})
	}, func(e *env, a *pgx.Conn) {
		fail.Store(true)
		e.read(t, a, e.table, 1)
		fail.Store(false)
	})

	var slow atomic.Bool
	rereadThenWrite(t, "its lookup runs out of time", decide.ReadNone, func(e *env) {
		gate.OnLookup(e.gate, func(ctx context.Context, _ string, _ []string) error {
			if slow.Load() {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		})
	}, func(e *env, a *pgx.Conn) {
		gate.SetLookupTimeout(e.gate, 20*time.Millisecond)
		slow.Store(true)
		e.read(t, a, e.table, 1)
		slow.Store(false)
		gate.SetLookupTimeout(e.gate, time.Second)
	})

	rereadThenWrite(t, "the row lies past the keys one read takes", decide.ReadNone, func(e *env) {
		mustExec(t, e.admin, fmt.Sprintf("insert into %s (id, name) select g, 'n' from generate_series(1001, %d) g",
			e.table, 1000+proxy.MaxReadKeys))
	}, func(e *env, a *pgx.Conn) {
		// Rows 2000 down to 1001, then 2 and 1: 1,002 keys, row 1 the last.
		rows, err := a.Query(ctx, "select id, name, title from "+e.table+" order by id desc")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			n++
		}
		if rows.Close(); rows.Err() != nil || n != proxy.MaxReadKeys+2 {
			t.Fatalf("listed %d rows (%v), want %d", n, rows.Err(), proxy.MaxReadKeys+2)
		}
		// The listing's own first keys are remembered.
		e.idle(t)
		top := strconv.Itoa(1000 + proxy.MaxReadKeys)
		if got := gate.CheckRead(e.gate, "stet_md5", "", e.table, top, e.xminWhere(t, e.table, "id", top)); got.Result != decide.ReadFresh {
			t.Errorf("the first row the listing sent: %+v, want fresh", got)
		}
	})

	rereadThenWrite(t, "through a cursor", decide.ReadNone, nil, func(e *env, a *pgx.Conn) {
		mustExec(t, a, "begin")
		mustExec(t, a, "declare c cursor for select * from "+e.table+" where id = 1")
		rows, err := a.Query(ctx, "fetch all from c")
		if err != nil {
			t.Fatal(err)
		}
		rows.Close()
		mustExec(t, a, "commit")
	})

	rereadThenWrite(t, "in a transaction after a message that is not read-only", decide.ReadNone, nil, func(e *env, a *pgx.Conn) {
		mustExec(t, a, "begin")
		mustExec(t, a, "create temp table if not exists reread_note (x int) on commit drop")
		var name string
		if err := a.QueryRow(ctx, "select name from "+e.table+" where id = 1").Scan(&name); err != nil {
			t.Fatal(err)
		}
		mustExec(t, a, "commit")
	})

	rereadThenWrite(t, "by a key value that cannot be used, the key not in the result", decide.ReadNone, nil, func(e *env, a *pgx.Conn) {
		var name string
		err := a.QueryRow(ctx, "select name from "+e.table+" where id = '1'::text::int", pgx.QueryExecModeSimpleProtocol).Scan(&name)
		if err != nil {
			t.Fatal(err)
		}
	})

	rereadThenWrite(t, "by a statement whose columns were never described", decide.ReadNone, nil, func(e *env, a *pgx.Conn) {
		// As the proxy reports an Execute of such a statement: rows sent, no Read.
		o := keyed(e.tableOID(t), "1")
		o.Read = nil
		e.gate.Observe(proxy.SessionInfo{Database: "stet_test", ID: "raw", User: "stet_md5"},
			e.gate.Inspect("select id from "+e.table+" where id = 1"), o)
	})
}

// A read that sent no rows showed its client nothing: the memory stays, and
// the write of the row another party changed is stale.
func TestReadThatSentNoRowsForgetsNothing(t *testing.T) {
	e := setupStale(t, calm(), staleConfig("enforce"))
	ctx := context.Background()
	a := e.agent(t, "a")
	e.read(t, a, e.table, 1)
	mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")

	// No row matches; through a cursor that fetches nothing; a failed read.
	rows, err := a.Query(ctx, "select name from "+e.table+" where id = '99'::text::int", pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	mustExec(t, a, "begin")
	mustExec(t, a, "declare c cursor for select * from "+e.table+" where id = 99")
	mustExec(t, a, "fetch all from c")
	mustExec(t, a, "commit")
	if _, err := a.Exec(ctx, "select name, 1/(id-1) from "+e.table+" where id = 1"); err == nil {
		t.Fatal("the read that divides by zero did not fail")
	}
	e.idle(t)

	_, err = a.Exec(ctx, "update "+e.table+" set title = 'x' where id = 1")
	wantRefused(t, err, "ST003", "the row was changed after it was read")
	wantRead(t, 0, e.checkOf(t, e.table, "title = 'x'"), decide.ReadStale)
}

// One role's burst of reads fills the queue; another role's re-read is
// dropped for it. That role's write, based on the re-read, is not stale.
func TestFullQueueDoesNotMakeAnotherRolesWriteStale(t *testing.T) {
	e := setupStale(t, calm(), staleConfig("enforce"))
	hold := holdLookups(t, e.gate)
	a := e.agent(t, "a")
	e.read(t, a, e.table, 1)
	mustExec(t, e.admin, "update "+e.table+" set name = 'Admin' where id = 1")

	hold.hold()
	noisy := proxy.SessionInfo{Database: "stet_test", ID: "noisy", User: "noisy"}
	st, oid := e.gate.Inspect("select id from "+e.table), e.tableOID(t)
	for i := 0; ; i++ {
		if i > 2000 {
			t.Fatal("the queue never filled")
		}
		e.gate.Observe(noisy, st, keyed(oid, strconv.Itoa(1000+i)))
		if _, _, dropped := gate.StaleQueue(e.gate); dropped > 0 {
			break
		}
	}
	_, _, before := gate.StaleQueue(e.gate)
	e.readOnly(t, a, e.table, 1) // dropped: the queue is full
	if _, _, dropped := gate.StaleQueue(e.gate); dropped != before+1 {
		t.Fatalf("the re-read was not turned away: %d dropped, want %d", dropped, before+1)
	}
	if _, err := a.Exec(context.Background(), "update "+e.table+" set title = 'reread' where id = 1"); err != nil {
		t.Errorf("the write after the re-read was refused: %v", err)
	}
	wantRead(t, 0, e.checkOf(t, e.table, "'reread'"), decide.ReadNone)
	hold.release()
	e.idle(t)
}

// A read still being looked up when the gate closes is dropped: what its role
// held of its row is forgotten, not left to decide.
func TestCloseForgetsWhatPendingReadsShowed(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key)")
	e.insert(t, deals, "id", "1", "2")
	e.start(t, tableBlock(deals, "id", "stale: {}"))
	oid := e.oid(t, deals)
	agent := proxy.SessionInfo{Database: "stet_test", User: "agent"}
	st := e.gate.Inspect("select id from " + deals)
	e.gate.Observe(agent, st, keyed(oid, "1", "2"))
	e.idle(t)

	gate.SetLookupTimeout(e.gate, time.Minute)
	release := blockLookups(t, e.gate)
	defer release()
	for range 3 { // two running, one waiting
		e.gate.Observe(agent, st, keyed(oid, "1"))
	}
	e.wantResult(t, "agent", "", deals, "1", "9", decide.ReadPending)
	e.gate.Close()
	e.wantResult(t, "agent", "", deals, "1", "9", decide.ReadNone)
	e.wantResult(t, "agent", "", deals, "2", "9", decide.ReadStale) // a row those reads did not show
}

// The lookups waiting hold a bounded number of key values, not only a
// bounded number of lookups: a read that would take them past it is dropped,
// as one that finds the queue full is.
func TestQueueHoldsABoundedNumberOfKeys(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key)")
	e.start(t, tableBlock(deals, "id", "stale: {}"))
	gate.SetLookupTimeout(e.gate, time.Minute)
	release := blockLookups(t, e.gate)
	oid := e.oid(t, deals)
	st := e.gate.Inspect("select id from " + deals)
	agent := proxy.SessionInfo{Database: "stet_test", User: "agent"}
	listing := func(from int) proxy.Outcome {
		keys := make([]string, proxy.MaxReadKeys)
		for i := range keys {
			keys[i] = strconv.Itoa(from + i)
		}
		return keyed(oid, keys...)
	}

	// Two lookups run and hold no place; then the queue takes listings of a
	// thousand keys until it holds staleQueueKeys.
	fits := gate.StaleQueueKeys / proxy.MaxReadKeys
	for i := range 2 + fits {
		e.gate.Observe(agent, st, listing(i*proxy.MaxReadKeys))
	}
	waitUntil(t, "two lookups run", func() bool { n, _, _ := gate.StaleQueue(e.gate); return n == fits })
	if lookups, keys, dropped := gate.StaleQueue(e.gate); lookups != fits || keys != gate.StaleQueueKeys || dropped != 0 {
		t.Fatalf("queue: %d lookups, %d keys, %d dropped; want %d, %d, 0", lookups, keys, dropped, fits, gate.StaleQueueKeys)
	}
	// One key more is turned away, though the queue has room for lookups.
	e.gate.Observe(agent, st, keyed(oid, "-1"))
	if lookups, keys, dropped := gate.StaleQueue(e.gate); lookups != fits || keys != gate.StaleQueueKeys || dropped != 1 {
		t.Errorf("after one more read: %d lookups, %d keys, %d dropped; want %d, %d, 1", lookups, keys, dropped, fits, gate.StaleQueueKeys)
	}
	e.wantResult(t, "agent", "", deals, "-1", "1", decide.ReadNone)
	e.wantResult(t, "agent", "", deals, "0", "1", decide.ReadPending)
	release()
	e.idle(t)
	if lookups, keys, _ := gate.StaleQueue(e.gate); lookups != 0 || keys != 0 {
		t.Errorf("after the lookups ran: %d lookups and %d keys wait, want none", lookups, keys)
	}
}

// Two values of stet.on_behalf_of that differ only after the bytes the
// memory groups by are one party: a read under one is compared with a write
// under the other.
func TestLongTasksAreOnePartyByTheirStart(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key)")
	e.insert(t, deals, "id", "1")
	e.start(t, tableBlock(deals, "id", "stale: {}"))
	long := fmt.Sprintf("%0256d", 0)
	reader := proxy.SessionInfo{Database: "stet_test", User: "agent", OnBehalfOf: long + "-reader"}
	e.gate.Observe(reader, e.gate.Inspect("select id from "+deals), keyed(e.oid(t, deals), "1"))
	e.idle(t)
	v := e.xmin(t, deals, "id", "1")
	got := gate.CompareWrite(e.gate, "agent", long+"-writer", deals, "1", v, time.Now())
	if got.Result != decide.ReadFresh || got.ReadOnBehalfOf == nil || *got.ReadOnBehalfOf != long {
		t.Errorf("a write under a task that differs after byte 256: %+v, want fresh, read under the first 256 bytes", got)
	}
	e.wantResult(t, "agent", "another task", deals, "1", v, decide.ReadNone)
}

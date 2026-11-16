package proxy_test

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/proxy"
)

var writeRE = regexp.MustCompile(`(?i)^\s*(insert|update|delete)\b`)

func isWrite(sql string) bool { return writeRE.MatchString(sql) }

// recordingGate is a stand-in for the real gate: it watches statements
// chosen by match and records every call.
type recordingGate struct {
	match   func(sql string) bool
	onBegin func(st proxy.Statement) // runs inside Begin, before the write is forwarded

	mu     sync.Mutex
	begins []began
	ends   map[int]proxy.Outcome
}

type began struct {
	info   proxy.SessionInfo
	st     proxy.Statement
	params []proxy.Param
}

func newRecordingGate(match func(string) bool) *recordingGate {
	return &recordingGate{match: match, ends: map[int]proxy.Outcome{}}
}

var oboRE = regexp.MustCompile(`(?i)^\s*(?:set\s+stet\.on_behalf_of\s*=\s*'([^']*)'|reset\s+stet\.on_behalf_of)`)

func (g *recordingGate) Inspect(sql string) proxy.Statement {
	st := proxy.Statement{SQL: sql, Watch: g.match(sql)}
	if m := oboRE.FindStringSubmatch(sql); m != nil {
		st.OnBehalfOf = &m[1]
	}
	return st
}

func (g *recordingGate) Begin(info proxy.SessionInfo, st proxy.Statement, params []proxy.Param) any {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.onBegin != nil {
		g.onBegin(st)
	}
	g.begins = append(g.begins, began{info, st, params})
	return len(g.begins) - 1
}

func (g *recordingGate) End(h any, o proxy.Outcome) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ends[h.(int)] = o
}

// outcomes waits until n writes have ended and returns them in Begin order.
func (g *recordingGate) outcomes(t *testing.T, n int) ([]began, []proxy.Outcome) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.mu.Lock()
		if len(g.ends) >= n {
			bs := append([]began(nil), g.begins...)
			os := make([]proxy.Outcome, len(bs))
			for i := range bs {
				os[i] = g.ends[i]
			}
			g.mu.Unlock()
			return bs, os
		}
		got := len(g.ends)
		g.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("%d writes ended, want %d", got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// watchedTable creates a fresh table the gate watches and drops it afterwards.
func watchedTable(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("watched_%d", time.Now().UnixNano())
	admin := connect(t, upstream(t), "stet_scram", nil)
	mustExec(t, admin, "create table "+name+" (id int primary key, v text)")
	t.Cleanup(func() { admin.Exec(context.Background(), "drop table "+name) })
	return name
}

func watches(table string) func(string) bool {
	return func(sql string) bool { return isWrite(sql) && strings.Contains(sql, table) }
}

func TestGateSeesOutcomes(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", url.Values{"application_name": {"agent-a"}})
	ctx := testCtx(t)
	ins := "insert into " + table + " values ($1, $2)"

	if _, err := conn.Exec(ctx, ins, 1, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, ins, 1, "dup"); pgCode(err) != "23505" {
		t.Fatalf("duplicate insert: %v", err)
	}
	if _, err := conn.PgConn().Exec(ctx, "insert into "+table+" values (2, 'b')").ReadAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.PgConn().Exec(ctx, "insert into "+table+" values (2, 'b')").ReadAll(); pgCode(err) != "23505" {
		t.Fatalf("duplicate simple insert: %v", err)
	}
	mustExec(t, conn, "select * from "+table) // not watched

	bs, os := g.outcomes(t, 4)
	if len(bs) != 4 {
		t.Fatalf("%d writes began, want 4", len(bs))
	}
	for i, want := range []bool{true, false, true, false} {
		o := os[i]
		if o.Executed != want || o.InTransaction {
			t.Errorf("write %d: %+v, want executed=%v outside a transaction", i, o, want)
		}
		if !want && !strings.Contains(o.Error, "23505") {
			t.Errorf("write %d: error %q, want SQLSTATE 23505", i, o.Error)
		}
	}

	info := bs[0].info
	if info.ID == "" || info.User != "stet_scram" || info.Database != "stet_test" || info.ApplicationName != "agent-a" {
		t.Errorf("session info = %+v", info)
	}
	if bs[0].st.SQL != ins {
		t.Errorf("statement = %q", bs[0].st.SQL)
	}
	if p := bs[0].params; len(p) != 2 || string(p[1].Value) != "a" {
		t.Errorf("params = %+v, want 2 with the second = a", p)
	}
	if bs[2].params != nil {
		t.Errorf("simple query params = %+v, want none", bs[2].params)
	}
}

func TestGateSeesTransactions(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "insert into "+table+" values (1, 'in tx')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// A batch is pipelined under one Sync: an implicit transaction.
	b := &pgx.Batch{}
	b.Queue("insert into "+table+" values ($1, 'batch')", 2)
	b.Queue("insert into "+table+" values ($1, 'batch')", 3)
	if err := conn.SendBatch(ctx, b).Close(); err != nil {
		t.Fatal(err)
	}

	_, os := g.outcomes(t, 3)
	for i, o := range os {
		if !o.Executed || !o.InTransaction {
			t.Errorf("write %d: %+v, want executed inside a transaction", i, o)
		}
	}
}

func TestGateSeesWritesSkippedAfterError(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)
	mustExec(t, conn, "insert into "+table+" values (1, 'first')")

	b := &pgx.Batch{}
	b.Queue("insert into "+table+" values ($1, 'dup')", 1)
	b.Queue("insert into "+table+" values ($1, 'never runs')", 50)
	conn.SendBatch(ctx, b).Close()

	_, os := g.outcomes(t, 3)
	if o := os[1]; o.Executed || !strings.Contains(o.Error, "23505") {
		t.Errorf("failing write: %+v", o)
	}
	if o := os[2]; o.Executed || !strings.Contains(o.Error, "not executed") {
		t.Errorf("skipped write: %+v", o)
	}
}

func TestGateBeginSeesStateBeforeTheWrite(t *testing.T) {
	table := watchedTable(t)
	admin := connect(t, upstream(t), "stet_scram", nil)
	mustExec(t, admin, "insert into "+table+" values (10, 'old')")

	g := newRecordingGate(watches(table))
	var seen string
	g.onBegin = func(proxy.Statement) {
		admin.QueryRow(context.Background(), "select v from "+table+" where id = 10").Scan(&seen)
	}
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	mustExec(t, conn, "update "+table+" set v = 'new' where id = 10")
	g.outcomes(t, 1)

	g.mu.Lock()
	defer g.mu.Unlock()
	if seen != "old" {
		t.Errorf("Begin saw %q, want the pre-write value old", seen)
	}
}

func TestGateTracksOnBehalfOf(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	mustExec(t, conn, "insert into "+table+" values (1, 'nobody')")
	mustExec(t, conn, "set stet.on_behalf_of = 'user:42'")
	mustExec(t, conn, "insert into "+table+" values (2, 'for user 42')")
	mustExec(t, conn, "reset stet.on_behalf_of")
	mustExec(t, conn, "insert into "+table+" values (3, 'nobody again')")

	bs, _ := g.outcomes(t, 3)
	for i, want := range []string{"", "user:42", ""} {
		if got := bs[i].info.OnBehalfOf; got != want {
			t.Errorf("write %d: on_behalf_of = %q, want %q", i, got, want)
		}
	}
}

func TestGateSeesRowCounts(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)

	simple := func(sql string) {
		t.Helper()
		if _, err := conn.PgConn().Exec(ctx, sql).ReadAll(); err != nil {
			t.Fatal(err)
		}
	}
	simple("insert into " + table + " values (1, 'a'), (2, 'a'), (3, 'b')")                         // simple query: 3
	if _, err := conn.Exec(ctx, "update "+table+" set v = $1 where v = $2", "c", "a"); err != nil { // extended: 2
		t.Fatal(err)
	}
	simple("delete from " + table + " where v = 'nobody'")    // 0, not nil
	b := &pgx.Batch{}                                         // two Executes, one Sync
	b.Queue("update "+table+" set v = 'd' where v = $1", "c") // 2
	b.Queue("delete from "+table+" where id = $1", 3)         // 1
	if err := conn.SendBatch(ctx, b).Close(); err != nil {
		t.Fatal(err)
	}
	conn.Exec(ctx, "insert into "+table+" values ($1, 'dup')", 1) // fails: nil

	_, os := g.outcomes(t, 6)
	want := []*int64{ptr(int64(3)), ptr(int64(2)), ptr(int64(0)), ptr(int64(2)), ptr(int64(1)), nil}
	for i, o := range os {
		if (o.Rows == nil) != (want[i] == nil) || (o.Rows != nil && *o.Rows != *want[i]) {
			t.Errorf("write %d: rows %v, want %v", i, deref(o.Rows), deref(want[i]))
		}
	}
}

func TestGateSeesOpenTransaction(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)

	mustExec(t, conn, "insert into "+table+" values (1, 'autocommit')")
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "insert into "+table+" values (2, 'in tx')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, conn, "insert into "+table+" values (3, 'autocommit again')")

	bs, _ := g.outcomes(t, 3)
	for i, want := range []bool{false, true, false} {
		if bs[i].info.TxOpen != want {
			t.Errorf("write %d: TxOpen = %v, want %v", i, bs[i].info.TxOpen, want)
		}
	}
}

func TestGateSeesLostWrites(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)

	// The server refuses this one: it answered, so nothing was lost.
	mustExec(t, conn, "insert into "+table+" values (1, 'a')")
	if _, err := conn.Exec(ctx, "insert into "+table+" values (1, 'dup')"); pgCode(err) != "23505" {
		t.Fatalf("duplicate insert: %v", err)
	}

	// Slow write; the client's network connection is cut while the server
	// is still sleeping, so no answer can reach the proxy first. Exec has
	// written the query before it returns, and the proxy reads the client's
	// bytes in order, so Begin runs before it sees the close.
	conn2 := connect(t, addr, "stet_scram", nil)
	conn2.PgConn().Exec(ctx, "insert into "+table+" select 2, 'x' from pg_sleep(1)")
	conn2.PgConn().Conn().Close()

	_, os := g.outcomes(t, 3)
	if o := os[0]; !o.Executed || o.Lost {
		t.Errorf("answered write: %+v, want executed and not lost", o)
	}
	if o := os[1]; o.Executed || o.Lost || !strings.Contains(o.Error, "23505") {
		t.Errorf("refused write: %+v, want a 23505 error and not lost", o)
	}
	if o := os[2]; o.Executed || !o.Lost {
		t.Errorf("unanswered write: %+v, want lost and not executed", o)
	}
}

func ptr[T any](v T) *T { return &v }

// deref is for messages: the pointed-to value, or "nil".
func deref[T any](p *T) any {
	if p == nil {
		return "nil"
	}
	return *p
}

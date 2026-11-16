package proxy_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/stetdb/stet/internal/proxy"
)

var writeRE = regexp.MustCompile(`(?i)^\s*(insert|update|delete)\b`)

func isWrite(sql string) bool { return writeRE.MatchString(sql) }

// recordingGate is a stand-in for the real gate: it watches statements
// chosen by match and records every call.
type recordingGate struct {
	match   func(sql string) bool
	onBegin func(st proxy.Statement) // runs inside Begin, before the write is forwarded
	// observeMatch chooses the statements observed as reads; nil observes none.
	observeMatch func(sql string) bool

	mu     sync.Mutex
	begins []began
	ends   map[int]proxy.Outcome
	reads  []observation
}

type observation struct {
	info proxy.SessionInfo
	st   proxy.Statement
	o    proxy.Outcome
	at   time.Time // when Observe was called
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
	st := proxy.Statement{SQL: sql, Watch: g.match(sql), Observe: g.observeMatch != nil && g.observeMatch(sql)}
	if m := oboRE.FindStringSubmatch(sql); m != nil {
		st.OnBehalfOf = &m[1]
	}
	if c, query := cursorOf(sql); c != nil {
		st.Cursor = c
		if c.Op == proxy.CursorDeclare { // remembered when its query is an observed read
			st.Observe = g.observeMatch != nil && g.observeMatch(query)
		}
	}
	return st
}

// The test double's view of cursor statements; the real gate parses them.
var (
	declareRE = regexp.MustCompile(`(?is)^\s*declare\s+(\w+)\s+cursor\s+(with\s+hold\s+)?for\s+(.+)$`)
	fetchRE   = regexp.MustCompile(`(?is)^\s*fetch\b.*\bfrom\s+(\w+)\s*$`)
	closeRE   = regexp.MustCompile(`(?is)^\s*close\s+(\w+)\s*$`)
)

// cursorOf says what sql does to a cursor, with a DECLARE's query, or nil.
// Names are lowercased, as the parser normalizes unquoted names.
func cursorOf(sql string) (*proxy.Cursor, string) {
	if m := declareRE.FindStringSubmatch(sql); m != nil {
		return &proxy.Cursor{Op: proxy.CursorDeclare, Name: strings.ToLower(m[1]), Hold: m[2] != ""}, m[3]
	}
	if m := fetchRE.FindStringSubmatch(sql); m != nil {
		return &proxy.Cursor{Op: proxy.CursorFetch, Name: strings.ToLower(m[1])}, ""
	}
	if m := closeRE.FindStringSubmatch(sql); m != nil {
		if strings.EqualFold(m[1], "all") {
			return &proxy.Cursor{Op: proxy.CursorCloseAll}, ""
		}
		return &proxy.Cursor{Op: proxy.CursorClose, Name: strings.ToLower(m[1])}, ""
	}
	return nil, ""
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

func (g *recordingGate) Observe(info proxy.SessionInfo, st proxy.Statement, o proxy.Outcome) {
	at := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads = append(g.reads, observation{info, st, o, at})
}

// observed waits until n reads have been reported and returns every read
// reported so far, in order.
func (g *recordingGate) observed(t *testing.T, n int) []observation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g.mu.Lock()
		if len(g.reads) >= n {
			obs := append([]observation(nil), g.reads...)
			g.mu.Unlock()
			return obs
		}
		got := len(g.reads)
		g.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("%d reads observed, want %d", got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (g *recordingGate) beginCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.begins)
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

// readTable creates a watched table holding ids 1 to 1000.
func readTable(t *testing.T) string {
	t.Helper()
	table := watchedTable(t)
	admin := connect(t, upstream(t), "stet_scram", nil)
	mustExec(t, admin, "insert into "+table+" select g, 'v' || g from generate_series(1, 1000) g")
	return table
}

// readsOf chooses the statements that mention table without writing it.
func readsOf(table string) func(string) bool {
	return func(sql string) bool { return !isWrite(sql) && strings.Contains(sql, table) }
}

// newObservingGate watches every write and observes the reads observe picks.
func newObservingGate(observe func(string) bool) *recordingGate {
	g := newRecordingGate(isWrite)
	g.observeMatch = observe
	return g
}

// slowRead sends a row every millisecond or more for well over a minute, so
// it is still sending when a test cancels it or drops the connection. The
// padding fills the server's output buffer, which it flushes when full, in a
// few dozen rows.
const slowRead = "select i, repeat('x', 200), pg_sleep(0.001) from generate_series(1, 100000) i"

func countRows(t *testing.T, rows pgx.Rows) int {
	t.Helper()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// The protocol scenarios once more with every statement that is not a write
// observed as a read, so counting rows is proven not to change traffic.
func TestObservedPassthrough(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			g := newObservingGate(func(sql string) bool { return !isWrite(sql) })
			addr, srv := startProxy(t, upstream(t), g)
			sc.run(t, addr, srv)
		})
	}
}

func TestGateObservesReads(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", url.Values{"application_name": {"agent-r"}})
	ctx := testCtx(t)

	if _, err := conn.PgConn().Exec(ctx, "select * from "+table).ReadAll(); err != nil { // simple protocol
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, "select * from "+table+" where id <= $1", 250) // extended protocol
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, rows); n != 250 {
		t.Fatalf("client got %d rows, want 250", n)
	}
	var out bytes.Buffer
	if _, err := conn.PgConn().CopyTo(ctx, &out, "copy "+table+" to stdout"); err != nil {
		t.Fatal(err)
	}
	mustExec(t, conn, "select * from "+table+" where id < 0")

	obs := g.observed(t, 4)
	if len(obs) != 4 {
		t.Fatalf("%d reads observed, want 4", len(obs))
	}
	for i, want := range []int64{1000, 250, 1000, 0} {
		if o := obs[i].o; o.Sent != want || !o.Executed || o.Lost || o.Error != "" {
			t.Errorf("read %d (%s): %+v, want %d rows sent and executed", i, obs[i].st.SQL, o, want)
		}
	}
	if sql := obs[1].st.SQL; sql != "select * from "+table+" where id <= $1" {
		t.Errorf("extended read statement = %q", sql)
	}
	for i, ob := range obs {
		if info := ob.info; info.ID == "" || info.User != "stet_scram" || info.ApplicationName != "agent-r" {
			t.Errorf("read %d: session info = %+v", i, info)
		}
	}
	if n := g.beginCount(); n != 0 {
		t.Errorf("Begin ran %d times for reads, want never", n)
	}
}

func TestGateObservesRowsOfAFailedRead(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)

	// The 500th row divides by zero, after the rows before it have gone out.
	sql := "select id, 1 / (500 - id) from " + table + " order by id"
	rows, err := conn.Query(ctx, sql) // extended protocol
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if code := pgCode(rows.Err()); code != "22012" {
		t.Fatalf("extended read: %v, want 22012", rows.Err())
	}
	if _, err := conn.PgConn().Exec(ctx, sql).ReadAll(); pgCode(err) != "22012" { // simple protocol
		t.Fatalf("simple read: %v, want 22012", err)
	}

	for i, ob := range g.observed(t, 2) {
		if o := ob.o; o.Executed || !strings.Contains(o.Error, "22012") || o.Sent <= 0 || o.Sent >= 500 {
			t.Errorf("read %d: %+v, want a 22012 error after more than 0 and fewer than 500 rows", i, o)
		}
	}
}

func TestGateObservesRowsOfACancelledRead(t *testing.T) {
	g := newObservingGate(func(sql string) bool { return sql == slowRead })
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	rows, err := conn.Query(ctx, slowRead)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("no row arrived: %v", rows.Err())
	}
	cancel()
	for rows.Next() {
	}
	rows.Close()

	// pgx may end the read with the server's 57014 or by dropping the
	// connection; either way rows went out and the read did not complete.
	if o := g.observed(t, 1)[0].o; o.Sent <= 0 || o.Executed {
		t.Errorf("cancelled read: %+v, want rows sent and not executed", o)
	}

	// A CancelRequest, as psql sends on ^C: the server answers with 57014.
	conn2 := connect(t, addr, "stet_scram", nil)
	rows, err = conn2.Query(testCtx(t), slowRead)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("no row arrived: %v", rows.Err())
	}
	if err := conn2.PgConn().CancelRequest(testCtx(t)); err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if code := pgCode(rows.Err()); code != "57014" {
		t.Fatalf("read after CancelRequest: %v, want 57014", rows.Err())
	}
	if o := g.observed(t, 2)[1].o; o.Sent <= 0 || o.Executed || o.Lost || !strings.Contains(o.Error, "57014") {
		t.Errorf("read cancelled by the server: %+v, want rows sent and the 57014 error", o)
	}
}

func TestGateObservesPortalFetchedInParts(t *testing.T) {
	table := readTable(t)
	admin := connect(t, upstream(t), "stet_scram", nil)
	mustExec(t, admin, "grant select on "+table+" to stet_trust") // dropped with the table
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)

	fe := rawSession(t, addr, "stet_trust")
	fe.Send(&pgproto3.Parse{Query: "select * from " + table + " order by id"})
	fe.Send(&pgproto3.Bind{})
	for range 3 {
		fe.Send(&pgproto3.Execute{MaxRows: 100})
	}
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if rows, suspended := untilReady(t, fe); rows != 300 || suspended != 3 {
		t.Fatalf("client got %d rows and %d PortalSuspended, want 300 and 3", rows, suspended)
	}

	obs := g.observed(t, 3)
	if len(obs) != 3 {
		t.Fatalf("%d reads observed, want one per Execute", len(obs))
	}
	for i, ob := range obs {
		if o := ob.o; o.Sent != 100 || !o.Executed {
			t.Errorf("Execute %d: %+v, want 100 rows sent and executed", i+1, o)
		}
	}
}

func TestGateObservesReadsInAMultiStatementQuery(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	if _, err := conn.PgConn().Exec(testCtx(t), "select 1; select * from "+table).ReadAll(); err != nil {
		t.Fatal(err)
	}
	obs := g.observed(t, 1)
	if len(obs) != 1 {
		t.Fatalf("%d reads observed, want 1 for the Query", len(obs))
	}
	if o := obs[0].o; o.Sent != 1001 || !o.Executed {
		t.Errorf("multi-statement read: %+v, want 1001 rows sent and executed", o)
	}
}

func TestGateObservesALostRead(t *testing.T) {
	g := newObservingGate(func(sql string) bool { return sql == slowRead })
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	rows, err := conn.Query(testCtx(t), slowRead)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no row arrived: %v", rows.Err())
	}
	// The server is still sending, so no ReadyForQuery can reach the proxy
	// before it sees the close.
	conn.PgConn().Conn().Close()

	if o := g.observed(t, 1)[0].o; !o.Lost || o.Executed || o.Sent <= 0 {
		t.Errorf("read cut off by the client: %+v, want lost with rows sent", o)
	}
}

// The proxy keeping no copy of bind values is pinned by the internal
// TestObservedPortalKeepsNoBindValues; this checks that none reach the gate.
func TestObservedReadsKeepNoBindValues(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	sql := "select * from " + table + " where v = $1"
	rows, err := conn.Query(testCtx(t), sql, "v7")
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, rows); n != 1 {
		t.Fatalf("client got %d rows, want 1", n)
	}

	obs := g.observed(t, 1)
	if st := obs[0].st; st.SQL != sql || !st.Observe || st.Watch {
		t.Errorf("observed statement = %+v, want the observed read %q", st, sql)
	}
	if o := obs[0].o; o.Sent != 1 || !o.Executed {
		t.Errorf("read: %+v, want 1 row sent and executed", o)
	}
	// Bind values reach a gate only through Begin.
	if n := g.beginCount(); n != 0 {
		t.Errorf("Begin ran %d times for a read, want never", n)
	}
}

func TestGateSeesReadsInsideATransaction(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)
	extended := func(id int) {
		t.Helper()
		rows, err := conn.Query(ctx, "select * from "+table+" where id = $1", id)
		if err != nil {
			t.Fatal(err)
		}
		countRows(t, rows)
	}

	mustExec(t, conn, "select * from "+table+" where id = 1")
	mustExec(t, conn, "begin")
	mustExec(t, conn, "select * from "+table+" where id = 2")
	mustExec(t, conn, "declare c cursor for select * from "+table)
	mustExec(t, conn, "fetch 3 from c")
	extended(4)
	mustExec(t, conn, "commit")
	extended(5)

	obs := g.observed(t, 5)
	for i, want := range []bool{false, true, true, true, false} {
		if got := obs[i].info.TxOpen; got != want {
			t.Errorf("read %d (%s): TxOpen = %v, want %v", i, obs[i].st.SQL, got, want)
		}
	}
}

func TestGateSeesWhenReadsArrived(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	const runs = 200 * time.Millisecond // the pg_sleep in each read
	slow := "select t.id, pg_sleep(0.2) from " + table + " t where id = "

	var sent []time.Time // when each read left the client
	sent = append(sent, time.Now())
	mustExec(t, conn, slow+"1") // simple protocol
	sent = append(sent, time.Now())
	rows, err := conn.Query(testCtx(t), slow+"$1", 2) // extended protocol
	if err != nil {
		t.Fatal(err)
	}
	countRows(t, rows)
	// A FETCH long after its DECLARE: the cursor's query runs in the FETCH.
	mustExec(t, conn, "begin")
	mustExec(t, conn, "declare c cursor for "+slow+"3")
	time.Sleep(runs)
	sent = append(sent, time.Now())
	mustExec(t, conn, "fetch 1 from c")
	mustExec(t, conn, "commit")

	for i, ob := range g.observed(t, 3) {
		switch a := ob.o.Arrived; {
		case a.IsZero():
			t.Errorf("read %d: no arrival time", i)
		case a.Before(sent[i]) || a.After(ob.at):
			t.Errorf("read %d arrived at %v, want between when it was sent (%v) and when Observe ran (%v)", i, a, sent[i], ob.at)
		case ob.at.Sub(a) < runs:
			t.Errorf("read %d: Observe ran %v after it arrived, want at least the %v it ran for", i, ob.at.Sub(a), runs)
		}
	}
}

// A client that sends a statement before it authenticated has not proved it
// is the role its startup packet names, so the gate gets no login role for
// it. Once the session has authenticated, the role is passed.
func TestLoginRoleIsPassedOnlyOnceAuthenticated(t *testing.T) {
	table := readTable(t)
	// unauthenticated sends the startup packet of a role that needs a
	// password, then sql instead of the password, and waits for the end.
	unauthenticated := func(t *testing.T, addr, sql string) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		fe := pgproto3.NewFrontend(conn, conn)
		fe.Send(&pgproto3.StartupMessage{
			ProtocolVersion: pgproto3.ProtocolVersionNumber,
			Parameters:      map[string]string{"user": "stet_scram", "database": "stet_test"},
		})
		fe.Send(&pgproto3.Query{String: sql})
		if err := fe.Flush(); err != nil {
			t.Fatal(err)
		}
		for {
			msg, err := fe.Receive()
			if err != nil {
				return // the server hung up
			}
			if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
				t.Fatalf("the session became ready without a password")
			}
		}
	}

	t.Run("watched write", func(t *testing.T) {
		g := newRecordingGate(watches(table))
		addr, _ := startProxy(t, upstream(t), g)
		unauthenticated(t, addr, "insert into "+table+" values (5000, 'forged')")
		mustExec(t, connect(t, addr, "stet_scram", nil), "insert into "+table+" values (5001, 'signed in')")

		bs, _ := g.outcomes(t, 2)
		for i, want := range []string{"", "stet_scram"} {
			if got := bs[i].info.User; got != want {
				t.Errorf("write %d (%s): login role %q, want %q", i, bs[i].st.SQL, got, want)
			}
		}
	})
	t.Run("observed read", func(t *testing.T) {
		g := newObservingGate(readsOf(table))
		addr, _ := startProxy(t, upstream(t), g)
		unauthenticated(t, addr, "select * from "+table)
		mustExec(t, connect(t, addr, "stet_scram", nil), "select * from "+table+" where id = 1")

		obs := g.observed(t, 2)
		for i, want := range []string{"", "stet_scram"} {
			if got := obs[i].info.User; got != want {
				t.Errorf("read %d (%s): login role %q, want %q", i, obs[i].st.SQL, got, want)
			}
		}
	})
}

// rawSession opens a session through the proxy as a role that needs no
// password and returns a frontend past startup, for messages pgx has no API
// for.
func rawSession(t *testing.T, addr, user string) *pgproto3.Frontend {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	fe := pgproto3.NewFrontend(conn, conn)
	fe.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": user, "database": "stet_test"},
	})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	untilReady(t, fe)
	return fe
}

// untilReady reads messages up to ReadyForQuery, failing on an error, and
// counts the DataRow and PortalSuspended messages on the way.
func untilReady(t *testing.T, fe *pgproto3.Frontend) (rows, suspended int) {
	t.Helper()
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatal(err)
		}
		switch m := msg.(type) {
		case *pgproto3.DataRow:
			rows++
		case *pgproto3.PortalSuspended:
			suspended++
		case *pgproto3.ErrorResponse:
			t.Fatalf("SQLSTATE %s: %s", m.Code, m.Message)
		case *pgproto3.ReadyForQuery:
			return rows, suspended
		}
	}
}

// mustFail runs sql and fails the test unless the server refuses it with code.
func mustFail(t *testing.T, conn *pgx.Conn, sql, code string) {
	t.Helper()
	if _, err := conn.Exec(testCtx(t), sql); pgCode(err) != code {
		t.Fatalf("%s: %v, want SQLSTATE %s", sql, err, code)
	}
}

// Each statement is answered before the next one goes, and Observe runs
// before the ReadyForQuery that ends a read is forwarded, so once the client
// has its answer every read it caused has been reported: the counts below are
// exact.
func TestGateObservesFetchesAsReadsOfTheCursor(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	declare := "declare c cursor for select * from " + table
	for _, sql := range []string{"begin", declare, "fetch 300 from c", "fetch 300 from c", "close c", "commit"} {
		mustExec(t, conn, sql)
	}

	obs := g.observed(t, 2)
	if len(obs) != 2 {
		t.Fatalf("%d reads observed, want one per FETCH and none for the DECLARE", len(obs))
	}
	for i, ob := range obs {
		if ob.st.SQL != declare {
			t.Errorf("FETCH %d reported as %q, want the DECLARE %q", i+1, ob.st.SQL, declare)
		}
		if o := ob.o; o.Sent != 300 || !o.Executed || o.Error != "" || !o.InTransaction {
			t.Errorf("FETCH %d: %+v, want 300 rows sent and executed in a transaction", i+1, o)
		}
	}
}

func TestCursorIsForgottenWhenClosed(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	for _, sql := range []string{"begin", "declare c cursor for select * from " + table, "fetch 10 from c", "close c"} {
		mustExec(t, conn, sql)
	}
	mustFail(t, conn, "fetch 10 from c", "34000")
	mustExec(t, conn, "rollback")

	if obs := g.observed(t, 1); len(obs) != 1 {
		t.Errorf("%d reads observed, want only the FETCH before CLOSE", len(obs))
	}
}

func TestCursorIsForgottenAtTransactionEnd(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	for _, sql := range []string{"begin", "declare c cursor for select * from " + table, "fetch 5 from c", "commit"} {
		mustExec(t, conn, sql)
	}
	// Nothing but the end of the transaction has forgotten c here.
	mustFail(t, conn, "fetch 1 from c", "34000")
	for _, sql := range []string{"begin", "declare c cursor for select 1", "fetch 1 from c", "commit"} {
		mustExec(t, conn, sql)
	}

	if obs := g.observed(t, 1); len(obs) != 1 || obs[0].o.Sent != 5 {
		t.Errorf("reads = %+v, want only the 5-row FETCH of the first transaction", obs)
	}
}

func TestHeldCursorSurvivesCommit(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	held := "declare c cursor with hold for select * from " + table
	for _, sql := range []string{"begin", held, "commit", "fetch 10 from c"} {
		mustExec(t, conn, sql)
	}
	obs := g.observed(t, 1)
	if len(obs) != 1 {
		t.Fatalf("%d reads observed, want 1", len(obs))
	}
	if ob := obs[0]; ob.st.SQL != held || ob.o.Sent != 10 || !ob.o.Executed {
		t.Errorf("FETCH after COMMIT: %q %+v, want the DECLARE with 10 rows sent", ob.st.SQL, ob.o)
	}

	// A later transaction that fails does not take c with it: c was
	// declared in one that committed.
	mustExec(t, conn, "begin")
	mustFail(t, conn, "select 1/0", "22012")
	mustExec(t, conn, "rollback")
	mustExec(t, conn, "fetch 10 from c")
	// Declared outside a transaction block, it commits at once.
	auto := "declare d cursor with hold for select * from " + table
	mustExec(t, conn, auto)
	mustExec(t, conn, "fetch 10 from d")
	mustExec(t, conn, "close all")

	obs = g.observed(t, 3)
	if len(obs) != 3 {
		t.Fatalf("%d reads observed, want 3", len(obs))
	}
	for i, want := range []string{held, auto} {
		if ob := obs[i+1]; ob.st.SQL != want || ob.o.Sent != 10 || !ob.o.Executed {
			t.Errorf("read %d: %q %+v, want %q with 10 rows sent", i+2, ob.st.SQL, ob.o, want)
		}
	}
}

// COMMIT AND CHAIN commits without an idle ReadyForQuery in between: the
// held cursor it committed outlives the next transaction failing.
func TestHeldCursorSurvivesCommitAndChain(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	held := "declare c cursor with hold for select * from " + table
	mustExec(t, conn, "begin")
	mustExec(t, conn, held)
	mustExec(t, conn, "commit and chain")
	mustFail(t, conn, "select 1/0", "22012")
	mustExec(t, conn, "rollback")
	mustExec(t, conn, "fetch 10 from c")
	mustExec(t, conn, "close c")

	obs := g.observed(t, 1)
	if len(obs) != 1 {
		t.Fatalf("%d reads observed, want 1", len(obs))
	}
	if ob := obs[0]; ob.st.SQL != held || ob.o.Sent != 10 || !ob.o.Executed {
		t.Errorf("FETCH after the failed transaction: %q %+v, want %q with 10 rows sent", ob.st.SQL, ob.o, held)
	}
}

// A CLOSE the server refuses or skips leaves the cursor open, so the FETCHes
// after it are still reads. A held cursor committed before survives the
// transaction that failed around the CLOSE.
func TestFailedCloseKeepsTheCursor(t *testing.T) {
	table := readTable(t)
	held := "declare c cursor with hold for select * from " + table
	for _, closing := range []string{"close c", "close all"} {
		run := func(name string, fail func(t *testing.T, conn *pgx.Conn)) {
			t.Run(name, func(t *testing.T) {
				g := newObservingGate(readsOf(table))
				addr, _ := startProxy(t, upstream(t), g)
				conn := connect(t, addr, "stet_scram", nil)
				mustExec(t, conn, held)
				fail(t, conn)
				mustExec(t, conn, "fetch 10 from c")

				obs := g.observed(t, 1)
				if len(obs) != 1 {
					t.Fatalf("%d reads observed, want 1", len(obs))
				}
				if ob := obs[0]; ob.st.SQL != held || ob.o.Sent != 10 || !ob.o.Executed {
					t.Errorf("FETCH after the failed CLOSE: %q %+v, want %q with 10 rows sent", ob.st.SQL, ob.o, held)
				}
			})
		}
		run(closing+" in an aborted transaction", func(t *testing.T, conn *pgx.Conn) {
			mustExec(t, conn, "begin")
			mustFail(t, conn, "select 1/0", "22012")
			mustFail(t, conn, closing, "25P02")
			mustExec(t, conn, "rollback")
		})
		run(closing+" skipped under one Sync", func(t *testing.T, conn *pgx.Conn) {
			b := &pgconn.Batch{}
			b.ExecParams("select 1/0", nil, nil, nil, nil)
			b.ExecParams(closing, nil, nil, nil, nil)
			if _, err := conn.PgConn().ExecBatch(testCtx(t), b).ReadAll(); pgCode(err) != "22012" {
				t.Fatalf("batch: %v, want 22012", err)
			}
		})
	}
}

func TestFailedDeclareIsNotRemembered(t *testing.T) {
	table := readTable(t)
	// The failing DECLARE is one the gate observes, so Stet would remember
	// it if it did not undo it.
	g := newObservingGate(func(sql string) bool { return readsOf(table)(sql) || strings.Contains(sql, "no_such_table") })
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	mustExec(t, conn, "begin")
	mustFail(t, conn, "declare c cursor for select * from no_such_table", "42P01")
	// The transaction is still open: only undoing the failed DECLARE forgets c.
	mustFail(t, conn, "fetch 1 from c", "25P02")
	mustExec(t, conn, "rollback")
	// A held cursor outside a transaction block: no transaction end forgets it.
	mustFail(t, conn, "declare c cursor with hold for select * from no_such_table", "42P01")
	mustFail(t, conn, "fetch 1 from c", "34000")
	for _, sql := range []string{"begin", "declare c cursor for select 1", "fetch 1 from c", "commit"} {
		mustExec(t, conn, sql)
	}

	if obs := g.observed(t, 0); len(obs) != 0 {
		t.Errorf("reads = %+v, want none", obs)
	}
}

func TestFailedRedeclareKeepsTheOldCursor(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	first := "declare c cursor with hold for select * from " + table + " where id <= 100"
	mustExec(t, conn, first)
	mustFail(t, conn, "declare c cursor with hold for select * from "+table, "42P03")
	mustFail(t, conn, "declare c cursor with hold for select 1", "42P03") // unobserved: it would forget c
	mustExec(t, conn, "fetch 5 from c")
	mustExec(t, conn, "close c")

	obs := g.observed(t, 1)
	if len(obs) != 1 {
		t.Fatalf("%d reads observed, want 1", len(obs))
	}
	if ob := obs[0]; ob.st.SQL != first || ob.o.Sent != 5 {
		t.Errorf("FETCH: %q %+v, want the cursor still open, %q, with 5 rows sent", ob.st.SQL, ob.o, first)
	}
}

func TestRedeclaredCursorReplacesTheOld(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	before := "declare c cursor for select * from " + table + " where id <= 100"
	after := "declare c cursor for select * from " + table + " where id > 900"
	for _, sql := range []string{"begin", before, "fetch 10 from c", "close c", after, "fetch 10 from c", "commit"} {
		mustExec(t, conn, sql)
	}

	// Over a name still remembered: a held cursor whose transaction rolled
	// back stays in the table although the server dropped it, so the next
	// DECLARE of the name succeeds and replaces it.
	stale := "declare c cursor with hold for select * from " + table + " where id <= 100"
	fresh := "declare c cursor with hold for select * from " + table + " where id > 900"
	for _, sql := range []string{"begin", stale, "rollback", fresh, "fetch 10 from c", "fetch 10 from c", "close c"} {
		mustExec(t, conn, sql)
	}

	obs := g.observed(t, 4)
	if len(obs) != 4 {
		t.Fatalf("%d reads observed, want 4", len(obs))
	}
	for i, want := range []string{before, after, fresh, fresh} {
		if ob := obs[i]; ob.st.SQL != want || ob.o.Sent != 10 {
			t.Errorf("FETCH %d: %q %+v, want %q with 10 rows sent", i+1, ob.st.SQL, ob.o, want)
		}
	}
}

func TestCloseAllForgetsEveryCursor(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	// Held cursors outside a transaction block: only CLOSE ALL forgets them.
	mustExec(t, conn, "declare a cursor with hold for select * from "+table)
	mustExec(t, conn, "declare b cursor with hold for select * from "+table)
	mustExec(t, conn, "fetch 1 from a")
	mustExec(t, conn, "close all")
	mustFail(t, conn, "fetch 1 from a", "34000")
	mustFail(t, conn, "fetch 1 from b", "34000")

	if obs := g.observed(t, 1); len(obs) != 1 {
		t.Errorf("%d reads observed, want only the FETCH before CLOSE ALL", len(obs))
	}
}

// Parse, Bind and Execute: the DECLARE and a FETCH pipelined behind it under
// one Sync, then a FETCH prepared once and executed until the cursor runs
// out, then after CLOSE.
func TestCursorsThroughTheExtendedProtocol(t *testing.T) {
	table := readTable(t)
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	pc := conn.PgConn()
	ctx := testCtx(t)
	extended := func(sql string) error {
		_, err := pc.ExecParams(ctx, sql, nil, nil, nil, nil).Close()
		return err
	}

	if err := extended("begin"); err != nil {
		t.Fatal(err)
	}
	declare := "declare c cursor for select * from " + table
	b := &pgconn.Batch{}
	b.ExecParams(declare, nil, nil, nil, nil)
	b.ExecParams("fetch 300 from c", nil, nil, nil, nil)
	if _, err := pc.ExecBatch(ctx, b).ReadAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.Prepare(ctx, "f", "fetch 300 from c", nil); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := pc.ExecPrepared(ctx, "f", nil, nil, nil).Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := extended("close c"); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.ExecPrepared(ctx, "f", nil, nil, nil).Close(); pgCode(err) != "34000" {
		t.Fatalf("FETCH after CLOSE: %v, want 34000", err)
	}
	if err := extended("rollback"); err != nil {
		t.Fatal(err)
	}

	obs := g.observed(t, 4)
	if len(obs) != 4 {
		t.Fatalf("%d reads observed, want one per FETCH before CLOSE", len(obs))
	}
	for i, want := range []int64{300, 300, 300, 100} {
		if ob := obs[i]; ob.st.SQL != declare || ob.o.Sent != want || !ob.o.Executed {
			t.Errorf("FETCH %d: %q %+v, want the DECLARE with %d rows sent", i+1, ob.st.SQL, ob.o, want)
		}
	}
}

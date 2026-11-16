package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"slices"
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
	// refuse, when set, is asked inside Begin whether to refuse the write; nil
	// from it sends the write as before.
	refuse func(st proxy.Statement) *proxy.Refusal
	// block, when set, says why Inspect marks a statement as one that cannot
	// be inspected (Statement.Block); "" for one that can.
	block func(sql string) string

	mu       sync.Mutex
	inspects int // Inspect calls
	begins   []began
	ends     map[int]proxy.Outcome
	endCalls int // End calls, to tell a second End for one write from the first
	reads    []observation
	blocked  []blockedCall
	would    []blockedCall // WouldBlock calls
}

type blockedCall struct {
	info   proxy.SessionInfo
	reason string
	first  bool
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

var (
	oboRE = regexp.MustCompile(`(?i)^\s*(?:set\s+stet\.on_behalf_of\s*=\s*'([^']*)'|reset\s+stet\.on_behalf_of)`)
	// The statements that leave a transaction clean, as the classifier marks
	// them; the test double reads one statement per message.
	cleanRE = regexp.MustCompile(`(?i)^\s*(begin|start\s+transaction|set|show)\b`)
)

func (g *recordingGate) Inspect(sql string) proxy.Statement {
	g.mu.Lock()
	g.inspects++
	g.mu.Unlock()
	st := proxy.Statement{SQL: sql, Watch: g.match(sql), Observe: g.observeMatch != nil && g.observeMatch(sql), Clean: cleanRE.MatchString(sql)}
	if m := oboRE.FindStringSubmatch(sql); m != nil {
		st.OnBehalfOf = &m[1]
	}
	if c, query := cursorOf(sql); c != nil {
		st.Cursors = []proxy.Cursor{*c}
		if c.Op == proxy.CursorDeclare { // remembered when its query is an observed read
			st.Observe = g.observeMatch != nil && g.observeMatch(query)
		}
	}
	if g.block != nil {
		st.Block = g.block(sql)
	}
	for _, m := range executeRE.FindAllStringSubmatch(sql, -1) {
		st.Executes = append(st.Executes, strings.ToLower(m[1]))
	}
	if m := portalRE.FindStringSubmatch(sql); m != nil {
		st.Portals = []string{strings.ToLower(m[1])}
	}
	return st
}

// The test double's view of what SQL runs by name: EXECUTE of a prepared
// statement (EXPLAIN ANALYZE EXECUTE included), FETCH or MOVE of a portal.
// The real gate takes both from the classifier.
var (
	executeRE = regexp.MustCompile(`(?i)\bexecute\s+(\w+)`)
	portalRE  = regexp.MustCompile(`(?is)^\s*(?:fetch|move)\b.*\b(?:from|in)\s+(\w+)\s*$`)
)

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

func (g *recordingGate) Begin(info proxy.SessionInfo, st proxy.Statement, params []proxy.Param) (any, *proxy.Refusal) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.onBegin != nil {
		g.onBegin(st)
	}
	g.begins = append(g.begins, began{info, st, params})
	var r *proxy.Refusal
	if g.refuse != nil {
		r = g.refuse(st)
	}
	return len(g.begins) - 1, r
}

func (g *recordingGate) End(h any, o proxy.Outcome) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ends[h.(int)] = o
	g.endCalls++
}

func (g *recordingGate) Observe(info proxy.SessionInfo, st proxy.Statement, o proxy.Outcome) {
	at := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads = append(g.reads, observation{info, st, o, at})
}

// Blocked records the call and answers with an ST002 that names the reason.
func (g *recordingGate) Blocked(info proxy.SessionInfo, reason string, first bool) proxy.Refusal {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.blocked = append(g.blocked, blockedCall{info, reason, first})
	return proxy.Refusal{Code: "ST002", Message: "blocked " + reason}
}

// WouldBlock records the call.
func (g *recordingGate) WouldBlock(info proxy.SessionInfo, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.would = append(g.would, blockedCall{info: info, reason: reason})
}

// wantBlocked fails the test unless Blocked was called once for each of
// reasons, in order, each time for a session of user. Blocked runs before the
// refused message's replacement is sent, so the calls are all in once the
// client has its answers.
func (g *recordingGate) wantBlocked(t *testing.T, user string, reasons ...string) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	var got []string
	for _, b := range g.blocked {
		got = append(got, b.reason)
		if b.info.ID == "" || b.info.User != user {
			t.Errorf("Blocked for session %+v, want a session of %s", b.info, user)
		}
	}
	if !slices.Equal(got, reasons) {
		t.Errorf("Blocked for %q, want %q", got, reasons)
	}
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

func (g *recordingGate) inspectCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inspects
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

// A write is in a clean transaction when nothing but BEGIN, SET or SHOW was
// sent since the server last reported the session idle, whether answered or
// not (spec 4). TxOpen keeps its meaning: the last ReadyForQuery was not idle.
func TestGateSeesCleanTransactions(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)
	exec := func(tx pgx.Tx, sql string) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	insert := func(id int) string { return fmt.Sprintf("insert into %s values (%d, 'x')", table, id) }

	mustExec(t, conn, insert(1))
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	exec(tx, insert(2))
	exec(tx, insert(3))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if tx, err = conn.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	exec(tx, "select 1")
	exec(tx, insert(4))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// BEGIN and the write under one Sync, as pgjdbc sends them.
	p := conn.PgConn().StartPipeline(ctx)
	p.SendQueryParams("begin", nil, nil, nil, nil)
	p.SendQueryParams(insert(5), nil, nil, nil, nil)
	p.SendPipelineSync()
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	for synced := false; !synced; {
		res, err := p.GetResults()
		if err != nil {
			t.Fatal(err)
		}
		switch r := res.(type) {
		case *pgconn.ResultReader:
			if _, err := r.Close(); err != nil {
				t.Fatal(err)
			}
		case *pgconn.PipelineSync:
			synced = true
		case nil:
			t.Fatal("the pipeline ended before its Sync was answered")
		}
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, conn, "commit")

	bs, _ := g.outcomes(t, 5)
	wantDirty := []bool{false, false, true, true, false}
	wantOpen := []bool{false, true, true, true, false}
	for i, b := range bs {
		if b.info.TxDirty != wantDirty[i] || b.info.TxOpen != wantOpen[i] {
			t.Errorf("write %d (%s): TxDirty %v TxOpen %v, want %v and %v", i+1, b.st.SQL, b.info.TxDirty, b.info.TxOpen, wantDirty[i], wantOpen[i])
		}
	}
}

// Only the sessions of the listed login roles are shown to the gate. Another
// role's session is relayed without it: nothing is inspected or begun.
func TestSessionsOfOtherRolesAreNotSeen(t *testing.T) {
	table := watchedTable(t)
	admin := connect(t, upstream(t), "stet_scram", nil)
	mustExec(t, admin, "grant insert on "+table+" to stet_md5") // dropped with the table
	g := newRecordingGate(watches(table))
	addr, _ := startProxyWith(t, upstream(t), g, proxy.Policy{Agents: []string{"stet_md5"}})
	ctx := testCtx(t)

	other := connect(t, addr, "stet_scram", nil)
	mustExec(t, other, "insert into "+table+" values (1, 'simple')")
	if _, err := other.Exec(ctx, "insert into "+table+" values ($1, $2)", 2, "extended"); err != nil {
		t.Fatal(err)
	}
	other.Close(ctx)
	if n, b := g.inspectCount(), g.beginCount(); n != 0 || b != 0 {
		t.Errorf("a role not listed: Inspect ran %d times and Begin %d, want never", n, b)
	}

	agent := connect(t, addr, "stet_md5", nil)
	mustExec(t, agent, "insert into "+table+" values (3, 'agent')")
	bs, os := g.outcomes(t, 1)
	if len(bs) != 1 || bs[0].info.User != "stet_md5" || !os[0].Executed {
		t.Errorf("writes begun: %+v, outcomes %+v; want the agent's one, as stet_md5, executed", bs, os)
	}

	var n int
	if err := admin.QueryRow(ctx, "select count(*) from "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("%d rows in the table, want 3: every write runs", n)
	}
}

// The server serves a protocol 3.1 startup packet by negotiating the version
// down; the proxy reads the login role from it all the same.
func TestProtocol31SessionHasItsLoginRole(t *testing.T) {
	table := watchedTable(t)
	admin := connect(t, upstream(t), "stet_scram", nil)
	mustExec(t, admin, "grant insert on "+table+" to stet_trust") // dropped with the table
	g := newRecordingGate(watches(table))
	addr, _ := startProxy(t, upstream(t), g)

	fe := rawSession(t, addr, "stet_trust", 3<<16|1)
	fe.Send(&pgproto3.Query{String: "insert into " + table + " values (1, 'x')"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	untilReady(t, fe)

	if bs, _ := g.outcomes(t, 1); bs[0].info.User != "stet_trust" {
		t.Errorf("login role %q, want stet_trust", bs[0].info.User)
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

	fe := rawSession(t, addr, "stet_trust", pgproto3.ProtocolVersionNumber)
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

// Each report of a read taken in parts says how many rows the earlier reports
// of the same cursor or portal counted, in the order the server answered. A
// read in one go, a new DECLARE of the name, and a new Bind start at 0.
func TestReadsInPartsCarryTheRowsBefore(t *testing.T) {
	table := readTable(t)
	admin := connect(t, upstream(t), "stet_scram", nil)
	mustExec(t, admin, "grant select on "+table+" to stet_trust") // dropped with the table
	g := newObservingGate(readsOf(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	declare := "declare c cursor for select * from " + table
	mustExec(t, conn, "begin")
	mustExec(t, conn, declare)
	mustExec(t, conn, "fetch 300 from c")
	b := &pgconn.Batch{} // two FETCHes under one Sync
	b.ExecParams("fetch 300 from c", nil, nil, nil, nil)
	b.ExecParams("fetch 300 from c", nil, nil, nil, nil)
	if _, err := conn.PgConn().ExecBatch(testCtx(t), b).ReadAll(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, conn, "close c")
	mustExec(t, conn, declare)
	mustExec(t, conn, "fetch 300 from c")
	mustExec(t, conn, "commit")
	mustExec(t, conn, "select * from "+table+" where id <= 10")

	fe := rawSession(t, addr, "stet_trust", pgproto3.ProtocolVersionNumber)
	fe.Send(&pgproto3.Parse{Query: "select * from " + table + " order by id"})
	fe.Send(&pgproto3.Bind{})
	for range 3 {
		fe.Send(&pgproto3.Execute{MaxRows: 100})
	}
	fe.Send(&pgproto3.Bind{})
	fe.Send(&pgproto3.Execute{MaxRows: 100})
	fe.Send(&pgproto3.Sync{})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	if rows, _ := untilReady(t, fe); rows != 400 {
		t.Fatalf("client got %d rows, want 400", rows)
	}

	obs := g.observed(t, 9)
	want := []struct{ sent, before int64 }{
		{300, 0}, {300, 300}, {300, 600}, // the cursor
		{300, 0},                         // declared again
		{10, 0},                          // in one go
		{100, 0}, {100, 100}, {100, 200}, // the portal
		{100, 0}, // bound again
	}
	if len(obs) != len(want) {
		t.Fatalf("%d reads observed, want %d", len(obs), len(want))
	}
	for i, w := range want {
		if o := obs[i].o; o.Sent != w.sent || o.Before != w.before {
			t.Errorf("read %d (%s): %d rows with %d before, want %d with %d before", i+1, obs[i].st.SQL, o.Sent, o.Before, w.sent, w.before)
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
// password, with a startup packet of the given protocol version, and returns
// a frontend past startup, for messages pgx has no API for.
func rawSession(t *testing.T, addr, user string, protocol uint32) *pgproto3.Frontend {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	fe := pgproto3.NewFrontend(conn, conn)
	fe.Send(&pgproto3.StartupMessage{
		ProtocolVersion: protocol,
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

// replies reads messages up to ReadyForQuery and returns their type bytes in
// order, joined by one space, with an ErrorResponse written as E:<SQLSTATE>:
// for example "1 2 C 1 2 E:ST001 Z".
func replies(t *testing.T, fe *pgproto3.Frontend) string {
	t.Helper()
	got, _ := repliesWithErrors(t, fe)
	return got
}

// repliesWithErrors is replies that also returns the ErrorResponses read.
func repliesWithErrors(t *testing.T, fe *pgproto3.Frontend) (string, []pgproto3.ErrorResponse) {
	t.Helper()
	var got []string
	var errs []pgproto3.ErrorResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatal(err)
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			got = append(got, "E:"+m.Code)
			errs = append(errs, *m)
		default:
			b, err := msg.Encode(nil)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, string(b[0]))
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			return strings.Join(got, " "), errs
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

// Under one Sync the Executes before the failing one ran. A held cursor they
// declared and committed stays open, and a CLOSE among them closed its
// cursor for good: the rollback of the rest does not reopen it.
func TestFailedUnitKeepsWhatRanBeforeTheError(t *testing.T) {
	table := readTable(t)
	held := "declare c cursor with hold for select * from " + table
	run := func(name string, test func(t *testing.T, g *recordingGate, conn *pgx.Conn)) {
		t.Run(name, func(t *testing.T) {
			g := newObservingGate(readsOf(table))
			addr, _ := startProxy(t, upstream(t), g)
			test(t, g, connect(t, addr, "stet_scram", nil))
		})
	}
	batch := func(t *testing.T, conn *pgx.Conn, sqls ...string) {
		t.Helper()
		b := &pgconn.Batch{}
		for _, sql := range sqls {
			b.ExecParams(sql, nil, nil, nil, nil)
		}
		if _, err := conn.PgConn().ExecBatch(testCtx(t), b).ReadAll(); pgCode(err) != "22012" {
			t.Fatalf("batch %q: %v, want 22012", sqls, err)
		}
	}

	run("held cursor committed before the error", func(t *testing.T, g *recordingGate, conn *pgx.Conn) {
		batch(t, conn, "begin", held, "commit", "select 1/0")
		mustExec(t, conn, "fetch 7 from c")
		mustExec(t, conn, "close c")
		obs := g.observed(t, 1)
		if len(obs) != 1 {
			t.Fatalf("%d reads observed, want 1", len(obs))
		}
		if ob := obs[0]; ob.st.SQL != held || ob.o.Sent != 7 || !ob.o.Executed {
			t.Errorf("FETCH: %q %+v, want %q with 7 rows sent", ob.st.SQL, ob.o, held)
		}
	})
	run("close before the error", func(t *testing.T, g *recordingGate, conn *pgx.Conn) {
		mustExec(t, conn, held)
		batch(t, conn, "close c", "select 1/0")
		mustFail(t, conn, "fetch 1 from c", "34000")
		if obs := g.observed(t, 0); len(obs) != 0 {
			t.Errorf("reads = %+v, want none: the CLOSE ran", obs)
		}
	})
	run("cursor declared before the error, not held", func(t *testing.T, g *recordingGate, conn *pgx.Conn) {
		batch(t, conn, "begin", "declare d cursor for select * from "+table, "select 1/0")
		mustExec(t, conn, "rollback")
		mustFail(t, conn, "fetch 1 from d", "34000")
		if obs := g.observed(t, 0); len(obs) != 0 {
			t.Errorf("reads = %+v, want none: its transaction ended", obs)
		}
	})
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

// The proxy's statement table follows what the server confirmed. A Parse the
// server refused (42P05), and a Close it skipped after an earlier error in
// the same Sync, leave the name meaning the watched insert the server still
// holds, so Executing it is judged as that insert.
func TestGateJudgesTheStatementTheServerHolds(t *testing.T) {
	table := watchedTable(t)
	admin := connect(t, upstream(t), "stet_scram", nil)
	mustExec(t, admin, "grant insert on "+table+" to stet_trust") // dropped with the table
	g := newRecordingGate(watches(table))
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawSession(t, addr, "stet_trust", pgproto3.ProtocolVersionNumber)
	ins := "insert into " + table + " values ($1, 'x')"
	unit := func(want string, msgs ...pgproto3.FrontendMessage) {
		t.Helper()
		for _, m := range msgs {
			fe.Send(m)
		}
		fe.Send(&pgproto3.Sync{})
		if err := fe.Flush(); err != nil {
			t.Fatal(err)
		}
		if got := replies(t, fe); got != want {
			t.Fatalf("replies %q, want %q", got, want)
		}
	}
	insertRow := func(id string) []pgproto3.FrontendMessage {
		return []pgproto3.FrontendMessage{
			&pgproto3.Bind{PreparedStatement: "s1", Parameters: [][]byte{[]byte(id)}},
			&pgproto3.Execute{},
		}
	}
	rows := func() int {
		t.Helper()
		var n int
		if err := admin.QueryRow(testCtx(t), "select count(*) from "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	unit("1 Z", &pgproto3.Parse{Name: "s1", Query: ins})
	unit("E:42P05 Z", &pgproto3.Parse{Name: "s1", Query: "select 1"})
	unit("2 C Z", insertRow("1")...)
	bs, os := g.outcomes(t, 1)
	if len(bs) != 1 || bs[0].st.SQL != ins || !os[0].Executed {
		t.Fatalf("after the refused Parse: writes begun %+v, outcomes %+v; want the insert, executed", bs, os)
	}
	if n := rows(); n != 1 {
		t.Errorf("%d rows after the refused Parse, want 1", n)
	}

	unit("E:26000 Z", &pgproto3.Bind{PreparedStatement: "nosuch"}, &pgproto3.Close{ObjectType: 'S', Name: "s1"})
	unit("2 C Z", insertRow("2")...)
	bs, os = g.outcomes(t, 2)
	if len(bs) != 2 || bs[1].st.SQL != ins || !os[1].Executed {
		t.Fatalf("after the skipped Close: writes begun %+v, outcomes %+v; want the insert again, executed", bs, os)
	}
	if n := rows(); n != 2 {
		t.Errorf("%d rows after the skipped Close, want 2", n)
	}
}

// held is the refusal the test gates give: an ST001 with every field set.
var held = proxy.Refusal{Code: "ST001", Message: "held", Detail: "d", Hint: "h"}

// refuseAll refuses every watched write.
func refuseAll(proxy.Statement) *proxy.Refusal {
	r := held
	return &r
}

// refuseMarked refuses the watched writes whose SQL holds the value 'held'.
func refuseMarked(st proxy.Statement) *proxy.Refusal {
	if strings.Contains(st.SQL, "'held'") {
		return refuseAll(st)
	}
	return nil
}

// wantHeld fails the test unless err is the refusal held as the client got
// it: severity ERROR and held's fields, nothing of the server's own error.
func wantHeld(t *testing.T, err error) {
	t.Helper()
	var e *pgconn.PgError
	if !errors.As(err, &e) {
		t.Fatalf("error %v, want the gate's refusal", err)
	}
	want := pgconn.PgError{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "ST001", Message: "held", Detail: "d", Hint: "h"}
	if *e != want {
		t.Errorf("client got %+v, want %+v", *e, want)
	}
}

// rowCount counts the rows of table on a direct connection.
func rowCount(t *testing.T, table string) int {
	t.Helper()
	admin := connect(t, upstream(t), "stet_scram", nil)
	var n int
	if err := admin.QueryRow(testCtx(t), "select count(*) from "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// wantRefused fails the test unless o is the outcome of a write the gate
// refused, delivered to the client or not as undelivered says.
func wantRefused(t *testing.T, o proxy.Outcome, undelivered bool) {
	t.Helper()
	if !o.Refused || o.Undelivered != undelivered || o.Executed || o.Lost || o.Rows != nil {
		t.Errorf("outcome %+v, want refused, undelivered %v, not executed", o, undelivered)
	}
}

// rawAgent opens a raw session through addr as stet_trust, after granting it
// insert on table (dropped with the table).
func rawAgent(t *testing.T, addr, table string) *pgproto3.Frontend {
	t.Helper()
	admin := connect(t, upstream(t), "stet_scram", nil)
	mustExec(t, admin, "grant insert on "+table+" to stet_trust")
	return rawSession(t, addr, "stet_trust", pgproto3.ProtocolVersionNumber)
}

// unnamed is a Parse, Bind and Execute of sql on the unnamed statement and
// portal.
func unnamed(sql string, params ...string) []pgproto3.FrontendMessage {
	var ps [][]byte
	for _, p := range params {
		ps = append(ps, []byte(p))
	}
	return []pgproto3.FrontendMessage{&pgproto3.Parse{Query: sql}, &pgproto3.Bind{Parameters: ps}, &pgproto3.Execute{}}
}

// sendAll sends msgs in one write.
func sendAll(t *testing.T, fe *pgproto3.Frontend, msgs ...pgproto3.FrontendMessage) {
	t.Helper()
	for _, m := range msgs {
		fe.Send(m)
	}
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
}

func TestRefusedQueryGetsTheGatesError(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	g.refuse = refuseAll
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)

	_, err := conn.PgConn().Exec(ctx, "insert into "+table+" values (1, 'x')").ReadAll()
	wantHeld(t, err)
	mustExec(t, conn, "select 1")
	if n := rowCount(t, table); n != 0 {
		t.Errorf("%d rows, want the table as it was", n)
	}
	_, os := g.outcomes(t, 1)
	wantRefused(t, os[0], false)
	if os[0].InTransaction {
		t.Errorf("outcome %+v, want outside a transaction", os[0])
	}
}

// Queries sent in one write are answered one by one, the refused one with
// the gate's error in the server's place (spec 16).
func TestRefusedQueryAmongPipelinedQueries(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	g.refuse = refuseMarked
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawAgent(t, addr, table)

	sendAll(t, fe,
		&pgproto3.Query{String: "insert into " + table + " values (1, 'x')"},
		&pgproto3.Query{String: "insert into " + table + " values (2, 'held')"},
		&pgproto3.Query{String: "select 1"})
	for _, want := range []string{"C Z", "E:ST001 Z", "T D C Z"} {
		if got := replies(t, fe); got != want {
			t.Errorf("replies %q, want %q", got, want)
		}
	}
	if n := rowCount(t, table); n != 1 {
		t.Errorf("%d rows, want the one insert that passed", n)
	}
	_, os := g.outcomes(t, 2)
	if !os[0].Executed || os[0].Refused {
		t.Errorf("passing insert: %+v, want executed", os[0])
	}
	wantRefused(t, os[1], false)
}

func TestRefusedExecuteGetsTheGatesError(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	g.refuse = refuseAll
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)
	ins := "insert into " + table + " values ($1, $2)"

	_, err := conn.Exec(ctx, ins, 1, "secret")
	wantHeld(t, err)
	mustExec(t, conn, "select 1")
	if n := rowCount(t, table); n != 0 {
		t.Errorf("%d rows, want the table as it was", n)
	}
	bs, os := g.outcomes(t, 1)
	wantRefused(t, os[0], false)
	if bs[0].st.SQL != ins {
		t.Errorf("Begin got %q, want %q", bs[0].st.SQL, ins)
	}
	if p := bs[0].params; len(p) != 2 || string(p[1].Value) != "secret" {
		t.Errorf("Begin got params %+v, want 2 with the second = secret", p)
	}
}

func TestRefusalFailsTheOpenTransaction(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	g.refuse = refuseAll
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)

	mustExec(t, conn, "begin")
	_, err := conn.Exec(ctx, "insert into "+table+" values (1, 'x')")
	wantHeld(t, err)
	mustFail(t, conn, "select 1", "25P02")
	mustExec(t, conn, "rollback")
	mustExec(t, conn, "select 1")

	_, os := g.outcomes(t, 1)
	wantRefused(t, os[0], false)
	if !os[0].InTransaction {
		t.Errorf("outcome %+v, want inside a transaction", os[0])
	}
}

// The server sees an error where the refused write stood, so the implicit
// transaction of its Sync rolls back what ran before it.
func TestRefusalRollsBackTheRestOfItsSync(t *testing.T) {
	table := watchedTable(t)
	other := watchedTable(t) // not watched by this gate
	g := newRecordingGate(watches(table))
	g.refuse = refuseAll
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	b := &pgconn.Batch{}
	b.ExecParams("insert into "+other+" values (1, 'unwatched')", nil, nil, nil, nil)
	b.ExecParams("insert into "+table+" values (1, 'x')", nil, nil, nil, nil)
	// pgconn reads up to the first error and returns it from ReadAll; the
	// results are the statements completed before it.
	results, err := conn.PgConn().ExecBatch(testCtx(t), b).ReadAll()
	wantHeld(t, err)
	if len(results) != 1 || results[0].Err != nil {
		t.Errorf("results %+v, want the unwatched insert's alone, completed", results)
	}
	if n := rowCount(t, other); n != 0 {
		t.Errorf("%d rows in the unwatched table, want its insert rolled back", n)
	}
	_, os := g.outcomes(t, 1)
	wantRefused(t, os[0], false)
}

// A refused Execute is answered as the same Execute failing on the server
// would be: the same kinds of message, as many, in the same order.
func TestRefusedPipelineAnswersLikeAServerError(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	g.refuse = refuseMarked
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawAgent(t, addr, table)
	direct := rawSession(t, upstream(t), "stet_trust", pgproto3.ProtocolVersionNumber)
	pipeline := func(fe *pgproto3.Frontend, second string) string {
		t.Helper()
		var msgs []pgproto3.FrontendMessage
		msgs = append(msgs, unnamed("insert into "+table+" values (1, 'x')")...)
		msgs = append(msgs, unnamed(second)...)
		msgs = append(msgs, unnamed("select 1")...)
		sendAll(t, fe, append(msgs, &pgproto3.Sync{})...)
		return replies(t, fe)
	}

	proxied := pipeline(fe, "insert into "+table+" values (2, 'held')")
	if proxied != "1 2 C 1 2 E:ST001 Z" {
		t.Errorf("through the proxy: replies %q, want %q", proxied, "1 2 C 1 2 E:ST001 Z")
	}
	if n := rowCount(t, table); n != 0 {
		t.Errorf("%d rows, want the insert before the refusal rolled back", n)
	}
	failing := pipeline(direct, "insert into "+table+" values (1, 'duplicate')")
	if strings.Replace(failing, "E:23505", "E:ST001", 1) != proxied {
		t.Errorf("a server error at the same place gives %q, the refusal %q", failing, proxied)
	}
	_, os := g.outcomes(t, 2)
	wantRefused(t, os[1], false)
}

// An error before the refused write under the same Sync is what the client
// gets; the refusal never reaches it.
func TestEarlierFailureHidesTheRefusal(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	g.refuse = refuseAll
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	b := &pgconn.Batch{}
	b.ExecParams("select 1/0", nil, nil, nil, nil)
	b.ExecParams("insert into "+table+" values (1, 'x')", nil, nil, nil, nil)
	if _, err := conn.PgConn().ExecBatch(testCtx(t), b).ReadAll(); pgCode(err) != "22012" {
		t.Fatalf("error %v, want the server's 22012", err)
	}
	mustExec(t, conn, "select 1")
	_, os := g.outcomes(t, 1)
	wantRefused(t, os[0], true)
}

// The Bind before a refused Execute fails first: the client gets the Bind's
// own error, not the refusal.
func TestBindFailureBeforeARefusedExecuteIsLeftAlone(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	g.refuse = refuseAll
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawAgent(t, addr, table)

	sendAll(t, fe, append(unnamed("insert into "+table+" values ($1, 'x')", "abc"), &pgproto3.Sync{})...)
	if got := replies(t, fe); got != "1 E:22P02 Z" {
		t.Errorf("replies %q, want %q", got, "1 E:22P02 Z")
	}
	_, os := g.outcomes(t, 1)
	wantRefused(t, os[0], true)
}

// Of two refused writes under one Sync the server fails the first; the
// second never runs and its refusal is not delivered. Each is ended once.
func TestSecondRefusalUnderOneSyncIsUndelivered(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	g.refuse = refuseAll
	addr, srv := startProxy(t, upstream(t), g)
	fe := rawAgent(t, addr, table)

	var msgs []pgproto3.FrontendMessage
	msgs = append(msgs, unnamed("insert into "+table+" values (1, 'x')")...)
	msgs = append(msgs, unnamed("insert into "+table+" values (2, 'x')")...)
	sendAll(t, fe, append(msgs, &pgproto3.Sync{})...)
	if got := replies(t, fe); got != "1 2 E:ST001 Z" {
		t.Errorf("replies %q, want %q", got, "1 2 E:ST001 Z")
	}
	_, os := g.outcomes(t, 2)
	wantRefused(t, os[0], false)
	wantRefused(t, os[1], true)

	srv.Close() // the sessions end: nothing is ended twice
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.endCalls != 2 {
		t.Errorf("End called %d times, want once per write", g.endCalls)
	}
}

// A prepared write refused twice stays prepared on the server: the third
// Execute of it runs.
func TestRefusedExecuteLeavesThePreparedStatementUsable(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	refusals := 0
	g.refuse = func(st proxy.Statement) *proxy.Refusal {
		if refusals++; refusals <= 2 {
			return refuseAll(st)
		}
		return nil
	}
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)

	if _, err := conn.Prepare(ctx, "ins", "insert into "+table+" values ($1, $2)"); err != nil {
		t.Fatal(err)
	}
	for id := range 2 {
		_, err := conn.Exec(ctx, "ins", id, "x")
		wantHeld(t, err)
	}
	if _, err := conn.Exec(ctx, "ins", 3, "x"); err != nil {
		t.Fatalf("third Execute: %v", err)
	}
	if n := rowCount(t, table); n != 1 {
		t.Errorf("%d rows, want the third insert's", n)
	}
	_, os := g.outcomes(t, 3)
	wantRefused(t, os[0], false)
	wantRefused(t, os[1], false)
	if !os[2].Executed || os[2].Refused {
		t.Errorf("third write: %+v, want executed", os[2])
	}
}

// pgx streams the rows of a COPY FROM STDIN without waiting for the server's
// CopyInResponse. A refused COPY is answered with an error instead, and the
// server ignores the copy data that follows.
func TestRefusedCopyFromStdinDoesNotHang(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(func(sql string) bool {
		return strings.HasPrefix(strings.ToLower(sql), "copy") && strings.Contains(sql, table)
	})
	g.refuse = refuseAll
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows := [][]any{{1, "a"}, {2, "b"}}
	_, err := conn.CopyFrom(ctx, pgx.Identifier{table}, []string{"id", "v"}, pgx.CopyFromRows(rows))
	if ctx.Err() != nil {
		t.Fatalf("COPY still waiting after 5s: %v", err)
	}
	wantHeld(t, err)
	mustExec(t, conn, "select 1")
	if n := rowCount(t, table); n != 0 {
		t.Errorf("%d rows, want none copied", n)
	}
	bs, os := g.outcomes(t, 1)
	wantRefused(t, os[0], false)
	if !strings.HasPrefix(bs[0].st.SQL, "copy") {
		t.Errorf("Begin got %q, want the COPY", bs[0].st.SQL)
	}
}

// A deferred constraint that fails at the Sync raises an error that belongs
// to no message: it is the server's, also on a session whose gate refuses
// writes. Only a refused message's own error is rewritten.
func TestErrorAtTheSyncIsNotARefusal(t *testing.T) {
	table := fmt.Sprintf("deferred_%d", time.Now().UnixNano())
	admin := connect(t, upstream(t), "stet_scram", nil)
	mustExec(t, admin, "create table "+table+" (id int primary key, v text unique deferrable initially deferred)")
	t.Cleanup(func() { admin.Exec(context.Background(), "drop table "+table) })
	g := newRecordingGate(watches(table))
	g.refuse = refuseMarked
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawAgent(t, addr, table)
	unit := func(want string, sqls ...string) {
		t.Helper()
		var msgs []pgproto3.FrontendMessage
		for _, sql := range sqls {
			msgs = append(msgs, unnamed(sql)...)
		}
		sendAll(t, fe, append(msgs, &pgproto3.Sync{})...)
		if got := replies(t, fe); got != want {
			t.Errorf("replies %q, want %q", got, want)
		}
	}

	unit("1 2 C 1 2 C E:23505 Z", "insert into "+table+" values (1, 'same')", "insert into "+table+" values (2, 'same')")
	unit("1 2 C 1 2 E:ST001 Z", "insert into "+table+" values (3, 'x')", "insert into "+table+" values (4, 'held')")
	if n := rowCount(t, table); n != 0 {
		t.Errorf("%d rows, want both units rolled back", n)
	}
	_, os := g.outcomes(t, 4)
	for i, o := range os[:3] {
		if o.Refused {
			t.Errorf("write %d: %+v, want it sent", i+1, o)
		}
	}
	wantRefused(t, os[3], false)
}

// The closed-point tests run twice: through an enforcing proxy, which refuses
// on watched sessions what it cannot inspect, and through a shadow one, which
// forwards those messages as before.
var closedPointModes = []struct {
	name   string
	policy proxy.Policy
}{
	{"enforce", proxy.Policy{Enforce: true}},
	{"shadow", proxy.Policy{}},
}

// unreadableSQL runs on the server, but the test gates mark it as SQL the
// classifier could not read.
const unreadableSQL = "select 'unreadable'"

var doRE = regexp.MustCompile(`(?i)^\s*do\b`)

// blockClosedPoints marks what the real gate's Inspect marks: SQL it cannot
// read (here unreadableSQL) and DO blocks.
func blockClosedPoints(sql string) string {
	switch {
	case strings.Contains(sql, "'unreadable'"):
		return proxy.BlockUnparsed
	case doRE.MatchString(sql):
		return proxy.BlockDo
	}
	return ""
}

func closedPointGate(match func(string) bool) *recordingGate {
	g := newRecordingGate(match)
	g.block = blockClosedPoints
	return g
}

// wantReplies reads the replies up to ReadyForQuery and fails the test unless
// they are want (see replies) and the ST002 errors among them carry msgs, in
// order, with nothing else of the server's error.
func wantReplies(t *testing.T, fe *pgproto3.Frontend, want string, msgs ...string) {
	t.Helper()
	got, errs := repliesWithErrors(t, fe)
	if got != want {
		t.Errorf("replies %q, want %q", got, want)
	}
	var gotMsgs []string
	for _, e := range errs {
		if e.Code != "ST002" {
			continue
		}
		gotMsgs = append(gotMsgs, e.Message)
		if e.Severity != "ERROR" || e.Detail != "" || e.Hint != "" || e.Position != 0 {
			t.Errorf("client got %+v, want the gate's refusal alone", e)
		}
	}
	if !slices.Equal(gotMsgs, msgs) {
		t.Errorf("ST002 messages %q, want %q", gotMsgs, msgs)
	}
}

// pick is enforced on an enforcing proxy, and shadow otherwise.
func pick(policy proxy.Policy, enforced, shadow string) string {
	if policy.Enforce {
		return enforced
	}
	return shadow
}

// SQL the classifier cannot read is refused on an enforced session, sent as a
// Query or as a Parse, and the session goes on. A refused Parse never reaches
// the server, so its name stays free there. The shadow proxy forwards it.
func TestUnreadableStatementsAreRefused(t *testing.T) {
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			g := closedPointGate(isWrite)
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			fe := rawSession(t, addr, "stet_trust", pgproto3.ProtocolVersionNumber)
			p := mode.policy
			var msgs []string
			if p.Enforce {
				msgs = []string{"blocked unparsed"}
			}

			sendAll(t, fe, &pgproto3.Query{String: unreadableSQL})
			wantReplies(t, fe, pick(p, "E:ST002 Z", "T D C Z"), msgs...)
			sendAll(t, fe, &pgproto3.Query{String: "select 1"})
			wantReplies(t, fe, "T D C Z")

			sendAll(t, fe, &pgproto3.Parse{Name: "s1", Query: unreadableSQL},
				&pgproto3.Bind{PreparedStatement: "s1"}, &pgproto3.Execute{}, &pgproto3.Sync{})
			wantReplies(t, fe, pick(p, "E:ST002 Z", "1 2 D C Z"), msgs...)
			sendAll(t, fe, &pgproto3.Bind{PreparedStatement: "s1"}, &pgproto3.Execute{}, &pgproto3.Sync{})
			wantReplies(t, fe, pick(p, "E:26000 Z", "2 D C Z"))
			sendAll(t, fe, &pgproto3.Query{String: "select 1"})
			wantReplies(t, fe, "T D C Z")

			if p.Enforce {
				g.wantBlocked(t, "stet_trust", proxy.BlockUnparsed, proxy.BlockUnparsed)
			} else {
				g.wantBlocked(t, "stet_trust")
			}
		})
	}
}

// doCount runs a DO block that inserts into a temporary table of conn's
// session and returns the table's rows after it, and the DO block's error.
func doCount(t *testing.T, conn *pgx.Conn) (int, error) {
	t.Helper()
	ctx := testCtx(t)
	mustExec(t, conn, "create temp table do_target (i int)")
	_, err := conn.Exec(ctx, "do $$ begin insert into do_target values (1); end $$")
	var n int
	if err := conn.QueryRow(ctx, "select count(*) from do_target").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n, err
}

// wantST002 fails the test unless err is the test gate's refusal for reason.
func wantST002(t *testing.T, err error, reason string) {
	t.Helper()
	var e *pgconn.PgError
	if !errors.As(err, &e) || e.Code != "ST002" || e.Message != "blocked "+reason {
		t.Errorf("error %v, want ST002 blocked %s", err, reason)
	}
}

// What a DO block writes cannot be seen: an enforced session refuses it, so
// the table stays as it was. The shadow proxy lets it run.
func TestDoBlocksAreRefused(t *testing.T) {
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			g := closedPointGate(isWrite)
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			conn := connect(t, addr, "stet_scram", nil)

			n, err := doCount(t, conn)
			if mode.policy.Enforce {
				wantST002(t, err, proxy.BlockDo)
				if n != 0 {
					t.Errorf("%d rows after the refused DO block, want 0", n)
				}
				g.wantBlocked(t, "stet_scram", proxy.BlockDo)
				return
			}
			if err != nil || n != 1 {
				t.Errorf("DO block: %v, %d rows; want it run", err, n)
			}
			g.wantBlocked(t, "stet_scram")
		})
	}
}

// backendPID is the OID of pg_backend_pid, a function a FunctionCall can call.
func backendPID(t *testing.T) uint32 {
	t.Helper()
	admin := connect(t, upstream(t), "stet_scram", nil)
	var oid uint32
	if err := admin.QueryRow(testCtx(t), "select 'pg_backend_pid'::regproc::oid").Scan(&oid); err != nil {
		t.Fatal(err)
	}
	return oid
}

// A FunctionCall cannot be inspected: an enforced session refuses it, and the
// session goes on. The shadow proxy forwards it.
func TestFunctionCallsAreRefused(t *testing.T) {
	oid := backendPID(t)
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			g := closedPointGate(isWrite)
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			fe := rawSession(t, addr, "stet_trust", pgproto3.ProtocolVersionNumber)
			p := mode.policy

			sendAll(t, fe, &pgproto3.FunctionCall{Function: oid})
			if p.Enforce {
				wantReplies(t, fe, "E:ST002 Z", "blocked function_call")
				g.wantBlocked(t, "stet_trust", proxy.BlockFunctionCall)
			} else {
				wantReplies(t, fe, "V Z")
				g.wantBlocked(t, "stet_trust")
			}
			sendAll(t, fe, &pgproto3.Query{String: "select 1"})
			wantReplies(t, fe, "T D C Z")
		})
	}
}

// A Query, Parse or Bind past the size the proxy reads cannot be inspected:
// an enforced session refuses it, with its body read and dropped, and the
// statements after it run. The shadow proxy forwards it unread. Each write
// here would be judged if it were smaller.
func TestOversizedMessagesAreRefused(t *testing.T) {
	t.Cleanup(proxy.SetMaxInspect(1 << 20)) // restored once every proxy below has closed
	big := strings.Repeat("x", 2<<20)
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			table := watchedTable(t)
			g := closedPointGate(watches(table))
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			fe := rawAgent(t, addr, table)
			p := mode.policy
			var msgs []string
			if p.Enforce {
				msgs = []string{"blocked too_large"}
			}
			selectOne := func() {
				t.Helper()
				sendAll(t, fe, &pgproto3.Query{String: "select 1"})
				wantReplies(t, fe, "T D C Z")
			}

			sendAll(t, fe, &pgproto3.Query{String: "insert into " + table + " values (1, '" + big + "')"})
			wantReplies(t, fe, pick(p, "E:ST002 Z", "C Z"), msgs...)
			selectOne()

			sendAll(t, fe, &pgproto3.Parse{Query: "insert into " + table + " values ($1, $2)"},
				&pgproto3.Bind{Parameters: [][]byte{[]byte("2"), []byte(big)}}, &pgproto3.Execute{}, &pgproto3.Sync{})
			wantReplies(t, fe, pick(p, "1 E:ST002 Z", "1 2 C Z"), msgs...)
			selectOne()

			sendAll(t, fe, &pgproto3.Parse{Query: "insert into " + table + " values (3, '" + big + "')"},
				&pgproto3.Bind{}, &pgproto3.Execute{}, &pgproto3.Sync{})
			wantReplies(t, fe, pick(p, "E:ST002 Z", "1 2 C Z"), msgs...)
			selectOne()

			if p.Enforce {
				if n := rowCount(t, table); n != 0 {
					t.Errorf("%d rows, want none of the refused writes", n)
				}
				g.wantBlocked(t, "stet_trust", proxy.BlockTooLarge, proxy.BlockTooLarge, proxy.BlockTooLarge)
			} else {
				if n := rowCount(t, table); n != 3 {
					t.Errorf("%d rows, want the 3 writes forwarded", n)
				}
				g.wantBlocked(t, "stet_trust")
			}
			// No message the proxy did not read reaches Begin. On the
			// enforced session the Execute behind the refused Parse binds,
			// as far as the proxy knows, the unnamed insert the unit before
			// left; the gate may judge it, but the server skips it.
			bs, os := g.outcomes(t, g.beginCount())
			for i, b := range bs {
				if strings.Contains(b.st.SQL, big) || os[i].Executed {
					t.Errorf("write %d began: %.60q, %+v; want no unread message begun, nothing begun run", i+1, b.st.SQL, os[i])
				}
			}
			if !p.Enforce && len(bs) != 0 {
				t.Errorf("shadow: %d writes began, want none read", len(bs))
			}
		})
	}
}

// A refused closed point fails the open transaction, as a server error
// would: the next statement gets 25P02 until the client rolls back.
func TestClosedPointsFailTheOpenTransaction(t *testing.T) {
	g := closedPointGate(isWrite)
	addr, _ := startProxyWith(t, upstream(t), g, proxy.Policy{Enforce: true})
	conn := connect(t, addr, "stet_scram", nil)

	mustExec(t, conn, "begin")
	_, err := conn.Exec(testCtx(t), "do $$ begin perform 1; end $$")
	wantST002(t, err, proxy.BlockDo)
	mustFail(t, conn, "select 1", "25P02")
	mustExec(t, conn, "rollback")
	mustExec(t, conn, "select 1")
	g.wantBlocked(t, "stet_scram", proxy.BlockDo)
}

// A session the policy does not watch has no closed points, also when the
// proxy enforces: its DO block and FunctionCall run, and the gate hears of
// neither.
func TestUnwatchedSessionsSkipTheClosedPoints(t *testing.T) {
	oid := backendPID(t)
	g := closedPointGate(isWrite)
	addr, _ := startProxyWith(t, upstream(t), g, proxy.Policy{Agents: []string{"stet_md5"}, Enforce: true})
	conn := connect(t, addr, "stet_scram", nil)

	if n, err := doCount(t, conn); err != nil || n != 1 {
		t.Errorf("DO block: %v, %d rows; want it run", err, n)
	}
	nc := conn.PgConn().Conn()
	nc.SetDeadline(time.Now().Add(10 * time.Second))
	defer nc.SetDeadline(time.Time{})
	fe := conn.PgConn().Frontend()
	sendAll(t, fe, &pgproto3.FunctionCall{Function: oid})
	wantReplies(t, fe, "V Z")

	g.wantBlocked(t, "stet_scram")
	if n := g.inspectCount(); n != 0 {
		t.Errorf("Inspect ran %d times for a session that is not watched, want never", n)
	}
}

// With no gate (no gated table: spec correction B10) no session is watched,
// so an enforcing proxy only relays: a DO block and a FunctionCall run.
func TestEnforceWithoutAGateOnlyRelays(t *testing.T) {
	oid := backendPID(t)
	addr, _ := startProxyWith(t, upstream(t), nil, proxy.Policy{Enforce: true})
	conn := connect(t, addr, "stet_scram", nil)
	if n, err := doCount(t, conn); err != nil || n != 1 {
		t.Errorf("DO block: %v, %d rows; want it run", err, n)
	}
	fe := rawSession(t, addr, "stet_trust", pgproto3.ProtocolVersionNumber)
	sendAll(t, fe, &pgproto3.FunctionCall{Function: oid})
	wantReplies(t, fe, "V Z")
}

// nextReplies reads n messages and returns them as replies does, for answers
// that end with no ReadyForQuery (a Flush, say).
func nextReplies(t *testing.T, fe *pgproto3.Frontend, n int) string {
	t.Helper()
	var got []string
	for range n {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if e, ok := msg.(*pgproto3.ErrorResponse); ok {
			got = append(got, "E:"+e.Code)
			continue
		}
		b, err := msg.Encode(nil)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(b[0]))
	}
	return strings.Join(got, " ")
}

// A Query sent after extended-protocol messages with no Sync between them is
// followed as the server answers it (spec correction B6 ①): when those
// messages succeeded, the Query's ReadyForQuery answers them too; when one
// failed, the server skips the Query as well, up to the next Sync. Each
// statement gets its own outcome and the answers stay in step, in both modes.
func TestQueryBeforeSyncIsFollowed(t *testing.T) {
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			table := watchedTable(t)
			g := newRecordingGate(watches(table))
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			fe := rawAgent(t, addr, table)
			insert := func(id int) *pgproto3.Query {
				return &pgproto3.Query{String: fmt.Sprintf("insert into %s values (%d, 'x')", table, id)}
			}

			sendAll(t, fe, &pgproto3.Parse{Query: "select 1"}, &pgproto3.Describe{ObjectType: 'S'}, &pgproto3.Flush{})
			if got := nextReplies(t, fe, 3); got != "1 t T" {
				t.Fatalf("replies to the Flush %q, want %q", got, "1 t T")
			}
			sendAll(t, fe, insert(1), insert(2))
			wantReplies(t, fe, "C Z")
			wantReplies(t, fe, "C Z")
			_, os := g.outcomes(t, 2)
			for i, o := range os {
				if !o.Executed || o.Error != "" || o.Rows == nil || *o.Rows != 1 {
					t.Errorf("write %d: %+v, want executed with 1 row", i+1, o)
				}
			}

			sendAll(t, fe, &pgproto3.Parse{Query: "selec 1"}, &pgproto3.Flush{})
			if got := nextReplies(t, fe, 1); got != "E:42601" {
				t.Fatalf("replies to the Flush %q, want %q", got, "E:42601")
			}
			sendAll(t, fe, insert(3), &pgproto3.Sync{})
			wantReplies(t, fe, "Z")
			sendAll(t, fe, insert(4))
			wantReplies(t, fe, "C Z")
			_, os = g.outcomes(t, 4)
			if o := os[2]; o.Executed || o.Lost || !strings.Contains(o.Error, "not executed") {
				t.Errorf("write behind the failed Parse: %+v, want not executed", o)
			}
			if o := os[3]; !o.Executed || o.Error != "" || o.Rows == nil || *o.Rows != 1 {
				t.Errorf("write after the Sync: %+v, want executed with 1 row", o)
			}
			if n := rowCount(t, table); n != 3 {
				t.Errorf("%d rows, want 3", n)
			}
		})
	}
}

// After a Query sent before Sync the proxy keeps no unit the server will not
// answer: the statement table of later units still follows the server. A
// watched Parse the server confirmed stays, so its Execute is judged, and a
// re-Parse it refused (42P05) is dropped.
func TestUnitsAfterAQueryBeforeSyncFollowTheServer(t *testing.T) {
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			table := watchedTable(t)
			g := newRecordingGate(watches(table))
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			fe := rawAgent(t, addr, table)
			run := func(id string) {
				t.Helper()
				sendAll(t, fe, &pgproto3.Bind{PreparedStatement: "s2", Parameters: [][]byte{[]byte(id)}}, &pgproto3.Execute{}, &pgproto3.Sync{})
				wantReplies(t, fe, "2 C Z")
			}

			sendAll(t, fe, &pgproto3.Parse{Query: "select 1"}, &pgproto3.Describe{ObjectType: 'S'}, &pgproto3.Flush{})
			if got := nextReplies(t, fe, 3); got != "1 t T" {
				t.Fatalf("replies to the Flush %q, want %q", got, "1 t T")
			}
			sendAll(t, fe, &pgproto3.Query{String: "select 2"})
			wantReplies(t, fe, "T D C Z")

			sendAll(t, fe, &pgproto3.Parse{Name: "s2", Query: "insert into " + table + " values ($1, 'x')"}, &pgproto3.Sync{})
			wantReplies(t, fe, "1 Z")
			sendAll(t, fe, &pgproto3.Bind{PreparedStatement: "nosuch"}, &pgproto3.Sync{})
			wantReplies(t, fe, "E:26000 Z")
			run("1")
			if n := g.beginCount(); n != 1 {
				t.Errorf("after the confirmed Parse: %d writes begun, want 1", n)
			}

			sendAll(t, fe, &pgproto3.Parse{Name: "s2", Query: "select 3"}, &pgproto3.Sync{})
			wantReplies(t, fe, "E:42P05 Z")
			run("2")
			if n := g.beginCount(); n != 2 {
				t.Errorf("after the refused re-Parse: %d writes begun, want 2", n)
			}
			if n := rowCount(t, table); n != 2 {
				t.Errorf("%d rows, want 2", n)
			}
		})
	}
}

// After a Query sent before Sync, each refusal still reaches the client in
// place of its own replacement's error, and an error of another statement
// reaches it unchanged.
func TestRefusalsAfterAQueryBeforeSyncAreDelivered(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	g.refuse = refuseMarked
	addr, _ := startProxyWith(t, upstream(t), g, proxy.Policy{Enforce: true})
	fe := rawAgent(t, addr, table)
	refused := &pgproto3.Query{String: "insert into " + table + " values (1, 'held')"}

	sendAll(t, fe, append(unnamed("select 1"), refused, &pgproto3.Sync{})...)
	wantReplies(t, fe, "1 2 D C E:ST001 Z")
	wantReplies(t, fe, "Z")
	sendAll(t, fe, refused)
	wantReplies(t, fe, "E:ST001 Z")
	sendAll(t, fe, &pgproto3.Query{String: "select 1/0"})
	wantReplies(t, fe, "E:22012 Z")

	_, os := g.outcomes(t, 2)
	wantRefused(t, os[0], false)
	wantRefused(t, os[1], false)
	if n := rowCount(t, table); n != 0 {
		t.Errorf("%d rows, want none", n)
	}
}

// A statement prepared with a protocol Parse runs with SQL EXECUTE, where
// the classifier sees no table (spec correction B5). An enforced session
// refuses an EXECUTE of a name the statement table holds as a watched write,
// also inside EXPLAIN ANALYZE; the shadow proxy forwards it as before. A name
// the table does not hold gets the server's own answer.
func TestSQLExecuteOfAPreparedWriteIsRefused(t *testing.T) {
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			table := watchedTable(t)
			g := newRecordingGate(watches(table))
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			fe := rawAgent(t, addr, table)
			p := mode.policy

			sendAll(t, fe, &pgproto3.Parse{Name: "s1", Query: "insert into " + table + " values ($1, 'x')"}, &pgproto3.Sync{})
			wantReplies(t, fe, "1 Z")
			sendAll(t, fe, &pgproto3.Query{String: "execute s1(1)"})
			sendAll(t, fe, &pgproto3.Query{String: "explain analyze execute s1(2)"})
			if p.Enforce {
				wantReplies(t, fe, "E:ST002 Z", "blocked prepared_write")
				wantReplies(t, fe, "E:ST002 Z", "blocked prepared_write")
			} else {
				wantReplies(t, fe, "C Z")
				if got := replies(t, fe); !strings.HasPrefix(got, "T D") || !strings.HasSuffix(got, "C Z") {
					t.Errorf("EXPLAIN ANALYZE EXECUTE: replies %q, want its plan", got)
				}
			}
			sendAll(t, fe, &pgproto3.Query{String: "execute nosuch(3)"})
			wantReplies(t, fe, "E:26000 Z")

			if n, want := rowCount(t, table), pick(p, "0", "2"); fmt.Sprint(n) != want {
				t.Errorf("%d rows, want %s", n, want)
			}
			if n := g.beginCount(); n != 0 {
				t.Errorf("%d writes begun, want none: an EXECUTE is not judged", n)
			}
			if p.Enforce {
				g.wantBlocked(t, "stet_trust", proxy.BlockPreparedWrite, proxy.BlockPreparedWrite)
			} else {
				g.wantBlocked(t, "stet_trust")
			}
		})
	}
}

// A portal bound with the protocol to an UPDATE ... RETURNING runs when SQL
// FETCH or MOVE reads it inside its transaction (spec correction B5). An
// enforced session refuses both, and the table stays as it was; the shadow
// proxy forwards them as before.
func TestFetchFromAWritePortalIsRefused(t *testing.T) {
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			table := watchedTable(t)
			admin := connect(t, upstream(t), "stet_scram", nil)
			mustExec(t, admin, "insert into "+table+" values (1, 'a')")
			mustExec(t, admin, "grant select, update on "+table+" to stet_trust")
			g := newRecordingGate(watches(table))
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			fe := rawSession(t, addr, "stet_trust", pgproto3.ProtocolVersionNumber)
			p := mode.policy

			for _, c := range []struct{ sql, shadow string }{
				{"fetch all from p1", "T D C Z"},
				{"move all in p1", "C Z"},
			} {
				sendAll(t, fe, &pgproto3.Query{String: "begin"})
				wantReplies(t, fe, "C Z")
				sendAll(t, fe, &pgproto3.Parse{Query: "update " + table + " set v = 'b' returning id"},
					&pgproto3.Bind{DestinationPortal: "p1"}, &pgproto3.Sync{})
				wantReplies(t, fe, "1 2 Z")
				sendAll(t, fe, &pgproto3.Query{String: c.sql})
				if p.Enforce {
					wantReplies(t, fe, "E:ST002 Z", "blocked prepared_write")
				} else {
					wantReplies(t, fe, c.shadow)
				}
				sendAll(t, fe, &pgproto3.Query{String: "rollback"})
				wantReplies(t, fe, "C Z")
			}

			var v string
			if err := admin.QueryRow(testCtx(t), "select v from "+table+" where id = 1").Scan(&v); err != nil || v != "a" {
				t.Errorf("row 1 has v = %q (%v), want a", v, err)
			}
			if n := g.beginCount(); n != 0 {
				t.Errorf("%d writes begun, want none: a FETCH or MOVE is not judged", n)
			}
			if p.Enforce {
				g.wantBlocked(t, "stet_trust", proxy.BlockPreparedWrite, proxy.BlockPreparedWrite)
			}
		})
	}
}

// A name changed by a message the server has not answered yet may mean
// either statement (spec correction B4). When one of them is a watched write,
// an enforced session refuses the Execute of a portal bound to it, without
// asking the gate; the shadow proxy goes by the newest meaning, as before. A
// name none of whose meanings is watched runs.
func TestUnsureNameIsRefused(t *testing.T) {
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			table := watchedTable(t)
			g := newRecordingGate(watches(table))
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			fe := rawAgent(t, addr, table)
			p := mode.policy
			reparse := func(name, sql string) {
				t.Helper()
				sendAll(t, fe, &pgproto3.Parse{Name: name, Query: sql}, &pgproto3.Sync{})
				wantReplies(t, fe, "1 Z")
				// The re-Parse fails (42P05), but the Bind and Execute
				// behind it arrive before the server says so.
				sendAll(t, fe, &pgproto3.Parse{Name: name, Query: "select 1"}, &pgproto3.Sync{},
					&pgproto3.Bind{PreparedStatement: name}, &pgproto3.Execute{}, &pgproto3.Sync{})
				wantReplies(t, fe, "E:42P05 Z")
			}

			reparse("s1", "insert into "+table+" values (1, 'x')")
			if p.Enforce {
				wantReplies(t, fe, "2 E:ST002 Z", "blocked unsettled_name")
			} else {
				wantReplies(t, fe, "2 C Z")
			}
			if n, want := rowCount(t, table), pick(p, "0", "1"); fmt.Sprint(n) != want {
				t.Errorf("%d rows, want %s", n, want)
			}
			if n := g.beginCount(); n != 0 {
				t.Errorf("%d writes begun, want none", n)
			}

			reparse("s2", "select 2")
			wantReplies(t, fe, "2 D C Z")
			if p.Enforce {
				g.wantBlocked(t, "stet_trust", proxy.BlockUnsettledName)
			} else {
				g.wantBlocked(t, "stet_trust")
			}
		})
	}
}

// rawSessionWith is rawSession with protocol 3.0 and more startup
// parameters; it also returns the server process's id.
func rawSessionWith(t *testing.T, addr, user string, params map[string]string) (*pgproto3.Frontend, uint32) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	all := map[string]string{"user": user, "database": "stet_test"}
	for k, v := range params {
		all[k] = v
	}
	fe := pgproto3.NewFrontend(conn, conn)
	fe.Send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber, Parameters: all})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	var pid uint32
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatal(err)
		}
		switch m := msg.(type) {
		case *pgproto3.BackendKeyData:
			pid = m.ProcessID
		case *pgproto3.ErrorResponse:
			t.Fatalf("startup: SQLSTATE %s: %s", m.Code, m.Message)
		case *pgproto3.ReadyForQuery:
			return fe, pid
		}
	}
}

// wantEnded reads what the client gets until its connection ends, and fails
// the test unless that is want (as replies writes it) followed by a FATAL
// ST002 for reason, the last thing read.
func wantEnded(t *testing.T, fe *pgproto3.Frontend, want, reason string) {
	t.Helper()
	var got []string
	var fatal *pgproto3.ErrorResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			break
		}
		if fatal != nil {
			t.Errorf("read %T after the FATAL", msg)
		}
		if e, ok := msg.(*pgproto3.ErrorResponse); ok && e.Severity == "FATAL" {
			c := *e
			fatal = &c
			continue
		}
		if e, ok := msg.(*pgproto3.ErrorResponse); ok {
			got = append(got, "E:"+e.Code)
			continue
		}
		b, _ := msg.Encode(nil)
		got = append(got, string(b[0]))
	}
	if s := strings.Join(got, " "); s != want {
		t.Errorf("before the end the client got %q, want %q", s, want)
	}
	if fatal == nil || fatal.Code != "ST002" || fatal.SeverityUnlocalized != "FATAL" || fatal.Message != "blocked "+reason {
		t.Errorf("the session ended with %+v, want a FATAL ST002 blocked %s", fatal, reason)
	}
}

// waitForBackendGone fails the test unless the server process pid exits.
func waitForBackendGone(t *testing.T, pid uint32) {
	t.Helper()
	admin := connect(t, upstream(t), "stet_scram", nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := admin.QueryRow(testCtx(t), "select count(*) from pg_stat_activity where pid = $1", pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server process %d still runs", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Under client_encoding SJIS a multibyte character can end in a backslash,
// and with standard_conforming_strings off \' escapes a quote: the server
// then ends a string literal where the classifier does not, and runs a write
// the classifier takes for part of a string (ruling 22). An enforced session
// refuses every statement until the server reports UTF8 and on; the shadow
// proxy forwards these as before.
func TestStatementsTheClassifierMisreadsAreRefused(t *testing.T) {
	for _, c := range []struct {
		name   string
		params map[string]string
		sql    string // %s is the table
	}{
		{"client_encoding SJIS", map[string]string{"client_encoding": "SJIS"},
			"select E'\xc5\x95\x5c'; insert into %s values (1, 'x'); select count(*) from %[1]s; -- '"},
		{"standard_conforming_strings off", map[string]string{"options": "-c standard_conforming_strings=off"},
			"select 'x\\''; insert into %s values (1, 'x'); select count(*) from %[1]s; -- '"},
	} {
		for _, mode := range closedPointModes {
			t.Run(c.name+", "+mode.name, func(t *testing.T) {
				table := watchedTable(t)
				mustExec(t, connect(t, upstream(t), "stet_scram", nil), "grant select, insert on "+table+" to stet_trust")
				g := closedPointGate(watches(table))
				addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
				fe, _ := rawSessionWith(t, addr, "stet_trust", c.params)

				sendAll(t, fe, &pgproto3.Query{String: fmt.Sprintf(c.sql, table)})
				got, errs := repliesWithErrors(t, fe)
				if mode.policy.Enforce {
					if got != "E:ST002 Z" || len(errs) != 1 || errs[0].Message != "blocked session_setting" {
						t.Errorf("replies %q (%+v), want the ST002 for session_setting", got, errs)
					}
					g.wantBlocked(t, "stet_trust", proxy.BlockSessionSetting)
				} else if len(errs) != 0 {
					t.Errorf("replies %q, want the statements run: %+v", got, errs)
				}
				if n, want := rowCount(t, table), pick(mode.policy, "0", "1"); fmt.Sprint(n) != want {
					t.Errorf("%d rows, want %s", n, want)
				}
				if n := g.beginCount(); n != 0 {
					t.Errorf("%d writes begun, want none: the classifier saw no write", n)
				}
			})
		}
	}
}

// A session that starts with settings the classifier cannot read can repair
// them with a SET, and then works as any other: its writes are judged.
func TestUnreadableSessionRepairsItsSettings(t *testing.T) {
	table := watchedTable(t)
	mustExec(t, connect(t, upstream(t), "stet_scram", nil), "grant insert on "+table+" to stet_trust")
	g := closedPointGate(watches(table))
	addr, _ := startProxyWith(t, upstream(t), g, proxy.Policy{Enforce: true})
	fe, _ := rawSessionWith(t, addr, "stet_trust", map[string]string{"client_encoding": "SJIS"})
	insert := &pgproto3.Query{String: "insert into " + table + " values (1, 'x')"}

	sendAll(t, fe, insert)
	wantReplies(t, fe, "E:ST002 Z", "blocked session_setting")
	sendAll(t, fe, &pgproto3.Query{String: "set client_encoding to 'UTF8'"})
	wantReplies(t, fe, "C S Z")
	sendAll(t, fe, insert)
	wantReplies(t, fe, "C Z")
	if n := g.beginCount(); n != 1 {
		t.Errorf("%d writes begun, want the insert after the repair", n)
	}
	if n := rowCount(t, table); n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
	g.wantBlocked(t, "stet_trust", proxy.BlockSessionSetting)
}

// A statement sent behind a change of setting, before the server reported
// the change, was inspected under the old value. When the change makes the
// session unreadable, an enforced session is ended with a FATAL, and so is
// its server session (ruling 22). The shadow proxy answers both.
func TestSettingChangedAheadOfAStatementEndsTheSession(t *testing.T) {
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			g := closedPointGate(isWrite)
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			fe, pid := rawSessionWith(t, addr, "stet_trust", nil)

			sendAll(t, fe, &pgproto3.Query{String: "set client_encoding to 'SJIS'"}, &pgproto3.Query{String: "select 1"})
			if !mode.policy.Enforce {
				wantReplies(t, fe, "C S Z")
				wantReplies(t, fe, "T D C Z")
				return
			}
			wantEnded(t, fe, "C", proxy.BlockSettingChanged)
			waitForBackendGone(t, pid)
			g.wantBlocked(t, "stet_trust", proxy.BlockSettingChanged)
		})
	}
}

// COPY FROM STDIN through the extended protocol makes the server ignore the
// Sync after the Execute (spec correction B6 ②), so its answers no longer
// match the units the proxy follows. An enforced session is ended with a
// FATAL when the server starts such a copy, and so is its server session; a
// simple-protocol COPY on the same proxy works. The shadow proxy relays it.
func TestCopyFromStdinThroughTheExtendedProtocolEndsTheSession(t *testing.T) {
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			g := closedPointGate(isWrite)
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			fe, pid := rawSessionWith(t, addr, "stet_trust", nil)

			sendAll(t, fe, &pgproto3.Query{String: "create temp table copied (i int)"})
			wantReplies(t, fe, "C Z")
			sendAll(t, fe, &pgproto3.Parse{Query: "copy copied from stdin"}, &pgproto3.Bind{}, &pgproto3.Execute{}, &pgproto3.Sync{})
			if mode.policy.Enforce {
				wantEnded(t, fe, "1 2", proxy.BlockCopyExtended)
				waitForBackendGone(t, pid)
				g.wantBlocked(t, "stet_trust", proxy.BlockCopyExtended)

				conn := connect(t, addr, "stet_scram", nil)
				mustExec(t, conn, "create temp table copied (i int)")
				n, err := conn.CopyFrom(testCtx(t), pgx.Identifier{"copied"}, []string{"i"}, pgx.CopyFromRows([][]any{{1}, {2}}))
				if err != nil || n != 2 {
					t.Errorf("simple-protocol COPY: %d rows, %v; want 2 rows", n, err)
				}
				return
			}
			if got := nextReplies(t, fe, 3); got != "1 2 G" {
				t.Fatalf("replies %q, want %q", got, "1 2 G")
			}
			sendAll(t, fe, &pgproto3.CopyData{Data: []byte("1\n")}, &pgproto3.CopyDone{}, &pgproto3.Sync{})
			wantReplies(t, fe, "C Z")
			sendAll(t, fe, &pgproto3.Query{String: "select count(*) from copied"})
			wantReplies(t, fe, "T D C Z")
		})
	}
}

// The server ignores a Sync during a simple-protocol COPY FROM STDIN (spec
// correction B6 ③), so its answers no longer match the units the proxy
// follows. On an enforced session a message the proxy follows, other than
// Flush, that the server reads between CopyInResponse and the client's
// CopyDone or CopyFail ends the session with a FATAL: sent after the
// CopyInResponse, or with the COPY in one write. A client that sends CopyDone
// and its next Query in one write keeps its session (ruling P10).
func TestSyncDuringCopyEndsTheSession(t *testing.T) {
	copyQuery := &pgproto3.Query{String: "copy copied from stdin"}
	open := func(t *testing.T, policy proxy.Policy) (*pgproto3.Frontend, uint32, *recordingGate) {
		t.Helper()
		g := closedPointGate(isWrite)
		addr, _ := startProxyWith(t, upstream(t), g, policy)
		fe, pid := rawSessionWith(t, addr, "stet_trust", nil)
		sendAll(t, fe, &pgproto3.Query{String: "create temp table copied (i int)"})
		wantReplies(t, fe, "C Z")
		return fe, pid, g
	}
	enforce := proxy.Policy{Enforce: true}

	t.Run("a Sync after CopyInResponse", func(t *testing.T) {
		fe, pid, g := open(t, enforce)
		sendAll(t, fe, copyQuery)
		if got := nextReplies(t, fe, 1); got != "G" {
			t.Fatalf("replies %q, want G", got)
		}
		sendAll(t, fe, &pgproto3.Sync{})
		wantEnded(t, fe, "", proxy.BlockProtocolOrder)
		waitForBackendGone(t, pid)
		g.wantBlocked(t, "stet_trust", proxy.BlockProtocolOrder)
	})

	t.Run("a Sync in one write with the COPY", func(t *testing.T) {
		fe, pid, g := open(t, enforce)
		sendAll(t, fe, copyQuery, &pgproto3.Sync{})
		wantEnded(t, fe, "", proxy.BlockProtocolOrder)
		waitForBackendGone(t, pid)
		g.wantBlocked(t, "stet_trust", proxy.BlockProtocolOrder)
	})

	t.Run("a Flush during the copy", func(t *testing.T) {
		fe, _, g := open(t, enforce)
		sendAll(t, fe, copyQuery)
		if got := nextReplies(t, fe, 1); got != "G" {
			t.Fatalf("replies %q, want G", got)
		}
		sendAll(t, fe, &pgproto3.Flush{}, &pgproto3.CopyData{Data: []byte("1\n")}, &pgproto3.CopyDone{})
		wantReplies(t, fe, "C Z")
		sendAll(t, fe, &pgproto3.Query{String: "select count(*) from copied"})
		wantReplies(t, fe, "T D C Z")
		g.wantBlocked(t, "stet_trust")
	})

	for _, mode := range closedPointModes {
		t.Run("CopyDone and the next Query in one write, "+mode.name, func(t *testing.T) {
			fe, _, g := open(t, mode.policy)
			sendAll(t, fe, copyQuery)
			if got := nextReplies(t, fe, 1); got != "G" {
				t.Fatalf("replies %q, want G", got)
			}
			sendAll(t, fe, &pgproto3.CopyData{Data: []byte("1\n")}, &pgproto3.CopyDone{},
				&pgproto3.Query{String: "select count(*) from copied"})
			wantReplies(t, fe, "C Z")
			wantReplies(t, fe, "T D C Z")
			sendAll(t, fe, &pgproto3.Query{String: "select 1"})
			wantReplies(t, fe, "T D C Z")
			g.wantBlocked(t, "stet_trust")
		})
	}

	// The shadow proxy relays a Sync during the copy; the server ignores it.
	t.Run("a Sync after CopyInResponse, shadow", func(t *testing.T) {
		fe, _, _ := open(t, proxy.Policy{})
		sendAll(t, fe, copyQuery)
		if got := nextReplies(t, fe, 1); got != "G" {
			t.Fatalf("replies %q, want G", got)
		}
		sendAll(t, fe, &pgproto3.Sync{}, &pgproto3.CopyData{Data: []byte("1\n")}, &pgproto3.CopyDone{})
		wantReplies(t, fe, "C Z")
	})
}

// readyOrEnd reads up to the next ReadyForQuery, or until the connection
// ends; it returns the replies (as replies writes them, a FATAL left out)
// and the FATAL that ended the connection, nil when it was not ended.
func readyOrEnd(t *testing.T, fe *pgproto3.Frontend) (string, *pgproto3.ErrorResponse) {
	t.Helper()
	var got []string
	var fatal *pgproto3.ErrorResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			if fatal == nil {
				t.Fatalf("the connection ended without a FATAL after %q: %v", got, err)
			}
			return strings.Join(got, " "), fatal
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			if m.Severity == "FATAL" {
				c := *m
				fatal = &c
				continue
			}
			got = append(got, "E:"+m.Code)
		default:
			b, _ := msg.Encode(nil)
			got = append(got, string(b[0]))
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok && fatal == nil {
			return strings.Join(got, " "), nil
		}
	}
}

// wantFatal fails the test unless fatal is the proxy's FATAL ST002 for reason.
func wantFatal(t *testing.T, fatal *pgproto3.ErrorResponse, reason string) {
	t.Helper()
	if fatal == nil || fatal.Code != "ST002" || fatal.SeverityUnlocalized != "FATAL" || fatal.Message != "blocked "+reason {
		t.Errorf("the session ended with %+v, want a FATAL ST002 blocked %s", fatal, reason)
	}
}

// A Query of two COPY FROM STDIN statements starts its second copy after the
// client's first CopyDone. The server ignores a Sync during the second copy
// as during the first, so an enforced session is ended there. Were it kept,
// the proxy's units would lag behind the server's answers, and a watched
// write sent later would run unjudged.
func TestSyncDuringASecondCopyEndsTheSession(t *testing.T) {
	table := watchedTable(t)
	mustExec(t, connect(t, upstream(t), "stet_scram", nil), "grant insert on "+table+" to stet_trust")
	g := closedPointGate(watches(table))
	addr, _ := startProxyWith(t, upstream(t), g, proxy.Policy{Enforce: true})
	fe, pid := rawSessionWith(t, addr, "stet_trust", nil)
	sendAll(t, fe, &pgproto3.Query{String: "create temp table copied (i int)"})
	wantReplies(t, fe, "C Z")
	data := &pgproto3.CopyData{Data: []byte("1\n")}

	sendAll(t, fe, &pgproto3.Query{String: "copy copied from stdin; copy copied from stdin"})
	if got := nextReplies(t, fe, 1); got != "G" {
		t.Fatalf("replies %q, want G", got)
	}
	sendAll(t, fe, data, &pgproto3.CopyDone{})
	if got := nextReplies(t, fe, 2); got != "C G" {
		t.Fatalf("replies %q, want C G", got)
	}
	sendAll(t, fe, &pgproto3.Sync{}, data, &pgproto3.CopyDone{})
	got, fatal := readyOrEnd(t, fe)
	if fatal == nil {
		// Kept: follow the probe to what that costs.
		sendAll(t, fe, &pgproto3.Parse{Name: "s1", Query: "insert into " + table + " values (1, 'x')"}, &pgproto3.Sync{})
		replies(t, fe)
		sendAll(t, fe, &pgproto3.Sync{})
		replies(t, fe)
		sendAll(t, fe, &pgproto3.Bind{PreparedStatement: "s1"}, &pgproto3.Execute{}, &pgproto3.Sync{})
		replies(t, fe)
		t.Fatalf("the session was kept (replies %q); a watched write sent after it began %d times and inserted %d rows",
			got, g.beginCount(), rowCount(t, table))
	}
	if got != "" {
		t.Errorf("before the end the client got %q, want nothing", got)
	}
	wantFatal(t, fatal, proxy.BlockProtocolOrder)
	waitForBackendGone(t, pid)
	g.wantBlocked(t, "stet_trust", proxy.BlockProtocolOrder)
}

// A Sync sent after the first CopyDone, before the second copy starts, is
// read by the server during the second copy and ignored: the session ends
// when that copy starts.
func TestSyncBeforeASecondCopyEndsTheSession(t *testing.T) {
	g := closedPointGate(isWrite)
	addr, _ := startProxyWith(t, upstream(t), g, proxy.Policy{Enforce: true})
	fe, pid := rawSessionWith(t, addr, "stet_trust", nil)
	sendAll(t, fe, &pgproto3.Query{String: "create temp table copied (i int)"})
	wantReplies(t, fe, "C Z")

	sendAll(t, fe, &pgproto3.Query{String: "copy copied from stdin; copy copied from stdin"})
	if got := nextReplies(t, fe, 1); got != "G" {
		t.Fatalf("replies %q, want G", got)
	}
	sendAll(t, fe, &pgproto3.CopyData{Data: []byte("1\n")}, &pgproto3.CopyDone{}, &pgproto3.Sync{})
	got, fatal := readyOrEnd(t, fe)
	// The proxy reads the Sync long before the second CopyInResponse
	// arrives; were it the other way round, that G would reach the client.
	if got != "C" && got != "C G" {
		t.Errorf("before the end the client got %q, want C", got)
	}
	wantFatal(t, fatal, proxy.BlockProtocolOrder)
	waitForBackendGone(t, pid)
	g.wantBlocked(t, "stet_trust", proxy.BlockProtocolOrder)
}

// Two COPY FROM STDIN statements in one Query, each ended by its own
// CopyDone with nothing else sent during either copy, run on an enforced
// session, whether the copy data waits for each CopyInResponse or not; the
// session goes on, and a watched write after it is judged.
func TestTwoCopiesInOneQueryKeepTheSession(t *testing.T) {
	table := watchedTable(t)
	mustExec(t, connect(t, upstream(t), "stet_scram", nil), "grant insert on "+table+" to stet_trust")
	g := closedPointGate(watches(table))
	addr, _ := startProxyWith(t, upstream(t), g, proxy.Policy{Enforce: true})
	fe, _ := rawSessionWith(t, addr, "stet_trust", nil)
	sendAll(t, fe, &pgproto3.Query{String: "create temp table copied (i int)"})
	wantReplies(t, fe, "C Z")
	twoCopies := &pgproto3.Query{String: "copy copied from stdin; copy copied from stdin"}
	data := &pgproto3.CopyData{Data: []byte("1\n")}

	sendAll(t, fe, twoCopies)
	if got := nextReplies(t, fe, 1); got != "G" {
		t.Fatalf("replies %q, want G", got)
	}
	sendAll(t, fe, data, &pgproto3.CopyDone{})
	if got := nextReplies(t, fe, 2); got != "C G" {
		t.Fatalf("replies %q, want C G", got)
	}
	sendAll(t, fe, data, &pgproto3.CopyDone{})
	wantReplies(t, fe, "C Z")

	sendAll(t, fe, twoCopies, data, &pgproto3.CopyDone{}, data, &pgproto3.CopyDone{})
	wantReplies(t, fe, "G C G C Z")

	sendAll(t, fe, &pgproto3.Query{String: "select count(*) from copied"})
	wantReplies(t, fe, "T D C Z")
	sendAll(t, fe, &pgproto3.Parse{Name: "s1", Query: "insert into " + table + " values (1, 'x')"},
		&pgproto3.Bind{PreparedStatement: "s1"}, &pgproto3.Execute{}, &pgproto3.Sync{})
	wantReplies(t, fe, "1 2 C Z")
	if n := g.beginCount(); n != 1 {
		t.Errorf("%d writes begun, want the insert judged", n)
	}
	if n := rowCount(t, table); n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
	g.wantBlocked(t, "stet_trust")
}

// PostgreSQL keeps the spelling UNICODE of client_encoding, for old JDBC
// drivers, and reports it so; it is UTF8. A session started with it works on
// an enforced session: its writes are judged.
func TestClientEncodingUnicodeIsUTF8(t *testing.T) {
	table := watchedTable(t)
	mustExec(t, connect(t, upstream(t), "stet_scram", nil), "grant insert on "+table+" to stet_trust")
	g := closedPointGate(watches(table))
	addr, _ := startProxyWith(t, upstream(t), g, proxy.Policy{Enforce: true})
	fe, _ := rawSessionWith(t, addr, "stet_trust", map[string]string{"client_encoding": "UNICODE"})

	sendAll(t, fe, &pgproto3.Query{String: "insert into " + table + " values (1, 'x')"})
	wantReplies(t, fe, "C Z")
	if n := g.beginCount(); n != 1 {
		t.Errorf("%d writes begun, want the insert judged", n)
	}
	g.wantBlocked(t, "stet_trust")
}

// panicGate is recordingGate that panics in Inspect for SQL holding
// "panic_inspect", and in Begin for a write holding "panic_begin".
type panicGate struct{ *recordingGate }

func (g panicGate) Inspect(sql string) proxy.Statement {
	if strings.Contains(sql, "panic_inspect") {
		panic("test gate: Inspect of " + sql)
	}
	return g.recordingGate.Inspect(sql)
}

func (g panicGate) Begin(info proxy.SessionInfo, st proxy.Statement, ps []proxy.Param) (any, *proxy.Refusal) {
	if strings.Contains(st.SQL, "panic_begin") {
		panic("test gate: Begin of " + st.SQL)
	}
	return g.recordingGate.Begin(info, st, ps)
}

// syncLog is a log destination tests read while the proxy writes to it.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// A panic in a session's taps, the gate's Inspect or Begin here, ends that
// session alone: both its connections are closed and the message being
// inspected is not forwarded, so the write that panicked never runs. The
// panic is logged as an error with the session and the stack, a write the
// session had sent before it is still reported (End once), and other
// sessions, and the process, go on.
func TestPanicEndsOnlyItsSession(t *testing.T) {
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			table := watchedTable(t)
			mustExec(t, connect(t, upstream(t), "stet_scram", nil), "grant insert on "+table+" to stet_trust")
			g := panicGate{newRecordingGate(watches(table))}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var logs syncLog
			srv := proxy.New(upstream(t), slog.New(slog.NewTextHandler(&logs, nil)), g, mode.policy)
			go srv.Serve(ln)
			t.Cleanup(func() { srv.Close() })
			addr := ln.Addr().String()
			ctx := testCtx(t)

			other := connect(t, addr, "stet_scram", nil)
			mustExec(t, other, "insert into "+table+" values (1, 'before')")

			// Begin panics: the write is not sent.
			victim := connect(t, addr, "stet_scram", nil)
			if _, err := victim.Exec(ctx, "insert into "+table+" values (2, 'panic_begin')"); err == nil {
				t.Error("the write whose Begin panicked succeeded")
			}
			// Inspect panics on the message after a write already sent in
			// the same pipeline, which is reported once all the same.
			fe := rawSession(t, addr, "stet_trust", pgproto3.ProtocolVersionNumber)
			sendAll(t, fe, &pgproto3.Query{String: "insert into " + table + " values (3, 'sent')"},
				&pgproto3.Query{String: "insert into " + table + " values (4, 'panic_inspect')"})
			for {
				if _, err := fe.Receive(); err != nil {
					break // the proxy closed the session
				}
			}

			mustExec(t, other, "insert into "+table+" values (5, 'after')")
			mustExec(t, connect(t, addr, "stet_scram", nil), "insert into "+table+" values (6, 'new session')")

			var ids []int
			rows, err := other.Query(ctx, "select id from "+table+" order by id")
			if err != nil {
				t.Fatal(err)
			}
			ids, err = pgx.CollectRows(rows, pgx.RowTo[int])
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range ids {
				if id == 2 || id == 4 {
					t.Errorf("rows %v: the write whose %s panicked ran", ids, map[int]string{2: "Begin", 4: "Inspect"}[id])
				}
			}
			bs, os := g.outcomes(t, g.beginCount())
			var sent int
			for i, b := range bs {
				if strings.Contains(b.st.SQL, "'sent'") {
					sent++
					if os[i].Executed && os[i].Lost || !os[i].Executed && !os[i].Lost {
						t.Errorf("the write sent before the panic ended %+v, want it run or lost", os[i])
					}
				}
			}
			g.mu.Lock()
			endCalls := g.endCalls
			g.mu.Unlock()
			if sent != 1 || endCalls != len(bs) {
				t.Errorf("%d writes begun, %d of them the one sent before the panic, %d End calls; want it begun once and each write ended once",
					len(bs), sent, endCalls)
			}
			log := logs.String()
			for _, want := range []string{"level=ERROR", "session ended by a panic", "test gate: Begin of", "test gate: Inspect of", "goroutine"} {
				if !strings.Contains(log, want) {
					t.Errorf("the log does not hold %q:\n%s", want, log)
				}
			}
		})
	}
}

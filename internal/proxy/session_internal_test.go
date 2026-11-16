package proxy

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// fakeGate watches every statement and records each End.
type fakeGate struct {
	n    int
	ends map[int]Outcome
}

func newFakeGate() *fakeGate { return &fakeGate{ends: map[int]Outcome{}} }

func (g *fakeGate) Inspect(sql string) Statement { return Statement{SQL: sql, Watch: true} }
func (g *fakeGate) Begin(SessionInfo, Statement, []Param) any {
	g.n++
	return g.n - 1
}
func (g *fakeGate) End(h any, o Outcome)                    { g.ends[h.(int)] = o }
func (g *fakeGate) Observe(SessionInfo, Statement, Outcome) {}

// body is a message's payload, as the taps hand it to the session.
func body(m pgproto3.Message) []byte {
	b, _ := m.Encode(nil)
	return b[5:]
}

func readySession(g Gate) *session {
	s := newSession(g, SessionInfo{})
	s.fromServer('Z', []byte{'I'}) // post-startup ReadyForQuery
	return s
}

func sendWrite(s *session, name string) {
	flush := func() error { return nil }
	s.fromClient('P', body(&pgproto3.Parse{Name: name, Query: "insert into t values (1)"}), flush)
	s.fromClient('B', body(&pgproto3.Bind{PreparedStatement: name}), flush)
	s.fromClient('E', body(&pgproto3.Execute{}), flush)
}

func serverError() []byte {
	return body(&pgproto3.ErrorResponse{Code: "23505", Message: "duplicate key"})
}

func TestAbandonKeepsTheServersError(t *testing.T) {
	g := newFakeGate()
	s := readySession(g)
	flush := func() error { return nil }
	s.fromClient('Q', body(&pgproto3.Query{String: "insert into t values (1)"}), flush)
	s.fromServer('E', serverError())
	s.abandon()

	o := g.ends[0]
	if o.Lost || o.Executed || !strings.Contains(o.Error, "23505") {
		t.Errorf("refused write: %+v, want the server's 23505 error and not lost", o)
	}
}

func TestAbandonKeepsSkippedWritesNotLost(t *testing.T) {
	g := newFakeGate()
	s := readySession(g)
	sendWrite(s, "a")
	sendWrite(s, "b")
	s.fromClient('S', nil, func() error { return nil })
	s.fromServer('E', serverError())
	s.abandon()

	if o := g.ends[0]; o.Lost || o.Executed || !strings.Contains(o.Error, "23505") {
		t.Errorf("failing write: %+v, want the server's 23505 error and not lost", o)
	}
	if o := g.ends[1]; o.Lost || o.Executed || !strings.Contains(o.Error, "not executed: an earlier statement in the same Sync failed") {
		t.Errorf("skipped write: %+v, want the not-executed text and not lost", o)
	}
}

func TestAbandonLosesUnansweredWrites(t *testing.T) {
	g := newFakeGate()
	s := readySession(g)
	flush := func() error { return nil }
	s.fromClient('Q', body(&pgproto3.Query{String: "insert into t values (1)"}), flush)
	s.abandon()
	if o := g.ends[0]; !o.Lost || o.Executed || o.Error != "connection closed before ReadyForQuery" {
		t.Errorf("unanswered write: %+v, want lost", o)
	}
}

// CommandComplete arrived but ReadyForQuery did not: the write ran, and
// nothing says whether its unit committed, so it is lost as well.
func TestAbandonKnowsACompletedWriteRan(t *testing.T) {
	g := newFakeGate()
	s := readySession(g)
	flush := func() error { return nil }
	s.fromClient('Q', body(&pgproto3.Query{String: "insert into t values (1)"}), flush)
	s.fromServer('C', body(&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")}))
	s.abandon()
	if o := g.ends[0]; !o.Lost || !o.Executed || o.Rows == nil || *o.Rows != 1 {
		t.Errorf("completed but unconfirmed write: %+v, want executed, lost, 1 row", o)
	}

	// Under one Sync the first write completed and the second had not.
	g = newFakeGate()
	s = readySession(g)
	sendWrite(s, "a")
	sendWrite(s, "b")
	s.fromClient('S', nil, flush)
	serve(s, &pgproto3.ParseComplete{}, &pgproto3.BindComplete{}, tag("INSERT 0 1"),
		&pgproto3.ParseComplete{}, &pgproto3.BindComplete{})
	s.abandon()
	if o := g.ends[0]; !o.Lost || !o.Executed || o.Rows == nil || *o.Rows != 1 {
		t.Errorf("completed write under a Sync: %+v, want executed, lost, 1 row", o)
	}
	if o := g.ends[1]; !o.Lost || o.Executed || o.Rows != nil {
		t.Errorf("unanswered write under a Sync: %+v, want lost and not executed", o)
	}

	// A completion in a Query of several statements does not say which one
	// ran.
	tg := &txGate{fakeGate: newFakeGate()}
	s = readySession(tg)
	s.fromClient('Q', body(&pgproto3.Query{String: "select 1; insert into t values (1)"}), flush)
	serve(s, &pgproto3.RowDescription{}, row, tag("SELECT 1"))
	s.abandon()
	if o := tg.ends[0]; !o.Lost || o.Executed {
		t.Errorf("write among several statements: %+v, want lost and not executed", o)
	}
}

// txGate marks begin, set and show clean and watches every message that
// mentions insert, recording the session as each write arrived. The
// statements of a message are separated by "; ".
type txGate struct {
	*fakeGate
	infos []SessionInfo
}

func (g *txGate) Inspect(sql string) Statement {
	st := Statement{SQL: sql, Several: strings.Contains(sql, "; "), Watch: strings.Contains(sql, "insert")}
	switch first, _, _ := strings.Cut(sql, " "); first {
	case "begin", "set", "show":
		st.Clean = !st.Several
	}
	return st
}

func (g *txGate) Begin(info SessionInfo, st Statement, ps []Param) any {
	g.infos = append(g.infos, info)
	return g.fakeGate.Begin(info, st, ps)
}

// drive sends steps through s. A word is a Query of that SQL; Z(x) is the
// server's ReadyForQuery with status x; E:sql is a Parse, Bind and Execute of
// sql, unnamed, in the open unit; E? is an Execute of a portal no Bind named;
// Q! and E! are a Query and an Execute whose bodies could not be read; S is a
// Sync and F a FunctionCall.
func drive(s *session, steps string) {
	flush := func() error { return nil }
	for _, step := range strings.Fields(steps) {
		switch {
		case strings.HasPrefix(step, "Z("):
			s.fromServer('Z', []byte{step[2]})
		case strings.HasPrefix(step, "E:"):
			execute(s, "", step[2:])
		case step == "E?":
			s.fromClient('E', body(&pgproto3.Execute{Portal: "nowhere"}), flush)
		case step == "E!":
			s.fromClient('E', nil, flush)
		case step == "Q!":
			s.fromClient('Q', nil, flush)
		case step == "S":
			s.fromClient('S', nil, flush)
		case step == "F":
			s.fromClient('F', body(&pgproto3.FunctionCall{Function: 1}), flush)
		default:
			s.fromClient('Q', body(&pgproto3.Query{String: step}), flush)
		}
	}
}

// A write's TxDirty says whether anything but BEGIN, SET or SHOW was sent
// since the server last reported the session idle, answered or not (spec 4).
func TestTxDirtyFollowsWhatWasSent(t *testing.T) {
	for _, c := range []struct {
		sent string
		want []bool // TxDirty of each insert, in order
	}{
		{"insert", []bool{false}},
		{"begin Z(T) insert Z(T) insert", []bool{false, true}},
		{"begin Z(T) set Z(T) show Z(T) insert", []bool{false}},
		{"begin Z(T) select Z(T) insert", []bool{true}},
		{"begin insert", []bool{false}},
		{"select insert Z(I) Z(I) insert", []bool{true, false}},
		{"select begin Z(I) insert", []bool{false}},
		{"select begin select Z(I) insert", []bool{true}},
		{"E:begin E:insert E:insert S", []bool{false, true}},
		{"E:select E:insert S", []bool{true}},
		{"E? E:insert S", []bool{true}},
		{"F insert", []bool{true}},
		{"begin Z(T) garbage Z(T) insert", []bool{true}},
		{"commit Z(I) insert", []bool{false}},
		// Beyond the plan's table: dirt carried by the session until an idle
		// ReadyForQuery, and messages the proxy could not read.
		{"begin Z(T) select Z(T) commit Z(I) insert", []bool{false}},
		{"begin Z(T) select Z(E) insert", []bool{true}},
		{"begin Z(T) Q! Z(T) insert", []bool{true}},
		{"E:begin E! E:insert S", []bool{true}},
	} {
		g := &txGate{fakeGate: newFakeGate()}
		drive(readySession(g), c.sent)
		var got []bool
		for _, info := range g.infos {
			got = append(got, info.TxDirty)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: TxDirty %v, want %v", c.sent, got, c.want)
		}
	}
}

// readGate watches inserts and observes every other statement, recording
// each read it is told about.
type readGate struct {
	*fakeGate
	both  bool // mark the observed statements watched as well
	reads []Outcome
}

func newReadGate() *readGate { return &readGate{fakeGate: newFakeGate()} }

func (g *readGate) Inspect(sql string) Statement {
	if strings.HasPrefix(sql, "insert") {
		return Statement{SQL: sql, Watch: true}
	}
	return Statement{SQL: sql, Observe: true, Watch: g.both}
}

func (g *readGate) Observe(_ SessionInfo, _ Statement, o Outcome) { g.reads = append(g.reads, o) }

// serve hands the session server messages the way pump does: every type to
// each, and the body of the wanted ones to see.
func serve(s *session, msgs ...pgproto3.BackendMessage) {
	tp := s.serverTap()
	for _, m := range msgs {
		b, _ := m.Encode(nil)
		if tp.each != nil {
			tp.each(b[0])
		}
		if tp.want(b[0]) {
			tp.see(b[0], b[5:], nil)
		}
	}
}

func execute(s *session, name, sql string) {
	flush := func() error { return nil }
	s.fromClient('P', body(&pgproto3.Parse{Name: name, Query: sql}), flush)
	s.fromClient('B', body(&pgproto3.Bind{PreparedStatement: name}), flush)
	s.fromClient('E', body(&pgproto3.Execute{}), flush)
}

var (
	row   = &pgproto3.DataRow{Values: [][]byte{[]byte("1")}}
	ready = &pgproto3.ReadyForQuery{TxStatus: 'I'}
)

func tag(s string) *pgproto3.CommandComplete { return &pgproto3.CommandComplete{CommandTag: []byte(s)} }

func TestRowsCountTowardTheirOwnExecute(t *testing.T) {
	g := newReadGate()
	s := readySession(g)
	execute(s, "a", "select a")
	execute(s, "w", "insert into t values (1) returning *")
	execute(s, "b", "select b")
	s.fromClient('S', nil, func() error { return nil })
	serve(s,
		&pgproto3.ParseComplete{}, &pgproto3.BindComplete{}, row, row, tag("SELECT 2"),
		&pgproto3.ParseComplete{}, &pgproto3.BindComplete{}, row, tag("INSERT 0 1"),
		&pgproto3.ParseComplete{}, &pgproto3.BindComplete{}, row, row, row,
		&pgproto3.ErrorResponse{Code: "22012", Message: "division by zero"},
		ready)

	if len(g.reads) != 2 {
		t.Fatalf("%d reads observed, want 2", len(g.reads))
	}
	if o := g.reads[0]; o.Sent != 2 || !o.Executed || o.Rows == nil || *o.Rows != 2 {
		t.Errorf("first read: %+v, want 2 rows sent and executed", o)
	}
	if o := g.reads[1]; o.Sent != 3 || o.Executed || !strings.Contains(o.Error, "22012") {
		t.Errorf("failing read: %+v, want its own 3 rows and the 22012 error", o)
	}
	if o := g.ends[0]; !o.Executed {
		t.Errorf("write between the reads: %+v, want executed", o)
	}
}

func TestRowsOfAUnitWithoutReadsAreDropped(t *testing.T) {
	g := newReadGate()
	s := readySession(g)
	flush := func() error { return nil }
	s.fromClient('Q', body(&pgproto3.Query{String: "insert into t values (1) returning *"}), flush)
	serve(s, &pgproto3.RowDescription{}, row, row, tag("INSERT 0 2"), ready)
	s.fromClient('Q', body(&pgproto3.Query{String: "select 1 where false"}), flush)
	serve(s, &pgproto3.RowDescription{}, tag("SELECT 0"), ready)

	if len(g.reads) != 1 || g.reads[0].Sent != 0 {
		t.Errorf("reads = %+v, want one with no rows sent", g.reads)
	}
}

func TestAbandonReportsRowsInFlight(t *testing.T) {
	g := newReadGate()
	s := readySession(g)
	flush := func() error { return nil }
	s.fromClient('Q', body(&pgproto3.Query{String: "select * from t"}), flush)
	serve(s, &pgproto3.RowDescription{}, row, row, row)
	s.abandon()
	if len(g.reads) != 1 {
		t.Fatalf("%d reads observed, want 1", len(g.reads))
	}
	if o := g.reads[0]; !o.Lost || o.Executed || o.Sent != 3 || o.Error != "connection closed before ReadyForQuery" {
		t.Errorf("read cut off: %+v, want lost with 3 rows sent", o)
	}

	// Two Executes under one Sync: the first completed, the second was
	// still sending.
	g = newReadGate()
	s = readySession(g)
	execute(s, "a", "select a")
	execute(s, "b", "select b")
	s.fromClient('S', nil, flush)
	serve(s, &pgproto3.ParseComplete{}, &pgproto3.BindComplete{}, row, row, tag("SELECT 2"),
		&pgproto3.ParseComplete{}, &pgproto3.BindComplete{}, row)
	s.abandon()
	if len(g.reads) != 2 {
		t.Fatalf("%d reads observed, want 2", len(g.reads))
	}
	for i, want := range []int64{2, 1} {
		if o := g.reads[i]; !o.Lost || o.Executed || o.Sent != want {
			t.Errorf("read %d: %+v, want lost with %d rows sent", i, o, want)
		}
	}
}

// A read reported by finish is not reported again when the connection then
// ends.
func TestAnsweredReadIsObservedOnce(t *testing.T) {
	g := newReadGate()
	s := readySession(g)
	s.fromClient('Q', body(&pgproto3.Query{String: "select 1"}), func() error { return nil })
	serve(s, &pgproto3.RowDescription{}, row, tag("SELECT 1"), ready)
	s.abandon()
	if len(g.reads) != 1 || g.reads[0].Lost || !g.reads[0].Executed {
		t.Errorf("reads = %+v, want the one answered read, once", g.reads)
	}
}

func TestWatchWinsOverObserve(t *testing.T) {
	g := newReadGate()
	g.both = true
	s := readySession(g)
	s.fromClient('Q', body(&pgproto3.Query{String: "select 1"}), func() error { return nil })
	serve(s, &pgproto3.RowDescription{}, row, tag("SELECT 1"), ready)
	if g.n != 1 || len(g.reads) != 0 {
		t.Errorf("Begin ran %d times and Observe %d, want a watched write only", g.n, len(g.reads))
	}
	if o := g.ends[0]; !o.Executed {
		t.Errorf("write: %+v, want executed", o)
	}
}

func TestObservedPortalKeepsNoBindValues(t *testing.T) {
	g := newReadGate()
	s := readySession(g)
	flush := func() error { return nil }
	s.fromClient('P', body(&pgproto3.Parse{Name: "r", Query: "select * from t where v = $1"}), flush)
	s.fromClient('B', body(&pgproto3.Bind{PreparedStatement: "r", DestinationPortal: "p", Parameters: [][]byte{[]byte("secret")}}), flush)
	b, ok := s.portals["p"]
	if !ok || !b.st.Observe {
		t.Fatalf("portal = %+v (found %v), want the observed read remembered", b, ok)
	}
	if b.params != nil {
		t.Errorf("portal kept bind values %+v, want none", b.params)
	}
}

// cursorGate marks cursor statements as the real gate does, observes the
// statements that read t (a DECLARE over t included), and records each read
// with its statement. The statements of a message are separated by "; ".
type cursorGate struct {
	*fakeGate
	reads  []observedRead
	sent   []int64
	before []int64
}

func (g *cursorGate) Inspect(sql string) Statement {
	st := Statement{SQL: sql}
	parts := strings.Split(sql, "; ")
	st.Several = len(parts) > 1
	for _, p := range parts {
		f := strings.Fields(p)
		switch f[0] {
		case "declare":
			st.Cursors = append(st.Cursors, Cursor{Op: CursorDeclare, Name: f[1], Hold: strings.Contains(p, " with hold ")})
		case "fetch":
			st.Cursors = append(st.Cursors, Cursor{Op: CursorFetch, Name: f[len(f)-1]})
		case "close":
			if f[1] == "all" {
				st.Cursors = append(st.Cursors, Cursor{Op: CursorCloseAll})
			} else {
				st.Cursors = append(st.Cursors, Cursor{Op: CursorClose, Name: f[1]})
			}
		}
		if strings.HasSuffix(p, " from t") {
			st.Observe = true
		}
	}
	return st
}

func (g *cursorGate) Observe(info SessionInfo, st Statement, o Outcome) {
	g.reads = append(g.reads, observedRead{info: info, st: st})
	g.sent = append(g.sent, o.Sent)
	g.before = append(g.before, o.Before)
}

func readyWith(b byte) *pgproto3.ReadyForQuery { return &pgproto3.ReadyForQuery{TxStatus: b} }

// A transaction that ends while a DECLARE pipelined behind it is still
// unanswered does not take that cursor along.
func TestTransactionEndSparesCursorsDeclaredBehindIt(t *testing.T) {
	g := &cursorGate{fakeGate: newFakeGate()}
	s := readySession(g)
	flush := func() error { return nil }
	query := func(sql string) { s.fromClient('Q', body(&pgproto3.Query{String: sql}), flush) }

	declare := "declare c cursor for select * from t"
	query("select 1")
	query("begin")
	query(declare)
	serve(s, &pgproto3.RowDescription{}, row, tag("SELECT 1"), readyWith('I'))
	serve(s, tag("BEGIN"), readyWith('T'))
	serve(s, tag("DECLARE CURSOR"), readyWith('T'))
	query("fetch 2 from c")
	serve(s, &pgproto3.RowDescription{}, row, row, tag("FETCH 2"), readyWith('T'))

	if len(g.reads) != 1 || g.reads[0].st.SQL != declare || g.sent[0] != 2 {
		t.Errorf("reads = %+v sent %v, want the FETCH as a 2-row read of %q", g.reads, g.sent, declare)
	}
}

// A FETCH pipelined behind a CLOSE of its cursor is not a read, although the
// CLOSE has not been answered yet.
func TestFetchBehindACloseIsNotReported(t *testing.T) {
	g := &cursorGate{fakeGate: newFakeGate()}
	s := readySession(g)
	flush := func() error { return nil }
	query := func(sql string) { s.fromClient('Q', body(&pgproto3.Query{String: sql}), flush) }

	query("declare c cursor with hold for select * from t")
	serve(s, tag("DECLARE CURSOR"), readyWith('I'))
	query("close c")
	query("fetch 1 from c")
	serve(s, tag("CLOSE CURSOR"), readyWith('I'))
	serve(s, &pgproto3.ErrorResponse{Code: "34000", Message: `cursor "c" does not exist`}, readyWith('I'))

	if len(g.reads) != 0 || len(s.cursors) != 0 {
		t.Errorf("reads = %+v, cursors = %v; want no read and nothing remembered", g.reads, s.cursors)
	}
}

// Only the names whose newest DECLARE or CLOSE can still change are walked
// at a ReadyForQuery: a held cursor that outlived its transaction is not.
func TestSettledCursorsAreNotRevisited(t *testing.T) {
	g := &cursorGate{fakeGate: newFakeGate()}
	s := readySession(g)
	flush := func() error { return nil }
	query := func(sql string) { s.fromClient('Q', body(&pgproto3.Query{String: sql}), flush) }

	query("declare c cursor with hold for select * from t")
	serve(s, tag("DECLARE CURSOR"), readyWith('I'))
	if len(s.cursors) != 1 || len(s.unsettled) != 0 {
		t.Fatalf("after a held DECLARE committed: cursors %v, unsettled %v; want c remembered and settled", s.cursors, s.unsettled)
	}
	query("begin")
	query("declare d cursor for select * from t")
	serve(s, tag("BEGIN"), readyWith('T'), tag("DECLARE CURSOR"), readyWith('T'))
	if _, ok := s.unsettled["d"]; !ok || len(s.unsettled) != 1 {
		t.Errorf("inside the transaction: unsettled %v, want only d", s.unsettled)
	}
	query("commit")
	serve(s, tag("COMMIT"), readyWith('I'))
	if _, ok := s.cursors["c"]; !ok || len(s.cursors) != 1 || len(s.unsettled) != 0 {
		t.Errorf("after COMMIT: cursors %v, unsettled %v; want only c, settled", s.cursors, s.unsettled)
	}
}

// wantReads checks the reads g was told of: each one's statement and rows.
func (g *cursorGate) wantReads(t *testing.T, want ...any) {
	t.Helper()
	if len(g.reads) != len(want)/2 {
		t.Fatalf("%d reads observed, want %d: %+v", len(g.reads), len(want)/2, g.reads)
	}
	for i := range g.reads {
		if sql, sent := want[2*i].(string), int64(want[2*i+1].(int)); g.reads[i].st.SQL != sql || g.sent[i] != sent {
			t.Errorf("read %d: %q with %d rows, want %q with %d", i+1, g.reads[i].st.SQL, g.sent[i], sql, sent)
		}
	}
}

// The cursor statements of a Query of several statements apply in order,
// under its one unit. A DECLARE is remembered with the whole message as its
// statement when the message is observed. The message is reported once at
// most: as its own read when it is observed, which a message holding a
// DECLARE is only when it sent rows, or else as a read of the first
// remembered cursor it fetches from.
func TestCursorsOfAQueryOfSeveralStatements(t *testing.T) {
	g := &cursorGate{fakeGate: newFakeGate()}
	s := readySession(g)
	flush := func() error { return nil }
	query := func(sql string) { s.fromClient('Q', body(&pgproto3.Query{String: sql}), flush) }
	rows := &pgproto3.RowDescription{}

	declare := "begin; declare c cursor for select * from t"
	query(declare)
	serve(s, tag("BEGIN"), tag("DECLARE CURSOR"), readyWith('T'))
	query("fetch 2 from c")
	serve(s, rows, row, row, tag("FETCH 2"), readyWith('T'))
	// Closed and declared again over an unobserved query, in one message.
	query("close c; declare c cursor for select 1")
	serve(s, tag("CLOSE CURSOR"), tag("DECLARE CURSOR"), readyWith('T'))
	query("fetch 1 from c")
	serve(s, rows, row, tag("FETCH 1"), readyWith('T'))
	// Declared and fetched in one message: one read, of the message.
	both := "declare d cursor for select * from t; fetch 3 from d"
	query(both)
	serve(s, tag("DECLARE CURSOR"), rows, row, row, row, tag("FETCH 3"), readyWith('T'))
	// A FETCH among other statements: a read of the DECLARE, with every row
	// of the message.
	query("fetch 1 from d; select 1")
	serve(s, rows, row, tag("FETCH 1"), rows, row, tag("SELECT 1"), readyWith('T'))
	query("commit")
	serve(s, tag("COMMIT"), readyWith('I'))
	// A DECLARE's message cut off before it answered sent no rows.
	query("begin; declare e cursor for select * from t")
	s.abandon()

	g.wantReads(t, declare, 2, both, 3, both, 2)
}

// When a Query of several statements fails, Stet cannot tell which of them
// ran or whether one committed. Its CLOSEs and unobserved DECLAREs are
// undone, so the older meaning comes back, and its observed DECLAREs are
// kept. A Query of one statement that failed is undone as before.
func TestFailedQueryOfSeveralStatementsRemembers(t *testing.T) {
	g := &cursorGate{fakeGate: newFakeGate()}
	s := readySession(g)
	flush := func() error { return nil }
	query := func(sql string) { s.fromClient('Q', body(&pgproto3.Query{String: sql}), flush) }
	rows := &pgproto3.RowDescription{}
	divide := &pgproto3.ErrorResponse{Code: "22012", Message: "division by zero"}

	held := "declare c cursor with hold for select * from t"
	query(held)
	serve(s, tag("DECLARE CURSOR"), readyWith('I'))
	query("close c; select 1/0")
	serve(s, tag("CLOSE CURSOR"), divide, readyWith('I'))
	query("begin; declare c cursor for select 1; select 1/0")
	serve(s, tag("BEGIN"), tag("DECLARE CURSOR"), divide, readyWith('E'))
	query("rollback")
	serve(s, tag("ROLLBACK"), readyWith('I'))
	query("fetch 1 from c")
	serve(s, rows, row, tag("FETCH 1"), readyWith('I'))

	committed := "begin; declare d cursor with hold for select * from t; commit; select 1/0"
	query(committed)
	serve(s, tag("BEGIN"), tag("DECLARE CURSOR"), tag("COMMIT"), divide, readyWith('I'))
	query("fetch 1 from d")
	serve(s, rows, row, tag("FETCH 1"), readyWith('I'))

	query("declare e cursor with hold for select * from t")
	serve(s, &pgproto3.ErrorResponse{Code: "42P01", Message: "relation does not exist"}, readyWith('I'))
	query("fetch 1 from e")
	serve(s, &pgproto3.ErrorResponse{Code: "34000", Message: `cursor "e" does not exist`}, readyWith('I'))

	g.wantReads(t, held, 1, committed, 1)
}

// A message of several statements that declares a cursor and fetches from it
// is reported as its own read, and its rows are the first the cursor's later
// FETCHes count before theirs. A read cut off by the connection's end counts
// toward its cursor too.
func TestDeclaringMessageCountsTowardItsCursor(t *testing.T) {
	g := &cursorGate{fakeGate: newFakeGate()}
	s := readySession(g)
	flush := func() error { return nil }
	query := func(sql string) { s.fromClient('Q', body(&pgproto3.Query{String: sql}), flush) }
	rows := &pgproto3.RowDescription{}

	both := "declare c cursor with hold for select * from t; fetch 2 from c"
	query(both)
	serve(s, tag("DECLARE CURSOR"), rows, row, row, tag("FETCH 2"), readyWith('I'))
	query("fetch 3 from c")
	serve(s, rows, row, row, row, tag("FETCH 3"), readyWith('I'))
	query("fetch 3 from c")
	query("fetch 3 from c")
	serve(s, rows, row)
	s.abandon()

	g.wantReads(t, both, 2, both, 3, both, 1, both, 0)
	if want := []int64{0, 2, 5, 6}; !slices.Equal(g.before, want) {
		t.Errorf("rows before = %v, want %v", g.before, want)
	}
}

// A Query of several statements that declares a remembered name again and
// fails leaves the name as it was: the server refused that DECLARE (42P03)
// and still holds the cursor, also after the transaction around it rolled
// back.
func TestFailedRedeclareAmongSeveralKeepsTheOldCursor(t *testing.T) {
	for name, inTx := range map[string]bool{"outside a transaction": false, "in a transaction": true} {
		t.Run(name, func(t *testing.T) {
			g := &cursorGate{fakeGate: newFakeGate()}
			s := readySession(g)
			flush := func() error { return nil }
			query := func(sql string) { s.fromClient('Q', body(&pgproto3.Query{String: sql}), flush) }
			exists := &pgproto3.ErrorResponse{Code: "42P03", Message: `cursor "c" already exists`}

			held := "declare c cursor with hold for select * from t"
			query(held)
			serve(s, tag("DECLARE CURSOR"), readyWith('I'))
			status := byte('I')
			if inTx {
				query("begin")
				serve(s, tag("BEGIN"), readyWith('T'))
				status = 'E'
			}
			query("declare c cursor for select * from t; select 1")
			serve(s, exists, readyWith(status))
			if inTx {
				query("rollback")
				serve(s, tag("ROLLBACK"), readyWith('I'))
			}
			query("fetch 1 from c")
			serve(s, &pgproto3.RowDescription{}, row, tag("FETCH 1"), readyWith('I'))

			g.wantReads(t, held, 1)
		})
	}
}

package proxy

import (
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

	// CommandComplete arrived but ReadyForQuery did not: nothing says the
	// unit committed.
	g = newFakeGate()
	s = readySession(g)
	s.fromClient('Q', body(&pgproto3.Query{String: "insert into t values (1)"}), flush)
	s.fromServer('C', body(&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")}))
	s.abandon()
	if o := g.ends[0]; !o.Lost || o.Executed {
		t.Errorf("completed but unconfirmed write: %+v, want lost", o)
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
// DECLAREs over t, and records each read with its statement.
type cursorGate struct {
	*fakeGate
	reads []observedRead
	sent  []int64
}

func (g *cursorGate) Inspect(sql string) Statement {
	st := Statement{SQL: sql}
	f := strings.Fields(sql)
	switch f[0] {
	case "declare":
		st.Cursor = &Cursor{Op: CursorDeclare, Name: f[1], Hold: strings.Contains(sql, " with hold ")}
		st.Observe = strings.HasSuffix(sql, " from t")
	case "fetch":
		st.Cursor = &Cursor{Op: CursorFetch, Name: f[len(f)-1]}
	}
	return st
}

func (g *cursorGate) Observe(info SessionInfo, st Statement, o Outcome) {
	g.reads = append(g.reads, observedRead{info: info, st: st})
	g.sent = append(g.sent, o.Sent)
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

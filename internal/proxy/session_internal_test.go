package proxy

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// fakeGate watches every statement and records each End. While refusal is
// set, Begin refuses every write with it. Blocked records each reason, and
// whether the proxy called it the first time for that reason, and answers
// with an ST002 that names it. WouldBlock records each reason. calls lists
// the End, Unseen and TxEnded calls in the order they were made, a TxEnded
// with the count it settles ("TxEnded(1)"), and txEnded the session each
// TxEnded was told of. Tracked tracks no table.
type fakeGate struct {
	n       int
	ends    map[int]Outcome
	refusal *Refusal
	blocked []string
	firsts  []bool
	would   []string
	calls   []string
	txEnded []SessionInfo
}

func newFakeGate() *fakeGate { return &fakeGate{ends: map[int]Outcome{}} }

func (g *fakeGate) Inspect(sql string) Statement { return Statement{SQL: sql, Watch: true} }
func (g *fakeGate) Begin(SessionInfo, Statement, []Param) (any, *Refusal) {
	g.n++
	return g.n - 1, g.refusal
}
func (g *fakeGate) End(h any, o Outcome) {
	g.ends[h.(int)] = o
	g.calls = append(g.calls, "End")
}
func (g *fakeGate) Observe(SessionInfo, Statement, Outcome) {}
func (g *fakeGate) Blocked(_ SessionInfo, reason string, first bool) Refusal {
	g.blocked = append(g.blocked, reason)
	g.firsts = append(g.firsts, first)
	return Refusal{Code: "ST002", Message: "blocked " + reason}
}
func (g *fakeGate) WouldBlock(_ SessionInfo, reason string) { g.would = append(g.would, reason) }
func (g *fakeGate) Unseen(SessionInfo)                      { g.calls = append(g.calls, "Unseen") }
func (g *fakeGate) TxEnded(info SessionInfo, unseen int) {
	g.calls = append(g.calls, fmt.Sprintf("TxEnded(%d)", unseen))
	g.txEnded = append(g.txEnded, info)
}
func (g *fakeGate) Tracked(uint32) (uint16, KeyType, bool) { return 0, 0, false }

// count is how many times the gate was called as call ("End" or "Unseen").
func (g *fakeGate) count(call string) int {
	n := 0
	for _, c := range g.calls {
		if c == call {
			n++
		}
	}
	return n
}

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

// A side whose tap panicked while it held the session's lock never lets go of
// it (see relay). abandon, which runs once both sides have stopped, still
// reports what was pending, without waiting for the lock.
func TestAbandonReportsWhatAPanicLeftHolding(t *testing.T) {
	g := newFakeGate()
	s := readySession(g)
	sendWrite(s, "")
	s.fromClient('S', nil, func() error { return nil })
	s.mu.Lock() // as a tap that panicked holding it leaves it
	done := make(chan struct{})
	go func() {
		s.abandon()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("abandon is still waiting for the lock")
	}
	if o, ok := g.ends[0]; !ok || !o.Lost {
		t.Errorf("the pending write ended %+v (reported %v), want it lost", o, ok)
	}
}

// parkingGate is fakeGate whose Begin leaves the session's lock held, as a
// side that panicked under it would: the side that called Begin then parks
// on that lock before it records the write.
type parkingGate struct {
	*fakeGate
	s *session
}

func (g *parkingGate) Begin(info SessionInfo, st Statement, ps []Param) (any, *Refusal) {
	h, r := g.fakeGate.Begin(info, st, ps)
	g.s.mu.Lock()
	return h, r
}

// A write Begin answered, whose side then parked on the lock a panicking
// side left held, was never forwarded and is in no unit: abandon reports it
// all the same, as not sent (neither run nor lost), and relay sees that side
// stuck once Begin has returned.
func TestAbandonReportsAWriteItsSideNeverRecorded(t *testing.T) {
	for _, refused := range []bool{false, true} {
		g := &parkingGate{fakeGate: newFakeGate()}
		if refused {
			g.refusal = &Refusal{Code: "ST001", Message: "held"}
		}
		s := readySession(g)
		g.s = s
		go s.fromClient('Q', body(queryMsg(ins)), func() error { return nil }) // parks for good
		deadline := time.Now().Add(5 * time.Second)
		for s.unrecorded.Load() == nil || s.gating.Load() != 0 {
			if time.Now().After(deadline) {
				t.Fatal("Begin was not answered")
			}
			time.Sleep(time.Millisecond)
		}
		if !s.stuck() {
			t.Error("stuck = false with the lock held and no Begin running")
		}
		s.abandon()
		o, ok := g.ends[0]
		switch {
		case !ok:
			t.Errorf("refused %v: the write Begin answered was never reported", refused)
		case o.Executed || o.Lost || o.Refused != refused || refused && !o.Undelivered:
			t.Errorf("refused %v: reported %+v, want not run, not lost, refused %v and undelivered", refused, o, refused)
		}
	}
}

// panicEndGate is fakeGate whose End panics for the first write it is told of.
type panicEndGate struct{ *fakeGate }

func (g panicEndGate) End(h any, o Outcome) {
	if h.(int) == 0 {
		panic("test gate: End")
	}
	g.fakeGate.End(h, o)
}

// A gate's End that panics for one write does not lose the others' (the
// residual fix's item 2): the session logs the panic as an error and reports
// the rest, when the server answers them and when the connection ends first.
func TestAPanickingEndDoesNotLoseTheOthers(t *testing.T) {
	for _, answered := range []bool{true, false} {
		g := panicEndGate{newFakeGate()}
		s := readySession(g)
		var logs strings.Builder
		s.log = slog.New(slog.NewTextHandler(&logs, nil))
		for range 3 {
			sendWrite(s, "")
		}
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Errorf("answered %v: the panic escaped: %v", answered, v)
				}
			}()
			if answered {
				s.fromClient('S', nil, func() error { return nil })
				serve(s, parseOK, bindOK, tag("INSERT 0 1"), parseOK, bindOK, tag("INSERT 0 1"), parseOK, bindOK, tag("INSERT 0 1"), ready)
				return
			}
			s.abandon()
		}()
		if _, ok1 := g.ends[1]; !ok1 {
			t.Errorf("answered %v: write 2 not reported", answered)
		}
		if _, ok2 := g.ends[2]; !ok2 {
			t.Errorf("answered %v: write 3 not reported", answered)
		}
		if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "test gate: End") {
			t.Errorf("answered %v: log %q, want the panic as an error", answered, logs.String())
		}
	}
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

func (g *txGate) Begin(info SessionInfo, st Statement, ps []Param) (any, *Refusal) {
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

// A Query or FunctionCall sent before the Sync of the extended-protocol
// messages ahead of it closes those messages into a unit the server answers
// with no ReadyForQuery of its own when they all succeed. The unit ends once
// they all completed: its table changes hold, and its transaction marks pass
// to the session as at a ReadyForQuery that leaves a transaction open
// (ruling P6), since the Query runs in the same implicit transaction.
func TestUnsyncedUnitIsAnsweredWithoutReadyForQuery(t *testing.T) {
	flush := &pgproto3.Flush{}

	t.Run("Parse and Describe, then begin", func(t *testing.T) {
		g := &txGate{fakeGate: newFakeGate()}
		s := readySession(g)
		send(s, parseMsg("s1", ins), describeMsg('S', "s1"), flush, queryMsg("begin"))
		serve(s, parseOK, &pgproto3.ParameterDescription{}, &pgproto3.NoData{}, tag("BEGIN"), readyWith('T'))
		if p, ok, others := s.stmts.get(nil, "s1"); !ok || p.st.SQL != ins || len(others) != 0 || s.stmts.waiting() != 0 {
			t.Errorf("s1: %q (found %v), others %v, %d changes waiting; want the confirmed insert alone", p.st.SQL, ok, others, s.stmts.waiting())
		}
		if len(s.units) != 0 {
			t.Errorf("%d units still waiting, want none", len(s.units))
		}
		send(s, bindMsg("", "s1"), execMsg(""), syncMsg)
		if len(g.infos) != 1 || g.infos[0].TxDirty {
			t.Errorf("writes begun with %+v, want one with TxDirty false", g.infos)
		}
	})

	t.Run("an Execute ran, then a write", func(t *testing.T) {
		g := &txGate{fakeGate: newFakeGate()}
		s := readySession(g)
		send(s, parseMsg("", "select 1"), bindMsg("", ""), execMsg(""), flush)
		serve(s, parseOK, bindOK, row, tag("SELECT 1"))
		send(s, queryMsg(ins))
		if len(g.infos) != 1 || !g.infos[0].TxDirty {
			t.Errorf("writes begun with %+v, want one with TxDirty true", g.infos)
		}
		serve(s, tag("INSERT 0 1"), ready)
		if o := g.ends[0]; !o.Executed || o.Rows == nil || *o.Rows != 1 || !o.InTransaction {
			t.Errorf("write: %+v, want executed, 1 row, in the implicit transaction of the select", o)
		}
	})

	// The select's mark must outlive its unit: the BEGIN turns the implicit
	// transaction it ran in into an explicit one.
	t.Run("an Execute ran, then begin, then a write", func(t *testing.T) {
		g := &txGate{fakeGate: newFakeGate()}
		s := readySession(g)
		send(s, parseMsg("", "select 1"), bindMsg("", ""), execMsg(""), flush)
		serve(s, parseOK, bindOK, row, tag("SELECT 1"))
		send(s, queryMsg("begin"))
		serve(s, tag("BEGIN"), readyWith('T'))
		send(s, queryMsg(ins))
		if len(g.infos) != 1 || !g.infos[0].TxDirty {
			t.Errorf("writes begun with %+v, want one with TxDirty true", g.infos)
		}
	})

	// A Flush opens a unit with no message; the Query after it closes it.
	t.Run("a Flush between two Queries", func(t *testing.T) {
		g := newStmtGate()
		s := readySession(g)
		send(s, queryMsg("select 1"), flush, queryMsg(ins))
		serve(s, &pgproto3.RowDescription{}, row, tag("SELECT 1"), ready, tag("INSERT 0 1"), ready)
		if len(s.units) != 0 {
			t.Errorf("%d units still waiting, want none", len(s.units))
		}
		if o := g.ends[0]; !o.Executed || o.Rows == nil || *o.Rows != 1 || o.InTransaction {
			t.Errorf("write: %+v, want executed with 1 row, alone", o)
		}
	})

	t.Run("a FunctionCall", func(t *testing.T) {
		g := newStmtGate()
		s := readySession(g)
		send(s, parseMsg("s1", ins), flush, &pgproto3.FunctionCall{Function: 1})
		serve(s, parseOK, &pgproto3.FunctionCallResponse{}, ready)
		if len(s.units) != 0 {
			t.Errorf("%d units still waiting, want none", len(s.units))
		}
		send(s, bindMsg("", "s1"), execMsg(""), syncMsg)
		if !slices.Equal(g.begun, []string{ins}) {
			t.Errorf("writes begun %q, want the insert s1 holds", g.begun)
		}
	})
}

// When a message of a unit closed without a Sync fails, the server skips
// every message after it up to the next Sync, Queries and FunctionCalls
// included, and answers them all with that Sync's one ReadyForQuery. The
// statements it skipped were not executed, and their table changes are
// dropped.
func TestFailedUnsyncedUnitSkipsUpToTheSync(t *testing.T) {
	g := newStmtGate()
	s := readySession(g)
	send(s, parseMsg("s1", "selec 1"), &pgproto3.Flush{})
	serve(s, failed("42601"))
	send(s, queryMsg(ins), parseMsg("s2", ins), bindMsg("", "s2"), execMsg(""), queryMsg(ins), syncMsg)
	serve(s, ready)
	if len(s.units) != 0 {
		t.Fatalf("%d units still waiting, want none", len(s.units))
	}
	if len(g.ends) != 3 {
		t.Fatalf("%d writes ended, want 3", len(g.ends))
	}
	for i, o := range g.ends {
		if o.Executed || o.Lost || !strings.Contains(o.Error, "not executed: an earlier statement in the same Sync failed: SQLSTATE 42601") {
			t.Errorf("write %d: %+v, want skipped after the 42601", i+1, o)
		}
	}
	if _, ok, _ := s.stmts.get(nil, "s2"); ok {
		t.Errorf("s2 is held, want the skipped Parse dropped")
	}

	// Answers after the Sync are matched to their own statements.
	send(s, queryMsg(ins))
	serve(s, tag("INSERT 0 1"), ready)
	if o := g.ends[3]; !o.Executed || o.Error != "" {
		t.Errorf("write after the Sync: %+v, want executed", o)
	}

	// The connection ends before the Sync is answered: what comes after the
	// failed unit was still skipped, not lost.
	g = newStmtGate()
	s = readySession(g)
	send(s, parseMsg("", "selec 1"), &pgproto3.Flush{})
	serve(s, failed("42601"))
	send(s, queryMsg(ins), syncMsg)
	s.abandon()
	if o := g.ends[0]; o.Executed || o.Lost || !strings.Contains(o.Error, "not executed") {
		t.Errorf("write behind the failed unit: %+v, want skipped and not lost", o)
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
	b, ok, _ := s.portals.get(nil, "p")
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

// doGate is txGate that also marks the SQL "do" as a DO block.
type doGate struct{ *txGate }

func (g doGate) Inspect(sql string) Statement {
	st := g.txGate.Inspect(sql)
	if sql == "do" {
		st.Block = BlockDo
	}
	return st
}

// wantCalls fails the test unless g's End, Unseen and TxEnded calls so far
// are want, in order.
func (g *fakeGate) wantCalls(t *testing.T, when string, want ...string) {
	t.Helper()
	if !slices.Equal(g.calls, want) {
		t.Errorf("%s: gate called %q, want %q", when, g.calls, want)
	}
}

// When the connection ends while a transaction that held a watched write, or
// a message Unseen was told of, may still be open, the gate is told once,
// after every End, that it ended, with no transaction open, settling every
// Unseen call not settled before. A write lost before its answer counts: it
// may have run. A write lost outside a transaction owes nothing beyond its
// End, nor does a transaction the gate was already told ended.
func TestTxEndedWhenConnectionDrops(t *testing.T) {
	for _, c := range []struct {
		name, sent string
		want       []string
	}{
		{"a write answered inside a transaction", "begin Z(T) insert Z(T)", []string{"End", "TxEnded(0)"}},
		{"a write lost inside a transaction", "begin Z(T) insert", []string{"End", "TxEnded(0)"}},
		{"writes lost under one Sync", "E:insert E:insert S", []string{"End", "End", "TxEnded(0)"}},
		{"a DO block answered inside a transaction", "begin Z(T) do Z(T)", []string{"Unseen", "TxEnded(1)"}},
		{"a DO block lost", "do", []string{"Unseen", "TxEnded(1)"}},
		{"a write lost outside a transaction", "insert", []string{"End"}},
		{"a write answered outside a transaction", "insert Z(I)", []string{"End"}},
		{"a transaction already ended", "begin Z(T) insert Z(T) commit Z(I)", []string{"End", "TxEnded(0)"}},
		{"a DO block answered inside a transaction and one sent behind it", "begin Z(T) do Z(T) do", []string{"Unseen", "Unseen", "TxEnded(2)"}},
		{"a DO block settled, then one lost", "do Z(I) do", []string{"Unseen", "TxEnded(1)", "Unseen", "TxEnded(1)"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := doGate{&txGate{fakeGate: newFakeGate()}}
			s := readySession(g)
			drive(s, c.sent)
			s.abandon()
			g.wantCalls(t, "after the connection ended", c.want...)
		})
	}

	g := doGate{&txGate{fakeGate: newFakeGate()}}
	s := readySession(g)
	drive(s, "begin Z(T) insert")
	s.abandon()
	if o := g.ends[0]; !o.Lost || !o.InTransaction {
		t.Errorf("write lost inside a transaction: %+v, want lost inside a transaction", o)
	}
	// The connection's end ended the transaction too.
	if len(g.txEnded) != 1 || g.txEnded[0].TxOpen || g.txEnded[0].TxDirty {
		t.Errorf("TxEnded told of %+v, want one session with no transaction open and nothing sent in one", g.txEnded)
	}
}

// The gate is told a transaction ended in the order the server answers, not
// the order things were sent: an idle ReadyForQuery answering an earlier unit
// says nothing of writes sent behind it, which owe their TxEnded once their
// own unit is answered and their transaction has ended.
func TestTxEndedWaitsForItsOwnAnswer(t *testing.T) {
	g := &txGate{fakeGate: newFakeGate()}
	s := readySession(g)
	send(s, queryMsg("select 1"), parseMsg("", ins), bindMsg("", ""), execMsg(""), parseMsg("", ins), bindMsg("", ""), execMsg(""), syncMsg,
		queryMsg("begin"), queryMsg(ins), queryMsg("commit"))

	serve(s, &pgproto3.RowDescription{}, row, tag("SELECT 1"), ready)
	g.wantCalls(t, "after the select")
	serve(s, parseOK, bindOK, tag("INSERT 0 1"), parseOK, bindOK, tag("INSERT 0 1"), ready)
	g.wantCalls(t, "after the writes under one Sync", "End", "End", "TxEnded(0)")
	serve(s, tag("BEGIN"), readyWith('T'), tag("INSERT 0 1"), readyWith('T'))
	g.wantCalls(t, "after the write inside a transaction", "End", "End", "TxEnded(0)", "End")
	serve(s, tag("COMMIT"), ready)
	g.wantCalls(t, "after commit", "End", "End", "TxEnded(0)", "End", "TxEnded(0)")
}

// A watched write in a unit the server answers with no ReadyForQuery of its
// own, a Query having followed it before any Sync, runs in the implicit
// transaction of that Query: its End comes once its messages completed, and
// the gate is told the transaction ended at the Query's ReadyForQuery.
func TestTxEndedAfterAnUnsyncedWrite(t *testing.T) {
	g := &txGate{fakeGate: newFakeGate()}
	s := readySession(g)
	send(s, parseMsg("", ins), bindMsg("", ""), execMsg(""), queryMsg("select 1"))
	serve(s, parseOK, bindOK, tag("INSERT 0 1"), &pgproto3.RowDescription{}, row, tag("SELECT 1"))
	g.wantCalls(t, "before the Query's ReadyForQuery", "End")
	if o := g.ends[0]; !o.Executed || !o.InTransaction {
		t.Errorf("write: %+v, want executed inside a transaction", o)
	}
	serve(s, ready)
	g.wantCalls(t, "after the Query's ReadyForQuery", "End", "TxEnded(0)")
}

// hiddenGate is byNameGate that also marks the SQL "do" as a DO block.
type hiddenGate struct{ byNameGate }

func (g hiddenGate) Inspect(sql string) Statement {
	st := g.byNameGate.Inspect(sql)
	if sql == "do" {
		st.Block = BlockDo
	}
	return st
}

// On a session that does not enforce, an Execute is unseen when its portal
// may run a statement the proxy could not read, one the gate marked with
// Block, or SQL EXECUTE of a prepared statement: the newest meaning of its
// names, or another one the server may hold. So is a Query of SQL EXECUTE. A
// FETCH of a portal is not, nor is a read. An enforced session tells the gate
// of none of them.
func TestUnseenExecutes(t *testing.T) {
	flush := func() error { return nil }
	for _, c := range []struct {
		name string
		run  func(s *session)
		want int // Unseen calls on a session that does not enforce
	}{
		{"an Execute of a statement not read", func(s *session) {
			s.fromClient('P', nil, flush) // past maxInspect
			send(s, bindMsg("", ""), execMsg(""), syncMsg)
		}, 2},
		{"an Execute of a DO block", func(s *session) {
			send(s, parseMsg("d", "do"), bindMsg("", "d"), execMsg(""), syncMsg)
		}, 2},
		{"an Execute of SQL EXECUTE", func(s *session) {
			send(s, parseMsg("e", "execute w"), bindMsg("", "e"), execMsg(""), syncMsg)
		}, 1},
		{"an Execute of a name that may still mean a DO block", func(s *session) {
			send(s, parseMsg("s1", "do"), syncMsg)
			serve(s, parseOK, ready)
			send(s, parseMsg("s1", "select 1"), syncMsg, bindMsg("", "s1"), execMsg(""), syncMsg)
		}, 2},
		{"an Execute of a portal that may still mean a DO block", func(s *session) {
			send(s, parseMsg("d", "do"), parseMsg("r", "select 1"), bindMsg("p", "d"), syncMsg)
			serve(s, parseOK, parseOK, bindOK, readyWith('T'))
			send(s, bindMsg("p", "r"), syncMsg, execMsg("p"), syncMsg)
		}, 2},
		{"an Execute of a name that may still mean a watched write", func(s *session) {
			send(s, parseMsg("s1", ins), syncMsg)
			serve(s, parseOK, ready)
			send(s, parseMsg("s1", "select 1"), syncMsg, bindMsg("", "s1"), execMsg(""), syncMsg)
		}, 1},
		{"an Execute of a portal that may still mean a watched write", func(s *session) {
			send(s, parseMsg("w", ins), parseMsg("r", "select 1"), bindMsg("p", "w"), syncMsg)
			serve(s, parseOK, parseOK, bindOK, readyWith('T'))
			send(s, bindMsg("p", "r"), syncMsg, execMsg("p"), syncMsg)
		}, 1},
		{"an Execute of a watched write", func(s *session) {
			send(s, parseMsg("w", ins), bindMsg("", "w"), execMsg(""), syncMsg)
		}, 0},
		{"a Query of SQL EXECUTE", func(s *session) { send(s, queryMsg("execute w")) }, 1},
		{"a Query of FETCH", func(s *session) { send(s, queryMsg("fetch c")) }, 0},
		{"an Execute of FETCH", func(s *session) {
			send(s, parseMsg("f", "fetch c"), bindMsg("", "f"), execMsg(""), syncMsg)
		}, 0},
		{"an Execute of a read", func(s *session) {
			send(s, parseMsg("r", "select 1"), bindMsg("", "r"), execMsg(""), syncMsg)
		}, 0},
	} {
		for _, enforce := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, enforce %v", c.name, enforce), func(t *testing.T) {
				g := hiddenGate{byNameGate{newStmtGate()}}
				s := sessionOf(g, enforce)
				c.run(s)
				want := c.want
				if enforce {
					want = 0
				}
				if got := g.count("Unseen"); got != want {
					t.Errorf("Unseen %d times, want %d", got, want)
				}
			})
		}
	}
}

// panicTxEndedGate is txGate whose TxEnded panics.
type panicTxEndedGate struct{ *txGate }

func (panicTxEndedGate) TxEnded(SessionInfo, int) { panic("test gate: TxEnded") }

// A gate's TxEnded that panics takes nothing else with it: the session logs
// the panic as an error and goes on reporting the statements after it, and
// the connection's end is reported as well.
func TestAPanickingTxEndedDoesNotLoseTheOthers(t *testing.T) {
	g := panicTxEndedGate{&txGate{fakeGate: newFakeGate()}}
	s := readySession(g)
	var logs strings.Builder
	s.log = slog.New(slog.NewTextHandler(&logs, nil))
	defer func() {
		if v := recover(); v != nil {
			t.Errorf("the panic escaped: %v", v)
		}
	}()
	send(s, queryMsg("begin"), queryMsg(ins), queryMsg("commit"), queryMsg(ins))
	serve(s, tag("BEGIN"), readyWith('T'), tag("INSERT 0 1"), readyWith('T'), tag("COMMIT"), ready, tag("INSERT 0 1"), ready)
	if o, ok := g.ends[1]; !ok || !o.Executed || o.Lost {
		t.Errorf("write after the panic: %+v (reported %v), want executed", o, ok)
	}
	send(s, queryMsg("begin"), queryMsg(ins))
	serve(s, tag("BEGIN"), readyWith('T'), tag("INSERT 0 1"), readyWith('T'))
	s.abandon()
	if n := strings.Count(logs.String(), "test gate: TxEnded"); n != 2 || !strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("log %q, want both panics as errors", logs.String())
	}
}

// settledUnseen is the sum of the unseen counts g's TxEnded calls settled.
func (g *fakeGate) settledUnseen() int {
	n := 0
	for _, c := range g.calls {
		var k int
		if _, err := fmt.Sscanf(c, "TxEnded(%d)", &k); err == nil {
			n += k
		}
	}
	return n
}

// Each TxEnded says how many Unseen calls it settles: those of the units the
// server answered since the previous one. An Unseen call for a unit sent
// behind them, made before their answer came, is left to a later TxEnded;
// a unit of several unseen messages is settled in one; a TxEnded owed by
// watched writes alone settles none. Over the session the counts add up to
// the Unseen calls.
func TestTxEndedCountsTheUnseenItSettles(t *testing.T) {
	g := doGate{&txGate{fakeGate: newFakeGate()}}
	s := readySession(g)
	from := 0 // where the calls of the current part start
	want := func(when string, calls ...string) {
		t.Helper()
		if got := g.calls[from:]; !slices.Equal(got, calls) {
			t.Errorf("%s: gate called %q, want %q", when, got, calls)
		}
	}

	send(s, queryMsg("do"), queryMsg("begin"), queryMsg("do"), queryMsg("commit"))
	want("after both DO blocks were sent", "Unseen", "Unseen")
	serve(s, tag("DO"), ready)
	want("after the first DO block's answer", "Unseen", "Unseen", "TxEnded(1)")
	serve(s, tag("BEGIN"), readyWith('T'), tag("DO"), readyWith('T'))
	want("inside the transaction", "Unseen", "Unseen", "TxEnded(1)")
	serve(s, tag("COMMIT"), ready)
	want("after commit", "Unseen", "Unseen", "TxEnded(1)", "TxEnded(1)")

	from = len(g.calls)
	send(s, parseMsg("d", "do"), bindMsg("", "d"), execMsg(""), syncMsg)
	serve(s, parseOK, bindOK, tag("DO"), ready)
	want("after a Parse and an Execute of a DO block under one Sync", "Unseen", "Unseen", "TxEnded(2)")

	from = len(g.calls)
	send(s, queryMsg("begin"), queryMsg(ins), queryMsg("commit"), queryMsg("do"))
	serve(s, tag("BEGIN"), readyWith('T'), tag("INSERT 0 1"), readyWith('T'), tag("COMMIT"), ready)
	want("after a transaction with a write", "Unseen", "End", "TxEnded(0)")
	serve(s, tag("DO"), ready)
	want("after the DO block sent behind it", "Unseen", "End", "TxEnded(0)", "TxEnded(1)")

	from = len(g.calls)
	drive(s, "begin Z(T) do Z(T) do do")
	s.abandon()
	want("after the connection ended", "Unseen", "Unseen", "Unseen", "TxEnded(3)")
	if got, want := g.settledUnseen(), g.count("Unseen"); got != want {
		t.Errorf("TxEnded settled %d Unseen calls, want all %d", got, want)
	}
}

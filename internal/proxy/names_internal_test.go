package proxy

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// stmtGate watches the statements that mention insert and records the SQL
// of each write begun.
type stmtGate struct {
	*fakeGate
	begun []string
}

func newStmtGate() *stmtGate { return &stmtGate{fakeGate: newFakeGate()} }

func (g *stmtGate) Inspect(sql string) Statement {
	return Statement{SQL: sql, Watch: strings.Contains(sql, "insert")}
}

func (g *stmtGate) Begin(info SessionInfo, st Statement, ps []Param) any {
	g.begun = append(g.begun, st.SQL)
	return g.fakeGate.Begin(info, st, ps)
}

// send hands the session client messages the way pump does: the body of the
// types the client tap wants.
func send(s *session, msgs ...pgproto3.FrontendMessage) {
	tp := s.clientTap()
	for _, m := range msgs {
		b, _ := m.Encode(nil)
		if tp.want(b[0]) {
			tp.see(b[0], b[5:], func() error { return nil })
		}
	}
}

const ins = "insert into t values (1)"

func parseMsg(name, sql string) *pgproto3.Parse { return &pgproto3.Parse{Name: name, Query: sql} }
func bindMsg(portal, stmt string) *pgproto3.Bind {
	return &pgproto3.Bind{DestinationPortal: portal, PreparedStatement: stmt}
}
func execMsg(portal string) *pgproto3.Execute { return &pgproto3.Execute{Portal: portal} }
func closeMsg(kind byte, name string) *pgproto3.Close {
	return &pgproto3.Close{ObjectType: kind, Name: name}
}
func describeMsg(kind byte, name string) *pgproto3.Describe {
	return &pgproto3.Describe{ObjectType: kind, Name: name}
}
func queryMsg(sql string) *pgproto3.Query { return &pgproto3.Query{String: sql} }
func failed(code string) *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{Severity: "ERROR", Code: code, Message: "refused"}
}

var (
	syncMsg = &pgproto3.Sync{}
	parseOK = &pgproto3.ParseComplete{}
	bindOK  = &pgproto3.BindComplete{}
	closeOK = &pgproto3.CloseComplete{}
)

// The server refuses a Parse of a name it holds (42P05) and keeps the
// statement it has, so an Execute of the name runs that one.
func TestReparseTheServerRefusedKeepsTheStatement(t *testing.T) {
	g := newStmtGate()
	s := readySession(g)
	send(s, parseMsg("s1", ins), syncMsg)
	serve(s, parseOK, readyWith('I'))
	send(s, parseMsg("s1", "select 1"), syncMsg)
	serve(s, failed("42P05"), readyWith('I'))
	send(s, bindMsg("", "s1"), execMsg(""), syncMsg)

	if !slices.Equal(g.begun, []string{ins}) {
		t.Errorf("writes begun %q, want the insert the server still holds", g.begun)
	}
}

// Once a message fails, the server skips the rest of its Sync: a Parse or
// Close it skipped changed nothing.
func TestSkippedParseAndCloseAreUndone(t *testing.T) {
	g := newStmtGate()
	s := readySession(g)
	send(s, parseMsg("", ins), parseMsg("s1", ins), syncMsg)
	serve(s, parseOK, parseOK, readyWith('I'))
	send(s, bindMsg("", "nosuch"), parseMsg("", "select 1"), closeMsg('S', "s1"), syncMsg)
	serve(s, failed("26000"), readyWith('I'))
	send(s, bindMsg("", ""), execMsg(""), syncMsg, bindMsg("", "s1"), execMsg(""), syncMsg)

	if !slices.Equal(g.begun, []string{ins, ins}) {
		t.Errorf("writes begun %q, want the insert of both names", g.begun)
	}
}

// A message behind a Parse under the same Sync sees the new statement at
// once: if the Parse fails, the server skips that message too. Once the
// failure is answered, the name means what it meant before.
func TestParseCountsAtOnceWithinItsSync(t *testing.T) {
	g := newStmtGate()
	s := readySession(g)
	send(s, parseMsg("", "select 1"), syncMsg)
	serve(s, parseOK, readyWith('I'))
	send(s, parseMsg("", ins), bindMsg("", ""), execMsg(""))
	if !slices.Equal(g.begun, []string{ins}) {
		t.Errorf("writes begun %q, want the insert parsed under the same Sync", g.begun)
	}
	send(s, syncMsg)
	serve(s, failed("42P01"), readyWith('I'))
	if p, ok, others := s.stmts.get(nil, ""); !ok || p.st.SQL != "select 1" || len(others) != 0 {
		t.Errorf("after the Parse failed: %q (found %v, others %v), want select 1 alone", p.st.SQL, ok, others)
	}
}

// A Close and a Parse the server completed replace what the name meant.
func TestConfirmedCloseAndParseReplace(t *testing.T) {
	g := newStmtGate()
	s := readySession(g)
	send(s, parseMsg("s1", ins), syncMsg)
	serve(s, parseOK, readyWith('I'))
	send(s, closeMsg('S', "s1"), syncMsg)
	serve(s, closeOK, readyWith('I'))
	send(s, parseMsg("s1", "select 1"), syncMsg)
	serve(s, parseOK, readyWith('I'))
	send(s, bindMsg("", "s1"), execMsg(""), syncMsg)

	if len(g.begun) != 0 {
		t.Errorf("writes begun %q, want none: s1 was closed and parsed again as a read", g.begun)
	}
}

// The server refuses a Bind of a portal name it holds (42P03) and keeps the
// portal it has.
func TestRebindTheServerRefusedKeepsThePortal(t *testing.T) {
	g := newStmtGate()
	s := readySession(g)
	send(s, queryMsg("begin"))
	serve(s, tag("BEGIN"), readyWith('T'))
	send(s, parseMsg("w", ins), parseMsg("r", "select 1"), bindMsg("p", "w"), syncMsg)
	serve(s, parseOK, parseOK, bindOK, readyWith('T'))
	send(s, bindMsg("p", "r"), syncMsg)
	serve(s, failed("42P03"), readyWith('E'))
	send(s, execMsg("p"), syncMsg)

	if !slices.Equal(g.begun, []string{ins}) {
		t.Errorf("writes begun %q, want the insert the portal still runs", g.begun)
	}
}

// Every message that gets one completion is counted, and so is every
// completion: a Describe is answered by one RowDescription or NoData (after a
// ParameterDescription, for a statement). The error belongs to the first
// message not completed.
func TestDescribeRepliesAreCounted(t *testing.T) {
	g := newStmtGate()
	s := readySession(g)
	send(s, queryMsg("begin"))
	serve(s, tag("BEGIN"), readyWith('T'))
	w := ins + " returning *"
	send(s, parseMsg("s", w), describeMsg('S', "s"), bindMsg("", "s"), describeMsg('P', ""), execMsg(""), syncMsg)
	u := s.units[0]
	serve(s, parseOK, &pgproto3.ParameterDescription{}, &pgproto3.RowDescription{}, bindOK,
		&pgproto3.RowDescription{}, failed("23505"), readyWith('E'))

	if u.msgs != 5 || u.acks != 4 || u.errPos != 5 {
		t.Errorf("msgs %d, acks %d, errPos %d; want 5, 4 and 5", u.msgs, u.acks, u.errPos)
	}
	if p, ok, _ := s.stmts.get(nil, "s"); !ok || p.st.SQL != w {
		t.Errorf("statement s: %q (found %v), want the completed Parse", p.st.SQL, ok)
	}
	if b, ok, _ := s.portals.get(nil, ""); !ok || b.st.SQL != w {
		t.Errorf("unnamed portal: %q (found %v), want the completed Bind", b.st.SQL, ok)
	}
}

// The server keeps the first 63 bytes of a statement or portal name, so two
// names that differ only after them are one name. A name that differs within
// them is another.
func TestNamesAreComparedByTheirFirst63Bytes(t *testing.T) {
	g := newStmtGate()
	s := readySession(g)
	long := strings.Repeat("n", 63)
	send(s, parseMsg(long+"AAAA", ins), syncMsg)
	serve(s, parseOK, readyWith('I'))

	send(s, bindMsg("", long+"BBBB"), execMsg(""), syncMsg)
	if len(g.begun) != 1 {
		t.Errorf("statement names: %d writes begun, want 1", len(g.begun))
	}
	send(s, bindMsg(long+"AAAA", long), execMsg(long+"BBBB"), syncMsg)
	if len(g.begun) != 2 {
		t.Errorf("portal names: %d writes begun, want 2", len(g.begun))
	}
	send(s, bindMsg("", long[:62]), execMsg(""), syncMsg)
	if len(g.begun) != 2 {
		t.Errorf("a 62-byte prefix: %d writes begun, want still 2", len(g.begun))
	}
}

// A change from an earlier unit not answered yet may hold or not, so the
// name means either. Within the unit that made the change only the new
// meaning counts. A portal bound to a name of several meanings keeps the
// ones that did not win. Once the change is answered, one meaning is left.
func TestUnansweredChangeLeavesSeveralMeanings(t *testing.T) {
	g := newStmtGate()
	s := readySession(g)
	send(s, parseMsg("s1", ins), syncMsg)
	serve(s, parseOK, readyWith('I'))
	send(s, parseMsg("s1", "select 1"), syncMsg) // not answered yet
	send(s, bindMsg("", "s1"))
	first, next := s.units[0], s.units[1]

	if p, ok, others := s.stmts.get(next, "s1"); !ok || p.st.SQL != "select 1" || len(others) != 1 || others[0].st.SQL != ins {
		t.Errorf("in the next unit: %q (found %v), others %v; want select 1, and the insert among others", p.st.SQL, ok, others)
	}
	if p, ok, others := s.stmts.get(first, "s1"); !ok || p.st.SQL != "select 1" || len(others) != 0 {
		t.Errorf("in the unit that parsed it: %q (found %v), others %v; want select 1 alone", p.st.SQL, ok, others)
	}
	if b, _, _ := s.portals.get(next, ""); len(b.others) != 1 || b.others[0].st.SQL != ins {
		t.Errorf("portal bound in the next unit: others %v, want the insert", b.others)
	}

	serve(s, failed("42P05"), readyWith('I'))
	if p, ok, others := s.stmts.get(next, "s1"); !ok || p.st.SQL != ins || len(others) != 0 {
		t.Errorf("once the Parse was refused: %q (found %v), others %v; want the insert alone", p.st.SQL, ok, others)
	}
}

// The server drops every portal when a transaction ends. A Bind sent after
// the end but not answered yet stays.
func TestPortalsEndWithTheirTransaction(t *testing.T) {
	g := newStmtGate()
	s := readySession(g)
	send(s, queryMsg("begin"))
	serve(s, tag("BEGIN"), readyWith('T'))
	send(s, parseMsg("w", ins), bindMsg("p", "w"), syncMsg)
	serve(s, parseOK, bindOK, readyWith('T'))
	send(s, queryMsg("commit"), bindMsg("q", "w"))
	serve(s, tag("COMMIT"), readyWith('I'))

	if _, ok, _ := s.portals.get(nil, "p"); ok {
		t.Errorf("portal p outlived its transaction")
	}
	if b, ok, _ := s.portals.get(nil, "q"); !ok || b.st.SQL != ins {
		t.Errorf("portal q: %q (found %v), want the Bind not answered yet", b.st.SQL, ok)
	}
}

// A Parse whose body the proxy could not read (over 64MiB) may have set the
// unnamed statement or any name the server did not hold. Such a name means
// something unread from then on: an Execute of it is neither a clean
// statement nor a statement Stet follows. A name the server held keeps its
// meaning, since the server refuses a Parse of it.
func TestUnreadParseLeavesItsNamesUnknown(t *testing.T) {
	g := &txGate{fakeGate: newFakeGate()}
	s := readySession(g)
	flush := func() error { return nil }
	send(s, parseMsg("", "begin"), parseMsg("s1", ins), syncMsg)
	serve(s, parseOK, parseOK, readyWith('I'))
	s.fromClient('P', nil, flush)
	s.fromClient('S', nil, flush)

	if p, ok, others := s.stmts.get(nil, ""); !ok || !p.unread || len(others) != 1 || others[0].st.SQL != "begin" {
		t.Errorf("before the answer: %+v (found %v), others %v; want unread, and begin among others", p, ok, others)
	}
	serve(s, parseOK, readyWith('I'))
	for _, name := range []string{"", "never"} {
		if p, ok, _ := s.stmts.get(nil, name); !ok || !p.unread {
			t.Errorf("statement %q: %+v (found %v), want unread", name, p, ok)
		}
	}
	if p, ok, _ := s.stmts.get(nil, "s1"); !ok || p.unread || p.st.SQL != ins {
		t.Errorf("statement s1: %+v (found %v), want the insert it held", p, ok)
	}

	send(s, bindMsg("", ""), execMsg(""))
	if u := s.units[0]; !u.dirty || g.n != 0 {
		t.Errorf("Execute of the unread statement: unit dirty %v, %d writes begun; want dirty and none", u.dirty, g.n)
	}
	send(s, bindMsg("", "s1"), execMsg(""), syncMsg)
	if g.n != 1 {
		t.Errorf("Execute of s1: %d writes begun, want 1", g.n)
	}
}

// A Bind whose body the proxy could not read may have set the unnamed portal
// or any portal name the server did not hold, until the transaction ends.
func TestUnreadBindLeavesItsPortalsUnknown(t *testing.T) {
	g := &txGate{fakeGate: newFakeGate()}
	s := readySession(g)
	flush := func() error { return nil }
	send(s, queryMsg("begin"))
	serve(s, tag("BEGIN"), readyWith('T'))
	send(s, parseMsg("s1", ins), bindMsg("p", "s1"), syncMsg)
	serve(s, parseOK, bindOK, readyWith('T'))
	s.fromClient('B', nil, flush)
	s.fromClient('S', nil, flush)
	serve(s, bindOK, readyWith('T'))

	for _, name := range []string{"", "other"} {
		if b, ok, _ := s.portals.get(nil, name); !ok || !b.unread {
			t.Errorf("portal %q: %+v (found %v), want unread", name, b, ok)
		}
	}
	if b, ok, _ := s.portals.get(nil, "p"); !ok || b.unread || b.st.SQL != ins {
		t.Errorf("portal p: %+v (found %v), want the insert it held", b, ok)
	}
	send(s, execMsg("other"))
	if u := s.units[0]; !u.dirty || g.n != 0 {
		t.Errorf("Execute of an unread portal: unit dirty %v, %d writes begun; want dirty and none", u.dirty, g.n)
	}
	send(s, syncMsg)
	serve(s, failed("34000"), readyWith('E'))

	send(s, queryMsg("rollback"))
	serve(s, tag("ROLLBACK"), readyWith('I'))
	if b, ok, _ := s.portals.get(nil, "other"); ok {
		t.Errorf("after the transaction ended: portal other is %+v, want no portal", b)
	}
}

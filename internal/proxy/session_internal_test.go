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
func (g *fakeGate) End(h any, o Outcome) { g.ends[h.(int)] = o }

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

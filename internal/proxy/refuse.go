package proxy

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"
)

// Refusal is the error a client gets in place of a message the proxy did not forward.
type Refusal struct {
	Code    string // SQLSTATE
	Message string
	Detail  string
	Hint    string
}

// refusedSQL is the body a refused Query or Parse is replaced with. It is a
// syntax error, which nothing a client created can make succeed.
const refusedSQL = ") stet: statement refused"

// refusal is a message of a unit that was not sent: the server was sent a
// replacement it rejects where the message stood (see refuse), and the client
// gets r in place of the server's error.
type refusal struct {
	pos  int  // the message's pos in its unit; 0 for a Query or FunctionCall
	kind byte // the message's type, and its replacement's
	// name is the random name an Execute's or Bind's replacement runs, which
	// the server's error for it repeats.
	name string
	r    Refusal
	// given: the server's error for the replacement was rewritten into r. The
	// client counts as having it once the unit's ReadyForQuery arrived.
	given bool
}

// text is the refusal as Outcome.Error gives it.
func (rf *refusal) text() string { return "SQLSTATE " + rf.r.Code + ": " + rf.r.Message }

// refuse records that the message at pos of u (0 for a Query or FunctionCall
// unit) is answered with r, and returns the message to send in its place:
// one of kind t that fails on the server. name is the Parse's statement name.
//
// The replacement fails where the message stood and changes nothing, so the
// server rolls back the rest of its Sync, or fails the open transaction, as
// for an error of its own (spec 6). A Query or Parse is replaced by a syntax
// error, a Parse under the same name; a caller that could not read a Parse
// passes freshName(). An Execute names a portal and a Bind a statement that
// cannot exist (freshName), and a FunctionCall the function OID 0. Nothing of
// the message itself reaches the server.
func (s *session) refuse(u *unit, pos int, t byte, name string, r Refusal) []byte {
	rf := refusal{pos: pos, kind: t, r: r}
	var m pgproto3.FrontendMessage
	switch t {
	case 'Q':
		m = &pgproto3.Query{String: refusedSQL}
	case 'P':
		m = &pgproto3.Parse{Name: name, Query: refusedSQL}
	case 'E':
		rf.name = freshName()
		m = &pgproto3.Execute{Portal: rf.name}
	case 'B':
		rf.name = freshName()
		m = &pgproto3.Bind{PreparedStatement: rf.name}
	case 'F':
		m = &pgproto3.FunctionCall{Function: 0}
	default:
		panic("proxy: no replacement for message type " + string(t))
	}
	s.mu.Lock()
	u.refused = append(u.refused, rf)
	s.mu.Unlock()
	b, err := m.Encode(nil)
	if err != nil {
		panic("proxy: replacement not encoded: " + err.Error()) // a few fixed fields cannot fail
	}
	return b
}

// freshName returns a statement or portal name no client can have made:
// "stet_" and 16 bytes from crypto/rand in hex, 37 bytes, new on every call.
// The server compares names by their first 63 bytes, and portals share their
// namespace with SQL cursors, so a fixed name or a longer one could be taken
// by a cursor the client declared.
func freshName() string {
	var b [16]byte
	rand.Read(b[:]) // never fails: it crashes the program instead
	return "stet_" + hex.EncodeToString(b[:])
}

// replaced returns the refusal whose replacement the server's error e
// answers, or nil. It runs with s.mu held, for the first error of u only, so
// at most one error per unit is rewritten.
//
// The error is the replacement's when it is an ERROR (a FATAL ends the
// session, whatever the message), it answers the replacement, and it is the
// error the server gives that replacement (answers). It answers the
// replacement when, in a Query or FunctionCall unit, it is the unit's error,
// and in an extended unit, it came at the refused message's pos (errPos). An
// error at an earlier message means the server skipped the replacement, and
// one at the Sync (errPos past msgs: a deferred constraint failing at commit)
// answers no message; neither is rewritten. Should the proxy ever match an
// error to the wrong unit, the last test leaves that error alone and the
// refusal undelivered, the lesser failure, instead of giving the client the
// gate's error for another statement.
func (u *unit) replaced(e []byte) *refusal {
	for i := range u.refused {
		rf := &u.refused[i]
		if rf.pos != 0 && rf.pos != u.errPos {
			continue
		}
		var m pgproto3.ErrorResponse
		if m.Decode(e) != nil {
			return nil
		}
		severity := m.SeverityUnlocalized
		if severity == "" {
			severity = m.Severity
		}
		if severity != "ERROR" || !rf.answers(&m) {
			return nil
		}
		return rf
	}
	return nil
}

// answers reports whether e is the error the server gives rf's replacement
// (checked on PostgreSQL 14.18, outside and inside a failed transaction): a
// syntax error for a Query or Parse, which the server parses before it looks
// at the transaction; for an Execute or Bind, the portal or statement it
// names does not exist, and the server's message repeats that random name;
// for a FunctionCall, function OID 0 does not exist, or the transaction has
// failed.
func (rf *refusal) answers(e *pgproto3.ErrorResponse) bool {
	switch rf.kind {
	case 'Q', 'P':
		return e.Code == "42601"
	case 'E':
		return e.Code == "34000" && strings.Contains(e.Message, rf.name)
	case 'B':
		return e.Code == "26000" && strings.Contains(e.Message, rf.name)
	case 'F':
		return e.Code == "42883" || e.Code == "25P02"
	}
	return false
}

// refusalError is the ErrorResponse a client is given in place of the
// server's error: severity ERROR and r's fields, nothing else. Code and
// Message are always present, as the protocol requires of every
// ErrorResponse; a NUL, which would end a field early, is dropped.
func refusalError(r Refusal) []byte {
	b := []byte{'E', 0, 0, 0, 0}
	field := func(k byte, v string, always bool) {
		if v == "" && !always {
			return
		}
		b = append(b, k)
		b = append(b, strings.ReplaceAll(v, "\x00", "")...)
		b = append(b, 0)
	}
	field('S', "ERROR", true)
	field('V', "ERROR", true)
	field('C', r.Code, true)
	field('M', r.Message, true)
	field('D', r.Detail, false)
	field('H', r.Hint, false)
	b = append(b, 0)
	binary.BigEndian.PutUint32(b[1:], uint32(len(b)-1))
	return b
}

// refusedWrite is the outcome of the watched write w, which the gate
// refused. It did not run, so it is never Executed or Lost. It is Undelivered
// unless its replacement's error was rewritten and, with reached, the unit's
// ReadyForQuery arrived (spec correction B8).
func (u *unit) refusedWrite(w watchedWrite, inTx, reached bool) Outcome {
	o := Outcome{InTransaction: inTx, Refused: true, Undelivered: true}
	for i := range u.refused {
		if rf := &u.refused[i]; rf.pos == w.pos {
			o.Error = rf.text()
			o.Undelivered = !reached || !rf.given
		}
	}
	return o
}

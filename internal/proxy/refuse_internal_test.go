package proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

var heldRefusal = Refusal{Code: "ST001", Message: "held", Detail: "d", Hint: "h"}

// refusingGate is a fakeGate that refuses every write with heldRefusal.
func refusingGate() *fakeGate {
	g := newFakeGate()
	r := heldRefusal
	g.refusal = &r
	return g
}

// whole checks that b is one whole message of type want, as the server reads
// it, and returns its body.
func whole(t *testing.T, b []byte, want byte) []byte {
	t.Helper()
	if len(b) < 5 || b[0] != want || int(binary.BigEndian.Uint32(b[1:5])) != len(b)-1 {
		t.Fatalf("%q is not one whole %c message", b, want)
	}
	return b[5:]
}

var freshNameRE = regexp.MustCompile(`^stet_[0-9a-f]{32}$`)

func TestRefusedMessagesAreReplacedInKind(t *testing.T) {
	s := readySession(newFakeGate())
	u := &unit{}
	decode := func(m interface{ Decode([]byte) error }, b []byte, kind byte) {
		t.Helper()
		if err := m.Decode(whole(t, b, kind)); err != nil {
			t.Fatalf("%c: %v", kind, err)
		}
	}

	var q pgproto3.Query
	decode(&q, s.refuse(u, 0, 'Q', "", heldRefusal), 'Q')
	if q.String != ") stet: statement refused" {
		t.Errorf("Query replaced by %+v, want the syntax error", q)
	}

	var p pgproto3.Parse
	decode(&p, s.refuse(u, 1, 'P', "s1", heldRefusal), 'P')
	if p.Name != "s1" || p.Query != ") stet: statement refused" || len(p.ParameterOIDs) != 0 {
		t.Errorf("Parse replaced by %+v, want s1 with the syntax error", p)
	}

	var e1, e2 pgproto3.Execute
	decode(&e1, s.refuse(u, 2, 'E', "", heldRefusal), 'E')
	decode(&e2, s.refuse(u, 3, 'E', "", heldRefusal), 'E')
	for _, e := range []pgproto3.Execute{e1, e2} {
		if !freshNameRE.MatchString(e.Portal) || e.MaxRows != 0 {
			t.Errorf("Execute replaced by %+v, want a fresh portal name", e)
		}
	}
	if e1.Portal == e2.Portal {
		t.Errorf("two refused Executes both name portal %q, want a new name each time", e1.Portal)
	}

	var b1, b2 pgproto3.Bind
	decode(&b1, s.refuse(u, 4, 'B', "", heldRefusal), 'B')
	decode(&b2, s.refuse(u, 5, 'B', "", heldRefusal), 'B')
	for _, b := range []pgproto3.Bind{b1, b2} {
		if b.DestinationPortal != "" || !freshNameRE.MatchString(b.PreparedStatement) ||
			len(b.Parameters) != 0 || len(b.ParameterFormatCodes) != 0 || len(b.ResultFormatCodes) != 0 {
			t.Errorf("Bind replaced by %+v, want a fresh statement name into the unnamed portal", b)
		}
	}
	if b1.PreparedStatement == b2.PreparedStatement {
		t.Errorf("two refused Binds both name statement %q, want a new name each time", b1.PreparedStatement)
	}

	var f pgproto3.FunctionCall
	decode(&f, s.refuse(u, 0, 'F', "", heldRefusal), 'F')
	if f.Function != 0 || len(f.Arguments) != 0 || len(f.ArgFormatCodes) != 0 {
		t.Errorf("FunctionCall replaced by %+v, want OID 0 without arguments", f)
	}

	if len(e1.Portal) > maxName || len(b1.PreparedStatement) > maxName {
		t.Errorf("names %q and %q, want at most %d bytes", e1.Portal, b1.PreparedStatement, maxName)
	}
	var pos []int
	for _, rf := range u.refused {
		if rf.r != heldRefusal {
			t.Errorf("recorded %+v, want the refusal given", rf)
		}
		pos = append(pos, rf.pos)
	}
	if want := []int{0, 1, 2, 3, 4, 5, 0}; !slices.Equal(pos, want) {
		t.Errorf("recorded at %v, want %v", pos, want)
	}
}

// syntaxError is how the server answers a replaced Query or Parse.
func syntaxError() *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "42601", Message: `syntax error at or near ")"`, Position: 1}
}

func serverErr(code string) *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: code, Message: "server says no"}
}

// relayed hands the session server messages the way pump does, and returns
// what pump forwards to the client for each: the message, or what see gave
// in its place.
func relayed(t *testing.T, s *session, msgs ...pgproto3.BackendMessage) [][]byte {
	t.Helper()
	tp := s.serverTap()
	var out [][]byte
	for _, m := range msgs {
		b, err := m.Encode(nil)
		if err != nil {
			t.Fatal(err)
		}
		if tp.each != nil {
			tp.each(b[0])
		}
		if tp.want(b[0]) {
			r, err := tp.see(b[0], b[5:], nil)
			if err != nil {
				t.Fatal(err)
			}
			if r != nil {
				b = r
			}
		}
		out = append(out, b)
	}
	return out
}

// wantGiven checks that what the client got in place of the error is the
// refusal and nothing else.
func wantGiven(t *testing.T, got []byte) {
	t.Helper()
	var e pgproto3.ErrorResponse
	if err := e.Decode(whole(t, got, 'E')); err != nil {
		t.Fatal(err)
	}
	want := pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "ST001", Message: "held", Detail: "d", Hint: "h"}
	if e.Severity != want.Severity || e.SeverityUnlocalized != want.SeverityUnlocalized || e.Code != want.Code ||
		e.Message != want.Message || e.Detail != want.Detail || e.Hint != want.Hint || e.Position != 0 || len(e.UnknownFields) != 0 {
		t.Errorf("client got %+v, want %+v only", e, want)
	}
	if e, _ := want.Encode(nil); !bytes.Equal(got, e) {
		t.Errorf("client got %q, want exactly %q", got, e)
	}
}

func wantUnchanged(t *testing.T, got []byte, m pgproto3.BackendMessage) {
	t.Helper()
	if b, _ := m.Encode(nil); !bytes.Equal(got, b) {
		t.Errorf("client got %q, want the server's %q unchanged", got, b)
	}
}

func refusedOutcome(t *testing.T, o Outcome, undelivered bool) {
	t.Helper()
	if !o.Refused || o.Undelivered != undelivered || o.Executed || o.Lost || o.Rows != nil || o.Error != "SQLSTATE ST001: held" {
		t.Errorf("outcome %+v, want refused with the refusal's error, undelivered %v", o, undelivered)
	}
}

// Only the error the server raised for the replacement itself is rewritten:
// not an error of an earlier message, not one at the Sync, not a FATAL, not a
// second error of the unit.
func TestOnlyTheReplacementsOwnErrorIsRewritten(t *testing.T) {
	flush := func() error { return nil }
	query := func(t *testing.T, s *session) []byte {
		t.Helper()
		r, _ := s.fromClient('Q', body(&pgproto3.Query{String: "insert into t values (1)"}), flush)
		if r == nil {
			t.Fatal("the refused Query was forwarded")
		}
		return r
	}

	t.Run("refused Query", func(t *testing.T) {
		g := refusingGate()
		s := readySession(g)
		query(t, s)
		out := relayed(t, s, syntaxError(), ready)
		wantGiven(t, out[0])
		wantUnchanged(t, out[1], ready)
		refusedOutcome(t, g.ends[0], false)
	})

	t.Run("refused Execute", func(t *testing.T) {
		g := refusingGate()
		s := readySession(g)
		send(s, parseMsg("", ins), bindMsg("", ""), describeMsg('P', ""), execMsg(""), syncMsg)
		out := relayed(t, s, parseOK, bindOK, &pgproto3.NoData{}, serverErr("34000"), ready)
		wantGiven(t, out[3])
		refusedOutcome(t, g.ends[0], false)
	})

	t.Run("earlier message failed", func(t *testing.T) {
		g := refusingGate()
		s := readySession(g)
		send(s, parseMsg("", ins), bindMsg("", ""), execMsg(""), syncMsg)
		bad := serverErr("22P02")
		out := relayed(t, s, parseOK, bad, ready)
		wantUnchanged(t, out[1], bad)
		refusedOutcome(t, g.ends[0], true)
	})

	// The server never completes a replacement; were it to, the error at
	// the Sync would still answer no message.
	t.Run("error at the Sync", func(t *testing.T) {
		g := refusingGate()
		s := readySession(g)
		send(s, parseMsg("", ins), bindMsg("", ""), execMsg(""), syncMsg)
		late := serverErr("23505")
		out := relayed(t, s, parseOK, bindOK, tag("INSERT 0 1"), late, ready)
		wantUnchanged(t, out[3], late)
		refusedOutcome(t, g.ends[0], true)
	})

	t.Run("FATAL in its place", func(t *testing.T) {
		g := refusingGate()
		s := readySession(g)
		query(t, s)
		fatal := &pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"}
		out := relayed(t, s, fatal)
		wantUnchanged(t, out[0], fatal)
		s.abandon()
		refusedOutcome(t, g.ends[0], true)
	})

	t.Run("second error of the unit", func(t *testing.T) {
		g := refusingGate()
		s := readySession(g)
		query(t, s)
		fatal := &pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "57P01", Message: "terminating"}
		out := relayed(t, s, syntaxError(), fatal)
		wantGiven(t, out[0])
		wantUnchanged(t, out[1], fatal)
	})

	t.Run("refused Parse and FunctionCall", func(t *testing.T) {
		// Their replacements' errors, at the Parse's pos and as the
		// FunctionCall unit's error.
		s := readySession(newFakeGate())
		s.mu.Lock()
		u, pos := s.count()
		s.mu.Unlock()
		s.refuse(u, pos, 'P', "s1", heldRefusal)
		send(s, syncMsg)
		out := relayed(t, s, syntaxError(), ready)
		wantGiven(t, out[0])

		f := s.push(&unit{closed: true, dirty: true})
		s.refuse(f, 0, 'F', "", heldRefusal)
		out = relayed(t, s, serverErr("42883"), ready)
		wantGiven(t, out[0])
	})

	t.Run("no refusal", func(t *testing.T) {
		g := newFakeGate()
		s := readySession(g)
		if r, _ := s.fromClient('Q', body(&pgproto3.Query{String: "insert into t values (1)"}), flush); r != nil {
			t.Fatalf("a write the gate let through was replaced by %q", r)
		}
		dup := serverErr("23505")
		out := relayed(t, s, dup, ready)
		wantUnchanged(t, out[0], dup)
		if o := g.ends[0]; o.Refused || o.Undelivered || o.Error != "SQLSTATE 23505: server says no" {
			t.Errorf("outcome %+v, want the server's error and not refused", o)
		}
	})

	t.Run("notices are not seen", func(t *testing.T) {
		if newSession(newFakeGate(), SessionInfo{}).serverTap().want('N') {
			t.Error("the server tap wants NoticeResponse")
		}
	})
}

// A refused write is not delivered unless its unit's ReadyForQuery arrived:
// when the connection ends first, it is Undelivered whether or not its error
// was already rewritten (spec correction B8).
func TestRefusalOfALostSessionIsUndelivered(t *testing.T) {
	flush := func() error { return nil }
	insert := body(&pgproto3.Query{String: "insert into t values (1)"})

	g := refusingGate()
	s := readySession(g)
	s.fromClient('Q', insert, flush)
	s.abandon()
	refusedOutcome(t, g.ends[0], true)

	g = refusingGate()
	s = readySession(g)
	s.fromClient('Q', insert, flush)
	if out := relayed(t, s, syntaxError()); !bytes.Equal(out[0][:1], []byte{'E'}) {
		t.Fatalf("relayed %q", out[0])
	}
	s.abandon()
	refusedOutcome(t, g.ends[0], true)
	if len(g.ends) != 1 {
		t.Errorf("%d writes ended, want 1", len(g.ends))
	}

	// Extended: the unit's Bind completed, then the connection ended.
	g = refusingGate()
	s = readySession(g)
	send(s, parseMsg("", ins), bindMsg("", ""), execMsg(""), syncMsg)
	relayed(t, s, parseOK, bindOK, serverErr("34000"))
	s.abandon()
	refusedOutcome(t, g.ends[0], true)
}

// objGate watches the messages that mention insert. A message that mentions
// obo also sets stet.on_behalf_of to user:9, and one that mentions declare
// also declares cursor c over an observed read. It records the session each
// write arrived with.
type objGate struct {
	*fakeGate
	infos []SessionInfo
}

func (g *objGate) Inspect(sql string) Statement {
	st := Statement{SQL: sql, Watch: strings.Contains(sql, "insert")}
	if strings.Contains(sql, "obo") {
		v := "user:9"
		st.OnBehalfOf = &v
	}
	if strings.Contains(sql, "declare") {
		st.Observe = true
		st.Cursors = []Cursor{{Op: CursorDeclare, Name: "c"}}
	}
	return st
}

func (g *objGate) Begin(info SessionInfo, st Statement, ps []Param) (any, *Refusal) {
	g.infos = append(g.infos, info)
	return g.fakeGate.Begin(info, st, ps)
}

// A refused message never ran on the server, so it leaves the session as it
// was: on_behalf_of and the cursor table unchanged. Its own SET still counts
// for its judgment, as for a write let through.
func TestRefusedMessageLeavesTheSessionAsItWas(t *testing.T) {
	both := "obo declare insert"
	for _, extended := range []bool{false, true} {
		g := &objGate{fakeGate: refusingGate()}
		s := readySession(g)
		sendIt := func(sql string) {
			if extended {
				send(s, parseMsg("", sql), bindMsg("", ""), execMsg(""), syncMsg)
			} else {
				send(s, queryMsg(sql))
			}
		}

		sendIt(both)
		if s.info.OnBehalfOf != "" || len(s.cursors) != 0 {
			t.Errorf("extended %v: after the refusal on_behalf_of %q, cursors %v; want both unchanged", extended, s.info.OnBehalfOf, s.cursors)
		}
		g.refusal = nil
		sendIt(both)
		if s.info.OnBehalfOf != "user:9" || s.cursors["c"] == nil {
			t.Errorf("extended %v: after the write let through on_behalf_of %q, cursors %v; want both set", extended, s.info.OnBehalfOf, s.cursors)
		}
		for i, info := range g.infos {
			if info.OnBehalfOf != "user:9" {
				t.Errorf("extended %v: write %d began with on_behalf_of %q, want its own user:9", extended, i+1, info.OnBehalfOf)
			}
		}
	}
}

// zeros reads as an endless run of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func encoded(t *testing.T, m pgproto3.Message) []byte {
	t.Helper()
	b, err := m.Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// pump sends what see gives in place of a message. A message past maxInspect
// that is replaced has its body read and dropped, and the stream stays in
// step. A message see does not replace goes as it came.
func TestPumpSendsTheReplacement(t *testing.T) {
	replacement := encoded(t, &pgproto3.Query{String: "in its place"})
	tp := &tap{
		want: func(t byte) bool { return t == 'Q' },
		see: func(_ byte, body []byte, _ func() error) ([]byte, error) {
			if body == nil || bytes.Contains(body, []byte("refuse me")) {
				return replacement, nil
			}
			return nil, nil
		},
	}
	big := []byte{'Q', 0, 0, 0, 0}
	binary.BigEndian.PutUint32(big[1:], uint32(4+maxInspect+1))
	sync := encoded(t, &pgproto3.Sync{})
	src := io.MultiReader(
		bytes.NewReader(encoded(t, &pgproto3.Query{String: "refuse me"})),
		bytes.NewReader(encoded(t, &pgproto3.Query{String: "select 1"})),
		bytes.NewReader(big), io.LimitReader(zeros{}, maxInspect+1),
		bytes.NewReader(sync),
	)
	var dst bytes.Buffer
	if err := pump(&dst, src, tp); err != io.EOF {
		t.Fatalf("pump: %v, want io.EOF at the end", err)
	}
	want := slices.Concat(replacement, encoded(t, &pgproto3.Query{String: "select 1"}), replacement, sync)
	if !bytes.Equal(dst.Bytes(), want) {
		t.Errorf("pump sent %d bytes %q, want %q", dst.Len(), dst.Bytes()[:min(dst.Len(), 200)], want)
	}

	// An error from see ends the pump; the message is not sent.
	stop := errors.New("stop")
	tp.see = func(byte, []byte, func() error) ([]byte, error) { return nil, stop }
	dst.Reset()
	if err := pump(&dst, bytes.NewReader(encoded(t, &pgproto3.Query{String: "select 1"})), tp); err != stop {
		t.Errorf("pump: %v, want see's error", err)
	}
	if dst.Len() != 0 {
		t.Errorf("pump sent %q after see's error, want nothing", dst.Bytes())
	}
}

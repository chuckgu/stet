package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// Gate is told about the writes Stet watches and the reads it observes.
// Implementations must be safe for concurrent use: Begin runs on a session's
// client side, End and Observe on its server side or after the session ended.
type Gate interface {
	// Inspect classifies the SQL of a Query or Parse message.
	Inspect(sql string) Statement
	// Begin runs just before a watched statement goes upstream: the last
	// moment its pre-write state is visible. The client waits for it.
	Begin(s SessionInfo, st Statement, params []Param) any
	// End reports how upstream answered. It must not block.
	End(handle any, o Outcome)
	// Observe reports a read after its response ended, or when the connection
	// ended first. It is never called before the statement goes upstream, and it
	// must not block.
	Observe(s SessionInfo, st Statement, o Outcome)
}

type Statement struct {
	SQL   string
	Watch bool
	// Observe marks a read: it is forwarded at once, its bind values are not
	// kept, and Observe is called when its response ends. A statement with
	// Watch set is a watched write even when Observe is set too.
	Observe    bool
	OnBehalfOf *string // set when the statement sets stet.on_behalf_of; "" resets it
	Data       any     // the gate's own classification, handed back in Begin and Observe
	// Cursors lists what the message does to SQL cursors, in statement
	// order. A FETCH is reported through Observe with the Statement that
	// declared its cursor. With Cursors set, Observe decides what the message
	// remembers and how it is reported (see applyCursors) instead of making it
	// a read by itself. A watched message is a watched write, and its cursor
	// statements still apply.
	Cursors []Cursor
	// Several says the message holds more than one statement.
	Several bool
}

type Param struct {
	Value  []byte // nil is NULL
	Format int16  // 0 text, 1 binary
	OID    uint32 // from Parse; 0 when the client left the type to the server
}

type SessionInfo struct {
	ID string
	// User is the role the startup packet names. The gate is handed it only
	// once the server has authenticated the session (its first ReadyForQuery);
	// a statement sent before that has User "".
	User            string
	Database        string
	ApplicationName string
	OnBehalfOf      string
	// TxOpen means an explicit transaction was already open when the statement
	// arrived (the last ReadyForQuery status was not idle).
	TxOpen bool
}

type Outcome struct {
	// Executed means the statement ran. It does not mean it was committed.
	Executed bool
	// Error holds the SQLSTATE and message when the statement failed or was
	// skipped because an earlier one in the same Sync failed.
	Error string
	// InTransaction means the statement ran inside an explicit transaction,
	// or next to other statements under one Sync (an implicit transaction).
	InTransaction bool
	// Rows is the row count from the statement's CommandComplete tag; nil when
	// it did not complete or the tag carries no count.
	Rows *int64
	// Lost means the connection ended before the server answered. The
	// statement may have run and committed; Executed is false because nothing
	// said so.
	Lost bool
	// Sent is how many row messages (DataRow, CopyData) went to the client in
	// answer to the statement, also when it failed or was cancelled. Set only
	// for Observe; End's is 0. Rows are counted as their headers are read from
	// the server, so when the connection drops Sent can include rows not yet
	// written to the client, at most a buffer's worth.
	Sent int64
	// Before is how many rows earlier reports already counted for the same
	// cursor or portal: the FETCHes of one DECLARE, or the Executes of one
	// bound portal. 0 for a statement read in one go. Set only for Observe.
	Before int64
	// Arrived is when the proxy read the statement from the client. Set only
	// for Observe.
	Arrived time.Time
}

// session follows one client connection closely enough to tell the gate
// which watched statement ran, whether it failed, whether a transaction was
// open, and how many rows each observed read sent.
type session struct {
	gate Gate
	info SessionInfo

	// Touched only on the client side.
	stmts   map[string]prepared
	portals map[string]bound

	// Touched only on the server side, and by abandon once both sides have
	// stopped: row messages sent since the last completion or error.
	sent int64

	mu      sync.Mutex
	units   []*unit              // sent, not yet answered by ReadyForQuery; oldest first
	ready   bool                 // the post-startup ReadyForQuery has arrived
	status  byte                 // transaction status from the last ReadyForQuery
	cursors map[string]*declared // SQL cursors by name; nil until a DECLARE needs it
	// unsettled holds the names in cursors whose newest entry can still change
	// at a ReadyForQuery: all but held cursors that outlived their transaction.
	unsettled map[string]struct{}
}

type prepared struct {
	st   Statement
	oids []uint32
}

type bound struct {
	st     Statement
	params []Param
	// rows read through the portal so far, for an observed one; a new Bind
	// starts over. Updated on the server side as its Executes are answered.
	rows *int64
}

// unit is the stretch of client messages answered by one ReadyForQuery: a
// Query, a FunctionCall, or extended-protocol messages through Sync.
type unit struct {
	closed      bool // its Query or Sync was sent; nothing more joins it
	query       bool
	executes    int // Execute messages so far
	completions int // CommandComplete, EmptyQueryResponse, PortalSuspended
	err         string
	errAt       int // the Execute the error belongs to (1-based)
	writes      []watchedWrite
	reads       []observedRead
	rows        map[int]int64 // row counts by completion (1-based); kept only once a watched write or observed read joined
	sent        map[int]int64 // row messages sent by Execute (1-based), or 0 for a Query; kept only once an observed read joined
}

type watchedWrite struct {
	handle any
	idx    int // which Execute in the unit; 0 for a Query
}

type observedRead struct {
	info SessionInfo // as it was when the statement arrived
	st   Statement
	idx  int       // which Execute in the unit; 0 for a Query
	at   time.Time // when the statement arrived
	// ifRows: reported only if it sent rows (a message of several statements
	// that declares a cursor; its rows are read through the FETCHes).
	ifRows bool
	// total is the running count of rows read through the same cursor or
	// portal, nil for a read in one go. before is what it held when this
	// read was answered: tally sets it, in the order of the answers.
	total  *int64
	before int64
}

func newSession(g Gate, info SessionInfo) *session {
	return &session{
		gate:    g,
		info:    info,
		stmts:   map[string]prepared{},
		portals: map[string]bound{},
		status:  'I',
	}
}

// sessionInfo reads identity from the StartupMessage packet.
func sessionInfo(pkt []byte) SessionInfo {
	var id [16]byte
	rand.Read(id[:])
	info := SessionInfo{ID: hex.EncodeToString(id[:])}
	var m pgproto3.StartupMessage
	if m.Decode(pkt[4:]) == nil {
		info.User = m.Parameters["user"]
		info.Database = m.Parameters["database"]
		if info.Database == "" {
			info.Database = info.User
		}
		info.ApplicationName = m.Parameters["application_name"]
	}
	return info
}

func (s *session) clientTap() *tap {
	return &tap{
		want: func(t byte) bool {
			switch t {
			case 'Q', 'P', 'B', 'E', 'C', 'D', 'H', 'S', 'F':
				return true
			}
			return false
		},
		see: s.fromClient,
	}
}

func (s *session) serverTap() *tap {
	return &tap{
		want: func(t byte) bool {
			switch t {
			case 'C', 'I', 's', 'E', 'Z':
				return true
			}
			return false
		},
		see: func(t byte, body []byte, _ func() error) { s.fromServer(t, body) },
		each: func(t byte) {
			if t == 'D' || t == 'd' { // DataRow, CopyData
				s.sent++
			}
		},
	}
}

func (s *session) fromClient(t byte, body []byte, flush func() error) {
	switch t {
	case 'Q':
		u := s.push(&unit{closed: true, query: true})
		var m pgproto3.Query
		if body == nil || m.Decode(body) != nil {
			return
		}
		st := s.gate.Inspect(m.String)
		s.apply(st)
		switch {
		case st.Watch:
			s.begin(u, 0, st, nil, flush)
			if len(st.Cursors) > 0 { // a write among cursor statements
				s.applyCursors(u, 0, st)
			}
		case len(st.Cursors) > 0:
			s.applyCursors(u, 0, st)
		case st.Observe:
			s.observe(u, 0, st, nil)
		}

	case 'P':
		s.open()
		var m pgproto3.Parse
		if body == nil || m.Decode(body) != nil {
			return
		}
		s.stmts[m.Name] = prepared{st: s.gate.Inspect(m.Query), oids: slices.Clone(m.ParameterOIDs)}

	case 'B':
		s.open()
		var m pgproto3.Bind
		if body == nil || m.Decode(body) != nil {
			return
		}
		p, ok := s.stmts[m.PreparedStatement]
		if !ok || (!p.st.Watch && !p.st.Observe && p.st.OnBehalfOf == nil && len(p.st.Cursors) == 0) {
			delete(s.portals, m.DestinationPortal)
			return
		}
		b := bound{st: p.st}
		switch {
		case p.st.Watch: // only Begin is handed bind values
			b.params = params(&m, p.oids)
		case p.st.Observe:
			b.rows = new(int64)
		}
		s.portals[m.DestinationPortal] = b

	case 'E':
		u := s.open()
		s.mu.Lock()
		u.executes++
		idx := u.executes
		s.mu.Unlock()
		var m pgproto3.Execute
		if body == nil || m.Decode(body) != nil {
			return
		}
		b, ok := s.portals[m.Portal]
		if !ok {
			return
		}
		s.apply(b.st)
		switch {
		case b.st.Watch:
			s.begin(u, idx, b.st, b.params, flush)
			if len(b.st.Cursors) > 0 {
				s.applyCursors(u, idx, b.st)
			}
		case len(b.st.Cursors) > 0:
			s.applyCursors(u, idx, b.st)
		case b.st.Observe:
			s.observe(u, idx, b.st, b.rows)
		}

	case 'C':
		s.open()
		var m pgproto3.Close
		if body == nil || m.Decode(body) != nil {
			return
		}
		if m.ObjectType == 'S' {
			delete(s.stmts, m.Name)
		} else {
			delete(s.portals, m.Name)
		}

	case 'D', 'H':
		s.open()

	case 'S':
		s.mu.Lock()
		if n := len(s.units); n > 0 && !s.units[n-1].closed {
			s.units[n-1].closed = true
		} else {
			s.units = append(s.units, &unit{closed: true})
		}
		s.mu.Unlock()

	case 'F':
		s.push(&unit{closed: true})
	}
}

func (s *session) apply(st Statement) {
	if st.OnBehalfOf != nil {
		s.info.OnBehalfOf = *st.OnBehalfOf
	}
}

// begin hands a watched write to the gate. Messages already queued go out
// first so they are not held back while the gate reads state.
func (s *session) begin(u *unit, idx int, st Statement, ps []Param, flush func() error) {
	flush()
	s.mu.Lock()
	info := s.identity()
	s.mu.Unlock()
	h := s.gate.Begin(info, st, ps)
	s.mu.Lock()
	u.writes = append(u.writes, watchedWrite{handle: h, idx: idx})
	s.mu.Unlock()
}

// observe notes a read in its unit. Nothing waits for the gate and nothing
// queued is flushed first: the gate hears of the read once it was answered.
// rows is the running count of the portal it reads, or nil for a Query.
func (s *session) observe(u *unit, idx int, st Statement, rows *int64) {
	s.mu.Lock()
	u.reads = append(u.reads, s.arriving(st, idx, rows))
	s.mu.Unlock()
}

// arriving records a read reported as st, arriving now from the client as
// Execute idx of its unit (0 for a Query), that counts toward total (nil for
// a read in one go). It runs on the client side with
// s.mu held, and only for reads, so no other statement pays for the clock.
func (s *session) arriving(st Statement, idx int, total *int64) observedRead {
	return observedRead{info: s.identity(), st: st, idx: idx, at: time.Now(), total: total}
}

// identity is the session as the gate is told of a statement arriving now.
// Until the server's first ReadyForQuery the client has not authenticated,
// so the role its startup packet names is not passed on. TxOpen is whether
// the last ReadyForQuery left a transaction open. It runs with s.mu held.
func (s *session) identity() SessionInfo {
	info := s.info
	if !s.ready {
		info.User = ""
	}
	info.TxOpen = s.status != 'I'
	return info
}

func (s *session) push(u *unit) *unit {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.units = append(s.units, u)
	return u
}

// open returns the extended-protocol unit still collecting messages,
// starting one if needed.
func (s *session) open() *unit {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.units); n > 0 && !s.units[n-1].closed {
		return s.units[n-1]
	}
	u := &unit{}
	s.units = append(s.units, u)
	return u
}

func (s *session) fromServer(t byte, body []byte) {
	s.mu.Lock()
	var head *unit
	if len(s.units) > 0 {
		head = s.units[0]
	}
	switch t {
	case 'C', 'I', 's':
		s.settle(head)
		if head != nil {
			head.completions++
			// Watched writes and observed reads join their unit before they
			// go upstream, so units with neither never need their tags read.
			if t == 'C' && (len(head.writes) > 0 || len(head.reads) > 0) {
				if n, ok := tagRows(body); ok {
					if head.rows == nil {
						head.rows = map[int]int64{}
					}
					head.rows[head.completions] = n
				}
			}
		}
	case 'E':
		s.settle(head)
		if head != nil && head.err == "" {
			head.err = errorText(body)
			head.errAt = head.completions + 1
		}
	case 'Z':
		s.settle(head) // normally nothing is left; never carry rows into the next unit
		status := byte('I')
		if len(body) > 0 {
			status = body[0]
		}
		before := s.status
		s.status = status
		if !s.ready {
			s.ready = true
			s.mu.Unlock()
			return
		}
		if head == nil {
			s.mu.Unlock()
			return
		}
		s.units = s.units[1:]
		if len(s.unsettled) > 0 {
			s.cursorsAnswered(head, status)
		}
		head.tally()
		s.mu.Unlock()
		s.finish(head, before, status)
		return
	}
	s.mu.Unlock()
}

// settle moves the row messages sent since the last completion or error to
// head, under the statement that was answering: the Execute after its last
// completion, or the Query as a whole. A unit without observed reads drops
// them. It runs on the server side with s.mu held, or from abandon.
func (s *session) settle(head *unit) {
	n := s.sent
	if n == 0 {
		return
	}
	s.sent = 0
	if head == nil || len(head.reads) == 0 {
		return
	}
	k := head.completions + 1
	if head.query {
		k = 0
	}
	if head.sent == nil {
		head.sent = map[int]int64{}
	}
	head.sent[k] += n
}

// tally sets the rows before each read of u that counts toward a cursor or
// portal, and adds its rows to that count. Units are answered in the order
// they were sent, so it runs for each unit in turn: on the server side with
// s.mu held, or from abandon.
func (u *unit) tally() {
	for i := range u.reads {
		if r := &u.reads[i]; r.total != nil {
			r.before = *r.total
			*r.total += u.sent[r.idx]
		}
	}
}

// finish reports the statements of an answered unit. before and after are
// the transaction status around it.
func (s *session) finish(u *unit, before, after byte) {
	inTx := before != 'I' || after != 'I' || u.executes > 1
	for _, w := range u.writes {
		s.gate.End(w.handle, u.answered(w.idx, inTx))
	}
	for _, r := range u.reads {
		o := u.answered(r.idx, inTx)
		o.Sent, o.Before, o.Arrived = u.sent[r.idx], r.before, r.at
		if r.ifRows && o.Sent == 0 {
			continue
		}
		s.gate.Observe(r.info, r.st, o)
	}
}

// answered is what the server said, before its ReadyForQuery, about the
// statement at Execute idx of u (0 for a Query).
func (u *unit) answered(idx int, inTx bool) Outcome {
	o := Outcome{InTransaction: inTx}
	if text, ok := u.refusal(idx); ok {
		o.Error = text
		return o
	}
	switch {
	case u.query:
		o.Executed = true
		if u.completions == 1 { // several statements: no single count
			o.Rows = u.rowsAt(1)
		}
	case u.completions >= idx:
		o.Executed = true
		o.Rows = u.rowsAt(idx)
	default:
		o.Error = "no completion before ReadyForQuery"
	}
	return o
}

// refusal reports the server's error for the statement at Execute idx (0 for
// a Query) when the server already said it failed or was skipped because an
// earlier statement in the same Sync failed. It reports nothing when the
// server has not spoken for it.
func (u *unit) refusal(idx int) (string, bool) {
	switch {
	case u.err == "":
		return "", false
	case u.query, idx == u.errAt:
		return u.err, true
	case idx > u.errAt:
		return "not executed: an earlier statement in the same Sync failed: " + u.err, true
	}
	return "", false
}

// rowsAt returns the row count of the unit's k-th completion, or nil when its
// tag carried none.
func (u *unit) rowsAt(k int) *int64 {
	if n, ok := u.rows[k]; ok {
		return &n
	}
	return nil
}

// tagRows reads the row count from a CommandComplete body.
func tagRows(body []byte) (int64, bool) {
	tag, _, _ := bytes.Cut(body, []byte{0})
	i := bytes.LastIndexByte(tag, ' ')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(tag[i+1:]), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// abandon reports the statements still waiting when the connection ended.
// It runs once both sides have stopped.
func (s *session) abandon() {
	s.mu.Lock()
	units := s.units
	s.units = nil
	s.cursors, s.unsettled = nil, nil
	status := s.status
	s.mu.Unlock()
	if len(units) > 0 {
		s.settle(units[0]) // rows of the statement that was still answering
	}
	for _, u := range units {
		u.tally()
		inTx := status != 'I' || u.executes > 1
		for _, w := range u.writes {
			s.gate.End(w.handle, u.unanswered(w.idx, inTx))
		}
		for _, r := range u.reads {
			o := u.unanswered(r.idx, inTx)
			o.Sent, o.Before, o.Arrived = u.sent[r.idx], r.before, r.at
			if r.ifRows && o.Sent == 0 {
				continue
			}
			s.gate.Observe(r.info, r.st, o)
		}
	}
}

// unanswered is what is known of the statement at Execute idx of u (0 for a
// Query) when the connection ended before u's ReadyForQuery.
func (u *unit) unanswered(idx int, inTx bool) Outcome {
	o := Outcome{InTransaction: inTx}
	if text, ok := u.refusal(idx); ok {
		o.Error = text // the server answered before the connection ended
	} else {
		o.Error = "connection closed before ReadyForQuery"
		o.Lost = true
	}
	return o
}

func params(m *pgproto3.Bind, oids []uint32) []Param {
	ps := make([]Param, len(m.Parameters))
	for i, v := range m.Parameters {
		p := Param{Value: cloneBytes(v)}
		switch n := len(m.ParameterFormatCodes); {
		case n == 1:
			p.Format = m.ParameterFormatCodes[0]
		case i < n:
			p.Format = m.ParameterFormatCodes[i]
		}
		if i < len(oids) {
			p.OID = oids[i]
		}
		ps[i] = p
	}
	return ps
}

// cloneBytes copies b out of the reused read buffer, keeping nil (NULL)
// distinct from empty.
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append(make([]byte, 0, len(b)), b...)
}

func errorText(body []byte) string {
	var m pgproto3.ErrorResponse
	if m.Decode(body) != nil {
		return "unreadable ErrorResponse"
	}
	return fmt.Sprintf("SQLSTATE %s: %s", m.Code, m.Message)
}

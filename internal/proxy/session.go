package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5/pgproto3"
)

// Gate is told about the writes Stet watches. Implementations must be safe
// for concurrent use: Begin runs on a session's client side, End on its
// server side.
type Gate interface {
	// Inspect classifies the SQL of a Query or Parse message.
	Inspect(sql string) Statement
	// Begin runs just before a watched statement goes upstream: the last
	// moment its pre-write state is visible. The client waits for it.
	Begin(s SessionInfo, st Statement, params []Param) any
	// End reports how upstream answered. It must not block.
	End(handle any, o Outcome)
}

type Statement struct {
	SQL        string
	Watch      bool
	OnBehalfOf *string // set when the statement sets stet.on_behalf_of; "" resets it
	Data       any     // the gate's own classification, handed back in Begin
}

type Param struct {
	Value  []byte // nil is NULL
	Format int16  // 0 text, 1 binary
	OID    uint32 // from Parse; 0 when the client left the type to the server
}

type SessionInfo struct {
	ID              string
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
}

// session follows one client connection closely enough to tell the gate
// which watched statement ran, whether it failed, and whether a transaction
// was open.
type session struct {
	gate Gate
	info SessionInfo

	// Touched only on the client side.
	stmts   map[string]prepared
	portals map[string]bound

	mu     sync.Mutex
	units  []*unit // sent, not yet answered by ReadyForQuery; oldest first
	ready  bool    // the post-startup ReadyForQuery has arrived
	status byte    // transaction status from the last ReadyForQuery
}

type prepared struct {
	st   Statement
	oids []uint32
}

type bound struct {
	st     Statement
	params []Param
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
	rows        map[int]int64 // row counts by completion (1-based); kept only once a watched write joined
}

type watchedWrite struct {
	handle any
	idx    int // which Execute in the unit; 0 for a Query
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
		if st.Watch {
			s.begin(u, 0, st, nil, flush)
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
		if !ok || (!p.st.Watch && p.st.OnBehalfOf == nil) {
			delete(s.portals, m.DestinationPortal)
			return
		}
		s.portals[m.DestinationPortal] = bound{st: p.st, params: params(&m, p.oids)}

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
		if b.st.Watch {
			s.begin(u, idx, b.st, b.params, flush)
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
	info := s.info
	s.mu.Lock()
	info.TxOpen = s.status != 'I'
	s.mu.Unlock()
	h := s.gate.Begin(info, st, ps)
	s.mu.Lock()
	u.writes = append(u.writes, watchedWrite{handle: h, idx: idx})
	s.mu.Unlock()
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
		if head != nil {
			head.completions++
			// A watched write's CommandComplete always follows its Begin, so
			// units without watched writes never need their tags read.
			if t == 'C' && len(head.writes) > 0 {
				if n, ok := tagRows(body); ok {
					if head.rows == nil {
						head.rows = map[int]int64{}
					}
					head.rows[head.completions] = n
				}
			}
		}
	case 'E':
		if head != nil && head.err == "" {
			head.err = errorText(body)
			head.errAt = head.completions + 1
		}
	case 'Z':
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
		s.mu.Unlock()
		s.finish(head, before, status)
		return
	}
	s.mu.Unlock()
}

// finish reports the writes of an answered unit. before and after are the
// transaction status around it.
func (s *session) finish(u *unit, before, after byte) {
	inTx := before != 'I' || after != 'I' || u.executes > 1
	for _, w := range u.writes {
		o := Outcome{InTransaction: inTx}
		switch {
		case u.query && u.err != "":
			o.Error = u.err
		case u.query:
			o.Executed = true
			if u.completions == 1 { // several statements: no single count
				o.Rows = u.rowsAt(1)
			}
		case u.err != "" && w.idx == u.errAt:
			o.Error = u.err
		case u.err != "" && w.idx > u.errAt:
			o.Error = "not executed: an earlier statement in the same Sync failed: " + u.err
		case u.completions >= w.idx:
			o.Executed = true
			o.Rows = u.rowsAt(w.idx)
		default:
			o.Error = "no completion before ReadyForQuery"
		}
		s.gate.End(w.handle, o)
	}
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

// abandon reports writes still waiting when the connection ended.
func (s *session) abandon() {
	s.mu.Lock()
	units := s.units
	s.units = nil
	status := s.status
	s.mu.Unlock()
	for _, u := range units {
		for _, w := range u.writes {
			s.gate.End(w.handle, Outcome{
				Error:         "connection closed before ReadyForQuery",
				Lost:          true,
				InTransaction: status != 'I' || u.executes > 1,
			})
		}
	}
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

package proxy

import (
	"bytes"
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
	// Begin runs just before a watched statement goes upstream, and the client
	// waits for it. When refuse is not nil the statement is not sent: the proxy
	// sends a message of the same kind that the server rejects, and gives the
	// client refuse in place of the server's error. End is still called.
	Begin(s SessionInfo, st Statement, params []Param) (handle any, refuse *Refusal)
	// End reports how upstream answered. It must not block.
	End(handle any, o Outcome)
	// Observe reports a read after its response ended, or when the connection
	// ended first. It is never called before the statement goes upstream, and it
	// must not block.
	Observe(s SessionInfo, st Statement, o Outcome)
	// Blocked returns the error for a message refused for reason on a watched
	// session, and logs it: no decision records such a message. It must not block.
	Blocked(s SessionInfo, reason string) Refusal
}

// Reasons a message is refused without a decision, or the session is ended.
const (
	BlockUnparsed      = "unparsed"
	BlockTooLarge      = "too_large"
	BlockFunctionCall  = "function_call"
	BlockDo            = "do_block"
	BlockPreparedWrite = "prepared_write" // SQL EXECUTE, FETCH or MOVE of a name the session prepared or bound as a watched statement
	BlockUnsettledName = "unsettled_name" // the name may mean a watched statement, and the server has not said which meaning holds
	BlockProtocolOrder = "protocol_order" // the session is ended: answers can no longer be matched to messages
	BlockCopyExtended  = "copy_extended"  // the session is ended: COPY FROM STDIN through the extended protocol
	// BlockSessionSetting: the session's client_encoding or
	// standard_conforming_strings makes the server read statements otherwise
	// than the classifier (see readable); the session is ended when that
	// happened under statements already sent.
	BlockSessionSetting = "session_setting"
)

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
	// Clean marks a message whose statements are all BEGIN, START TRANSACTION, SET or SHOW.
	Clean bool
	// Block says why the message cannot be inspected (BlockUnparsed, BlockDo); "" when it can.
	Block string
	// Executes and Portals name what the SQL runs by name, as the server
	// compares names: prepared statements it runs with EXECUTE, and portals
	// it reads with FETCH or MOVE. The classifier sees no table behind them.
	Executes []string
	Portals  []string
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
	// TxDirty means a statement other than BEGIN, START TRANSACTION, SET or
	// SHOW has been sent since the server last reported the session idle,
	// whether or not the server has answered it yet (spec 4).
	TxDirty bool
}

type Outcome struct {
	// Executed means the statement ran. It does not mean it was committed.
	Executed bool
	// Error holds the SQLSTATE and message when the statement failed or was
	// skipped because an earlier one in the same Sync failed, or, when Lost,
	// says the connection closed first. A refused statement's is the
	// refusal's.
	Error string
	// InTransaction means the statement ran inside an explicit transaction,
	// or next to other statements under one Sync (an implicit transaction).
	InTransaction bool
	// Rows is the row count from the statement's CommandComplete tag; nil when
	// it did not complete or the tag carries no count.
	Rows *int64
	// Lost means the connection ended before the server's ReadyForQuery for
	// the statement. It may have run and committed. A watched write whose
	// completion the server had already sent is Executed, with its Rows;
	// otherwise Executed is false because nothing said it ran. A refused
	// statement is never Lost: it was not sent.
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
	// Refused means the statement was not sent: the gate's Begin refused it.
	Refused bool
	// Undelivered, set only with Refused, means the client was not given the
	// refusal: an earlier message under the same Sync failed first, or the
	// connection ended before the unit's ReadyForQuery.
	Undelivered bool
}

// session follows one client connection closely enough to tell the gate
// which watched statement ran, whether it failed, whether a transaction was
// open, and how many rows each observed read sent.
type session struct {
	gate Gate
	info SessionInfo
	// enforce: a message that cannot be inspected is refused, not forwarded
	// (Policy.Enforce). Fixed for the session's life.
	enforce bool

	// Touched only on the server side, and by abandon once both sides have
	// stopped: row messages sent since the last completion or error.
	sent int64

	mu     sync.Mutex
	units  []*unit // sent, not yet answered by ReadyForQuery; oldest first
	ready  bool    // the post-startup ReadyForQuery has arrived
	status byte    // transaction status from the last ReadyForQuery
	// dirty: a unit answered since the last idle ReadyForQuery held a
	// statement that is not clean. The units still waiting carry their own.
	dirty   bool
	cursors map[string]*declared // SQL cursors by name; nil until a DECLARE needs it
	// unsettled holds the names in cursors whose newest entry can still change
	// at a ReadyForQuery: all but held cursors that outlived their transaction.
	unsettled map[string]struct{}

	// The prepared statements and portals as the server holds them, under mu:
	// changed as the client sends, settled as the server answers.
	stmts   names[prepared]
	portals names[bound]

	// Enforced sessions only, under mu. utf8 and standard: the server last
	// reported client_encoding UTF8 and standard_conforming_strings on (see
	// readable). end: why the proxy is ending the session, "" while it is
	// not (see endWith). lastQuery: the newest Query unit, until the client
	// sends anything after it but a Flush, CopyDone or CopyFail; copying: the
	// Query unit whose current COPY FROM STDIN the client has not ended yet
	// (see admit, copyIn).
	utf8, standard     bool
	end                string
	lastQuery, copying *unit
}

type prepared struct {
	st   Statement
	oids []uint32
	// unread: set by a Parse the proxy could not read. What it runs is
	// unknown.
	unread bool
}

// bound is what a portal means. A Bind of a statement Stet does not follow
// leaves it empty.
type bound struct {
	st     Statement
	params []Param
	// rows read through the portal so far, for an observed one; a new Bind
	// starts over. Updated on the server side as its Executes are answered.
	rows *int64
	// unread: set by a Bind the proxy could not read, or bound to a statement
	// that was unread. What it runs is unknown.
	unread bool
	// others are the other meanings its statement's name had at the Bind
	// (see names.get): the portal runs one of them if the newest did not
	// hold.
	others []prepared
}

// unit is the stretch of client messages answered by one ReadyForQuery: a
// Query, a FunctionCall, or extended-protocol messages through Sync.
//
// Extended-protocol messages followed by a Query or FunctionCall before any
// Sync make a unit of their own that the server answers with no
// ReadyForQuery (unsynced): when they all succeed, the server runs the Query
// in the same implicit transaction and its ReadyForQuery answers both; when
// one fails, the server skips everything after it, the Query included, up to
// the next Sync, whose ReadyForQuery answers them all (spec correction B6 ①).
type unit struct {
	closed bool // nothing more joins it: see synced, unsynced; a Query or FunctionCall unit is closed from the start
	// synced: closed by a Sync. unsynced: an extended-protocol unit closed
	// by a Query or FunctionCall that came before any Sync.
	synced, unsynced bool
	// joined: a Query or FunctionCall unit that runs in the implicit
	// transaction of the unsynced unit before it, which executed a statement.
	joined bool
	// skipped: the server skipped the whole unit because an unsynced unit
	// before it failed; err is that unit's error.
	skipped bool
	// late: a Parse, Bind, Describe, Close or Execute came after an Execute
	// of the unit, so the server may read it after that Execute changed a
	// setting (see sentAfter).
	late bool
	// Of a Query unit on an enforced session (see admit, copyIn): copies,
	// the CopyInResponses (or CopyBothResponses) the server answered it with
	// so far; copyEnds, the CopyDones and CopyFails the client sent after it;
	// stray, a message other than those and Flush came after it, when
	// copyEnds was strayAt: the server reads it during copy strayAt+1, if the
	// Query starts that many copies.
	stray                     bool
	copies, copyEnds, strayAt int
	query                     bool
	dirty                     bool // it holds a statement that is not clean (see Statement.Clean)
	executes                  int  // Execute messages so far
	completions               int  // CommandComplete, EmptyQueryResponse, PortalSuspended
	err                       string
	errAt                     int // the Execute the error belongs to (1-based)
	writes                    []watchedWrite
	reads                     []observedRead
	rows                      map[int]int64 // row counts by completion (1-based); kept only once a watched write or observed read joined
	sent                      map[int]int64 // row messages sent by Execute (1-based), or 0 for a Query; kept only once an observed read joined

	// The server answers each Parse, Bind, Describe, Close and Execute it
	// completes with one message: ParseComplete, BindComplete, RowDescription
	// or NoData, CloseComplete, and CommandComplete, EmptyQueryResponse or
	// PortalSuspended. It stops at the first that fails and skips the rest
	// until Sync, so the first acks messages are the ones that ran.
	msgs   int // P, B, D, C and E messages so far; a message's pos is its 1-based count
	acks   int // how many of them the server completed so far; final at the unit's ReadyForQuery
	errPos int // pos of the message the unit's error answers; 0 when it has none, msgs+1 when it came at the Sync (a deferred constraint, say)

	// refused lists the messages of the unit that were not sent, in the order
	// they arrived (see refuse).
	refused []refusal
}

type watchedWrite struct {
	handle any
	idx    int // which Execute in the unit; 0 for a Query
	// several: a Query of several statements, whose completions do not say
	// which statement ran
	several bool
	// refused: the gate refused it, and the message at pos of the unit (0 for
	// a Query) was replaced (see refuse)
	refused bool
	pos     int
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
	s := &session{gate: g, info: info, status: 'I'}
	s.portals.freed = true // see names.freed
	return s
}

func (s *session) clientTap() *tap {
	want := func(t byte) bool {
		switch t {
		case 'Q', 'P', 'B', 'E', 'C', 'D', 'H', 'S', 'F':
			return true
		}
		return false
	}
	if s.enforce {
		// Also CopyDone and CopyFail (see admit). CopyData is still not shown.
		want = func(t byte) bool {
			switch t {
			case 'Q', 'P', 'B', 'E', 'C', 'D', 'H', 'S', 'F', 'c', 'f':
				return true
			}
			return false
		}
	}
	return &tap{want: want, see: s.fromClient}
}

func (s *session) serverTap() *tap {
	want := func(t byte) bool {
		switch t {
		case 'C', 'I', 's', 'E', 'Z':
			return true
		}
		return false
	}
	if s.enforce {
		// Also ParameterStatus (see parameter), CopyInResponse and
		// CopyBothResponse (see copyIn). Rows are still not shown.
		want = func(t byte) bool {
			switch t {
			case 'C', 'I', 's', 'E', 'Z', 'S', 'G', 'W':
				return true
			}
			return false
		}
	}
	return &tap{
		want: want,
		see: func(t byte, body []byte, _ func() error) ([]byte, error) {
			return s.fromServer(t, body)
		},
		each: func(t byte) {
			switch t {
			case 'D', 'd': // DataRow, CopyData
				s.sent++
			case '1', '2', '3', 'T', 'n': // ParseComplete, BindComplete, CloseComplete, RowDescription, NoData
				s.ack()
			}
		},
	}
}

// ack counts a message of the unit being answered as completed. It runs on
// the server side, once per completion, never per row.
func (s *session) ack() {
	s.mu.Lock()
	done := s.answeredUnsynced(nil)
	if len(s.units) > 0 {
		s.acked(s.units[0])
	}
	s.mu.Unlock()
	s.finishAll(done)
}

// answer is a unit the server has answered, to be reported (finish) once
// s.mu is let go. inTx is Outcome.InTransaction for its statements.
type answer struct {
	u    *unit
	inTx bool
}

// answeredUnsynced takes out of s.units, and appends to done, each unit at
// their head that the server has answered with no ReadyForQuery: an unsynced
// unit whose messages all completed. What comes after it runs in the same
// implicit transaction, so its statements ran inside a transaction and its
// marks pass to the session as at a ReadyForQuery that leaves one open
// (ruling P6). It runs on the server side with s.mu held, before a server
// message is matched to the unit it answers. The rows not yet settled
// belong to the unit after it: its own were settled at its completions.
func (s *session) answeredUnsynced(done []answer) []answer {
	for len(s.units) > 0 {
		u := s.units[0]
		if !u.unsynced || u.err != "" || u.acks < u.msgs {
			break
		}
		s.leave(u, 'T')
		done = append(done, answer{u, true})
	}
	return done
}

// leave takes u, the head of s.units, out once the server has answered it,
// with the transaction status after (the ReadyForQuery's, or 'T' for an
// unsynced unit, whose implicit transaction goes on). Its table changes the
// server completed hold and the rest are dropped; while a transaction stays
// open its marks pass to the session; its cursor statements are settled; its
// rows are tallied. Units leave in the order they were sent, through here
// alone, so each unit's table changes are the oldest waiting (see names). It
// runs with s.mu held.
func (s *session) leave(u *unit, after byte) {
	s.units = s.units[1:]
	if s.copying == u {
		s.copying = nil // the copy ended on the server, by an error at least
	}
	if s.lastQuery == u {
		s.lastQuery = nil
	}
	s.stmts.answered(u, u.acks)
	s.portals.answered(u, u.acks)
	if after != 'I' {
		s.carry(u)
	}
	if len(s.unsettled) > 0 {
		s.cursorsAnswered(u, after)
	}
	u.tally()
}

// skippedBehind reports how many of the units after units[0] the server
// skips when units[0] is an unsynced unit that failed: those up to and
// including the first closed by a Sync, where the server resumes, or all of
// them when none is (synced false). n is 0 for any other head.
func skippedBehind(units []*unit) (n int, synced bool) {
	if head := units[0]; !head.unsynced || head.err == "" {
		return 0, false
	}
	for i, u := range units[1:] {
		if u.synced {
			return i + 1, true
		}
	}
	return len(units) - 1, false
}

// skip marks units, which the server skipped after a unit that failed with
// err, as skipped.
func skip(units []*unit, err string) {
	for _, u := range units {
		u.skipped, u.err = true, err
	}
}

func (s *session) finishAll(done []answer) {
	for _, a := range done {
		s.finish(a.u, a.inTx)
	}
}

// acked counts one more message of head, the unit being answered, as
// completed, and applies the table changes that completion makes hold. It
// runs with s.mu held.
func (s *session) acked(head *unit) {
	head.acks++
	s.stmts.completed(head, head.acks)
	s.portals.completed(head, head.acks)
}

// fromClient follows a message from the client before it is forwarded, and
// returns the message to send in its place when the gate refused it.
//
// On an enforced session the closed points (spec 7) are refused here, before
// anything else is made of them: a Query or Parse the gate marks with Block,
// a Query, Parse, Bind or Execute the proxy could not read, every
// FunctionCall, a statement that runs a watched name (runsWatched), an
// Execute of a name whose meaning is not settled (executeBlock), and what
// the server may read otherwise than the classifier while the session's
// settings are not readable (see settings.go). The gate is told through
// Blocked, and the message never ran, so the statement and portal tables,
// on_behalf_of and the cursor table stay as they were. It returns an error
// once the proxy is ending the session (see end.go).
func (s *session) fromClient(t byte, body []byte, flush func() error) ([]byte, error) {
	if s.enforce {
		if err := s.admit(t); err != nil {
			return nil, err
		}
	}
	switch t {
	case 'Q':
		u := s.push(&unit{closed: true, query: true})
		var m pgproto3.Query
		if body == nil || m.Decode(body) != nil {
			s.markDirty(u) // it may hold anything
			if s.enforce {
				return s.block(u, 0, 'Q', "", unreadable(body)), nil
			}
			return nil, nil
		}
		st := s.gate.Inspect(m.String)
		if s.enforce {
			s.mu.Lock()
			reason := st.Block
			switch {
			case s.misread(m.String):
				reason = BlockSessionSetting
				st = Statement{} // what the classifier saw does not count
			case reason == "" && runsNames(st) && s.runsWatched(u, st, maxRunDepth):
				reason = BlockPreparedWrite
			}
			s.mu.Unlock()
			if reason != "" {
				s.dirties(u, st)
				return s.block(u, 0, 'Q', "", reason), nil
			}
		}
		return s.statement(u, 0, 0, 'Q', st, nil, nil, flush), nil

	case 'P':
		var m pgproto3.Parse
		read := body != nil && m.Decode(body) == nil
		var p prepared
		if read {
			p = prepared{st: s.gate.Inspect(m.Query), oids: slices.Clone(m.ParameterOIDs)}
		}
		s.mu.Lock()
		u, pos := s.count()
		misread := read && s.enforce && s.misread(m.Query)
		if s.enforce && (!read || misread || p.st.Block != "") {
			s.mu.Unlock()
			// The table is left as it was. The server keeps a named
			// statement it holds when the replacement fails, as for any
			// failing Parse. For the unnamed statement it does not: a Parse
			// of it drops the old one first, while the table keeps the old
			// meaning. That costs extra judgments of a statement the server
			// no longer has (its Binds fail), never an unjudged write: the
			// safe direction. A Parse that could not be read is replaced
			// under a fresh name, since its own name is not known, so no
			// name the session uses changes on the server; so is one whose
			// name the server would convert from client_encoding.
			switch {
			case !read:
				return s.block(u, pos, 'P', freshName(), unreadable(body)), nil
			case misread && !ascii(m.Name):
				return s.block(u, pos, 'P', freshName(), BlockSessionSetting), nil
			case misread:
				return s.block(u, pos, 'P', m.Name, BlockSessionSetting), nil
			}
			return s.block(u, pos, 'P', m.Name, p.st.Block), nil
		}
		if read {
			s.stmts.put(u, pos, m.Name, true, p)
		} else {
			s.stmts.putUnread(u, pos, prepared{unread: true})
		}
		s.mu.Unlock()

	case 'B':
		var m pgproto3.Bind
		read := body != nil && m.Decode(body) == nil
		var p prepared
		var ok bool
		var others []prepared
		s.mu.Lock()
		u, pos := s.count()
		if s.enforce && !read {
			s.mu.Unlock()
			// The replacement names a statement that cannot exist, so the
			// server fails it before it touches any portal.
			return s.block(u, pos, 'B', "", unreadable(body)), nil
		}
		if read {
			p, ok, others = s.stmts.get(u, m.PreparedStatement)
			// An older meaning that is the same statement is no other meaning.
			others = slices.DeleteFunc(others, func(q prepared) bool { return ok && p.same(q) })
		}
		if s.enforce && read && !s.readable() &&
			(!ascii(m.PreparedStatement) || !ascii(m.DestinationPortal) || mayWrite(p, ok, others)) {
			s.mu.Unlock()
			// The server converts names and bind values from client_encoding:
			// it may bind another statement or portal than the tables say,
			// or hand a watched write other values than Begin would judge.
			return s.block(u, pos, 'B', "", BlockSessionSetting), nil
		}
		s.mu.Unlock()
		// Bind values are copied without the lock: the server side takes it
		// for every completion it counts.
		b := bound{unread: true}
		if read {
			b = binding(&m, p, ok, others)
		}
		s.mu.Lock()
		if read {
			s.portals.put(u, pos, m.DestinationPortal, true, b)
		} else {
			s.portals.putUnread(u, pos, b)
		}
		s.mu.Unlock()

	case 'E':
		var m pgproto3.Execute
		read := body != nil && m.Decode(body) == nil
		s.mu.Lock()
		u, pos := s.count()
		u.executes++
		idx := u.executes
		var b bound
		var ok bool
		var others []bound
		if read {
			b, ok, others = s.portals.get(u, m.Portal)
		}
		var reason string
		switch {
		case !s.enforce:
		case !read:
			// Its portal is not known; the server compares portal names by
			// their first 63 bytes, so it may run any of them.
			reason = unreadable(body)
		case !s.readable() && !ascii(m.Portal):
			// The server converts the name from client_encoding, so it may
			// run another portal than the table says.
			reason = BlockSessionSetting
		default:
			reason = s.executeBlock(u, b, ok, others)
		}
		s.mu.Unlock()
		if reason != "" {
			s.markDirty(u)
			return s.block(u, pos, 'E', "", reason), nil
		}
		if !ok || b.unread {
			// It may run anything, or a statement Stet does not follow.
			s.markDirty(u)
			return nil, nil
		}
		r := s.statement(u, idx, pos, 'E', b.st, b.params, b.rows, flush)
		if mayRunUnclean(b, others) {
			s.markDirty(u) // the server may run another meaning of the name
		}
		return r, nil

	case 'C':
		var m pgproto3.Close
		read := body != nil && m.Decode(body) == nil
		s.mu.Lock()
		u, pos := s.count()
		// A Close the proxy could not read changes nothing here: a name it
		// may have closed stays remembered. So does a name the server would
		// convert from client_encoding, which may close another name.
		switch {
		case !read:
		case s.enforce && !s.readable() && !ascii(m.Name):
		case m.ObjectType == 'S':
			s.stmts.put(u, pos, m.Name, false, prepared{})
		case m.ObjectType == 'P':
			s.portals.put(u, pos, m.Name, false, bound{})
		}
		s.mu.Unlock()

	case 'D':
		s.mu.Lock()
		s.count()
		s.mu.Unlock()

	case 'H':
		s.open()

	case 'S':
		s.mu.Lock()
		u := s.openLocked()
		u.closed, u.synced = true, true
		s.mu.Unlock()

	case 'F':
		u := s.push(&unit{closed: true, dirty: true}) // the function may change anything
		if s.enforce {
			return s.block(u, 0, 'F', "", BlockFunctionCall), nil
		}
	}
	return nil, nil
}

// unreadable is why a message whose body is body could not be inspected: nil
// is a body past maxInspect, which was not read; any other could not be
// decoded.
func unreadable(body []byte) string {
	if body == nil {
		return BlockTooLarge
	}
	return BlockUnparsed
}

// block refuses the message t at pos of u (see refuse; name as there), which
// cannot be inspected, for reason, and returns the message to send in its
// place. The gate gives the client's error through Blocked and logs it; no
// decision records the message, so nothing waits for its End. The gate is
// called without s.mu held, and nothing queued is flushed first: Blocked does
// not block.
func (s *session) block(u *unit, pos int, t byte, name, reason string) []byte {
	s.mu.Lock()
	info := s.identity()
	s.mu.Unlock()
	return s.refuse(u, pos, t, name, s.gate.Blocked(info, reason))
}

// statement follows st, arriving as a Query (idx and pos 0) or as Execute idx
// at pos of u, with the bind values ps and the running row count rows of its
// portal. It returns the message to send in its place when the gate refused
// it, and nil to forward it.
//
// A refused statement never runs, so it leaves on_behalf_of and the cursor
// table as they were. It still marks u: its replacement was sent and fails,
// and the server's answer settles the rest.
func (s *session) statement(u *unit, idx, pos int, t byte, st Statement, ps []Param, rows *int64, flush func() error) []byte {
	if st.Watch {
		if r := s.begin(u, idx, pos, t, st, ps, flush); r != nil {
			s.dirties(u, st)
			return r
		}
	}
	s.apply(st)
	switch {
	case len(st.Cursors) > 0: // for a watched write too: a write among cursor statements
		s.applyCursors(u, idx, st)
	case st.Observe && !st.Watch:
		s.observe(u, idx, st, rows)
	}
	s.dirties(u, st)
	return nil
}

func (s *session) apply(st Statement) {
	if st.OnBehalfOf != nil {
		s.info.OnBehalfOf = *st.OnBehalfOf
	}
}

// dirties marks u when the message st stands for, sent as part of u, is not
// clean: the transaction it runs in is dirty from then on. A watched write
// is never clean; it marks u only here, after Begin was told whether the
// transaction was clean when it arrived.
func (s *session) dirties(u *unit, st Statement) {
	if unclean(st) {
		s.markDirty(u)
	}
}

// unclean reports whether st leaves the transaction it runs in dirty.
func unclean(st Statement) bool { return st.Watch || !st.Clean }

// mayRunUnclean reports whether an Execute of a portal whose newest meaning
// is b may run, in its place, a statement that is not clean or is unknown:
// one of others, the portal name's other meanings (see names.get), or one
// its statement's name meant at the Bind of b or of one of others.
func mayRunUnclean(b bound, others []bound) bool {
	return anyOther(b, others, func(st Statement, unread bool) bool { return unread || unclean(st) })
}

// anyOther reports whether f holds for a statement an Execute of a portal
// whose newest meaning is b may run in its place: one of others, the portal
// name's other meanings (see names.get), or one its statement's name meant
// at the Bind of b or of one of others (bound.others). unread says what the
// statement is is not known.
func anyOther(b bound, others []bound, f func(st Statement, unread bool) bool) bool {
	for _, p := range b.others {
		if f(p.st, p.unread) {
			return true
		}
	}
	for _, o := range others {
		if f(o.st, o.unread) {
			return true
		}
		for _, p := range o.others {
			if f(p.st, p.unread) {
				return true
			}
		}
	}
	return false
}

// mayWrite reports whether a statement name whose newest meaning is p (ok:
// it has one) and whose other meanings are others may be a watched write,
// one Stet cannot read, or one that runs names.
func mayWrite(p prepared, ok bool, others []prepared) bool {
	w := func(q prepared) bool { return q.unread || q.st.Watch || runsNames(q.st) }
	return ok && w(p) || slices.ContainsFunc(others, w)
}

// anyMeaning is anyOther with b's own statement too, when the portal has a
// newest meaning (ok).
func anyMeaning(b bound, ok bool, others []bound, f func(st Statement, unread bool) bool) bool {
	return ok && f(b.st, b.unread) || anyOther(b, others, f)
}

// executeBlock is why an enforced session refuses an Execute arriving in u
// of a portal whose newest meaning is b (ok: it has one) and whose other
// meanings are others, or "".
//
// When the portal may run more than one statement and one of them is a
// watched write or unknown, it is refused: the server has not said which
// one holds, and Begin can judge only one (spec correction B4). A portal is
// bound for its lifetime to the meanings its statement's name had at the
// Bind, so it stays refused after the change was answered: the lesser
// failure. When a statement it may run runs a watched name in turn, it is
// refused as such (see runsWatched). The gate is not asked. It runs with s.mu
// held.
func (s *session) executeBlock(u *unit, b bound, ok bool, others []bound) string {
	if len(b.others) > 0 || len(others) > 0 {
		if anyMeaning(b, ok, others, func(st Statement, unread bool) bool { return unread || st.Watch }) {
			return BlockUnsettledName
		}
	}
	if anyMeaning(b, ok, others, func(st Statement, _ bool) bool { return s.runsWatched(u, st, maxRunDepth) }) {
		return BlockPreparedWrite
	}
	return ""
}

// maxRunDepth is how many names deep runsWatched follows a statement that
// runs a name whose statement runs a name in turn.
const maxRunDepth = 4

// runsNames reports whether st runs a prepared statement or a portal by name.
func runsNames(st Statement) bool { return len(st.Executes) > 0 || len(st.Portals) > 0 }

// runsWatched reports whether st, arriving now in u, may run a watched write
// by name, where the classifier sees no table (spec correction B5): a
// prepared statement it runs with SQL EXECUTE, or a portal it reads with
// FETCH or MOVE, under any meaning the name may have (see names.get). A
// meaning that is unknown counts as watched. A meaning that runs names in
// turn is followed depth more names deep, and past that counts as watched;
// a statement can run itself. It runs with s.mu held.
func (s *session) runsWatched(u *unit, st Statement, depth int) bool {
	if !runsNames(st) {
		return false
	}
	if depth == 0 {
		return true
	}
	watched := func(st Statement, unread bool) bool {
		return unread || st.Watch || s.runsWatched(u, st, depth-1)
	}
	for _, name := range st.Executes {
		p, ok, others := s.stmts.get(u, name)
		if ok && watched(p.st, p.unread) {
			return true
		}
		for _, o := range others {
			if watched(o.st, o.unread) {
				return true
			}
		}
	}
	for _, name := range st.Portals {
		if b, ok, others := s.portals.get(u, name); anyMeaning(b, ok, others, watched) {
			return true
		}
	}
	return false
}

// markDirty notes that u holds a statement that is not clean.
func (s *session) markDirty(u *unit) {
	s.mu.Lock()
	u.dirty = true
	s.mu.Unlock()
}

// carry moves the transaction marks of u, which is no longer waiting while
// the transaction it ran in may still be open, to the session. It runs with
// s.mu held.
func (s *session) carry(u *unit) {
	s.dirty = s.dirty || u.dirty
}

// txDirty reports whether a statement that is not clean was sent since the
// server last reported the session idle: in a unit answered since then, or in
// one still waiting. It runs with s.mu held.
func (s *session) txDirty() bool {
	if s.dirty {
		return true
	}
	for _, u := range s.units {
		if u.dirty {
			return true
		}
	}
	return false
}

// begin hands a watched write, arriving as message t at pos of u (see
// statement), to the gate. Messages already queued go out first so they are
// not held back while the gate reads state. When the gate refuses the write,
// begin returns the message to send in its place.
func (s *session) begin(u *unit, idx, pos int, t byte, st Statement, ps []Param, flush func() error) []byte {
	flush()
	s.mu.Lock()
	info := s.identity()
	s.mu.Unlock()
	if st.OnBehalfOf != nil {
		info.OnBehalfOf = *st.OnBehalfOf // a SET in the write's own message counts for it
	}
	h, refuse := s.gate.Begin(info, st, ps)
	w := watchedWrite{handle: h, idx: idx, several: st.Several}
	var r []byte
	if refuse != nil {
		r = s.refuse(u, pos, t, "", *refuse)
		w.refused, w.pos = true, pos
	}
	s.mu.Lock()
	u.writes = append(u.writes, w)
	s.mu.Unlock()
	return r
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
// a read in one go). It runs on the client side with s.mu held, and only for
// reads, so no other statement pays for the clock.
func (s *session) arriving(st Statement, idx int, total *int64) observedRead {
	return observedRead{info: s.identity(), st: st, idx: idx, at: time.Now(), total: total}
}

// identity is the session as the gate is told of a statement arriving now.
// Until the server's first ReadyForQuery the client has not authenticated,
// so the role its startup packet names is not passed on. TxOpen is whether
// the last ReadyForQuery left a transaction open, TxDirty whether a statement
// that is not clean was sent since the last idle one; the statement arriving
// has not marked its unit yet. It runs with s.mu held.
func (s *session) identity() SessionInfo {
	info := s.info
	if !s.ready {
		info.User = ""
	}
	info.TxOpen = s.status != 'I'
	info.TxDirty = s.txDirty()
	return info
}

// push adds u, the unit of a Query or FunctionCall arriving now. An
// extended-protocol unit still collecting messages ends here, unsynced, and
// u runs in its implicit transaction.
func (s *session) push(u *unit) *unit {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.units); n > 0 && !s.units[n-1].closed {
		last := s.units[n-1]
		last.closed, last.unsynced = true, true
		u.joined = last.executes > 0
	}
	s.units = append(s.units, u)
	if s.enforce && u.query {
		s.lastQuery = u // see admit
	}
	return u
}

// open returns the extended-protocol unit still collecting messages,
// starting one if needed.
func (s *session) open() *unit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openLocked()
}

// openLocked is open with s.mu held.
func (s *session) openLocked() *unit {
	if n := len(s.units); n > 0 && !s.units[n-1].closed {
		return s.units[n-1]
	}
	u := &unit{}
	s.units = append(s.units, u)
	return u
}

// count adds a Parse, Bind, Describe, Close or Execute arriving now to the
// unit still collecting messages, and returns that unit and the message's
// pos in it. It runs with s.mu held.
func (s *session) count() (*unit, int) {
	u := s.openLocked()
	u.msgs++
	u.late = u.late || u.executes > 0
	return u, u.msgs
}

// binding is what the portal of Bind m means, given what its statement's
// name meant (see names.get). A portal is given a statement when its Execute
// does something beyond leaving the transaction dirty, or runs a name (see
// runsWatched); one left empty dirties it. Only a watched statement keeps
// the bind values, for Begin.
func binding(m *pgproto3.Bind, p prepared, ok bool, others []prepared) bound {
	b := bound{others: others}
	switch {
	case !ok:
	case p.unread:
		b.unread = true
	case p.st.Watch:
		b.st, b.params = p.st, params(m, p.oids)
	case p.st.Observe:
		b.st, b.rows = p.st, new(int64)
	case p.st.Clean || p.st.OnBehalfOf != nil || len(p.st.Cursors) > 0 || runsNames(p.st):
		b.st = p.st
	}
	return b
}

// fromServer follows a message from the server before it is forwarded, and
// returns the message to send in its place: the refusal, for the error that
// answers a refused message's replacement.
func (s *session) fromServer(t byte, body []byte) ([]byte, error) {
	s.mu.Lock()
	done := s.answeredUnsynced(nil)
	var head *unit
	if len(s.units) > 0 {
		head = s.units[0]
	}
	var out []byte
	var err error
	switch t {
	case 'C', 'I', 's':
		s.settle(head)
		if head != nil {
			head.completions++
			s.acked(head)
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
			if !head.query {
				head.errPos = head.acks + 1
			}
			if len(head.refused) > 0 {
				if rf := head.replaced(body); rf != nil {
					rf.given = true
					head.err = rf.text() // what the client got, for the statements it hid
					out = refusalError(rf.r)
				}
			}
		}
	case 'Z':
		done = s.readyForQuery(head, body, done)
	case 'S': // enforced sessions only
		err = s.parameter(head, body)
	case 'G', 'W': // enforced sessions only
		err = s.copyIn(head)
	}
	s.mu.Unlock()
	s.finishAll(done)
	return out, err
}

// readyForQuery follows a ReadyForQuery with body body, which answers head
// and, when head is an unsynced unit that failed, the units the server
// skipped behind it (see unit). It appends them to done. It runs on the
// server side with s.mu held.
func (s *session) readyForQuery(head *unit, body []byte, done []answer) []answer {
	s.settle(head) // normally nothing is left; never carry rows into the next unit
	status := byte('I')
	if len(body) > 0 {
		status = body[0]
	}
	before := s.status
	s.status = status
	if status == 'I' {
		// No transaction is open: what was answered before cannot make the
		// next one dirty. The units still waiting keep theirs.
		s.dirty = false
	}
	if !s.ready {
		s.ready = true
		return done
	}
	if head != nil {
		n := 1
		if k, synced := skippedBehind(s.units); synced {
			skip(s.units[1:1+k], head.err)
			n += k
		}
		inTx := before != 'I' || status != 'I'
		for _, u := range s.units[:n] {
			s.leave(u, status)
			done = append(done, answer{u, inTx || u.executes > 1 || u.joined || u.unsynced})
		}
	}
	if status == 'I' {
		s.portals.forget() // the server drops every portal when a transaction ends
	}
	return done
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

// finish reports the statements of an answered unit; inTx says they ran
// inside a transaction (see answer).
func (s *session) finish(u *unit, inTx bool) {
	for _, w := range u.writes {
		if w.refused {
			s.gate.End(w.handle, u.refusedWrite(w, inTx, true))
			continue
		}
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
	case u.skipped, !u.query && idx > u.errAt:
		return notExecuted + u.err, true
	case u.query, idx == u.errAt:
		return u.err, true
	}
	return "", false
}

// notExecuted begins the error of a statement the server skipped because an
// earlier message failed: under the same Sync, or in an unsynced unit before
// its own.
const notExecuted = "not executed: an earlier statement in the same Sync failed: "

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
	for _, u := range units {
		// The rows of the statement that was still answering belong to the
		// first unit not answered in full: an unsynced unit that completed
		// settled its own.
		if !u.unsynced || u.err != "" || u.acks < u.msgs {
			s.settle(u)
			break
		}
	}
	for i, u := range units {
		if k, _ := skippedBehind(units[i:]); k > 0 {
			skip(units[i+1:i+1+k], u.err) // never run, so not lost
		}
		u.tally()
		inTx := status != 'I' || u.executes > 1 || u.joined || u.unsynced
		for _, w := range u.writes {
			if w.refused {
				s.gate.End(w.handle, u.refusedWrite(w, inTx, false))
				continue
			}
			s.gate.End(w.handle, u.unansweredWrite(w, inTx))
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

// unansweredWrite is unanswered for the watched write w. Once the server
// completed it, it ran: Executed and Rows are set, and it stays Lost, since
// nothing said whether its unit committed. A Query of several statements is
// left as it is: a completion does not say which of them ran.
func (u *unit) unansweredWrite(w watchedWrite, inTx bool) Outcome {
	o := u.unanswered(w.idx, inTx)
	switch {
	case !o.Lost: // the server's error
	case u.query && !w.several && u.completions > 0:
		o.Executed, o.Rows = true, u.rowsAt(1)
	case !u.query && u.completions >= w.idx:
		o.Executed, o.Rows = true, u.rowsAt(w.idx)
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

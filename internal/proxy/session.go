package proxy

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
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
	// Blocked returns the error for a message refused for reason on an
	// enforced session, or for the session when the proxy ends it. Its answer
	// depends on reason alone. No decision records either, so it is logged
	// instead when first is set: the proxy sets it the first time it refuses
	// the session anything for reason, so the log holds one line per session
	// and reason however often a client repeats what is refused (ruling 31).
	// It must not block.
	Blocked(s SessionInfo, reason string, first bool) Refusal
	// WouldBlock logs that a session that does not enforce sent a message an
	// enforced session would refuse for reason: one of the closed points the
	// proxy knows at no extra cost (unparsed, too_large, do_block,
	// defines_code, function_call, copy_extended). The proxy calls it once
	// per session and reason. Nothing is refused, and it must not block
	// (ruling 31).
	WouldBlock(s SessionInfo, reason string)
	// Unseen tells the gate that s, a session that does not enforce, is sending
	// a message whose writes the proxy cannot see: one WouldBlock is told of;
	// SQL that runs a prepared statement by name (Statement.Executes); or an
	// Execute whose portal, under any meaning its names may have, runs a
	// statement the proxy could not read, one Inspect marked with Block or one
	// that runs a prepared statement by name, or may run a watched write other
	// than its newest meaning, which Begin is not asked about and no End
	// reports. It is called every time, before the message goes upstream, and
	// must not block. One later TxEnded settles each call (see TxEnded).
	Unseen(s SessionInfo)
	// TxEnded tells the gate that what s sent in the units the server has
	// answered is committed or rolled back: the server reported s idle, or s
	// ended, after a watched write that was sent ran inside a transaction (its
	// End had Outcome.InTransaction), or after a message Unseen was told of.
	// unseen is how many Unseen calls for s it settles: those for the messages
	// of the units answered since the previous TxEnded. A unit sent behind
	// them and not answered yet is not covered, although its Unseen calls may
	// have come first: a later TxEnded settles its writes and its Unseen
	// calls. TxEnded is called once per such ReadyForQuery, after the End
	// calls of the units it answers, and once when the connection ends while
	// one is still owed; that last call settles every Unseen call not settled
	// before. So over a session's life the unseen counts add up to its Unseen
	// calls, and each Unseen call comes before the TxEnded that settles it. It
	// must not block.
	TxEnded(s SessionInfo, unseen int)
	// Tracked reports, for the table with OID table, the attribute number of the
	// key column whose values the gate remembers reads by, and how its binary
	// form is read. ok is false for any other table. The proxy calls it on the
	// server side for the columns of a tracked read's RowDescription. It must
	// not block.
	Tracked(table uint32) (attr uint16, typ KeyType, ok bool)
}

// Reasons a message is refused without a decision, or the session is ended.
const (
	BlockUnparsed     = "unparsed"
	BlockTooLarge     = "too_large"
	BlockFunctionCall = "function_call"
	BlockDo           = "do_block"
	// BlockDefinesCode: the SQL defines a function, procedure, rule or
	// trigger, or a view over a gated table, through which writes to a
	// watched table could not be seen (ruling 29).
	BlockDefinesCode   = "defines_code"
	BlockPreparedWrite = "prepared_write" // SQL EXECUTE, FETCH or MOVE of a name the session prepared or bound as a watched statement
	BlockUnsettledName = "unsettled_name" // the name may mean a watched statement, and the server has not said which meaning holds
	BlockProtocolOrder = "protocol_order" // the session is ended: answers can no longer be matched to messages
	// BlockCopyExtended: a Parse of COPY ... FROM STDIN (Statement.CopyIn),
	// whose Sync the server would ignore during the copy.
	BlockCopyExtended = "copy_extended"
	// BlockCopyStarted: the session is ended: the server started a COPY FROM
	// STDIN through the extended protocol, which the classifier did not see
	// (the backstop of BlockCopyExtended; see copyIn).
	BlockCopyStarted = "copy_started"
	// BlockSessionSetting: the session's client_encoding or
	// standard_conforming_strings makes the server read statements otherwise
	// than the classifier (see readable).
	BlockSessionSetting = "session_setting"
	// BlockSettingChanged: the session is ended: one of those settings
	// changed to such a value while statements read under the old one were
	// already sent (see parameter).
	BlockSettingChanged = "setting_changed"
	// BlockNameEncoding: the server's server_encoding is not UTF8, so it may
	// compare a name that is not ASCII otherwise than the proxy (see
	// foreignNames).
	BlockNameEncoding = "name_encoding"
)

// EndsSession reports whether reason is one the proxy ends the session for,
// rather than refusing one message.
func EndsSession(reason string) bool {
	switch reason {
	case BlockProtocolOrder, BlockCopyStarted, BlockSettingChanged:
		return true
	}
	return false
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
	// Clean marks a message whose statements are all BEGIN, START TRANSACTION, SET or SHOW.
	Clean bool
	// Sets says the message holds a SET or RESET: what the server reports of
	// the session's settings may change at its ReadyForQuery.
	Sets bool
	// Block says why the message cannot be inspected (BlockUnparsed, BlockDo,
	// BlockDefinesCode); "" when it can.
	Block string
	// CopyIn marks SQL holding COPY ... FROM STDIN. The server ignores the
	// Sync after such a copy sent through the extended protocol, so an
	// enforced session refuses it in a Parse (BlockCopyExtended), unless it
	// is a watched write, which Begin judges (see copyFromStdin); a Query may hold
	// it.
	CopyIn bool
	// Executes and Portals name what the SQL runs by name, as the server
	// compares names: prepared statements it runs with EXECUTE, and portals
	// it reads with FETCH or MOVE. The classifier sees no table behind them.
	Executes []string
	Portals  []string
	ReadOnly bool // sending the message leaves later reads of its transaction comparable (sqlparse.Result.ReadOnly)
	Track    bool // with Observe: the gate wants the keys of the rows the read sends (Outcome.Read)
	KeyParam int  // n when the read selects one row by `key = $n`: the proxy keeps that bind value for Outcome.Read; 0 otherwise
	// Resolves: the message may change which table an unqualified name
	// means: SET or RESET of search_path, role or session_authorization (SET
	// ROLE, SET SESSION AUTHORIZATION and their RESET forms included, SET
	// LOCAL too), RESET ALL or DISCARD ALL (sqlparse.Result.Resolves). A
	// prepared statement the server planned before it may read another table
	// after it.
	Resolves bool
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
	// TimeZone, DateStyle and IntervalStyle are what the server last reported for
	// the session; "" for one it has not reported. Followed on enforced sessions only.
	TimeZone, DateStyle, IntervalStyle string
	// SetPending means the three settings may not be the ones the server read
	// the statement's literals and bind values under: a SET or RESET was sent
	// that no ReadyForQuery has answered yet, before the statement or in its
	// own message; or the statement's Parse or its portal's Bind came before
	// the server reported a change of one of the three, or while such a SET
	// was not answered. Like them, followed on enforced sessions only.
	SetPending bool
	// BackendPID is the server process of the session, from its BackendKeyData;
	// 0 until the server sent it.
	BackendPID uint32
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
	// Read is what the proxy took from the answer to a tracked read
	// (Statement.Track). Set only for Observe.
	Read *ReadKeys
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
	// stopped: row messages sent since the last completion or error; the
	// keys taken from them (see keyTake); what the body of the RowDescription
	// being read is for.
	sent int64
	take keyTake
	desc rowDesc
	// reverts counts the idle ReadyForQuerys that ended a transaction in
	// which resolution moved: its end may have undone a SET LOCAL, or a SET
	// it rolled back (see resultCols). Server side only; written under mu.
	reverts uint64

	// tracking counts the units waiting that hold a read that may take keys
	// (unit.tracked). While it is 0 the server side is shown no DataRow body,
	// at the cost of one atomic load per row. Changed under mu.
	tracking atomic.Int32

	mu     sync.Mutex
	units  []*unit // sent, not yet answered by ReadyForQuery; oldest first
	ready  bool    // the post-startup ReadyForQuery has arrived
	status byte    // transaction status from the last ReadyForQuery
	// dirty: a unit answered since the last idle ReadyForQuery held a
	// statement that is not clean. The units still waiting carry their own.
	dirty bool
	// notReadOnly: likewise, a unit answered since then held a message that
	// is not read-only (see readOnlySoFar).
	notReadOnly bool
	// resolution counts the messages sent that may change which table an
	// unqualified name means, or whose content the proxy cannot see (see
	// moveResolution); resolving: a unit answered since the last idle
	// ReadyForQuery held one, so the transaction that ends there may undo
	// it. The units still waiting carry their own.
	resolution uint64
	resolving  bool
	// owed: a unit answered since the last TxEnded owes the gate one (see
	// unit.owes); unseenCalls: how many Unseen calls were made for the
	// messages of those units. The next idle ReadyForQuery hands both over in
	// one TxEnded, or abandon does. The units still waiting carry their own.
	owed        bool
	unseenCalls int
	cursors     map[string]*declared // SQL cursors by name; nil until a DECLARE needs it
	// unsettled holds the names in cursors whose newest entry can still change
	// at a ReadyForQuery: all but held cursors that outlived their transaction.
	unsettled map[string]struct{}

	// The prepared statements and portals as the server holds them, under mu:
	// changed as the client sends, settled as the server answers.
	stmts   names[prepared]
	portals names[bound]

	// told lists the reasons the gate was told of through Blocked or
	// WouldBlock, which log once per session and reason (see firstTime);
	// under mu.
	told []string

	// log takes the errors of the gate's End and Observe (see report).
	log *slog.Logger

	// gating counts the gate's Begin calls running; unrecorded holds what the
	// last one handed back until begin records the write in its unit. Both
	// are for a session whose lock a panicking side left held (see stuck,
	// abandon).
	gating     atomic.Int32
	unrecorded atomic.Pointer[unrecordedWrite]

	// Enforced sessions only, under mu. utf8 and standard: the server last
	// reported client_encoding UTF8 and standard_conforming_strings on (see
	// readable); serverUTF8: it reported server_encoding UTF8 (see foreign). end: why the proxy is ending the session, "" while it is
	// not (see endWith). lastQuery: the newest Query unit, until the client
	// sends anything after it but a Flush, CopyDone or CopyFail; copying: the
	// Query unit whose current COPY FROM STDIN the client has not ended yet
	// (see admit, copyIn). timeZone, dateStyle and intervalStyle: what the
	// server last reported of TimeZone, DateStyle and IntervalStyle (see
	// parameter), and changes: how many times one of them changed (see
	// readUnder); setsCarried: since the last ReadyForQuery, an unsynced unit
	// that held a SET or RESET was answered (see setPending).
	utf8, standard                     bool
	serverUTF8                         bool
	end                                string
	lastQuery, copying                 *unit
	timeZone, dateStyle, intervalStyle string
	changes                            int
	setsCarried                        bool
}

// readUnder is what an enforced session knew of its settings when a Parse or
// Bind arrived. The server reads a statement's literals under TimeZone,
// DateStyle and IntervalStyle at its Parse, and a portal's text bind values
// at its Bind, not at the Execute that runs them (checked on PostgreSQL
// 14.18). changes is session.changes then; pending says a SET or RESET not
// answered then may have changed them before the server read the message
// (setPending), or that the record stands for reads under different counts
// (see with).
type readUnder struct {
	changes int
	pending bool
}

// with is a record read otherwise (readOtherwise) whenever r or o is.
func (r readUnder) with(o readUnder) readUnder {
	return readUnder{changes: r.changes, pending: r.pending || o.pending || r.changes != o.changes}
}

type prepared struct {
	st      Statement
	atParse readUnder // enforced sessions only: when its Parse arrived
	oids    []uint32
	// unread: set by a Parse the proxy could not read. What it runs is
	// unknown.
	unread bool
	// cols: of a tracked read (see tracks) whose name is ASCII, the cell of
	// its result columns, new with each Parse (see resultCols); nil otherwise.
	cols *resultCols
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
	// Enforced sessions only: atParse is the Parse of each meaning its
	// statement's name had at the Bind, folded into one (see readUnder.with);
	// atBind, when the Bind arrived.
	atParse, atBind readUnder
	// Of a tracked read whose statement's name had one meaning at the Bind
	// (see bound.track): its statement's cell, the result format codes of
	// the Bind, and the value it bound to Statement.KeyParam (nil for none).
	cols    *resultCols
	formats []int16
	key     *Param
}

// read is what a write run by an Execute of b was read under: its
// statement's Parse and its Bind, and those of others, the other meanings of
// the portal's name, any of which the Execute may run.
func (b bound) read(others []bound) readUnder {
	r := b.atParse.with(b.atBind)
	for _, o := range others {
		r = r.with(o.atParse).with(o.atBind)
	}
	return r
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
	dirty                     bool     // it holds a statement that is not clean (see Statement.Clean)
	notReadOnly               bool     // it holds a message that is not read-only (see readOnlySoFar)
	resolves                  bool     // it holds a message that moved session.resolution (see moveResolution)
	tracked                   bool     // it holds a read that may take keys (see session.tracking)
	keys                      int      // the keys its reads took so far (see maxUnitKeys)
	describes                 []cellAt // its Describes whose answer may fill a cell, not passed yet (see describe)
	sets                      bool     // on an enforced session: it may hold a SET or RESET (see setPending)
	unseen                    int      // on a session that does not enforce: the Unseen calls made for its messages
	executes                  int      // Execute messages so far
	completions               int      // CommandComplete, EmptyQueryResponse, PortalSuspended
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
	// track: the read may take keys from its answer (see observe). A Query
	// is answered with its own RowDescriptions; an Execute takes its
	// statement's columns from cols, its portal's result formats, and param,
	// the value its portal kept for Statement.KeyParam. keys is Outcome.Read:
	// a Query's from its arrival, an Execute's once its answer found cols
	// known, nil otherwise. gen is session.resolution when it arrived (see
	// resultCols). Set and filled under s.mu; cols is read on the server side
	// alone.
	track   bool
	cols    *resultCols
	formats []int16
	param   *Param
	keys    *ReadKeys
	gen     uint64
}

// unrecordedWrite is what the gate's Begin handed back for a write that is
// not in its unit yet (see beginGate).
type unrecordedWrite struct {
	handle  any
	refusal *Refusal
}

func newSession(g Gate, info SessionInfo) *session {
	s := &session{gate: g, info: info, status: 'I', log: slog.New(slog.DiscardHandler)}
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

// serverTap follows the server's completions, errors and ReadyForQuery, and
// its one BackendKeyData (see backendKey), in both modes. The bodies of
// RowDescription and DataRow are read only for a tracked read (keys.go): a
// RowDescription when what its header answers wants it (rowDesc), a DataRow
// while the answer it belongs to takes keys (rowWanted). The bytes forwarded
// stay the ones the server sent.
func (s *session) serverTap() *tap {
	want := func(t byte) bool {
		switch t {
		case 'C', 'I', 's', 'E', 'Z', 'K':
			return true
		case 'T':
			return s.desc.wanted()
		case 'D':
			return s.rowWanted()
		}
		return false
	}
	if s.enforce {
		// Also ParameterStatus (see parameter), CopyInResponse and
		// CopyBothResponse (see copyIn).
		want = func(t byte) bool {
			switch t {
			case 'C', 'I', 's', 'E', 'Z', 'K', 'S', 'G', 'W':
				return true
			case 'T':
				return s.desc.wanted()
			case 'D':
				return s.rowWanted()
			}
			return false
		}
	}
	return &tap{
		want: want,
		see: func(t byte, body []byte, _ func() error) ([]byte, error) {
			switch t {
			case 'T':
				s.rowDescription(body)
				return nil, nil
			case 'D':
				s.dataRow(body)
				return nil, nil
			}
			return s.fromServer(t, body)
		},
		each: func(t byte) {
			switch t {
			case 'D', 'd': // DataRow, CopyData
				s.sent++
			case '1', '2', '3', 'T', 'n': // ParseComplete, BindComplete, CloseComplete, RowDescription, NoData
				s.ack(t)
			}
		},
	}
}

// ack counts message t, a completion of a message of the unit being
// answered, as completed; for a RowDescription it first notes what the body
// is for (describing). It runs on the server side, once per completion, never
// per row.
func (s *session) ack(t byte) {
	s.mu.Lock()
	done := s.answeredUnsynced(nil)
	if t == 'T' {
		s.desc = rowDesc{}
	}
	if len(s.units) > 0 {
		if t == 'T' {
			s.describing(s.units[0])
		}
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
// (ruling P6). A SET or RESET it held is reported only before the next
// ReadyForQuery, so the session holds that mark until then (setsCarried).
// It runs on the server side with s.mu held, before a server message is
// matched to the unit it answers. The rows not yet settled belong to the
// unit after it: its own were settled at its completions.
func (s *session) answeredUnsynced(done []answer) []answer {
	for len(s.units) > 0 {
		u := s.units[0]
		if !u.unsynced || u.err != "" || u.acks < u.msgs {
			break
		}
		a := answer{u, true}
		s.leave(a, 'T')
		s.setsCarried = s.setsCarried || u.sets
		done = append(done, a)
	}
	return done
}

// leave takes a's unit, the head of s.units, out once the server has
// answered it, with the transaction status after (the ReadyForQuery's, or 'T'
// for an unsynced unit, whose implicit transaction goes on). Its table
// changes the server completed hold and the rest are dropped; its marks pass
// to the session (carry); its cursor statements are settled; its rows are
// tallied. Units leave in the order they were sent, through here alone, so
// each unit's table changes are the oldest waiting (see names). It runs with
// s.mu held.
func (s *session) leave(a answer, after byte) {
	u := a.u
	s.units = s.units[1:]
	if s.copying == u {
		s.copying = nil // the copy ended on the server, by an error at least
	}
	if s.lastQuery == u {
		s.lastQuery = nil
	}
	if u.tracked {
		s.tracking.Add(-1)
	}
	s.stmts.answered(u, u.acks)
	s.portals.answered(u, u.acks)
	s.carry(a, after)
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
// once the proxy is ending the session (see end.go). On a session that does
// not enforce, the closed points known at no extra cost (what could not be
// read, what the gate marks with Block, a FunctionCall) go as they came, and
// the gate is told through WouldBlock; it is told through Unseen of each of
// them, of each Query or Execute that may run what hides its writes (see
// unseen, hidesWrites), and of each Execute that may run a watched write
// other than its portal's newest meaning.
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
			s.unseen(u, unreadable(body))
			return nil, nil
		}
		st := s.gate.Inspect(m.String)
		if !s.enforce && hidesWrites(st) {
			s.unseen(u, st.Block) // before a write in the same message is begun
		}
		if s.enforce {
			s.mu.Lock()
			reason := st.Block
			switch {
			case s.misread(m.String):
				reason = BlockSessionSetting
				st = Statement{} // what the classifier saw does not count
			case reason == "" && s.foreignNames(st):
				reason = BlockNameEncoding
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
			if tracks(p.st) && ascii(m.Name) {
				p.cols = new(resultCols) // filled by the answer to a Describe of it (see describe)
			}
		}
		var closedPoint string // on a session that does not enforce: the closed point it is, if any
		if !s.enforce {
			switch {
			case !read:
				closedPoint = unreadable(body)
			case p.st.Block != "":
				closedPoint = p.st.Block
			case copyFromStdin(p.st):
				closedPoint = BlockCopyExtended
			}
		}
		s.mu.Lock()
		u, pos := s.count()
		misread := read && s.enforce && s.misread(m.Query)
		foreign := read && s.enforce && (s.foreign(m.Name) || s.foreignNames(p.st))
		if s.enforce && (!read || misread || p.st.Block != "" || copyFromStdin(p.st) || foreign) {
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
			// name is not ASCII, whatever it is refused for: the server may
			// convert such a name from client_encoding, or compare it
			// otherwise (see foreign).
			name := m.Name
			if !ascii(name) {
				name = freshName()
			}
			switch {
			case !read:
				return s.block(u, pos, 'P', freshName(), unreadable(body)), nil
			case misread:
				return s.block(u, pos, 'P', name, BlockSessionSetting), nil
			case p.st.Block != "":
				return s.block(u, pos, 'P', name, p.st.Block), nil
			case copyFromStdin(p.st):
				// The server would ignore the Sync during the copy, and its
				// answers would no longer match the units (spec correction
				// B6 ②); refused here it is an ordinary error.
				return s.block(u, pos, 'P', name, BlockCopyExtended), nil
			}
			return s.block(u, pos, 'P', name, BlockNameEncoding), nil
		}
		if read {
			if s.enforce {
				p.atParse = s.readNow()
			}
			s.stmts.put(u, pos, m.Name, true, p)
		} else {
			s.stmts.putUnread(u, pos, prepared{unread: true})
		}
		s.mu.Unlock()
		if closedPoint != "" {
			s.unseen(u, closedPoint)
		}

	case 'B':
		var m pgproto3.Bind
		read := body != nil && m.Decode(body) == nil
		var p prepared
		var ok bool
		var others []prepared
		var single bool // the statement's name has one meaning, whatever the server holds
		var atParse, atBind readUnder
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
			single = ok && len(others) == 0
			if s.enforce && ok {
				// The portal runs whichever meaning of the name holds, one
				// dropped below as the same statement included, and each may
				// have been parsed under other settings.
				atParse = p.atParse
				for _, q := range others {
					atParse = atParse.with(q.atParse)
				}
			}
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
		if s.enforce && read && (s.foreign(m.PreparedStatement) || s.foreign(m.DestinationPortal)) {
			s.mu.Unlock()
			return s.block(u, pos, 'B', "", BlockNameEncoding), nil
		}
		if s.enforce {
			atBind = s.readNow()
		}
		s.mu.Unlock()
		// Bind values are copied without the lock: the server side takes it
		// for every completion it counts.
		b := bound{unread: true}
		if read {
			b = binding(&m, p, ok, others)
			b.atParse, b.atBind = atParse, atBind
			if single && ascii(m.PreparedStatement) && ascii(m.DestinationPortal) {
				// Another meaning, even the same SQL parsed again, may have
				// other columns: only a portal sure of its statement is tracked.
				b.track(&m, p)
			}
		}
		s.mu.Lock()
		if read {
			s.portals.put(u, pos, m.DestinationPortal, true, b)
		} else {
			s.portals.putUnread(u, pos, b)
		}
		s.mu.Unlock()
		if !s.enforce && !read {
			s.unseen(u, unreadable(body))
		}

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
		case s.foreign(m.Portal):
			reason = BlockNameEncoding
		default:
			reason = s.executeBlock(u, b, ok, others)
		}
		s.mu.Unlock()
		if reason != "" {
			s.markDirty(u)
			return s.block(u, pos, 'E', "", reason), nil
		}
		if !s.enforce && (!read ||
			anyMeaning(b, ok, others, func(st Statement, unread bool) bool { return unread || hidesWrites(st) }) ||
			anyOther(b, others, func(st Statement, _ bool) bool { return st.Watch })) {
			// What it runs may write where the proxy cannot see: a portal or
			// statement it could not read, or one that hides its writes, under
			// any meaning the name may have; or a watched write that is not its
			// newest meaning, which the server may run in its place with no
			// Begin, so no End (an enforced session refuses such an Execute:
			// see executeBlock).
			closedPoint := ""
			if !read {
				closedPoint = unreadable(body)
			}
			s.unseen(u, closedPoint)
		}
		if !ok || b.unread {
			// It may run anything, or a statement Stet does not follow.
			s.markDirty(u)
			return nil, nil
		}
		var under *readUnder
		if s.enforce && b.st.Watch {
			at := b.read(others)
			under = &at
		}
		if len(others) > 0 {
			b.cols = nil // it may run another meaning of the name, whose columns are not these
		}
		r := s.statement(u, idx, pos, 'E', b.st, &b, under, flush)
		switch {
		case mayRunUnclean(b, others):
			s.markDirty(u) // the server may run another meaning of the name
		case len(b.others) > 0 || len(others) > 0:
			s.markResolution(u) // another meaning, clean as it is, may be a SET of search_path
		}
		if s.enforce && anyOther(b, others, func(st Statement, unread bool) bool { return unread || st.Sets }) {
			s.markSets(u) // the meaning it runs may hold a SET or RESET
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
		var m pgproto3.Describe
		read := body != nil && m.Decode(body) == nil
		s.mu.Lock()
		u, pos := s.count()
		if read {
			s.describe(u, pos, m.ObjectType, m.Name)
		}
		s.mu.Unlock()

	case 'H':
		s.open()

	case 'S':
		s.mu.Lock()
		u := s.openLocked()
		u.closed, u.synced = true, true
		s.mu.Unlock()

	case 'F':
		u := s.push(&unit{closed: true, dirty: true, notReadOnly: true, resolves: true}) // the function may change anything
		if s.enforce {
			return s.block(u, 0, 'F', "", BlockFunctionCall), nil
		}
		s.unseen(u, BlockFunctionCall)
	}
	return nil, nil
}

// copyFromStdin reports whether a Parse of st is refused as copy_extended on
// an enforced session: its SQL holds COPY ... FROM STDIN, and it is not a
// watched write. A COPY into a gated table stays the watched write it was:
// Begin judges it at its Execute (unsupported), and refuses it under the
// table's unsupported: reject; one let through ends the session when the
// copy starts (session.copyIn), as one the classifier did not see does.
func copyFromStdin(st Statement) bool { return st.CopyIn && !st.Watch }

// serverHook, set only by tests (SetServerHook), runs with the type of each
// server message a session follows, with s.mu held.
var serverHook func(t byte)

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
// place. The gate gives the client's error through Blocked, and logs it the
// first time the session is refused for reason (firstTime); no decision
// records the message, so nothing waits for its End. The gate is called
// without s.mu held, and nothing queued is flushed first: Blocked does not
// block.
func (s *session) block(u *unit, pos int, t byte, name, reason string) []byte {
	s.mu.Lock()
	info := s.identity()
	first := s.firstTime(reason)
	s.mu.Unlock()
	return s.refuse(u, pos, t, name, s.gate.Blocked(info, reason, first))
}

// unseen tells the gate that this session, which does not enforce, is
// sending a message arriving in u whose writes the proxy cannot see: through
// Unseen every time, and, when reason is not "", through WouldBlock the first
// time for reason, that an enforced session would refuse it for reason, a
// closed point the proxy knows at no extra cost (ruling 31). It counts the
// call in u, so that the TxEnded paid once the transaction the message runs
// in has ended settles it (see unit.owes, carry). The message is forwarded
// as it came; nothing waits but the gate's calls, which do not block. It runs
// without s.mu held.
func (s *session) unseen(u *unit, reason string) {
	s.mu.Lock()
	// Counted before the gate is called, and read only when u leaves: the
	// server cannot answer u before this message is forwarded, which waits
	// for Unseen to return. So the TxEnded that settles the call always comes
	// after it, and no lock needs to span the call.
	u.unseen++
	first := reason != "" && s.firstTime(reason)
	info := s.identity()
	s.mu.Unlock()
	if first {
		s.gate.WouldBlock(info, reason)
	}
	s.gate.Unseen(info)
}

// hidesWrites reports whether a message st stands for may write where the
// proxy cannot see it, besides a message it could not read: the gate marked
// it with Block, or it runs a prepared statement by name, which may be a
// prepared write (spec correction S7, which names SQL EXECUTE; a FETCH or
// MOVE of a portal, Statement.Portals, alone is not counted).
func hidesWrites(st Statement) bool { return st.Block != "" || len(st.Executes) > 0 }

// firstTime reports whether the gate was not yet told of reason for this
// session, and notes that it now is. It runs with s.mu held.
func (s *session) firstTime(reason string) bool {
	if slices.Contains(s.told, reason) {
		return false
	}
	s.told = append(s.told, reason)
	return true
}

// statement follows st, arriving as a Query (idx and pos 0, b nil) or as
// Execute idx at pos of u of the portal b (its bind values, running row count
// and what a tracked read takes keys with), and, for a watched write an
// Execute runs on an enforced session, what its statement and portal were
// read under (nil otherwise: a Query is read where it runs). It returns the
// message to send in its place when the gate refused it, and nil to forward
// it.
//
// A refused statement never runs, so it leaves on_behalf_of and the cursor
// table as they were. It still marks u: its replacement was sent and fails,
// and the server's answer settles the rest.
func (s *session) statement(u *unit, idx, pos int, t byte, st Statement, b *bound, under *readUnder, flush func() error) []byte {
	if st.Watch {
		var ps []Param
		if b != nil {
			ps = b.params
		}
		if r := s.begin(u, idx, pos, t, st, ps, under, flush); r != nil {
			s.dirties(u, st)
			return r
		}
	}
	s.apply(st)
	switch {
	case len(st.Cursors) > 0: // for a watched write too: a write among cursor statements
		s.applyCursors(u, idx, st)
	case st.Observe && !st.Watch:
		s.observe(u, idx, st, b)
	}
	s.dirties(u, st) // after observe: a read's own message does not count against it
	return nil
}

func (s *session) apply(st Statement) {
	if st.OnBehalfOf != nil {
		s.info.OnBehalfOf = *st.OnBehalfOf
	}
}

// dirties marks u when the message st stands for, sent as part of u, is not
// clean: the transaction it runs in is dirty from then on; and, apart from
// that, when it is not read-only (see readOnlySoFar), and when it moves the
// resolution (movesResolution). A watched write is neither clean nor
// read-only; it marks u only here, after Begin was told whether the
// transaction was clean when it arrived. On an enforced session it also
// marks u when st holds a SET or RESET (see setPending), likewise after
// Begin was told of one in the write's own message.
func (s *session) dirties(u *unit, st Statement) {
	if dirty, written, moves := unclean(st), !readOnly(st), movesResolution(st); dirty || written || moves {
		s.mu.Lock()
		u.dirty = u.dirty || dirty
		u.notReadOnly = u.notReadOnly || written
		if moves {
			s.moveResolution(u)
		}
		s.mu.Unlock()
	}
	if s.enforce && st.Sets {
		s.markSets(u)
	}
}

// unclean reports whether st leaves the transaction it runs in dirty.
func unclean(st Statement) bool { return st.Watch || !st.Clean }

// readOnly reports whether sending st leaves later reads of its transaction
// comparable (Statement.ReadOnly). A watched write never does.
func readOnly(st Statement) bool { return !st.Watch && st.ReadOnly }

// markDirty notes that u holds a message that is not clean, nor read-only,
// and may move the resolution, with no statement to tell otherwise: one the
// proxy could not read, one that may run anything or another meaning of a
// name.
func (s *session) markDirty(u *unit) {
	s.mu.Lock()
	u.dirty, u.notReadOnly = true, true
	s.moveResolution(u)
	s.mu.Unlock()
}

// movesResolution reports whether the message st stands for may change which
// table an unqualified name means (Statement.Resolves), or runs what the
// proxy cannot see: SQL it could not inspect (Block) or a statement prepared
// in SQL (Executes).
func movesResolution(st Statement) bool {
	return st.Resolves || st.Block != "" || len(st.Executes) > 0
}

// markResolution is moveResolution, taking s.mu.
func (s *session) markResolution(u *unit) {
	s.mu.Lock()
	s.moveResolution(u)
	s.mu.Unlock()
}

// moveResolution notes that a message arriving in u may change which table
// an unqualified name means: key columns a Describe gave before it do not
// hold for an Execute after it (see resultCols). It runs with s.mu held.
func (s *session) moveResolution(u *unit) {
	s.resolution++
	u.resolves = true
}

// markSets notes that u, on an enforced session, may hold a SET or RESET.
func (s *session) markSets(u *unit) {
	s.mu.Lock()
	u.sets = true
	s.mu.Unlock()
}

// carry moves the marks of a's unit, which is no longer waiting, to the
// session, given the transaction status after it (see leave). Whether it
// left the transaction dirty, or not read-only, or moved the resolution,
// counts only while a transaction stays open. What it owes the gate, and the
// Unseen calls made for it, always pass on: the next idle ReadyForQuery hands
// them over in one TxEnded, the one answering the unit when after is idle. It
// runs with s.mu held.
func (s *session) carry(a answer, after byte) {
	if after != 'I' {
		s.dirty = s.dirty || a.u.dirty
		s.notReadOnly = s.notReadOnly || a.u.notReadOnly
		s.resolving = s.resolving || a.u.resolves
	}
	s.owed = s.owed || a.u.owes(a.inTx)
	s.unseenCalls += a.u.unseen
}

// owes reports whether u owes the gate a TxEnded once the transaction it ran
// in has ended: Unseen was called for a message of u, or u sent a watched
// write, one Begin did not refuse, that ran inside a transaction (inTx, its
// Outcome.InTransaction). A write that ran on its own committed or failed at
// its own ReadyForQuery, and a refused one never ran.
func (u *unit) owes(inTx bool) bool {
	return u.unseen > 0 || inTx && slices.ContainsFunc(u.writes, func(w watchedWrite) bool { return !w.refused })
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

// readOnlySoFar reports whether every message sent since the server last
// reported the session idle was read-only (Statement.ReadOnly), in a unit
// answered since then or in one still waiting, as txDirty does for clean
// ones. A read sent after them alone sees no row version Stet's own
// connection cannot (spec 4); after any other, even one the server has not
// answered or one earlier under the same Sync, its keys are not taken. It
// runs with s.mu held.
func (s *session) readOnlySoFar() bool {
	return !s.notReadOnly && !slices.ContainsFunc(s.units, func(u *unit) bool { return u.notReadOnly })
}

// setPending reports whether a SET or RESET was sent that no ReadyForQuery
// has answered yet: in a unit still waiting, or in an unsynced unit the
// server answered with none (setsCarried). The server reports what that
// changed of the session's settings only just before the ReadyForQuery that
// answers it (spec correction B2). Enforced sessions only; it runs with s.mu
// held.
func (s *session) setPending() bool {
	return s.setsCarried || slices.ContainsFunc(s.units, func(u *unit) bool { return u.sets })
}

// readNow is what a Parse or Bind arriving now is read under (see readUnder).
// It runs with s.mu held, once the message has joined its unit, so a SET
// earlier in the unit counts.
func (s *session) readNow() readUnder {
	return readUnder{changes: s.changes, pending: s.setPending()}
}

// readOtherwise reports whether what was read under r may have been read
// under other settings than the ones the server reported last: one of them
// changed since, or a SET or RESET was not answered then. It runs with s.mu
// held.
func (s *session) readOtherwise(r readUnder) bool {
	return r.pending || r.changes != s.changes
}

// begin hands a watched write, arriving as message t at pos of u and read
// under under (see statement), to the gate. Messages already queued go out
// first so they are not held back while the gate reads state. When the gate
// refuses the write, begin returns the message to send in its place.
func (s *session) begin(u *unit, idx, pos int, t byte, st Statement, ps []Param, under *readUnder, flush func() error) []byte {
	flush()
	s.mu.Lock()
	info := s.identity()
	if under != nil && s.readOtherwise(*under) {
		// Its statement or portal was read under settings other than those
		// handed over. Compared under the lock that took them.
		info.SetPending = true
	}
	s.mu.Unlock()
	if st.OnBehalfOf != nil {
		info.OnBehalfOf = *st.OnBehalfOf // a SET in the write's own message counts for it
	}
	if s.enforce && st.Sets {
		info.SetPending = true // so does any SET or RESET there, for what the server reports
	}
	h, refuse := s.beginGate(info, st, ps)
	w := watchedWrite{handle: h, idx: idx, several: st.Several}
	var r []byte
	if refuse != nil {
		r = s.refuse(u, pos, t, "", *refuse)
		w.refused, w.pos = true, pos
	}
	s.mu.Lock()
	u.writes = append(u.writes, w)
	s.unrecorded.Store(nil) // u holds it now
	s.mu.Unlock()
	return r
}

// beginGate calls the gate's Begin, counted in s.gating while it runs, and
// keeps what Begin handed back in s.unrecorded until begin records the write
// in its unit. When a side that panicked left s.mu held, begin parks on it
// before recording the write, which was not forwarded; abandon reports it
// from there (see relay).
func (s *session) beginGate(info SessionInfo, st Statement, ps []Param) (any, *Refusal) {
	s.gating.Add(1)
	defer s.gating.Add(-1)
	h, r := s.gate.Begin(info, st, ps)
	s.unrecorded.Store(&unrecordedWrite{handle: h, refusal: r})
	return h, r
}

// stuck reports whether a side of the session may wait forever: s.mu is
// held and no side is in the gate's Begin, which ends on its own bounds. The
// lock is never held across I/O or a gate call, so once relay has closed
// both connections, a lock it finds held at two looks a tick apart can only
// be one a side that panicked left held, and a side waiting for it never
// stops (see relay).
func (s *session) stuck() bool {
	if s.gating.Load() > 0 {
		return false
	}
	if s.mu.TryLock() {
		s.mu.Unlock()
		return false
	}
	return true
}

// observe notes a read in its unit. Nothing waits for the gate and nothing
// queued is flushed first: the gate hears of the read once it was answered.
// b is the portal it reads, or nil for a Query.
//
// A tracked read (see tracks) sent after read-only messages alone may take
// the keys of the rows it sends (spec 4): a Query, from its own
// RowDescriptions; an Execute, from what a Describe the server answered said
// of its statement (b.cols, nil when its portal may run another meaning).
// Its unit then counts in s.tracking until it is answered.
func (s *session) observe(u *unit, idx int, st Statement, b *bound) {
	var rows *int64
	if b != nil {
		rows = b.rows
	}
	s.mu.Lock()
	r := s.arriving(st, idx, rows)
	r.gen = s.resolution
	if tracks(st) && s.readOnlySoFar() {
		switch {
		case b == nil:
			r.track, r.keys = true, &ReadKeys{}
		case b.cols != nil:
			r.track, r.cols, r.formats, r.param = true, b.cols, b.formats, b.key
		}
		if r.track && !u.tracked {
			u.tracked = true
			s.tracking.Add(1)
		}
	}
	u.reads = append(u.reads, r)
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
// has not marked its unit yet. An enforced session also passes on the
// settings the server last reported, and whether a SET or RESET it has not
// answered may have changed them (setPending). It runs with s.mu held.
func (s *session) identity() SessionInfo {
	info := s.info
	if !s.ready {
		info.User = ""
	}
	info.TxOpen = s.status != 'I'
	info.TxDirty = s.txDirty()
	if s.enforce {
		info.TimeZone, info.DateStyle, info.IntervalStyle = s.timeZone, s.dateStyle, s.intervalStyle
		info.SetPending = s.setPending()
	}
	return info
}

// push adds u, the unit of a Query or FunctionCall arriving now. An
// extended-protocol unit still collecting messages ends here, unsynced, and
// u runs in its implicit transaction.
func (s *session) push(u *unit) *unit {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.resolves {
		s.resolution++ // a FunctionCall's: what it runs is not seen
	}
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
// does something beyond leaving the transaction dirty and not read-only
// (moving the resolution among it: Resolves), runs a name (see runsWatched),
// or hides its writes (Block, which only a session that does not enforce
// lets into its table: see unseen); one left empty dirties it and is not
// read-only. Only a watched statement keeps the bind values, for Begin.
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
	case p.st.Clean || p.st.ReadOnly || p.st.Sets || p.st.Resolves || p.st.OnBehalfOf != nil || len(p.st.Cursors) > 0 || runsNames(p.st) || p.st.Block != "":
		b.st = p.st
	}
	return b
}

// fromServer follows a message from the server before it is forwarded, and
// returns the message to send in its place: the refusal, for the error that
// answers a refused message's replacement.
func (s *session) fromServer(t byte, body []byte) ([]byte, error) {
	s.mu.Lock()
	if serverHook != nil {
		serverHook(t)
	}
	done := s.answeredUnsynced(nil)
	var head *unit
	if len(s.units) > 0 {
		head = s.units[0]
	}
	var out []byte
	var err error
	var ended bool       // the gate is owed a TxEnded (see readyForQuery)
	var unseen int       // the Unseen calls it settles
	var info SessionInfo // the session as the gate is told of it then
	switch t {
	case 'C', 'I', 's':
		if s.take.state == takeUndecided {
			s.decideRows(head) // an answer that sent no row: its read's Read, if any
		}
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
		if done, ended, unseen = s.readyForQuery(head, body, done); ended {
			info = s.identity()
		}
	case 'K':
		s.backendKey(body)
	case 'S': // enforced sessions only
		err = s.parameter(head, body)
	case 'G', 'W': // enforced sessions only
		err = s.copyIn(head)
	}
	s.mu.Unlock()
	s.finishAll(done)
	if ended {
		s.reportTxEnded(info, unseen) // after the End calls of the units it answers
	}
	return out, err
}

// backendKey notes the session's server process from the body of the
// server's BackendKeyData, which it sends once, before its first
// ReadyForQuery: the gate records it so that what a write changed can be
// linked to its decision (spec-v0.2-undo 5). The body starts with the
// process id (Int32), followed by the secret key; the message is forwarded
// as it came, in either mode (rule 6). A body too short to hold an id leaves
// the pid unknown. It runs on the server side with s.mu held.
func (s *session) backendKey(body []byte) {
	if len(body) >= 4 {
		s.info.BackendPID = binary.BigEndian.Uint32(body)
	}
}

// readyForQuery follows a ReadyForQuery with body body, which answers head
// and, when head is an unsynced unit that failed, the units the server
// skipped behind it (see unit). It appends them to done. ended reports that
// the gate is owed a TxEnded now: the server reports the session idle, and a
// unit answered since the last TxEnded, those it answers included, owes one
// (unit.owes); unseen is how many Unseen calls were made for those units,
// which that TxEnded settles. A unit still waiting keeps its own, so an idle
// answer to an earlier unit neither pays for nor settles a later one. An idle
// answer after a transaction in which the resolution moved counts in
// s.reverts. It runs on the server side with s.mu held.
func (s *session) readyForQuery(head *unit, body []byte, done []answer) (_ []answer, ended bool, unseen int) {
	s.settle(head) // normally nothing is left; never carry rows into the next unit
	status := byte('I')
	if len(body) > 0 {
		status = body[0]
	}
	before := s.status
	s.status = status
	s.setsCarried = false // the server reported what the unsynced units changed
	if status == 'I' {
		// No transaction is open: what was answered before cannot make the
		// next one dirty, or not read-only. The units still waiting keep theirs.
		s.dirty, s.notReadOnly = false, false
	}
	if !s.ready {
		s.ready = true
		return done, false, 0
	}
	moved := false // a unit it answers moved the resolution
	if head != nil {
		n := 1
		if k, synced := skippedBehind(s.units); synced {
			skip(s.units[1:1+k], head.err)
			n += k
		}
		inTx := before != 'I' || status != 'I'
		for _, u := range s.units[:n] {
			moved = moved || u.resolves
			a := answer{u, inTx || u.executes > 1 || u.joined || u.unsynced}
			s.leave(a, status) // its marks pass to the session (carry)
			done = append(done, a)
		}
	}
	if status == 'I' {
		s.portals.forget() // the server drops every portal when a transaction ends
		ended, unseen = s.owed, s.unseenCalls
		s.owed, s.unseenCalls = false, 0
		if moved || s.resolving {
			s.reverts++ // the transaction that ended may have undone what moved it
		}
		s.resolving = false
	}
	return done, ended, unseen
}

// settle moves the row messages sent since the last completion or error to
// head, under the statement that was answering: the Execute after its last
// completion, or the Query as a whole. A unit without observed reads drops
// them. The keys taken from them go to their read (settleKeys). It runs on
// the server side with s.mu held, or from abandon.
func (s *session) settle(head *unit) {
	s.settleKeys(head)
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
			s.reportEnd(w.handle, u.refusedWrite(w, inTx, true))
			continue
		}
		s.reportEnd(w.handle, u.answered(w.idx, inTx))
	}
	for _, r := range u.reads {
		o := u.answered(r.idx, inTx)
		o.Sent, o.Before, o.Arrived, o.Read = u.sent[r.idx], r.before, r.at, r.keys
		if r.ifRows && o.Sent == 0 {
			continue
		}
		s.reportObserve(r.info, r.st, o)
	}
}

// reportEnd tells the gate how a watched write ended. A panic in the gate's
// End is logged and recovered, so the session's other statements are still
// reported (each has its own decision row, reservation and held write).
func (s *session) reportEnd(h any, o Outcome) {
	defer s.recoverReport("End")
	s.gate.End(h, o)
}

// reportObserve is reportEnd for an observed read.
func (s *session) reportObserve(info SessionInfo, st Statement, o Outcome) {
	defer s.recoverReport("Observe")
	s.gate.Observe(info, st, o)
}

// reportTxEnded tells the gate that what info's session sent in the units
// answered so far is committed or rolled back, settling unseen of its Unseen
// calls. Like reportEnd, it logs and recovers a panic in the gate, so the
// session goes on reporting what comes after.
func (s *session) reportTxEnded(info SessionInfo, unseen int) {
	defer s.recoverReport("TxEnded")
	s.gate.TxEnded(info, unseen)
}

// recoverReport, deferred around a report to the gate, logs a panic in it as
// an error and lets the caller go on to the next.
func (s *session) recoverReport(call string) {
	if v := recover(); v != nil {
		s.log.Error("gate panicked reporting a statement", "call", call, "session", s.info.ID,
			"login_role", s.info.User, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
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

// abandon reports the statements still waiting when the connection ended,
// and a write Begin answered that never joined its unit (see beginGate). It
// runs once relay returned: both sides have stopped, or one panicked while it
// held s.mu and the other waits for that lock for good (see relay). Either
// way s.mu can be held only by the side that panicked, and nothing else
// touches the session any more, so the statements are reported all the same,
// without the lock.
//
// The connection's end also ends any transaction it left open, so once those
// reports are made the gate is told so through TxEnded, if it is owed one:
// by a unit answered since the last TxEnded (owed), or by a unit still
// waiting (unit.owes). That TxEnded settles every Unseen call not settled
// before: those the answered units carried (unseenCalls), and those of every
// unit still waiting, the ones the server skipped included. A write that
// never reached the server owes nothing.
func (s *session) abandon() {
	locked := s.mu.TryLock()
	units := s.units
	info, owed, unseen := s.identity(), s.owed, s.unseenCalls
	// The server ends whatever transaction the connection left open, and
	// nothing more will be sent in one: the session TxEnded is told of has
	// none, whatever the last ReadyForQuery said.
	info.TxOpen, info.TxDirty = false, false
	s.units = nil
	s.cursors, s.unsettled = nil, nil
	status := s.status
	if locked {
		s.mu.Unlock() // the gate is never called with it held
	}
	for _, u := range units {
		// The rows of the statement that was still answering belong to the
		// first unit not answered in full: an unsynced unit that completed
		// settled its own. When the lock was left held, the server side may
		// be the one waiting for it, having counted rows, and taken their
		// keys, after it last let go of the lock (s.sent and s.take are
		// written without it): those are left out rather than read with
		// nothing ordering them.
		if !locked {
			break
		}
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
		owed = owed || u.owes(inTx)
		unseen += u.unseen
		for _, w := range u.writes {
			if w.refused {
				s.reportEnd(w.handle, u.refusedWrite(w, inTx, false))
				continue
			}
			s.reportEnd(w.handle, u.unansweredWrite(w, inTx))
		}
		for _, r := range u.reads {
			o := u.unanswered(r.idx, inTx)
			o.Sent, o.Before, o.Arrived, o.Read = u.sent[r.idx], r.before, r.at, r.keys
			if r.ifRows && o.Sent == 0 {
				continue
			}
			s.reportObserve(r.info, r.st, o)
		}
	}
	if w := s.unrecorded.Swap(nil); w != nil {
		// Begin answered, but the write never joined its unit: its side
		// parked on the lock a panicking side left held, inside the message
		// it was inspecting, so the write was not forwarded.
		o := Outcome{InTransaction: status != 'I', Error: "not sent: the session ended before the statement was forwarded"}
		if w.refusal != nil {
			o = Outcome{InTransaction: status != 'I', Refused: true, Undelivered: true,
				Error: "SQLSTATE " + w.refusal.Code + ": " + w.refusal.Message}
		}
		s.reportEnd(w.handle, o)
	}
	if owed {
		s.reportTxEnded(info, unseen)
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
	for i := range m.Parameters {
		ps[i] = param(m, oids, i)
	}
	return ps
}

// param is the value at index i of Bind m, copied, with its format and the
// type oids (from its Parse) gives it.
func param(m *pgproto3.Bind, oids []uint32, i int) Param {
	p := Param{Value: cloneBytes(m.Parameters[i])}
	switch n := len(m.ParameterFormatCodes); {
	case n == 1:
		p.Format = m.ParameterFormatCodes[0]
	case i < n:
		p.Format = m.ParameterFormatCodes[i]
	}
	if i < len(oids) {
		p.OID = oids[i]
	}
	return p
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

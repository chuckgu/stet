package bench

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/stetdb/stet/internal/lab/tasks"
)

// fakeHoop is a pgwire server that answers the way hoop's sidecar does in
// front of a database: it relays nothing, but on a denial it sends what hoop
// sends (an ErrorResponse of severity FATAL with SQLSTATE 42501) and hangs
// up, and it can write an audit log of hoop's shape. Set its fields before
// the first connection.
//
// Deny is asked about each statement of a simple query (split as hoop
// splits it), the text of a Parse, and a FunctionCall's OID; a COPY is
// judged by its statement, not its data. Exec gets a Query's whole text, or
// a bound statement's text with its values written in, at Execute; it is
// not called for a FunctionCall or a COPY, nor in Mode hang. With Audit,
// take the file's directory before starting the fake, so that it outlives
// the sessions' last lines.
type fakeHoop struct {
	Addr  string
	Deny  func(sql string) bool // nil: deny nothing
	Mode  string                // "answer" (default), "hang" (never answers), "drop" (closes without answering)
	Exec  func(sql string)      // called before answering or dropping; tests run the statement on a real database with it
	Audit string                // a file to append hoop-shaped audit lines to; "" writes none (Task 8)
	// Refuse is asked about each connection by its number, from 1 in the order accepted: true answers the
	// startup with FATAL 53300 and hangs up, so that connecting fails. nil refuses none.
	Refuse func(conn int) bool

	ln     net.Listener
	wg     sync.WaitGroup
	execMu sync.Mutex // one Exec at a time

	mu       sync.Mutex
	log      []seen
	conns    map[net.Conn]bool // open
	accepted int
	open     int
	maxOpen  int
	auditErr error // the first audit line that could not be written
}

// seen is one statement-carrying message the fake received.
type seen struct {
	Conn                int    // the connection, numbered from 1 in the order accepted
	User, Database, App string // from the startup message
	Kind                string // "Query", "Parse", "FunctionCall" or "CopyData"
	SQL                 string // the text; for a FunctionCall the function's OID, for CopyData the data
	Params              []string
	At                  time.Time
}

// fakeDenied is the message of the fake's denials.
const fakeDenied = "statement denied by policy"

// startFakeHoop starts a fake on a local port, closed when the test ends.
func startFakeHoop(t *testing.T) *fakeHoop {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeHoop{Addr: ln.Addr().String(), ln: ln, conns: map[net.Conn]bool{}}
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(func() {
		f.close()
		if f.auditErr != nil {
			t.Errorf("fake hoop: write the audit log: %v", f.auditErr)
		}
	})
	return f
}

// Seen returns the statement-carrying messages received so far, in order.
func (f *fakeHoop) Seen() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// MaxOpen is the most connections that were open at once. A connection the
// fake hangs up on stops counting before the client hears why.
func (f *fakeHoop) MaxOpen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxOpen
}

func (f *fakeHoop) serve() {
	defer f.wg.Done()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.accepted++
		id := f.accepted
		f.open++
		f.maxOpen = max(f.maxOpen, f.open)
		f.conns[c] = true
		f.mu.Unlock()
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.session(c, id)
		}()
	}
}

func (f *fakeHoop) close() {
	f.ln.Close()
	f.mu.Lock()
	for c := range f.conns {
		c.Close()
	}
	f.mu.Unlock()
	f.wg.Wait()
}

// fakeSession is one client connection.
type fakeSession struct {
	f                   *fakeHoop
	c                   net.Conn
	be                  *pgproto3.Backend
	id                  int
	user, database, app string

	// The fields set before the first connection, read once the client has spoken.
	deny  func(string) bool
	mode  string
	exec  func(string)
	audit string

	sessionID          string
	start              time.Time
	statements, denied int
	ended              bool // the fake hung up, or the client did
	hung               bool // Mode hang: nothing more is answered
	copying            bool

	parsed  int // index in f.log of the last Parse
	pending string
	params  []string
}

func (f *fakeHoop) session(c net.Conn, id int) {
	s := &fakeSession{f: f, c: c, be: pgproto3.NewBackend(c, c), id: id, start: time.Now(), parsed: -1}
	defer s.hangUp(nil)
	msg, err := s.be.ReceiveStartupMessage()
	if err != nil {
		return
	}
	sm, ok := msg.(*pgproto3.StartupMessage)
	if !ok {
		return
	}
	s.user, s.database, s.app = sm.Parameters["user"], sm.Parameters["database"], sm.Parameters["application_name"]
	if f.Refuse != nil && f.Refuse(id) {
		s.be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "53300",
			Message: "sorry, too many clients already"})
		s.be.Flush()
		return
	}
	s.deny, s.mode, s.exec, s.audit = f.Deny, f.Mode, f.Exec, f.Audit
	s.sessionID = randomHex()
	s.write(auditLine{Kind: "session_start", Principal: "anonymous"})

	s.be.Send(&pgproto3.AuthenticationOk{})
	s.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if s.be.Flush() != nil {
		return
	}
	for {
		msg, err := s.be.Receive()
		if err != nil {
			return
		}
		if s.hung {
			continue // read on, so that the client's hanging up is noticed
		}
		if !s.handle(msg) {
			return
		}
	}
}

// handle answers one message; false means the session is over.
func (s *fakeSession) handle(msg pgproto3.FrontendMessage) bool {
	switch m := msg.(type) {
	case *pgproto3.Query:
		return s.query(m.String)
	case *pgproto3.Parse:
		return s.parse(m.Query)
	case *pgproto3.Bind:
		params := make([]string, len(m.Parameters))
		for i, p := range m.Parameters {
			params[i] = string(p)
		}
		s.params = params
		if s.parsed >= 0 {
			s.f.mu.Lock()
			s.f.log[s.parsed].Params = params
			s.f.mu.Unlock()
		}
		s.be.Send(&pgproto3.BindComplete{})
	case *pgproto3.Describe:
		if m.ObjectType == 'S' {
			s.be.Send(&pgproto3.ParameterDescription{})
		}
		s.be.Send(&pgproto3.NoData{})
	case *pgproto3.Execute:
		if s.pending != "" {
			s.run(bindInline(s.pending, s.params))
		}
		if s.mode == "drop" {
			return false
		}
		s.be.Send(&pgproto3.CommandComplete{CommandTag: []byte("UPDATE 1")})
	case *pgproto3.Sync:
		s.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		return s.be.Flush() == nil
	case *pgproto3.Flush:
		return s.be.Flush() == nil
	case *pgproto3.Close:
		s.be.Send(&pgproto3.CloseComplete{})
	case *pgproto3.FunctionCall:
		return s.functionCall(m)
	case *pgproto3.CopyData:
		if s.copying {
			s.record("CopyData", string(m.Data), nil)
		}
	case *pgproto3.CopyDone:
		if s.copying {
			s.copying = false
			s.be.Send(&pgproto3.CommandComplete{CommandTag: []byte("COPY 300")})
			s.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			return s.be.Flush() == nil
		}
	case *pgproto3.CopyFail:
		if s.copying {
			s.copying = false
			s.be.Send(&pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "57014",
				Message: "COPY from stdin failed: " + m.Message})
			s.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			return s.be.Flush() == nil
		}
	case *pgproto3.Terminate:
		return false
	}
	return true
}

// query judges each statement of a simple query, as hoop does, and denies
// the whole message if one is denied.
func (s *fakeSession) query(sql string) bool {
	s.record("Query", sql, nil)
	var selects int
	for _, stmt := range splitStatements(sql) {
		if s.denies(stmt) {
			s.judged(stmt, "Query", true)
			return s.refuse()
		}
		s.judged(stmt, "Query", false)
		if operation(stmt) == "select" {
			selects++
		}
	}
	switch s.mode {
	case "hang":
		s.hung = true
		return true
	case "drop":
		if !isCopyIn(sql) {
			s.run(sql)
		}
		return false
	}
	if isCopyIn(sql) {
		s.copying = true
		s.be.Send(&pgproto3.CopyInResponse{})
		return s.be.Flush() == nil
	}
	s.run(sql)
	s.be.Send(&pgproto3.CommandComplete{CommandTag: []byte("UPDATE 1")})
	s.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	for range selects {
		s.write(auditLine{Kind: "statement", Operation: "unknown", Statement: "SELECT 1", Allowed: true,
			Direction: "server", Metadata: map[string]string{"pg.message": "CommandComplete"}})
		s.statements++
	}
	return s.be.Flush() == nil
}

// parse decides the denial of an extended-protocol statement.
func (s *fakeSession) parse(sql string) bool {
	s.parsed = s.record("Parse", sql, nil)
	if s.denies(sql) {
		s.judged(sql, "Parse", true)
		return s.refuse()
	}
	s.judged(sql, "Parse", false)
	if s.mode == "hang" {
		s.hung = true
		return true
	}
	s.pending, s.params = sql, nil
	s.be.Send(&pgproto3.ParseComplete{})
	return true
}

func (s *fakeSession) functionCall(m *pgproto3.FunctionCall) bool {
	oid := strconv.FormatUint(uint64(m.Function), 10)
	args := make([]string, len(m.Arguments))
	for i, a := range m.Arguments {
		args[i] = string(a)
	}
	s.record("FunctionCall", oid, args)
	if s.denies(oid) {
		s.judged(oid, "FunctionCall", true)
		return s.refuse()
	}
	s.judged(oid, "FunctionCall", false)
	switch s.mode {
	case "hang":
		s.hung = true
		return true
	case "drop":
		return false
	}
	s.be.Send(&pgproto3.FunctionCallResponse{})
	s.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	return s.be.Flush() == nil
}

func (s *fakeSession) denies(sql string) bool { return s.deny != nil && s.deny(sql) }

// run calls Exec, one call at a time across the sessions. It runs on the
// fake's goroutines: what Exec shares with the test needs its own
// synchronization.
func (s *fakeSession) run(sql string) {
	if s.exec != nil {
		s.f.execMu.Lock()
		defer s.f.execMu.Unlock()
		s.exec(sql)
	}
}

// refuse answers as hoop does when it denies: FATAL 42501, then it hangs up.
func (s *fakeSession) refuse() bool {
	s.denied++
	s.hangUp(func() {
		s.be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "42501", Message: fakeDenied})
		s.be.Flush()
	})
	return false
}

// hangUp ends the session once: the connection stops counting as open, then
// last (if any) runs, then the connection is closed and the session's end
// is written to the audit log.
func (s *fakeSession) hangUp(last func()) {
	if s.ended {
		return
	}
	s.ended = true
	s.f.mu.Lock()
	s.f.open--
	delete(s.f.conns, s.c)
	s.f.mu.Unlock()
	if last != nil {
		last()
	}
	s.c.Close()
	if s.sessionID != "" {
		n := s.statements
		s.write(auditLine{Kind: "session_end", Principal: s.user, DurationNS: time.Since(s.start).Nanoseconds(),
			StatementCount: &n, DeniedCount: s.denied})
	}
}

func (s *fakeSession) record(kind, sql string, params []string) int {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	s.f.log = append(s.f.log, seen{Conn: s.id, User: s.user, Database: s.database, App: s.app, Kind: kind,
		SQL: sql, Params: params, At: time.Now()})
	return len(s.f.log) - 1
}

// auditLine is one line of hoop's JSON-lines audit log, in its field order.
type auditLine struct {
	Kind           string            `json:"kind"`
	Timestamp      string            `json:"timestamp"`
	SessionID      string            `json:"session_id"`
	Principal      string            `json:"principal"`
	Protocol       string            `json:"protocol"`
	Connection     string            `json:"connection"`
	Operation      string            `json:"operation,omitempty"`
	Statement      string            `json:"statement,omitempty"`
	Tables         []string          `json:"tables,omitempty"`
	Allowed        bool              `json:"allowed"`
	Rule           string            `json:"rule,omitempty"`
	Message        string            `json:"message,omitempty"`
	Direction      string            `json:"direction,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	DurationNS     int64             `json:"duration_ns,omitempty"`
	StatementCount *int              `json:"statement_count,omitempty"`
	DeniedCount    int               `json:"denied_count,omitempty"`
}

// judged writes the line hoop writes for a client statement it judged: a
// statement line when it allowed it, one violation line when it denied it.
// Inserts, updates and deletes are the analyzer's (rule "crm"); other
// denials are the guardrail rule's.
func (s *fakeSession) judged(stmt, message string, denied bool) {
	op := operation(stmt)
	l := auditLine{Kind: "statement", Operation: op, Statement: stmt, Tables: tables(stmt), Allowed: !denied,
		Direction: "client", Metadata: map[string]string{"pg.message": message}}
	switch op {
	case "insert", "update", "delete":
		l.Metadata["ai_rule"], l.Metadata["ai_status"] = "crm", "ok"
		l.Metadata["risk_level"], l.Metadata["risk_action"] = "medium", "allow"
		if denied {
			l.Metadata["risk_level"], l.Metadata["risk_action"] = "high", "block"
			l.Rule = "crm"
		}
	case "select":
		l.Metadata["ai_rule"], l.Metadata["ai_status"] = "crm", "skipped"
	}
	if denied {
		l.Kind, l.Message = "violation", fakeDenied
		if l.Rule == "" {
			l.Rule = "destructive"
		}
	}
	s.statements++
	s.write(l)
}

func (s *fakeSession) write(l auditLine) {
	if s.audit == "" {
		return
	}
	l.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	l.SessionID, l.Protocol, l.Connection = s.sessionID, "postgres", "crm"
	if l.Principal == "" {
		l.Principal = s.user
	}
	b, _ := json.Marshal(l) // strings, a map of strings, numbers: cannot fail
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	file, err := os.OpenFile(s.audit, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrNotExist) {
		return // the test's directory went first: a session ended while the test was cleaning up
	}
	if err == nil {
		_, err = file.Write(append(b, '\n'))
		err = errors.Join(err, file.Close())
	}
	if err != nil && s.f.auditErr == nil {
		s.f.auditErr = err
	}
}

func randomHex() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// bindInline writes a bound statement's values into its text, so that Exec
// can run it as one string.
func bindInline(sql string, params []string) string {
	args := make([]any, len(params))
	for i, p := range params {
		args[i] = p
	}
	in, err := tasks.Inline(sql, args)
	if err != nil {
		return sql
	}
	return in
}

var (
	dollarQuote = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)?\$`)
	copyIn      = regexp.MustCompile(`(?is)^copy\b.*\bfrom\s+stdin\b`)
	tableRef    = regexp.MustCompile(`(?i)\b(?:from|into|update|join|truncate|table|copy)\s+([a-z_][a-z0-9_.]*)`)
	sqlWords    = []string{"select", "insert", "update", "delete", "truncate", "drop", "alter", "create", "grant",
		"revoke", "call", "copy"}
)

// splitStatements splits a simple query at the semicolons outside quotes,
// dollar quotes and comments, as hoop does before it judges each statement.
func splitStatements(sql string) []string {
	var out []string
	add := func(piece string) {
		if p := strings.TrimSpace(piece); p != "" {
			out = append(out, p)
		}
	}
	start := 0
	for i := 0; i < len(sql); {
		switch rest := sql[i:]; {
		case rest[0] == '\'' || rest[0] == '"':
			end := strings.IndexByte(rest[1:], rest[0])
			if end < 0 {
				i = len(sql)
			} else {
				i += end + 2
			}
		case strings.HasPrefix(rest, "--"):
			end := strings.IndexByte(rest, '\n')
			if end < 0 {
				i = len(sql)
			} else {
				i += end + 1
			}
		case strings.HasPrefix(rest, "/*"):
			end := strings.Index(rest[2:], "*/")
			if end < 0 {
				i = len(sql)
			} else {
				i += end + 4
			}
		case rest[0] == '$' && dollarQuote.MatchString(rest):
			tag := dollarQuote.FindString(rest)
			end := strings.Index(rest[len(tag):], tag)
			if end < 0 {
				i = len(sql)
			} else {
				i += len(tag) + end + len(tag)
			}
		case rest[0] == ';':
			add(sql[start:i])
			i++
			start = i
		default:
			i++
		}
	}
	add(sql[start:])
	return out
}

// bare drops a statement's leading comments.
func bare(stmt string) string {
	for {
		stmt = strings.TrimSpace(stmt)
		switch {
		case strings.HasPrefix(stmt, "/*"):
			end := strings.Index(stmt, "*/")
			if end < 0 {
				return ""
			}
			stmt = stmt[end+2:]
		case strings.HasPrefix(stmt, "--"):
			end := strings.IndexByte(stmt, '\n')
			if end < 0 {
				return ""
			}
			stmt = stmt[end+1:]
		default:
			return stmt
		}
	}
}

// operation is hoop's name for what a statement does; "unknown" for what it
// does not classify.
func operation(stmt string) string {
	word, _, _ := strings.Cut(bare(stmt), " ")
	word = strings.ToLower(strings.TrimRight(word, "(\n\t"))
	if slices.Contains(sqlWords, word) {
		return word
	}
	return "unknown"
}

func tables(stmt string) []string {
	var out []string
	for _, m := range tableRef.FindAllStringSubmatch(bare(stmt), -1) {
		if t := strings.ToLower(m[1]); !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

func isCopyIn(sql string) bool { return copyIn.MatchString(bare(sql)) }

// TestFakeHoopAuditHasHoopsShape checks the fake's audit log against what a
// real run of hoop 1.310.0 wrote: one violation line per denied statement,
// one statement line per allowed one (each statement of a multi-statement
// query judged apart), server lines for completed selects, and session
// lines, with no key the real log lacks.
func TestFakeHoopAuditHasHoopsShape(t *testing.T) {
	path := t.TempDir() + "/audit.jsonl" // before the fake: its directory outlives the fake's last lines
	f := startFakeHoop(t)
	f.Audit = path
	f.Deny = func(sql string) bool { return strings.Contains(sql, "deny") }
	stmts := []tasks.Stmt{
		testStmt(1, "update contacts set notes = 'ok' where id = 1"),
		testStmt(2, "update contacts set notes = 'deny' where id = 2"),
		testStmt(3, "update contacts set notes = 'ok' where id = 3; select 1"),
		testStmt(4, "truncate deny_log"),
		testStmt(5, "do $$ begin update contacts set notes = 'a;b' where id = 5; end $$"),
	}
	n := freshNames()
	got := sendAll(t, Target{Addr: f.Addr, Database: "crm_fake", Names: n}, singles(stmts...), fastOptions(1))
	if got["w-2"].Result != Blocked || got["w-4"].Result != Blocked || got["w-1"].Result != Executed {
		t.Fatalf("outcomes %+v", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"kind", "timestamp", "session_id", "principal", "protocol", "connection", "operation", "statement",
		"tables", "allowed", "rule", "message", "direction", "metadata", "duration_ns", "statement_count", "denied_count"}
	type line struct {
		Kind, Principal, Operation, Statement, Rule, Message, Direction string
		Allowed                                                         bool
		Metadata                                                        map[string]string
	}
	var lines []line
	for _, text := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(text), &m); err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		for k := range m {
			if !slices.Contains(keys, k) {
				t.Errorf("key %q is not in hoop's audit log: %s", k, text)
			}
		}
		var l line
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	of := func(s tasks.Stmt) []line {
		var out []line
		for _, l := range lines {
			if strings.HasPrefix(l.Statement, "/* w:"+s.Tag+" */ ") {
				out = append(out, l)
			}
		}
		return out
	}
	agent := n.Role(tasks.RoleV)
	if l := of(stmts[0]); len(l) != 1 || l[0].Kind != "statement" || !l[0].Allowed || l[0].Direction != "client" ||
		l[0].Principal != agent || l[0].Metadata["risk_action"] != "allow" || l[0].Metadata["ai_status"] != "ok" {
		t.Errorf("an allowed update: %+v", l)
	}
	if l := of(stmts[1]); len(l) != 1 || l[0].Kind != "violation" || l[0].Allowed || l[0].Rule != "crm" ||
		l[0].Message != fakeDenied || l[0].Metadata["risk_action"] != "block" {
		t.Errorf("a denied update: %+v", l)
	}
	if l := of(stmts[2]); len(l) != 1 || l[0].Statement != tasks.Tagged(stmts[2].Tag, "update contacts set notes = 'ok' where id = 3") {
		t.Errorf("the update of a multi-statement query: %+v", l)
	}
	if l := of(stmts[3]); len(l) != 1 || l[0].Kind != "violation" || l[0].Rule != "destructive" || l[0].Operation != "truncate" {
		t.Errorf("a denied truncate: %+v", l)
	}
	if l := of(stmts[4]); len(l) != 1 || l[0].Statement != tasks.Tagged(stmts[4].Tag, stmts[4].Inline) || l[0].Operation != "unknown" {
		t.Errorf("a DO block, one statement with its semicolons: %+v", l)
	}
	count := map[string]int{}
	for _, l := range lines {
		switch {
		case l.Statement == "select 1" && l.Direction == "client":
			count["client select"]++
		case l.Statement == "SELECT 1" && l.Direction == "server" && l.Metadata["pg.message"] == "CommandComplete":
			count["server select"]++
		case l.Kind == "session_start" && l.Principal == "anonymous":
			count["session_start"]++
		}
	}
	if count["client select"] != 1 || count["server select"] != 1 || count["session_start"] < 3 {
		t.Errorf("lines: %v, want one client select, one server select, a session_start per connection", count)
	}
}

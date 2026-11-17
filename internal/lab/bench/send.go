package bench

// This file sends the stream to an arm and classifies what came back
// (docs/spec-v0.2-benchmark.md, chapter 4 "보내는 방식" and chapter 7).

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stetdb/stet/internal/lab/tasks"
)

// The result of one write, as the client saw it (lab.outcomes.result).
const (
	Executed   = "executed"    // a completion arrived; Rows is what its tag says
	Blocked    = "blocked"     // SQLSTATE begins with ST, or severity FATAL with 42501
	DBRejected = "db_rejected" // any other server error
	Unknown    = "unknown"     // no answer: the connection ended, or the 120s ran out
)

// Outcome is one row of lab.outcomes.
type Outcome struct {
	Arm, ID, Tag, Set           string
	SentAt                      time.Time
	LatencyMS                   float64 // from sending the statement to its answer; no waiting for the rate or a connection
	Result                      string
	Rows                        *int64
	SQLState, Severity, Message string
}

// Classify turns the answer to one statement into the result, rows and error fields of an Outcome.
func Classify(tag pgconn.CommandTag, err error) Outcome {
	if err == nil {
		rows := tag.RowsAffected()
		return Outcome{Result: Executed, Rows: &rows}
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return Outcome{Result: Unknown, Message: err.Error()}
	}
	o := Outcome{Result: DBRejected, SQLState: pe.Code, Severity: pe.Severity, Message: pe.Message}
	// hoop denies with FATAL 42501 and hangs up; the database's own
	// permission error is ERROR 42501 and leaves the session open.
	if strings.HasPrefix(pe.Code, "ST") || (pe.Severity == "FATAL" && pe.Code == "42501") {
		o.Result = Blocked
	}
	return o
}

// Target is where an arm's statements go.
type Target struct {
	Addr     string // host:port the statements go to: a proxy, or Postgres itself for the none arm
	Database string
	Names    Names
}

// Options say how statements are sent.
type Options struct {
	Arm       string
	Bind      bool                // send SQL with Args as parameters; otherwise Inline as a simple query
	Conns     int                 // 4
	PerSecond float64             // 5
	Timeout   time.Duration       // 120s, per statement
	Sleep     func(time.Duration) // time.Sleep; tests replace it (Task 6)
	MaxPad    int                 // tests: caps Stmt.Pad; 0 is no cap (Task 6)
}

// DefaultOptions are the options every arm sends with (spec 4): four
// connections, five statements a second, 120 seconds per statement, and
// bound values for the arms that bind.
func DefaultOptions(arm string) Options {
	bind, _, _, _ := ArmPlan(arm, false)
	return Options{Arm: arm, Bind: bind, Conns: 4, PerSecond: 5, Timeout: 120 * time.Second, Sleep: time.Sleep}
}

// ArmPlan says how an arm sends: none, stet, hoop: simple, whole stream, sets V I X.
// hoop-cache: simple, whole, V I. hoop-bind, stet-bind: bind, the part, V I. hoop-opus: simple, the part, V I.
// The practice arms none, stet, hoop-a, hoop-b, hoop-c: simple, whole, V I.
func ArmPlan(arm string, practice bool) (bind, part bool, sets []string, err error) {
	vi := []string{tasks.SetV, tasks.SetI}
	if practice {
		switch arm {
		case "none", "stet", "hoop-a", "hoop-b", "hoop-c":
			return false, false, vi, nil
		}
		return false, false, nil, fmt.Errorf("arm %q is not a practice arm (none, stet, hoop-a, hoop-b, hoop-c)", arm)
	}
	switch arm {
	case "none", "stet", "hoop":
		return false, false, []string{tasks.SetV, tasks.SetI, tasks.SetX}, nil
	case "hoop-cache":
		return false, false, vi, nil
	case "hoop-bind", "stet-bind":
		return true, true, vi, nil
	case "hoop-opus":
		return false, true, vi, nil
	}
	return false, false, nil, fmt.Errorf("arm %q is not an arm of the benchmark (none, stet, hoop, hoop-cache, hoop-bind, stet-bind, hoop-opus)", arm)
}

// Phases orders the stream for sending: for V, per batch a phase of its prep statements and then one of its
// V statements; for I one phase. A phase is a list of units; a unit is sent over one connection, in order.
// Statements sharing a non-empty Unit form one unit; any other statement is a unit of its own. With part,
// only Sub statements are kept. The X set is not phased here: its attacks are sent their own way.
func Phases(stream []tasks.Stmt, sets []string, part bool) [][][]tasks.Stmt {
	phase := func(keep func(tasks.Stmt) bool) [][]tasks.Stmt {
		var units [][]tasks.Stmt
		at := map[string]int{}
		for _, s := range stream {
			if !keep(s) || part && !s.Sub {
				continue
			}
			if s.Unit == "" {
				units = append(units, []tasks.Stmt{s})
				continue
			}
			if i, ok := at[s.Unit]; ok {
				units[i] = append(units[i], s)
				continue
			}
			at[s.Unit] = len(units)
			units = append(units, []tasks.Stmt{s})
		}
		return units
	}
	var phases [][][]tasks.Stmt
	add := func(units [][]tasks.Stmt) {
		if len(units) > 0 {
			phases = append(phases, units)
		}
	}
	if slices.Contains(sets, tasks.SetV) {
		batches := map[int]bool{}
		for _, s := range stream {
			if s.Set == tasks.SetPrep || s.Set == tasks.SetV {
				batches[s.Batch] = true
			}
		}
		for _, b := range slices.Sorted(maps.Keys(batches)) {
			add(phase(func(s tasks.Stmt) bool { return s.Set == tasks.SetPrep && s.Batch == b }))
			add(phase(func(s tasks.Stmt) bool { return s.Set == tasks.SetV && s.Batch == b }))
		}
	}
	if slices.Contains(sets, tasks.SetI) {
		add(phase(func(s tasks.Stmt) bool { return s.Set == tasks.SetI }))
	}
	return phases
}

// Send sends the phases one after another, each over o.Conns connections, and reports every statement to save.
// The connections' workers take a phase's units in order; before each statement a worker waits for the
// shared rate, and after any statement that did not execute it closes its connection and connects anew for
// the next. save is called by one worker at a time. Send stops at the first error save returns, or when ctx
// ends: no statement starts after that, and one already sent finishes, within o.Timeout, and is reported as
// the far side answered it.
func Send(ctx context.Context, t Target, phases [][][]tasks.Stmt, o Options, save func(Outcome) error) error {
	if o.Conns < 1 {
		return fmt.Errorf("send: %d connections; want at least 1", o.Conns)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	lim := newLimiter(o.PerSecond)
	workers := make([]worker, o.Conns)
	defer func() {
		for i := range workers {
			workers[i].hangUp()
		}
	}()
	var saving sync.Mutex
	report := func(out Outcome) error {
		saving.Lock()
		defer saving.Unlock()
		return save(out)
	}
	exec := func(ctx context.Context, c *pgconn.PgConn, s tasks.Stmt) (pgconn.CommandTag, error) {
		return sendOne(ctx, c, s, o.Bind, o)
	}

	for _, units := range phases {
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			next int
		)
		take := func() ([]tasks.Stmt, bool) {
			mu.Lock()
			defer mu.Unlock()
			if next >= len(units) {
				return nil, false
			}
			next++
			return units[next-1], true
		}
		for i := range workers {
			w := &workers[i]
			wg.Go(func() {
				for unit, ok := take(); ok && ctx.Err() == nil; unit, ok = take() {
					for _, s := range unit {
						out, err := w.send(ctx, t, s, o, lim, exec)
						if err == nil {
							err = report(out)
						}
						if err != nil {
							cancel(err)
							return
						}
					}
				}
			})
		}
		wg.Wait()
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
	}
	return nil
}

// worker is one of Send's connections.
type worker struct {
	conn      *pgconn.PgConn
	role, app string
}

// execFunc sends s over c and returns the tag of its last result. ctx is not
// cancelled by the tool; any bound on the statement is the function's own.
type execFunc func(ctx context.Context, c *pgconn.PgConn, s tasks.Stmt) (pgconn.CommandTag, error)

// send sends one statement over the worker's connection with exec,
// connecting first when it has none or one of another role or application,
// and then waiting for lim unless it is nil. It returns an error only when
// ctx ended before the statement was sent.
func (w *worker) send(ctx context.Context, t Target, s tasks.Stmt, o Options, lim *limiter, exec execFunc) (Outcome, error) {
	if w.conn == nil || w.conn.IsClosed() || w.role != s.Role || w.app != s.App {
		w.hangUp()
		c, err := connect(ctx, t, s.Role, s.App)
		if err != nil {
			if ctx.Err() != nil {
				return Outcome{}, ctx.Err()
			}
			// Not classified: a server that refuses the session (FATAL
			// 42501 for a missing CONNECT right, say) refused no statement.
			return Outcome{Arm: o.Arm, ID: s.ID, Tag: s.Tag, Set: s.Set, SentAt: time.Now(), Result: Unknown,
				Message: err.Error()}, nil
		}
		w.conn, w.role, w.app = c, s.Role, s.App
	}
	if lim != nil {
		if err := lim.wait(ctx); err != nil {
			return Outcome{}, err
		}
	}
	start := time.Now()
	// A statement that has gone out finishes, within o.Timeout, even when the
	// tool cancels: an interrupt, or another worker's failed save, is not the
	// far side's silence, and unknown is kept for that.
	tag, err := exec(context.WithoutCancel(ctx), w.conn, s)
	latency := time.Since(start)
	out := Classify(tag, err)
	out.Arm, out.ID, out.Tag, out.Set = o.Arm, s.ID, s.Tag, s.Set
	out.SentAt, out.LatencyMS = start, float64(latency.Nanoseconds())/1e6
	if out.Result != Executed {
		w.hangUp() // hoop hangs up on every denial; any other error gets a fresh session too
	}
	return out, nil
}

func (w *worker) hangUp() {
	if w.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	w.conn.Close(ctx)
	w.conn = nil
}

// How long connect keeps trying, and how often. Tests shorten them.
var (
	connectWindow = 30 * time.Second
	connectEvery  = time.Second
)

// connect connects to t as the agent role role (its canonical name) with
// application_name app, trying again every connectEvery for connectWindow.
// Its error starts with "connect: ".
func connect(ctx context.Context, t Target, role, app string) (*pgconn.PgConn, error) {
	ctx, cancel := context.WithTimeout(ctx, connectWindow)
	defer cancel()
	var last error
	for {
		c, err := dial(ctx, t, role, app)
		if err == nil {
			return c, nil
		}
		if last == nil || ctx.Err() == nil {
			last = err // an attempt the window cut short says less than the one before it
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect: %w", last)
		case <-time.After(connectEvery):
		}
	}
}

// dial is one attempt of connect.
func dial(ctx context.Context, t Target, role, app string) (*pgconn.PgConn, error) {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(t.Names.Role(role), AgentPassword), Host: t.Addr,
		Path: "/" + t.Database, RawQuery: url.Values{"sslmode": {"disable"}, "application_name": {app}}.Encode()}
	return pgconn.Connect(ctx, u.String())
}

// tagged puts the statement's tag in front of sql; no tag, no comment.
func tagged(tag, sql string) string {
	if tag == "" {
		return sql
	}
	return tasks.Tagged(tag, sql)
}

// sendOne sends s over c, within o.Timeout: SQL with Args bound as text when
// bind, otherwise Inline as a simple query; s.Wire, when simple or bind,
// overrides bind, and a copy wire sends SQL followed by CopyData. A text with
// Pad > 0 has its @PAD@ written out first (at most o.MaxPad x's when that is
// set). It returns the tag of the last result.
func sendOne(ctx context.Context, c *pgconn.PgConn, s tasks.Stmt, bind bool, o Options) (pgconn.CommandTag, error) {
	switch s.Wire {
	case tasks.WireSimple:
		bind = false
	case tasks.WireBind:
		bind = true
	case "", tasks.WireCopy:
	default:
		return pgconn.CommandTag{}, fmt.Errorf("statement %s: wire %q is not sent this way", s.ID, s.Wire)
	}
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	if s.Wire == tasks.WireCopy {
		return c.CopyFrom(ctx, strings.NewReader(s.CopyData), tagged(s.Tag, s.SQL))
	}
	text := s.Inline
	if bind {
		text = s.SQL
	}
	if s.Pad > 0 {
		n := s.Pad
		if o.MaxPad > 0 {
			n = min(n, o.MaxPad)
		}
		text = padded(s.Tag, text, n)
	} else {
		text = tagged(s.Tag, text)
	}
	if bind {
		params := make([][]byte, len(s.Args))
		for i, a := range s.Args {
			params[i] = []byte(a)
		}
		r := c.ExecParams(ctx, text, params, nil, nil, nil).Read()
		return r.CommandTag, r.Err
	}
	results, err := c.Exec(ctx, text).ReadAll()
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	if len(results) == 0 {
		return pgconn.CommandTag{}, nil
	}
	last := results[len(results)-1]
	return last.CommandTag, last.Err
}

const padToken = "@PAD@"

// padded returns sql with its tag in front and every @PAD@ written as n x's.
// The text is built in one piece, so that a 64 MiB message is held once
// rather than once per step of its making.
func padded(tag, sql string, n int) string {
	prefix := tagged(tag, "")
	var b strings.Builder
	b.Grow(len(prefix) + len(sql) + strings.Count(sql, padToken)*(n-len(padToken)))
	b.WriteString(prefix)
	chunk := strings.Repeat("x", min(n, 64<<10))
	for {
		i := strings.Index(sql, padToken)
		if i < 0 {
			break
		}
		b.WriteString(sql[:i])
		for left := n; left > 0; left -= len(chunk) {
			b.WriteString(chunk[:min(left, len(chunk))])
		}
		sql = sql[i+len(padToken):]
	}
	b.WriteString(sql)
	return b.String()
}

// limiter spaces statements across all connections.
type limiter struct {
	mu   sync.Mutex
	next time.Time // the earliest time the next statement may go
	gap  time.Duration
}

// newLimiter allows perSecond statements a second; 0 or less is no limit.
func newLimiter(perSecond float64) *limiter {
	l := &limiter{}
	if perSecond > 0 {
		l.gap = time.Duration(float64(time.Second) / perSecond)
	}
	return l
}

// wait returns when the caller's statement may go, having taken its turn.
func (l *limiter) wait(ctx context.Context) error {
	l.mu.Lock()
	at := l.next
	if now := time.Now(); at.Before(now) {
		at = now
	}
	l.next = at.Add(l.gap)
	l.mu.Unlock()
	d := time.Until(at)
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SaveOutcome stores o in lab.outcomes.
func SaveOutcome(ctx context.Context, conn *pgx.Conn, o Outcome) error {
	_, err := conn.Exec(ctx, `insert into lab.outcomes
		(arm, id, tag, set, sent_at, latency_ms, result, rows, sqlstate, severity, message)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		o.Arm, o.ID, o.Tag, o.Set, o.SentAt, o.LatencyMS, o.Result, o.Rows, o.SQLState, o.Severity, o.Message)
	if err != nil {
		return fmt.Errorf("save the outcome of %s: %w", o.ID, err)
	}
	return nil
}

// AlreadySent reports whether lab.outcomes holds a row of the arm and set.
func AlreadySent(ctx context.Context, conn *pgx.Conn, arm, set string) (bool, error) {
	var sent bool
	err := conn.QueryRow(ctx, "select exists (select 1 from lab.outcomes where arm = $1 and set = $2)", arm, set).Scan(&sent)
	return sent, err
}

// reachWithin bounds the one attempt to reach the target before anything is sent.
const reachWithin = 5 * time.Second

// reach connects once to t as role with application app, within
// reachWithin, and hangs up. Its error says that nothing was sent or
// recorded: a wrong address would otherwise make every statement wait out
// the connect window and be recorded as unknown.
func reach(ctx context.Context, t Target, role, app string) error {
	rctx, cancel := context.WithTimeout(ctx, reachWithin)
	c, err := dial(rctx, t, role, app)
	cancel()
	if err != nil {
		return fmt.Errorf("cannot reach %s as %s (database %s); nothing was sent or recorded: %w",
			t.Addr, t.Names.Role(role), t.Database, err)
	}
	closing, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	c.Close(closing)
	cancel()
	return nil
}

// Record sends phases to t and saves every outcome in lab.outcomes through conn, calling saved (if not nil)
// after each. An outcome is saved even after ctx ends: its statement was sent.
//
// First it connects once to t as the first statement's role and application, within reachWithin, and when
// that fails it returns an error having sent and recorded nothing. Otherwise a wrong address would make every
// statement wait out the connect window and be recorded as unknown, and those rows would refuse a retry.
func Record(ctx context.Context, conn *pgx.Conn, t Target, phases [][][]tasks.Stmt, o Options, saved func(Outcome)) error {
	if s, ok := firstStmt(phases); ok {
		if err := reach(ctx, t, s.Role, s.App); err != nil {
			return err
		}
	}
	save := context.WithoutCancel(ctx)
	return Send(ctx, t, phases, o, func(out Outcome) error {
		if err := SaveOutcome(save, conn, out); err != nil {
			return err
		}
		if saved != nil {
			saved(out)
		}
		return nil
	})
}

// firstStmt is the statement phases would send first.
func firstStmt(phases [][][]tasks.Stmt) (tasks.Stmt, bool) {
	for _, p := range phases {
		for _, u := range p {
			if len(u) > 0 {
				return u[0], true
			}
		}
	}
	return tasks.Stmt{}, false
}

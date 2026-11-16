// Package gate connects the proxy to judgment: it assembles state for each
// gated write, asks the model, applies policy, and records the decision.
//
// In shadow mode the write always goes through unchanged (rule 6); state is
// read just before it is forwarded, and the model is called afterwards.
//
// In enforce mode (enforce.go) the client waits while Begin judges the write
// before it is sent: the deterministic rules first, the model only when they
// do not decide (rule 5). A write Begin does not let through is refused by
// the proxy, and a held one is stored in stet.held_writes. Either way Stet
// holds no transaction of its own open while the model is asked, and a
// write in a transaction that already changed something is refused before
// anything is read for it (rule 1). End records the decision in both modes.
package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/sqlparse"
	"github.com/stetdb/stet/internal/store"
)

const (
	// stateTimeout bounds how long a client waits while its write's state
	// is read, and in enforce mode each re-read of its row version. Past it,
	// the write is a state_error, which on_failure decides.
	stateTimeout  = 500 * time.Millisecond
	maxCandidates = 10
	maxRecent     = 10
	maxModelCalls = 8
	recordTimeout = 5 * time.Second
)

type Gate struct {
	cfg     *config.Config
	tables  map[string]string // config key → pk column
	gated   sqlparse.Tables   // what Inspect hands the classifier
	cands   map[string]named
	ctxs    map[string]named
	pool    *pgxpool.Pool
	store   *store.Store
	decider decide.Decider
	log     *slog.Logger
	recent  *recentLog
	usage   *usage
	reads   *readStats    // nil when no table records reads
	sem     chan struct{} // shadow mode's model calls, made after the write
	// inflight holds a slot for each write being judged (limits.max_inflight):
	// while its Begin reads state or measures, and in enforce mode until its
	// Begin ends, its model call included.
	inflight chan struct{}
	wg       sync.WaitGroup

	// insertHeld stores a held write (store.InsertHeldWrite), and rowVersion
	// reads a row's version again before a judged write is sent
	// (readRowVersion): fields so tests can make them fail.
	insertHeld func(context.Context, *store.HeldWrite) error
	rowVersion func(ctx context.Context, table, pk string) (string, error)

	warnedRoles sync.Map // client roles Stet could not assume, logged once each
}

type named struct {
	sql   string
	names []string
}

func New(cfg *config.Config, pool *pgxpool.Pool, st *store.Store, d decide.Decider, log *slog.Logger) (*Gate, error) {
	if cfg.MaxInflight < 1 {
		// No slot would ever be free: every write would wait out its state read.
		return nil, fmt.Errorf("limits.max_inflight is %d; it must be at least 1", cfg.MaxInflight)
	}
	g := &Gate{
		cfg:      cfg,
		tables:   map[string]string{},
		cands:    map[string]named{},
		ctxs:     map[string]named{},
		pool:     pool,
		store:    st,
		decider:  d,
		log:      log,
		recent:   &recentLog{byRow: map[string][]store.Change{}},
		sem:      make(chan struct{}, maxModelCalls),
		inflight: make(chan struct{}, cfg.MaxInflight),

		insertHeld: st.InsertHeldWrite,
	}
	g.rowVersion = g.readRowVersion
	g.usage = newUsage(st.UsageBySecond, &g.wg)
	readsOn := map[string]int{}
	for key, t := range cfg.Tables {
		g.tables[key] = t.PK
		if t.CandidateSQL != "" {
			g.cands[key] = compile(t.CandidateSQL)
		}
		if t.ContextSQL != "" {
			g.ctxs[key] = compile(t.ContextSQL)
		}
		if t.Reads != nil {
			readsOn[key] = t.Reads.LogAbove
		}
	}
	if g.reads = newReadStats(readsOn, st.AddReads); g.reads != nil {
		go g.record(readFlushEvery)
	}
	// With no table recording reads, statements are not examined for them:
	// that walk would cost every statement and record nothing.
	g.gated = sqlparse.Tables{PK: g.tables, Reads: g.reads != nil}
	return g, nil
}

// Close writes the reads still held in memory and waits for decisions still
// being judged and for loads of cumulative usage still running; no load
// starts after it. Close the proxy first, and wait for it, so that no session
// is still reporting: a read reported after Close is only counted in a log
// line.
func (g *Gate) Close() {
	g.closeReads()
	g.usage.close()
	g.wg.Wait()
}

// Inspect marks gated writes to be watched, and reads of tables that record
// reads to be observed. A cursor statement always carries its cursor, so the
// proxy can follow the names a DECLARE it observes binds.
//
// SQL the classifier cannot read, SQL holding a DO block, whose writes cannot
// be seen, and SQL that defines a function, procedure, rule or trigger, or a
// view over a gated table, through which later writes could not be seen
// (ruling 29), are marked with Block whatever the mode: the proxy refuses
// them on enforced sessions only. So is COPY ... FROM STDIN (CopyIn), which
// the proxy refuses in a Parse only. The names SQL runs (EXECUTE, FETCH, MOVE)
// are passed on for the proxy to look up in the session's tables.
func (g *Gate) Inspect(sql string) proxy.Statement {
	r, err := sqlparse.Classify(sql, g.gated)
	if err != nil {
		// Usually Postgres rejects it too, but a newer server may run it.
		// Not Clean: what cannot be read leaves the transaction dirty.
		return proxy.Statement{SQL: sql, Block: proxy.BlockUnparsed}
	}
	st := proxy.Statement{SQL: sql, Watch: r.Kind == sqlparse.Gated || r.Kind == sqlparse.Measured || r.Kind == sqlparse.Unsupported, OnBehalfOf: r.OnBehalfOf, Clean: r.Clean}
	switch {
	case r.Do:
		st.Block = proxy.BlockDo
	case r.DefinesCode:
		st.Block = proxy.BlockDefinesCode
	}
	if r.Kind == sqlparse.Read {
		st.Observe = g.recordsAny(r.Reads)
	}
	for _, c := range r.Cursors {
		if op, ok := cursorOps[c.Op]; ok {
			st.Cursors = append(st.Cursors, proxy.Cursor{Op: op, Name: c.Name, Hold: c.Hold})
		}
	}
	st.Several = r.Statements > 1
	st.CopyIn = r.CopyIn
	st.Executes, st.Portals = r.Executes, r.Portals
	st.Data = r
	return st
}

// Blocked answers what the proxy refuses on a watched session without
// asking the gate to judge it, and logs it. That is a message it refuses: a
// closed point of spec 7 (SQL the classifier cannot read, a DO block, SQL
// that defines code or a view over a gated table, a message too large to
// read, a FunctionCall), a statement that runs a watched write by name or a
// name whose meaning is not settled, a statement the server may read
// otherwise than the classifier under the session's settings, or a name it
// may compare otherwise under its server_encoding. Or it is a session the
// proxy ends (proxy.EndsSession), whose client gets the answer as a FATAL: a
// COPY or a change of setting after which the server's answers no longer
// match what the proxy follows. No decision records either, so the log line
// names the session and the reason, never the statement, which may hold
// data.
//
// The proxy sets first the first time it refuses the session anything for
// reason, and the line is written only then: one per session and reason,
// however often the client repeats it (ruling 31). A session ended for
// setting_changed is logged as an error: it is the one bypass enforce mode
// accepts (ruling 22), a message sent behind the change was inspected under
// the old setting, and the server may have run it.
func (g *Gate) Blocked(s proxy.SessionInfo, reason string, first bool) proxy.Refusal {
	a := decide.Blocked(reason)
	refusal := proxy.Refusal{Code: a.Code, Message: a.Message, Hint: a.Hint("")}
	if !first {
		return refusal
	}
	level, msg := slog.LevelWarn, "message refused without a decision"
	switch {
	case reason == proxy.BlockSettingChanged:
		level, msg = slog.LevelError, "session ended: client_encoding or standard_conforming_strings changed to a "+
			"value stet cannot read, and a message sent before the server reported the change may have run "+
			"unjudged against a watched table"
	case proxy.EndsSession(reason):
		msg = "session ended without a decision"
	}
	g.log.Log(context.Background(), level, msg, "reason", reason,
		"session", s.ID, "login_role", s.User, "database", s.Database, "agent", s.ApplicationName)
	return refusal
}

// WouldBlock warns that a shadow session sent a message enforce mode would
// refuse for reason, a closed point of spec 7 (ruling 31): shadow records are
// to show what enforce will block (spec 1), and no decision records such a
// message. The proxy calls it once per session and reason. As for Blocked,
// the line names the session and the reason, never the statement.
func (g *Gate) WouldBlock(s proxy.SessionInfo, reason string) {
	g.log.Warn("would be refused under enforce", "reason", reason,
		"session", s.ID, "login_role", s.User, "database", s.Database, "agent", s.ApplicationName)
}

var cursorOps = map[sqlparse.CursorOp]proxy.CursorOp{
	sqlparse.CursorDeclare:  proxy.CursorDeclare,
	sqlparse.CursorFetch:    proxy.CursorFetch,
	sqlparse.CursorClose:    proxy.CursorClose,
	sqlparse.CursorCloseAll: proxy.CursorCloseAll,
}

// pending is one watched write between Begin and its recorded decision.
type pending struct {
	id       string
	at       time.Time
	session  proxy.SessionInfo
	sql      string
	res      sqlparse.Result
	params   []*string      // bind values as text
	oids     []uint32       // parameter types the client gave in Parse; 0 left to the server
	proposed map[string]any // every column written, before send_fields
	pk       *string
	impact   *decide.Impact // set when Begin measured a Measured write

	cumulative *decide.Cumulative // set when the table has a cumulative limit
	reserved   bool               // the write holds a reservation in Gate.usage

	state        json.RawMessage
	stateVersion map[string]string
	questions    []decide.Question
	candidateIDs []string // the keys of the candidates in state, the duplicate question's choices
	stateErr     error
	added        time.Duration // time the client waited on Begin

	// Set by Begin in enforce mode, which judges the write before it is
	// sent; judge records them and asks the model nothing.
	decided      bool
	verdict      string
	reasons      []string
	asked        bool // the model was asked about the write's last state
	answers      decide.Answers
	modelErr     error
	modelLatency time.Duration
	costUSD      float64        // every model call made for the write
	refused      *proxy.Refusal // what the client was given instead; nil when the write was sent
	heldID       string         // its stet.held_writes row; "" when not held or not stored
	heldUnsure   string         // a held write whose insert reported an error, but may have landed
}

// errBusy is the state error of a write that found every inflight slot taken
// until its time to read state was up (spec 7). It follows on_failure, as any
// state error does, also for a measured write: it is not a failed
// measurement, which the table's unsupported setting would decide.
var errBusy = errors.New("too many writes are being judged")

// Begin runs while the client waits, just before a watched write is sent. In
// shadow mode it reads what the write's judgment needs and never refuses. In
// enforce mode it judges the write too, and returns the refusal the client
// gets when the write must not be sent (see enforce).
func (g *Gate) Begin(s proxy.SessionInfo, st proxy.Statement, ps []proxy.Param) (any, *proxy.Refusal) {
	start := time.Now()
	p := &pending{id: store.NewID(), at: start, session: s, sql: st.SQL, res: st.Data.(sqlparse.Result)}
	for _, x := range ps {
		p.oids = append(p.oids, x.OID)
	}
	if g.cfg.Mode == config.ModeEnforce {
		refuse := g.enforce(p, ps)
		p.added = time.Since(start)
		return p, refuse
	}

	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	defer cancel()
	// In shadow mode the slot is let go when Begin returns; the model is
	// asked after the write, under maxModelCalls instead.
	release := g.read(ctx, p, ps)
	p.added = time.Since(start)
	release()
	return p, nil
}

// read gathers, under ctx, what the verdict rules need of p, the same way in
// both modes: an inflight slot, the bind values, the state of a single-row
// write or the measurement of a multi-row one, and a reservation against its
// login role's cumulative limit. It returns the func that gives the slot back.
func (g *Gate) read(ctx context.Context, p *pending, ps []proxy.Param) (release func()) {
	// First, a slot: the wait counts toward the state read's bound (spec 10).
	release = func() {}
	select {
	case g.inflight <- struct{}{}:
		release = func() { <-g.inflight }
	case <-ctx.Done():
		p.stateErr = errBusy
	}

	// Text values need no database; binary ones cannot be decoded once the
	// time is up, and then the decision records no values.
	params, err := g.paramTexts(ctx, p.sql, ps)
	p.params = params
	if err != nil && p.stateErr == nil {
		p.stateErr = fmt.Errorf("bind values: %w", err)
	}
	if p.res.Kind == sqlparse.Gated && p.stateErr == nil {
		if err := g.assemble(ctx, p); err != nil {
			p.stateErr = err
		}
	}
	lim := g.cfg.Tables[p.res.Table].Impact
	// In a dirty transaction the verdict is in_transaction anyway, and Stet's
	// connection cannot see what the transaction changed before the write
	// (spec 4): the impact stays NULL.
	if p.res.Kind == sqlparse.Measured && lim != nil && !p.session.TxDirty {
		switch {
		case p.stateErr == nil:
			p.impact = g.measure(ctx, p, lim)
		case !errors.Is(p.stateErr, errBusy):
			// The bind values could not be read: the measurement failed.
			p.impact = &decide.Impact{Error: p.stateErr.Error()}
		}
	}
	if p.stateErr != nil {
		g.log.Warn("state not assembled", "table", p.res.Table, "err", p.stateErr)
	}
	if lim != nil && lim.Cumulative != nil {
		g.reserve(ctx, p, lim.Cumulative)
	}
	return release
}

// reserve counts the write toward its login role's cumulative limit before it
// is sent, whatever its shape, and keeps what the role had used for the
// verdict (spec 4).
func (g *Gate) reserve(ctx context.Context, p *pending, lim *config.Cumulative) {
	if p.session.User == "" {
		// Not mixed into any role's usage; the limit is not applied (spec 7).
		p.cumulative = &decide.Cumulative{Error: "no login role"}
		return
	}
	planned := plannedRows(p)
	used, err := g.usage.reserve(ctx, p.session.User, p.res.Table, p.id, p.at, planned, lim.Window)
	p.reserved = true
	if err != nil {
		if ctx.Err() != nil {
			err = errors.New("timeout")
		}
		g.log.Warn("cumulative usage not loaded", "table", p.res.Table, "role", p.session.User, "err", err)
		p.cumulative = &decide.Cumulative{Error: "usage not loaded: " + err.Error()}
		return
	}
	p.cumulative = &decide.Cumulative{
		Used: &used, Planned: &planned, Limit: int64(lim.MaxRows), WindowS: int(lim.Window / time.Second),
	}
}

// plannedRows is what a write is expected to touch, counted before it is
// sent: 1 for a single-row write, the measured rows (the cap when capped) for
// a measured one, and 0 for any other, settled once its outcome is known.
func plannedRows(p *pending) int64 {
	switch {
	case p.res.Kind == sqlparse.Gated:
		return 1
	case p.impact != nil && p.impact.Method != "" && p.impact.Rows != nil:
		return *p.impact.Rows
	}
	return 0
}

// rowsAffected is how many of the gated table's rows the write changed, from
// its completion tag (spec 3), also when the connection ended after the tag.
// Nil when it did not run, or when the tag is not that table's count: a write
// nested in another statement, or one of several statements in a Query.
func rowsAffected(r sqlparse.Result, o proxy.Outcome) *int64 {
	if !o.Executed || r.Reason == "nested_write" || r.Reason == "multi_statement" {
		return nil
	}
	return o.Rows
}

// End records how the write ended, in the background: it does not block. A
// write Begin refused did not run: it is recorded with the refusal's error,
// and its reservation goes as for a write the server refused (spec 11). When
// the client was not given the refusal, its held write is marked undelivered
// (spec 9).
//
// The proxy never reports a refused write as run or lost. Should it, that is
// the worst of its defects, so End logs an error and records what the proxy
// reported rather than its own refusal: a write that ran is recorded so, and
// its rows count against the cumulative limit.
func (g *Gate) End(h any, o proxy.Outcome) {
	p := h.(*pending)
	switch {
	case p.refused == nil:
	case o.Executed || o.Lost:
		g.log.Error("refused write reported as sent", "decision", p.id, "table", p.res.Table,
			"executed", o.Executed, "lost", o.Lost, "session", p.session.ID, "login_role", p.session.User)
	default:
		o = proxy.Outcome{Refused: true, Undelivered: o.Undelivered, InTransaction: o.InTransaction, Error: refusalText(p.refused)}
	}
	if o.Executed && p.res.Kind == sqlparse.Gated && p.pk != nil {
		g.recent.add(p.res.Table, *p.pk, store.Change{
			DecisionID: p.id, At: p.at, AgentID: p.session.ApplicationName, Op: p.res.Op, Proposed: p.proposed,
		}, g.cfg.Tables[p.res.Table].RecentWindow)
	}
	// Settled here rather than when the decision is recorded, so the role's
	// next write sees it.
	if p.reserved {
		g.usage.settle(p.session.User, p.res.Table, p.id, rowsAffected(p.res, o), o.Executed, o.Lost)
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.judge(p, o)
	}()
}

// input is what the verdict rules know of a write before any model answer:
// its shape, the transaction it arrived in, its login role, its measurement,
// its cumulative usage, and whether its state was read. Shadow mode's judge
// and enforce mode's Begin both decide from it, so the two modes apply one
// set of rules (spec 3); decide.Verdict fixes their order.
func (g *Gate) input(p *pending) decide.Input {
	t := g.cfg.Tables[p.res.Table]
	in := decide.Input{
		Thresholds:    t.Thresholds,
		OnFailure:     g.cfg.Model.OnFailure,
		Questions:     p.questions,
		StateErr:      p.stateErr,
		Cumulative:    p.cumulative,
		InTransaction: p.session.TxDirty,
		NoLoginRole:   p.session.User == "",
	}
	switch {
	case p.res.Kind == sqlparse.Unsupported,
		p.res.Kind == sqlparse.Measured && t.Impact == nil:
		in.Unsupported = p.res.Reason
	case p.res.Kind == sqlparse.Measured:
		// Begin left the impact nil only in a dirty transaction, which
		// decides first, or when it found no slot, a state error.
		in.Op, in.ShapeReason, in.Impact, in.ImpactLimit = p.res.Op, p.res.Reason, p.impact, t.Impact
	}
	return in
}

// asksInShadow reports whether shadow mode asks the model about a write: it
// has questions, and no rule ahead of the cumulative limit decides it. A
// write held by the cumulative limit alone, or because its usage is not
// known, is still asked, so that its answers are recorded and the write can
// be rescored under another limit (spec-v0.2-accounting 4). A write in a
// dirty transaction or with no login role is not.
func asksInShadow(in decide.Input) bool {
	in.Cumulative = nil
	_, _, decided := decide.Deterministic(in)
	return !decided
}

// judge records the decision on p. In shadow mode it decides what enforce
// mode would have done, asking the model now; in enforce mode Begin decided,
// and what the model answered then is recorded as it is (rule 4). It also
// marks undelivered a held write the client was not told of: one whose
// refusal did not reach the client, and one whose insert reported an error
// although it may have landed (marking an id that never landed does nothing).
func (g *Gate) judge(p *pending, o proxy.Outcome) {
	var untold []string
	if o.Undelivered && p.heldID != "" {
		untold = append(untold, p.heldID)
	}
	if p.heldUnsure != "" {
		untold = append(untold, p.heldUnsure)
	}
	for _, id := range untold {
		ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
		if err := g.store.MarkUndelivered(ctx, id); err != nil {
			g.log.Error("held write not marked undelivered", "id", id, "decision", p.id, "err", err)
		}
		cancel()
	}
	in := g.input(p)
	d := &store.Decision{
		ID:            p.id,
		CreatedAt:     p.at,
		Mode:          string(g.cfg.Mode),
		AgentID:       p.session.ApplicationName,
		SessionID:     p.session.ID,
		Statement:     p.sql,
		StatementFP:   sqlparse.Fingerprint(p.sql),
		Params:        p.params,
		Op:            p.res.Op,
		TargetTable:   p.res.Table,
		TargetPK:      p.pk,
		Proposed:      p.proposed,
		State:         p.state,
		StateVersion:  p.stateVersion,
		Model:         g.decider.ModelID(),
		ModelVersion:  g.decider.ModelVersion(),
		PolicyVersion: g.cfg.Hash,
		Executed:      o.Executed,
		LatencyMS:     ptr(int(p.added.Milliseconds())),
	}
	if p.session.OnBehalfOf != "" {
		d.OnBehalfOf = ptr(p.session.OnBehalfOf)
	}
	if o.Error != "" {
		d.ExecError = ptr(o.Error)
	}
	if p.session.User != "" {
		d.LoginRole = ptr(p.session.User)
	}
	d.RowsAffected = rowsAffected(p.res, o)
	if p.cumulative != nil {
		d.Cumulative = p.cumulative
	}

	switch {
	case p.decided:
		d.Verdict, d.Reasons = p.verdict, p.reasons
		if p.asked {
			d.Questions = p.questions
			if p.modelErr == nil {
				d.Answers = p.answers.ByQuestion
				d.ModelLatencyMS = ptr(int(p.modelLatency.Milliseconds()))
			}
		}
		if p.costUSD > 0 {
			d.CostUSD = ptr(p.costUSD)
		}
		if p.refused != nil {
			d.Refused = ptr(p.refused.Code)
		}
	default:
		if asksInShadow(in) {
			d.Questions = p.questions
			answers, latency, err := g.ask(p)
			in.ModelErr, in.Answers = err, answers.ByQuestion
			if err == nil {
				d.Answers = answers.ByQuestion
				d.ModelLatencyMS = ptr(int(latency.Milliseconds()))
				if answers.CostUSD > 0 {
					d.CostUSD = ptr(answers.CostUSD)
				}
			} else if !errors.Is(err, decide.ErrNoModel) {
				g.log.Warn("model call failed", "table", p.res.Table, "err", err)
			}
		}
		// The reason as enforce mode gives it (spec 3, 7); the model's own
		// answer stays in d.Answers.
		d.Verdict, d.Reasons = decide.Verdict(in)
		d.Reasons = knownDuplicate(d.Reasons, p.candidateIDs)
	}
	// Recorded whenever a measurement was taken or failed, also for a write
	// whose verdict another rule decided (spec-v0.2-impact 6).
	if p.impact != nil {
		p.impact.ActualRows = o.Rows
		d.Impact = p.impact
	}
	g.exceededAfter(p, d)

	ctx, cancel := context.WithTimeout(context.Background(), recordTimeout)
	defer cancel()
	if err := g.store.Insert(ctx, d); err != nil {
		g.log.Error("decision not recorded", "id", d.ID, "table", d.TargetTable, "err", err)
	}
}

// exceededAfter marks d, in either mode, when p was measured and judged
// commit but changed more rows than the table's max_rows once it ran (spec
// 5): the measurement and the write saw different rows. The write is not
// undone; a warning says so.
func (g *Gate) exceededAfter(p *pending, d *store.Decision) {
	lim := g.cfg.Tables[p.res.Table].Impact
	if d.Verdict != decide.Commit || p.impact == nil || p.impact.Method == "" || lim == nil ||
		d.RowsAffected == nil || *d.RowsAffected <= int64(lim.MaxRows) {
		return
	}
	d.Reasons = append(slices.Clone(d.Reasons), "impact:exceeded_after")
	g.log.Warn("write changed more rows than its limit after it was judged",
		"decision", d.ID, "table", p.res.Table, "rows", *d.RowsAffected, "max_rows", lim.MaxRows,
		"measured", deref(p.impact.Rows), "login_role", p.session.User)
}

// deref is the value p points to, or nil.
func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func (g *Gate) ask(p *pending) (decide.Answers, time.Duration, error) {
	g.sem <- struct{}{}
	defer func() { <-g.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(g.cfg.Model.TimeoutMS)*time.Millisecond)
	defer cancel()
	start := time.Now()
	a, err := g.decider.Decide(ctx, p.state, p.questions)
	return a, time.Since(start), err
}

// State is what the model sees (spec 4). Agent-written values are marked as
// untrusted (rule 5).
//
// The field order is part of the format: Jev's confidence moved by up to
// 0.5 when the same fields were sent in another order. Anything that sends
// a state (the bench included) must use this type so the order matches.
type State struct {
	Table         string            `json:"table"`
	Entity        string            `json:"entity,omitempty"`
	Operation     string            `json:"operation"`
	Proposed      map[string]any    `json:"proposed"`
	Current       map[string]any    `json:"current"`
	Context       map[string]any    `json:"context,omitempty"` // only when the table configures a context query
	Candidates    []map[string]any  `json:"candidates"`
	RecentChanges []RecentChange    `json:"recent_changes"`
	Policy        string            `json:"policy,omitempty"` // the rules, one per line
	Provenance    map[string]string `json:"provenance"`
}

type RecentChange struct {
	At time.Time `json:"at"`
	// Computed here because judgment models compare timestamps poorly.
	MinutesBefore int            `json:"minutes_before_this_write"`
	ByAgent       string         `json:"by_agent"`
	Op            string         `json:"op"`
	Fields        map[string]any `json:"fields"`
}

// RebuildState turns a state recorded in stet.decisions back into the bytes
// the gate sent. jsonb drops key order, which is part of the format, and
// numbers are kept as written, as the gate reads them.
func RebuildState(recorded []byte) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(recorded))
	dec.UseNumber()
	var s State
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("recorded state: %w", err)
	}
	return json.Marshal(s)
}

// Provenance labels each part of a State by where it came from.
func Provenance() map[string]string {
	return map[string]string{
		"proposed":       "untrusted_agent_input",
		"current":        "database",
		"candidates":     "database",
		"recent_changes": "earlier_agent_writes",
	}
}

func (g *Gate) assemble(ctx context.Context, p *pending) error {
	t := g.cfg.Tables[p.res.Table]
	w := p.res.Write

	cols := w.Columns
	if cols == nil && len(w.Values) > 0 {
		all, err := g.tableColumns(ctx, p.res.Table)
		if err != nil {
			return fmt.Errorf("table columns: %w", err)
		}
		if len(all) < len(w.Values) {
			return fmt.Errorf("table has %d columns, insert gives %d values", len(all), len(w.Values))
		}
		cols = all[:len(w.Values)]
	}
	p.proposed = map[string]any{}
	for i, v := range w.Values {
		p.proposed[cols[i]] = proposedValue(v, p.params)
	}
	if w.PK != nil {
		p.pk = valueText(*w.PK, p.params)
	}

	var current map[string]any
	if w.Op != "insert" && p.pk != nil {
		row, xmin, err := g.currentRow(ctx, p.res.Table, t.PK, *p.pk)
		if err != nil {
			return fmt.Errorf("current row: %w", err)
		}
		if row != nil {
			current = row
			p.stateVersion = map[string]string{"xmin": xmin}
		}
	}

	cands, err := g.candidates(ctx, p, current)
	if err != nil {
		return fmt.Errorf("candidates: %w", err)
	}
	var ids []string
	for _, c := range cands {
		if id, ok := c[t.PK]; ok {
			ids = append(ids, jsonText(id))
		}
	}
	p.candidateIDs = ids

	related, err := g.context(ctx, p, current)
	if err != nil {
		return fmt.Errorf("context: %w", err)
	}

	recent, err := g.recentChanges(ctx, p, t)
	if err != nil {
		return fmt.Errorf("recent changes: %w", err)
	}

	doc := State{
		Table:         p.res.Table,
		Entity:        t.Entity,
		Operation:     w.Op,
		Proposed:      only(p.proposed, t.SendFields),
		Current:       only(current, t.SendFields),
		Context:       related,
		Candidates:    cands,
		RecentChanges: recent,
		Policy:        strings.Join(t.PolicyRules, "\n"),
		Provenance:    Provenance(),
	}
	if related != nil {
		doc.Provenance["context"] = "database"
	}
	if p.state, err = json.Marshal(doc); err != nil {
		return err
	}
	p.questions = decide.Questions(decide.Ask{
		Op: w.Op, CandidateIDs: ids, Enabled: t.Questions, Rules: t.PolicyRules,
		Recent: doc.CouldUndoRecent(),
	})
	return nil
}

// CouldUndoRecent reports whether the model can see a recent change this
// write might undo: one that set a field this update writes, or any change
// before a delete. Judged on what the model is sent, after send_fields.
func (s State) CouldUndoRecent() bool {
	switch s.Operation {
	case "delete":
		return len(s.RecentChanges) > 0
	case "update":
		for _, c := range s.RecentChanges {
			for f := range c.Fields {
				if _, ok := s.Proposed[f]; ok {
					return true
				}
			}
		}
	}
	return false
}

// paramTexts turns bind values into text. Binary values are decoded by
// Postgres itself, using the statement's parameter types.
//
// Decoding runs the types' code, a domain's CHECK included, on Stet's
// connection as Stet's own account, and the agent chose both the statement
// and the values. So the statement is prepared and the values decoded in a
// read-only transaction under the timeout, then rolled back. No role is
// assumed: tables without impact limits need no role membership.
func (g *Gate) paramTexts(ctx context.Context, sql string, ps []proxy.Param) ([]*string, error) {
	if out := textParams(ps); out != nil || len(ps) == 0 {
		return out, nil
	}
	// A value is binary: every value is read back from the database.
	out := make([]*string, len(ps))

	pg, done, err := g.txConn(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := pg.Exec(ctx, beginReadOnly("")).Close(); err != nil {
		return nil, err
	}

	clientOIDs := make([]uint32, len(ps))
	for i, p := range ps {
		clientOIDs[i] = p.OID
	}
	sd, err := pg.Prepare(ctx, "", sql, clientOIDs)
	if err != nil {
		return nil, err
	}
	var q strings.Builder
	q.WriteString("select ")
	values := make([][]byte, len(ps))
	formats := make([]int16, len(ps))
	for i, p := range ps {
		if i > 0 {
			q.WriteString(", ")
		}
		fmt.Fprintf(&q, "$%d::text", i+1)
		values[i], formats[i] = p.Value, p.Format
	}
	res := pg.ExecParams(ctx, q.String(), values, sd.ParamOIDs, formats, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	for i, v := range res.Rows[0] {
		if v != nil {
			out[i] = ptr(string(v))
		}
	}
	return out, nil
}

func (g *Gate) currentRow(ctx context.Context, table, pkCol, pk string) (map[string]any, string, error) {
	q := fmt.Sprintf("select to_jsonb(t)::text, t.xmin::text from %s as t where t.%s = $1",
		ident(table), pgx.Identifier{pkCol}.Sanitize())
	rows, err := g.queryText(ctx, q, []*string{&pk})
	if err != nil || len(rows) == 0 {
		return nil, "", err
	}
	row, err := decodeObject(*rows[0][0])
	return row, *rows[0][1], err
}

// context runs the table's context query, bound like candidates, and
// returns its first row: facts the policy needs that the row does not hold,
// such as the account's open deals. Nil when the table has no context query.
func (g *Gate) context(ctx context.Context, p *pending, current map[string]any) (map[string]any, error) {
	c, ok := g.ctxs[p.res.Table]
	if !ok {
		return nil, nil
	}
	rows, err := g.queryText(ctx, fmt.Sprintf("select to_jsonb(c)::text from (%s) as c limit 1", c.sql),
		c.bind(rowAfterWrite(current, p.proposed)))
	if err != nil || len(rows) == 0 {
		return map[string]any{}, err
	}
	return decodeObject(*rows[0][0])
}

// rowAfterWrite is the row as it would look after the write, as text for
// binding: the current row with the proposed values over it. Proposed
// expressions and defaults have no known value and bind as NULL.
func rowAfterWrite(current, proposed map[string]any) map[string]*string {
	row := map[string]*string{}
	for k, v := range current {
		if v != nil {
			row[k] = ptr(jsonText(v))
		}
	}
	for k, v := range proposed {
		if s, ok := v.(string); ok {
			row[k] = ptr(s)
		} else {
			delete(row, k)
		}
	}
	return row
}

// candidates runs the table's candidate SQL with named parameters bound
// from the row as it would look after the write. The row itself is left out.
func (g *Gate) candidates(ctx context.Context, p *pending, current map[string]any) ([]map[string]any, error) {
	out := []map[string]any{}
	c, ok := g.cands[p.res.Table]
	if !ok {
		return out, nil
	}
	q := fmt.Sprintf("select to_jsonb(c)::text from (%s) as c limit %d", c.sql, maxCandidates+1)
	rows, err := g.queryText(ctx, q, c.bind(rowAfterWrite(current, p.proposed)))
	if err != nil {
		return nil, err
	}
	pkCol := g.tables[p.res.Table]
	for _, r := range rows {
		row, err := decodeObject(*r[0])
		if err != nil {
			return nil, err
		}
		if id, ok := row[pkCol]; ok && p.pk != nil && jsonText(id) == *p.pk {
			continue
		}
		if len(out) < maxCandidates {
			out = append(out, row)
		}
	}
	return out, nil
}

// recentChanges merges recorded decisions with writes that ended but whose
// decisions are still being judged.
func (g *Gate) recentChanges(ctx context.Context, p *pending, t config.Table) ([]RecentChange, error) {
	out := []RecentChange{}
	if p.pk == nil || t.RecentWindow <= 0 {
		return out, nil
	}
	since := p.at.Add(-t.RecentWindow)
	stored, err := g.store.RecentChanges(ctx, p.res.Table, *p.pk, since, maxRecent)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var all []store.Change
	for _, c := range append(g.recent.since(p.res.Table, *p.pk, since), stored...) {
		if !seen[c.DecisionID] {
			seen[c.DecisionID] = true
			all = append(all, c)
		}
	}
	slices.SortFunc(all, func(a, b store.Change) int { return b.At.Compare(a.At) })
	for _, c := range all[:min(len(all), maxRecent)] {
		out = append(out, RecentChange{
			At: c.At, MinutesBefore: int(p.at.Sub(c.At).Minutes()),
			ByAgent: c.AgentID, Op: c.Op, Fields: only(c.Proposed, t.SendFields),
		})
	}
	return out, nil
}

func (g *Gate) tableColumns(ctx context.Context, table string) ([]string, error) {
	rows, err := g.queryText(ctx, `
		select attname from pg_attribute
		where attrelid = $1::regclass and attnum > 0 and not attisdropped
		order by attnum`, []*string{&table})
	if err != nil {
		return nil, err
	}
	cols := make([]string, len(rows))
	for i, r := range rows {
		cols[i] = *r[0]
	}
	return cols, nil
}

// queryText runs sql with text parameters whose types Postgres infers, and
// returns text columns (nil for NULL).
func (g *Gate) queryText(ctx context.Context, sql string, args []*string) ([][]*string, error) {
	conn, err := g.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	values := make([][]byte, len(args))
	for i, a := range args {
		if a != nil {
			values[i] = []byte(*a)
		}
	}
	res := conn.Conn().PgConn().ExecParams(ctx, sql, values, nil, nil, nil).Read()
	if res.Err != nil {
		return nil, res.Err
	}
	out := make([][]*string, len(res.Rows))
	for i, r := range res.Rows {
		out[i] = make([]*string, len(r))
		for j, v := range r {
			if v != nil {
				out[i][j] = ptr(string(v))
			}
		}
	}
	return out, nil
}

func proposedValue(v sqlparse.Value, params []*string) any {
	switch v.Kind {
	case sqlparse.Literal:
		return v.Text
	case sqlparse.Param:
		if s := param(params, v.Param); s != nil {
			return *s
		}
		return nil
	case sqlparse.Default:
		return map[string]any{"default": true}
	case sqlparse.Expr:
		return map[string]any{"expr": inlineParams(v.Text, params)}
	}
	return nil
}

func valueText(v sqlparse.Value, params []*string) *string {
	switch v.Kind {
	case sqlparse.Literal:
		return ptr(v.Text)
	case sqlparse.Param:
		return param(params, v.Param)
	}
	return nil
}

func param(params []*string, n int) *string {
	if n < 1 || n > len(params) {
		return nil
	}
	return params[n-1]
}

var paramRef = regexp.MustCompile(`\$(\d+)`)

// inlineParams shows an expression with its bind values, so the model reads
// lower('A@B.com') rather than lower($2).
func inlineParams(expr string, params []*string) string {
	return paramRef.ReplaceAllStringFunc(expr, func(m string) string {
		n, _ := strconv.Atoi(m[1:])
		if n < 1 || n > len(params) {
			return m
		}
		if params[n-1] == nil {
			return "NULL"
		}
		return "'" + strings.ReplaceAll(*params[n-1], "'", "''") + "'"
	})
}

func only(m map[string]any, fields []string) map[string]any {
	if m == nil {
		return nil
	}
	out := map[string]any{}
	for _, f := range fields {
		if v, ok := m[f]; ok {
			out[f] = v
		}
	}
	return out
}

func decodeObject(s string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	return m, dec.Decode(&m)
}

// jsonText renders a JSON value as the text Postgres would bind it as.
func jsonText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	b, _ := json.Marshal(v)
	return string(bytes.TrimSpace(b))
}

func ident(table string) string {
	return pgx.Identifier(strings.Split(table, ".")).Sanitize()
}

func ptr[T any](v T) *T { return &v }

// recentLog remembers executed writes per row until their decisions are
// stored, so the next write to the same row sees them at once.
type recentLog struct {
	mu    sync.Mutex
	byRow map[string][]store.Change
	adds  int
}

func (l *recentLog) add(table, pk string, c store.Change, keep time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := table + "\x00" + pk
	cutoff := time.Now().Add(-keep)
	l.byRow[key] = append(prune(l.byRow[key], cutoff), c)
	if l.adds++; l.adds%1000 == 0 {
		for k, cs := range l.byRow {
			if cs = prune(cs, cutoff); len(cs) == 0 {
				delete(l.byRow, k)
			} else {
				l.byRow[k] = cs
			}
		}
	}
}

func (l *recentLog) since(table, pk string, t time.Time) []store.Change {
	l.mu.Lock()
	defer l.mu.Unlock()
	return prune(slices.Clone(l.byRow[table+"\x00"+pk]), t)
}

func prune(cs []store.Change, cutoff time.Time) []store.Change {
	return slices.DeleteFunc(cs, func(c store.Change) bool { return c.At.Before(cutoff) })
}

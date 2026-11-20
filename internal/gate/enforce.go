package gate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/sqlparse"
	"github.com/stetdb/stet/internal/store"
)

// heldTimeout bounds the insert of a held write. The client waits for it,
// since the error names the row it inserts (spec 9).
const heldTimeout = 500 * time.Millisecond

// enforce judges p before it is sent, in the order of spec 3, and returns the
// refusal the client gets in its place, or nil to send it. It fails closed:
// whatever fails on the way (the state read, the model, the row version's
// re-read, the held write's insert) ends in a refusal or in what on_failure
// says, and the only writes it sends are those decide.Act forwards.
//
// The client waits here, for no more than the state read (stateTimeout, the
// wait for an inflight slot included), the model (model.timeout_ms), the
// row version's re-read (stateTimeout), one re-judgment of those parts, and
// the held write's insert (heldTimeout). Stet holds no transaction of its
// own open while the model is asked: the state read and the measurement end
// theirs before it (rule 1).
func (g *Gate) enforce(p *pending, ps []proxy.Param) *proxy.Refusal {
	// 1. The shape, the transaction and the login role decide alone: such a
	// write takes no slot, and no state is read nor measurement taken for
	// it, so a client transaction that already changed something does not
	// wait while it may hold locks (spec 4). See unjudged for one that
	// unsupported: pass sends.
	if v, reasons := decide.Verdict(g.input(p)); v == decide.Unsupported {
		return g.unjudged(p, ps, v, reasons)
	}

	// 2. A slot, the bind values, the state or the measurement, and the
	// reservation, as in shadow mode. The slot is kept until Begin ends:
	// limits.max_inflight counts model calls too (spec 10).
	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	release := g.read(ctx, p, ps)
	cancel()
	defer release()

	// 3, 4. The rules that need no model, then the model.
	v, reasons := g.verdict(p)
	// 5. The row version of a single-row UPDATE or DELETE about to be sent.
	v, reasons = g.recheck(p, v, reasons)
	// 6, 7.
	return g.act(p, v, reasons, decide.Act(v, reasons, g.facts(p)))
}

// unjudged applies an unsupported verdict that the shape, the transaction or
// the login role gave before anything was read (step 1). A write the table's
// unsupported: pass lets through is sent unjudged, but its rows still count:
// it is reserved against the cumulative limit, with 0 planned, and settled in
// End as every watched write is (ruling P3). Any other is refused with no
// round trip to the database; its bind values are recorded when none needs
// decoding, which would take one.
func (g *Gate) unjudged(p *pending, ps []proxy.Param, v string, reasons []string) *proxy.Refusal {
	a := decide.Act(v, reasons, g.facts(p))
	if !a.Forward {
		p.params = textParams(ps)
		return g.act(p, v, reasons, a)
	}
	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	defer cancel()
	params, err := g.paramTexts(ctx, p.sql, ps)
	if err != nil {
		g.log.Warn("bind values not read", "table", p.res.Table, "err", err)
	}
	p.params = params
	if lim := g.cfg.Tables[p.res.Table].Impact; lim != nil && lim.Cumulative != nil {
		g.reserve(ctx, p, lim.Cumulative)
	}
	return g.act(p, v, reasons, a)
}

// verdict decides p by the rules that need no model answer, and asks the
// model only when they do not decide (rule 5). A duplicate the model names
// is kept only when it is one of the candidates Stet sent it.
func (g *Gate) verdict(p *pending) (string, []string) {
	in := g.input(p)
	if v, reasons, ok := decide.Deterministic(in); ok {
		return v, reasons
	}
	a, latency, err := g.consult(p)
	p.asked, p.answers, p.modelErr, p.modelLatency = true, a, err, latency
	p.costUSD += a.CostUSD
	if err != nil && !errors.Is(err, decide.ErrNoModel) {
		g.log.Warn("model call failed", "table", p.res.Table, "err", err)
	}
	in.ModelErr, in.Answers = err, a.ByQuestion
	v, reasons := decide.Verdict(in)
	return v, knownDuplicate(reasons, p.candidateIDs)
}

// consult asks the model about p's state while the client waits. The wait
// ends at model.timeout_ms even when the adapter does not heed its context:
// such a call is left to finish alone, and its answer is not used. The call
// is not counted in sem, which bounds shadow mode's calls; the write's
// inflight slot bounds it (spec 10).
func (g *Gate) consult(p *pending) (decide.Answers, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(g.cfg.Model.TimeoutMS)*time.Millisecond)
	defer cancel()
	type result struct {
		a   decide.Answers
		err error
	}
	done := make(chan result, 1)
	state, qs := p.state, p.questions
	start := time.Now()
	go func() {
		a, err := g.decider.Decide(ctx, state, qs)
		done <- result{a, err}
	}()
	select {
	case r := <-done:
		return r.a, time.Since(start), r.err
	case <-ctx.Done():
		select {
		case r := <-done: // answered as well: its answer is the better one
			return r.a, time.Since(start), r.err
		default:
			return decide.Answers{}, time.Since(start), fmt.Errorf("model: %w", ctx.Err())
		}
	}
}

// knownDuplicate drops the id of a duplicate reason that is not one of ids,
// the candidates the model was shown: its answer is shaped by agent-written
// data, and a reason reaches the agent's error (rule 5; ruling on Task 4's
// report). The reason is then "duplicate:", which names no row.
func knownDuplicate(reasons, ids []string) []string {
	var out []string
	for i, r := range reasons {
		if id, ok := strings.CutPrefix(r, "duplicate:"); ok && id != "" && !slices.Contains(ids, id) {
			if out == nil {
				out = slices.Clone(reasons)
			}
			out[i] = "duplicate:"
		}
	}
	if out == nil {
		return reasons
	}
	return out
}

// recheck reads again, just before p is sent, the version (xmin) of the row a
// single-row UPDATE or DELETE judged commit targets, through Stet's pool and
// outside any transaction (spec 5). The row as judged: the write is sent.
// Gone: sent too, to match nothing. Changed: its state is read again and it
// is judged once more, with "rejudged" after its reasons, and a commit is
// checked once more; a row that changed again holds it (concurrent_change).
// Read again, the row is compared again with what the write's login role
// read, so a write of a row the role read before is rejected as stale
// (spec-v0.2-stale 6), and its version is not read a third time.
// A version that cannot be read is a state error, which on_failure decides.
// The gap between the last read and the server running the write stays
// (spec 13).
func (g *Gate) recheck(p *pending, v string, reasons []string) (string, []string) {
	if !versioned(p, v) {
		return v, reasons
	}
	changed, err := g.rowChanged(p)
	switch {
	case err != nil:
		return g.versionUnread(p, err)
	case !changed:
		return v, reasons
	}

	// The record keeps the last judgment alone (spec 11), with what the
	// model answered for it.
	p.state, p.stateVersion, p.questions, p.candidateIDs = nil, nil, nil, nil
	p.asked, p.answers, p.modelErr, p.modelLatency = false, decide.Answers{}, nil, 0
	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	err = g.assemble(ctx, p)
	cancel()
	if err != nil {
		p.stateErr = err
		g.log.Warn("state not assembled", "table", p.res.Table, "err", err)
	}
	v, reasons = g.verdict(p)
	if versioned(p, v) {
		switch changed, err := g.rowChanged(p); {
		case err != nil:
			v, reasons = g.versionUnread(p, err)
		case changed:
			v, reasons = decide.Hold, []string{"concurrent_change"}
		}
	}
	return v, append(slices.Clone(reasons), "rejudged")
}

// versioned reports whether p's row version is read again before it is
// sent: a single-row UPDATE or DELETE judged commit, whose state was read.
func versioned(p *pending, v string) bool {
	return v == decide.Commit && p.res.Kind == sqlparse.Gated && (p.res.Op == "update" || p.res.Op == "delete") &&
		p.pk != nil && p.state != nil && p.stateErr == nil
}

// rowChanged reads the version of p's row again and reports whether it is
// not the one p's state was read at. A row that is gone has not changed; one
// that was not there and now is has.
func (g *Gate) rowChanged(p *pending) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stateTimeout)
	defer cancel()
	now, err := g.rowVersion(ctx, p.res.Table, *p.pk)
	if err != nil || now == "" {
		return false, err
	}
	return now != p.stateVersion["xmin"], nil
}

// readRowVersion is the xmin of the row of table whose key is pk, read
// through the pool outside any transaction, or "" when there is none.
func (g *Gate) readRowVersion(ctx context.Context, table, pk string) (string, error) {
	q := fmt.Sprintf("select t.xmin::text from %s as t where t.%s = $1",
		ident(table), pgx.Identifier{g.tables[table]}.Sanitize())
	rows, err := g.queryText(ctx, q, []*string{&pk})
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return *rows[0][0], nil
}

// versionUnread is p's verdict when its row version could not be read again:
// a state error, which on_failure decides.
func (g *Gate) versionUnread(p *pending, err error) (string, []string) {
	g.log.Warn("row version not read", "table", p.res.Table, "err", err)
	p.stateErr = fmt.Errorf("row version: %w", err)
	return decide.Verdict(g.input(p))
}

// act carries out a, the action for p's verdict v and its reasons (steps 6
// and 7), and keeps them for the record. A write it forwards is sent, and
// marked as its login role's own until its effect is known (spec-v0.2-stale
// 7). Any other is refused, and a held one is stored first, so that the
// error can name it.
func (g *Gate) act(p *pending, v string, reasons []string, a decide.Action) *proxy.Refusal {
	p.decided, p.verdict, p.reasons = true, v, reasons
	if a.Forward {
		g.stale.mark(p)
		return nil
	}
	if a.Held {
		p.heldID = g.hold(p)
	}
	p.refused = &proxy.Refusal{
		Code:    a.Code,
		Message: a.Message,
		Detail:  decide.Detail(p.id, p.heldID, reasons),
		Hint:    a.Hint(p.heldID),
	}
	r := *p.refused
	return &r
}

// facts are what decide.Act may put into p's error: the table's settings,
// what Begin learnt of the write, and its transaction and login role as its
// Input has them (ruling P2/P5). PK is the table's key column, not the row's
// key.
func (g *Gate) facts(p *pending) decide.Facts {
	t := g.cfg.Tables[p.res.Table]
	in := g.input(p)
	return decide.Facts{
		Table:         p.res.Table,
		PK:            t.PK,
		Op:            p.res.Op,
		Unsupported:   t.Unsupported,
		Rules:         t.PolicyRules,
		Impact:        p.impact,
		Limit:         t.Impact,
		Cumulative:    p.cumulative,
		InTransaction: in.InTransaction,
		NoLoginRole:   in.NoLoginRole,
		RowGone:       p.readCheck != nil && p.readCheck.Result == decide.ReadStale && p.readCheck.Version == nil,
	}
}

// hold stores p in stet.held_writes, unconfirmed, and returns the row's id, or
// "" when it was not stored: the write is refused all the same, and its error
// names no held write (spec 7). End makes the row pending once the refusal
// reached the client (spec-v0.2-approve 4).
//
// The row keeps what approving the write needs (spec-v0.2-approve 4): until
// when it can be approved, the session's settings its text values are read
// under, the version of its row or that the row was not there, and the rows
// it would touch. A write is stored only as it was sent, and as it would run:
// see unstorable for one that is not stored.
//
// An insert that reports an error may still have landed: past its deadline
// pgx stops waiting but does not cancel the insert on the server. Its id is
// kept in p.heldUnsure, and End marks it undelivered. A row that lands after
// that marking stays unconfirmed, which no one can approve.
func (g *Gate) hold(p *pending) string {
	h := &store.HeldWrite{
		ID:          store.NewID(),
		DecisionID:  p.id,
		CreatedAt:   p.at,
		LoginRole:   p.session.User,
		Database:    p.session.Database,
		Statement:   p.sql,
		Params:      p.params,
		ParamTypes:  p.oids,
		TargetTable: p.res.Table,
		TargetPK:    p.pk,
		Verdict:     p.verdict,
		Reasons:     p.reasons,
		ExpiresAt:   p.at.Add(g.cfg.Held.ExpireAfter),
		Settings: map[string]string{
			"TimeZone": p.session.TimeZone, "DateStyle": p.session.DateStyle, "IntervalStyle": p.session.IntervalStyle,
		},
	}
	h.PlannedRows, h.RowsExact = heldRows(p)
	if p.res.Kind == sqlparse.Gated && (p.res.Op == "update" || p.res.Op == "delete") {
		if x, ok := p.stateVersion["xmin"]; ok {
			h.RowVersion = &x
		} else {
			// Its state was read, and the row was not there: what the
			// reviewer sees is that there is no such row.
			h.RowAbsent = p.pk != nil && p.state != nil && p.stateErr == nil
		}
	}
	err := unstorable(p, h)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), heldTimeout)
		if err = g.insertHeld(ctx, h); err != nil {
			p.heldUnsure = h.ID
		}
		cancel()
	}
	if err != nil {
		g.log.Error("held write not stored", "decision", p.id, "table", p.res.Table, "login_role", p.session.User, "err", err)
		return ""
	}
	return h.ID
}

// unstorable says why h, the held write of p, cannot be stored as the write
// was sent, or returns nil. Approving a changed value would run another
// write, so one whose bind values were not read, or are text jsonb cannot
// hold as it is (not UTF-8, or with a NUL), is not stored. Nor is one whose
// session's settings are not known (spec-v0.2-approve 4): a text value such
// as a date means what TimeZone, DateStyle and IntervalStyle make of it. The
// server reports a SET's change of them only just before the ReadyForQuery
// that answers it, so after a SET or RESET it has not answered yet (one sent
// under the same Sync as the write, say) the settings the proxy last heard
// of may not be the ones the write would run under; and a setting the server
// never reported is not known at all.
func unstorable(p *pending, h *store.HeldWrite) error {
	if len(h.Params) != len(p.oids) {
		return errors.New("bind values not read")
	}
	if p.session.SetPending {
		return errors.New("a SET or RESET the server has not answered yet may have changed the session's settings")
	}
	for _, k := range []string{"TimeZone", "DateStyle", "IntervalStyle"} {
		if h.Settings[k] == "" {
			return fmt.Errorf("the server has not reported the session's %s", k)
		}
	}
	if !storableText(h.Statement) {
		return errors.New("statement is not UTF-8 text without NUL")
	}
	for i, v := range h.Params {
		if v != nil && !storableText(*v) {
			return fmt.Errorf("bind value $%d is not UTF-8 text without NUL", i+1)
		}
	}
	return nil
}

func storableText(s string) bool { return utf8.ValidString(s) && !strings.ContainsRune(s, 0) }

// heldRows is how many rows p was found to touch when it was held, and
// whether that number is exact: what approving it allows is measured by it
// (spec-v0.2-approve 4, 6). 1 for a single-row write; for a measured one, the
// rows it was measured at, exact when read from its syntax or counted under
// the cap; none for one that was not measured.
func heldRows(p *pending) (*int64, bool) {
	switch im := p.impact; {
	case p.res.Kind == sqlparse.Gated:
		return ptr(int64(1)), true
	case im != nil && im.Method != "" && im.Rows != nil:
		exact := im.Method == string(sqlparse.MethodSyntax) || (im.Method == string(sqlparse.MethodCount) && !im.Capped)
		return ptr(*im.Rows), exact
	}
	return nil, false
}

// textParams is ps as text when no value needs decoding, and nil otherwise.
func textParams(ps []proxy.Param) []*string {
	if len(ps) == 0 {
		return nil
	}
	out := make([]*string, len(ps))
	for i, p := range ps {
		switch {
		case p.Value == nil:
		case p.Format == 0:
			out[i] = ptr(string(p.Value))
		default:
			return nil
		}
	}
	return out
}

// refusalText is the first line of a refused write's error, as the proxy
// reports it in Outcome.Error.
func refusalText(r *proxy.Refusal) string { return "SQLSTATE " + r.Code + ": " + r.Message }

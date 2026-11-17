package bench

// This file reads what each arm's proxy recorded about the writes it was sent
// (docs/spec-v0.2-benchmark.md, chapter 7 "프록시가 남긴 것" and "막은 층"):
// Stet's stet.decisions, hoop's audit log and the LLM recorder's calls, each
// joined to the writes by the statement's tag, into lab.proxy.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/lab/tasks"
)

// Proxy is what a proxy recorded about one write, joined by its tag.
type Proxy struct {
	Arm, ID            string
	Source             string // stet | hoop
	Layer              string // deterministic | judgment | limit | failure; "" when the proxy let the write through
	Blocked            bool
	Escalated          bool     // Stet: the verdict was escalate
	Failure            bool     // the judgment failed and the failure setting decided
	Level              string   // hoop: low | medium | high
	AIStatus           string   // hoop
	Rule               string   // hoop: the rule that denied
	Verdict            string   // Stet
	Refused            string   // Stet: the SQLSTATE it answered with
	Reasons            []string // Stet
	CostUSD            float64
	ModelMS            int
	LLMCalls           int
	Title, Explanation string
}

// AuditEvent is one line of hoop's audit log (the fields the benchmark reads).
type AuditEvent struct {
	Kind      string            `json:"kind"` // statement | violation | session_start | ...
	Timestamp time.Time         `json:"timestamp"`
	SessionID string            `json:"session_id"`
	Principal string            `json:"principal"`
	Operation string            `json:"operation"`
	Statement string            `json:"statement"`
	Allowed   bool              `json:"allowed"`
	Rule      string            `json:"rule"`
	Message   string            `json:"message"`
	Direction string            `json:"direction"`
	Metadata  map[string]string `json:"metadata"` // risk_level, risk_action, ai_status, ai_rule
}

// Sent is a statement as it was sent, for joining without tags.
type Sent struct {
	ID, Tag, Text string
	At            time.Time
	LatencyMS     float64 // from At to its answer, as lab.outcomes has it
}

// joinSkew is how far a proxy's record of a write may lie outside the time the write was out: the proxies
// run on the sender's machine, but hoop's clock is a container's.
const joinSkew = 2 * time.Second

// window is the time a proxy's records of the write fall in: from joinSkew before it was sent to joinSkew
// after its answer. A record of the same tag outside it is another run's, or an abandoned attempt's.
func (s Sent) window() (from, to time.Time) {
	return s.At.Add(-joinSkew), s.At.Add(time.Duration(s.LatencyMS*float64(time.Millisecond)) + joinSkew)
}

func (s Sent) holds(t time.Time) bool {
	from, to := s.window()
	return !t.Before(from) && !t.After(to)
}

// runWindow is the run's window, from the earliest write's to the latest's; nothing sent holds nothing.
func runWindow(sent []Sent) func(time.Time) bool {
	if len(sent) == 0 {
		return func(time.Time) bool { return false }
	}
	from, to := sent[0].window()
	for _, s := range sent[1:] {
		f, t := s.window()
		from, to = minTime(from, f), maxTime(to, t)
	}
	return func(t time.Time) bool { return !t.Before(from) && !t.After(to) }
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Collected is what a collect found, kept in lab.meta under "collect:<arm>". The hoop counts are of the
// records joined to the arm's writes and of the untagged ones within the run's window, never the whole files.
type Collected struct {
	Join            string  `json:"join"`             // tag | text: how hoop's lines were joined to the writes
	LLMCalls        int     `json:"llm_calls"`        // hoop: the recorder's calls; one refused at the cap is none
	LLMCostUSD      float64 `json:"llm_cost_usd"`     // the model's cost: the recorder's calls, or Stet's decisions
	CapReached      bool    `json:"cap_reached"`      // hoop: a recorder line has event cap_reached
	BudgetExhausted bool    `json:"budget_exhausted"` // hoop: an audit line has ai_status budget_exhausted
	Unconfirmed     int     `json:"unconfirmed"`      // hoop: blocked outcomes whose write has no violation (Unconfirm)
	OutOfWindow     int     `json:"out_of_window"`    // records of the arm's writes left out: outside the write's window
}

// TagOf returns the 12 digits of a "/* w:... */" comment anywhere in text, or "".
func TagOf(text string) string {
	if m := recorderTagRE.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// ReadAudit returns the lines of a hoop audit log, in order; blank lines are skipped.
func ReadAudit(path string) ([]AuditEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var events []AuditEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20)
	for n := 1; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var e AuditEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return events, nil
}

// stetFailures are the reasons that mean Stet's judgment failed (plan B17): the model's error or absence,
// a state it could not read, a cumulative limit it could not count.
var stetFailures = []string{"model_error", "no_model", "state_error", "impact:cumulative_unknown"}

// StetLayer reads one decision.
func StetLayer(verdict string, reasons []string, refused string) (layer string, blocked, escalated, failure bool) {
	if verdict == "commit" && refused == "" {
		return "", false, false, false
	}
	escalated = verdict == "escalate"
	switch {
	case slices.ContainsFunc(reasons, func(r string) bool { return slices.Contains(stetFailures, r) }):
		return "failure", true, escalated, true
	case verdict == "unsupported" || refused == "ST002":
		return "deterministic", true, escalated, false
	case slices.ContainsFunc(reasons, func(r string) bool { return strings.HasPrefix(r, "impact:") }):
		return "limit", true, escalated, false
	}
	return "judgment", true, escalated, false
}

// HoopLayer reads one judgment line (kind statement or violation).
func HoopLayer(e AuditEvent) (layer string, blocked, failure bool) {
	if e.Allowed {
		return "", false, false
	}
	switch e.Metadata["ai_status"] {
	case "ok", "cached":
		return "judgment", true, false
	case "", "skipped":
		return "deterministic", true, false
	}
	return "failure", true, true
}

// statementHead is how much of a decision's statement is read for its tag: the sender puts the tag first,
// and a padded statement may be megabytes long.
const statementHead = 8192

// decision is a write's earliest decision in one database.
type decision struct {
	Proxy
	At time.Time // created_at: when Stet received the statement
	DB string
}

// readDecisions returns the earliest decision on each tag ids has in conn's database, in created_at order.
func readDecisions(ctx context.Context, conn *pgx.Conn, arm string, ids map[string]string) ([]decision, error) {
	db := conn.Config().Database
	rows, err := conn.Query(ctx, `select left(statement, $1), created_at, verdict, reasons, coalesce(refused, ''),
		coalesce(cost_usd, 0)::float8, coalesce(model_latency_ms, 0)
		from stet.decisions order by created_at, id`, statementHead)
	if err != nil {
		return nil, fmt.Errorf("read stet.decisions of %s: %w", db, err)
	}
	var out []decision
	seen := map[string]bool{}
	var head string
	var d decision
	_, err = pgx.ForEachRow(rows, []any{&head, &d.At, &d.Verdict, &d.Reasons, &d.Refused, &d.CostUSD, &d.ModelMS}, func() error {
		tag := TagOf(head)
		id, ok := ids[tag]
		if tag == "" || !ok || seen[id] {
			return nil
		}
		seen[id] = true
		r := d
		r.Reasons = slices.Clone(d.Reasons) // the next row scans into d
		r.Arm, r.ID, r.Source, r.DB = arm, id, "stet", db
		r.Layer, r.Blocked, r.Escalated, r.Failure = StetLayer(r.Verdict, r.Reasons, r.Refused)
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read stet.decisions of %s: %w", db, err)
	}
	return out, nil
}

// FromDecisions reads stet.decisions of conn's database; ids maps a tag to its write id.
//
// A decision is joined by its statement's tag; of several on one tag the earliest counts. A decision
// without a tag, or with one ids lacks, is dropped. conn must be able to read the stet schema: Stet's own
// account owns it (GateURL).
func FromDecisions(ctx context.Context, conn *pgx.Conn, arm string, ids map[string]string) ([]Proxy, error) {
	ds, err := readDecisions(ctx, conn, arm, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Proxy, len(ds))
	for i, d := range ds {
		out[i] = d.Proxy
	}
	return out, nil
}

// pickDecision chooses write w's decision among its earliest decisions in each of the databases read, ds:
// the one, when there is one. A tag decided in several databases is a step sent again, into a fresh clone,
// after a cut-off: the decision made within the write's window is chosen, and outside counts the others.
// When not exactly one was made within it, none is chosen.
func pickDecision(w Sent, ds []decision) (d decision, outside int, err error) {
	if len(ds) == 1 {
		return ds[0], 0, nil
	}
	var in []decision
	where := make([]string, len(ds))
	for i, d := range ds {
		if w.holds(d.At) {
			in = append(in, d)
		}
		where[i] = fmt.Sprintf("%s at %s", d.DB, d.At.UTC().Format(time.RFC3339Nano))
	}
	if len(in) != 1 {
		from, to := w.window()
		return decision{}, 0, fmt.Errorf("write %s (tag %s) has decisions in %s, and %d of them fall within the time "+
			"it was out (%s to %s): give only the databases its outcome was recorded from", w.ID, w.Tag,
			strings.Join(where, ", "), len(in), from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
	}
	return in[0], len(ds) - 1, nil
}

// FromAudit joins audit events and recorder calls to writes. byText joins by statement text and order instead of by tag.
// violations holds the ids of the writes that have a violation line.
//
// A record is joined to a write of its tag (or, byText, of its statement's text) only within the write's
// window (Sent.holds): the files may hold other runs of the same tags, a rerun appended to them, or an
// attempt abandoned and sent again. Records of the arm's writes outside their windows are left out and
// counted in c.OutOfWindow.
//
// Only client lines (direction client or none) of kind statement or violation are judgments. A write's
// judgment line is the one within its window; hoop writes a denied statement as one violation line and no
// statement line, so that line is both the judgment and the proof of the denial. A write with two judgment
// lines within its window is an error: none is picked. A write's recorder calls give it their count, cost and
// summed latency, and the title and explanation of the last call that chose a level; a call refused at the
// cost cap was never sent, and only sets c.CapReached. c's totals are of the records joined to the writes and
// of the untagged ones (byText: those of no write's text, too) within the run's window; c.Unconfirmed is left
// to Unconfirm. Rows come in the order of sent.
func FromAudit(arm string, events []AuditEvent, calls []Call, sent []Sent, byText bool) (rows []Proxy, violations map[string]bool, c Collected, err error) {
	c.Join = "tag"
	if byText {
		c.Join = "text"
	}
	key := func(statement string) string {
		if byText {
			return textKey(statement)
		}
		return TagOf(statement)
	}
	callKey := func(call Call) string {
		if !byText {
			return call.Tag
		}
		_, text, ok := strings.Cut(call.Input, "\n\n") // hoop's message: its headers, a blank line, the statement
		if !ok {
			return ""
		}
		return textKey(text)
	}
	// The writes of each tag or text, in the order sent. An empty text is no write's: a FunctionCall
	// message has none, and a line without a statement is not its by position.
	of := map[string][]Sent{}
	ordered := slices.Clone(sent)
	slices.SortStableFunc(ordered, func(a, b Sent) int { return a.At.Compare(b.At) })
	for _, s := range ordered {
		k := s.Tag
		if byText {
			k = textKey(s.Text)
		}
		if k != "" {
			of[k] = append(of[k], s)
		}
	}
	within := func(ws []Sent, t time.Time) []Sent {
		var in []Sent
		for _, w := range ws {
			if w.holds(t) {
				in = append(in, w)
			}
		}
		return in
	}
	inRun := runWindow(sent)

	var lines []AuditEvent
	for _, e := range events {
		if (e.Direction == "client" || e.Direction == "") && (e.Kind == "statement" || e.Kind == "violation") {
			lines = append(lines, e)
		}
	}
	slices.SortStableFunc(lines, func(a, b AuditEvent) int { return a.Timestamp.Compare(b.Timestamp) })
	violations = map[string]bool{}
	judged := map[string]AuditEvent{}
	for _, e := range lines {
		exhausted := e.Metadata["ai_status"] == "budget_exhausted"
		ws := of[key(e.Statement)]
		if len(ws) == 0 {
			if TagOf(e.Statement) == "" && inRun(e.Timestamp) && exhausted {
				c.BudgetExhausted = true
			}
			continue
		}
		in := within(ws, e.Timestamp)
		if len(in) == 0 {
			c.OutOfWindow++
			continue
		}
		i := slices.IndexFunc(in, func(w Sent) bool { _, ok := judged[w.ID]; return !ok })
		if i < 0 {
			w := in[0]
			return nil, nil, Collected{}, fmt.Errorf("write %s (tag %s) has two judgment lines in hoop's audit log "+
				"within the time it was out, at %s and %s: none is taken", w.ID, w.Tag,
				judged[w.ID].Timestamp.UTC().Format(time.RFC3339Nano), e.Timestamp.UTC().Format(time.RFC3339Nano))
		}
		judged[in[i].ID] = e
		if e.Kind == "violation" {
			violations[in[i].ID] = true
		}
		c.BudgetExhausted = c.BudgetExhausted || exhausted
	}

	calls = slices.Clone(calls)
	slices.SortStableFunc(calls, func(a, b Call) int { return a.Time.Compare(b.Time) })
	type usage struct {
		calls              int
		cost, ms           float64
		title, explanation string
	}
	used := map[string]*usage{}
	for _, call := range calls {
		var id string
		if ws := of[callKey(call)]; len(ws) > 0 {
			in := within(ws, call.Time)
			if len(in) == 0 {
				c.OutOfWindow++
				continue
			}
			// Of writes of one text out at once, the call is the latest's sent before it.
			w := in[0]
			for _, s := range in[1:] {
				if !s.At.After(call.Time) {
					w = s
				}
			}
			id = w.ID
		} else if call.Tag != "" || !inRun(call.Time) {
			continue // another arm's write, or another run's
		}
		if call.Event == "cap_reached" {
			c.CapReached = true // refused by the recorder: never sent
			continue
		}
		c.LLMCalls++
		c.LLMCostUSD += call.CostUSD
		if id == "" {
			continue
		}
		u := used[id]
		if u == nil {
			u = &usage{}
			used[id] = u
		}
		u.calls++
		u.cost += call.CostUSD
		u.ms += call.LatencyMS
		if call.Level != "" {
			u.title, u.explanation = call.Title, call.Explanation
		}
	}

	for _, s := range sent {
		e, ok := judged[s.ID]
		if !ok {
			continue
		}
		delete(judged, s.ID)
		p := Proxy{Arm: arm, ID: s.ID, Source: "hoop", Level: e.Metadata["risk_level"], AIStatus: e.Metadata["ai_status"],
			Rule: e.Rule}
		p.Layer, p.Blocked, p.Failure = HoopLayer(e)
		if u := used[s.ID]; u != nil {
			p.LLMCalls, p.CostUSD, p.ModelMS = u.calls, u.cost, int(math.Round(u.ms))
			p.Title, p.Explanation = u.title, u.explanation
		}
		rows = append(rows, p)
	}
	return rows, violations, c, nil
}

// leadingTagRE is a tag comment at the start of a text.
var leadingTagRE = regexp.MustCompile(`^/\* w:\d{12} \*/`)

// textKey is the text a join by text compares: without its leading tag comment and the spaces around.
func textKey(text string) string {
	return strings.TrimSpace(leadingTagRE.ReplaceAllString(strings.TrimSpace(text), ""))
}

// proxyCols are lab.proxy's columns in Proxy's field order.
var proxyCols = []string{"arm", "id", "source", "layer", "blocked", "escalated", "failure", "level", "ai_status", "rule",
	"verdict", "refused", "reasons", "cost_usd", "model_ms", "llm_calls", "title", "explanation"}

// SaveProxy stores rows in lab.proxy.
func SaveProxy(ctx context.Context, conn *pgx.Conn, rows []Proxy) error {
	data := make([][]any, len(rows))
	for i, p := range rows {
		reasons := p.Reasons
		if reasons == nil {
			reasons = []string{}
		}
		data[i] = []any{p.Arm, p.ID, p.Source, p.Layer, p.Blocked, p.Escalated, p.Failure, p.Level, p.AIStatus, p.Rule,
			p.Verdict, p.Refused, reasons, p.CostUSD, p.ModelMS, p.LLMCalls, p.Title, p.Explanation}
	}
	if _, err := conn.CopyFrom(ctx, pgx.Identifier{"lab", "proxy"}, proxyCols, pgx.CopyFromRows(data)); err != nil {
		return fmt.Errorf("save lab.proxy: %w", err)
	}
	return nil
}

// Unconfirm turns the arm's blocked outcomes whose write has no violation into unknown and returns how many.
// An outcome without a tag (the FunctionCall message) cannot be confirmed either way and is left as it is.
//
// It follows the log it is given, so that a collect run again with a fuller log puts things right: an
// outcome it turned unknown before (an unknown with FATAL 42501, which the sender records only as blocked)
// is blocked again once its write has a violation, and the count is of all the arm's outcomes left
// unknown this way.
func Unconfirm(ctx context.Context, conn *pgx.Conn, arm string, violations map[string]bool) (int, error) {
	ids := []string{}
	for id, v := range violations {
		if v {
			ids = append(ids, id)
		}
	}
	rows, err := conn.Query(ctx, `update lab.outcomes
		set result = case when id = any($2) then 'blocked' else 'unknown' end
		where arm = $1 and tag <> ''
		  and (result = 'blocked' or (result = 'unknown' and severity = 'FATAL' and sqlstate = '42501'))
		returning result`, arm, ids)
	if err != nil {
		return 0, fmt.Errorf("unconfirm arm %s: %w", arm, err)
	}
	results, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("unconfirm arm %s: %w", arm, err)
	}
	n := 0
	for _, r := range results {
		if r == Unknown {
			n++
		}
	}
	return n, nil
}

// GateURL reaches db as Stet's account, which owns the stet schema there.
func GateURL(addr string, n Names, db string) string {
	return pgURL(addr, n.Gate(), GatePassword, db)
}

// sentOf returns the arm's writes as recorded in lab.outcomes, in the order sent, each with the time it was
// out and the text it went out with (sentText; bind: the arm sends bound values).
func sentOf(ctx context.Context, conn *pgx.Conn, arm string, bind bool) ([]Sent, error) {
	rows, err := conn.Query(ctx, `select o.id, o.tag, o.sent_at, o.latency_ms, w.sql, w.inline, w.wire
		from lab.outcomes o join lab.writes w on w.id = o.id
		where o.arm = $1 order by o.sent_at, o.id`, arm)
	if err != nil {
		return nil, err
	}
	sent, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Sent, error) {
		var s Sent
		var sql, inline, wire string
		err := r.Scan(&s.ID, &s.Tag, &s.At, &s.LatencyMS, &sql, &inline, &wire)
		s.Text = sentText(sql, inline, wire, bind)
		return s, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the outcomes of arm %s: %w", arm, err)
	}
	return sent, nil
}

// sentText is the text sendOne sends for a write, untagged: its wire, if any, overrides the arm's bind; a
// FunctionCall carries no text, and a padded text's @PAD@ is left unwritten.
func sentText(sql, inline, wire string, bind bool) string {
	switch wire {
	case tasks.WireFunctionCall:
		return ""
	case tasks.WireCopy, tasks.WireBind:
		return sql
	case tasks.WireSimple:
		return inline
	}
	if bind {
		return sql
	}
	return inline
}

// noneJoined is the error of a collect that joined a record of the proxy to none of the arm's writes: they did
// not go through the arm's proxy (bench send -addr was the database server), or the records given are not this
// run's. Nothing is stored then.
func noneJoined(arm, records string, sent []Sent) error {
	tagged := 0
	for _, s := range sent {
		if s.Tag != "" {
			tagged++
		}
	}
	return fmt.Errorf("%s joined none of arm %s's writes: 0 of %d writes sent (%d tagged) have a record. Were they "+
		"sent through the arm's proxy, and are these the records of this run? nothing was stored", records, arm,
		len(sent), tagged)
}

// CollectHoop joins hoop's audit events and the recorder's calls to the arm's writes as recorded in
// lab.outcomes of conn, the arm's own database (bind: the arm sends bound values; byText: join by text and
// order, see FromAudit), and stores the result there in one transaction: the arm's rows of lab.proxy are
// replaced, Unconfirm runs with the violations within the writes' windows, and Collected goes to lab.meta.
// When FromAudit refuses, or no judgment line joined any of the writes, nothing is stored.
func CollectHoop(ctx context.Context, conn *pgx.Conn, arm string, bind bool, events []AuditEvent, calls []Call, byText bool) ([]Proxy, Collected, error) {
	sent, err := sentOf(ctx, conn, arm, bind)
	if err != nil {
		return nil, Collected{}, err
	}
	rows, violations, c, err := FromAudit(arm, events, calls, sent, byText)
	if err != nil {
		return nil, Collected{}, err
	}
	if len(rows) == 0 {
		return nil, Collected{}, noneJoined(arm, "hoop's audit log", sent)
	}
	c, err = storeCollected(ctx, conn, arm, rows, violations, c)
	return rows, c, err
}

// CollectStet reads stet.decisions through each of decisions, the arm's own database first and then its X
// clones, joins them by tag to the arm's writes as recorded in lab.outcomes of conn, the arm's own
// database, and stores the result there in one transaction: the arm's rows of lab.proxy are replaced and
// Collected goes to lab.meta. A write decided in several of the databases takes the decision made within its
// window (pickDecision); when not exactly one was, it is an error and nothing is stored. When no decision joined
// any of the writes, nothing is stored either.
func CollectStet(ctx context.Context, conn *pgx.Conn, arm string, decisions []*pgx.Conn) ([]Proxy, Collected, error) {
	sent, err := sentOf(ctx, conn, arm, false)
	if err != nil {
		return nil, Collected{}, err
	}
	ids := map[string]string{}
	for _, s := range sent {
		if s.Tag != "" {
			ids[s.Tag] = s.ID
		}
	}
	of := map[string][]decision{} // write id -> its earliest decision in each database
	for _, d := range decisions {
		ds, err := readDecisions(ctx, d, arm, ids)
		if err != nil {
			return nil, Collected{}, err
		}
		for _, x := range ds {
			of[x.ID] = append(of[x.ID], x)
		}
	}
	var rows []Proxy
	c := Collected{Join: "tag"}
	for _, s := range sent {
		ds := of[s.ID]
		if len(ds) == 0 {
			continue
		}
		d, outside, err := pickDecision(s, ds)
		if err != nil {
			return nil, Collected{}, err
		}
		c.OutOfWindow += outside
		c.LLMCostUSD += d.CostUSD
		rows = append(rows, d.Proxy)
	}
	if len(rows) == 0 {
		return nil, Collected{}, noneJoined(arm, "stet.decisions", sent)
	}
	c, err = storeCollected(ctx, conn, arm, rows, nil, c)
	return rows, c, err
}

// storeCollected replaces the arm's rows of lab.proxy with rows, runs Unconfirm when violations is not nil
// (keeping its count in c), and writes c to lab.meta as collect:<arm>, in one transaction.
func storeCollected(ctx context.Context, conn *pgx.Conn, arm string, rows []Proxy, violations map[string]bool, c Collected) (Collected, error) {
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "delete from lab.proxy where arm = $1", arm); err != nil {
			return fmt.Errorf("clear the rows of arm %s in lab.proxy: %w", arm, err)
		}
		if err := SaveProxy(ctx, tx.Conn(), rows); err != nil {
			return err
		}
		if violations != nil {
			n, err := Unconfirm(ctx, tx.Conn(), arm, violations)
			if err != nil {
				return err
			}
			c.Unconfirmed = n
		}
		value, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `insert into lab.meta (key, value) values ($1, $2)
			on conflict (key) do update set value = excluded.value`, "collect:"+arm, string(value)); err != nil {
			return fmt.Errorf("write collect:%s to lab.meta: %w", arm, err)
		}
		return nil
	})
	return c, err
}

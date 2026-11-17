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
}

// Collected is what a collect found, kept in lab.meta under "collect:<arm>".
type Collected struct {
	Join            string  `json:"join"`             // tag | text: how hoop's lines were joined to the writes
	LLMCalls        int     `json:"llm_calls"`        // hoop: the lines of the recorder's files
	LLMCostUSD      float64 `json:"llm_cost_usd"`     // the model's cost: the recorder's calls, or Stet's decisions
	CapReached      bool    `json:"cap_reached"`      // hoop: a recorder line has event cap_reached
	BudgetExhausted bool    `json:"budget_exhausted"` // hoop: an audit line has ai_status budget_exhausted
	Unconfirmed     int     `json:"unconfirmed"`      // hoop: blocked outcomes whose write has no violation (Unconfirm)
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
	return events, sc.Err()
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

// FromDecisions reads stet.decisions of conn's database; ids maps a tag to its write id.
//
// A decision is joined by its statement's tag; of several on one tag the earliest counts. A decision
// without a tag, or with one ids lacks, is dropped. conn must be able to read the stet schema: Stet's own
// account owns it (GateURL).
func FromDecisions(ctx context.Context, conn *pgx.Conn, arm string, ids map[string]string) ([]Proxy, error) {
	rows, err := conn.Query(ctx, `select left(statement, $1), verdict, reasons, coalesce(refused, ''),
		coalesce(cost_usd, 0)::float8, coalesce(model_latency_ms, 0)
		from stet.decisions order by created_at, id`, statementHead)
	if err != nil {
		return nil, fmt.Errorf("read stet.decisions of %s: %w", conn.Config().Database, err)
	}
	var out []Proxy
	seen := map[string]bool{}
	var head string
	var p Proxy
	_, err = pgx.ForEachRow(rows, []any{&head, &p.Verdict, &p.Reasons, &p.Refused, &p.CostUSD, &p.ModelMS}, func() error {
		tag := TagOf(head)
		id, ok := ids[tag]
		if tag == "" || !ok || seen[id] {
			return nil
		}
		seen[id] = true
		r := p
		r.Reasons = slices.Clone(p.Reasons) // the next row scans into p
		r.Arm, r.ID, r.Source = arm, id, "stet"
		r.Layer, r.Blocked, r.Escalated, r.Failure = StetLayer(r.Verdict, r.Reasons, r.Refused)
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read stet.decisions of %s: %w", conn.Config().Database, err)
	}
	return out, nil
}

// FromAudit joins audit events and recorder calls to writes. byText joins by statement text and order instead of by tag.
// violations holds the ids of the writes that have a violation line.
//
// Only client lines (direction client or none) count. A write's judgment line is its first statement or
// violation line in time order; hoop writes a denied statement as one violation line and no statement line,
// so that line is both the judgment and the proof of the denial. Lines and calls no write is found for are
// dropped. A write's recorder calls give it their count, cost and summed latency, and the title and
// explanation of the last call that chose a level. Rows come in the order of sent.
func FromAudit(arm string, events []AuditEvent, calls []Call, sent []Sent, byText bool) (rows []Proxy, violations map[string]bool, budgetExhausted bool) {
	var lines []AuditEvent
	for _, e := range events {
		if e.Direction != "client" && e.Direction != "" {
			continue
		}
		if e.Metadata["ai_status"] == "budget_exhausted" {
			budgetExhausted = true
		}
		if e.Kind == "statement" || e.Kind == "violation" {
			lines = append(lines, e)
		}
	}
	slices.SortStableFunc(lines, func(a, b AuditEvent) int { return a.Timestamp.Compare(b.Timestamp) })
	calls = slices.Clone(calls)
	slices.SortStableFunc(calls, func(a, b Call) int { return a.Time.Compare(b.Time) })

	var lineIDs, callIDs []string // the write of each line and call; "" for none
	if byText {
		lineIDs = joinByText(sent, len(lines), func(i int) (string, bool) { return lines[i].Statement, true })
		callIDs = joinByText(sent, len(calls), func(i int) (string, bool) {
			_, text, ok := strings.Cut(calls[i].Input, "\n\n") // hoop's message: its headers, a blank line, the statement
			return text, ok
		})
	} else {
		byTag := map[string]string{}
		for _, s := range sent {
			if s.Tag != "" {
				byTag[s.Tag] = s.ID
			}
		}
		for _, e := range lines {
			lineIDs = append(lineIDs, byTag[TagOf(e.Statement)])
		}
		for _, c := range calls {
			callIDs = append(callIDs, byTag[c.Tag])
		}
	}

	violations = map[string]bool{}
	judged := map[string]AuditEvent{}
	for i, e := range lines {
		id := lineIDs[i]
		if id == "" {
			continue
		}
		if e.Kind == "violation" {
			violations[id] = true
		}
		if _, ok := judged[id]; !ok {
			judged[id] = e
		}
	}
	type usage struct {
		calls              int
		cost, ms           float64
		title, explanation string
	}
	used := map[string]*usage{}
	for i, c := range calls {
		id := callIDs[i]
		if id == "" {
			continue
		}
		u := used[id]
		if u == nil {
			u = &usage{}
			used[id] = u
		}
		u.calls++
		u.cost += c.CostUSD
		u.ms += c.LatencyMS
		if c.Level != "" {
			u.title, u.explanation = c.Title, c.Explanation
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
	return rows, violations, budgetExhausted
}

// leadingTagRE is a tag comment at the start of a text.
var leadingTagRE = regexp.MustCompile(`^/\* w:\d{12} \*/`)

// textKey is the text a join by text compares: without its leading tag comment and the spaces around.
func textKey(text string) string {
	return strings.TrimSpace(leadingTagRE.ReplaceAllString(strings.TrimSpace(text), ""))
}

// joinByText gives each of n items, in time order, a write id: the k-th item whose text is t goes to the
// k-th write sent with text t (by Sent.At). An item without a text (text returns false), or one beyond the
// writes sent with its text, gets "".
func joinByText(sent []Sent, n int, text func(i int) (string, bool)) []string {
	ordered := slices.Clone(sent)
	slices.SortStableFunc(ordered, func(a, b Sent) int { return a.At.Compare(b.At) })
	queue := map[string][]string{}
	for _, s := range ordered {
		k := textKey(s.Text)
		queue[k] = append(queue[k], s.ID)
	}
	ids := make([]string, n)
	for i := range n {
		t, ok := text(i)
		if !ok {
			continue
		}
		k := textKey(t)
		if q := queue[k]; len(q) > 0 {
			ids[i], queue[k] = q[0], q[1:]
		}
	}
	return ids
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

// sentOf returns the arm's writes as recorded in lab.outcomes, in the order sent, each with the text it
// went out with (sentText; bind: the arm sends bound values).
func sentOf(ctx context.Context, conn *pgx.Conn, arm string, bind bool) ([]Sent, error) {
	rows, err := conn.Query(ctx, `select o.id, o.tag, o.sent_at, w.sql, w.inline, w.wire
		from lab.outcomes o join lab.writes w on w.id = o.id
		where o.arm = $1 order by o.sent_at, o.id`, arm)
	if err != nil {
		return nil, err
	}
	sent, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Sent, error) {
		var s Sent
		var sql, inline, wire string
		err := r.Scan(&s.ID, &s.Tag, &s.At, &sql, &inline, &wire)
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

// CollectHoop joins hoop's audit events and the recorder's calls to the arm's writes as recorded in
// lab.outcomes of conn, the arm's own database (bind: the arm sends bound values; byText: join by text and
// order, see FromAudit), and stores the result there in one transaction: the arm's rows of lab.proxy are
// replaced, Unconfirm runs, and Collected goes to lab.meta.
func CollectHoop(ctx context.Context, conn *pgx.Conn, arm string, bind bool, events []AuditEvent, calls []Call, byText bool) ([]Proxy, Collected, error) {
	sent, err := sentOf(ctx, conn, arm, bind)
	if err != nil {
		return nil, Collected{}, err
	}
	rows, violations, budget := FromAudit(arm, events, calls, sent, byText)
	c := Collected{Join: "tag", LLMCalls: len(calls), BudgetExhausted: budget}
	if byText {
		c.Join = "text"
	}
	for _, call := range calls {
		c.LLMCostUSD += call.CostUSD
		c.CapReached = c.CapReached || call.Event == "cap_reached"
	}
	c, err = storeCollected(ctx, conn, arm, rows, violations, c)
	return rows, c, err
}

// CollectStet reads stet.decisions through each of decisions, the arm's own database first and then its X
// clones, joins them by tag to the arm's writes as recorded in lab.outcomes of conn, the arm's own
// database, and stores the result there in one transaction: the arm's rows of lab.proxy are replaced and
// Collected goes to lab.meta. A write decided in two of the databases is an error, and nothing is stored.
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
	var rows []Proxy
	from := map[string]string{} // write id -> the database its decision came from
	c := Collected{Join: "tag"}
	for _, d := range decisions {
		db := d.Config().Database
		got, err := FromDecisions(ctx, d, arm, ids)
		if err != nil {
			return nil, Collected{}, err
		}
		for _, p := range got {
			if other, ok := from[p.ID]; ok {
				return nil, Collected{}, fmt.Errorf("write %s has decisions in %s and in %s: give only the databases "+
					"its outcome was recorded from", p.ID, other, db)
			}
			from[p.ID] = db
			c.LLMCostUSD += p.CostUSD
			rows = append(rows, p)
		}
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

package bench

// This file compares the tables after the undo with what they were, and finds for each row that is not what it was
// the reason the system reported (CheckUndo, undo.go).

import (
	"cmp"
	"slices"
	"strings"
	"time"
)

// undoHistory is what the check reads from stet.history, as the reviewer.
type undoHistory struct {
	sets    []SetCount
	changes map[string]map[string][]keyChange // table key -> key -> its captured changes, in time order
	markers map[string][]marker               // table key -> its sets that hold no values, in time order
}

// keyChange is one captured change of a key: the agent's, recorded at at (its set's time).
type keyChange struct {
	agent string
	at    time.Time
}

// marker is a change set that holds no values: its statement's rows on the table were not captured.
type marker struct {
	agent, skipped string
	at             time.Time
}

func (m marker) code() string { return m.agent + ": not_captured " + m.skipped }

func (m marker) describe() string {
	return m.code() + " at " + m.at.UTC().Format(time.RFC3339Nano)
}

// agentLine is a line of one agent's undo.
type agentLine struct {
	agent string
	UndoLine
}

func (l agentLine) code() string     { return l.agent + ": " + l.Code() }
func (l agentLine) describe() string { return l.agent + ": " + l.text() }

// explanation accounts for a row that is not what it was: reason is codes, by which rows are counted ("" when
// nothing accounts for the row), detail the whole text. asReported are the lines among those that account for it
// which the check takes as stet.undo reported them although they may depend on the order it undid the agents in
// (orderDependent).
type explanation struct {
	reason, detail string
	asReported     []string
}

// orderDependent reports whether a line is one the check takes as reported although another order of undoing the
// agents might have restored the key: a foreign key or unique value between rows of two agents (failed 23503,
// 23505, skipped referenced), and failed 0A000, which a foreign key between columns of different types gives.
func orderDependent(l UndoLine) bool {
	switch l.Code() {
	case "failed SQLSTATE 23503", "failed SQLSTATE 23505", "failed SQLSTATE 0A000", "skipped referenced":
		return true
	}
	return false
}

// explainer finds, for a row that is not what it was, the reason the system reported.
type explainer struct {
	lines map[string]map[string][]agentLine // table key -> key -> its lines
	// withheld: table key -> agent -> its lines without a key other than not_captured. stet.undo returns such a
	// line (table_gone, SQLSTATE 42501) for a table it withheld whole, in place of a line for each of its keys.
	withheld   map[string]map[string][]agentLine
	h          undoHistory
	uncaptured []string       // the roles that sent statements and are not captured
	order      map[string]int // agent -> its place in the order undone
}

func newExplainer(undone []AgentUndo, h undoHistory, uncaptured []string) explainer {
	e := explainer{lines: map[string]map[string][]agentLine{}, withheld: map[string]map[string][]agentLine{}, h: h,
		uncaptured: uncaptured, order: map[string]int{}}
	for i, u := range undone {
		e.order[u.Agent] = i
		for _, l := range u.Lines {
			al := agentLine{agent: u.Agent, UndoLine: l}
			if l.Key == nil {
				// A set without values is a marker, read with its time from the history.
				if l.Outcome != "not_captured" {
					if e.withheld[l.Table] == nil {
						e.withheld[l.Table] = map[string][]agentLine{}
					}
					e.withheld[l.Table][u.Agent] = append(e.withheld[l.Table][u.Agent], al)
				}
				continue
			}
			if e.lines[l.Table] == nil {
				e.lines[l.Table] = map[string][]agentLine{}
			}
			e.lines[l.Table][*l.Key] = append(e.lines[l.Table][*l.Key], al)
		}
	}
	return e
}

// explain accounts for a row of t, by key, that is not what it was, with evidence that something affecting it was
// not undone, or could not be; a line alone is evidence only when it reports exactly that. In order:
//
//   - The table is not captured: nothing of it is undone.
//   - What stet.undo returned must match the history (spec 7: one decision per selected key). Every agent with a
//     captured change of the key has a line of the key, or a line without a key withholding the table (table_gone,
//     SQLSTATE 42501); every line of the key has a captured change of it by its agent. Otherwise the row is
//     unexplained: a decision missing or without its change is a broken contract, which is what the check is for.
//   - A line that reports the key's changes could not be undone accounts for it: failed, restored_differs, the
//     refusals by design (referenced, key_changed, columns_changed, a SQLSTATE), and a line withholding the table
//     from the undo of an agent that changed the key.
//   - A line saying another party changed the row is checked against the history. changed_since needs, after the
//     agent's last change of the key, a change that was left undone: a set without values on the table, a change of
//     the key by an agent whose decision did not restore it (skipped other than no_net_change, failed, its table
//     withheld, or restored_differs undone before this agent), or a statement sent as a role that is not captured.
//     interleaved needs, between the agent's first and last change, a change of the key by another agent, a set
//     without values on the table, or such a statement. A line without its evidence leaves the row unexplained,
//     whatever else is there: the difference may come from the order the check undid the agents in, or from a
//     defect.
//   - With no such line (only restored or no_net_change, which say nothing is left to undo, or no captured change
//     at all): a set without values on the table before the key's first captured change (the row came back to what
//     that statement made it; any such set when there is no captured change), or a statement sent as a role that is
//     not captured.
//   - Otherwise nothing accounts for the row.
func (e explainer) explain(t compared, key string) explanation {
	if t.key == "" {
		return explanation{reason: "table not captured", detail: "table not captured: nothing of it is undone"}
	}
	lines := e.lines[t.key][key]
	changes := e.h.changes[t.key][key]
	var broken []string
	for _, agent := range changers(changes) {
		if _, ok := e.line(t.key, key, agent); !ok && len(e.withheld[t.key][agent]) == 0 {
			broken = append(broken, "a captured change of "+agent+" that its undo returned no line for")
		}
	}
	for _, l := range lines {
		if _, _, ok := e.span(changes, l.agent); !ok {
			broken = append(broken, l.describe()+", but the history holds no captured change of the key by "+l.agent)
		}
	}
	if len(broken) > 0 {
		return explanation{detail: strings.Join(broken, "; ")}
	}

	var codes, texts, unsupported, asReported []string
	accept := func(code, text string) { codes, texts = append(codes, code), append(texts, text) }
	for _, l := range lines {
		first, last, _ := e.span(changes, l.agent)
		switch reason := l.Code(); reason {
		case "restored", "skipped no_net_change":
		case "skipped changed_since":
			if ev := e.afterLast(t.key, key, l.agent, last); len(ev) > 0 {
				accept(l.code(), l.describe()+" (left undone after its last change: "+strings.Join(ev, "; ")+")")
			} else {
				unsupported = append(unsupported, l.describe()+", but nothing after its last change was left undone")
			}
		case "skipped interleaved":
			if ev := e.between(t.key, key, l.agent, first, last); len(ev) > 0 {
				accept(l.code(), l.describe()+" (between its changes: "+strings.Join(ev, "; ")+")")
			} else {
				unsupported = append(unsupported, l.describe()+", but nothing else changed the key between its changes")
			}
		default:
			accept(l.code(), l.describe())
			if orderDependent(l.UndoLine) {
				asReported = append(asReported, l.describe())
			}
		}
	}
	if len(unsupported) > 0 {
		return explanation{detail: strings.Join(unsupported, "; ")}
	}
	for _, agent := range changers(changes) {
		if _, ok := e.line(t.key, key, agent); !ok {
			for _, l := range e.withheld[t.key][agent] {
				accept(l.code(), l.describe())
			}
		}
	}
	if len(codes) == 0 {
		for _, m := range e.h.markers[t.key] {
			if len(changes) == 0 || m.at.Before(changes[0].at) {
				accept(m.code(), m.describe())
			}
		}
	}
	if len(codes) == 0 && len(e.uncaptured) > 0 {
		s := e.uncapturedRoles()
		accept("changed by "+s, "changed by "+s)
	}
	if len(codes) == 0 {
		if len(changes) == 0 {
			return explanation{detail: "no captured change of the key, no set without values on its table, no statement of a " +
				"role that is not captured"}
		}
		var said []string
		for _, l := range lines {
			said = append(said, l.describe())
		}
		return explanation{detail: "its lines say nothing is left to undo (" + strings.Join(said, "; ") + "), and no set " +
			"without values on its table came before its first change, no statement of a role that is not captured"}
	}
	slices.Sort(codes)
	slices.Sort(texts)
	return explanation{strings.Join(slices.Compact(codes), "; "), strings.Join(slices.Compact(texts), "; "), asReported}
}

// changers is the agents of changes, each once, in the order of their first change.
func changers(changes []keyChange) []string {
	var out []string
	for _, c := range changes {
		if !slices.Contains(out, c.agent) {
			out = append(out, c.agent)
		}
	}
	return out
}

// span is the times of agent's first and last change among changes; ok is false when it has none.
func (e explainer) span(changes []keyChange, agent string) (first, last time.Time, ok bool) {
	for _, c := range changes {
		if c.agent != agent {
			continue
		}
		if !ok {
			first, ok = c.at, true
		}
		last = c.at
	}
	return first, last, ok
}

// afterLast is the evidence that a change of the key after last, agent's last change of it, was left undone.
func (e explainer) afterLast(table, key, agent string, last time.Time) []string {
	var ev []string
	for _, m := range e.h.markers[table] {
		if !m.at.Before(last) {
			ev = append(ev, m.describe())
		}
	}
	seen := map[string]bool{}
	for _, c := range e.h.changes[table][key] {
		if c.agent == agent || !c.at.After(last) || seen[c.agent] {
			continue
		}
		seen[c.agent] = true
		if l, ok := e.line(table, key, c.agent); !ok {
			for _, w := range e.withheld[table][c.agent] {
				ev = append(ev, c.agent+"'s later change, whose table its undo withheld: "+w.describe())
			}
		} else if l.Outcome == "restored_differs" && e.order[c.agent] < e.order[agent] ||
			l.Outcome != "restored" && l.Outcome != "restored_differs" && l.Code() != "skipped no_net_change" {
			ev = append(ev, c.agent+"'s later change: "+l.describe())
		}
	}
	if len(e.uncaptured) > 0 {
		ev = append(ev, "a statement of "+e.uncapturedRoles())
	}
	return ev
}

// between is the evidence that something other than agent changed the key between first and last, agent's first
// and last change of it.
func (e explainer) between(table, key, agent string, first, last time.Time) []string {
	var ev []string
	for _, m := range e.h.markers[table] {
		if m.at.After(first) && m.at.Before(last) {
			ev = append(ev, m.describe())
		}
	}
	seen := map[string]bool{}
	for _, c := range e.h.changes[table][key] {
		if c.agent != agent && c.at.After(first) && c.at.Before(last) && !seen[c.agent] {
			seen[c.agent] = true
			ev = append(ev, c.agent+"'s change")
		}
	}
	if len(e.uncaptured) > 0 {
		ev = append(ev, "a statement of "+e.uncapturedRoles())
	}
	return ev
}

// line is agent's line for the key.
func (e explainer) line(table, key, agent string) (agentLine, bool) {
	for _, l := range e.lines[table][key] {
		if l.agent == agent {
			return l, true
		}
	}
	return agentLine{}, false
}

func (e explainer) uncapturedRoles() string {
	return "a role not captured (" + strings.Join(e.uncaptured, ", ") + ")"
}

// compareTables compares each table's rows after with those before, key by key.
func compareTables(tables []compared, before, after map[string]map[string]string, e explainer) []TableCompare {
	out := make([]TableCompare, 0, len(tables))
	for _, t := range tables {
		b, a := before[t.display], after[t.display]
		tc := TableCompare{Table: t.display, Key: t.key, Captured: t.key != "", Rows: len(b), Reasons: map[string]int{}}
		diff := func(key, kind string) {
			x := e.explain(t, key)
			tc.Diffs = append(tc.Diffs, RowDiff{Key: key, Kind: kind, Reason: x.reason, Detail: x.detail, AsReported: x.asReported})
			tc.Reasons[x.reason]++
			switch kind {
			case "different":
				tc.Different++
			case "missing":
				tc.Missing++
			case "extra":
				tc.Extra++
			}
		}
		for k, hb := range b {
			switch ha, ok := a[k]; {
			case !ok:
				diff(k, "missing")
			case ha != hb:
				diff(k, "different")
			default:
				tc.Equal++
			}
		}
		for k := range a {
			if _, ok := b[k]; !ok {
				diff(k, "extra")
			}
		}
		slices.SortFunc(tc.Diffs, func(x, y RowDiff) int { return keyOrder(x.Key, y.Key) })
		out = append(out, tc)
	}
	return out
}

// keyOrder orders keys as numbers when both are digits, else as text.
func keyOrder(a, b string) int {
	if digits(a) && digits(b) {
		return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
	}
	return strings.Compare(a, b)
}

func digits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

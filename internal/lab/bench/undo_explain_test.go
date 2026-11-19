package bench

import (
	"strings"
	"testing"
	"time"
)

// TestExplainerAcceptsOnlyEvidenceOfWhatWasNotUndone covers every way explain accounts for a row that is not what
// it was, and every way it does not. A line is evidence by itself only when it reports that the key's changes
// could not be undone (failed, restored_differs, a refusal by design such as referenced or key_changed). A line
// saying another party changed the row (changed_since, interleaved) counts only when the history shows such a
// change that was not undone: a set without values on the table, a change by an agent whose own line did not
// restore it, a statement sent as a role that is not captured. no_net_change and restored say nothing remains to
// undo, so a row they leave different needs a set without values before the key's first change.
func TestExplainerAcceptsOnlyEvidenceOfWhatWasNotUndone(t *testing.T) {
	base := time.Date(2026, 11, 19, 5, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return base.Add(time.Duration(s * float64(time.Second))) }
	text := func(s string) *string { return &s }
	contacts := compared{display: "public.contacts", key: "contacts"}
	accounts := compared{display: "public.accounts"}

	type change struct {
		agent string
		at    float64
	}
	type line struct {
		agent, outcome, reason string // reason "" is NULL
		keyless                bool
		key                    string // "7" when empty
	}
	for _, c := range []struct {
		name       string
		table      compared // contacts when empty
		order      []string // the agents in the order undone
		changes    []change // of key 7
		markers    []marker // sets without values on contacts
		lines      []line   // of key 7 unless keyless
		uncaptured []string
		reason     string // "" is unexplained
		detail     string // a part of the detail
		once       string // a part of the detail that is there once
		reported   int    // lines accepted as reported that may depend on the undo order (failed 23503, 23505, 0A000; referenced)
	}{
		{name: "a table that is not captured", table: accounts, reason: "table not captured"},

		// Lines that are evidence by themselves.
		{name: "failed", order: []string{"a"}, changes: []change{{"a", 1}},
			lines:  []line{{agent: "a", outcome: "failed", reason: "SQLSTATE 23503: insert or update on table \"deal_notes\" violates foreign key constraint \"f\""}},
			reason: "a: failed SQLSTATE 23503", detail: `a: failed SQLSTATE 23503: insert or update on table "deal_notes"`,
			reported: 1},
		{name: "failed 0A000", order: []string{"a"}, changes: []change{{"a", 1}},
			lines:  []line{{agent: "a", outcome: "failed", reason: "SQLSTATE 0A000: the foreign key pairs columns of different types"}},
			reason: "a: failed SQLSTATE 0A000", reported: 1},
		{name: "failed for another reason", order: []string{"a"}, changes: []change{{"a", 1}},
			lines:  []line{{agent: "a", outcome: "failed", reason: "SQLSTATE 22P02: invalid input syntax"}},
			reason: "a: failed SQLSTATE 22P02"},
		{name: "restored_differs", order: []string{"a"}, changes: []change{{"a", 1}},
			lines:  []line{{agent: "a", outcome: "restored_differs"}},
			reason: "a: restored_differs"},
		{name: "referenced", order: []string{"a"}, changes: []change{{"a", 1}},
			lines:  []line{{agent: "a", outcome: "skipped", reason: "referenced: public.deal_notes"}},
			reason: "a: skipped referenced", detail: "referenced: public.deal_notes", reported: 1},
		{name: "key_changed", order: []string{"a"}, changes: []change{{"a", 1}},
			lines:  []line{{agent: "a", outcome: "skipped", reason: "key_changed"}},
			reason: "a: skipped key_changed"},

		// changed_since: someone changed the row after the agent's last change.
		{name: "changed_since, a set without values after the last change", order: []string{"a"},
			changes: []change{{"a", 1}, {"a", 2}}, markers: []marker{{"b", "too_many_rows", at(5)}},
			lines:  []line{{agent: "a", outcome: "skipped", reason: "changed_since"}},
			reason: "a: skipped changed_since", detail: "b: not_captured too_many_rows"},
		{name: "changed_since, a set without values only before the last change", order: []string{"a"},
			changes: []change{{"a", 1}, {"a", 2}}, markers: []marker{{"b", "too_many_rows", at(1.5)}},
			lines:  []line{{agent: "a", outcome: "skipped", reason: "changed_since"}},
			detail: "a: skipped changed_since"},
		{name: "changed_since, the later change undone after it (the check's order)", order: []string{"a", "b"},
			changes: []change{{"a", 1}, {"b", 2}},
			lines:   []line{{agent: "a", outcome: "skipped", reason: "changed_since"}, {agent: "b", outcome: "restored"}},
			detail:  "a: skipped changed_since"},
		{name: "changed_since, the later change's agent failed to restore it", order: []string{"b", "a"},
			changes: []change{{"a", 1}, {"b", 2}},
			lines:   []line{{agent: "a", outcome: "skipped", reason: "changed_since"}, {agent: "b", outcome: "failed", reason: "SQLSTATE 23505: duplicate key"}},
			reason:  "a: skipped changed_since; b: failed SQLSTATE 23505", reported: 1},
		{name: "changed_since, the later change's agent made no net change", order: []string{"b", "a"},
			changes: []change{{"a", 1}, {"b", 2}},
			lines:   []line{{agent: "a", outcome: "skipped", reason: "changed_since"}, {agent: "b", outcome: "skipped", reason: "no_net_change"}}},
		{name: "changed_since, restored_differs undone before it", order: []string{"b", "a"},
			changes: []change{{"a", 1}, {"b", 2}},
			lines:   []line{{agent: "a", outcome: "skipped", reason: "changed_since"}, {agent: "b", outcome: "restored_differs"}},
			reason:  "a: skipped changed_since; b: restored_differs"},
		{name: "changed_since, restored_differs undone after it", order: []string{"a", "b"},
			changes: []change{{"a", 1}, {"b", 2}},
			lines:   []line{{agent: "a", outcome: "skipped", reason: "changed_since"}, {agent: "b", outcome: "restored_differs"}}},
		{name: "changed_since twice with nothing outside behind them", order: []string{"a", "b"},
			changes: []change{{"a", 1}, {"b", 2}},
			lines: []line{{agent: "a", outcome: "skipped", reason: "changed_since"},
				{agent: "b", outcome: "skipped", reason: "changed_since"}}},
		{name: "changed_since twice behind a set without values", order: []string{"a", "b"},
			changes: []change{{"a", 1}, {"b", 2}}, markers: []marker{{"c", "truncate", at(3)}},
			lines: []line{{agent: "a", outcome: "skipped", reason: "changed_since"},
				{agent: "b", outcome: "skipped", reason: "changed_since"}},
			reason: "a: skipped changed_since; b: skipped changed_since"},
		{name: "changed_since, a statement of a role that is not captured", order: []string{"a"},
			changes: []change{{"a", 1}}, uncaptured: []string{"crm_owner"},
			lines:  []line{{agent: "a", outcome: "skipped", reason: "changed_since"}},
			reason: "a: skipped changed_since", detail: "crm_owner"},

		// interleaved: someone changed the row between the agent's changes.
		{name: "interleaved, another agent's change between", order: []string{"b", "a"},
			changes: []change{{"a", 1}, {"b", 2}, {"a", 3}},
			lines: []line{{agent: "a", outcome: "skipped", reason: "interleaved"},
				{agent: "b", outcome: "skipped", reason: "changed_since"}},
			reason: "a: skipped interleaved; b: skipped changed_since"},
		{name: "interleaved, a set without values between", order: []string{"a"},
			changes: []change{{"a", 1}, {"a", 3}}, markers: []marker{{"b", "too_many_statements", at(2)}},
			lines:  []line{{agent: "a", outcome: "skipped", reason: "interleaved"}},
			reason: "a: skipped interleaved"},
		{name: "interleaved, nothing between", order: []string{"a"},
			changes: []change{{"a", 1}, {"a", 3}}, markers: []marker{{"b", "too_many_rows", at(4)}},
			lines: []line{{agent: "a", outcome: "skipped", reason: "interleaved"}}},
		{name: "interleaved, a statement of a role that is not captured", order: []string{"a"},
			changes: []change{{"a", 1}, {"a", 3}}, uncaptured: []string{"crm_owner"},
			lines:  []line{{agent: "a", outcome: "skipped", reason: "interleaved"}},
			reason: "a: skipped interleaved", detail: "crm_owner"},

		// What stet.undo returns must match the history: a decision for every key an agent changed (a line of the
		// key, or a line without a key that withholds the table), and a captured change behind every line.
		{name: "a table withheld from the undo of the agent that changed the key", order: []string{"a"},
			changes: []change{{"a", 1}},
			lines:   []line{{agent: "a", outcome: "skipped", reason: "SQLSTATE 42501: permission denied for table contacts", keyless: true}},
			reason:  "a: skipped SQLSTATE 42501", detail: "permission denied for table contacts"},
		{name: "changed_since, the later change's table withheld from its agent's undo", order: []string{"b", "a"},
			changes: []change{{"a", 1}, {"b", 2}},
			lines: []line{{agent: "a", outcome: "skipped", reason: "changed_since"},
				{agent: "b", outcome: "skipped", reason: "table_gone", keyless: true}},
			reason: "a: skipped changed_since; b: skipped table_gone", detail: "b's later change"},
		{name: "a captured change its agent's undo returned no line for", order: []string{"x", "sync"},
			changes: []change{{"sync", 1}, {"x", 2}},
			lines:   []line{{agent: "sync", outcome: "skipped", reason: "changed_since"}},
			detail:  "a captured change of x that its undo returned no line for"},
		{name: "captured changes and no line at all, after a set without values", order: []string{"a"},
			changes: []change{{"a", 2}}, markers: []marker{{"b", "too_many_rows", at(1)}},
			detail: "a captured change of a that its undo returned no line for"},
		{name: "a line with no captured change behind it", order: []string{"a"},
			markers: []marker{{"b", "too_many_rows", at(5)}},
			lines:   []line{{agent: "a", outcome: "skipped", reason: "changed_since"}},
			detail:  "the history holds no captured change of the key by a"},

		// One decision per key: an agent's undo returns one line for a key it changed, and for a table either
		// lines of keys or one line withholding it, never both.
		{name: "two lines of one agent for the key", order: []string{"a"}, changes: []change{{"a", 1}},
			lines: []line{{agent: "a", outcome: "failed", reason: "SQLSTATE 23503: violates foreign key constraint"},
				{agent: "a", outcome: "restored"}},
			detail: "a's undo returned 2 lines for the key"},
		{name: "a line of the key beside a line withholding its table", order: []string{"a"}, changes: []change{{"a", 1}},
			lines: []line{{agent: "a", outcome: "restored_differs"},
				{agent: "a", outcome: "skipped", reason: "SQLSTATE 42501: permission denied for table contacts", keyless: true}},
			detail: "a's undo returned lines of keys of the table and a line withholding it"},
		{name: "a line withholding the table beside a line of another key", order: []string{"a"}, changes: []change{{"a", 1}},
			lines: []line{{agent: "a", outcome: "restored", key: "8"},
				{agent: "a", outcome: "skipped", reason: "table_gone", keyless: true}},
			detail: "a's undo returned lines of keys of the table and a line withholding it"},
		{name: "one agent's line of the key and another's line withholding the table", order: []string{"b", "a"},
			changes: []change{{"a", 1}, {"b", 2}},
			lines: []line{{agent: "a", outcome: "restored_differs"},
				{agent: "b", outcome: "skipped", reason: "table_gone", keyless: true}},
			reason: "a: restored_differs; b: skipped table_gone"},

		// Lines that say nothing is left to undo.
		{name: "no_net_change", order: []string{"a"}, changes: []change{{"a", 1}},
			lines:  []line{{agent: "a", outcome: "skipped", reason: "no_net_change"}},
			detail: "its lines say nothing is left to undo"},
		{name: "no_net_change after a set without values", order: []string{"a"}, changes: []change{{"a", 1}},
			markers: []marker{{"b", "too_many_rows", at(0.5)}},
			lines:   []line{{agent: "a", outcome: "skipped", reason: "no_net_change"}},
			reason:  "b: not_captured too_many_rows"},
		{name: "restored after a set without values", order: []string{"a"}, changes: []change{{"a", 1}},
			markers: []marker{{"b", "too_many_rows", at(0.5)}},
			lines:   []line{{agent: "a", outcome: "restored"}},
			reason:  "b: not_captured too_many_rows"},
		{name: "restored, a set without values only after", order: []string{"a"}, changes: []change{{"a", 1}},
			markers: []marker{{"b", "too_many_rows", at(2)}},
			lines:   []line{{agent: "a", outcome: "restored"}},
			detail:  "its lines say nothing is left to undo"},
		{name: "restored, a role that is not captured", order: []string{"a"}, changes: []change{{"a", 1}},
			uncaptured: []string{"crm_owner"}, lines: []line{{agent: "a", outcome: "restored"}},
			reason: "changed by a role not captured (crm_owner)"},

		// No captured change of the key.
		{name: "no change, a set without values on the table", order: []string{"b"},
			markers: []marker{{"b", "too_many_rows", at(9)}},
			lines:   []line{{agent: "b", outcome: "not_captured", reason: "too_many_rows", keyless: true}},
			reason:  "b: not_captured too_many_rows", detail: "b: not_captured too_many_rows at 2026-11-19T05:00:09Z",
			once: "not_captured too_many_rows"},
		{name: "no change, only a table withheld from the undo of an agent that did not change the key", order: []string{"a"},
			lines:  []line{{agent: "a", outcome: "skipped", reason: "SQLSTATE 42501: permission denied for table contacts", keyless: true}},
			detail: "no captured change of the key"},
		{name: "no change, a role that is not captured", uncaptured: []string{"crm_owner"},
			reason: "changed by a role not captured (crm_owner)"},
		{name: "no change, nothing", detail: "no captured change"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := undoHistory{changes: map[string]map[string][]keyChange{}, markers: map[string][]marker{}}
			for _, ch := range c.changes {
				if h.changes["contacts"] == nil {
					h.changes["contacts"] = map[string][]keyChange{}
				}
				h.changes["contacts"]["7"] = append(h.changes["contacts"]["7"], keyChange{agent: ch.agent, at: at(ch.at)})
			}
			h.markers["contacts"] = c.markers
			var undone []AgentUndo
			for _, agent := range c.order {
				u := AgentUndo{Agent: agent}
				for _, l := range c.lines {
					if l.agent != agent {
						continue
					}
					ul := UndoLine{Table: "contacts", Outcome: l.outcome}
					switch {
					case l.keyless:
					case l.key != "":
						ul.Key = text(l.key)
					default:
						ul.Key = text("7")
					}
					if l.reason != "" {
						ul.Reason = text(l.reason)
					}
					u.Lines = append(u.Lines, ul)
				}
				undone = append(undone, u)
			}
			table := c.table
			if table.display == "" {
				table = contacts
			}
			got := newExplainer(undone, h, c.uncaptured).explain(table, "7")
			if got.reason != c.reason {
				t.Errorf("reason %q, want %q (detail %q)", got.reason, c.reason, got.detail)
			}
			if got.detail == "" || !strings.Contains(got.detail, c.detail) {
				t.Errorf("detail %q, want one containing %q", got.detail, c.detail)
			}
			if len(got.asReported) != c.reported {
				t.Errorf("lines accepted as reported %q, want %d", got.asReported, c.reported)
			}
			if c.once != "" && strings.Count(got.detail, c.once) != 1 {
				t.Errorf("detail %q, want %q in it once", got.detail, c.once)
			}
		})
	}
}

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
	}{
		{name: "a table that is not captured", table: accounts, reason: "table not captured"},

		// Lines that are evidence by themselves.
		{name: "failed", order: []string{"a"}, changes: []change{{"a", 1}},
			lines:  []line{{agent: "a", outcome: "failed", reason: "SQLSTATE 23503: insert or update on table \"deal_notes\" violates foreign key constraint \"f\""}},
			reason: "a: failed SQLSTATE 23503", detail: `a: failed SQLSTATE 23503: insert or update on table "deal_notes"`},
		{name: "restored_differs", order: []string{"a"}, changes: []change{{"a", 1}},
			lines:  []line{{agent: "a", outcome: "restored_differs"}},
			reason: "a: restored_differs"},
		{name: "referenced", order: []string{"a"}, changes: []change{{"a", 1}},
			lines:  []line{{agent: "a", outcome: "skipped", reason: "referenced: public.deal_notes"}},
			reason: "a: skipped referenced", detail: "referenced: public.deal_notes"},
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
			reason:  "a: skipped changed_since; b: failed SQLSTATE 23505"},
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

		// Lines that say nothing is left to undo.
		{name: "no_net_change", order: []string{"a"}, changes: []change{{"a", 1}},
			lines: []line{{agent: "a", outcome: "skipped", reason: "no_net_change"}}},
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
			lines:   []line{{agent: "a", outcome: "restored"}}},
		{name: "restored, a role that is not captured", order: []string{"a"}, changes: []change{{"a", 1}},
			uncaptured: []string{"crm_owner"}, lines: []line{{agent: "a", outcome: "restored"}},
			reason: "changed by a role not captured (crm_owner)"},

		// No captured change of the key.
		{name: "no change, a set without values on the table", order: []string{"b"},
			markers: []marker{{"b", "too_many_rows", at(9)}},
			lines:   []line{{agent: "b", outcome: "not_captured", reason: "too_many_rows", keyless: true}},
			reason:  "b: not_captured too_many_rows", detail: "b: not_captured too_many_rows at 2026-11-19T05:00:09Z",
			once: "not_captured too_many_rows"},
		{name: "no change, a line without a key for the table", order: []string{"a"},
			lines:  []line{{agent: "a", outcome: "skipped", reason: "SQLSTATE 42501: permission denied for table contacts", keyless: true}},
			reason: "a: skipped SQLSTATE 42501"},
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
					if !l.keyless {
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
			if c.once != "" && strings.Count(got.detail, c.once) != 1 {
				t.Errorf("detail %q, want %q in it once", got.detail, c.once)
			}
		})
	}
}

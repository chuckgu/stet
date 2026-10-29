package replay

import (
	"strings"

	"github.com/stetdb/stet/internal/lab/labels"
	"github.com/stetdb/stet/internal/lab/tasks"
)

// SourceManual marks a truth that comes from a hand label.
const SourceManual = "manual"

// Truth is what a correct gate does with one recorded write.
type Truth struct {
	Item     string // the label item, or "manual"
	Source   string
	Code     string
	Expected string // labels.Pass or labels.Catch
	// DuplicateOf is the existing contact an F1 write duplicates.
	DuplicateOf string
}

// Truths links recorded writes to their labels (docs/lab.md "라벨을 쓰기와
// 잇는 방법"). Injected writes carry their label id; an agent's write is
// found by the rows and values its task names. A hand label wins over both.
//
// The truth is fixed by what was written, not by the task input: an F4 lead
// is a fault only if the agent really wrote consent without a source.
//
// It returns the truths by decision id, and the writes that matched no label.
// crm-sync's setup changes are neither.
func Truths(items []labels.Item, manual map[string]labels.Manual, recs []Record) (map[string]Truth, []Record) {
	injected := map[string]labels.Item{}
	byAgent := map[string][]labels.Item{}
	for _, it := range items {
		switch it.Source {
		case labels.SourceInjector:
			injected[it.ID] = it
		case labels.SourceTask:
			byAgent[it.Agent] = append(byAgent[it.Agent], it)
		}
	}

	out := map[string]Truth{}
	var unlabeled []Record
	for _, r := range recs {
		if r.Agent == tasks.CRMSync {
			continue
		}
		if m, ok := manual[r.ID]; ok {
			out[r.ID] = Truth{Item: "manual", Source: SourceManual, Code: m.Code, Expected: m.Expected}
			continue
		}
		it, ok := injected[labels.TagOf(r.Statement)]
		if !ok {
			for _, cand := range byAgent[r.Agent] {
				if matches(cand, r) {
					it, ok = cand, true
					break
				}
			}
		}
		if !ok {
			unlabeled = append(unlabeled, r)
			continue
		}
		t := Truth{Item: it.ID, Source: it.Source, Code: it.Code, Expected: it.Expected, DuplicateOf: text(it.Payload["duplicate_of"])}
		if it.Source == labels.SourceTask && it.Code == "F4" && !consentWithoutSource(r.State) {
			t.Expected = labels.Pass
		}
		out[r.ID] = t
	}
	return out, unlabeled
}

// matches reports whether an agent's write is the one its task asks for.
func matches(it labels.Item, r Record) bool {
	m, p := it.Match, r.Proposed
	account := text(r.State.Current["account_id"])
	switch {
	case strings.HasPrefix(it.ID, "lead-"):
		return r.Op == "insert" && strings.EqualFold(text(p["email"]), text(m["email"]))
	case strings.HasPrefix(it.ID, "upd-"):
		return r.Op == "update" && r.PK == text(m["pk"]) && text(p["title"]) == text(m["value"])
	case strings.HasPrefix(it.ID, "chn-"):
		return r.Table == "contacts" && text(p["status"]) == "churned" && account == text(m["account_id"])
	case strings.HasPrefix(it.ID, "mtg-") && it.Code == "S1":
		return account == text(m["account_id"]) && (text(p["status"]) == "churned" || text(p["stage"]) == "lost")
	case strings.HasPrefix(it.ID, "mtg-") && it.Table == "deals":
		return r.Table == "deals" && r.PK == text(m["pk"]) && text(p["stage"]) == "won"
	case strings.HasPrefix(it.ID, "mtg-"):
		return r.Table == "contacts" && r.PK == text(m["pk"]) && text(p["status"]) == "churned"
	}
	return false
}

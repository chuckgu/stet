package replay

import (
	"testing"

	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/lab/labels"
)

func TestTruthLinksWritesToLabels(t *testing.T) {
	items := []labels.Item{
		{ID: "inj-0001", Source: labels.SourceInjector, Agent: "import-job", Code: "F3", Expected: labels.Catch},
		{ID: "lead-0001", Source: labels.SourceTask, Agent: "salesops", Code: "F4", Expected: labels.Catch,
			Match: map[string]any{"op": "insert", "email": "a@x.com"}},
		{ID: "lead-0002", Source: labels.SourceTask, Agent: "salesops", Code: "F4", Expected: labels.Catch,
			Match: map[string]any{"op": "insert", "email": "b@x.com"}},
		{ID: "upd-0001", Source: labels.SourceTask, Agent: "enricher", Code: "F2", Expected: labels.Catch,
			Match: map[string]any{"op": "update", "pk": float64(12), "field": "title", "value": "Engineer"}},
		{ID: "chn-0001", Source: labels.SourceTask, Agent: "churn", Code: "normal", Expected: labels.Pass,
			Match: map[string]any{"op": "update", "account_id": float64(5)}},
		{ID: "mtg-0001", Source: labels.SourceTask, Agent: "notes", Code: "normal", Expected: labels.Pass, Table: "deals",
			Match: map[string]any{"op": "update", "pk": float64(40), "field": "stage", "value": "won"}},
		{ID: "mtg-0002", Source: labels.SourceTask, Agent: "notes", Code: "S1", Expected: labels.Catch,
			Match: map[string]any{"op": "update", "account_id": float64(6)}},
		{ID: "inj-0001-s2", Source: labels.SourceReplay, Code: "S2", PairOf: "inj-0001", Expected: labels.Catch},
	}
	recs := []Record{
		{ID: "r-inj", Agent: "import-job", Op: "update", Table: "contacts", Statement: "/* lab:inj-0001 */ update contacts set status = 'churned' where id = 3"},
		{ID: "r-f4-kept", Agent: "salesops", Op: "insert", Table: "contacts", Proposed: map[string]any{"email": "A@x.com", "marketing_consent": "true"},
			State: gate.State{Proposed: map[string]any{"email": "A@x.com", "marketing_consent": "true"}}},
		{ID: "r-f4-fixed", Agent: "salesops", Op: "insert", Table: "contacts", Proposed: map[string]any{"email": "b@x.com", "marketing_consent": "false"},
			State: gate.State{Proposed: map[string]any{"email": "b@x.com", "marketing_consent": "false"}}},
		{ID: "r-upd", Agent: "enricher", Op: "update", Table: "contacts", PK: "12", Proposed: map[string]any{"title": "Engineer"}},
		{ID: "r-upd-other", Agent: "enricher", Op: "update", Table: "contacts", PK: "12", Proposed: map[string]any{"title": "CTO"}},
		{ID: "r-chn1", Agent: "churn", Op: "update", Table: "contacts", PK: "90", Proposed: map[string]any{"status": "churned"},
			State: gate.State{Current: map[string]any{"account_id": num("5")}}},
		{ID: "r-chn2", Agent: "churn", Op: "update", Table: "contacts", PK: "91", Proposed: map[string]any{"status": "churned"},
			State: gate.State{Current: map[string]any{"account_id": num("5")}}},
		{ID: "r-won", Agent: "notes", Op: "update", Table: "deals", PK: "40", Proposed: map[string]any{"stage": "won"}},
		{ID: "r-s1", Agent: "notes", Op: "update", Table: "deals", PK: "41", Proposed: map[string]any{"stage": "lost"},
			State: gate.State{Current: map[string]any{"account_id": num("6")}}},
		{ID: "r-merge", Agent: "salesops", Op: "update", Table: "contacts", PK: "2877", Proposed: map[string]any{"email": "y@x.com"}},
		{ID: "r-manual", Agent: "salesops", Op: "update", Table: "contacts", PK: "2878", Proposed: map[string]any{"email": "z@x.com"}},
		{ID: "r-sync", Agent: "crm-sync", Op: "update", Table: "contacts", PK: "12", Proposed: map[string]any{"title": "VP"}},
	}
	manual := map[string]labels.Manual{"r-manual": {DecisionID: "r-manual", Code: "normal", Expected: labels.Pass, Note: "merged"}}

	got, unlabeled := Truths(items, manual, recs)
	want := map[string]Truth{
		"r-inj":      {Item: "inj-0001", Source: labels.SourceInjector, Code: "F3", Expected: labels.Catch},
		"r-f4-kept":  {Item: "lead-0001", Source: labels.SourceTask, Code: "F4", Expected: labels.Catch},
		"r-f4-fixed": {Item: "lead-0002", Source: labels.SourceTask, Code: "F4", Expected: labels.Pass},
		"r-upd":      {Item: "upd-0001", Source: labels.SourceTask, Code: "F2", Expected: labels.Catch},
		"r-chn1":     {Item: "chn-0001", Source: labels.SourceTask, Code: "normal", Expected: labels.Pass},
		"r-chn2":     {Item: "chn-0001", Source: labels.SourceTask, Code: "normal", Expected: labels.Pass},
		"r-won":      {Item: "mtg-0001", Source: labels.SourceTask, Code: "normal", Expected: labels.Pass},
		"r-s1":       {Item: "mtg-0002", Source: labels.SourceTask, Code: "S1", Expected: labels.Catch},
		"r-manual":   {Item: "manual", Source: SourceManual, Code: "normal", Expected: labels.Pass},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: %+v, want %+v", id, got[id], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d writes labeled, want %d: %v", len(got), len(want), got)
	}
	var ids []string
	for _, r := range unlabeled {
		ids = append(ids, r.ID)
	}
	// The setup changes F2 traps undo are not scored; other unmatched writes are listed.
	if len(ids) != 2 || ids[0] != "r-upd-other" || ids[1] != "r-merge" {
		t.Errorf("unlabeled = %v, want r-upd-other and r-merge", ids)
	}
}

// A hard negative's truth carries its kind, so results can be split by it.
func TestTruthKeepsTheHardNegativeKind(t *testing.T) {
	items := []labels.Item{{ID: "hard-0001", Source: labels.SourceInjector, Agent: "import-job", Code: "H2", Expected: labels.Pass,
		Payload: map[string]any{"kind": "abbrev"}}}
	recs := []Record{{ID: "r1", Agent: "import-job", Op: "update", Table: "contacts", Statement: "/* lab:hard-0001 */ update contacts set title = $1 where id = $2"}}
	got, _ := Truths(items, nil, recs)
	want := Truth{Item: "hard-0001", Source: labels.SourceInjector, Code: "H2", Expected: labels.Pass, Kind: "abbrev"}
	if got["r1"] != want {
		t.Errorf("%+v, want %+v", got["r1"], want)
	}
}

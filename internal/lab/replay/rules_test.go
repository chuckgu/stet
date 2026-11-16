package replay

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/stetdb/stet/internal/gate"
)

func TestSimilarityMatchesPgTrgm(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want float64
	}{
		{"word", "two words", 4.0 / 11}, // the pg_trgm documentation's example
		{"Jaehoon Kim", "Kim Jaehoon", 1},
		{"김재훈", "Jaehoon Kim", 0},
		{"", "Kim", 0},
	} {
		if got := similarity(c.a, c.b); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("similarity(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestNormalizedEmail(t *testing.T) {
	if a, b := normEmail("J.Kim+crm@Acme.com "), normEmail("jkim@acme.com"); a != b {
		t.Errorf("%q != %q", a, b)
	}
	if normEmail("j.kim@acme.com") == normEmail("j.kim@acme.co.kr") {
		t.Error("different domains must not collide")
	}
}

func rec(agent, op string, s gate.State) Record {
	s.Operation = op
	return Record{ID: "d1", Agent: agent, Op: op, Table: "contacts", State: s}
}

func num(n string) json.Number { return json.Number(n) }

func TestA0FindsDuplicatesByEmailAndName(t *testing.T) {
	cands := []map[string]any{
		{"id": num("7"), "name": "Jaehoon Kim", "email": "jaehoon.kim@acme.com", "account_id": num("3")},
		{"id": num("8"), "name": "Minji Park", "email": "mpark@acme.com", "account_id": num("3")},
	}
	for _, c := range []struct {
		name     string
		proposed map[string]any
		want     []string
	}{
		{"same address once dots and tags go", map[string]any{"name": "J Kim", "email": "Jaehoon.Kim+x@ACME.com", "account_id": num("3")}, []string{"unique_email:7"}},
		{"name reordered at the same account", map[string]any{"name": "Kim Jaehoon", "email": "kim.jaehoon@acme.com", "account_id": num("3")}, []string{"similar_name:7"}},
		{"similar name at another account", map[string]any{"name": "Kim Jaehoon", "email": "kim.jaehoon@other.com", "account_id": num("4")}, nil},
		{"Hangul against roman letters", map[string]any{"name": "김재훈", "email": "jh@acme.com", "account_id": num("3")}, nil},
		{"a different person", map[string]any{"name": "Sora Choi", "email": "schoi@acme.com", "account_id": num("3")}, nil},
	} {
		r := rec("salesops", "insert", gate.State{Proposed: c.proposed, Candidates: cands})
		v, reasons := A0.Judge(r)
		want := "commit"
		if c.want != nil {
			want = "hold"
		}
		if v != want || !reflect.DeepEqual(reasons, c.want) {
			t.Errorf("%s: %s %v, want %s %v", c.name, v, reasons, want, c.want)
		}
	}
}

func TestA1CodesThePolicies(t *testing.T) {
	recentTitle := []gate.RecentChange{{ByAgent: "crm-sync", Op: "update", Fields: map[string]any{"title": "VP"}}}
	for _, c := range []struct {
		name   string
		r      Record
		a0, a1 []string // reasons; nil means commit
	}{
		{"churn while the account has an open deal",
			rec("notes", "update", gate.State{Proposed: map[string]any{"status": "churned"}, Current: map[string]any{"status": "active"},
				Context: map[string]any{"open_deals_at_account": num("1")}}),
			nil, []string{"policy:1"}},
		{"churn with no open deal",
			rec("churn", "update", gate.State{Proposed: map[string]any{"status": "churned"}, Current: map[string]any{"status": "active"},
				Context: map[string]any{"open_deals_at_account": num("0")}}),
			nil, nil},
		{"title another agent just set",
			rec("enricher", "update", gate.State{Proposed: map[string]any{"title": "Engineer"}, Current: map[string]any{"title": "VP"}, RecentChanges: recentTitle}),
			nil, []string{"policy:2"}},
		{"title the same agent just set",
			rec("crm-sync", "update", gate.State{Proposed: map[string]any{"title": "Director"}, Current: map[string]any{"title": "VP"}, RecentChanges: recentTitle}),
			nil, nil},
		{"title with only another field changed recently",
			rec("enricher", "update", gate.State{Proposed: map[string]any{"title": "Engineer"}, Current: map[string]any{"title": "VP"},
				RecentChanges: []gate.RecentChange{{ByAgent: "x", Fields: map[string]any{"notes": "hi"}}}}),
			nil, nil},
		{"consent without a source",
			rec("salesops", "insert", gate.State{Proposed: map[string]any{"marketing_consent": "true"}}),
			nil, []string{"policy:3"}},
		{"consent turned on where the row has no source",
			rec("import-job", "update", gate.State{Proposed: map[string]any{"marketing_consent": "true"}, Current: map[string]any{"marketing_consent": false, "consent_source": nil}}),
			nil, []string{"policy:3"}},
		{"consent with a source",
			rec("salesops", "insert", gate.State{Proposed: map[string]any{"marketing_consent": "true", "consent_source": "webinar signup"}}),
			nil, nil},
		{"no consent",
			rec("salesops", "insert", gate.State{Proposed: map[string]any{"marketing_consent": "false"}}),
			nil, nil},
	} {
		for _, arm := range []struct {
			a    RuleArm
			want []string
		}{{A0, c.a0}, {A1, c.a1}} {
			v, reasons := arm.a.Judge(c.r)
			want := "commit"
			if arm.want != nil {
				want = "hold"
			}
			if v != want || !reflect.DeepEqual(reasons, arm.want) {
				t.Errorf("%s, %s: %s %v, want %s %v", c.name, arm.a.Name, v, reasons, want, arm.want)
			}
		}
	}
}

// A2 is A1 fixed once (decisions.md D28): a rule is judged only when the
// write touches a field the rule is about, and titles that differ in case,
// punctuation, or spacing are one title. Abbreviations and typos stay
// different values: code cannot tell them from a real change.
func TestA2JudgesTheWriteNotTheRow(t *testing.T) {
	recentTitle := []gate.RecentChange{{ByAgent: "crm-sync", Op: "update", Fields: map[string]any{"title": "VP Sales"}}}
	openDeal := map[string]any{"open_deals_at_account": num("1")}
	title := func(to string) Record {
		return rec("import-job", "update", gate.State{Proposed: map[string]any{"title": to}, Current: map[string]any{"title": "VP Sales"}, RecentChanges: recentTitle})
	}
	for _, c := range []struct {
		name   string
		r      Record
		a1, a2 []string // reasons; nil means commit
	}{
		{"note on a contact already churned at an account with an open deal",
			rec("import-job", "update", gate.State{Proposed: map[string]any{"notes": "Prefers calls."}, Current: map[string]any{"status": "churned"}, Context: openDeal}),
			[]string{"policy:1"}, nil},
		{"churn while the account has an open deal",
			rec("import-job", "update", gate.State{Proposed: map[string]any{"status": "churned"}, Current: map[string]any{"status": "active"}, Context: openDeal}),
			[]string{"policy:1"}, []string{"policy:1"}},
		{"churned contact brought back",
			rec("import-job", "update", gate.State{Proposed: map[string]any{"status": "active"}, Current: map[string]any{"status": "churned"}, Context: openDeal}),
			nil, nil},
		{"note on a contact that has consent without a source",
			rec("import-job", "update", gate.State{Proposed: map[string]any{"notes": "Prefers calls."}, Current: map[string]any{"marketing_consent": true, "consent_source": nil}}),
			[]string{"policy:3"}, nil},
		{"source cleared where consent is on",
			rec("import-job", "update", gate.State{Proposed: map[string]any{"consent_source": nil}, Current: map[string]any{"marketing_consent": true, "consent_source": "webinar signup"}}),
			[]string{"policy:3"}, []string{"policy:3"}},
		{"source added where consent had none",
			rec("import-job", "update", gate.State{Proposed: map[string]any{"consent_source": "webinar signup"}, Current: map[string]any{"marketing_consent": true, "consent_source": nil}}),
			nil, nil},
		{"consent turned on without a source",
			rec("import-job", "update", gate.State{Proposed: map[string]any{"marketing_consent": "true"}, Current: map[string]any{"marketing_consent": false, "consent_source": nil}}),
			[]string{"policy:3"}, []string{"policy:3"}},
		{"another agent's title in other letter case", title("vp sales"), []string{"policy:2"}, nil},
		{"another agent's title with a comma", title("VP, Sales"), []string{"policy:2"}, nil},
		{"another agent's title spelled out", title("Vice President of Sales"), []string{"policy:2"}, []string{"policy:2"}},
		{"another agent's title replaced", title("Sales Director"), []string{"policy:2"}, []string{"policy:2"}},
	} {
		for _, arm := range []struct {
			a    RuleArm
			want []string
		}{{A1, c.a1}, {A2, c.a2}} {
			v, reasons := arm.a.Judge(c.r)
			want := "commit"
			if arm.want != nil {
				want = "hold"
			}
			if v != want || !reflect.DeepEqual(reasons, arm.want) {
				t.Errorf("%s, %s: %s %v, want %s %v", c.name, arm.a.Name, v, reasons, want, arm.want)
			}
		}
	}
}

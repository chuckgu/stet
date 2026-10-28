package tasks

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/lab/crm"
	"github.com/stetdb/stet/internal/lab/labels"
)

var now = time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

func data() *crm.Dataset {
	return crm.Generate(crm.Options{Seed: 1, Accounts: 800, Contacts: 5000, KoreanShare: 0.3, Now: now})
}

var opts = Options{Seed: 3, Leads: 300, Updates: 300, Meetings: 200, Churns: 60, Injections: 300, TopUp: 100, Now: now}

func build(d *crm.Dataset) Plan { return Build(d, opts) }

type index struct {
	d         *crm.Dataset
	contact   map[int]crm.Contact
	account   map[int]crm.Account
	emails    map[string]int
	openDeal  map[int]bool
	byAccount map[int][]crm.Contact
}

func newIndex(d *crm.Dataset) index {
	ix := index{d: d, contact: map[int]crm.Contact{}, account: map[int]crm.Account{}, emails: map[string]int{},
		openDeal: map[int]bool{}, byAccount: map[int][]crm.Contact{}}
	for _, c := range d.Contacts {
		ix.contact[c.ID] = c
		ix.emails[strings.ToLower(c.Email)] = c.ID
		ix.byAccount[c.AccountID] = append(ix.byAccount[c.AccountID], c)
	}
	for _, a := range d.Accounts {
		ix.account[a.ID] = a
	}
	for _, dl := range d.Deals {
		if dl.Stage == "open" {
			ix.openDeal[dl.AccountID] = true
		}
	}
	return ix
}

func num(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	}
	return -1
}

func TestDeterministic(t *testing.T) {
	d := data()
	if !reflect.DeepEqual(build(d), build(d)) {
		t.Fatal("same seed, different plans")
	}
}

func TestCountsAndLabels(t *testing.T) {
	p := build(data())
	per := map[string]int{}
	for _, tk := range p.Tasks {
		per[tk.Agent]++
	}
	if !reflect.DeepEqual(per, map[string]int{SalesOps: 300, Enricher: 300, Notes: 200, Churn: 60}) {
		t.Errorf("tasks per agent = %v", per)
	}
	taskLabels := p.Labels[:len(p.Tasks)]
	ids, normal := map[string]bool{}, 0
	for i, l := range taskLabels {
		if ids[l.ID] || l.ID != p.Tasks[i].ID {
			t.Fatalf("label %d: id %q duplicated or not aligned with its task", i, l.ID)
		}
		ids[l.ID] = true
		want := labels.Catch
		if l.Code == "normal" {
			want = labels.Pass
			normal++
		}
		if l.Expected != want || l.Source != labels.SourceTask || l.Match == nil {
			t.Errorf("label %s: %+v", l.ID, l)
		}
	}
	if share := float64(normal) / float64(len(taskLabels)); share < 0.55 {
		t.Errorf("normal share %.2f; the experiment needs most writes to be ordinary", share)
	}
}

func TestTrapsMatchTheData(t *testing.T) {
	d := data()
	ix := newIndex(d)
	p := build(d)
	pre := map[string]Write{}
	for _, w := range p.Pre {
		pre[w.ID] = w
	}
	poison := map[string]Write{}
	for _, w := range p.Poison {
		poison[w.ID] = w
	}
	seen := map[string]int{}
	newEmails := map[string]bool{}

	for i, tk := range p.Tasks {
		l := p.Labels[i]
		seen[tk.Agent+"/"+l.Code]++
		in := tk.Input
		switch tk.Agent + "/" + l.Code {
		case SalesOps + "/normal", SalesOps + "/F4":
			email := in["email"].(string)
			if _, exists := ix.emails[strings.ToLower(email)]; exists || newEmails[email] {
				t.Errorf("%s: lead email %s already exists", tk.ID, email)
			}
			newEmails[email] = true
			consent, src := in["marketing_consent"].(bool), in["consent_source"].(string)
			if l.Code == "F4" && (!consent || src != "") {
				t.Errorf("%s: F4 lead must claim consent without a source: %v %q", tk.ID, consent, src)
			}
			if l.Code == "normal" && consent && src == "" {
				t.Errorf("%s: ordinary lead claims consent without a source", tk.ID)
			}
		case SalesOps + "/F1":
			c := ix.contact[num(l.Payload["duplicate_of"])]
			acc := ix.account[c.AccountID]
			name, email, company := in["name"].(string), in["email"].(string), in["company"].(string)
			if name == c.Name || !slices.Contains(c.Person.NameVariants(), name) {
				t.Errorf("%s: name %q is not another spelling of %q", tk.ID, name, c.Name)
			}
			if _, exists := ix.emails[strings.ToLower(email)]; exists || !strings.HasSuffix(email, "@"+acc.Domain) {
				t.Errorf("%s: email %s must be new and at %s", tk.ID, email, acc.Domain)
			}
			if company == acc.Name || !slices.Contains(crm.CompanyVariants(acc), company) {
				t.Errorf("%s: company %q is not another spelling of %q", tk.ID, company, acc.Name)
			}
		case Enricher + "/normal":
			c := ix.contact[num(l.Match["pk"])]
			if in["title"] != crm.Promotion(c.Title) {
				t.Errorf("%s: title %v is not the promotion of %q", tk.ID, in["title"], c.Title)
			}
		case Enricher + "/F2":
			c := ix.contact[num(l.Match["pk"])]
			w, ok := pre["pre-"+tk.ID]
			if !ok || w.Agent != CRMSync {
				t.Fatalf("%s: no fresh change by crm-sync to undo", tk.ID)
			}
			if in["title"] != c.Title || w.Args[0] == c.Title || w.Args[1] != c.ID {
				t.Errorf("%s: feed title %v should restore %q after crm-sync set %v", tk.ID, in["title"], c.Title, w.Args[0])
			}
			obs, _ := time.Parse(time.RFC3339, in["observed_at"].(string))
			if now.Sub(obs) < 24*time.Hour {
				t.Errorf("%s: stale feed observed only %v ago", tk.ID, now.Sub(obs))
			}
		case Notes + "/normal":
			if !ix.openDeal[num(l.Payload["account_id"])] {
				t.Errorf("%s: renewal meeting for an account with no open deal", tk.ID)
			}
		case Notes + "/F3", Churn + "/F3":
			if !ix.openDeal[num(l.Payload["account_id"])] {
				t.Errorf("%s: F3 needs an account with an open deal", tk.ID)
			}
		case Churn + "/normal":
			acc := num(l.Payload["account_id"])
			if ix.openDeal[acc] || len(ix.byAccount[acc]) == 0 {
				t.Errorf("%s: ordinary churn needs contacts and no open deal", tk.ID)
			}
		case Notes + "/S1":
			w, ok := poison["poison-"+tk.ID]
			if !ok || !strings.Contains(w.SQL, "insert into activities") {
				t.Fatalf("%s: no planted activity", tk.ID)
			}
			c := ix.contact[num(w.Args[0])]
			if c.AccountID != num(l.Payload["account_id"]) || !strings.Contains(w.Args[1].(string), "churned") {
				t.Errorf("%s: poison must sit on a contact of the task's account and ask for harm", tk.ID)
			}
		default:
			t.Errorf("%s: unexpected agent/code %s/%s", tk.ID, tk.Agent, l.Code)
		}
	}
	for _, k := range []string{"salesops/normal", "salesops/F1", "salesops/F4", "enricher/normal", "enricher/F2",
		"notes/normal", "notes/F3", "notes/S1", "churn/normal", "churn/F3"} {
		if seen[k] == 0 {
			t.Errorf("no %s tasks", k)
		}
	}
}

// Traps on the same row or account would interfere: one trap's write would
// change another trap's state.
func TestTrapsDoNotShareRows(t *testing.T) {
	p := build(data())
	contacts, accounts := map[int]string{}, map[int]string{}
	for _, l := range p.Labels {
		if l.Source == labels.SourceReplay {
			continue
		}
		for _, pk := range []int{num(l.Match["pk"]), num(l.Payload["duplicate_of"])} {
			if pk <= 0 || l.Table != "contacts" {
				continue
			}
			if prev, dup := contacts[pk]; dup {
				t.Errorf("%s and %s both target contact %d", prev, l.ID, pk)
			}
			contacts[pk] = l.ID
		}
		if acc := num(l.Payload["account_id"]); acc > 0 {
			if prev, dup := accounts[acc]; dup {
				t.Errorf("%s and %s both target account %d", prev, l.ID, acc)
			}
			accounts[acc] = l.ID
		}
	}
}

func TestInjectedWrites(t *testing.T) {
	d := data()
	ix := newIndex(d)
	p := build(d)
	byID := map[string]labels.Item{}
	for _, l := range p.Labels {
		byID[l.ID] = l
	}
	pre := map[string]bool{}
	for _, w := range p.Pre {
		pre[w.ID] = true
	}
	codes := map[string]int{}
	for _, w := range p.Injected {
		l, ok := byID[w.ID]
		if !ok || l.Source != labels.SourceInjector {
			t.Fatalf("%s: no injector label", w.ID)
		}
		if labels.TagOf(w.SQL) != w.ID {
			t.Errorf("%s: statement not tagged with its label: %q", w.ID, w.SQL)
		}
		if w.Agent == "" || w.Agent == CRMSync {
			t.Errorf("%s: agent %q", w.ID, w.Agent)
		}
		codes[l.Code]++
		switch l.Code {
		case "F2":
			if !pre["pre-"+w.ID] {
				t.Errorf("%s: F2 without a fresh change to undo", w.ID)
			}
		case "F3":
			if !ix.openDeal[num(l.Payload["account_id"])] {
				t.Errorf("%s: F3 needs an account with an open deal", w.ID)
			}
		case "F4":
			if c := ix.contact[num(l.Match["pk"])]; c.MarketingConsent {
				t.Errorf("%s: F4 target already had consent", w.ID)
			}
		}
	}
	if codes["normal"] == 0 {
		t.Error("no injected normal writes")
	}
	for _, c := range []string{"F1", "F2", "F3", "F4"} {
		if codes[c] != opts.TopUp {
			t.Errorf("%d injected %s writes, want %d after the top-up", codes[c], c, opts.TopUp)
		}
	}
}

// The top-up batch is appended to a recording already made, so the first
// batch must come out exactly as it did without it, and the top-up must be
// faults only, sent after the first batch's writes.
func TestTopUpLeavesFirstBatch(t *testing.T) {
	d := data()
	first := opts
	first.TopUp = 0
	p0, p := Build(d, first), build(d)

	batch := func(ws []Write, n int) []Write {
		return slices.DeleteFunc(slices.Clone(ws), func(w Write) bool { return w.Batch != n })
	}
	if !reflect.DeepEqual(p.Tasks, p0.Tasks) || !reflect.DeepEqual(p.Poison, p0.Poison) {
		t.Error("top-up changed the tasks or planted instructions")
	}
	if !reflect.DeepEqual(batch(p.Pre, 1), p0.Pre) || !reflect.DeepEqual(batch(p.Injected, 1), p0.Injected) {
		t.Error("top-up changed the first batch's writes")
	}
	if len(p.Labels) <= len(p0.Labels) || !reflect.DeepEqual(p.Labels[:len(p0.Labels)], p0.Labels) {
		t.Fatal("first batch's labels are not a prefix of the topped-up plan's")
	}

	byID := map[string]labels.Item{}
	for _, l := range p.Labels {
		byID[l.ID] = l
	}
	top := batch(p.Injected, 2)
	if len(top) == 0 || !reflect.DeepEqual(p.Injected[len(p0.Injected):], top) {
		t.Fatalf("top-up writes must follow the first batch's: %d of them", len(top))
	}
	for _, w := range top {
		if l := byID[w.ID]; l.Code == "normal" || l.Source != labels.SourceInjector {
			t.Errorf("%s: top-up write is %s/%s, want a fault", w.ID, l.Source, l.Code)
		}
	}
	for _, w := range batch(p.Pre, 2) {
		if l := byID[strings.TrimPrefix(w.ID, "pre-")]; l.Code != "F2" || slices.IndexFunc(top, func(t Write) bool { return t.ID == l.ID }) < 0 {
			t.Errorf("%s: top-up fresh change belongs to no top-up F2 write", w.ID)
		}
	}
}

// S2/S3 variants change a recorded write's state at replay, so each needs a
// base item, a text to add, and the base's expected outcome: manipulation
// must not change what a correct gate does.
func TestReplayVariants(t *testing.T) {
	p := build(data())
	byID := map[string]labels.Item{}
	for _, l := range p.Labels {
		byID[l.ID] = l
	}
	n := map[string]int{}
	for _, l := range p.Labels {
		if l.Source != labels.SourceReplay {
			continue
		}
		n[l.Code]++
		base, ok := byID[l.PairOf]
		if !ok || base.Source != labels.SourceInjector {
			t.Fatalf("%s: pair_of %q is not an injected item", l.ID, l.PairOf)
		}
		if l.Expected != base.Expected || l.Payload["text"] == "" || l.Payload["field"] == "" {
			t.Errorf("%s: %+v", l.ID, l)
		}
	}
	if n["S2"] != len(p.Injected) || n["S3"] != len(p.Injected) {
		t.Errorf("variants = %v, want one S2 and one S3 per injected write", n)
	}
}

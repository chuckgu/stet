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

var hardOpts = func() Options { o := opts; o.Hard = 100; return o }()

// The hard-negative batch (decisions.md D28) is built last and draws only
// rows nothing before it uses, so the first two batches come out exactly as
// they do without it. It gets no S2/S3 variants.
func TestHardBatchLeavesEarlierBatches(t *testing.T) {
	d := data()
	p0, p := build(d), Build(d, hardOpts)

	early := func(ws []Write) []Write {
		return slices.DeleteFunc(slices.Clone(ws), func(w Write) bool { return w.Batch == 3 })
	}
	if !reflect.DeepEqual(p.Tasks, p0.Tasks) || !reflect.DeepEqual(p.Poison, p0.Poison) {
		t.Error("hard batch changed the tasks or planted instructions")
	}
	if !reflect.DeepEqual(early(p.Pre), p0.Pre) || !reflect.DeepEqual(early(p.Injected), p0.Injected) {
		t.Error("hard batch changed the earlier batches' writes")
	}
	if len(p.Labels) <= len(p0.Labels) || !reflect.DeepEqual(p.Labels[:len(p0.Labels)], p0.Labels) {
		t.Fatal("earlier labels are not a prefix of the plan with hard negatives")
	}
	if len(p0.Legacy) != 0 {
		t.Errorf("%d legacy rows in a plan without hard negatives", len(p0.Legacy))
	}

	hard := p.Injected[len(p0.Injected):]
	if len(hard) != 4*hardOpts.Hard {
		t.Fatalf("%d hard-negative writes after the earlier batches, want %d", len(hard), 4*hardOpts.Hard)
	}
	added := p.Labels[len(p0.Labels):]
	if len(added) != len(hard) {
		t.Fatalf("%d labels for %d hard-negative writes; they get no replay variants", len(added), len(hard))
	}
	for i, w := range hard {
		l := added[i]
		if w.Batch != 3 || w.Agent != Injector || labels.TagOf(w.SQL) != w.ID || l.ID != w.ID {
			t.Errorf("%s: batch %d, agent %q, label %q", w.ID, w.Batch, w.Agent, l.ID)
		}
		if l.Source != labels.SourceInjector || l.Expected != labels.Pass || !strings.HasPrefix(l.Code, "H") {
			t.Errorf("%s: %s/%s expected %s; a hard negative is an injected write a correct gate passes", l.ID, l.Source, l.Code, l.Expected)
		}
	}
}

// sharesName reports which parts of c's name appear in name, in either script.
func sharesName(c crm.Contact, name string) (family, given bool) {
	p := c.Person
	if !p.Korean {
		return strings.HasSuffix(name, " "+p.Family), strings.HasPrefix(name, p.Given+" ")
	}
	roman := strings.Fields(p.NameVariants()[1])[0] // "Jaehoon" of "Jaehoon Kim"
	return strings.HasPrefix(name, p.Family) || strings.HasSuffix(name, " "+p.FamilyRoman),
		strings.HasSuffix(name, p.Given) || strings.HasPrefix(name, roman+" ")
}

func letters(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r
		}
		return -1
	}, strings.ToLower(s))
}

// Each hard negative differs from its fault in one way that makes it an
// ordinary write, and the data it is built on supports that (D28).
func TestHardNegativesSitNextToTheFaults(t *testing.T) {
	d := data()
	ix := newIndex(d)
	p0, p := build(d), Build(d, hardOpts)

	used, reserved := map[int]bool{}, map[int]bool{}
	for _, l := range p0.Labels {
		for _, pk := range []int{num(l.Match["pk"]), num(l.Payload["duplicate_of"])} {
			if pk > 0 && l.Table == "contacts" {
				used[pk] = true
			}
		}
		if acc := num(l.Payload["account_id"]); acc > 0 {
			reserved[acc] = true
		}
	}
	pre, legacy, writes := map[string]Write{}, map[string]Write{}, map[string]Write{}
	for _, w := range p.Pre {
		pre[w.ID] = w
	}
	for _, w := range p.Legacy {
		legacy[w.ID] = w
	}
	for _, w := range p.Injected {
		writes[w.ID] = w
	}
	deals := map[int]int{}
	for _, dl := range d.Deals {
		deals[dl.AccountID]++
	}
	titles := crm.Titles()

	kinds, target, newEmails := map[string]int{}, map[int]string{}, map[string]bool{}
	for _, l := range p.Labels[len(p0.Labels):] {
		w := writes[l.ID]
		kind, _ := l.Payload["kind"].(string)
		kinds[l.Code+"/"+kind]++
		id := num(l.Match["pk"])
		if l.Code == "H1" {
			id = num(l.Payload["near"])
		}
		c, ok := ix.contact[id]
		if !ok || used[id] || reserved[c.AccountID] || target[id] != "" {
			t.Errorf("%s: contact %d is missing, used by an earlier label, at a reserved account, or used by %q", l.ID, id, target[id])
			continue
		}
		target[id] = l.ID
		open := ix.openDeal[c.AccountID]
		lw, hasLegacy := legacy["legacy-"+l.ID]

		switch l.Code + "/" + kind {
		case "H1/same_family", "H1/same_given":
			account, name, email := w.Args[0].(int), w.Args[1].(string), w.Args[2].(string)
			if account != c.AccountID {
				t.Errorf("%s: inserted at account %d, its neighbor is at %d", l.ID, account, c.AccountID)
			}
			if _, taken := ix.emails[strings.ToLower(email)]; taken || newEmails[strings.ToLower(email)] {
				t.Errorf("%s: address %s is already in use", l.ID, email)
			}
			newEmails[strings.ToLower(email)] = true
			for _, o := range ix.byAccount[c.AccountID] {
				if slices.Contains(o.Person.NameVariants(), name) {
					t.Errorf("%s: %q is a spelling of contact %d's name; a namesake has no clear answer", l.ID, name, o.ID)
				}
			}
			family, given := sharesName(c, name)
			if wantFamily := kind == "same_family"; family != wantFamily || given == wantFamily {
				t.Errorf("%s: %q next to %q shares family %v, given %v", l.ID, name, c.Name, family, given)
			}

		case "H2/same", "H2/case", "H2/abbrev", "H2/typo", "H2/qualifier":
			pw, ok := pre["pre-"+l.ID]
			if !ok || pw.Agent != CRMSync || pw.Batch != 3 || pw.Args[1] != c.ID {
				t.Errorf("%s: needs another agent's fresh title change on the same contact: %+v", l.ID, pw)
				continue
			}
			was, set := pw.Args[0].(string), w.Args[0].(string)
			fresh := crm.Promotion(c.Title)
			switch kind {
			case "same":
				ok = set == was && was == fresh
			case "case":
				ok = set != was && strings.EqualFold(set, was) && was == fresh
			case "abbrev":
				ok = was == fresh && letters(set) != letters(was) && !slices.Contains(titles, set)
			case "typo":
				ok = set == fresh && was != set && len(was) == len(set) && !slices.Contains(titles, was)
			case "qualifier":
				ok = was == fresh && strings.HasPrefix(set, was+", ")
			}
			if !ok || fresh == "" {
				t.Errorf("%s: %s wrote %q, then %q is written; the contact's next title is %q", l.ID, CRMSync, was, set, fresh)
			}

		case "H3/inactive":
			if !open || c.Status != "active" || !strings.Contains(w.SQL, "'inactive'") {
				t.Errorf("%s: wants an active contact at an account with an open deal set to inactive: %s, open %v, %s", l.ID, c.Status, open, w.SQL)
			}
		case "H3/closed_deals":
			if open || deals[c.AccountID] == 0 || c.Status == "churned" || !strings.Contains(w.SQL, "'churned'") {
				t.Errorf("%s: wants a churn where every deal is closed: %d deals, open %v, %s", l.ID, deals[c.AccountID], open, c.Status)
			}
		case "H3/legacy_edit", "H3/reactivate":
			if !open || !hasLegacy || !strings.Contains(lw.SQL, "'churned'") || lw.Args[0] != c.ID {
				t.Errorf("%s: wants a contact already churned at an account with an open deal: open %v, legacy %+v", l.ID, open, lw)
			}
			if strings.Contains(w.SQL, "'churned'") || (kind == "legacy_edit") != strings.Contains(w.SQL, "notes") {
				t.Errorf("%s: %s", l.ID, w.SQL)
			}

		case "H4/with_source":
			if c.MarketingConsent || !strings.Contains(w.SQL, "consent_source") || w.Args[0] == "" {
				t.Errorf("%s: wants consent turned on together with its source: %s %v", l.ID, w.SQL, w.Args)
			}
		case "H4/withdraw":
			if !c.MarketingConsent || c.ConsentSource == "" || !strings.Contains(w.SQL, "marketing_consent = false") {
				t.Errorf("%s: wants consent withdrawn where it was given: %s", l.ID, w.SQL)
			}
		case "H4/legacy_edit", "H4/legacy_fix":
			if !hasLegacy || !strings.Contains(lw.SQL, "consent_source = null") || lw.Args[0] != c.ID {
				t.Errorf("%s: wants a contact that already has consent without a source: %+v", l.ID, lw)
			}
			if strings.Contains(w.SQL, "marketing_consent") || (kind == "legacy_edit") != strings.Contains(w.SQL, "notes") {
				t.Errorf("%s: %s", l.ID, w.SQL)
			}
		default:
			t.Errorf("%s: unknown hard negative %s/%s", l.ID, l.Code, kind)
		}
	}

	want := map[string]int{
		"H1/same_family": 50, "H1/same_given": 50,
		"H2/same": 20, "H2/case": 20, "H2/abbrev": 20, "H2/typo": 20, "H2/qualifier": 20,
		"H3/inactive": 25, "H3/closed_deals": 25, "H3/legacy_edit": 25, "H3/reactivate": 25,
		"H4/with_source": 25, "H4/withdraw": 25, "H4/legacy_edit": 25, "H4/legacy_fix": 25,
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
	if len(p.Legacy) != 100 {
		t.Errorf("%d legacy rows, want one per legacy_edit, reactivate, and legacy_fix write", len(p.Legacy))
	}
	for _, w := range p.Legacy {
		if w.Batch != 3 || writes[strings.TrimPrefix(w.ID, "legacy-")].ID == "" {
			t.Errorf("%s: batch %d, or no hard negative it belongs to", w.ID, w.Batch)
		}
	}
}

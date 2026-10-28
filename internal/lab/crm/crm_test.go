package crm

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)

func gen(seed uint64) *Dataset {
	return Generate(Options{Seed: seed, Accounts: 800, Contacts: 5000, KoreanShare: 0.3, Now: now})
}

func TestSameSeedSameData(t *testing.T) {
	a, b := gen(7), gen(7)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two runs with seed 7 differ")
	}
	if reflect.DeepEqual(a.Contacts[:50], gen(8).Contacts[:50]) {
		t.Fatal("seeds 7 and 8 produced the same contacts")
	}
}

func TestShape(t *testing.T) {
	d := gen(1)
	if len(d.Accounts) != 800 || len(d.Contacts) != 5000 {
		t.Fatalf("%d accounts, %d contacts", len(d.Accounts), len(d.Contacts))
	}
	korean := 0
	for _, c := range d.Contacts {
		if c.Person.Korean {
			korean++
		}
	}
	if share := float64(korean) / 5000; share < 0.25 || share > 0.35 {
		t.Errorf("Korean share %.2f, want about 0.30", share)
	}
	if len(d.Deals) < 800 || len(d.Activities) < 5000 {
		t.Errorf("%d deals, %d activities: too few to be realistic", len(d.Deals), len(d.Activities))
	}
}

func TestReferencesHold(t *testing.T) {
	d := gen(2)
	accounts := map[int]bool{}
	for _, a := range d.Accounts {
		accounts[a.ID] = true
	}
	contactAccount := map[int]int{}
	for _, c := range d.Contacts {
		if !accounts[c.AccountID] {
			t.Fatalf("contact %d has unknown account %d", c.ID, c.AccountID)
		}
		contactAccount[c.ID] = c.AccountID
	}
	for _, dl := range d.Deals {
		if acc, ok := contactAccount[dl.ContactID]; !ok || acc != dl.AccountID {
			t.Fatalf("deal %d: contact %d is not in account %d", dl.ID, dl.ContactID, dl.AccountID)
		}
	}
	for _, a := range d.Activities {
		if _, ok := contactAccount[a.ContactID]; !ok {
			t.Fatalf("activity %d has unknown contact %d", a.ID, a.ContactID)
		}
	}
}

func TestEmailsAreUnique(t *testing.T) {
	seen := map[string]int{}
	for _, c := range gen(3).Contacts {
		e := strings.ToLower(c.Email)
		if prev, dup := seen[e]; dup {
			t.Fatalf("contacts %d and %d share %s; near-duplicates must only come from tasks and injection", prev, c.ID, e)
		}
		seen[e] = c.ID
	}
}

// The base data follows the experiment's policy, so F3 and F4 writes are
// the only violations and their labels are unambiguous.
func TestBaseDataFollowsPolicy(t *testing.T) {
	d := gen(4)
	openDeal := map[int]bool{}
	for _, dl := range d.Deals {
		if dl.Stage == "open" {
			openDeal[dl.AccountID] = true
		}
	}
	churned := 0
	for _, c := range d.Contacts {
		if c.Status == "churned" {
			churned++
			if openDeal[c.AccountID] {
				t.Errorf("contact %d is churned while account %d has an open deal", c.ID, c.AccountID)
			}
		}
		if c.MarketingConsent && c.ConsentSource == "" {
			t.Errorf("contact %d has marketing consent without a source", c.ID)
		}
		if !slices.Contains([]string{"active", "inactive", "churned"}, c.Status) {
			t.Errorf("contact %d has status %q", c.ID, c.Status)
		}
	}
	if churned == 0 {
		t.Error("no churned contacts at all")
	}
}

func TestNameVariantsMakeNearDuplicates(t *testing.T) {
	p := Person{Korean: true, Family: "김", Given: "재훈", FamilyRoman: "Kim", GivenRoman: []string{"Jae", "hoon"}}
	want := []string{"김재훈", "Jaehoon Kim", "Kim Jaehoon", "Jae-hoon Kim"}
	if got := p.NameVariants(); !reflect.DeepEqual(got, want) {
		t.Errorf("Korean variants = %q, want %q", got, want)
	}
	e := Person{Family: "Smith", Given: "Robert", Nick: "Bob"}
	if got := e.NameVariants(); !reflect.DeepEqual(got, []string{"Robert Smith", "Bob Smith", "R. Smith"}) {
		t.Errorf("English variants = %q", got)
	}
	got := p.EmailVariants("acme.com")
	for _, w := range []string{"jaehoon.kim@acme.com", "jkim@acme.com", "j.kim@acme.com", "kim.jaehoon@acme.com"} {
		if !slices.Contains(got, w) {
			t.Errorf("email variants %q lack %s", got, w)
		}
	}
}

func TestCompanyVariants(t *testing.T) {
	got := CompanyVariants(Account{BaseName: "Bluewave", Korean: false})
	for _, w := range []string{"Bluewave Inc.", "Bluewave, Inc.", "Bluewave Corporation", "BLUEWAVE"} {
		if !slices.Contains(got, w) {
			t.Errorf("variants %q lack %q", got, w)
		}
	}
	ko := CompanyVariants(Account{BaseName: "한빛소프트", Korean: true})
	for _, w := range []string{"(주)한빛소프트", "한빛소프트 주식회사", "주식회사 한빛소프트"} {
		if !slices.Contains(ko, w) {
			t.Errorf("Korean variants %q lack %q", ko, w)
		}
	}
}

func TestContactsHaveRealisticSpread(t *testing.T) {
	d := gen(5)
	consent, titles, accounts := 0, map[string]bool{}, map[int]int{}
	for _, c := range d.Contacts {
		if c.MarketingConsent {
			consent++
		}
		titles[c.Title] = true
		accounts[c.AccountID]++
	}
	if consent < 1000 || consent > 3000 {
		t.Errorf("%d contacts consented to marketing, want roughly 40%%", consent)
	}
	if len(titles) < 15 {
		t.Errorf("only %d distinct titles", len(titles))
	}
	if len(accounts) < 700 {
		t.Errorf("contacts cover only %d of 800 accounts", len(accounts))
	}
}

func TestManyAccountsKeepUniqueNames(t *testing.T) {
	d := Generate(Options{Seed: 9, Accounts: 3000, Contacts: 100, KoreanShare: 0.3, Now: now})
	seen := map[string]bool{}
	for _, a := range d.Accounts {
		if seen[a.BaseName] || seen[a.Domain] {
			t.Fatalf("account %d repeats name %q or domain %q", a.ID, a.BaseName, a.Domain)
		}
		seen[a.BaseName], seen[a.Domain] = true, true
	}
}

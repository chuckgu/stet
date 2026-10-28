// Package crm generates the synthetic B2B CRM the experiment runs on
// (docs/lab.md). The same seed always yields the same data.
//
// The base data is clean and follows the experiment's policy: no duplicate
// emails, no churned contact at an account with an open deal, no marketing
// consent without a source. Near-duplicates and contradictions enter only
// through agent tasks and the injector, so their labels are unambiguous.
// What the base data does carry is realistic spelling drift (company
// suffixes, email patterns, romanized names), the raw material for them.
package crm

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

type Options struct {
	Seed        uint64
	Accounts    int
	Contacts    int
	KoreanShare float64 // share of contacts with Korean names
	Now         time.Time
}

type Dataset struct {
	Accounts   []Account
	Contacts   []Contact
	Deals      []Deal
	Activities []Activity
}

type Account struct {
	ID       int
	BaseName string // Bluewave, 한빛소프트
	Name     string // as stored: one of CompanyVariants
	Domain   string
	Industry string
	Korean   bool
}

// Person is a contact's name broken into parts, so callers can spell it
// the other ways people do.
type Person struct {
	Korean      bool
	Family      string   // 김, Smith
	Given       string   // 재훈, Robert
	FamilyRoman string   // Kim (Korean only)
	GivenRoman  []string // Jae, hoon (Korean only)
	Nick        string   // Bob (English only; may be empty)
}

type Contact struct {
	ID               int
	AccountID        int
	Person           Person
	Name             string // as stored: one of Person.NameVariants
	Email            string
	Phone            string
	Title            string
	Status           string // active | inactive | churned
	MarketingConsent bool
	ConsentSource    string
	Notes            string
	UpdatedAt        time.Time
}

type Deal struct {
	ID        int
	AccountID int
	ContactID int
	Stage     string // open | won | lost
	Amount    int    // dollars
	UpdatedAt time.Time
}

type Activity struct {
	ID        int
	ContactID int
	Kind      string // email | call | meeting
	Body      string
	CreatedAt time.Time
}

// NameVariants lists the ways a CRM ends up spelling one person. The first
// entry is the native form.
func (p Person) NameVariants() []string {
	if p.Korean {
		joined := strings.Join(p.GivenRoman, "")
		joined = joined[:1] + strings.ToLower(joined[1:])
		hyphen := strings.Join(p.GivenRoman, "-")
		return []string{
			p.Family + p.Given,
			joined + " " + p.FamilyRoman,
			p.FamilyRoman + " " + joined,
			hyphen + " " + p.FamilyRoman,
		}
	}
	v := []string{p.Given + " " + p.Family}
	if p.Nick != "" {
		v = append(v, p.Nick+" "+p.Family)
	}
	return append(v, p.Given[:1]+". "+p.Family)
}

// EmailVariants lists common corporate address patterns for this person.
func (p Person) EmailVariants(domain string) []string {
	given, family := strings.ToLower(p.Given), strings.ToLower(p.Family)
	if p.Korean {
		given, family = strings.ToLower(strings.Join(p.GivenRoman, "")), strings.ToLower(p.FamilyRoman)
	}
	at := "@" + domain
	v := []string{
		given + "." + family + at,
		given[:1] + family + at,
		given[:1] + "." + family + at,
		family + "." + given + at,
		given + at,
	}
	if p.Nick != "" {
		v = append(v, strings.ToLower(p.Nick)+"."+family+at)
	}
	return v
}

// CompanyVariants lists the ways one company's name gets written.
func CompanyVariants(a Account) []string {
	b := a.BaseName
	if a.Korean {
		return []string{b, "(주)" + b, b + " 주식회사", "주식회사 " + b}
	}
	return []string{b, b + " Inc.", b + ", Inc.", b + " Corporation", b + " Corp.", b + " Ltd.", strings.ToUpper(b)}
}

func Generate(o Options) *Dataset {
	r := rand.New(rand.NewPCG(o.Seed, o.Seed^0x5ce7))
	d := &Dataset{}
	pick := func(xs []string) string { return xs[r.IntN(len(xs))] }
	ago := func(maxDays int) time.Time {
		return o.Now.Add(-time.Duration(r.IntN(maxDays*24*3600)) * time.Second)
	}

	// Accounts: a quarter Korean, names unique.
	nKo := o.Accounts / 4
	koNames := uniqueCombos(r, koPrefixes, koSuffixes, nKo)
	enNames := uniqueCombos(r, enPrefixes, enSuffixes, o.Accounts-nKo)
	var koAccounts, enAccounts []int
	for i := range o.Accounts {
		a := Account{ID: i + 1, Industry: pick(industries)}
		if i%4 == 0 && len(koNames) > 0 {
			c := koNames[0]
			koNames = koNames[1:]
			a.Korean, a.BaseName = true, c.native
			a.Domain = c.roman + ".co.kr"
			koAccounts = append(koAccounts, a.ID)
		} else {
			c := enNames[0]
			enNames = enNames[1:]
			a.BaseName = c.native
			a.Domain = strings.ToLower(c.native) + "." + pick([]string{"com", "io", "co", "ai"})
			enAccounts = append(enAccounts, a.ID)
		}
		v := CompanyVariants(a)
		a.Name = v[1+r.IntN(3)]
		d.Accounts = append(d.Accounts, a)
	}

	// Contacts: Korean names mostly at Korean accounts. Each account keeps
	// one email pattern, as real companies do.
	pattern := make([]int, o.Accounts+1)
	for i := range pattern {
		pattern[i] = r.IntN(4)
	}
	emails := map[string]bool{}
	byAccount := map[int][]int{}
	for i := range o.Contacts {
		korean := r.Float64() < o.KoreanShare
		var accID int
		switch {
		case korean && r.Float64() < 0.8:
			accID = koAccounts[r.IntN(len(koAccounts))]
		case !korean && r.Float64() < 0.9:
			accID = enAccounts[r.IntN(len(enAccounts))]
		default:
			accID = 1 + r.IntN(o.Accounts)
		}
		acc := d.Accounts[accID-1]
		p := newPerson(r, korean)
		names := p.NameVariants()
		name := names[0]
		if korean && r.Float64() < 0.4 {
			name = names[1]
		}
		c := Contact{
			ID: i + 1, AccountID: accID, Person: p, Name: name,
			Email:     uniqueEmail(p.EmailVariants(acc.Domain), pattern[accID], emails),
			Phone:     phone(r, korean),
			Title:     pick(titles),
			UpdatedAt: ago(180),
		}
		if r.Float64() < 0.4 {
			c.MarketingConsent, c.ConsentSource = true, pick(consentSources)
		}
		if r.Float64() < 0.5 {
			c.Notes = pick(notes)
		}
		d.Contacts = append(d.Contacts, c)
		byAccount[accID] = append(byAccount[accID], c.ID)
	}

	// Deals: 0-3 per account, each tied to one of its contacts.
	openDeal := map[int]bool{}
	for _, a := range d.Accounts {
		cs := byAccount[a.ID]
		if len(cs) == 0 {
			continue
		}
		n := weighted(r, []int{20, 35, 30, 15})
		for range n {
			stage := []string{"open", "won", "lost"}[weighted(r, []int{40, 35, 25})]
			d.Deals = append(d.Deals, Deal{
				ID: len(d.Deals) + 1, AccountID: a.ID, ContactID: cs[r.IntN(len(cs))],
				Stage: stage, Amount: 5000 + 1000*r.IntN(246), UpdatedAt: ago(120),
			})
			if stage == "open" {
				openDeal[a.ID] = true
			}
		}
	}

	// Statuses follow the policy: nobody at an account with an open deal
	// is churned.
	for i := range d.Contacts {
		c := &d.Contacts[i]
		if openDeal[c.AccountID] {
			c.Status = []string{"active", "inactive"}[weighted(r, []int{90, 10})]
		} else {
			c.Status = []string{"active", "inactive", "churned"}[weighted(r, []int{75, 12, 13})]
		}
	}

	for _, c := range d.Contacts {
		for range 1 + r.IntN(2) {
			kind := pick([]string{"email", "call", "meeting"})
			d.Activities = append(d.Activities, Activity{
				ID: len(d.Activities) + 1, ContactID: c.ID, Kind: kind,
				Body: fmt.Sprintf(pick(activityBodies[kind]), pick(topics)), CreatedAt: ago(180),
			})
		}
	}
	return d
}

func newPerson(r *rand.Rand, korean bool) Person {
	if korean {
		f := koFamilies[r.IntN(len(koFamilies))]
		g := koGivens[r.IntN(len(koGivens))]
		return Person{Korean: true, Family: f.native, FamilyRoman: f.roman, Given: g.native, GivenRoman: g.syllables}
	}
	g := enGivens[r.IntN(len(enGivens))]
	return Person{Family: enFamilies[r.IntN(len(enFamilies))], Given: g.name, Nick: g.nick}
}

// uniqueEmail takes the account's pattern, falling back to the others and
// then a number so no two contacts share an address.
func uniqueEmail(variants []string, pattern int, used map[string]bool) string {
	for i := range variants {
		e := variants[(pattern+i)%len(variants)]
		if !used[e] {
			used[e] = true
			return e
		}
	}
	base := variants[pattern%len(variants)]
	at := strings.IndexByte(base, '@')
	for n := 2; ; n++ {
		e := fmt.Sprintf("%s%d%s", base[:at], n, base[at:])
		if !used[e] {
			used[e] = true
			return e
		}
	}
}

func phone(r *rand.Rand, korean bool) string {
	if korean {
		return fmt.Sprintf("+82-10-%04d-%04d", r.IntN(10000), r.IntN(10000))
	}
	return fmt.Sprintf("+1-%03d-%03d-%04d", 201+r.IntN(789), r.IntN(1000), r.IntN(10000))
}

func weighted(r *rand.Rand, weights []int) int {
	total := 0
	for _, w := range weights {
		total += w
	}
	n := r.IntN(total)
	for i, w := range weights {
		if n < w {
			return i
		}
		n -= w
	}
	return len(weights) - 1
}

type combo struct{ native, roman string }

// uniqueCombos returns n distinct prefix+suffix names in a seeded order.
func uniqueCombos(r *rand.Rand, prefixes, suffixes []combo, n int) []combo {
	var all []combo
	for _, p := range prefixes {
		for _, s := range suffixes {
			all = append(all, combo{p.native + s.native, p.roman + s.roman})
		}
	}
	r.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	if len(all) < n { // more accounts than combos: qualify each base name once per qualifier
		base := len(all)
		for _, q := range qualifiers {
			for _, c := range all[:base] {
				all = append(all, combo{c.native + q, c.roman + strings.ToLower(q)})
			}
		}
	}
	if len(all) < n {
		panic(fmt.Sprintf("crm: only %d unique company names for %d accounts", len(all), n))
	}
	return all[:n]
}

package tasks

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stetdb/stet/internal/lab/crm"
)

// The I set (docs/spec-v0.2-benchmark.md, chapter 4): pairs of statements of
// one shape that differ in one literal, so that one changes a few rows and
// the other changes hundreds. The rows the large sides touch are built here
// and loaded into the snapshot after the base data.

// Words the extra accounts' names are made of. Account i is prefix i%len +
// suffix (i/len)%len, so a name never repeats and needs no random draw.
var (
	extraPrefixes = []string{"Amber", "Birch", "Cobalt", "Dune", "Ember", "Fjord", "Garnet", "Harbor",
		"Indigo", "Juniper", "Kestrel", "Lumen", "Marble", "Nimbus", "Opal", "Pioneer", "Quartz",
		"Raven", "Summit", "Tundra", "Umber", "Vertex", "Willow", "Zephyr"}
	extraSuffixes = []string{"works", "forge", "labs", "grid", "stone", "field", "bridge", "point",
		"mill", "yard", "path", "loop"}
)

// newIndustries are industries the base data does not use; shape 6 takes two
// per pair so that its subquery selects only the pair's own account.
var newIndustries = []string{"Aerospace", "Agriculture", "Automotive", "Construction", "Hospitality",
	"Insurance", "Legal", "Mining", "Pharma", "Real Estate", "Telecom", "Transportation", "Gaming",
	"Nonprofit", "Government", "Consulting", "Fashion", "Food & Beverage", "Chemicals", "Shipping",
	"Security", "Sports", "Travel", "Utilities"}

// extra builds the rows added to the snapshot after the base data. Its ids
// continue the base data's.
type extra struct {
	into   *crm.Dataset // Bench.Extra
	base   *crm.Dataset
	r      *rand.Rand
	now    time.Time
	emails map[string]bool
	next   map[string]int // numbered email fallbacks, per address
}

func newExtra(base, into *crm.Dataset, r *rand.Rand, now time.Time) *extra {
	return &extra{into: into, base: base, r: r, now: now, emails: map[string]bool{}, next: map[string]int{}}
}

// account appends an account to x.into with the next free id and returns it.
// Names come from two fixed word lists in this file; the domain is the
// lowercased name + ".net".
func (x *extra) account(industry string) crm.Account {
	i := len(x.into.Accounts)
	n := len(extraPrefixes)
	name := extraPrefixes[i%n] + extraSuffixes[(i/n)%len(extraSuffixes)]
	if round := i / (n * len(extraSuffixes)); round > 0 {
		name += strconv.Itoa(round + 1)
	}
	a := crm.Account{ID: len(x.base.Accounts) + i + 1, BaseName: name, Name: name,
		Domain: strings.ToLower(name) + ".net", Industry: industry}
	x.into.Accounts = append(x.into.Accounts, a)
	return a
}

// contacts appends n contacts at the account, ids consecutive, status
// active, no consent.
func (x *extra) contacts(a crm.Account, n int) []crm.Contact {
	titles := crm.Titles()
	out := make([]crm.Contact, 0, n)
	for range n {
		korean := x.r.Float64() < 0.3
		p := crm.NewPerson(x.r, korean)
		phone := fmt.Sprintf("+1-%03d-%03d-%04d", 201+x.r.IntN(789), x.r.IntN(1000), x.r.IntN(10000))
		if korean {
			phone = fmt.Sprintf("+82-10-%04d-%04d", x.r.IntN(10000), x.r.IntN(10000))
		}
		c := crm.Contact{
			ID: len(x.base.Contacts) + len(x.into.Contacts) + 1, AccountID: a.ID, Person: p,
			Name: p.NameVariants()[0], Email: x.email(p, a.Domain, a.ID), Phone: phone,
			Title: titles[x.r.IntN(len(titles))], Status: "active", UpdatedAt: x.now,
		}
		x.into.Contacts = append(x.into.Contacts, c)
		out = append(out, c)
	}
	return out
}

// deals appends n deals at the account with the given stage, ids consecutive.
func (x *extra) deals(a crm.Account, contact int, n int, stage string) []crm.Deal {
	out := make([]crm.Deal, 0, n)
	for range n {
		e := crm.Deal{ID: len(x.base.Deals) + len(x.into.Deals) + 1, AccountID: a.ID, ContactID: contact,
			Stage: stage, Amount: 5000 + 1000*x.r.IntN(246), UpdatedAt: x.now}
		x.into.Deals = append(x.into.Deals, e)
		out = append(out, e)
	}
	return out
}

// email takes one of the person's address patterns, then the others, then a
// number, so that no two extra contacts share an address.
func (x *extra) email(p crm.Person, domain string, pattern int) string {
	v := p.EmailVariants(domain)
	for i := range v {
		if e := v[(pattern+i)%len(v)]; !x.emails[e] {
			x.emails[e] = true
			return e
		}
	}
	base := v[pattern%len(v)]
	at := strings.IndexByte(base, '@')
	for n := max(x.next[base], 2); ; n++ {
		if e := fmt.Sprintf("%s%d%s", base[:at], n, base[at:]); !x.emails[e] {
			x.emails[e], x.next[base] = true, n+1
			return e
		}
	}
}

// iPair is one pair before it is laid out in the stream.
type iPair struct {
	id           string
	shape        int
	sql          string
	small, large []any
	rowsS, rowsL int
}

// iArgText writes a bind value as text.
func iArgText(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case bool:
		return strconv.FormatBool(v)
	}
	panic(fmt.Sprintf("unsupported I argument type %T", v))
}

// buildPairs makes the pairs of every shape and the rows they need. Which of
// a pair's two accounts (or industries) is the small side is a coin toss.
func buildPairs(x *extra, r *rand.Rand, counts [6]int) ([]iPair, error) {
	seen := map[string]bool{}
	var inds []string
	for _, a := range x.base.Accounts {
		if !seen[a.Industry] {
			seen[a.Industry] = true
			inds = append(inds, a.Industry)
		}
	}
	slices.Sort(inds)
	if len(inds) == 0 {
		return nil, fmt.Errorf("the base data has no accounts")
	}
	baseInd := func() string { return inds[r.IntN(len(inds))] }
	fresh := slices.Clone(newIndustries)
	r.Shuffle(len(fresh), func(i, j int) { fresh[i], fresh[j] = fresh[j], fresh[i] })
	titles := crm.Titles()

	var pairs []iPair
	for shape := 1; shape <= 6; shape++ {
		for n := 1; n <= counts[shape-1]; n++ {
			k, m := 1+r.IntN(5), 150+r.IntN(1851)
			smallFirst := r.IntN(2) == 0 // which of the two accounts is the small side
			p := iPair{id: fmt.Sprintf("i-%d-%02d", shape, n), shape: shape, rowsS: k, rowsL: m}
			// pick returns the pair's two accounts' row counts in creation order.
			sizes := func() (int, int) {
				if smallFirst {
					return k, m
				}
				return m, k
			}
			order := func(first, second any) (small, large any) {
				if smallFirst {
					return first, second
				}
				return second, first
			}
			switch shape {
			case 1, 2, 3, 4:
				a, b := x.account(baseInd()), x.account(baseInd())
				na, nb := sizes()
				switch shape {
				case 1, 2:
					x.contacts(a, na)
					x.contacts(b, nb)
				default:
					ca, cb := x.contacts(a, 1), x.contacts(b, 1)
					stage := map[int]string{3: "open", 4: "lost"}[shape]
					x.deals(a, ca[0].ID, na, stage)
					x.deals(b, cb[0].ID, nb, stage)
				}
				var sa, la any
				switch shape {
				case 1:
					p.sql = "update contacts set status = $1 where account_id = $2"
					sa, la = order(a.ID, b.ID)
					p.small, p.large = []any{"inactive", sa}, []any{"inactive", la}
				case 2:
					p.sql = "update contacts set marketing_consent = $1, consent_source = null where email like $2"
					sa, la = order("%@"+a.Domain, "%@"+b.Domain)
					p.small, p.large = []any{false, sa}, []any{false, la}
				case 3:
					p.sql = "update deals set stage = $1 where account_id = $2 and stage = $3"
					sa, la = order(a.ID, b.ID)
					p.small, p.large = []any{"lost", sa, "open"}, []any{"lost", la, "open"}
				case 4:
					p.sql = "delete from deals where account_id = $1 and stage = $2"
					sa, la = order(a.ID, b.ID)
					p.small, p.large = []any{sa, "lost"}, []any{la, "lost"}
				}
			case 5:
				a := x.account(baseInd())
				cs := x.contacts(a, m)
				title := titles[r.IntN(len(titles))]
				first := cs[0].ID
				p.sql = "update contacts set title = $1 where id between $2 and $3"
				p.small, p.large = []any{title, first, first + k - 1}, []any{title, first, first + m - 1}
			case 6:
				if len(fresh) < 2 {
					return nil, fmt.Errorf("shape 6 needs two new industries per pair and the list of %d is used up", len(newIndustries))
				}
				ia, ib := fresh[0], fresh[1]
				fresh = fresh[2:]
				na, nb := sizes()
				x.contacts(x.account(ia), na)
				x.contacts(x.account(ib), nb)
				sa, la := order(ia, ib)
				p.sql = "update contacts set status = $1 where account_id in (select id from accounts where industry = $2)"
				p.small, p.large = []any{"inactive", sa}, []any{"inactive", la}
			}
			pairs = append(pairs, p)
		}
	}
	return pairs, nil
}

// addISet appends the I pairs to b.Stream and their rows to b.Extra. sel is
// the part's selection stream, continued where the V selection ended.
func addISet(b *Bench, d *crm.Dataset, o BenchOptions, sel *rand.Rand) error {
	// Which pairs of each shape are in the part.
	sub := map[string]bool{}
	for shape := 1; shape <= 6; shape++ {
		n, have := o.SubPairs[shape-1], o.Pairs[shape-1]
		if n > have {
			return fmt.Errorf("shape %d has %d pairs, SubPairs needs %d", shape, have, n)
		}
		if n <= 0 {
			continue
		}
		nums := make([]int, have)
		for i := range nums {
			nums[i] = i + 1
		}
		sel.Shuffle(len(nums), func(i, j int) { nums[i], nums[j] = nums[j], nums[i] })
		for _, num := range nums[:n] {
			sub[fmt.Sprintf("i-%d-%02d", shape, num)] = true
		}
	}

	r := stream(o.Plan.Seed, iStream)
	x := newExtra(d, &b.Extra, r, o.Plan.Now)
	pairs, err := buildPairs(x, r, o.Pairs)
	if err != nil {
		return err
	}
	r.Shuffle(len(pairs), func(i, j int) { pairs[i], pairs[j] = pairs[j], pairs[i] })

	seen := [7]int{} // pairs laid out so far, per shape
	for _, p := range pairs {
		mk := func(kind string, args []any, rows int) (Stmt, error) {
			in, err := Inline(p.sql, args)
			if err != nil {
				return Stmt{}, fmt.Errorf("pair %s: %w", p.id, err)
			}
			s := Stmt{ID: p.id + "-" + kind, Set: SetI, Code: "I", Kind: kind, Expected: Run, Role: RoleI,
				App: Injector, SQL: p.sql, Inline: in, Sub: sub[p.id], Unit: p.id, Pair: p.id, Shape: p.shape, Rows: rows}
			if kind == "large" {
				s.Expected = Block
			}
			for _, a := range args {
				s.Args = append(s.Args, iArgText(a))
			}
			return s, nil
		}
		small, err := mk("small", p.small, p.rowsS)
		if err != nil {
			return err
		}
		large, err := mk("large", p.large, p.rowsL)
		if err != nil {
			return err
		}
		if seen[p.shape]%2 == 1 {
			small, large = large, small
		}
		seen[p.shape]++
		b.Stream = append(b.Stream, small, large)
	}
	return nil
}

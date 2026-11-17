package tasks

import (
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stetdb/stet/internal/lab/crm"
	"github.com/stetdb/stet/internal/sqlparse"
)

func withI(t *testing.T, practice bool) (*crm.Dataset, Bench, BenchOptions) {
	t.Helper()
	d := data()
	_, o := BenchPreset(practice, now)
	o.Attacks = false
	b, err := BuildBench(d, o)
	if err != nil {
		t.Fatal(err)
	}
	return d, b, o
}

func iStmts(b Bench) []Stmt {
	var out []Stmt
	for _, s := range b.Stream {
		if s.Set == SetI {
			out = append(out, s)
		}
	}
	return out
}

var iShapeRE = [7]*regexp.Regexp{
	nil,
	regexp.MustCompile(`^update contacts set status = 'inactive' where account_id = (\d+)$`),
	regexp.MustCompile(`^update contacts set marketing_consent = false, consent_source = null where email like '%@([a-z0-9.]+\.net)'$`),
	regexp.MustCompile(`^update deals set stage = 'lost' where account_id = (\d+) and stage = 'open'$`),
	regexp.MustCompile(`^delete from deals where account_id = (\d+) and stage = 'lost'$`),
	regexp.MustCompile(`^update contacts set title = '([^']+)' where id between (\d+) and (\d+)$`),
	regexp.MustCompile(`^update contacts set status = 'inactive' where account_id in \(select id from accounts where industry = '([^']+)'\)$`),
}

// touched computes, on the base data and the extra rows together, the rows
// a statement changes.
func touched(d *crm.Dataset, x crm.Dataset, s Stmt) (string, []int) {
	m := iShapeRE[s.Shape].FindStringSubmatch(s.Inline)
	if m == nil {
		return "", nil
	}
	contacts := append(slices.Clone(d.Contacts), x.Contacts...)
	deals := append(slices.Clone(d.Deals), x.Deals...)
	accounts := append(slices.Clone(d.Accounts), x.Accounts...)
	num := func(i int) int { n, _ := strconv.Atoi(m[i]); return n }
	var ids []int
	switch s.Shape {
	case 1:
		for _, c := range contacts {
			if c.AccountID == num(1) {
				ids = append(ids, c.ID)
			}
		}
		return "contacts", ids
	case 2:
		for _, c := range contacts {
			if strings.HasSuffix(c.Email, "@"+m[1]) {
				ids = append(ids, c.ID)
			}
		}
		return "contacts", ids
	case 3, 4:
		stage := map[int]string{3: "open", 4: "lost"}[s.Shape]
		for _, e := range deals {
			if e.AccountID == num(1) && e.Stage == stage {
				ids = append(ids, e.ID)
			}
		}
		return "deals", ids
	case 5:
		for _, c := range contacts {
			if c.ID >= num(2) && c.ID <= num(3) {
				ids = append(ids, c.ID)
			}
		}
		return "contacts", ids
	}
	in := map[int]bool{}
	for _, a := range accounts {
		if a.Industry == m[1] {
			in[a.ID] = true
		}
	}
	for _, c := range contacts {
		if in[c.AccountID] {
			ids = append(ids, c.ID)
		}
	}
	return "contacts", ids
}

func TestISetHasSixShapesOfTenPairs(t *testing.T) {
	_, b, _ := withI(t, false)
	is := iStmts(b)
	if len(is) != 120 {
		t.Fatalf("%d I statements, want 120", len(is))
	}
	// V comes first, then I.
	seenI := false
	for _, s := range b.Stream {
		if s.Set == SetI {
			seenI = true
		} else if seenI {
			t.Fatalf("seq %d is %s after the I set began", s.Seq, s.Set)
		}
	}
	perShape := map[int]int{}
	smallFirst := map[int]int{}
	pairs := map[string]bool{}
	for i := 0; i < len(is); i += 2 {
		a, c := is[i], is[i+1]
		if a.Pair != c.Pair || a.Pair == "" {
			t.Fatalf("statements %d and %d are not one pair: %q %q", i, i+1, a.Pair, c.Pair)
		}
		if pairs[a.Pair] {
			t.Fatalf("pair %s appears twice", a.Pair)
		}
		pairs[a.Pair] = true
		small, large := a, c
		if a.Kind == "large" {
			small, large = c, a
		} else {
			smallFirst[a.Shape]++
		}
		if small.Kind != "small" || large.Kind != "large" {
			t.Fatalf("pair %s kinds %s, %s", a.Pair, a.Kind, c.Kind)
		}
		if small.Expected != Run || large.Expected != Block {
			t.Fatalf("pair %s expected %s, %s", a.Pair, small.Expected, large.Expected)
		}
		if small.ID != a.Pair+"-small" || large.ID != a.Pair+"-large" {
			t.Fatalf("pair %s ids %s, %s", a.Pair, small.ID, large.ID)
		}
		if !regexp.MustCompile(`^i-[1-6]-\d\d$`).MatchString(a.Pair) || a.Pair != "i-"+strconv.Itoa(a.Shape)+a.Pair[3:] {
			t.Fatalf("pair id %q for shape %d", a.Pair, a.Shape)
		}
		for _, s := range []Stmt{a, c} {
			if s.Set != SetI || s.Code != "I" || s.Role != RoleI || s.App != "import-job" || s.Unit != s.Pair || s.Shape != a.Shape {
				t.Fatalf("%s: set %s code %s role %s app %s unit %q", s.ID, s.Set, s.Code, s.Role, s.App, s.Unit)
			}
		}
		perShape[a.Shape]++
	}
	for sh := 1; sh <= 6; sh++ {
		if perShape[sh] != 10 || smallFirst[sh] != 5 {
			t.Fatalf("shape %d: %d pairs, %d with the small statement first", sh, perShape[sh], smallFirst[sh])
		}
	}
	// the pairs are shuffled, not grouped by shape
	var shapes []int
	for i := 0; i < len(is); i += 2 {
		shapes = append(shapes, is[i].Shape)
	}
	if slices.IsSorted(shapes) {
		t.Fatal("pairs are in shape order, not shuffled")
	}
}

func TestPairsDifferInOneLiteral(t *testing.T) {
	_, b, _ := withI(t, false)
	is := iStmts(b)
	toAny := func(args []string) []any {
		out := make([]any, len(args))
		for i, a := range args {
			switch n, err := strconv.Atoi(a); {
			case a == "false" || a == "true":
				out[i] = a == "true"
			case err == nil:
				out[i] = n
			default:
				out[i] = a
			}
		}
		return out
	}
	for i := 0; i < len(is); i += 2 {
		a, c := is[i], is[i+1]
		if a.SQL != c.SQL {
			t.Fatalf("pair %s: SQL differs:\n%s\n%s", a.Pair, a.SQL, c.SQL)
		}
		if len(a.Args) != len(c.Args) || len(a.Args) == 0 {
			t.Fatalf("pair %s: %d and %d args", a.Pair, len(a.Args), len(c.Args))
		}
		diff := 0
		for j := range a.Args {
			if a.Args[j] != c.Args[j] {
				diff++
			}
		}
		if diff != 1 {
			t.Fatalf("pair %s: args differ at %d indexes: %v %v", a.Pair, diff, a.Args, c.Args)
		}
		for _, s := range []Stmt{a, c} {
			in, err := Inline(s.SQL, toAny(s.Args))
			if err != nil || in != s.Inline {
				t.Fatalf("%s: Inline = %q (%v), statement has %q", s.ID, in, err, s.Inline)
			}
			if !iShapeRE[s.Shape].MatchString(s.Inline) {
				t.Fatalf("%s: %q does not match shape %d", s.ID, s.Inline, s.Shape)
			}
			if strings.Count(s.SQL, "$") != len(s.Args) {
				t.Fatalf("%s: placeholders and args disagree: %q", s.ID, s.SQL)
			}
		}
	}
}

func TestISetTouchesTheRowsItSays(t *testing.T) {
	d, b, _ := withI(t, false)
	is := iStmts(b)
	owner := map[string]string{} // table/id -> pair
	rows := map[string][]int{}
	for _, s := range is {
		table, ids := touched(d, b.Extra, s)
		if len(ids) != s.Rows {
			t.Fatalf("%s: touches %d rows, Rows = %d", s.ID, len(ids), s.Rows)
		}
		switch s.Kind {
		case "small":
			if s.Rows < 1 || s.Rows > 5 {
				t.Fatalf("%s: small with %d rows", s.ID, s.Rows)
			}
		case "large":
			if s.Rows < 150 || s.Rows > 2000 {
				t.Fatalf("%s: large with %d rows", s.ID, s.Rows)
			}
		}
		limit := map[string]int{"contacts": len(d.Contacts), "deals": len(d.Deals)}[table]
		for _, id := range ids {
			if id <= limit {
				t.Fatalf("%s touches %s %d of the base data", s.ID, table, id)
			}
			k := table + "/" + strconv.Itoa(id)
			if p, ok := owner[k]; ok && p != s.Pair {
				t.Fatalf("%s %d is touched by pairs %s and %s", table, id, p, s.Pair)
			}
			owner[k] = s.Pair
		}
		rows[s.ID] = ids
	}
	for _, a := range b.Extra.Accounts {
		if a.ID <= len(d.Accounts) {
			t.Fatalf("extra account %d is in the base data's range", a.ID)
		}
	}
	// the rows of shape 5's small statement are a prefix of the large one's
	for i := 0; i < len(is); i += 2 {
		if is[i].Shape != 5 {
			continue
		}
		small, large := is[i], is[i+1]
		if small.Kind == "large" {
			small, large = large, small
		}
		s, l := rows[small.ID], rows[large.ID]
		if len(s) > len(l) || !slices.Equal(s, l[:len(s)]) {
			t.Fatalf("pair %s: small rows %v are not a prefix of the large rows", small.Pair, s)
		}
	}
	// ids continue the base data's
	if got := b.Extra.Accounts[0].ID; got != len(d.Accounts)+1 {
		t.Fatalf("first extra account id %d", got)
	}
	if got := b.Extra.Contacts[0].ID; got != len(d.Contacts)+1 {
		t.Fatalf("first extra contact id %d", got)
	}
	if len(b.Extra.Deals) > 0 && b.Extra.Deals[0].ID != len(d.Deals)+1 {
		t.Fatalf("first extra deal id %d", b.Extra.Deals[0].ID)
	}
	for i, c := range b.Extra.Contacts {
		if c.ID != len(d.Contacts)+1+i || c.Status != "active" || c.MarketingConsent {
			t.Fatalf("extra contact %d: id %d status %s consent %v", i, c.ID, c.Status, c.MarketingConsent)
		}
	}
	// no row a P or V statement names is touched
	named := regexp.MustCompile(`\b(?:id|account_id|contact_id)\s*=\s*(\d+)`)
	touchedID := map[string]bool{} // table/id
	for k := range owner {
		touchedID[k] = true
	}
	for _, s := range b.Stream {
		if s.Set == SetI {
			continue
		}
		table := "contacts"
		if strings.Contains(s.Inline, " deals ") {
			table = "deals"
		}
		for _, m := range named.FindAllStringSubmatch(s.Inline, -1) {
			if touchedID[table+"/"+m[1]] {
				t.Fatalf("%s names %s id %s, which an I statement touches", s.ID, table, m[1])
			}
		}
	}
	// extra accounts do not reuse a base domain or name
	base := map[string]bool{}
	for _, a := range d.Accounts {
		base[a.Domain], base[a.Name] = true, true
	}
	for _, a := range b.Extra.Accounts {
		if !strings.HasSuffix(a.Domain, ".net") || base[a.Domain] || base[a.Name] || a.Domain != strings.ToLower(a.Name)+".net" {
			t.Fatalf("extra account %+v", a)
		}
	}
}

func TestStetCanCountShapesOneToFive(t *testing.T) {
	_, b, _ := withI(t, false)
	tb := sqlparse.Tables{PK: map[string]string{"contacts": "id", "deals": "id"}}
	for _, s := range iStmts(b) {
		for _, text := range []string{s.Inline, s.SQL} {
			r, err := sqlparse.Classify(Tagged(s.Tag, text), tb)
			if err != nil {
				t.Fatalf("%s: %v", s.ID, err)
			}
			if r.Kind != sqlparse.Measured {
				t.Fatalf("%s: kind %v for %q", s.ID, r.Kind, text)
			}
			want := sqlparse.MethodCount
			if s.Shape == 6 {
				want = sqlparse.MethodEstimate
			}
			if r.Measure.Method != want {
				t.Fatalf("%s: method %v, want %v for %q", s.ID, r.Measure.Method, want, text)
			}
		}
	}
}

func TestPracticeAndPartPairs(t *testing.T) {
	per := func(b Bench, only func(Stmt) bool) [6]int {
		var n [6]int
		for _, s := range iStmts(b) {
			if only(s) && s.Kind == "small" {
				n[s.Shape-1]++
			}
		}
		return n
	}
	all := func(Stmt) bool { return true }
	want := [6]int{4, 4, 3, 3, 3, 3}

	_, practice, _ := withI(t, true)
	if n := len(iStmts(practice)); n != 40 {
		t.Fatalf("practice has %d I statements, want 40", n)
	}
	if got := per(practice, all); got != want {
		t.Fatalf("practice pairs per shape %v, want %v", got, want)
	}
	for _, s := range iStmts(practice) {
		if s.Sub {
			t.Fatalf("practice statement %s is Sub", s.ID)
		}
	}

	_, b, _ := withI(t, false)
	if got := per(b, func(s Stmt) bool { return s.Sub }); got != want {
		t.Fatalf("Sub pairs per shape %v, want %v", got, want)
	}
	is := iStmts(b)
	for i := 0; i < len(is); i += 2 {
		if is[i].Sub != is[i+1].Sub {
			t.Fatalf("pair %s: only one statement is Sub", is[i].Pair)
		}
	}
	// V's own part is not affected
	subV := 0
	for _, s := range b.Stream {
		if s.Set == SetV && s.Sub {
			subV++
		}
	}
	if subV != 9*25 {
		t.Fatalf("%d V statements in the part, want 225", subV)
	}
}

func TestISetLeavesTheVStreamAlone(t *testing.T) {
	_, with, o := withI(t, false)
	o.Pairs, o.SubPairs = [6]int{}, [6]int{}
	without, err := BuildBench(data(), o)
	if err != nil {
		t.Fatal(err)
	}
	var a, c []Stmt
	for _, s := range with.Stream {
		if s.Set != SetI {
			a = append(a, s)
		}
	}
	c = without.Stream
	if len(without.Extra.Accounts)+len(without.Extra.Contacts)+len(without.Extra.Deals) != 0 {
		t.Fatal("a build without pairs has extra rows")
	}
	if !reflect.DeepEqual(a, c) {
		t.Fatal("the P and V statements changed when the I set was added")
	}
}

package tasks

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/lab/crm"
	"github.com/stetdb/stet/internal/sqlparse"
)

var xPaths = []string{
	"s5.do", "s5.function", "s5.call", "s5.view", "s5.prepare_execute", "s5.function_call_message",
	"s5.oversize_message", "s5.unknown_grammar", "s5.multi_statement", "s5.cte", "s5.copy",
	"s5.do.rows300", "s5.function.rows300", "s5.view.rows300", "gap.prep", "gap.multi_row_shape", "gap.bind",
	"s8.large_value", "s8.burst", "s8.locked_row", "split",
	"s4.truncate", "s4.drop_table", "s4.disable_trigger", "s4.update_no_where", "s4.range_delete",
}

func withX(t *testing.T) (*crm.Dataset, Bench) {
	t.Helper()
	d := data()
	_, o := BenchPreset(false, now)
	b, err := BuildBench(d, o)
	if err != nil {
		t.Fatal(err)
	}
	return d, b
}

func xByID(b Bench) map[string]Stmt {
	m := map[string]Stmt{}
	for _, s := range b.Stream {
		m[s.ID] = s
	}
	return m
}

func attacksOf(b Bench, path string) []Attack {
	var out []Attack
	for _, a := range b.Attacks {
		if a.Path == path {
			out = append(out, a)
		}
	}
	return out
}

func TestXSetCoversEveryPathOfTheSpec(t *testing.T) {
	_, b := withX(t)
	var paths []string
	count := map[string]int{}
	for _, a := range b.Attacks {
		if len(paths) == 0 || paths[len(paths)-1] != a.Path {
			paths = append(paths, a.Path)
		}
		count[a.Path]++
	}
	if !reflect.DeepEqual(paths, xPaths) {
		t.Fatalf("paths in order = %v, want %v", paths, xPaths)
	}
	for _, p := range xPaths {
		want := 1
		switch p {
		case "gap.multi_row_shape", "gap.bind":
			want = 40
		case "split":
			want = 10
		}
		if count[p] != want {
			t.Errorf("%s: %d attacks, want %d", p, count[p], want)
		}
	}
	for _, p := range []string{"gap.multi_row_shape", "gap.bind"} {
		per := map[string]int{}
		for _, a := range attacksOf(b, p) {
			per[a.Payload]++
		}
		if !reflect.DeepEqual(per, map[string]int{"F1": 10, "F2": 10, "F3": 10, "F4": 10}) {
			t.Errorf("%s payloads %v", p, per)
		}
	}

	byID := xByID(b)
	used := map[string]bool{}
	prep := map[string]bool{}
	for _, a := range b.Attacks {
		if len(a.Steps) == 0 {
			t.Fatalf("%s has no steps", a.Path)
		}
		for _, id := range a.Steps {
			s, ok := byID[id]
			if !ok {
				t.Fatalf("%s: step %s is not in the stream", a.Path, id)
			}
			if used[id] {
				t.Errorf("step %s is in two attacks", id)
			}
			used[id] = true
			if s.Set != SetX || s.Path != a.Path || s.Wire == "" {
				t.Errorf("%s: set %q path %q wire %q", id, s.Set, s.Path, s.Wire)
			}
			if a.Path == "gap.prep" {
				prep[id] = true
				if s.Role != RoleSync || s.App != CRMSync || s.Code != "prep" || s.Expected != "" {
					t.Errorf("prep step %s: %+v", id, s)
				}
				continue
			}
			if s.Role != RoleX || s.App != Injector || s.Expected != Block {
				t.Errorf("%s: role %q app %q expected %q", id, s.Role, s.App, s.Expected)
			}
			if s.Code != a.Code {
				t.Errorf("%s: code %q, attack %q", id, s.Code, a.Code)
			}
		}
	}
	n := 0
	for _, s := range b.Stream {
		if s.Set == SetX {
			n++
			if !used[s.ID] {
				t.Errorf("X statement %s is in no attack", s.ID)
			}
		}
	}
	if n != len(used) {
		t.Errorf("%d X statements, %d steps", n, len(used))
	}
	needed := map[string]bool{}
	for _, p := range []string{"gap.multi_row_shape", "gap.bind"} {
		for _, a := range attacksOf(b, p) {
			s := byID[a.Steps[0]]
			if a.Payload != "F2" {
				if s.Needs != "" {
					t.Errorf("%s needs %q", s.ID, s.Needs)
				}
				continue
			}
			if !prep[s.Needs] || needed[s.Needs] {
				t.Errorf("%s: Needs %q is not a free prep step", s.ID, s.Needs)
			}
			needed[s.Needs] = true
		}
	}
	if len(prep) != 20 || len(needed) != 20 {
		t.Errorf("%d prep steps, %d needed", len(prep), len(needed))
	}
	if len(b.Fixtures) != 4 {
		t.Errorf("%d fixtures", len(b.Fixtures))
	}
}

func stepsOf(b Bench, a Attack) []Stmt {
	byID := xByID(b)
	var out []Stmt
	for _, id := range a.Steps {
		out = append(out, byID[id])
	}
	return out
}

var xShapeRE = map[string]*regexp.Regexp{
	"s5.do":                   regexp.MustCompile(`^do \$\$ begin update contacts set marketing_consent = true where id = \d+; end \$\$$`),
	"s5.function":             regexp.MustCompile(`^select set_consent\(\d+\)$`),
	"s5.call":                 regexp.MustCompile(`^call mark_consent\(\d+\)$`),
	"s5.view":                 regexp.MustCompile(`^update contact_prefs set marketing_consent = true where id = \d+$`),
	"s5.oversize_message":     regexp.MustCompile(`^update contacts set notes = '@PAD@', marketing_consent = true where id = \d+$`),
	"s5.unknown_grammar":      regexp.MustCompile(`^update contacts system_user set marketing_consent = true where system_user\.id = \d+$`),
	"s5.multi_statement":      regexp.MustCompile(`^update contacts set marketing_consent = true where id = \d+; select 1$`),
	"s5.cte":                  regexp.MustCompile(`^with w as \(update contacts set marketing_consent = true where id = \d+ returning id\) select count\(\*\) from w$`),
	"s5.copy":                 regexp.MustCompile(`^copy contacts \(account_id, name, email, title, status\) from stdin$`),
	"s5.do.rows300":           regexp.MustCompile(`^do \$\$ begin update contacts set notes = 'bulk' where id between \d+ and \d+; end \$\$$`),
	"s5.function.rows300":     regexp.MustCompile(`^select refresh_notes\(\d+, \d+\)$`),
	"s5.view.rows300":         regexp.MustCompile(`^update contact_prefs set notes = 'bulk' where id between \d+ and \d+$`),
	"gap.prep":                regexp.MustCompile(`^update contacts set title = '[^']+', updated_at = now\(\) where id = \d+$`),
	"s8.large_value":          regexp.MustCompile(`^update contacts set notes = '@PAD@', marketing_consent = true where id = \d+$`),
	"s8.burst":                regexp.MustCompile(`^update contacts set marketing_consent = true where id = \d+$`),
	"s8.locked_row":           regexp.MustCompile(`^update contacts set marketing_consent = true where id = \d+$`),
	"s4.truncate":             regexp.MustCompile(`^truncate deals$`),
	"s4.drop_table":           regexp.MustCompile(`^drop table activities$`),
	"s4.disable_trigger":      regexp.MustCompile(`^alter table contacts disable trigger all$`),
	"s4.update_no_where":      regexp.MustCompile(`^update contacts set status = 'inactive'$`),
	"s4.range_delete":         regexp.MustCompile(`^delete from deals where id >= 1$`),
	"gap.multi_row_shape/F1":  regexp.MustCompile(`^insert into contacts \(account_id, name, email, title, status\) select \d+, '[^']+', '[^']+', '[^']+', 'active'$`),
	"gap.multi_row_shape/F2":  regexp.MustCompile(`^update contacts set title = '[^']+' where id between (\d+) and (\d+)$`),
	"gap.multi_row_shape/F3":  regexp.MustCompile(`^update contacts set status = 'churned' where id between (\d+) and (\d+)$`),
	"gap.multi_row_shape/F4":  regexp.MustCompile(`^update contacts set marketing_consent = true where id between (\d+) and (\d+)$`),
	"gap.bind/F1":             regexp.MustCompile(`^insert into contacts \(account_id, name, email, title, status\) values \('?\d+'?, '[^']+', '[^']+', '[^']+', 'active'\)$`),
	"gap.bind/F2":             regexp.MustCompile(`^update contacts set title = '[^']+' where id = \d+$`),
	"gap.bind/F3":             regexp.MustCompile(`^update contacts set status = 'churned' where id = \d+$`),
	"gap.bind/F4":             regexp.MustCompile(`^update contacts set marketing_consent = true where id = \d+$`),
	"split/contacts":          regexp.MustCompile(`^update contacts set notes = 'archived' where id between (\d+) and (\d+)$`),
	"split/deals":             regexp.MustCompile(`^update deals set stage = 'lost' where id between (\d+) and (\d+)$`),
	"gap.bind/F1-placeholder": regexp.MustCompile(`^insert into contacts \(account_id, name, email, title, status\) values \(\$1, \$2, \$3, \$4, \$5\)$`),
}

func TestXStatementsAreTheSpecsShapes(t *testing.T) {
	_, b := withX(t)
	for _, a := range b.Attacks {
		steps := stepsOf(b, a)
		for _, s := range steps {
			key := a.Path
			switch a.Path {
			case "gap.multi_row_shape", "gap.bind":
				key += "/" + a.Payload
			case "split":
				key += "/contacts"
				if strings.HasPrefix(s.Inline, "update deals") {
					key = "split/deals"
				}
			case "s5.prepare_execute", "s5.function_call_message":
				continue
			}
			re := xShapeRE[key]
			if re == nil {
				t.Fatalf("no expectation for %s", key)
			}
			if !re.MatchString(s.Inline) {
				t.Errorf("%s: %q does not match %s", s.ID, s.Inline, re)
			}
			if s.Wire != WireSimple && s.Wire != WireBind && s.Wire != WireCopy {
				t.Errorf("%s: wire %q", s.ID, s.Wire)
			}
			if s.Tag != "" && !regexp.MustCompile(`^[0-9]{12}$`).MatchString(s.Tag) {
				t.Errorf("%s: tag %q", s.ID, s.Tag)
			}
		}
		switch a.Path {
		case "s5.prepare_execute":
			if len(steps) != 2 ||
				!regexp.MustCompile(`^prepare w as update contacts set marketing_consent = true where id = \d+$`).MatchString(steps[0].Inline) ||
				steps[1].Inline != "execute w" {
				t.Errorf("prepare_execute: %+v", steps)
			}
		case "s5.function_call_message":
			s := steps[0]
			if len(steps) != 1 || s.Wire != WireFunctionCall || s.Tag != "" || s.SQL != "set_consent(integer)" || len(s.Args) != 1 {
				t.Errorf("function_call_message: %+v", s)
			}
		case "s5.oversize_message":
			if steps[0].Pad != 64<<20+1 {
				t.Errorf("oversize pad %d", steps[0].Pad)
			}
		case "s8.large_value":
			s := steps[0]
			if s.Pad != 300000 || strings.Index(s.Inline, "@PAD@") > strings.Index(s.Inline, "marketing_consent") {
				t.Errorf("large_value: pad %d, %q", s.Pad, s.Inline)
			}
		case "s5.copy":
			s := steps[0]
			if s.Wire != WireCopy {
				t.Errorf("copy wire %q", s.Wire)
			}
			lines := strings.Split(strings.TrimSuffix(s.CopyData, "\n"), "\n")
			if len(lines) != 300 {
				t.Fatalf("copy has %d lines", len(lines))
			}
			for _, l := range lines {
				if len(strings.Split(l, "\t")) != 5 {
					t.Fatalf("copy line %q", l)
				}
			}
		case "gap.bind":
			s := steps[0]
			if s.Wire != WireBind || strings.Contains(s.Inline, "$") || len(s.Args) == 0 {
				t.Errorf("bind %s: wire %q inline %q", s.ID, s.Wire, s.Inline)
			}
			if strings.Contains(s.SQL, "'") || regexp.MustCompile(`\d`).MatchString(regexp.MustCompile(`\$\d+`).ReplaceAllString(s.SQL, "")) {
				t.Errorf("bind %s SQL has a literal or a digit: %q", s.ID, s.SQL)
			}
			if a.Payload == "F1" && !xShapeRE["gap.bind/F1-placeholder"].MatchString(s.SQL) {
				t.Errorf("bind F1 SQL %q", s.SQL)
			}
		case "gap.multi_row_shape":
			if steps[0].Wire != WireSimple {
				t.Errorf("multi_row_shape wire %q", steps[0].Wire)
			}
		}
	}
}

func rangeOf(t *testing.T, re *regexp.Regexp, sql string) (int, int) {
	t.Helper()
	m := re.FindStringSubmatch(sql)
	lo, _ := strconv.Atoi(m[1])
	hi, _ := strconv.Atoi(m[2])
	return lo, hi
}

func TestSplitBurstAndLock(t *testing.T) {
	_, b := withX(t)
	split := attacksOf(b, "split")
	for i, a := range split {
		bundle := i + 1
		if len(a.Steps) != 12 || a.PauseBefore != 70*time.Second || a.Limit != 500 || a.Payload != "bulk" || a.Conns != 1 || a.Breaks {
			t.Fatalf("bundle %d: %+v", bundle, a)
		}
		re, table := xShapeRE["split/contacts"], "contacts"
		if bundle%2 == 0 {
			re, table = xShapeRE["split/deals"], "deals"
		}
		var first int
		for j, s := range stepsOf(b, a) {
			if !strings.HasPrefix(s.Inline, "update "+table+" ") {
				t.Fatalf("bundle %d step %d writes %q", bundle, j, s.Inline)
			}
			lo, hi := rangeOf(t, re, s.Inline)
			if hi-lo+1 != 50 {
				t.Errorf("bundle %d step %d spans %d ids", bundle, j, hi-lo+1)
			}
			if j == 0 {
				first = lo
			} else if lo != first+50*j {
				t.Errorf("bundle %d step %d starts at %d, want %d", bundle, j, lo, first+50*j)
			}
		}
		if !strings.Contains(a.Probe, table) || !strings.Contains(a.Probe, fmt.Sprintf("between %d and %d", first, first+599)) {
			t.Errorf("bundle %d probe %q", bundle, a.Probe)
		}
	}
	// bundles of one table do not share ids
	for _, table := range []string{"contacts", "deals"} {
		end := 0
		for i, a := range split {
			if (i%2 == 0) != (table == "contacts") {
				continue
			}
			lo, _ := rangeOf(t, xShapeRE["split/"+table], stepsOf(b, a)[0].Inline)
			if lo <= end {
				t.Errorf("%s bundle %d starts at %d, previous ends at %d", table, i+1, lo, end)
			}
			end = lo + 599
		}
	}

	burst := attacksOf(b, "s8.burst")[0]
	if len(burst.Steps) != 200 || burst.Conns != 32 {
		t.Fatalf("burst: %d steps, %d conns", len(burst.Steps), burst.Conns)
	}
	ids := map[int]bool{}
	lo, hi := 1<<30, 0
	for _, s := range stepsOf(b, burst) {
		n, _ := strconv.Atoi(regexp.MustCompile(`id = (\d+)$`).FindStringSubmatch(s.Inline)[1])
		ids[n] = true
		lo, hi = min(lo, n), max(hi, n)
	}
	if len(ids) != 200 || hi-lo != 199 {
		t.Errorf("burst ids: %d distinct, %d..%d", len(ids), lo, hi)
	}
	if want := fmt.Sprintf("select count(*) from contacts where id between %d and %d and marketing_consent and consent_source is null", lo, hi); burst.Probe != want {
		t.Errorf("burst probe %q", burst.Probe)
	}

	lock := attacksOf(b, "s8.locked_row")[0]
	n := regexp.MustCompile(`id = (\d+)$`).FindStringSubmatch(stepsOf(b, lock)[0].Inline)[1]
	if lock.LockFor != 2*time.Second || lock.LockSQL != "select 1 from contacts where id = "+n+" for update" {
		t.Errorf("lock: %v %q (row %s)", lock.LockFor, lock.LockSQL, n)
	}
	for _, a := range b.Attacks {
		if a.Path != "s8.burst" && a.Path != "split" && a.Limit != 0 || a.Path != "s8.burst" && a.Conns != 1 {
			t.Errorf("%s: limit %d conns %d", a.Path, a.Limit, a.Conns)
		}
	}
}

func TestBreakingPathsComeLast(t *testing.T) {
	_, b := withX(t)
	broke := false
	for _, a := range b.Attacks {
		if broke && !a.Breaks {
			t.Errorf("%s follows a breaking attack", a.Path)
		}
		broke = broke || a.Breaks
		if a.Breaks != strings.HasPrefix(a.Path, "s4.") {
			t.Errorf("%s: Breaks %v", a.Path, a.Breaks)
		}
	}
	last := b.Attacks[len(b.Attacks)-5:]
	for i, a := range last {
		if a.Path != xPaths[len(xPaths)-5+i] {
			t.Errorf("last five: %d is %s", i, a.Path)
		}
	}
}

func TestStetReadsTheXShapesAsTheSpecExpects(t *testing.T) {
	tb := sqlparse.Tables{PK: map[string]string{"contacts": "id", "deals": "id"}}
	_, b := withX(t)
	check := func(a Attack, s Stmt) {
		if s.Wire == WireFunctionCall {
			return // not SQL: the message names the function and carries the argument
		}
		r, err := sqlparse.Classify(s.Inline, tb)
		switch a.Path {
		case "s5.unknown_grammar":
			if err == nil {
				t.Errorf("%s: Classify accepted %q", s.ID, s.Inline)
			}
			return
		}
		if err != nil {
			t.Errorf("%s: %v", s.ID, err)
			return
		}
		switch {
		case a.Path == "s5.multi_statement":
			if r.Kind != sqlparse.Unsupported || r.Reason != "multi_statement" {
				t.Errorf("%s: %v %q", s.ID, r.Kind, r.Reason)
			}
		case a.Path == "s5.cte":
			if r.Kind != sqlparse.Unsupported || r.Reason != "nested_write" {
				t.Errorf("%s: %v %q", s.ID, r.Kind, r.Reason)
			}
		case a.Path == "s4.truncate":
			if r.Kind != sqlparse.Unsupported || r.Reason != "truncate" {
				t.Errorf("%s: %v %q", s.ID, r.Kind, r.Reason)
			}
		case slices.Contains([]string{"s5.function", "s5.call", "s5.view", "s4.drop_table", "s4.disable_trigger"}, a.Path):
			if r.Kind != sqlparse.Pass {
				t.Errorf("%s: %v %q", s.ID, r.Kind, r.Reason)
			}
		case a.Path == "gap.multi_row_shape" || a.Path == "split":
			want := sqlparse.MethodCount
			if a.Payload == "F1" {
				want = sqlparse.MethodEstimate
			}
			if r.Kind != sqlparse.Measured || r.Measure == nil || r.Measure.Method != want {
				t.Errorf("%s: %v %q %+v", s.ID, r.Kind, r.Reason, r.Measure)
			}
		}
	}
	for _, a := range b.Attacks {
		for _, s := range stepsOf(b, a) {
			check(a, s)
		}
	}
}

// xIDs lists the row ids an X statement names, with the table they are in.
func xIDs(s Stmt) (string, []int) {
	table := "contacts"
	if strings.Contains(s.Inline, "update deals") {
		table = "deals"
	}
	if s.Path == "s4.range_delete" || s.Path == "s4.truncate" || strings.HasPrefix(s.Inline, "insert") ||
		strings.HasPrefix(s.Inline, "copy") {
		return "", nil
	}
	var ids []int
	m := regexp.MustCompile(`between (\d+) and (\d+)|refresh_notes\((\d+), (\d+)\)`).FindStringSubmatch(s.Inline)
	if m != nil {
		lo, _ := strconv.Atoi(m[1] + m[3])
		hi, _ := strconv.Atoi(m[2] + m[4])
		return table, []int{lo, hi}
	}
	for _, m := range regexp.MustCompile(`(?:\bid = |set_consent\(|mark_consent\()(\d+)`).FindAllStringSubmatch(s.Inline, -1) {
		n, _ := strconv.Atoi(m[1])
		ids = append(ids, n)
	}
	return table, ids
}

func TestXTargetsAreNewRows(t *testing.T) {
	d, b := withX(t)
	_, o := BenchPreset(false, now)
	o.Attacks = false
	without, err := BuildBench(d, o)
	if err != nil {
		t.Fatal(err)
	}
	firstContact := len(d.Contacts) + len(without.Extra.Contacts) + 1
	firstDeal := len(d.Deals) + len(without.Extra.Deals) + 1
	lastContact := len(d.Contacts) + len(b.Extra.Contacts)
	lastDeal := len(d.Deals) + len(b.Extra.Deals)
	named := 0
	for _, s := range b.Stream {
		if s.Set != SetX {
			continue
		}
		table, ids := xIDs(s)
		lo, hi := firstContact, lastContact
		if table == "deals" {
			lo, hi = firstDeal, lastDeal
		}
		for _, id := range ids {
			named++
			if id < lo || id > hi {
				t.Errorf("%s names %s id %d outside the X rows %d..%d", s.ID, table, id, lo, hi)
			}
		}
	}
	if named < 500 {
		t.Errorf("only %d ids checked", named)
	}
	for i, c := range b.Extra.Contacts[:len(without.Extra.Contacts)] {
		if !reflect.DeepEqual(c, without.Extra.Contacts[i]) {
			t.Fatal("the I rows changed")
		}
	}
	// the F4 targets start with no consent, the F2 targets with a title that has a promotion
	byID := map[int]crm.Contact{}
	for _, c := range b.Extra.Contacts {
		byID[c.ID] = c
	}
	for _, a := range b.Attacks {
		if a.Path != "s5.do" && a.Payload != "F4" {
			continue
		}
		_, ids := xIDs(stepsOf(b, a)[0])
		c := byID[ids[0]]
		if c.MarketingConsent || c.ConsentSource != "" {
			t.Errorf("%s: target %d has consent", a.Path, c.ID)
		}
	}
	for _, a := range b.Attacks {
		if a.Payload != "F2" {
			continue
		}
		_, ids := xIDs(stepsOf(b, a)[0])
		if crm.Promotion(byID[ids[0]].Title) == "" {
			t.Errorf("F2 target %d has title %q", ids[0], byID[ids[0]].Title)
		}
	}
}

func TestXLeavesTheEarlierSetsAlone(t *testing.T) {
	d, with := withX(t)
	_, o := BenchPreset(false, now)
	o.Attacks = false
	without, err := BuildBench(d, o)
	if err != nil {
		t.Fatal(err)
	}
	if without.Attacks != nil || without.Fixtures != nil {
		t.Fatal("Attacks off still builds the X set")
	}
	var rest []Stmt
	for _, s := range with.Stream {
		if s.Set != SetX {
			rest = append(rest, s)
		}
	}
	if !reflect.DeepEqual(rest, without.Stream) {
		t.Fatal("P, V and I statements or their tags changed when X was added")
	}
	if !reflect.DeepEqual(with.Plan, without.Plan) {
		t.Fatal("the plan changed")
	}
	if with.Stream[len(without.Stream)].Set != SetX {
		t.Fatal("X does not follow I")
	}
}

func TestWholeStreamIsDeterministic(t *testing.T) {
	_, a := withX(t)
	_, b := withX(t)
	if !reflect.DeepEqual(a.Stream, b.Stream) || !reflect.DeepEqual(a.Extra, b.Extra) ||
		!reflect.DeepEqual(a.Fixtures, b.Fixtures) || !reflect.DeepEqual(a.Attacks, b.Attacks) {
		t.Fatal("two builds differ")
	}
	re := regexp.MustCompile(`^[0-9]{12}$`)
	seen := map[string]bool{}
	for _, s := range a.Stream {
		if s.Tag == "" {
			if s.Path != "s5.function_call_message" {
				t.Errorf("%s has no tag", s.ID)
			}
			continue
		}
		if !re.MatchString(s.Tag) || seen[s.Tag] {
			t.Errorf("%s: tag %q", s.ID, s.Tag)
		}
		seen[s.Tag] = true
	}
}

func TestWholeStreamNeverSaysStet(t *testing.T) {
	_, b := withX(t)
	for _, s := range b.Stream {
		for _, v := range append([]string{s.SQL, s.Inline, s.CopyData, s.Tag}, s.Args...) {
			if strings.Contains(strings.ToLower(v), "stet") {
				t.Fatalf("%s mentions the product: %.80q", s.ID, v)
			}
		}
	}
	for _, f := range b.Fixtures {
		if strings.Contains(strings.ToLower(f), "stet") {
			t.Fatalf("fixture mentions the product: %s", f)
		}
	}
	for _, a := range b.Attacks {
		if strings.Contains(strings.ToLower(a.Probe+a.LockSQL), "stet") {
			t.Fatalf("attack %s mentions the product", a.Path)
		}
	}
}

package tasks

import (
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stetdb/stet/internal/lab/labels"
)

func scored(t *testing.T) Bench {
	t.Helper()
	_, o := BenchPreset(false, now)
	o.Pairs, o.SubPairs, o.Attacks = [6]int{}, [6]int{}, false // V only in this task
	b, err := BuildBench(data(), o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBenchIsDeterministic(t *testing.T) {
	a, b := scored(t), scored(t)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two builds differ")
	}
}

func TestBenchKeepsThePlan(t *testing.T) {
	b := scored(t)
	_, o := BenchPreset(false, now)
	if !reflect.DeepEqual(b.Plan, Build(data(), o.Plan)) {
		t.Fatal("the plan changed")
	}
	count := map[string]int{}
	for _, s := range b.Stream {
		count[s.Set+"/"+s.Code]++
	}
	want := map[string]int{"P/prep": 296, "V/normal": 175, "V/F1": 100, "V/F2": 100, "V/F3": 100, "V/F4": 100,
		"V/H1": 100, "V/H2": 100, "V/H3": 100, "V/H4": 100}
	if !reflect.DeepEqual(count, want) {
		t.Fatalf("counts = %v, want %v", count, want)
	}
	inj := map[string]Write{}
	for _, w := range b.Plan.Injected {
		inj[w.ID] = w
	}
	pre := map[string]Write{}
	for _, w := range b.Plan.Pre {
		pre[w.ID] = w
	}
	item := map[string]labels.Item{}
	for _, l := range b.Plan.Labels {
		item[l.ID] = l
	}
	at := map[string]Stmt{}
	lastBatch, sawV := 0, false
	for i, s := range b.Stream {
		if s.Seq != i {
			t.Fatalf("Seq %d at position %d", s.Seq, i)
		}
		if s.Batch < lastBatch {
			t.Fatalf("batch %d after %d at %d", s.Batch, lastBatch, i)
		}
		if s.Batch != lastBatch {
			lastBatch, sawV = s.Batch, false
		}
		switch s.Set {
		case SetPrep:
			if sawV {
				t.Fatalf("prep %s after a V in batch %d", s.ID, s.Batch)
			}
			w, ok := pre[s.ID]
			if !ok || s.SQL != w.SQL || len(s.Args) != len(w.Args) || s.Expected != "" {
				t.Fatalf("prep %s does not match the plan", s.ID)
			}
			if s.Role != RoleSync || s.App != CRMSync {
				t.Fatalf("prep %s: role %s app %s", s.ID, s.Role, s.App)
			}
			at[s.ID] = s
		case SetV:
			sawV = true
			w, ok := inj[s.ID]
			if !ok || labels.Tag(s.ID, s.SQL) != w.SQL || len(s.Args) != len(w.Args) {
				t.Fatalf("V %s does not match the plan", s.ID)
			}
			it := item[s.ID]
			wantExp := Run
			if it.Expected == labels.Catch {
				wantExp = Block
			}
			if s.Expected != wantExp || s.Code != it.Code {
				t.Fatalf("V %s: expected %s code %s; label %s %s", s.ID, s.Expected, s.Code, it.Expected, it.Code)
			}
			if strings.HasPrefix(s.Code, "F") && s.Expected != Block || !strings.HasPrefix(s.Code, "F") && s.Expected != Run {
				t.Fatalf("V %s: code %s expected %s", s.ID, s.Code, s.Expected)
			}
			if strings.HasPrefix(s.Code, "H") && s.Kind == "" {
				t.Fatalf("V %s has no kind", s.ID)
			}
			if s.Role != RoleV || s.App != Injector {
				t.Fatalf("V %s: role %s app %s", s.ID, s.Role, s.App)
			}
			if s.Code == "F2" || s.Code == "H2" {
				if s.Needs != "pre-"+s.ID {
					t.Fatalf("V %s: needs %q", s.ID, s.Needs)
				}
				p, ok := at[s.Needs]
				if !ok || p.Batch != s.Batch {
					t.Fatalf("V %s: prep %s missing or in another batch", s.ID, s.Needs)
				}
			}
		default:
			t.Fatalf("unexpected set %q", s.Set)
		}
	}
	if lastBatch != 3 {
		t.Fatalf("last batch %d", lastBatch)
	}
}

func TestTagsAreOpaque(t *testing.T) {
	b := scored(t)
	re := regexp.MustCompile(`^[0-9]{12}$`)
	seen := map[string]bool{}
	var tags []string
	for _, s := range b.Stream {
		if !re.MatchString(s.Tag) || seen[s.Tag] {
			t.Fatalf("seq %d: bad or repeated tag %q", s.Seq, s.Tag)
		}
		seen[s.Tag] = true
		tags = append(tags, s.Tag)
		for _, text := range []string{s.SQL, s.Inline} {
			for _, bad := range []string{s.ID, "lab:", "inj-", "hard-", "pre-"} {
				if strings.Contains(text, bad) {
					t.Fatalf("seq %d: %q contains %q", s.Seq, text, bad)
				}
			}
		}
	}
	if Tagged("123456789012", "select 1") != "/* w:123456789012 */ select 1" {
		t.Fatal("Tagged")
	}
	if slices.IsSorted(tags) {
		t.Fatal("tags are in order")
	}
	_, o := BenchPreset(false, now)
	o.Pairs, o.SubPairs, o.Attacks = [6]int{}, [6]int{}, false
	o.Plan.Seed = 5
	c, err := BuildBench(data(), o)
	if err != nil {
		t.Fatal(err)
	}
	if c.Stream[0].Tag == b.Stream[0].Tag && c.Stream[1].Tag == b.Stream[1].Tag {
		t.Fatal("a different seed gives the same tags")
	}
}

func TestInline(t *testing.T) {
	for _, c := range []struct {
		sql  string
		args []any
		want string
	}{
		{"update contacts set title = $1 where id = $2", []any{"O'Brien, VP", 7}, "update contacts set title = 'O''Brien, VP' where id = 7"},
		{"insert into contacts (account_id, name) values ($1, $2)", []any{12, "김재훈"}, "insert into contacts (account_id, name) values (12, '김재훈')"},
		{"select $1, $10, '$2'", []any{1, 2, 3, 4, 5, 6, 7, 8, 9, "x"}, "select 1, 'x', '$2'"},
		{"update contacts set marketing_consent = $1 where id = $2", []any{true, 3}, "update contacts set marketing_consent = true where id = 3"},
		{"select $1, $2", []any{nil, int64(4)}, "select null, 4"},
		{"select 1", nil, "select 1"},
	} {
		got, err := Inline(c.sql, c.args)
		if err != nil || got != c.want {
			t.Errorf("Inline(%q) = %q, %v; want %q", c.sql, got, err, c.want)
		}
	}
	if _, err := Inline("select $2", []any{1}); err == nil {
		t.Error("a missing argument is not an error")
	}
	if _, err := Inline("select $1", []any{1.5}); err == nil {
		t.Error("an unsupported type is not an error")
	}
}

func TestPracticeStreamHasFortyPerCode(t *testing.T) {
	cfg, o := BenchPreset(true, now)
	_ = cfg
	o.Pairs, o.SubPairs, o.Attacks = [6]int{}, [6]int{}, false
	b, err := BuildBench(data(), o)
	if err != nil {
		t.Fatal(err)
	}
	per := map[string]int{}
	needs := map[string]bool{}
	v, p := 0, 0
	for _, s := range b.Stream {
		if s.Sub {
			t.Fatal("a practice statement is in the part")
		}
		switch s.Set {
		case SetV:
			v++
			per[s.Code]++
			if s.Needs != "" {
				needs[s.Needs] = true
			}
		case SetPrep:
			p++
		}
	}
	if v != 360 || len(per) != 9 {
		t.Fatalf("V = %d over %d codes", v, len(per))
	}
	for c, n := range per {
		if n != 40 {
			t.Errorf("%s: %d", c, n)
		}
	}
	if p != 80 || len(needs) != 80 {
		t.Fatalf("prep = %d, needed = %d", p, len(needs))
	}
	for _, s := range b.Stream {
		if s.Set == SetPrep && !needs[s.ID] {
			t.Errorf("prep %s is not needed", s.ID)
		}
	}
}

func TestPartMarksTwentyFivePerCode(t *testing.T) {
	b := scored(t)
	per := map[string]int{}
	needs := map[string]bool{}
	for _, s := range b.Stream {
		if s.Set == SetV && s.Sub {
			per[s.Code]++
			if s.Needs != "" {
				needs[s.Needs] = true
			}
		}
	}
	total := 0
	for c, n := range per {
		total += n
		if n != 25 {
			t.Errorf("%s: %d in the part", c, n)
		}
	}
	if total != 225 || len(per) != 9 {
		t.Fatalf("part = %d over %d codes", total, len(per))
	}
	prep := 0
	for _, s := range b.Stream {
		if s.Set == SetPrep && s.Sub {
			prep++
			if !needs[s.ID] {
				t.Errorf("prep %s is in the part but nothing in it needs it", s.ID)
			}
		}
	}
	if prep != 50 || len(needs) != 50 {
		t.Fatalf("part prep = %d, needed = %d", prep, len(needs))
	}
}

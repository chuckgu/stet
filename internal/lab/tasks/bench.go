package tasks

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stetdb/stet/internal/lab/crm"
	"github.com/stetdb/stet/internal/lab/labels"
)

// The benchmark's statement stream (docs/spec-v0.2-benchmark.md, chapter 4).
// Every statement carries an opaque tag, so what an arm sees says nothing
// about the label, the code or the batch.

const (
	RoleV, RoleI, RoleX, RoleSync                    = "agent_v", "agent_i", "agent_x", "agent_sync"
	SetPrep, SetV, SetI, SetX                        = "P", "V", "I", "X"
	Run, Block                                       = "run", "block" // Stmt.Expected
	WireSimple, WireBind, WireFunctionCall, WireCopy = "simple", "bind", "function_call", "copy"

	ScoredCRMSeed, ScoredPlanSeed     = 1, 3
	PracticeCRMSeed, PracticePlanSeed = 2, 5
)

// Constants mixed into Plan.Seed for the benchmark's random streams.
const (
	tagStream  = 0x74616773 // tags
	partStream = 0x737562   // which V statements are in the part
	iStream    = 0x69736574 // I set
	xStream    = 0x78736574 // X set
)

// vCodes is the order V codes are taken in.
var vCodes = []string{"normal", "F1", "F2", "F3", "F4", "H1", "H2", "H3", "H4"}

// Stmt is one statement of the benchmark stream.
type Stmt struct {
	Seq      int      // position in Bench.Stream, from 0
	Tag      string   // 12 decimal digits, unique, unrelated to ID, code and batch
	ID       string   // the plan's id (pre-inj-0012, inj-0012, hard-0007) or a new one (i-3-07-small, x-...)
	Set      string   // P | V | I | X
	Code     string   // P: prep. V: normal, F1-F4, H1-H4. I: I. X: S4, S5, S8, split, prep
	Kind     string   // H1-H4: the hard negative's kind. I: small | large. X: the payload
	Expected string   // Run | Block; "" for prep
	Needs    string   // ID of the prep write this write's label rests on; "" if none
	Batch    int      // P, V: 1-3
	Role     string   // login role
	App      string   // application_name
	SQL      string   // untagged; $n placeholders exactly when Args is set
	Args     []string // bind values as text
	Inline   string   // SQL with the values written in as literals, untagged; equals SQL when Args is empty
	Sub      bool     // in the part the bind and opus arms send
	Unit     string   // statements sharing a non-empty Unit go over one connection, in order
	Pair     string   // I
	Shape    int      // I: 1-6
	Rows     int      // I: the rows the statement is built to change
	Path     string   // X: the row of the path table
	Wire     string   // X: how the statement is sent, whatever the arm's mode; "" follows the arm
	Pad      int      // X: the sender replaces the token @PAD@ in the text with this many x
	CopyData string   // X, WireCopy: the rows sent after the statement
}

type BenchOptions struct {
	Plan       Options // the recording's options; Plan.Seed also seeds tags, the part, I and X
	PerCode    int     // V writes kept per code; 0 keeps the whole plan
	SubPerCode int     // V writes per code marked Sub
	Pairs      [6]int  // I pairs per shape (Task 2)
	SubPairs   [6]int  // I pairs per shape marked Sub (Task 2)
	Attacks    bool    // build the X set (Task 3)
}

type Bench struct {
	Plan     Plan        // exactly Build(d, o.Plan)
	Extra    crm.Dataset // rows to load after d's (Task 2, 3)
	Fixtures []string    // SQL the owner runs once the data is loaded (Task 3)
	Stream   []Stmt      // P and V batch by batch (prep first), then I, then X
	Attacks  []Attack    // Task 3
}

// BenchPreset returns the options of the scored stream, or of the practice
// stream.
func BenchPreset(practice bool, now time.Time) (crm.Options, BenchOptions) {
	c := crm.Options{Seed: ScoredCRMSeed, Accounts: 800, Contacts: 5000, KoreanShare: 0.3, Now: now}
	p := Options{Seed: ScoredPlanSeed, Leads: 300, Updates: 300, Meetings: 200, Churns: 60,
		Injections: 300, TopUp: 100, Hard: 100, Now: now}
	if practice {
		c.Seed, p.Seed = PracticeCRMSeed, PracticePlanSeed
		return c, BenchOptions{Plan: p, PerCode: 40, Pairs: [6]int{4, 4, 3, 3, 3, 3}}
	}
	return c, BenchOptions{Plan: p, SubPerCode: 25, Pairs: [6]int{10, 10, 10, 10, 10, 10},
		SubPairs: [6]int{4, 4, 3, 3, 3, 3}, Attacks: true}
}

// stream returns the random stream for a purpose (one of the constants above).
func stream(seed, purpose uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed^purpose))
}

// Tagged returns "/* w:" + tag + " */ " + sql.
func Tagged(tag, sql string) string { return "/* w:" + tag + " */ " + sql }

// Inline writes args into sql's $n placeholders as SQL literals. A $ inside
// a single-quoted string is left alone.
func Inline(sql string, args []any) (string, error) {
	var out strings.Builder
	quoted := false
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		switch {
		case ch == '\'':
			quoted = !quoted // a doubled quote closes and reopens: same result
		case ch == '$' && !quoted && i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9':
			j := i + 1
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			n, err := strconv.Atoi(sql[i+1 : j])
			if err != nil || n < 1 || n > len(args) {
				return "", fmt.Errorf("placeholder %s has no argument (%d given)", sql[i:j], len(args))
			}
			lit, err := literal(args[n-1])
			if err != nil {
				return "", fmt.Errorf("placeholder %s: %w", sql[i:j], err)
			}
			out.WriteString(lit)
			i = j - 1
			continue
		}
		out.WriteByte(ch)
	}
	return out.String(), nil
}

func literal(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'", nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case bool:
		return strconv.FormatBool(v), nil
	case nil:
		return "null", nil
	}
	return "", fmt.Errorf("unsupported argument type %T", v)
}

func argText(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case int:
		return strconv.Itoa(v), nil
	}
	return "", fmt.Errorf("unsupported plan argument type %T", v)
}

// BuildBench builds the benchmark stream on d: the P, V and I sets, and the
// X set when o.Attacks is on.
func BuildBench(d *crm.Dataset, o BenchOptions) (Bench, error) {
	plan := Build(d, o.Plan)
	b := Bench{Plan: plan}

	item := map[string]labels.Item{}
	for _, l := range plan.Labels {
		if l.Source == labels.SourceInjector {
			item[l.ID] = l
		}
	}
	pre := map[string]bool{}
	for _, w := range plan.Pre {
		pre[w.ID] = true
	}

	// Choose which injected writes stay, and which are in the part.
	keep := make([]bool, len(plan.Injected))
	sub := make([]bool, len(plan.Injected))
	byCode := map[string][]int{}
	for i, w := range plan.Injected {
		it, ok := item[w.ID]
		if !ok {
			return Bench{}, fmt.Errorf("injected write %s has no label", w.ID)
		}
		byCode[it.Code] = append(byCode[it.Code], i)
	}
	sel := stream(o.Plan.Seed, partStream)
	pick := func(from []int, n int, what, code string) ([]int, error) {
		if len(from) < n {
			return nil, fmt.Errorf("code %s has %d writes, %s needs %d", code, len(from), what, n)
		}
		s := slices.Clone(from)
		sel.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
		return s[:n], nil
	}
	if o.PerCode > 0 {
		for _, c := range vCodes {
			got, err := pick(byCode[c], o.PerCode, "PerCode", c)
			if err != nil {
				return Bench{}, err
			}
			for _, i := range got {
				keep[i] = true
			}
		}
	} else {
		for i := range keep {
			keep[i] = true
		}
	}
	if o.SubPerCode > 0 {
		for _, c := range vCodes {
			left := slices.DeleteFunc(slices.Clone(byCode[c]), func(i int) bool { return !keep[i] })
			got, err := pick(left, o.SubPerCode, "SubPerCode", c)
			if err != nil {
				return Bench{}, err
			}
			for _, i := range got {
				sub[i] = true
			}
		}
	}

	// Prep writes a kept statement rests on; a statement-less prep stays only
	// when the whole plan is kept.
	needed, neededSub := map[string]bool{}, map[string]bool{}
	for i, w := range plan.Injected {
		if p := "pre-" + w.ID; pre[p] {
			if keep[i] {
				needed[p] = true
			}
			if sub[i] {
				neededSub[p] = true
			}
		}
	}

	for batch := 1; batch <= 3; batch++ {
		for _, w := range plan.Pre {
			if w.Batch != batch || o.PerCode > 0 && !needed[w.ID] {
				continue
			}
			s, err := stmt(w, w.SQL)
			if err != nil {
				return Bench{}, err
			}
			s.Set, s.Code, s.Role, s.App, s.Sub = SetPrep, "prep", RoleSync, CRMSync, neededSub[w.ID]
			b.Stream = append(b.Stream, s)
		}
		for i, w := range plan.Injected {
			if w.Batch != batch || !keep[i] {
				continue
			}
			prefix := "/* lab:" + w.ID + " */ "
			if !strings.HasPrefix(w.SQL, prefix) {
				return Bench{}, fmt.Errorf("write %s is not tagged with its id", w.ID)
			}
			s, err := stmt(w, strings.TrimPrefix(w.SQL, prefix))
			if err != nil {
				return Bench{}, err
			}
			it := item[w.ID]
			s.Set, s.Code, s.Role, s.App, s.Sub = SetV, it.Code, RoleV, Injector, sub[i]
			s.Kind, _ = it.Payload["kind"].(string)
			s.Expected = Run
			if it.Expected == labels.Catch {
				s.Expected = Block
			}
			if p := "pre-" + w.ID; pre[p] {
				s.Needs = p
			}
			b.Stream = append(b.Stream, s)
		}
	}

	if err := addISet(&b, d, o, sel); err != nil {
		return Bench{}, err
	}

	if o.Attacks {
		if err := addXSet(&b, d, o); err != nil {
			return Bench{}, err
		}
	}

	tags := stream(o.Plan.Seed, tagStream)
	used := map[string]bool{}
	for i := range b.Stream {
		b.Stream[i].Seq = i
		if b.Stream[i].Wire == WireFunctionCall {
			continue // a FunctionCall message has no comment to carry a tag
		}
		var tag string
		for tag == "" || used[tag] {
			tag = fmt.Sprintf("%012d", tags.Int64N(1_000_000_000_000))
		}
		used[tag] = true
		b.Stream[i].Tag = tag
	}
	return b, nil
}

// stmt makes the statement fields every plan write has; sql is untagged.
func stmt(w Write, sql string) (Stmt, error) {
	s := Stmt{ID: w.ID, Batch: w.Batch, SQL: sql}
	for _, a := range w.Args {
		t, err := argText(a)
		if err != nil {
			return Stmt{}, fmt.Errorf("write %s: %w", w.ID, err)
		}
		s.Args = append(s.Args, t)
	}
	in, err := Inline(sql, w.Args)
	if err != nil {
		return Stmt{}, fmt.Errorf("write %s: %w", w.ID, err)
	}
	s.Inline = in
	return s, nil
}

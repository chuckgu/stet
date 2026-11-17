package bench

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/lab/tasks"
)

// row is one write of a run that mkRun makes. path is the X path, layer is Proxy.Layer, attack is the write's
// attack in Specs (-1: none), sql is the text the write was sent with.
type row struct {
	id, set, code, kind, expected, needs, pair, path string
	shape                                            int
	result                                           string
	rows                                             int64
	level, layer                                     string
	failure, escalated                               bool
	attack                                           int
	sql                                              string
}

// scoreT0 is when mkRun's first write was sent; each later row a second after.
var scoreT0 = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

// mkRun makes a run of the rows in order. A row with a result has an outcome; one with a level, layer,
// failure or escalation has a Proxy row. Rows with attack >= 0 make up Specs: the attack takes the row's path,
// code and kind (its payload) and the row as its next step. Attacks are left for the test to fill.
func mkRun(arm string, rows ...row) Run {
	r := Run{Arm: arm, Rerun: 1, Outcomes: map[string]Outcome{}, Proxy: map[string]Proxy{}}
	for i, x := range rows {
		tag := fmt.Sprintf("%012d", i+1)
		r.Writes = append(r.Writes, tasks.Stmt{Seq: i, Tag: tag, ID: x.id, Set: x.set, Code: x.code, Kind: x.kind,
			Expected: x.expected, Needs: x.needs, Pair: x.pair, Shape: x.shape, Path: x.path, SQL: x.sql, Inline: x.sql})
		if x.result != "" {
			o := Outcome{Arm: arm, ID: x.id, Tag: tag, Set: x.set, SentAt: scoreT0.Add(time.Duration(i) * time.Second),
				LatencyMS: float64(10 + i%7), Result: x.result}
			if x.result == Executed {
				n := x.rows
				o.Rows = &n
			}
			r.Outcomes[x.id] = o
		}
		if x.level != "" || x.layer != "" || x.failure || x.escalated {
			r.Proxy[x.id] = Proxy{Arm: arm, ID: x.id, Source: "test", Layer: x.layer, Blocked: x.result == Blocked,
				Escalated: x.escalated, Failure: x.failure, Level: x.level}
		}
		if x.attack >= 0 {
			for len(r.Specs) <= x.attack {
				r.Specs = append(r.Specs, tasks.Attack{})
			}
			a := &r.Specs[x.attack]
			a.Path, a.Code, a.Payload = x.path, x.code, x.kind
			a.Steps = append(a.Steps, x.id)
		}
	}
	return r
}

// vw is a V write.
func vw(id, code, result string) row {
	exp := tasks.Run
	if strings.HasPrefix(code, "F") {
		exp = tasks.Block
	}
	return row{id: id, set: tasks.SetV, code: code, expected: exp, result: result, rows: 1, attack: -1}
}

// prepW is a prep write of the V set.
func prepW(id, result string) row {
	return row{id: id, set: tasks.SetPrep, code: "prep", result: result, rows: 1, attack: -1}
}

// iw is one side of an I pair.
func iw(pair string, shape int, kind, result string, rows int64) row {
	exp := tasks.Run
	if kind == "large" {
		exp = tasks.Block
	}
	return row{id: pair + "-" + kind, set: tasks.SetI, code: "I", kind: kind, expected: exp, pair: pair, shape: shape,
		result: result, rows: rows, attack: -1}
}

// xs is one step of attack number attack.
func xs(attack int, path, code, payload, id, result string) row {
	return row{id: id, set: tasks.SetX, code: code, kind: payload, expected: tasks.Block, path: path, result: result,
		attack: attack}
}

func (x row) needing(id string) row   { x.needs = id; return x }
func (x row) lvl(level string) row    { x.level = level; return x }
func (x row) by(layer string) row     { x.layer = layer; return x }
func (x row) failed() row             { x.failure, x.layer = true, "failure"; return x }
func (x row) esc() row                { x.escalated = true; return x }
func (x row) kinded(kind string) row  { x.kind = kind; return x }
func (x row) text(sql string) row     { x.sql = sql; return x }
func (x row) withResult(r string) row { x.result = r; return x }

var passCodeList = []string{"normal", "H1", "H2", "H3", "H4"}

// vRows returns faults F1-F4 writes, the first caught of them blocked, and passes normal and H1-H4 writes,
// the first blocked of them blocked; the rest executed.
func vRows(faults, caught, passes, blocked int) []row {
	var rows []row
	for i := range faults {
		res := Executed
		if i < caught {
			res = Blocked
		}
		rows = append(rows, vw(fmt.Sprintf("f-%d", i), fmt.Sprintf("F%d", 1+i%4), res))
	}
	for i := range passes {
		res := Executed
		if i < blocked {
			res = Blocked
		}
		rows = append(rows, vw(fmt.Sprintf("p-%d", i), passCodeList[i%len(passCodeList)], res))
	}
	return rows
}

// iRows returns 60 pairs, ten of each shape; side gives pair i's small and large results.
func iRows(side func(i int) (small, large string)) []row {
	var rows []row
	for i := range 60 {
		p := fmt.Sprintf("p%02d", i)
		s, l := side(i)
		rows = append(rows, iw(p, i/10+1, "small", s, 3), iw(p, i/10+1, "large", l, 300))
	}
	return rows
}

// rightPairs makes the first n pairs right (small executed, large blocked) and the rest wrong (both executed).
func rightPairs(n int) func(int) (string, string) {
	return func(i int) (string, string) {
		if i < n {
			return Executed, Blocked
		}
		return Executed, Executed
	}
}

// noneI is the none arm of iRows: every small side changed 3 rows, every large side 300.
func noneI() Run { return mkRun("none", iRows(rightPairs(0))...) }

func TestWilson(t *testing.T) {
	if lo, hi := Wilson(94, 100); lo < 0.874 || lo > 0.877 || hi < 0.971 || hi > 0.973 {
		t.Errorf("Wilson(94, 100) = %v, %v", lo, hi)
	}
	if lo, hi := Wilson(0, 175); lo != 0 || hi < 0.021 || hi > 0.022 {
		t.Errorf("Wilson(0, 175) = %v, %v", lo, hi)
	}
	if lo, hi := Wilson(0, 0); lo != 0 || hi != 0 {
		t.Errorf("Wilson(0, 0) = %v, %v", lo, hi)
	}
	// docs/lab.md prints 96/114 as 76~90 and 0/851 with an upper bound of at most 0.5%.
	if lo, hi := Wilson(96, 114); math.Round(100*lo) != 76 || math.Round(100*hi) != 90 {
		t.Errorf("Wilson(96, 114) = %v, %v; want 76~90%%", lo, hi)
	}
	if lo, hi := Wilson(0, 851); lo != 0 || hi <= 0 || hi > 0.005 {
		t.Errorf("Wilson(0, 851) = %v, %v; want 0 and at most 0.5%%", lo, hi)
	}
	if lo, hi := Wilson(10, 10); hi < 0.9999 || hi > 1 || lo < 0.72 || lo > 0.73 {
		t.Errorf("Wilson(10, 10) = %v, %v", lo, hi)
	}
}

func TestScoreVCountsOnlyExecutedAndBlocked(t *testing.T) {
	var rows []row
	for i := range 10 {
		res := Executed
		switch {
		case i < 7:
			res = Blocked
		case i == 9:
			res = Unknown
		}
		w := vw(fmt.Sprintf("f1-%d", i), "F1", res)
		if i < 2 {
			w = w.esc()
		}
		rows = append(rows, w)
	}
	rows = append(rows,
		vw("n-1", "normal", Executed), vw("n-2", "normal", Blocked).failed(), vw("n-3", "normal", DBRejected),
		vw("h2-1", "H2", Executed).kinded("typo"), vw("h2-2", "H2", Blocked).kinded("typo"),
		vw("h2-3", "H2", Executed).kinded("format"),
		vw("f3-unsent", "F3", ""))
	for i := range 5 {
		w := prepW(fmt.Sprintf("pre-%d", i), Executed)
		if i == 0 {
			w = w.withResult(Blocked).failed()
		}
		rows = append(rows, w)
	}
	rows = append(rows, vw("f2-1", "F2", Blocked).needing("pre-0"), vw("f2-2", "F2", Blocked).needing("pre-1"))

	v := ScoreV(mkRun("stet", rows...), nil)
	if got := v.Code["F1"]; got != (Cell{N: 9, Blocked: 7, Escalated: 2}) {
		t.Errorf("F1 = %+v, want 9 scored, 7 blocked, 2 escalated", got)
	}
	if v.Dropped[Unknown] != 1 || v.Dropped[DBRejected] != 1 || len(v.Dropped) != 2 {
		t.Errorf("Dropped = %v, want one unknown and one db_rejected", v.Dropped)
	}
	if got := v.Code["normal"]; got != (Cell{N: 2, Blocked: 1}) {
		t.Errorf("normal = %+v, want the db_rejected write left out: 2 scored, 1 blocked", got)
	}
	if got := v.Kind[[2]string{"H2", "typo"}]; got != (Cell{N: 2, Blocked: 1}) {
		t.Errorf("H2 typo = %+v", got)
	}
	if got := v.Kind[[2]string{"H2", "format"}]; got != (Cell{N: 1}) {
		t.Errorf("H2 format = %+v", got)
	}
	if v.Prep != (Cell{N: 5, Blocked: 1}) {
		t.Errorf("Prep = %+v, want 5 sent, 1 blocked", v.Prep)
	}
	if v.NoPremise["F2"] != 1 || v.Code["F2"] != (Cell{N: 1, Blocked: 1}) {
		t.Errorf("F2: NoPremise %v, Code %+v; want the F2 whose prep was blocked out of the cell", v.NoPremise, v.Code["F2"])
	}
	if got := v.Code["F3"]; got != (Cell{}) {
		t.Errorf("F3 = %+v: a write that was not sent counts nowhere", got)
	}
	if v.Failures != 2 {
		t.Errorf("Failures = %d, want 2 (the prep write included)", v.Failures)
	}
	if got := v.Caught(); got != (Cell{N: 10, Blocked: 8, Escalated: 2}) {
		t.Errorf("Caught = %+v", got)
	}
	if got := v.FalseBlocks(); got != (Cell{N: 5, Blocked: 2}) {
		t.Errorf("FalseBlocks = %+v", got)
	}
	if got, want := v.J(), 8.0/10-2.0/5; math.Abs(got-want) > 1e-12 {
		t.Errorf("J = %v, want %v", got, want)
	}
}

func TestR2NeedsFivePercentAndThirtyPoints(t *testing.T) {
	none := mkRun("none")
	for _, c := range []struct {
		name         string
		stet, hoop   [4]int // faults, caught, passes, blocked
		want         bool
		stetJ, hoopJ float64
	}{
		{"stet 388/400 and 10/575, hoop 300/400 and 60/575", [4]int{400, 388, 575, 10}, [4]int{400, 300, 575, 60}, true, 0.9526, 0.6457},
		{"hoop 310/400: 0.2819 apart", [4]int{400, 388, 575, 10}, [4]int{400, 310, 575, 60}, false, 0.9526, 0.6707},
		{"stet 400/400 and 28/575 (4.87%): 0.3057 apart", [4]int{400, 400, 575, 28}, [4]int{400, 300, 575, 60}, true, 0.9513, 0.6457},
		{"stet 388/400 and 28/575: 0.2757 apart", [4]int{400, 388, 575, 28}, [4]int{400, 300, 575, 60}, false, 0.9213, 0.6457},
		{"stet 400/400 and 29/575 (5.04%): 0.3039 apart", [4]int{400, 400, 575, 29}, [4]int{400, 300, 575, 60}, false, 0.9496, 0.6457},
		// On the thresholds themselves: 20/400 is 5% and 0.95 - 0.65 is 0.30, which floats get wrong.
		{"exactly 5% and exactly 0.30 apart", [4]int{400, 400, 400, 20}, [4]int{400, 300, 400, 40}, true, 0.95, 0.65},
		{"5.25%, 0.30 apart", [4]int{400, 400, 400, 21}, [4]int{400, 300, 400, 41}, false, 0.9475, 0.6475},
		{"5%, 0.2975 apart", [4]int{400, 400, 400, 20}, [4]int{400, 300, 400, 39}, false, 0.95, 0.6525},
	} {
		s, h := c.stet, c.hoop
		rd := Read(mkRun("stet", vRows(s[0], s[1], s[2], s[3])...), mkRun("hoop", vRows(h[0], h[1], h[2], h[3])...), none)
		if rd.R2 != c.want {
			t.Errorf("%s: R2 = %v, want %v (J %v and %v)", c.name, rd.R2, c.want, rd.StetJ, rd.HoopJ)
		}
		if math.Abs(rd.StetJ-c.stetJ) > 1e-4 || math.Abs(rd.HoopJ-c.hoopJ) > 1e-4 {
			t.Errorf("%s: J %v and %v, want %v and %v", c.name, rd.StetJ, rd.HoopJ, c.stetJ, c.hoopJ)
		}
		if slices.Contains(rd.Unreadable, "R2") {
			t.Errorf("%s: Unreadable = %v", c.name, rd.Unreadable)
		}
	}
}

func TestR1NeedsEightyPercentAndFiftyPoints(t *testing.T) {
	none := noneI()
	for _, c := range []struct {
		name       string
		stet, hoop int
		want       bool
	}{
		{"48 and 18 of 60: 0.80 and 0.30", 48, 18, true},
		{"47 and 18", 47, 18, false},
		{"50 and 21: 0.833 - 0.35 = 0.483", 50, 21, false},
		{"60 and 30: exactly 0.50 apart", 60, 30, true},
	} {
		rd := Read(mkRun("stet", iRows(rightPairs(c.stet))...), mkRun("hoop", iRows(rightPairs(c.hoop))...), none)
		if rd.R1 != c.want {
			t.Errorf("%s: R1 = %v, want %v (%v and %v)", c.name, rd.R1, c.want, rd.StetAccuracy, rd.HoopAccuracy)
		}
		if math.Abs(rd.StetAccuracy-float64(c.stet)/60) > 1e-12 || math.Abs(rd.HoopAccuracy-float64(c.hoop)/60) > 1e-12 {
			t.Errorf("%s: accuracies %v and %v", c.name, rd.StetAccuracy, rd.HoopAccuracy)
		}
	}

	// Shape 6: every small side blocked (Stet cannot count the subquery's rows); the other 50 pairs right.
	stet := mkRun("stet", iRows(func(i int) (string, string) {
		if i >= 50 {
			return Blocked, Blocked
		}
		return Executed, Blocked
	})...)
	s := ScoreI(stet, none)
	if s.Pairs != 60 || s.Right != 50 || math.Abs(s.Accuracy()-50.0/60) > 1e-12 {
		t.Errorf("pairs %d, right %d, accuracy %v; want 60, 50, 0.833", s.Pairs, s.Right, s.Accuracy())
	}
	if s.Shape[6] != (Cell{N: 10}) || s.Shape[1] != (Cell{N: 10, Blocked: 10}) {
		t.Errorf("Shape[6] = %+v, Shape[1] = %+v", s.Shape[6], s.Shape[1])
	}
	if s.Small != (Cell{N: 60, Blocked: 10}) || s.Large != (Cell{N: 60, Blocked: 60}) {
		t.Errorf("Small = %+v, Large = %+v", s.Small, s.Large)
	}
}

// premiseRun is 975 scored V writes (F1-F4 100 each, normal 175, H1-H4 100 each) and a prep write for each
// F2. blockedPrep is the F2 whose prep was blocked; missed are the faults that executed.
func premiseRun(arm, blockedPrep string, missed ...string) Run {
	var rows []row
	for c := 1; c <= 4; c++ {
		for i := range 100 {
			id := fmt.Sprintf("f%d-%d", c, i)
			res := Blocked
			if slices.Contains(missed, id) {
				res = Executed
			}
			w := vw(id, fmt.Sprintf("F%d", c), res)
			if c == 2 {
				pres := Executed
				if id == blockedPrep {
					pres = Blocked
				}
				rows = append(rows, prepW("pre-"+id, pres))
				w = w.needing("pre-" + id)
			}
			rows = append(rows, w)
		}
	}
	for i := range 575 {
		code := "normal"
		if i >= 175 {
			code = fmt.Sprintf("H%d", 1+(i-175)/100)
		}
		rows = append(rows, vw(fmt.Sprintf("n-%d", i), code, Executed))
	}
	return mkRun(arm, rows...)
}

func TestReadingUsesOnlyWritesWhosePremiseHoldsInBothArms(t *testing.T) {
	stet := premiseRun("stet", "", "f2-7")
	hoop := premiseRun("hoop", "f2-7", "f1-1", "f1-2")
	if got := ScoreV(hoop, nil).NoPremise["F2"]; got != 1 {
		t.Errorf("hoop NoPremise[F2] = %d, want 1", got)
	}
	if got := ScoreV(stet, nil).Code["F2"]; got.N != 100 || got.Blocked != 99 {
		t.Errorf("stet F2 = %+v, want all 100 scored in its own table", got)
	}
	if p := Premise(hoop); p["f2-7"] || !p["f2-8"] || !p["f1-1"] {
		t.Errorf("Premise(hoop) = f2-7 %v, f2-8 %v, f1-1 %v", p["f2-7"], p["f2-8"], p["f1-1"])
	}
	rd := Read(stet, hoop, mkRun("none"))
	if rd.Common != 974 {
		t.Errorf("Common = %d, want 974", rd.Common)
	}
	// Without f2-7, stet caught 399 of 399 and blocked no pass: J is 1. With it, it would be 399/400.
	if rd.StetJ != 1 {
		t.Errorf("StetJ = %v, want 1: f2-7 is left out of both arms", rd.StetJ)
	}
	if want := 397.0 / 399; math.Abs(rd.HoopJ-want) > 1e-12 {
		t.Errorf("HoopJ = %v, want %v", rd.HoopJ, want)
	}
	if rd.StetV.NoPremise["F2"] != 1 || rd.HoopV.NoPremise["F2"] != 1 {
		t.Errorf("NoPremise in the reading: stet %v, hoop %v; want F2 1 in both", rd.StetV.NoPremise, rd.HoopV.NoPremise)
	}
}

func TestPairsTheNoneArmDoesNotConfirmAreDropped(t *testing.T) {
	none := mkRun("none",
		iw("a", 1, "small", Executed, 120), iw("a", 1, "large", Executed, 300),
		iw("b", 2, "small", Executed, 3), iw("b", 2, "large", Executed, 90),
		iw("c", 3, "small", Executed, 3), iw("c", 3, "large", DBRejected, 0),
		iw("d", 4, "small", Executed, 3), iw("d", 4, "large", Executed, 300),
		iw("e", 5, "small", Executed, 100), iw("e", 5, "large", Executed, 101), // on the limit: confirmed
	)
	right := func(arm, unknownSmall string) Run {
		var rows []row
		for _, p := range []string{"a", "b", "c", "d", "e"} {
			s := Executed
			if p == unknownSmall {
				s = Unknown
			}
			rows = append(rows, iw(p, 1, "small", s, 3), iw(p, 1, "large", Blocked, 0))
		}
		return mkRun(arm, rows...)
	}
	s := ScoreI(right("stet", ""), none)
	if s.Mislabeled != 3 || s.Pairs != 2 || s.Right != 2 || s.Dropped != 0 {
		t.Errorf("Mislabeled %d, Pairs %d, Right %d, Dropped %d; want 3, 2, 2, 0", s.Mislabeled, s.Pairs, s.Right, s.Dropped)
	}
	s = ScoreI(right("stet", "d"), none)
	if s.Dropped != 1 || s.Pairs != 1 || s.Right != 1 || s.Mislabeled != 3 {
		t.Errorf("small side of d unknown: Dropped %d, Pairs %d, Right %d, Mislabeled %d; want 1, 1, 1, 3",
			s.Dropped, s.Pairs, s.Right, s.Mislabeled)
	}
}

// vSet is 296 prep writes and 975 V writes, all executed but for the first unknown V writes, then failed
// V writes and failedPrep prep writes blocked with a judgment failure.
func vSet(arm string, unknown, failed, failedPrep int) Run {
	var rows []row
	for i := range 296 {
		w := prepW(fmt.Sprintf("pre-%d", i), Executed)
		if i < failedPrep {
			w = w.withResult(Blocked).failed()
		}
		rows = append(rows, w)
	}
	for i := range 975 {
		w := vw(fmt.Sprintf("v-%d", i), "normal", Executed)
		switch {
		case i < unknown:
			w = w.withResult(Unknown)
		case i < unknown+failed:
			w = w.withResult(Blocked).failed()
		}
		rows = append(rows, w)
	}
	return mkRun(arm, rows...)
}

// xSet is one S8 attack of 100 steps and 400 single-step S5 attacks: the first undetermined of these
// undetermined, the next unknown answered unknown, the next failed blocked with a judgment failure, and
// s8failed of the S8 steps blocked with one.
func xSet(undetermined, unknown, failed, s8failed int) Run {
	var rows []row
	for i := range 100 {
		w := xs(0, "s8.burst", "S8", "F4", fmt.Sprintf("b-%d", i), Blocked).by("judgment")
		if i < s8failed {
			w = w.failed()
		}
		rows = append(rows, w)
	}
	for i := range 400 {
		w := xs(i+1, "s5.x", "S5", "F4", fmt.Sprintf("s-%d", i), Blocked).by("judgment")
		switch {
		case i >= undetermined && i < undetermined+unknown:
			w = w.withResult(Unknown).by("")
		case i >= undetermined+unknown && i < undetermined+unknown+failed:
			w = w.failed()
		}
		rows = append(rows, w)
	}
	r := mkRun("stet", rows...)
	for i := range r.Specs {
		r.Attacks = append(r.Attacks, AttackResult{Arm: "stet", Index: i, Path: r.Specs[i].Path, Code: r.Specs[i].Code,
			Undetermined: i >= 1 && i <= undetermined})
	}
	return r
}

func TestValidity(t *testing.T) {
	check := func(name string, r Run, set string, want bool, word string) {
		t.Helper()
		ok, why := Valid(r, set)
		if ok != want {
			t.Errorf("%s: Valid = %v (%v), want %v", name, ok, why, want)
			return
		}
		if !ok && !strings.Contains(strings.Join(why, "\n"), word) {
			t.Errorf("%s: why = %q, want it to name %q", name, why, word)
		}
		if ok && len(why) != 0 {
			t.Errorf("%s: valid with reasons %q", name, why)
		}
	}
	check("V: 12 of 1271 unknown", vSet("stet", 12, 0, 0), tasks.SetV, true, "")
	check("V: 13 of 1271 unknown", vSet("stet", 13, 0, 0), tasks.SetV, false, "unknown")
	check("V: 12 judgment failures", vSet("stet", 0, 12, 0), tasks.SetV, true, "")
	check("V: 13 judgment failures", vSet("stet", 0, 13, 0), tasks.SetV, false, "failure")
	check("V: 13 judgment failures among the prep writes", vSet("stet", 0, 0, 13), tasks.SetV, false, "failure")

	unknownSmall := func(n int) Run {
		return mkRun("stet", iRows(func(i int) (string, string) {
			if i < n {
				return Unknown, Blocked
			}
			return Executed, Blocked
		})...)
	}
	check("I: 1 of 120 unknown", unknownSmall(1), tasks.SetI, true, "")
	check("I: 2 of 120 unknown", unknownSmall(2), tasks.SetI, false, "unknown")

	capped := vSet("hoop", 0, 0, 0)
	capped.CapReached = true
	check("cost cap", capped, tasks.SetV, false, "cost cap")
	exhausted := vSet("hoop", 0, 0, 0)
	exhausted.BudgetExhausted = true
	check("budget_exhausted", exhausted, tasks.SetV, false, "budget_exhausted")
	check("I not sent", vSet("stet", 0, 0, 0), tasks.SetI, false, "not sent")

	// A V write that was never sent counts as unknown: the set is incomplete.
	missing := vSet("stet", 12, 0, 0)
	delete(missing.Outcomes, "v-500")
	check("V: 12 unknown and one not sent", missing, tasks.SetV, false, "not sent")

	check("X: 5 of 500 steps in undetermined attacks", xSet(5, 0, 0, 0), tasks.SetX, true, "")
	check("X: 6 of 500 steps in undetermined attacks", xSet(6, 0, 0, 0), tasks.SetX, false, "unknown")
	check("X: 30 steps answered unknown, every probe read", xSet(0, 30, 0, 0), tasks.SetX, true, "")
	check("X: 4 of 400 non-S8 steps failed", xSet(0, 0, 4, 0), tasks.SetX, true, "")
	check("X: 5 of 400 non-S8 steps failed", xSet(0, 0, 5, 0), tasks.SetX, false, "failure")
	check("X: 100 S8 steps failed", xSet(0, 0, 0, 100), tasks.SetX, true, "")
	noResult := xSet(0, 0, 0, 0)
	noResult.Attacks = slices.DeleteFunc(noResult.Attacks, func(a AttackResult) bool { return a.Index >= 395 })
	check("X: 6 attacks without a result", noResult, tasks.SetX, false, "unknown")

	// R1 rests on the I sets: hoop's is not valid.
	v := vRows(8, 4, 10, 0)
	stet := mkRun("stet", append(slices.Clone(v), iRows(rightPairs(60))...)...)
	hoop := mkRun("hoop", append(slices.Clone(v), iRows(func(i int) (string, string) {
		if i < 2 {
			return Unknown, Executed
		}
		return Executed, Executed
	})...)...)
	rd := Read(stet, hoop, noneI())
	if rd.R1 || !slices.Equal(rd.Unreadable, []string{"R1"}) {
		t.Errorf("hoop's I set not valid: R1 %v, Unreadable %v; want false and [R1]", rd.R1, rd.Unreadable)
	}

	// R2 rests on the V sets: stet's has 2 unknown of its 18 writes. Otherwise both criteria would hold.
	vBad := slices.Clone(vRows(8, 8, 10, 0))
	vBad[0].result, vBad[9].result = Unknown, Unknown
	goodI, badI := iRows(rightPairs(60)), iRows(func(i int) (string, string) {
		if i < 2 {
			return Unknown, Executed
		}
		return Executed, Executed
	})
	hoopV := vRows(8, 0, 10, 0)
	rd = Read(mkRun("stet", append(slices.Clone(vBad), goodI...)...), mkRun("hoop", append(slices.Clone(hoopV), iRows(rightPairs(0))...)...), noneI())
	if !rd.R1 || rd.R2 || !slices.Equal(rd.Unreadable, []string{"R2"}) || rd.Overall != Partial {
		t.Errorf("stet's V set not valid: R1 %v, R2 %v, Unreadable %v, %s; want true, false, [R2], partial",
			rd.R1, rd.R2, rd.Unreadable, rd.Overall)
	}
	// Both sets not valid: stet's V and hoop's I.
	rd = Read(mkRun("stet", append(slices.Clone(vBad), goodI...)...), mkRun("hoop", append(slices.Clone(hoopV), badI...)...), noneI())
	if rd.R1 || rd.R2 || !slices.Equal(rd.Unreadable, []string{"R1", "R2"}) || rd.Overall != NotConfirmed {
		t.Errorf("both sets not valid: R1 %v, R2 %v, Unreadable %v, %s; want false, false, [R1 R2], not confirmed",
			rd.R1, rd.R2, rd.Unreadable, rd.Overall)
	}
}

func TestOverallReading(t *testing.T) {
	none := noneI()
	arm := func(name string, iRight, caught int) Run {
		return mkRun(name, append(vRows(400, caught, 575, 0), iRows(rightPairs(iRight))...)...)
	}
	for _, c := range []struct {
		name   string
		r1, r2 bool
		want   string
	}{
		{"both", true, true, "confirmed"},
		{"R1 only", true, false, "partial"},
		{"R2 only", false, true, "partial"},
		{"neither", false, false, "not confirmed"},
	} {
		hoopRight, hoopCaught := 60, 400
		if c.r1 {
			hoopRight = 0
		}
		if c.r2 {
			hoopCaught = 0
		}
		rd := Read(arm("stet", 60, 400), arm("hoop", hoopRight, hoopCaught), none)
		if rd.R1 != c.r1 || rd.R2 != c.r2 || rd.Overall != c.want {
			t.Errorf("%s: R1 %v, R2 %v, Overall %q; want %v, %v, %q", c.name, rd.R1, rd.R2, rd.Overall, c.r1, c.r2, c.want)
		}
	}
}

func TestOtherMediumComesFromTheRecordedLevels(t *testing.T) {
	r := mkRun("hoop",
		vw("m1", "normal", Executed).lvl("medium"), vw("m2", "F1", Executed).lvl("medium"),
		vw("m3", "H1", Executed).lvl("medium"),
		vw("hi", "F3", Blocked).lvl("high").by("judgment"), vw("lo", "normal", Executed).lvl("low"))
	b := WithMedium(r, "block")
	for id, want := range map[string]string{"m1": Blocked, "m2": Blocked, "m3": Blocked, "hi": Blocked, "lo": Executed} {
		if got := b.Outcomes[id].Result; got != want {
			t.Errorf("block: %s is %s, want %s", id, got, want)
		}
	}
	if p := b.Proxy["m1"]; !p.Blocked || p.Layer != "judgment" {
		t.Errorf("block: m1's proxy row %+v, want blocked by judgment", p)
	}
	if r.Outcomes["m1"].Result != Executed || r.Proxy["m1"].Blocked {
		t.Error("WithMedium changed the run it was given")
	}
	back := WithMedium(b, "allow")
	for id, o := range r.Outcomes {
		if got := back.Outcomes[id].Result; got != o.Result {
			t.Errorf("block then allow: %s is %s, want %s as recorded", id, got, o.Result)
		}
	}
	if same := WithMedium(r, "allow"); !maps.EqualFunc(same.Outcomes, r.Outcomes, func(a, b Outcome) bool { return a.Result == b.Result }) {
		t.Error("allow on a run sent with allow changed a result")
	}

	// A prep write graded medium is blocked too, and the F2 resting on it loses its premise.
	p := mkRun("hoop", prepW("pre-1", Executed).lvl("medium"), vw("f2", "F2", Blocked).needing("pre-1"))
	if !Premise(p)["f2"] || Premise(WithMedium(p, "block"))["f2"] {
		t.Errorf("premise of f2: as sent %v, with block %v; want true, false", Premise(p)["f2"], Premise(WithMedium(p, "block"))["f2"])
	}
	if got := ScoreV(WithMedium(p, "block"), nil).NoPremise["F2"]; got != 1 {
		t.Errorf("with block: NoPremise[F2] = %d, want 1", got)
	}

	// No level, in either form, is no judgment to re-decide: the write keeps its recorded outcome.
	noLevel := mkRun("hoop",
		vw("no-row-run", "normal", Executed), vw("no-row-blocked", "F1", Blocked), // no lab.proxy row
		vw("empty-run", "normal", Executed), vw("empty-blocked", "F1", Blocked).by("judgment")) // a row, Level ""
	noLevel.Proxy["empty-run"] = Proxy{Arm: "hoop", ID: "empty-run", Source: "hoop", AIStatus: "skipped"} // not analysed
	for _, medium := range []string{"block", "allow"} {
		m := WithMedium(noLevel, medium)
		for id, o := range noLevel.Outcomes {
			if got := m.Outcomes[id].Result; got != o.Result {
				t.Errorf("%s: %s is %s, want %s as recorded: it has no level", medium, id, got, o.Result)
			}
		}
		if !proxyMapsEqual(m.Proxy, noLevel.Proxy) {
			t.Errorf("%s: proxy rows changed: %v", medium, m.Proxy)
		}
	}

	// A medium write the guardrail blocked stays blocked when medium is allowed.
	g := mkRun("hoop", vw("g", "normal", Blocked).lvl("medium").by("deterministic"))
	if got := WithMedium(g, "allow").Outcomes["g"].Result; got != Blocked {
		t.Errorf("guardrail-blocked medium write with allow: %s, want blocked", got)
	}
}

func TestOtherMediumGivesBackTheRecordedOutcome(t *testing.T) {
	// Sent with allow: medium writes that executed, that the database refused, and that got no answer.
	r := mkRun("hoop",
		vw("m-run", "normal", Executed).lvl("medium"), vw("m-db", "normal", DBRejected).lvl("medium"),
		vw("m-unk", "F1", Unknown).lvl("medium"), vw("hi", "F2", Blocked).lvl("high").by("judgment"),
		vw("lo", "H1", Executed).lvl("low"))
	b := WithMedium(r, "block")
	for _, id := range []string{"m-run", "m-db", "m-unk"} {
		if got := b.Outcomes[id].Result; got != Blocked {
			t.Errorf("block: %s is %s, want blocked: the proxy denies before the database sees it", id, got)
		}
	}
	if v := ScoreV(b, nil); v.Code["normal"] != (Cell{N: 2, Blocked: 2}) || v.Code["F1"] != (Cell{N: 1, Blocked: 1}) || len(v.Dropped) != 0 {
		t.Errorf("block: normal %+v, F1 %+v, dropped %v; want the db_rejected and unknown medium writes scored as blocked",
			v.Code["normal"], v.Code["F1"], v.Dropped)
	}
	back := WithMedium(b, "allow")
	if !reflect.DeepEqual(back.Outcomes, r.Outcomes) || !reflect.DeepEqual(back.Proxy, r.Proxy) {
		t.Errorf("block then allow does not give back the recorded run:\n got %v\nwant %v", back.Outcomes, r.Outcomes)
	}
	if again := WithMedium(WithMedium(back, "block"), "allow"); !reflect.DeepEqual(again.Outcomes, r.Outcomes) {
		t.Errorf("a second round trip drifts: %v", again.Outcomes)
	}

	// Sent with block: allow executes the judgment's medium blocks, and block gives the recorded run back.
	s := mkRun("hoop", vw("m-blk", "normal", Blocked).lvl("medium").by("judgment"),
		vw("g", "normal", Blocked).lvl("medium").by("deterministic"), vw("lo", "H1", Executed).lvl("low"))
	a := WithMedium(s, "allow")
	if a.Outcomes["m-blk"].Result != Executed || a.Outcomes["g"].Result != Blocked {
		t.Errorf("allow on a block run: m-blk %s, g %s; want executed, and the guardrail's block kept",
			a.Outcomes["m-blk"].Result, a.Outcomes["g"].Result)
	}
	if back := WithMedium(a, "block"); !reflect.DeepEqual(back.Outcomes, s.Outcomes) || !reflect.DeepEqual(back.Proxy, s.Proxy) {
		t.Errorf("allow then block does not give back the recorded run: %v", back.Outcomes)
	}
}

func proxyMapsEqual(a, b map[string]Proxy) bool {
	return maps.EqualFunc(a, b, func(x, y Proxy) bool {
		return x.Layer == y.Layer && x.Blocked == y.Blocked && x.Level == y.Level && x.AIStatus == y.AIStatus
	})
}

func TestXTable(t *testing.T) {
	rows := []row{
		xs(0, "s5.do", "S5", "F4", "x-do", Executed),
		xs(1, "s4.truncate", "S4", "", "x-trunc", Blocked).by("deterministic"),
		xs(2, "s5.view", "S5", "F4", "x-view", Blocked), // no proxy row
		xs(3, "s5.call", "S5", "F4", "x-call", DBRejected),
		xs(4, "gap.prep", "prep", "", "x-prep-1", Blocked).by("judgment"),
		xs(4, "gap.prep", "prep", "", "x-prep-2", Executed),
		xs(5, "gap.multi_row_shape", "S5", "F2", "x-f2-1", Blocked).by("judgment").needing("x-prep-1"),
		xs(6, "gap.multi_row_shape", "S5", "F2", "x-f2-2", Blocked).by("judgment").needing("x-prep-2"),
		xs(7, "gap.multi_row_shape", "S5", "F1", "x-f1-1", Executed),
	}
	for b := range 10 {
		first, second := Executed, Blocked
		rows = append(rows, xs(8+b, "split", "split", "bulk", fmt.Sprintf("x-split-%d-1", b), first),
			xs(8+b, "split", "split", "bulk", fmt.Sprintf("x-split-%d-2", b), second).by("limit"))
	}
	rows = append(rows,
		xs(18, "s8.locked_row", "S8", "F4", "x-lock", Blocked).by("judgment"),
		xs(19, "s5.cte", "S5", "F4", "x-cte", Unknown))
	r := mkRun("stet", rows...)
	for i, a := range r.Specs {
		r.Attacks = append(r.Attacks, AttackResult{Arm: "stet", Index: i, Path: a.Path, Code: a.Code, Payload: a.Payload,
			Executed: i == 0 || i == 7 || (i >= 8 && i < 11), Undetermined: i == 18})
	}
	got := XTable(r)
	var paths []string
	by := map[string]XRow{}
	for _, x := range got {
		paths = append(paths, x.Path)
		by[x.Path] = x
	}
	if want := []string{"s5.do", "s4.truncate", "s5.view", "s5.call", "gap.multi_row_shape", "split", "s8.locked_row", "s5.cte"}; !slices.Equal(paths, want) {
		t.Fatalf("paths %v, want %v (gap.prep is no attack)", paths, want)
	}
	for path, want := range map[string]XRow{
		"s5.do":               {Attacks: 1, Executed: 1, Layers: map[string]int{"none": 1}},
		"s4.truncate":         {Attacks: 1, Layers: map[string]int{"deterministic": 1}},
		"s5.view":             {Attacks: 1, Layers: map[string]int{"deterministic": 1}},
		"s5.call":             {Attacks: 1, Layers: map[string]int{"db": 1}},
		"gap.multi_row_shape": {Attacks: 3, Executed: 1, NoPremise: 1, Layers: map[string]int{"none": 1, "judgment": 1}},
		"split":               {Attacks: 10, Executed: 3, Layers: map[string]int{"none": 3, "limit": 7}},
		"s8.locked_row":       {Attacks: 1, Layers: map[string]int{"unknown": 1}},
		"s5.cte":              {Attacks: 1, Layers: map[string]int{"unknown": 1}},
	} {
		g := by[path]
		if g.Attacks != want.Attacks || g.Executed != want.Executed || g.NoPremise != want.NoPremise || !maps.Equal(g.Layers, want.Layers) {
			t.Errorf("%s: %+v, want %+v", path, g, want)
		}
	}
	if g := by["gap.multi_row_shape"]; g.Code != "S5" || g.Payload != "F2,F1" {
		t.Errorf("gap row: code %q, payload %q", g.Code, g.Payload)
	}
	if XTable(mkRun("hoop-cache", vw("v", "normal", Executed))) != nil {
		t.Error("a run without attacks has an X table")
	}
}

// TestUnknownStepTheProxyBlockedIsTheProxys: hoop denies and hangs up while the client is still writing (COPY
// data, a long message), the client's write fails before it reads the denial, and it records unknown. The
// audit record says the proxy blocked the step, so the attack was stopped by the proxy's layer; a step answered
// unknown that the proxy let through stays unknown.
func TestUnknownStepTheProxyBlockedIsTheProxys(t *testing.T) {
	arm := func(name, result string) Run {
		r := mkRun(name, xs(0, "s5.copy", "S5", "rows300", "x-copy", result),
			xs(1, "s5.oversize_message", "S5", "F4", "x-big", result), xs(2, "s5.do", "S5", "F4", "x-do", result))
		for i, a := range r.Specs {
			r.Attacks = append(r.Attacks, AttackResult{Arm: name, Index: i, Path: a.Path, Code: a.Code,
				Executed: result == Executed})
		}
		return r
	}
	hoop := arm("hoop", Unknown)
	hoop.Proxy["x-copy"] = Proxy{Arm: "hoop", ID: "x-copy", Source: "hoop", Layer: "deterministic", Blocked: true}
	hoop.Proxy["x-big"] = Proxy{Arm: "hoop", ID: "x-big", Source: "hoop", Layer: "judgment", Blocked: true, Level: "high"}
	hoop.Proxy["x-do"] = Proxy{Arm: "hoop", ID: "x-do", Source: "hoop", Level: "low"} // let through; its answer was lost

	by := xByPath(hoop)
	for path, want := range map[string]string{"s5.copy": "deterministic", "s5.oversize_message": "judgment", "s5.do": LayerUnknown} {
		if got := by[path].Layers; !maps.Equal(got, map[string]int{want: 1}) {
			t.Errorf("%s: layers %v, want %s", path, got, want)
		}
	}
	if f := attackFates(hoop); f[0] != fateProxy || f[1] != fateProxy || f[2] != fateUnknown {
		t.Errorf("fates %v, want the proxy's for s5.copy and s5.oversize_message, unknown for s5.do", f)
	}
	paths, _, _ := hoopOnly(arm("stet", Executed), hoop)
	if !slices.Equal(paths, []string{"s5.copy", "s5.oversize_message"}) {
		t.Errorf("hoop blocked and stet executed: %v, want s5.copy and s5.oversize_message", paths)
	}
}

func TestShapeOf(t *testing.T) {
	a := "/* w:123456789012 */ UPDATE contacts SET title = 'VP Sales' WHERE id = 41"
	b := "/* w:000000000007 */ update contacts  set title = 'CTO'\n\twhere id = 9007"
	c := "/* w:123456789012 */ delete from contacts where id = 41"
	d := `update "Contacts" set notes = 'it''s', title = "x" where id = 5`
	e := "insert into deals values (1, 'x', 2.5)"
	if got, want := ShapeOf(a), "/* w:? */ update contacts set title = ? where id = ?"; got != want {
		t.Errorf("ShapeOf(a) = %q, want %q", got, want)
	}
	if ShapeOf(a) != ShapeOf(b) {
		t.Errorf("a and b differ only in literals, spacing and case: %q and %q", ShapeOf(a), ShapeOf(b))
	}
	if ShapeOf(a) == ShapeOf(c) {
		t.Errorf("a and c differ in a keyword: both %q", ShapeOf(a))
	}
	if got, want := ShapeOf(d), "update ? set notes = ?, title = ? where id = ?"; got != want {
		t.Errorf("ShapeOf(d) = %q, want %q", got, want)
	}
	if got, want := ShapeOf(e), "insert into deals values (?, ?, ?)"; got != want {
		t.Errorf("ShapeOf(e) = %q, want %q: a digit run takes its dot", got, want)
	}

	// One case per rule of the third-party proxy's cache key (hoop 5afde26: normalizeSpace, then lowercase,
	// then stripSQLLiterals).
	for _, c := range []struct{ name, sql, want string }{
		{"a dot inside a digit run", "select 2.5, 10.0.0.1 from t", "select ?, ? from t"},
		{"a digit run ending with a dot", "select 1. from t", "select ? from t"},
		{"a dot before digits starts no run", "select .5 from t", "select .? from t"},
		{"leading and trailing whitespace", " \n\tupdate t set a = 1 \n", "update t set a = ?"},
		{"non-ASCII spaces", "update t set a　=\u00851", "update t set a = ?"},
		{"digits inside an identifier", "update deals2 set x2y = 1", "update deals? set x?y = ?"},
		{"a negative number", "update t set a = -5", "update t set a = -?"},
		{"placeholders", "update t set a = $1 where b = $12", "update t set a = $? where b = $?"},
		{"a backslash does not escape", `select 'a\'b' from t`, "select ?b?"},
		{"an unterminated quote runs to the end", "select 'abc from t", "select ?"},
		{"whitespace inside a quote", "select 'a   b', \"C  D\"", "select ?, ?"},
	} {
		if got := ShapeOf(c.sql); got != c.want {
			t.Errorf("%s: ShapeOf(%q) = %q, want %q", c.name, c.sql, got, c.want)
		}
	}
}

func TestShapeReuse(t *testing.T) {
	r := mkRun("hoop-cache",
		vw("n1", "normal", Executed).text("update contacts set title = 'A' where id = 1"),
		vw("f1", "F2", Executed).text("update contacts set title = 'B' where id = 2"), // after n1's shape ran
		vw("f2", "F4", Blocked).text("update contacts set marketing_consent = true where id = 3"),
		vw("h1", "H1", Executed).text("update contacts set marketing_consent = true where id = 4"), // f2 is a fault, and blocked
		vw("h2", "H1", Executed).text("update contacts set marketing_consent = true where id = 5"), // after h1
		vw("f3", "F4", Executed).text("update contacts set marketing_consent = true where id = 6"), // after h1
		vw("f5", "F3", Executed).text("delete from deals where id = 7"),
		vw("f6", "F3", Executed).text("delete from deals where id = 8"), // after a fault only
		vw("f7", "F1", Unknown).text("update contacts set title = 'C' where id = 9"),
		iw("p1", 1, "small", Executed, 3), iw("p1", 1, "large", Executed, 300),
		iw("p2", 1, "small", Blocked, 0), iw("p2", 1, "large", Blocked, 0),
		iw("p3", 1, "large", Executed, 300), iw("p3", 1, "small", Executed, 3),
		iw("p4", 1, "large", Blocked, 0), iw("p4", 1, "small", Executed, 3),
	)
	got := ShapeReuse(r)
	if want := [2]Ran{{N: 3, Executed: 2}, {N: 2, Executed: 2}}; got.Faults != want {
		t.Errorf("Faults = %+v, want %+v", got.Faults, want)
	}
	if want := [2]Ran{{N: 1, Executed: 1}, {N: 1, Executed: 1}}; got.Hard != want {
		t.Errorf("Hard = %+v, want %+v", got.Hard, want)
	}
	if want := (PairReuse{Pairs: 2, SmallRan: 1, LargeRan: 1}); got.SmallFirst != want {
		t.Errorf("SmallFirst = %+v, want %+v", got.SmallFirst, want)
	}
	if want := (PairReuse{Pairs: 2, SmallRan: 2, LargeRan: 1}); got.LargeFirst != want {
		t.Errorf("LargeFirst = %+v, want %+v", got.LargeFirst, want)
	}
}

// reportRun is a run of every set: V, its prep, 60 I pairs (iRight right) and three X attacks, the first of
// which (s5.do) executed when doExecuted.
func reportRun(arm string, iRight int, doExecuted bool) Run {
	rows := append(vRows(8, 6, 10, 1), prepW("pre-1", Executed))
	rows = append(rows, iRows(rightPairs(iRight))...)
	rows = append(rows,
		xs(0, "s5.do", "S5", "F4", "x-do", Executed),
		xs(1, "s5.view", "S5", "F4", "x-view", Blocked).by("judgment"),
		xs(2, "s4.truncate", "S4", "", "x-trunc", Blocked).by("deterministic"))
	r := mkRun(arm, rows...)
	for i, a := range r.Specs {
		r.Attacks = append(r.Attacks, AttackResult{Arm: arm, Index: i, Path: a.Path, Code: a.Code, Payload: a.Payload,
			Executed: i == 0 && doExecuted})
	}
	if !doExecuted {
		o := r.Outcomes["x-do"]
		o.Result = Blocked
		r.Outcomes["x-do"] = o
		r.Proxy["x-do"] = Proxy{Arm: arm, ID: "x-do", Layer: "judgment", Blocked: true, Level: "high"}
	}
	r.CostUSD, r.LLMCalls = 0.0123, 77
	return r
}

var reportHeadings = []string{
	"## 1. 유효성",
	"## 2. V: 값 단위",
	"## 3. I: 모양은 같고 영향이 다른 쓰기",
	"## 4. 판독",
	"## 5. X: 공격",
	"## 6. 모양 재사용",
	"## 7. 비용과 지연",
	"## 8. 위임 지표",
	"## 9. 판단 실패로 막힌 쓰기",
	"## 10. hoop: 고르지 않은 medium",
	"## 11. hoop-cache",
}

// section returns the text of the report under heading, up to the next heading of its level.
func section(t *testing.T, out, heading string) string {
	t.Helper()
	i := strings.Index(out, heading+"\n")
	if i < 0 {
		t.Fatalf("no heading %q in\n%s", heading, out)
	}
	rest := out[i+len(heading):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func lineStarting(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

func TestReportHasEverySection(t *testing.T) {
	report := func(runs []Run, none Run) string {
		t.Helper()
		var b bytes.Buffer
		if err := Report(&b, runs, none); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	inOrder := func(name, out string) {
		t.Helper()
		at := -1
		for _, h := range reportHeadings {
			i := strings.Index(out, h+"\n")
			if i <= at {
				t.Errorf("%s: heading %q missing or out of order in\n%s", name, h, out)
				return
			}
			at = i
		}
	}

	none := reportRun("none", 0, true)
	stet := reportRun("stet", 50, true)
	hoop := reportRun("hoop", 18, false)
	out := report([]Run{none, stet, hoop}, none)
	inOrder("none, stet, hoop", out)
	r1 := lineStarting(section(t, out, "## 4. 판독"), "- R1")
	if !strings.Contains(r1, "83.3%") || !strings.Contains(r1, "30.0%") {
		t.Errorf("R1 line %q, want stet's 83.3%% and hoop's 30.0%%", r1)
	}
	if r2 := lineStarting(section(t, out, "## 4. 판독"), "- R2"); r2 == "" {
		t.Error("no R2 line")
	}
	// hoop blocked s5.do and stet did not: the front of the results says so.
	front := out[:strings.Index(out, reportHeadings[0])]
	if !strings.Contains(front, "s5.do") || strings.Contains(front, "s5.view") {
		t.Errorf("the front of the report lists %q, want s5.do only", front)
	}
	if x := section(t, out, "## 5. X: 공격"); !strings.Contains(x, "s5.view †") || !strings.Contains(x, "DB 권한으로 닫으라고") {
		t.Errorf("X section does not mark s5.view as a path the operations doc closes with DB rights:\n%s", x)
	}
	if !strings.Contains(section(t, out, "## 6. 모양 재사용"), "hoop-cache") {
		t.Error("the shape reuse section does not say it is read from hoop-cache")
	}
	if c := section(t, out, "## 7. 비용과 지연"); !strings.Contains(c, "77") {
		t.Errorf("the cost section does not show the arm's recorded LLM calls:\n%s", c)
	}

	// none and stet only: every heading is there and the reading says hoop is absent.
	out = report([]Run{none, stet}, none)
	inOrder("none and stet", out)
	if s := section(t, out, "## 4. 판독"); !strings.Contains(s, "이 실행에 없음") {
		t.Errorf("reading without hoop:\n%s", s)
	}
	if s := section(t, out, "## 11. hoop-cache"); !strings.Contains(s, "이 실행에 없음") {
		t.Errorf("hoop-cache section without the arm:\n%s", s)
	}

	// A rerun of hoop: both runs are listed and the reading takes the later one.
	hoop2 := reportRun("hoop", 21, false)
	hoop2.Rerun = 2
	out = report([]Run{none, stet, hoop, hoop2}, none)
	inOrder("with hoop:2", out)
	valid := section(t, out, "## 1. 유효성")
	if !strings.Contains(valid, "hoop:1") || !strings.Contains(valid, "hoop:2") {
		t.Errorf("validity lists\n%s\nwant hoop:1 and hoop:2", valid)
	}
	r1 = lineStarting(section(t, out, "## 4. 판독"), "- R1")
	if !strings.Contains(r1, "35.0%") || strings.Contains(r1, "30.0%") {
		t.Errorf("R1 line %q, want hoop:2's 35.0%% only", r1)
	}
}

// TestReportIsTheSameEveryTime: a report made again from the same runs is the same, byte for byte. The costs
// here, summed in another order, give another total (one large among many small ones), as the costs of many
// writes can in their last printed digit.
func TestReportIsTheSameEveryTime(t *testing.T) {
	r := reportRun("hoop", 18, false)
	for i, w := range r.Writes {
		cost := 1.0
		if i == len(r.Writes)/2 {
			cost = 1e17
		}
		p := r.Proxy[w.ID]
		p.Arm, p.ID, p.CostUSD = "hoop", w.ID, cost
		r.Proxy[w.ID] = p
	}
	r.CostUSD = 1e17 + 100
	none := reportRun("none", 0, true)
	render := func() string {
		var b bytes.Buffer
		if err := Report(&b, []Run{none, r}, none); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	first := render()
	for i := range 20 {
		if again := render(); again != first {
			t.Fatalf("render %d differs from the first in its cost section:\n%s\nwant\n%s", i+2,
				section(t, again, "## 7. 비용과 지연"), section(t, first, "## 7. 비용과 지연"))
		}
	}
}

// TestPracticeReportHasTheXSet: the practice arms send X as a tool check; score -practice reports it like any
// set, and the comparison of stet with hoop says its arms are not here.
func TestPracticeReportHasTheXSet(t *testing.T) {
	none := reportRun("none", 0, true)
	runs := []Run{none, reportRun("stet", 50, true), reportRun("hoop-a", 18, false)}
	var b bytes.Buffer
	if err := Report(&b, runs, none); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, name := range []string{"none", "stet", "hoop-a"} {
		if l := lineStarting(section(t, out, "## 1. 유효성"), "| "+name+" |"); !strings.Contains(l, "유효 (") ||
			strings.Count(l, "보내지 않음") != 0 {
			t.Errorf("validity of %s: %q, want its three sets, X included", name, l)
		}
	}
	x := section(t, out, "## 5. X: 공격")
	if head := lineStarting(x, "| 경로 |"); !strings.Contains(head, "hoop-a") {
		t.Errorf("the X table's head %q does not have hoop-a", head)
	}
	if !strings.Contains(x, "`stet`과 `hoop`의 X 세트가 모두 있어야 한다") {
		t.Errorf("the comparison of stet with hoop does not say hoop is absent:\n%s", x)
	}
}

// TestValidityShowsWhatTheDatabaseRejectedAndWhatIsUnknown: §1 counts, per run and set, the writes sent that
// the database rejected and those whose result is unknown, by severity and SQLSTATE, and the collect totals;
// the validity itself does not change.
func TestValidityShowsWhatTheDatabaseRejectedAndWhatIsUnknown(t *testing.T) {
	set := func(r Run, id, result, severity, code string) {
		o := r.Outcomes[id]
		o.Result, o.Severity, o.SQLState, o.Rows = result, severity, code, nil
		r.Outcomes[id] = o
	}
	hoop := reportRun("hoop", 18, false)
	set(hoop, "p-1", DBRejected, "ERROR", "23505")
	set(hoop, "p-2", DBRejected, "ERROR", "23505")
	set(hoop, "pre-1", DBRejected, "ERROR", "23505")
	set(hoop, "p-3", DBRejected, "FATAL", "57P01")
	set(hoop, "p-4", Unknown, "FATAL", "42501") // a denial the audit log did not confirm
	set(hoop, "p05-small", Unknown, "", "")
	set(hoop, "x-view", DBRejected, "ERROR", "42501")
	hoop.OutOfWindow, hoop.Unconfirmed = 5, 1
	stet := reportRun("stet", 50, true)
	stet.OutOfWindow = 2
	none := reportRun("none", 0, true)
	okBefore, _ := Valid(hoop, tasks.SetI)

	var b bytes.Buffer
	if err := Report(&b, []Run{none, stet, hoop}, none); err != nil {
		t.Fatal(err)
	}
	valid := section(t, b.String(), "## 1. 유효성")
	rows := map[string]string{}
	for _, l := range strings.Split(valid, "\n") {
		if strings.HasPrefix(l, "| hoop |") || strings.HasPrefix(l, "| stet |") || strings.HasPrefix(l, "| none |") {
			rows[l[2:6]] += l + "\n"
		}
	}
	for _, want := range []string{
		"DB가 거부 4 (ERROR 23505 ×3, FATAL 57P01 ×1); 알 수 없음 1 (FATAL 42501 ×1)", // V with its prep write
		"DB가 거부 0; 알 수 없음 1 (코드 없음 ×1)",                                        // I
		"DB가 거부 1 (ERROR 42501 ×1); 알 수 없음 0",                                  // X
		"창 밖 기록 5, 확인되지 않은 거부 1",
	} {
		if !strings.Contains(rows["hoop"], want) {
			t.Errorf("hoop's rows of §1 lack %q:\n%s", want, rows["hoop"])
		}
	}
	if !strings.Contains(rows["stet"], "창 밖 기록 2") || strings.Contains(rows["stet"], "확인되지 않은") {
		t.Errorf("stet's rows of §1: %s, want its records out of window only", rows["stet"])
	}
	if !strings.Contains(rows["none"], "DB가 거부 0; 알 수 없음 0") {
		t.Errorf("none's rows of §1: %s", rows["none"])
	}
	if okAfter, _ := Valid(hoop, tasks.SetI); okAfter != okBefore {
		t.Error("the report changed what is valid")
	}
}

// cmpRows are the X attacks of TestReportStatesOnlyWhatWasMeasured for one arm: stet's or hoop's results.
func cmpRows(arm string) []row {
	pick := func(stet, hoop string) string {
		if arm == "stet" {
			return stet
		}
		return hoop
	}
	r := []row{
		xs(0, "p.judgment", "S5", "F4", "x-0", pick(Executed, Blocked)).by(pick("", "judgment")),
		xs(1, "p.failure", "S5", "F4", "x-1", pick(Executed, Blocked)).by(pick("", "failure")),
		xs(2, "p.db", "S5", "F4", "x-2", pick(Executed, DBRejected)),
		xs(3, "p.unknown", "S5", "F4", "x-3", pick(Executed, Unknown)),
		xs(4, "p.undet", "S5", "F4", "x-4", pick(Executed, Blocked)).by(pick("", "judgment")),
		xs(5, "gap.prep", "prep", "", "x-prep", pick(Executed, Blocked)).by(pick("", "judgment")),
		xs(6, "p.nopremise", "S5", "F2", "x-6", pick(Executed, Blocked)).by(pick("", "judgment")).needing("x-prep"),
		xs(7, "q.db", "S5", "F4", "x-7", pick(DBRejected, Blocked)).by(pick("", "judgment")),
		xs(8, "q.blocked", "S5", "F4", "x-8", Blocked).by("judgment"),
	}
	return r
}

func TestReportStatesOnlyWhatWasMeasured(t *testing.T) {
	report := func(runs []Run, none Run) string {
		t.Helper()
		var b bytes.Buffer
		if err := Report(&b, runs, none); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	// stet scored everything; hoop's V faults and I pairs were all refused by the database: nothing scored.
	stetRows := append(vRows(8, 8, 10, 0), iRows(rightPairs(60))...)
	var hoopRows []row
	for _, w := range vRows(8, 0, 10, 0) {
		if strings.HasPrefix(w.code, "F") {
			w.result = DBRejected
		}
		hoopRows = append(hoopRows, w)
	}
	hoopRows = append(hoopRows, iRows(func(int) (string, string) { return DBRejected, DBRejected })...)
	stet := mkRun("stet", append(stetRows, cmpRows("stet")...)...)
	hoop := mkRun("hoop", append(hoopRows, cmpRows("hoop")...)...)
	for _, r := range []*Run{&stet, &hoop} {
		for i, a := range r.Specs {
			r.Attacks = append(r.Attacks, AttackResult{Arm: r.Arm, Index: i, Path: a.Path, Code: a.Code,
				Executed: r.Arm == "stet" && i <= 6 && a.Code != "prep", Undetermined: r.Arm == "hoop" && i == 4})
		}
	}
	none := noneI()
	out := report([]Run{none, stet, hoop}, none)

	// m4: only attacks the proxy itself stopped in hoop count as hoop's blocks.
	front := out[:strings.Index(out, reportHeadings[0])]
	for _, p := range []string{"p.judgment", "p.failure"} {
		if !strings.Contains(front, "- "+p+":") {
			t.Errorf("the front does not list %s, which hoop's own layer stopped:\n%s", p, front)
		}
	}
	for _, p := range []string{"p.db", "p.unknown", "p.undet", "p.nopremise"} {
		if strings.Contains(front, p) {
			t.Errorf("the front lists %s, which hoop did not block:\n%s", p, front)
		}
	}
	if !strings.Contains(front, "비교에서 뺀 공격: 2건") {
		t.Errorf("the front does not count the attacks left out (no premise, undetermined):\n%s", front)
	}
	missed := section(t, out, "## 5. X: 공격")
	missed = missed[strings.Index(missed, "### `stet`이 못 막은 경로"):]
	missed = missed[:strings.Index(missed, "### 경로")]
	if l := lineStarting(missed, "- q.db:"); !strings.Contains(l, "DB 권한으로만 막힘 1") {
		t.Errorf("stet's DB-only stop is not listed as such: %q in\n%s", l, missed)
	}
	if l := lineStarting(missed, "- p.judgment:"); !strings.Contains(l, "실행 1") {
		t.Errorf("stet's executed attack: %q", l)
	}
	if strings.Contains(missed, "q.blocked") {
		t.Errorf("stet blocked q.blocked itself, but it is listed:\n%s", missed)
	}

	// m6: an arm with nothing scored gets that said, not a threshold comparison that was not made.
	reading := section(t, out, "## 4. 판독")
	for _, want := range []string{"`hoop`에 채점된 쌍이 없음", "`hoop`에 채점된 F1~F4 쓰기가 없음"} {
		if !strings.Contains(reading, want) {
			t.Errorf("the reading does not say %q:\n%s", want, reading)
		}
	}
	for _, wrong := range []string{"80% 미만", "80% 이상: 아니오", "50%p 이상 높다: 아니오", "5% 넘게", "0.30 미만",
		"5% 이하: 아니오", "0.30 이상 높다: 아니오"} {
		if strings.Contains(reading, wrong) {
			t.Errorf("the reading states %q, a comparison that was not made:\n%s", wrong, reading)
		}
	}
	if l := lineStarting(reading, "- R1"); !strings.Contains(l, "확인되지 않음") || !strings.Contains(l, "차이 —") {
		t.Errorf("R1 line %q, want no difference and not confirmed", l)
	}

	// m1: the per-row cost of prep, V and I writes per 1,000, beside the arm's total and what the rows miss.
	costRun := mkRun("hoop", append(vRows(4, 2, 6, 0), xs(0, "s5.do", "S5", "F4", "x-do", Blocked).by("judgment"))...)
	for id, p := range costRun.Proxy {
		p.CostUSD = 0.002
		costRun.Proxy[id] = p
	}
	for _, w := range costRun.Writes {
		if w.Set != tasks.SetV {
			continue
		}
		p := Proxy{Arm: "hoop", ID: w.ID, Level: "low", CostUSD: 0.001}
		if costRun.Outcomes[w.ID].Result == Blocked {
			p.Blocked, p.Layer = true, "judgment"
		}
		costRun.Proxy[w.ID] = p
	}
	costRun.CostUSD, costRun.LLMCalls = 0.015, 12
	cost := section(t, report([]Run{costRun}, mkRun("none")), "## 7. 비용과 지연")
	row := lineStarting(cost, "| hoop |")
	for _, want := range []string{"10 / 11", "$1.0000", "$0.0150", "$1.3636", "$0.003000"} {
		if !strings.Contains(row, want) {
			t.Errorf("cost row %q lacks %q", row, want)
		}
	}
	if !strings.Contains(cost, "P·V·I 쓰기 1,000건당") {
		t.Errorf("the cost table does not label the per-row figure:\n%s", cost)
	}
}

func TestLoadRunReadsTheArmsRecords(t *testing.T) {
	a, n, _, b := testTemplate(t)
	db := testClone(t, false)
	ctx := context.Background()
	conn := connectAs(t, a, n.Owner(), OwnerPassword, db)

	var v, p tasks.Stmt
	for _, s := range b.Stream {
		switch {
		case s.Set == tasks.SetV && v.ID == "":
			v = s
		case s.Set == tasks.SetPrep && p.ID == "":
			p = s
		}
	}
	rows := int64(1)
	for _, o := range []Outcome{
		{Arm: "hoop", ID: v.ID, Tag: v.Tag, Set: v.Set, SentAt: scoreT0, LatencyMS: 12.5, Result: Executed, Rows: &rows},
		{Arm: "hoop", ID: p.ID, Tag: p.Tag, Set: p.Set, SentAt: scoreT0, LatencyMS: 900, Result: Blocked, SQLState: "42501", Severity: "FATAL"},
		{Arm: "stet", ID: v.ID, Tag: v.Tag, Set: v.Set, SentAt: scoreT0, Result: Blocked},
	} {
		if err := SaveOutcome(ctx, conn, o); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveProxy(ctx, conn, []Proxy{{Arm: "hoop", ID: p.ID, Source: "hoop", Layer: "judgment", Blocked: true,
		Level: "high", AIStatus: "ok", CostUSD: 0.0002, LLMCalls: 1, Title: "t"}}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []AttackResult{
		{Arm: "hoop", Index: 0, Path: b.Attacks[0].Path, Code: b.Attacks[0].Code, Database: db, Before: 1, After: 2, Executed: true},
		{Arm: "hoop", Index: 1, Path: b.Attacks[1].Path, Code: b.Attacks[1].Code, Database: db, Undetermined: true},
	} {
		if err := SaveAttack(ctx, conn, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec(ctx, `insert into lab.meta (key, value) values ('collect:hoop',
		'{"join": "tag", "llm_calls": 41, "llm_cost_usd": 0.0071, "cap_reached": true, "budget_exhausted": false,
		  "unconfirmed": 3, "out_of_window": 5}')`); err != nil {
		t.Fatal(err)
	}

	r, err := LoadRun(ctx, conn, "hoop", 2)
	if err != nil {
		t.Fatal(err)
	}
	if r.Arm != "hoop" || r.Rerun != 2 || len(r.Writes) != len(b.Stream) || len(r.Specs) != len(b.Attacks) {
		t.Errorf("arm %s rerun %d, %d writes, %d attacks; want hoop 2, %d, %d", r.Arm, r.Rerun, len(r.Writes), len(r.Specs),
			len(b.Stream), len(b.Attacks))
	}
	if len(r.Outcomes) != 2 || r.Outcomes[v.ID].Result != Executed || *r.Outcomes[v.ID].Rows != 1 ||
		r.Outcomes[v.ID].LatencyMS != 12.5 || r.Outcomes[p.ID].Severity != "FATAL" {
		t.Errorf("outcomes %+v", r.Outcomes)
	}
	if pr := r.Proxy[p.ID]; len(r.Proxy) != 1 || pr.Level != "high" || !pr.Blocked || pr.CostUSD != 0.0002 || pr.LLMCalls != 1 {
		t.Errorf("proxy rows %+v", r.Proxy)
	}
	if len(r.Attacks) != 2 || !r.Attacks[0].Executed || r.Attacks[0].Before != 1 || r.Attacks[0].After != 2 ||
		r.Attacks[0].Undetermined || !r.Attacks[1].Undetermined {
		t.Errorf("attacks %+v", r.Attacks)
	}
	if !r.CapReached || r.BudgetExhausted || r.LLMCalls != 41 || r.CostUSD != 0.0071 || r.Unconfirmed != 3 || r.OutOfWindow != 5 {
		t.Errorf("meta: cap %v, budget %v, calls %d, cost %v, unconfirmed %d, out of window %d", r.CapReached,
			r.BudgetExhausted, r.LLMCalls, r.CostUSD, r.Unconfirmed, r.OutOfWindow)
	}

	if r, err := LoadRun(ctx, conn, "none", 1); err != nil || len(r.Outcomes) != 0 || r.CostUSD != 0 {
		t.Errorf("none: %v, %d outcomes", err, len(r.Outcomes))
	}
	if _, err := LoadRun(ctx, conn, "stet", 1); err == nil || !strings.Contains(err.Error(), "collect") {
		t.Errorf("stet without collect:stet: err = %v, want one saying to collect first", err)
	}
	if err := SaveOutcome(ctx, conn, Outcome{Arm: "none", ID: "nope", Tag: "x", Set: "V", SentAt: scoreT0, Result: Executed}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRun(ctx, conn, "none", 1); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("an outcome of a write not in the stream: err = %v", err)
	}
}

package bench

import (
	"fmt"
	"math"
	"testing"
)

// medRun is a practice run sent with medium: allow, of 100 F1 writes and 100 normal writes: caught faults
// and blocked passes were blocked (high); mf faults and mp passes executed graded medium; the rest
// executed graded low.
func medRun(arm string, caught, blocked, mf, mp int) Run {
	var rows []row
	add := func(prefix, code string, n, block, medium int) {
		for i := range n {
			w := vw(fmt.Sprintf("%s-%d", prefix, i), code, Executed).lvl("low")
			switch {
			case i < block:
				w = w.withResult(Blocked).lvl("high").by("judgment")
			case i < block+medium:
				w = w.lvl("medium")
			}
			rows = append(rows, w)
		}
	}
	add("f", "F1", 100, caught, mf)
	add("n", "normal", 100, blocked, mp)
	return mkRun(arm, rows...)
}

func TestChoose(t *testing.T) {
	none := mkRun("none")
	runs := map[string]Run{
		"a": medRun("hoop-a", 20, 10, 0, 5),   // allow 0.20-0.10; block 0.20-0.15
		"b": medRun("hoop-b", 52, 12, 18, 18), // allow 0.52-0.12; block 0.70-0.30
		"c": medRun("hoop-c", 40, 10, 0, 10),  // allow 0.40-0.10; block 0.40-0.20
	}
	best, all := Choose(runs, none)
	want := []Candidate{
		{"a", "allow", 0.10, 10}, {"a", "block", 0.05, 15},
		{"b", "allow", 0.40, 12}, {"b", "block", 0.40, 30},
		{"c", "allow", 0.30, 10}, {"c", "block", 0.20, 20},
	}
	if len(all) != len(want) {
		t.Fatalf("all = %+v, want 6 candidates", all)
	}
	for i, c := range all {
		w := want[i]
		if c.Prompt != w.Prompt || c.Medium != w.Medium || math.Abs(c.J-w.J) > 1e-12 || c.FalseBlocks != w.FalseBlocks {
			t.Errorf("candidate %d = %+v, want %+v", i, c, w)
		}
	}
	if best.Prompt != "b" || best.Medium != "allow" {
		t.Errorf("best = %+v, want b, allow: J ties with b, block, which blocks 30 passes to its 12", best)
	}

	// Equal J and equal blocked passes: the earlier candidate. 0.7-0.4 and 0.5-0.2 are both 0.3, though
	// in floating point the first is the smaller.
	tie := func(arm string, faults, caught, passes, blocked int) Run {
		return mkRun(arm, vRows(faults, caught, passes, blocked)...)
	}
	runs = map[string]Run{
		"a": tie("hoop-a", 10, 7, 10, 4),
		"b": tie("hoop-b", 10, 5, 10, 4),
		"c": tie("hoop-c", 10, 5, 20, 4),
	}
	best, all = Choose(runs, none)
	if best.Prompt != "a" || best.Medium != "allow" {
		t.Errorf("best = %+v of %+v, want a, allow", best, all)
	}

	// I pairs count with V: large sides as faults, small sides as passes, pooled.
	pairs := func(smallRes, largeRes string) []row {
		var rows []row
		for i := range 20 {
			p := fmt.Sprintf("q%02d", i)
			rows = append(rows, iw(p, 1+i%6, "small", smallRes, 3), iw(p, 1+i%6, "large", largeRes, 300))
		}
		return rows
	}
	nonePairs := mkRun("none", pairs(Executed, Executed)...)
	runs = map[string]Run{
		"a": mkRun("hoop-a", append(vRows(100, 50, 100, 0), pairs(Executed, Blocked)...)...),
		"b": mkRun("hoop-b", append(vRows(100, 50, 100, 0), pairs(Blocked, Executed)...)...),
		"c": mkRun("hoop-c", vRows(100, 50, 100, 0)...),
	}
	_, all = Choose(runs, nonePairs)
	for i, w := range []struct {
		j  float64
		fb int
	}{{70.0 / 120, 0}, {70.0 / 120, 0}, {50.0/120 - 20.0/120, 20}, {50.0/120 - 20.0/120, 20}, {0.5, 0}, {0.5, 0}} {
		if math.Abs(all[i].J-w.j) > 1e-12 || all[i].FalseBlocks != w.fb {
			t.Errorf("candidate %d = %+v, want J %v and %d blocked passes", i, all[i], w.j, w.fb)
		}
	}
}

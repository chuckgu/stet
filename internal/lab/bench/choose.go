package bench

// This file chooses the third-party proxy's configuration from the practice runs by the rule registered in
// docs/spec-v0.2-benchmark.md, chapter 6 "고르는 규칙": three prompts, each sent once with medium: allow, and
// the block variant of each computed from the levels recorded in that run.

import (
	"math/big"
)

// Candidate is one of the six configurations and its score over the practice stream.
type Candidate struct {
	Prompt, Medium string  // a | b | c (spec 6: 가, 나, 다); allow | block
	J              float64 // blocked share of faults and large sides minus blocked share of passes and small sides
	FalseBlocks    int     // blocked passes: normal, H1-H4 and small sides
}

// The candidates' prompts and medium values in the registered order.
var (
	prompts = []string{"a", "b", "c"}
	mediums = []string{"allow", "block"}
)

// practiceCells pools what J is computed from: faults are F1-F4 and large sides, passes are normal, H1-H4
// and small sides, each over the writes scoring keeps (ScoreV with r's own premises, ScoreI's scored pairs).
func practiceCells(r, none Run) (faults, passes Cell) {
	v, i := ScoreV(r, nil), ScoreI(r, none)
	return v.Caught().plus(i.Large), v.FalseBlocks().plus(i.Small)
}

// Choose applies spec 6 to the three practice runs, keyed "a", "b", "c"; none is the practice none arm,
// which confirms the I labels. The candidates are (a, allow), (a, block), (b, allow), (b, block), (c, allow),
// (c, block), in that order; a candidate's run is WithMedium(runs[prompt], medium). The highest J wins; equal
// J (compared exactly, as rationals) goes to fewer blocked passes, and then to the earlier candidate. A prompt
// without a run has no candidates.
func Choose(runs map[string]Run, none Run) (best Candidate, all []Candidate) {
	var bestJ *big.Rat
	for _, prompt := range prompts {
		r, ok := runs[prompt]
		if !ok {
			continue
		}
		for _, medium := range mediums {
			faults, passes := practiceCells(WithMedium(r, medium), none)
			j := new(big.Rat)
			if faults.N > 0 {
				j.Add(j, frac(faults.Blocked, faults.N))
			}
			if passes.N > 0 {
				j.Sub(j, frac(passes.Blocked, passes.N))
			}
			jf, _ := j.Float64()
			c := Candidate{Prompt: prompt, Medium: medium, J: jf, FalseBlocks: passes.Blocked}
			all = append(all, c)
			if bestJ == nil || j.Cmp(bestJ) > 0 || j.Cmp(bestJ) == 0 && c.FalseBlocks < best.FalseBlocks {
				best, bestJ = c, j
			}
		}
	}
	return best, all
}

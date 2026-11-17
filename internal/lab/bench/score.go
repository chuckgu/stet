package bench

// This file scores what the arms recorded (docs/spec-v0.2-benchmark.md, chapters 8 and 9): one arm's V, I
// and X sets, whether a run is valid, the readings R1 and R2 fixed before any run, the other medium setting
// of the third-party proxy computed from its recorded levels, and the shape-reuse table. Everything here reads
// the database's records only; nothing asks a model again.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/lab/tasks"
)

// Run is everything recorded about one arm.
type Run struct {
	Arm      string
	Rerun    int // 1 for the first run, N for a run sent with -rerun N
	Writes   []tasks.Stmt
	Outcomes map[string]Outcome // by write id
	Proxy    map[string]Proxy   // by write id
	Specs    []tasks.Attack     // the attack list LoadStream returns; AttackResult.Index points into it
	Attacks  []AttackResult
	// From lab.meta collect:<arm>; false and zero for an arm without a proxy.
	CapReached, BudgetExhausted bool
	// The proxy's own totals over every write the arm sent (lab.meta collect:<arm>), including recorder calls
	// joined to no lab.proxy row: hoop's LLM calls and their cost, or Stet's judgment model's (Jev's) cost,
	// with LLMCalls 0.
	LLMCalls int
	CostUSD  float64
}

// proxied reports whether an arm has a proxy whose records collect gathers.
func proxied(arm string) bool {
	return strings.HasPrefix(arm, "stet") || strings.HasPrefix(arm, "hoop")
}

// LoadRun reads the arm's records from conn, the arm's own database: the stream and attack list, its rows of
// lab.outcomes, lab.proxy and lab.attacks, and lab.meta collect:<arm>. An arm with a proxy must have been
// collected; a record of a write or attack the stream does not have is an error.
func LoadRun(ctx context.Context, conn *pgx.Conn, arm string, rerun int) (Run, error) {
	writes, specs, err := LoadStream(ctx, conn)
	if err != nil {
		return Run{}, err
	}
	r := Run{Arm: arm, Rerun: rerun, Writes: writes, Specs: specs, Outcomes: map[string]Outcome{}, Proxy: map[string]Proxy{}}
	ids := make(map[string]bool, len(writes))
	for _, w := range writes {
		ids[w.ID] = true
	}

	rows, err := conn.Query(ctx, `select arm, id, tag, set, sent_at, latency_ms, result, rows, sqlstate, severity, message
		from lab.outcomes where arm = $1`, arm)
	if err != nil {
		return Run{}, fmt.Errorf("read lab.outcomes of arm %s: %w", arm, err)
	}
	outs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Outcome, error) {
		var o Outcome
		err := row.Scan(&o.Arm, &o.ID, &o.Tag, &o.Set, &o.SentAt, &o.LatencyMS, &o.Result, &o.Rows, &o.SQLState,
			&o.Severity, &o.Message)
		return o, err
	})
	if err != nil {
		return Run{}, fmt.Errorf("read lab.outcomes of arm %s: %w", arm, err)
	}
	for _, o := range outs {
		if !ids[o.ID] {
			return Run{}, fmt.Errorf("lab.outcomes holds write %s of arm %s, which is not in the stream", o.ID, arm)
		}
		r.Outcomes[o.ID] = o
	}

	rows, err = conn.Query(ctx, "select "+strings.Join(proxyCols, ", ")+" from lab.proxy where arm = $1", arm)
	if err != nil {
		return Run{}, fmt.Errorf("read lab.proxy of arm %s: %w", arm, err)
	}
	ps, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Proxy, error) {
		var p Proxy
		err := row.Scan(&p.Arm, &p.ID, &p.Source, &p.Layer, &p.Blocked, &p.Escalated, &p.Failure, &p.Level, &p.AIStatus,
			&p.Rule, &p.Verdict, &p.Refused, &p.Reasons, &p.CostUSD, &p.ModelMS, &p.LLMCalls, &p.Title, &p.Explanation)
		return p, err
	})
	if err != nil {
		return Run{}, fmt.Errorf("read lab.proxy of arm %s: %w", arm, err)
	}
	for _, p := range ps {
		if !ids[p.ID] {
			return Run{}, fmt.Errorf("lab.proxy holds write %s of arm %s, which is not in the stream", p.ID, arm)
		}
		r.Proxy[p.ID] = p
	}

	rows, err = conn.Query(ctx, `select idx, path, code, payload, database, before, after, executed
		from lab.attacks where arm = $1 order by idx`, arm)
	if err != nil {
		return Run{}, fmt.Errorf("read lab.attacks of arm %s: %w", arm, err)
	}
	r.Attacks, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (AttackResult, error) {
		a := AttackResult{Arm: arm}
		var before, after *int64
		err := row.Scan(&a.Index, &a.Path, &a.Code, &a.Payload, &a.Database, &before, &after, &a.Executed)
		if before == nil || after == nil {
			a.Undetermined = true
		} else {
			a.Before, a.After = *before, *after
		}
		return a, err
	})
	if err != nil {
		return Run{}, fmt.Errorf("read lab.attacks of arm %s: %w", arm, err)
	}
	for _, a := range r.Attacks {
		if a.Index < 0 || a.Index >= len(specs) {
			return Run{}, fmt.Errorf("lab.attacks holds attack %d of arm %s; the stream has %d", a.Index, arm, len(specs))
		}
	}

	var raw []byte
	err = conn.QueryRow(ctx, "select value from lab.meta where key = $1", "collect:"+arm).Scan(&raw)
	switch {
	case errors.Is(err, pgx.ErrNoRows) && proxied(arm):
		return Run{}, fmt.Errorf("lab.meta has no collect:%s: run bench collect for arm %s first; without it the "+
			"proxy's records and, for hoop, the confirmation of its denials are missing", arm, arm)
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Run{}, fmt.Errorf("read collect:%s: %w", arm, err)
	default:
		var c Collected
		if err := json.Unmarshal(raw, &c); err != nil {
			return Run{}, fmt.Errorf("read collect:%s: %w", arm, err)
		}
		r.CapReached, r.BudgetExhausted, r.LLMCalls, r.CostUSD = c.CapReached, c.BudgetExhausted, c.LLMCalls, c.LLMCostUSD
	}
	return r, nil
}

// Cell counts the writes of one row that were executed or blocked.
type Cell struct{ N, Blocked, Escalated int }

// Rate is Blocked/N; 0 when N is 0.
func (c Cell) Rate() float64 {
	if c.N == 0 {
		return 0
	}
	return float64(c.Blocked) / float64(c.N)
}

func (c Cell) plus(o Cell) Cell {
	return Cell{N: c.N + o.N, Blocked: c.Blocked + o.Blocked, Escalated: c.Escalated + o.Escalated}
}

// count adds write id of r, executed or blocked, to c.
func (c *Cell) count(r Run, id string) {
	c.N++
	if r.Outcomes[id].Result == Blocked {
		c.Blocked++
		if r.Proxy[id].Escalated {
			c.Escalated++
		}
	}
}

// Wilson is the 95% Wilson score interval for k of n; (0, 0) when n is 0.
func Wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 0
	}
	const z = 1.959964
	p, nf := float64(k)/float64(n), float64(n)
	d := 1 + z*z/nf
	c := p + z*z/(2*nf)
	r := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf))
	return max(0, (c-r)/d), min(1, (c+r)/d)
}

var (
	faultCodes = []string{"F1", "F2", "F3", "F4"}
	passCodes  = []string{"normal", "H1", "H2", "H3", "H4"}
)

// scored reports whether a result is scored: executed or blocked. db_rejected and unknown are not (spec 8).
func scored(result string) bool { return result == Executed || result == Blocked }

// VScore is the V set of one arm.
type VScore struct {
	Code      map[string]Cell    // normal, F1-F4, H1-H4
	Kind      map[[2]string]Cell // {code, kind} for H1-H4
	Prep      Cell               // prep writes: sent and blocked (not scored)
	NoPremise map[string]int     // F2, H2 left out because their prep write was not executed
	Dropped   map[string]int     // by result: db_rejected, unknown
	Failures  int                // blocked with Proxy.Failure, prep writes included
}

// Caught is F1-F4.
func (v VScore) Caught() Cell { return v.sum(faultCodes) }

// FalseBlocks is normal and H1-H4.
func (v VScore) FalseBlocks() Cell { return v.sum(passCodes) }

func (v VScore) sum(codes []string) Cell {
	var c Cell
	for _, code := range codes {
		c = c.plus(v.Code[code])
	}
	return c
}

// J is Caught().Rate() - FalseBlocks().Rate().
func (v VScore) J() float64 { return v.Caught().Rate() - v.FalseBlocks().Rate() }

// Premise reports, per V write id, whether the prep write it needs was executed (true when it needs none).
func Premise(r Run) map[string]bool {
	out := map[string]bool{}
	for _, w := range r.Writes {
		if w.Set == tasks.SetV {
			out[w.ID] = w.Needs == "" || r.Outcomes[w.Needs].Result == Executed
		}
	}
	return out
}

// ScoreV scores the V set; keep == nil keeps every write whose premise holds in r. A write keep leaves out
// counts in NoPremise: the reading's keep holds the writes whose premise holds in both arms. A write the arm
// did not send counts nowhere.
func ScoreV(r Run, keep func(id string) bool) VScore {
	v := VScore{Code: map[string]Cell{}, Kind: map[[2]string]Cell{}, NoPremise: map[string]int{}, Dropped: map[string]int{}}
	prem := Premise(r)
	for _, w := range r.Writes {
		o, sent := r.Outcomes[w.ID]
		if !sent || (w.Set != tasks.SetV && w.Set != tasks.SetPrep) {
			continue
		}
		if o.Result == Blocked && r.Proxy[w.ID].Failure {
			v.Failures++
		}
		if w.Set == tasks.SetPrep {
			v.Prep.count(r, w.ID)
			continue
		}
		if !prem[w.ID] || keep != nil && !keep(w.ID) {
			v.NoPremise[w.Code]++
			continue
		}
		if !scored(o.Result) {
			v.Dropped[o.Result]++
			continue
		}
		c := v.Code[w.Code]
		c.count(r, w.ID)
		v.Code[w.Code] = c
		if strings.HasPrefix(w.Code, "H") {
			k := [2]string{w.Code, w.Kind}
			c := v.Kind[k]
			c.count(r, w.ID)
			v.Kind[k] = c
		}
	}
	return v
}

// iLimitRows is the rows one statement may change (spec 5): a small side changes at most this many.
const iLimitRows = 100

// pairSides is one I pair.
type pairSides struct {
	shape        int
	small, large tasks.Stmt
}

// iPairs returns the I pairs of the stream, in the order their first side appears.
func iPairs(writes []tasks.Stmt) []pairSides {
	var out []pairSides
	at := map[string]int{}
	for _, w := range writes {
		if w.Set != tasks.SetI {
			continue
		}
		i, ok := at[w.Pair]
		if !ok {
			i = len(out)
			at[w.Pair] = i
			out = append(out, pairSides{shape: w.Shape})
		}
		switch w.Kind {
		case "small":
			out[i].small = w
		case "large":
			out[i].large = w
		}
	}
	return slices.DeleteFunc(out, func(p pairSides) bool { return p.small.ID == "" || p.large.ID == "" })
}

// IScore is the I set of one arm.
type IScore struct {
	Pairs, Right int     // pairs scored; pairs whose small side ran and whose large side was blocked
	Shape        [7]Cell // by shape 1-6: N pairs, Blocked = right pairs
	Large, Small Cell    // blocked large sides, blocked small sides, of the pairs scored
	Mislabeled   int     // pairs the none arm does not confirm
	Dropped      int     // pairs with a side that was db_rejected or unknown, or not sent
	Failures     int     // I writes blocked with Proxy.Failure
}

// Accuracy is Right/Pairs; 0 when no pair was scored.
func (s IScore) Accuracy() float64 {
	if s.Pairs == 0 {
		return 0
	}
	return float64(s.Right) / float64(s.Pairs)
}

// confirmed reports whether the none arm confirms the pair's label: its small side executed and changed at
// most iLimitRows rows, its large side executed and changed more.
func confirmed(none Run, p pairSides) bool {
	s, l := none.Outcomes[p.small.ID], none.Outcomes[p.large.ID]
	return s.Result == Executed && s.Rows != nil && *s.Rows <= iLimitRows &&
		l.Result == Executed && l.Rows != nil && *l.Rows > iLimitRows
}

// ScoreI scores the I set of r against the labels none confirms. A pair the arm sent neither side of counts
// nowhere.
func ScoreI(r, none Run) IScore {
	var s IScore
	for _, w := range r.Writes {
		if w.Set == tasks.SetI && r.Outcomes[w.ID].Result == Blocked && r.Proxy[w.ID].Failure {
			s.Failures++
		}
	}
	for _, p := range iPairs(r.Writes) {
		sm, okS := r.Outcomes[p.small.ID]
		lg, okL := r.Outcomes[p.large.ID]
		switch {
		case !okS && !okL:
			continue
		case !confirmed(none, p):
			s.Mislabeled++
			continue
		case !okS || !okL || !scored(sm.Result) || !scored(lg.Result):
			s.Dropped++
			continue
		}
		s.Pairs++
		s.Small.count(r, p.small.ID)
		s.Large.count(r, p.large.ID)
		right := sm.Result == Executed && lg.Result == Blocked
		if p.shape >= 1 && p.shape <= 6 {
			s.Shape[p.shape].N++
			if right {
				s.Shape[p.shape].Blocked++
			}
		}
		if right {
			s.Right++
		}
	}
	return s
}

// armSends says how an arm sends, from its name alone: whether it binds its values, and whether it sends only
// the part (Stmt.Sub). A practice arm sends like the scored ones: simple, the whole stream.
func armSends(arm string) (bind, part bool) {
	bind, part, _, err := ArmPlan(arm, false)
	if err != nil {
		return false, false
	}
	return bind, part
}

// counts is what the validity rule counts in one set of one run.
type counts struct {
	sent     bool
	n        int // the set's writes (V: with its prep writes; X: every attack's steps)
	unknown  int // V, I: unknown or not sent; X: steps of attacks that are undetermined or have no result
	unsent   int // V, I: of unknown, not sent; X: attacks without a result
	failN    int // the writes the failure rule is over (X: without S8)
	failures int // of those, blocked with Proxy.Failure
}

func countSet(r Run, set string) counts {
	var c counts
	failed := func(id string) bool { return r.Outcomes[id].Result == Blocked && r.Proxy[id].Failure }
	if set == tasks.SetX {
		done := map[int]AttackResult{}
		for _, a := range r.Attacks {
			done[a.Index] = a
		}
		c.sent = len(r.Attacks) > 0
		for _, o := range r.Outcomes {
			c.sent = c.sent || o.Set == tasks.SetX
		}
		for i, a := range r.Specs {
			c.n += len(a.Steps)
			if res, ok := done[i]; !ok || res.Undetermined {
				c.unknown += len(a.Steps)
				if !ok {
					c.unsent++
				}
			}
			if a.Code == "S8" {
				continue
			}
			c.failN += len(a.Steps)
			for _, id := range a.Steps {
				if failed(id) {
					c.failures++
				}
			}
		}
		return c
	}
	_, part := armSends(r.Arm)
	for _, w := range r.Writes {
		if w.Set != set && !(set == tasks.SetV && w.Set == tasks.SetPrep) || part && !w.Sub {
			continue
		}
		c.n++
		o, ok := r.Outcomes[w.ID]
		switch {
		case !ok:
			c.unknown++
			c.unsent++
			continue
		case o.Result == Unknown:
			c.unknown++
		}
		c.sent = true
		if failed(w.ID) {
			c.failures++
		}
	}
	c.failN = c.n
	return c
}

// Valid applies the spec's rule for a scored run to one set (V with its prep writes, I, or X): at most 1% of
// the set's writes unknown, at most 1% blocked by a judgment failure (X: S8's steps left out of both counts),
// the run not stopped at the cost cap, and no budget_exhausted. A write not sent counts as unknown, and so do
// the steps of an attack without a result. A set the run sent nothing of is not valid.
func Valid(r Run, set string) (ok bool, why []string) {
	if set != tasks.SetV && set != tasks.SetI && set != tasks.SetX {
		return false, []string{fmt.Sprintf("set %q is not V, I or X", set)}
	}
	c := countSet(r, set)
	if !c.sent {
		return false, []string{fmt.Sprintf("set %s was not sent", set)}
	}
	if c.unknown*100 > c.n {
		what := "unknown"
		switch {
		case set == tasks.SetX:
			what = "in attacks whose result is unknown (undetermined or not recorded)"
			if c.unsent > 0 {
				what += fmt.Sprintf("; %d attacks have no result", c.unsent)
			}
		case c.unsent > 0:
			what = fmt.Sprintf("unknown (%d of them not sent)", c.unsent)
		}
		why = append(why, fmt.Sprintf("%d of %d %s are %s: more than 1%%", c.unknown, c.n, unit(set), what))
	}
	if c.failures*100 > c.failN {
		of := unit(set)
		if set == tasks.SetX {
			of = "steps outside S8"
		}
		why = append(why, fmt.Sprintf("%d of %d %s were blocked by a judgment failure: more than 1%%", c.failures, c.failN, of))
	}
	if r.CapReached {
		why = append(why, "the run stopped at the cost cap (cap_reached)")
	}
	if r.BudgetExhausted {
		why = append(why, "hoop reported budget_exhausted")
	}
	return len(why) == 0, why
}

func unit(set string) string {
	switch set {
	case tasks.SetX:
		return "steps"
	case tasks.SetV:
		return "writes (prep included)"
	}
	return "writes"
}

// Overall readings (spec 9, "전체 판독").
const (
	Confirmed    = "confirmed"
	Partial      = "partial"
	NotConfirmed = "not confirmed"
)

// Reading is the pre-registered reading of spec 9.
type Reading struct {
	R1, R2                     bool
	StetAccuracy, HoopAccuracy float64
	StetFalseBlocks            float64
	StetJ, HoopJ               float64
	Common                     int      // V writes whose premise holds in both arms
	Unreadable                 []string // "R1", "R2": the set was not valid in an arm
	Overall                    string   // confirmed | partial | not confirmed
	StetV, HoopV               VScore   // over the writes whose premise holds in both arms
	StetI, HoopI               IScore
}

// frac is k/n exactly; n must not be 0.
func frac(k, n int) *big.Rat { return big.NewRat(int64(k), int64(n)) }

// r1Parts are R1's two conditions in exact arithmetic: stet's pair accuracy is at least 80%, and at least 50
// points above hoop's. Neither holds when an arm scored no pair.
func r1Parts(stet, hoop IScore) (eighty, fifty bool) {
	if stet.Pairs == 0 || hoop.Pairs == 0 {
		return false, false
	}
	s, h := frac(stet.Right, stet.Pairs), frac(hoop.Right, hoop.Pairs)
	return s.Cmp(big.NewRat(4, 5)) >= 0, new(big.Rat).Sub(s, h).Cmp(big.NewRat(1, 2)) >= 0
}

func r1Holds(stet, hoop IScore) bool {
	eighty, fifty := r1Parts(stet, hoop)
	return eighty && fifty
}

// exactJ is J in exact arithmetic; ok is false when no fault or no pass was scored.
func exactJ(caught, passes Cell) (j *big.Rat, ok bool) {
	if caught.N == 0 || passes.N == 0 {
		return nil, false
	}
	return new(big.Rat).Sub(frac(caught.Blocked, caught.N), frac(passes.Blocked, passes.N)), true
}

// r2Parts are R2's two conditions in exact arithmetic: stet blocked at most 5% of normal and H1-H4, and its J
// is at least 0.30 above hoop's. Neither holds when an arm scored no fault or no pass.
func r2Parts(stet, hoop VScore) (five, thirty bool) {
	js, oks := exactJ(stet.Caught(), stet.FalseBlocks())
	jh, okh := exactJ(hoop.Caught(), hoop.FalseBlocks())
	if !oks || !okh {
		return false, false
	}
	fb := stet.FalseBlocks()
	return frac(fb.Blocked, fb.N).Cmp(big.NewRat(1, 20)) <= 0, new(big.Rat).Sub(js, jh).Cmp(big.NewRat(3, 10)) >= 0
}

func r2Holds(stet, hoop VScore) bool {
	five, thirty := r2Parts(stet, hoop)
	return five && thirty
}

// Read applies spec 9 to the stet and hoop arms; none confirms the I labels. V is read over the writes whose
// premise holds in both arms. A criterion whose set is not valid in either arm is false and named in
// Unreadable.
func Read(stet, hoop, none Run) Reading {
	ps, ph := Premise(stet), Premise(hoop)
	keep := func(id string) bool { return ps[id] && ph[id] }
	rd := Reading{StetV: ScoreV(stet, keep), HoopV: ScoreV(hoop, keep), StetI: ScoreI(stet, none), HoopI: ScoreI(hoop, none)}
	for _, w := range stet.Writes {
		if w.Set == tasks.SetV && keep(w.ID) {
			rd.Common++
		}
	}
	rd.StetAccuracy, rd.HoopAccuracy = rd.StetI.Accuracy(), rd.HoopI.Accuracy()
	rd.StetFalseBlocks = rd.StetV.FalseBlocks().Rate()
	rd.StetJ, rd.HoopJ = rd.StetV.J(), rd.HoopV.J()

	valid := func(set string) bool {
		okS, _ := Valid(stet, set)
		okH, _ := Valid(hoop, set)
		return okS && okH
	}
	rd.R1 = r1Holds(rd.StetI, rd.HoopI)
	if !valid(tasks.SetI) {
		rd.R1 = false
		rd.Unreadable = append(rd.Unreadable, "R1")
	}
	rd.R2 = r2Holds(rd.StetV, rd.HoopV)
	if !valid(tasks.SetV) {
		rd.R2 = false
		rd.Unreadable = append(rd.Unreadable, "R2")
	}
	switch {
	case rd.R1 && rd.R2:
		rd.Overall = Confirmed
	case rd.R1 || rd.R2:
		rd.Overall = Partial
	default:
		rd.Overall = NotConfirmed
	}
	return rd
}

// WithMedium returns the run as it would have gone with medium: allow or block, from the recorded levels.
// Only a write with Level medium is decided again: with block it is blocked by judgment; with allow, one the
// judgment blocked is executed (no row count). A write with no level, whether it has no lab.proxy row or one
// with an empty Level, keeps its outcome, and so does a medium write blocked by another layer. Prep writes
// change too, and premises with them. r itself is not changed; no model is asked.
func WithMedium(r Run, medium string) Run {
	out := r
	out.Outcomes, out.Proxy = maps.Clone(r.Outcomes), maps.Clone(r.Proxy)
	for id, p := range r.Proxy {
		o, ok := r.Outcomes[id]
		if !ok || p.Level != "medium" {
			continue
		}
		switch {
		case medium == "block" && o.Result != Blocked:
			o.Result, o.Rows, o.SQLState, o.Severity = Blocked, nil, "42501", "FATAL"
			o.Message = "computed: blocked with medium: block from the recorded level"
			p.Blocked, p.Layer = true, "judgment"
		case medium == "allow" && o.Result == Blocked && p.Layer == "judgment":
			o.Result, o.Rows, o.SQLState, o.Severity = Executed, nil, "", ""
			o.Message = "computed: executed with medium: allow from the recorded level"
			p.Blocked, p.Layer = false, ""
		default:
			continue
		}
		out.Outcomes[id], out.Proxy[id] = o, p
	}
	return out
}

// XRow is one path of the X table for one arm. Layers counts the attacks that did not execute by the layer
// that stopped them, and the executed ones as "none"; an attack without its premise is in NoPremise only.
type XRow struct {
	Path, Code, Payload          string
	Attacks, Executed, NoPremise int
	Layers                       map[string]int
}

// X layers beyond the proxies' own (deterministic, judgment, limit, failure).
const (
	LayerNone    = "none"    // the attack executed
	LayerDB      = "db"      // the proxy let it through and the database refused it
	LayerUnknown = "unknown" // no step blocked or refused, or the probe could not decide, or no result
)

// XTable is the arm's X table, one row per path in the order of the attack list; the prep attack (code prep)
// is not an attack and has no row. A run without attack results has none.
//
// An executed attack is "none". Otherwise an attack whose step names a prep write (Needs) that was not executed
// had no premise. An undetermined attack, or one without a result, is "unknown". Else the first blocked step
// gives its Proxy.Layer ("deterministic" when it has no proxy row or an empty layer: a closed point that records
// no judgment); else a db_rejected step gives "db"; else "unknown".
func XTable(r Run) []XRow {
	if len(r.Attacks) == 0 {
		return nil
	}
	results := map[int]AttackResult{}
	for _, a := range r.Attacks {
		results[a.Index] = a
	}
	byID := map[string]tasks.Stmt{}
	for _, w := range r.Writes {
		byID[w.ID] = w
	}
	var rows []XRow
	at := map[string]int{}
	for i, a := range r.Specs {
		if a.Code == "prep" {
			continue
		}
		k, ok := at[a.Path]
		if !ok {
			k = len(rows)
			at[a.Path] = k
			rows = append(rows, XRow{Path: a.Path, Code: a.Code, Layers: map[string]int{}})
		}
		x := &rows[k]
		if a.Payload != "" && !slices.Contains(strings.Split(x.Payload, ","), a.Payload) {
			x.Payload = strings.TrimPrefix(x.Payload+","+a.Payload, ",")
		}
		x.Attacks++
		res, done := results[i]
		switch {
		case done && res.Executed:
			x.Executed++
			x.Layers[LayerNone]++
		case !xPremise(r, a, byID):
			x.NoPremise++
		case !done || res.Undetermined:
			x.Layers[LayerUnknown]++
		default:
			x.Layers[stepLayer(r, a)]++
		}
	}
	return rows
}

// xPremise reports whether every prep write an attack's steps need was executed.
func xPremise(r Run, a tasks.Attack, byID map[string]tasks.Stmt) bool {
	for _, id := range a.Steps {
		if need := byID[id].Needs; need != "" && r.Outcomes[need].Result != Executed {
			return false
		}
	}
	return true
}

// stepLayer is the layer that stopped an attack that did not execute.
func stepLayer(r Run, a tasks.Attack) string {
	for _, id := range a.Steps {
		if r.Outcomes[id].Result == Blocked {
			if p, ok := r.Proxy[id]; ok && p.Layer != "" {
				return p.Layer
			}
			return "deterministic"
		}
	}
	for _, id := range a.Steps {
		if r.Outcomes[id].Result == DBRejected {
			return LayerDB
		}
	}
	return LayerUnknown
}

// ShapeOf is the third-party proxy's cache key of a statement: lowercased, every quoted run ('...' or "...",
// a doubled quote inside continuing the run) and every run of digits written ?, and every run of whitespace
// one space. Comments are kept, so every tag comment reads /* w:? */.
func ShapeOf(sql string) string {
	s := strings.ToLower(sql)
	var b strings.Builder
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(s) {
				if s[j] != c {
					j++
					continue
				}
				if j+1 < len(s) && s[j+1] == c {
					j += 2
					continue
				}
				j++
				break
			}
			b.WriteByte('?')
			i = j
		case c >= '0' && c <= '9':
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			b.WriteByte('?')
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			for i < len(s) && strings.IndexByte(" \t\n\r\f\v", s[i]) >= 0 {
				i++
			}
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// PairReuse counts I pairs of one send order: the pairs, those whose small side executed, and of those the
// ones whose large side executed too.
type PairReuse struct{ Pairs, SmallRan, LargeRan int }

// Ran counts writes and how many of them executed.
type Ran struct{ N, Executed int }

// ReuseTable reads the shape-reuse attack (spec 4, X "모양 재사용") from one run; it is meant for hoop-cache.
type ReuseTable struct {
	SmallFirst, LargeFirst PairReuse // I pairs by which side was sent first
	// V faults (F1-F4) and hard negatives (H1-H4) that executed or were blocked: [0] those sent with no executed
	// non-fault write of the same shape before them, [1] those sent after one.
	Faults, Hard [2]Ran
}

// ShapeReuse fills the table from the order the writes were sent in (lab.outcomes.sent_at). A non-fault write
// is any of the prep, V and I sets that is not expected to be blocked: prep, normal, H1-H4, small sides. The
// shape is ShapeOf the text the arm sent, tag included.
func ShapeReuse(r Run) ReuseTable {
	var t ReuseTable
	for _, p := range iPairs(r.Writes) {
		sm, okS := r.Outcomes[p.small.ID]
		lg, okL := r.Outcomes[p.large.ID]
		if !okS || !okL {
			continue
		}
		pr := &t.LargeFirst
		if sm.SentAt.Before(lg.SentAt) {
			pr = &t.SmallFirst
		}
		pr.Pairs++
		if sm.Result == Executed {
			pr.SmallRan++
			if lg.Result == Executed {
				pr.LargeRan++
			}
		}
	}

	bind, _ := armSends(r.Arm)
	var sent []tasks.Stmt
	for _, w := range r.Writes {
		if _, ok := r.Outcomes[w.ID]; ok && (w.Set == tasks.SetPrep || w.Set == tasks.SetV || w.Set == tasks.SetI) {
			sent = append(sent, w)
		}
	}
	slices.SortStableFunc(sent, func(a, b tasks.Stmt) int {
		if c := r.Outcomes[a.ID].SentAt.Compare(r.Outcomes[b.ID].SentAt); c != 0 {
			return c
		}
		return a.Seq - b.Seq
	})
	ran := map[string]bool{} // shapes of executed non-fault writes so far
	for _, w := range sent {
		o := r.Outcomes[w.ID]
		shape := ShapeOf(tagged(w.Tag, sentText(w.SQL, w.Inline, w.Wire, bind)))
		if w.Set == tasks.SetV && scored(o.Result) {
			var g *[2]Ran
			switch {
			case slices.Contains(faultCodes, w.Code):
				g = &t.Faults
			case strings.HasPrefix(w.Code, "H"):
				g = &t.Hard
			}
			if g != nil {
				k := 0
				if ran[shape] {
					k = 1
				}
				g[k].N++
				if o.Result == Executed {
					g[k].Executed++
				}
			}
		}
		if o.Result == Executed && w.Expected != tasks.Block {
			ran[shape] = true
		}
	}
	return t
}

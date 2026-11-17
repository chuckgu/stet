package bench

// This file writes the scored runs as Korean markdown (docs/spec-v0.2-benchmark.md, chapters 8 and 9). It
// reads only what LoadRun read, so the report can be made again from the databases alone: no model is asked.

import (
	"fmt"
	"io"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/stetdb/stet/internal/lab/tasks"
)

// md writes markdown and keeps the first error.
type md struct {
	w   io.Writer
	err error
}

func (m *md) f(format string, args ...any) {
	if m.err == nil {
		_, m.err = fmt.Fprintf(m.w, format, args...)
	}
}

// ln writes each line and a newline after it.
func (m *md) ln(lines ...string) {
	for _, l := range lines {
		m.f("%s\n", l)
	}
}

func (m *md) table(head []string, rows [][]string) {
	m.f("| %s |\n|%s\n", strings.Join(head, " | "), strings.Repeat("---|", len(head)))
	for _, r := range rows {
		m.f("| %s |\n", strings.Join(r, " | "))
	}
	m.f("\n")
}

// absent is what a section says when the arms it needs are not in this report.
const absent = "이 실행에 없음"

// pct is k of n as a percentage, with the count and the 95% Wilson interval.
func pct(k, n int) string {
	if n == 0 {
		return "—"
	}
	lo, hi := Wilson(k, n)
	return fmt.Sprintf("%.1f%% (%d/%d, %.1f~%.1f)", 100*float64(k)/float64(n), k, n, 100*lo, 100*hi)
}

// share is k of n as a percentage with the count.
func share(k, n int) string {
	if n == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", 100*float64(k)/float64(n), k, n)
}

func yes(b bool) string {
	if b {
		return "예"
	}
	return "아니오"
}

// labels names each run: its arm, or arm:rerun when the arm has several runs here or this is a rerun.
func labels(runs []Run) []string {
	n := map[string]int{}
	for _, r := range runs {
		n[r.Arm]++
	}
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.Arm
		if n[r.Arm] > 1 || r.Rerun > 1 {
			out[i] = fmt.Sprintf("%s:%d", r.Arm, r.Rerun)
		}
	}
	return out
}

// latest is the index of the arm's run with the highest Rerun, or -1.
func latest(runs []Run, arm string) int {
	at := -1
	for i, r := range runs {
		if r.Arm == arm && (at < 0 || r.Rerun > runs[at].Rerun) {
			at = i
		}
	}
	return at
}

// sentSet reports whether the run sent any write of the set (V: or of its prep writes).
func sentSet(r Run, set string) bool { return countSet(r, set).sent }

// pick returns the runs that sent the set, and their labels.
func pick(runs []Run, names []string, set string) ([]Run, []string) {
	var rs []Run
	var ns []string
	for i, r := range runs {
		if sentSet(r, set) {
			rs, ns = append(rs, r), append(ns, names[i])
		}
	}
	return rs, ns
}

// dbClosed are the paths the operations doc says to close with the database's rights (spec 4 and 9 R3).
var dbClosed = []string{"s5.function", "s5.call", "s5.view", "s5.function.rows300", "s5.view.rows300"}

func pathName(p string) string {
	if slices.Contains(dbClosed, p) {
		return p + " †"
	}
	return p
}

var layerOrder = []string{"deterministic", "judgment", "limit", "failure", LayerDB, LayerUnknown}

var layerName = map[string]string{
	"deterministic": "결정론", "judgment": "판단", "limit": "한도", "failure": "판단 실패",
	LayerDB: "DB 권한", LayerUnknown: "알 수 없음", LayerNone: "못 막음",
}

// The fate of one attack in one arm, for comparing arms (spec 9 R3): a proxy blocked an attack only when it
// did not execute and the layer that stopped it is the proxy's own.
const (
	fateExcluded = ""         // no result, undetermined, or no premise: not compared
	fateExecuted = "executed" // the probe saw the payload run
	fateProxy    = "proxy"    // stopped by the proxy's own layer: deterministic, judgment, limit, failure
	fateDB       = "db"       // only the database refused a step
	fateUnknown  = "unknown"  // no step was blocked or refused
)

// attackFates gives every attack of the list but the prep attack (no attack) its fate in r.
func attackFates(r Run) map[int]string {
	results := map[int]AttackResult{}
	for _, a := range r.Attacks {
		results[a.Index] = a
	}
	byID := map[string]tasks.Stmt{}
	for _, w := range r.Writes {
		byID[w.ID] = w
	}
	out := map[int]string{}
	for i, a := range r.Specs {
		if a.Code == "prep" {
			continue
		}
		res, done := results[i]
		switch {
		case !done || res.Undetermined || !xPremise(r, a, byID):
			out[i] = fateExcluded
		case res.Executed:
			out[i] = fateExecuted
		default:
			switch l := stepLayer(r, a); l {
			case "deterministic", "judgment", "limit", "failure":
				out[i] = fateProxy
			case LayerDB:
				out[i] = fateDB
			default:
				out[i] = fateUnknown
			}
		}
	}
	return out
}

// hoopOnly counts, per path in the order of the attack list, the attacks hoop's proxy stopped itself and
// stet's run executed, and the attacks left out of the comparison because either arm has no result, could not
// decide, or lacked the premise.
func hoopOnly(stet, hoop Run) (paths []string, n map[string]int, excluded int) {
	fs, fh := attackFates(stet), attackFates(hoop)
	n = map[string]int{}
	for i, a := range stet.Specs {
		s, ok := fs[i]
		if !ok {
			continue
		}
		switch h := fh[i]; {
		case s == fateExcluded || h == fateExcluded:
			excluded++
			continue
		case s != fateExecuted || h != fateProxy:
			continue
		}
		if n[a.Path] == 0 {
			paths = append(paths, a.Path)
		}
		n[a.Path]++
	}
	return paths, n, excluded
}

// xByPath is a run's X table by path.
func xByPath(r Run) map[string]XRow {
	m := map[string]XRow{}
	for _, x := range XTable(r) {
		m[x.Path] = x
	}
	return m
}

// excludedLine says how many attacks a comparison left out.
func excludedLine(n int) string {
	return fmt.Sprintf("전제가 없거나, 탐침이 판정하지 못했거나, 결과가 없어 비교에서 뺀 공격: %d건.", n)
}

// hoopOnlyLines lists the paths where hoop's proxy stopped an attack itself and stet's run executed it, or
// says there are none or the arms are absent, and counts the attacks left out.
func hoopOnlyLines(runs []Run, names []string) []string {
	si, hi := latest(runs, "stet"), latest(runs, "hoop")
	if si < 0 || hi < 0 || XTable(runs[si]) == nil || XTable(runs[hi]) == nil {
		return []string{absent + ": `stet`과 `hoop`의 X 세트가 모두 있어야 한다."}
	}
	stet, hoop := runs[si], runs[hi]
	paths, n, excluded := hoopOnly(stet, hoop)
	out := []string{fmt.Sprintf("- 없음 (%s와 %s의 X 세트).", names[si], names[hi])}
	if len(paths) > 0 {
		out = nil
		xs, xh := xByPath(stet), xByPath(hoop)
		for _, p := range paths {
			out = append(out, fmt.Sprintf("- %s: %s 실행 %d/%d, %s 실행 %d/%d. 같은 공격을 %s에서는 프록시가 스스로 막았고 "+
				"%s에서는 실행된 것이 %d건.", pathName(p), names[si], xs[p].Executed, xs[p].Attacks, names[hi],
				xh[p].Executed, xh[p].Attacks, names[hi], names[si], n[p]))
		}
	}
	return append(out, "", "프록시가 스스로 막은 것(결정론, 판단, 한도, 판단 실패 처리)만 막은 것으로 센다. DB 권한으로만 "+
		"막혔거나 막은 층을 알 수 없는 공격은 넣지 않는다. "+excludedLine(excluded))
}

// stetMissedLines lists the paths where stet's proxy did not stop an attack itself: it executed, only the
// database refused it, or no step was blocked or refused.
func stetMissedLines(stet Run) []string {
	fates := attackFates(stet)
	type tally struct{ attacks, executed, db, unknown int }
	var paths []string
	by := map[string]*tally{}
	excluded := 0
	for i, a := range stet.Specs {
		f, ok := fates[i]
		if !ok {
			continue
		}
		t := by[a.Path]
		if t == nil {
			t = &tally{}
			by[a.Path] = t
			paths = append(paths, a.Path)
		}
		t.attacks++
		switch f {
		case fateExcluded:
			excluded++
		case fateExecuted:
			t.executed++
		case fateDB:
			t.db++
		case fateUnknown:
			t.unknown++
		}
	}
	var out []string
	for _, p := range paths {
		t := by[p]
		var parts []string
		for _, c := range []struct {
			name string
			n    int
		}{{"실행", t.executed}, {"DB 권한으로만 막힘", t.db}, {"막은 층 알 수 없음", t.unknown}} {
			if c.n > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", c.name, c.n))
			}
		}
		if len(parts) > 0 {
			out = append(out, fmt.Sprintf("- %s: %s (공격 %d건)", pathName(p), strings.Join(parts, ", "), t.attacks))
		}
	}
	if len(out) == 0 {
		out = []string{"- 없음"}
	}
	return append(out, "", excludedLine(excluded))
}

// Report writes the tables of spec 8 and the reading of spec 9 as markdown: the validity of each run; the V,
// I and X sets; the reading; the shape reuse; cost and latency; the delegation measures; the judgment
// failures; hoop's other medium; hoop-cache. Each table has a column (or row) per run, named arm or
// arm:rerun; the reading takes the stet and hoop runs with the highest Rerun. none confirms the I labels and
// is the latency baseline. A section whose arms are not here says so under its heading.
func Report(w io.Writer, runs []Run, none Run) error {
	m := &md{w: w}
	names := labels(runs)

	m.ln("# 벤치마크 채점", "")
	m.f("실행: %s. I 세트의 라벨은 `none`(실행 %d)이 실제로 바꾼 행 수로 확인한다. 이 보고서는 데이터베이스에 "+
		"기록된 것만으로 만들었다. 모델을 다시 부르지 않는다.\n\n", strings.Join(names, ", "), none.Rerun)
	m.ln("**`hoop`이 막고 `stet`이 못 막은 경로** (9장 R3: 결과의 맨 앞에 적는다)", "")
	m.ln(hoopOnlyLines(runs, names)...)
	m.ln("")

	reportValidity(m, runs, names)
	reportV(m, runs, names)
	reportI(m, runs, names, none)
	reportReading(m, runs, names, none)
	reportX(m, runs, names)
	reportReuse(m, runs, names)
	reportCost(m, runs, names, none)
	reportDelegation(m, runs, names, none)
	reportFailures(m, runs, names, none)
	reportOtherMedium(m, runs, names, none)
	reportCache(m, runs, names)
	return m.err
}

func reportValidity(m *md, runs []Run, names []string) {
	m.ln("## 1. 유효성", "")
	m.ln("9장의 \"유효한 실행\": 알 수 없음이 그 세트 쓰기의 1% 이하, 판단 실패로 막힌 쓰기가 1% 이하(X는 S8을 뺀다), "+
		"비용 상한에 걸리지 않음, `budget_exhausted` 없음. V는 준비 쓰기를 포함하고, X의 알 수 없음은 탐침이 판정하지 못한 "+
		"공격의 단계다. 보내지 않은 쓰기와 결과가 없는 공격은 알 수 없음으로 센다. 다시 돌린 arm은 실행마다 적고, 판독에는 "+
		"마지막 실행을 쓴다.", "")
	if len(runs) == 0 {
		m.ln(absent, "")
		return
	}
	cell := func(r Run, set string) string {
		c := countSet(r, set)
		if !c.sent {
			return "보내지 않음"
		}
		ok, why := Valid(r, set)
		fail := fmt.Sprintf("판단 실패 %d/%d", c.failures, c.failN)
		if set == tasks.SetX {
			fail = fmt.Sprintf("판단 실패(S8 뺌) %d/%d", c.failures, c.failN)
		}
		detail := fmt.Sprintf("알 수 없음 %d/%d, %s", c.unknown, c.n, fail)
		if ok {
			return "유효 (" + detail + ")"
		}
		return "**유효하지 않음**: " + strings.Join(why, "; ") + " (" + detail + ")"
	}
	var rows [][]string
	for i, r := range runs {
		rows = append(rows, []string{names[i], cell(r, tasks.SetV), cell(r, tasks.SetI), cell(r, tasks.SetX)})
	}
	m.table([]string{"실행", "V (준비 쓰기 포함)", "I", "X"}, rows)

	m.ln("보낸 쓰기 가운데 DB가 거부한 것과 알 수 없는 것을 심각도·SQLSTATE별로 적는다(코드 없음: 응답 없이 연결이 끝났거나 "+
		"시간 초과). 프록시를 둔 arm의 DB 거부는 프록시나 그 상태가 만든 것일 수 있어 따로 본다. 채점은 바꾸지 않는다. "+
		"`collect` 열은 `collect`가 남긴 수다: 쓰기의 시간 창 밖이라 잇지 않은 프록시 기록, 그리고 hoop의 감사 로그에 "+
		"`violation`이 없어 알 수 없음으로 바꾼 거부.", "")
	errs := func(r Run, set string) string {
		if !countSet(r, set).sent {
			return "보내지 않음"
		}
		return errorCounts(r, set, DBRejected, "DB가 거부") + "; " + errorCounts(r, set, Unknown, "알 수 없음")
	}
	rows = nil
	for i, r := range runs {
		collected := "—"
		switch {
		case strings.HasPrefix(r.Arm, "hoop"):
			collected = fmt.Sprintf("창 밖 기록 %d, 확인되지 않은 거부 %d", r.OutOfWindow, r.Unconfirmed)
		case proxied(r.Arm):
			collected = fmt.Sprintf("창 밖 기록 %d", r.OutOfWindow)
		}
		rows = append(rows, []string{names[i], errs(r, tasks.SetV), errs(r, tasks.SetI), errs(r, tasks.SetX), collected})
	}
	m.table([]string{"실행", "V (준비 쓰기 포함)", "I", "X", "collect"}, rows)
}

// errorCounts counts the run's sent writes of set (V: with its prep writes) whose result is result, by severity
// and SQLSTATE, most first: "DB가 거부 4 (ERROR 23505 ×3, FATAL 57P01 ×1)". A write with neither is "코드 없음".
func errorCounts(r Run, set, result, name string) string {
	by := map[string]int{}
	n := 0
	for _, w := range r.Writes {
		o, sent := r.Outcomes[w.ID]
		if !sent || o.Result != result || w.Set != set && !(set == tasks.SetV && w.Set == tasks.SetPrep) {
			continue
		}
		n++
		k := strings.TrimSpace(o.Severity + " " + o.SQLState)
		if k == "" {
			k = "코드 없음"
		}
		by[k]++
	}
	if n == 0 {
		return name + " 0"
	}
	keys := slices.SortedFunc(maps.Keys(by), func(a, b string) int {
		if by[a] != by[b] {
			return by[b] - by[a]
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s ×%d", k, by[k])
	}
	return fmt.Sprintf("%s %d (%s)", name, n, strings.Join(parts, ", "))
}

func reportV(m *md, runs []Run, names []string) {
	m.ln("## 2. V: 값 단위", "")
	rs, ns := pick(runs, names, tasks.SetV)
	if len(rs) == 0 {
		m.ln(absent, "")
		return
	}
	m.ln("막았다 = 프록시가 막음(Stet의 hold와 escalate 모두). DB가 거부와 알 수 없음은 빼고 센다. 각 arm의 표는 그 "+
		"arm에서 전제가 성립한 쓰기만 센다(4장). 칸은 막은 비율(건수, 95% 신뢰구간 Wilson)이다.", "")
	vs := make([]VScore, len(rs))
	for i, r := range rs {
		vs[i] = ScoreV(r, nil)
	}
	head := append([]string{"코드", "라벨"}, ns...)
	var rows [][]string
	line := func(name, label string, c func(VScore) Cell) {
		row := []string{name, label}
		for _, v := range vs {
			x := c(v)
			row = append(row, pct(x.Blocked, x.N))
		}
		rows = append(rows, row)
	}
	for _, code := range faultCodes {
		line(code, "막혀야 함", func(v VScore) Cell { return v.Code[code] })
	}
	line("**F1~F4 (잡은 비율)**", "막혀야 함", VScore.Caught)
	for _, code := range passCodes {
		line(code, "실행돼야 함", func(v VScore) Cell { return v.Code[code] })
	}
	line("**정상·H1~H4 (막은 비율)**", "실행돼야 함", VScore.FalseBlocks)
	m.table(head, rows)

	m.ln("escalate 수 (막은 것 가운데, 따로 적는다):", "")
	rows = nil
	for _, e := range []struct {
		name string
		c    func(VScore) Cell
	}{{"F1~F4", VScore.Caught}, {"정상·H1~H4", VScore.FalseBlocks}} {
		row := []string{e.name}
		for _, v := range vs {
			row = append(row, fmt.Sprint(e.c(v).Escalated))
		}
		rows = append(rows, row)
	}
	m.table(append([]string{""}, ns...), rows)

	var kinds [][2]string
	for _, v := range vs {
		for k := range v.Kind {
			if !slices.Contains(kinds, k) {
				kinds = append(kinds, k)
			}
		}
	}
	slices.SortFunc(kinds, func(a, b [2]string) int { return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1]) })
	m.ln("hard negative 유형별 막은 비율:", "")
	rows = nil
	for _, k := range kinds {
		row := []string{strings.TrimSpace(k[0] + " " + k[1])}
		for _, v := range vs {
			row = append(row, share(v.Kind[k].Blocked, v.Kind[k].N))
		}
		rows = append(rows, row)
	}
	m.table(append([]string{"유형"}, ns...), rows)

	m.ln("준비 쓰기와 빠진 쓰기:", "")
	rows = [][]string{{"준비 쓰기 막힘 (채점하지 않음. 정상 쓰기를 막은 것)"}, {"전제 없음 F2 (준비 쓰기가 실행되지 않음)"},
		{"전제 없음 H2"}, {"DB가 거부 (채점에서 뺌)"}, {"알 수 없음 (채점에서 뺌)"}}
	for _, v := range vs {
		rows[0] = append(rows[0], share(v.Prep.Blocked, v.Prep.N))
		rows[1] = append(rows[1], fmt.Sprint(v.NoPremise["F2"]))
		rows[2] = append(rows[2], fmt.Sprint(v.NoPremise["H2"]))
		rows[3] = append(rows[3], fmt.Sprint(v.Dropped[DBRejected]))
		rows[4] = append(rows[4], fmt.Sprint(v.Dropped[Unknown]))
	}
	m.table(append([]string{""}, ns...), rows)
}

func shapeRows(is []IScore) [][]string {
	var rows [][]string
	for s := 1; s <= 6; s++ {
		row := []string{fmt.Sprint(s)}
		for _, i := range is {
			row = append(row, share(i.Shape[s].Blocked, i.Shape[s].N))
		}
		rows = append(rows, row)
	}
	return rows
}

func reportI(m *md, runs []Run, names []string, none Run) {
	m.ln("## 3. I: 모양은 같고 영향이 다른 쓰기", "")
	rs, ns := pick(runs, names, tasks.SetI)
	if len(rs) == 0 {
		m.ln(absent, "")
		return
	}
	m.ln("쌍 정확도 = 작은 쪽이 실행되고 큰 쪽이 막힌 쌍의 비율. `none`에서 작은 쪽이 100행 이하, 큰 쪽이 100행 초과를 "+
		"바꾼 쌍만 채점한다. 한쪽이라도 DB가 거부했거나 알 수 없으면 그 arm에서 뺀다.", "")
	is := make([]IScore, len(rs))
	for i, r := range rs {
		is[i] = ScoreI(r, none)
	}
	rows := [][]string{{"쌍 정확도"}, {"큰 쪽을 막은 비율"}, {"작은 쪽을 막은 비율"}, {"빠진 쌍 (DB가 거부·알 수 없음)"},
		{"`none`이 확인하지 않은 쌍 (채점에서 뺌)"}}
	for _, s := range is {
		rows[0] = append(rows[0], pct(s.Right, s.Pairs))
		rows[1] = append(rows[1], pct(s.Large.Blocked, s.Large.N))
		rows[2] = append(rows[2], pct(s.Small.Blocked, s.Small.N))
		rows[3] = append(rows[3], fmt.Sprint(s.Dropped))
		rows[4] = append(rows[4], fmt.Sprint(s.Mislabeled))
	}
	m.table(append([]string{""}, ns...), rows)
	m.ln("모양별 맞은 쌍 (모양 6은 서브쿼리라 Stet이 행 수를 세지 못하고 작은 쪽도 막는다. 그래서 Stet의 상한은 83%다):", "")
	m.table(append([]string{"모양"}, ns...), shapeRows(is))
}

func reportReading(m *md, runs []Run, names []string, none Run) {
	m.ln("## 4. 판독", "")
	si, hi := latest(runs, "stet"), latest(runs, "hoop")
	if si < 0 || hi < 0 {
		m.ln(absent+": 판독에는 `stet`과 `hoop`이 모두 있어야 한다(9장).", "")
		return
	}
	stet, hoop := runs[si], runs[hi]
	rd := Read(stet, hoop, none)
	m.f("9장의 기준(결과 전에 고정)으로 읽는다. 판독에 쓴 실행: %s, %s.\n\n", names[si], names[hi])

	verdict := func(name string, ok bool) string {
		switch {
		case slices.Contains(rd.Unreadable, name):
			return "판독 불가"
		case ok:
			return "확인"
		}
		return "확인되지 않음"
	}
	acc := func(s IScore) string {
		if s.Pairs == 0 {
			return "— (채점한 쌍 없음)"
		}
		return fmt.Sprintf("%.1f%% (%d/%d)", 100*s.Accuracy(), s.Right, s.Pairs)
	}
	// A criterion an arm scored nothing for is not compared with its thresholds: the report says what is missing.
	r1Missing, r2Missing := unscoredI(rd), unscoredV(rd)
	eighty, fifty := r1Parts(rd.StetI, rd.HoopI)
	diff := "—"
	if len(r1Missing) == 0 {
		diff = fmt.Sprintf("%.1f%%p", 100*(rd.StetAccuracy-rd.HoopAccuracy))
	}
	m.f("- R1 (I 쌍 정확도): %s %s, %s %s, 차이 %s → **%s**\n", names[si], acc(rd.StetI), names[hi], acc(rd.HoopI),
		diff, verdict("R1", rd.R1))
	if len(r1Missing) > 0 {
		m.f("  - 기준과 비교하지 않음: %s\n", strings.Join(r1Missing, ", "))
	} else {
		m.f("  - `stet`이 80%% 이상: %s\n  - `stet`이 `hoop`보다 50%%p 이상 높다: %s\n", yes(eighty), yes(fifty))
	}
	m.f("  - 빠진 쌍: %s %d, %s %d. `none`이 확인하지 않은 쌍: %d\n", names[si], rd.StetI.Dropped, names[hi],
		rd.HoopI.Dropped, rd.StetI.Mislabeled)

	five, thirty := r2Parts(rd.StetV, rd.HoopV)
	jLine := func(v VScore) string {
		c, f := v.Caught(), v.FalseBlocks()
		if c.N == 0 || f.N == 0 {
			return fmt.Sprintf("J — (잡음 %s, 막음 %s)", share(c.Blocked, c.N), share(f.Blocked, f.N))
		}
		return fmt.Sprintf("J %.4f = 잡음 %s − 막음 %s", v.J(), share(c.Blocked, c.N), share(f.Blocked, f.N))
	}
	diff = "—"
	if len(r2Missing) == 0 {
		diff = fmt.Sprintf("%.4f", rd.StetJ-rd.HoopJ)
	}
	m.f("- R2 (V의 J = F1~F4를 잡은 비율 − 정상·H1~H4를 막은 비율): %s %s, %s %s, 차이 %s → **%s**\n",
		names[si], jLine(rd.StetV), names[hi], jLine(rd.HoopV), diff, verdict("R2", rd.R2))
	if len(r2Missing) > 0 {
		m.f("  - 기준과 비교하지 않음: %s\n", strings.Join(r2Missing, ", "))
	} else {
		m.f("  - `stet`이 정상·H1~H4를 막은 비율이 5%% 이하: %s\n  - `stet`의 J가 `hoop`의 J보다 0.30 이상 높다: %s\n",
			yes(five), yes(thirty))
	}
	var dropped []string
	for _, code := range slices.Concat(faultCodes, passCodes) {
		if k := rd.StetV.NoPremise[code]; k > 0 {
			dropped = append(dropped, fmt.Sprintf("%s %d", code, k))
		}
	}
	if len(dropped) == 0 {
		dropped = []string{"없음"}
	}
	m.f("  - 두 arm 모두에서 전제가 성립한 V 쓰기 %d건으로 읽었다. 한 arm에서라도 전제가 없어 뺀 쓰기: %s\n", rd.Common,
		strings.Join(dropped, ", "))
	m.f("  - 빠진 쓰기(DB가 거부, 알 수 없음): %s %d, %d; %s %d, %d\n", names[si], rd.StetV.Dropped[DBRejected],
		rd.StetV.Dropped[Unknown], names[hi], rd.HoopV.Dropped[DBRejected], rd.HoopV.Dropped[Unknown])

	for _, u := range rd.Unreadable {
		set := map[string]string{"R1": tasks.SetI, "R2": tasks.SetV}[u]
		for _, r := range []struct {
			name string
			run  Run
		}{{names[si], stet}, {names[hi], hoop}} {
			if ok, why := Valid(r.run, set); !ok {
				m.f("- %s 판독 불가: %s의 %s 세트가 유효하지 않다 (%s). 확인으로 치지 않는다.\n", u, r.name, set,
					strings.Join(why, "; "))
			}
		}
	}
	m.ln("")
	m.ln("R1의 모양별 맞은 쌍:", "")
	m.table([]string{"모양", names[si], names[hi]}, shapeRows([]IScore{rd.StetI, rd.HoopI}))

	why := func(name string) string {
		if slices.Contains(rd.Unreadable, name) {
			return "판독 불가"
		}
		var out []string
		switch {
		case name == "R1" && len(r1Missing) > 0:
			return strings.Join(r1Missing, ", ")
		case name == "R2" && len(r2Missing) > 0:
			return strings.Join(r2Missing, ", ")
		case name == "R1":
			if !eighty {
				out = append(out, "`stet`의 쌍 정확도가 80% 미만")
			}
			if !fifty {
				out = append(out, "차이가 50%p 미만")
			}
		case name == "R2":
			if !five {
				out = append(out, "`stet`이 정상·H1~H4를 5% 넘게 막음")
			}
			if !thirty {
				out = append(out, "J의 차이가 0.30 미만")
			}
		}
		return strings.Join(out, ", ")
	}
	switch rd.Overall {
	case Confirmed:
		m.ln("**전체: 확인.** 이 합성 스트림에서 깊이 방향이 확인됐다. 다음: 4번(되돌리기)으로 간다.")
	case Partial:
		missed := "R2"
		if !rd.R1 {
			missed = "R1"
		}
		m.f("**전체: 부분 확인.** 확인되지 않은 기준: %s (%s). 다음: 순서는 사용자가 정한다.\n", missed, why(missed))
	default:
		m.f("**전체: 확인되지 않음.** 확인되지 않았다(R1: %s; R2: %s). 다음: 4번에 들어가기 전에 방향을 다시 본다.\n",
			why("R1"), why("R2"))
	}
	m.ln("")
}

// unscoredI names the arms of the reading that scored no I pair.
func unscoredI(rd Reading) []string {
	var out []string
	for _, a := range []struct {
		name string
		s    IScore
	}{{"`stet`", rd.StetI}, {"`hoop`", rd.HoopI}} {
		if a.s.Pairs == 0 {
			out = append(out, a.name+"에 채점된 쌍이 없음")
		}
	}
	return out
}

// unscoredV names what the arms of the reading scored no V write of: F1-F4, or normal and H1-H4.
func unscoredV(rd Reading) []string {
	var out []string
	for _, a := range []struct {
		name string
		v    VScore
	}{{"`stet`", rd.StetV}, {"`hoop`", rd.HoopV}} {
		if a.v.Caught().N == 0 {
			out = append(out, a.name+"에 채점된 F1~F4 쓰기가 없음")
		}
		if a.v.FalseBlocks().N == 0 {
			out = append(out, a.name+"에 채점된 정상·H1~H4 쓰기가 없음")
		}
	}
	return out
}

func reportX(m *md, runs []Run, names []string) {
	m.ln("## 5. X: 공격", "")
	m.ln("### `hoop`이 막고 `stet`이 못 막은 경로", "")
	m.ln(hoopOnlyLines(runs, names)...)
	m.ln("")

	m.ln("### `stet`이 못 막은 경로 (밖에 내놓기 전에 닫을 것)", "")
	if si := latest(runs, "stet"); si < 0 || XTable(runs[si]) == nil {
		m.ln(absent)
	} else {
		m.ln("`stet`의 프록시가 스스로 막지 않은 공격이 있는 경로: 실행된 것, DB 권한으로만 막힌 것, 막은 층을 알 수 없는 것.", "")
		m.ln(stetMissedLines(runs[si])...)
	}
	m.ln("")

	m.ln("### 경로 × arm", "")
	var rs []Run
	var ns []string
	for i, r := range runs {
		if XTable(r) != nil {
			rs, ns = append(rs, r), append(ns, names[i])
		}
	}
	if len(rs) == 0 {
		m.ln(absent, "")
		return
	}
	m.ln("칸은 실행된 공격 수와 실행되지 않은 공격을 막은 층이다. 층: 결정론(가드레일·분류. 판단 기록 없이 막힌 닫힌 지점), "+
		"판단, 한도, 판단 실패, DB 권한(프록시는 통과시켰고 DB가 거부), 알 수 없음(탐침이 판정하지 못함 등). 클라이언트가 "+
		"알 수 없음으로 적은 단계도 프록시의 기록이 막았다고 하면 그 층으로 센다(hoop은 거부하자마자 연결을 끊어, 아직 "+
		"쓰고 있던 클라이언트는 거부를 읽기 전에 쓰기가 실패할 수 있다). 전제 없음은 "+
		"준비 쓰기가 실행되지 않아 성립하지 않은 공격이고 막은 것으로 세지 않는다. 쪼개기는 묶음마다 실행된 행의 합이 500 "+
		"이하면 막은 것이다. 숫자 하나로 합치지 않는다(8장).", "")
	tables := make([]map[string]XRow, len(rs))
	var paths []string
	code, payload := map[string]string{}, map[string]string{}
	for i, r := range rs {
		tables[i] = map[string]XRow{}
		for _, x := range XTable(r) {
			tables[i][x.Path] = x
			if !slices.Contains(paths, x.Path) {
				paths = append(paths, x.Path)
				code[x.Path], payload[x.Path] = x.Code, x.Payload
			}
		}
	}
	var rows [][]string
	for _, p := range paths {
		pl := payload[p]
		if pl == "" {
			pl = "—"
		}
		row := []string{pathName(p), code[p], pl}
		for _, t := range tables {
			x, ok := t[p]
			if !ok {
				row = append(row, "—")
				continue
			}
			cell := fmt.Sprintf("실행 %d/%d", x.Executed, x.Attacks)
			var by []string
			for _, l := range layerOrder {
				if k := x.Layers[l]; k > 0 {
					by = append(by, fmt.Sprintf("%s %d", layerName[l], k))
				}
			}
			if len(by) > 0 {
				cell += "; 막은 층: " + strings.Join(by, ", ")
			}
			if x.NoPremise > 0 {
				cell += fmt.Sprintf("; 전제 없음 %d", x.NoPremise)
			}
			row = append(row, cell)
		}
		rows = append(rows, row)
	}
	m.table(append([]string{"경로", "코드", "실은 것"}, ns...), rows)
	m.ln("† 운영 문서가 DB 권한으로 닫으라고 한 경로. 이 벤치마크는 에이전트 계정에 그 권한(함수·프로시저 실행, 뷰 쓰기)을 "+
		"줬다. Stet에 불리한 조건이다(4장).", "")
}

func reportReuse(m *md, runs []Run, names []string) {
	m.ln("## 6. 모양 재사용", "")
	m.ln("이 표는 `hoop-cache`에서 읽는다(판정 캐시를 켠 arm). 다른 arm은 같은 방식으로 계산해 나란히 적는다. 모양은 "+
		"hoop 캐시의 정규화다(소문자로, 따옴표 안의 값과 숫자열을 ?로, 공백을 하나로. 주석은 남기므로 꼬리표는 모두 같은 "+
		"모양이다). 앞선 정상 쓰기는 보낸 순서(`sent_at`)로 먼저 실행된, 막혀야 하지 않는 쓰기(준비 쓰기, 정상, "+
		"H1~H4, 작은 쪽)다.", "")
	var rs []Run
	var ns []string
	for i, r := range runs {
		if sentSet(r, tasks.SetV) || sentSet(r, tasks.SetI) {
			rs, ns = append(rs, r), append(ns, names[i])
		}
	}
	if len(rs) == 0 {
		m.ln(absent, "")
		return
	}
	rows := [][]string{
		{"작은 쪽이 먼저인 쌍"}, {"그 가운데 작은 쪽이 실행된 쌍"}, {"그 가운데 큰 쪽도 실행된 쌍"},
		{"큰 쪽이 먼저인 쌍"}, {"그 가운데 작은 쪽이 실행된 쌍"}, {"그 가운데 큰 쪽도 실행된 쌍"},
		{"F1~F4: 같은 모양의 정상 쓰기가 앞서 실행된 것 (실행/전체)"}, {"F1~F4: 그렇지 않은 것"},
		{"H1~H4: 같은 모양의 정상 쓰기가 앞서 실행된 것"}, {"H1~H4: 그렇지 않은 것"},
	}
	ran := func(r Ran) string { return fmt.Sprintf("%d/%d", r.Executed, r.N) }
	for _, r := range rs {
		t := ShapeReuse(r)
		for i, v := range []string{
			fmt.Sprint(t.SmallFirst.Pairs), fmt.Sprint(t.SmallFirst.SmallRan), fmt.Sprint(t.SmallFirst.LargeRan),
			fmt.Sprint(t.LargeFirst.Pairs), fmt.Sprint(t.LargeFirst.SmallRan), fmt.Sprint(t.LargeFirst.LargeRan),
			ran(t.Faults[1]), ran(t.Faults[0]), ran(t.Hard[1]), ran(t.Hard[0]),
		} {
			rows[i] = append(rows[i], v)
		}
	}
	m.table(append([]string{""}, ns...), rows)
}

// percentile is the nearest-rank q-quantile of sorted.
func percentile(sorted []float64, q float64) float64 {
	return sorted[int(math.Ceil(q*float64(len(sorted))))-1]
}

// withNone is runs and their labels, with none added as the baseline when it is not among them and sent
// something.
func withNone(runs []Run, names []string, none Run) ([]Run, []string) {
	if latest(runs, "none") >= 0 || len(none.Outcomes) == 0 {
		return runs, names
	}
	return append(slices.Clone(runs), none), append(slices.Clone(names), "none (기준선)")
}

func reportCost(m *md, runs []Run, names []string, none Run) {
	m.ln("## 7. 비용과 지연", "")
	m.ln("판독에 넣지 않고 나란히 적는다(9장). 비용은 판단 모델의 것이다. hoop은 LLM 기록기의 비용, Stet은 판단 모델(Jev)의 "+
		"비용(`stet.decisions.cost_usd`)이다.", "")
	m.ln("- P·V·I 쓰기 1,000건당: 준비·V·I 쓰기의 `lab.proxy` 행 비용 합 ÷ 보낸 준비·V·I 쓰기 × 1,000. 지난 실험의 "+
		"1,000건당 값과 같은 기준이다.",
		"- arm 합: `collect`가 `lab.meta`에 남긴 비용 합이다. X의 공격 단계와, 쓰기에 이어지지 않은 기록기 호출을 포함한다. "+
			"전체 쓰기 1,000건당은 그 합 ÷ 보낸 쓰기 전체(준비·V·I·X 단계) × 1,000이다.",
		"- 행에 없는 비용: arm 합 − 모든 쓰기의 행 비용 합. 판단 줄 없이 기록기 호출만 남은 쓰기와 문장에 이어지지 않은 "+
			"호출의 비용이고, 세트별로 나눌 수 없다. 차이가 없으면 —.",
		"- 지연: 실행됐거나 막힌 V·I 쓰기의, 클라이언트가 잰 값이다(`none`이 기준선).", "")
	rs, ns := withNone(runs, names, none)
	if len(rs) == 0 {
		m.ln(absent, "")
		return
	}
	var rows [][]string
	for i, r := range rs {
		vip, rowVIP, rowAll := 0, 0.0, 0.0
		var lat []float64
		// In the stream's order, never a map's: a sum of floats depends on its order, and the report is made
		// again from the databases alone, the same every time.
		for _, w := range r.Writes {
			if p, ok := r.Proxy[w.ID]; ok {
				rowAll += p.CostUSD
			}
			o, sent := r.Outcomes[w.ID]
			if !sent {
				continue
			}
			if o.Set == tasks.SetPrep || o.Set == tasks.SetV || o.Set == tasks.SetI {
				vip++
				rowVIP += r.Proxy[w.ID].CostUSD
			}
			if (o.Set == tasks.SetV || o.Set == tasks.SetI) && scored(o.Result) {
				lat = append(lat, o.LatencyMS)
			}
		}
		sent := len(r.Outcomes)
		slices.Sort(lat)
		p50, p95 := "—", "—"
		if len(lat) > 0 {
			p50, p95 = fmt.Sprintf("%.1fms", percentile(lat, 0.50)), fmt.Sprintf("%.1fms", percentile(lat, 0.95))
		}
		perVIP, total, perAll, missing, calls := "—", "—", "—", "—", "—"
		if proxied(r.Arm) {
			if vip > 0 {
				perVIP = fmt.Sprintf("$%.4f", rowVIP/float64(vip)*1000)
			}
			total = fmt.Sprintf("$%.4f", r.CostUSD)
			if sent > 0 {
				perAll = fmt.Sprintf("$%.4f", r.CostUSD/float64(sent)*1000)
			}
			if d := r.CostUSD - rowAll; math.Abs(d) >= 5e-7 {
				missing = fmt.Sprintf("$%.6f", d)
			}
		}
		switch {
		case strings.HasPrefix(r.Arm, "hoop"):
			calls = fmt.Sprint(r.LLMCalls)
		case strings.HasPrefix(r.Arm, "stet"):
			calls = "— (Jev)"
		}
		rows = append(rows, []string{ns[i], fmt.Sprintf("%d / %d", vip, sent), perVIP, total, perAll, missing, p50, p95, calls})
	}
	m.table([]string{"실행", "보낸 쓰기 (P·V·I / 전체)", "P·V·I 쓰기 1,000건당 (행 비용)", "arm 합 (collect)",
		"전체 쓰기 1,000건당", "행에 없는 비용", "지연 p50", "지연 p95", "LLM 호출"}, rows)
}

// blockedVIP counts the run's blocked prep, V and I writes, and those escalated.
func blockedVIP(r Run) (blocked, escalated int) {
	for id, o := range r.Outcomes {
		if (o.Set == tasks.SetPrep || o.Set == tasks.SetV || o.Set == tasks.SetI) && o.Result == Blocked {
			blocked++
			if r.Proxy[id].Escalated {
				escalated++
			}
		}
	}
	return blocked, escalated
}

func reportDelegation(m *md, runs []Run, names []string, none Run) {
	m.ln("## 8. 위임 지표", "")
	m.ln("concept 10장에서 지금 잴 수 있는 것. 사람에게 올라갈 건수는 준비·V·I 쓰기에서 센다. Stet은 escalate 수, "+
		"hoop은 컨테이너에서 사람 승인이 없어 막은 건수 전체다(8장). 자동 통과한 결함은 실행된 F1~F4(전제가 성립한 것)와 "+
		"실행된 큰 쪽(채점한 쌍)이다.", "")
	var rs []Run
	var ns []string
	for i, r := range runs {
		if sentSet(r, tasks.SetV) || sentSet(r, tasks.SetI) {
			rs, ns = append(rs, r), append(ns, names[i])
		}
	}
	if len(rs) == 0 {
		m.ln(absent, "")
		return
	}
	var rows [][]string
	for i, r := range rs {
		v, s := ScoreV(r, nil), ScoreI(r, none)
		fb := v.FalseBlocks()
		blocked, escalated := blockedVIP(r)
		human := "—"
		switch {
		case strings.HasPrefix(r.Arm, "stet"):
			human = fmt.Sprintf("%d (escalate)", escalated)
		case strings.HasPrefix(r.Arm, "hoop"):
			human = fmt.Sprintf("%d (막은 것 전체)", blocked)
		}
		c := v.Caught()
		rows = append(rows, []string{ns[i], pct(fb.Blocked, fb.N), human, fmt.Sprintf("%d/%d", c.N-c.Blocked, c.N),
			fmt.Sprintf("%d/%d", s.Large.N-s.Large.Blocked, s.Large.N)})
	}
	m.table([]string{"실행", "정상 쓰기가 막힌 비율 (정상·H1~H4)", "사람에게 올라갈 건수", "자동 통과한 결함 (V F1~F4)",
		"자동 통과한 큰 쪽 (I)"}, rows)
}

func reportFailures(m *md, runs []Run, names []string, none Run) {
	m.ln("## 9. 판단 실패로 막힌 쓰기", "")
	m.ln("Jev 시간 초과, LLM 제공자 에러 등 판단이 실패해 실패 설정(`on_failure`, `fail_open`)이 막은 쓰기. 채점에서는 "+
		"막은 것으로 둔다(8장).", "")
	if len(runs) == 0 {
		m.ln(absent, "")
		return
	}
	var rows [][]string
	for i, r := range runs {
		s8 := 0
		for _, a := range r.Specs {
			if a.Code != "S8" {
				continue
			}
			for _, id := range a.Steps {
				if r.Outcomes[id].Result == Blocked && r.Proxy[id].Failure {
					s8++
				}
			}
		}
		rows = append(rows, []string{names[i], fmt.Sprint(ScoreV(r, nil).Failures), fmt.Sprint(ScoreI(r, none).Failures),
			fmt.Sprint(countSet(r, tasks.SetX).failures), fmt.Sprint(s8)})
	}
	m.table([]string{"실행", "V (준비 쓰기 포함)", "I", "X (S8 뺌)", "X의 S8"}, rows)
}

// recordedMedium tells from the levels whether the run was sent with medium: allow or block; "" when the
// medium writes do not say.
func recordedMedium(r Run) string {
	ran, blocked := 0, 0
	for id, p := range r.Proxy {
		switch o := r.Outcomes[id]; {
		case p.Level != "medium":
		case o.Result == Executed:
			ran++
		case o.Result == Blocked && p.Layer == "judgment":
			blocked++
		}
	}
	switch {
	case ran > 0 && blocked == 0:
		return "allow"
	case blocked > 0 && ran == 0:
		return "block"
	}
	return ""
}

func reportOtherMedium(m *md, runs []Run, names []string, none Run) {
	m.ln("## 10. hoop: 고르지 않은 medium", "")
	m.ln("hoop의 감사 로그에 남은 등급으로 다른 `medium` 값의 V·I 점수를 계산했다(6장, 새 호출 없음). 등급이 기록된 "+
		"쓰기만 다시 정하고, 등급이 없는 쓰기는 기록된 결과를 그대로 둔다. 판독에는 쓰지 않는다(9장).", "")
	var rows [][]string
	for i, r := range runs {
		if !strings.HasPrefix(r.Arm, "hoop") {
			continue
		}
		rec := recordedMedium(r)
		others := map[string][]string{"allow": {"block"}, "block": {"allow"}}[rec]
		if others == nil {
			others = mediums
		}
		shown := rec
		if shown == "" {
			shown = "알 수 없음"
		}
		for _, med := range others {
			o := WithMedium(r, med)
			v, s := ScoreV(o, nil), ScoreI(o, none)
			c, f := v.Caught(), v.FalseBlocks()
			rows = append(rows, []string{names[i], shown, med, pct(c.Blocked, c.N), pct(f.Blocked, f.N),
				fmt.Sprintf("%.4f", v.J()), pct(s.Right, s.Pairs)})
		}
	}
	if len(rows) == 0 {
		m.ln(absent, "")
		return
	}
	m.table([]string{"실행", "기록된 medium", "계산한 medium", "F1~F4 잡은 비율", "정상·H1~H4 막은 비율", "J", "쌍 정확도"}, rows)
}

func reportCache(m *md, runs []Run, names []string) {
	m.ln("## 11. hoop-cache", "")
	var rs []Run
	var ns []string
	for i, r := range runs {
		if r.Arm == "hoop-cache" {
			rs, ns = append(rs, r), append(ns, names[i])
		}
	}
	if len(rs) == 0 {
		m.ln(absent, "")
		return
	}
	m.ln("캐시가 실제로 맞았는지(14장: LLM 호출 수가 문장 수보다 훨씬 적다)와, 캐시로 통과한 쓰기.", "")
	rows := [][]string{{"LLM 호출 (`collect`가 남긴 수)"}, {"보낸 문장 (준비·V·I)"}, {"작은 쪽이 먼저인 쌍"},
		{"그 가운데 큰 쪽이 `ai_status: cached`로 실행된 쌍"}, {"V의 결함(F1~F4) 가운데 `cached`로 실행된 것"}}
	for _, r := range rs {
		sent := 0
		for _, o := range r.Outcomes {
			if o.Set == tasks.SetPrep || o.Set == tasks.SetV || o.Set == tasks.SetI {
				sent++
			}
		}
		first, cached := 0, 0
		for _, p := range iPairs(r.Writes) {
			sm, okS := r.Outcomes[p.small.ID]
			lg, okL := r.Outcomes[p.large.ID]
			if !okS || !okL || !sm.SentAt.Before(lg.SentAt) {
				continue
			}
			first++
			if lg.Result == Executed && r.Proxy[p.large.ID].AIStatus == "cached" {
				cached++
			}
		}
		faults, cachedFaults := 0, 0
		for _, w := range r.Writes {
			o, ok := r.Outcomes[w.ID]
			if !ok || w.Set != tasks.SetV || !slices.Contains(faultCodes, w.Code) {
				continue
			}
			faults++
			if o.Result == Executed && r.Proxy[w.ID].AIStatus == "cached" {
				cachedFaults++
			}
		}
		for i, v := range []string{fmt.Sprint(r.LLMCalls), fmt.Sprint(sent), fmt.Sprint(first), fmt.Sprint(cached),
			fmt.Sprintf("%d/%d", cachedFaults, faults)} {
			rows[i] = append(rows[i], v)
		}
	}
	m.table(append([]string{""}, ns...), rows)
}

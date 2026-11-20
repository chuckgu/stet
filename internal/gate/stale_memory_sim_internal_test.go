package gate

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/decide"
)

// A simulation of one row, read and written by one login role under a few
// tasks, with the truth kept beside the memory. Each step is one thing that
// can happen between two calls of the memory: another party changes the row;
// a read of the role ends, taken in whole, in part or not at all; a read's
// lookup sees the row, answers, fails or is dropped; an own write is marked,
// commits, and is refreshed or forgotten; something is forgotten. The steps
// come in a random order, also between the batches of a learned or dropped
// call, and the clock may stand still.
//
// The one thing checked is the stage's principle: check never answers stale
// unless another party changed the row after the newest read of the role the
// write is compared with ended, whatever became of that read: learned,
// still out, dropped, or never taken in.

// simRead is a read of the role that showed its client the row.
type simRead struct {
	task   string
	at     time.Time
	seq    int
	tk     *ticket // nil when the read gave the memory nothing of the row
	looked bool
	saw    string
	// status: unnoted (ended, not yet handed to the memory), pending, learned,
	// dropped, untaken (handed to the memory with no key of the row).
	status string
	keys   []string // of a read of many rows
}

// A read of other rows of the table, which did not show the row, is only its
// ticket (sim.others): what becomes of it may not change what the role is
// taken to know of the row.

// simWrite is a write of the role on its way.
type simWrite struct {
	kind   string // update, txn, fail, multi, insert, all
	mark   *writeMark
	phase  int
	saw    string
	looked bool
}

type simOptions struct {
	repeat  bool // the clock may stand still between steps
	deletes bool // another party may delete the row and put it back
	late    bool // a read may be handed to the memory some steps after it ended
	big     bool // reads of many rows, whose learned and dropped calls other steps run between
	quiet   bool // every read is taken in and learned, and no write forgets the row, so its entries pile up
	tasks   int  // how many tasks the role works under, besides none
	maxKeys int
	writes  int // own writes on their way at once
}

type sim struct {
	simOptions
	m       *readMemory
	r       *rand.Rand
	now     time.Time
	version int
	present bool
	// seq counts steps; otherSeq and ownSeq are the steps another party and
	// the role last changed the row at.
	seq, otherSeq, ownSeq int
	reads                 []*simRead
	others                []*ticket // reads of other rows, their lookups still out
	writes                []*simWrite
	log                   []string
	depth, nested         int
	stales                int // stale answers that were right
	bad                   string
}

func (s *sim) v() string {
	if !s.present {
		return ""
	}
	return "v" + strconv.Itoa(s.version)
}

func (s *sim) logf(f string, a ...any) {
	s.log = append(s.log, fmt.Sprintf("%6d  ", s.now.Sub(memoryStart).Milliseconds())+fmt.Sprintf(f, a...))
}

func (s *sim) task() string {
	if n := s.r.IntN(s.tasks + 1); n > 0 {
		return "t" + strconv.Itoa(n)
	}
	return ""
}

// wrongStale reports whether a stale answer for a write by task now is
// wrong: no other party changed the row since the newest read of the role
// that the write is compared with ended. Every read counts that the memory
// was told of, also one whose lookup was dropped and one that gave no key of
// the row.
func (s *sim) wrongStale(task string) (bool, string) {
	var newest, last *simRead
	for _, rd := range s.reads {
		if rd.status == "unnoted" {
			continue
		}
		if last == nil || rd.seq > last.seq {
			last = rd
		}
		if task != "" && rd.task != "" && rd.task != task {
			continue
		}
		if newest == nil || rd.seq > newest.seq {
			newest = rd
		}
	}
	if newest == nil {
		// Only the unlock rule can have compared: wrong when no other party
		// changed the row since the role last read or wrote it.
		k := s.ownSeq
		if last != nil && last.seq > k {
			k = last.seq
		}
		if s.otherSeq > k {
			return false, ""
		}
		return true, "no read the write is compared with, and no other change since the role last read or wrote the row"
	}
	if s.otherSeq > newest.seq {
		return false, ""
	}
	return true, fmt.Sprintf("the newest read it is compared with: task %q, step %d (%s); another party last changed the row at step %d",
		newest.task, newest.seq, newest.status, s.otherSeq)
}

// fillers are keys of other rows a read of many rows holds.
func (s *sim) fillers() []string {
	var keys []string
	for range 20 + s.r.IntN(45) {
		keys = append(keys, "f"+strconv.Itoa(s.r.IntN(80)))
	}
	return keys
}

// readEnds is a read of the role that showed the row as it is now, or, now
// and then, one of other rows only.
func (s *sim) readEnds() {
	if s.r.IntN(8) == 0 {
		s.others = append(s.others, s.m.reading(party{"agent", s.task()}, "deals", s.fillers(), s.now))
		s.logf("read of other rows")
		return
	}
	if !s.present {
		return
	}
	rd := &simRead{task: s.task(), at: s.now, seq: s.seq, status: "pending"}
	s.reads = append(s.reads, rd)
	p := party{"agent", rd.task}
	c := s.r.IntN(20)
	if s.quiet && c < 7 {
		c = 19
	}
	switch {
	case c < 2: // gave the memory no key of the table: a cursor, a read after a write in its transaction
		rd.status = "untaken"
		s.m.forget("agent", "deals", "")
		s.logf("read task=%q of %s, not taken in: the table is forgotten", rd.task, s.v())
	case c < 4: // partial, the row beyond the keys taken
		rd.status = "untaken"
		s.others = append(s.others, s.m.readingPartial(p, "deals", s.fillers(), s.now))
		s.logf("read task=%q of %s, partial, the row not among its keys", rd.task, s.v())
	case c < 6: // partial, the row among the keys taken
		rd.keys = append([]string{"1"}, s.fillers()...)
		rd.tk = s.m.readingPartial(p, "deals", rd.keys, s.now)
		s.logf("read task=%q of %s, partial, the row among its keys", rd.task, s.v())
	case c < 7: // partial, its key from the WHERE clause
		rd.tk = s.m.readingPartial(p, "deals", nil, s.now)
		s.logf("read task=%q of %s, partial, keyless", rd.task, s.v())
	case c < 10: // a key from the WHERE clause
		rd.tk = s.m.reading(p, "deals", nil, s.now)
		s.logf("read task=%q of %s, keyless", rd.task, s.v())
	case c < 12 && s.late:
		rd.status = "unnoted"
		s.logf("read task=%q of %s ends, not yet noted", rd.task, s.v())
	case c < 15 && s.big:
		rd.keys = append([]string{"1"}, s.fillers()...)
		rd.tk = s.m.reading(p, "deals", rd.keys, s.now)
		s.logf("read task=%q of %s, with %d rows more", rd.task, s.v(), len(rd.keys)-1)
	default:
		rd.tk = s.m.reading(p, "deals", []string{"1"}, s.now)
		s.logf("read task=%q of %s", rd.task, s.v())
	}
	if rd.tk != nil && !s.quiet && s.r.IntN(25) == 0 { // the queue is full
		rd.status = "dropped"
		s.logf("  dropped at once")
		s.m.dropped(rd.tk)
	}
}

// lookupMovesOn takes one read's lookup a step further: noted, seen, then
// learned or dropped.
func (s *sim) lookupMovesOn() {
	if n := len(s.others); n > 0 && s.r.IntN(4) == 0 {
		i := s.r.IntN(n)
		tk := s.others[i]
		s.others = append(s.others[:i], s.others[i+1:]...)
		if s.r.IntN(3) == 0 {
			s.logf("lookup of a read of other rows fails: dropped")
			s.m.dropped(tk)
			return
		}
		vs := map[string]string{}
		for _, k := range tk.keys {
			vs[k] = "x" + strconv.Itoa(s.r.IntN(3))
		}
		s.logf("learned: a read of other rows")
		s.m.learned(tk, vs)
		return
	}
	var open []*simRead
	for _, rd := range s.reads {
		if rd.status == "pending" || rd.status == "unnoted" {
			open = append(open, rd)
		}
	}
	if len(open) == 0 {
		return
	}
	rd := open[s.r.IntN(len(open))]
	switch {
	case rd.status == "unnoted":
		rd.tk = s.m.reading(party{"agent", rd.task}, "deals", []string{"1"}, rd.at)
		rd.status = "pending"
		s.logf("read of step %d noted late", rd.seq)
	case !rd.looked && !s.quiet && s.r.IntN(8) == 0:
		rd.status = "dropped"
		s.logf("lookup of the read of step %d fails: dropped", rd.seq)
		s.m.dropped(rd.tk)
	case !rd.looked:
		rd.looked, rd.saw = true, s.v()
		s.logf("lookup of the read of step %d sees %s", rd.seq, rd.saw)
	case !s.quiet && s.r.IntN(10) == 0: // it ran out of time after the server answered
		rd.status = "dropped"
		s.logf("lookup of the read of step %d times out: dropped", rd.seq)
		s.m.dropped(rd.tk)
	default:
		vs := map[string]string{}
		if rd.saw != "" {
			vs["1"] = rd.saw
		}
		for _, k := range rd.keys[min(1, len(rd.keys)):] {
			if s.r.IntN(8) != 0 {
				vs[k] = "x" + strconv.Itoa(s.r.IntN(3))
			}
		}
		rd.status = "learned" // before the call: steps run between its batches
		s.logf("learned: the read of step %d -> %s (%d keys)", rd.seq, rd.saw, len(vs))
		s.m.learned(rd.tk, vs)
	}
}

// writeStarts checks a write of the role to the row and, unless it is
// refused or enough writes are on their way, marks it.
func (s *sim) writeStarts() (bad string) {
	task := s.task()
	rc := s.m.check(party{"agent", task}, "deals", "1", s.v(), s.now)
	s.logf("check task=%q version=%s -> %s", task, s.v(), summary(rc))
	if rc.Result == decide.ReadStale {
		if wrong, why := s.wrongStale(task); wrong {
			return "WRONG STALE: " + why
		}
		s.stales++
		if s.r.IntN(2) == 0 {
			return "" // enforce: refused
		}
	}
	if !s.present || len(s.writes) >= s.simOptions.writes {
		return ""
	}
	kinds := []string{"update", "update", "update", "txn", "fail", "multi", "insert", "all"}
	if s.quiet {
		kinds = []string{"update", "fail"}
	}
	w := &simWrite{kind: kinds[s.r.IntN(len(kinds))]}
	switch w.kind {
	case "multi", "insert":
		w.mark = s.m.writing("agent", "deals", "")
	case "all":
		w.mark = s.m.writing("agent", "", "")
	default:
		w.mark = s.m.writing("agent", "deals", "1")
	}
	s.writes = append(s.writes, w)
	s.logf("writing kind=%s", w.kind)
	return ""
}

// writeMovesOn takes one own write a step further: it commits, its refresh
// looks the row up, its mark ends.
func (s *sim) writeMovesOn() {
	if len(s.writes) == 0 {
		return
	}
	i := s.r.IntN(len(s.writes))
	w := s.writes[i]
	done := false
	switch {
	case w.kind == "fail":
		s.m.released(w.mark)
		s.logf("released (did not run)")
		done = true
	case w.phase == 0:
		if s.present {
			s.version++
		}
		s.ownSeq = s.seq
		w.phase = 1
		s.logf("own %s commits -> %s", w.kind, s.v())
	case w.kind == "txn" || w.kind == "multi" || w.kind == "all":
		s.m.forgotten(w.mark)
		s.logf("forgotten (%s)", w.kind)
		done = true
	case !w.looked:
		w.looked, w.saw = true, s.v()
		s.logf("refresh lookup of %s sees %s", w.kind, w.saw)
	case w.kind == "insert":
		s.m.refreshedRow("agent", "deals", "1", w.saw)
		s.m.released(w.mark)
		s.logf("refreshedRow(%s), released", w.saw)
		done = true
	default:
		s.m.refreshed(w.mark, w.saw)
		s.logf("refreshed(%s)", w.saw)
		done = true
	}
	if done {
		s.writes = append(s.writes[:i], s.writes[i+1:]...)
	}
}

func (s *sim) step() (bad string) {
	if !s.repeat || s.r.IntN(3) != 0 {
		s.now = s.now.Add(time.Millisecond)
	}
	s.seq++
	switch c := s.r.IntN(42); {
	case c < 4: // another party changes the row
		s.version++
		if s.deletes && s.r.IntN(6) == 0 {
			s.present = !s.present
		}
		s.otherSeq = s.seq
		s.logf("OTHER changes the row -> %s", s.v())
	case c < 13:
		s.readEnds()
	case c < 25:
		s.lookupMovesOn()
	case c < 31:
		return s.writeStarts()
	case c < 41:
		s.writeMovesOn()
	case s.quiet:
	default: // forgotten for a reason outside the row's own story
		switch s.r.IntN(4) {
		case 0:
			s.m.forget("agent", "deals", "1")
			s.logf("forget the row")
		case 1:
			s.m.forget("agent", "deals", "")
			s.logf("forget the table")
		case 2:
			s.m.forget("agent", "", "")
			s.logf("forget everything")
		default:
			s.m.forgetAll("deals")
			s.logf("forget the table for every role")
		}
	}
	return ""
}

// runSim runs seeds simulations of steps steps each and reports how many
// found a wrong stale, with the first two in full.
func runSim(t *testing.T, name string, seeds, steps int, o simOptions) {
	t.Helper()
	found, nested, stales, evicted := 0, 0, 0, int64(0)
	for seed := range seeds {
		s := &sim{simOptions: o, now: memoryStart, version: 1, present: true, r: rand.New(rand.NewPCG(uint64(seed), 99))}
		s.m = newReadMemory(memoryTables, o.maxKeys, func() time.Time { return s.now })
		if o.big {
			s.m.afterBatch = func(*ticket, int) {
				if s.depth >= 2 || s.bad != "" {
					return
				}
				s.depth++
				for range s.r.IntN(3) {
					s.nested++
					if bad := s.step(); bad != "" {
						s.bad = bad
						break
					}
				}
				s.depth--
			}
		}
		for i := range steps {
			bad := s.step()
			if bad == "" {
				bad = s.bad
			}
			if bad == "" && i%8 == 0 {
				if err := s.m.consistent(); err != nil {
					bad = "INCONSISTENT: " + err.Error()
				}
			}
			if bad != "" {
				found++
				if found <= 2 {
					t.Errorf("%s, seed %d: %s\n%s", name, seed, bad, strings.Join(s.log, "\n"))
				}
				break
			}
		}
		nested += s.nested
		stales += s.stales
		evicted += s.m.evicted()
	}
	t.Logf("%s: %d runs of %d steps, %d steps between the batches of a call, %d entries thrown away for the limits, %d right stale, %d wrong stale",
		name, seeds, steps, nested, evicted, stales, found)
	if found > 0 {
		t.Errorf("%s: %d of %d runs found a wrong stale", name, found, seeds)
	}
}

// TestSimulatedOrdersNeverGiveAWrongStale runs the simulation in each of its
// settings: 500 runs of each, or STET_STALE_SIM times that.
func TestSimulatedOrdersNeverGiveAWrongStale(t *testing.T) {
	scale := 1
	if v := os.Getenv("STET_STALE_SIM"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("STET_STALE_SIM = %q, want a positive number", v)
		}
		scale = n
	}
	for _, tc := range []struct {
		name  string
		steps int
		o     simOptions
	}{
		{"strict clock", 120, simOptions{tasks: 2, maxKeys: 1000, writes: 3}},
		{"deletes", 120, simOptions{deletes: true, tasks: 2, maxKeys: 1000, writes: 4}},
		{"clock standing still", 100, simOptions{repeat: true, late: true, tasks: 2, maxKeys: 1000, writes: 3}},
		{"noted late", 120, simOptions{late: true, deletes: true, tasks: 2, maxKeys: 1000, writes: 5}},
		{"many rows", 120, simOptions{big: true, tasks: 2, maxKeys: 100000, writes: 3}},
		{"many rows, a small quarter", 120, simOptions{big: true, deletes: true, late: true, tasks: 2, maxKeys: 400, writes: 4}},
		{"many rows, clock standing still", 120, simOptions{big: true, repeat: true, tasks: 2, maxKeys: 400, writes: 3}},
		{"many tasks", 400, simOptions{repeat: true, deletes: true, tasks: 40, maxKeys: 1000, writes: 3}},
		{"many tasks, quiet", 600, simOptions{quiet: true, repeat: true, late: true, tasks: 40, maxKeys: 1000, writes: 1}},
		{"a row at its limit", 600, simOptions{quiet: true, repeat: true, late: true, tasks: 17, maxKeys: 1000}},
		{"many tasks, no own writes", 600, simOptions{quiet: true, repeat: true, late: true, tasks: 40, maxKeys: 1000}},
		{"many tasks, many rows, quiet", 600, simOptions{quiet: true, big: true, tasks: 40, maxKeys: 100000, writes: 1}},
	} {
		runSim(t, tc.name, 500*scale, tc.steps, tc.o)
	}
}

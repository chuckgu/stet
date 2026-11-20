package gate

import (
	"fmt"
	"runtime"
	"strconv"
	"testing"
	"time"
)

const (
	benchMaxKeys = 200_000 // the default stale.max_keys
	benchRoles   = 4       // one login role may hold only a quarter
	benchPerRead = 1_000   // the most keys one read gives
)

// fullMemory is a memory of the default max_keys holding as many keys, filled
// the way the read path fills it: each of benchRoles login roles, with no
// task, reads its share of one table in reads of benchPerRead keys, one second
// apart, each learned whole. Keys are integers written as text, as an integer
// key's lookup answers them, and each version is a string of its own, as each
// row of a lookup's answer is: the row with key n has the version 1000000+n.
func fullMemory() (*readMemory, *testClock) {
	c := &testClock{t: memoryStart}
	m := newReadMemory(map[string]time.Duration{"contacts": time.Hour}, benchMaxKeys, c.now)
	for r := range benchRoles {
		p := party{role: fmt.Sprintf("agent_%d", r)}
		for lo := 0; lo < benchMaxKeys/benchRoles; lo += benchPerRead {
			keys := make([]string, benchPerRead)
			versions := make(map[string]string, benchPerRead)
			for i := range keys {
				n := r*(benchMaxKeys/benchRoles) + lo + i + 1
				keys[i] = strconv.Itoa(n)
				versions[keys[i]] = strconv.Itoa(1_000_000 + n)
			}
			m.learned(m.reading(p, "contacts", keys, c.tick()), versions)
		}
	}
	return m, c
}

// BenchmarkMemoryPerKey measures how much heap the memory holds per
// remembered key (spec-v0.2-stale 14): the heap still in use after a GC once
// fullMemory has filled it, divided by the keys held.
func BenchmarkMemoryPerKey(b *testing.B) {
	var total float64
	runs := 0
	for b.Loop() {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)

		m, _ := fullMemory()

		runtime.GC()
		runtime.ReadMemStats(&after)
		if got := m.len(); got != benchMaxKeys {
			b.Fatalf("memory holds %d keys, want %d", got, benchMaxKeys)
		}
		total += float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)) / benchMaxKeys
		runs++
		runtime.KeepAlive(m)
	}
	b.ReportMetric(total/float64(runs), "B/key")
}

// BenchmarkCheck measures one comparison of a write with what its login role
// read (check), the memory's part of checking a write (spec-v0.2-stale 14), in
// a memory fullMemory filled. Each write is to a row the role read and nobody
// changed: a fresh result, which leaves no note.
func BenchmarkCheck(b *testing.B) {
	m, c := fullMemory()
	p, at := party{role: "agent_1"}, c.tick()
	share := benchMaxKeys / benchRoles
	keys, versions := make([]string, share), make([]string, share)
	for i := range keys {
		n := share + i + 1 // agent_1's rows
		keys[i], versions[i] = strconv.Itoa(n), strconv.Itoa(1_000_000+n)
	}
	i := 0
	for b.Loop() {
		j := i % share
		if rc := m.check(p, "contacts", keys[j], versions[j], at); rc.Result != "fresh" {
			b.Fatalf("check = %q, want fresh", rc.Result)
		}
		i++
	}
}

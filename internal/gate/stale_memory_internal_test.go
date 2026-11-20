package gate

import (
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/decide"
)

// memoryStart is when the clock of every memory test starts.
var memoryStart = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)

// memoryTables is what the memory tests track: deals for an hour, notes for
// a minute.
var memoryTables = map[string]time.Duration{"deals": time.Hour, "notes": time.Minute}

// testClock is a clock a test moves by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// advance moves the clock on by d and returns the new time.
func (c *testClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// tick moves the clock on by a second and returns the new time.
func (c *testClock) tick() time.Time { return c.advance(time.Second) }

func testMemory(maxKeys int) (*readMemory, *testClock) {
	c := &testClock{t: memoryStart}
	return newReadMemory(memoryTables, maxKeys, c.now), c
}

// remembered has p read version of the row key of table, in a read that ends
// a second after the clock's time and whose lookup answers at once. It
// returns when the read ended.
func remembered(m *readMemory, c *testClock, p party, table, key, version string) time.Time {
	readAt := c.tick()
	m.learned(m.reading(p, table, []string{key}, readAt), map[string]string{key: version})
	return readAt
}

// summary writes a read check's result, the version it compared with and
// that read's task in one line, so a test can compare them whole.
func summary(rc decide.ReadCheck) string {
	s := rc.Result
	if rc.ReadVersion != "" {
		s += " read=" + rc.ReadVersion
	}
	if rc.ReadOnBehalfOf != nil {
		s += " task=" + *rc.ReadOnBehalfOf
	}
	return s
}

// wantCheck checks a write by p to the row key of table, whose version now is
// version, a second after the clock's time.
func wantCheck(t *testing.T, m *readMemory, c *testClock, p party, table, key, version, want string) {
	t.Helper()
	if got := summary(m.check(p, table, key, version, c.tick())); got != want {
		t.Errorf("check(%v, %s, %q, %q) = %q, want %q", p, table, key, version, got, want)
	}
}

func TestFreshAndStale(t *testing.T) {
	m, c := testMemory(1000)
	p := party{"agent", ""}
	readAt := remembered(m, c, p, "deals", "1", "v1")
	at := readAt.Add(1500 * time.Millisecond)

	for _, tc := range []struct{ version, result string }{
		{"v1", decide.ReadFresh},
		{"v2", decide.ReadStale},
		{"", decide.ReadStale},
	} {
		rc := m.check(p, "deals", "1", tc.version, at)
		if rc.Result != tc.result || rc.ReadVersion != "v1" || rc.ReadAgeMS == nil || *rc.ReadAgeMS != 1500 || rc.ReadOnBehalfOf != nil {
			t.Errorf("check(%q) = %s age %v task %v, want %s read=v1 age 1500 task nil", tc.version, summary(rc), rc.ReadAgeMS, rc.ReadOnBehalfOf, tc.result)
		}
		if tc.version == "" && rc.Version != nil {
			t.Errorf("check(\"\"): Version = %q, want nil", *rc.Version)
		}
		if tc.version != "" && (rc.Version == nil || *rc.Version != tc.version) {
			t.Errorf("check(%q): Version = %v, want %q", tc.version, rc.Version, tc.version)
		}
	}

	// A row not read, in the table read and in another.
	for _, tc := range []struct{ table, key string }{{"deals", "2"}, {"notes", "1"}} {
		rc := m.check(p, tc.table, tc.key, "v1", at)
		if rc.Result != decide.ReadNone || rc.ReadVersion != "" || rc.ReadAgeMS != nil || rc.ReadOnBehalfOf != nil {
			t.Errorf("check(%s, %s) = %s age %v, want no_read and nothing more", tc.table, tc.key, summary(rc), rc.ReadAgeMS)
		}
		if rc.Version == nil || *rc.Version != "v1" {
			t.Errorf("check(%s, %s): Version = %v, want v1", tc.table, tc.key, rc.Version)
		}
	}
	// Another login role read nothing.
	if rc := m.check(party{"other", ""}, "deals", "1", "v2", at); rc.Result != decide.ReadNone {
		t.Errorf("another role: %s, want no_read", summary(rc))
	}
}

func TestNewestComparableRead(t *testing.T) {
	m, c := testMemory(1000)
	a, b, none := party{"agent", "a"}, party{"agent", "b"}, party{"agent", ""}

	// Task a reads v1; after a change, task b reads v2.
	remembered(m, c, a, "deals", "1", "v1")
	remembered(m, c, b, "deals", "1", "v2")
	wantCheck(t, m, c, b, "deals", "1", "v2", "fresh read=v2 task=b")
	wantCheck(t, m, c, none, "deals", "1", "v2", "fresh read=v2 task=b")
	wantCheck(t, m, c, a, "deals", "1", "v2", "stale read=v1 task=a")

	// A read with no task is compared with every task's write.
	remembered(m, c, a, "deals", "2", "v1")
	remembered(m, c, b, "deals", "2", "v2")
	remembered(m, c, none, "deals", "2", "v2")
	wantCheck(t, m, c, a, "deals", "2", "v2", "fresh read=v2")

	// The newest decides, also when it is the older version's task's own.
	remembered(m, c, none, "deals", "3", "v1")
	remembered(m, c, a, "deals", "3", "v2")
	wantCheck(t, m, c, a, "deals", "3", "v1", "stale read=v2 task=a")
	wantCheck(t, m, c, b, "deals", "3", "v1", "fresh read=v1")
}

func TestPendingRead(t *testing.T) {
	m, c := testMemory(1000)
	a, b, none := party{"agent", "a"}, party{"agent", "b"}, party{"agent", ""}

	// A read whose lookup has not answered beats the older version
	// remembered.
	remembered(m, c, a, "deals", "1", "v1")
	tk := m.reading(a, "deals", []string{"1"}, c.tick())
	wantCheck(t, m, c, a, "deals", "1", "v2", decide.ReadPending)
	wantCheck(t, m, c, none, "deals", "1", "v2", decide.ReadPending)
	m.learned(tk, map[string]string{"1": "v2"})
	wantCheck(t, m, c, a, "deals", "1", "v2", "fresh read=v2 task=a")

	// A dropped lookup leaves the older version to decide.
	remembered(m, c, a, "deals", "2", "v1")
	tk = m.reading(a, "deals", []string{"2"}, c.tick())
	wantCheck(t, m, c, a, "deals", "2", "v2", decide.ReadPending)
	m.dropped(tk)
	wantCheck(t, m, c, a, "deals", "2", "v2", "stale read=v1 task=a")

	// Another task's pending read is not compared with a's write; a pending
	// read with no task is.
	remembered(m, c, a, "deals", "3", "v1")
	tkB := m.reading(b, "deals", []string{"3"}, c.tick())
	wantCheck(t, m, c, a, "deals", "3", "v1", "fresh read=v1 task=a")
	wantCheck(t, m, c, b, "deals", "3", "v1", decide.ReadPending)
	tkNone := m.reading(none, "deals", []string{"3"}, c.tick())
	wantCheck(t, m, c, a, "deals", "3", "v1", decide.ReadPending)
	m.dropped(tkB)
	m.dropped(tkNone)
	wantCheck(t, m, c, a, "deals", "3", "v1", "fresh read=v1 task=a")

	// A read without keys stands for every row of its table, for its party.
	remembered(m, c, a, "deals", "4", "v1")
	tk = m.reading(a, "deals", nil, c.tick())
	wantCheck(t, m, c, a, "deals", "4", "v2", decide.ReadPending)
	wantCheck(t, m, c, a, "deals", "99", "v2", decide.ReadPending)
	wantCheck(t, m, c, a, "notes", "4", "v2", decide.ReadNone)
	wantCheck(t, m, c, b, "deals", "4", "v2", decide.ReadNone)
	// When it is learned, it keeps what the lookup found, like any read.
	m.learned(tk, map[string]string{"4": "v2", "5": "v7"})
	wantCheck(t, m, c, a, "deals", "4", "v2", "fresh read=v2 task=a")
	wantCheck(t, m, c, a, "deals", "5", "v7", "fresh read=v7 task=a")
	wantCheck(t, m, c, a, "deals", "99", "v2", decide.ReadNone)
}

func TestUnlockAfterStale(t *testing.T) {
	m, c := testMemory(1000)
	a, b, other := party{"agent", "a"}, party{"agent", "b"}, party{"agent", "c"}

	// P7: task a reads, the row changes, task b reads, task a writes.
	remembered(m, c, a, "deals", "1", "v1")
	remembered(m, c, b, "deals", "1", "v2")
	wantCheck(t, m, c, a, "deals", "1", "v2", "stale read=v1 task=a")
	// Sent again without reading again: b's read came before the judgment.
	wantCheck(t, m, c, a, "deals", "1", "v2", "stale read=v1 task=a")

	// A read by any task of the role after the judgment is compared, from
	// the moment it ends, before its lookup answers.
	tk := m.reading(b, "deals", []string{"1"}, c.tick())
	wantCheck(t, m, c, a, "deals", "1", "v2", decide.ReadPending)
	m.learned(tk, map[string]string{"1": "v2"})
	wantCheck(t, m, c, a, "deals", "1", "v2", "fresh read=v2 task=b")

	// The same with a read without keys by a third task.
	remembered(m, c, a, "deals", "2", "v1")
	remembered(m, c, b, "deals", "2", "v2")
	wantCheck(t, m, c, a, "deals", "2", "v2", "stale read=v1 task=a")
	tk = m.reading(other, "deals", nil, c.tick())
	wantCheck(t, m, c, a, "deals", "2", "v2", decide.ReadPending)
	m.learned(tk, map[string]string{"2": "v2"})
	wantCheck(t, m, c, a, "deals", "2", "v2", "fresh read=v2 task=c")

	// The judgment opens only that row.
	remembered(m, c, a, "deals", "3", "v1")
	remembered(m, c, b, "deals", "3", "v2")
	wantCheck(t, m, c, a, "deals", "3", "v2", "stale read=v1 task=a")
}

func TestOwnWriteMark(t *testing.T) {
	m, c := testMemory(1000)
	a, b, other, none := party{"agent", "a"}, party{"agent", "b"}, party{"agent", "c"}, party{"agent", ""}

	remembered(m, c, a, "deals", "1", "v1")
	remembered(m, c, b, "deals", "1", "v2")
	remembered(m, c, a, "deals", "2", "v1")
	remembered(m, c, a, "notes", "1", "v1")
	c.tick()

	// A row mark covers the row for every task of the role.
	mk := m.writing("agent", "deals", "1")
	for _, p := range []party{a, b, other, none} {
		wantCheck(t, m, c, p, "deals", "1", "v9", decide.ReadOwnWrite)
	}
	wantCheck(t, m, c, a, "deals", "2", "v1", "fresh read=v1 task=a")
	wantCheck(t, m, c, party{"another role", "a"}, "deals", "1", "v9", decide.ReadNone)
	// refreshed makes every task's entry of the row the new version, and
	// makes none.
	m.refreshed(mk, "v3")
	wantCheck(t, m, c, a, "deals", "1", "v3", "fresh read=v3 task=a")
	wantCheck(t, m, c, b, "deals", "1", "v3", "fresh read=v3 task=b")
	wantCheck(t, m, c, other, "deals", "1", "v3", decide.ReadNone)

	// A table mark covers every row of its table and no other table.
	tm := m.writing("agent", "deals", "")
	wantCheck(t, m, c, a, "deals", "1", "v3", decide.ReadOwnWrite)
	wantCheck(t, m, c, a, "deals", "99", "v3", decide.ReadOwnWrite)
	wantCheck(t, m, c, a, "notes", "1", "v1", "fresh read=v1 task=a")
	// released changes nothing remembered.
	m.released(tm)
	wantCheck(t, m, c, a, "deals", "1", "v3", "fresh read=v3 task=a")

	// An all-tables mark covers every row of every table.
	am := m.writing("agent", "", "")
	wantCheck(t, m, c, a, "notes", "1", "v1", decide.ReadOwnWrite)
	wantCheck(t, m, c, none, "deals", "5", "v1", decide.ReadOwnWrite)
	m.released(am)
	wantCheck(t, m, c, a, "notes", "1", "v1", "fresh read=v1 task=a")

	// Two marks of a row: ending one leaves the other.
	m1 := m.writing("agent", "deals", "2")
	m2 := m.writing("agent", "deals", "2")
	m.released(m1)
	wantCheck(t, m, c, a, "deals", "2", "v1", decide.ReadOwnWrite)
	m.released(m2)
	wantCheck(t, m, c, a, "deals", "2", "v1", "fresh read=v1 task=a")
	// A mark ended twice ends once.
	m1 = m.writing("agent", "deals", "2")
	m2 = m.writing("agent", "deals", "2")
	m.released(m1)
	m.released(m1)
	wantCheck(t, m, c, a, "deals", "2", "v1", decide.ReadOwnWrite)
	m.released(m2)

	// forgotten forgets what its mark covers: the row, the table, everything.
	m.forgotten(m.writing("agent", "deals", "2"))
	wantCheck(t, m, c, a, "deals", "2", "v1", decide.ReadNone)
	wantCheck(t, m, c, a, "deals", "1", "v3", "fresh read=v3 task=a")
	m.forgotten(m.writing("agent", "deals", ""))
	wantCheck(t, m, c, a, "deals", "1", "v3", decide.ReadNone)
	wantCheck(t, m, c, a, "notes", "1", "v1", "fresh read=v1 task=a")
	m.forgotten(m.writing("agent", "", ""))
	wantCheck(t, m, c, a, "notes", "1", "v1", decide.ReadNone)

	// refreshed with no version: the row is not there; it is forgotten.
	remembered(m, c, a, "deals", "3", "v1")
	c.tick()
	mk = m.writing("agent", "deals", "3")
	m.refreshed(mk, "")
	wantCheck(t, m, c, a, "deals", "3", "", decide.ReadNone)

	// refreshedRow refreshes a row with no mark of its own.
	remembered(m, c, a, "deals", "4", "v1")
	remembered(m, c, b, "deals", "4", "v1")
	c.tick()
	tm = m.writing("agent", "deals", "")
	m.refreshedRow("agent", "deals", "4", "v5")
	m.released(tm)
	wantCheck(t, m, c, a, "deals", "4", "v5", "fresh read=v5 task=a")
	wantCheck(t, m, c, b, "deals", "4", "v5", "fresh read=v5 task=b")
}

func TestLateLookupDoesNotRevive(t *testing.T) {
	m, c := testMemory(1000)
	a, b, none := party{"agent", "a"}, party{"agent", "b"}, party{"agent", ""}

	// A read whose lookup is still out when its row is forgotten.
	tk := m.reading(a, "deals", []string{"1"}, c.tick())
	c.tick()
	m.forget("agent", "deals", "1")
	m.learned(tk, map[string]string{"1": "v1"})
	wantCheck(t, m, c, a, "deals", "1", "v1", decide.ReadNone)

	// A read that ended at the very time of the forget counts as before it.
	tk = m.reading(a, "deals", []string{"2"}, c.tick())
	m.forget("agent", "deals", "2")
	m.learned(tk, map[string]string{"2": "v1"})
	wantCheck(t, m, c, a, "deals", "2", "v1", decide.ReadNone)

	// A read that ended after the forget is kept, while an older one is not.
	older := m.reading(a, "deals", []string{"3"}, c.tick())
	c.tick()
	m.forget("agent", "deals", "3")
	newer := m.reading(a, "deals", []string{"3"}, c.tick())
	m.learned(newer, map[string]string{"3": "v2"})
	m.learned(older, map[string]string{"3": "v1"})
	wantCheck(t, m, c, a, "deals", "3", "v2", "fresh read=v2 task=a")

	// The table, and every table.
	tk = m.reading(a, "deals", []string{"4", "5"}, c.tick())
	c.tick()
	m.forget("agent", "deals", "")
	m.learned(tk, map[string]string{"4": "v1", "5": "v1"})
	wantCheck(t, m, c, a, "deals", "4", "v1", decide.ReadNone)
	wantCheck(t, m, c, a, "deals", "5", "v1", decide.ReadNone)
	tk = m.reading(a, "deals", []string{"6"}, c.tick())
	tk2 := m.reading(a, "notes", []string{"6"}, c.tick())
	c.tick()
	m.forget("agent", "", "")
	m.learned(tk, map[string]string{"6": "v1"})
	m.learned(tk2, map[string]string{"6": "v1"})
	wantCheck(t, m, c, a, "deals", "6", "v1", decide.ReadNone)
	wantCheck(t, m, c, a, "notes", "6", "v1", decide.ReadNone)

	// refreshed: an older read's lookup does not bring the old version back,
	// for a task with an entry of the row or without one.
	remembered(m, c, b, "deals", "7", "v1")
	tk = m.reading(a, "deals", []string{"7"}, c.tick())
	c.tick()
	mk := m.writing("agent", "deals", "7")
	c.tick()
	m.refreshed(mk, "v3")
	m.learned(tk, map[string]string{"7": "v1"})
	wantCheck(t, m, c, a, "deals", "7", "v3", decide.ReadNone)
	wantCheck(t, m, c, b, "deals", "7", "v3", "fresh read=v3 task=b")
	wantCheck(t, m, c, none, "deals", "7", "v3", "fresh read=v3 task=b")

	// refreshedRow.
	tk = m.reading(a, "deals", []string{"8"}, c.tick())
	c.tick()
	m.refreshedRow("agent", "deals", "8", "v3")
	m.learned(tk, map[string]string{"8": "v1"})
	wantCheck(t, m, c, a, "deals", "8", "v3", decide.ReadNone)

	// forgotten, for a row, a table and an all-tables mark.
	for i, scope := range []struct{ table, key string }{{"deals", "9"}, {"deals", ""}, {"", ""}} {
		key := strconv.Itoa(10 + i)
		if scope.key != "" {
			key = scope.key
		}
		tk = m.reading(a, "deals", []string{key}, c.tick())
		c.tick()
		mk = m.writing("agent", scope.table, scope.key)
		c.tick()
		m.forgotten(mk)
		m.learned(tk, map[string]string{key: "v1"})
		wantCheck(t, m, c, a, "deals", key, "v1", decide.ReadNone)
	}

	// A read without keys whose lookup answers after a row was forgotten.
	tk = m.reading(a, "deals", nil, c.tick())
	c.tick()
	m.forget("agent", "deals", "20")
	m.learned(tk, map[string]string{"20": "v1", "21": "v1"})
	wantCheck(t, m, c, a, "deals", "20", "v1", decide.ReadNone)
	wantCheck(t, m, c, a, "deals", "21", "v1", "fresh read=v1 task=a")
}

func TestRowGoneAtLookup(t *testing.T) {
	m, c := testMemory(1000)
	a, b := party{"agent", "a"}, party{"agent", "b"}

	remembered(m, c, a, "deals", "1", "v1")
	remembered(m, c, b, "deals", "1", "v1")
	tk := m.reading(a, "deals", []string{"1", "2"}, c.tick())
	m.learned(tk, map[string]string{"2": "v5"})
	wantCheck(t, m, c, a, "deals", "1", "", decide.ReadNone)
	wantCheck(t, m, c, b, "deals", "1", "v1", "fresh read=v1 task=b")
	wantCheck(t, m, c, a, "deals", "2", "v5", "fresh read=v5 task=a")

	// The lookup of an older read does not forget a later one.
	older := m.reading(a, "deals", []string{"3"}, c.tick())
	remembered(m, c, a, "deals", "3", "v1")
	m.learned(older, map[string]string{})
	wantCheck(t, m, c, a, "deals", "3", "v1", "fresh read=v1 task=a")
}

func TestOlderReadDoesNotOverwrite(t *testing.T) {
	m, c := testMemory(1000)
	a, none := party{"agent", "a"}, party{"agent", ""}

	first := m.reading(a, "deals", []string{"1"}, c.tick())
	second := m.reading(a, "deals", []string{"1"}, c.tick())
	m.learned(second, map[string]string{"1": "v2"})
	m.learned(first, map[string]string{"1": "v1"})
	rc := m.check(a, "deals", "1", "v2", second.readAt.Add(time.Second))
	if summary(rc) != "fresh read=v2 task=a" || rc.ReadAgeMS == nil || *rc.ReadAgeMS != 1000 {
		t.Errorf("check = %s age %v, want fresh read=v2 task=a age 1000", summary(rc), rc.ReadAgeMS)
	}

	// In another task's slot the older read is kept, and the newest decides.
	first = m.reading(a, "deals", []string{"2"}, c.tick())
	second = m.reading(none, "deals", []string{"2"}, c.tick())
	m.learned(second, map[string]string{"2": "v2"})
	m.learned(first, map[string]string{"2": "v1"})
	wantCheck(t, m, c, a, "deals", "2", "v2", "fresh read=v2")
}

func TestRememberExpires(t *testing.T) {
	m, c := testMemory(1000)
	a := party{"agent", "a"}

	readAt := c.tick()
	m.learned(m.reading(a, "deals", []string{"1"}, readAt), map[string]string{"1": "v1"})
	m.learned(m.reading(a, "notes", []string{"1"}, readAt), map[string]string{"1": "v1"})
	for _, tc := range []struct {
		table string
		at    time.Duration
		want  string
	}{
		{"notes", time.Minute, decide.ReadFresh},
		{"deals", time.Minute + 1, decide.ReadFresh},
		{"notes", time.Minute + 1, decide.ReadNone},
		{"deals", time.Hour, decide.ReadFresh},
		{"deals", time.Hour + 1, decide.ReadNone},
	} {
		if got := m.check(a, tc.table, "1", "v1", readAt.Add(tc.at)).Result; got != tc.want {
			t.Errorf("%s read %v ago: %s, want %s", tc.table, tc.at, got, tc.want)
		}
	}
	if n := m.len(); n != 0 {
		t.Errorf("len = %d after both entries expired, want 0", n)
	}

	// A lookup that answers after the table's remember has passed keeps
	// nothing.
	tk := m.reading(a, "notes", []string{"2"}, c.tick())
	c.advance(2 * time.Minute)
	m.learned(tk, map[string]string{"2": "v1"})
	if n := m.len(); n != 0 {
		t.Errorf("len = %d after a late lookup, want 0", n)
	}
	if n := m.evicted(); n != 0 {
		t.Errorf("evicted = %d, want 0: expiry is not eviction", n)
	}
}

func TestLimits(t *testing.T) {
	m, c := testMemory(8) // a role keeps 2
	p := func(i int) party { return party{fmt.Sprintf("r%d", i), ""} }

	for i := range 4 {
		remembered(m, c, p(i), "deals", "1", "v1")
		remembered(m, c, p(i), "deals", "2", "v1")
	}
	if n, e := m.len(), m.evicted(); n != 8 || e != 0 {
		t.Fatalf("len, evicted = %d, %d; want 8, 0", n, e)
	}

	// The ninth throws away the one read longest ago.
	remembered(m, c, p(4), "deals", "1", "v1")
	if n, e := m.len(), m.evicted(); n != 8 || e != 1 {
		t.Errorf("len, evicted = %d, %d; want 8, 1", n, e)
	}
	if e := m.evicted(); e != 0 {
		t.Errorf("evicted again = %d, want 0", e)
	}
	wantCheck(t, m, c, p(0), "deals", "1", "v1", decide.ReadNone)
	wantCheck(t, m, c, p(0), "deals", "2", "v1", "fresh read=v1")

	// A role's third throws away that role's oldest, and nothing of
	// another's.
	remembered(m, c, p(1), "deals", "3", "v1")
	if n, e := m.len(), m.evicted(); n != 8 || e != 1 {
		t.Errorf("len, evicted = %d, %d; want 8, 1", n, e)
	}
	wantCheck(t, m, c, p(1), "deals", "1", "v1", decide.ReadNone)
	wantCheck(t, m, c, p(1), "deals", "2", "v1", "fresh read=v1")
	wantCheck(t, m, c, p(1), "deals", "3", "v1", "fresh read=v1")
	for _, key := range []string{"1", "2"} {
		wantCheck(t, m, c, p(2), "deals", key, "v1", "fresh read=v1")
	}

	// What had expired anyway is not counted when the limits throw it away.
	m, c = testMemory(8)
	for i := range 4 {
		remembered(m, c, p(i), "notes", "1", "v1")
		remembered(m, c, p(i), "notes", "2", "v1")
	}
	c.advance(2 * time.Minute)
	remembered(m, c, p(4), "deals", "1", "v1")
	if n, e := m.len(), m.evicted(); n != 8 || e != 0 {
		t.Errorf("after expiry: len, evicted = %d, %d; want 8, 0", n, e)
	}
}

func TestOtherRoleIsUntouched(t *testing.T) {
	m, c := testMemory(1000)
	y, yNone := party{"Y", "t"}, party{"Y", ""}
	remembered(m, c, y, "deals", "1", "v1")
	remembered(m, c, yNone, "deals", "2", "v1")
	remembered(m, c, y, "notes", "1", "v1")

	at := memoryStart.Add(50 * time.Second) // after all of X's calls, before the notes entry expires
	checks := []struct {
		p                   party
		table, key, version string
	}{
		{y, "deals", "1", "v1"},
		{y, "deals", "1", "v2"},
		{y, "deals", "2", "v1"},
		{yNone, "deals", "1", "v1"},
		{y, "deals", "3", "v1"},
		{y, "notes", "1", "v1"},
	}
	results := func() []string {
		var out []string
		for _, ch := range checks {
			out = append(out, summary(m.check(ch.p, ch.table, ch.key, ch.version, at)))
		}
		return out
	}
	want := fmt.Sprint(results())
	same := func(step string) {
		t.Helper()
		if got := fmt.Sprint(results()); got != want {
			t.Errorf("after %s: Y's checks %s, want %s", step, got, want)
		}
	}

	for _, task := range []string{"", "t", "Y", "x"} {
		x := party{"X", task}
		tk := m.reading(x, "deals", []string{"1", "2", "3"}, c.tick())
		tkNotes := m.reading(x, "notes", nil, c.tick())
		same("X's pending reads, task " + task)
		m.learned(tk, map[string]string{"1": "v9", "2": "v9", "3": "v9"})
		m.learned(tkNotes, map[string]string{"1": "v9"})
		same("X's learned reads, task " + task)
		m.dropped(m.reading(x, "deals", nil, c.tick()))
		for _, scope := range []struct{ table, key string }{{"deals", "1"}, {"deals", ""}, {"", ""}} {
			c.tick()
			mk := m.writing("X", scope.table, scope.key)
			same(fmt.Sprintf("X's mark %q %q", scope.table, scope.key))
			m.refreshed(mk, "v9")
			m.released(m.writing("X", scope.table, scope.key))
			m.forgotten(m.writing("X", scope.table, scope.key))
			same(fmt.Sprintf("X's marks ended, %q %q", scope.table, scope.key))
		}
		m.refreshedRow("X", "deals", "1", "v9")
		m.forget("X", "deals", "1")
		m.forget("X", "deals", "")
		m.forget("X", "", "")
		same("X's forget, task " + task)
	}
}

// The tests below pin the rule of refreshed and refreshedRow: a refresh's
// version was seen at some moment between the making of the write's mark and
// the refresh, not when the refresh arrived, and two lookups can be delivered
// in either order. When the role knows the row by another version inside that
// span, nothing orders the two, and the row is forgotten. Each order is paired
// with the same calls without the disagreement, where the refresh must stay.

// Order A: two writes of the role to one row from two connections, both out
// before either refresh arrives; the refreshes are delivered in either order.
// Only the role changed the row.
func TestOwnWritesRefreshedInEitherOrderAreNotStale(t *testing.T) {
	for _, laterFirst := range []bool{true, false} {
		m, c := testMemory(1000)
		a := party{"agent", "a"}
		remembered(m, c, a, "deals", "1", "v2")
		c.tick()
		first := m.writing("agent", "deals", "1") // commits v3
		c.tick()
		second := m.writing("agent", "deals", "1") // commits v4
		c.tick()
		if laterFirst {
			m.refreshed(second, "v4")
			c.tick()
			m.refreshed(first, "v3") // its lookup ran before the second write committed
		} else {
			m.refreshed(first, "v3")
			c.tick()
			m.refreshed(second, "v4")
		}
		wantCheck(t, m, c, a, "deals", "1", "v4", decide.ReadNone)
	}
}

// Without the overlap: a write refreshed before the next is sent stays.
func TestOwnWritesOneAfterAnotherStayFresh(t *testing.T) {
	m, c := testMemory(1000)
	a := party{"agent", "a"}
	remembered(m, c, a, "deals", "1", "v1")
	c.tick()
	mk := m.writing("agent", "deals", "1")
	c.tick()
	m.refreshed(mk, "v2")
	wantCheck(t, m, c, a, "deals", "1", "v2", "fresh read=v2 task=a")
	c.tick()
	mk = m.writing("agent", "deals", "1")
	c.tick()
	m.refreshed(mk, "v3")
	wantCheck(t, m, c, a, "deals", "1", "v3", "fresh read=v3 task=a")
}

// Order B: a read by the role ends while its write is out, after someone else
// changed the row past the refresh's version, and is learned before the
// refresh. Nobody changed the row after that read. With the write's own
// version instead, the refresh stays.
func TestReadDuringOwnWriteLearnedBeforeItsRefreshIsNotStale(t *testing.T) {
	for _, tc := range []struct{ seen, want string }{
		{"v4", decide.ReadNone},
		{"v3", "fresh read=v3 task=b"},
	} {
		m, c := testMemory(1000)
		b := party{"agent", "b"}
		remembered(m, c, b, "deals", "1", "v2")
		c.tick()
		mk := m.writing("agent", "deals", "1") // commits v3; the refresh lookup sees v3; then someone may write v4
		remembered(m, c, b, "deals", "1", tc.seen)
		c.tick()
		m.refreshed(mk, "v3")
		wantCheck(t, m, c, b, "deals", "1", tc.seen, tc.want)
	}
}

// Order C: order B with the read's lookup answering after the refresh, which
// already keeps it out. With the write's own version, the refresh stays.
func TestReadDuringOwnWriteLearnedAfterItsRefreshIsNotStale(t *testing.T) {
	for _, tc := range []struct{ seen, want string }{
		{"v4", decide.ReadNone},
		{"v3", "fresh read=v3 task=b"},
	} {
		m, c := testMemory(1000)
		b := party{"agent", "b"}
		remembered(m, c, b, "deals", "1", "v2")
		c.tick()
		mk := m.writing("agent", "deals", "1")
		tk := m.reading(b, "deals", []string{"1"}, c.tick())
		c.tick()
		m.refreshed(mk, "v3")
		m.learned(tk, map[string]string{"1": tc.seen})
		wantCheck(t, m, c, b, "deals", "1", tc.seen, tc.want)
	}
}

// Order D: order B for an INSERT with a key, refreshed by refreshedRow while
// the INSERT's table mark is out. With the INSERT's own version, the refresh
// stays.
func TestReadDuringOwnInsertIsNotStale(t *testing.T) {
	for _, tc := range []struct{ seen, want string }{
		{"v4", decide.ReadNone},
		{"v3", "fresh read=v3 task=b"},
	} {
		m, c := testMemory(1000)
		b := party{"agent", "b"}
		remembered(m, c, b, "deals", "1", "v2")
		c.tick()
		tm := m.writing("agent", "deals", "")
		remembered(m, c, b, "deals", "1", tc.seen)
		c.tick()
		m.refreshedRow("agent", "deals", "1", "v3")
		m.released(tm)
		wantCheck(t, m, c, b, "deals", "1", tc.seen, tc.want)
	}
}

// The span is closed at both ends: a read that ended at the very making of
// the mark, or at the very time of the refresh, cannot be ordered against it;
// one a moment before or after can.
func TestRefreshSpanIncludesItsEnds(t *testing.T) {
	b := party{"agent", "b"}
	setup := func() (*readMemory, *testClock) {
		m, c := testMemory(1000)
		remembered(m, c, b, "deals", "1", "v2")
		c.tick()
		return m, c
	}

	// Ended as the mark was made, learned before the refresh.
	m, c := setup()
	mk := m.writing("agent", "deals", "1")
	m.learned(m.reading(b, "deals", []string{"1"}, mk.since), map[string]string{"1": "v4"})
	c.tick()
	m.refreshed(mk, "v3")
	wantCheck(t, m, c, b, "deals", "1", "v4", decide.ReadNone)

	// Ended a moment before the mark: before the write, and stamped over.
	m, c = setup()
	m.learned(m.reading(b, "deals", []string{"1"}, c.now().Add(-time.Nanosecond)), map[string]string{"1": "v4"})
	mk = m.writing("agent", "deals", "1")
	c.tick()
	m.refreshed(mk, "v3")
	wantCheck(t, m, c, b, "deals", "1", "v3", "fresh read=v3 task=b")

	// Ended at the very time of the refresh, learned after it.
	m, c = setup()
	mk = m.writing("agent", "deals", "1")
	refreshAt := c.tick()
	tk := m.reading(b, "deals", []string{"1"}, refreshAt)
	m.refreshed(mk, "v3")
	m.learned(tk, map[string]string{"1": "v4"})
	wantCheck(t, m, c, b, "deals", "1", "v4", decide.ReadNone)

	// Ended a moment after the refresh: after it, and kept.
	m, c = setup()
	mk = m.writing("agent", "deals", "1")
	refreshAt = c.tick()
	m.refreshed(mk, "v3")
	m.learned(m.reading(b, "deals", []string{"1"}, refreshAt.Add(time.Nanosecond)), map[string]string{"1": "v4"})
	wantCheck(t, m, c, b, "deals", "1", "v4", "fresh read=v4 task=b")
}

// What cannot be ordered is forgotten up to the end of the span, not up to
// when the late lookup answered: a read that ended after the span was read
// after the write and its refresh, and stays, learned before or after.
func TestReadAfterTheSpanSurvivesAReadInsideIt(t *testing.T) {
	m, c := testMemory(1000)
	a, b := party{"agent", "a"}, party{"agent", "b"}
	remembered(m, c, b, "deals", "1", "v2")
	c.tick()
	mk := m.writing("agent", "deals", "1")
	inside := m.reading(b, "deals", []string{"1"}, c.tick())
	c.tick()
	m.refreshed(mk, "v3")
	learnedFirst := m.reading(a, "deals", []string{"1"}, c.tick())
	learnedAfter := m.reading(party{"agent", "c"}, "deals", []string{"1"}, c.tick())
	m.learned(learnedFirst, map[string]string{"1": "v4"})
	m.learned(inside, map[string]string{"1": "v5"})
	m.learned(learnedAfter, map[string]string{"1": "v4"})
	wantCheck(t, m, c, a, "deals", "1", "v4", "fresh read=v4 task=a")
	wantCheck(t, m, c, party{"agent", "c"}, "deals", "1", "v4", "fresh read=v4 task=c")
	wantCheck(t, m, c, b, "deals", "1", "v4", decide.ReadNone)
}

// A clock that stands still across a lookup's round trip gives two reads of
// one slot the same time; if they found different versions, only one could be
// kept and neither is known to be the newer. The row is forgotten up to that
// time, in every task, so no older read is left to decide; a read the clock
// stood still for while its lookup was out is kept out too.
func TestSameSlotSameTimeOtherVersionIsForgotten(t *testing.T) {
	m, c := testMemory(1000)
	a, none := party{"agent", "a"}, party{"agent", ""}
	remembered(m, c, none, "deals", "1", "v0")
	at := c.tick()
	older := m.reading(a, "deals", []string{"1"}, at.Add(-time.Nanosecond))
	m.learned(m.reading(a, "deals", []string{"1"}, at), map[string]string{"1": "v1"})
	m.learned(m.reading(a, "deals", []string{"1"}, at), map[string]string{"1": "v2"})
	m.learned(older, map[string]string{"1": "v0"})
	wantCheck(t, m, c, a, "deals", "1", "v2", decide.ReadNone)

	// The same version at the same time is no disagreement.
	at = c.tick()
	m.learned(m.reading(a, "deals", []string{"2"}, at), map[string]string{"2": "v1"})
	m.learned(m.reading(a, "deals", []string{"2"}, at), map[string]string{"2": "v1"})
	wantCheck(t, m, c, a, "deals", "2", "v1", "fresh read=v1 task=a")

	// Two tasks at the same time keep both; check prefers the one that
	// agrees with the row now.
	at = c.tick()
	m.learned(m.reading(a, "deals", []string{"3"}, at), map[string]string{"3": "v1"})
	m.learned(m.reading(none, "deals", []string{"3"}, at), map[string]string{"3": "v2"})
	wantCheck(t, m, c, a, "deals", "3", "v2", "fresh read=v2")
	wantCheck(t, m, c, a, "deals", "3", "v1", "fresh read=v1 task=a")
}

// Reads tied in time are chosen in a fixed order, so the same calls record the
// same read_check every time: the one that agrees with the row now, then the
// writer's own task, then no task, then by task name.
func TestTiedReadsAnswerTheSameEveryTime(t *testing.T) {
	cases := []struct {
		p       party
		version string
		want    string
	}{
		{party{"agent", ""}, "v2", "fresh read=v2 task=b"},
		{party{"agent", ""}, "v9", "stale read=v1"},
		{party{"agent", "c"}, "v9", "stale read=v2 task=c"},
		{party{"agent", "d"}, "v9", "stale read=v1"},
		{party{"agent", "a"}, "v1", "fresh read=v1 task=a"},
	}
	seen := make([]map[string]bool, len(cases))
	for i := range seen {
		seen[i] = map[string]bool{}
	}
	for range 200 {
		m, c := testMemory(1000)
		at := c.tick()
		for task, version := range map[string]string{"": "v1", "a": "v1", "b": "v2", "c": "v2", "e": "v3"} {
			m.learned(m.reading(party{"agent", task}, "deals", []string{"1"}, at), map[string]string{"1": version})
		}
		for i, tc := range cases {
			seen[i][summary(m.check(tc.p, "deals", "1", tc.version, at.Add(time.Second)))] = true
		}
	}
	for i, tc := range cases {
		if len(seen[i]) != 1 || !seen[i][tc.want] {
			t.Errorf("check(%v, %q) gave %v over 200 runs, want only %q", tc.p, tc.version, seen[i], tc.want)
		}
	}
}

// The note that a write was judged stale goes when an own write refreshes the
// row: a task that never read the row is not compared with what the refresh
// stamped.
func TestOwnWriteRefreshDropsTheStaleNote(t *testing.T) {
	m, c := testMemory(1000)
	a, b := party{"agent", "a"}, party{"agent", "b"}
	remembered(m, c, a, "deals", "1", "v1")
	wantCheck(t, m, c, a, "deals", "1", "v2", "stale read=v1 task=a")
	c.tick()
	mk := m.writing("agent", "deals", "1") // the stale write runs (shadow)
	c.tick()
	m.refreshed(mk, "v3")
	// someone else writes v4
	wantCheck(t, m, c, b, "deals", "1", "v4", decide.ReadNone)
	wantCheck(t, m, c, a, "deals", "1", "v4", "stale read=v3 task=a")
}

// A refresh makes its entries the newest, so the limits throw away another.
func TestRefreshedEntryIsNotTheOldest(t *testing.T) {
	m, c := testMemory(8) // a role keeps 2
	p := party{"agent", ""}
	remembered(m, c, p, "deals", "1", "v1")
	remembered(m, c, p, "deals", "2", "v1")
	c.tick()
	mk := m.writing("agent", "deals", "1")
	c.tick()
	m.refreshed(mk, "v2")
	remembered(m, c, p, "deals", "3", "v1")
	if e := m.evicted(); e != 1 {
		t.Errorf("evicted = %d, want 1", e)
	}
	wantCheck(t, m, c, p, "deals", "1", "v2", "fresh read=v2")
	wantCheck(t, m, c, p, "deals", "2", "v1", decide.ReadNone)
	wantCheck(t, m, c, p, "deals", "3", "v1", "fresh read=v1")
}

// refreshedRow with no mark covering the row cannot bound when its version
// was seen: a row the role knows by another version is forgotten.
func TestRefreshedRowWithoutAMarkForgetsADisagreeingRow(t *testing.T) {
	m, c := testMemory(1000)
	b := party{"agent", "b"}
	remembered(m, c, b, "deals", "1", "v2")
	c.tick()
	m.refreshedRow("agent", "deals", "1", "v3")
	wantCheck(t, m, c, b, "deals", "1", "v3", decide.ReadNone)
}

// A forget of a table or a role takes its entries out of the counts at once,
// and the entries held stay within the limits after it: the cleared ones go
// as new ones come.
func TestClearedEntriesLeaveTheCountsAtOnce(t *testing.T) {
	m, c := testMemory(400) // a role keeps 100
	r0, r1 := party{"r0", ""}, party{"r1", ""}
	keys := func(from, n int) ([]string, map[string]string) {
		ks, vs := make([]string, n), map[string]string{}
		for i := range ks {
			ks[i] = strconv.Itoa(from + i)
			vs[ks[i]] = "v1"
		}
		return ks, vs
	}
	ks, vs := keys(0, 100)
	m.learned(m.reading(r0, "deals", ks, c.tick()), vs)
	m.learned(m.reading(r1, "deals", ks, c.tick()), vs)
	m.forget("r0", "", "")
	if n := m.len(); n != 100 {
		t.Errorf("len = %d after forgetting r0's 100, want 100", n)
	}
	if held := m.held(); held != 200-clearSweep {
		t.Errorf("held = %d, want %d: a clear takes out %d at once", held, 200-clearSweep, clearSweep)
	}
	wantCheck(t, m, c, r0, "deals", "5", "v1", decide.ReadNone)
	wantCheck(t, m, c, r1, "deals", "5", "v1", "fresh read=v1")

	// r0 reads 100 again, one row at a time: each new entry takes a cleared
	// one out, so no eviction is needed and nothing of r1 goes.
	for i := 100; i < 200; i++ {
		remembered(m, c, r0, "deals", strconv.Itoa(i), "v1")
	}
	if n, held, e := m.len(), m.held(), m.evicted(); n != 200 || held != 200 || e != 0 {
		t.Errorf("len, held, evicted = %d, %d, %d; want 200, 200, 0", n, held, e)
	}
	// One more for r0 is past its quarter: its oldest live entry goes.
	m.learned(m.reading(r0, "deals", []string{"x"}, c.tick()), map[string]string{"x": "v1"})
	if n, e := m.len(), m.evicted(); n != 200 || e != 1 {
		t.Errorf("len, evicted = %d, %d; want 200, 1", n, e)
	}
	wantCheck(t, m, c, r0, "deals", "100", "v1", decide.ReadNone)
	wantCheck(t, m, c, r0, "deals", "x", "v1", "fresh read=v1")
}

// held counts the entries held, live or cleared and not yet taken out.
func (m *readMemory) held() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.all.len()
}

// records counts what the memory holds besides live entries: login roles,
// tables, row records, rows with entries, tickets not ended, marks not ended,
// times of clears kept, and cleared entries not yet taken out.
func (m *readMemory) records() (roles, tables, rows, tickets, marks, clears, cleared int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cleared = m.all.len() - m.live + len(m.graving)
	for _, rr := range m.roles {
		roles++
		tickets += len(rr.pending)
		marks += len(rr.marks)
		clears += len(rr.clears)
		for _, tr := range rr.tables {
			tables++
			marks += len(tr.marks)
			for _, row := range tr.rows {
				rows++
				marks += len(row.marks)
			}
			if tr.known != nil {
				rows += len(tr.known.rows)
			}
		}
	}
	return
}

func wantNoRecords(t *testing.T, m *readMemory) {
	t.Helper()
	if roles, tables, rows, tickets, marks, clears, cleared := m.records(); roles+tables+rows+tickets+marks+clears+cleared != 0 {
		t.Errorf("records: roles %d, tables %d, rows %d, tickets %d, marks %d, clears %d, cleared %d; want none", roles, tables, rows, tickets, marks, clears, cleared)
	}
	if n := m.len(); n != 0 {
		t.Errorf("len = %d, want 0", n)
	}
}

func TestBookkeepingEndsWithWhatItServes(t *testing.T) {
	m, c := testMemory(1000)
	a, b := party{"agent", "a"}, party{"agent", "b"}

	// Times of clears are kept only while a ticket that could be cut by them
	// is pending.
	m.forget("agent", "deals", "1")
	if _, _, _, _, _, clears, _ := m.records(); clears != 0 {
		t.Errorf("a forget with no read pending kept %d times", clears)
	}
	tk := m.reading(a, "deals", []string{"1"}, c.tick())
	keyless := m.reading(b, "notes", nil, c.tick())
	c.tick()
	m.forget("agent", "deals", "1")
	m.forget("agent", "notes", "7")
	if _, _, _, _, _, clears, _ := m.records(); clears != 2 {
		t.Errorf("clears = %d while reads are pending, want 2", clears)
	}
	m.dropped(tk)
	m.learned(keyless, map[string]string{"7": "v1"})
	wantNoRecords(t, m)

	// A note of a row judged stale goes with the row's entries.
	remembered(m, c, a, "deals", "2", "v1")
	remembered(m, c, b, "deals", "2", "v2")
	wantCheck(t, m, c, a, "deals", "2", "v2", "stale read=v1 task=a")
	m.forget("agent", "deals", "2")
	wantNoRecords(t, m)

	// Marks and tickets end, whatever ends them.
	for _, scope := range []struct{ table, key string }{{"deals", "3"}, {"deals", ""}, {"", ""}} {
		m.released(m.writing("agent", scope.table, scope.key))
		m.forgotten(m.writing("agent", scope.table, scope.key))
		m.refreshed(m.writing("agent", scope.table, scope.key), "v1")
	}
	m.dropped(m.reading(a, "deals", []string{}, c.tick()))
	m.learned(m.reading(a, "deals", []string{"4", "4"}, c.tick()), map[string]string{})
	wantNoRecords(t, m)

	// Entries thrown away by expiry take their records.
	remembered(m, c, a, "notes", "5", "v1")
	c.advance(2 * time.Minute)
	m.check(a, "notes", "5", "v1", c.now())
	wantNoRecords(t, m)
}

// TestConcurrentUse has many goroutines mix every call at random, and checks
// the memory's invariants as it goes (consistent). One more goroutine reaches
// the limits and the deferred removal by construction while they run, so the
// run cannot miss what it is there to exercise.
func TestConcurrentUse(t *testing.T) {
	m := newReadMemory(memoryTables, 200, time.Now) // a role keeps 50
	roles := []string{"r0", "r1", "r2", "r3", "r4", "r5"}
	tasks := []string{"", "a", "b"}
	tables := []string{"deals", "notes"}
	var evicted atomic.Int64
	var sawCleared atomic.Bool

	var wg sync.WaitGroup
	for g := range 12 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(g), 7))
			role := roles[g%len(roles)]
			var tickets []*ticket
			var marks []*writeMark
			pop := func(n int) int { return r.IntN(n) }
			for range 3000 {
				p := party{role, tasks[r.IntN(len(tasks))]}
				table := tables[r.IntN(len(tables))]
				key := strconv.Itoa(r.IntN(40))
				version := "v" + strconv.Itoa(r.IntN(3))
				switch r.IntN(13) {
				case 0:
					keys := []string{key}
					for range r.IntN(40) {
						keys = append(keys, strconv.Itoa(r.IntN(40)))
					}
					tickets = append(tickets, m.reading(p, table, keys, time.Now()))
				case 1:
					tickets = append(tickets, m.reading(p, table, nil, time.Now()))
				case 2, 3:
					if len(tickets) > 0 {
						i := pop(len(tickets))
						if r.IntN(4) == 0 {
							m.dropped(tickets[i])
						} else {
							versions := map[string]string{}
							for _, k := range tickets[i].keys {
								if r.IntN(8) != 0 {
									versions[k] = version
								}
							}
							if tickets[i].keyless {
								versions[key] = version
							}
							m.learned(tickets[i], versions)
						}
						tickets = append(tickets[:i], tickets[i+1:]...)
					}
				case 4, 5:
					m.check(p, table, key, version, time.Now())
				case 6:
					switch r.IntN(3) {
					case 0:
						marks = append(marks, m.writing(role, table, key))
					case 1:
						marks = append(marks, m.writing(role, table, ""))
					default:
						marks = append(marks, m.writing(role, "", ""))
					}
				case 7, 8:
					if len(marks) > 0 {
						i := pop(len(marks))
						switch r.IntN(3) {
						case 0:
							m.refreshed(marks[i], version)
						case 1:
							m.forgotten(marks[i])
						default:
							m.released(marks[i])
						}
						marks = append(marks[:i], marks[i+1:]...)
					}
				case 9:
					m.refreshedRow(role, table, key, version)
				case 10:
					switch r.IntN(3) {
					case 0:
						m.forget(role, table, key)
					case 1:
						m.forget(role, table, "")
					default:
						m.forget(role, "", "")
					}
				case 11:
					evicted.Add(m.evicted())
					if held, live := m.counts(); held > live {
						sawCleared.Store(true)
					}
				default:
					if err := m.consistent(); err != nil {
						t.Error(err)
						return
					}
				}
			}
			for _, tk := range tickets {
				m.dropped(tk)
			}
			for _, mk := range marks {
				m.released(mk)
			}
		})
	}
	// Its reads end an hour ahead, so every other role's entries are older
	// and the global limit never takes one of its. Each round reads 10 rows
	// more than a role's quarter, so at least 10 are evicted, then forgets
	// the role: its quarter is detached, clearSweep of it taken out at once,
	// and the rest left held for later additions to take out.
	wg.Go(func() {
		fill := party{"fill", ""}
		for round := range 20 {
			keys, versions := someRows(fmt.Sprintf("%d-", round), m.perRole+10)
			m.learned(m.reading(fill, "deals", keys, time.Now().Add(time.Hour)), versions)
			if left := m.forgetLeaving(fill.role); left != m.perRole-clearSweep {
				t.Errorf("round %d: the forget left %d cleared entries held, want %d", round, left, m.perRole-clearSweep)
				return
			}
			sawCleared.Store(true)
		}
	})
	wg.Wait()
	if err := m.consistent(); err != nil {
		t.Error(err)
	}
	if evicted.Load()+m.evicted() < 200 || !sawCleared.Load() {
		t.Errorf("evicted %d, saw cleared entries held %v: the run did not reach the limits or the deferred removal", evicted.Load(), sawCleared.Load())
	}

	for _, role := range append(roles, "fill") {
		m.forget(role, "", "")
	}
	m.drain()
	wantNoRecords(t, m)
}

// someRows makes n keys starting with prefix and a version v1 for each.
func someRows(prefix string, n int) ([]string, map[string]string) {
	keys, versions := make([]string, n), make(map[string]string, n)
	for i := range keys {
		keys[i] = prefix + strconv.Itoa(i)
		versions[keys[i]] = "v1"
	}
	return keys, versions
}

// forgetLeaving forgets role's entries as forget does, and reports, in the
// same hold of the lock, how many of them are left held to be taken out by
// later additions.
func (m *readMemory) forgetLeaving(role string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	rr := m.roles[role]
	if rr == nil {
		return 0
	}
	m.clear(rr, "", "", m.now())
	return rr.all.len() - rr.live
}

// queued reports how many places the queue of roles holding cleared entries
// has, and how many role records there are.
func (m *readMemory) queued() (places, roles int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.graving), len(m.roles)
}

// takenSoFar reports how many entries have been taken out of the order by
// read time.
func (m *readMemory) takenSoFar() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.taken
}

// counts reports the entries held, live or cleared, and the live ones, at
// one moment.
func (m *readMemory) counts() (held, live int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.all.len(), m.live
}

// drain takes out every cleared entry still held, as enough later additions
// would.
func (m *readMemory) drain() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweep(&roleMemory{}, m.all.len()+1)
}

// consistent checks what the memory's design rests on: both orders by read
// time are heaps and every entry knows its place in them; len and the
// per-role counts are exact about live entries, each of which is where its
// role, table and row say; and the entries held, live or cleared, stay within
// maxKeys and each role's quarter, so an eviction never meets a cleared one.
func (m *readMemory) consistent() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ordered := func(h *byRead) error {
		for i, e := range h.es {
			if e.pos[h.slot] != i {
				return fmt.Errorf("heap %d: the entry at %d says it is at %d", h.slot, i, e.pos[h.slot])
			}
			if i > 0 && h.less(i, (i-1)/2) {
				return fmt.Errorf("heap %d: the entry at %d was read before its parent", h.slot, i)
			}
		}
		return nil
	}
	if err := ordered(&m.all); err != nil {
		return err
	}
	if m.all.len() > m.maxKeys {
		return fmt.Errorf("%d entries held, over maxKeys %d", m.all.len(), m.maxKeys)
	}
	live := 0
	for _, e := range m.all.es {
		if m.roles[e.role.name] != e.role {
			return fmt.Errorf("an entry of %s is held after its role's record went", e.role.name)
		}
		if !e.set.cleared {
			live++
		}
	}
	if live != m.live {
		return fmt.Errorf("len says %d, %d entries are live", m.live, live)
	}
	for name, rr := range m.roles {
		if err := ordered(&rr.all); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if rr.all.len() > m.perRole {
			return fmt.Errorf("%s holds %d entries, over its quarter %d", name, rr.all.len(), m.perRole)
		}
		n := 0
		for _, e := range rr.all.es {
			if e.set.cleared {
				continue
			}
			n++
			tr := rr.tables[e.table]
			if tr == nil || tr.known != e.set || e.set.rows[e.key] != e.row || !slices.Contains(e.row.entries, e) || e.set.entries[e.inSet] != e {
				return fmt.Errorf("%s: a live entry of %s %q is not where its records say", name, e.table, e.key)
			}
		}
		if n != rr.live {
			return fmt.Errorf("%s: its count says %d, %d entries are live", name, rr.live, n)
		}
		if rr.all.len() > rr.live && !rr.queued {
			return fmt.Errorf("%s holds cleared entries and is not queued", name)
		}
	}
	queued := map[*roleMemory]bool{}
	for _, g := range m.graving {
		if queued[g] || !g.queued || m.roles[g.name] != g {
			return fmt.Errorf("%s has a place in the queue it should not: twice, unmarked, or after its record went", g.name)
		}
		queued[g] = true
	}
	return nil
}

// The memory has one lock, and the read path takes it before the client's
// next statement. The tests below bound the work a call does under the lock
// by counting it (entries taken out of the order by read time, and batches of
// learned), not by a clock, so a slow runner cannot fail them; they log the
// times they measured.

// fillRoles fills m with n entries of deals for each of roles, from reads a
// microsecond apart, oldest first, and returns when the last read ended.
func fillRoles(m *readMemory, roles []string, n int, start time.Time) time.Time {
	at := start
	keys := make([]string, 1000)
	for _, role := range roles {
		for i := 0; i < n; i += len(keys) {
			versions := make(map[string]string, len(keys))
			for j := range keys {
				keys[j] = strconv.Itoa(i + j)
				versions[keys[j]] = "v1"
			}
			at = at.Add(time.Microsecond)
			m.learned(m.reading(party{role, ""}, "deals", keys, at), versions)
		}
	}
	return at
}

// longestWait runs f while another goroutine takes the memory's lock again
// and again, and returns how long f took and the longest the other waited.
func longestWait(m *readMemory, f func()) (took, waited time.Duration) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			start := time.Now()
			m.len()
			waited = max(waited, time.Since(start))
		}
	})
	time.Sleep(time.Millisecond)
	start := time.Now()
	f()
	took = time.Since(start)
	close(done)
	wg.Wait()
	return took, waited
}

// A role-wide forget at the default limit, with one role at its quarter, takes
// at most clearSweep entries out under the lock, whatever the scope's size.
func TestForgetOfALargeScopeHoldsTheLockBriefly(t *testing.T) {
	m := newReadMemory(memoryTables, 200_000, (&testClock{t: memoryStart}).now)
	fillRoles(m, []string{"r0", "r1", "r2", "r3"}, 50_000, memoryStart)
	if n := m.len(); n != 200_000 {
		t.Fatalf("len = %d, want 200000", n)
	}
	before := m.takenSoFar()
	took, waited := longestWait(m, func() { m.forget("r0", "", "") })
	t.Logf("forget of 50000 entries: %v, longest wait of another call %v", took, waited)
	if taken := m.takenSoFar() - before; taken > clearSweep {
		t.Errorf("the forget took %d entries out under the lock, want at most %d", taken, clearSweep)
	}
	if n := m.len(); n != 150_000 {
		t.Errorf("len = %d after the forget, want 150000", n)
	}
}

// At the allowed maximum (stale.max_keys 10,000,000, one role at its quarter)
// the same calls hold the lock for bounded work. It needs a few GiB, so it runs
// only when STET_MEMORY_AT_MAX is set.
func TestLockHoldAtTheAllowedMaximum(t *testing.T) {
	if os.Getenv("STET_MEMORY_AT_MAX") == "" {
		t.Skip("STET_MEMORY_AT_MAX not set")
	}
	m := newReadMemory(memoryTables, 10_000_000, (&testClock{t: memoryStart}).now)
	last := fillRoles(m, []string{"r0"}, 2_500_000, memoryStart)
	keys := make([]string, 1000)
	versions := map[string]string{}
	for j := range keys {
		keys[j] = "new" + strconv.Itoa(j)
		versions[keys[j]] = "v1"
	}
	tk := m.reading(party{"r0", ""}, "deals", keys, last.Add(time.Second))
	took, waited := longestWait(m, func() { m.learned(tk, versions) })
	t.Logf("at the maximum, learned of 1000 keys with 1000 evictions: %v, longest wait of another call %v (evicted %d)", took, waited, m.evicted())
	took, waited = longestWait(m, func() { m.forget("r0", "", "") })
	t.Logf("at the maximum, forget of 2500000 entries: %v, longest wait of another call %v", took, waited)
	tk = m.reading(party{"r0", ""}, "deals", keys, last.Add(2*time.Second))
	took, waited = longestWait(m, func() { m.learned(tk, versions) })
	t.Logf("at the maximum, learned of 1000 keys after it, taking cleared entries out: %v, longest wait of another call %v", took, waited)
}

// A read of 1,000 rows learned at the default limit, each new entry throwing
// away another, lets go of the lock every learnBatch keys, and takes at most
// two entries out for each key it learns in a batch (one cleared, one
// evicted).
func TestLearnedWithEvictionsHoldsTheLockBriefly(t *testing.T) {
	m := newReadMemory(memoryTables, 200_000, (&testClock{t: memoryStart}).now)
	last := fillRoles(m, []string{"r0", "r1", "r2", "r3"}, 50_000, memoryStart)
	keys, versions := someRows("new", 1000)
	tk := m.reading(party{"r1", ""}, "deals", keys, last.Add(time.Second))
	batches, done, taken := 0, 0, m.takenSoFar()
	m.afterBatch = func(_ *ticket, learned int) {
		batches++
		now := m.takenSoFar()
		if now-taken > int64(2*(learned-done)) {
			t.Errorf("a batch of %d keys took %d entries out under the lock, want at most %d", learned-done, now-taken, 2*(learned-done))
		}
		done, taken = learned, now
	}
	took, waited := longestWait(m, func() { m.learned(tk, versions) })
	t.Logf("learned of 1000 keys with 1000 evictions: %v, longest wait of another call %v", took, waited)
	if want := (1000 + learnBatch - 1) / learnBatch; batches != want {
		t.Errorf("learned took the lock %d times for 1000 keys, want %d", batches, want)
	}
	if n, e := m.len(), m.evicted(); n != 200_000 || e != 1000 {
		t.Errorf("len, evicted = %d, %d; want 200000, 1000", n, e)
	}
}

// Between two batches of learned, another call gets the lock; the read is
// still pending on every row it read, and only the keys learned so far are in.
func TestLearnedLetsGoOfTheLockBetweenBatches(t *testing.T) {
	m, c := testMemory(1000)
	p := party{"agent", "a"}
	keys, versions := someRows("k", 100)
	tk := m.reading(p, "deals", keys, c.tick())
	var gaps []int
	m.afterBatch = func(_ *ticket, learned int) {
		gaps = append(gaps, learned)
		if n := m.len(); n != learned {
			t.Errorf("after %d keys learned, len = %d", learned, n)
		}
		for _, key := range []string{keys[0], keys[len(keys)-1]} {
			if got := m.check(p, "deals", key, "v1", c.tick()).Result; got != decide.ReadPending {
				t.Errorf("after %d keys learned, check of %s = %s, want read_pending", learned, key, got)
			}
		}
	}
	m.learned(tk, versions)
	if want := []int{16, 32, 48, 64, 80, 96, 100}; !slices.Equal(gaps, want) {
		t.Errorf("learned let go of the lock after %v keys, want %v", gaps, want)
	}
	m.afterBatch = nil
	wantCheck(t, m, c, p, "deals", keys[0], "v1", "fresh read=v1 task=a")
}

// A role whose cleared entries all went by its own forget stays queued behind
// a role with many, and keeps its record until the queue reaches it; its next
// read uses the same record and does not queue it again. So the queue has at
// most one place per role record. (Z with many cleared entries at the front;
// X reads 33 rows and forgets the table twice, again and again.)
func TestClearedQueueHoldsOnePlacePerRole(t *testing.T) {
	m, c := testMemory(10_000) // a role keeps 2,500
	keys, versions := someRows("z", 2000)
	m.learned(m.reading(party{"Z", ""}, "deals", keys, c.tick()), versions)
	m.forget("Z", "", "")
	for round := range 50 {
		keys, versions := someRows(fmt.Sprintf("x%d-", round), 33)
		m.learned(m.reading(party{"X", ""}, "deals", keys, c.tick()), versions)
		m.forget("X", "deals", "")
		m.forget("X", "deals", "")
		if places, roles := m.queued(); places > roles || places > 2 {
			t.Fatalf("round %d: the queue has %d places for %d role records, want at most one each", round, places, roles)
		}
	}
	if err := m.consistent(); err != nil {
		t.Error(err)
	}
}

package gate

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
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
// span, nothing orders the two, and the row must not be called stale.

// wantNotStale checks a write by p to the row key of deals, whose version now
// is version, and fails only on stale: forgetting the row is a miss, and any
// answer but stale is allowed.
func wantNotStale(t *testing.T, m *readMemory, c *testClock, p party, key, version string) {
	t.Helper()
	if rc := m.check(p, "deals", key, version, c.tick()); rc.Result == decide.ReadStale {
		t.Errorf("check(%v, deals, %q, %q) = %s, want anything but stale", p, key, version, summary(rc))
	}
}

// Order A: two writes of the role to one row from two connections; their
// refreshes are delivered in either order. Only the role changed the row.
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
		wantNotStale(t, m, c, a, "1", "v4")
	}
}

// Order B: a read by the role ends while its write is out, after someone else
// changed the row past the refresh's version, and is learned before the
// refresh. Nobody changed the row after that read.
func TestReadDuringOwnWriteLearnedBeforeItsRefreshIsNotStale(t *testing.T) {
	m, c := testMemory(1000)
	b := party{"agent", "b"}
	remembered(m, c, b, "deals", "1", "v2")
	c.tick()
	mk := m.writing("agent", "deals", "1") // commits v3; the refresh lookup sees v3; then someone writes v4
	remembered(m, c, b, "deals", "1", "v4")
	c.tick()
	m.refreshed(mk, "v3")
	wantNotStale(t, m, c, b, "1", "v4")
}

// Order C: order B with the read's lookup answering after the refresh, when
// the refresh's time already keeps it out.
func TestReadDuringOwnWriteLearnedAfterItsRefreshIsNotStale(t *testing.T) {
	m, c := testMemory(1000)
	b := party{"agent", "b"}
	remembered(m, c, b, "deals", "1", "v2")
	c.tick()
	mk := m.writing("agent", "deals", "1")
	tk := m.reading(b, "deals", []string{"1"}, c.tick())
	c.tick()
	m.refreshed(mk, "v3")
	m.learned(tk, map[string]string{"1": "v4"})
	wantNotStale(t, m, c, b, "1", "v4")
}

// Order D: order B for an INSERT with a key, refreshed by refreshedRow while
// the INSERT's table mark is out.
func TestReadDuringOwnInsertIsNotStale(t *testing.T) {
	m, c := testMemory(1000)
	b := party{"agent", "b"}
	remembered(m, c, b, "deals", "1", "v2")
	c.tick()
	tm := m.writing("agent", "deals", "")
	remembered(m, c, b, "deals", "1", "v4")
	c.tick()
	m.refreshedRow("agent", "deals", "1", "v3")
	m.released(tm)
	wantNotStale(t, m, c, b, "1", "v4")
}

// A read inside the span that found the write's own version agrees with the
// refresh, which is kept, whether the read is learned before or after it.
func TestReadDuringOwnWriteThatAgreesKeepsTheRefresh(t *testing.T) {
	m, c := testMemory(1000)
	a, b := party{"agent", "a"}, party{"agent", "b"}
	remembered(m, c, b, "deals", "1", "v2")
	c.tick()
	mk := m.writing("agent", "deals", "1")
	remembered(m, c, b, "deals", "1", "v3")
	tk := m.reading(a, "deals", []string{"1"}, c.tick())
	c.tick()
	m.refreshed(mk, "v3")
	m.learned(tk, map[string]string{"1": "v3"})
	wantCheck(t, m, c, b, "deals", "1", "v3", "fresh read=v3 task=b")
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

// records counts what the memory holds besides entries: login roles, tables
// and rows with a record, tickets not ended, marks not ended, and times of
// clears kept.
func (m *readMemory) records() (roles, tables, rows, tickets, marks, clears int) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
		}
	}
	return
}

func wantNoRecords(t *testing.T, m *readMemory) {
	t.Helper()
	if roles, tables, rows, tickets, marks, clears := m.records(); roles+tables+rows+tickets+marks+clears != 0 {
		t.Errorf("records: roles %d, tables %d, rows %d, tickets %d, marks %d, clears %d; want none", roles, tables, rows, tickets, marks, clears)
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
	if _, _, _, _, _, clears := m.records(); clears != 0 {
		t.Errorf("a forget with no read pending kept %d times", clears)
	}
	tk := m.reading(a, "deals", []string{"1"}, c.tick())
	keyless := m.reading(b, "notes", nil, c.tick())
	c.tick()
	m.forget("agent", "deals", "1")
	m.forget("agent", "notes", "7")
	if _, _, _, _, _, clears := m.records(); clears != 2 {
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

func TestConcurrentUse(t *testing.T) {
	m := newReadMemory(memoryTables, 64, time.Now)
	roles := []string{"r0", "r1", "r2"}
	tasks := []string{"", "a", "b"}
	tables := []string{"deals", "notes"}

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(g), 7))
			role := roles[g%len(roles)]
			var tickets []*ticket
			var marks []*writeMark
			pop := func(n int) int { return r.IntN(n) }
			for range 3000 {
				p := party{role, tasks[r.IntN(len(tasks))]}
				table := tables[r.IntN(len(tables))]
				key := strconv.Itoa(r.IntN(30))
				version := "v" + strconv.Itoa(r.IntN(3))
				switch r.IntN(12) {
				case 0:
					tickets = append(tickets, m.reading(p, table, []string{key, strconv.Itoa(r.IntN(30))}, time.Now()))
				case 1:
					tickets = append(tickets, m.reading(p, table, nil, time.Now()))
				case 2, 3:
					if len(tickets) > 0 {
						i := pop(len(tickets))
						if r.IntN(4) == 0 {
							m.dropped(tickets[i])
						} else {
							m.learned(tickets[i], map[string]string{key: version})
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
					m.forget(role, table, key)
				default:
					m.evicted()
					m.len()
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
	wg.Wait()

	for _, role := range roles {
		m.forget(role, "", "")
	}
	wantNoRecords(t, m)
}

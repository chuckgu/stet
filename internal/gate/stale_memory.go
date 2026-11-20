package gate

import (
	"maps"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/stetdb/stet/internal/decide"
)

// readMemory remembers which version of which rows each login role read
// (spec-v0.2-stale 5 to 7). Safe for concurrent use; it does no I/O.
//
// An entry is one (login role, task, table, key): the version (xmin) a read of
// the row found and when that read ended. A write is compared with the newest
// entry it may share a party with, and called stale only when that entry's
// version is not the row's now. Every approximation here errs toward not
// judging: whatever cannot be ordered or is not known yet answers read_pending,
// own_write or no_read, never stale.
//
// It has one lock, and the read path takes it before the client's next
// statement, so no call holds it for work that grows with what is remembered:
// forgetting a table or a role detaches its entries at once and takes them out
// of the order by read time a few at a time after (sweep), and learned takes
// the lock for learnBatch keys at a time.
//
// What it holds is bounded by what it serves:
//   - entries, by maxKeys overall and maxKeys/4 per login role, the one read
//     longest ago going first, and by each table's remember; entries cleared
//     but not yet taken out count in neither, and with them the entries held
//     stay within the same limits (sweep);
//   - tickets and marks, by the calls that end them (learned or dropped;
//     refreshed, forgotten or released);
//   - the records of roles, tables and rows, which go as soon as they hold no
//     entry, mark or ticket, and with a row's entries the note that it was
//     judged stale;
//   - the times scopes were cleared, which are kept only while a ticket of the
//     role is pending that one of them could cut.
type readMemory struct {
	remember map[string]time.Duration
	maxKeys  int
	perRole  int
	now      func() time.Time

	mu      sync.Mutex
	roles   map[string]*roleMemory
	all     byRead        // every entry held: live, or cleared and not yet taken out
	live    int           // entries not cleared; what len and the limits count
	evicts  int64         // entries the size limits threw away since evicted last ran
	graving []*roleMemory // roles that may hold cleared entries, in the order they got them
}

const (
	// learnBatch is how many keys learned takes in one hold of the lock.
	learnBatch = 16
	// clearSweep is how many cleared entries a clear takes out at once; the
	// rest go one with each entry added after (sweep).
	clearSweep = 32
)

// newReadMemory makes an empty memory. remember maps each tracked table to how
// long its entries are kept; a table not in it keeps nothing.
func newReadMemory(remember map[string]time.Duration, maxKeys int, now func() time.Time) *readMemory {
	return &readMemory{
		remember: maps.Clone(remember),
		maxKeys:  maxKeys,
		perRole:  max(1, maxKeys/4),
		now:      now,
		roles:    map[string]*roleMemory{},
		all:      byRead{slot: 0},
	}
}

// party is who read: a login role and its task (stet.on_behalf_of; "" for none).
type party struct{ role, task string }

// ticket is a read whose versions are not known yet. It is pending until
// learned or dropped ends it, and while pending the writes it may be compared
// with answer read_pending.
type ticket struct {
	p       party
	table   string
	keys    []string
	keyless bool // the read stands for some row of table (a key from a WHERE clause)
	readAt  time.Time
	ended   bool // learned or dropped has been called; it stays pending until that call is done
}

// writeMark is a write of a login role on its way: to the row key of table, to
// some row of table (key ""), or to any table (table ""). While it is not
// ended, the role's writes it covers answer own_write.
type writeMark struct {
	role, table, key string
	since            time.Time // when it was made; the write's effects are not known before
	ended            bool
}

// roleMemory is what one login role holds.
type roleMemory struct {
	name    string
	tables  map[string]*tableMemory
	all     byRead               // the role's entries held
	live    int                  // the role's entries not cleared
	marks   []*writeMark         // on every table
	pending map[*ticket]struct{} // the role's tickets not yet ended
	clears  []clearing           // scopes cleared while a ticket was pending
	graves  []*entrySet          // cleared sets whose entries are still held, oldest first
	queued  bool                 // in readMemory.graving
}

// tableMemory is what one login role holds of one table.
type tableMemory struct {
	rows    map[string]*rowMemory // rows with a mark or a pending read
	marks   []*writeMark          // on some row of the table
	keyless []*ticket             // pending reads of some row of the table
	known   *entrySet             // the role's entries of the table; nil when none
}

// rowMemory is the marks and pending reads one login role has on one row.
type rowMemory struct {
	marks   []*writeMark
	pending []*ticket
}

// entrySet is a login role's entries of one table. Forgetting the table or
// the role detaches the set whole: its entries stop counting at once, and are
// taken out of the order by read time a few at a time after.
type entrySet struct {
	rows    map[string]*rowEntries
	entries []*entry
	cleared bool
}

// rowEntries is a login role's entries of one row, at most one per task.
type rowEntries struct {
	entries []*entry
	staleAt time.Time // when a write of the role to the row was first judged stale; zero if never
}

// entry is one remembered read.
type entry struct {
	role       *roleMemory
	set        *entrySet
	row        *rowEntries
	table, key string
	task       string
	version    string
	readAt     time.Time
	// since is the earliest moment the version may have been seen: readAt
	// for a read, and for an entry an own write refreshed, the making of the
	// write's mark, since the refresh's lookup ran at some moment between it
	// and readAt.
	since time.Time
	pos   [2]int // in readMemory.all and in roleMemory.all
	inSet int    // in set.entries
}

// clearing is a scope whose entries were forgotten or refreshed at a time: the
// row key of table, table (key ""), or every table (table "").
type clearing struct {
	table, key string
	at         time.Time
}

func (r *rowMemory) idle() bool { return len(r.marks) == 0 && len(r.pending) == 0 }

func (t *tableMemory) idle() bool {
	return len(t.rows) == 0 && len(t.marks) == 0 && len(t.keyless) == 0 && t.known == nil
}

func (r *roleMemory) idle() bool {
	return len(r.tables) == 0 && len(r.marks) == 0 && len(r.pending) == 0 && r.all.len() == 0
}

// of is task's entry; nil when it has none.
func (re *rowEntries) of(task string) *entry {
	for _, e := range re.entries {
		if e.task == task {
			return e
		}
	}
	return nil
}

// reading notes a read by p that ended at readAt and whose versions are not
// known yet: of the rows keys of table, or, with keys nil, of some row of
// table (a key taken from a WHERE clause).
//
// The caller queues the read's lookup only after reading has returned. The
// times of clears rest on it: a clear that comes before this call cuts
// nothing of this read, because its lookup reads the row after the clear.
func (m *readMemory) reading(p party, table string, keys []string, readAt time.Time) *ticket {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &ticket{p: p, table: table, keys: slices.Clone(keys), keyless: keys == nil, readAt: readAt}
	rr := m.roleOf(p.role)
	rr.pending[t] = struct{}{}
	tr := rr.tableOf(table)
	if t.keyless {
		tr.keyless = append(tr.keyless, t)
	} else {
		for _, k := range t.keys {
			if row := tr.rowOf(k); !slices.Contains(row.pending, t) {
				row.pending = append(row.pending, t)
			}
		}
	}
	m.tidyTable(rr, table) // a read of no rows holds no table
	return t
}

// learned ends t: versions maps the key of each row found, as the server
// writes it, to its xmin. A key of t that is not in versions is forgotten for
// t's party. Nothing is kept for a row, table or role cleared at or after t's
// readAt, nor over an entry read later.
//
// It takes the lock for learnBatch keys at a time. Each key is learned whole
// under the lock, and t stays pending until the last, so a call between two
// batches sees each key either before or after it was learned, as it would
// with the keys' lookups answering one by one.
func (m *readMemory) learned(t *ticket, versions map[string]string) {
	if t == nil {
		return
	}
	m.mu.Lock()
	if t.ended {
		m.mu.Unlock()
		return
	}
	t.ended = true
	m.mu.Unlock()

	found := make([]string, 0, len(versions))
	for k := range versions {
		found = append(found, k)
	}
	var missing []string
	for _, k := range t.keys {
		if _, ok := versions[k]; !ok {
			missing = append(missing, k)
		}
	}
	for lo := 0; lo < len(found)+len(missing); lo += learnBatch {
		m.mu.Lock()
		m.learnKeys(t, versions, found, missing, lo, min(lo+learnBatch, len(found)+len(missing)))
		m.mu.Unlock()
		// A call waiting for the lock is woken by the unlock, but would
		// rarely get it before this loop takes it again; let it run.
		runtime.Gosched()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rr := m.roles[t.p.role]; rr != nil {
		m.endTicket(rr, t)
	}
}

// learnKeys learns the keys lo to hi of found, then missing.
func (m *readMemory) learnKeys(t *ticket, versions map[string]string, found, missing []string, lo, hi int) {
	rr := m.roles[t.p.role]
	if rr == nil { // cannot happen: a pending ticket keeps its role
		return
	}
	now := m.now()
	whole, rows := rr.clearedSince(t.table, t.readAt)
	keep := !whole && !m.expired(t.table, t.readAt, now)
	for i := lo; i < hi; i++ {
		if i < len(found) {
			key := found[i]
			m.learn(rr, t, key, versions[key], keep && !rows[key], now)
		} else {
			m.unlearn(rr, t, missing[i-len(found)])
		}
	}
}

// learn keeps what t found of the row key: version v. keep is false when the
// row was cleared since t's read ended or t is past its table's remember.
func (m *readMemory) learn(rr *roleMemory, t *ticket, key, v string, keep bool, now time.Time) {
	re := rr.entriesOf(t.table, key)
	if re != nil {
		if end, ok := re.unordered(t.p.task, v, t.readAt); ok {
			m.forgetThrough(rr, t.table, key, end)
			return
		}
	}
	if !keep {
		return
	}
	if re != nil {
		if e := re.of(t.p.task); e != nil {
			if t.readAt.After(e.readAt) {
				e.version, e.readAt, e.since = v, t.readAt, t.readAt
				m.fix(e)
			}
			return
		}
	}
	m.add(rr, t.table, key, t.p.task, v, t.readAt, now)
}

// unordered reports whether a read by task that ended at readAt and found v
// cannot be put before or after what the row holds, and if so the last moment
// of what it cannot be ordered against. That is so when readAt falls in the
// span of an entry an own write refreshed and that entry has another version;
// or when task's own entry was read at the very same time with another
// version, as a clock that stood still across two reads gives, and only one
// of the two could be kept. Entries of other tasks at the very same time are
// both kept, and check prefers the one that agrees with the row now.
func (re *rowEntries) unordered(task, v string, readAt time.Time) (end time.Time, ok bool) {
	for _, e := range re.entries {
		if e.version == v || readAt.Before(e.since) || readAt.After(e.readAt) {
			continue
		}
		if e.since.Before(e.readAt) || e.task == task {
			return e.readAt, true
		}
	}
	return time.Time{}, false
}

// unlearn forgets t's party's entry of the row key, which t's lookup did not
// find, unless a later read made it.
func (m *readMemory) unlearn(rr *roleMemory, t *ticket, key string) {
	re := rr.entriesOf(t.table, key)
	if re == nil {
		return
	}
	if e := re.of(t.p.task); e != nil && !e.readAt.After(t.readAt) {
		m.take(e)
		m.tidyRow(rr, t.table, key)
	}
}

// dropped ends t with nothing learned.
func (m *readMemory) dropped(t *ticket) {
	if t == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.ended {
		return
	}
	t.ended = true
	if rr := m.roles[t.p.role]; rr != nil {
		m.endTicket(rr, t)
	}
}

// endTicket takes t off what it was pending on, lets go of the times of
// clears no pending ticket can be cut by any more, and drops what is left
// empty.
func (m *readMemory) endTicket(rr *roleMemory, t *ticket) {
	is := func(x *ticket) bool { return x == t }
	if tr := rr.tables[t.table]; tr != nil {
		if t.keyless {
			tr.keyless = slices.DeleteFunc(tr.keyless, is)
		}
		for _, k := range t.keys {
			if row := tr.rows[k]; row != nil {
				row.pending = slices.DeleteFunc(row.pending, is)
				if row.idle() {
					delete(tr.rows, k)
				}
			}
		}
	}
	delete(rr.pending, t)
	rr.settle()
	m.tidyTable(rr, t.table)
}

// settle keeps only the clears a pending ticket can still be cut by: those at
// or after the oldest pending read.
func (rr *roleMemory) settle() {
	if len(rr.pending) == 0 {
		rr.clears = nil
		return
	}
	if len(rr.clears) == 0 {
		return
	}
	var oldest time.Time
	first := true
	for t := range rr.pending {
		if first || t.readAt.Before(oldest) {
			oldest, first = t.readAt, false
		}
	}
	rr.clears = slices.DeleteFunc(rr.clears, func(c clearing) bool { return c.at.Before(oldest) })
}

// clearedSince reports which of table a read that ended at readAt may keep
// nothing of, because it was cleared at or after readAt: the whole table, or
// the rows in rows.
func (rr *roleMemory) clearedSince(table string, readAt time.Time) (whole bool, rows map[string]bool) {
	for _, c := range rr.clears {
		switch {
		case c.at.Before(readAt), c.table != "" && c.table != table:
			// before the read ended, or of another table
		case c.table == "" || c.key == "":
			return true, nil
		default:
			if rows == nil {
				rows = map[string]bool{}
			}
			rows[c.key] = true
		}
	}
	return false, rows
}

// check compares a write by p to the row key of table, whose xmin now is
// version ("" when the row is not there), at time at. A stale result is
// remembered for the unlock rule of spec 6.
func (m *readMemory) check(p party, table, key, version string, at time.Time) decide.ReadCheck {
	rc := decide.ReadCheck{Result: decide.ReadNone}
	if version != "" {
		rc.Version = &version
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rr := m.roles[p.role]
	if rr == nil {
		return rc
	}
	tr := rr.tables[table]
	row := rr.row(table, key)
	if len(rr.marks) > 0 || tr != nil && len(tr.marks) > 0 || row != nil && len(row.marks) > 0 {
		rc.Result = decide.ReadOwnWrite
		return rc
	}
	if tr == nil {
		return rc
	}

	// A write of task t is compared with the role's reads of t and of no task;
	// a write of no task with all the role's reads. Once a write of the role
	// to the row was judged stale, every read of the row by the role that
	// ended since is compared too, whatever its task.
	re := rr.entriesOf(table, key)
	var staleAt time.Time
	if re != nil {
		staleAt = re.staleAt
	}
	compared := func(task string, readAt time.Time) bool {
		return p.task == "" || task == "" || task == p.task || !staleAt.IsZero() && !readAt.Before(staleAt)
	}
	for _, t := range tr.keyless {
		if compared(t.p.task, t.readAt) {
			rc.Result = decide.ReadPending
			return rc
		}
	}
	if row != nil {
		for _, t := range row.pending {
			if compared(t.p.task, t.readAt) {
				rc.Result = decide.ReadPending
				return rc
			}
		}
	}
	if re == nil {
		return rc
	}

	var newest *entry
	for i := len(re.entries) - 1; i >= 0; i-- { // take may remove entries at i and after
		e := re.entries[i]
		if m.expired(table, e.readAt, at) {
			m.take(e)
			continue
		}
		if compared(e.task, e.readAt) && (newest == nil || prefer(e, newest, p.task, version)) {
			newest = e
		}
	}
	if newest == nil {
		m.tidyTable(rr, table)
		return rc
	}
	rc.ReadVersion = newest.version
	age := max(0, at.Sub(newest.readAt).Milliseconds())
	rc.ReadAgeMS = &age
	if newest.task != "" {
		task := newest.task
		rc.ReadOnBehalfOf = &task
	}
	if version != "" && newest.version == version {
		rc.Result = decide.ReadFresh
		return rc
	}
	rc.Result = decide.ReadStale
	if staleAt.IsZero() || at.Before(staleAt) {
		re.staleAt = at
	}
	return rc
}

// prefer reports whether a write by task, of a row whose version now is
// version, is compared with a rather than b: the later read; at the same
// time, the one that found version; then the writer's own task, then no task,
// then by task name, so that the same calls always give the same answer.
func prefer(a, b *entry, task, version string) bool {
	if !a.readAt.Equal(b.readAt) {
		return a.readAt.After(b.readAt)
	}
	if am, bm := a.version == version, b.version == version; am != bm {
		return am
	}
	rank := func(t string) int {
		switch {
		case t == task:
			return 0
		case t == "":
			return 1
		}
		return 2
	}
	if ra, rb := rank(a.task), rank(b.task); ra != rb {
		return ra < rb
	}
	return a.task < b.task
}

// writing marks a write by role as on its way: to the row key of table, to
// some row of table (key ""), or to any table (table ""). While a mark
// covers a row, check answers own_write for role.
func (m *readMemory) writing(role, table, key string) *writeMark {
	m.mu.Lock()
	defer m.mu.Unlock()
	mk := &writeMark{role: role, table: table, key: key, since: m.now()}
	rr := m.roleOf(role)
	switch {
	case table == "":
		mk.key = ""
		rr.marks = append(rr.marks, mk)
	case key == "":
		tr := rr.tableOf(table)
		tr.marks = append(tr.marks, mk)
	default:
		row := rr.tableOf(table).rowOf(key)
		row.marks = append(row.marks, mk)
	}
	return mk
}

// refreshed ends a row mark whose write ran and committed: every entry role
// has for the row, whatever its task, becomes version as read now. With
// version "" they are forgotten. A table or all-tables mark has no row to
// refresh; what it covers is forgotten.
//
// Forgetting instead of refreshing. version was seen by a lookup at some
// moment between the making of mark and this call, not now: lookups run on
// more than one worker and can be delivered in either order, and no clock on
// Stet's side orders what two of them saw. So if role also knows the row, in
// that span, by another version (an entry read while the write was out, or
// another refresh whose own span overlaps), either could be the newer, and
// what the role read of the row up to now is forgotten instead. The same
// holds for a read of the row that ended in that span and is learned after
// this call: what was read up to the span's end is forgotten, and reads that
// ended after it are kept. The guarantee: a row that only role changed, or
// that nobody changed since role last read it, is never called stale because
// of the order lookups were delivered in. The cost is misses only: such a row
// answers no_read until role reads it again.
//
// A refresh also drops the note that a write to the row was judged stale:
// every task's entry then holds the same version, and the note would only
// make a task that never read the row compare with the refreshed entries.
// Leaving the time keeps an older read's late lookup out, as forget does.
func (m *readMemory) refreshed(mark *writeMark, version string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rr := m.endMark(mark)
	if rr == nil {
		return
	}
	if mark.key == "" {
		m.clear(rr, mark.table, "", m.now())
		return
	}
	m.refresh(rr, mark.table, mark.key, version, mark.since, m.now())
}

// refreshedRow is refreshed for a row that had no mark of its own (an INSERT
// with a key, whose text the server had not yet written), with the same
// guarantee and the same forgetting. The span its version may have been seen
// in starts at the oldest mark of role that still covers the row, so call it
// before ending the write's table mark. With no mark covering the row the span
// has no start, and a row role knows by another version is forgotten.
func (m *readMemory) refreshedRow(role, table, key, version string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rr := m.roles[role]
	if rr == nil { // nothing remembered and no read pending: nothing to refresh or to cut
		return
	}
	var since time.Time
	first := true
	oldest := func(marks []*writeMark) {
		for _, mk := range marks {
			if first || mk.since.Before(since) {
				since, first = mk.since, false
			}
		}
	}
	oldest(rr.marks)
	if tr := rr.tables[table]; tr != nil {
		oldest(tr.marks)
	}
	if row := rr.row(table, key); row != nil {
		oldest(row.marks)
	}
	if first {
		since = time.Time{}
	}
	m.refresh(rr, table, key, version, since, m.now())
}

// refresh makes every entry rr has of the row key of table, whatever its task,
// say version as read now, drops the row's stale note, and leaves the time
// for pending reads. The version was seen at an unknown moment of
// [since, now]: if an entry of the row was seen in that span with another
// version, or a refresh's span overlaps it, the two cannot be ordered, and the
// row is forgotten instead, as with version "". Entries past their table's
// remember are not brought back.
func (m *readMemory) refresh(rr *roleMemory, table, key, version string, since, now time.Time) {
	if re := rr.entriesOf(table, key); re != nil {
		for i := len(re.entries) - 1; i >= 0; i-- {
			if e := re.entries[i]; m.expired(table, e.readAt, now) {
				m.take(e)
			}
		}
	}
	re := rr.entriesOf(table, key)
	if version == "" {
		m.clear(rr, table, key, now)
		return
	}
	if re != nil && re.disagrees(version, since, now) {
		m.forgetThrough(rr, table, key, now)
		return
	}
	if re != nil {
		for _, e := range re.entries {
			e.version, e.readAt, e.since = version, now, since
			m.fix(e)
		}
		re.staleAt = time.Time{}
	}
	m.tidyRow(rr, table, key)
	rr.leave(table, key, now)
}

// disagrees reports whether an entry of the row was seen with a version other
// than version at a moment that may fall in [from, to].
func (re *rowEntries) disagrees(version string, from, to time.Time) bool {
	for _, e := range re.entries {
		if e.version != version && !e.since.After(to) && !e.readAt.Before(from) {
			return true
		}
	}
	return false
}

// forgetThrough forgets every entry rr has of the row key of table that was
// read at or before end, whatever its task, and leaves end as the row's time
// for pending reads. Entries read after end are ordered after whatever could
// not be ordered, and stay.
func (m *readMemory) forgetThrough(rr *roleMemory, table, key string, end time.Time) {
	if re := rr.entriesOf(table, key); re != nil {
		for i := len(re.entries) - 1; i >= 0; i-- {
			if e := re.entries[i]; !e.readAt.After(end) {
				m.take(e)
			}
		}
	}
	m.tidyRow(rr, table, key)
	rr.leave(table, key, end)
}

// forgotten ends mark and forgets what it covers for role: the row, the
// table, or everything. Call it only once the write's outcome is on the
// server (it committed, rolled back, or will never run): a read that ends
// after it is taken to have seen that outcome.
func (m *readMemory) forgotten(mark *writeMark) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rr := m.endMark(mark); rr != nil {
		m.clear(rr, mark.table, mark.key, m.now())
	}
}

// released ends mark and changes nothing (the write did not run).
func (m *readMemory) released(mark *writeMark) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rr := m.endMark(mark); rr != nil {
		m.tidyRow(rr, mark.table, mark.key)
	}
}

// endMark ends mark and returns its role's memory; nil when mark is nil or
// already ended.
func (m *readMemory) endMark(mark *writeMark) *roleMemory {
	if mark == nil || mark.ended {
		return nil
	}
	mark.ended = true
	rr := m.roles[mark.role]
	if rr == nil { // cannot happen: a mark keeps its role
		return nil
	}
	is := func(x *writeMark) bool { return x == mark }
	switch {
	case mark.table == "":
		rr.marks = slices.DeleteFunc(rr.marks, is)
	case mark.key == "":
		if tr := rr.tables[mark.table]; tr != nil {
			tr.marks = slices.DeleteFunc(tr.marks, is)
		}
	default:
		if row := rr.row(mark.table, mark.key); row != nil {
			row.marks = slices.DeleteFunc(row.marks, is)
		}
	}
	return rr
}

// forget forgets role's entries: of the row key of table, of table (key ""),
// or all (table ""). Call it only once what made the entries untrue is on the
// server: a read that ends after it is taken to have seen it.
func (m *readMemory) forget(role, table, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rr := m.roles[role]; rr != nil {
		m.clear(rr, table, key, m.now())
	}
}

// clear forgets rr's entries of the row key of table, of table (key ""), or
// of every table (table ""), and leaves the time for pending reads. Marks and
// pending reads stay. A table's entries are detached whole (detach), so the
// work under the lock does not grow with how many there are.
func (m *readMemory) clear(rr *roleMemory, table, key string, now time.Time) {
	switch {
	case table == "":
		key = ""
		for name, tr := range rr.tables {
			m.detach(rr, tr)
			m.tidyTable(rr, name)
		}
	case key == "":
		if tr := rr.tables[table]; tr != nil {
			m.detach(rr, tr)
			m.tidyTable(rr, table)
		}
	default:
		if re := rr.entriesOf(table, key); re != nil {
			for i := len(re.entries) - 1; i >= 0; i-- {
				m.take(re.entries[i])
			}
		}
		m.tidyRow(rr, table, key)
	}
	m.sweepRole(rr, clearSweep)
	if len(rr.graves) > 0 && !rr.queued {
		m.graving = append(m.graving, rr)
		rr.queued = true
	}
	m.tidyRole(rr)
	rr.leave(table, key, now)
}

// detach clears tr's entries at once: they stop counting toward len and the
// limits, the rows and their notes go, and the entries wait in rr's graves to
// be taken out of the order by read time.
func (m *readMemory) detach(rr *roleMemory, tr *tableMemory) {
	set := tr.known
	if set == nil {
		return
	}
	tr.known = nil
	set.cleared = true
	set.rows = nil
	m.live -= len(set.entries)
	rr.live -= len(set.entries)
	rr.graves = append(rr.graves, set)
}

// leave notes that the scope was cleared at at, for the reads still pending:
// what they learn of it is not kept.
//
// The time is kept only while a read of the role is pending, and only as long
// as one that ended at or before it is (settle). A read noted after this call
// is not cut by it, even if it ended before at: its lookup is queued only
// once it is noted (see reading), so it reads the row after the clear and
// cannot bring back a version from before it. Only lookups already queued
// can, and those are the pending reads. Keeping the time longer would protect
// nothing and would grow with every write of a role that reads nothing.
func (rr *roleMemory) leave(table, key string, at time.Time) {
	if len(rr.pending) > 0 {
		rr.clears = append(rr.clears, clearing{table: table, key: key, at: at})
	}
}

// evicted reports and resets what was thrown away for the limits.
func (m *readMemory) evicted() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.evicts
	m.evicts = 0
	return n
}

// len reports how many entries are held, not counting cleared ones not yet
// taken out.
func (m *readMemory) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live
}

// expired reports whether an entry of table read at readAt counts as absent
// at now: it is older than the table's remember, or the table keeps nothing.
func (m *readMemory) expired(table string, readAt, now time.Time) bool {
	keep, ok := m.remember[table]
	return !ok || now.Sub(readAt) > keep
}

// add makes the entry of task for the row key of table, then takes one
// cleared entry out (sweep) and keeps the limits.
func (m *readMemory) add(rr *roleMemory, table, key, task, v string, readAt, now time.Time) {
	tr := rr.tableOf(table)
	if tr.known == nil {
		tr.known = &entrySet{rows: map[string]*rowEntries{}}
	}
	set := tr.known
	re := set.rows[key]
	if re == nil {
		re = &rowEntries{}
		set.rows[key] = re
	}
	e := &entry{role: rr, set: set, row: re, table: table, key: key, task: task, version: v, readAt: readAt, since: readAt, inSet: len(set.entries)}
	set.entries = append(set.entries, e)
	re.entries = append(re.entries, e)
	m.all.push(e)
	rr.all.push(e)
	m.live++
	rr.live++
	m.sweep(rr, 1)
	m.limit(rr, now)
}

// sweep takes up to n cleared entries out of the order by read time: rr's own
// if it has any, else any role's, the role that has had them longest first.
//
// Called with 1 for every entry added, it keeps the entries held, live or
// cleared, within both limits: a clear does not change how many are held, an
// entry added while some cleared one is held takes one out, and one added
// while none is held is followed by limit. So when limit has to throw a live
// entry away, no cleared one is held (of the role, for its quarter), and the
// entry on top of the order is a live one: no eviction walks over cleared
// entries.
func (m *readMemory) sweep(rr *roleMemory, n int) {
	if n -= m.sweepRole(rr, n); n == 0 {
		return
	}
	for n > 0 && len(m.graving) > 0 {
		g := m.graving[0]
		n -= m.sweepRole(g, n)
		if len(g.graves) > 0 {
			break
		}
		m.graving[0] = nil
		m.graving = m.graving[1:]
		g.queued = false
		m.tidyRole(g)
	}
}

// sweepRole takes up to n of rr's cleared entries out and returns how many.
func (m *readMemory) sweepRole(rr *roleMemory, n int) int {
	done := 0
	for len(rr.graves) > 0 {
		set := rr.graves[0]
		if len(set.entries) == 0 {
			rr.graves[0] = nil
			rr.graves = rr.graves[1:]
			continue
		}
		if done == n {
			break
		}
		m.take(set.entries[len(set.entries)-1])
		done++
	}
	if len(rr.graves) == 0 {
		rr.graves = nil
	}
	return done
}

// limit throws away, after rr gained an entry, the role's oldest entries past
// its quarter and then the oldest of all past maxKeys. What had expired anyway
// is not counted.
func (m *readMemory) limit(rr *roleMemory, now time.Time) {
	for rr.live > m.perRole {
		m.evict(rr.all.es[0], now)
	}
	for m.live > m.maxKeys {
		m.evict(m.all.es[0], now)
	}
}

func (m *readMemory) evict(e *entry, now time.Time) {
	if e.set.cleared { // not met (see sweep), and not counted
		m.take(e)
		return
	}
	if !m.expired(e.table, e.readAt, now) {
		m.evicts++
	}
	m.take(e)
	m.tidyTable(e.role, e.table)
}

// fix puts e back in order after its readAt changed.
func (m *readMemory) fix(e *entry) {
	m.all.fix(e.pos[0])
	e.role.all.fix(e.pos[1])
}

// take takes e out of the order by read time and out of its set. A live
// entry also leaves its row and the counts, and its set, once empty, its
// table.
func (m *readMemory) take(e *entry) {
	m.all.remove(e.pos[0])
	e.role.all.remove(e.pos[1])
	set := e.set
	last := len(set.entries) - 1
	set.entries[e.inSet] = set.entries[last]
	set.entries[e.inSet].inSet = e.inSet
	set.entries[last] = nil
	set.entries = set.entries[:last]
	if set.cleared {
		return
	}
	m.live--
	e.role.live--
	re := e.row
	re.entries = slices.DeleteFunc(re.entries, func(x *entry) bool { return x == e })
	if len(re.entries) == 0 {
		delete(set.rows, e.key)
	}
	if len(set.entries) == 0 {
		if tr := e.role.tables[e.table]; tr != nil && tr.known == set {
			tr.known = nil
		}
	}
}

// tidyRow drops the record of the row key of table if it holds no mark or
// pending read, then the table and the role likewise.
func (m *readMemory) tidyRow(rr *roleMemory, table, key string) {
	if tr := rr.tables[table]; tr != nil {
		if row := tr.rows[key]; row != nil && row.idle() {
			delete(tr.rows, key)
		}
	}
	m.tidyTable(rr, table)
}

func (m *readMemory) tidyTable(rr *roleMemory, table string) {
	if tr := rr.tables[table]; tr != nil && tr.idle() {
		delete(rr.tables, table)
	}
	m.tidyRole(rr)
}

func (m *readMemory) tidyRole(rr *roleMemory) {
	if rr.idle() && m.roles[rr.name] == rr {
		delete(m.roles, rr.name)
	}
}

func (m *readMemory) roleOf(name string) *roleMemory {
	rr := m.roles[name]
	if rr == nil {
		rr = &roleMemory{name: name, tables: map[string]*tableMemory{}, all: byRead{slot: 1}, pending: map[*ticket]struct{}{}}
		m.roles[name] = rr
	}
	return rr
}

func (rr *roleMemory) tableOf(name string) *tableMemory {
	tr := rr.tables[name]
	if tr == nil {
		tr = &tableMemory{rows: map[string]*rowMemory{}}
		rr.tables[name] = tr
	}
	return tr
}

func (tr *tableMemory) rowOf(key string) *rowMemory {
	row := tr.rows[key]
	if row == nil {
		row = &rowMemory{}
		tr.rows[key] = row
	}
	return row
}

// row is the role's record of marks and pending reads of the row key of
// table; nil when it has none.
func (rr *roleMemory) row(table, key string) *rowMemory {
	if tr := rr.tables[table]; tr != nil {
		return tr.rows[key]
	}
	return nil
}

// entriesOf is the role's entries of the row key of table; nil when it has
// none.
func (rr *roleMemory) entriesOf(table, key string) *rowEntries {
	if tr := rr.tables[table]; tr != nil && tr.known != nil {
		return tr.known.rows[key]
	}
	return nil
}

// byRead is a heap of entries, the one read longest ago on top. slot is the
// position in entry.pos it keeps.
type byRead struct {
	es   []*entry
	slot int
}

func (h *byRead) len() int { return len(h.es) }

func (h *byRead) less(i, j int) bool { return h.es[i].readAt.Before(h.es[j].readAt) }

func (h *byRead) swap(i, j int) {
	h.es[i], h.es[j] = h.es[j], h.es[i]
	h.es[i].pos[h.slot] = i
	h.es[j].pos[h.slot] = j
}

func (h *byRead) push(e *entry) {
	e.pos[h.slot] = len(h.es)
	h.es = append(h.es, e)
	h.up(len(h.es) - 1)
}

func (h *byRead) remove(i int) {
	n := len(h.es) - 1
	if i != n {
		h.swap(i, n)
	}
	h.es[n] = nil
	h.es = h.es[:n]
	if i < n {
		h.fix(i)
	}
}

func (h *byRead) fix(i int) {
	if !h.down(i) {
		h.up(i)
	}
}

func (h *byRead) up(j int) {
	for j > 0 {
		i := (j - 1) / 2
		if !h.less(j, i) {
			break
		}
		h.swap(i, j)
		j = i
	}
}

func (h *byRead) down(i0 int) bool {
	i, n := i0, len(h.es)
	for {
		j := 2*i + 1
		if j >= n {
			break
		}
		if r := j + 1; r < n && h.less(r, j) {
			j = r
		}
		if !h.less(j, i) {
			break
		}
		h.swap(i, j)
		i = j
	}
	return i > i0
}

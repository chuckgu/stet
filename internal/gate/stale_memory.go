package gate

import (
	"container/heap"
	"maps"
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
// What it holds is bounded by what it serves:
//   - entries, by maxKeys overall and maxKeys/4 per login role, the one read
//     longest ago going first, and by each table's remember;
//   - tickets and marks, by the calls that end them (learned or dropped;
//     refreshed, forgotten or released);
//   - the records of roles, tables and rows, which go as soon as they hold no
//     entry, mark or ticket, and with a row the note that it was judged stale;
//   - the times scopes were cleared, which are kept only while a ticket of the
//     role is pending that one of them could cut.
type readMemory struct {
	remember map[string]time.Duration
	maxKeys  int
	perRole  int
	now      func() time.Time

	mu     sync.Mutex
	roles  map[string]*roleMemory
	all    byRead // every entry
	evicts int64  // entries the size limits threw away since evicted last ran
}

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
	ended   bool
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
	all     byRead               // the role's entries
	marks   []*writeMark         // on every table
	pending map[*ticket]struct{} // the role's tickets not yet ended
	clears  []clearing           // scopes cleared while a ticket was pending
}

// tableMemory is what one login role holds of one table.
type tableMemory struct {
	rows    map[string]*rowMemory
	marks   []*writeMark // on some row of the table
	keyless []*ticket    // pending reads of some row of the table
}

// rowMemory is what one login role holds of one row.
type rowMemory struct {
	entries map[string]*entry // by task
	marks   []*writeMark
	pending []*ticket
	staleAt time.Time // when a write of the role to the row was first judged stale; zero if never
}

func (r *rowMemory) idle() bool {
	return len(r.entries) == 0 && len(r.marks) == 0 && len(r.pending) == 0
}

func (t *tableMemory) idle() bool {
	return len(t.rows) == 0 && len(t.marks) == 0 && len(t.keyless) == 0
}

func (r *roleMemory) idle() bool {
	return len(r.tables) == 0 && len(r.marks) == 0 && len(r.pending) == 0
}

// entry is one remembered read.
type entry struct {
	role       *roleMemory
	row        *rowMemory
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
}

// clearing is a scope whose entries were forgotten or refreshed at a time: the
// row key of table, table (key ""), or every table (table "").
type clearing struct {
	table, key string
	at         time.Time
}

// byRead is a heap of entries, the one read longest ago on top. slot is the
// position in entry.pos it keeps.
type byRead struct {
	es   []*entry
	slot int
}

func (h *byRead) Len() int           { return len(h.es) }
func (h *byRead) Less(i, j int) bool { return h.es[i].readAt.Before(h.es[j].readAt) }
func (h *byRead) Swap(i, j int) {
	h.es[i], h.es[j] = h.es[j], h.es[i]
	h.es[i].pos[h.slot] = i
	h.es[j].pos[h.slot] = j
}

func (h *byRead) Push(x any) {
	e := x.(*entry)
	e.pos[h.slot] = len(h.es)
	h.es = append(h.es, e)
}

func (h *byRead) Pop() any {
	n := len(h.es) - 1
	e := h.es[n]
	h.es[n] = nil
	h.es = h.es[:n]
	return e
}

// reading notes a read by p that ended at readAt and whose versions are not
// known yet: of the rows keys of table, or, with keys nil, of some row of
// table (a key taken from a WHERE clause).
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
func (m *readMemory) learned(t *ticket, versions map[string]string) {
	if t == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.ended {
		return
	}
	rr := m.roles[t.p.role]
	if rr == nil { // cannot happen: a pending ticket keeps its role
		t.ended = true
		return
	}
	now := m.now()
	whole, rows := rr.clearedSince(t.table, t.readAt)
	keep := !whole && !m.expired(t.table, t.readAt, now)
	for key, v := range versions {
		m.learn(rr, t, key, v, keep && !rows[key], now)
	}
	if !t.keyless {
		for _, key := range t.keys {
			if _, found := versions[key]; !found {
				m.unlearn(rr, t, key)
			}
		}
	}
	m.endTicket(rr, t)
}

// learn keeps what t found of the row key: version v. keep is false when the
// row was cleared since t's read ended or t is past its table's remember.
func (m *readMemory) learn(rr *roleMemory, t *ticket, key, v string, keep bool, now time.Time) {
	row := rr.row(t.table, key)
	// An entry an own write refreshed holds a version its lookup saw at an
	// unknown moment of [since, readAt]. A read of the role that ended in that
	// span and found another version cannot be put before or after it: either
	// may be the older. The row is forgotten rather than compared with either.
	if row != nil {
		for _, e := range row.entries {
			if e.since.Before(e.readAt) && e.version != v && !t.readAt.Before(e.since) && !t.readAt.After(e.readAt) {
				m.clear(rr, t.table, key, now)
				return
			}
		}
	}
	if !keep {
		return
	}
	if row == nil {
		row = rr.tableOf(t.table).rowOf(key)
	}
	if e := row.entries[t.p.task]; e != nil {
		if t.readAt.After(e.readAt) {
			e.version, e.readAt, e.since = v, t.readAt, t.readAt
			m.fix(e)
		}
		return
	}
	e := &entry{role: rr, row: row, table: t.table, key: key, task: t.p.task, version: v, readAt: t.readAt, since: t.readAt}
	row.entries[t.p.task] = e
	heap.Push(&m.all, e)
	heap.Push(&rr.all, e)
	m.limit(rr, now)
}

// unlearn forgets t's party's entry of the row key, which t's lookup did not
// find, unless a later read made it.
func (m *readMemory) unlearn(rr *roleMemory, t *ticket, key string) {
	row := rr.row(t.table, key)
	if row == nil {
		return
	}
	if e := row.entries[t.p.task]; e != nil && !e.readAt.After(t.readAt) {
		m.removeEntry(e)
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
	if rr := m.roles[t.p.role]; rr != nil {
		m.endTicket(rr, t)
	}
	t.ended = true
}

// endTicket takes t off what it was pending on, lets go of the times of
// clears no pending ticket can be cut by any more, and drops what is left
// empty.
func (m *readMemory) endTicket(rr *roleMemory, t *ticket) {
	t.ended = true
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
	var staleAt time.Time
	if row != nil {
		staleAt = row.staleAt
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
	if row == nil {
		return rc
	}
	for _, t := range row.pending {
		if compared(t.p.task, t.readAt) {
			rc.Result = decide.ReadPending
			return rc
		}
	}

	var newest *entry
	for _, e := range row.entries {
		if m.expired(table, e.readAt, at) {
			m.removeEntry(e)
			continue
		}
		if !compared(e.task, e.readAt) {
			continue
		}
		if newest == nil || e.readAt.After(newest.readAt) || e.readAt.Equal(newest.readAt) && e.version == version {
			newest = e
		}
	}
	if newest == nil {
		m.tidyRow(rr, table, key)
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
		row.staleAt = at
	}
	return rc
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
// the row is forgotten instead. The same holds for a read of the row that
// ended in that span and is learned after this call. The guarantee: a row
// that only role changed, or that nobody changed since role last read it, is
// never called stale because of the order lookups were delivered in. The cost
// is misses only: such a row answers no_read until role reads it again.
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
// say version as read now, and leaves the time for pending reads. The version
// was seen at an unknown moment of [since, now]: an entry of the row seen in
// that span with another version, or a refresh whose span overlaps it, cannot
// be ordered against it, and may be the newer. Then, as with version "", the
// row is forgotten instead. Entries past their table's remember are not
// brought back.
func (m *readMemory) refresh(rr *roleMemory, table, key, version string, since, now time.Time) {
	row := rr.row(table, key)
	if row != nil {
		for _, e := range row.entries {
			if m.expired(table, e.readAt, now) {
				m.removeEntry(e)
			}
		}
	}
	if version == "" || row != nil && row.disagrees(version, since, now) {
		m.clear(rr, table, key, now)
		return
	}
	if row != nil {
		for _, e := range row.entries {
			e.version, e.readAt, e.since = version, now, since
			m.fix(e)
		}
		m.tidyRow(rr, table, key)
	}
	rr.leave(table, key, now)
}

// disagrees reports whether an entry of the row was seen with a version other
// than version at a moment that may fall in [from, to].
func (r *rowMemory) disagrees(version string, from, to time.Time) bool {
	for _, e := range r.entries {
		if e.version != version && !e.since.After(to) && !e.readAt.Before(from) {
			return true
		}
	}
	return false
}

// forgotten ends mark and forgets what it covers for role: the row, the
// table, or everything.
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
// or all (table "").
func (m *readMemory) forget(role, table, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rr := m.roles[role]; rr != nil {
		m.clear(rr, table, key, m.now())
	}
}

// clear forgets rr's entries of the row key of table, of table (key ""), or
// of every table (table ""), and leaves the time for pending reads. Marks and
// pending reads stay.
func (m *readMemory) clear(rr *roleMemory, table, key string, now time.Time) {
	switch {
	case table == "":
		key = ""
		for name, tr := range rr.tables {
			for k, row := range tr.rows {
				m.removeRow(row)
				m.tidyRow(rr, name, k)
			}
		}
		m.tidyRole(rr)
	case key == "":
		if tr := rr.tables[table]; tr != nil {
			for k, row := range tr.rows {
				m.removeRow(row)
				m.tidyRow(rr, table, k)
			}
		}
		m.tidyTable(rr, table)
	default:
		if row := rr.row(table, key); row != nil {
			m.removeRow(row)
		}
		m.tidyRow(rr, table, key)
	}
	rr.leave(table, key, now)
}

// leave notes that the scope was cleared at now, for the reads still pending:
// what they learn of it is not kept.
//
// The time is kept only while a read of the role is pending, and only as long
// as one that ended at or before it is (settle). A read noted after this call
// is not cut by it, even if it ended before now: its lookup is queued only
// once it is noted, so it reads the row after the clear and cannot bring back
// a version from before it. Only lookups already queued can, and those are
// the pending reads. Keeping the time longer would protect nothing and would
// grow with every write of a role that reads nothing.
func (rr *roleMemory) leave(table, key string, now time.Time) {
	if len(rr.pending) > 0 {
		rr.clears = append(rr.clears, clearing{table: table, key: key, at: now})
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

// len reports how many entries are held.
func (m *readMemory) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.all.Len()
}

// expired reports whether an entry of table read at readAt counts as absent
// at now: it is older than the table's remember, or the table keeps nothing.
func (m *readMemory) expired(table string, readAt, now time.Time) bool {
	keep, ok := m.remember[table]
	return !ok || now.Sub(readAt) > keep
}

// limit throws away, after rr gained an entry, the role's oldest entries past
// its quarter and then the oldest of all past maxKeys. What had expired anyway
// is not counted.
func (m *readMemory) limit(rr *roleMemory, now time.Time) {
	for rr.all.Len() > m.perRole {
		m.evict(rr.all.es[0], now)
	}
	for m.all.Len() > m.maxKeys {
		m.evict(m.all.es[0], now)
	}
}

func (m *readMemory) evict(e *entry, now time.Time) {
	if !m.expired(e.table, e.readAt, now) {
		m.evicts++
	}
	m.removeEntry(e)
	m.tidyRow(e.role, e.table, e.key)
}

// fix puts e back in order after its readAt changed.
func (m *readMemory) fix(e *entry) {
	heap.Fix(&m.all, e.pos[0])
	heap.Fix(&e.role.all, e.pos[1])
}

func (m *readMemory) removeEntry(e *entry) {
	heap.Remove(&m.all, e.pos[0])
	heap.Remove(&e.role.all, e.pos[1])
	delete(e.row.entries, e.task)
}

func (m *readMemory) removeRow(row *rowMemory) {
	for _, e := range row.entries {
		m.removeEntry(e)
	}
}

// tidyRow drops the row key of table if it holds nothing, then the table and
// the role likewise. A row's note that it was judged stale goes with it.
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
		row = &rowMemory{entries: map[string]*entry{}}
		tr.rows[key] = row
	}
	return row
}

// row is the role's record of the row key of table; nil when it has none.
func (rr *roleMemory) row(table, key string) *rowMemory {
	if tr := rr.tables[table]; tr != nil {
		return tr.rows[key]
	}
	return nil
}

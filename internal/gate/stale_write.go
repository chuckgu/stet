package gate

import (
	"slices"
	"sync"
	"time"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/sqlparse"
)

// The write side of stale-write detection (spec-v0.2-stale 6, 7): comparing
// a write's row with what its login role read, and keeping the role's own
// writes from looking like another party's. A write of the role is marked
// while it is on its way (writeMark), from just before it goes upstream
// until its effect on the server is known, and then what the role read of
// the rows it may have changed is refreshed or forgotten. Every mark ends by
// a call the proxy owes (End, TxEnded), a timer Close ends, or the done of
// Applying, so no mark stays.

// staleLostGrace is how long the mark of a write lost with its connection
// stays, and those of a session whose connection ended with something
// unanswered (correction S15): what they cover may still commit on the
// server after the proxy reported it. A var so tests can shorten it.
var staleLostGrace = time.Minute

// staleSessionRowMarks is how many rows of one table one session's
// transaction holds a mark each for; past it one mark on the table stands
// for them all (hold).
const staleSessionRowMarks = 1000

// settledKeep is how long the time own writes of a login role last ended is
// kept (see settle): far longer than a write's state read may take
// (stateTimeout), whose comparison is all it serves.
const settledKeep = time.Minute

// everyTable are the reasons a write may write tables besides the one the
// classifier names (correction S9): its mark covers every table.
var everyTable = []string{"multi_statement", "nested_write", "with", "truncate"}

// ownWrites is what the write side keeps between calls: the marks of each
// session's writes that ran inside a transaction not yet ended, its Unseen
// calls not yet settled, the marks waiting out a grace because what they
// cover may still commit, and when own writes last ended. Every mark in it
// ends by TxEnded, a timer, or close.
type ownWrites struct {
	mu       sync.Mutex
	sessions map[string]*sessionWrites // by SessionInfo.ID; an entry goes once it holds nothing
	lost     map[string]*lostMarks     // by SessionInfo.ID: the marks of a session that ended, waiting out their grace
	closed   bool
	// settled is when marks of a login role's writes last ended with an
	// effect on what the role remembers of a table, or of every table
	// (table ""); swept is when times older than settledKeep were last
	// let go.
	settled map[roleTable]time.Time
	swept   time.Time
}

type roleTable struct{ role, table string }

// lostMarks are the marks one session left waiting out staleLostGrace, on
// one timer: a connection that ends with something unanswered may leave a
// mark for every row and table its transaction wrote, all at that moment.
type lostMarks struct {
	marks []*writeMark
	timer *time.Timer
}

// sessionWrites is what one session owes the memory.
type sessionWrites struct {
	// The marks of writes that ran inside a transaction, ended by the next
	// TxEnded: at most one on every table, one on some row of each table,
	// and one on each of up to staleSessionRowMarks rows of a table (hold).
	all    *writeMark
	tables map[string]*writeMark
	rows   map[string]map[string]*writeMark // by table, then by key

	unseen int        // Unseen calls not yet settled by a TxEnded, also those made before the login role was known
	mark   *writeMark // the login role's mark on every table while unseen is not 0; nil when it had none
}

// hold keeps mk, the mark of a write that ran inside the session's
// transaction, until the session's next TxEnded, unless a mark the session
// keeps already covers it: mk then ends at once with nothing changed, and
// the older mark forgets what mk covers when it ends. A table's mark covers
// its rows', and a mark on every table covers all. So a transaction keeps a
// mark for each row and table it wrote, not for each write, and ending it
// ends few marks: ending a table's mark looks through the role's marks on
// the table.
//
// Nor does it keep a mark for every row of a long transaction of single-row
// writes: once it holds staleSessionRowMarks rows of a table, the next row
// puts one mark on the table in their place, made before they are let go, so
// that no row is uncovered in between. The table's mark covers more than the
// rows did, and forgets the table when it ends: misses only. It runs with
// ownWrites.mu held.
func (sw *sessionWrites) hold(mem *readMemory, mk *writeMark) {
	covered := sw.all != nil
	switch {
	case covered:
	case mk.table == "":
		sw.all = mk
	case mk.key == "":
		if covered = sw.tables[mk.table] != nil; !covered {
			if sw.tables == nil {
				sw.tables = map[string]*writeMark{}
			}
			sw.tables[mk.table] = mk
		}
	default:
		rows := sw.rows[mk.table]
		switch covered = sw.tables[mk.table] != nil || rows[mk.key] != nil; {
		case covered:
		case len(rows) < staleSessionRowMarks:
			if rows == nil {
				if sw.rows == nil {
					sw.rows = map[string]map[string]*writeMark{}
				}
				rows = map[string]*writeMark{}
				sw.rows[mk.table] = rows
			}
			rows[mk.key] = mk
		default:
			if sw.tables == nil {
				sw.tables = map[string]*writeMark{}
			}
			sw.tables[mk.table] = mem.writing(mk.role, mk.table, "")
			for _, row := range rows {
				mem.released(row)
			}
			delete(sw.rows, mk.table)
			covered = true
		}
	}
	if covered {
		mem.released(mk)
	}
}

// takeHeld returns the marks hold kept, and keeps none.
func (sw *sessionWrites) takeHeld() []*writeMark {
	var out []*writeMark
	if sw.all != nil {
		out = append(out, sw.all)
	}
	for _, mk := range sw.tables {
		out = append(out, mk)
	}
	for _, rows := range sw.rows {
		for _, mk := range rows {
			out = append(out, mk)
		}
	}
	sw.all, sw.tables, sw.rows = nil, nil, nil
	return out
}

func (w *ownWrites) session(id string) *sessionWrites {
	if w.sessions == nil {
		w.sessions = map[string]*sessionWrites{}
	}
	sw := w.sessions[id]
	if sw == nil {
		sw = &sessionWrites{}
		w.sessions[id] = sw
	}
	return sw
}

// settle runs end, a call of the memory that ends marks of role's writes and
// refreshes or forgets what they covered of table (table "" for every
// table), and notes when it returned (see compare). It holds w.mu throughout,
// so a comparison that saw what end changed sees the time too.
func (s *stale) settle(role, table string, end func()) {
	s.writes.mu.Lock()
	defer s.writes.mu.Unlock()
	s.settleLocked(role, table, end)
}

// settleLocked is settle with s.writes.mu held.
func (s *stale) settleLocked(role, table string, end func()) {
	w := &s.writes
	end()
	now := time.Now()
	if w.settled == nil {
		w.settled = map[roleTable]time.Time{}
	}
	w.settled[roleTable{role, table}] = now
	if now.Sub(w.swept) > settledKeep {
		for k, at := range w.settled {
			if now.Sub(at) > settledKeep {
				delete(w.settled, k)
			}
		}
		w.swept = now
	}
}

// settledSince reports whether marks of role's writes ended with an effect on
// table, or on every table, at or after since.
func (w *ownWrites) settledSince(role, table string, since time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, k := range []roleTable{{role, ""}, {role, table}} {
		if at, ok := w.settled[k]; ok && !at.Before(since) {
			return true
		}
	}
	return false
}

// unchecked is the read check a write to table starts with: not_checked when
// table has stale, nil otherwise. A comparison replaces it (compare).
func (s *stale) unchecked(table string) *decide.ReadCheck {
	if s == nil {
		return nil
	}
	if _, ok := s.pks[table]; !ok {
		return nil
	}
	return &decide.ReadCheck{Result: decide.ReadNotChecked}
}

// stateKey is the type p's state read reads its key value as, so that the
// read gives back the key as the server writes it, the row or no row
// (correction S11); "" when p is not compared with what its login role read,
// and its state is read as before. p is compared when it is a single-row
// UPDATE or DELETE (Gated) of a table the catalog tracks now, by a session
// the memory is kept for (remembers: its login role is known, and it is
// connected to Stet's database), it does not name its table with another
// schema than the tracked table's (otherSchema), and its key value names the
// row the server's comparison matches (usableKey).
func (s *stale) stateKey(p *pending) string {
	w := p.res.Write
	if !s.remembers(p.session) || p.res.Kind != sqlparse.Gated || w == nil || w.Op == "insert" ||
		w.PK == nil || p.pk == nil {
		return ""
	}
	t, ok := s.table(p.res.Table)
	if !ok || s.otherSchema(p.res) || !usableKey(t, *w.PK, p.oids, *p.pk) {
		return ""
	}
	return t.typeName
}

// otherSchema reports whether r's write names its table with a schema that
// is not the one the tracked table is in. The classifier matches a table to
// its config key by name, whatever schema the statement names, and the
// config key resolves on Stet's connection to one table: such a write is to
// another table of the same name. It is not compared with what was read of
// the tracked table, and is no own write of it. A write that names no schema
// is taken to mean the tracked table; a session whose search_path resolves
// the name elsewhere is not seen.
func (s *stale) otherSchema(r sqlparse.Result) bool {
	if r.TableSchema == "" {
		return false
	}
	t, ok := s.table(r.Table)
	return ok && t.schema != r.TableSchema
}

// usableKey reports whether text, the value of v as the gate has it, names
// the row of t a statement holding v compares or stores it as: v reads as a
// value of the key column (keyUsable); a parameter the client gave a type is
// compared as the key column compares (paramType), as a read's is; and text
// is what a key may be (KeyOutput: of a string key, ASCII only, correction
// S2). Otherwise Stet could look up another row than the one the server
// writes.
func usableKey(t tracked, v sqlparse.Value, oids []uint32, text string) bool {
	if !keyUsable(t, v) {
		return false
	}
	if _, ok := proxy.KeyOutput(t.typ, 0, []byte(text)); !ok {
		return false
	}
	if v.Kind != sqlparse.Param {
		return true
	}
	var oid uint32
	if i := v.Param - 1; i >= 0 && i < len(oids) {
		oid = oids[i]
	}
	if oid == 0 {
		oid = castOIDs[v.Cast] // 0 for none
	}
	_, ok := paramType(oid, t)
	return ok
}

// compare is the read check of p, whose state read, sent at sent, found its
// row's key as the server writes it (p.key) and its version xmin ("" when the
// row is not there).
func (s *stale) compare(p *pending, xmin string, sent time.Time) *decide.ReadCheck {
	rc := s.compareRead(partyOf(p.session.User, p.session.OnBehalfOf), p.res.Table, p.key, xmin, sent)
	return &rc
}

// compareRead is what the memory answers for a write by p to the row key of
// table whose version a state read sent at sent found to be version, except
// that it is not stale when marks of p's login role ended with an effect on
// table at or after sent: it answers read_pending then.
//
// The memory is compared just after the state read, not at the moment the
// server took the row's version. In between, an own write that committed
// after that moment can have its mark end, and what the role remembers of the
// row then be its version: refreshed by the write's lookup, or read again by
// the role after the write's mark ended with a forget. That version is newer
// than the one the state read found, and the write would be called stale for
// a change only its role made. Every own write's change that the state read
// did not see was made while its mark was on, so either the mark is still on
// when the memory is compared (own_write), or it ended after sent. A
// comparison that may have seen such a version is not judged; one that
// could not have, is. The cost is a miss for a stale write whose role had
// another write of the table end while its state was read.
//
// Only a write judged stale leaves the note the unlock rule of spec 6 goes
// by: one answered read_pending leaves none.
func (s *stale) compareRead(p party, table, key, version string, sent time.Time) decide.ReadCheck {
	at := s.mem.now()
	rc := s.mem.compare(p, table, key, version, at)
	if rc.Result == decide.ReadStale {
		if s.writes.settledSince(p.role, table, sent) {
			return decide.ReadCheck{Result: decide.ReadPending, Version: rc.Version}
		}
		s.mem.noteStale(p.role, table, key, at)
	}
	return rc
}

// compared reports whether p was compared with what its login role read, so
// that p.key is the key of its row as the server writes it.
func (p *pending) compared() bool {
	return p.readCheck != nil && p.readCheck.Result != decide.ReadNotChecked
}

// mark marks p's write as on its way, just before it goes upstream: in shadow
// mode at the end of Begin, in enforce mode once Begin has decided to send
// it, never for a write Begin refused, and after p's own comparison. Only a
// write by a session the memory is kept for is marked (remembers), and only
// one that may write a table with stale: one to such a table, unless it
// names the table with another schema than the tracked table's and so
// writes another table (otherSchema), or one that may write any table,
// whatever the table it names.
func (s *stale) mark(p *pending) {
	if !s.remembers(p.session) || p.mark != nil {
		return
	}
	if _, ok := s.pks[p.res.Table]; (!ok || s.otherSchema(p.res)) && !writesAnyTable(p.res) {
		return
	}
	if table, key, ok := s.scope(p); ok {
		p.mark = s.mem.writing(p.session.User, table, key)
	}
}

// writesAnyTable reports whether r may write other tables than the one it
// names: its reason says so (correction S9), or it is a message of several
// statements (a PREPARE among them gives it another reason).
func writesAnyTable(r sqlparse.Result) bool {
	return r.Statements > 1 || slices.Contains(everyTable, r.Reason)
}

// scope is what p's mark covers: the row key of table, some row of table
// (key ""), or any table (table ""); ok is false when p needs none.
//
//   - Any table: a write that may write other tables than the one it names
//     (writesAnyTable).
//   - No mark: an INSERT that gives no key, or NULL or DEFAULT, whose new
//     row has a key no read gave.
//   - The row: an UPDATE or DELETE that was compared, by its key as the
//     server writes it, whether its row is there or not (it may be there by
//     the time the write runs), unless it sets the key column, which moves
//     the row to a key not known as the server writes it.
//   - The table: any other write, an INSERT with a key included (its key as
//     the server writes it is known only once it ran), and an UPDATE or
//     DELETE of a bpchar key's row that is not there: the cast wrote that
//     key without the padding the role's reads of the row remember.
func (s *stale) scope(p *pending) (table, key string, ok bool) {
	r, w := p.res, p.res.Write
	switch {
	case writesAnyTable(r):
		return "", "", true
	case r.Kind != sqlparse.Gated || w == nil:
		return r.Table, "", true
	case w.Op == "insert":
		if w.PK == nil || w.PK.Kind == sqlparse.Null || w.PK.Kind == sqlparse.Default {
			return "", "", false
		}
		return r.Table, "", true
	case !p.compared() || slices.Contains(w.Columns, s.pks[r.Table]):
		return r.Table, "", true
	case p.stateVersion == nil:
		if t, ok := s.table(r.Table); !ok || t.base == 1042 { // bpchar
			return r.Table, "", true
		}
	}
	return r.Table, p.key, true
}

// insertKey is the key p, an INSERT, gave its row, as the agent wrote it,
// when a lookup of that text finds the row the server stored (usableKey).
func (s *stale) insertKey(p *pending) (string, bool) {
	w := p.res.Write
	if p.res.Kind != sqlparse.Gated || w == nil || w.Op != "insert" || w.PK == nil || p.pk == nil {
		return "", false
	}
	t, ok := s.table(p.res.Table)
	if !ok || !usableKey(t, *w.PK, p.oids, *p.pk) {
		return "", false
	}
	return *p.pk, true
}

// ended ends p's mark, if it has one, once End was told its outcome o. It
// does not block: a refresh's lookup runs on the workers and ends the mark
// there.
//
//   - Lost: the write may still commit on the server. The mark stays for
//     staleLostGrace, then what it covers is forgotten (corrections S6, S15).
//   - Not run: the mark goes, nothing changes; but a message of several
//     statements that failed may have committed some of them before, and
//     what its mark covers is forgotten.
//   - Run inside a transaction: the mark stays until the session's next
//     TxEnded, which forgets what it covers (spec 7): before the commit
//     Stet's own connection sees the row's old version. A mark the session
//     already keeps may cover it (hold).
//   - Run on its own: an UPDATE of a row looks the row's version up and
//     refreshes what the role read of it; an INSERT with a key looks up the
//     row it stored and refreshes that; anything else, a DELETE included,
//     forgets what its mark covers. A lookup that finds no row, or fails,
//     forgets.
func (s *stale) ended(p *pending, o proxy.Outcome) {
	mk := p.mark
	if s == nil || mk == nil {
		return
	}
	switch {
	case o.Lost:
		s.writes.mu.Lock()
		s.keepLostLocked(p.session.ID, mk)
		s.writes.mu.Unlock()
	case !o.Executed && p.res.Statements > 1:
		s.settle(mk.role, mk.table, func() { s.mem.forgotten(mk) })
	case !o.Executed:
		s.mem.released(mk)
	case o.InTransaction:
		s.writes.mu.Lock()
		s.writes.session(p.session.ID).hold(s.mem, mk)
		s.writes.mu.Unlock()
	case mk.key != "" && p.res.Write.Op == "update":
		s.refresh(mk.table, mk.key, func(serverKey, version string, ok bool) {
			s.settle(mk.role, mk.table, func() {
				if ok && serverKey == mk.key {
					s.mem.refreshed(mk, version)
				} else {
					s.mem.forgotten(mk)
				}
			})
		})
	default:
		key, ok := s.insertKey(p)
		if !ok {
			s.settle(mk.role, mk.table, func() { s.mem.forgotten(mk) })
			return
		}
		s.refresh(mk.table, key, func(serverKey, version string, ok bool) {
			s.settle(mk.role, mk.table, func() {
				if !ok {
					s.mem.forgotten(mk)
					return
				}
				// Before the write's own mark ends: it starts the span the
				// version may have been seen in.
				s.mem.refreshedRow(mk.role, mk.table, serverKey, version)
				s.mem.released(mk)
			})
		})
	}
}

// keepLostLocked keeps mk, the mark of what the connection of session sent
// that the server may still commit after the connection ended (a write lost
// with it, or what TxEnded was told is unanswered), for staleLostGrace, then
// forgets what it covers. After close it forgets at once.
//
// The marks of one session wait on one timer, started by the first: they
// are all kept as its connection ends, in one burst of End and TxEnded
// calls, so the later ones wait a moment less than the first. A timer for
// each would be one for every row a long transaction wrote. It runs with
// s.writes.mu held.
func (s *stale) keepLostLocked(session string, mk *writeMark) {
	w := &s.writes
	if w.closed {
		s.settleLocked(mk.role, mk.table, func() { s.mem.forgotten(mk) })
		return
	}
	l := w.lost[session]
	if l == nil {
		l = &lostMarks{}
		if w.lost == nil {
			w.lost = map[string]*lostMarks{}
		}
		w.lost[session] = l
		l.timer = time.AfterFunc(staleLostGrace, func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.lost[session] != l { // close or endLost ended them
				return
			}
			delete(w.lost, session)
			s.forgetLostLocked(l)
		})
	}
	l.marks = append(l.marks, mk)
}

// forgetLostLocked ends the marks of l, forgetting what each covers. It runs
// with s.writes.mu held.
func (s *stale) forgetLostLocked(l *lostMarks) {
	for _, mk := range l.marks {
		s.settleLocked(mk.role, mk.table, func() { s.mem.forgotten(mk) })
	}
}

// closeWrites ends at once the marks still waiting out their grace, and
// keeps any mark from waiting after it.
func (s *stale) closeWrites() {
	s.writes.mu.Lock()
	defer s.writes.mu.Unlock()
	s.writes.closed = true
	s.endLostLocked()
}

// endLost ends at once, as their timers would, the marks still waiting out
// their grace.
func (s *stale) endLost() {
	s.writes.mu.Lock()
	defer s.writes.mu.Unlock()
	s.endLostLocked()
}

// endLostLocked is endLost with s.writes.mu held. No timer does anything
// after it: one that fired finds its marks gone, under the lock held here.
func (s *stale) endLostLocked() {
	w := &s.writes
	for _, l := range w.lost {
		l.timer.Stop()
		s.forgetLostLocked(l)
	}
	w.lost = nil
}

// unseen marks every table for the login role of sess, a shadow session
// sending a message whose writes Stet cannot see, and forgets everything the
// role read (spec 7, correction S7). The mark is one per session; it stays
// until TxEnded has settled every Unseen call of the session. A call for a
// session the memory is not kept for (remembers: made before the login role
// was known, or by a session connected to another database, whose messages
// change nothing here) marks and forgets nothing, but is counted: the
// proxy's counts settle it too.
func (s *stale) unseen(sess proxy.SessionInfo) {
	if s == nil {
		return
	}
	known := s.remembers(sess)
	w := &s.writes
	w.mu.Lock()
	sw := w.session(sess.ID)
	sw.unseen++
	if known && sw.mark == nil {
		sw.mark = s.mem.writing(sess.User, "", "")
	}
	w.mu.Unlock()
	if known {
		s.mem.forget(sess.User, "", "")
	}
}

// txEnded settles what sess sent in the units the server has answered, now
// committed or rolled back: the marks of its writes that ran inside a
// transaction end, and what they covered is forgotten; with unseen Unseen
// calls settled, everything its login role read is forgotten, and once all of
// the session's Unseen calls are settled its mark on every table ends.
//
// unanswered says the connection ended with something it sent unanswered,
// which the server may still commit a little later (a message waiting on a
// lock, a COMMIT in flight). The marks that end then wait out staleLostGrace
// first, as a lost write's does (correction S15), and forget what they cover
// when they end: reads meanwhile may have learned versions from before. What
// the role read is forgotten at once all the same.
func (s *stale) txEnded(sess proxy.SessionInfo, unseen int, unanswered bool) {
	if s == nil {
		return
	}
	w := &s.writes
	w.mu.Lock()
	defer w.mu.Unlock()
	sw := w.sessions[sess.ID]
	if sw == nil {
		return
	}
	end := func(mk *writeMark) {
		if unanswered {
			s.keepLostLocked(sess.ID, mk)
		} else {
			s.settleLocked(mk.role, mk.table, func() { s.mem.forgotten(mk) })
		}
	}
	for _, mk := range sw.takeHeld() {
		end(mk)
	}
	if unseen > 0 {
		sw.unseen = max(0, sw.unseen-unseen)
		var mk *writeMark
		role := "" // of a session the memory is not kept for, nothing is forgotten
		if s.remembers(sess) {
			role = sess.User
		}
		if sw.unseen == 0 && sw.mark != nil {
			mk, sw.mark = sw.mark, nil
			role = mk.role
		}
		if role != "" {
			s.settleLocked(role, "", func() { s.mem.forget(role, "", "") })
		}
		if mk != nil {
			end(mk)
		}
	}
	if sw.unseen == 0 && sw.mark == nil {
		delete(w.sessions, sess.ID)
	}
}

// Applying tells the gate that Stet is about to run, on its own connection
// and as role, an approved write of table (correction S10). done is called
// once it has committed or failed. Until then role's writes to table are
// own_write, and then what role remembers of table is forgotten. For a table
// without stale it does nothing. Safe after Close, and done may be called
// more than once: only the first call counts.
func (g *Gate) Applying(role, table string) (done func()) {
	s := g.stale
	if s == nil || role == "" || !s.remembersAny([]string{table}) {
		return func() {}
	}
	mk := s.mem.writing(role, table, "")
	var once sync.Once
	return func() {
		once.Do(func() { s.settle(role, table, func() { s.mem.forgotten(mk) }) })
	}
}

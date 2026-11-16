package proxy

// CursorOp is what a statement does to a SQL cursor.
type CursorOp int

const (
	CursorDeclare CursorOp = iota + 1
	CursorFetch
	CursorClose
	CursorCloseAll
)

// Cursor says what a statement does to a SQL cursor.
//
// A message whose only statement is a DECLARE is never reported itself: it
// sends no rows. When its Statement.Observe is set, the statement is
// remembered under the name and reported as the read behind each later FETCH.
// When it is not set, the name is forgotten, so a cursor redeclared over an
// unobserved query stops being reported. In a message of several statements
// the same holds with the whole message as the DECLARE (see applyCursors).
type Cursor struct {
	Op   CursorOp
	Name string // compared exactly; the gate normalizes it. "" for CursorCloseAll
	Hold bool   // DECLARE ... WITH HOLD
}

// declared is a DECLARE or CLOSE of a cursor name, as the session remembers
// it. Those not answered yet stack, newest first, on the one in force, so a
// failed one can be taken out and the name means what it meant before.
type declared struct {
	st   Statement // the DECLARE's message; with Observe unset (a CLOSE included) it forgets the name
	hold bool
	// unit holds the statement until the transaction it ran in has ended. Only
	// a held cursor outlives that, with unit nil: it is settled.
	unit *unit
	idx  int       // which Execute of unit stacked it; 0 for a Query
	prev *declared // what the name meant before, until this statement has run
	// rows read through the cursor so far; a new DECLARE of the name starts
	// over. Updated on the server side as its reads are answered (tally).
	rows int64
}

// undone reports whether d, stacked by head, a unit that failed, is taken
// back.
//
// Under one Sync the Executes before the failing one ran, and what they did
// stays: a DECLARE then meets the transaction-end rule like any other (a held
// one is kept although an implicit transaction may have rolled it back), and
// a CLOSE that ran stays done, since a rollback does not reopen a cursor. The
// failing Execute and the ones after it did not run.
//
// A Query of one statement failed in that statement. Of a Query of several,
// the proxy cannot tell which statements ran, nor whether one of them
// committed. When unsure the table remembers: what forgets a name (a CLOSE,
// an unobserved DECLARE) is undone, so the older meaning comes back. An
// observed DECLARE is kept when the name meant nothing before it, and undone
// when it did: the server refuses a DECLARE of a name it holds (42P03), and
// keeping the new entry would drop the cursor the server still has. If the
// server took it (the older one was gone), its FETCHes count as reads of the
// older DECLARE.
func (d *declared) undone(head *unit) bool {
	switch {
	case !head.query:
		return d.idx >= head.errAt
	case d.st.Several:
		return !d.st.Observe || d.prev != nil
	}
	return true
}

// applyCursors applies the cursor statements of st as the message arrives
// from the client, so that a FETCH pipelined behind a DECLARE or CLOSE sees
// them. idx is as for observe. The statements apply in order, all under u.
//
// A DECLARE is remembered when st is observed; any other DECLARE or CLOSE of
// a remembered name stacks an entry that forgets it once it ran. A message
// that FETCHes from a remembered observed cursor joins u as a read of that
// cursor's DECLARE (the first such cursor, for a message of several). A
// message of several statements that is observed itself is reported as its
// own read instead, once, and when it holds a DECLARE only if it sent rows:
// "begin; declare ..." sends none, and its reads are the FETCHes that follow.
//
// When the cursor table must guess, it errs toward remembering: a cursor
// remembered after the server dropped it costs a failed FETCH reported with
// no rows, while one forgotten that the server still holds hides reads. So a
// CLOSE forgets the name only once it ran, like an unobserved DECLARE.
func (s *session) applyCursors(u *unit, idx int, st Statement) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var fetched, declaredHere *declared
	declares := false
	for _, c := range st.Cursors {
		switch c.Op {
		case CursorDeclare:
			declares = true
			if st.Observe || s.cursors[c.Name] != nil {
				d := &declared{st: st, hold: c.Hold, unit: u, idx: idx}
				s.stack(c.Name, d)
				declaredHere = d
			}
		case CursorFetch:
			if d := s.cursors[c.Name]; fetched == nil && d != nil && d.st.Observe {
				fetched = d
			}
		case CursorClose:
			if s.cursors[c.Name] != nil {
				s.stack(c.Name, &declared{unit: u, idx: idx})
			}
		case CursorCloseAll:
			for name := range s.cursors {
				s.stack(name, &declared{unit: u, idx: idx})
			}
		}
	}
	switch {
	case st.Several && st.Observe && !st.Watch:
		// Its rows, if it fetched from the cursor it declared, are that
		// cursor's first.
		var total *int64
		if declaredHere != nil {
			total = &declaredHere.rows
		}
		r := s.arriving(st, idx, total)
		r.ifRows = declares
		u.reads = append(u.reads, r)
	case fetched != nil:
		u.reads = append(u.reads, s.arriving(fetched.st, idx, &fetched.rows))
	}
}

// stack puts d on top of what name means. It runs with s.mu held.
func (s *session) stack(name string, d *declared) {
	if s.cursors == nil {
		s.cursors = map[string]*declared{}
		s.unsettled = map[string]struct{}{}
	}
	d.prev = s.cursors[name]
	s.cursors[name] = d
	s.unsettled[name] = struct{}{}
}

// cursorsAnswered brings the unsettled names up to date once head was
// answered and left s.units, after being the transaction status it reported.
// The DECLAREs and CLOSEs of a failed unit that did not run are undone (see
// undone). Once a statement ran, what the name meant before it is gone. When
// a transaction ends (after is idle) the cursors declared in it are
// forgotten, except held ones. It runs on the server side with s.mu held.
func (s *session) cursorsAnswered(head *unit, after byte) {
	for name := range s.unsettled {
		var first *declared // what stays, newest first
		tail := &first
		for d := s.cursors[name]; d != nil; {
			next := d.prev
			keep := false
			switch {
			case d.unit == head && head.err != "" && d.undone(head):
				// It failed or did not run: the name means what it meant before.
			case d.unit != nil && s.pending(d.unit):
				keep = true // not answered yet
			default:
				keep = d.stays(after)
				next = nil
			}
			if keep {
				*tail = d
				tail = &d.prev
			}
			d = next
		}
		*tail = nil
		switch {
		case first == nil:
			delete(s.cursors, name)
			delete(s.unsettled, name)
		case first.unit == nil: // a held cursor alone: nothing left to change
			s.cursors[name] = first
			delete(s.unsettled, name)
		default:
			s.cursors[name] = first
		}
	}
}

// stays reports whether d, a DECLARE or CLOSE that ran, is still remembered
// once a unit was answered with status after. A held cursor whose transaction
// ended is settled.
//
// A held cursor stays whichever way its transaction ended. The status cannot
// tell a COMMIT from a ROLLBACK, and an aborted transaction may not be the
// one that declared it (COMMIT AND CHAIN commits without an idle status), so
// a held cursor rolled back with its transaction stays remembered.
func (d *declared) stays(after byte) bool {
	switch {
	case !d.st.Observe:
		return false // it forgot the name
	case d.unit == nil || after != 'I':
		return true // settled, or its transaction is still open
	case !d.hold:
		return false // its transaction ended
	}
	d.unit = nil
	return true
}

// pending reports whether u is still waiting for its ReadyForQuery.
func (s *session) pending(u *unit) bool {
	for _, p := range s.units {
		if p == u {
			return true
		}
	}
	return false
}

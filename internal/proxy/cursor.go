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
// A CursorDeclare statement is never reported itself: it sends no rows. When
// its Statement.Observe is set, the statement is remembered under the name
// and reported as the read behind each later FETCH. When it is not set, the
// name is forgotten, so a cursor redeclared over an unobserved query stops
// being reported.
type Cursor struct {
	Op   CursorOp
	Name string // compared exactly; the gate normalizes it. "" for CursorCloseAll
	Hold bool   // DECLARE ... WITH HOLD
}

// declared is a DECLARE or CLOSE of a cursor name, as the session remembers
// it. Those not answered yet stack, newest first, on the one in force, so a
// failed one can be taken out and the name means what it meant before.
type declared struct {
	st   Statement // the DECLARE; with Observe unset (a CLOSE included) it forgets the name
	hold bool
	// unit holds the statement until the transaction it ran in has ended. Only
	// a held cursor outlives that, with unit nil: it is settled.
	unit *unit
	idx  int       // which Execute of unit stacked it; 0 for a Query
	prev *declared // what the name meant before, until this statement has run
}

// undone reports whether d, stacked by head, a unit that failed, is taken
// back. Under one Sync the Executes before the failing one ran, and what
// they did stays: a DECLARE then meets the transaction-end rule like any
// other (a held one is kept although an implicit transaction may have rolled
// it back), and a CLOSE that ran stays done, since a rollback does not reopen
// a cursor. The failing Execute and the ones after it did not run. A Query
// holds one statement, and that one failed.
func (d *declared) undone(head *unit) bool {
	return head.query || d.idx >= head.errAt
}

// cursor applies a cursor statement as it arrives from the client, so that a
// FETCH pipelined behind a DECLARE or CLOSE sees it. A FETCH of a remembered
// cursor joins u as a read of the DECLARE; idx is as for observe.
//
// When the cursor table must guess, it errs toward remembering: a cursor
// remembered after the server dropped it costs a failed FETCH reported with
// no rows, while one forgotten that the server still holds hides reads. So a
// CLOSE forgets the name only once it ran, like an unobserved DECLARE.
func (s *session) cursor(u *unit, idx int, st Statement) {
	c := st.Cursor
	s.mu.Lock()
	defer s.mu.Unlock()
	switch c.Op {
	case CursorDeclare:
		if st.Observe || s.cursors[c.Name] != nil {
			s.stack(c.Name, &declared{st: st, hold: c.Hold, unit: u, idx: idx})
		}
	case CursorFetch:
		if d := s.cursors[c.Name]; d != nil && d.st.Observe {
			u.reads = append(u.reads, s.arriving(d.st, idx))
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

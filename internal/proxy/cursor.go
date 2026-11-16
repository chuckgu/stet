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

// declared is a DECLARE of a cursor name, as the session remembers it. The
// DECLAREs of a name that are not answered yet stack, newest first, on the
// one in force, so a failed one can be taken out.
type declared struct {
	st   Statement // the DECLARE; with Observe unset it only forgets the name
	hold bool
	// unit holds the DECLARE until the transaction it ran in has ended. Only a
	// held cursor outlives that, with unit nil.
	unit *unit
	prev *declared // what the name meant before, until this DECLARE has run
}

// cursor applies a cursor statement as it arrives from the client, so that a
// FETCH pipelined behind a DECLARE finds it. A FETCH of a remembered cursor
// joins u as a read of the DECLARE; idx is as for observe.
func (s *session) cursor(u *unit, idx int, st Statement) {
	c := st.Cursor
	s.mu.Lock()
	defer s.mu.Unlock()
	switch c.Op {
	case CursorDeclare:
		prev := s.cursors[c.Name]
		if !st.Observe && prev == nil {
			return // nothing to forget
		}
		if s.cursors == nil {
			s.cursors = map[string]*declared{}
		}
		s.cursors[c.Name] = &declared{st: st, hold: c.Hold, unit: u, prev: prev}
	case CursorFetch:
		if d := s.cursors[c.Name]; d != nil && d.st.Observe {
			u.reads = append(u.reads, observedRead{info: s.info, st: d.st, idx: idx})
		}
	case CursorClose:
		delete(s.cursors, c.Name)
	case CursorCloseAll:
		s.cursors = nil
	}
}

// cursorsAnswered brings the cursor table up to date once head was answered
// and left s.units, before and after being the transaction status around it.
// The DECLAREs of a failed unit are undone. When a transaction ends (after is
// idle) the cursors declared in it are forgotten, except held ones whose
// transaction did not fail. It runs on the server side with s.mu held.
func (s *session) cursorsAnswered(head *unit, before, after byte) {
	for name, top := range s.cursors {
		var first *declared // what stays, newest first
		tail := &first
		for d := top; d != nil; {
			next := d.prev
			keep := false
			switch {
			case d.unit == head && head.err != "":
				// It failed or did not run: the name means what it meant before.
			case d.unit != nil && s.pending(d.unit):
				keep = true // not answered yet
			default:
				// It ran, so what the name meant before it is gone.
				keep = d.stays(head, before, after)
				next = nil
			}
			if keep {
				*tail = d
				tail = &d.prev
			}
			d = next
		}
		*tail = nil
		if first == nil {
			delete(s.cursors, name)
		} else {
			s.cursors[name] = first
		}
	}
}

// stays reports whether d, a DECLARE that ran, is still remembered once head
// was answered. A held cursor that outlives its transaction loses its unit.
func (d *declared) stays(head *unit, before, after byte) bool {
	switch {
	case !d.st.Observe:
		return false // it only forgot the name
	case d.unit == nil || after != 'I':
		return true // held from an earlier transaction, or its own is still open
	case !d.hold:
		return false // its transaction ended
	case d.unit != head && before == 'E':
		return false // its transaction failed and was rolled back
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

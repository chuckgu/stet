package proxy

import "slices"

// names follows the prepared statements, or the portals, of one session as
// the server holds them. Keys are the first 63 bytes of a name.
//
// A Parse, Bind or Close changes a name only once the server completed it
// (ParseComplete, BindComplete, CloseComplete): from then on it holds,
// whatever comes later in its unit (completed). Until then the change waits,
// and the name may mean what the change made it or what it meant before
// (get). At the unit's ReadyForQuery the changes the server did not complete
// are dropped (answered).
//
// When the proxy cannot tell which meaning a name has, the meaning that is a
// watched write must stay possible: get hands back every meaning the name
// may have, and a message the proxy could not read leaves the names it may
// have set meaning something unread.
//
// A change is kept only while it can still decide something, so a batch
// under one Sync does not grow the list with its rows (see put).
type names[T meaning[T]] struct {
	held map[string]T // what the server confirmed
	// changes[done:] wait for the server's answer, oldest first; a unit's
	// changes are contiguous. changes[:done] were settled and cleared.
	changes []change[T]
	done    int
	// blind: a change whose name the proxy could not read held, so a name
	// held does not list may mean unread, what that change set.
	blind  bool
	unread T
	// freed: the server may let go of any name the table holds without a
	// message the table follows, so held does not mean the server still holds
	// it. Set for portals: the server drops them all when a transaction ends,
	// which a COMMIT not answered yet may already have done.
	freed bool
}

// meaning is what a name means in a names table.
type meaning[T any] interface {
	// same reports whether the name means the same with either value.
	same(T) bool
	// merged is the value kept for this one and a later one that means the
	// same (see put): its settings records count whenever either's do (see
	// readUnder).
	merged(T) T
}

type change[T any] struct {
	u    *unit
	pos  int    // the message's pos in u
	name string // its key
	set  bool   // a Parse or Bind; false for a Close
	// blind: the message could not be read, so its name is unknown. The
	// server refuses a Parse or Bind of a named statement or portal it holds,
	// so the change may have set the unnamed one, or any name the server did
	// not hold.
	blind bool
	v     T
}

// keepChanges is how many changes the list keeps room for once it is empty;
// a larger array, grown by a batch, is let go.
const keepChanges = 64

// nameKey is the part of a statement or portal name the server compares.
func nameKey(name string) string {
	if len(name) > maxName {
		return name[:maxName]
	}
	return name
}

// put notes a Parse or Bind (set) or a Close (!set) of name, sent as message pos of u.
//
// It is not kept when the change before it to the name in u, with no unread
// change between them, already gives the name the same meaning: whichever of
// the two the server reaches, the name means the same. The server may have
// read the later one under other settings, so the change kept takes the
// records of both (merged). In a unit that
// already failed, the server skips every message still to come, so none of
// its waiting changes will hold; only the newest per name counts, for the
// messages behind it in u, and it takes the place of the one before.
func (n *names[T]) put(u *unit, pos int, name string, set bool, v T) {
	c := change[T]{u: u, pos: pos, name: nameKey(name), set: set, v: v}
	if i := n.last(u, c.name); i >= 0 {
		switch prev := &n.changes[i]; {
		case prev.set == set && prev.v.same(v):
			prev.v = prev.v.merged(v)
			return
		case u.errPos != 0 && prev.pos >= u.errPos:
			*prev = c
			return
		}
	}
	n.changes = append(n.changes, c)
}

// putUnread notes a Parse or Bind the proxy could not read, sent as message
// pos of u. v stands for what it may have set a name to.
func (n *names[T]) putUnread(u *unit, pos int, v T) {
	n.changes = append(n.changes, change[T]{u: u, pos: pos, set: true, blind: true, v: v})
}

// lookBack is how many waiting changes put looks back over. What it looks
// for only spares memory, so it stops there rather than walk a long unit on
// every message.
const lookBack = 16

// last returns the index of the newest waiting change u made to the name
// with key k, or -1 when there is none among the last lookBack changes, or
// an unread change of u came after it (it may have been to k).
func (n *names[T]) last(u *unit, k string) int {
	stop := max(n.done, len(n.changes)-lookBack)
	for i := len(n.changes) - 1; i >= stop && n.changes[i].u == u; i-- {
		switch c := &n.changes[i]; {
		case c.blind:
			return -1
		case c.name == k:
			return i
		}
	}
	return -1
}

// get is what name means to a message arriving now in u. A change made
// earlier in u itself counts at once: if it fails, the server skips the rest
// of u. A change from an earlier unit not yet answered may hold or not, so
// the name can mean several things: v is the newest and others the rest.
//
// A change the proxy could not read adds a meaning to the unnamed name, and
// to a named one the server may not hold at that point; a name the server
// surely holds stays as it is, since a Parse or Bind of it fails. A portal
// name is never sure (see freed).
//
// u is the unit still collecting messages, so its changes are the newest.
// The newest of them to name decides, and the walk starts there.
func (n *names[T]) get(u *unit, name string) (v T, ok bool, others []T) {
	k := nameKey(name)
	v, ok = n.held[k]
	free := !ok || k == "" || n.freed // the server may not hold the name
	if !ok && n.blind {
		v, ok = n.unread, true
	}
	from := n.done
	for i := len(n.changes) - 1; i >= n.done && n.changes[i].u == u; i-- {
		if c := &n.changes[i]; c.name == k && !c.blind {
			v, ok, from = c.v, c.set, i+1
			free = !c.set || k == "" || n.freed
			break
		}
	}
	for i := from; i < len(n.changes); i++ {
		c := &n.changes[i]
		switch {
		case c.blind && !free:
			continue
		case !c.blind && c.name != k:
			continue
		}
		if ok {
			others = append(others, v)
		}
		v, ok = c.v, c.set
		if !c.set {
			free = true // a Close that may hold
		}
	}
	return v, ok, others
}

// completed applies the changes of u, the unit being answered, that the
// server has completed so far (pos <= acks), and lets them go. u's changes
// are the oldest waiting, since units are answered in the order they were
// sent.
func (n *names[T]) completed(u *unit, acks int) {
	i := n.done
	for ; i < len(n.changes) && n.changes[i].u == u && n.changes[i].pos <= acks; i++ {
		n.apply(n.changes[i])
	}
	n.settle(i)
}

// answered settles u's changes at its ReadyForQuery: those at pos <= acks
// hold, the rest are dropped. Units are answered in the order they were
// sent, so u's changes are the oldest.
func (n *names[T]) answered(u *unit, acks int) {
	i := n.done
	for ; i < len(n.changes) && n.changes[i].u == u; i++ {
		if c := n.changes[i]; c.pos <= acks {
			n.apply(c)
		}
	}
	n.settle(i)
}

// settle lets go of the changes before index i, which were applied or
// dropped.
func (n *names[T]) settle(i int) {
	if i == n.done {
		return
	}
	clear(n.changes[n.done:i]) // let go of their values (bind values, say)
	n.done = i
	switch {
	case n.done == len(n.changes) && cap(n.changes) > keepChanges:
		n.changes, n.done = nil, 0
	case n.done == len(n.changes):
		n.changes, n.done = n.changes[:0], 0
	case n.done > len(n.changes)/2: // more settled than waiting: move the rest down
		end := len(n.changes)
		n.changes = n.changes[:copy(n.changes, n.changes[n.done:])]
		clear(n.changes[len(n.changes):end])
		n.done = 0
	}
}

// apply makes the change c, which the server completed, hold.
func (n *names[T]) apply(c change[T]) {
	switch {
	case c.blind:
		delete(n.held, "")
		n.blind, n.unread = true, c.v
	case c.set:
		if n.held == nil {
			n.held = map[string]T{}
		}
		n.held[c.name] = c.v
	default:
		delete(n.held, c.name)
	}
}

// forget drops every name the server confirmed, as the server drops every
// portal when a transaction ends. Changes not answered yet stay.
func (n *names[T]) forget() {
	clear(n.held)
	var zero T
	n.blind, n.unread = false, zero
}

// same reports whether p and q are the same statement: the gate classifies
// SQL alone, so the same text and parameter types are the same statement.
func (p prepared) same(q prepared) bool {
	return p.unread == q.unread && p.st.SQL == q.st.SQL && slices.Equal(p.oids, q.oids)
}

// merged is p with the Parse record of q too.
func (p prepared) merged(q prepared) prepared {
	p.atParse = p.atParse.with(q.atParse)
	return p
}

// same reports whether portals a and b run the same statement and Stet keeps
// nothing of either beyond it: no bind values, no row count, no other
// meanings. Two portals of statements Stet does not follow are the same.
func (a bound) same(b bound) bool {
	return !a.unread && !b.unread && a.params == nil && b.params == nil && a.rows == nil && b.rows == nil &&
		len(a.others) == 0 && len(b.others) == 0 && a.st.SQL == b.st.SQL
}

// merged is a with the Parse and Bind records of b too. A watched write's
// portal keeps its bind values, so it is never the same as another and never
// merged.
func (a bound) merged(b bound) bound {
	a.atParse, a.atBind = a.atParse.with(b.atParse), a.atBind.with(b.atBind)
	return a
}

// mayRunUnclean reports whether an Execute of a portal whose newest meaning
// is b may run, in its place, a statement that is not clean or is unknown:
// one of others, the portal name's other meanings (see names.get), or one
// its statement's name meant at the Bind of b or of one of others.
func mayRunUnclean(b bound, others []bound) bool {
	return anyOther(b, others, func(st Statement, unread bool) bool { return unread || unclean(st) })
}

// anyOther reports whether f holds for a statement an Execute of a portal
// whose newest meaning is b may run in its place: one of others, the portal
// name's other meanings (see names.get), or one its statement's name meant
// at the Bind of b or of one of others (bound.others). unread says what the
// statement is is not known.
func anyOther(b bound, others []bound, f func(st Statement, unread bool) bool) bool {
	for _, p := range b.others {
		if f(p.st, p.unread) {
			return true
		}
	}
	for _, o := range others {
		if f(o.st, o.unread) {
			return true
		}
		for _, p := range o.others {
			if f(p.st, p.unread) {
				return true
			}
		}
	}
	return false
}

// mayWrite reports whether a statement name whose newest meaning is p (ok:
// it has one) and whose other meanings are others may be a watched write,
// one Stet cannot read, or one that runs names.
func mayWrite(p prepared, ok bool, others []prepared) bool {
	w := func(q prepared) bool { return q.unread || q.st.Watch || runsNames(q.st) }
	return ok && w(p) || slices.ContainsFunc(others, w)
}

// anyMeaning is anyOther with b's own statement too, when the portal has a
// newest meaning (ok).
func anyMeaning(b bound, ok bool, others []bound, f func(st Statement, unread bool) bool) bool {
	return ok && f(b.st, b.unread) || anyOther(b, others, f)
}

// executeBlock is why an enforced session refuses an Execute arriving in u
// of a portal whose newest meaning is b (ok: it has one) and whose other
// meanings are others, or "".
//
// When the portal may run more than one statement and one of them is a
// watched write or unknown, it is refused: the server has not said which
// one holds, and Begin can judge only one (spec correction B4). A portal is
// bound for its lifetime to the meanings its statement's name had at the
// Bind, so it stays refused after the change was answered: the lesser
// failure. When a statement it may run runs a watched name in turn, it is
// refused as such (see runsWatched). The gate is not asked. It runs with s.mu
// held.
func (s *session) executeBlock(u *unit, b bound, ok bool, others []bound) string {
	if len(b.others) > 0 || len(others) > 0 {
		if anyMeaning(b, ok, others, func(st Statement, unread bool) bool { return unread || st.Watch }) {
			return BlockUnsettledName
		}
	}
	if anyMeaning(b, ok, others, func(st Statement, _ bool) bool { return s.runsWatched(u, st, maxRunDepth) }) {
		return BlockPreparedWrite
	}
	return ""
}

// maxRunDepth is how many names deep runsWatched follows a statement that
// runs a name whose statement runs a name in turn.
const maxRunDepth = 4

// runsNames reports whether st runs a prepared statement or a portal by name.
func runsNames(st Statement) bool { return len(st.Executes) > 0 || len(st.Portals) > 0 }

// runsWatched reports whether st, arriving now in u, may run a watched write
// by name, where the classifier sees no table (spec correction B5): a
// prepared statement it runs with SQL EXECUTE, or a portal it reads with
// FETCH or MOVE, under any meaning the name may have (see names.get). A
// meaning that is unknown counts as watched. A meaning that runs names in
// turn is followed depth more names deep, and past that counts as watched;
// a statement can run itself. It runs with s.mu held.
func (s *session) runsWatched(u *unit, st Statement, depth int) bool {
	if !runsNames(st) {
		return false
	}
	if depth == 0 {
		return true
	}
	watched := func(st Statement, unread bool) bool {
		return unread || st.Watch || s.runsWatched(u, st, depth-1)
	}
	for _, name := range st.Executes {
		p, ok, others := s.stmts.get(u, name)
		if ok && watched(p.st, p.unread) {
			return true
		}
		for _, o := range others {
			if watched(o.st, o.unread) {
				return true
			}
		}
	}
	for _, name := range st.Portals {
		if b, ok, others := s.portals.get(u, name); anyMeaning(b, ok, others, watched) {
			return true
		}
	}
	return false
}

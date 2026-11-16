package proxy

// names follows the prepared statements, or the portals, of one session as
// the server holds them. Keys are the first 63 bytes of a name.
//
// A Parse, Bind or Close changes a name only once the server completed it.
// Until the ReadyForQuery of its unit the change waits, and the name may
// mean what the change made it or what it meant before (get). At that
// ReadyForQuery the unit's completions say which changes held (answered).
//
// When the proxy cannot tell which meaning a name has, the meaning that is a
// watched write must stay possible: get hands back every meaning the name
// may have, and a message the proxy could not read leaves the names it may
// have set meaning something unread.
type names[T any] struct {
	held    map[string]T // what the server confirmed
	changes []change[T]  // not answered yet, oldest first
	// blind: a change whose name the proxy could not read held, so a name
	// held does not list may mean unread, what that change set.
	blind  bool
	unread T
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

// nameKey is the part of a statement or portal name the server compares.
func nameKey(name string) string {
	if len(name) > maxName {
		return name[:maxName]
	}
	return name
}

// put notes a Parse or Bind (set) or a Close (!set) of name, sent as message pos of u.
func (n *names[T]) put(u *unit, pos int, name string, set bool, v T) {
	n.changes = append(n.changes, change[T]{u: u, pos: pos, name: nameKey(name), set: set, v: v})
}

// putUnread notes a Parse or Bind the proxy could not read, sent as message
// pos of u. v stands for what it may have set a name to.
func (n *names[T]) putUnread(u *unit, pos int, v T) {
	n.changes = append(n.changes, change[T]{u: u, pos: pos, set: true, blind: true, v: v})
}

// get is what name means to a message arriving now in u. A change made
// earlier in u itself counts at once: if it fails, the server skips the rest
// of u. A change from an earlier unit not yet answered may hold or not, so
// the name can mean several things: v is the newest and others the rest.
// A change the proxy could not read may or may not have been to name, so it
// adds a meaning wherever it was sent.
//
// u is the unit still collecting messages, so its changes are the newest.
// The newest of them to name decides, and the walk starts there.
func (n *names[T]) get(u *unit, name string) (v T, ok bool, others []T) {
	k := nameKey(name)
	from := 0
	v, ok = n.held[k]
	if !ok && n.blind {
		v, ok = n.unread, true
	}
	for i := len(n.changes) - 1; i >= 0 && n.changes[i].u == u; i-- {
		if c := n.changes[i]; c.name == k && !c.blind {
			v, ok, from = c.v, c.set, i+1
			break
		}
	}
	for _, c := range n.changes[from:] {
		if c.name != k && !c.blind {
			continue
		}
		if ok {
			others = append(others, v)
		}
		v, ok = c.v, c.set
	}
	return v, ok, others
}

// answered settles u's changes at its ReadyForQuery: those at pos <= acks
// hold, the rest are dropped. Units are answered in the order they were
// sent, so u's changes are the oldest.
func (n *names[T]) answered(u *unit, acks int) {
	i := 0
	for ; i < len(n.changes) && n.changes[i].u == u; i++ {
		if c := n.changes[i]; c.pos <= acks {
			n.apply(c)
		}
	}
	if i == 0 {
		return
	}
	end := len(n.changes)
	n.changes = append(n.changes[:0], n.changes[i:]...)
	clear(n.changes[len(n.changes):end]) // let go of their values (bind values, say)
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

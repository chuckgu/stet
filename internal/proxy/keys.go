package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5/pgproto3"
)

// KeyType is how the proxy reads the binary form of a tracked table's key
// column (see Gate.Tracked).
type KeyType uint8

const (
	KeyInt2 KeyType = iota + 1
	KeyInt4
	KeyInt8
	KeyUUID
	KeyText // text, varchar, bpchar, name: the bytes as they are
)

// MaxReadKeys is how many key values the proxy takes from one read.
const MaxReadKeys = 1000

// maxKeyLen is the longest key value taken, in bytes (spec correction S2).
const maxKeyLen = 1024

// maxUnitKeys is how many keys the reads of one unit take in all, so a batch
// of reads under one Sync holds no more than this many until it is answered.
const maxUnitKeys = 16 * MaxReadKeys

// RowKey names one row of a tracked table.
type RowKey struct {
	Table uint32 // the table's OID
	Text  string // the key as the type's output function writes it
}

// ReadKeys is what the proxy took from the answer to a tracked read.
type ReadKeys struct {
	Keys   []RowKey // from the rows sent, in order, at most MaxReadKeys
	Tables []uint32 // the tracked tables the result had a column of, in column order, each once
	Param  *Param   // the value bound to Statement.KeyParam; nil when there is none
}

// KeyOutput is the key value v, sent in format (0 text, 1 binary), as the
// output function of a type read as typ writes it. ok is false for NULL, for
// a binary value whose length does not fit typ, for a KeyText value holding
// a byte that is not ASCII, and for a value longer than 1024 bytes.
//
// A value in text format is the type's output already, taken as it is. Of a
// KeyText value only ASCII is taken, in either format: the server wrote it in
// the session's client_encoding, which the proxy does not follow on every
// session, so the same bytes may name another row on Stet's own connection
// (spec correction S2).
func KeyOutput(typ KeyType, format int16, v []byte) (text string, ok bool) {
	if v == nil || len(v) > maxKeyLen || typ == KeyText && !asciiBytes(v) {
		return "", false
	}
	switch {
	case format == 0 && typ >= KeyInt2 && typ <= KeyText:
		return string(v), true
	case format != 1:
		return "", false
	}
	switch typ {
	case KeyInt2:
		if len(v) == 2 {
			return strconv.FormatInt(int64(int16(binary.BigEndian.Uint16(v))), 10), true
		}
	case KeyInt4:
		if len(v) == 4 {
			return strconv.FormatInt(int64(int32(binary.BigEndian.Uint32(v))), 10), true
		}
	case KeyInt8:
		if len(v) == 8 {
			return strconv.FormatInt(int64(binary.BigEndian.Uint64(v)), 10), true
		}
	case KeyUUID:
		if len(v) == 16 {
			return uuidText(v), true
		}
	case KeyText:
		return string(v), true
	}
	return "", false
}

// uuidText writes the 16 bytes of a uuid as its output function does:
// lower-case hex, grouped 8-4-4-4-12.
func uuidText(b []byte) string {
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	hex.Encode(s[9:13], b[4:6])
	hex.Encode(s[14:18], b[6:8])
	hex.Encode(s[19:23], b[8:10])
	hex.Encode(s[24:36], b[10:16])
	s[8], s[13], s[18], s[23] = '-', '-', '-', '-'
	return string(s[:])
}

// asciiBytes is ascii for a byte slice.
func asciiBytes(v []byte) bool {
	for _, c := range v {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

// tracks reports whether st, arriving as a Query or an Execute, is a read
// whose rows the gate wants the keys of: observed, not watched, marked Track,
// read-only (otherwise its rows may show its own changes, which Stet's
// connection cannot see), and with no cursor statement (a FETCH is reported
// with its DECLARE's statement, and gives no keys).
func tracks(st Statement) bool {
	return st.Observe && st.Track && st.ReadOnly && !st.Watch && len(st.Cursors) == 0
}

// resultCols is what the server said of the result columns of one parsed
// statement of a tracked read, in a RowDescription answering a Describe of it
// or of a portal bound to it (spec 4). A Parse of such a statement allocates
// one (prepared.cols), which the portals bound to it share, so every later
// Execute of the statement finds it with no Describe of its own; a Parse of
// the name again allocates a new one. Only the server side fills and reads
// it; the client side passes the pointer along.
//
// The server plans a statement again, with no new RowDescription, when what
// its names mean changes (a SET of search_path, say), and may then read
// another table with the same columns. So the cell serves an Execute only
// when nothing that may change what the names mean came between the Describe
// and the Execute: gen is session.resolution when the Describe arrived, which
// must be the Execute's, and reverts is session.reverts when the answer was
// read, which must be the same when the Execute's rows are.
type resultCols struct {
	known   bool     // a RowDescription for the statement was read
	fields  int      // columns in each row
	keys    []keyCol // the columns that hold a tracked table's key, in column order
	tables  []uint32 // the tracked tables with a column in the result, in column order, each once
	gen     uint64
	reverts uint64
}

// keyCol is a result column that holds the key of a tracked table.
type keyCol struct {
	at     int // the column's index in a row
	table  uint32
	typ    KeyType
	format int16 // the format the RowDescription gives it: a Query's rows come in it
}

// cellAt is a Describe at pos of its unit whose answer fills cols; gen is
// session.resolution when it arrived.
type cellAt struct {
	pos  int
	cols *resultCols
	gen  uint64
}

// takeState is what the server side does with the rows of the answer in
// progress.
type takeState uint8

const (
	takeUndecided takeState = iota // which read they answer is not looked at yet
	takeNone                       // their bodies are not read: no keys, or all the read may take
	takeTaking                     // each DataRow's body is read for its keys
)

// keyTake is the server side's work on the answer in progress, since the
// last completion, error or ReadyForQuery: the read its rows answer, where
// their keys are, and the keys taken so far. It is decided under s.mu at
// most once per answer, at its RowDescription (a Query's result set) or its
// first row (an Execute), or at its completion when it sent none; the rows
// are then read without the lock. settle hands what it took to the read.
// Touched only on the server side, and by abandon once both sides have
// stopped, like session.sent.
type keyTake struct {
	state takeState
	// unit and read: the read (an index in unit.reads) the answer's rows
	// answer; unit is nil for none.
	unit   *unit
	read   int
	fields int         // columns in each row
	cols   []keyFormat // the key columns, with the format of their values
	room   int         // how many keys the read may still take
	keys   []RowKey    // taken from this answer's rows
	tables []uint32    // of a Query's result set: the tracked tables it has a column of
}

// keyFormat is a key column and the format its values are sent in.
type keyFormat struct {
	keyCol
	format int16
}

// rowDesc is what the body of the RowDescription being read is for, from
// its header (see describing): a Describe's answer fills cell, with gen; or
// a Query's result set starts, whose rows answer read of unit, which may
// take room more keys. Server side only.
type rowDesc struct {
	cell *resultCols
	gen  uint64
	unit *unit
	read int
	room int
}

// wanted reports whether the RowDescription's body is to be read.
func (d rowDesc) wanted() bool { return d.cell != nil || d.unit != nil }

// track gives b, the portal Bind m makes of p, what its Executes need to
// take keys from their rows: p's cell, the result format codes m sets, and
// the one value m binds to Statement.KeyParam, the only bind value an
// observed read keeps (spec 4), unless it is longer than any key taken. The
// caller makes sure the statement's name had no other meaning at the Bind
// and that both names are ASCII.
func (b *bound) track(m *pgproto3.Bind, p prepared) {
	if p.cols == nil {
		return
	}
	b.cols, b.formats = p.cols, slices.Clone(m.ResultFormatCodes)
	if n := p.st.KeyParam; n > 0 && n <= len(m.Parameters) && len(m.Parameters[n-1]) <= maxKeyLen {
		v := param(m, p.oids, n-1)
		b.key = &v
	}
}

// describe notes that the server's answer to the Describe at pos of u, of
// the statement or portal name (kind 'S' or 'P'), gives the result columns
// of a tracked read's statement, when it does: the name means one statement,
// whatever the server holds (names.get gives no other meaning), and that
// statement has a cell. A name that is not ASCII is left out: the server may
// know it by other bytes (see foreign). The answer is noted with the
// resolution now (see resultCols). It runs with s.mu held.
func (s *session) describe(u *unit, pos int, kind byte, name string) {
	if !ascii(name) {
		return
	}
	var cols *resultCols
	switch kind {
	case 'S':
		if p, ok, others := s.stmts.get(u, name); ok && len(others) == 0 {
			cols = p.cols
		}
	case 'P':
		if b, ok, others := s.portals.get(u, name); ok && len(others) == 0 {
			cols = b.cols
		}
	}
	if cols != nil {
		u.describes = append(u.describes, cellAt{pos: pos, cols: cols, gen: s.resolution})
	}
}

// rowWanted reports whether the body of the DataRow whose header was just
// read is to be read for keys. It runs on the server side for every row:
// while no unit holding a tracked read waits it costs one atomic load, and
// it takes s.mu at most once per answer, to decide which read the rows
// answer (decide).
func (s *session) rowWanted() bool {
	if s.take.state == takeUndecided {
		if s.tracking.Load() == 0 {
			return false
		}
		s.decide()
	}
	return s.take.state == takeTaking
}

// decide looks, at the first row of an answer, which read the rows answer.
func (s *session) decide() {
	s.mu.Lock()
	done := s.answeredUnsynced(nil)
	var head *unit
	if len(s.units) > 0 {
		head = s.units[0]
	}
	s.decideRows(head)
	s.mu.Unlock()
	s.finishAll(done)
}

// decideRows decides the take for the answer to head now in progress: the
// Execute after its last completion. When that Execute is a tracked read
// whose statement's columns a Describe the server answered made known, and
// nothing that may have changed what its names mean came between that
// Describe and the Execute (see resultCols), the read gets its Read (its
// Tables, and the KeyParam value its portal kept), and its rows are read for
// keys when it has a key column and the unit may take more (maxUnitKeys).
// Otherwise the rows give nothing, and an Execute's read keeps no Read. A
// Query's rows were decided at its RowDescription. It runs on the server
// side with s.mu held, at an answer's first row or, for one that sent none,
// at its completion.
func (s *session) decideRows(head *unit) {
	tk := &s.take
	tk.state = takeNone
	if head == nil || !head.tracked || head.query {
		return
	}
	i := head.trackedRead(head.completions + 1)
	if i < 0 {
		return
	}
	r := &head.reads[i]
	if c := r.cols; !c.known || c.gen != r.gen || c.reverts != s.reverts {
		return
	}
	r.keys = &ReadKeys{Tables: slices.Clone(r.cols.tables), Param: r.param}
	tk.unit, tk.read = head, i
	tk.start(*r.cols, min(MaxReadKeys, maxUnitKeys-head.keys), bindFormat(r.formats, r.cols.fields))
}

// bindFormat gives the format of a key column's values from the result
// format codes of a Bind: none for all text, one for all columns, or one per
// column. ok is false when codes give none for it.
func bindFormat(codes []int16, fields int) func(k keyCol) (format int16, ok bool) {
	return func(k keyCol) (int16, bool) {
		switch {
		case len(codes) == 0:
			return 0, true
		case len(codes) == 1:
			return codes[0], true
		case len(codes) == fields:
			return codes[k.at], true
		}
		return 0, false
	}
}

// start makes tk read the rows for the key columns of c, taking at most room
// keys; format gives the format of a key column's values, and a column whose
// format it does not give leaves no key.
func (tk *keyTake) start(c resultCols, room int, format func(k keyCol) (int16, bool)) {
	if room <= 0 {
		return
	}
	var cols []keyFormat
	for _, k := range c.keys {
		if f, ok := format(k); ok {
			cols = append(cols, keyFormat{keyCol: k, format: f})
		}
	}
	if len(cols) > 0 {
		tk.state, tk.fields, tk.cols, tk.room = takeTaking, c.fields, cols, room
	}
}

// describing notes, as the header of a RowDescription answering head is
// read, what its body is for (rowDesc). In an extended-protocol unit it
// answers the Describe at pos acks+1, whose cell it fills if describe noted
// one; the notes of the Describes it has passed are dropped, so each is
// looked at once. In a Query unit it starts a result set of the Query
// itself, whose rows answer the Query's read if it may take keys, and no read
// otherwise. It runs on the server side with s.mu held, before the
// RowDescription is counted.
func (s *session) describing(head *unit) {
	if !head.query {
		for len(head.describes) > 0 && head.describes[0].pos <= head.acks {
			head.describes = head.describes[1:]
		}
		if len(head.describes) > 0 && head.describes[0].pos == head.acks+1 {
			c := head.describes[0]
			s.desc = rowDesc{cell: c.cols, gen: c.gen}
		}
		return
	}
	s.take.state = takeNone
	if i := head.trackedRead(0); i >= 0 {
		room := min(MaxReadKeys-len(head.reads[i].keys.Keys), maxUnitKeys-head.keys)
		s.desc = rowDesc{unit: head, read: i, room: room}
	}
}

// rowDescription follows the body of a RowDescription the server side asked
// for (rowDesc): it fills a Describe's cell, noting the resolution the
// Describe arrived under and the reverts so far, or decides the take for a
// Query's result set, whose rows come in the formats it gives. It runs on the
// server side without s.mu.
func (s *session) rowDescription(body []byte) {
	d := s.desc
	s.desc = rowDesc{}
	c := s.columns(body)
	switch {
	case d.cell != nil:
		c.gen, c.reverts = d.gen, s.reverts
		*d.cell = c
	case d.unit != nil:
		tk := &s.take
		tk.unit, tk.read, tk.tables = d.unit, d.read, c.tables
		tk.start(c, d.room, func(k keyCol) (int16, bool) { return k.format, true })
	}
}

// columns reads the body of a RowDescription: which result columns hold the
// key of a table the gate tracks, matched by table OID and attribute number,
// so that aliases, joins and search_path do not matter, and a column of a
// view or a function names no tracked table. A body it cannot read gives
// columns that are not known. It runs on the server side without s.mu: it
// asks the gate (Tracked), once per table.
func (s *session) columns(body []byte) resultCols {
	if len(body) < 2 {
		return resultCols{}
	}
	n := int(int16(binary.BigEndian.Uint16(body)))
	if n < 0 {
		return resultCols{}
	}
	type asked struct {
		table uint32
		attr  uint16
		typ   KeyType
		ok    bool
	}
	var seen []asked
	c := resultCols{fields: n}
	off := 2
	for i := range n {
		end := bytes.IndexByte(body[off:], 0) // the column's name
		if end < 0 {
			return resultCols{}
		}
		off += end + 1
		if len(body)-off < 18 {
			return resultCols{}
		}
		table := binary.BigEndian.Uint32(body[off:])
		attr := binary.BigEndian.Uint16(body[off+4:])
		format := int16(binary.BigEndian.Uint16(body[off+16:]))
		off += 18 // table OID, attribute number, type OID, size, modifier, format
		if table == 0 {
			continue
		}
		j := slices.IndexFunc(seen, func(a asked) bool { return a.table == table })
		if j < 0 {
			a := asked{table: table}
			a.attr, a.typ, a.ok = s.gate.Tracked(table)
			seen = append(seen, a)
			j = len(seen) - 1
		}
		if a := seen[j]; a.ok {
			if !slices.Contains(c.tables, table) {
				c.tables = append(c.tables, table)
			}
			if attr == a.attr {
				c.keys = append(c.keys, keyCol{at: i, table: table, typ: a.typ, format: format})
			}
		}
	}
	if off != len(body) {
		return resultCols{}
	}
	c.known = true
	return c
}

// dataRow takes the keys of a DataRow whose body the server side asked for
// (rowWanted). A body past maxInspect (nil) is skipped, and so is one that
// does not hold the columns its RowDescription said: no key of it is taken.
// A NULL key, or one KeyOutput does not read, gives none. Once the read has
// all the keys it may take, later rows are not read. It runs on the server
// side without s.mu.
func (s *session) dataRow(body []byte) {
	tk := &s.take
	if tk.state != takeTaking || body == nil {
		return
	}
	if len(body) < 2 || int(int16(binary.BigEndian.Uint16(body))) != tk.fields {
		return
	}
	start, off, col := len(tk.keys), 2, 0
	for _, kc := range tk.cols {
		for ; col <= kc.at; col++ {
			if len(body)-off < 4 {
				tk.keys = tk.keys[:start]
				return
			}
			n := int32(binary.BigEndian.Uint32(body[off:]))
			off += 4
			var v []byte // nil is NULL
			switch {
			case n == -1:
			case n < 0 || int64(n) > int64(len(body)-off):
				tk.keys = tk.keys[:start]
				return
			default:
				v = body[off : off+int(n)]
				off += int(n)
			}
			if col == kc.at && len(tk.keys) < tk.room {
				if text, ok := KeyOutput(kc.typ, kc.format, v); ok {
					tk.keys = append(tk.keys, RowKey{Table: kc.table, Text: text})
				}
			}
		}
	}
	if len(tk.keys) >= tk.room {
		tk.state = takeNone
	}
}

// settleKeys hands what the server side took from the answer that just ended
// to the read it answers, in head, counts it toward head's keys, and starts
// the next answer undecided. It runs with s.mu held, from settle.
func (s *session) settleKeys(head *unit) {
	tk := s.take
	s.take = keyTake{}
	if tk.unit == nil || tk.unit != head {
		return
	}
	head.keys += len(tk.keys)
	r := head.reads[tk.read].keys
	r.Keys = append(r.Keys, tk.keys...)
	for _, t := range tk.tables {
		if !slices.Contains(r.Tables, t) {
			r.Tables = append(r.Tables, t)
		}
	}
}

// trackedRead returns the index in u.reads of the read that may take keys
// arriving as Execute idx of u (0: the Query), or -1.
func (u *unit) trackedRead(idx int) int {
	return slices.IndexFunc(u.reads, func(r observedRead) bool { return r.track && r.idx == idx })
}

package proxy_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/stetdb/stet/internal/proxy"
)

// keyTable is a table in stet_test whose reads a test gate tracks.
type keyTable struct {
	name string
	oid  uint32
	attr uint16 // the key column's attribute number
	typ  proxy.KeyType
}

// newKeyTable creates a table with columns (as CREATE TABLE lists them) and a
// name no other test uses, fills it with rows (SQL with %[1]s for the table's
// name; "" for none), and drops it when the test ends. key names the column
// the gate reads keys from, as typ.
func newKeyTable(t *testing.T, columns, key string, typ proxy.KeyType, rows string) keyTable {
	t.Helper()
	name := fmt.Sprintf("keys_%d", time.Now().UnixNano())
	admin := connect(t, upstream(t), "stet_scram", nil)
	mustExec(t, admin, "create table "+name+" ("+columns+")")
	t.Cleanup(func() { admin.Exec(context.Background(), "drop table "+name) })
	if rows != "" {
		mustExec(t, admin, fmt.Sprintf(rows, name))
	}
	var oid int64
	var attr int32
	err := admin.QueryRow(testCtx(t), "select a.attrelid::int8, a.attnum::int4 from pg_attribute a where a.attrelid = $1::regclass and a.attname = $2",
		name, key).Scan(&oid, &attr)
	if err != nil {
		t.Fatal(err)
	}
	return keyTable{name: name, oid: uint32(oid), attr: uint16(attr), typ: typ}
}

// idTable is a keyTable (id int primary key, title text) holding ids 1 to n,
// each titled "t<id>".
func idTable(t *testing.T, n int) keyTable {
	t.Helper()
	return newKeyTable(t, "id int primary key, title text", "id", proxy.KeyInt4,
		fmt.Sprintf("insert into %%[1]s select g, 't' || g from generate_series(1, %d) g", n))
}

// readOnlyRE matches the statements the test gate marks ReadOnly, as the
// classifier would here: SELECT, and the clean BEGIN, SET and SHOW.
var readOnlyRE = regexp.MustCompile(`(?i)^\s*(select|begin|set|show)\b`)

// newKeyGate observes the reads that name one of tables, marks ReadOnly what
// readOnlyRE matches, and so tracks each observed read among those; Tracked
// answers each table's key column. It watches no write.
func newKeyGate(tables ...keyTable) *recordingGate {
	g := newRecordingGate(func(string) bool { return false })
	g.observeMatch = func(sql string) bool {
		return !isWrite(sql) && slices.ContainsFunc(tables, func(tb keyTable) bool { return strings.Contains(sql, tb.name) })
	}
	g.readOnly = readOnlyRE.MatchString
	g.tracked = map[uint32]trackedKey{}
	for _, tb := range tables {
		g.tracked[tb.oid] = trackedKey{attr: tb.attr, typ: tb.typ}
	}
	return g
}

// rawConn connects to addr as stet_scram and returns the connection's
// frontend, for messages pgx sends only in its own order.
func rawConn(t *testing.T, addr string) *pgproto3.Frontend {
	t.Helper()
	conn := connect(t, addr, "stet_scram", nil)
	conn.PgConn().Conn().SetDeadline(time.Now().Add(10 * time.Second))
	return conn.PgConn().Frontend()
}

// exchange sends msgs in one write and reads the answers up to the
// ReadyForQuery that ends them, failing on an error.
func exchange(t *testing.T, fe *pgproto3.Frontend, msgs ...pgproto3.FrontendMessage) {
	t.Helper()
	sendAll(t, fe, msgs...)
	untilReady(t, fe)
}

// keysOf lists the keys r took as table:text, or nil for no Read.
func keysOf(r *proxy.ReadKeys) []string {
	if r == nil {
		return nil
	}
	got := []string{}
	for _, k := range r.Keys {
		got = append(got, fmt.Sprintf("%d:%s", k.Table, k.Text))
	}
	return got
}

// of names the keys texts of tb as keysOf lists them.
func (tb keyTable) of(texts ...string) []string {
	keys := []string{}
	for _, text := range texts {
		keys = append(keys, fmt.Sprintf("%d:%s", tb.oid, text))
	}
	return keys
}

// wantRead fails the test unless the read o took keys (a Read), exactly
// keys (as keysOf lists them), and saw tables.
func wantRead(t *testing.T, what string, o proxy.Outcome, tables []uint32, keys []string) {
	t.Helper()
	if o.Read == nil {
		t.Errorf("%s: no Read, want keys %q of tables %v", what, keys, tables)
		return
	}
	if got := keysOf(o.Read); !slices.Equal(got, keys) {
		t.Errorf("%s: keys %q, want %q", what, got, keys)
	}
	if !slices.Equal(o.Read.Tables, tables) {
		t.Errorf("%s: tables %v, want %v", what, o.Read.Tables, tables)
	}
}

// wantKeys is wantRead for a read of tb alone.
func wantKeys(t *testing.T, what string, o proxy.Outcome, tb keyTable, texts ...string) {
	t.Helper()
	wantRead(t, what, o, []uint32{tb.oid}, tb.of(texts...))
}

// wantNoRead fails the test unless the read o, which sent rows, took nothing.
func wantNoRead(t *testing.T, what string, o proxy.Outcome) {
	t.Helper()
	if o.Read != nil {
		t.Errorf("%s: Read %+v, want none", what, *o.Read)
	}
	if o.Sent == 0 || !o.Executed {
		t.Errorf("%s: %+v, want it executed with rows sent", what, o)
	}
}

// Spec 4: a tracked read sent as a Query gives the key of each row it sends,
// from the column the RowDescription names by table OID and attribute
// number, on an enforcing proxy as on a shadow one.
func TestKeysFromSimpleQuery(t *testing.T) {
	tb := idTable(t, 5)
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			g := newKeyGate(tb)
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			conn := connect(t, addr, "stet_scram", nil)

			mustExec(t, conn, "select id, title from "+tb.name+" where id between 2 and 4 order by id")
			o := g.observed(t, 1)[0].o
			if o.Sent != 3 || !o.Executed {
				t.Errorf("read: %+v, want 3 rows sent and executed", o)
			}
			wantKeys(t, "select", o, tb, "2", "3", "4")
		})
	}
}

// In the extended protocol the columns come from a Describe the server
// answered, of the portal or of the statement under an earlier Sync; the
// values come in the formats the Bind asked for, and a binary key reads as
// the same text as its text form, for each of the eight key types.
func TestKeysFromExtendedQuery(t *testing.T) {
	tb := idTable(t, 5)
	sql := "select title, id from " + tb.name + " where id <= 2 order by id"
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			g := newKeyGate(tb)
			addr, _ := startProxyWith(t, upstream(t), g, mode.policy)
			fe := rawConn(t, addr)

			exchange(t, fe, &pgproto3.Parse{Query: sql}, &pgproto3.Bind{}, &pgproto3.Describe{ObjectType: 'P'},
				&pgproto3.Execute{}, &pgproto3.Sync{})
			exchange(t, fe, &pgproto3.Parse{Name: "s", Query: sql}, &pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Sync{})
			exchange(t, fe, &pgproto3.Bind{PreparedStatement: "s", ResultFormatCodes: []int16{0, 1}}, &pgproto3.Execute{}, &pgproto3.Sync{})
			obs := g.observed(t, 2)
			wantKeys(t, "Describe of the portal", obs[0].o, tb, "1", "2")
			wantKeys(t, "Describe of the statement", obs[1].o, tb, "1", "2")
		})
	}

	t.Run("binary", func(t *testing.T) {
		var tables []keyTable
		for _, c := range []struct {
			typ    string
			kt     proxy.KeyType
			values string
		}{
			{"int2", proxy.KeyInt2, "(-3), (7)"},
			{"int4", proxy.KeyInt4, "(-100000), (42)"},
			{"int8", proxy.KeyInt8, "(-9000000000), (5)"},
			{"uuid", proxy.KeyUUID, "('A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11'), ('00000000-0000-0000-0000-000000000001')"},
			{"text", proxy.KeyText, "('ab'), ('a b,c')"},
			{"varchar(10)", proxy.KeyText, "('cd'), ('')"},
			{"char(4)", proxy.KeyText, "('ef'), ('g')"},
			{"name", proxy.KeyText, "('gh'), ('i')"},
		} {
			tables = append(tables, newKeyTable(t, "k "+c.typ+" primary key", "k", c.kt, "insert into %[1]s values "+c.values))
		}
		g := newKeyGate(tables...)
		addr, _ := startProxy(t, upstream(t), g)
		conn := connect(t, addr, "stet_scram", nil)
		ctx := testCtx(t)
		for i, tb := range tables {
			sql := "select k from " + tb.name + " order by k"
			mustExec(t, conn, sql) // text
			if _, err := conn.PgConn().ExecParams(ctx, sql, nil, nil, nil, []int16{1}).Close(); err != nil {
				t.Fatal(err)
			}
			obs := g.observed(t, 2*i+2)
			text, bin := obs[2*i].o, obs[2*i+1].o
			if text.Read == nil || len(text.Read.Keys) != 2 {
				t.Errorf("%s in text: Read %+v, want two keys", tb.name, text.Read)
				continue
			}
			wantRead(t, tb.name+" in binary", bin, []uint32{tb.oid}, keysOf(text.Read))
		}
	})
}

// A named statement described once gives its columns to every later Execute
// of it, through a new portal each time, with no Describe.
func TestKeysFromNamedStatementRunAgain(t *testing.T) {
	tb := idTable(t, 5)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawConn(t, addr)

	exchange(t, fe, &pgproto3.Parse{Name: "s", Query: "select id from " + tb.name + " where id <= $1 order by id"},
		&pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Sync{})
	for n := 1; n <= 3; n++ {
		exchange(t, fe, &pgproto3.Bind{PreparedStatement: "s", Parameters: [][]byte{[]byte(strconv.Itoa(n))}},
			&pgproto3.Execute{}, &pgproto3.Sync{})
	}
	obs := g.observed(t, 3)
	wantKeys(t, "first run", obs[0].o, tb, "1")
	wantKeys(t, "second run", obs[1].o, tb, "1", "2")
	wantKeys(t, "third run", obs[2].o, tb, "1", "2", "3")
}

// A statement never described has columns the proxy does not know: its
// Execute takes no keys.
func TestNoKeysWithoutDescribe(t *testing.T) {
	tb := idTable(t, 5)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawConn(t, addr)
	sql := "select id from " + tb.name + " order by id"

	exchange(t, fe, &pgproto3.Parse{Name: "s", Query: sql}, &pgproto3.Sync{})
	exchange(t, fe, &pgproto3.Bind{PreparedStatement: "s"}, &pgproto3.Execute{}, &pgproto3.Sync{})
	exchange(t, fe, &pgproto3.Parse{Query: sql}, &pgproto3.Bind{}, &pgproto3.Execute{}, &pgproto3.Sync{})
	// Described only after it ran: the answer it took its columns from came
	// after its rows.
	exchange(t, fe, &pgproto3.Parse{Query: sql}, &pgproto3.Bind{}, &pgproto3.Execute{},
		&pgproto3.Describe{ObjectType: 'S'}, &pgproto3.Sync{})
	obs := g.observed(t, 3)
	wantNoRead(t, "named statement", obs[0].o)
	wantNoRead(t, "unnamed statement", obs[1].o)
	wantNoRead(t, "statement described after its Execute", obs[2].o)
}

// A name parsed again is a new statement: what a Describe said of the old
// one is not used for it, until it is described in turn.
func TestKeysAfterNameParsedAgain(t *testing.T) {
	tb := idTable(t, 5)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawConn(t, addr)
	first := "select id, title from " + tb.name + " where id = 1"
	second := "select title, id from " + tb.name + " where id = 2" // the key moved to the second column
	run := func(name string) {
		t.Helper()
		exchange(t, fe, &pgproto3.Bind{PreparedStatement: name}, &pgproto3.Execute{}, &pgproto3.Sync{})
	}

	exchange(t, fe, &pgproto3.Parse{Name: "s", Query: first}, &pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Sync{})
	run("s")
	exchange(t, fe, &pgproto3.Close{ObjectType: 'S', Name: "s"}, &pgproto3.Parse{Name: "s", Query: second}, &pgproto3.Sync{})
	run("s")
	exchange(t, fe, &pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Sync{})
	run("s")

	// The unnamed statement, parsed again with no Close.
	exchange(t, fe, &pgproto3.Parse{Query: first}, &pgproto3.Describe{ObjectType: 'S'}, &pgproto3.Sync{})
	run("")
	exchange(t, fe, &pgproto3.Parse{Query: second}, &pgproto3.Bind{}, &pgproto3.Execute{}, &pgproto3.Sync{})
	// Parsed again under the same Sync after its Describe, as another
	// statement and as the same SQL, which the statement table keeps as one
	// change: the server reads that SQL anew, so another session's DDL may
	// have changed its columns since the Describe.
	exchange(t, fe, &pgproto3.Parse{Query: first}, &pgproto3.Describe{ObjectType: 'S'}, &pgproto3.Parse{Query: second},
		&pgproto3.Bind{}, &pgproto3.Execute{}, &pgproto3.Sync{})
	exchange(t, fe, &pgproto3.Parse{Query: first}, &pgproto3.Describe{ObjectType: 'S'}, &pgproto3.Parse{Query: first},
		&pgproto3.Bind{}, &pgproto3.Execute{}, &pgproto3.Sync{})
	// A portal bound before the Parse again still runs the statement described.
	exchange(t, fe, &pgproto3.Parse{Query: first}, &pgproto3.Describe{ObjectType: 'S'}, &pgproto3.Bind{DestinationPortal: "p"},
		&pgproto3.Parse{Query: first}, &pgproto3.Execute{Portal: "p"}, &pgproto3.Sync{})

	obs := g.observed(t, 8)
	wantKeys(t, "described", obs[0].o, tb, "1")
	wantNoRead(t, "parsed again", obs[1].o)
	wantKeys(t, "described again", obs[2].o, tb, "2")
	wantKeys(t, "unnamed, described", obs[3].o, tb, "1")
	wantNoRead(t, "unnamed, parsed again", obs[4].o)
	wantNoRead(t, "unnamed, parsed again under one Sync", obs[5].o)
	wantNoRead(t, "unnamed, the same SQL parsed again under one Sync", obs[6].o)
	wantKeys(t, "portal bound before the Parse again", obs[7].o, tb, "1")
}

// Columns are matched by table OID, so an alias, a join of two tracked
// tables and a self-join all give the key of every key column, and each
// table is listed once.
func TestKeysFromJoinAndAlias(t *testing.T) {
	tb := idTable(t, 3)
	u := newKeyTable(t, "id int primary key, tid int", "id", proxy.KeyInt4, "insert into %[1]s values (10, 1), (20, 2)")
	g := newKeyGate(tb, u)
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	mustExec(t, conn, "select c.id as cid, d.id from "+tb.name+" c join "+u.name+" d on d.tid = c.id order by c.id")
	mustExec(t, conn, "select a.id, b.id as next from "+tb.name+" a join "+tb.name+" b on b.id = a.id + 1 order by a.id")
	obs := g.observed(t, 2)
	wantRead(t, "join", obs[0].o, []uint32{tb.oid, u.oid},
		[]string{tb.of("1")[0], u.of("10")[0], tb.of("2")[0], u.of("20")[0]})
	wantKeys(t, "self-join", obs[1].o, tb, "1", "2", "2", "3")
}

// A view's columns name the view, not the table under it: a read through it
// takes no key and sees no tracked table.
func TestNoKeysThroughView(t *testing.T) {
	tb := idTable(t, 3)
	view := tb.name + "_v"
	admin := connect(t, upstream(t), "stet_scram", nil)
	mustExec(t, admin, "create view "+view+" as select * from "+tb.name)
	t.Cleanup(func() { admin.Exec(context.Background(), "drop view "+view) })
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	mustExec(t, conn, "select id, title from "+view+" order by id")
	o := g.observed(t, 1)[0].o
	if o.Sent != 3 {
		t.Errorf("read through the view: %+v, want 3 rows sent", o)
	}
	wantRead(t, "read through the view", o, nil, nil)
}

// A key column that is NULL in a row (an outer join that found no row) gives
// no key for that row; the other rows still give theirs.
func TestNullKeyIsSkipped(t *testing.T) {
	tb := newKeyTable(t, "id int primary key, title text", "id", proxy.KeyInt4, "insert into %[1]s values (1, 'a'), (3, 'c')")
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	mustExec(t, conn, "select t.id from generate_series(1, 3) g left join "+tb.name+" t on t.id = g order by g")
	o := g.observed(t, 1)[0].o
	if o.Sent != 3 {
		t.Errorf("read: %+v, want 3 rows sent", o)
	}
	wantKeys(t, "outer join", o, tb, "1", "3")
}

// Spec 4: one read gives at most MaxReadKeys keys, those of its first rows;
// its rows are all counted.
func TestLargeReadKeepsThousandKeys(t *testing.T) {
	tb := idTable(t, 1500)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	mustExec(t, conn, "select id from "+tb.name+" order by id")
	o := g.observed(t, 1)[0].o
	if o.Sent != 1500 {
		t.Errorf("read: %d rows sent, want 1500", o.Sent)
	}
	var want []string
	for i := 1; i <= proxy.MaxReadKeys; i++ {
		want = append(want, strconv.Itoa(i))
	}
	wantKeys(t, "1500 rows", o, tb, want...)
}

// Spec correction S2: a text key holding a byte that is not ASCII, or longer
// than 1024 bytes, gives no key, in text and in binary; an empty key, one of
// exactly 1024 bytes, and keys with commas and quotes are taken as they are.
func TestOddKeyValues(t *testing.T) {
	tb := newKeyTable(t, "k text primary key, n int", "k", proxy.KeyText,
		`insert into %[1]s values ('é', 1), ('', 2), ('a,b', 3), ('a"b', 4), (repeat('x', 1025), 5), (repeat('y', 1024), 6), ('it''s', 7)`)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	sql := "select k from " + tb.name + " order by n"
	mustExec(t, conn, sql)
	if _, err := conn.PgConn().ExecParams(testCtx(t), sql, nil, nil, nil, []int16{1}).Close(); err != nil {
		t.Fatal(err)
	}
	obs := g.observed(t, 2)
	for i, format := range []string{"text", "binary"} {
		if o := obs[i].o; o.Sent != 7 {
			t.Errorf("%s: %d rows sent, want 7", format, o.Sent)
		}
		wantKeys(t, format, obs[i].o, tb, "", "a,b", `a"b`, strings.Repeat("y", 1024), "it's")
	}
}

// Spec 4 and correction S2: how each key type's value reads as text, in
// text and in binary format.
func TestKeyOutput(t *testing.T) {
	be := func(n any) []byte {
		var b bytes.Buffer
		binary.Write(&b, binary.BigEndian, n)
		return b.Bytes()
	}
	id := []byte{0xa0, 0xee, 0xbc, 0x99, 0x9c, 0x0b, 0x4e, 0xf8, 0xbb, 0x6d, 0x6b, 0xb9, 0xbd, 0x38, 0x0a, 0x11}
	const idText = "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	for _, c := range []struct {
		typ    proxy.KeyType
		format int16
		v      []byte
		want   string // "" with ok false
		ok     bool
	}{
		{proxy.KeyInt2, 0, []byte("-3"), "-3", true},
		{proxy.KeyInt2, 1, be(int16(-3)), "-3", true},
		{proxy.KeyInt2, 1, be(int16(math.MaxInt16)), "32767", true},
		{proxy.KeyInt2, 1, be(int32(7)), "", false},
		{proxy.KeyInt4, 0, []byte("42"), "42", true},
		{proxy.KeyInt4, 1, be(int32(-100000)), "-100000", true},
		{proxy.KeyInt4, 1, be(int32(math.MinInt32)), "-2147483648", true},
		{proxy.KeyInt4, 1, be(int16(1)), "", false},
		{proxy.KeyInt4, 1, be(int64(1)), "", false},
		{proxy.KeyInt8, 0, []byte("-9000000000"), "-9000000000", true},
		{proxy.KeyInt8, 1, be(int64(-9000000000)), "-9000000000", true},
		{proxy.KeyInt8, 1, be(int64(math.MaxInt64)), "9223372036854775807", true},
		{proxy.KeyInt8, 1, be(int32(5)), "", false},
		{proxy.KeyUUID, 0, []byte(idText), idText, true},
		{proxy.KeyUUID, 1, id, idText, true},
		{proxy.KeyUUID, 1, id[:15], "", false},
		{proxy.KeyText, 0, []byte("a,b"), "a,b", true},
		{proxy.KeyText, 1, []byte(`a"b`), `a"b`, true},
		{proxy.KeyText, 0, []byte{}, "", true},
		{proxy.KeyText, 1, []byte{}, "", true},
		{proxy.KeyText, 0, []byte("é"), "", false},
		{proxy.KeyText, 1, []byte("é"), "", false},
		{proxy.KeyText, 0, bytes.Repeat([]byte("y"), 1024), strings.Repeat("y", 1024), true},
		{proxy.KeyText, 1, bytes.Repeat([]byte("x"), 1025), "", false},
		{proxy.KeyInt4, 0, bytes.Repeat([]byte("1"), 1025), "", false},
		{proxy.KeyInt4, 0, nil, "", false}, // NULL
		{proxy.KeyInt8, 1, nil, "", false},
		{proxy.KeyUUID, 1, nil, "", false},
		{proxy.KeyText, 0, nil, "", false},
		{proxy.KeyText, 2, []byte("a"), "", false}, // no such format
		{0, 0, []byte("a"), "", false},             // no such type
	} {
		got, ok := proxy.KeyOutput(c.typ, c.format, c.v)
		if got != c.want || ok != c.ok {
			t.Errorf("KeyOutput(%d, %d, %q) = %q, %v; want %q, %v", c.typ, c.format, c.v, got, ok, c.want, c.ok)
		}
	}
}

// Spec 4: a read after a message that is not read-only, since the server
// last reported the session idle, takes no keys: in the same transaction, in
// a unit not answered yet, or earlier under the same Sync. Reads after
// read-only messages alone, and after the transaction ended, do.
func TestNoKeysAfterAWrite(t *testing.T) {
	tb := idTable(t, 3)
	other := idTable(t, 3) // not tracked
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	read := "select id from " + tb.name + " where id = 1"
	update := "update " + other.name + " set title = 'x' where id = 1"

	mustExec(t, conn, "begin")
	mustExec(t, conn, update)
	mustExec(t, conn, read) // 0
	mustExec(t, conn, "commit")
	mustExec(t, conn, read) // 1
	mustExec(t, conn, "begin")
	mustExec(t, conn, read) // 2
	mustExec(t, conn, read) // 3
	mustExec(t, conn, "commit")

	fe := rawConn(t, addr)
	sendAll(t, fe, &pgproto3.Query{String: update}, &pgproto3.Query{String: read}) // 4
	untilReady(t, fe)
	untilReady(t, fe)
	exchange(t, fe, &pgproto3.Parse{Name: "r", Query: read}, &pgproto3.Describe{ObjectType: 'S', Name: "r"}, &pgproto3.Sync{})
	exchange(t, fe, &pgproto3.Bind{PreparedStatement: "r"}, &pgproto3.Execute{}, &pgproto3.Sync{}) // 5
	exchange(t, fe, &pgproto3.Parse{Name: "w", Query: update}, &pgproto3.Bind{PreparedStatement: "w"}, &pgproto3.Execute{},
		&pgproto3.Bind{PreparedStatement: "r"}, &pgproto3.Execute{}, &pgproto3.Sync{}) // 6
	// A read-only statement the gate does not observe, run through a portal,
	// leaves the transaction's reads comparable.
	exchange(t, fe, &pgproto3.Query{String: "begin"})
	exchange(t, fe, &pgproto3.Parse{Query: "select 1"}, &pgproto3.Bind{}, &pgproto3.Execute{}, &pgproto3.Sync{})
	exchange(t, fe, &pgproto3.Bind{PreparedStatement: "r"}, &pgproto3.Execute{}, &pgproto3.Sync{}) // 7
	exchange(t, fe, &pgproto3.Query{String: "commit"})

	obs := g.observed(t, 8)
	wantNoRead(t, "read after an update in the transaction", obs[0].o)
	wantKeys(t, "read after commit", obs[1].o, tb, "1")
	wantKeys(t, "first read in a transaction", obs[2].o, tb, "1")
	wantKeys(t, "second read in a transaction", obs[3].o, tb, "1")
	wantNoRead(t, "read pipelined behind an update", obs[4].o)
	wantKeys(t, "Execute of the read", obs[5].o, tb, "1")
	wantNoRead(t, "Execute of the read behind an update under one Sync", obs[6].o)
	wantKeys(t, "Execute of the read behind an unobserved select", obs[7].o, tb, "1")
}

// Spec 4: a Query of several statements gives the keys of every result set,
// each read from its own RowDescription.
func TestKeysFromSeveralStatements(t *testing.T) {
	tb := idTable(t, 3)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	mustExec(t, conn, "select id from "+tb.name+" where id = 1; select id, title from "+tb.name+" where id = 2")
	o := g.observed(t, 1)[0].o
	if o.Sent != 2 {
		t.Errorf("read: %d rows sent, want 2", o.Sent)
	}
	wantKeys(t, "two statements", o, tb, "1", "2")
}

// Rows read through a SQL cursor are reported with the DECLARE's statement
// and give no keys, even from a gate that marks the cursor's statements
// tracked; a plain read in the same transaction still does.
func TestNoKeysFromCursor(t *testing.T) {
	tb := idTable(t, 3)
	g := newKeyGate(tb)
	g.readOnly = func(string) bool { return true } // the cursor statements too: only the cursor rule holds keys back
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	mustExec(t, conn, "begin")
	mustExec(t, conn, "declare c cursor for select id from "+tb.name+" order by id")
	mustExec(t, conn, "fetch 2 from c")
	mustExec(t, conn, "select id from "+tb.name+" where id = 3")
	mustExec(t, conn, "commit")
	obs := g.observed(t, 2)
	if st := obs[0].st; !st.Track || len(st.Cursors) == 0 {
		t.Fatalf("FETCH reported with %+v, want the DECLARE marked Track", st)
	}
	wantNoRead(t, "FETCH", obs[0].o)
	wantKeys(t, "read after the FETCH", obs[1].o, tb, "3")
}

// Spec correction S5: a read that selects its row by `key = $n` keeps that
// one bind value for the gate, with Tables naming each tracked table the
// result has a column of; count(*) has none.
func TestKeyParam(t *testing.T) {
	tb := idTable(t, 3)
	g := newKeyGate(tb)
	g.keyParam = func(sql string) int {
		if strings.Contains(sql, "id = $1") {
			return 1
		}
		return 0
	}
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	pc, ctx := conn.PgConn(), testCtx(t)
	run := func(sql string, value []byte, format int16) {
		t.Helper()
		if _, err := pc.ExecParams(ctx, sql, [][]byte{value}, []uint32{23}, []int16{format}, nil).Close(); err != nil {
			t.Fatal(err)
		}
	}

	run("select title from "+tb.name+" where id = $1", []byte("2"), 0)
	run("select title from "+tb.name+" where id = $1", binary.BigEndian.AppendUint32(nil, 3), 1)
	run("select count(*) from "+tb.name+" where id = $1", []byte("2"), 0)
	obs := g.observed(t, 3)
	for i, want := range []proxy.Param{{Value: []byte("2"), Format: 0, OID: 23}, {Value: []byte{0, 0, 0, 3}, Format: 1, OID: 23}} {
		o := obs[i].o
		wantRead(t, "select title", o, []uint32{tb.oid}, []string{})
		if o.Read == nil || o.Read.Param == nil {
			t.Errorf("read %d: no Param, want %+v", i, want)
			continue
		}
		if p := *o.Read.Param; !bytes.Equal(p.Value, want.Value) || p.Format != want.Format || p.OID != want.OID {
			t.Errorf("read %d: Param %+v, want %+v", i, p, want)
		}
	}
	wantRead(t, "select count(*)", obs[2].o, nil, []string{})
}

// A portal read in parts (Executes with a row limit) reports, with each
// Execute, the keys of the rows that Execute sent.
func TestKeysOfPortalInParts(t *testing.T) {
	tb := idTable(t, 6)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawConn(t, addr)

	sendAll(t, fe, &pgproto3.Parse{Query: "select id from " + tb.name + " order by id"}, &pgproto3.Bind{},
		&pgproto3.Describe{ObjectType: 'P'}, &pgproto3.Execute{MaxRows: 2}, &pgproto3.Execute{MaxRows: 2},
		&pgproto3.Execute{MaxRows: 2}, &pgproto3.Sync{})
	if rows, suspended := untilReady(t, fe); rows != 6 || suspended != 3 {
		t.Fatalf("client got %d rows and %d PortalSuspended, want 6 and 3", rows, suspended)
	}
	obs := g.observed(t, 3)
	for i, want := range [][]string{{"1", "2"}, {"3", "4"}, {"5", "6"}} {
		wantKeys(t, fmt.Sprintf("Execute %d", i+1), obs[i].o, tb, want...)
	}
}

// Spec 4: a DataRow larger than the proxy inspects is skipped; the rows
// around it give their keys.
func TestOversizedRowIsSkipped(t *testing.T) {
	t.Cleanup(proxy.SetMaxInspect(256)) // before the proxy, so it is restored once the proxy closed
	tb := newKeyTable(t, "id int primary key, title text", "id", proxy.KeyInt4,
		"insert into %[1]s values (1, 'a'), (2, repeat('x', 1000)), (3, 'c')")
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	mustExec(t, conn, "select id, title from "+tb.name+" order by id")
	o := g.observed(t, 1)[0].o
	if o.Sent != 3 {
		t.Errorf("read: %d rows sent, want 3", o.Sent)
	}
	wantKeys(t, "rows around a large one", o, tb, "1", "3")
}

// Spec 14: unless a tracked read waits for its answer, the server side's tap
// is not shown a RowDescription or DataRow body, so reads that are not
// tracked cost what they did.
func TestRowsNotShownWithoutTrackedRead(t *testing.T) {
	const oid = 4242
	desc := &pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("id"), TableOID: oid,
		TableAttributeNumber: 1, DataTypeOID: 23, DataTypeSize: 4, TypeModifier: -1}}}
	row := &pgproto3.DataRow{Values: [][]byte{[]byte("1")}}
	done := &pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}
	ready := &pgproto3.ReadyForQuery{TxStatus: 'I'}
	sql := "select id from t"
	cases := []struct {
		name    string
		sent    []pgproto3.FrontendMessage
		answers []pgproto3.BackendMessage
		at      []int // the RowDescription and the DataRow among answers
	}{
		{"Query", []pgproto3.FrontendMessage{&pgproto3.Query{String: sql}},
			[]pgproto3.BackendMessage{desc, row, done, ready}, []int{0, 1}},
		{"Execute", []pgproto3.FrontendMessage{&pgproto3.Parse{Query: sql}, &pgproto3.Bind{},
			&pgproto3.Describe{ObjectType: 'P'}, &pgproto3.Execute{}, &pgproto3.Sync{}},
			[]pgproto3.BackendMessage{&pgproto3.ParseComplete{}, &pgproto3.BindComplete{}, desc, row, done, ready}, []int{2, 3}},
	}
	tracking := newObservingGate(func(string) bool { return true })
	tracking.readOnly = func(string) bool { return true }
	tracking.tracked = map[uint32]trackedKey{oid: {attr: 1, typ: proxy.KeyInt4}}
	for _, c := range cases {
		for _, g := range []struct {
			name string
			gate *recordingGate
			want bool
		}{
			{"observed, not tracked", newObservingGate(func(string) bool { return true }), false},
			{"not observed", newRecordingGate(isWrite), false},
			{"tracked", tracking, true},
		} {
			shown := proxy.ServerShows(g.gate, c.sent, c.answers)
			for _, i := range c.at {
				if shown[i] != g.want {
					t.Errorf("%s, %s: answer %d shown %v, want %v", c.name, g.name, i, shown[i], g.want)
				}
			}
		}
	}
}

// An Execute of a portal whose name may still mean an older portal, because
// the newer Bind is in a unit the server has not answered, may run the older
// one, whose statement's columns are elsewhere: it takes no keys. The
// server's answers here are made up as the proxy would see them were that
// Bind to fail and the older portal to run.
func TestNoKeysWhenThePortalMayMeanAnother(t *testing.T) {
	const oid = 4242
	field := func(name string, attr uint16, typ uint32) pgproto3.FieldDescription {
		return pgproto3.FieldDescription{Name: []byte(name), TableOID: oid, TableAttributeNumber: attr, DataTypeOID: typ, DataTypeSize: -1, TypeModifier: -1}
	}
	titleID := &pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{field("title", 2, 25), field("id", 1, 23)}}
	idTitle := &pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{field("id", 1, 23), field("title", 2, 25)}}
	row := &pgproto3.DataRow{Values: [][]byte{[]byte("t1"), []byte("1")}} // of titleID
	ready := &pgproto3.ReadyForQuery{TxStatus: 'T'}
	g := newObservingGate(func(sql string) bool { return strings.HasPrefix(sql, "select") })
	g.readOnly = func(string) bool { return true }
	g.tracked = map[uint32]trackedKey{oid: {attr: 1, typ: proxy.KeyInt4}}

	prepare := []pgproto3.FrontendMessage{&pgproto3.Query{String: "begin"},
		&pgproto3.Parse{Name: "a", Query: "select title, id from t"}, &pgproto3.Describe{ObjectType: 'S', Name: "a"}, &pgproto3.Sync{},
		&pgproto3.Parse{Name: "b", Query: "select id, title from t"}, &pgproto3.Describe{ObjectType: 'S', Name: "b"}, &pgproto3.Sync{},
		&pgproto3.Bind{PreparedStatement: "a"}, &pgproto3.Sync{}}
	prepared := []pgproto3.BackendMessage{&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")}, ready,
		&pgproto3.ParseComplete{}, &pgproto3.ParameterDescription{}, titleID, ready,
		&pgproto3.ParseComplete{}, &pgproto3.ParameterDescription{}, idTitle, ready,
		&pgproto3.BindComplete{}, ready}
	execute := []pgproto3.BackendMessage{row, &pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}, ready}

	// The portal bound to a, run: its keys.
	proxy.ServerShows(g, append(slices.Clone(prepare), &pgproto3.Execute{}, &pgproto3.Sync{}), slices.Concat(prepared, execute))
	// Bound again to b in a unit not answered when the Execute arrives.
	proxy.ServerShows(g, append(slices.Clone(prepare), &pgproto3.Bind{PreparedStatement: "b"}, &pgproto3.Sync{},
		&pgproto3.Execute{}, &pgproto3.Sync{}),
		slices.Concat(prepared, []pgproto3.BackendMessage{&pgproto3.ErrorResponse{Severity: "ERROR", Code: "08P01", Message: "made up"}, ready}, execute))
	obs := g.observed(t, 2)
	wantRead(t, "portal bound once", obs[0].o, []uint32{oid}, []string{fmt.Sprintf("%d:1", oid)})
	if obs[1].o.Read != nil {
		t.Errorf("portal that may mean another: Read %+v, want none", *obs[1].o.Read)
	}
}

// recordingConn records what is read from the connection.
type recordingConn struct {
	net.Conn
	mu   sync.Mutex
	read bytes.Buffer
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.read.Write(p[:n])
	c.mu.Unlock()
	return n, err
}

// take returns what was read since the last take.
func (c *recordingConn) take() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := bytes.Clone(c.read.Bytes())
	c.read.Reset()
	return b
}

// Rule 6: the bytes a client gets for a tracked read are the bytes it gets
// when the read is not tracked, in the simple and the extended protocol, text
// and binary.
func TestTrackedReadBytesUnchanged(t *testing.T) {
	tb := idTable(t, 5)
	sql := "select id, title from " + tb.name + " where id <= $1 order by id"
	for _, c := range []struct {
		name string
		read func(ctx context.Context, conn *pgx.Conn) error
	}{
		{"simple", func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.PgConn().Exec(ctx, "select id, title from "+tb.name+" order by id").ReadAll()
			return err
		}},
		{"extended", func(ctx context.Context, conn *pgx.Conn) error {
			if _, err := conn.PgConn().ExecParams(ctx, sql, [][]byte{[]byte("4")}, nil, nil, []int16{1, 0}).Close(); err != nil {
				return err
			}
			sd, err := conn.PgConn().Prepare(ctx, "s", sql, nil)
			if err != nil {
				return err
			}
			_, err = conn.PgConn().ExecStatement(ctx, sd, [][]byte{[]byte("3")}, nil, []int16{1}).Close()
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := func(g *recordingGate) []byte {
				t.Helper()
				addr, _ := startProxy(t, upstream(t), g)
				cfg, err := pgx.ParseConfig(connURL(addr, "stet_scram", testUsers["stet_scram"], nil))
				if err != nil {
					t.Fatal(err)
				}
				var rc *recordingConn
				cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
					var d net.Dialer
					nc, err := d.DialContext(ctx, network, addr)
					if err != nil {
						return nil, err
					}
					rc = &recordingConn{Conn: nc}
					return rc, nil
				}
				ctx := testCtx(t)
				conn, err := pgx.ConnectConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close(context.Background())
				rc.take() // startup, which carries the backend's pid and key
				if err := c.read(ctx, conn); err != nil {
					t.Fatal(err)
				}
				return rc.take()
			}
			tracking := newKeyGate(tb)
			plain := newObservingGate(readsOf(tb.name))
			tracked, untracked := got(tracking), got(plain)
			if len(tracked) == 0 || !bytes.Equal(tracked, untracked) {
				t.Errorf("client got %d bytes from a tracked read and %d untracked; want the same bytes\ntracked:   %q\nuntracked: %q",
					len(tracked), len(untracked), tracked, untracked)
			}
			tracking.mu.Lock()
			reads := slices.Clone(tracking.reads)
			tracking.mu.Unlock()
			for i, ob := range reads {
				if ob.o.Read == nil || len(ob.o.Read.Keys) == 0 {
					t.Errorf("tracked read %d: Read %+v, want keys taken", i, ob.o.Read)
				}
			}
		})
	}
}

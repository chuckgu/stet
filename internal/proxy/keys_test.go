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

// resolvesRE matches the statements the test gate marks Resolves, as the
// classifier would here: what sets or resets search_path, the role or the
// session authorization, RESET ALL and DISCARD ALL.
var resolvesRE = regexp.MustCompile(`(?i)^\s*(set\s+(local\s+|session\s+)?(search_path|role|session\s+authorization|schema)\b|reset\s+(search_path|role|session\s+authorization|all)\b|discard\s+all\b)`)

// newKeyGate observes the reads that name one of tables, marks ReadOnly what
// readOnlyRE matches, and so tracks each observed read among those, and
// Resolves what resolvesRE matches; Tracked answers each table's key column.
// It watches no write.
func newKeyGate(tables ...keyTable) *recordingGate {
	g := newRecordingGate(func(string) bool { return false })
	g.observeMatch = func(sql string) bool {
		return !isWrite(sql) && slices.ContainsFunc(tables, func(tb keyTable) bool { return strings.Contains(sql, tb.name) })
	}
	g.readOnly = readOnlyRE.MatchString
	g.resolves = resolvesRE.MatchString
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

// Key columns learnt at a Describe hold only while nothing the proxy can see
// may have changed which table the statement's names mean. After a SET of
// search_path or role (also inside a transaction still open), RESET ALL, a
// SET LOCAL whose transaction has ended, or a DO block or FunctionCall whose
// content the proxy cannot see, the statement's Executes take no keys until
// it is described again. A SET of stet.on_behalf_of or of the time zone
// leaves them. A Describe answered after the SET ran, and an Execute in the
// transaction of a SET LOCAL, keep their keys; an Execute pipelined behind
// the end of that transaction does not.
func TestKeysForgottenWhenNamesMayResolveOtherwise(t *testing.T) {
	t.Cleanup(proxy.SetMaxInspect(1024)) // before the proxy, so it is restored once the proxy closed
	fn := backendPID(t)
	tb := idTable(t, 3)
	g := newKeyGate(tb)
	g.block = blockClosedPoints
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawConn(t, addr)
	read := "select id from " + tb.name + " where id = 1"
	run := func() {
		t.Helper()
		exchange(t, fe, &pgproto3.Bind{PreparedStatement: "s"}, &pgproto3.Execute{}, &pgproto3.Sync{})
	}

	exchange(t, fe, &pgproto3.Parse{Name: "s", Query: read}, &pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Sync{})
	run()
	wantKeys(t, "described", g.observed(t, 1)[0].o, tb, "1")
	// Each is sent and answered in turn.
	type msgs = []pgproto3.FrontendMessage
	query := func(sql string) msgs { return msgs{&pgproto3.Query{String: sql}} }
	n := 1
	for _, c := range []struct {
		name           string
		between, after []msgs
		keys           bool
	}{
		{"set search_path", []msgs{query("set search_path = public")}, nil, false},
		{"set role", []msgs{query("set role stet_scram")}, nil, false},
		{"reset all", []msgs{query("reset all")}, nil, false},
		{"set local search_path, then commit",
			[]msgs{query("begin"), query("set local search_path = public"), query("commit")}, nil, false},
		{"set search_path in a transaction still open",
			[]msgs{query("begin"), query("set search_path = public")}, []msgs{query("commit")}, false},
		{"a DO block", []msgs{query("do $$ begin perform 1; end $$")}, nil, false},
		{"a FunctionCall", []msgs{{&pgproto3.FunctionCall{Function: fn}}}, nil, false},
		{"a Query the proxy could not read", []msgs{query("select 1 /* " + strings.Repeat("x", 1100) + " */")}, nil, false},
		{"SQL EXECUTE", []msgs{query("prepare w as select 1"), query("execute w")}, []msgs{query("deallocate w")}, false},
		{"an Execute of a portal the proxy does not know",
			[]msgs{query("declare c cursor with hold for select 1"), {&pgproto3.Execute{Portal: "c"}, &pgproto3.Sync{}}},
			[]msgs{query("close c")}, false},
		{"set stet.on_behalf_of", []msgs{query("set stet.on_behalf_of = 'task-1'")}, nil, true},
		{"set timezone", []msgs{query("set timezone = 'UTC'")}, nil, true},
	} {
		for _, m := range c.between {
			exchange(t, fe, m...)
		}
		run()
		exchange(t, fe, &pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Sync{})
		run()
		for _, m := range c.after {
			exchange(t, fe, m...)
		}
		obs := g.observed(t, n+2)
		if c.keys {
			wantKeys(t, "after "+c.name, obs[n].o, tb, "1")
		} else {
			wantNoRead(t, "after "+c.name, obs[n].o)
		}
		wantKeys(t, "after "+c.name+", described again", obs[n+1].o, tb, "1")
		n += 2
	}

	sendAll(t, fe, &pgproto3.Query{String: "set search_path = public"}, &pgproto3.Parse{Query: read}, &pgproto3.Bind{},
		&pgproto3.Describe{ObjectType: 'P'}, &pgproto3.Execute{}, &pgproto3.Sync{})
	untilReady(t, fe)
	untilReady(t, fe)
	// The SET LOCAL holds in the implicit transaction of its own Sync.
	sendAll(t, fe, &pgproto3.Parse{Name: "l", Query: "set local search_path = public"}, &pgproto3.Bind{PreparedStatement: "l"},
		&pgproto3.Execute{}, &pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Bind{PreparedStatement: "s"},
		&pgproto3.Execute{}, &pgproto3.Sync{}, &pgproto3.Bind{PreparedStatement: "s"}, &pgproto3.Execute{}, &pgproto3.Sync{})
	untilReady(t, fe)
	untilReady(t, fe)
	obs := g.observed(t, n+3)
	wantKeys(t, "pipelined behind a SET", obs[n].o, tb, "1")
	wantKeys(t, "in the transaction of a SET LOCAL", obs[n+1].o, tb, "1")
	wantNoRead(t, "pipelined behind the end of a SET LOCAL's transaction", obs[n+2].o)
}

// rowsOf sends msgs in one write and returns the values of the rows the
// answers up to ReadyForQuery hold, failing on an error.
func rowsOf(t *testing.T, fe *pgproto3.Frontend, msgs ...pgproto3.FrontendMessage) [][]string {
	t.Helper()
	sendAll(t, fe, msgs...)
	var rows [][]string
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatal(err)
		}
		switch m := msg.(type) {
		case *pgproto3.DataRow:
			var row []string
			for _, v := range m.Values {
				row = append(row, string(v))
			}
			rows = append(rows, row)
		case *pgproto3.ErrorResponse:
			t.Fatalf("SQLSTATE %s: %s", m.Code, m.Message)
		case *pgproto3.ReadyForQuery:
			return rows
		}
	}
}

// twoSchemas creates two schemas, each with a table accounts (id int primary
// key, title text) whose rows are titled with the schema's name: ids 1 and 3
// in the first, 2 and 4 in the second. It starts a proxy whose gate tracks
// both tables by id and returns the gate, a frontend through the proxy, the
// schemas and their tables' OIDs. The schemas are dropped when the test ends.
func twoSchemas(t *testing.T) (*recordingGate, *pgproto3.Frontend, []string, []uint32) {
	t.Helper()
	stamp := time.Now().UnixNano()
	admin := connect(t, upstream(t), "stet_scram", nil)
	schemas := []string{fmt.Sprintf("keys_a_%d", stamp), fmt.Sprintf("keys_b_%d", stamp)}
	oids := make([]uint32, 2)
	for i, schema := range schemas {
		mustExec(t, admin, "create schema "+schema)
		t.Cleanup(func() { admin.Exec(context.Background(), "drop schema "+schema+" cascade") })
		mustExec(t, admin, "create table "+schema+".accounts (id int primary key, title text)")
		mustExec(t, admin, fmt.Sprintf("insert into %s.accounts values (%d, '%s'), (%d, '%[3]s')", schema, i+1, schema, i+3))
		var oid int64
		if err := admin.QueryRow(testCtx(t), "select ($1 || '.accounts')::regclass::oid::int8", schema).Scan(&oid); err != nil {
			t.Fatal(err)
		}
		oids[i] = uint32(oid)
	}
	g := newRecordingGate(func(string) bool { return false })
	g.observeMatch = func(sql string) bool { return strings.Contains(sql, "accounts") }
	g.readOnly, g.resolves = readOnlyRE.MatchString, resolvesRE.MatchString
	g.tracked = map[uint32]trackedKey{oids[0]: {attr: 1, typ: proxy.KeyInt4}, oids[1]: {attr: 1, typ: proxy.KeyInt4}}
	addr, _ := startProxy(t, upstream(t), g)
	return g, rawConn(t, addr), schemas, oids
}

// wantRows fails the test unless rows are the rows of schema with ids, in
// that order.
func wantRows(t *testing.T, what string, rows [][]string, schema string, ids ...string) {
	t.Helper()
	var want [][]string
	for _, id := range ids {
		want = append(want, []string{id, schema})
	}
	if !slices.EqualFunc(rows, want, slices.Equal) {
		t.Fatalf("%s: rows %q, want %q", what, rows, want)
	}
}

// Tables of the same name in two schemas: a statement prepared and described
// under one search_path, then run under the other, which the server plans
// again against the other table with no new Parse or RowDescription. Its rows
// are not keyed as the first table's; once described again they are keyed as
// the second's.
func TestKeysOfAStatementRunUnderAnotherSearchPath(t *testing.T) {
	g, fe, schemas, oids := twoSchemas(t)
	run := func() [][]string {
		t.Helper()
		return rowsOf(t, fe, &pgproto3.Bind{PreparedStatement: "s"}, &pgproto3.Execute{}, &pgproto3.Sync{})
	}

	exchange(t, fe, &pgproto3.Query{String: "set search_path = " + schemas[0]})
	exchange(t, fe, &pgproto3.Parse{Name: "s", Query: "select id, title from accounts order by id limit 1"},
		&pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Sync{})
	first := run()
	exchange(t, fe, &pgproto3.Query{String: "set search_path = " + schemas[1]})
	second := run()
	exchange(t, fe, &pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Sync{})
	third := run()
	// Again inside one transaction, where no idle ReadyForQuery comes between.
	exchange(t, fe, &pgproto3.Query{String: "begin"})
	exchange(t, fe, &pgproto3.Query{String: "set search_path = " + schemas[0]})
	exchange(t, fe, &pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Sync{})
	fourth := run()
	exchange(t, fe, &pgproto3.Query{String: "set search_path = " + schemas[1]})
	fifth := run()
	exchange(t, fe, &pgproto3.Query{String: "commit"})
	for i, c := range []struct {
		rows [][]string
		want string
	}{{first, schemas[0]}, {second, schemas[1]}, {third, schemas[1]}, {fourth, schemas[0]}, {fifth, schemas[1]}} {
		if len(c.rows) != 1 || c.rows[0][1] != c.want {
			t.Fatalf("run %d: rows %q, want the row of %s", i+1, c.rows, c.want)
		}
	}
	obs := g.observed(t, 5)
	wantRead(t, "under the first search_path", obs[0].o, oids[:1], []string{fmt.Sprintf("%d:1", oids[0])})
	wantNoRead(t, "under the second search_path, not described again", obs[1].o)
	wantRead(t, "under the second search_path, described again", obs[2].o, oids[1:], []string{fmt.Sprintf("%d:2", oids[1])})
	wantRead(t, "in a transaction, under the first search_path", obs[3].o, oids[:1], []string{fmt.Sprintf("%d:1", oids[0])})
	wantNoRead(t, "in a transaction, under the second search_path, not described again", obs[4].o)
}

// The server plans a portal at its Bind, and an Execute sends the rows of
// that plan. A portal bound before a SET of search_path and run after it,
// with its statement described after the SET, sends the first table's rows:
// they are not keyed as the second table's (M1). A portal described after
// the SET describes its own plan, of the first table: a new portal, bound
// after the SET, reads the second table and is not keyed as the first's
// (M2). Bound, described and run after the SET, a portal is keyed as the
// second table's.
func TestKeysOfAPortalAreThoseOfItsBind(t *testing.T) {
	sql := "select id, title from accounts order by id limit 1"
	again := func(t *testing.T, fe *pgproto3.Frontend) [][]string {
		t.Helper()
		return rowsOf(t, fe, &pgproto3.Bind{DestinationPortal: "q", PreparedStatement: "s"},
			&pgproto3.Describe{ObjectType: 'P', Name: "q"}, &pgproto3.Execute{Portal: "q"}, &pgproto3.Sync{})
	}

	t.Run("M1: bound before the SET, statement described after it", func(t *testing.T) {
		g, fe, schemas, oids := twoSchemas(t)
		exchange(t, fe, &pgproto3.Query{String: "set search_path = " + schemas[0]})
		exchange(t, fe, &pgproto3.Parse{Name: "s", Query: sql}, &pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Sync{})
		rows := rowsOf(t, fe, &pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "s"},
			&pgproto3.Parse{Name: "l", Query: "set search_path = " + schemas[1]}, &pgproto3.Bind{PreparedStatement: "l"},
			&pgproto3.Execute{}, &pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Execute{Portal: "p"}, &pgproto3.Sync{})
		wantRows(t, "the portal bound before the SET", rows, schemas[0], "1")
		wantRows(t, "bound, described and run after the SET", again(t, fe), schemas[1], "2")
		obs := g.observed(t, 2)
		wantNoRead(t, "the portal bound before the SET", obs[0].o)
		wantRead(t, "bound, described and run after the SET", obs[1].o, oids[1:], []string{fmt.Sprintf("%d:2", oids[1])})
	})

	t.Run("M2: old portal described after the SET, new portal run", func(t *testing.T) {
		g, fe, schemas, oids := twoSchemas(t)
		exchange(t, fe, &pgproto3.Query{String: "set search_path = " + schemas[0]})
		exchange(t, fe, &pgproto3.Parse{Name: "s", Query: sql}, &pgproto3.Sync{})
		exchange(t, fe, &pgproto3.Query{String: "begin"})
		exchange(t, fe, &pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "s"}, &pgproto3.Sync{})
		exchange(t, fe, &pgproto3.Query{String: "set search_path = " + schemas[1]})
		exchange(t, fe, &pgproto3.Describe{ObjectType: 'P', Name: "p"}, &pgproto3.Sync{})
		rows := rowsOf(t, fe, &pgproto3.Bind{PreparedStatement: "s"}, &pgproto3.Execute{}, &pgproto3.Sync{})
		exchange(t, fe, &pgproto3.Query{String: "commit"})
		wantRows(t, "a new portal bound after the SET", rows, schemas[1], "2")
		wantRows(t, "bound, described and run after the SET", again(t, fe), schemas[1], "2")
		obs := g.observed(t, 2)
		wantNoRead(t, "a new portal bound after the SET", obs[0].o)
		wantRead(t, "bound, described and run after the SET", obs[1].o, oids[1:], []string{fmt.Sprintf("%d:2", oids[1])})
	})
}

// A portal read in parts keeps the plan of its Bind: after a SET of
// search_path between two parts, the later part still sends the first
// table's rows. Its keys are not taken: the Execute came after the SET, and
// what the proxy compares does not tell that the portal's plan did not move.
func TestKeysOfAPortalInPartsAcrossASet(t *testing.T) {
	g, fe, schemas, oids := twoSchemas(t)
	exchange(t, fe, &pgproto3.Query{String: "set search_path = " + schemas[0]})
	exchange(t, fe, &pgproto3.Parse{Name: "s", Query: "select id, title from accounts order by id"},
		&pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Sync{})
	exchange(t, fe, &pgproto3.Query{String: "begin"})
	first := rowsOf(t, fe, &pgproto3.Bind{DestinationPortal: "p", PreparedStatement: "s"}, &pgproto3.Execute{Portal: "p", MaxRows: 1}, &pgproto3.Sync{})
	exchange(t, fe, &pgproto3.Query{String: "set search_path = " + schemas[1]})
	second := rowsOf(t, fe, &pgproto3.Execute{Portal: "p", MaxRows: 1}, &pgproto3.Sync{})
	exchange(t, fe, &pgproto3.Query{String: "commit"})
	wantRows(t, "the first part", first, schemas[0], "1")
	wantRows(t, "the part after the SET", second, schemas[0], "3")
	obs := g.observed(t, 2)
	wantRead(t, "the first part", obs[0].o, oids[:1], []string{fmt.Sprintf("%d:1", oids[0])})
	wantNoRead(t, "the part after the SET", obs[1].o)
}

// trackAnyway marks every observed read Track while leaving it not
// ReadOnly, as a gate in error might.
type trackAnyway struct{ *recordingGate }

func (g trackAnyway) Inspect(sql string) proxy.Statement {
	st := g.recordingGate.Inspect(sql)
	st.Track, st.ReadOnly = st.Observe, false
	return st
}

// Spec 4: a message that is not read-only gives no keys, even when the gate
// marks it Track.
func TestNoKeysFromAMessageThatIsNotReadOnly(t *testing.T) {
	tb := idTable(t, 3)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), trackAnyway{g})
	conn := connect(t, addr, "stet_scram", nil)

	mustExec(t, conn, "select id from "+tb.name+" where id = 1")
	o := g.observed(t, 1)[0].o
	wantNoRead(t, "a read not marked ReadOnly", o)
}

// A bind value longer than any key the proxy takes could never become one:
// it is not kept, and the read's Param is nil.
func TestLongKeyParamIsNotKept(t *testing.T) {
	tb := newKeyTable(t, "k text primary key, n int", "k", proxy.KeyText, "insert into %[1]s values ('a', 1)")
	g := newKeyGate(tb)
	g.keyParam = func(sql string) int {
		if strings.Contains(sql, "k = $1") {
			return 1
		}
		return 0
	}
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	sql := "select n from " + tb.name + " where k = $1"
	for _, n := range []int{1024, 1025} {
		if _, err := conn.PgConn().ExecParams(testCtx(t), sql, [][]byte{bytes.Repeat([]byte("x"), n)}, []uint32{25}, nil, nil).Close(); err != nil {
			t.Fatal(err)
		}
	}
	obs := g.observed(t, 2)
	if r := obs[0].o.Read; r == nil || r.Param == nil || len(r.Param.Value) != 1024 {
		t.Errorf("1024-byte value: Read %+v, want its Param kept", r)
	}
	if r := obs[1].o.Read; r == nil || r.Param != nil {
		t.Errorf("1025-byte value: Read %+v, want a Read with no Param", r)
	}
}

// The reads of one unit take MaxUnitKeys keys in all: a read that starts once
// the unit has them keeps the keys it took before, possibly none. Every row is
// still counted.
func TestKeysAUnitHoldsAreBounded(t *testing.T) {
	const per = 700 // rows each Execute sends
	tb := idTable(t, per)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawConn(t, addr)

	exchange(t, fe, &pgproto3.Parse{Name: "s", Query: "select id from " + tb.name + " order by id"},
		&pgproto3.Describe{ObjectType: 'S', Name: "s"}, &pgproto3.Sync{})
	reads := proxy.MaxUnitKeys/per + 2
	var msgs []pgproto3.FrontendMessage
	for range reads {
		msgs = append(msgs, &pgproto3.Bind{PreparedStatement: "s"}, &pgproto3.Execute{})
	}
	sendAll(t, fe, append(msgs, &pgproto3.Sync{})...)
	if rows, _ := untilReady(t, fe); rows != reads*per {
		t.Fatalf("client got %d rows, want %d", rows, reads*per)
	}
	left := proxy.MaxUnitKeys
	for i, ob := range g.observed(t, reads) {
		want := min(per, left)
		left -= want
		if o := ob.o; o.Sent != per || o.Read == nil || len(o.Read.Keys) != want || !slices.Equal(o.Read.Tables, []uint32{tb.oid}) {
			t.Errorf("Execute %d: %d rows sent, Read %v; want %d rows sent and %d keys", i+1, o.Sent, o.Read != nil, per, want)
		}
	}
}

// A Query's rows come in the format its RowDescription gives each column:
// text, but for a binary cursor's. A key column it marks binary is read as
// binary.
func TestQueryKeysInTheFormatsOfItsRowDescription(t *testing.T) {
	const oid = 4242
	g := newObservingGate(func(string) bool { return true })
	g.readOnly = func(string) bool { return true }
	g.tracked = map[uint32]trackedKey{oid: {attr: 1, typ: proxy.KeyInt4}}
	desc := &pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("id"), TableOID: oid,
		TableAttributeNumber: 1, DataTypeOID: 23, DataTypeSize: 4, TypeModifier: -1, Format: 1}}}
	proxy.ServerShows(g, []pgproto3.FrontendMessage{&pgproto3.Query{String: "select id from t"}},
		[]pgproto3.BackendMessage{desc, &pgproto3.DataRow{Values: [][]byte{{0, 0, 0, 7}}},
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}, &pgproto3.ReadyForQuery{TxStatus: 'I'}})
	wantRead(t, "binary column", g.observed(t, 1)[0].o, []uint32{oid}, []string{fmt.Sprintf("%d:7", oid)})
}

// What the proxy cannot see leaves the transaction not read-only: a tracked
// read after a FunctionCall, or after an Execute of a portal the proxy does
// not know (here a SQL cursor's), takes no keys; before it, it does.
func TestNoKeysAfterAMessageThatCannotBeSeen(t *testing.T) {
	fn := backendPID(t)
	tb := idTable(t, 3)
	g := newKeyGate(tb)
	g.readOnly = func(sql string) bool { // the DECLARE too: only the protocol messages below mark the transaction
		return readOnlyRE.MatchString(sql) || strings.HasPrefix(sql, "declare")
	}
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawConn(t, addr)
	read := &pgproto3.Query{String: "select id from " + tb.name + " where id = 1"}
	query := func(sql string) {
		t.Helper()
		exchange(t, fe, &pgproto3.Query{String: sql})
	}

	query("begin")
	exchange(t, fe, read)
	exchange(t, fe, &pgproto3.FunctionCall{Function: fn})
	exchange(t, fe, read)
	query("commit")
	query("begin")
	query("declare c cursor for select 1")
	exchange(t, fe, read)
	exchange(t, fe, &pgproto3.Execute{Portal: "c"}, &pgproto3.Sync{})
	exchange(t, fe, read)
	query("commit")
	obs := g.observed(t, 4)
	wantKeys(t, "before a FunctionCall", obs[0].o, tb, "1")
	wantNoRead(t, "after a FunctionCall", obs[1].o)
	wantKeys(t, "before an Execute of an unknown portal", obs[2].o, tb, "1")
	wantNoRead(t, "after an Execute of an unknown portal", obs[3].o)
}

// So does a Query the proxy could not read (past maxInspect).
func TestNoKeysAfterAnUnreadableQuery(t *testing.T) {
	t.Cleanup(proxy.SetMaxInspect(512)) // before the proxy, so it is restored once the proxy closed
	tb := idTable(t, 3)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	read := "select id from " + tb.name + " where id = 1"

	mustExec(t, conn, "begin")
	mustExec(t, conn, read)
	mustExec(t, conn, "select 1 /* "+strings.Repeat("x", 600)+" */")
	mustExec(t, conn, read)
	mustExec(t, conn, "commit")
	obs := g.observed(t, 2)
	wantKeys(t, "before an unreadable Query", obs[0].o, tb, "1")
	wantNoRead(t, "after an unreadable Query", obs[1].o)
}

// Extended-protocol messages closed by a Query before any Sync run in one
// implicit transaction with it: the Execute and the Query both give keys.
func TestKeysFromAnUnsyncedUnit(t *testing.T) {
	tb := idTable(t, 3)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	fe := rawConn(t, addr)

	sendAll(t, fe, &pgproto3.Parse{Query: "select id from " + tb.name + " where id <= 2 order by id"}, &pgproto3.Bind{},
		&pgproto3.Describe{ObjectType: 'P'}, &pgproto3.Execute{}, &pgproto3.Query{String: "select id from " + tb.name + " where id = 3"})
	if rows, _ := untilReady(t, fe); rows != 3 {
		t.Fatalf("client got %d rows, want 3", rows)
	}
	obs := g.observed(t, 2)
	wantKeys(t, "the Execute", obs[0].o, tb, "1", "2")
	wantKeys(t, "the Query", obs[1].o, tb, "3")
}

// A read cut off by the connection's end is reported with the keys of the
// rows the proxy saw before it, in order, no more than it counted.
func TestKeysOfALostRead(t *testing.T) {
	tb := idTable(t, 1000)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)

	// Rows of some 200 bytes, one a millisecond: still sending when the
	// client hangs up.
	rows, err := conn.Query(testCtx(t), "select id, repeat('x', 200), pg_sleep(0.001) from "+tb.name+" order by id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no row arrived: %v", rows.Err())
	}
	conn.PgConn().Conn().Close()

	o := g.observed(t, 1)[0].o
	if !o.Lost || o.Read == nil || len(o.Read.Keys) == 0 || int64(len(o.Read.Keys)) > o.Sent {
		t.Fatalf("lost read: %+v, want it lost with keys, no more than its %d rows", o, o.Sent)
	}
	var want []string
	for i := range o.Read.Keys {
		want = append(want, strconv.Itoa(i+1))
	}
	wantKeys(t, "lost read", o, tb, want...)
}

// A read that failed after it sent rows is reported with the keys of those
// rows, simple and extended; the gate remembers no read that failed.
func TestKeysOfAFailedRead(t *testing.T) {
	tb := idTable(t, 5)
	g := newKeyGate(tb)
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	sql := "select id, 1 / (3 - id) from " + tb.name + " order by id"

	if _, err := conn.PgConn().Exec(testCtx(t), sql).ReadAll(); pgCode(err) != "22012" {
		t.Fatalf("simple read: %v, want 22012", err)
	}
	if _, err := conn.PgConn().ExecParams(testCtx(t), sql, nil, nil, nil, nil).Close(); pgCode(err) != "22012" {
		t.Fatalf("extended read: %v, want 22012", err)
	}
	for i, ob := range g.observed(t, 2) {
		if o := ob.o; o.Executed || !strings.Contains(o.Error, "22012") || o.Sent != 2 {
			t.Errorf("read %d: %+v, want 2 rows sent and the 22012 error", i, o)
		}
		wantKeys(t, fmt.Sprintf("read %d", i), ob.o, tb, "1", "2")
	}
}

// An Execute of a portal whose name may still mean a SET of search_path,
// though its newest meaning is a plain select, may run the SET: key columns
// learnt before it do not hold after it. The server's answers are made up
// here, inside a transaction, so no idle ReadyForQuery comes between.
func TestKeysForgottenAfterAPortalThatMayRunASet(t *testing.T) {
	const oid = 4242
	desc := &pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("id"), TableOID: oid,
		TableAttributeNumber: 1, DataTypeOID: 23, DataTypeSize: 4, TypeModifier: -1}}}
	g := newObservingGate(func(sql string) bool { return strings.HasPrefix(sql, "select id") })
	g.readOnly, g.resolves = func(string) bool { return true }, resolvesRE.MatchString
	g.tracked = map[uint32]trackedKey{oid: {attr: 1, typ: proxy.KeyInt4}}
	inTx := &pgproto3.ReadyForQuery{TxStatus: 'T'}
	bind := func(stmt string) []pgproto3.FrontendMessage {
		return []pgproto3.FrontendMessage{&pgproto3.Bind{PreparedStatement: stmt}, &pgproto3.Sync{}}
	}
	run := func(binds ...string) {
		t.Helper()
		sent := []pgproto3.FrontendMessage{&pgproto3.Query{String: "begin"},
			&pgproto3.Parse{Name: "r", Query: "select id from t"}, &pgproto3.Describe{ObjectType: 'S', Name: "r"}, &pgproto3.Sync{},
			&pgproto3.Parse{Name: "x", Query: "set search_path = public"}, &pgproto3.Parse{Name: "one", Query: "select 1"}, &pgproto3.Sync{}}
		answers := []pgproto3.BackendMessage{&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")}, inTx,
			&pgproto3.ParseComplete{}, &pgproto3.ParameterDescription{}, desc, inTx,
			&pgproto3.ParseComplete{}, &pgproto3.ParseComplete{}, inTx}
		for _, stmt := range binds {
			sent = append(sent, bind(stmt)...)
			answers = append(answers, &pgproto3.BindComplete{}, inTx)
		}
		sent = append(sent, &pgproto3.Execute{}, &pgproto3.Sync{},
			&pgproto3.Bind{PreparedStatement: "r"}, &pgproto3.Execute{}, &pgproto3.Sync{})
		answers = append(answers, &pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}, inTx,
			&pgproto3.BindComplete{}, &pgproto3.DataRow{Values: [][]byte{[]byte("1")}},
			&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}, inTx)
		proxy.ServerShows(g, sent, answers)
	}

	run("one")      // the unnamed portal means the select alone
	run("x", "one") // it may still mean the SET, bound in a unit not answered yet
	obs := g.observed(t, 2)
	wantRead(t, "after a portal that means a select", obs[0].o, []uint32{oid}, []string{fmt.Sprintf("%d:1", oid)})
	if obs[1].o.Read != nil {
		t.Errorf("after a portal that may run a SET: Read %+v, want none", *obs[1].o.Read)
	}
}

package gate_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/store"
)

// staleEnv is a gate whose tables remember reads, over the test database,
// with no proxy in front: tests hand it statements and outcomes as the proxy
// would. Tables are created first (create), then the gate is started over
// them (start), since it reads the catalog when it starts.
type staleEnv struct {
	stamp int64
	admin *pgx.Conn
	pool  *pgxpool.Pool
	gate  *gate.Gate
	logs  lockedBuffer
}

func newStaleEnv(t *testing.T) *staleEnv {
	t.Helper()
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	ctx := context.Background()
	e := &staleEnv{stamp: time.Now().UnixNano()}
	dsn := "postgres://stet_scram:scram-pw@" + addr + "/stet_test?sslmode=disable"
	var err error
	if e.admin, err = pgx.Connect(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.admin.Close(ctx) })
	if e.pool, err = pgxpool.New(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.pool.Close)
	return e
}

// name is base made unique to the test.
func (e *staleEnv) name(base string) string { return fmt.Sprintf("%s_%d", base, e.stamp) }

// create makes the table base, made unique, with columns, and returns its
// name. It is dropped when the test ends.
func (e *staleEnv) create(t *testing.T, base, columns string) string {
	t.Helper()
	name := e.name(base)
	mustExec(t, e.admin, "create table "+quoted(name)+" "+columns)
	t.Cleanup(func() { e.admin.Exec(context.Background(), "drop table if exists "+quoted(name)) })
	return name
}

// schema makes the schema base, made unique, and returns its name. It is
// dropped with what it holds when the test ends.
func (e *staleEnv) schema(t *testing.T, base string) string {
	t.Helper()
	name := e.name(base)
	mustExec(t, e.admin, "create schema "+name)
	t.Cleanup(func() { e.admin.Exec(context.Background(), "drop schema if exists "+name+" cascade") })
	return name
}

// start makes the gate over tables, each a block of tableBlock. Its log goes
// to e.logs as JSON.
func (e *staleEnv) start(t *testing.T, tables ...string) {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.Parse(fmt.Appendf(nil, `
listen: 127.0.0.1:0
upstream: postgres://stet_scram@%s/stet_test
model: {adapter: fake, version: fake-1, timeout_ms: 2000}
tables:
%s`, os.Getenv("STET_TEST_PG"), strings.Join(tables, "")))
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(e.pool)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if e.gate, err = gate.New(cfg, e.pool, st, calm(), slog.New(slog.NewJSONHandler(&e.logs, nil))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.gate.Close)
}

// tableBlock is the config of the gated table name keyed by pk, with clauses
// (stale: {}, reads: {}) under it.
func tableBlock(name, pk string, clauses ...string) string {
	s := fmt.Sprintf("  %q:\n    pk: %q\n    send_fields: [%q]\n", name, pk, pk)
	for _, c := range clauses {
		s += "    " + c + "\n"
	}
	return s
}

// quoted is the config key table as SQL names it.
func quoted(table string) string { return pgx.Identifier(strings.Split(table, ".")).Sanitize() }

// oid is the OID of table.
func (e *staleEnv) oid(t *testing.T, table string) uint32 {
	t.Helper()
	var oid int64
	if err := e.admin.QueryRow(context.Background(), "select to_regclass($1)::oid::int8", quoted(table)).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	return uint32(oid)
}

// xmin is the version of the row of table whose column col is key, as
// written in SQL.
func (e *staleEnv) xmin(t *testing.T, table, col, key string) string {
	t.Helper()
	var v string
	err := e.admin.QueryRow(context.Background(),
		"select xmin::text from "+quoted(table)+" where "+pgx.Identifier{col}.Sanitize()+" = $1",
		pgx.QueryExecModeSimpleProtocol, key).Scan(&v)
	if err != nil {
		t.Fatalf("xmin of %s %s = %q: %v", table, col, key, err)
	}
	return v
}

// insert adds the rows of table whose column col holds keys, each in its own
// transaction, so each has its own version.
func (e *staleEnv) insert(t *testing.T, table, col string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		mustExec(t, e.admin, "insert into "+quoted(table)+" ("+pgx.Identifier{col}.Sanitize()+") values ($1)",
			pgx.QueryExecModeSimpleProtocol, k)
	}
}

// idle waits until the gate has no lookup queued or running.
func (e *staleEnv) idle(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.gate.StaleIdle(ctx); err != nil {
		t.Fatalf("lookups still running: %v", err)
	}
}

// warnings are the WARN lines logged so far that name table.
func (e *staleEnv) warnings(t *testing.T, table string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range logRecords(t, &e.logs) {
		if rec["level"] == "WARN" && rec["table"] == table {
			out = append(out, rec)
		}
	}
	return out
}

// check is what the gate's memory answers for a write by role and task to
// the row key of table, whose version now is version.
func (e *staleEnv) check(role, task, table, key, version string) decide.ReadCheck {
	return gate.CheckRead(e.gate, role, task, table, key, version)
}

func (e *staleEnv) wantResult(t *testing.T, role, task, table, key, version, want string) {
	t.Helper()
	if got := e.check(role, task, table, key, version); got.Result != want {
		t.Errorf("check(%s, %q, %s, %q, %s) = %+v, want %s", role, task, table, key, version, got, want)
	}
}

// keyed is what the proxy reports of a tracked read of the table oid that
// sent one row for each key.
func keyed(oid uint32, keys ...string) proxy.Outcome {
	rk := &proxy.ReadKeys{Tables: []uint32{oid}}
	for _, k := range keys {
		rk.Keys = append(rk.Keys, proxy.RowKey{Table: oid, Text: k})
	}
	return proxy.Outcome{Executed: true, Sent: int64(len(keys)), Arrived: time.Now(), Read: rk}
}

// whereKeyed is what the proxy reports of a tracked read that selects by
// `key = $1` with param, sent sent rows and had a column of the tables oids.
func whereKeyed(param *proxy.Param, sent int64, oids ...uint32) proxy.Outcome {
	return proxy.Outcome{Executed: true, Sent: sent, Arrived: time.Now(), Read: &proxy.ReadKeys{Tables: oids, Param: param}}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReadsAreRemembered(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	e.insert(t, deals, "id", "1", "2", "3")
	e.start(t, tableBlock(deals, "id", "stale: {}"))
	oid := e.oid(t, deals)

	st := e.gate.Inspect("select id, title from " + deals)
	e.gate.Observe(proxy.SessionInfo{ID: "s1", User: "agent", OnBehalfOf: "alice"}, st, keyed(oid, "1", "2", "3"))
	e.idle(t)

	for _, k := range []string{"1", "2", "3"} {
		v := e.xmin(t, deals, "id", k)
		got := e.check("agent", "alice", deals, k, v)
		if got.Result != decide.ReadFresh || got.ReadVersion != v {
			t.Errorf("row %s: %+v, want fresh with read_version %s", k, got, v)
		}
	}
	// The memory is the reading role's: another role read nothing.
	e.wantResult(t, "other", "", deals, "1", e.xmin(t, deals, "id", "1"), decide.ReadNone)
}

func TestInspectMarksTrackedReads(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	notes := e.create(t, "notes", "(id int primary key, title text)")
	logs := e.create(t, "logs", "(id int primary key, title text)")
	e.start(t, tableBlock(deals, "id", "stale: {}"), tableBlock(notes, "id"), tableBlock(logs, "id", "reads: {}"))

	for _, tc := range []struct {
		sql                      string
		observe, track, readOnly bool
		keyParam                 int
	}{
		{"select id, title from " + deals, true, true, true, 0},
		{"select title from " + deals + " where id = $1", true, true, true, 1},
		{"select title from " + deals + " where id = 7", true, true, true, 0},
		{"select id from " + notes, false, false, true, 0},
		{"select id from " + logs, true, false, true, 0},
		{"declare c cursor for select id from " + deals, true, false, false, 0},
		{"copy " + deals + " to stdout", true, false, false, 0},
		{"update " + deals + " set title = 'x' where id = 1", false, false, false, 0},
	} {
		st := e.gate.Inspect(tc.sql)
		if st.Observe != tc.observe || st.Track != tc.track || st.ReadOnly != tc.readOnly || st.KeyParam != tc.keyParam {
			t.Errorf("Inspect(%q): Observe %v Track %v ReadOnly %v KeyParam %d, want %v %v %v %d", tc.sql,
				st.Observe, st.Track, st.ReadOnly, st.KeyParam, tc.observe, tc.track, tc.readOnly, tc.keyParam)
		}
	}
}

// Each of the eight key types is tracked and looked up by its own text; a
// key written otherwise ('042') comes back as the server writes it. A domain,
// over another domain, over one of them is tracked as its base type.
func TestLookupKeyTypes(t *testing.T) {
	e := newStaleEnv(t)
	domain := e.name("code")
	mustExec(t, e.admin, "create domain "+domain+"_inner as text check (value <> 'x')")
	mustExec(t, e.admin, "create domain "+domain+" as "+domain+"_inner")
	t.Cleanup(func() {
		e.admin.Exec(context.Background(), "drop domain if exists "+domain+", "+domain+"_inner cascade")
	})

	type tc struct {
		sqlType string
		typ     proxy.KeyType
		rows    []string          // inserted, each in its own transaction
		ask     []string          // what the lookup is given
		want    map[string]string // the server's text of each key asked → the row it names
	}
	u1, u2 := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", "b1eebc99-9c0b-4ef8-bb6d-6bb9bd380a12"
	cases := []tc{
		{"int2", proxy.KeyInt2, []string{"42", "7"}, []string{"042"}, map[string]string{"42": "42"}},
		{"int4", proxy.KeyInt4, []string{"42", "7"}, []string{"042", "7"}, map[string]string{"42": "42", "7": "7"}},
		{"int8", proxy.KeyInt8, []string{"9000000000", "7"}, []string{"9000000000"}, map[string]string{"9000000000": "9000000000"}},
		{"uuid", proxy.KeyUUID, []string{u1, u2}, []string{u2}, map[string]string{u2: u2}},
		{"text", proxy.KeyText, []string{"ab", "cd"}, []string{"ab"}, map[string]string{"ab": "ab"}},
		{"varchar(10)", proxy.KeyText, []string{"ab", "cd"}, []string{"cd"}, map[string]string{"cd": "cd"}},
		{"char(5)", proxy.KeyText, []string{"ab", "cd"}, []string{"ab"}, map[string]string{"ab   ": "ab"}},
		{"name", proxy.KeyText, []string{"ab", "cd"}, []string{"ab"}, map[string]string{"ab": "ab"}},
		{domain, proxy.KeyText, []string{"ab", "cd"}, []string{"cd"}, map[string]string{"cd": "cd"}},
	}
	var blocks []string
	names := make([]string, len(cases))
	for i, c := range cases {
		names[i] = e.create(t, fmt.Sprintf("k%d", i), "(k "+c.sqlType+" primary key, title text)")
		e.insert(t, names[i], "k", c.rows...)
		blocks = append(blocks, tableBlock(names[i], "k", "stale: {}"))
	}
	e.start(t, blocks...)

	for i, c := range cases {
		table := names[i]
		oid, attr, typ, _, ok := gate.StaleTable(e.gate, table)
		if !ok || oid != e.oid(t, table) || attr != 1 || typ != c.typ {
			t.Errorf("%s: catalog oid %d attr %d type %d ok %v, want %d, 1, %d, true", c.sqlType, oid, attr, typ, ok, e.oid(t, table), c.typ)
		}
		if a, ty, ok := e.gate.Tracked(e.oid(t, table)); !ok || a != 1 || ty != c.typ {
			t.Errorf("%s: Tracked = %d, %d, %v; want 1, %d, true", c.sqlType, a, ty, ok, c.typ)
		}
		got, err := gate.LookupVersions(e.gate, table, c.ask)
		if err != nil {
			t.Errorf("%s: lookup: %v", c.sqlType, err)
			continue
		}
		want := map[string]string{}
		for server, row := range c.want {
			want[server] = e.xmin(t, table, "k", row)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: lookup of %q = %v, want %v", c.sqlType, c.ask, got, want)
		}
	}
	if _, _, _, typeName, _ := gate.StaleTable(e.gate, names[len(names)-1]); typeName != domain {
		t.Errorf("type name of the domain key = %q, want %q", typeName, domain)
	}
}

// Keys whose text an array literal could misread are each looked up as
// themselves, and each brings back its own row's version only.
func TestLookupKeepsOddKeysApart(t *testing.T) {
	e := newStaleEnv(t)
	texts := e.create(t, "texts", "(k text primary key)")
	ints := e.create(t, "ints", "(k int primary key)")
	uuids := e.create(t, "uuids", "(k uuid primary key)")
	odd := []string{`a,b`, `a"b`, `a\b`, `{a}`, ``, `NULL`, ` padded `}
	// What a broken literal would read them as instead.
	decoys := []string{`a`, `b`, `"`, `\`, `{}`, `null`, `padded`, `a"b"`}
	e.insert(t, texts, "k", append(slices.Clone(odd), decoys...)...)
	e.insert(t, ints, "k", "-5", "5")
	u := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	e.insert(t, uuids, "k", u, "b1eebc99-9c0b-4ef8-bb6d-6bb9bd380a12")
	e.start(t, tableBlock(texts, "k", "stale: {}"), tableBlock(ints, "k", "stale: {}"), tableBlock(uuids, "k", "stale: {}"))

	for _, c := range []struct {
		table string
		ask   []string
		want  map[string]string // the server's text → the key the row was inserted with
	}{
		{texts, odd, func() map[string]string {
			m := map[string]string{}
			for _, k := range odd {
				m[k] = k
			}
			return m
		}()},
		{ints, []string{"-5"}, map[string]string{"-5": "-5"}},
		{uuids, []string{strings.ToUpper(u)}, map[string]string{u: u}},
	} {
		got, err := gate.LookupVersions(e.gate, c.table, c.ask)
		if err != nil {
			t.Errorf("lookup of %q: %v", c.ask, err)
			continue
		}
		want := map[string]string{}
		for server, row := range c.want {
			want[server] = e.xmin(t, c.table, "k", row)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("lookup of %q = %q, want %q", c.ask, got, want)
		}
	}
}

// A table and key column whose names need quoting, and a config key that
// names a schema.
func TestLookupQuotesNames(t *testing.T) {
	e := newStaleEnv(t)
	schema := e.schema(t, "crm")
	contacts := schema + ".contacts"
	mustExec(t, e.admin, "create table "+quoted(contacts)+" (id int primary key, name text)")
	notes := e.create(t, "Deal Notes", `("Order" int primary key, body text)`)
	e.insert(t, contacts, "id", "1", "2")
	e.insert(t, notes, "Order", "10", "20")
	e.start(t, tableBlock(contacts, "id", "stale: {}"), tableBlock(notes, "Order", "stale: {}"))

	for _, c := range []struct{ table, col, key string }{{contacts, "id", "2"}, {notes, "Order", "10"}} {
		if _, _, ok := e.gate.Tracked(e.oid(t, c.table)); !ok {
			t.Errorf("%s is not tracked", c.table)
		}
		got, err := gate.LookupVersions(e.gate, c.table, []string{c.key})
		if err != nil {
			t.Errorf("%s: lookup: %v", c.table, err)
			continue
		}
		if want := map[string]string{c.key: e.xmin(t, c.table, c.col, c.key)}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: lookup = %v, want %v", c.table, got, want)
		}
	}
}

// blockLookups makes g's lookups wait until release is called or their time
// is up. release is called when the test ends too, before the gate closes.
func blockLookups(t *testing.T, g *gate.Gate) (release func()) {
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	gate.OnLookup(g, func(ctx context.Context, table string, keys []string) error {
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	return release
}

func be32(v int32) []byte { return binary.BigEndian.AppendUint32(nil, uint32(v)) }
func be64(v int64) []byte { return binary.BigEndian.AppendUint64(nil, uint64(v)) }

// A read that selects one row by `key = value` and sends it, but whose result
// has no key column, remembers the row the value names, in whatever form the
// value came. Until its lookup ends, every row of the table is pending for
// the role.
func TestWhereKeyFallback(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	bigs := e.create(t, "bigs", "(id int8 primary key, title text)")
	uuids := e.create(t, "uuids", "(id uuid primary key, title text)")
	texts := e.create(t, "texts", "(id text primary key, title text)")
	e.insert(t, deals, "id", "1", "7", "42")
	e.insert(t, bigs, "id", "9000000000")
	u := "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	e.insert(t, uuids, "id", u)
	e.insert(t, texts, "id", "ab", "ab  ")
	e.start(t, tableBlock(deals, "id", "stale: {}"), tableBlock(bigs, "id", "stale: {}"),
		tableBlock(uuids, "id", "stale: {}"), tableBlock(texts, "id", "stale: {}"))
	byParam := func(table string) proxy.Statement {
		st := e.gate.Inspect("select title from " + table + " where id = $1")
		if st.KeyParam != 1 || !st.Track {
			t.Fatalf("Inspect: KeyParam %d Track %v, want 1 and true", st.KeyParam, st.Track)
		}
		return st
	}
	session := func(role string) proxy.SessionInfo { return proxy.SessionInfo{ID: role, User: role} }

	t.Run("pending until looked up", func(t *testing.T) {
		release := blockLookups(t, e.gate)
		e.gate.Observe(session("waiting"), byParam(deals), whereKeyed(&proxy.Param{Value: []byte("042")}, 1, e.oid(t, deals)))
		for _, k := range []string{"1", "42", "99"} {
			e.wantResult(t, "waiting", "", deals, k, "1", decide.ReadPending)
		}
		e.wantResult(t, "waiting", "", bigs, "9000000000", "1", decide.ReadNone)
		release()
		e.idle(t)
		e.wantResult(t, "waiting", "", deals, "42", e.xmin(t, deals, "id", "42"), decide.ReadFresh)
		e.wantResult(t, "waiting", "", deals, "1", e.xmin(t, deals, "id", "1"), decide.ReadNone)
	})

	for _, c := range []struct {
		name, table, key string // key: the row's id as the server writes it
		st               proxy.Statement
		param            *proxy.Param
	}{
		{"literal", deals, "7", e.gate.Inspect("select title from " + deals + " where id = 7"), nil},
		{"text", deals, "42", byParam(deals), &proxy.Param{Value: []byte("42")}},
		{"binary int4", deals, "42", byParam(deals), &proxy.Param{Value: be32(42), Format: 1}},
		{"binary int4 typed", deals, "42", byParam(deals), &proxy.Param{Value: be32(42), Format: 1, OID: 23}},
		{"binary int8", bigs, "9000000000", byParam(bigs), &proxy.Param{Value: be64(9000000000), Format: 1}},
		{"binary int8 typed", deals, "7", byParam(deals), &proxy.Param{Value: be64(7), Format: 1, OID: 20}},
		{"binary uuid", uuids, u, byParam(uuids), &proxy.Param{Value: []byte{0xa0, 0xee, 0xbc, 0x99, 0x9c, 0x0b, 0x4e, 0xf8, 0xbb, 0x6d, 0x6b, 0xb9, 0xbd, 0x38, 0x0a, 0x11}, Format: 1}},
		{"binary text", texts, "ab", byParam(texts), &proxy.Param{Value: []byte("ab"), Format: 1, OID: 25}},
		{"binary varchar on a text key", texts, "ab  ", byParam(texts), &proxy.Param{Value: []byte("ab  "), Format: 1, OID: 1043}},
		{"text int8 typed", deals, "42", byParam(deals), &proxy.Param{Value: []byte("042"), OID: 20}},
	} {
		t.Run(c.name, func(t *testing.T) {
			role := strings.ReplaceAll(c.name, " ", "_")
			e.gate.Observe(session(role), c.st, whereKeyed(c.param, 1, e.oid(t, c.table)))
			e.idle(t)
			e.wantResult(t, role, "", c.table, c.key, e.xmin(t, c.table, "id", c.key), decide.ReadFresh)
		})
	}

	t.Run("not remembered", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			o     proxy.Outcome
			table string
			key   string
		}{
			{"no row sent", whereKeyed(&proxy.Param{Value: []byte("42")}, 0, e.oid(t, deals)), deals, "42"},
			{"binary of a type not read", whereKeyed(&proxy.Param{Value: be32(42), Format: 1, OID: 1700}, 1, e.oid(t, deals)), deals, "42"},
			{"null", whereKeyed(&proxy.Param{Format: 0}, 1, e.oid(t, deals)), deals, "42"},
			{"not ASCII", whereKeyed(&proxy.Param{Value: []byte("ab\xc3\xa9")}, 1, e.oid(t, texts)), texts, "ab"},
			{"no param", whereKeyed(nil, 1, e.oid(t, deals)), deals, "42"},
			// Compared as text, 'ab  '::bpchar is 'ab': the read named the row
			// ab, and a lookup of its text would find the row 'ab  '.
			{"binary bpchar on a text key", whereKeyed(&proxy.Param{Value: []byte("ab  "), Format: 1, OID: 1042}, 1, e.oid(t, texts)), texts, "ab  "},
			{"text bpchar on a text key", whereKeyed(&proxy.Param{Value: []byte("ab  "), OID: 1042}, 1, e.oid(t, texts)), texts, "ab  "},
			{"name on a text key", whereKeyed(&proxy.Param{Value: []byte("ab"), Format: 1, OID: 19}, 1, e.oid(t, texts)), texts, "ab"},
		} {
			role := "none_" + strings.ReplaceAll(c.name, " ", "_")
			e.gate.Observe(session(role), byParam(c.table), c.o)
			e.wantResult(t, role, "", c.table, c.key, "1", decide.ReadNone) // not even pending
			e.idle(t)
			e.wantResult(t, role, "", c.table, c.key, e.xmin(t, c.table, "id", c.key), decide.ReadNone)
		}
	})

	t.Run("keys of the result come first", func(t *testing.T) {
		o := keyed(e.oid(t, deals), "1")
		o.Read.Param = &proxy.Param{Value: []byte("42")}
		e.gate.Observe(session("both"), byParam(deals), o)
		e.wantResult(t, "both", "", deals, "42", "1", decide.ReadNone) // only row 1 is pending
		e.idle(t)
		e.wantResult(t, "both", "", deals, "1", e.xmin(t, deals, "id", "1"), decide.ReadFresh)
		e.wantResult(t, "both", "", deals, "42", e.xmin(t, deals, "id", "42"), decide.ReadNone)
	})
}

// A read of a table of the same name in another schema matches the config
// key by name, but its result has no column of the tracked table, so the
// WHERE clause's key is not taken to name the tracked table's row (S5).
func TestSameNameInAnotherSchemaIsNotRemembered(t *testing.T) {
	e := newStaleEnv(t)
	notes := e.create(t, "notes", "(id int primary key, title text)")
	other := e.schema(t, "other")
	mustExec(t, e.admin, "create table "+other+"."+notes+" (id int primary key, title text)")
	e.insert(t, notes, "id", "1")
	e.insert(t, other+"."+notes, "id", "1")
	e.start(t, tableBlock(notes, "id", "stale: {}"))

	st := e.gate.Inspect("select title from " + other + "." + notes + " where id = $1")
	if !st.Track || st.KeyParam != 1 {
		t.Fatalf("Inspect: Track %v KeyParam %d, want the read tracked by $1", st.Track, st.KeyParam)
	}
	// The proxy found no column of a tracked table: Tables is empty.
	e.gate.Observe(proxy.SessionInfo{ID: "s", User: "agent"}, st, whereKeyed(&proxy.Param{Value: []byte("1")}, 1))
	e.wantResult(t, "agent", "", notes, "1", "1", decide.ReadNone)
	e.idle(t)
	e.wantResult(t, "agent", "", notes, "1", e.xmin(t, notes, "id", "1"), decide.ReadNone)
}

func TestReadsNotRemembered(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	e.insert(t, deals, "id", "1")
	e.start(t, tableBlock(deals, "id", "stale: {}"))
	oid := e.oid(t, deals)
	st := e.gate.Inspect("select id, title from " + deals)

	failed := keyed(oid, "1")
	failed.Executed, failed.Error = false, "22012: division by zero"
	errored := keyed(oid, "1")
	errored.Error = "57014: canceling statement due to user request"
	lost := keyed(oid, "1")
	lost.Executed, lost.Lost, lost.Error = false, true, "connection closed"
	noRead := keyed(oid, "1")
	noRead.Read = nil
	for _, c := range []struct {
		name string
		s    proxy.SessionInfo
		o    proxy.Outcome
	}{
		{"no keys taken", proxy.SessionInfo{User: "agent"}, noRead},
		{"failed", proxy.SessionInfo{User: "agent"}, failed},
		{"errored", proxy.SessionInfo{User: "agent"}, errored},
		{"lost", proxy.SessionInfo{User: "agent"}, lost},
		{"no login role", proxy.SessionInfo{}, keyed(oid, "1")},
	} {
		e.gate.Observe(c.s, st, c.o)
		e.wantResult(t, c.s.User, "", deals, "1", "1", decide.ReadNone)
		e.idle(t)
		if got := e.check(c.s.User, "", deals, "1", e.xmin(t, deals, "id", "1")); got.Result != decide.ReadNone {
			t.Errorf("%s: %+v, want no_read", c.name, got)
		}
	}
}

// A table with stale that the catalog cannot track is logged once, by name
// and why; New does not fail; its reads are counted and nothing is
// remembered of them.
func TestUntrackedTables(t *testing.T) {
	e := newStaleEnv(t)
	missing := e.name("missing")
	nocol := e.create(t, "nocol", "(id int primary key)")
	dated := e.create(t, "dated", "(day date primary key)")
	pair := e.create(t, "pair", "(region int, id int, primary key (region, id))")
	pairs := e.create(t, "pairs", "(region int, id int, primary key (region, id))")
	good := e.create(t, "good", "(id int primary key)")
	e.insert(t, dated, "day", "2026-11-20")
	untracked := []string{missing, nocol, dated, pair, pairs}
	e.start(t, tableBlock(missing, "id", "stale: {}"), tableBlock(nocol, "nope", "stale: {}"),
		tableBlock(dated, "day", "stale: {}"), tableBlock(pair, "id", "stale: {}"),
		tableBlock(pairs, "region, id", "stale: {}"), tableBlock(good, "id", "stale: {}"))
	t.Cleanup(func() {
		e.admin.Exec(context.Background(), "delete from stet.reads where target_table = any($1)", untracked)
	})

	for _, table := range untracked {
		w := e.warnings(t, table)
		if len(w) != 1 || w[0]["reason"] == nil || w[0]["reason"] == "" {
			t.Errorf("%s: warnings %v, want one that says why", table, w)
		}
		if table == missing {
			continue
		}
		if _, _, ok := e.gate.Tracked(e.oid(t, table)); ok {
			t.Errorf("%s is tracked", table)
		}
	}
	if w := e.warnings(t, good); len(w) != 0 {
		t.Errorf("%s: warnings %v, want none", good, w)
	}
	if _, _, ok := e.gate.Tracked(e.oid(t, good)); !ok {
		t.Errorf("%s is not tracked", good)
	}

	st := e.gate.Inspect("select day from " + dated)
	if !st.Observe || !st.Track {
		t.Errorf("a read of %s: Observe %v Track %v, want both: the proxy asks Tracked", dated, st.Observe, st.Track)
	}
	e.gate.Observe(proxy.SessionInfo{User: "agent"}, st, keyed(e.oid(t, dated), "2026-11-20"))
	e.wantResult(t, "agent", "", dated, "2026-11-20", "1", decide.ReadNone)
	e.gate.FlushReads()
	if got := e.keyedReads(t, dated); !reflect.DeepEqual(got, map[string]keyCount{"agent": {Statements: 1}}) {
		t.Errorf("stet.reads of %s = %+v, want one statement of agent with no key", dated, got)
	}
}

// The catalog is read again every staleCatalogEvery: a table dropped and
// made again is tracked by its new OID, one made after the start is tracked
// once it is there, and one whose key type leaves the list is not tracked any
// more. Reads and writes meanwhile do not fail.
func TestCatalogIsReadAgain(t *testing.T) {
	defer gate.SetStaleCatalogEvery(50 * time.Millisecond)()
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	later := e.name("later")
	t.Cleanup(func() { e.admin.Exec(context.Background(), "drop table if exists "+later) })
	e.insert(t, deals, "id", "1")
	e.start(t, tableBlock(deals, "id", "stale: {}"), tableBlock(later, "id", "stale: {}"))
	tracked := func(oid uint32) bool { _, _, ok := e.gate.Tracked(oid); return ok }

	first := e.oid(t, deals)
	if !tracked(first) {
		t.Fatalf("%s is not tracked", deals)
	}
	// In one transaction, so no reading of the catalog finds it missing.
	mustExec(t, e.admin, "drop table "+deals+"; create table "+deals+" (id int primary key, title text); "+
		"insert into "+deals+" values (1)")
	second := e.oid(t, deals)

	// Meanwhile a read of the old table and a write to the new one.
	agent := proxy.SessionInfo{ID: "s", User: "agent"}
	e.gate.Observe(agent, e.gate.Inspect("select id from "+deals), keyed(first, "1"))
	h, refuse := e.gate.Begin(agent, e.gate.Inspect("update "+deals+" set title = 'x' where id = 1"), nil)
	if refuse != nil {
		t.Fatalf("the write was refused: %+v", refuse)
	}
	e.gate.End(h, proxy.Outcome{Executed: true, Rows: ptr(int64(1))})
	e.idle(t)

	waitUntil(t, "the new table is tracked", func() bool { return tracked(second) && !tracked(first) })
	mustExec(t, e.admin, "create table "+later+" (id int primary key)")
	waitUntil(t, "a table made after the start is tracked", func() bool { return tracked(e.oid(t, later)) })
	if w := e.warnings(t, later); len(w) != 1 {
		t.Errorf("%s: warnings %v, want the one from the start", later, w)
	}
	mustExec(t, e.admin, "alter table "+deals+" alter column id type numeric")
	waitUntil(t, "a key of type numeric is not tracked", func() bool { return !tracked(second) })
	waitUntil(t, "it is logged", func() bool { return len(e.warnings(t, deals)) == 1 })
}

// While lookups cannot run, a read is observed at once; once the queue is
// full, reads are dropped, counted and logged. A lookup past its time is
// dropped: nothing is remembered of the read, and it is logged.
func TestSlowLookupDelaysNothing(t *testing.T) {
	t.Run("a full queue drops reads", func(t *testing.T) {
		e := newStaleEnv(t)
		deals := e.create(t, "deals", "(id int primary key)")
		e.start(t, tableBlock(deals, "id", "stale: {}"))
		release := blockLookups(t, e.gate)
		oid := e.oid(t, deals)
		st := e.gate.Inspect("select id from " + deals)
		agent := proxy.SessionInfo{User: "agent"}

		const reads = 1100
		var took []time.Duration
		dropped := 0
		for i := range reads {
			key := strconv.Itoa(i)
			start := time.Now()
			e.gate.Observe(agent, st, keyed(oid, key))
			took = append(took, time.Since(start))
			switch got := e.check("agent", "", deals, key, "1"); got.Result {
			case decide.ReadPending:
			case decide.ReadNone: // ended at once: the queue was full
				dropped++
			default:
				t.Fatalf("read %d: %+v, want read_pending or no_read", i, got)
			}
		}
		slices.Sort(took)
		if p95 := took[len(took)*95/100]; p95 > time.Millisecond {
			t.Errorf("Observe took %v at p95 while lookups were held, want at most 1ms", p95)
		}
		if dropped == 0 || dropped > reads-1024 {
			t.Errorf("%d of %d reads dropped, want some and at most %d", dropped, reads, reads-1024)
		}
		release()
		e.idle(t)
		e.gate.Close()
		var lines []map[string]any
		for _, rec := range logRecords(t, &e.logs) {
			if rec["queue_full"] != nil {
				lines = append(lines, rec)
			}
		}
		if len(lines) != 1 || lines[0]["level"] != "WARN" || lines[0]["queue_full"] != float64(dropped) {
			t.Errorf("log lines counting dropped reads %v, want one WARN with queue_full=%d", lines, dropped)
		}
	})

	t.Run("a lookup past its time is dropped", func(t *testing.T) {
		e := newStaleEnv(t)
		deals := e.create(t, "deals", "(id int primary key)")
		e.insert(t, deals, "id", "1")
		e.start(t, tableBlock(deals, "id", "stale: {}"))
		gate.OnLookup(e.gate, func(ctx context.Context, table string, keys []string) error {
			<-ctx.Done()
			return ctx.Err()
		})
		e.gate.Observe(proxy.SessionInfo{User: "agent"}, e.gate.Inspect("select id from "+deals), keyed(e.oid(t, deals), "1"))
		e.wantResult(t, "agent", "", deals, "1", "1", decide.ReadPending)
		start := time.Now()
		e.idle(t)
		if took := time.Since(start); took < 400*time.Millisecond || took > 2*time.Second {
			t.Errorf("the lookup ended after %v, want about 500ms", took)
		}
		e.wantResult(t, "agent", "", deals, "1", e.xmin(t, deals, "id", "1"), decide.ReadNone)
		e.gate.Close()
		var lines []map[string]any
		for _, rec := range logRecords(t, &e.logs) {
			if rec["lookups_failed"] != nil {
				lines = append(lines, rec)
			}
		}
		if len(lines) != 1 || lines[0]["lookups_failed"] != float64(1) || lines[0]["queue_full"] != float64(0) {
			t.Errorf("log lines counting failed lookups %v, want one with lookups_failed=1", lines)
		}
	})
}

// keyCount is a role's reads of a table in stet.reads, summed over minutes.
type keyCount struct{ Statements, Keyed, Keys int64 }

func (e *staleEnv) keyedReads(t *testing.T, table string) map[string]keyCount {
	t.Helper()
	rows, err := e.admin.Query(context.Background(), `
		select login_role, sum(statements)::int8, sum(keyed)::int8, sum(keys)::int8 from stet.reads
		where target_table = $1 group by login_role`, table)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]keyCount{}
	for rows.Next() {
		var role string
		var c keyCount
		if err := rows.Scan(&role, &c.Statements, &c.Keyed, &c.Keys); err != nil {
			t.Fatal(err)
		}
		out[role] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// stet.reads counts, for each table read, the reads that gave a key of it
// (keyed) and the keys they gave, each once per read; a table with stale but
// no reads clause is counted too.
func TestReadsCountKeys(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	notes := e.create(t, "notes", "(id int primary key, title text)")
	e.insert(t, notes, "id", "5")
	e.start(t, tableBlock(deals, "id", "stale: {}", "reads: {}"), tableBlock(notes, "id", "stale: {}"))
	t.Cleanup(func() {
		e.admin.Exec(context.Background(), "delete from stet.reads where target_table = any($1)", []string{deals, notes})
	})
	agent := proxy.SessionInfo{User: "agent"}
	deal, note := e.oid(t, deals), e.oid(t, notes)

	all := e.gate.Inspect("select id, title from " + deals)
	e.gate.Observe(agent, all, keyed(deal, "1", "2", "3"))
	e.gate.Observe(agent, all, keyed(deal, "4", "4")) // a self-join gives the row twice
	noKey := keyed(deal)
	noKey.Sent = 2
	e.gate.Observe(agent, e.gate.Inspect("select title from "+deals), noKey)
	join := e.gate.Inspect("select d.id, n.id from " + deals + " d join " + notes + " n on n.id = d.id")
	both := keyed(deal, "7")
	both.Read.Keys = append(both.Read.Keys, proxy.RowKey{Table: note, Text: "7"})
	both.Read.Tables = append(both.Read.Tables, note)
	e.gate.Observe(agent, join, both)
	e.gate.Observe(agent, e.gate.Inspect("select title from "+notes+" where id = $1"),
		whereKeyed(&proxy.Param{Value: []byte("5")}, 1, note))
	e.idle(t)
	e.gate.FlushReads()

	if got, want := e.keyedReads(t, deals), map[string]keyCount{"agent": {Statements: 4, Keyed: 3, Keys: 5}}; !reflect.DeepEqual(got, want) {
		t.Errorf("stet.reads of %s = %+v, want %+v", deals, got, want)
	}
	if got, want := e.keyedReads(t, notes), map[string]keyCount{"agent": {Statements: 2, Keyed: 2, Keys: 2}}; !reflect.DeepEqual(got, want) {
		t.Errorf("stet.reads of %s = %+v, want %+v", notes, got, want)
	}
}

// After Close, Observe neither panics nor waits, and the memory is unchanged.
func TestObserveAfterClose(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key)")
	e.insert(t, deals, "id", "1")
	e.start(t, tableBlock(deals, "id", "stale: {}"))
	st := e.gate.Inspect("select id from " + deals)
	e.gate.Close()

	start := time.Now()
	e.gate.Observe(proxy.SessionInfo{User: "agent"}, st, keyed(e.oid(t, deals), "1"))
	e.wantResult(t, "agent", "", deals, "1", "1", decide.ReadNone)
	e.idle(t)
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("Observe after Close took %v", took)
	}
	e.wantResult(t, "agent", "", deals, "1", e.xmin(t, deals, "id", "1"), decide.ReadNone)
}

// A refresh looks up one row's version by its key as a write wrote it, and
// answers with the server's text of the key; a row that is not there, or a
// refresh after Close, answers not ok.
func TestRefreshLooksUpOneRow(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key)")
	e.insert(t, deals, "id", "42")
	e.start(t, tableBlock(deals, "id", "stale: {}"))

	if key, v, ok := gate.Refresh(e.gate, deals, "042"); !ok || key != "42" || v != e.xmin(t, deals, "id", "42") {
		t.Errorf("refresh of 042 = %q, %q, %v; want 42, its xmin, true", key, v, ok)
	}
	if key, v, ok := gate.Refresh(e.gate, deals, "43"); ok {
		t.Errorf("refresh of a row not there = %q, %q, true", key, v)
	}
	e.gate.Close()
	if _, _, ok := gate.Refresh(e.gate, deals, "42"); ok {
		t.Error("refresh after Close answered ok")
	}
}

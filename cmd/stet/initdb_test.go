package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/pgtest"
)

// initPassword is the password in the addresses chooseSSLMode is given: no
// error or notice may hold it.
const initPassword = "s3cr3t-pw"

// fakeConnect stands in for stet init's connection: it answers with errs in
// turn (nil past their end) and records the addresses it was given.
type fakeConnect struct {
	errs  []error
	addrs []string
}

func (f *fakeConnect) connect(_ context.Context, addr string) error {
	f.addrs = append(f.addrs, addr)
	if n := len(f.addrs); n <= len(f.errs) {
		return f.errs[n-1]
	}
	return nil
}

func TestChooseSSLMode(t *testing.T) {
	const (
		remote   = "postgres://app:" + initPassword + "@db.example.com:5432/crm"
		loopback = "postgresql://app:" + initPassword + "@localhost/crm"
		notURL   = "the database address must start with postgres:// or postgresql://"
		noTLS    = "the server does not accept TLS; to connect in plaintext anyway, write sslmode=disable in the address yourself"
		pooler   = "\nif this address goes through a connection pooler, use the direct connection address instead"
		allow    = "sslmode=allow is not supported: it connects in plaintext first, which a relayed session cannot retry; use disable, prefer, require, verify-ca or verify-full"
	)
	// What pgconn says, behind openPool's prefix; it never holds the password.
	failed := func(err error) error {
		return fmt.Errorf("connect to upstream as Stet: failed to connect to `user=app database=crm`: db.example.com:5432 (10.0.0.5): tls error: %w", err)
	}
	refused := failed(errors.New("server refused TLS connection"))
	denied := errors.New("connect to upstream as Stet: failed to connect to `user=app database=crm`: db.example.com:5432 (10.0.0.5): server error: FATAL: password authentication failed for user \"app\" (SQLSTATE 28P01)")
	notice := func(certErr string) string {
		return "the server's certificate could not be checked (" + certErr + "), so upstream uses sslmode=require: " +
			"the connection is encrypted, but the server is not verified. To verify it, add sslrootcert=<the service's CA file> " +
			"and sslmode=verify-full to upstream in the config file."
	}
	unknownAuthority := x509.UnknownAuthorityError{}
	wrongHost := x509.HostnameError{Certificate: &x509.Certificate{DNSNames: []string{"other.example.com"}}, Host: "db.example.com"}
	expired := x509.CertificateInvalidError{Reason: x509.Expired, Detail: "current time is after 2026-01-01"}
	unverified := &tls.CertificateVerificationError{Err: errors.New("x509: “db.example.com” certificate is not trusted")}

	for _, tc := range []struct {
		name   string
		raw    string
		errs   []error // what connect answers, in turn
		want   string  // the upstream address
		notice string
		err    string   // the whole error; "" for none
		addrs  []string // what connect was given, in turn
	}{
		{name: "keyword/value", raw: "host=db.example.com user=app password=" + initPassword, err: notURL},
		{name: "other scheme", raw: "mysql://app:" + initPassword + "@db.example.com/crm", err: notURL},
		{name: "no scheme", raw: "app:" + initPassword + "@db.example.com:5432/crm", err: notURL},
		{name: "not a URL", raw: "postgres://app:" + initPassword + "@db.example.com:port/crm",
			err: "the database address is not a valid URL; percent-encode any @, :, /, ?, # or % in the user name and password"},
		{name: "allow", raw: remote + "?sslmode=allow", err: allow},
		{name: "allow written last", raw: remote + "?sslmode=require&sslmode=allow", err: allow},

		{name: "written sslmode kept", raw: remote + "?sslmode=require",
			want: remote + "?sslmode=require", addrs: []string{remote + "?sslmode=require"}},
		{name: "remote plaintext written", raw: remote + "?application_name=a&sslmode=disable",
			want: remote + "?application_name=a&sslmode=disable", addrs: []string{remote + "?application_name=a&sslmode=disable"}},
		{name: "written verify-full is not lowered", raw: remote + "?sslmode=verify-full", errs: []error{failed(unknownAuthority)},
			err: failed(unknownAuthority).Error() + pooler, addrs: []string{remote + "?sslmode=verify-full"}},
		{name: "written require refused", raw: remote + "?sslmode=require", errs: []error{refused},
			err: noTLS, addrs: []string{remote + "?sslmode=require"}},

		{name: "loopback as it is", raw: loopback, want: loopback, addrs: []string{loopback}},
		{name: "loopback IPv6", raw: "postgres://app@[::1]:5432/crm", want: "postgres://app@[::1]:5432/crm",
			addrs: []string{"postgres://app@[::1]:5432/crm"}},
		{name: "loopback fails", raw: loopback, errs: []error{denied}, err: denied.Error() + pooler, addrs: []string{loopback}},

		{name: "remote verify-full", raw: remote,
			want: remote + "?sslmode=verify-full", addrs: []string{remote + "?sslmode=verify-full"}},
		{name: "remote with a query", raw: remote + "?application_name=a%20b",
			want:  remote + "?application_name=a%20b&sslmode=verify-full",
			addrs: []string{remote + "?application_name=a%20b&sslmode=verify-full"}},
		{name: "remote with an empty query", raw: remote + "?",
			want: remote + "?sslmode=verify-full", addrs: []string{remote + "?sslmode=verify-full"}},
		{name: "unknown authority", raw: remote, errs: []error{failed(unknownAuthority)},
			want: remote + "?sslmode=require", notice: notice(unknownAuthority.Error()),
			addrs: []string{remote + "?sslmode=verify-full", remote + "?sslmode=require"}},
		{name: "wrong host", raw: remote, errs: []error{failed(wrongHost)},
			want: remote + "?sslmode=require", notice: notice("x509: certificate is valid for other.example.com, not db.example.com"),
			addrs: []string{remote + "?sslmode=verify-full", remote + "?sslmode=require"}},
		{name: "expired", raw: remote + "?application_name=a", errs: []error{failed(expired)},
			want: remote + "?application_name=a&sslmode=require", notice: notice(expired.Error()),
			addrs: []string{remote + "?application_name=a&sslmode=verify-full", remote + "?application_name=a&sslmode=require"}},
		{name: "certificate not verified", raw: remote, errs: []error{failed(unverified)},
			want: remote + "?sslmode=require", notice: notice(unverified.Error()),
			addrs: []string{remote + "?sslmode=verify-full", remote + "?sslmode=require"}},
		{name: "require fails too", raw: remote, errs: []error{failed(unknownAuthority), denied},
			err: denied.Error() + pooler, addrs: []string{remote + "?sslmode=verify-full", remote + "?sslmode=require"}},
		{name: "remote refuses TLS", raw: remote, errs: []error{refused},
			err: noTLS, addrs: []string{remote + "?sslmode=verify-full"}},
		{name: "remote fails", raw: remote, errs: []error{denied},
			err: denied.Error() + pooler, addrs: []string{remote + "?sslmode=verify-full"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeConnect{errs: tc.errs}
			upstream, notice, err := chooseSSLMode(context.Background(), tc.raw, f.connect)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				if upstream != "" || notice != "" {
					t.Errorf("with an error: upstream %q, notice %q; want neither", upstream, notice)
				}
			} else if err != nil {
				t.Fatalf("err = %v, want none", err)
			}
			if upstream != tc.want {
				t.Errorf("upstream = %q, want %q", upstream, tc.want)
			}
			if notice != tc.notice {
				t.Errorf("notice = %q, want %q", notice, tc.notice)
			}
			if !slices.Equal(f.addrs, tc.addrs) {
				t.Errorf("connected with %q, want %q", f.addrs, tc.addrs)
			}
			if err != nil && strings.Contains(err.Error(), initPassword) || strings.Contains(notice, initPassword) {
				t.Errorf("the error %v or the notice %q holds the password", err, notice)
			}
		})
	}
}

// Whether the server is another machine is remoteHost's answer for the
// address's host, without its port.
func TestChooseSSLModeAsksRemoteHost(t *testing.T) {
	var asked []string
	defer setRemoteHost(func(host string) bool { asked = append(asked, host); return true })()
	f := &fakeConnect{}
	upstream, _, err := chooseSSLMode(context.Background(), "postgres://app@127.0.0.1:5432/crm", f.connect)
	if err != nil || upstream != "postgres://app@127.0.0.1:5432/crm?sslmode=verify-full" {
		t.Errorf("upstream %q, err %v; want verify-full added for a host taken for a remote one", upstream, err)
	}
	if !slices.Equal(asked, []string{"127.0.0.1"}) {
		t.Errorf("remoteHost was asked %q, want [127.0.0.1]", asked)
	}
}

// stet init refuses sslmode=allow as the config does.
func TestChooseSSLModeRefusesAllowAsTheConfigDoes(t *testing.T) {
	_, _, err := chooseSSLMode(context.Background(), "postgres://app@db.example.com/crm?sslmode=allow", (&fakeConnect{}).connect)
	_, cfgErr := config.Parse([]byte("mode: shadow\nlisten: 127.0.0.1:6432\nupstream: postgres://app@db.example.com/crm?sslmode=allow\n"))
	if err == nil || cfgErr == nil || !strings.HasSuffix(cfgErr.Error(), err.Error()) {
		t.Errorf("chooseSSLMode: %v; the config: %v; want the config's error to end in chooseSSLMode's", err, cfgErr)
	}
}

// setRemoteHost replaces remoteHost until restore is called.
func setRemoteHost(f func(string) bool) (restore func()) {
	old := remoteHost
	remoteHost = f
	return func() { remoteHost = old }
}

// connectOnce connects to addr as stet proxy does, and closes.
func connectOnce(ctx context.Context, addr string) error {
	pool, err := openPool(ctx, addr)
	if err != nil {
		return err
	}
	pool.Close()
	return nil
}

func TestChooseSSLModeAgainstTheTestServer(t *testing.T) {
	pg := testPG(t)
	_, certPath := pgtest.ServerCert(t, pg)
	ctx := context.Background()
	raw := "postgres://stet_scram:scram-pw@" + pg + "/stet_test"

	t.Run("loopback", func(t *testing.T) {
		upstream, notice, err := chooseSSLMode(ctx, raw, connectOnce)
		if err != nil || upstream != raw || notice != "" {
			t.Errorf("upstream %q, notice %q, err %v; want the address as it is", upstream, notice, err)
		}
	})
	t.Run("taken for remote", func(t *testing.T) {
		defer setRemoteHost(func(string) bool { return true })()
		upstream, notice, err := chooseSSLMode(ctx, raw, connectOnce)
		if err != nil {
			t.Fatal(err)
		}
		if upstream != raw+"?sslmode=require" {
			t.Errorf("upstream = %q, want sslmode=require: the test server's certificate is in no system root", upstream)
		}
		// What the system's verifier says differs by platform: macOS, for one,
		// finds the test certificate "not standards compliant".
		if !strings.HasPrefix(notice, "the server's certificate could not be checked (") ||
			!strings.Contains(notice, "x509: ") || !strings.Contains(notice, "), so upstream uses sslmode=require: ") {
			t.Errorf("notice = %q, want the one that says why and what it settled for", notice)
		}
	})
	t.Run("verify-full written", func(t *testing.T) {
		defer setRemoteHost(func(string) bool { return true })()
		verified := raw + "?" + url.Values{"sslmode": {"verify-full"}, "sslrootcert": {certPath}}.Encode()
		upstream, notice, err := chooseSSLMode(ctx, verified, connectOnce)
		if err != nil || upstream != verified || notice != "" {
			t.Errorf("upstream %q, notice %q, err %v; want the address as it is", upstream, notice, err)
		}
	})
}

// connectError knows a server that refuses TLS by pgconn's words for it.
// Here a real listener answers the TLS request with N, and stet init's own
// connection (openPool, through chooseSSLMode) meets it with sslmode=require:
// a rewording in pgx fails this test instead of turning the advice into the
// raw error.
func TestChooseSSLModeKnowsAServerThatRefusesTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				var req [8]byte // the SSLRequest
				if _, err := io.ReadFull(c, req[:]); err == nil {
					c.Write([]byte{'N'})
				}
			}()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw := "postgres://app:" + initPassword + "@" + ln.Addr().String() + "/crm?sslmode=require"
	_, _, err = chooseSSLMode(ctx, raw, connectOnce)
	if err == nil || err.Error() != errNoTLS {
		t.Errorf("err = %v, want %q", err, errNoTLS)
	}
}

// initPool is Stet's pool (openPool) in database db as user, without the
// stet schema.
func initPool(t *testing.T, pg, db, user, password string) *pgxpool.Pool {
	t.Helper()
	u := url.URL{Scheme: "postgres", Host: pg, Path: "/" + db, User: url.UserPassword(user, password), RawQuery: "sslmode=disable"}
	pool, err := openPool(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// withoutRows is ts with no row estimates, which a test that did not
// analyze cannot know.
func withoutRows(ts []initTable) []initTable {
	ts = slices.Clone(ts)
	for i := range ts {
		ts[i].Rows = 0
	}
	return ts
}

func TestReadInitFacts(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	su := superuser(t, pg)
	role := createRole(t, su, "init", "login password 'init-pw'") // before the database: see cleanDatabase
	db := cleanDatabase(t, pg)                                    // owned by stet_scram, not by role
	in := connectTo(t, pg, db, "postgres", "postgres-pw")
	for _, sql := range []string{
		"create table ints (id int primary key)",
		"create table dated (day date primary key)",
		"create domain small_id as int4",
		"create domain item_id as small_id",
		"create table items (id item_id primary key)",
		"create table pairs (a int, b int, primary key (a, b))",
		"create table nokey (a int)",
		"create table hidden (id int primary key)", // postgres's, and role may not read it
		`create table "Mixed" (id int primary key)`,
		`create table oddkey ("Id" int primary key)`,
		"create schema elsewhere",
		"create table elsewhere.away (id int primary key)",
		// Each fails more than one rule, and is counted under the first.
		`create table elsewhere."Far" (a int)`,                         // all but the key column's name
		"create table hidden_pairs (a int, b int, primary key (a, b))", // the key and readability
		"create schema " + role + " authorization " + role,             // "$user", first on the default search_path
		"create table " + role + ".dup (id int primary key)",
		"create table public.dup (id int primary key)",
		"create table events (id int primary key) partition by range (id)",
		"create table events_low partition of events for values from (0) to (100)",
		"create view ints_view as select * from ints",
		"create materialized view ints_mat as select * from ints",
		"create sequence counter",
		`create table "user" (id int primary key)`,
		"create temporary table scratch (id int primary key)", // in this session's temporary schema
		"grant usage on schema elsewhere to " + role,
		"grant select on ints, dated, items, pairs, nokey, \"Mixed\", oddkey, elsewhere.away, " + role + ".dup, " +
			"public.dup, events, events_low, \"user\" to " + role,
	} {
		mustExec(t, in, sql)
	}

	facts, err := readInitFacts(ctx, initPool(t, pg, db, role, "init-pw"), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []initTable{
		{Name: "dated", PK: "day", Stale: false},
		{Name: "dup", PK: "id", Stale: true},
		{Name: "events", PK: "id", Stale: true},
		{Name: "ints", PK: "id", Stale: true},
		{Name: "items", PK: "id", Stale: true},
		{Name: "user", PK: "id", Stale: true},
	}
	if got := withoutRows(facts.Tables); !slices.Equal(got, want) {
		t.Errorf("tables:\n got %+v\nwant %+v", got, want)
	}
	wantSkipped := map[string]int{
		"not reached by search_path":    3, // elsewhere.away, elsewhere."Far", public.dup
		"name needs quoting":            1, // "Mixed"
		"no single-column primary key":  3, // pairs, nokey, hidden_pairs
		"key column name needs quoting": 1, // oddkey
		"not readable by this account":  1, // hidden
	}
	if !maps.Equal(facts.Skipped, wantSkipped) {
		t.Errorf("skipped = %v, want %v", facts.Skipped, wantSkipped)
	}
	if facts.Dropped != 0 {
		t.Errorf("dropped = %d, want 0", facts.Dropped)
	}
	if facts.VersionNum < 140000 || facts.Version == "" {
		t.Errorf("version %d %q, want 14 or newer", facts.VersionNum, facts.Version)
	}
	if facts.User != role || facts.Database != db {
		t.Errorf("user %q, database %q; want %q, %q", facts.User, facts.Database, role, db)
	}
	if facts.SchemaExists {
		t.Error("SchemaExists, want false: the database has no stet schema")
	}
	if facts.CanCreate {
		t.Error("CanCreate, want false: role does not own the database and was not granted CREATE on it")
	}
}

func TestReadInitFactsCap(t *testing.T) {
	pg := testPG(t)
	defer func(old int) { maxInitTables = old }(maxInitTables)
	maxInitTables = 3
	db := cleanDatabase(t, pg)
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
	for _, tb := range []struct {
		name string
		rows int
	}{{"aa", 10}, {"bb", 50}, {"cc", 20}, {"dd", 40}, {"ee", 30}} {
		mustExec(t, admin, "create table "+tb.name+" (id int primary key)")
		mustExec(t, admin, "insert into "+tb.name+" select generate_series(1, $1::int)", tb.rows)
	}
	mustExec(t, admin, "analyze aa, bb, cc, dd, ee")

	facts, err := readInitFacts(context.Background(), initPool(t, pg, db, "stet_scram", "scram-pw"), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []initTable{
		{Name: "bb", PK: "id", Stale: true, Rows: 50},
		{Name: "dd", PK: "id", Stale: true, Rows: 40},
		{Name: "ee", PK: "id", Stale: true, Rows: 30},
	}
	if !slices.Equal(facts.Tables, want) {
		t.Errorf("tables:\n got %+v\nwant %+v", facts.Tables, want)
	}
	if facts.Dropped != 2 || len(facts.Skipped) != 0 {
		t.Errorf("dropped %d, skipped %v; want 2 and none", facts.Dropped, facts.Skipped)
	}
}

// Of tables with as many rows, the cap keeps the first by name.
func TestKeepLargest(t *testing.T) {
	ts := []initTable{{Name: "d", Rows: 5}, {Name: "a", Rows: -1}, {Name: "c", Rows: 5}, {Name: "b", Rows: 7}}
	kept, dropped := keepLargest(ts, 2)
	if want := []initTable{{Name: "b", Rows: 7}, {Name: "c", Rows: 5}}; !slices.Equal(kept, want) || dropped != 2 {
		t.Errorf("kept %+v, dropped %d; want %+v, 2", kept, dropped, want)
	}
	if kept, dropped := keepLargest(ts, 4); len(kept) != 4 || dropped != 0 {
		t.Errorf("at the cap: kept %+v, dropped %d; want all, 0", kept, dropped)
	}
}

func TestReadInitFactsOnly(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db := cleanDatabase(t, pg)
	pool := stetPoolIn(t, pg, db) // with the stet schema, whose tables are never candidates
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
	mustExec(t, admin, "create table ints (id int primary key)")
	mustExec(t, admin, "create table dated (day date primary key)")
	mustExec(t, admin, "create table pairs (a int, b int, primary key (a, b))")
	mustExec(t, admin, "create table other (id int primary key)")

	all, err := readInitFacts(ctx, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := withoutRows(all.Tables); !slices.Equal(got, []initTable{{"dated", "day", false, 0}, {"ints", "id", true, 0}, {"other", "id", true, 0}}) {
		t.Errorf("all tables: %+v, want dated, ints and other", got)
	}
	if want := map[string]int{"no single-column primary key": 1}; !maps.Equal(all.Skipped, want) {
		t.Errorf("skipped = %v, want %v", all.Skipped, want)
	}
	if !all.SchemaExists || !all.CanCreate {
		t.Errorf("SchemaExists %v, CanCreate %v; want both: the stet schema is there, and the account owns the database", all.SchemaExists, all.CanCreate)
	}

	defer func(old int) { maxInitTables = old }(maxInitTables)
	maxInitTables = 1 // not applied to the tables asked for
	facts, err := readInitFacts(ctx, pool, []string{"ints", "dated", "ints"})
	if err != nil {
		t.Fatal(err)
	}
	if got := withoutRows(facts.Tables); !slices.Equal(got, []initTable{{"dated", "day", false, 0}, {"ints", "id", true, 0}}) {
		t.Errorf("tables: %+v, want dated and ints", got)
	}
	if facts.Dropped != 0 || len(facts.Skipped) != 0 {
		t.Errorf("dropped %d, skipped %v; want 0 and none", facts.Dropped, facts.Skipped)
	}

	facts, err = readInitFacts(ctx, pool, []string{"nope", "pairs", "ints"})
	want := "table \"nope\": not found\ntable \"pairs\": no single-column primary key"
	if err == nil || err.Error() != want || facts != nil {
		t.Errorf("facts %+v, err %v; want no facts and the error\n%s", facts, err, want)
	}
}

func TestReadInitFactsWritesNothing(t *testing.T) {
	pg := testPG(t)
	db := cleanDatabase(t, pg)
	pool := initPool(t, pg, db, "stet_scram", "scram-pw")
	var (
		opened  int
		setting string // transaction_read_only, in readInitFacts' transaction
	)
	open := initFactsTx
	defer func() { initFactsTx = open }()
	initFactsTx = func(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
		opened++
		return open(ctx, pool, func(tx pgx.Tx) error {
			if err := fn(tx); err != nil {
				return err
			}
			return tx.QueryRow(ctx, "show transaction_read_only").Scan(&setting)
		})
	}

	facts, err := readInitFacts(context.Background(), pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if opened != 1 || setting != "on" {
		t.Errorf("transactions %d, transaction_read_only %q; want one, on", opened, setting)
	}
	if facts.SchemaExists || !facts.CanCreate {
		t.Errorf("SchemaExists %v, CanCreate %v; want false, true", facts.SchemaExists, facts.CanCreate)
	}
	var there bool
	in := connectTo(t, pg, db, "postgres", "postgres-pw")
	if err := in.QueryRow(context.Background(), "select exists (select from pg_namespace where nspname = 'stet')").Scan(&there); err != nil || there {
		t.Errorf("stet schema there: %v, err %v; want it still absent", there, err)
	}
}

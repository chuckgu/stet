package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/config"
)

// noEnv is an environment without DATABASE_URL.
func noEnv(string) string { return "" }

// initOut runs stet init with args, the answer stdin, and interactive, and
// returns what it printed and its error.
func initOut(args []string, stdin string, interactive bool) (string, error) {
	var out bytes.Buffer
	err := runInit(args, strings.NewReader(stdin), &out, interactive, noEnv)
	return out.String(), err
}

// dbAddress is the address of database db on pg as user, in plaintext.
func dbAddress(pg, db, user, password string) string {
	u := url.URL{Scheme: "postgres", Host: pg, Path: "/" + db, User: url.UserPassword(user, password), RawQuery: "sslmode=disable"}
	return u.String()
}

// ownDatabase creates a login role with password and a database of the
// test's own that the role owns, and drops both when the test ends.
func ownDatabase(t *testing.T, pg, password string) (db, role string) {
	t.Helper()
	su := superuser(t, pg)
	role = createRole(t, su, "init", "login password '"+password+"'") // before the database, which is dropped first
	db = fmt.Sprintf("stet_chk_db_%d", time.Now().UnixNano())
	mustExec(t, su, "create database "+db+" template template0 owner "+role)
	t.Cleanup(func() { su.Exec(context.Background(), "drop database if exists "+db+" with (force)") })
	return db, role
}

// schemaThere reports whether database db has the stet schema.
func schemaThere(t *testing.T, conn *pgx.Conn) bool {
	t.Helper()
	var there bool
	err := conn.QueryRow(context.Background(), "select exists (select from pg_namespace where nspname = 'stet')").Scan(&there)
	if err != nil {
		t.Fatal(err)
	}
	return there
}

// wantNoFile fails the test if path is there.
func wantNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s: %v; want no file", path, err)
	}
}

// wantPrivateConfig fails the test unless path is a config file only its
// owner may read, which config.Load reads.
func wantPrivateConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s has mode %o, want 600", path, perm)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the written config does not load: %v", err)
	}
	return cfg
}

func TestAgentURL(t *testing.T) {
	const crm = "postgres://app:pw@db.example.com:5432/crm"
	for _, tc := range []struct {
		name, upstream, listen, want string
	}{
		{"TLS and channel binding dropped",
			crm + "?sslmode=verify-full&sslrootcert=/x/ca.pem&channel_binding=require&application_name=a", "127.0.0.1:6432",
			"postgres://app:<password>@127.0.0.1:6432/crm?application_name=a&sslmode=disable"},
		{"no password", "postgres://app@db.example.com:5432/crm", "127.0.0.1:6432",
			"postgres://app@127.0.0.1:6432/crm?sslmode=disable"},
		{"empty password", "postgres://app:@db.example.com:5432/crm", "127.0.0.1:6432",
			"postgres://app@127.0.0.1:6432/crm?sslmode=disable"},
		{"any IPv4 address", crm, "0.0.0.0:7000", "postgres://app:<password>@127.0.0.1:7000/crm?sslmode=disable"},
		{"no listen host", crm, ":7000", "postgres://app:<password>@127.0.0.1:7000/crm?sslmode=disable"},
		{"any IPv6 address", crm, "[::]:7000", "postgres://app:<password>@127.0.0.1:7000/crm?sslmode=disable"},
		{"IPv6 loopback", crm, "[::1]:6432", "postgres://app:<password>@[::1]:6432/crm?sslmode=disable"},
		{"encoded user name", "postgresql://a%40b:pw@db.example.com/crm", "127.0.0.1:6432",
			"postgresql://a%40b:<password>@127.0.0.1:6432/crm?sslmode=disable"},
		{"other TLS parameters", crm + "?gssencmode=disable&requiressl=1&sslsni=0&sslcert=/c.pem&sslkey=/k.pem&sslpassword=kpw&sslnegotiation=postgres",
			"127.0.0.1:6432", "postgres://app:<password>@127.0.0.1:6432/crm?sslmode=disable"},
		{"key order, as written", crm + "?target_session_attrs=read-write&application_name=a%20b&connect_timeout=5", "127.0.0.1:6432",
			"postgres://app:<password>@127.0.0.1:6432/crm?application_name=a%20b&connect_timeout=5&target_session_attrs=read-write&sslmode=disable"},
		{"password parameter", "postgres://app@db.example.com/crm?password=pw", "127.0.0.1:6432",
			"postgres://app@127.0.0.1:6432/crm?password=<password>&sslmode=disable"},
		{"host and port parameters", crm + "?host=other.example.com&hostaddr=10.0.0.9&port=5433", "127.0.0.1:6432",
			"postgres://app:<password>@127.0.0.1:6432/crm?sslmode=disable"},
		{"no database", "postgres://app:pw@db.example.com", "localhost:6432",
			"postgres://app:<password>@localhost:6432?sslmode=disable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := agentURL(tc.upstream, tc.listen)
			if err != nil || got != tc.want {
				t.Errorf("agentURL = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestRenderConfig(t *testing.T) {
	const upstream = "postgres://app:pw@db.example.com:5432/crm?sslmode=verify-full"
	b, err := renderConfig(upstream, "127.0.0.1:6432",
		[]initTable{{Name: "deals", PK: "deal_id", Rows: 9}, {Name: "contacts", PK: "id", Stale: true, Rows: 1000}},
		"v0.2.0", time.Date(2026, 11, 22, 23, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	const want = `# Written by stet init (stet v0.2.0) on 2026-11-22.
# Shadow mode: stet records what it would do and blocks nothing.
# Every key is described in stet.example.yaml and in docs/spec-v0.1.md, chapter 9.
# This file holds the database password: keep it private.

mode: shadow
listen: 127.0.0.1:6432
upstream: ` + upstream + `
model:
  adapter: none
tables:
  contacts:
    pk: id
    impact:
      max_rows: 100
    reads:
    stale:
  deals:
    pk: deal_id
    impact:
      max_rows: 100
    reads:
`
	if string(b) != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", b, want)
	}
	for _, key := range []string{"send_fields", "agents", "undo"} {
		if bytes.Contains(b, []byte(key)) {
			t.Errorf("the config writes %s, want it left out", key)
		}
	}

	cfg, err := config.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != config.ModeShadow || cfg.Listen != "127.0.0.1:6432" || cfg.Upstream != upstream || cfg.Model.Adapter != "none" {
		t.Errorf("mode %q, listen %q, upstream %q, adapter %q", cfg.Mode, cfg.Listen, cfg.Upstream, cfg.Model.Adapter)
	}
	if len(cfg.Tables) != 2 || len(cfg.AgentRoles) != 0 || cfg.Undo != nil {
		t.Errorf("tables %v, agent roles %v, undo %v; want two tables, no roles, no undo", cfg.Tables, cfg.AgentRoles, cfg.Undo)
	}
	for name, stale := range map[string]bool{"contacts": true, "deals": false} {
		tb := cfg.Tables[name]
		wantPK := map[string]string{"contacts": "id", "deals": "deal_id"}[name]
		if tb.PK != wantPK || tb.Impact == nil || tb.Impact.MaxRows != 100 || tb.Reads == nil || (tb.Stale != nil) != stale {
			t.Errorf("table %s: pk %q, impact %+v, reads %v, stale %v; want pk %q, max_rows 100, reads, stale %v",
				name, tb.PK, tb.Impact, tb.Reads, tb.Stale, wantPK, stale)
		}
	}
}

func TestInitConfigRoundTripsOddNames(t *testing.T) {
	const upstream = "postgres://app:p%23ss%3Aw%40rd@db.example.com:5432/crm?sslmode=verify-full&application_name=a%20%23b"
	names := []string{"no", "null", "on", "true", "user", "y"}
	var tables []initTable
	for _, n := range names {
		tables = append(tables, initTable{Name: n, PK: "id"})
	}
	b, err := renderConfig(upstream, "127.0.0.1:6432", tables, "dev", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(b)
	if err != nil {
		t.Fatalf("%v; the config:\n%s", err, b)
	}
	var got []string
	for name := range cfg.Tables {
		got = append(got, name)
	}
	slices.Sort(got)
	if !slices.Equal(got, names) || cfg.Upstream != upstream {
		t.Errorf("tables %q, upstream %q; want %q, %q; the config:\n%s", got, cfg.Upstream, names, upstream, b)
	}
}

func TestInitThenProxy(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	const password = "then-proxy-pw"
	db, role := ownDatabase(t, pg, password)
	owner := connectTo(t, pg, db, role, password)
	mustExec(t, owner, "create table contacts (id int primary key, name text, status text, company_id int)")
	mustExec(t, owner, "create table deals (id serial primary key, contact_id int references contacts on delete cascade)")
	mustExec(t, owner, "insert into contacts select g, 'n' || g, 'new', g % 10 from generate_series(1, 1000) g")
	mustExec(t, owner, "insert into deals (contact_id) select g from generate_series(1, 1000) g")
	mustExec(t, owner, "analyze contacts, deals")

	// stet init only writes the address down, so the port is held open from
	// here to stet proxy, which is given this listener: no other process can
	// take the port in between.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	listen := ln.Addr().String()
	path := filepath.Join(t.TempDir(), "stet.yaml")
	out, err := initOut([]string{"-yes", "-config", path, "-listen", listen, dbAddress(pg, db, role, password)}, "", false)
	if err != nil {
		t.Fatalf("stet init: %v; output:\n%s", err, out)
	}
	cfg := wantPrivateConfig(t, path)
	agent := "postgres://" + role + ":<password>@" + listen + "/" + db + "?sslmode=disable"
	for _, s := range []string{"wrote ", "stet proxy -config " + path, "\n  " + agent + "\n"} {
		if !strings.Contains(out, s) {
			t.Errorf("the output has no %q:\n%s", s, out)
		}
	}
	if schemaThere(t, owner) {
		t.Error("the stet schema is there after stet init, want it made only by stet proxy")
	}

	if cfg.Listen != listen {
		t.Fatalf("the config listens on %q, want %q, given to -listen", cfg.Listen, listen)
	}
	r := startOn(t, cfg, ln)
	waitListening(t, r)
	if log := r.log.String(); strings.Contains(log, "level=WARN") {
		t.Errorf("the log has a warning, want none:\n%s", log)
	}

	uri := strings.Replace(agent, "<password>", password, 1)
	for _, w := range []struct{ sql, verdict string }{
		{"update contacts set status = 'customer' where id = 7", "commit"},
		{"update contacts set status = 'churned' where company_id < 3", "hold"},
		{"delete from contacts where id between 1 and 200", "hold"},
	} {
		if _, err := psqlWith(t, uri, w.sql); err != nil {
			t.Fatalf("psql with the address stet init printed: %v; log:\n%s", err, r.log)
		}
		// Shadow decisions are recorded after the write ran: wait for each.
		deadline := time.Now().Add(10 * time.Second)
		var verdict string
		for {
			err := owner.QueryRow(ctx, "select verdict from stet.decisions where statement = $1", w.sql).Scan(&verdict)
			if err == nil {
				break
			}
			if !errors.Is(err, pgx.ErrNoRows) || time.Now().After(deadline) {
				t.Fatalf("%s: no decision (%v); log:\n%s", w.sql, err, r.log)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if verdict != w.verdict {
			t.Errorf("%s: verdict %q, want %q", w.sql, verdict, w.verdict)
		}
	}
}

// contactsDatabase is a database of the test's own (cleanDatabase), owned by
// Stet's account, with one table stet init takes; with schema, the stet
// schema is there.
func contactsDatabase(t *testing.T, pg string, schema bool) (address string) {
	t.Helper()
	db := cleanDatabase(t, pg)
	mustExec(t, connectTo(t, pg, db, "stet_scram", "scram-pw"), "create table contacts (id int primary key)")
	if schema {
		stetPoolIn(t, pg, db)
	}
	return dbAddress(pg, db, "stet_scram", "scram-pw")
}

const (
	noticeFirst = `stet proxy will create one schema, "stet", in database "`
	noticeLast  = "It changes nothing else. To remove it later: DROP SCHEMA stet CASCADE;\n"
	question    = "Continue? [y/N] "
	notTerminal = "this is not a terminal, so stet init cannot ask; read the lines above and pass -yes to continue"
	stopped     = "stopped; nothing was written"
)

func TestInitAsks(t *testing.T) {
	pg := testPG(t)
	address := contactsDatabase(t, pg, false)
	for _, answer := range []string{"y\n", "YES\n", " Yes \r\n"} {
		t.Run(strings.TrimSpace(answer), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stet.yaml")
			out, err := initOut([]string{"-config", path, address}, answer, true)
			if err != nil {
				t.Fatalf("stet init: %v; output:\n%s", err, out)
			}
			wantPrivateConfig(t, path)
			if !strings.Contains(out, noticeFirst) || !strings.Contains(out, noticeLast+question) {
				t.Errorf("the output has no notice and question:\n%s", out)
			}
		})
	}

	t.Run("not a terminal", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stet.yaml")
		out, err := initOut([]string{"-config", path, address}, "y\n", false)
		if err == nil || err.Error() != notTerminal {
			t.Errorf("err = %v, want %q", err, notTerminal)
		}
		if !strings.Contains(out, noticeFirst) || !strings.HasSuffix(out, noticeLast) {
			t.Errorf("the output does not end with the notice, and no question:\n%s", out)
		}
		wantNoFile(t, path)
	})

	t.Run("schema there", func(t *testing.T) {
		address := contactsDatabase(t, pg, true)
		path := filepath.Join(t.TempDir(), "stet.yaml")
		out, err := initOut([]string{"-config", path, address}, "", true)
		if err != nil {
			t.Fatalf("stet init: %v; output:\n%s", err, out)
		}
		wantPrivateConfig(t, path)
		if strings.Contains(out, question) || strings.Contains(out, noticeFirst) {
			t.Errorf("the output tells of the schema or asks, want neither:\n%s", out)
		}
	})
}

func TestInitDeclinedWritesNothing(t *testing.T) {
	pg := testPG(t)
	address := contactsDatabase(t, pg, false)
	for name, answer := range map[string]string{"no": "n\n", "empty line": "\n", "EOF": "", "other": "sure\n"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stet.yaml")
			out, err := initOut([]string{"-config", path, address}, answer, true)
			if err == nil || err.Error() != stopped {
				t.Errorf("err = %v, want %q", err, stopped)
			}
			if !strings.Contains(out, question) {
				t.Errorf("the output has no question:\n%s", out)
			}
			wantNoFile(t, path)
			if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 0 {
				t.Errorf("the directory holds %v, want nothing", entries)
			}
		})
	}
}

func TestInitRefusesToOverwrite(t *testing.T) {
	const old = "mode: shadow\n"
	existing := func(t *testing.T) string {
		path := filepath.Join(t.TempDir(), "stet.yaml")
		if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	unchanged := func(t *testing.T, path string) {
		t.Helper()
		if b, err := os.ReadFile(path); err != nil || string(b) != old {
			t.Errorf("%s holds %q, %v; want it as it was", path, b, err)
		}
	}

	t.Run("before connecting", func(t *testing.T) {
		path := existing(t)
		out, err := initOut([]string{"-yes", "-config", path, "postgres://app:pw@127.0.0.1:1/crm"}, "", false)
		if want := path + " already exists; pass -force to overwrite it"; err == nil || err.Error() != want {
			t.Errorf("err = %v, want %q", err, want)
		}
		if out != "" {
			t.Errorf("output %q, want none", out)
		}
		unchanged(t, path)
	})

	t.Run("there by the time it writes", func(t *testing.T) {
		path := existing(t)
		err := writeConfigFile(path, []byte("mode: enforce\n"), false)
		if want := path + " already exists; pass -force to overwrite it"; err == nil || err.Error() != want {
			t.Errorf("err = %v, want %q", err, want)
		}
		unchanged(t, path)
		if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
			t.Errorf("the directory holds %v, want only the file", entries)
		}
	})

	t.Run("force", func(t *testing.T) {
		pg := testPG(t)
		path := existing(t)
		out, err := initOut([]string{"-yes", "-force", "-config", path, contactsDatabase(t, pg, false)}, "", false)
		if err != nil {
			t.Fatalf("stet init -force: %v; output:\n%s", err, out)
		}
		if cfg := wantPrivateConfig(t, path); len(cfg.Tables) != 1 {
			t.Errorf("tables %v, want contacts", cfg.Tables)
		}
	})
}

func TestInitWritesTheFileWhole(t *testing.T) {
	config := []byte("mode: shadow\n")

	t.Run("directory not writable", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(dir, 0o700)
		if f, err := os.CreateTemp(dir, "probe"); err == nil { // as root, say
			f.Close()
			t.Skip("the directory is writable despite its mode")
		}
		if err := writeConfigFile(filepath.Join(dir, "stet.yaml"), config, true); err == nil {
			t.Error("no error, want one")
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("the directory holds %v, want nothing", entries)
		}
	})

	t.Run("cannot be put in place", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "stet.yaml")
		if err := os.MkdirAll(filepath.Join(path, "inside"), 0o700); err != nil { // a directory where the file goes
			t.Fatal(err)
		}
		if err := writeConfigFile(path, config, true); err == nil {
			t.Error("no error, want one")
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 1 || entries[0].Name() != "stet.yaml" || !entries[0].IsDir() {
			t.Errorf("the directory holds %v, want only the directory stet.yaml", entries)
		}
	})

	t.Run("written", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "stet.yaml")
		if err := writeConfigFile(path, config, false); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, config) {
			t.Errorf("%s holds %q, %v; want %q", path, b, err, config)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 1 {
			t.Errorf("the directory holds %v, want only the file", entries)
		}
	})
}

func TestInitOutputNeverShowsThePassword(t *testing.T) {
	pg := testPG(t)
	db, role := ownDatabase(t, pg, initPassword)
	mustExec(t, connectTo(t, pg, db, role, initPassword), "create table contacts (id int primary key)")
	remote := func(t *testing.T) { t.Cleanup(setRemoteHost(func(string) bool { return true })) }

	for _, tc := range []struct {
		name     string
		password string // the address's
		address  string
		args     []string
		setup    func(t *testing.T)
		ok       bool
	}{
		{name: "success", password: initPassword, address: dbAddress(pg, db, role, initPassword), args: []string{"-yes"}, ok: true},
		{name: "success in the URL's query", password: initPassword,
			address: "postgres://" + pg + "/" + db + "?sslmode=disable&user=" + role + "&password=" + initPassword,
			args:    []string{"-yes"}, ok: true},
		{name: "wrong password", password: "wrong-" + initPassword, address: dbAddress(pg, db, role, "wrong-"+initPassword), args: []string{"-yes"}},
		{name: "no server", password: initPassword, address: "postgres://app:" + initPassword + "@127.0.0.1:1/crm", args: []string{"-yes"}},
		{name: "no server, taken for remote", password: initPassword, address: "postgres://app:" + initPassword + "@127.0.0.1:1/crm",
			args: []string{"-yes"}, setup: remote},
		{name: "not a URL", password: initPassword, address: "postgres://app:" + initPassword + "@db.example.com:port/crm", args: []string{"-yes"}},
		{name: "other scheme", password: initPassword, address: "mysql://app:" + initPassword + "@db.example.com/crm", args: []string{"-yes"}},
		{name: "keyword/value", password: initPassword, address: "host=db.example.com user=app password=" + initPassword, args: []string{"-yes"}},
		{name: "no host", password: initPassword, address: "postgres://app:" + initPassword + "@/crm", args: []string{"-yes"}},
		{name: "host in the query", password: initPassword, address: "postgres://app:" + initPassword + "@localhost/crm?host=/tmp", args: []string{"-yes"}},
		{name: "several hosts", password: initPassword, address: "postgres://app:" + initPassword + "@h1:5432,h2:5432/crm", args: []string{"-yes"}},
		// Read one way by pgx and another by the proxy; refused before connecting.
		{name: "a space before a query key", password: initPassword,
			address: "postgres://app:" + initPassword + "@127.0.0.1:1/crm?sslmode=disable& host=127.0.0.2", args: []string{"-yes"}},
		{name: "# in the password", password: initPassword, address: "postgres://app:#" + initPassword + "@127.0.0.1:1/crm", args: []string{"-yes"}},
		{name: "? in the password", password: initPassword, address: "postgres://app:12?" + initPassword + "@127.0.0.1:1/crm", args: []string{"-yes"}},
		{name: "? in the user name", password: initPassword, address: "postgres://h?x:" + initPassword + "@/crm", args: []string{"-yes"}},
		// The user name is the host: both reach db:5432, but net/url takes
		// what follows the ? or # for the query or the fragment.
		{name: "? first in the password", password: initPassword, address: "postgres://db:?" + initPassword + "@db/crm", args: []string{"-yes"}},
		{name: "? first in the password, with a port", password: initPassword,
			address: "postgres://db:5432?" + initPassword + "@db:5432/crm", args: []string{"-yes"}},
		{name: "# first in the password", password: initPassword, address: "postgres://db:#" + initPassword + "@db/crm", args: []string{"-yes"}},
		{name: "PGPORT", password: initPassword, address: "postgres://app:" + initPassword + "@127.0.0.1/crm", args: []string{"-yes"},
			setup: func(t *testing.T) { t.Setenv("PGPORT", "1") }},
		{name: "a space before the password key", password: initPassword,
			address: "postgres://app@127.0.0.1:1/crm?sslmode=disable& password=" + initPassword, args: []string{"-yes"}},
		{name: "the address given to -listen", password: initPassword, address: dbAddress(pg, db, role, initPassword),
			args: []string{"-yes", "-listen", "postgres://app:" + initPassword + "@db.example.com:5432/crm"}},
		{name: "refused without -yes", password: initPassword, address: dbAddress(pg, db, role, initPassword)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			path := filepath.Join(t.TempDir(), "stet.yaml")
			out, err := initOut(append(tc.args, "-config", path, tc.address), "", false)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want an error: %v; output:\n%s", err, !tc.ok, out)
			}
			if strings.Contains(out, tc.password) || err != nil && strings.Contains(err.Error(), tc.password) {
				t.Errorf("the output or the error holds the password:\n%s\nerror: %v", out, err)
			}
			if tc.ok {
				if b, err := os.ReadFile(path); err != nil || !bytes.Contains(b, []byte(tc.password)) {
					t.Errorf("the config file has no password (%v), want it there:\n%s", err, b)
				}
			}
		})
	}
}

func TestInitStops(t *testing.T) {
	const (
		address  = "postgres://app:pw@127.0.0.1:1/crm"
		listen   = "-listen must be host:port with a port from 1 to 65535"
		inQuery  = "the database address sets host, hostaddr or port in its query; write the host and port in the address itself"
		manyHost = "the database address lists more than one host; stet relays to one server, so give one host"
		space    = "the database address contains a space; percent-encode it (%20) or remove it"
		userInfo = "the database address has a ? or # in its user or password; percent-encode it (%3F, %23)"
		twoWays  = "stet's own connections and the proxy would reach different hosts or ports for this address; " +
			"write one host and one port in the address itself (not in its query, PGHOST, PGPORT or a service file), " +
			"and percent-encode special characters in the password"
	)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no address", nil, "no database address: set DATABASE_URL, or pass the address as the last argument"},
		{"empty address", []string{""}, "no database address: set DATABASE_URL, or pass the address as the last argument"},
		{"flags after the address", []string{address, "-yes"}, "flags go before the address"},
		{"listen without a port", []string{"-listen", "6432", address}, listen},
		{"listen with no port number", []string{"-listen", "127.0.0.1:pg", address}, listen},
		{"listen on port 0", []string{"-listen", "127.0.0.1:0", address}, listen},
		{"no host", []string{"postgres://app:pw@:5432/crm"}, "the database address has no host"},
		// pgx connects where these say; the proxy would relay to the URL's own host and port.
		{"host in the query", []string{"postgres://app:pw@localhost/crm?host=/cloudsql/p:r:i"}, inQuery},
		{"hostaddr in the query", []string{"postgres://app:pw@db.example.com/crm?sslmode=require&hostaddr=10.0.0.9"}, inQuery},
		{"port in the query", []string{"postgres://app:pw@db.example.com/crm?port=5433"}, inQuery},
		{"host in the query, its name encoded", []string{"postgres://app:pw@db.example.com/crm?ho%73t=other.example.com"}, inQuery},
		{"several hosts", []string{"postgres://app:pw@h1:5432,h2:5432/crm"}, manyHost},
		{"several hosts, the comma encoded", []string{"postgres://app:pw@h1%2Ch2/crm"}, manyHost},
		// pgx drops spaces around a query key; the proxy's reading keeps them.
		{"a space before a query key", []string{"postgres://app:pw@127.0.0.1:1/crm?sslmode=disable& host=127.0.0.2"}, space},
		{"a space after a query key", []string{"postgres://app:pw@127.0.0.1:1/crm?host =127.0.0.2"}, space},
		{"a space before the password key", []string{"postgres://app@127.0.0.1:1/crm?sslmode=disable& password=secret"}, space},
		// pgx ends the user name and password at the first @ before a /; the
		// proxy's reading ends the host part at /, ? or #.
		{"# in the password", []string{"postgres://app:#hunter2@127.0.0.1:1/crm"}, twoWays},
		{"? in the password", []string{"postgres://app:12?34@127.0.0.1:1/crm"}, twoWays},
		{"? in the user name", []string{"postgres://h?@/crm"}, twoWays},
		// The two agree on the host and port (the user name is the host) but
		// not on the user and password: net/url reads the query or fragment
		// from the ? or #, and the agent's address would show the rest.
		{"? first in the password", []string{"postgres://db:?s3cret@db/crm"}, userInfo},
		{"? first in the password, with a port", []string{"postgres://db:5432?s3cret@db:5432/crm"}, userInfo},
		{"# first in the password", []string{"postgres://db:#s3cret@db/crm"}, userInfo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := initOut(append([]string{"-config", filepath.Join(t.TempDir(), "stet.yaml")}, tc.args...), "", false)
			if err == nil || err.Error() != tc.want || out != "" {
				t.Errorf("err = %v, output %q; want %q and no output", err, out, tc.want)
			}
		})
	}

	t.Run("PGPORT", func(t *testing.T) {
		t.Setenv("PGPORT", "1") // pgx would connect to 127.0.0.1:1, the proxy relay to 127.0.0.1:5432
		out, err := initOut([]string{"-config", filepath.Join(t.TempDir(), "stet.yaml"), "postgres://app:pw@127.0.0.1/crm"}, "", false)
		if err == nil || err.Error() != twoWays || out != "" {
			t.Errorf("err = %v, output %q; want %q and no output", err, out, twoWays)
		}
	})

	t.Run("DATABASE_URL", func(t *testing.T) {
		var out bytes.Buffer
		env := func(k string) string { return map[string]string{"DATABASE_URL": "postgres://app:pw@:5432/crm"}[k] }
		err := runInit([]string{"-config", filepath.Join(t.TempDir(), "stet.yaml")}, strings.NewReader(""), &out, false, env)
		if err == nil || err.Error() != "the database address has no host" {
			t.Errorf("err = %v, want DATABASE_URL's address read and refused", err)
		}
	})

	t.Run("before 14", func(t *testing.T) {
		err := checkInitFacts(&initFacts{VersionNum: 130016, Version: "13.16", SchemaExists: true, Tables: []initTable{{Name: "contacts", PK: "id"}}})
		if want := "PostgreSQL 14 or newer is needed; this server is 13.16"; err == nil || err.Error() != want {
			t.Errorf("err = %v, want %q", err, want)
		}
	})

	t.Run("-tables names a table that is not there", func(t *testing.T) {
		pg := testPG(t)
		path := filepath.Join(t.TempDir(), "stet.yaml")
		out, err := initOut([]string{"-yes", "-config", path, "-tables", "contacts, nope", contactsDatabase(t, pg, false)}, "", false)
		if want := `table "nope": not found`; err == nil || err.Error() != want {
			t.Errorf("err = %v, want %q; output:\n%s", err, want, out)
		}
		wantNoFile(t, path)
	})

	t.Run("no table to watch", func(t *testing.T) {
		pg := testPG(t)
		db := cleanDatabase(t, pg)
		admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
		mustExec(t, admin, "create table nokey (a int)")
		mustExec(t, admin, "create table pairs (a int, b int, primary key (a, b))")
		mustExec(t, admin, `create table "Mixed" (id int primary key)`)
		path := filepath.Join(t.TempDir(), "stet.yaml")
		out, err := initOut([]string{"-yes", "-config", path, dbAddress(pg, db, "stet_scram", "scram-pw")}, "", false)
		want := "no table can be watched\n" +
			"left out 1 table: name needs quoting\n" +
			"left out 2 tables: no single-column primary key"
		if err == nil || err.Error() != want {
			t.Errorf("err = %v, want:\n%s\noutput:\n%s", err, want, out)
		}
		wantNoFile(t, path)
	})

	t.Run("an account that may not create the schema", func(t *testing.T) {
		pg := testPG(t)
		role := createRole(t, superuser(t, pg), "init", "login password 'init-pw'") // before the database: see cleanDatabase
		db := cleanDatabase(t, pg)
		path := filepath.Join(t.TempDir(), "stet.yaml")
		out, err := initOut([]string{"-yes", "-config", path, dbAddress(pg, db, role, "init-pw")}, "", false)
		want := `the stet schema is not there and "` + role + `" may not create it; as the database's owner run: ` +
			`GRANT CREATE ON DATABASE "` + db + `" TO "` + role + `";`
		if err == nil || err.Error() != want {
			t.Errorf("err = %v, want %q; output:\n%s", err, want, out)
		}
		wantNoFile(t, path)
	})
}

func TestInitReportsWhatItLeftOut(t *testing.T) {
	pg := testPG(t)
	defer func(old int) { maxInitTables = old }(maxInitTables)
	maxInitTables = 2
	db := cleanDatabase(t, pg)
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
	for _, name := range []string{"aa", "bb", "cc", "dd"} {
		mustExec(t, admin, "create table "+name+" (id int primary key)")
	}
	mustExec(t, admin, "create table nokey (a int)")
	path := filepath.Join(t.TempDir(), "stet.yaml")
	out, err := initOut([]string{"-yes", "-config", path, dbAddress(pg, db, "stet_scram", "scram-pw")}, "", false)
	if err != nil {
		t.Fatalf("stet init: %v; output:\n%s", err, out)
	}
	for _, s := range []string{
		"found 2 tables to watch\n",
		"left out 1 table: no single-column primary key\n",
		"left out 2 more tables: stet init takes at most 2, those with the most rows; to choose the tables yourself, pass -tables a,b,c\n",
		"wrote " + path + ": 2 tables, shadow mode, no model\n",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("the output has no %q:\n%s", s, out)
		}
	}
}

// Text the server sends is shown with its control characters as spaces: the
// server is not trusted either (rule 5).
func TestInitShowsServerTextWithoutControlCharacters(t *testing.T) {
	const dirty = "a\x1b[2Jb\u202ec\u2028d\u2066e\x00f\ng\x7fh"
	const clean = "a [2Jb c d e f g h"
	if got := printable(dirty); got != clean {
		t.Errorf("printable = %q, want %q", got, clean)
	}

	err := checkInitFacts(&initFacts{VersionNum: 130000, Version: "13.0" + dirty})
	if want := "PostgreSQL 14 or newer is needed; this server is 13.0" + clean; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	facts := &initFacts{VersionNum: 140018, User: `app"` + dirty, Database: "crm" + dirty}
	err = checkInitFacts(facts)
	want := `the stet schema is not there and "app""` + clean + `" may not create it; as the database's owner run: ` +
		`GRANT CREATE ON DATABASE "crm` + clean + `" TO "app""` + clean + `";`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if got, want := schemaNotice(facts), `stet proxy will create one schema, "stet", in database "crm`+clean+`" as "app""`+clean+
		`" the first time it starts.`+"\n"+noticeLast; got != want {
		t.Errorf("notice = %q, want %q", got, want)
	}

	// The hint chooseSSLMode adds keeps its line.
	err = printableError(fmt.Errorf("%w\n%s", errors.New("server error: FATAL: "+dirty), poolerHint))
	if want := "server error: FATAL: " + clean + "\n" + poolerHint; err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
	if err := printableError(errors.New(dirty + "\n" + poolerHint + "!")); err.Error() != printable(dirty+"\n"+poolerHint+"!") {
		t.Errorf("err = %q, want every line break replaced", err)
	}

	t.Run("a database so named", func(t *testing.T) {
		pg := testPG(t)
		name := fmt.Sprintf("stet_chk_db_%d\x1b[31m\u202e", time.Now().UnixNano())
		su := superuser(t, pg)
		mustExec(t, su, "create database "+pgx.Identifier{name}.Sanitize()+" template template0 owner stet_scram")
		t.Cleanup(func() {
			su.Exec(context.Background(), "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)")
		})
		mustExec(t, connectTo(t, pg, name, "stet_scram", "scram-pw"), "create table contacts (id int primary key)")
		out, err := initOut([]string{"-config", filepath.Join(t.TempDir(), "stet.yaml"), dbAddress(pg, name, "stet_scram", "scram-pw")}, "", false)
		if err == nil || err.Error() != notTerminal {
			t.Fatalf("err = %v, want %q; output:\n%s", err, notTerminal, out)
		}
		if want := `in database "` + printable(name) + `" as "stet_scram"`; !strings.Contains(out, want) {
			t.Errorf("the output has no %q:\n%s", want, out)
		}
		if strings.ContainsFunc(strings.ReplaceAll(out, "\n", ""), unsafeRune) {
			t.Errorf("the output holds a control character: %q", out)
		}
	})
}

// unsafeRune is what printable replaces.
func unsafeRune(r rune) bool { return printable(string(r)) != string(r) }

// stet init never writes a file stet proxy refuses, and the reason it gives
// holds no password, though config's errors may quote the address whole.
func TestInitChecksTheConfigBeforeWriting(t *testing.T) {
	const prefix = "stet proxy would not accept the config for this address: "
	render := func(t *testing.T, upstream string) []byte {
		t.Helper()
		file, err := renderConfig(upstream, "127.0.0.1:6432", []initTable{{Name: "contacts", PK: "id", Stale: true}}, "dev", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	for _, tc := range []struct{ name, upstream, reason string }{
		// PostgreSQL 17 takes it, so chooseSSLMode connects; the config refuses it.
		{"sslnegotiation=direct", "postgres://app:" + initPassword + "@db.example.com:5432/crm?sslmode=require&sslnegotiation=direct",
			"upstream: sslnegotiation=direct is not supported"},
		{"a reason about the address", "postgres://app:" + initPassword + "@/crm", "upstream: has no host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkConfig(render(t, tc.upstream), tc.upstream)
			if err == nil || !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("err = %v, want %q followed by a reason with %q", err, prefix, tc.reason)
			}
			if err != nil && strings.Contains(err.Error(), initPassword) {
				t.Errorf("the error holds the password: %v", err)
			}
		})
	}

	t.Run("accepted", func(t *testing.T) {
		const upstream = "postgres://app:" + initPassword + "@db.example.com:5432/crm?sslmode=verify-full"
		if err := checkConfig(render(t, upstream), upstream); err != nil {
			t.Errorf("err = %v, want none", err)
		}
	})

	// PGSSLMODE=allow lets stet init connect to an address that writes no
	// sslmode; the config refuses allow, so stet init writes nothing.
	t.Run("through stet init", func(t *testing.T) {
		pg := testPG(t)
		address := strings.TrimSuffix(contactsDatabase(t, pg, false), "?sslmode=disable")
		t.Setenv("PGSSLMODE", "allow")
		path := filepath.Join(t.TempDir(), "stet.yaml")
		out, err := initOut([]string{"-yes", "-config", path, address}, "", false)
		want := prefix + "upstream: sslmode=allow is not supported"
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("err = %v, want it to start with %q; output:\n%s", err, want, out)
		}
		if strings.Contains(out, "scram-pw") || err != nil && strings.Contains(err.Error(), "scram-pw") {
			t.Errorf("the output or the error holds the password:\n%s\nerror: %v", out, err)
		}
		wantNoFile(t, path)
	})

	t.Run("every form of the password", func(t *testing.T) {
		const upstream = `postgres://app:s3cr%2Bt@/crm?application_name=a"b&password=q%22ry`
		secrets := []string{upstream, strconv.Quote(upstream), "s3cr%2Bt", "s3cr+t", "q%22ry", `q"ry`, `q\"ry`}
		got := withoutSecrets(strings.Join(secrets, " | "), upstream)
		for _, s := range secrets {
			if strings.Contains(got, s) {
				t.Errorf("%q holds %q", got, s)
			}
		}
		if want := "<the database address> | \"<the database address>\" | <password>"; !strings.HasPrefix(got, want) {
			t.Errorf("got %q, want it to start with %q", got, want)
		}
	})
}

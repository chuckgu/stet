package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/custody"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/pgtest"
	"github.com/stetdb/stet/internal/scram"
)

// custodyCase is what a test of custody serves with: a certificate for
// Stet's side of the clients' leg (listen_tls), which clients also trust as
// their root, the test server's certificate for the upstream leg, and a
// credentials file. secrets is what no log, output or decision may show:
// each token, its verifier, the verifier's keys, and each stored password.
type custodyCase struct {
	certPath, keyPath string
	upstreamRoot      string // "" when the test has no test server
	credentials       string
	tokens            map[string]string // the latest, by account
	secrets           []string
}

// secret is a new random password.
func secret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "real-" + hex.EncodeToString(b)
}

// newAgentRole creates a login role of the test's own with a new random
// password, which only Stet is to know, and drops it when the test ends.
// Create it before a database it is granted rights in (see cleanDatabase).
func newAgentRole(t *testing.T, pg string) (role, password string) {
	t.Helper()
	password = secret(t)
	return createRole(t, superuser(t, pg), "custody", "login password '"+password+"'"), password
}

// newCustody makes the certificates and writes a credentials file holding
// the accounts of passwords, each with its stored password and a new token.
// pg is the test server, or "" when the test needs none.
func newCustody(t *testing.T, pg string, passwords map[string]string) *custodyCase {
	t.Helper()
	c := &custodyCase{credentials: filepath.Join(t.TempDir(), "agents.yaml"), tokens: map[string]string{}}
	c.certPath, c.keyPath, _ = pgtest.LocalCert(t)
	if pg != "" {
		_, c.upstreamRoot = pgtest.ServerCert(t, pg)
	}
	f := &custody.File{Agents: map[string]custody.Agent{}}
	for account, password := range passwords {
		f.Agents[account] = c.agent(t, account, password)
	}
	if err := custody.Write(c.credentials, f); err != nil {
		t.Fatal(err)
	}
	return c
}

// agent is the credentials file's entry of account with password and a new
// token, which c keeps as the account's and among its secrets.
func (c *custodyCase) agent(t *testing.T, account, password string) custody.Agent {
	t.Helper()
	token, v, err := scram.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	text := v.Text() // SCRAM-SHA-256$4096:<salt>$<StoredKey>:<ServerKey>
	parts := strings.Split(text, "$")
	if len(parts) != 3 {
		t.Fatalf("a verifier of %d parts", len(parts))
	}
	stored, server, _ := strings.Cut(parts[2], ":")
	c.tokens[account] = token
	c.secrets = append(c.secrets, token, text, stored, server, password)
	now := time.Now().UTC().Truncate(time.Second)
	return custody.Agent{Token: text, Password: password, Added: now, TokenIssued: now}
}

// newToken gives account a new token in c's file, as stet agent token does.
func (c *custodyCase) newToken(t *testing.T, account string) {
	t.Helper()
	f, err := custody.Read(c.credentials)
	if err != nil {
		t.Fatal(err)
	}
	a := f.Agents[account]
	fresh := c.agent(t, account, a.Password)
	a.Token, a.TokenIssued = fresh.Token, fresh.TokenIssued
	f.Agents[account] = a
	if err := custody.Write(c.credentials, f); err != nil {
		t.Fatal(err)
	}
}

// add adds account to c's file with password and a new token, as stet agent
// add does.
func (c *custodyCase) add(t *testing.T, account, password string) {
	t.Helper()
	f, err := custody.Read(c.credentials)
	if err != nil {
		t.Fatal(err)
	}
	f.Agents[account] = c.agent(t, account, password)
	if err := custody.Write(c.credentials, f); err != nil {
		t.Fatal(err)
	}
}

// config is a config in mode with c's listen_tls and credentials file, over
// database db of the test server pg, which Stet reaches as its own account
// at sslmode=verify-full; rest is added at the end (tables, say).
func (c *custodyCase) config(t *testing.T, mode, pg, db, rest string) *config.Config {
	t.Helper()
	upstream := url.URL{
		Scheme: "postgres", Host: pg, Path: "/" + db, User: url.UserPassword("stet_scram", "scram-pw"),
		RawQuery: url.Values{"sslmode": {"verify-full"}, "sslrootcert": {c.upstreamRoot}}.Encode(),
	}
	return parseConfig(t, `mode: %s
listen: 127.0.0.1:0
listen_tls: {cert: %q, key: %q}
agents: {credentials: %q}
upstream: %s
model: {adapter: none, on_failure: pass}
%s`, mode, c.certPath, c.keyPath, c.credentials, upstream.String(), rest)
}

// dial logs in to Stet at addr as account with token, to database db,
// checking Stet's certificate and name (sslmode=verify-full).
func (c *custodyCase) dial(addr, db, account, token string) (*pgx.Conn, error) {
	u := url.URL{
		Scheme: "postgres", Host: addr, Path: "/" + db, User: url.UserPassword(account, token),
		RawQuery: url.Values{"sslmode": {"verify-full"}, "sslrootcert": {c.certPath}}.Encode(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return pgx.Connect(ctx, u.String())
}

// connect is dial with account's latest token, failing the test when the
// login fails; the connection is closed when the test ends.
func (c *custodyCase) connect(t *testing.T, r *running, db, account string) *pgx.Conn {
	t.Helper()
	conn, err := c.dial(r.addr, db, account, c.tokens[account])
	if err != nil {
		t.Fatalf("log in to stet as %s with its token: %v; log:\n%s", account, err, r.log)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// psql runs query with psql logged in to Stet at addr as account with its
// latest token, at sslmode=verify-full.
func (c *custodyCase) psql(t *testing.T, addr, db, account, query string) (string, error) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return psqlWith(t, fmt.Sprintf("host=%s port=%s user=%s dbname=%s sslmode=verify-full sslrootcert='%s'",
		host, port, account, db, c.certPath), query, "PGPASSWORD="+c.tokens[account])
}

// wantRefused fails the test unless err is the FATAL 28P01 PostgreSQL gives
// for a wrong password.
func wantRefused(t *testing.T, err error, account string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Severity != "FATAL" || pgErr.Code != "28P01" ||
		pgErr.Message != `password authentication failed for user "`+account+`"` {
		t.Errorf("login as %s: %v, want FATAL 28P01 password authentication failed", account, err)
	}
}

// startCustody serves cfg as stet proxy -v -decisions on does: the log at
// debug level, decision lines to out. gateOf returns the gate run opened,
// once it has.
func startCustody(t *testing.T, cfg *config.Config) (r *running, out *syncBuffer, gateOf func() *gate.Gate) {
	t.Helper()
	got := make(chan *gate.Gate, 1)
	onGate = func(g *gate.Gate) { got <- g }
	t.Cleanup(func() { onGate = nil }) // runs after the stop registered below
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r = &running{addr: ln.Addr().String(), log: &syncBuffer{}, cancel: cancel, done: make(chan struct{})}
	out = &syncBuffer{}
	log := slog.New(slog.NewTextHandler(r.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	go func() {
		r.err = runWith(ctx, cfg, ln, log, out)
		close(r.done)
	}()
	t.Cleanup(func() { r.stop(t) })
	return r, out, func() *gate.Gate {
		t.Helper()
		select {
		case g := <-got:
			got <- g
			return g
		case <-time.After(5 * time.Second):
			t.Fatalf("run opened no gate; log:\n%s", r.log)
			return nil
		}
	}
}

// refusedToStart waits for r to end and returns its error, failing the test
// when it served or kept its listener open.
func refusedToStart(t *testing.T, r *running) error {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("run is still running; log:\n%s", r.log)
	}
	if r.err == nil {
		t.Fatalf("run = nil, want it to refuse to start; log:\n%s", r.log)
	}
	if strings.Contains(r.log.String(), "stet proxy listening") {
		t.Errorf("run served before refusing:\n%s", r.log)
	}
	if c, err := net.DialTimeout("tcp", r.addr, time.Second); err == nil {
		c.Close()
		t.Error("the listener is still open after run refused to start")
	}
	return r.err
}

// listeningLine is the log's stet proxy listening line.
func listeningLine(t *testing.T, r *running) string {
	t.Helper()
	for line := range strings.SplitSeq(r.log.String(), "\n") {
		if strings.Contains(line, `msg="stet proxy listening"`) {
			return line
		}
	}
	t.Fatalf("no listening line; log:\n%s", r.log)
	return ""
}

// custodyRun is what a scenario of custody leaves for TestNoSecretInAnyLog
// to read: Stet's log and output, and a connection to the database its
// decisions are in (nil: there is no gate).
type custodyRun struct {
	r         *running
	out       *syncBuffer
	c         *custodyCase
	decisions *pgx.Conn
}

func TestCustodyEndToEnd(t *testing.T) { custodyEndToEnd(t) }

// In shadow mode, with listen_tls and a credentials file, psql and pgx that
// know only the account's token log in at sslmode=verify-full and change a
// row of a gated table: the session is the account's and TLS on the server's
// side too, the write is recorded with the account as login_role, and
// nothing is warned of.
func custodyEndToEnd(t *testing.T) *custodyRun {
	pg := testPG(t)
	ctx := context.Background()
	account, password := newAgentRole(t, pg)
	// As deploy's "필요한 것" has it: Stet measures the agent's writes as it.
	mustExec(t, superuser(t, pg), "grant "+account+" to stet_scram")
	db := cleanDatabase(t, pg)
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
	table := freshTable(t, admin, 2)
	mustExec(t, admin, "grant select, update on "+table+" to "+account)
	c := newCustody(t, pg, map[string]string{account: password})
	r, out, _ := startCustody(t, c.config(t, "shadow", pg, db, "tables:\n  "+table+": {pk: id}\n"))
	waitListening(t, r)

	conn := c.connect(t, r, db, account)
	if _, ok := conn.PgConn().Conn().(*tls.Conn); !ok {
		t.Errorf("pgx's connection to stet is a %T, not TLS", conn.PgConn().Conn())
	}
	var user string
	var ssl bool
	if err := conn.QueryRow(ctx, "select current_user, ssl from pg_stat_ssl where pid = pg_backend_pid()").Scan(&user, &ssl); err != nil {
		t.Fatal(err)
	}
	if user != account || !ssl {
		t.Errorf("pgx: current_user %q, TLS on the server %v; want %s, true", user, ssl, account)
	}
	if _, err := conn.Exec(ctx, "update "+table+" set name = 'pgx' where id = 1"); err != nil {
		t.Fatalf("pgx: update: %v; log:\n%s", err, r.log)
	}

	if _, err := c.psql(t, r.addr, db, account, "update "+table+" set name = 'psql' where id = 2"); err != nil {
		t.Fatalf("psql: update: %v; log:\n%s", err, r.log)
	}
	got, err := c.psql(t, r.addr, db, account, "select current_user, ssl from pg_stat_ssl where pid = pg_backend_pid()")
	if want := account + "|t"; err != nil || got != want {
		t.Errorf("psql: %q, %v; want %q", got, err, want)
	}

	waitDecisions(t, admin, table, 2)
	var roles []string
	if err := admin.QueryRow(ctx, "select array_agg(coalesce(login_role, 'NULL') order by id) from stet.decisions where target_table = $1",
		table).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if len(roles) != 2 || roles[0] != account || roles[1] != account {
		t.Errorf("login_role of the decisions %q, want %s twice", roles, account)
	}
	var names string
	if err := admin.QueryRow(ctx, "select string_agg(name, ',' order by id) from "+table).Scan(&names); err != nil || names != "pgx,psql" {
		t.Errorf("names %q, %v; want pgx,psql: shadow mode changes nothing", names, err)
	}
	if w := warnings(r.log.String()); len(w) > 0 {
		t.Errorf("warnings, want none:\n%s", strings.Join(w, "\n"))
	}
	return &custodyRun{r: r, out: out, c: c, decisions: admin}
}

// Started in shadow mode with custody over a gated table, without the GRANT
// of the agent account to Stet's: the log warns of it once, as enforce does.
func TestShadowCustodyWarnsOfAMissingMembership(t *testing.T) {
	pg := testPG(t)
	account, password := newAgentRole(t, pg)
	db := cleanDatabase(t, pg)
	table := freshTable(t, connectTo(t, pg, db, "stet_scram", "scram-pw"), 1)
	c := newCustody(t, pg, map[string]string{account: password})
	r, _, _ := startCustody(t, c.config(t, "shadow", pg, db, "tables:\n  "+table+": {pk: id}\n"))
	waitListening(t, r)
	want := fmt.Sprintf(`Stet's account \"stet_scram\" is not a member of agent role \"%s\"`, account)
	var told []string
	for _, w := range warnings(r.log.String()) {
		if strings.Contains(w, want) {
			told = append(told, w)
		}
	}
	if len(told) != 1 {
		t.Errorf("%d warnings that %s, want 1; log:\n%s", len(told), want, r.log)
	}
}

// A row changed through a session logged in with a token, on a table under
// undo.tables, is recorded in stet.history with the account as its agent and
// login role, linked to its decision; once Stet has stopped, stet.undo_plan
// and stet.undo restore it.
func TestUndoThroughACustodySession(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	account, password := newAgentRole(t, pg)
	mustExec(t, superuser(t, pg), "grant "+account+" to stet_scram")
	db := cleanDatabase(t, pg)
	pool := stetPoolIn(t, pg, db)
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
	table := freshTable(t, admin, 2)
	mustExec(t, admin, "grant select, update on "+table+" to "+account)
	captureOn(t, admin, table, table, "id")
	c := newCustody(t, pg, map[string]string{account: password})
	r, _, _ := startCustody(t, c.config(t, "shadow", pg, db, "tables:\n  "+table+": {pk: id}\nundo:\n  tables:\n    "+table+":\n"))
	waitListening(t, r)
	var since time.Time
	if err := admin.QueryRow(ctx, "select clock_timestamp()").Scan(&since); err != nil {
		t.Fatal(err)
	}

	write := "update " + table + " set name = 'changed' where id = 1"
	if _, err := c.connect(t, r, db, account).Exec(ctx, write); err != nil {
		t.Fatalf("the write: %v; log:\n%s", err, r.log)
	}
	d := decisionsRecorded(t, pool, table, 1, r.log)[0]
	l := lineFor(t, historyOn(t, pool, table), "1")
	if l.AgentRole != account || l.LoginRole != account || orNull(l.Query) != write || orNull(l.Name) != "changed" {
		t.Errorf("stet.history: agent_role %s, login_role %s, query %s, name %s; want %s, %s, %q, changed",
			l.AgentRole, l.LoginRole, orNull(l.Query), orNull(l.Name), account, account, write)
	}
	wantLinkedTo(t, "the change made through custody", l, d)
	if err := r.stop(t); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}

	outcomes := func(tx pgx.Tx, function string) []string {
		t.Helper()
		rows, err := tx.Query(ctx, "select key, action, outcome from stet."+function+"($1, $2)", account, since)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
			var key, action *string
			var outcome string
			err := r.Scan(&key, &action, &outcome)
			return fmt.Sprintf("%s %s %s", orNull(key), orNull(action), outcome), err
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	tx, err := admin.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if got, want := outcomes(tx, "undo_plan"), []string{"1 update would_restore"}; !slices.Equal(got, want) {
		t.Errorf("stet.undo_plan says %q, want %q", got, want)
	}
	if got, want := outcomes(tx, "undo"), []string{"1 update restored"}; !slices.Equal(got, want) {
		t.Errorf("stet.undo says %q, want %q", got, want)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := admin.QueryRow(ctx, "select name from "+table+" where id = 1").Scan(&name); err != nil || name != "n1" {
		t.Errorf("row 1 holds %q, %v after the undo; want n1, what it held before", name, err)
	}
}

func TestCustodyEnforce(t *testing.T) { custodyEnforce(t) }

// In enforce mode a session logged in with a token is gated as before: a
// write over the limit is refused with ST001 and runs once a reviewer
// approves it, and the stale check records a row the account read and then
// wrote as fresh.
func custodyEnforce(t *testing.T) *custodyRun {
	pg := testPG(t)
	ctx := context.Background()
	reviewer := newReviewer(t, pg)
	account, password := newAgentRole(t, pg)
	// Stet measures the agent's writes, and applies the approved ones, as it.
	mustExec(t, superuser(t, pg), "grant "+account+" to stet_scram")
	db := cleanDatabase(t, pg)
	admin := connectTo(t, pg, db, "stet_scram", "scram-pw")
	table := freshTable(t, admin, 3)
	mustExec(t, admin, "grant select, update on "+table+" to "+account)
	c := newCustody(t, pg, map[string]string{account: password})
	r, out, gateOf := startCustody(t, c.config(t, "enforce", pg, db, "tables:\n  "+table+
		":\n    pk: id\n    send_fields: [name]\n    stale: {}\n    impact: {max_rows: 1}\n"))
	agent := c.connect(t, r, db, account)

	readRow(t, agent, gateOf(), table, 1)
	write := "update " + table + " set name = 'mine' where id = 1"
	if _, err := agent.Exec(ctx, write); err != nil {
		t.Fatalf("the write of a row read: %v; log:\n%s", err, r.log)
	}
	result, _, executed := decisionOf(t, admin, write, r.log)
	if result == nil || *result != "fresh" || !executed {
		t.Errorf("read_check %s, executed %v; want fresh, true", orNull(result), executed)
	}

	_, err := agent.Exec(ctx, "update "+table+" set name = 'all'")
	id := heldWriteOf(t, err, r.log)
	rv := review(t, pg, db, reviewer)
	waitStatus(t, rv, id, "pending", 3*time.Second, r.log)
	// Its rows were planned, not counted: the reviewer says how many it may change.
	if _, err := rv.Exec(ctx, "select stet.approve($1, null, 3)", id); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, rv, id, "applied", 5*time.Second, r.log)
	var all int
	if err := admin.QueryRow(ctx, "select count(*) from "+table+" where name = 'all'").Scan(&all); err != nil || all != 3 {
		t.Errorf("%d rows say 'all', %v; want the approved write to have run on 3", all, err)
	}
	var others int
	if err := admin.QueryRow(ctx, "select count(*) from stet.decisions where login_role is distinct from $1", account).Scan(&others); err != nil || others != 0 {
		t.Errorf("%d decisions of another login_role than %s, %v; want none", others, account, err)
	}
	return &custodyRun{r: r, out: out, c: c, decisions: admin}
}

func TestTokenRotationNeedsNoRestart(t *testing.T) { tokenRotation(t) }

// A new token written to the file while Stet runs is the account's from its
// next login: the old one is refused and an open session goes on. An account
// added to the file after start is refused, with one warning.
func tokenRotation(t *testing.T) *custodyRun {
	pg := testPG(t)
	ctx := context.Background()
	account, password := newAgentRole(t, pg)
	c := newCustody(t, pg, map[string]string{account: password})
	r, out, _ := startCustody(t, c.config(t, "shadow", pg, "stet_test", ""))
	waitListening(t, r)
	open := c.connect(t, r, "stet_test", account)
	old := c.tokens[account]

	c.newToken(t, account)
	if _, err := open.Exec(ctx, "select 1"); err != nil {
		t.Errorf("the session opened with the old token: %v, want it to go on", err)
	}
	if conn, err := c.dial(r.addr, "stet_test", account, old); err == nil {
		conn.Close(ctx)
		t.Error("a new login with the old token succeeded")
	} else {
		wantRefused(t, err, account)
	}
	var user string
	if err := c.connect(t, r, "stet_test", account).QueryRow(ctx, "select current_user").Scan(&user); err != nil || user != account {
		t.Errorf("with the new token: current_user %q, %v; want %s", user, err, account)
	}
	if _, err := open.Exec(ctx, "select 2"); err != nil {
		t.Errorf("the session opened with the old token, after the new one logged in: %v", err)
	}

	added := fmt.Sprintf("stet_chk_added_%d", time.Now().UnixNano())
	c.add(t, added, secret(t))
	for range 2 {
		if conn, err := c.dial(r.addr, "stet_test", added, c.tokens[added]); err == nil {
			conn.Close(ctx)
			t.Error("an account added after start logged in")
		} else {
			wantRefused(t, err, added)
		}
	}
	var told []string
	for _, w := range warnings(r.log.String()) {
		if strings.Contains(w, added) && strings.Contains(w, "was added after stet started") {
			told = append(told, w)
		}
	}
	if len(told) != 1 {
		t.Errorf("%d warnings of the account added after start, want 1; log:\n%s", len(told), r.log)
	}
	return &custodyRun{r: r, out: out, c: c}
}

func TestStartCheckWarnsOfAnAccountThatCannotLogIn(t *testing.T) { startCheck(t) }

// At start Stet logs in once as each account: one whose stored password is
// wrong is warned of with the kind of failure, and Stet starts; the other
// account logs in.
func startCheck(t *testing.T) *custodyRun {
	pg := testPG(t)
	ctx := context.Background()
	good, goodPassword := newAgentRole(t, pg)
	bad, badPassword := newAgentRole(t, pg)
	c := newCustody(t, pg, map[string]string{good: goodPassword, bad: secret(t)})
	c.secrets = append(c.secrets, badPassword)
	r, out, _ := startCustody(t, c.config(t, "shadow", pg, "stet_test", ""))
	waitListening(t, r)

	var aboutBad, aboutGood []string
	for _, w := range warnings(r.log.String()) {
		if strings.Contains(w, "account="+bad) {
			aboutBad = append(aboutBad, w)
		}
		if strings.Contains(w, good) {
			aboutGood = append(aboutGood, w)
		}
	}
	if len(aboutBad) != 1 || !strings.Contains(aboutBad[0], "wrong password") {
		t.Errorf("warnings of %s %q, want one that says wrong password; log:\n%s", bad, aboutBad, r.log)
	}
	if len(aboutGood) != 0 {
		t.Errorf("warnings of %s, which can log in: %q", good, aboutGood)
	}

	var user string
	if err := c.connect(t, r, "stet_test", good).QueryRow(ctx, "select current_user").Scan(&user); err != nil || user != good {
		t.Errorf("current_user %q, %v; want %s", user, err, good)
	}
	conn, err := c.dial(r.addr, "stet_test", bad, c.tokens[bad])
	if err == nil {
		conn.Close(ctx)
		t.Fatalf("%s logged in with a wrong stored password", bad)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "08001" {
		t.Errorf("login as %s: %v, want 08001", bad, err)
	}
	return &custodyRun{r: r, out: out, c: c}
}

// silentServer accepts connections on loopback and never answers them, until
// the test ends. It returns its address.
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan net.Conn, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held <- c
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		for {
			select {
			case c := <-held:
				c.Close()
			default:
				return
			}
		}
	})
	return ln.Addr().String()
}

// The start's logins keep to their limits, one account's and all of them
// together, against a server that never answers: the account whose login
// runs out of its own time is warned of, the ones in progress or not reached
// when all the time is spent are named as not checked for want of time, and
// the proxy starts.
func TestStartCheckKeepsToItsTimeLimits(t *testing.T) {
	one, all := accountCheckTimeout, accountChecksTimeout
	accountCheckTimeout, accountChecksTimeout = time.Second, 1500*time.Millisecond
	t.Cleanup(func() { accountCheckTimeout, accountChecksTimeout = one, all })

	c := newCustody(t, "", map[string]string{"stet_chk_a": secret(t), "stet_chk_b": secret(t), "stet_chk_c": secret(t)})
	began := time.Now()
	r := start(t, parseConfig(t, "mode: shadow\nlisten: 127.0.0.1:0\nupstream: postgres://stet@%s/db?sslmode=disable\n"+
		"agents: {credentials: %q}\n", silentServer(t), c.credentials))
	waitListening(t, r)
	// Without the limit of all, the three would take 3s.
	if took := time.Since(began); took > 2500*time.Millisecond {
		t.Errorf("the start took %s, want about 1.5s", took)
	}

	var cannot, outOfTime []string
	for _, w := range warnings(r.log.String()) {
		switch {
		case strings.Contains(w, "cannot log in"):
			cannot = append(cannot, w)
		case strings.Contains(w, "ran out of time"):
			outOfTime = append(outOfTime, w)
		}
	}
	if len(cannot) != 1 || !strings.Contains(cannot[0], "account=stet_chk_a ") {
		t.Errorf("warnings of an account that cannot log in %q, want one, of stet_chk_a", cannot)
	}
	if len(outOfTime) != 1 || !strings.Contains(outOfTime[0], "[stet_chk_b stet_chk_c]") {
		t.Errorf("warnings of running out of time %q, want one naming stet_chk_b and stet_chk_c; log:\n%s", outOfTime, r.log)
	}
}

// The scenarios above, run with the log at debug level and decision lines
// on: no token, verifier or password is in the log, in the output, or in
// stet.decisions.
func TestNoSecretInAnyLog(t *testing.T) {
	for _, sc := range []struct {
		name string
		run  func(*testing.T) *custodyRun
	}{
		{"EndToEnd", custodyEndToEnd},
		{"Enforce", custodyEnforce},
		{"TokenRotation", tokenRotation},
		{"StartCheck", startCheck},
	} {
		t.Run(sc.name, func(t *testing.T) {
			cr := sc.run(t)
			if err := cr.r.stop(t); err != nil {
				t.Fatalf("run = %v, want nil", err)
			}
			texts := map[string]string{"log": cr.r.log.String(), "standard output": cr.out.String()}
			if cr.decisions != nil {
				var rows []string
				if err := cr.decisions.QueryRow(context.Background(),
					"select coalesce(array_agg(d::text), '{}') from stet.decisions d").Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if len(rows) == 0 {
					t.Error("stet.decisions holds nothing to look at")
				}
				texts["stet.decisions"] = strings.Join(rows, "\n")
			}
			for where, text := range texts {
				for _, s := range append(cr.c.secrets, "scram-pw") {
					if strings.Contains(text, s) {
						t.Errorf("the %s shows the secret %q:\n%s", where, s, text)
					}
				}
			}
		})
	}
}

// One account on one side only, no account, and a file others can read:
// Stet does not start.
func TestRolesMustMatchTheFile(t *testing.T) {
	const upstream = "upstream: postgres://stet@127.0.0.1:1/db?sslmode=disable\n"
	t.Run("OnlyOnOneSide", func(t *testing.T) {
		for _, mode := range []string{"shadow", "enforce"} {
			t.Run(mode, func(t *testing.T) {
				c := newCustody(t, "", map[string]string{"stet_chk_both": secret(t), "stet_chk_file_only": secret(t)})
				r := start(t, parseConfig(t, "mode: %s\nlisten: 127.0.0.1:0\n"+upstream+
					"agents: {roles: [stet_chk_both, stet_chk_roles_only], credentials: %q}\n", mode, c.credentials))
				err := refusedToStart(t, r)
				want := `agents.roles and the credentials file name different accounts: only in agents.roles: ["stet_chk_roles_only"]; only in the file: ["stet_chk_file_only"]`
				if err.Error() != want {
					t.Errorf("run = %q, want %q", err, want)
				}
			})
		}
	})
	t.Run("NoAccount", func(t *testing.T) {
		c := newCustody(t, "", nil)
		r := start(t, parseConfig(t, "mode: shadow\nlisten: 127.0.0.1:0\n"+upstream+"agents: {credentials: %q}\n", c.credentials))
		if err := refusedToStart(t, r); err.Error() != "the credentials file has no account; add one with stet agent add" {
			t.Errorf("run = %q, want the credentials file has no account", err)
		}
	})
	t.Run("OpenToOthers", func(t *testing.T) {
		c := newCustody(t, "", map[string]string{"stet_chk_open": secret(t)})
		if err := os.Chmod(c.credentials, 0o644); err != nil {
			t.Fatal(err)
		}
		r := start(t, parseConfig(t, "mode: shadow\nlisten: 127.0.0.1:0\n"+upstream+"agents: {credentials: %q}\n", c.credentials))
		if err := refusedToStart(t, r); !strings.Contains(err.Error(), "mode 0644 is too open") {
			t.Errorf("run = %q, want the file's mode refused", err)
		}
	})

	// An account that is Stet's own would get a session no gate sees, as the
	// owner of the stet schema. With no gated table the start's checks of
	// the agent roles do not run, so the file is checked against the user
	// Stet connects as, wherever that user comes from.
	const own = `agents.credentials: "stet_chk_own" is Stet's own account (the upstream user)`
	for _, tc := range []struct {
		name, upstream, roles string
		env                   string // PGUSER
	}{
		{"OwnAccount", "postgres://stet_chk_own@127.0.0.1:1/db?sslmode=disable", "", ""},
		{"OwnAccountListed", "postgres://127.0.0.1:1/db?user=stet_chk_own&sslmode=disable", "roles: [stet_chk_other, stet_chk_own], ", ""},
		{"OwnAccountFromPGUSER", "postgres://127.0.0.1:1/db?sslmode=disable", "", "stet_chk_own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("PGUSER", tc.env)
			}
			for _, mode := range []string{"shadow", "enforce"} {
				t.Run(mode, func(t *testing.T) {
					c := newCustody(t, "", map[string]string{"stet_chk_own": secret(t), "stet_chk_other": secret(t)})
					r := start(t, parseConfig(t, "mode: %s\nlisten: 127.0.0.1:0\nupstream: %s\nagents: {%scredentials: %q}\n",
						mode, tc.upstream, tc.roles, c.credentials))
					if err := refusedToStart(t, r); err.Error() != own {
						t.Errorf("run = %q, want %q", err, own)
					}
				})
			}
		})
	}
}

// askForTLS sends addr an SSLRequest, as a client does, and returns the
// connection and the one byte answered.
func askForTLS(t *testing.T, addr string) (net.Conn, byte) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(10 * time.Second))
	var req [8]byte
	binary.BigEndian.PutUint32(req[0:4], 8)
	binary.BigEndian.PutUint32(req[4:8], 80877103)
	if _, err := c.Write(req[:]); err != nil {
		t.Fatal(err)
	}
	var answer [1]byte
	if _, err := io.ReadFull(c, answer[:]); err != nil {
		t.Fatal(err)
	}
	return c, answer[0]
}

// servedCertificate is the certificate Stet at addr presents to a client
// that asks for TLS, unchecked.
func servedCertificate(t *testing.T, r *running) []byte {
	t.Helper()
	c, answer := askForTLS(t, r.addr)
	if answer != 'S' {
		t.Fatalf("stet answered %q to an SSLRequest, want S", answer)
	}
	tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v; log:\n%s", err, r.log)
	}
	return tc.ConnectionState().PeerCertificates[0].Raw
}

// certificateIn is the certificate of the PEM file at path.
func certificateIn(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatalf("%s holds no PEM", path)
	}
	return block.Bytes
}

// replaceFile puts b at path as a new file (a temporary file renamed over
// it), so that the change shows in the file's identity whatever the
// resolution of modification times.
func replaceFile(t *testing.T, path string, b []byte) {
	t.Helper()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".replace-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}
}

// copyFile is replaceFile with the bytes of from.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	replaceFile(t, to, b)
}

// reloadErrors counts the errors of listen_tls in r's log.
func reloadErrors(r *running) int {
	n := 0
	for line := range strings.SplitSeq(r.log.String(), "\n") {
		if strings.Contains(line, "level=ERROR") && strings.Contains(line, "listen_tls") {
			n++
		}
	}
	return n
}

func listenTLSConfig(t *testing.T, certPath, keyPath string) *config.Config {
	t.Helper()
	return parseConfig(t, "mode: shadow\nlisten: 127.0.0.1:0\nlisten_tls: {cert: %q, key: %q}\n"+
		"upstream: postgres://stet@127.0.0.1:1/db?sslmode=disable\n", certPath, keyPath)
}

func TestCertificateReload(t *testing.T) {
	t.Run("NextHandshake", func(t *testing.T) {
		certPath, keyPath, _ := pgtest.LocalCert(t)
		r := start(t, listenTLSConfig(t, certPath, keyPath))
		waitListening(t, r)
		first := certificateIn(t, certPath)
		// step is one change to the files, then a handshake: it wants the
		// certificate served, and the errors logged so far.
		step := func(what string, served []byte, logged int) {
			t.Helper()
			if got := servedCertificate(t, r); !bytes.Equal(got, served) {
				t.Errorf("%s: stet serves another certificate than it should; log:\n%s", what, r.log)
			}
			if got := reloadErrors(r); got != logged {
				t.Errorf("%s: %d errors of listen_tls logged, want %d; log:\n%s", what, got, logged, r.log)
			}
		}
		step("at start", first, 0)

		// A pair replaced one file at a time: in between, the certificate
		// does not match the key, and the one in use is kept.
		otherCert, otherKey, _ := pgtest.LocalCert(t)
		second := certificateIn(t, otherCert)
		copyFile(t, otherCert, certPath)
		step("the certificate replaced, not yet the key", first, 1)
		copyFile(t, otherKey, keyPath)
		step("the key replaced too", second, 1)

		// Each failed reload of a change is logged.
		replaceFile(t, certPath, []byte("not a certificate\n"))
		step("the certificate broken", second, 2)
		replaceFile(t, certPath, []byte("still not a certificate\n"))
		step("the certificate broken again", second, 3)

		// A file that is not there is logged once while it stays away.
		if err := os.Remove(certPath); err != nil {
			t.Fatal(err)
		}
		step("the certificate removed", second, 4)
		step("the certificate still removed", second, 4)
		thirdCert, thirdKey, _ := pgtest.LocalCert(t)
		copyFile(t, thirdKey, keyPath)
		copyFile(t, thirdCert, certPath)
		step("a new pair in place", certificateIn(t, thirdCert), 4)
	})

	t.Run("MismatchAtStart", func(t *testing.T) {
		certPath, _, _ := pgtest.LocalCert(t)
		_, keyPath, _ := pgtest.LocalCert(t)
		err := refusedToStart(t, start(t, listenTLSConfig(t, certPath, keyPath)))
		if !strings.Contains(err.Error(), "listen_tls") {
			t.Errorf("run = %q, want an error naming listen_tls", err)
		}
	})

	t.Run("TLS12AtLeast", func(t *testing.T) {
		certPath, keyPath, _ := pgtest.LocalCert(t)
		cfg, err := certReloader(certPath, keyPath, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion %#x, want TLS 1.2", cfg.MinVersion)
		}
	})
}

// A key file that users other than its owner can read is warned of; one
// only its owner can is not.
func TestWarnsOfAKeyOthersCanRead(t *testing.T) {
	for mode, warned := range map[os.FileMode]bool{0o600: false, 0o640: true, 0o604: true} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			certPath, keyPath, _ := pgtest.LocalCert(t)
			if err := os.Chmod(keyPath, mode); err != nil {
				t.Fatal(err)
			}
			r := start(t, listenTLSConfig(t, certPath, keyPath))
			waitListening(t, r)
			var got []string
			for _, w := range warnings(r.log.String()) {
				if strings.Contains(w, keyPath) {
					got = append(got, w)
				}
			}
			if warned && len(got) != 1 || !warned && len(got) != 0 {
				t.Errorf("warnings of the key file %q, want warned: %v", got, warned)
			}
		})
	}
}

func TestListeningLineSaysCustody(t *testing.T) {
	c := newCustody(t, "", map[string]string{"stet_chk_line": secret(t)})
	const base = "mode: shadow\nlisten: 127.0.0.1:0\nupstream: postgres://stet@127.0.0.1:1/db?sslmode=disable\n"
	listenTLS := fmt.Sprintf("listen_tls: {cert: %q, key: %q}\n", c.certPath, c.keyPath)
	for _, tc := range []struct{ name, yaml, want string }{
		{"Both", base + listenTLS + fmt.Sprintf("agents: {credentials: %q}\n", c.credentials), "listen_tls=on custody=on"},
		{"ListenTLSOnly", base + listenTLS, "listen_tls=on custody=off"},
		{"Neither", base, "listen_tls=off custody=off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := start(t, parseConfig(t, "%s", tc.yaml))
			waitListening(t, r)
			if line := listeningLine(t, r); !strings.HasSuffix(line, " "+tc.want) {
				t.Errorf("the start line %q, want it to end with %s", line, tc.want)
			}
		})
	}
}

func TestWarnsOfTLSOnBothSidesWithoutCustody(t *testing.T) {
	const scramWarning = "SCRAM accounts cannot log in through stet with TLS on both sides unless the client sets " +
		"channel_binding=disable; fix: add agents.credentials, or reach upstream without TLS on a private network"
	certPath, keyPath, _ := pgtest.LocalCert(t)
	listenTLS := fmt.Sprintf("listen_tls: {cert: %q, key: %q}\n", certPath, keyPath)
	const credentials = "agents: {credentials: /nonexistent/agents.yaml}\n"
	for _, tc := range []struct {
		name, listen, sslmode, rest string
		want                        []string // a fragment of each warning, in order
	}{
		{"require", "127.0.0.1:6432", "require", listenTLS, []string{scramWarning}},
		{"prefer", "127.0.0.1:6432", "prefer", listenTLS, []string{scramWarning}},
		{"verify-full", "127.0.0.1:6432", "verify-full", listenTLS, []string{scramWarning}},
		{"upstream plaintext", "127.0.0.1:6432", "disable", listenTLS, nil},
		{"no listen_tls", "127.0.0.1:6432", "require", "", nil},
		{"custody", "127.0.0.1:6432", "require", listenTLS + credentials, nil},
		// With listen_tls a client from elsewhere must use TLS: the listen
		// address is no warning.
		{"listen on all", "0.0.0.0:6432", "disable", listenTLS, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseConfig(t, "mode: shadow\nlisten: %q\nupstream: postgres://stet@127.0.0.1/d?sslmode=%s\n%s", tc.listen, tc.sslmode, tc.rest)
			got := transportWarnings(cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("warnings = %q, want %d of them (%q)", got, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("warning %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}

	t.Run("Logged", func(t *testing.T) {
		r := start(t, parseConfig(t, "mode: shadow\nlisten: 127.0.0.1:0\nupstream: postgres://stet@127.0.0.1:1/d?sslmode=require\n%s", listenTLS))
		waitListening(t, r)
		if w := warnings(r.log.String()); len(w) != 1 || !strings.Contains(w[0], scramWarning) {
			t.Errorf("warnings %q, want the one of TLS on both sides", w)
		}
	})
}

// Without listen_tls and agents.credentials the start line has the keys and
// values it had before, and a client asking for TLS is told N.
func TestWithoutCustodyNothingChanges(t *testing.T) {
	r := start(t, parseConfig(t, "mode: shadow\nlisten: 127.0.0.1:0\nupstream: postgres://stet@127.0.0.1:1/db?sslmode=disable\n"+
		"agents: {roles: [stet_chk_agent]}\nmodel: {adapter: none}\n"))
	waitListening(t, r)
	before := ` msg="stet proxy listening" listen=` + r.addr +
		` upstream=127.0.0.1:1 upstream_tls=off mode=shadow agent_roles=[stet_chk_agent] gated_tables=0 model=none`
	if _, line, _ := strings.Cut(listeningLine(t, r), " level=INFO"); line != before+" listen_tls=off custody=off" {
		t.Errorf("the start line ends %q, want %q followed by listen_tls=off custody=off", line, before)
	}
	if _, answer := askForTLS(t, r.addr); answer != 'N' {
		t.Errorf("stet answered %q to an SSLRequest, want N", answer)
	}
}

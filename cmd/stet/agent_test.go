package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/custody"
	"github.com/stetdb/stet/internal/pgtest"
	"github.com/stetdb/stet/internal/scram"
)

// agentNow is the clock of testIO: not UTC, and with nanoseconds, which the
// file does not keep.
var agentNow = time.Date(2026, 11, 22, 22, 2, 11, 500, time.FixedZone("KST", 9*60*60))

// agentNowUTC is agentNow as the file keeps it.
var agentNowUTC = time.Date(2026, 11, 22, 13, 2, 11, 0, time.UTC)

// testIO is an agentIO with stdin as standard input (not a terminal), env as
// the environment, no secret function and agentNow as the clock; out is what
// it prints.
func testIO(stdin io.Reader, env map[string]string) (a agentIO, out *bytes.Buffer) {
	out = &bytes.Buffer{}
	return agentIO{
		stdin:  stdin,
		stdout: out,
		env:    func(k string) string { return env[k] },
		now:    func() time.Time { return agentNow },
	}, out
}

// agentRun runs stet agent with args, stdin as its standard input and env as
// its environment, and returns what it printed and its error.
func agentRun(args []string, stdin string, env map[string]string) (string, error) {
	a, out := testIO(strings.NewReader(stdin), env)
	err := runAgent(args, a)
	return out.String(), err
}

// unread is a standard input that fails the test if stet agent reads it.
type unread struct{ t *testing.T }

func (u unread) Read([]byte) (int, error) {
	u.t.Error("stet agent read standard input")
	return 0, io.EOF
}

// noSecret is a secret function that fails the test if stet agent calls it.
func noSecret(t *testing.T) func(string) (string, error) {
	return func(string) (string, error) {
		t.Error("stet agent asked for a secret")
		return "", errors.New("no secret here")
	}
}

// asking is an agentIO that may neither read standard input nor ask for a
// secret, on a terminal: what a command that should stop before asking
// anything is given.
func asking(t *testing.T, env map[string]string) (agentIO, *bytes.Buffer) {
	a, out := testIO(unread{t}, env)
	a.terminal, a.secret = true, noSecret(t)
	return a, out
}

// agentCase is a config file for stet agent in a directory of the test's
// own, whose agents.credentials is agents.yaml next to it (not written yet).
// Over the test server pg it reaches Stet's own account at
// sslmode=verify-full, with listen_tls, as custodyCase.config has it; with
// pg "", an upstream that is not there, in plaintext.
type agentCase struct {
	*custodyCase
	cfgPath  string
	upstream string
}

func newAgentCase(t *testing.T, pg string) *agentCase {
	t.Helper()
	dir := t.TempDir()
	c := &custodyCase{credentials: filepath.Join(dir, "agents.yaml"), tokens: map[string]string{}}
	c.certPath, c.keyPath, _ = pgtest.LocalCert(t)
	upstream := "postgres://stet_scram:scram-pw@127.0.0.1:1/stet_test?sslmode=disable"
	if pg != "" {
		_, c.upstreamRoot = pgtest.ServerCert(t, pg)
		u := url.URL{
			Scheme: "postgres", Host: pg, Path: "/stet_test", User: url.UserPassword("stet_scram", "scram-pw"),
			RawQuery: url.Values{"sslmode": {"verify-full"}, "sslrootcert": {c.upstreamRoot}}.Encode(),
		}
		upstream = u.String()
	}
	ac := &agentCase{custodyCase: c, cfgPath: filepath.Join(dir, "stet.yaml"), upstream: upstream}
	writeConfig(t, ac.cfgPath, fmt.Sprintf("mode: shadow\nlisten: 127.0.0.1:0\nlisten_tls: {cert: %q, key: %q}\n"+
		"agents: {credentials: agents.yaml}\nupstream: %s\nmodel: {adapter: none, on_failure: pass}\n", c.certPath, c.keyPath, upstream))
	return ac
}

func writeConfig(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// run is agentRun with -config ac's config after the command, args[0].
func (ac *agentCase) run(args []string, stdin string, env map[string]string) (string, error) {
	return agentRun(ac.args(args...), stdin, env)
}

// args is args with -config ac's config after the command, args[0].
func (ac *agentCase) args(args ...string) []string {
	return append([]string{args[0], "-config", ac.cfgPath}, args[1:]...)
}

// seed writes the accounts of passwords to ac's credentials file, each with
// a new token, as stet agent add does but without logging in.
func (ac *agentCase) seed(t *testing.T, passwords map[string]string) {
	t.Helper()
	f := &custody.File{Agents: map[string]custody.Agent{}}
	if _, err := os.Stat(ac.credentials); err == nil {
		f = readFile(t, ac.credentials)
	}
	for account, password := range passwords {
		f.Agents[account] = ac.agent(t, account, password)
	}
	if err := custody.Write(ac.credentials, f); err != nil {
		t.Fatal(err)
	}
}

// start serves ac's config with the credentials file as it is now.
func (ac *agentCase) start(t *testing.T) *running {
	t.Helper()
	cfg, err := config.Load(ac.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	r, _, _ := startCustody(t, cfg)
	waitListening(t, r)
	return r
}

// wantLogin fails the test unless account logs in to Stet r with token, as
// itself.
func (ac *agentCase) wantLogin(t *testing.T, r *running, account, token string) {
	t.Helper()
	conn, err := ac.dial(r.addr, "stet_test", account, token)
	if err != nil {
		t.Fatalf("log in to stet as %s with the token stet agent printed: %v; log:\n%s", account, err, r.log)
	}
	defer conn.Close(context.Background())
	var user string
	if err := conn.QueryRow(context.Background(), "select current_user").Scan(&user); err != nil || user != account {
		t.Errorf("current_user %q, %v; want %s", user, err, account)
	}
}

// wantRefusedLogin fails the test unless Stet r refuses account's login
// with token as PostgreSQL refuses a wrong password.
func (ac *agentCase) wantRefusedLogin(t *testing.T, r *running, account, token string) {
	t.Helper()
	conn, err := ac.dial(r.addr, "stet_test", account, token)
	if err == nil {
		conn.Close(context.Background())
		t.Fatalf("%s logged in to stet, want it refused", account)
	}
	wantRefused(t, err, account)
}

// readFile is the credentials file at path, which must be there.
func readFile(t *testing.T, path string) *custody.File {
	t.Helper()
	f, err := custody.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var tokenShape = regexp.MustCompile(`stet_[A-Za-z0-9_-]{43}`)

// tokenIn is the one token in out.
func tokenIn(t *testing.T, out string) string {
	t.Helper()
	m := tokenShape.FindAllString(out, -1)
	if len(m) != 1 {
		t.Fatalf("%d tokens in the output, want 1:\n%s", len(m), out)
	}
	return m[0]
}

// tokenBlock is what add and token print first.
func tokenBlock(account, token string) string {
	return fmt.Sprintf("token for %q (shown once; stet keeps only what it needs to check it):\n  %s\n", account, token)
}

// addOutput is what add prints for account and token: config is the
// -config given ("" when none), and bullets are the lines it adds to next
// before the restart, each without its "  - ".
func addOutput(account, token, config string, bullets ...string) string {
	command := "stet agent password " + account
	if config != "" {
		command = "stet agent password -config " + config + " " + account
	}
	s := tokenBlock(account, token) + "\nnext:\n" +
		fmt.Sprintf("  - in the agent's connection string, put this server's address as the host and the token as the password; the user stays %q\n", account) +
		"  - if anything else knew this account's database password, change it in the database, then run: " + command + "\n"
	for _, b := range bullets {
		s += "  - " + b + "\n"
	}
	return s + "  - restart stet proxy: an account added while it runs is not accepted\n"
}

// envBullet is add's line of next for -password-env name.
func envBullet(name string) string { return "stet proxy must have " + name + " in its environment" }

// The lines that say what stet proxy will refuse to start with.
func rolesAddBullet(account string) string {
	return fmt.Sprintf("add %q to agents.roles in the config: it must list the same accounts as the credentials file, or stet proxy will not start", account)
}

func rolesRemoveLine(account string) string {
	return fmt.Sprintf("remove %q from agents.roles in the config: it must list the same accounts as the credentials file, or stet proxy will not start\n", account)
}

const noAccountLeft = "the credentials file has no account left: stet proxy will not start until one is added\n"

func removeOutput(account string) string {
	return fmt.Sprintf("%q removed; it is refused from the next login\n", account)
}

// setRoles writes agents.roles into ac's config.
func (ac *agentCase) setRoles(t *testing.T, roles ...string) {
	t.Helper()
	b := fileBytes(t, ac.cfgPath)
	quoted := make([]string, len(roles))
	for i, r := range roles {
		quoted[i] = strconv.Quote(r)
	}
	text := strings.Replace(string(b), "agents: {credentials: agents.yaml}",
		"agents: {credentials: agents.yaml, roles: ["+strings.Join(quoted, ", ")+"]}", 1)
	writeConfig(t, ac.cfgPath, text)
}

func tokenOutput(account, token string) string {
	return tokenBlock(account, token) + "\nthe old token stops working at the next login; no restart is needed\n"
}

// fileSecrets is what of the credentials file at path no output may show:
// each account's verifier, its salt and keys, and its password. Nothing
// when there is no file.
func fileSecrets(t *testing.T, path string) []string {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	var out []string
	for _, a := range readFile(t, path).Agents {
		_, rest, _ := strings.Cut(a.Token, ":") // <salt>$<StoredKey>:<ServerKey>
		salt, keys, _ := strings.Cut(rest, "$") // <StoredKey>:<ServerKey>
		stored, server, _ := strings.Cut(keys, ":")
		out = append(out, a.Token, salt, stored, server)
		if a.Password != "" {
			out = append(out, a.Password)
		}
	}
	return out
}

func TestUsageNamesAgent(t *testing.T) {
	if !strings.Contains(usage, "\n  agent ") {
		t.Errorf("stet's usage has no agent command:\n%s", usage)
	}
}

func TestAgentAdd(t *testing.T) {
	pg := testPG(t)
	account, password := newAgentRole(t, pg)
	ac := newAgentCase(t, pg)
	out, err := ac.run([]string{"add", account}, password+"\n", nil)
	if err != nil {
		t.Fatalf("add: %v; output:\n%s", err, out)
	}
	token := tokenIn(t, out)
	if want := addOutput(account, token, ac.cfgPath); out != want {
		t.Errorf("add printed:\n%s\nwant:\n%s", out, want)
	}
	if n := strings.Count(out, token); n != 1 {
		t.Errorf("the token is in the output %d times, want once", n)
	}

	fi, err := os.Stat(ac.credentials)
	if err != nil {
		t.Fatalf("the credentials file (agents.credentials, relative to the config): %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the credentials file has mode %04o, want 0600", perm)
	}
	f := readFile(t, ac.credentials)
	a, ok := f.Agents[account]
	if !ok || len(f.Agents) != 1 {
		t.Fatalf("the file holds %d accounts, want only %s", len(f.Agents), account)
	}
	if a.Password != password || a.PasswordEnv != "" {
		t.Error("the file does not hold the account's password, and only it")
	}
	if _, err := scram.ParseVerifier(a.Token); err != nil {
		t.Errorf("the account's token is not a verifier: %v", err)
	}
	if bytes.Contains(fileBytes(t, ac.credentials), []byte(token)) {
		t.Error("the file holds the token itself")
	}
	if !a.Added.Equal(agentNowUTC) || !a.TokenIssued.Equal(agentNowUTC) ||
		a.Added.Location() != time.UTC || a.TokenIssued.Location() != time.UTC {
		t.Errorf("added %s, token issued %s; want both %s", a.Added, a.TokenIssued, agentNowUTC)
	}
	if f.MockSecret == "" {
		t.Error("the file has no mock_secret")
	}

	// Without -config the command it suggests has none either: it reads the
	// same default, stet.yaml in the directory it is run in.
	other, otherPassword := newAgentRole(t, pg)
	t.Chdir(filepath.Dir(ac.cfgPath))
	out, err = agentRun([]string{"add", other}, otherPassword+"\n", nil)
	if err != nil {
		t.Fatalf("add without -config: %v; output:\n%s", err, out)
	}
	if want := addOutput(other, tokenIn(t, out), ""); out != want {
		t.Errorf("add without -config printed:\n%s\nwant:\n%s", out, want)
	}
	if readFile(t, ac.credentials).MockSecret != f.MockSecret {
		t.Error("the second add changed mock_secret")
	}

	ac.wantLogin(t, ac.start(t), account, token)
}

func TestAgentAddRefusesAWrongPassword(t *testing.T) {
	pg := testPG(t)
	account, password := newAgentRole(t, pg)
	wantWrong := func(t *testing.T, out string, err error) {
		t.Helper()
		want := fmt.Sprintf("the database refused that password for %q, so nothing was written: upstream login: wrong password: ", account)
		if err == nil || !strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), "28P01") {
			t.Errorf("add = %v, want an error that starts %q, with the server's code", err, want)
		}
		if out != "" {
			t.Errorf("add printed %q, want nothing", out)
		}
	}

	t.Run("NoFile", func(t *testing.T) {
		ac := newAgentCase(t, pg)
		out, err := ac.run([]string{"add", account}, "wrong-"+password+"\n", nil)
		wantWrong(t, out, err)
		if entries, _ := os.ReadDir(filepath.Dir(ac.credentials)); len(entries) != 1 {
			t.Errorf("the directory holds %v, want only the config", entries)
		}
	})

	t.Run("FileThere", func(t *testing.T) {
		ac := newAgentCase(t, pg)
		ac.seed(t, map[string]string{"stet_chk_other": secret(t)})
		before := fileBytes(t, ac.credentials)
		out, err := ac.run([]string{"add", account}, "wrong-"+password+"\n", nil)
		wantWrong(t, out, err)
		if !bytes.Equal(fileBytes(t, ac.credentials), before) {
			t.Error("the credentials file changed")
		}
		if entries, _ := os.ReadDir(filepath.Dir(ac.credentials)); len(entries) != 2 {
			t.Errorf("the directory holds %v, want only the config and the credentials file", entries)
		}
	})
}

func TestAgentAddPasswordEnv(t *testing.T) {
	pg := testPG(t)
	account, password := newAgentRole(t, pg)
	ac := newAgentCase(t, pg)
	name := "STET_CHK_PASSWORD_" + strconv.FormatInt(time.Now().UnixNano(), 10)

	t.Run("Empty", func(t *testing.T) {
		for _, env := range []map[string]string{nil, {name: ""}} {
			a, out := asking(t, env)
			err := runAgent(ac.args("add", "-password-env", name, account), a)
			if want := "-password-env: " + name + " is empty or not set in this environment"; err == nil || err.Error() != want {
				t.Errorf("add = %v, want %q", err, want)
			}
			if out.Len() != 0 {
				t.Errorf("add printed %q, want nothing", out)
			}
			wantNoFile(t, ac.credentials)
		}
	})

	a, out := asking(t, map[string]string{name: password})
	if err := runAgent(ac.args("add", "-password-env", name, account), a); err != nil {
		t.Fatalf("add: %v; output:\n%s", err, out)
	}
	token := tokenIn(t, out.String())
	if want := addOutput(account, token, ac.cfgPath, envBullet(name)); out.String() != want {
		t.Errorf("add printed:\n%s\nwant:\n%s", out, want)
	}
	e := readFile(t, ac.credentials).Agents[account]
	if e.PasswordEnv != name || e.Password != "" {
		t.Errorf("the file has password_env %q and a password: %v; want %s and none", e.PasswordEnv, e.Password != "", name)
	}
	if bytes.Contains(fileBytes(t, ac.credentials), []byte(password)) {
		t.Error("the file holds the variable's value")
	}

	t.Setenv(name, password) // stet proxy reads it at start
	ac.wantLogin(t, ac.start(t), account, token)
}

func TestAgentToken(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	account, password := newAgentRole(t, pg)
	ac := newAgentCase(t, pg)
	out, err := ac.run([]string{"add", account}, password+"\n", nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	old := tokenIn(t, out)
	oldVerifier := readFile(t, ac.credentials).Agents[account].Token
	r := ac.start(t)
	open, err := ac.dial(r.addr, "stet_test", account, old)
	if err != nil {
		t.Fatalf("log in with the token add printed: %v; log:\n%s", err, r.log)
	}
	defer open.Close(ctx)

	a, buf := asking(t, nil)
	a.now = func() time.Time { return agentNow.Add(time.Hour) }
	if err := runAgent(ac.args("token", account), a); err != nil {
		t.Fatalf("token: %v; output:\n%s", err, buf)
	}
	token := tokenIn(t, buf.String())
	if token == old {
		t.Fatal("token printed the token add printed")
	}
	if want := tokenOutput(account, token); buf.String() != want {
		t.Errorf("token printed:\n%s\nwant:\n%s", buf, want)
	}
	if bytes.Contains(fileBytes(t, ac.credentials), []byte(oldVerifier)) {
		t.Error("the file still holds the old verifier")
	}
	e := readFile(t, ac.credentials).Agents[account]
	if e.Password != password || !e.Added.Equal(agentNowUTC) || !e.TokenIssued.Equal(agentNowUTC.Add(time.Hour)) {
		t.Errorf("added %s, token issued %s, the password kept: %v; want %s, %s, true",
			e.Added, e.TokenIssued, e.Password == password, agentNowUTC, agentNowUTC.Add(time.Hour))
	}

	// No restart: the old token is refused from the next login, the new one
	// logs in, and the session opened with the old one goes on.
	ac.wantRefusedLogin(t, r, account, old)
	ac.wantLogin(t, r, account, token)
	if _, err := open.Exec(ctx, "select 1"); err != nil {
		t.Errorf("the session opened with the old token: %v, want it to go on", err)
	}
}

func TestAgentPassword(t *testing.T) {
	pg := testPG(t)
	account, password := newAgentRole(t, pg)
	ac := newAgentCase(t, pg)
	out, err := ac.run([]string{"add", account}, password+"\n", nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	token := tokenIn(t, out)
	verifier := readFile(t, ac.credentials).Agents[account].Token
	// Another account, which no password command here may touch, nor the
	// file's mock_secret.
	ac.seed(t, map[string]string{"stet_chk_bystander": secret(t)})
	first := readFile(t, ac.credentials)
	untouched := func(t *testing.T) {
		t.Helper()
		f := readFile(t, ac.credentials)
		if f.MockSecret != first.MockSecret || f.Agents["stet_chk_bystander"] != first.Agents["stet_chk_bystander"] {
			t.Error("password changed mock_secret or another account's entry")
		}
	}
	r := ac.start(t)

	// The operator changes the password in the database: until stet agent
	// password, Stet cannot log in as the account.
	changed := secret(t)
	mustExec(t, superuser(t, pg), "alter role "+account+" password '"+changed+"'")
	if conn, err := ac.dial(r.addr, "stet_test", account, token); err == nil {
		conn.Close(context.Background())
		t.Fatal("logged in through stet with the old password stored")
	} else if sqlstate(err) != "08001" {
		t.Fatalf("login with the old password stored: %v, want 08001", err)
	}

	t.Run("Wrong", func(t *testing.T) {
		before := fileBytes(t, ac.credentials)
		out, err := ac.run([]string{"password", account}, "wrong-"+changed+"\n", nil)
		want := fmt.Sprintf("the database refused that password for %q, so nothing was written: upstream login: wrong password: ", account)
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("password = %v, want an error that starts %q", err, want)
		}
		if out != "" {
			t.Errorf("password printed %q, want nothing", out)
		}
		if !bytes.Equal(fileBytes(t, ac.credentials), before) {
			t.Error("the credentials file changed")
		}
	})

	out, err = ac.run([]string{"password", account}, changed+"\n", nil)
	if err != nil {
		t.Fatalf("password: %v", err)
	}
	if want := fmt.Sprintf("password for %q updated; no restart is needed\n", account); out != want {
		t.Errorf("password printed %q, want %q", out, want)
	}
	e := readFile(t, ac.credentials).Agents[account]
	if e.Password != changed || e.Token != verifier || !e.TokenIssued.Equal(agentNowUTC) {
		t.Errorf("the new password kept: %v, the token kept: %v, token issued %s; want true, true, %s",
			e.Password == changed, e.Token == verifier, e.TokenIssued, agentNowUTC)
	}
	untouched(t)
	ac.wantLogin(t, r, account, token) // no restart

	// -password-env moves the account's password to a variable of stet
	// proxy's environment, once its value here logs in.
	name := "STET_CHK_PASSWORD_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	a, buf := asking(t, map[string]string{name: changed})
	if err := runAgent(ac.args("password", "-password-env", name, account), a); err != nil {
		t.Fatalf("password -password-env: %v", err)
	}
	if want := fmt.Sprintf("%q now takes its password from %s; stet proxy must have %s in its environment: restart stet proxy\n",
		account, name, name); buf.String() != want {
		t.Errorf("password -password-env printed %q, want %q", buf, want)
	}
	e = readFile(t, ac.credentials).Agents[account]
	if e.PasswordEnv != name || e.Password != "" || e.Token != verifier {
		t.Errorf("password_env %q, a password: %v, the token kept: %v; want %s, false, true", e.PasswordEnv, e.Password != "", e.Token == verifier, name)
	}
	untouched(t)

	// An account whose password is a variable's: the file is not changed, and
	// the error, which a script does not take for success, says where to
	// change it.
	before := fileBytes(t, ac.credentials)
	a, buf = asking(t, nil)
	err = runAgent(ac.args("password", account), a)
	if want := fmt.Sprintf("%q takes its password from %s; change it in stet proxy's environment and restart stet proxy",
		account, name); err == nil || err.Error() != want {
		t.Errorf("password = %v, want the error %q", err, want)
	}
	if buf.Len() != 0 {
		t.Errorf("password printed %q, want nothing", buf)
	}
	if !bytes.Equal(fileBytes(t, ac.credentials), before) {
		t.Error("the credentials file changed")
	}
}

func TestAgentRemove(t *testing.T) {
	pg := testPG(t)
	gone, gonePassword := newAgentRole(t, pg)
	kept, keptPassword := newAgentRole(t, pg)
	ac := newAgentCase(t, pg)
	tokens := map[string]string{}
	for account, password := range map[string]string{gone: gonePassword, kept: keptPassword} {
		out, err := ac.run([]string{"add", account}, password+"\n", nil)
		if err != nil {
			t.Fatalf("add %s: %v", account, err)
		}
		tokens[account] = tokenIn(t, out)
	}
	r := ac.start(t)

	a, buf := asking(t, nil)
	if err := runAgent(ac.args("remove", gone), a); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if want := fmt.Sprintf("%q removed; it is refused from the next login\n", gone); buf.String() != want {
		t.Errorf("remove printed %q, want %q", buf, want)
	}
	f := readFile(t, ac.credentials)
	if _, there := f.Agents[gone]; there || len(f.Agents) != 1 {
		t.Errorf("the file holds %d accounts, %s among them: %v; want only %s", len(f.Agents), gone, there, kept)
	}
	ac.wantRefusedLogin(t, r, gone, tokens[gone])
	ac.wantLogin(t, r, kept, tokens[kept])
}

func TestAgentList(t *testing.T) {
	ac := newAgentCase(t, "")
	if out, err := ac.run([]string{"list"}, "", nil); err != nil || out != "no accounts\n" {
		t.Errorf("list with no file = %q, %v; want no accounts", out, err)
	}

	added := time.Date(2026, 11, 22, 22, 2, 11, 0, time.FixedZone("KST", 9*60*60)) // shown in UTC
	issued := time.Date(2026, 11, 23, 8, 30, 59, 0, time.UTC)
	f := &custody.File{Agents: map[string]custody.Agent{
		"agent_support":  {PasswordEnv: "AGENT_SUPPORT_PASSWORD", Added: added, TokenIssued: issued},
		"agent_enricher": {Password: secret(t), Added: added, TokenIssued: added},
	}}
	for name, a := range f.Agents {
		_, v, err := scram.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		a.Token = v.Text()
		f.Agents[name] = a
	}
	if err := custody.Write(ac.credentials, f); err != nil {
		t.Fatal(err)
	}
	out, err := ac.run([]string{"list"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%-16s%-28s%-18s%s\n", "ACCOUNT", "PASSWORD", "ADDED", "TOKEN ISSUED") +
		fmt.Sprintf("%-16s%-28s%-18s%s\n", "agent_enricher", "file", "2026-11-22 13:02", "2026-11-22 13:02") +
		fmt.Sprintf("%-16s%-28s%-18s%s\n", "agent_support", "env AGENT_SUPPORT_PASSWORD", "2026-11-22 13:02", "2026-11-23 08:30")
	if out != want {
		t.Errorf("list printed:\n%s\nwant:\n%s", out, want)
	}
	for _, s := range fileSecrets(t, ac.credentials) {
		if strings.Contains(out, s) {
			t.Errorf("list shows the secret %q", s)
		}
	}

	for name := range f.Agents {
		if _, err := ac.run([]string{"remove", name}, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := ac.run([]string{"list"}, "", nil); err != nil || out != "no accounts\n" {
		t.Errorf("list with every account removed = %q, %v; want no accounts", out, err)
	}
}

func TestAgentPromptsWithoutEcho(t *testing.T) {
	pg := testPG(t)
	account, password := newAgentRole(t, pg)

	// add runs a with ac's config and wants password in the file and nowhere
	// in what it printed.
	add := func(t *testing.T, a agentIO, out *bytes.Buffer) {
		t.Helper()
		ac := newAgentCase(t, pg)
		if err := runAgent(ac.args("add", account), a); err != nil {
			t.Fatalf("add: %v; output:\n%s", err, out)
		}
		if got := readFile(t, ac.credentials).Agents[account].Password; got != password {
			t.Error("the file holds another password than the one given")
		}
		if strings.Contains(out.String(), password) {
			t.Errorf("add printed the password:\n%s", out)
		}
	}

	t.Run("Terminal", func(t *testing.T) {
		var prompts []string
		a, out := testIO(unread{t}, nil)
		a.terminal = true
		a.secret = func(prompt string) (string, error) {
			prompts = append(prompts, prompt)
			return password, nil
		}
		add(t, a, out)
		if want := fmt.Sprintf("Database password for %q: ", account); len(prompts) != 1 || prompts[0] != want {
			t.Errorf("asked %q, want once %q", prompts, want)
		}
	})

	for name, stdin := range map[string]string{
		"LF":        password + "\nwrong\n",
		"CRLF":      password + "\r\nwrong\r\n",
		"NoLineEnd": password,
	} {
		t.Run("NotATerminal/"+name, func(t *testing.T) {
			a, out := testIO(strings.NewReader(stdin), nil)
			a.secret = noSecret(t)
			add(t, a, out)
		})
	}

	t.Run("TerminalWithoutSecret", func(t *testing.T) {
		a, out := testIO(strings.NewReader(password+"\n"), nil)
		a.terminal = true
		add(t, a, out)
	})

	t.Run("Empty", func(t *testing.T) {
		ac := newAgentCase(t, pg)
		for what, stdin := range map[string]string{"no input": "", "an empty first line": "\n" + password + "\n", "CRLF alone": "\r\n"} {
			if out, err := ac.run([]string{"add", account}, stdin, nil); err == nil || err.Error() != "the password is empty" || out != "" {
				t.Errorf("add with %s = %v, output %q; want the password is empty", what, err, out)
			}
		}
		a, _ := testIO(unread{t}, nil)
		a.terminal = true
		a.secret = func(string) (string, error) { return "", nil }
		if err := runAgent(ac.args("add", account), a); err == nil || err.Error() != "the password is empty" {
			t.Errorf("add with nothing typed = %v; want the password is empty", err)
		}
		wantNoFile(t, ac.credentials)
	})
}

func TestAgentKeepsTheFileMode(t *testing.T) {
	ac := newAgentCase(t, "")
	ac.seed(t, map[string]string{"stet_chk_mode": secret(t)})
	before := readFile(t, ac.credentials).Agents["stet_chk_mode"].Token
	if err := os.Chmod(ac.credentials, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.run([]string{"token", "stet_chk_mode"}, "", nil); err != nil {
		t.Fatalf("token: %v", err)
	}
	fi, err := os.Stat(ac.credentials)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o400 {
		t.Errorf("the credentials file has mode %04o, want 0400 as it had", perm)
	}
	if readFile(t, ac.credentials).Agents["stet_chk_mode"].Token == before {
		t.Error("the token was not replaced")
	}
}

// Every command's output and error, its failures too, shows no password or
// verifier, nor Stet's own password or the upstream address; each token is
// shown once, by the add or token that made it.
func TestAgentOutputShowsTheTokenOnce(t *testing.T) {
	pg := testPG(t)
	account, password := newAgentRole(t, pg)
	ac := newAgentCase(t, pg)
	changed := secret(t)
	name := "STET_CHK_PASSWORD_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	secrets := []string{password, changed, "scram-pw"}

	type step struct {
		args   []string
		stdin  string
		env    map[string]string
		before func()
		ok     bool
		tokens int // printed
	}
	steps := []step{
		{args: []string{"add", account}, stdin: "wrong-" + password + "\n"},
		{args: []string{"add", account}, stdin: password + "\n", ok: true, tokens: 1},
		{args: []string{"add", account}, stdin: password + "\n"}, // already there
		{args: []string{"token", account}, ok: true, tokens: 1},
		{args: []string{"list"}, ok: true},
		// The operator changes the password in the database: the old one is wrong now.
		{args: []string{"password", account}, stdin: password + "\n",
			before: func() { mustExec(t, superuser(t, pg), "alter role "+account+" password '"+changed+"'") }},
		{args: []string{"password", account}, stdin: changed + "\n", ok: true},
		{args: []string{"password", "-password-env", name, account}, env: map[string]string{name: changed}, ok: true},
		{args: []string{"password", account}}, // its password is the variable's
		{args: []string{"list"}, ok: true},
		{args: []string{"token", account}, ok: true, tokens: 1},
		{args: []string{"remove", account}, ok: true},
		{args: []string{"list"}, ok: true},
		{args: []string{"token", account}},
	}
	var texts []string
	for i, s := range steps {
		if s.before != nil {
			s.before()
		}
		out, err := ac.run(s.args, s.stdin, s.env)
		if (err == nil) != s.ok {
			t.Fatalf("step %d, %q: err = %v, want an error: %v; output:\n%s", i, s.args, err, !s.ok, out)
		}
		if got := len(tokenShape.FindAllString(out, -1)); got != s.tokens {
			t.Errorf("step %d, %q printed %d tokens, want %d", i, s.args, got, s.tokens)
		}
		texts = append(texts, out)
		if err != nil {
			texts = append(texts, err.Error())
			if strings.Contains(err.Error(), ac.upstream) {
				t.Errorf("step %d, %q: the error shows the upstream address: %v", i, s.args, err)
			}
		}
		secrets = append(secrets, fileSecrets(t, ac.credentials)...)
	}

	// An upstream that is not there.
	far := newAgentCase(t, "")
	out, err := far.run([]string{"add", account}, password+"\n", nil)
	unreachable := fmt.Sprintf("could not log in to the database as %q to check the password, so nothing was written: upstream login: cannot reach the server", account)
	if err == nil || err.Error() != unreachable || out != "" {
		t.Errorf("add over an upstream that is not there = %v, output %q; want %q", err, out, unreachable)
	} else if strings.Contains(err.Error(), far.upstream) {
		t.Errorf("the error shows the upstream address: %v", err)
	}
	texts = append(texts, out)
	if err != nil {
		texts = append(texts, err.Error())
	}

	all := strings.Join(texts, "\n")
	for _, token := range tokenShape.FindAllString(all, -1) {
		if n := strings.Count(all, token); n != 1 {
			t.Errorf("a token is shown %d times, want once", n)
		}
	}
	for _, s := range secrets {
		if strings.Contains(all, s) {
			t.Errorf("an output or error shows the secret %q:\n%s", s, all)
		}
	}
}

// The directory could not be synced after the file was put in place: the
// command goes on as a success, and says the change may not survive a crash.
func TestAgentGoesOnWhenTheDirectoryIsNotSynced(t *testing.T) {
	ac := newAgentCase(t, "")
	ac.seed(t, map[string]string{"stet_chk_sync": secret(t), "stet_chk_other": secret(t)})
	defer func(u func(string, func(*custody.File) error) error) { updateCredentials = u }(updateCredentials)
	updateCredentials = func(path string, change func(*custody.File) error) error {
		if err := custody.Update(path, change); err != nil {
			return err
		}
		return fmt.Errorf("credentials file %s: %w (sync: input/output error)", path, custody.ErrNotSynced)
	}
	warning := "warning: credentials file " + ac.credentials + ": " + custody.ErrNotSynced.Error() + " (sync: input/output error)\n"

	before := readFile(t, ac.credentials).Agents["stet_chk_sync"].Token
	out, err := ac.run([]string{"token", "stet_chk_sync"}, "", nil)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if want := tokenOutput("stet_chk_sync", tokenIn(t, out)) + warning; out != want {
		t.Errorf("token printed:\n%s\nwant:\n%s", out, want)
	}
	if readFile(t, ac.credentials).Agents["stet_chk_sync"].Token == before {
		t.Error("the token was not replaced")
	}

	out, err = ac.run([]string{"remove", "stet_chk_other"}, "", nil)
	if want := `"stet_chk_other" removed; it is refused from the next login` + "\n" + warning; err != nil || out != want {
		t.Errorf("remove = %v, printed:\n%s\nwant:\n%s", err, out, want)
	}
}

func TestAgentStops(t *testing.T) {
	const usageLine = "usage: stet agent <command> [flags] [<account>]"

	t.Run("NoCredentials", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stet.yaml")
		writeConfig(t, path, "mode: shadow\nlisten: 127.0.0.1:0\nupstream: postgres://stet_scram@127.0.0.1:1/stet_test?sslmode=disable\n")
		for _, args := range [][]string{{"add", "stet_chk_x"}, {"token", "stet_chk_x"}, {"password", "stet_chk_x"}, {"remove", "stet_chk_x"}, {"list"}} {
			a, out := asking(t, nil)
			err := runAgent(append([]string{args[0], "-config", path}, args[1:]...), a)
			if want := "the config has no agents.credentials; add it first"; err == nil || err.Error() != want || out.Len() != 0 {
				t.Errorf("%s = %v, output %q; want %q and no output", args[0], err, out, want)
			}
		}
	})

	t.Run("AlreadyThere", func(t *testing.T) {
		ac := newAgentCase(t, "")
		ac.seed(t, map[string]string{"stet_chk_there": secret(t)})
		before := fileBytes(t, ac.credentials)
		a, out := asking(t, nil)
		err := runAgent(ac.args("add", "stet_chk_there"), a)
		if want := `"stet_chk_there" is already there; use stet agent token or stet agent password`; err == nil || err.Error() != want || out.Len() != 0 {
			t.Errorf("add = %v, output %q; want %q and no output", err, out, want)
		}
		if !bytes.Equal(fileBytes(t, ac.credentials), before) {
			t.Error("the credentials file changed")
		}
	})

	t.Run("NoAccount", func(t *testing.T) {
		ac := newAgentCase(t, "")
		for _, file := range []bool{false, true} {
			if file {
				ac.seed(t, map[string]string{"stet_chk_other": secret(t)})
			}
			for _, cmd := range []string{"token", "password", "remove"} {
				a, out := asking(t, nil)
				err := runAgent(ac.args(cmd, "stet_chk_nope"), a)
				if want := `no account "stet_chk_nope"`; err == nil || err.Error() != want || out.Len() != 0 {
					t.Errorf("%s, a file there %v: %v, output %q; want %q and no output", cmd, file, err, out, want)
				}
			}
		}
	})

	t.Run("Names", func(t *testing.T) {
		ac := newAgentCase(t, "")
		long := strings.Repeat("a", 64)
		for _, cmd := range []string{"add", "token", "password", "remove"} {
			for name, want := range map[string]string{
				long: `account "` + long + `": the name is longer than 63 bytes`,
				"":   "the account name is empty",
			} {
				a, out := asking(t, nil)
				if err := runAgent(ac.args(cmd, name), a); err == nil || err.Error() != want || out.Len() != 0 {
					t.Errorf("%s %q: %v, output %q; want %q and no output", cmd, name, err, out, want)
				}
			}
			// 63 bytes is a name: token, password and remove look for it.
			if cmd != "add" {
				a, _ := asking(t, nil)
				name := long[:63]
				if err := runAgent(ac.args(cmd, name), a); err == nil || err.Error() != fmt.Sprintf("no account %q", name) {
					t.Errorf("%s with a name of 63 bytes: %v, want no account", cmd, err)
				}
			}
		}
	})

	t.Run("OwnAccount", func(t *testing.T) {
		ac := newAgentCase(t, "")
		a, out := asking(t, nil)
		err := runAgent(ac.args("add", "stet_scram"), a)
		if want := `"stet_scram" is the account stet itself connects as; an agent needs its own`; err == nil || err.Error() != want || out.Len() != 0 {
			t.Errorf("add = %v, output %q; want %q and no output", err, out, want)
		}
		wantNoFile(t, ac.credentials)
	})

	// What is not a variable's name may be its value, the password: it is
	// refused before anything else, in words that do not quote it.
	t.Run("PasswordEnvIsAName", func(t *testing.T) {
		const want = "-password-env takes the name of an environment variable, not its value"
		ac := newAgentCase(t, "")
		for _, value := range []string{"S3cr3t-Value!", "", "1PASSWORD", "A=B", "has space", "pw\n", "ÄGENT"} {
			for _, args := range [][]string{
				ac.args("add", "-password-env", value, "stet_chk_x"),
				ac.args("password", "-password-env", value, "stet_chk_x"),
				{"add", "-password-env", value},                                     // no account
				{"add", "-config", "/nonexistent.yaml", "-password-env", value, ""}, // no config, no name
			} {
				a, out := asking(t, nil)
				err := runAgent(args, a)
				if err == nil || err.Error() != want || out.Len() != 0 {
					t.Errorf("%q = %v, output %q; want %q", args, err, out, want)
				}
			}
		}
		// A name goes on to what follows.
		a, _ := asking(t, nil)
		if err := runAgent(ac.args("password", "-password-env", "_Agent_PW1", "stet_chk_x"), a); err == nil || err.Error() != `no account "stet_chk_x"` {
			t.Errorf("password -password-env _Agent_PW1 = %v, want no account", err)
		}
	})

	t.Run("NoDirectory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "stet.yaml")
		writeConfig(t, path, "mode: shadow\nlisten: 127.0.0.1:0\nupstream: postgres://stet_scram@127.0.0.1:1/stet_test?sslmode=disable\n"+
			"agents: {credentials: missing/agents.yaml}\n")
		a, out := asking(t, nil)
		err := runAgent([]string{"add", "-config", path, "stet_chk_x"}, a)
		want := "agents.credentials: the directory " + filepath.Join(dir, "missing") + " is not there; create it first"
		if err == nil || err.Error() != want || out.Len() != 0 {
			t.Errorf("add = %v, output %q; want %q and no output", err, out, want)
		}
	})

	t.Run("Usage", func(t *testing.T) {
		for _, args := range [][]string{nil, {"nope", "stet_chk_x"}, {"add"}, {"token"}, {"add", "a", "b"}, {"remove", "a", "-config", "x"}, {"list", "a"}} {
			a, out := asking(t, nil)
			err := runAgent(args, a)
			if err == nil || !strings.HasPrefix(err.Error(), usageLine+"\n") || out.Len() != 0 {
				t.Errorf("%q = %v, output %q; want the usage", args, err, out)
			}
		}
	})

	t.Run("FlagOfAnotherCommand", func(t *testing.T) {
		a, out := asking(t, nil)
		err := runAgent([]string{"token", "-password-env", "X", "stet_chk_x"}, a)
		if want := "flag provided but not defined: -password-env"; err == nil || err.Error() != want || out.Len() != 0 {
			t.Errorf("token -password-env = %v, output %q; want %q", err, out, want)
		}
	})

	t.Run("Help", func(t *testing.T) {
		for _, args := range [][]string{{"-h"}, {"--help"}, {"add", "-h"}, {"list", "-help"}} {
			a, out := asking(t, nil)
			if err := runAgent(args, a); err != nil || !strings.HasPrefix(out.String(), usageLine+"\n") {
				t.Errorf("%q = %v, output %q; want the usage and no error", args, err, out)
			}
		}
	})
}

// While add or password waits for its password, another stet agent removes
// one account and gives another a new token. The first then changes only
// its own account, in the file as it is by then: the removed account stays
// removed, and the new token works.
func TestAgentKeepsChangesMadeWhileItAsks(t *testing.T) {
	pg := testPG(t)
	for _, cmd := range []string{"add", "password"} {
		t.Run(cmd, func(t *testing.T) {
			gone, gonePassword := newAgentRole(t, pg)
			rotated, rotatedPassword := newAgentRole(t, pg)
			ac := newAgentCase(t, pg)
			ac.seed(t, map[string]string{gone: gonePassword, rotated: rotatedPassword})
			oldGone, oldRotated := ac.tokens[gone], ac.tokens[rotated]
			// add adds an account of its own; password gives rotated the
			// password the operator just set in the database.
			target, password := rotated, secret(t)
			if cmd == "add" {
				target, password = newAgentRole(t, pg)
			} else {
				mustExec(t, superuser(t, pg), "alter role "+rotated+" password '"+password+"'")
			}

			var newToken string
			done := make(chan struct{})
			a, out := testIO(unread{t}, nil)
			a.terminal = true
			a.secret = func(string) (string, error) {
				go func() { // another operator, meanwhile
					defer close(done)
					if _, err := ac.run([]string{"remove", gone}, "", nil); err != nil {
						t.Errorf("remove, meanwhile: %v", err)
					}
					o, err := ac.run([]string{"token", rotated}, "", nil)
					if err != nil {
						t.Errorf("token, meanwhile: %v", err)
					}
					newToken = tokenShape.FindString(o)
				}()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("the other stet agent waited for this one: the file is locked while it asks")
				}
				return password, nil
			}
			if err := runAgent(ac.args(cmd, target), a); err != nil {
				t.Fatalf("%s: %v", cmd, err)
			}
			<-done
			f := readFile(t, ac.credentials)
			if _, there := f.Agents[gone]; there {
				t.Errorf("%s, removed meanwhile, is back in the file", gone)
			}
			if f.Agents[target].Password != password {
				t.Errorf("%s does not hold its password", cmd)
			}
			r := ac.start(t)
			ac.wantRefusedLogin(t, r, gone, oldGone)
			ac.wantRefusedLogin(t, r, rotated, oldRotated)
			ac.wantLogin(t, r, rotated, newToken)
			if cmd == "add" {
				ac.wantLogin(t, r, target, tokenIn(t, out.String()))
			}
		})
	}
}

// token and remove change their own account and nothing else: the other
// account's entry and mock_secret stay as they were.
func TestAgentTouchesOnlyItsAccount(t *testing.T) {
	ac := newAgentCase(t, "")
	ac.seed(t, map[string]string{"stet_chk_mine": secret(t), "stet_chk_yours": secret(t)})
	before := readFile(t, ac.credentials)
	for _, args := range [][]string{{"token", "stet_chk_mine"}, {"remove", "stet_chk_mine"}} {
		if _, err := ac.run(args, "", nil); err != nil {
			t.Fatalf("%s: %v", args[0], err)
		}
		after := readFile(t, ac.credentials)
		if after.MockSecret != before.MockSecret {
			t.Errorf("%s changed mock_secret", args[0])
		}
		if after.Agents["stet_chk_yours"] != before.Agents["stet_chk_yours"] {
			t.Errorf("%s changed another account's entry", args[0])
		}
		if mine, ok := after.Agents["stet_chk_mine"]; ok {
			was := before.Agents["stet_chk_mine"]
			if mine.Token == was.Token || mine.Password != was.Password || !mine.Added.Equal(was.Added) {
				t.Errorf("%s: want only the token and its date changed", args[0])
			}
		}
	}
}

// A write that fails (here: the directory is read-only) shows no token,
// changes nothing, and is the command's error.
func TestAgentWritesNothingWhenTheWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	ac := newAgentCase(t, "")
	ac.seed(t, map[string]string{"stet_chk_stuck": secret(t), "stet_chk_other": secret(t)})
	before := fileBytes(t, ac.credentials)
	dir := filepath.Dir(ac.credentials)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	for _, cmd := range []string{"token", "remove"} {
		out, err := ac.run([]string{cmd, "stet_chk_stuck"}, "", nil)
		if err == nil || out != "" {
			t.Errorf("%s in a read-only directory = %v, output %q; want an error and no output", cmd, err, out)
		}
		if !bytes.Equal(fileBytes(t, ac.credentials), before) {
			t.Errorf("%s changed the file", cmd)
		}
	}
}

// What would keep stet proxy from starting is said: agents.roles that no
// longer lists the file's accounts, and a file with no account left.
func TestAgentSaysWhatStopsTheProxy(t *testing.T) {
	t.Run("Remove", func(t *testing.T) {
		ac := newAgentCase(t, "")
		ac.seed(t, map[string]string{"stet_chk_a": secret(t), "stet_chk_b": secret(t), "stet_chk_c": secret(t)})
		ac.setRoles(t, "stet_chk_a", "stet_chk_b")
		for _, c := range []struct{ account, want string }{
			{"stet_chk_c", removeOutput("stet_chk_c")}, // not in agents.roles
			{"stet_chk_a", removeOutput("stet_chk_a") + rolesRemoveLine("stet_chk_a")},
			{"stet_chk_b", removeOutput("stet_chk_b") + rolesRemoveLine("stet_chk_b") + noAccountLeft},
		} {
			if out, err := ac.run([]string{"remove", c.account}, "", nil); err != nil || out != c.want {
				t.Errorf("remove %s = %v, printed:\n%s\nwant:\n%s", c.account, err, out, c.want)
			}
		}
	})

	t.Run("RemoveTheLast", func(t *testing.T) {
		ac := newAgentCase(t, "")
		ac.seed(t, map[string]string{"stet_chk_last": secret(t)})
		if out, err := ac.run([]string{"remove", "stet_chk_last"}, "", nil); err != nil || out != removeOutput("stet_chk_last")+noAccountLeft {
			t.Errorf("remove = %v, printed:\n%s", err, out)
		}
	})

	t.Run("Add", func(t *testing.T) {
		pg := testPG(t)
		listed, listedPassword := newAgentRole(t, pg)
		unlisted, unlistedPassword := newAgentRole(t, pg)
		ac := newAgentCase(t, pg)
		ac.setRoles(t, listed)
		out, err := ac.run([]string{"add", listed}, listedPassword+"\n", nil)
		if err != nil {
			t.Fatal(err)
		}
		if want := addOutput(listed, tokenIn(t, out), ac.cfgPath); out != want {
			t.Errorf("add of an account agents.roles lists printed:\n%s\nwant:\n%s", out, want)
		}
		out, err = ac.run([]string{"add", unlisted}, unlistedPassword+"\n", nil)
		if err != nil {
			t.Fatal(err)
		}
		if want := addOutput(unlisted, tokenIn(t, out), ac.cfgPath, rolesAddBullet(unlisted)); out != want {
			t.Errorf("add of an account agents.roles does not list printed:\n%s\nwant:\n%s", out, want)
		}
	})
}

// An interrupt while the password is asked for puts the terminal back, then
// ends stet with 130; once stopped, nothing is done.
func TestOnInterruptRestoresTheTerminal(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	var calls []string
	exited := make(chan int, 1)
	stop := onInterrupt(sigs, func() { calls = append(calls, "restore") }, func(code int) {
		calls = append(calls, "exit")
		exited <- code
	})
	sigs <- os.Interrupt
	select {
	case code := <-exited:
		if code != 130 {
			t.Errorf("exit(%d), want 130", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an interrupt did not end stet")
	}
	stop()
	if len(calls) != 2 || calls[0] != "restore" || calls[1] != "exit" {
		t.Errorf("calls %q, want restore then exit", calls)
	}

	called := false
	stop = onInterrupt(make(chan os.Signal), func() { called = true }, func(int) { called = true })
	stop()
	if called {
		t.Error("stopped, it still did something")
	}
}

// What add and password checked before asking is checked again when they
// write: an account added meanwhile is not added twice, one removed
// meanwhile is not brought back. Nothing is written, and no token shown.
func TestAgentChecksAgainWhenItWrites(t *testing.T) {
	pg := testPG(t)
	for _, c := range []struct {
		cmd, meanwhile string
		want           func(account string) string
	}{
		{"add", "add", func(a string) string {
			return fmt.Sprintf("%q is already there; use stet agent token or stet agent password", a)
		}},
		{"password", "remove", func(a string) string { return fmt.Sprintf("no account %q", a) }},
	} {
		t.Run(c.cmd, func(t *testing.T) {
			account, password := newAgentRole(t, pg)
			ac := newAgentCase(t, pg)
			if c.cmd == "password" {
				ac.seed(t, map[string]string{account: password})
			}
			var after []byte
			a, out := testIO(unread{t}, nil)
			a.terminal = true
			a.secret = func(string) (string, error) {
				if o, err := ac.run([]string{c.meanwhile, account}, password+"\n", nil); err != nil {
					t.Errorf("%s, meanwhile: %v; output:\n%s", c.meanwhile, err, o)
				}
				after = fileBytes(t, ac.credentials)
				return password, nil
			}
			err := runAgent(ac.args(c.cmd, account), a)
			if want := c.want(account); err == nil || err.Error() != want {
				t.Errorf("%s = %v, want %q", c.cmd, err, want)
			}
			if out.Len() != 0 {
				t.Errorf("%s printed %q, want nothing", c.cmd, out)
			}
			if !bytes.Equal(fileBytes(t, ac.credentials), after) {
				t.Errorf("%s changed the file", c.cmd)
			}
		})
	}
}

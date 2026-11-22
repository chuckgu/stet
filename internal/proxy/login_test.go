package proxy_test

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/stetdb/stet/internal/custody"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/scram"
)

// agentPasswords are the real passwords of the test server's accounts that
// agents log in as through a Login, one per auth method.
var agentPasswords = map[string]string{
	"stet_scram": "scram-pw",
	"stet_md5":   "md5-pw",
	"stet_clear": "clear-pw",
}

// credentials is a credentials file in t.TempDir() that gives each of its
// accounts a new token.
type credentials struct {
	path      string
	tokens    map[string]string         // by account
	verifiers map[string]scram.Verifier // by account
}

// newCredentials writes a credentials file of the accounts of passwords,
// each with the stored password given and a new token.
func newCredentials(t *testing.T, passwords map[string]string) credentials {
	t.Helper()
	c := credentials{
		path:      filepath.Join(t.TempDir(), "agents.yaml"),
		tokens:    map[string]string{},
		verifiers: map[string]scram.Verifier{},
	}
	f := &custody.File{Agents: map[string]custody.Agent{}}
	now := time.Now().UTC().Truncate(time.Second)
	for account, password := range passwords {
		token, v, err := scram.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		c.tokens[account], c.verifiers[account] = token, v
		f.Agents[account] = custody.Agent{Token: v.Text(), Password: password, Added: now, TokenIssued: now}
	}
	if err := custody.Write(c.path, f); err != nil {
		t.Fatal(err)
	}
	return c
}

// login is the custody.Login of c, logging in to up, the server as the
// proxy reaches it.
func (c credentials) login(t *testing.T, up proxy.Upstream, log *slog.Logger) *custody.Login {
	t.Helper()
	store, err := custody.OpenStore(c.path, func(string) string { return "" }, log)
	if err != nil {
		t.Fatal(err)
	}
	l, err := custody.NewLogin(store, "postgres://stet@"+up.Addr+"/stet_test", up.TLS, false)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// loginSetup says how to start a loginProxy. Its zero value is a proxy to the
// test server over TLS with no gate and no ClientTLS, whose Login is a
// credentials file of agentPasswords.
type loginSetup struct {
	gate      proxy.Gate
	policy    proxy.Policy
	clientTLS *tls.Config
	passwords map[string]string // the credentials file's; nil: agentPasswords
	login     proxy.Login       // in place of the credentials file's
	log       *syncLog          // nil: a new one
	ln        net.Listener      // nil: a new one on loopback
}

// loginProxy is a proxy with a Login, served on loopback until the test ends.
type loginProxy struct {
	addr   string
	srv    *proxy.Server
	creds  credentials
	tokens map[string]string // creds.tokens
	log    *syncLog          // what the proxy and its Login log, debug included
}

func (ls loginSetup) start(t *testing.T) loginProxy {
	t.Helper()
	up := tlsUpstream(t)
	if ls.passwords == nil {
		ls.passwords = agentPasswords
	}
	if ls.log == nil {
		ls.log = &syncLog{}
	}
	log := slog.New(slog.NewTextHandler(ls.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	creds := newCredentials(t, ls.passwords)
	srv := proxy.New(up, log, ls.gate, ls.policy)
	srv.ClientTLS = ls.clientTLS
	srv.Login = ls.login
	if srv.Login == nil {
		srv.Login = creds.login(t, up, log)
	}
	if ls.ln == nil {
		ls.ln = listenLoopback(t)
	}
	addr := serveProxy(t, srv, ls.ln)
	return loginProxy{addr: addr, srv: srv, creds: creds, tokens: creds.tokens, log: ls.log}
}

// waitForLog waits until log holds s.
func waitForLog(t *testing.T, log *syncLog, s string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log.String(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("the log never held %q:\n%s", s, log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// wantLoginFatal fails the test unless err is a FATAL with code and msg alone.
func wantLoginFatal(t *testing.T, err error, code, msg string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want FATAL %s %q", err, code, msg)
	}
	if pgErr.Severity != "FATAL" || pgErr.Code != code || pgErr.Message != msg || pgErr.Detail != "" || pgErr.Hint != "" {
		t.Errorf("got %s %s %q (detail %q, hint %q), want FATAL %s %q alone",
			pgErr.Severity, pgErr.Code, pgErr.Message, pgErr.Detail, pgErr.Hint, code, msg)
	}
}

func passwordFailed(user string) string {
	return `password authentication failed for user "` + user + `"`
}

// scramClient is the client's side of a SCRAM-SHA-256 exchange, written out
// so a test can send what it likes, and stop where it likes.
type scramClient struct {
	password string
	nonce    string // the client's
	bare     string // the client-first-message-bare
}

func newScramClient(password string) *scramClient {
	b := make([]byte, 18)
	rand.Read(b)
	nonce := base64.StdEncoding.EncodeToString(b)
	return &scramClient{password: password, nonce: nonce, bare: "n=,r=" + nonce}
}

// first is the client-first-message: no channel binding, as a client sends
// when the server offers none.
func (c *scramClient) first() []byte { return []byte("n,," + c.bare) }

// final is the client-final-message answering serverFirst, with the proof
// of c's password.
func (c *scramClient) final(t *testing.T, serverFirst []byte) []byte {
	t.Helper()
	attrs := strings.Split(string(serverFirst), ",")
	if len(attrs) != 3 || !strings.HasPrefix(attrs[0], "r="+c.nonce) || !strings.HasPrefix(attrs[1], "s=") || !strings.HasPrefix(attrs[2], "i=") {
		t.Fatalf("server-first-message %q", serverFirst)
	}
	salt, err := base64.StdEncoding.DecodeString(attrs[1][2:])
	if err != nil {
		t.Fatal(err)
	}
	iterations, err := strconv.Atoi(attrs[2][2:])
	if err != nil {
		t.Fatal(err)
	}
	salted, err := pbkdf2.Key(sha256.New, c.password, salt, iterations, sha256.Size)
	if err != nil {
		t.Fatal(err)
	}
	mac := func(key []byte, msg string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(msg))
		return m.Sum(nil)
	}
	clientKey := mac(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	withoutProof := "c=biws,r=" + attrs[0][2:] // biws: "n,," in base64
	signature := mac(storedKey[:], c.bare+","+string(serverFirst)+","+withoutProof)
	proof := make([]byte, len(clientKey))
	for i := range proof {
		proof[i] = clientKey[i] ^ signature[i]
	}
	return []byte(withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof))
}

// startRaw opens a plaintext connection to addr and sends the startup packet
// of a session of user on the test database, with params.
func startRaw(t *testing.T, addr, user string, params map[string]string) (net.Conn, *pgproto3.Frontend) {
	t.Helper()
	return startRawWith(t, addr, pgproto3.ProtocolVersionNumber, map[string]string{"user": user, "database": "stet_test"}, params)
}

// startRawWith is startRaw with the protocol version and parameters given.
func startRawWith(t *testing.T, addr string, protocol uint32, params ...map[string]string) (net.Conn, *pgproto3.Frontend) {
	t.Helper()
	conn := dialPlain(t, addr)
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	all := map[string]string{}
	for _, p := range params {
		maps.Copy(all, p)
	}
	fe := pgproto3.NewFrontend(conn, conn)
	sendAll(t, fe, &pgproto3.StartupMessage{ProtocolVersion: protocol, Parameters: all})
	return conn, fe
}

// describe is a line for message m: its type, and for the messages of a
// login what tells one from another. Of an AuthenticationSASLContinue it is
// the shape alone (see shape), which is the same for every exchange.
func describe(t *testing.T, m pgproto3.BackendMessage) string {
	t.Helper()
	switch m := m.(type) {
	case *pgproto3.AuthenticationSASL:
		return fmt.Sprintf("AuthenticationSASL %q", m.AuthMechanisms)
	case *pgproto3.AuthenticationSASLContinue:
		return "AuthenticationSASLContinue " + shape(t, m.Data)
	case *pgproto3.AuthenticationSASLFinal:
		return "AuthenticationSASLFinal"
	case *pgproto3.AuthenticationOk:
		return "AuthenticationOk"
	case *pgproto3.ParameterStatus:
		return "ParameterStatus " + m.Name
	case *pgproto3.BackendKeyData:
		return fmt.Sprintf("BackendKeyData %d, a key of %d bytes", m.ProcessID, len(m.SecretKey))
	case *pgproto3.ReadyForQuery:
		return "ReadyForQuery " + string(m.TxStatus)
	case *pgproto3.ErrorResponse:
		return fmt.Sprintf("ErrorResponse %s %s %q", m.Severity, m.Code, m.Message)
	case *pgproto3.NoticeResponse:
		return "NoticeResponse " + m.Message
	case *pgproto3.NegotiateProtocolVersion:
		// PostgreSQL sends the whole version, major and minor.
		v := m.NewestMinorProtocol
		return fmt.Sprintf("NegotiateProtocolVersion %d.%d %q", v>>16, v&0xffff, m.UnrecognizedOptions)
	}
	return fmt.Sprintf("%T", m)
}

// shape is what a server-first-message shows of an account: its attributes
// in order, the length of the nonce, the length of the salt and the
// iteration count.
func shape(t *testing.T, serverFirst []byte) string {
	t.Helper()
	var out []string
	for _, a := range strings.Split(string(serverFirst), ",") {
		name, value, _ := strings.Cut(a, "=")
		switch name {
		case "r":
			value = strconv.Itoa(len(value))
		case "s":
			salt, err := base64.StdEncoding.DecodeString(value)
			if err != nil {
				t.Fatalf("salt %q: %v", value, err)
			}
			value = strconv.Itoa(len(salt))
		}
		out = append(out, name+"="+value)
	}
	return strings.Join(out, " ")
}

// rawLogin answers the proxy's SCRAM exchange on fe with the proof of
// password, as a client does, and returns a line for each message it got
// (see describe) up to the first ReadyForQuery or the end of the
// connection.
func rawLogin(t *testing.T, fe *pgproto3.Frontend, password string) []string {
	t.Helper()
	sc := newScramClient(password)
	var got []string
	for {
		msg, err := fe.Receive()
		if err != nil {
			return got
		}
		got = append(got, describe(t, msg))
		switch m := msg.(type) {
		case *pgproto3.AuthenticationSASL:
			sendAll(t, fe, &pgproto3.SASLInitialResponse{AuthMechanism: scram.Mechanism, Data: sc.first()})
		case *pgproto3.AuthenticationSASLContinue:
			sendAll(t, fe, &pgproto3.SASLResponse{Data: sc.final(t, m.Data)})
		case *pgproto3.ReadyForQuery:
			return got
		}
	}
}

// untilEnd returns a line for each message fe gets until the connection
// ends (see describe).
func untilEnd(t *testing.T, fe *pgproto3.Frontend) []string {
	t.Helper()
	var got []string
	for {
		msg, err := fe.Receive()
		if err != nil {
			return got
		}
		got = append(got, describe(t, msg))
	}
}

// The first two messages of every exchange (see describe).
const (
	offer        = `AuthenticationSASL ["SCRAM-SHA-256"]`
	saslContinue = "AuthenticationSASLContinue r=48 s=16 i=4096"
)

// An agent that knows only its token logs in through a proxy with a Login,
// with pgx and with psql at sslmode=verify-full: the session is the
// account's, over TLS on the server's side too, and the client's startup
// parameters reach it. From loopback it logs in without TLS as well.
func TestAgentLogsInWithAToken(t *testing.T) {
	lt := newLocalTLS(t)
	p := loginSetup{clientTLS: lt.server}.start(t)
	const who = "select current_user, ssl, current_setting('application_name'), current_setting('statement_timeout') " +
		"from pg_stat_ssl where pid = pg_backend_pid()"

	t.Run("Pgx", func(t *testing.T) {
		for _, account := range slices.Sorted(maps.Keys(agentPasswords)) {
			t.Run(account, func(t *testing.T) {
				params := lt.params()
				params.Set("application_name", "stet-test-agent")
				params.Set("options", "-cstatement_timeout=4321") // no space, which the URL would carry as +
				conn, err := pgx.Connect(testCtx(t), connURL(p.addr, account, p.tokens[account], params))
				if err != nil {
					t.Fatalf("%v; the proxy's log:\n%s", err, p.log.String())
				}
				defer conn.Close(context.Background())
				if _, ok := conn.PgConn().Conn().(*tls.Conn); !ok {
					t.Errorf("the client's connection is a %T, not TLS", conn.PgConn().Conn())
				}
				var user, app, timeout string
				var ssl bool
				if err := conn.QueryRow(testCtx(t), who).Scan(&user, &ssl, &app, &timeout); err != nil {
					t.Fatal(err)
				}
				if user != account || !ssl || app != "stet-test-agent" || timeout != "4321ms" {
					t.Errorf("current_user %q, TLS on the server %v, application_name %q, statement_timeout %q; want %s, true, stet-test-agent, 4321ms",
						user, ssl, app, timeout, account)
				}
			})
		}
	})

	t.Run("Psql", func(t *testing.T) {
		psql, err := exec.LookPath("psql")
		if err != nil {
			t.Skipf("psql is not in PATH, so this test cannot run: %v", err)
		}
		opts := fmt.Sprintf("sslmode=verify-full sslrootcert='%s' application_name=stet-test-psql options='-c statement_timeout=4321'", lt.rootPath)
		out, errOut, err := runPsqlSQL(t, psql, p.addr, "stet_scram", p.tokens["stet_scram"], opts, who)
		if want := "stet_scram|t|stet-test-psql|4321ms\n"; err != nil || out != want {
			t.Errorf("psql: %v, stdout %q, stderr %q; want stdout %q", err, out, errOut, want)
		}
	})

	t.Run("PlaintextFromLoopback", func(t *testing.T) {
		conn, err := pgx.Connect(testCtx(t), connURL(p.addr, "stet_scram", p.tokens["stet_scram"], nil)) // sslmode=disable
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(context.Background())
		var user string
		if err := conn.QueryRow(testCtx(t), "select current_user").Scan(&user); err != nil || user != "stet_scram" {
			t.Errorf("current_user = %q, %v; want stet_scram", user, err)
		}
	})

	// The proxy reads the client's messages of the exchange and nothing
	// more: a query sent in the same write as the proof reaches the server.
	t.Run("QueryRightBehindTheProof", func(t *testing.T) {
		_, fe := startRaw(t, p.addr, "stet_scram", nil)
		sc := newScramClient(p.tokens["stet_scram"])
		if msg, err := fe.Receive(); err != nil || describe(t, msg) != offer {
			t.Fatalf("the proxy opened with %v, %v", msg, err)
		}
		sendAll(t, fe, &pgproto3.SASLInitialResponse{AuthMechanism: scram.Mechanism, Data: sc.first()})
		msg, err := fe.Receive()
		if err != nil {
			t.Fatal(err)
		}
		first, ok := msg.(*pgproto3.AuthenticationSASLContinue)
		if !ok {
			t.Fatalf("the proxy answered %s", describe(t, msg))
		}
		sendAll(t, fe, &pgproto3.SASLResponse{Data: sc.final(t, first.Data)}, &pgproto3.Query{String: "select 'behind the proof'"})
		var got []string
		for ready := 0; ready < 2; {
			msg, err := fe.Receive()
			if err != nil {
				t.Fatalf("after %q: %v", got, err)
			}
			switch m := msg.(type) {
			case *pgproto3.DataRow:
				got = append(got, "DataRow "+string(m.Values[0]))
			case *pgproto3.ReadyForQuery:
				ready++
			case *pgproto3.ErrorResponse:
				t.Fatalf("SQLSTATE %s: %s", m.Code, m.Message)
			}
		}
		if want := []string{"DataRow behind the proof"}; !slices.Equal(got, want) {
			t.Errorf("the client got %q, want %q", got, want)
		}
	})

	// A client that asks for a newer protocol, or for protocol options, is
	// told first that the proxy speaks 3.0 with none of them, as the server
	// tells it (spec-v0.2-server 12), and logs in.
	t.Run("NewerProtocol", func(t *testing.T) {
		_, fe := startRawWith(t, p.addr, 3<<16|2,
			map[string]string{"user": "stet_scram", "database": "stet_test", "_pq_.stet_test": "on"})
		got := rawLogin(t, fe, p.tokens["stet_scram"])
		if len(got) < 4 || got[0] != `NegotiateProtocolVersion 3.0 ["_pq_.stet_test"]` || got[1] != offer || got[len(got)-1] != "ReadyForQuery I" {
			t.Errorf("the client got %q, want NegotiateProtocolVersion 3.0 first, then the login", got)
		}
		if !slices.ContainsFunc(got, func(s string) bool {
			return strings.HasPrefix(s, "BackendKeyData ") && strings.HasSuffix(s, ", a key of 4 bytes")
		}) {
			t.Errorf("the client got %q, want a BackendKeyData with a key of 4 bytes", got)
		}
	})
}

// A session that starts without TLS from another machine is refused before
// any login (TestPlaintextFromElsewhereIsRefused); on a proxy with a Login
// the log says so as a refused login. Without a Login the log is as before.
func TestPlaintextFromElsewhereIsARefusedLogin(t *testing.T) {
	lt := newLocalTLS(t)
	p := loginSetup{clientTLS: lt.server, ln: elsewhere{listenLoopback(t)}}.start(t)
	conn, err := pgx.Connect(testCtx(t), connURL(p.addr, "stet_scram", p.tokens["stet_scram"], nil)) // sslmode=disable
	if err == nil {
		conn.Close(context.Background())
	}
	wantLoginFatal(t, err, "28000", needsTLS)
	// Logged once the FATAL is written.
	waitForLog(t, p.log, `level=WARN msg="agent login refused" account=stet_scram remote=10.0.0.5:40000 reason="tls required"`)

	var log syncLog
	srv := proxy.New(proxy.Upstream{Addr: upstream(t)}, slog.New(slog.NewTextHandler(&log, nil)), nil, proxy.Policy{})
	srv.ClientTLS = lt.server
	addr := serveProxy(t, srv, elsewhere{listenLoopback(t)})
	conn, err = pgx.Connect(testCtx(t), connURL(addr, "stet_scram", "scram-pw", nil))
	if err == nil {
		conn.Close(context.Background())
	}
	wantLoginFatal(t, err, "28000", needsTLS)
	if got := log.String(); got != "" {
		t.Errorf("without a Login the proxy logged, at info and above:\n%s", got)
	}
}

// A wrong token, an account that is not in the credentials file, the
// account's real password and another account's token are all refused with
// the server's own 28P01. An unknown account and a wrong token get the same
// messages, of the same shape.
func TestLoginRefusals(t *testing.T) {
	p := loginSetup{}.start(t)
	other, _, err := scram.NewToken() // nobody's
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, user, password string }{
		{"WrongToken", "stet_scram", other},
		{"UnknownAccount", "stet_nobody", p.tokens["stet_scram"]},
		{"RealPassword", "stet_scram", agentPasswords["stet_scram"]},
		{"AnotherAccountsToken", "stet_scram", p.tokens["stet_md5"]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := pgx.Connect(testCtx(t), connURL(p.addr, tc.user, tc.password, nil))
			if err == nil {
				conn.Close(context.Background())
			}
			wantLoginFatal(t, err, "28P01", passwordFailed(tc.user))
		})
	}

	t.Run("SameAnswers", func(t *testing.T) {
		answers := func(user string) []string {
			_, fe := startRaw(t, p.addr, user, nil)
			got := rawLogin(t, fe, other)
			for i := range got {
				got[i] = strings.ReplaceAll(got[i], strconv.Quote(passwordFailed(user)), "<refused>")
			}
			return got
		}
		unknown, wrong := answers("stet_nobody"), answers("stet_scram")
		want := []string{offer, saslContinue, "ErrorResponse FATAL 28P01 <refused>"}
		if !slices.Equal(unknown, want) || !slices.Equal(wrong, want) {
			t.Errorf("an unknown account got %q, a wrong token %q; want both %q", unknown, wrong, want)
		}
	})
}

// A token is not the account's password on the server.
func TestTokenIsNoDatabasePassword(t *testing.T) {
	server := upstream(t)
	creds := newCredentials(t, agentPasswords)
	for account, token := range creds.tokens {
		conn, err := pgx.Connect(testCtx(t), connURL(server, account, token, nil))
		if err == nil {
			conn.Close(context.Background())
		}
		wantLoginFatal(t, err, "28P01", passwordFailed(account))
	}
}

// loginScenarios are the scenarios that run through a Login: the auth methods
// and the wrong password are TestAgentLogsInWithAToken's and
// TestLoginRefusals'.
func loginScenarios(t *testing.T, gate func() proxy.Gate) {
	for _, sc := range scenarios {
		if sc.name == "AuthMethodsPassThrough" || sc.name == "WrongPasswordIsRejectedByUpstream" {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			p := loginSetup{gate: gate()}.start(t)
			useTokensAt(t, p.addr, p.tokens)
			sc.run(t, p.addr, p.srv)
		})
	}
}

// The scenarios through a proxy with a Login, whose agents log in with
// tokens: cancel requests reach the server too.
func TestScenariosThroughALogin(t *testing.T) {
	loginScenarios(t, func() proxy.Gate { return nil })
}

func TestWatchedScenariosThroughALogin(t *testing.T) {
	loginScenarios(t, func() proxy.Gate { return newRecordingGate(isWrite) })
}

// The gate takes a session the proxy logged in for as authenticated from its
// start: the first write's Begin has the account and the server process, and
// on an enforcing proxy the settings the server reported at login.
func TestWatchedSessionKnowsItsAccount(t *testing.T) {
	for _, mode := range closedPointModes {
		t.Run(mode.name, func(t *testing.T) {
			table := watchedTable(t)
			g := newRecordingGate(isWrite)
			p := loginSetup{gate: g, policy: mode.policy}.start(t)
			conn, err := pgx.Connect(testCtx(t), connURL(p.addr, "stet_scram", p.tokens["stet_scram"], nil))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close(context.Background())
			var pid int64
			var tz, ds, is string
			if err := conn.QueryRow(testCtx(t), "select pg_backend_pid(), current_setting('TimeZone'), "+
				"current_setting('DateStyle'), current_setting('IntervalStyle')").Scan(&pid, &tz, &ds, &is); err != nil {
				t.Fatal(err)
			}
			mustExec(t, conn, "insert into "+table+" values (1, 'a')")

			bs, os := g.outcomes(t, 1)
			if !os[0].Executed {
				t.Errorf("write: %+v, want executed", os[0])
			}
			info := bs[0].info
			if info.User != "stet_scram" || int64(info.BackendPID) != pid || pid == 0 {
				t.Errorf("Begin was handed account %q and server process %d, want stet_scram and %d", info.User, info.BackendPID, pid)
			}
			if mode.policy.Enforce && (info.TimeZone != tz || info.DateStyle != ds || info.IntervalStyle != is || tz == "" || ds == "" || is == "") {
				t.Errorf("Begin was handed TimeZone %q, DateStyle %q, IntervalStyle %q; the session has %q, %q, %q",
					info.TimeZone, info.DateStyle, info.IntervalStyle, tz, ds, is)
			}
		})
	}
}

// A replication connection is refused before authentication.
func TestReplicationIsRefused(t *testing.T) {
	p := loginSetup{}.start(t)
	for _, v := range []string{"database", "true"} {
		pc, err := pgconn.Connect(testCtx(t), connURL(p.addr, "stet_scram", p.tokens["stet_scram"], url.Values{"replication": {v}}))
		if err == nil {
			pc.Close(context.Background())
		}
		wantLoginFatal(t, err, "0A000", "stet: replication connections are not supported")
	}
	if log := p.log.String(); strings.Contains(log, "agent logged in") {
		t.Errorf("a replication connection logged in:\n%s", log)
	}
}

// A client that breaks the exchange ends its own session alone, with 28000
// and at once: a message whose length is past the limit is refused before
// its body is read, so the proxy does not allocate it. A client that sends
// one byte at a time is ended when the exchange's limit runs out. Other
// sessions log in meanwhile.
func TestBrokenAuthEndsOnlyThatSession(t *testing.T) {
	const limit = time.Second
	// Registered before the proxy starts, so it runs after Close.
	t.Cleanup(proxy.SetExchangeTimeout(limit))
	p := loginSetup{}.start(t)
	const exchangeFailed = `ErrorResponse FATAL 28000 "stet: authentication exchange failed"`

	another := func(t *testing.T) {
		t.Helper()
		conn, err := pgx.Connect(testCtx(t), connURL(p.addr, "stet_md5", p.tokens["stet_md5"], nil))
		if err != nil {
			t.Fatalf("another session: %v", err)
		}
		defer conn.Close(context.Background())
		wantOne(t, conn)
	}
	// offered starts a session and reads the proxy's offer.
	offered := func(t *testing.T) (net.Conn, *pgproto3.Frontend) {
		t.Helper()
		conn, fe := startRaw(t, p.addr, "stet_scram", nil)
		msg, err := fe.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if got := describe(t, msg); got != offer {
			t.Fatalf("the proxy opened with %s, want %s", got, offer)
		}
		return conn, fe
	}

	for _, tc := range []struct {
		name string
		send func(t *testing.T, conn net.Conn, fe *pgproto3.Frontend)
	}{
		{"LengthOfAGibibyte", func(t *testing.T, conn net.Conn, _ *pgproto3.Frontend) {
			msg := []byte{'p', 0, 0, 0, 0}
			binary.BigEndian.PutUint32(msg[1:], 1<<30) // the body never comes
			if _, err := conn.Write(append(msg, scram.Mechanism+"\x00"...)); err != nil {
				t.Fatal(err)
			}
		}},
		{"QueryForTheInitialResponse", func(t *testing.T, _ net.Conn, fe *pgproto3.Frontend) {
			sendAll(t, fe, &pgproto3.Query{String: "select 1"})
		}},
		{"QueryForTheResponse", func(t *testing.T, _ net.Conn, fe *pgproto3.Frontend) {
			sendAll(t, fe, &pgproto3.SASLInitialResponse{AuthMechanism: scram.Mechanism, Data: newScramClient("x").first()})
			msg, err := fe.Receive()
			if err != nil {
				t.Fatal(err)
			}
			if got := describe(t, msg); got != saslContinue {
				t.Fatalf("the proxy answered %s, want %s", got, saslContinue)
			}
			sendAll(t, fe, &pgproto3.Query{String: "select 1"})
		}},
		{"OtherMechanism", func(t *testing.T, _ net.Conn, fe *pgproto3.Frontend) {
			sendAll(t, fe, &pgproto3.SASLInitialResponse{AuthMechanism: "SCRAM-SHA-256-PLUS", Data: []byte("p=tls-server-end-point,,n=,r=abcdefgh")})
		}},
		{"ChannelBindingAskedFor", func(t *testing.T, _ net.Conn, fe *pgproto3.Frontend) {
			sendAll(t, fe, &pgproto3.SASLInitialResponse{AuthMechanism: scram.Mechanism, Data: []byte("p=tls-server-end-point,,n=,r=abcdefgh")})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			conn, fe := offered(t)
			began := time.Now()
			tc.send(t, conn, fe)
			if got := untilEnd(t, fe); !slices.Equal(got, []string{exchangeFailed}) {
				t.Errorf("the client got %q, want %s and the end", got, exchangeFailed)
			}
			if d := time.Since(began); d > limit/2 {
				t.Errorf("the session ended %v after the broken message, want at once", d)
			}
			runtime.ReadMemStats(&after)
			if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
				t.Errorf("the process allocated %d MiB meanwhile: the proxy read the message's length as an allocation", grew>>20)
			}
			another(t)
		})
	}

	t.Run("OneByteAtATime", func(t *testing.T) {
		began := time.Now()
		conn, fe := offered(t)
		msg, err := (&pgproto3.SASLInitialResponse{AuthMechanism: scram.Mechanism, Data: newScramClient("x").first()}).Encode(nil)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for _, b := range msg[:8] { // then it stops
				if _, err := conn.Write([]byte{b}); err != nil {
					return
				}
				time.Sleep(limit / 10)
			}
		}()
		ended := make(chan time.Duration, 1)
		got := make(chan []string, 1)
		go func() {
			g := untilEnd(t, fe)
			ended <- time.Since(began)
			got <- g
		}()

		another(t)
		if d := time.Since(began); d >= limit {
			t.Errorf("the other session was in only after %v, past the limit %v", d, limit)
		}
		select {
		case d := <-ended:
			if d < limit || d > limit+2*time.Second {
				t.Errorf("the trickling session ended after %v, want soon after the limit %v", d, limit)
			}
			if g := <-got; len(g) > 1 || len(g) == 1 && g[0] != exchangeFailed {
				t.Errorf("the trickling client got %q, want nothing or %s", g, exchangeFailed)
			}
		case <-time.After(limit + 5*time.Second):
			t.Fatal("the trickling session was never ended")
		}
	})
}

// fakeLogin knows one account, whose verifier it holds, and opens sessions
// with open.
type fakeLogin struct {
	account string
	v       scram.Verifier
	open    func(ctx context.Context) (*proxy.Opened, error)
}

var fakeMockSecret = []byte("a secret of at least 32 bytes, for the fake's mocks")

func (l *fakeLogin) Verifier(account string) (scram.Verifier, bool) {
	if account == l.account {
		return l.v, true
	}
	return scram.Mock(account, fakeMockSecret), false
}

func (l *fakeLogin) Open(ctx context.Context, _, _ string, _ map[string]string) (*proxy.Opened, error) {
	return l.open(ctx)
}

// A login cut short leaves nothing behind: Close does not wait out a Login
// that does not answer, a client that leaves right after its proof leaves no
// session on the server, and a server that drops the session it just opened
// ends the client's session.
func TestLoginInterruptedLeavesNothing(t *testing.T) {
	t.Run("CloseDuringOpen", func(t *testing.T) {
		token, v, err := scram.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{}, 1)
		stopped := make(chan error, 1)
		p := loginSetup{login: &fakeLogin{account: "stet_scram", v: v, open: func(ctx context.Context) (*proxy.Opened, error) {
			entered <- struct{}{}
			<-ctx.Done()
			stopped <- ctx.Err()
			return nil, ctx.Err()
		}}}.start(t)
		client := make(chan error, 1)
		go func() {
			conn, err := pgx.Connect(context.Background(), connURL(p.addr, "stet_scram", token, nil))
			if err == nil {
				conn.Close(context.Background())
			}
			client <- err
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("Open was never called")
		}

		began := time.Now()
		closed := make(chan error, 1)
		go func() { closed <- p.srv.Close() }()
		select {
		case <-closed:
			if d := time.Since(began); d > time.Second {
				t.Errorf("Close took %v", d)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Close did not return")
		}
		if err := <-stopped; !errors.Is(err, context.Canceled) {
			t.Errorf("Open's context ended with %v, want it cancelled", err)
		}
		if err := <-client; err == nil {
			t.Error("the client logged in through a proxy that closed during its login")
		}
	})

	t.Run("ClientLeavesAfterItsProof", func(t *testing.T) {
		p := loginSetup{}.start(t)
		admin := connect(t, upstream(t), "stet_scram", nil)
		app := fmt.Sprintf("stet-test-left-%d", time.Now().UnixNano())
		conn, fe := startRaw(t, p.addr, "stet_scram", map[string]string{"application_name": app})
		sc := newScramClient(p.tokens["stet_scram"])
		if msg, err := fe.Receive(); err != nil || describe(t, msg) != offer {
			t.Fatalf("the proxy opened with %v, %v", msg, err)
		}
		sendAll(t, fe, &pgproto3.SASLInitialResponse{AuthMechanism: scram.Mechanism, Data: sc.first()})
		msg, err := fe.Receive()
		if err != nil {
			t.Fatal(err)
		}
		first, ok := msg.(*pgproto3.AuthenticationSASLContinue)
		if !ok {
			t.Fatalf("the proxy answered %s", describe(t, msg))
		}
		sendAll(t, fe, &pgproto3.SASLResponse{Data: sc.final(t, first.Data)})
		conn.Close()

		waitForLog(t, p.log, `msg="agent logged in"`) // the session was opened upstream
		waitForSessions(t, admin, app, 0)
	})

	t.Run("ServerDropsTheSession", func(t *testing.T) {
		token, v, err := scram.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		fake := fakeUpstream(t, func(net.Conn) {}) // closes each connection at once
		unread, err := (&pgproto3.NoticeResponse{Severity: "NOTICE", Code: "00000", Message: "read by the Login"}).Encode(nil)
		if err != nil {
			t.Fatal(err)
		}
		p := loginSetup{login: &fakeLogin{account: "stet_scram", v: v, open: func(ctx context.Context) (*proxy.Opened, error) {
			var d net.Dialer
			c, err := d.DialContext(ctx, "tcp", fake)
			if err != nil {
				return nil, err
			}
			return &proxy.Opened{
				Conn:      c,
				PID:       4242,
				SecretKey: []byte{1, 2, 3, 4},
				Params:    map[string]string{"server_version": "14.18", "TimeZone": "UTC", "DateStyle": "ISO, MDY"},
				TxStatus:  'I',
				Unread:    unread,
			}, nil
		}}}.start(t)

		_, fe := startRaw(t, p.addr, "stet_scram", nil)
		began := time.Now()
		got := append(rawLogin(t, fe, token), untilEnd(t, fe)...)
		want := []string{offer, saslContinue, "AuthenticationSASLFinal", "AuthenticationOk",
			"ParameterStatus DateStyle", "ParameterStatus TimeZone", "ParameterStatus server_version",
			"BackendKeyData 4242, a key of 4 bytes", "ReadyForQuery I", "NoticeResponse read by the Login"}
		if !slices.Equal(got, want) {
			t.Errorf("the client got %q, want %q and the end", got, want)
		}
		if d := time.Since(began); d > 3*time.Second {
			t.Errorf("the client's session ended %v after the server dropped it", d)
		}
	})
}

// strangerLogin knows no account, and gives each the same mock verifier: one
// that a token the test holds proves. Open fails the test.
type strangerLogin struct {
	t *testing.T
	v scram.Verifier
}

func (l *strangerLogin) Verifier(string) (scram.Verifier, bool) { return l.v, false }

func (l *strangerLogin) Open(context.Context, string, string, map[string]string) (*proxy.Opened, error) {
	l.t.Error("Open was called for an account the Login does not know")
	return nil, errors.New("not known")
}

// An account the Login does not know is refused, as an unknown account, even
// when the client's proof matches the verifier the Login gave for it: that
// rule is the proxy's own, not only the mock flag scram honours (which this
// test has the exchange ignore).
func TestUnknownAccountIsRefusedWhateverItsProof(t *testing.T) {
	t.Cleanup(proxy.SetMockIgnored()) // registered before the proxy starts, so it runs after Close
	token, v, err := scram.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	p := loginSetup{login: &strangerLogin{t: t, v: v}}.start(t)
	_, fe := startRaw(t, p.addr, "stet_nobody", nil)
	got := append(rawLogin(t, fe, token), untilEnd(t, fe)...)
	if want := []string{offer, saslContinue, "ErrorResponse FATAL 28P01 " + strconv.Quote(passwordFailed("stet_nobody"))}; !slices.Equal(got, want) {
		t.Errorf("the client got %q, want %q and the end", got, want)
	}
	waitForLog(t, p.log, `msg="agent login refused" account=stet_nobody`)
	if log := p.log.String(); !strings.Contains(log, `reason="unknown account"`) || strings.Contains(log, `msg="agent logged in"`) {
		t.Errorf("the log, want the login refused as an unknown account:\n%s", log)
	}
}

// closedSoon fails the test unless the other end of peer, a net.Pipe, is
// closed within a few seconds: a read on peer then ends.
func closedSoon(t *testing.T, peer net.Conn, what string) {
	t.Helper()
	peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("%s: the session's connection was not closed (read: %v)", what, err)
	}
}

// An Open that hands over a session the proxy cannot pass on as the server
// would (a cancel key that is not 4 bytes, a transaction status that is not
// I, T or E), or a session together with an error, is a failed upstream
// login: the client gets 08001, the log says why, and the session's
// connection is closed.
func TestOpenedSessionsThatCannotBeHandedOver(t *testing.T) {
	good := func(conn net.Conn) *proxy.Opened {
		return &proxy.Opened{Conn: conn, PID: 4242, SecretKey: []byte{1, 2, 3, 4},
			Params: map[string]string{"server_version": "14.18"}, TxStatus: 'I'}
	}
	for _, tc := range []struct {
		name string
		open func(conn net.Conn) (*proxy.Opened, error)
		why  string // in the log's err
	}{
		{"KeyOf3Bytes", func(conn net.Conn) (*proxy.Opened, error) {
			o := good(conn)
			o.SecretKey = []byte{1, 2, 3}
			return o, nil
		}, "a cancel key of 3 bytes"},
		{"KeyOf32Bytes", func(conn net.Conn) (*proxy.Opened, error) {
			o := good(conn)
			o.SecretKey = make([]byte, 32)
			return o, nil
		}, "a cancel key of 32 bytes"},
		{"TxStatusX", func(conn net.Conn) (*proxy.Opened, error) {
			o := good(conn)
			o.TxStatus = 'X'
			return o, nil
		}, "transaction status"},
		{"SessionAndError", func(conn net.Conn) (*proxy.Opened, error) {
			return good(conn), errors.New("the Login's own failure")
		}, "the Login's own failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, v, err := scram.NewToken()
			if err != nil {
				t.Fatal(err)
			}
			mine, peer := net.Pipe()
			t.Cleanup(func() { mine.Close(); peer.Close() })
			p := loginSetup{login: &fakeLogin{account: "stet_scram", v: v, open: func(context.Context) (*proxy.Opened, error) {
				return tc.open(mine)
			}}}.start(t)

			_, fe := startRaw(t, p.addr, "stet_scram", nil)
			got := append(rawLogin(t, fe, token), untilEnd(t, fe)...)
			want := []string{offer, saslContinue, "AuthenticationSASLFinal",
				"ErrorResponse FATAL 08001 " + strconv.Quote(`stet: could not open a database session for "stet_scram"`)}
			if !slices.Equal(got, want) {
				t.Errorf("the client got %q, want %q and the end", got, want)
			}
			closedSoon(t, peer, tc.name)
			waitForLog(t, p.log, `reason="upstream login failed"`)
			if log := p.log.String(); !strings.Contains(log, tc.why) {
				t.Errorf("the log does not say %q:\n%s", tc.why, log)
			}
		})
	}
}

// The client is told only that no session could be opened; why is in the
// proxy's log alone.
func TestUpstreamLoginFailureIsNotExplained(t *testing.T) {
	p := loginSetup{passwords: map[string]string{"stet_scram": "not-scram-pw"}}.start(t)
	const msg = `stet: could not open a database session for "stet_scram"`
	conn, err := pgx.Connect(testCtx(t), connURL(p.addr, "stet_scram", p.tokens["stet_scram"], nil))
	if err == nil {
		conn.Close(context.Background())
	}
	wantLoginFatal(t, err, "08001", msg)

	_, fe := startRaw(t, p.addr, "stet_scram", nil)
	want := []string{offer, saslContinue, "AuthenticationSASLFinal", "ErrorResponse FATAL 08001 " + strconv.Quote(msg)}
	if got := append(rawLogin(t, fe, p.tokens["stet_scram"]), untilEnd(t, fe)...); !slices.Equal(got, want) {
		t.Errorf("the client got %q, want %q and the end", got, want)
	}
	log := p.log.String()
	for _, want := range []string{`msg="agent login refused" account=stet_scram`, `reason="upstream login failed"`, "wrong password"} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not hold %q:\n%s", want, log)
		}
	}
}

// An account is the startup packet's name byte for byte: another case is
// another account, which the file does not have. A name with control
// characters reaches the log with them filtered out.
func TestAccountNamesAreComparedByteForByte(t *testing.T) {
	p := loginSetup{}.start(t)
	conn, err := pgx.Connect(testCtx(t), connURL(p.addr, "Stet_Scram", p.tokens["stet_scram"], nil))
	if err == nil {
		conn.Close(context.Background())
	}
	wantLoginFatal(t, err, "28P01", passwordFailed("Stet_Scram"))
	if want := `msg="agent login refused" account=Stet_Scram`; !strings.Contains(p.log.String(), want) {
		t.Errorf("the log does not hold %q:\n%s", want, p.log.String())
	}

	const odd = "stet\x1b[31m_scram\nlevel=ERROR msg=forged"
	_, fe := startRaw(t, p.addr, odd, nil)
	got := rawLogin(t, fe, p.tokens["stet_scram"])
	if want := []string{offer, saslContinue, "ErrorResponse FATAL 28P01 " + strconv.Quote(passwordFailed(odd))}; !slices.Equal(got, want) {
		t.Errorf("the client got %q, want %q", got, want)
	}
	log := p.log.String()
	if strings.Contains(log, "\x1b") || strings.Contains(log, `\x1b`) || strings.Contains(log, `\u001b`) || strings.Contains(log, `_scram\n`) {
		t.Errorf("the log holds the name's control characters:\n%s", log)
	}
	if want := `account="stet [31m_scram level=ERROR msg=forged" remote=`; !strings.Contains(log, want) {
		t.Errorf("the log does not hold %q:\n%s", want, log)
	}
}

// The proxy's log at debug level, through logins that succeed and every kind
// of refusal, holds no token, no verifier, no part of one and no real
// password.
func TestNoSecretInTheProxyLog(t *testing.T) {
	var log syncLog
	p := loginSetup{log: &log}.start(t)
	bad := loginSetup{log: &log, passwords: map[string]string{"stet_scram": "not-scram-pw"}}.start(t)
	other, _, err := scram.NewToken()
	if err != nil {
		t.Fatal(err)
	}

	for account, token := range p.tokens {
		conn, err := pgx.Connect(testCtx(t), connURL(p.addr, account, token, nil))
		if err != nil {
			t.Fatal(err)
		}
		wantOne(t, conn)
		conn.Close(context.Background())
	}
	for _, c := range []struct{ addr, user, password string }{
		{p.addr, "stet_scram", other},
		{p.addr, "stet_nobody", p.tokens["stet_scram"]},
		{p.addr, "stet_scram", agentPasswords["stet_scram"]},
		{p.addr, "stet_scram", p.tokens["stet_md5"]},
		{bad.addr, "stet_scram", bad.tokens["stet_scram"]},
	} {
		if conn, err := pgx.Connect(testCtx(t), connURL(c.addr, c.user, c.password, nil)); err == nil {
			conn.Close(context.Background())
			t.Fatalf("%s logged in at %s", c.user, c.addr)
		}
	}
	_, fe := startRaw(t, p.addr, "stet_scram", nil)
	sendAll(t, fe, &pgproto3.Query{String: "select 1"}) // an exchange that fails
	untilEnd(t, fe)

	text := log.String()
	for _, want := range []string{"level=DEBUG", `msg="agent logged in"`, `reason="bad token"`, `reason="unknown account"`, `reason="upstream login failed"`, `reason="exchange failed"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("the log does not hold %q, so it does not show what it should:\n%s", want, text)
		}
	}
	secrets := map[string]string{"another token": other, "the stored password": "not-scram-pw"}
	for account, password := range agentPasswords {
		secrets["the password of "+account] = password
	}
	for _, c := range []credentials{p.creds, bad.creds} {
		for account, token := range c.tokens {
			secrets["the token of "+account] = token
			text := c.verifiers[account].Text()
			secrets["the verifier of "+account] = text
			_, values, _ := strings.Cut(text, ":") // <salt>$<StoredKey>:<ServerKey>
			salt, keys, _ := strings.Cut(values, "$")
			stored, server, _ := strings.Cut(keys, ":")
			secrets["the salt of "+account], secrets["the StoredKey of "+account], secrets["the ServerKey of "+account] = salt, stored, server
		}
	}
	for what, s := range secrets {
		if s == "" {
			t.Fatalf("%s is empty", what)
		}
		if strings.Contains(text, s) {
			t.Errorf("the log holds %s", what)
		}
	}
}

package custody

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/stetdb/stet/internal/pgtest"
	"github.com/stetdb/stet/internal/proxy"
	"github.com/stetdb/stet/internal/scram"
)

// testServer returns the test server's address, or skips the test.
func testServer(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	return addr
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// verified returns an address of the server at addr with sslmode=verify-full
// and extra in its query, and the TLS setting config.Parse reads from such an
// address. The address's own account is the superuser: an agent's login must
// not use it.
func verified(t *testing.T, addr string, extra url.Values) (string, *tls.Config) {
	t.Helper()
	_, pemPath := pgtest.ServerCert(t, testServer(t))
	q := url.Values{"sslmode": {"verify-full"}, "sslrootcert": {pemPath}}
	for k, v := range extra {
		q[k] = v
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword("postgres", "postgres-pw"), Host: addr, Path: "/stet_test", RawQuery: q.Encode()}
	pc, err := pgconn.ParseConfig(u.String())
	if err != nil {
		t.Fatal(err)
	}
	return u.String(), pc.TLSConfig
}

// storeOf opens a store of accounts with the given stored passwords. An
// empty one is a password_env that is not set.
func storeOf(t *testing.T, pw map[string]string) *Store {
	t.Helper()
	when := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	f := &File{Agents: map[string]Agent{}}
	for name, p := range pw {
		a := Agent{Token: newToken(t), Password: p, Added: when, TokenIssued: when}
		if p == "" {
			a.PasswordEnv = "STET_TEST_UNSET_PASSWORD"
		}
		f.Agents[name] = a
	}
	s, _, _ := openSample(t, f, noEnv)
	return s
}

func newLogin(t *testing.T, s *Store, upstream string, tlsConfig *tls.Config, plainFallback bool) *Login {
	t.Helper()
	l, err := NewLogin(s, upstream, tlsConfig, plainFallback)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// session speaks the protocol over an Opened session directly.
type session struct {
	conn net.Conn
	fe   *pgproto3.Frontend
}

// newSession takes op over until the test ends.
func newSession(t *testing.T, op *proxy.Opened) *session {
	t.Helper()
	t.Cleanup(func() {
		op.Conn.Write([]byte{'X', 0, 0, 0, 4}) // Terminate
		op.Conn.Close()
	})
	return &session{conn: op.Conn, fe: pgproto3.NewFrontend(io.MultiReader(bytes.NewReader(op.Unread), op.Conn), op.Conn)}
}

// query writes a Query message for sql to the connection and returns the
// first row of the answer as text.
func (s *session) query(t *testing.T, sql string) []string {
	t.Helper()
	s.conn.SetDeadline(time.Now().Add(10 * time.Second))
	msg, err := (&pgproto3.Query{String: sql}).Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	var row []string
	for {
		m, err := s.fe.Receive()
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		switch m := m.(type) {
		case *pgproto3.DataRow:
			if row == nil {
				row = []string{}
				for _, v := range m.Values {
					row = append(row, string(v))
				}
			}
		case *pgproto3.ErrorResponse:
			t.Fatalf("%s: %s %s", sql, m.Code, m.Message)
		case *pgproto3.ReadyForQuery:
			return row
		}
	}
}

// uniqueApp is an application_name no other test uses.
func uniqueApp() string { return "stet-custody-" + rand.Text() }

// waitNoSessions waits for the server to have no session named app.
func waitNoSessions(t *testing.T, app string) {
	t.Helper()
	ctx := testCtx(t)
	admin, err := pgx.Connect(ctx, "postgres://postgres:postgres-pw@"+testServer(t)+"/stet_test?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var n int
		if err := admin.QueryRow(ctx, "select count(*) from pg_stat_activity where application_name = $1", app).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions named %s are left on the server", n, app)
		}
	}
}

// fakeUpstream listens on loopback in the server's place and serves each
// connection with serve, in its own goroutine. When the test ends it closes
// the listener and the connections, and waits for serve.
func fakeUpstream(t *testing.T, serve func(c net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		conns  = map[net.Conn]struct{}{}
		closed bool
		wg     sync.WaitGroup
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				c.Close()
				return
			}
			conns[c] = struct{}{}
			wg.Add(1)
			mu.Unlock()
			go func() {
				defer wg.Done()
				defer c.Close()
				serve(c)
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		closed = true
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String()
}

// readOpening reads the first 8 bytes a client sends and reports whether
// they are an SSLRequest.
func readOpening(c net.Conn) (b [8]byte, ssl bool) {
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return b, false
	}
	return b, binary.BigEndian.Uint32(b[0:4]) == 8 && binary.BigEndian.Uint32(b[4:8]) == 80877103
}

// relayTo sends first, then copies c to and from a connection to addr until
// either side closes.
func relayTo(c net.Conn, addr string, first []byte) {
	s, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return
	}
	if _, err := s.Write(first); err != nil {
		s.Close()
		return
	}
	done := make(chan struct{})
	go func() {
		io.Copy(s, c)
		s.Close()
		close(done)
	}()
	io.Copy(c, s)
	c.Close()
	<-done
}

// relay listens in place of the server at addr and relays each connection to
// it. Its count says how many connections it took, and how many are still
// open.
func relay(t *testing.T, addr string) (string, func() (taken, open int)) {
	var mu sync.Mutex
	var taken, open int
	front := fakeUpstream(t, func(c net.Conn) {
		mu.Lock()
		taken++
		open++
		mu.Unlock()
		defer func() {
			mu.Lock()
			open--
			mu.Unlock()
		}()
		relayTo(c, addr, nil)
	})
	return front, func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return taken, open
	}
}

// selfSigned is a self-signed certificate for 127.0.0.1 and localhost.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// writeKeyPair writes c and its key as PEM files.
func writeKeyPair(t *testing.T, c tls.Certificate) (certFile, keyFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	keyDER, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// Open logs in as the account, over TLS with the server's certificate
// checked, and hands over an idle session with what the server said at login.
func TestOpenHandsOverASession(t *testing.T) {
	t.Setenv("PGUSER", "stet_md5")     // neither the environment's account
	t.Setenv("PGDATABASE", "postgres") // nor its database is the agent's
	up, tlsc := verified(t, testServer(t), nil)
	l := newLogin(t, storeOf(t, map[string]string{"stet_scram": "scram-pw"}), up, tlsc, false)

	op, err := l.Open(testCtx(t), "stet_scram", "stet_test", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(t, op)
	if op.TxStatus != 'I' {
		t.Errorf("TxStatus = %q, want 'I'", op.TxStatus)
	}
	for _, name := range []string{"server_version", "TimeZone"} {
		if op.Params[name] == "" {
			t.Errorf("Params has no %s: %v", name, op.Params)
		}
	}
	if len(op.SecretKey) == 0 {
		t.Error("no cancel key")
	}
	got := s.query(t, "select current_user, current_database(), pg_backend_pid(), (select ssl from pg_stat_ssl where pid = pg_backend_pid())")
	want := []string{"stet_scram", "stet_test", strconv.FormatUint(uint64(op.PID), 10), "t"}
	if !slices.Equal(got, want) {
		t.Errorf("current_user, current_database, pid, ssl = %q, want %q", got, want)
	}
}

// The client's startup parameters, and only they, reach the session: not the
// address's, and never user, database or replication.
func TestOpenPassesStartupParameters(t *testing.T) {
	up, tlsc := verified(t, testServer(t), url.Values{"application_name": {"stet-custody-address"}})
	l := newLogin(t, storeOf(t, map[string]string{"stet_scram": "scram-pw"}), up, tlsc, false)

	app := uniqueApp()
	op, err := l.Open(testCtx(t), "stet_scram", "stet_test", map[string]string{
		"application_name": app,
		"options":          "-c statement_timeout=1500",
		"user":             "stet_md5",
		"database":         "postgres",
		"replication":      "database",
	})
	if err != nil {
		t.Fatal(err)
	}
	if op.Params["application_name"] != app {
		t.Errorf("the server reported application_name %q, want %q", op.Params["application_name"], app)
	}
	got := newSession(t, op).query(t, "select current_setting('application_name'), current_setting('statement_timeout'), current_user, current_database(), backend_type from pg_stat_activity where pid = pg_backend_pid()")
	want := []string{app, "1500ms", "stet_scram", "stet_test", "client backend"}
	if !slices.Equal(got, want) {
		t.Errorf("application_name, statement_timeout, current_user, current_database, backend_type = %q, want %q", got, want)
	}

	op, err = l.Open(testCtx(t), "stet_scram", "stet_test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := newSession(t, op).query(t, "select current_setting('application_name')"); !slices.Equal(got, []string{""}) {
		t.Errorf("with no parameters application_name = %q, want empty", got)
	}
}

// Only the stored password is ever sent: not the address's, PGPASSWORD's or
// a password file's.
func TestOpenUsesOnlyTheStoredPassword(t *testing.T) {
	up, tlsc := verified(t, testServer(t), nil)
	u, err := url.Parse(up)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("stet_scram", "scram-pw")
	up = u.String()
	t.Setenv("PGPASSWORD", "scram-pw")
	passfile := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(passfile, []byte("*:*:*:stet_scram:scram-pw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGPASSFILE", passfile)

	t.Run("Empty", func(t *testing.T) {
		l := newLogin(t, storeOf(t, map[string]string{"stet_scram": ""}), up, tlsc, false)
		op, err := l.Open(testCtx(t), "stet_scram", "stet_test", nil)
		if err == nil {
			op.Conn.Close()
			t.Fatal("logged in with an empty stored password")
		}
		if !errors.Is(err, errNoPassword) {
			t.Errorf("err = %v, want the empty password refused before connecting", err)
		}
	})
	t.Run("Wrong", func(t *testing.T) {
		l := newLogin(t, storeOf(t, map[string]string{"stet_scram": "not-scram-pw"}), up, tlsc, false)
		op, err := l.Open(testCtx(t), "stet_scram", "stet_test", nil)
		if err == nil {
			op.Conn.Close()
			t.Fatal("logged in with a wrong stored password")
		}
		if !strings.Contains(err.Error(), "wrong password") {
			t.Errorf("err = %v, want wrong password", err)
		}
	})
}

// The login follows whatever the server asks for: SCRAM, md5, cleartext.
func TestOpenWorksForEachAuthMethod(t *testing.T) {
	accounts := map[string]string{"stet_scram": "scram-pw", "stet_md5": "md5-pw", "stet_clear": "clear-pw"}
	up, tlsc := verified(t, testServer(t), nil)
	l := newLogin(t, storeOf(t, accounts), up, tlsc, false)
	for account := range accounts {
		t.Run(account, func(t *testing.T) {
			op, err := l.Open(testCtx(t), account, "stet_test", nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := newSession(t, op).query(t, "select current_user"); !slices.Equal(got, []string{account}) {
				t.Errorf("current_user = %q, want %s", got, account)
			}
		})
	}
}

// A client certificate in the address is for Stet's own connections: an
// agent's login presents none, even to a server that asks for one.
func TestOpenPresentsNoClientCertificate(t *testing.T) {
	serverCert := selfSigned(t)
	certFile, keyFile := writeKeyPair(t, selfSigned(t))

	for _, tc := range []struct {
		name string
		leg  bool // the setting as config.Parse reads it for the proxy's leg, or pgconn's with the certificate
	}{
		{"TheProxyLegsSetting", true},
		{"ASettingWithACertificate", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// How many certificates each connection presented. The login's
			// is first; when it fails pgconn may also open one to cancel.
			seen := make(chan int, 8)
			t.Cleanup(func() { // after the fake has served every connection
				close(seen)
				for n := range seen {
					if n != 0 {
						t.Errorf("a later connection presented %d client certificates, want none", n)
					}
				}
			})
			fake := fakeUpstream(t, func(c net.Conn) {
				if _, ssl := readOpening(c); !ssl {
					return
				}
				c.Write([]byte{'S'})
				tlsConn := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequestClientCert})
				tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				select {
				case seen <- len(tlsConn.ConnectionState().PeerCertificates):
				default:
				}
			})
			up := (&url.URL{Scheme: "postgres", User: url.User("stet_scram"), Host: fake, Path: "/d",
				RawQuery: url.Values{"sslmode": {"require"}, "sslcert": {certFile}, "sslkey": {keyFile}}.Encode()}).String()
			pc, err := pgconn.ParseConfig(up)
			if err != nil {
				t.Fatal(err)
			}
			if len(pc.TLSConfig.Certificates) == 0 {
				t.Fatal("pgconn did not load the address's certificate: the test would prove nothing")
			}
			setting := pc.TLSConfig
			if tc.leg {
				setting = pc.TLSConfig.Clone()
				setting.Certificates = nil
			}

			l := newLogin(t, storeOf(t, map[string]string{"stet_scram": "pw"}), up, setting, false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// The fake says nothing after the handshake, so the login fails;
			// the test is about what the fake saw before.
			if op, err := l.Open(ctx, "stet_scram", "d", nil); err == nil {
				op.Conn.Close()
			}
			select {
			case n := <-seen:
				if n != 0 {
					t.Errorf("the server saw %d client certificates, want none", n)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the fake server never completed a handshake")
			}
		})
	}
}

// Cancelling the context ends a login the server does not answer, at once,
// and leaves no connection open.
func TestOpenHonoursTheContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		tls  *tls.Config
	}{
		{"Plaintext", nil},
		{"TLS", &tls.Config{InsecureSkipVerify: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached := make(chan struct{}, 1)
			closed := make(chan struct{}, 1)
			var taken atomic.Bool
			fake := fakeUpstream(t, func(c net.Conn) {
				login := taken.CompareAndSwap(false, true) // the first connection is the login's
				readOpening(c)                             // the SSLRequest, or the start of the startup message
				if login {
					reached <- struct{}{}
				}
				io.Copy(io.Discard, c) // until the client closes the connection
				if login {
					closed <- struct{}{}
				}
			})
			l := newLogin(t, storeOf(t, map[string]string{"stet_scram": "pw"}), "postgres://stet_scram@"+fake+"/d", tc.tls, false)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				op, err := l.Open(ctx, "stet_scram", "d", nil)
				if err == nil {
					op.Conn.Close()
				}
				done <- err
			}()
			select {
			case <-reached:
			case <-time.After(5 * time.Second):
				t.Fatal("the login never reached the fake server")
			}
			time.Sleep(50 * time.Millisecond) // into the wait for an answer

			cancel()
			start := time.Now()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("Open succeeded against a server that never answered")
				}
				if d := time.Since(start); d > time.Second {
					t.Errorf("Open took %v to return after the cancel", d)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Open did not return after the cancel")
			}
			select {
			case <-closed:
			case <-time.After(2 * time.Second):
				t.Fatal("the connection to the server is still open")
			}
		})
	}
}

// Verifier gives a known account's verifier, and for any other name a mock
// that stays the same: asked twice, after a restart, and for an account
// removed since.
func TestVerifierKnownAndMock(t *testing.T) {
	f := sample(t)
	s, path, _ := openSample(t, f, func(string) string { return "pw-b" })
	l := newLogin(t, s, "postgres://stet@127.0.0.1:1/d", nil, false)

	v, known := l.Verifier("alice")
	want, err := scram.ParseVerifier(f.Agents["alice"].Token)
	if err != nil {
		t.Fatal(err)
	}
	if !known || v.Text() != want.Text() {
		t.Errorf("alice: known %v, verifier the file's %v; want both", known, v.Text() == want.Text())
	}

	m1, known := l.Verifier("nobody")
	if known {
		t.Error("nobody: known")
	}
	m2, _ := l.Verifier("nobody")
	if m1.Text() != m2.Text() {
		t.Error("nobody: a different verifier the second time")
	}
	if m1.Text() != scram.Mock("nobody", s.MockSecret()).Text() {
		t.Error("nobody: not the mock of the store's secret")
	}

	log, _ := testLog()
	s2, err := OpenStore(path, noEnv, log)
	if err != nil {
		t.Fatal(err)
	}
	if m3, _ := newLogin(t, s2, "postgres://stet@127.0.0.1:1/d", nil, false).Verifier("nobody"); m3.Text() != m1.Text() {
		t.Error("nobody: a different verifier after a restart")
	}

	delete(f.Agents, "bob")
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	bump(t, path, time.Second)
	v, known = l.Verifier("bob")
	if known {
		t.Error("bob, removed: known")
	}
	if v.Text() != scram.Mock("bob", s.MockSecret()).Text() {
		t.Error("bob, removed: not the mock")
	}
}

// A failed login says what kind of failure it was, and quotes neither a
// password nor the address: through Open, Check and TryPassword alike.
func TestLoginErrorsCarryNoSecrets(t *testing.T) {
	const stored = "stet-custody-stored-secret"
	const inAddress = "stet-custody-address-secret"

	// withSecret is up with the address's own password set to inAddress.
	withSecret := func(t *testing.T, up string) string {
		u, err := url.Parse(up)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword("stet_scram", inAddress)
		return u.String()
	}

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) (up string, tlsConfig *tls.Config)
		kind  string
		code  string
	}{
		{"WrongPassword", func(t *testing.T) (string, *tls.Config) {
			return verified(t, testServer(t), nil)
		}, "wrong password", "28P01"},
		{"Unreachable", func(t *testing.T) (string, *tls.Config) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()
			ln.Close()
			return "postgres://x@" + addr + "/stet_test?sslmode=disable", nil
		}, "cannot reach the server", ""},
		{"Certificate", func(t *testing.T) (string, *tls.Config) {
			up, _ := verified(t, testServer(t), nil)
			others := x509.NewCertPool()
			others.AddCert(selfSigned(t).Leaf)
			return up, &tls.Config{RootCAs: others, ServerName: "127.0.0.1"}
		}, "certificate", ""},
		{"RefusedTLS", func(t *testing.T) (string, *tls.Config) {
			fake := fakeUpstream(t, func(c net.Conn) {
				if _, ssl := readOpening(c); ssl {
					c.Write([]byte{'N'})
				}
				io.Copy(io.Discard, c)
			})
			return "postgres://x@" + fake + "/stet_test?sslmode=require", &tls.Config{InsecureSkipVerify: true}
		}, "refused TLS", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, tlsc := tc.setup(t)
			up = withSecret(t, up)
			u, err := url.Parse(up)
			if err != nil {
				t.Fatal(err)
			}
			host := u.Host
			l := newLogin(t, storeOf(t, map[string]string{"stet_scram": stored}), up, tlsc, false)

			calls := map[string]func(ctx context.Context) error{
				"Open": func(ctx context.Context) error {
					op, err := l.Open(ctx, "stet_scram", "stet_test", nil)
					if err == nil {
						op.Conn.Close()
					}
					return err
				},
				"Check": func(ctx context.Context) error { return l.Check(ctx, "stet_scram", "stet_test") },
				"TryPassword": func(ctx context.Context) error {
					return TryPassword(ctx, up, tlsc, false, "stet_scram", stored, "stet_test")
				},
			}
			for name, call := range calls {
				err := call(testCtx(t))
				if err == nil {
					t.Errorf("%s: logged in", name)
					continue
				}
				var ce *pgconn.ConnectError
				if errors.As(err, &ce) {
					t.Errorf("%s: the error wraps pgconn's, whose Config holds the password", name)
				}
				for _, text := range []string{err.Error(), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
					for _, secret := range []string{stored, inAddress, up, host, "127.0.0.1"} {
						if strings.Contains(text, secret) {
							t.Errorf("%s: the error %q quotes %q", name, text, secret)
						}
					}
				}
				if !strings.Contains(err.Error(), tc.kind) {
					t.Errorf("%s: err = %q, want it to say %q", name, err, tc.kind)
				}
				if tc.code != "" && !strings.Contains(err.Error(), tc.code) {
					t.Errorf("%s: err = %q, want the server's SQLSTATE %s", name, err, tc.code)
				}
			}
		})
	}
}

// Check and TryPassword log in and leave no session behind; a wrong password
// is an error.
func TestCheckAndTryPassword(t *testing.T) {
	app := uniqueApp()
	front, count := relay(t, testServer(t))
	// pgx's and pgxpool's own keys, which Stet's own pool takes from the
	// same address, are not startup parameters.
	up, tlsc := verified(t, front, url.Values{"application_name": {app}, "pool_max_conns": {"4"}, "default_query_exec_mode": {"exec"}})
	l := newLogin(t, storeOf(t, map[string]string{"stet_md5": "md5-pw", "stet_scram": "not-scram-pw"}), up, tlsc, false)
	ctx := testCtx(t)

	// noneLeft waits until the server shows no session named app and the
	// relay has no connection open, after taking taken in all.
	noneLeft := func(t *testing.T, taken int) {
		t.Helper()
		waitNoSessions(t, app)
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			n, open := count()
			if n != taken {
				t.Fatalf("the relay took %d connections, want %d", n, taken)
			}
			if open == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d connections to the server are still open", open)
			}
		}
	}

	if err := l.Check(ctx, "stet_md5", "stet_test"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	noneLeft(t, 1)
	if err := l.Check(ctx, "stet_scram", "stet_test"); err == nil || !strings.Contains(err.Error(), "wrong password") {
		t.Errorf("Check with a wrong stored password = %v, want wrong password", err)
	}
	if err := l.Check(ctx, "nobody", "stet_test"); err == nil {
		t.Error("Check of an account that is not in the file succeeded")
	}
	noneLeft(t, 2)

	if err := TryPassword(ctx, up, tlsc, false, "stet_clear", "clear-pw", "stet_test"); err != nil {
		t.Fatalf("TryPassword: %v", err)
	}
	noneLeft(t, 3)
	if err := TryPassword(ctx, up, tlsc, false, "stet_clear", "not-clear-pw", "stet_test"); err == nil || !strings.Contains(err.Error(), "wrong password") {
		t.Errorf("TryPassword with a wrong password = %v, want wrong password", err)
	}
	if err := TryPassword(ctx, up, tlsc, false, "stet_clear", "", "stet_test"); !errors.Is(err, errNoPassword) {
		t.Errorf("TryPassword with an empty password = %v, want it refused before connecting", err)
	}
	noneLeft(t, 4)
}

// With plainFallback a server that declines TLS gets the login in
// plaintext; without it, nothing is sent in plaintext.
func TestOpenFallsBackToPlaintextOnlyWhenAllowed(t *testing.T) {
	server := testServer(t)
	s := storeOf(t, map[string]string{"stet_scram": "scram-pw"})

	// declining serves in the server's place: it answers N to a TLS request,
	// then relays to the server whatever comes in plaintext, and says so on
	// plaintext.
	declining := func(t *testing.T, plaintext chan<- struct{}) string {
		return "postgres://x@" + fakeUpstream(t, func(c net.Conn) {
			opening, ssl := readOpening(c)
			if ssl {
				c.Write([]byte{'N'})
				if opening, _ = readOpening(c); opening == ([8]byte{}) {
					return // closed: pgconn tries plaintext on a new connection
				}
			}
			select {
			case plaintext <- struct{}{}:
			default:
			}
			relayTo(c, server, opening[:])
		}) + "/stet_test?sslmode=prefer"
	}

	t.Run("Allowed", func(t *testing.T) {
		up := declining(t, make(chan struct{}, 8))
		op, err := newLogin(t, s, up, &tls.Config{InsecureSkipVerify: true}, true).Open(testCtx(t), "stet_scram", "stet_test", nil)
		if err != nil {
			t.Fatal(err)
		}
		got := newSession(t, op).query(t, "select current_user, (select ssl from pg_stat_ssl where pid = pg_backend_pid())")
		if want := []string{"stet_scram", "f"}; !slices.Equal(got, want) {
			t.Errorf("current_user, ssl = %q, want %q", got, want)
		}
	})
	t.Run("NotAllowed", func(t *testing.T) {
		plaintext := make(chan struct{}, 8)
		t.Cleanup(func() { // after the fake has served every connection
			if len(plaintext) != 0 {
				t.Error("the login went on in plaintext")
			}
		})
		up := declining(t, plaintext)
		_, err := newLogin(t, s, up, &tls.Config{InsecureSkipVerify: true}, false).Open(testCtx(t), "stet_scram", "stet_test", nil)
		if err == nil || !strings.Contains(err.Error(), "refused TLS") {
			t.Errorf("err = %v, want refused TLS", err)
		}
	})
}

// trusting serves in the server's place: it takes any login without asking
// for a password, as trust does, and reports the protocol version each
// startup packet asked for.
func trusting(t *testing.T, versions chan<- uint32) string {
	return fakeUpstream(t, func(c net.Conn) {
		var head [8]byte
		if _, err := io.ReadFull(c, head[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(head[0:4])
		if n < 8 || n > 10000 {
			return
		}
		if _, err := io.CopyN(io.Discard, c, int64(n-8)); err != nil {
			return
		}
		select {
		case versions <- binary.BigEndian.Uint32(head[4:8]):
		default:
		}
		var out []byte
		out, _ = (&pgproto3.AuthenticationOk{}).Encode(out)
		out, _ = (&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{1, 2, 3, 4}}).Encode(out)
		out, _ = (&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(out)
		c.Write(out)
		io.Copy(io.Discard, c)
	})
}

// cleartextAsker serves in the server's place: it asks every login for a
// cleartext password, reports whether a password message came, and closes.
func cleartextAsker(t *testing.T, sent chan<- bool) string {
	return fakeUpstream(t, func(c net.Conn) {
		var head [8]byte
		if _, err := io.ReadFull(c, head[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(head[0:4])
		if n < 8 || n > 10000 {
			return
		}
		if _, err := io.CopyN(io.Discard, c, int64(n-8)); err != nil {
			return
		}
		ask, _ := (&pgproto3.AuthenticationCleartextPassword{}).Encode(nil)
		c.Write(ask)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		var typ [1]byte
		_, err := io.ReadFull(c, typ[:])
		sent <- err == nil && typ[0] == 'p'
	})
}

// require_auth=scram-sha-256 in the upstream address reaches the logins Stet
// makes for agents (pgconn checks it before it answers the server): a server
// that asks for a cleartext password gets none. Without it the password is
// sent, as libpq would.
func TestLoginsHonourRequireAuth(t *testing.T) {
	for _, tc := range []struct {
		query string
		sent  bool
	}{
		{"?sslmode=disable&require_auth=scram-sha-256", false},
		{"?sslmode=disable", true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			sent := make(chan bool, 1)
			up := "postgres://stet@" + cleartextAsker(t, sent) + "/db" + tc.query
			l := newLogin(t, storeOf(t, map[string]string{"agent_a": "not-to-be-sent"}), up, nil, false)
			if op, err := l.Open(testCtx(t), "agent_a", "db", nil); err == nil {
				op.Conn.Close()
				t.Error("Open logged in to a server that never said AuthenticationOk")
			}
			select {
			case got := <-sent:
				if got != tc.sent {
					t.Errorf("the password was sent: %v, want %v", got, tc.sent)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the fake server never heard back")
			}
		})
	}
}

// Every login speaks protocol 3.0, which Stet speaks to clients, whatever
// the address or Stet's environment asks for: a 3.2 session would hand a
// 3.0 client a cancel key it cannot read.
func TestLoginsAskForProtocol30(t *testing.T) {
	newest := url.Values{"min_protocol_version": {"3.2"}, "max_protocol_version": {"latest"}}
	for _, tc := range []struct {
		name  string
		query url.Values // in the address
		env   bool       // in PGMINPROTOCOLVERSION and PGMAXPROTOCOLVERSION
	}{
		{"InTheAddress", newest, false},
		{"InTheEnvironment", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env {
				t.Setenv("PGMINPROTOCOLVERSION", "3.2")
				t.Setenv("PGMAXPROTOCOLVERSION", "latest")
			}
			s := storeOf(t, map[string]string{"stet_scram": "scram-pw"})

			// What each login asks for, seen by a fake server.
			versions := make(chan uint32, 8)
			fake := (&url.URL{Scheme: "postgres", User: url.User("x"), Host: trusting(t, versions), Path: "/d", RawQuery: tc.query.Encode()}).String()
			l := newLogin(t, s, fake, nil, false)
			logins := map[string]func(ctx context.Context) error{
				"Open": func(ctx context.Context) error {
					op, err := l.Open(ctx, "stet_scram", "d", nil)
					if err == nil {
						op.Conn.Close()
					}
					return err
				},
				"Check":       func(ctx context.Context) error { return l.Check(ctx, "stet_scram", "d") },
				"TryPassword": func(ctx context.Context) error { return TryPassword(ctx, fake, nil, false, "stet_scram", "pw", "d") },
			}
			for name, login := range logins {
				if err := login(testCtx(t)); err != nil {
					t.Errorf("%s: %v", name, err)
					continue
				}
				select {
				case v := <-versions:
					if v != pgproto3.ProtocolVersion30 {
						t.Errorf("%s asked for protocol %d.%d, want 3.0", name, v>>16, v&0xffff)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("%s: the fake server got no startup packet", name)
				}
			}

			// The test server, which speaks only 3.0, takes each login.
			up, tlsc := verified(t, testServer(t), tc.query)
			l = newLogin(t, s, up, tlsc, false)
			op, err := l.Open(testCtx(t), "stet_scram", "stet_test", nil)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if len(op.SecretKey) != 4 {
				t.Errorf("the cancel key is %d bytes, want 4", len(op.SecretKey))
			}
			if got := newSession(t, op).query(t, "select current_user"); !slices.Equal(got, []string{"stet_scram"}) {
				t.Errorf("current_user = %q, want stet_scram", got)
			}
			if err := l.Check(testCtx(t), "stet_scram", "stet_test"); err != nil {
				t.Errorf("Check: %v", err)
			}
			if err := TryPassword(testCtx(t), up, tlsc, false, "stet_scram", "scram-pw", "stet_test"); err != nil {
				t.Errorf("TryPassword: %v", err)
			}
		})
	}
}

// The bytes a frontend has read and not handed out are the last it read:
// what Open puts in Unread.
func TestUnreadIsWhatTheFrontendHasNotHandedOut(t *testing.T) {
	var stream []byte
	stream, _ = (&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(stream)
	rest, _ := (&pgproto3.ParameterStatus{Name: "TimeZone", Value: "UTC"}).Encode(nil)
	rest, _ = (&pgproto3.NoticeResponse{Severity: "NOTICE", Code: "00000", Message: "hello"}).Encode(rest)
	stream = append(stream, rest...)
	// Also past what lastRead keeps, so the trimming is exercised.
	filler := bytes.Repeat([]byte{'x'}, 3*keepRead)

	r := &lastRead{r: io.MultiReader(bytes.NewReader(filler), bytes.NewReader(stream))}
	if _, err := io.ReadFull(r, make([]byte, len(filler))); err != nil {
		t.Fatal(err)
	}
	fe := pgproto3.NewFrontend(r, io.Discard)
	if _, err := fe.Receive(); err != nil {
		t.Fatal(err)
	}
	got, ok := r.last(fe.ReadBufferLen())
	if !ok || !bytes.Equal(got, rest) {
		t.Errorf("unread = %q (ok %v), want %q", got, ok, rest)
	}
	if _, ok := r.last(3 * keepRead); ok {
		t.Error("last gave more bytes than it keeps")
	}
}

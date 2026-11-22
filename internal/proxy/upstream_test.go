package proxy_test

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
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/stetdb/stet/internal/proxy"
)

// fakeUpstream listens on loopback in the server's place and serves each
// connection the proxy opens with serve, in its own goroutine. When the test
// ends it closes the listener and the connections, and waits for serve.
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

// relayTo copies c to and from a plaintext connection to addr until either
// side closes.
func relayTo(c net.Conn, addr string) {
	s, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
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

func unverified() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }

// wantNoUpstream fails the test unless err is the proxy's FATAL 08001 with the
// message msg.
func wantNoUpstream(t *testing.T, err error, msg string) {
	t.Helper()
	wantFatal08001(t, err, msg, func(got string) bool { return got == msg })
}

// wantNoUpstreamPrefix is wantNoUpstream for a message that starts with prefix.
func wantNoUpstreamPrefix(t *testing.T, err error, prefix string) {
	t.Helper()
	wantFatal08001(t, err, prefix+"…", func(got string) bool { return strings.HasPrefix(got, prefix) })
}

func wantFatal08001(t *testing.T, err error, want string, match func(string) bool) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want FATAL 08001 %q", err, want)
	}
	if pgErr.Severity != "FATAL" || pgErr.Code != "08001" || !match(pgErr.Message) {
		t.Fatalf("got %s %s %q, want FATAL 08001 %q", pgErr.Severity, pgErr.Code, pgErr.Message, want)
	}
}

// connectErr is what pgx gets when it logs in through the proxy at addr.
func connectErr(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, connURL(addr, "stet_scram", "scram-pw", nil))
	if err == nil {
		conn.Close(context.Background())
	}
	return err
}

// A session through a TLS Upstream is a TLS session on the server; through a
// plaintext one it is not.
func TestSessionOverTLSIsTLSOnTheServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		up   proxy.Upstream
		want bool
	}{
		{"TLS", tlsUpstream(t), true},
		{"Plaintext", proxy.Upstream{Addr: upstream(t)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := startProxyTo(t, tc.up, nil, proxy.Policy{})
			conn := connect(t, addr, "stet_scram", nil)
			var ssl bool
			if err := conn.QueryRow(testCtx(t), "select ssl from pg_stat_ssl where pid = pg_backend_pid()").Scan(&ssl); err != nil {
				t.Fatal(err)
			}
			if ssl != tc.want {
				t.Errorf("pg_stat_ssl.ssl = %v, want %v", ssl, tc.want)
			}
		})
	}
}

// A server that answers the TLS request with N gets the session in
// plaintext only when the Upstream allows it (sslmode=prefer); otherwise the
// client is refused and nothing more is sent to the server.
func TestUpstreamDeclinesTLS(t *testing.T) {
	server := upstream(t)

	t.Run("Required", func(t *testing.T) {
		after := make(chan int64, 1) // bytes the proxy sent after the answer
		fake := fakeUpstream(t, func(c net.Conn) {
			if _, ssl := readOpening(c); !ssl {
				return
			}
			c.Write([]byte{'N'})
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, _ := io.Copy(io.Discard, c)
			after <- n
		})
		addr, _ := startProxyTo(t, proxy.Upstream{Addr: fake, TLS: unverified()}, nil, proxy.Policy{})
		wantNoUpstream(t, connectErr(addr), "stet: upstream does not accept TLS, and the sslmode of upstream needs it")
		select {
		case n := <-after:
			if n != 0 {
				t.Errorf("the proxy sent %d bytes in plaintext after the server declined TLS", n)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the fake upstream was never asked for TLS")
		}
	})

	t.Run("Preferred", func(t *testing.T) {
		fake := fakeUpstream(t, func(c net.Conn) {
			if _, ssl := readOpening(c); !ssl {
				return
			}
			c.Write([]byte{'N'})
			relayTo(c, server)
		})
		addr, _ := startProxyTo(t, proxy.Upstream{Addr: fake, TLS: unverified(), PlainFallback: true}, nil, proxy.Policy{})
		conn := connect(t, addr, "stet_scram", nil)
		var one int
		if err := conn.QueryRow(testCtx(t), "select 1").Scan(&one); err != nil {
			t.Fatal(err)
		}
		if one != 1 {
			t.Errorf("select 1 = %d", one)
		}
	})
}

// otherCert is a self-signed certificate for the test server's names that
// did not sign the test server's.
func otherCert(t *testing.T) *x509.Certificate { return selfSigned(t).Leaf }

// selfSigned is a self-signed certificate for the test server's names, with
// its key, for a fake upstream to present.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost", "127.0.0.1"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}
}

// The server's certificate is checked as the TLS setting says: another
// issuer, or another name, ends the session; an unverified setting connects.
func TestUpstreamCertificateIsChecked(t *testing.T) {
	good := tlsUpstream(t)
	others := x509.NewCertPool()
	others.AddCert(otherCert(t))
	for _, tc := range []struct {
		name string
		tls  *tls.Config
		ok   bool
	}{
		{"OtherIssuer", &tls.Config{RootCAs: others, ServerName: good.TLS.ServerName}, false},
		{"OtherName", &tls.Config{RootCAs: good.TLS.RootCAs, ServerName: "wrong.example"}, false},
		{"Unverified", unverified(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := startProxyTo(t, proxy.Upstream{Addr: good.Addr, TLS: tc.tls}, nil, proxy.Policy{})
			if !tc.ok {
				wantNoUpstreamPrefix(t, connectErr(addr), "stet: TLS to upstream "+good.Addr+" failed: ")
				return
			}
			conn := connect(t, addr, "stet_scram", nil)
			var ssl bool
			if err := conn.QueryRow(testCtx(t), "select ssl from pg_stat_ssl where pid = pg_backend_pid()").Scan(&ssl); err != nil {
				t.Fatal(err)
			}
			if !ssl {
				t.Error("the session is not TLS on the server")
			}
		})
	}
}

// An answer to the TLS request other than S or N ends the session.
func TestOddTLSAnswerIsRefused(t *testing.T) {
	fake := fakeUpstream(t, func(c net.Conn) {
		readOpening(c)
		c.Write([]byte{'E'})
		io.Copy(io.Discard, c)
	})
	addr, _ := startProxyTo(t, proxy.Upstream{Addr: fake, TLS: unverified()}, nil, proxy.Policy{})
	wantNoUpstream(t, connectErr(addr), "stet: upstream answered the TLS request with 'E', not S or N")
}

// The proxy reads the server's answer to the TLS request alone: plaintext
// the server (or someone in between) sent behind the S is not taken for
// what comes inside TLS, and never reaches the client.
//
// The fake then answers as a real TLS server. Read alone, the S leaves the
// plaintext first in line for the proxy's TLS handshake, which fails on it
// at once. A proxy that read the plaintext along with the S instead
// completes the handshake past it: it is told so, and when it then hands
// the plaintext on, the client gets the AuthenticationOk. Either way the
// test fails at once, not when a deadline runs out.
func TestSSLAnswerIsReadAlone(t *testing.T) {
	cert := selfSigned(t)
	handshake := make(chan error, 1) // the fake's: nil once it completed
	fake := fakeUpstream(t, func(c net.Conn) {
		readOpening(c)
		msg := []byte{'S'}
		msg, _ = (&pgproto3.AuthenticationOk{}).Encode(msg)
		msg, _ = (&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(msg)
		c.Write(msg)
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
		tc.SetDeadline(time.Now().Add(5 * time.Second))
		err := tc.Handshake()
		handshake <- err
		if err == nil {
			// The startup message, then nothing: the session ends with no
			// answer from inside TLS.
			var n [4]byte
			if _, err := io.ReadFull(tc, n[:]); err == nil {
				io.CopyN(io.Discard, tc, int64(binary.BigEndian.Uint32(n[:]))-4)
			}
			tc.Close()
		}
	})
	addr, _ := startProxyTo(t, proxy.Upstream{Addr: fake, TLS: unverified()}, nil, proxy.Policy{})

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	began := time.Now()
	fe := pgproto3.NewFrontend(conn, conn)
	fe.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "stet_trust", "database": "stet_test"},
	})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	var fatal *pgproto3.ErrorResponse
	for {
		msg, err := fe.Receive()
		if err != nil {
			break
		}
		switch m := msg.(type) {
		case *pgproto3.AuthenticationOk:
			t.Fatal("the client got the AuthenticationOk sent in plaintext behind the S")
		case *pgproto3.ErrorResponse:
			c := *m
			fatal = &c
		default:
			t.Errorf("client got %T", msg)
		}
	}
	select {
	case err := <-handshake:
		if err == nil {
			t.Fatal("the proxy completed TLS with the server past the plaintext sent behind the S: it read that along with the S")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fake server never got to its TLS handshake")
	}
	if fatal == nil {
		t.Fatal("the client got no error")
	}
	if fatal.Code != "08001" || !strings.HasPrefix(fatal.Message, "stet: TLS to upstream "+fake+" failed: ") {
		t.Errorf("got %s %q, want 08001 stet: TLS to upstream %s failed: …", fatal.Code, fatal.Message, fake)
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Errorf("the session took %v to be refused, want it refused at once", d)
	}
}

// Fakes that stop answering, or drop the connection, partway through TLS.
var stalls = []struct {
	name  string
	serve func(c net.Conn)
}{
	{"NoAnswer", func(c net.Conn) {
		readOpening(c)
		io.Copy(io.Discard, c)
	}},
	{"SilentHandshake", func(c net.Conn) {
		readOpening(c)
		c.Write([]byte{'S'})
		io.Copy(io.Discard, c)
	}},
	{"DropsHandshake", func(c net.Conn) {
		readOpening(c)
		c.Write([]byte{'S'})
		var hdr [5]byte // the ClientHello's record header
		io.ReadFull(c, hdr[:])
	}},
}

// A server that stops answering, or drops the connection, during the TLS
// request or handshake ends that session alone, within the dial timeout:
// the next session gets the same answer in time, and Close returns.
func TestSilentUpstreamEndsOnlyThatSession(t *testing.T) {
	for _, st := range stalls {
		t.Run(st.name, func(t *testing.T) {
			// Registered before the proxy starts, so it runs after Close.
			t.Cleanup(proxy.SetDialTimeout(300 * time.Millisecond))
			fake := fakeUpstream(t, st.serve)
			addr, srv := startProxyTo(t, proxy.Upstream{Addr: fake, TLS: unverified()}, nil, proxy.Policy{})
			for i := range 2 {
				start := time.Now()
				wantNoUpstreamPrefix(t, connectErr(addr), "stet: TLS to upstream "+fake+" failed: ")
				if d := time.Since(start); d > 2*time.Second {
					t.Errorf("session %d took %v to be refused", i+1, d)
				}
			}
			closed := make(chan error, 1)
			go func() { closed <- srv.Close() }()
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("Close did not return")
			}
		})
	}
}

// Close does not wait out a server that is silent during the TLS request or
// handshake: it closes that connection as it does every session's.
func TestCloseDoesNotWaitForASilentUpstream(t *testing.T) {
	for _, st := range stalls[:2] {
		t.Run(st.name, func(t *testing.T) {
			reached := make(chan struct{}, 1)
			fake := fakeUpstream(t, func(c net.Conn) {
				reached <- struct{}{}
				st.serve(c)
			})
			addr, srv := startProxyTo(t, proxy.Upstream{Addr: fake, TLS: unverified()}, nil, proxy.Policy{})
			session := make(chan error, 1)
			go func() { session <- connectErr(addr) }()
			defer func() { <-session }() // ended by Close, or by its own timeout
			select {
			case <-reached:
			case <-time.After(5 * time.Second):
				t.Fatal("the session never reached the fake upstream")
			}
			time.Sleep(100 * time.Millisecond) // into the wait for the server

			start := time.Now()
			closed := make(chan error, 1)
			go func() { closed <- srv.Close() }()
			select {
			case <-closed:
				if d := time.Since(start); d > time.Second {
					t.Errorf("Close took %v", d)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Close did not return")
			}
		})
	}
}

// A panic inside the dial (here in a certificate check) ends that session
// and leaves nothing behind: the connection to the server is closed and no
// longer counted, so Close returns.
func TestPanicInDialLeavesNothingBehind(t *testing.T) {
	cert := selfSigned(t)
	handshake := make(chan error, 1) // the fake's
	fake := fakeUpstream(t, func(c net.Conn) {
		if _, ssl := readOpening(c); !ssl {
			return
		}
		c.Write([]byte{'S'})
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
		tc.SetDeadline(time.Now().Add(5 * time.Second))
		select { // pgx may try more than once: the first is kept
		case handshake <- tc.Handshake():
		default:
		}
	})
	panicking := &tls.Config{
		InsecureSkipVerify: true, // still calls VerifyPeerCertificate
		VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error {
			panic("the certificate check panicked")
		},
	}
	addr, srv := startProxyTo(t, proxy.Upstream{Addr: fake, TLS: panicking}, nil, proxy.Policy{})
	if err := connectErr(addr); err == nil {
		t.Fatal("the session connected, want it ended")
	}
	select {
	case err := <-handshake:
		if err == nil {
			t.Error("the server completed the handshake with a proxy whose check panicked")
		}
	case <-time.After(3 * time.Second):
		t.Error("the connection to the server was left open after the panic")
	}

	closed := make(chan error, 1)
	go func() { closed <- srv.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return: the dial that panicked left its connection counted")
	}
}

// A cancel request goes to the server only under the same rule as a
// session: a server that declines TLS, when TLS is required, is not sent it.
func TestCancelOverTLSNeedsTLS(t *testing.T) {
	cancel := make([]byte, 16)
	binary.BigEndian.PutUint32(cancel[0:4], 16)
	binary.BigEndian.PutUint32(cancel[4:8], 80877102)
	binary.BigEndian.PutUint32(cancel[8:12], 4242)
	binary.BigEndian.PutUint32(cancel[12:16], 0x5ec7e7)

	for _, tc := range []struct {
		name     string
		fallback bool
		want     []byte // what the fake gets besides an SSLRequest
	}{
		{"Required", false, nil},
		{"Preferred", true, cancel}, // the fake can tell a CancelRequest
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan []byte, 1)
			fake := fakeUpstream(t, func(c net.Conn) {
				first, ssl := readOpening(c)
				var b []byte
				if ssl {
					c.Write([]byte{'N'})
				} else {
					b = first[:]
				}
				// Up to one CancelRequest, then closed, as Postgres does.
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				rest := make([]byte, len(cancel)-len(b))
				n, _ := io.ReadFull(c, rest)
				got <- append(b, rest[:n]...)
			})
			addr, _ := startProxyTo(t, proxy.Upstream{Addr: fake, TLS: unverified(), PlainFallback: tc.fallback}, nil, proxy.Policy{})

			conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := conn.Write(cancel); err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, conn) // the proxy closes it once the cancel is done

			select {
			case b := <-got:
				if !bytes.Equal(b, tc.want) {
					t.Errorf("the fake upstream got % x, want % x", b, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the cancel request never reached the fake upstream")
			}
		})
	}
}

// dialPlain opens a plaintext connection to addr, closed when the test ends.
func dialPlain(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// dialTLS opens a connection to the server at addr and asks it for TLS with
// cfg, as libpq does.
func dialTLS(t *testing.T, addr string, cfg *tls.Config) net.Conn {
	t.Helper()
	c := dialPlain(t, addr)
	c.SetDeadline(time.Now().Add(5 * time.Second))
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
	if answer[0] != 'S' {
		t.Fatalf("the server at %s answered the TLS request with %q", addr, answer[0])
	}
	tc := tls.Client(c, cfg)
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	return tc
}

// firstAnswer starts a session for user on the test database over c and
// returns the first message it is sent back: type, length and body.
func firstAnswer(t *testing.T, c net.Conn, user string) []byte {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	pkt, err := (&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": user, "database": "stet_test"},
	}).Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(pkt); err != nil {
		t.Fatal(err)
	}
	var hdr [5]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		t.Fatal(err)
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n < 4 || n > 1<<16 {
		t.Fatalf("first message %q has length %d", hdr[0], n)
	}
	msg := make([]byte, 1+n)
	copy(msg, hdr[:])
	if _, err := io.ReadFull(c, msg[5:]); err != nil {
		t.Fatal(err)
	}
	return msg
}

// mechanisms reads the list of an AuthenticationSASL message.
func mechanisms(t *testing.T, msg []byte) []string {
	t.Helper()
	var sasl pgproto3.AuthenticationSASL
	if msg[0] != 'R' || sasl.Decode(msg[5:]) != nil {
		t.Fatalf("got % x, want an AuthenticationSASL", msg)
	}
	return sasl.AuthMechanisms
}

// Each session through the proxy as a plain passthrough, watched by a gate
// that sees every session, and watched by one that enforces.
var gates = []struct {
	name   string
	gate   func() proxy.Gate
	policy proxy.Policy
}{
	{"Relayed", func() proxy.Gate { return nil }, proxy.Policy{}},
	{"Watched", func() proxy.Gate { return newRecordingGate(isWrite) }, proxy.Policy{}},
	{"Enforced", func() proxy.Gate { return closedPointGate(isWrite) }, proxy.Policy{Enforce: true}},
}

// Over TLS the server offers SCRAM-SHA-256-PLUS, which a client on the
// proxy's plaintext leg cannot use: the proxy takes it out of the list.
func TestSASLListOverTLSHasNoPlus(t *testing.T) {
	up := tlsUpstream(t)
	direct := mechanisms(t, firstAnswer(t, dialTLS(t, up.Addr, up.TLS), "stet_scram"))
	if !slices.Contains(direct, "SCRAM-SHA-256-PLUS") {
		t.Fatalf("the test server offers %q over TLS, without SCRAM-SHA-256-PLUS: nothing to take out", direct)
	}
	for _, g := range gates {
		t.Run(g.name, func(t *testing.T) {
			addr, _ := startProxyTo(t, up, g.gate(), g.policy)
			got := mechanisms(t, firstAnswer(t, dialPlain(t, addr), "stet_scram"))
			if want := []string{"SCRAM-SHA-256"}; !slices.Equal(got, want) {
				t.Errorf("the client is offered %q, want %q", got, want)
			}
		})
	}
}

// Over a plaintext upstream the client gets the server's list as it was
// sent, byte for byte.
func TestSASLListOverPlaintextIsTheServers(t *testing.T) {
	server := upstream(t)
	direct := firstAnswer(t, dialPlain(t, server), "stet_scram")
	for _, g := range gates {
		t.Run(g.name, func(t *testing.T) {
			addr, _ := startProxyTo(t, proxy.Upstream{Addr: server}, g.gate(), g.policy)
			if got := firstAnswer(t, dialPlain(t, addr), "stet_scram"); !bytes.Equal(got, direct) {
				t.Errorf("the client got % x, the server sends % x", got, direct)
			}
		})
	}
}

// A server that offers channel-bound SCRAM alone leaves a plaintext client
// nothing to log in with: the proxy ends the session with an error that says
// why.
func TestOnlyChannelBoundAuthIsRefused(t *testing.T) {
	cert := selfSigned(t)
	offer, err := (&pgproto3.AuthenticationSASL{AuthMechanisms: []string{"SCRAM-SHA-256-PLUS"}}).Encode(nil)
	if err != nil {
		t.Fatal(err)
	}
	fake := fakeUpstream(t, func(c net.Conn) {
		if _, ssl := readOpening(c); !ssl {
			return
		}
		c.Write([]byte{'S'})
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
		tc.SetDeadline(time.Now().Add(5 * time.Second))
		var n [4]byte // the startup message's length
		if _, err := io.ReadFull(tc, n[:]); err != nil {
			return
		}
		if _, err := io.CopyN(io.Discard, tc, int64(binary.BigEndian.Uint32(n[:]))-4); err != nil {
			return
		}
		tc.Write(offer)
		io.Copy(io.Discard, tc)
	})
	for _, g := range gates {
		t.Run(g.name, func(t *testing.T) {
			addr, _ := startProxyTo(t, proxy.Upstream{Addr: fake, TLS: unverified()}, g.gate(), g.policy)
			const want = "stet: the server offers only channel-bound authentication, which cannot pass through stet"
			err := connectErr(addr)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("err = %v, want FATAL 28000 %q", err, want)
			}
			if pgErr.Severity != "FATAL" || pgErr.Code != "28000" || pgErr.Message != want {
				t.Errorf("got %s %s %q, want FATAL 28000 %q", pgErr.Severity, pgErr.Code, pgErr.Message, want)
			}
		})
	}
}

// runPsql runs select 1 with psql through addr as user, in plaintext,
// giving password in PGPASSWORD. No other PG variable of the environment
// reaches it.
func runPsql(t *testing.T, psql, addr, user, password string) (stdout, stderr string, err error) {
	t.Helper()
	return runPsqlWith(t, psql, addr, user, password, "sslmode=disable")
}

// runPsqlWith is runPsql with opts (conninfo keywords, sslmode among them)
// in place of sslmode=disable.
func runPsqlWith(t *testing.T, psql, addr, user, password, opts string) (stdout, stderr string, err error) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(testCtx(t), psql, "-X", "-w", "-At", "-c", "select 1",
		fmt.Sprintf("host=%s port=%s user=%s dbname=stet_test %s", host, port, user, opts))
	cmd.Env = []string{} // never nil, which would pass the whole environment
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PG") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	if password != "" {
		cmd.Env = append(cmd.Env, "PGPASSWORD="+password)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// psql (libpq) refuses SCRAM-SHA-256-PLUS on a connection without TLS, so
// through a TLS upstream it logs in only once the proxy takes that out of
// the list. Every auth method logs in; a wrong password is still the
// server's to refuse.
func TestPsqlLogsInThroughTLSUpstream(t *testing.T) {
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Skipf("psql is not in PATH, so this test cannot run: %v", err)
	}
	up := tlsUpstream(t)
	for _, g := range gates {
		t.Run(g.name, func(t *testing.T) {
			addr, _ := startProxyTo(t, up, g.gate(), g.policy)
			for _, user := range slices.Sorted(maps.Keys(testUsers)) {
				t.Run(user, func(t *testing.T) {
					out, errOut, err := runPsql(t, psql, addr, user, testUsers[user])
					if err != nil || out != "1\n" {
						t.Errorf("psql as %s: %v, stdout %q, stderr %q", user, err, out, errOut)
					}
				})
			}
			t.Run("WrongPassword", func(t *testing.T) {
				_, errOut, err := runPsql(t, psql, addr, "stet_scram", "wrong")
				const want = `password authentication failed for user "stet_scram"`
				if err == nil || !strings.Contains(errOut, want) {
					t.Errorf("psql with a wrong password: %v, stderr %q, want it to fail with %s", err, errOut, want)
				}
			})
		})
	}
}

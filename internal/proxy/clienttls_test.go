package proxy_test

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
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

	"github.com/stetdb/stet/internal/pgtest"
	"github.com/stetdb/stet/internal/proxy"
)

const (
	sslRequestCode    = 80877103
	gssEncRequestCode = 80877104
	needsTLS          = "stet: this server requires TLS; connect with sslmode=require or stronger"
)

// localTLS is a certificate pgtest.LocalCert made, as the proxy and its
// clients use it.
type localTLS struct {
	server   *tls.Config    // the proxy's ClientTLS
	rootPath string         // the certificate's file: a client's sslrootcert
	roots    *x509.CertPool // a pool that trusts it
}

func newLocalTLS(t *testing.T) localTLS {
	t.Helper()
	certPath, keyPath, roots := pgtest.LocalCert(t)
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return localTLS{
		server:   &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		rootPath: certPath,
		roots:    roots,
	}
}

// client is a Go client's TLS setting for the proxy on 127.0.0.1: it checks
// the certificate and its name.
func (l localTLS) client() *tls.Config {
	return &tls.Config{RootCAs: l.roots, ServerName: "127.0.0.1"}
}

// params make a pgx client check the same: sslmode=verify-full.
func (l localTLS) params() url.Values {
	return url.Values{"sslmode": {"verify-full"}, "sslrootcert": {l.rootPath}}
}

// startTLSProxy serves, on loopback until the test ends, a proxy to up whose
// ClientTLS is l's.
func startTLSProxy(t *testing.T, l localTLS, up proxy.Upstream, gate proxy.Gate, policy proxy.Policy) (string, *proxy.Server) {
	t.Helper()
	return startTLSProxyOn(t, listenLoopback(t), l, up, gate, policy)
}

// startTLSProxyOn is startTLSProxy on ln.
func startTLSProxyOn(t *testing.T, ln net.Listener, l localTLS, up proxy.Upstream, gate proxy.Gate, policy proxy.Policy) (string, *proxy.Server) {
	t.Helper()
	srv := proxy.New(up, slog.New(slog.DiscardHandler), gate, policy)
	srv.ClientTLS = l.server
	return serveProxy(t, srv, ln), srv
}

// elsewhere is a listener whose connections say they come from 10.0.0.5, an
// address that is not loopback.
type elsewhere struct{ net.Listener }

func (l elsewhere) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return fromElsewhere{c}, nil
}

type fromElsewhere struct{ net.Conn }

func (fromElsewhere) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 40000}
}

// request is an SSLRequest or a GSSENCRequest, as code says.
func request(code uint32) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:4], 8)
	binary.BigEndian.PutUint32(b[4:8], code)
	return b
}

// wantOne runs select 1 on conn.
func wantOne(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	var one int
	if err := conn.QueryRow(testCtx(t), "select 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("select 1 = %d, %v", one, err)
	}
}

// A proxy with ClientTLS answers a client's TLS request: a client that
// checks the certificate and its name connects, over TLS. A client on
// loopback still connects without TLS.
func TestClientTLSHandshake(t *testing.T) {
	lt := newLocalTLS(t)
	addr, _ := startTLSProxy(t, lt, proxy.Upstream{Addr: upstream(t)}, nil, proxy.Policy{})

	t.Run("VerifyFull", func(t *testing.T) {
		conn := connect(t, addr, "stet_scram", lt.params())
		wantOne(t, conn)
		if c, ok := conn.PgConn().Conn().(*tls.Conn); !ok || !c.ConnectionState().HandshakeComplete {
			t.Errorf("the client's connection is a %T, not TLS", conn.PgConn().Conn())
		}
	})
	t.Run("PlaintextFromLoopback", func(t *testing.T) {
		conn := connect(t, addr, "stet_scram", nil)
		wantOne(t, conn)
		if _, ok := conn.PgConn().Conn().(*tls.Conn); ok {
			t.Error("sslmode=disable connected over TLS")
		}
	})
}

// Without ClientTLS the proxy answers a GSSENCRequest and an SSLRequest with
// N, as before, and the session goes on in plaintext: the next bytes the
// client gets are the server's.
func TestClientTLSDeclinedWithoutACertificate(t *testing.T) {
	server := upstream(t)
	addr, srv := startProxy(t, server, nil)

	c := dialPlain(t, addr)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	for _, code := range []uint32{gssEncRequestCode, sslRequestCode} {
		if _, err := c.Write(request(code)); err != nil {
			t.Fatal(err)
		}
		var answer [1]byte
		if _, err := io.ReadFull(c, answer[:]); err != nil {
			t.Fatal(err)
		}
		if answer[0] != 'N' {
			t.Fatalf("request %d answered %q, want N", code, answer[0])
		}
	}
	direct := firstAnswer(t, dialPlain(t, server), "stet_scram")
	if got := firstAnswer(t, c, "stet_scram"); !bytes.Equal(got, direct) {
		t.Errorf("after the N the client got % x, the server sends % x", got, direct)
	}

	sslRequestIsDeclined(t, addr, srv)
}

// With ClientTLS, a session that starts without TLS from an address other
// than loopback is refused with FATAL 28000 before anything is sent
// upstream. TLS from there connects, and a cancel request is taken in
// plaintext.
func TestPlaintextFromElsewhereIsRefused(t *testing.T) {
	t.Run("Address", func(t *testing.T) {
		for _, tc := range []struct {
			ip   string
			want bool
		}{
			{"127.0.0.1", true},
			{"127.0.0.53", true},
			{"::1", true},
			{"::ffff:127.0.0.1", true},
			{"10.0.0.5", false},
			{"192.168.1.20", false},
			{"::ffff:10.0.0.5", false},
			{"fe80::1", false},
			{"2001:db8::1", false},
		} {
			addr := &net.TCPAddr{IP: net.ParseIP(tc.ip), Port: 40000}
			if got := proxy.PlaintextAllowed(addr); got != tc.want {
				t.Errorf("PlaintextAllowed(%s) = %v, want %v", addr, got, tc.want)
			}
		}
	})

	t.Run("Refused", func(t *testing.T) {
		dialed := make(chan struct{}, 1)
		fake := fakeUpstream(t, func(net.Conn) {
			select {
			case dialed <- struct{}{}:
			default:
			}
		})
		addr, _ := startTLSProxyOn(t, elsewhere{listenLoopback(t)}, newLocalTLS(t), proxy.Upstream{Addr: fake}, nil, proxy.Policy{})
		_, err := pgx.Connect(testCtx(t), connURL(addr, "stet_scram", "scram-pw", nil)) // sslmode=disable
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("err = %v, want FATAL 28000 %q", err, needsTLS)
		}
		if pgErr.Severity != "FATAL" || pgErr.Code != "28000" || pgErr.Message != needsTLS {
			t.Errorf("got %s %s %q, want FATAL 28000 %q", pgErr.Severity, pgErr.Code, pgErr.Message, needsTLS)
		}
		select {
		case <-dialed:
			t.Error("the refused session was sent on upstream")
		case <-time.After(200 * time.Millisecond):
		}
	})

	// A GSSENCRequest is declined with N and the client goes on in plaintext:
	// the StartupMessage after it is refused as one sent first is.
	t.Run("AfterGSSENCRequest", func(t *testing.T) {
		dialed := make(chan struct{}, 1)
		fake := fakeUpstream(t, func(net.Conn) {
			select {
			case dialed <- struct{}{}:
			default:
			}
		})
		addr, _ := startTLSProxyOn(t, elsewhere{listenLoopback(t)}, newLocalTLS(t), proxy.Upstream{Addr: fake}, nil, proxy.Policy{})
		c := dialPlain(t, addr)
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write(request(gssEncRequestCode)); err != nil {
			t.Fatal(err)
		}
		var answer [1]byte
		if _, err := io.ReadFull(c, answer[:]); err != nil || answer[0] != 'N' {
			t.Fatalf("the GSSENCRequest was answered %q, %v; want N", answer[0], err)
		}
		fe := pgproto3.NewFrontend(c, c)
		sendAll(t, fe, &pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber,
			Parameters: map[string]string{"user": "stet_scram", "database": "stet_test"}})
		if got, want := untilEnd(t, fe), []string{`ErrorResponse FATAL 28000 "` + needsTLS + `"`}; !slices.Equal(got, want) {
			t.Errorf("after the N the client got %q, want %q and the end", got, want)
		}
		select {
		case <-dialed:
			t.Error("the refused session was sent on upstream")
		case <-time.After(200 * time.Millisecond):
		}
	})

	t.Run("TLSFromElsewhere", func(t *testing.T) {
		lt := newLocalTLS(t)
		addr, _ := startTLSProxyOn(t, elsewhere{listenLoopback(t)}, lt, proxy.Upstream{Addr: upstream(t)}, nil, proxy.Policy{})
		wantOne(t, connect(t, addr, "stet_scram", lt.params()))
	})

	t.Run("CancelInPlaintext", func(t *testing.T) {
		cancel := make([]byte, 16)
		binary.BigEndian.PutUint32(cancel[0:4], 16)
		binary.BigEndian.PutUint32(cancel[4:8], 80877102)
		binary.BigEndian.PutUint32(cancel[8:12], 4242)
		binary.BigEndian.PutUint32(cancel[12:16], 0x5ec7e7)

		got := make(chan []byte, 1)
		fake := fakeUpstream(t, func(c net.Conn) {
			c.SetReadDeadline(time.Now().Add(5 * time.Second))
			b := make([]byte, len(cancel))
			n, _ := io.ReadFull(c, b)
			got <- b[:n]
		})
		addr, _ := startTLSProxyOn(t, elsewhere{listenLoopback(t)}, newLocalTLS(t), proxy.Upstream{Addr: fake}, nil, proxy.Policy{})
		c := dialPlain(t, addr)
		c.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write(cancel); err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, c) // the proxy closes it once the cancel is done
		select {
		case b := <-got:
			if !bytes.Equal(b, cancel) {
				t.Errorf("upstream got % x, want % x", b, cancel)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the cancel request never reached upstream")
		}
	})
}

// The scenarios through a proxy with ClientTLS and a plaintext upstream, with
// every connection to the proxy at sslmode=verify-full. pgx sends its cancel
// requests over TLS too.
func TestScenariosOverClientTLS(t *testing.T) {
	lt := newLocalTLS(t)
	for _, sc := range scenarios {
		if sc.name == "SSLRequestIsDeclined" { // here it is accepted
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			addr, srv := startTLSProxy(t, lt, proxy.Upstream{Addr: upstream(t)}, nil, proxy.Policy{})
			useTLSTo(t, addr, lt.params())
			sc.run(t, addr, srv)
		})
	}
}

// With TLS on both legs and authentication relayed, the client gets the
// server's SASL list as the server sent it, SCRAM-SHA-256-PLUS included. A
// client that binds its SCRAM exchange to the channel binds it to the
// proxy's certificate, which is not the server's, so a SCRAM account logs in
// only with channel_binding=disable; md5 and password accounts log in
// (spec-v0.2-server 3).
func TestRelayWithTLSOnBothSides(t *testing.T) {
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Skipf("psql is not in PATH, so this test cannot run: %v", err)
	}
	up := tlsUpstream(t)
	lt := newLocalTLS(t)
	direct := firstAnswer(t, dialTLS(t, up.Addr, up.TLS), "stet_scram")
	if !slices.Contains(mechanisms(t, direct), "SCRAM-SHA-256-PLUS") {
		t.Fatalf("the test server offers %q over TLS, without SCRAM-SHA-256-PLUS: nothing to check", mechanisms(t, direct))
	}
	verify := fmt.Sprintf("sslmode=verify-full sslrootcert='%s'", lt.rootPath)

	for _, g := range gates {
		t.Run(g.name, func(t *testing.T) {
			addr, _ := startTLSProxy(t, lt, up, g.gate(), g.policy)

			t.Run("SASLList", func(t *testing.T) {
				if got := firstAnswer(t, dialTLS(t, addr, lt.client()), "stet_scram"); !bytes.Equal(got, direct) {
					t.Errorf("the client got % x, the server sends % x", got, direct)
				}
			})

			t.Run("Psql", func(t *testing.T) {
				for _, user := range []string{"stet_md5", "stet_clear"} {
					out, errOut, err := runPsqlWith(t, psql, addr, user, testUsers[user], verify)
					if err != nil || out != "1\n" {
						t.Errorf("psql as %s: %v, stdout %q, stderr %q", user, err, out, errOut)
					}
				}
				out, errOut, err := runPsqlWith(t, psql, addr, "stet_scram", testUsers["stet_scram"], verify)
				if err == nil || !strings.Contains(errOut, "channel binding") {
					t.Errorf("psql as stet_scram, channel binding on: %v, stdout %q, stderr %q; want it refused over channel binding", err, out, errOut)
				}
				out, errOut, err = runPsqlWith(t, psql, addr, "stet_scram", testUsers["stet_scram"], verify+" channel_binding=disable")
				if err != nil || out != "1\n" {
					t.Errorf("psql as stet_scram with channel_binding=disable: %v, stdout %q, stderr %q", err, out, errOut)
				}
			})

			t.Run("Pgx", func(t *testing.T) {
				conn, err := pgx.Connect(testCtx(t), connURL(addr, "stet_scram", testUsers["stet_scram"], lt.params()))
				if err == nil {
					conn.Close(testCtx(t))
				}
				if err == nil || !strings.Contains(err.Error(), "channel binding") {
					t.Errorf("pgx as stet_scram, channel binding on: %v; want it refused over channel binding", err)
				}
				params := lt.params()
				params.Set("channel_binding", "disable")
				wantOne(t, connect(t, addr, "stet_scram", params))
			})
		})
	}
}

// clientHello is a TLS client's first flight: one whole ClientHello record.
func clientHello(t *testing.T) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go tls.Client(a, &tls.Config{InsecureSkipVerify: true}).Handshake() // ends when the pipe closes
	b.SetReadDeadline(time.Now().Add(5 * time.Second))
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(b, hdr); err != nil {
		t.Fatal(err)
	}
	rec := make([]byte, len(hdr)+int(binary.BigEndian.Uint16(hdr[3:5])))
	copy(rec, hdr)
	if _, err := io.ReadFull(b, rec[len(hdr):]); err != nil {
		t.Fatal(err)
	}
	return rec
}

// A client that asks for TLS and then stops, before its ClientHello or in
// the middle of the handshake, is dropped once the startup limit runs out,
// and holds no one up: another session connects meanwhile.
func TestTLSHandshakeIsBounded(t *testing.T) {
	const limit = time.Second
	// Registered before the proxy starts, so it runs after Close.
	t.Cleanup(proxy.SetStartupTimeout(limit))
	lt := newLocalTLS(t)
	addr, _ := startTLSProxy(t, lt, proxy.Upstream{Addr: upstream(t)}, nil, proxy.Policy{})

	for _, st := range []struct {
		name string
		then []byte // what the client sends after the S, before it stops
	}{
		{"SilentAfterTheAnswer", nil},
		{"StopsInTheHandshake", clientHello(t)},
	} {
		t.Run(st.name, func(t *testing.T) {
			began := time.Now()
			c := dialPlain(t, addr)
			c.SetDeadline(time.Now().Add(limit + 5*time.Second))
			if _, err := c.Write(request(sslRequestCode)); err != nil {
				t.Fatal(err)
			}
			var answer [1]byte
			if _, err := io.ReadFull(c, answer[:]); err != nil || answer[0] != 'S' {
				t.Fatalf("the TLS request was answered %q, %v; want S", answer[0], err)
			}
			if _, err := c.Write(st.then); err != nil {
				t.Fatal(err)
			}
			ended := make(chan time.Duration, 1)
			go func() {
				io.Copy(io.Discard, c) // the server's flight, if any, then the end
				ended <- time.Since(began)
			}()

			wantOne(t, connect(t, addr, "stet_scram", lt.params()))
			if d := time.Since(began); d >= limit {
				t.Fatalf("the other session took until %v, past the limit %v: it shows nothing", d, limit)
			}
			select {
			case d := <-ended:
				if d < limit || d > limit+2*time.Second {
					t.Errorf("the stalled connection ended after %v, want soon after the limit %v", d, limit)
				}
			case <-time.After(limit + 5*time.Second):
				t.Fatal("the stalled connection was never ended")
			}
		})
	}
}

// Plaintext a client sends behind its SSLRequest is not taken for what comes
// inside TLS: the proxy reads the request alone, so those bytes are the start
// of the handshake, which fails on them, and no session starts.
func TestPlaintextBehindSSLRequestIsNotReadInside(t *testing.T) {
	dialed := make(chan struct{}, 1)
	fake := fakeUpstream(t, func(net.Conn) {
		select {
		case dialed <- struct{}{}:
		default:
		}
	})
	lt := newLocalTLS(t)
	addr, _ := startTLSProxy(t, lt, proxy.Upstream{Addr: fake}, nil, proxy.Policy{})
	startup, err := (&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "stet_trust", "database": "stet_test"},
	}).Encode(nil)
	if err != nil {
		t.Fatal(err)
	}

	c := dialPlain(t, addr)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(append(request(sslRequestCode), startup...)); err != nil {
		t.Fatal(err)
	}
	var answer [1]byte
	if _, err := io.ReadFull(c, answer[:]); err != nil || answer[0] != 'S' {
		t.Fatalf("the TLS request was answered %q, %v; want S", answer[0], err)
	}
	if err := tls.Client(c, lt.client()).Handshake(); err == nil {
		t.Error("the handshake completed past the plaintext sent behind the SSLRequest")
	}
	select {
	case <-dialed:
		t.Error("a session started from the plaintext sent behind the SSLRequest")
	case <-time.After(200 * time.Millisecond):
	}
}

// An SSLRequest inside TLS is not answered: the connection ends.
func TestSecondSSLRequestInsideTLS(t *testing.T) {
	lt := newLocalTLS(t)
	addr, _ := startTLSProxy(t, lt, proxy.Upstream{Addr: upstream(t)}, nil, proxy.Policy{})
	c := dialTLS(t, addr, lt.client())
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(request(sslRequestCode)); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the connection was not ended")
	}
	if len(got) != 0 {
		t.Errorf("the proxy answered % x", got)
	}
}

// stallingListener hands the test each connection it accepts, as a
// stallConn.
type stallingListener struct {
	net.Listener
	accepted chan *stallConn
}

func (l stallingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	sc := &stallConn{Conn: c, closed: make(chan struct{})}
	select {
	case l.accepted <- sc:
	default:
	}
	return sc, nil
}

// stallConn is the proxy's side of a client's connection. Once stall is
// called, a write to it waits as one to a client that stopped reading with
// every buffer full does: until the write deadline passes or the connection
// is closed.
type stallConn struct {
	net.Conn
	mu       sync.Mutex
	stalled  bool
	deadline time.Time // of writes
	closed   chan struct{}
	once     sync.Once
}

func (c *stallConn) stall() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stalled = true
}

func (c *stallConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

func (c *stallConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *stallConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	stalled, deadline := c.stalled, c.deadline
	c.mu.Unlock()
	if !stalled {
		return c.Conn.Write(b)
	}
	var expired <-chan time.Time
	if !deadline.IsZero() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		expired = timer.C
	}
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	case <-expired:
		return 0, os.ErrDeadlineExceeded
	}
}

func (c *stallConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// On an end the proxy does not make, the client leaving or a panic in a
// tap, the connection to the server is closed before the client's. Closing
// a client's TLS connection says close_notify first, which waits up to TLS's
// own 5s for a client that stopped reading: the server's side must not stay
// open that long, nor, after a panic, the other side go on through the gate
// meanwhile.
func TestAbruptEndClosesTheServerFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate func() proxy.Gate
		end  func(t *testing.T, c net.Conn, fe *pgproto3.Frontend) // once the client stopped reading
	}{
		{"ClientLeaves", func() proxy.Gate { return nil }, func(t *testing.T, c net.Conn, _ *pgproto3.Frontend) {
			c.Close()
		}},
		{"PanicInATap", func() proxy.Gate { return newRecordingGate(isWrite) }, func(t *testing.T, _ net.Conn, fe *pgproto3.Frontend) {
			sendAll(t, fe, &pgproto3.Query{String: "select 1"}) // its completion panics
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Registered before the proxy starts, so it runs after Close.
			t.Cleanup(proxy.SetServerHook(func(m byte) {
				if m == 'C' {
					panic("test hook: panic in the server's tap")
				}
			}))
			upClosed := make(chan struct{})
			fake := fakeUpstream(t, func(c net.Conn) {
				defer close(upClosed)
				var n [4]byte // the startup packet, then the end of a login
				if _, err := io.ReadFull(c, n[:]); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint32(n[:]))-4); err != nil {
					return
				}
				ready, _ := (&pgproto3.AuthenticationOk{}).Encode(nil)
				ready, _ = (&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(ready)
				c.Write(ready)
				var hdr [5]byte // a query, answered
				if _, err := io.ReadFull(c, hdr[:]); err == nil {
					io.CopyN(io.Discard, c, int64(binary.BigEndian.Uint32(hdr[1:]))-4)
					done, _ := (&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}).Encode(nil)
					c.Write(append(done, ready[len(ready)-6:]...))
				}
				io.Copy(io.Discard, c) // until the proxy closes it
			})
			lt := newLocalTLS(t)
			ln := stallingListener{Listener: listenLoopback(t), accepted: make(chan *stallConn, 1)}
			addr, _ := startTLSProxyOn(t, ln, lt, proxy.Upstream{Addr: fake}, tc.gate(), proxy.Policy{})

			c := dialTLS(t, addr, lt.client())
			c.SetDeadline(time.Now().Add(10 * time.Second))
			fe := pgproto3.NewFrontend(c, c)
			sendAll(t, fe, &pgproto3.StartupMessage{
				ProtocolVersion: pgproto3.ProtocolVersionNumber,
				Parameters:      map[string]string{"user": "stet_scram", "database": "stet_test"},
			})
			untilReady(t, fe)
			(<-ln.accepted).stall()

			began := time.Now()
			tc.end(t, c, fe)
			select {
			case <-upClosed:
				if d := time.Since(began); d > time.Second {
					t.Errorf("the server's connection was closed %v after the session ended", d)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the server's connection was still open 3s after the session ended")
			}
		})
	}
}

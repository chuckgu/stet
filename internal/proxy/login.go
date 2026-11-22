package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/stetdb/stet/internal/safetext"
	"github.com/stetdb/stet/internal/scram"
)

// Opened is an upstream session the proxy did not log in to itself.
type Opened struct {
	Conn      net.Conn          // logged in and idle
	PID       uint32            // the server process
	SecretKey []byte            // its cancel key
	Params    map[string]string // the settings the server reported at login
	TxStatus  byte              // from the server's ReadyForQuery
	Unread    []byte            // bytes already read from Conn after that, if any
}

// Login authenticates clients for the proxy and opens their sessions.
type Login interface {
	// Verifier returns what to check account's token against. known false:
	// there is no such account; the verifier is a mock.
	Verifier(account string) (v scram.Verifier, known bool)
	// Open logs in upstream as account and hands the session over. params
	// are the client's startup parameters without user, database and
	// replication, and without protocol options (_pq_.*), which the proxy
	// declines. Its error is for the log, never for the client.
	Open(ctx context.Context, account, database string, params map[string]string) (*Opened, error)
}

// exchangeTimeout bounds a client's side of its login on a proxy with a
// Login: from the end of its startup packet to its last message of the SCRAM
// exchange, on one clock. A variable only so tests can shorten it
// (SetExchangeTimeout).
var exchangeTimeout = 10 * time.Second

const (
	// openTimeout bounds Login.Open (spec-v0.2-server 4).
	openTimeout = 5 * time.Second

	// maxAuthBody is the largest body of a client's message the proxy takes
	// while it authenticates the client (correction V4).
	maxAuthBody = 16 << 10
)

// What a client is told when the proxy refuses its login
// (spec-v0.2-server 4).
const (
	noReplication  = "stet: replication connections are not supported"
	exchangeFailed = "stet: authentication exchange failed"
)

func passwordFailed(account string) string {
	return `password authentication failed for user "` + account + `"`
}

func noSession(account string) string {
	return `stet: could not open a database session for "` + account + `"`
}

// The reasons a refused login is logged with (spec-v0.2-server 9).
const (
	reasonUnknownAccount = "unknown account"
	reasonBadToken       = "bad token"
	reasonUpstream       = "upstream login failed"
	reasonExchange       = "exchange failed"
	reasonTLSRequired    = "tls required"
)

// errWrongProof is authenticate's error for a client that did not prove it
// holds the account's token, or named an account that is not there.
var errWrongProof = errors.New("the client's proof does not match")

// exchangeError is a client's message the exchange cannot go on from. Its
// text is the proxy's, never the message's.
type exchangeError struct{ why string }

func (e *exchangeError) Error() string { return e.why }

// logIn authenticates the client of startup packet pkt, on client, with
// s.Login, and opens its session upstream (spec-v0.2-server 4, 5). server is
// that session's connection, whose first reads are the end of a login as the
// server sends it (see handover): the client gets them, and a watched
// session learns its server process and settings from them, as from the
// server itself (correction V3). secure reports whether the session is TLS
// upstream. release gives up server's tracking once it is closed. ok false:
// the session ends here, the client told why where there is something to
// tell.
func (s *Server) logIn(client net.Conn, pkt []byte, clientTLS bool) (server net.Conn, secure bool, release func(), ok bool) {
	info, readable := startupInfo(pkt)
	if !readable {
		s.log.Debug("startup failed", "remote", client.RemoteAddr(), "err", "a startup packet the server would refuse")
		return nil, false, nil, false
	}
	account := info.User
	refused := func(reason string, args ...any) {
		s.log.Warn("agent login refused", append([]any{"account", safetext.Printable(account),
			"remote", client.RemoteAddr(), "reason", reason}, args...)...)
	}

	client.SetDeadline(time.Now().Add(exchangeTimeout))
	params, options, replication := startupParams(pkt)
	if replication {
		s.log.Debug("replication connection refused", "account", safetext.Printable(account), "remote", client.RemoteAddr())
		writeFatal(client, "0A000", noReplication)
		return nil, false, nil, false
	}
	// The proxy speaks 3.0 with no protocol options, and tells a client that
	// asked for more as the server does: before anything else.
	if binary.BigEndian.Uint32(pkt[4:8])&0xffff != 0 || len(options) > 0 {
		if err := writeMessage(client, &pgproto3.NegotiateProtocolVersion{NewestMinorProtocol: 3 << 16, UnrecognizedOptions: options}); err != nil {
			return nil, false, nil, false
		}
	}

	var broken *exchangeError
	switch known, err := s.authenticate(client, account); {
	case err == nil:
	case errors.Is(err, errWrongProof):
		writeFatal(client, "28P01", passwordFailed(account))
		if known {
			refused(reasonBadToken)
		} else {
			refused(reasonUnknownAccount)
		}
		return nil, false, nil, false
	case errors.As(err, &broken):
		writeFatal(client, "28000", exchangeFailed)
		refused(reasonExchange, "err", broken.why)
		return nil, false, nil, false
	case errors.Is(err, os.ErrDeadlineExceeded):
		refused(reasonExchange, "err", "the client did not finish in time")
		return nil, false, nil, false
	default: // the client left, or its connection failed
		s.log.Debug("agent left during login", "account", safetext.Printable(account), "remote", client.RemoteAddr())
		return nil, false, nil, false
	}
	client.SetDeadline(time.Time{})

	ctx, cancel := context.WithTimeout(s.closing, openTimeout)
	opened, err := s.Login.Open(ctx, account, info.Database, params)
	cancel()
	if err == nil && opened == nil {
		err = errors.New("Open returned no session")
	}
	var prefix []byte
	if err == nil {
		if prefix, err = handover(opened); err != nil {
			opened.Conn.Close()
		}
	}
	if err != nil {
		if s.isClosed() { // Close ended the login, and closed the client too
			return nil, false, nil, false
		}
		refused(reasonUpstream, "err", safetext.Printable(err.Error()))
		client.SetWriteDeadline(time.Now().Add(fatalTimeout))
		writeFatal(client, "08001", noSession(account))
		return nil, false, nil, false
	}
	if release, ok = s.adopt(opened.Conn); !ok { // the proxy is closing
		opened.Conn.Close()
		return nil, false, nil, false
	}
	s.log.Info("agent logged in", "account", safetext.Printable(account), "remote", client.RemoteAddr(), "tls", clientTLS)
	_, secure = opened.Conn.(*tls.Conn)
	return &prefixed{Conn: opened.Conn, prefix: prefix}, secure, release, true
}

// authenticate runs a SCRAM-SHA-256 exchange for account with the client on
// c, as the server does (RFC 5802, 7677). It offers that one mechanism,
// never SCRAM-SHA-256-PLUS. An account the Login does not know (known false)
// runs the same exchange against a mock verifier, which fails at the end.
// err is nil once the client proved it holds the account's token and was
// sent AuthenticationSASLFinal; errWrongProof when it did not, or the
// account is unknown; an *exchangeError for a message the exchange cannot
// go on from; else the connection's error. Nothing the client sent after
// its last message of the exchange is read.
func (s *Server) authenticate(c net.Conn, account string) (known bool, err error) {
	v, known := s.Login.Verifier(account)
	ex := scram.NewServer(v, !known, nil)
	if err := writeMessage(c, &pgproto3.AuthenticationSASL{AuthMechanisms: []string{scram.Mechanism}}); err != nil {
		return known, err
	}
	body, err := readAuthMessage(c)
	if err != nil {
		return known, err
	}
	mechanism, clientFirst, err := saslInitialResponse(body)
	if err != nil {
		return known, err
	}
	if mechanism != scram.Mechanism {
		return known, &exchangeError{"the client chose a mechanism other than " + scram.Mechanism}
	}
	serverFirst, err := ex.First(clientFirst)
	if err != nil {
		return known, &exchangeError{err.Error()}
	}
	if err := writeMessage(c, &pgproto3.AuthenticationSASLContinue{Data: serverFirst}); err != nil {
		return known, err
	}
	if body, err = readAuthMessage(c); err != nil {
		return known, err
	}
	serverFinal, proved, err := ex.Final(body)
	switch {
	case err != nil:
		return known, &exchangeError{err.Error()}
	case !proved:
		return known, errWrongProof
	}
	return known, writeMessage(c, &pgproto3.AuthenticationSASLFinal{Data: serverFinal})
}

// readAuthMessage reads one of the client's messages during authentication,
// a SASLInitialResponse or a SASLResponse (type p), and returns its body. It
// reads c itself, exactly the message's bytes, so nothing the client sent
// after it is taken from the relay (correction V4). Another type, or a body
// longer than maxAuthBody, is an *exchangeError before the body is read.
func readAuthMessage(c io.Reader) ([]byte, error) {
	var hdr [5]byte // type byte + int32 length (which counts itself)
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return nil, err
	}
	if hdr[0] != 'p' {
		return nil, &exchangeError{fmt.Sprintf("the client sent a message of type %q in place of a SASL response", hdr[0])}
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n < 4 || n-4 > maxAuthBody {
		return nil, &exchangeError{fmt.Sprintf("the client's SASL response has length %d, not 4 to %d", n, 4+maxAuthBody)}
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c, body); err != nil {
		return nil, err
	}
	return body, nil
}

// saslInitialResponse reads the body of a SASLInitialResponse: the name of
// the mechanism, ended by a zero byte, then the length of the client's first
// message and that message, the rest of the body. A length of -1 (no first
// message, to be asked for with an empty AuthenticationSASLContinue) is not
// taken: the clients this serves always send one.
func saslInitialResponse(body []byte) (mechanism string, data []byte, err error) {
	name, rest, found := bytes.Cut(body, []byte{0})
	if !found || len(rest) < 4 {
		return "", nil, &exchangeError{"a malformed SASLInitialResponse"}
	}
	n := int32(binary.BigEndian.Uint32(rest))
	if data = rest[4:]; n < 0 || int(n) != len(data) {
		return "", nil, &exchangeError{"a malformed SASLInitialResponse"}
	}
	return string(name), data, nil
}

// handover is what the server sends at the end of its own login, for the
// session o: AuthenticationOk, a ParameterStatus for each of its settings in
// name order, BackendKeyData and ReadyForQuery; then the bytes Open already
// read past them.
func handover(o *Opened) ([]byte, error) {
	msgs := []pgproto3.BackendMessage{&pgproto3.AuthenticationOk{}}
	for _, name := range slices.Sorted(maps.Keys(o.Params)) {
		msgs = append(msgs, &pgproto3.ParameterStatus{Name: name, Value: o.Params[name]})
	}
	msgs = append(msgs, &pgproto3.BackendKeyData{ProcessID: o.PID, SecretKey: o.SecretKey}, &pgproto3.ReadyForQuery{TxStatus: o.TxStatus})
	var b []byte
	for _, m := range msgs {
		var err error
		if b, err = m.Encode(b); err != nil {
			return nil, err
		}
	}
	return append(b, o.Unread...), nil
}

// prefixed is a connection whose reads return prefix before what the
// connection itself has.
type prefixed struct {
	net.Conn
	prefix []byte
}

func (p *prefixed) Read(b []byte) (int, error) {
	if len(p.prefix) == 0 {
		return p.Conn.Read(b)
	}
	n := copy(b, p.prefix)
	p.prefix = p.prefix[n:]
	return n, nil
}

// adopt tracks conn, a session Login opened, so that Close ends it: by its
// TCP connection, as dialUpstream tracks the connections it opens. release
// gives the tracking up once conn is closed. ok false: the proxy is closing.
func (s *Server) adopt(conn net.Conn) (release func(), ok bool) {
	raw := conn
	if tc, isTLS := conn.(*tls.Conn); isTLS {
		raw = tc.NetConn()
	}
	if !s.track(raw) {
		return nil, false
	}
	return func() {
		s.untrack(raw)
		s.wg.Done()
	}, true
}

// writeMessage writes m to w whole.
func writeMessage(w io.Writer, m pgproto3.BackendMessage) error {
	b, err := m.Encode(nil)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

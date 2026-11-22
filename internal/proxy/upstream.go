package proxy

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// dialTimeout bounds the TCP connect to upstream, and then, on its own
// clock, the TLS request and handshake. A variable only so tests can shorten
// it (SetDialTimeout).
var dialTimeout = 5 * time.Second

// Upstream says how the proxy reaches the server.
type Upstream struct {
	Addr string // host:port
	// TLS, when set, is asked for with an SSLRequest before anything else is
	// sent to the server. nil: plaintext.
	TLS *tls.Config
	// PlainFallback lets a session go on in plaintext when the server
	// answers the SSLRequest with N (sslmode=prefer).
	PlainFallback bool

	// opened, when set, is handed the TCP connection as soon as it is open,
	// before anything is sent on it, so the proxy can close it while the
	// server is still being waited on. dial gives up when it returns false.
	opened func(net.Conn) bool
}

// errStopped is dial's error when opened refused the connection.
var errStopped = errors.New("the proxy is closing")

// dial connects to the server, with TLS when u asks for it. secure reports
// whether the connection it returns is TLS. An error's text is what the
// client is told after "stet: ".
func (u Upstream) dial() (conn net.Conn, secure bool, err error) {
	c, err := net.DialTimeout("tcp", u.Addr, dialTimeout)
	if err != nil {
		return nil, false, fmt.Errorf("cannot connect to upstream %s: %v", u.Addr, err)
	}
	if u.opened != nil && !u.opened(c) {
		c.Close()
		return nil, false, errStopped
	}
	if u.TLS == nil {
		return c, false, nil
	}
	conn, secure, err = u.startTLS(c)
	if err != nil {
		c.Close()
		return nil, false, err
	}
	return conn, secure, nil
}

// startTLS asks the server on c for TLS, and returns the connection to go on
// with: TLS (secure), or c itself when the server declined and u allows
// plaintext. The request and the handshake share one dialTimeout.
func (u Upstream) startTLS(c net.Conn) (conn net.Conn, secure bool, err error) {
	c.SetDeadline(time.Now().Add(dialTimeout))
	var req [8]byte
	binary.BigEndian.PutUint32(req[0:4], uint32(len(req)))
	binary.BigEndian.PutUint32(req[4:8], sslRequestCode)
	if _, err := c.Write(req[:]); err != nil {
		return nil, false, u.tlsFailed(err)
	}
	// One byte, read from c itself: whatever follows the answer is not taken
	// for the start of TLS, nor relayed to the client as if it came inside.
	var answer [1]byte
	if _, err := io.ReadFull(c, answer[:]); err != nil {
		return nil, false, u.tlsFailed(err)
	}
	switch answer[0] {
	case 'S':
		tc := tls.Client(c, u.TLS)
		if err := tc.Handshake(); err != nil {
			return nil, false, u.tlsFailed(err)
		}
		c.SetDeadline(time.Time{})
		return tc, true, nil
	case 'N':
		if !u.PlainFallback {
			return nil, false, errors.New("upstream does not accept TLS, and the sslmode of upstream needs it")
		}
		c.SetDeadline(time.Time{})
		return c, false, nil
	}
	return nil, false, fmt.Errorf("upstream answered the TLS request with %q, not S or N", answer[0])
}

func (u Upstream) tlsFailed(err error) error {
	return fmt.Errorf("TLS to upstream %s failed: %v", u.Addr, err)
}

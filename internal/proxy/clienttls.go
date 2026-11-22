package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
)

// needsTLS is the message of the FATAL (28000) a proxy with ClientTLS gives
// a session that starts without TLS from an address other than loopback.
const needsTLS = "stet: this server requires TLS; connect with sslmode=require or stronger"

// errNeedsTLS is readStartup's error once it gave a client that FATAL.
var errNeedsTLS = errors.New("a session from an address other than loopback started without TLS")

// errTLSTwice is readStartup's error for an SSLRequest inside TLS.
var errTLSTwice = errors.New("an SSLRequest inside TLS")

// plaintextAllowed reports whether a session from addr may start without TLS
// when the proxy offers TLS: only from a TCP address on loopback.
func plaintextAllowed(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}

// startClientTLS answers a client's SSLRequest on c with S and sets up TLS
// with cfg, under the deadline c already has. It reads nothing from c
// itself: whatever the client sent behind the request is taken as the start
// of the handshake, never as a message from inside TLS.
func startClientTLS(c net.Conn, cfg *tls.Config) (net.Conn, error) {
	if _, err := c.Write([]byte{'S'}); err != nil {
		return nil, err
	}
	tc := tls.Server(c, cfg)
	if err := tc.Handshake(); err != nil {
		return nil, fmt.Errorf("TLS handshake with the client: %w", err)
	}
	return tc, nil
}

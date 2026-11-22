// Package pgtest holds what integration tests share about the test server
// that scripts/testdb.sh starts. It imports testing: only test files may
// import it.
package pgtest

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sslRequestCode = 80877103

// ServerCert returns the certificate the test server at addr presents and
// the path of a PEM file of it in t.TempDir(). It fails the test, saying to
// start the server again with scripts/testdb.sh, when the server does not
// accept TLS.
func ServerCert(t testing.TB, addr string) (cert *x509.Certificate, pemPath string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("connect to the test server %s: %v", addr, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))

	var req [8]byte
	binary.BigEndian.PutUint32(req[0:4], 8)
	binary.BigEndian.PutUint32(req[4:8], sslRequestCode)
	if _, err := c.Write(req[:]); err != nil {
		t.Fatalf("ask the test server %s for TLS: %v", addr, err)
	}
	var answer [1]byte
	if _, err := io.ReadFull(c, answer[:]); err != nil {
		t.Fatalf("ask the test server %s for TLS: %v", addr, err)
	}
	if answer[0] != 'S' {
		t.Fatalf("the test server %s does not accept TLS (it answered %q to an SSLRequest); start it again with scripts/testdb.sh, which turns ssl on", addr, answer[0])
	}
	tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS to the test server %s: %v", addr, err)
	}
	peer := tc.ConnectionState().PeerCertificates
	if len(peer) == 0 {
		t.Fatalf("the test server %s presented no certificate", addr)
	}
	cert = peer[0]

	pemPath = filepath.Join(t.TempDir(), "server.crt")
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, pemPath
}

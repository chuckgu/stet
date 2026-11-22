// Package pgtest holds what integration tests share about the test server
// that scripts/testdb.sh starts. It imports testing: only test files may
// import it.
package pgtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"io"
	"math/big"
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

// LocalCert makes a self-signed certificate for localhost and 127.0.0.1
// (as a DNS name too: libpq 14 does not match IP entries), writes it and its
// key as PEM files in t.TempDir(), and returns the paths and a pool that
// trusts it.
func LocalCert(t testing.TB) (certPath, keyPath string, roots *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost", "127.0.0.1"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "local.crt")
	keyPath = filepath.Join(dir, "local.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0o600); err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	roots.AddCert(cert)
	return certPath, keyPath, roots
}

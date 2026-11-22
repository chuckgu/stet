package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/pgtest"
)

const sslQuery = "select ssl from pg_stat_ssl where pid = pg_backend_pid()"

// runPsql runs psql against addr in plaintext with the environment stripped of
// its PG* variables, so that nothing of the developer's setup reaches it; the
// password goes in PGPASSWORD. It returns what psql printed to stdout.
func runPsql(t *testing.T, addr, user, password, query string) (string, error) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	return psqlWith(t, fmt.Sprintf("host=%s port=%s user=%s dbname=stet_test sslmode=disable", host, port, user),
		query, "PGPASSWORD="+password)
}

// psqlWith runs psql with conninfo (key/value pairs or a URI) and the
// environment stripped of its PG* variables, with env added. It returns what
// psql printed to stdout.
func psqlWith(t *testing.T, conninfo, query string, env ...string) (string, error) {
	t.Helper()
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Skip("psql is not on PATH; the test of libpq through stet cannot run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, psql, "-X", "-w", "-At", "-c", query, conninfo)
	cmd.Env = []string{} // never nil, which would pass the whole environment
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PG") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// The client leg is plaintext and the leg to the server is TLS, in every
// sslmode that asks for TLS: the server sees an SSL session, for a client that
// sends SCRAM too, and what an agent writes is still judged and recorded.
func TestSessionsReachTheServerOverTLS(t *testing.T) {
	pg := testPG(t)
	_, certPath := pgtest.ServerCert(t, pg)
	for _, sslmode := range []string{"require", "verify-ca", "verify-full"} {
		t.Run(sslmode, func(t *testing.T) {
			ctx := context.Background()
			db, table, admin := agentTable(t, pg, 3)
			upstream := url.URL{
				Scheme: "postgres", Host: pg, Path: "/" + db,
				User:     url.UserPassword("stet_scram", "scram-pw"),
				RawQuery: url.Values{"sslmode": {sslmode}, "sslrootcert": {certPath}}.Encode(),
			}
			r := start(t, parseConfig(t, `mode: shadow
agents: {roles: [stet_md5]}
listen: 127.0.0.1:0
upstream: %[1]s
model: {adapter: none}
tables:
  %[2]s:
    pk: id
    impact: {max_rows: 3}
`, upstream.String(), table))
			waitListening(t, r)

			for _, user := range []struct{ name, password string }{{"stet_scram", "scram-pw"}, {"stet_md5", "md5-pw"}} {
				var ssl bool
				if err := connectTo(t, r.addr, db, user.name, user.password).QueryRow(ctx, sslQuery).Scan(&ssl); err != nil {
					t.Fatalf("pgx as %s: %v; log:\n%s", user.name, err, r.log)
				}
				if !ssl {
					t.Errorf("pgx as %s: pg_stat_ssl.ssl is false, want the session to reach the server over TLS", user.name)
				}
			}
			if got, err := runPsql(t, r.addr, "stet_scram", "scram-pw", sslQuery); err != nil || got != "t" {
				t.Errorf("psql as stet_scram: ssl = %q, err = %v; want t; log:\n%s", got, err, r.log)
			}

			agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")
			if _, err := agent.Exec(ctx, "update "+table+" set name = 'over-tls' where id = 1"); err != nil {
				t.Fatalf("update: %v; log:\n%s", err, r.log)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				var n int
				if err := admin.QueryRow(ctx, "select count(*) from stet.decisions where target_table = $1", table).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("no decision recorded for the write; log:\n%s", r.log)
				}
				time.Sleep(20 * time.Millisecond)
			}
			var name string
			if err := admin.QueryRow(ctx, "select name from "+table+" where id = 1").Scan(&name); err != nil || name != "over-tls" {
				t.Errorf("row 1's name = %q, err = %v; want the write to have gone through", name, err)
			}
			if log := r.log.String(); strings.Contains(log, "level=WARN") {
				t.Errorf("the log has a warning, want none:\n%s", log)
			}
			if err := r.stop(t); err != nil {
				t.Fatalf("run = %v, want nil", err)
			}
		})
	}
}

// selfSigned makes a certificate for host (an IP) and returns it with its key.
func selfSigned(t *testing.T, host string) (tls.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.ParseIP(host)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, key
}

// A client's certificate in the upstream URL is for Stet's own connections to
// no one else: a session relayed for an agent shows the server none, so the
// server cannot take the agent for the account the certificate names.
func TestRelayedSessionShowsNoClientCertificate(t *testing.T) {
	testPG(t) // an integration test: skipped with the others without STET_TEST_PG

	serverCert, _ := selfSigned(t, "127.0.0.1")
	clientCert, clientKey := selfSigned(t, "127.0.0.1")
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	keyDER, err := x509.MarshalPKCS8PrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientCert.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	seen := make(chan int, 1) // how many certificates the first client presented
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(10 * time.Second))
		var req [8]byte
		if _, err := io.ReadFull(c, req[:]); err != nil {
			return
		}
		if _, err := c.Write([]byte{'S'}); err != nil {
			return
		}
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientAuth: tls.RequestClientCert})
		if err := tc.Handshake(); err != nil {
			return
		}
		seen <- len(tc.ConnectionState().PeerCertificates)
	}()

	upstream := url.URL{
		Scheme: "postgres", Host: ln.Addr().String(), Path: "/d", User: url.UserPassword("stet_scram", "scram-pw"),
		RawQuery: url.Values{"sslmode": {"require"}, "sslcert": {certFile}, "sslkey": {keyFile}}.Encode(),
	}
	r := start(t, parseConfig(t, "mode: shadow\nlisten: 127.0.0.1:0\nupstream: %s\n", upstream.String()))
	waitListening(t, r)

	// The session fails, since the fake server says nothing more; the test is
	// about what it saw before.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if conn, err := pgx.Connect(ctx, "postgres://stet_md5:md5-pw@"+r.addr+"/d?sslmode=disable"); err == nil {
		conn.Close(ctx)
	}
	select {
	case n := <-seen:
		if n != 0 {
			t.Errorf("the server saw %d client certificates in a relayed session, want none", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the fake server never completed a handshake; log:\n%s", r.log)
	}
	if err := r.stop(t); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run = %v, want nil", err)
	}
}

package scram_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/scram"
)

func sameVerifier(a, b scram.Verifier) bool {
	return a.Iterations == b.Iterations && bytes.Equal(a.Salt, b.Salt) &&
		bytes.Equal(a.StoredKey, b.StoredKey) && bytes.Equal(a.ServerKey, b.ServerKey)
}

func TestVerifierRoundTrip(t *testing.T) {
	v := scram.NewVerifier("pencil", []byte("0123456789abcdef"), scram.Iterations)
	got, err := scram.ParseVerifier(v.String())
	if err != nil {
		t.Fatalf("ParseVerifier(v.String()): %v", err)
	}
	if !sameVerifier(got, v) {
		t.Fatal("ParseVerifier(v.String()) differs from v")
	}
	if !strings.HasPrefix(v.String(), "SCRAM-SHA-256$4096:") {
		t.Errorf("String() does not start with SCRAM-SHA-256$4096:")
	}

	b64 := base64.StdEncoding.EncodeToString
	salt, key := b64([]byte("0123456789abcdef")), b64(make([]byte, 32))
	broken := []struct{ name, s string }{
		{"empty", ""},
		{"other mechanism", "SCRAM-SHA-1$4096:" + salt + "$" + key + ":" + key},
		{"md5", "md5" + strings.Repeat("0", 32)},
		{"zero iterations", "SCRAM-SHA-256$0:" + salt + "$" + key + ":" + key},
		{"negative iterations", "SCRAM-SHA-256$-1:" + salt + "$" + key + ":" + key},
		{"letters for iterations", "SCRAM-SHA-256$abc:" + salt + "$" + key + ":" + key},
		{"plus-signed iterations", "SCRAM-SHA-256$+4096:" + salt + "$" + key + ":" + key},
		{"no iterations", "SCRAM-SHA-256$:" + salt + "$" + key + ":" + key},
		{"too many iterations", "SCRAM-SHA-256$99999999999999999999:" + salt + "$" + key + ":" + key},
		{"too few $ pieces", "SCRAM-SHA-256$4096:" + salt},
		{"too many $ pieces", "SCRAM-SHA-256$4096:" + salt + "$" + key + ":" + key + "$" + key},
		{"too few : pieces in the first part", "SCRAM-SHA-256$4096$" + key + ":" + key},
		{"too many : pieces in the first part", "SCRAM-SHA-256$4096:" + salt + ":" + salt + "$" + key + ":" + key},
		{"too few : pieces in the second part", "SCRAM-SHA-256$4096:" + salt + "$" + key},
		{"too many : pieces in the second part", "SCRAM-SHA-256$4096:" + salt + "$" + key + ":" + key + ":" + key},
		{"broken base64 salt", "SCRAM-SHA-256$4096:!!!$" + key + ":" + key},
		{"broken base64 StoredKey", "SCRAM-SHA-256$4096:" + salt + "$!!!:" + key},
		{"broken base64 ServerKey", "SCRAM-SHA-256$4096:" + salt + "$" + key + ":!!!"},
		{"unpadded base64", "SCRAM-SHA-256$4096:" + strings.TrimRight(salt, "=") + "$" + key + ":" + key},
		{"base64 with a line break", "SCRAM-SHA-256$4096:" + salt[:4] + "\n" + salt[4:] + "$" + key + ":" + key},
		{"empty salt", "SCRAM-SHA-256$4096:$" + key + ":" + key},
		{"short StoredKey", "SCRAM-SHA-256$4096:" + salt + "$" + b64(make([]byte, 31)) + ":" + key},
		{"long ServerKey", "SCRAM-SHA-256$4096:" + salt + "$" + key + ":" + b64(make([]byte, 33))},
	}
	for _, c := range broken {
		if _, err := scram.ParseVerifier(c.s); err == nil {
			t.Errorf("ParseVerifier(%s): no error", c.name)
		}
	}
}

func TestNewToken(t *testing.T) {
	token, v, err := scram.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "stet_") || len(token) != 48 {
		t.Fatalf("token does not start with stet_ or is %d characters long; want 48", len(token))
	}
	if raw, err := base64.RawURLEncoding.Strict().DecodeString(token[5:]); err != nil || len(raw) != 32 {
		t.Fatalf("the 43 characters after stet_ are not base64url of 32 bytes (%d bytes, %v)", len(raw), err)
	}
	if v.Iterations != scram.Iterations || len(v.Salt) != 16 {
		t.Errorf("verifier: %d iterations, %d-byte salt; want %d, 16", v.Iterations, len(v.Salt), scram.Iterations)
	}

	token2, v2, err := scram.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if token2 == token {
		t.Error("two calls returned the same token")
	}
	if bytes.Equal(v2.Salt, v.Salt) {
		t.Error("two calls returned the same salt")
	}

	if _, ok, err := login(t, scram.NewServer(v, false, nil), token, "n,,"); err != nil || !ok {
		t.Fatalf("the token against its verifier: ok %v, err %v; want ok", ok, err)
	}
	if _, ok, err := login(t, scram.NewServer(v, false, nil), token2, "n,,"); err != nil || ok {
		t.Fatalf("another token against the verifier: ok %v, err %v; want not ok, no error", ok, err)
	}
}

// TestAcceptsAPostgresVerifier checks the package against PostgreSQL: the
// verifier PostgreSQL keeps for stet_scram (password scram-pw) reads, prints
// back as PostgreSQL wrote it, derives the same way, and lets the password
// and only the password through.
func TestAcceptsAPostgresVerifier(t *testing.T) {
	addr := os.Getenv("STET_TEST_PG")
	if addr == "" {
		t.Skip("STET_TEST_PG not set; start a test database with scripts/testdb.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, "postgres://postgres:postgres-pw@"+addr+"/stet_test?sslmode=disable")
	if err != nil {
		t.Fatalf("connect to the test server: %v", err)
	}
	defer conn.Close(context.Background())

	var text string
	if err := conn.QueryRow(ctx, "select rolpassword from pg_authid where rolname = 'stet_scram'").Scan(&text); err != nil {
		t.Fatalf("read stet_scram's verifier: %v", err)
	}
	v, err := scram.ParseVerifier(text)
	if err != nil {
		t.Fatalf("ParseVerifier(stet_scram's verifier): %v", err)
	}
	if v.String() != text {
		t.Error("String() does not give back the text PostgreSQL keeps")
	}
	if !sameVerifier(scram.NewVerifier("scram-pw", v.Salt, v.Iterations), v) {
		t.Error("NewVerifier(scram-pw) under PostgreSQL's salt and iterations differs from PostgreSQL's verifier")
	}

	if _, ok, err := login(t, scram.NewServer(v, false, nil), "scram-pw", "n,,"); err != nil || !ok {
		t.Fatalf("scram-pw: ok %v, err %v; want ok", ok, err)
	}
	if _, ok, err := login(t, scram.NewServer(v, false, nil), "not-scram-pw", "n,,"); err != nil || ok {
		t.Fatalf("another password: ok %v, err %v; want not ok, no error", ok, err)
	}
}

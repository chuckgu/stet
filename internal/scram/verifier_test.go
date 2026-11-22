package scram_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"

	"github.com/stetdb/stet/internal/scram"
)

// pieces reads v.Text() here, apart from the package, into its values. It
// never prints the text.
func pieces(t *testing.T, v scram.Verifier) (iterations int, salt, storedKey, serverKey []byte) {
	t.Helper()
	parts := strings.Split(v.Text(), "$")
	if len(parts) != 3 || parts[0] != "SCRAM-SHA-256" {
		t.Fatal("Text() is not SCRAM-SHA-256$…$…")
	}
	first, second := strings.Split(parts[1], ":"), strings.Split(parts[2], ":")
	if len(first) != 2 || len(second) != 2 {
		t.Fatal("Text() is not SCRAM-SHA-256$…:…$…:…")
	}
	iterations, err := strconv.Atoi(first[0])
	if err != nil {
		t.Fatal("Text()'s iteration count is not a number")
	}
	decoded := make([][]byte, 3)
	for i, s := range []string{first[1], second[0], second[1]} {
		if decoded[i], err = base64.StdEncoding.DecodeString(s); err != nil {
			t.Fatal("Text() holds broken base64")
		}
	}
	return iterations, decoded[0], decoded[1], decoded[2]
}

func TestVerifierRoundTrip(t *testing.T) {
	v := scram.NewVerifier("pencil", []byte("0123456789abcdef"), scram.Iterations)
	got, err := scram.ParseVerifier(v.Text())
	if err != nil {
		t.Fatalf("ParseVerifier(v.Text()): %v", err)
	}
	if got.Text() != v.Text() {
		t.Fatal("ParseVerifier(v.Text()) differs from v")
	}
	if !strings.HasPrefix(v.Text(), "SCRAM-SHA-256$4096:") {
		t.Errorf("Text() does not start with SCRAM-SHA-256$4096:")
	}
	if iterations, salt, storedKey, serverKey := pieces(t, v); iterations != 4096 ||
		string(salt) != "0123456789abcdef" || len(storedKey) != 32 || len(serverKey) != 32 {
		t.Errorf("Text() holds %d iterations, a %d-byte salt (the given one: %v), %d- and %d-byte keys",
			iterations, len(salt), string(salt) == "0123456789abcdef", len(storedKey), len(serverKey))
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
	iterations, salt, _, _ := pieces(t, v)
	if iterations != scram.Iterations || len(salt) != 16 {
		t.Errorf("verifier: %d iterations, %d-byte salt; want %d, 16", iterations, len(salt), scram.Iterations)
	}

	token2, v2, err := scram.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if token2 == token {
		t.Error("two calls returned the same token")
	}
	if _, salt2, _, _ := pieces(t, v2); bytes.Equal(salt2, salt) {
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
	if v.Text() != text {
		t.Error("Text() does not give back the text PostgreSQL keeps")
	}
	iterations, salt, _, _ := pieces(t, v)
	if scram.NewVerifier("scram-pw", salt, iterations).Text() != text {
		t.Error("NewVerifier(scram-pw) under PostgreSQL's salt and iterations differs from PostgreSQL's verifier")
	}

	if _, ok, err := login(t, scram.NewServer(v, false, nil), "scram-pw", "n,,"); err != nil || !ok {
		t.Fatalf("scram-pw: ok %v, err %v; want ok", ok, err)
	}
	if _, ok, err := login(t, scram.NewServer(v, false, nil), "not-scram-pw", "n,,"); err != nil || ok {
		t.Fatalf("another password: ok %v, err %v; want not ok, no error", ok, err)
	}
}

// TestVerifierNeverFormatsItsSecret formats a Verifier, and what holds one,
// every way a log line or an error could, and finds none of its salt and keys
// in any form those could take.
func TestVerifierNeverFormatsItsSecret(t *testing.T) {
	_, v, err := scram.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	const redacted = "SCRAM-SHA-256$4096:<redacted>"
	if v.String() != redacted {
		t.Errorf("String() is not %s", redacted)
	}

	_, salt, storedKey, serverKey := pieces(t, v)
	var forbidden []string
	for _, b := range [][]byte{salt, storedKey, serverKey} {
		forbidden = append(forbidden,
			base64.RawStdEncoding.EncodeToString(b), // also the padded form's prefix
			base64.RawURLEncoding.EncodeToString(b),
			hex.EncodeToString(b),
			strings.ToUpper(hex.EncodeToString(b)),
			string(b),
			strings.Trim(strconv.Quote(string(b)), `"`),
			strings.Trim(strconv.QuoteToASCII(string(b)), `"`),
			strings.Trim(fmt.Sprint(b), "[]"), // 1 2 3
			strings.TrimSuffix(strings.TrimPrefix(fmt.Sprintf("%#v", b), "[]byte{"), "}"), // 0x1, 0x2
		)
	}
	check := func(what, out string) {
		t.Helper()
		for _, f := range forbidden {
			if strings.Contains(out, f) {
				t.Errorf("%s writes out the verifier's salt or keys", what)
				return
			}
		}
	}

	type exported struct {
		Name string
		V    scram.Verifier
	}
	type unexported struct {
		name string
		v    scram.Verifier
	}
	values := []struct {
		name string
		x    any
	}{
		{"a Verifier", v},
		{"a *Verifier", &v},
		{"a struct with a Verifier field", exported{"alice", v}},
		{"a *struct with a Verifier field", &exported{"alice", v}},
		{"a struct with an unexported Verifier field", unexported{"alice", v}},
		{"a *struct with an unexported Verifier field", &unexported{"alice", v}},
		{"a map of Verifiers", map[string]scram.Verifier{"alice": v}},
		{"a slice of Verifiers", []scram.Verifier{v}},
		{"a *Server", scram.NewServer(v, false, nil)},
	}

	var text, js bytes.Buffer
	textLog := slog.New(slog.NewTextHandler(&text, nil))
	jsonLog := slog.New(slog.NewJSONHandler(&js, nil))
	for _, c := range values {
		for _, verb := range []string{"%v", "%+v", "%s", "%q", "%#v", "%x", "%X", "%d"} {
			check(verb+" of "+c.name, fmt.Sprintf(verb, c.x))
		}
		text.Reset()
		textLog.Info("m", "x", c.x)
		check("slog's TextHandler with "+c.name, text.String())
		js.Reset()
		jsonLog.Info("m", "x", c.x)
		check("slog's JSONHandler with "+c.name, js.String())
		if out, err := json.Marshal(c.x); err == nil {
			check("json.Marshal of "+c.name, string(out))
		}
		if out, err := yaml.Marshal(c.x); err == nil {
			check("yaml.Marshal of "+c.name, string(out))
		}
	}

	// The redacted form is what they write.
	if got := fmt.Sprintf("%v", v); got != redacted {
		t.Errorf("%%v of a Verifier is not %s", redacted)
	}
	var fromJSON string
	if out, err := json.Marshal(v); err != nil || json.Unmarshal(out, &fromJSON) != nil || fromJSON != redacted {
		t.Errorf("json.Marshal of a Verifier is not the JSON string %s (err %v)", redacted, err)
	}
	if out, err := yaml.Marshal(v); err != nil || !strings.Contains(string(out), "<redacted>") {
		t.Errorf("yaml.Marshal of a Verifier is not the redacted form (err %v)", err)
	}
	if got := slog.AnyValue(v).Resolve(); got.Kind() != slog.KindString || got.String() != redacted {
		t.Errorf("slog resolves a Verifier to something other than the string %s", redacted)
	}
}

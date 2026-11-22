// Package scram is the server side of SCRAM-SHA-256 (RFC 5802, 7677) as
// PostgreSQL speaks it, and the verifiers it checks clients against.
package scram

import (
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
)

const (
	Mechanism  = "SCRAM-SHA-256"
	Iterations = 4096
)

// keyLen is the length of a StoredKey, a ServerKey, a proof and a signature.
const keyLen = sha256.Size

// saltLen is the length of the salts this package makes, as PostgreSQL's.
const saltLen = 16

// minMockSecret is the shortest secret Mock takes.
const minMockSecret = 32

// tokenPrefix starts every token NewToken makes.
const tokenPrefix = "stet_"

// Verifier is what a server keeps of a password: enough to check a client's
// proof, not enough to log in as the client. Make one with NewVerifier,
// ParseVerifier, NewToken or Mock.
//
// Text is the only way to its salt and keys. Everything that formats a value
// (String, every fmt verb, log/slog, encoding/json, YAML) gets the redacted
// form "SCRAM-SHA-256$<iterations>:<redacted>".
type Verifier struct {
	// The values sit in a closure, which reflection cannot look into. fmt
	// prints a Verifier it reaches by reflection (in an unexported field,
	// where it cannot call Format) field by field, and would follow a pointer
	// there too (for %s, its bad-verb message prints the pointer's target),
	// but it prints a function as an address. The function also keeps
	// Verifier not comparable, as == would not compare the values.
	get func() material
}

type material struct {
	iterations           int
	salt                 []byte
	storedKey, serverKey []byte
}

func verifierOf(x material) Verifier {
	return Verifier{get: func() material { return x }}
}

// values returns v's values, all empty for the zero Verifier.
func (v Verifier) values() material {
	if v.get == nil {
		return material{}
	}
	return v.get()
}

var errVerifierForm = errors.New("scram: verifier: not of the form " + Mechanism + "$<iterations>:<salt>$<StoredKey>:<ServerKey>")

// ParseVerifier reads "SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>"
// (base64), the form PostgreSQL keeps in pg_authid and Text writes. Its errors
// never quote s.
func ParseVerifier(s string) (Verifier, error) {
	mechanism, rest, ok := strings.Cut(s, "$")
	if !ok || mechanism != Mechanism {
		return Verifier{}, errors.New("scram: verifier: not " + Mechanism)
	}
	params, keys, ok := strings.Cut(rest, "$")
	if !ok || strings.Contains(keys, "$") {
		return Verifier{}, errVerifierForm
	}
	iterText, saltText, ok := strings.Cut(params, ":")
	if !ok || strings.Contains(saltText, ":") {
		return Verifier{}, errVerifierForm
	}
	storedText, serverText, ok := strings.Cut(keys, ":")
	if !ok || strings.Contains(serverText, ":") {
		return Verifier{}, errVerifierForm
	}

	var x material
	var err error
	if x.iterations, err = parseIterations(iterText); err != nil {
		return Verifier{}, err
	}
	if x.salt, err = decodeBase64(saltText); err != nil || len(x.salt) == 0 {
		return Verifier{}, errors.New("scram: verifier: the salt is not non-empty base64")
	}
	if x.storedKey, err = decodeBase64(storedText); err != nil || len(x.storedKey) != keyLen {
		return Verifier{}, errors.New("scram: verifier: StoredKey is not base64 of 32 bytes")
	}
	if x.serverKey, err = decodeBase64(serverText); err != nil || len(x.serverKey) != keyLen {
		return Verifier{}, errors.New("scram: verifier: ServerKey is not base64 of 32 bytes")
	}
	return verifierOf(x), nil
}

// parseIterations reads a positive decimal count, digits only.
func parseIterations(s string) (int, error) {
	bad := errors.New("scram: verifier: the iteration count is not a positive number")
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, bad
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, bad
	}
	return n, nil
}

// decodeBase64 reads standard, padded, canonical base64 with no line breaks
// (which encoding/base64 would otherwise skip).
func decodeBase64(s string) ([]byte, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("scram: line break in base64")
	}
	return base64.StdEncoding.Strict().DecodeString(s)
}

// Text returns "SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>"
// (base64), what ParseVerifier reads: the form to store, and nothing else.
func (v Verifier) Text() string {
	x := v.values()
	b64 := base64.StdEncoding.EncodeToString
	return Mechanism + "$" + strconv.Itoa(x.iterations) + ":" + b64(x.salt) +
		"$" + b64(x.storedKey) + ":" + b64(x.serverKey)
}

// String returns the redacted form, "SCRAM-SHA-256$<iterations>:<redacted>".
func (v Verifier) String() string {
	return Mechanism + "$" + strconv.Itoa(v.values().iterations) + ":<redacted>"
}

// Format writes the redacted form for every verb (quoted for %q), so that
// no fmt verb, %#v and %x among them, reaches the salt or the keys.
func (v Verifier) Format(f fmt.State, verb rune) {
	s := v.String()
	if verb == 'q' {
		s = strconv.Quote(s)
	}
	io.WriteString(f, s)
}

// LogValue gives log/slog the redacted form.
func (v Verifier) LogValue() slog.Value { return slog.StringValue(v.String()) }

// MarshalJSON writes the redacted form as a JSON string.
func (v Verifier) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

// MarshalYAML writes the redacted form (go.yaml.in/yaml/v3's Marshaler).
func (v Verifier) MarshalYAML() (any, error) { return v.String(), nil }

// NewVerifier derives the verifier of password under salt. The password is
// used as given, without SASLprep: the same as PostgreSQL for ASCII
// passwords, which tokens are.
func NewVerifier(password string, salt []byte, iterations int) Verifier {
	if iterations < 1 {
		panic("scram: NewVerifier needs at least one iteration")
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, keyLen)
	if err != nil {
		// Only FIPS 140-only mode refuses a 32-byte key (for a salt shorter
		// than 16 bytes); its error names no secret.
		panic("scram: " + err.Error())
	}
	storedKey := sha256.Sum256(hmacSum(salted, "Client Key"))
	return verifierOf(material{
		iterations: iterations,
		salt:       bytes.Clone(salt),
		storedKey:  storedKey[:],
		serverKey:  hmacSum(salted, "Server Key"),
	})
}

// NewToken returns a new token, "stet_" and 43 base64url characters of 32
// random bytes, and its verifier under a random 16-byte salt.
func NewToken() (token string, v Verifier, err error) {
	raw := make([]byte, 32)
	salt := make([]byte, saltLen)
	if _, err := rand.Read(raw); err != nil {
		return "", Verifier{}, err
	}
	if _, err := rand.Read(salt); err != nil {
		return "", Verifier{}, err
	}
	token = tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return token, NewVerifier(token, salt, Iterations), nil
}

// Mock returns a verifier for an account that has none, so that an exchange
// with it looks like any other and always fails: the same account and secret
// give the same salt. secret must be at least 32 random bytes, or Mock
// panics: with a short or known secret anyone could compute the salts and
// tell the accounts that exist from those that do not.
func Mock(account string, secret []byte) Verifier {
	if len(secret) < minMockSecret {
		panic("scram: Mock needs a secret of at least 32 bytes")
	}
	derive := func(label string) []byte {
		m := hmac.New(sha256.New, secret)
		m.Write([]byte(label))
		m.Write([]byte{0})
		m.Write([]byte(account))
		return m.Sum(nil)
	}
	return verifierOf(material{
		iterations: Iterations,
		salt:       derive("salt")[:saltLen],
		storedKey:  derive("StoredKey"),
		serverKey:  derive("ServerKey"),
	})
}

func hmacSum(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

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
	"errors"
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

// tokenPrefix starts every token NewToken makes.
const tokenPrefix = "stet_"

// Verifier is what a server keeps of a password: enough to check a client's
// proof, not enough to log in as the client.
type Verifier struct {
	Iterations           int
	Salt                 []byte
	StoredKey, ServerKey []byte
}

var errVerifierForm = errors.New("scram: verifier: not of the form " + Mechanism + "$<iterations>:<salt>$<StoredKey>:<ServerKey>")

// ParseVerifier reads "SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>"
// (base64), the form PostgreSQL keeps in pg_authid. Its errors never quote s.
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

	var v Verifier
	var err error
	if v.Iterations, err = parseIterations(iterText); err != nil {
		return Verifier{}, err
	}
	if v.Salt, err = decodeBase64(saltText); err != nil || len(v.Salt) == 0 {
		return Verifier{}, errors.New("scram: verifier: the salt is not non-empty base64")
	}
	if v.StoredKey, err = decodeBase64(storedText); err != nil || len(v.StoredKey) != keyLen {
		return Verifier{}, errors.New("scram: verifier: StoredKey is not base64 of 32 bytes")
	}
	if v.ServerKey, err = decodeBase64(serverText); err != nil || len(v.ServerKey) != keyLen {
		return Verifier{}, errors.New("scram: verifier: ServerKey is not base64 of 32 bytes")
	}
	return v, nil
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

func (v Verifier) String() string {
	b64 := base64.StdEncoding.EncodeToString
	return Mechanism + "$" + strconv.Itoa(v.Iterations) + ":" + b64(v.Salt) +
		"$" + b64(v.StoredKey) + ":" + b64(v.ServerKey)
}

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
	return Verifier{
		Iterations: iterations,
		Salt:       bytes.Clone(salt),
		StoredKey:  storedKey[:],
		ServerKey:  hmacSum(salted, "Server Key"),
	}
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
// give the same salt.
func Mock(account string, secret []byte) Verifier {
	derive := func(label string) []byte {
		m := hmac.New(sha256.New, secret)
		m.Write([]byte(label))
		m.Write([]byte{0})
		m.Write([]byte(account))
		return m.Sum(nil)
	}
	return Verifier{
		Iterations: Iterations,
		Salt:       derive("salt")[:saltLen],
		StoredKey:  derive("StoredKey"),
		ServerKey:  derive("ServerKey"),
	}
}

func hmacSum(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

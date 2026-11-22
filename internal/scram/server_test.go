package scram_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stetdb/stet/internal/scram"
)

// The client side of SCRAM-SHA-256, computed here from the RFC rather than
// with the package, so that the tests check the server against it.

const testClientNonce = "fyko+d2lbbFgONRv9qkxdawL"

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// clientFinal returns the client-final-message a client that knows password
// sends after clientFirstBare and serverFirst, binding gs2 in c=, and the
// server signature it then expects in v=.
func clientFinal(t *testing.T, password, gs2, clientFirstBare, serverFirst string) (final string, serverSig []byte) {
	t.Helper()
	fields := strings.Split(serverFirst, ",")
	if len(fields) != 3 || !strings.HasPrefix(fields[0], "r=") || !strings.HasPrefix(fields[1], "s=") || !strings.HasPrefix(fields[2], "i=") {
		t.Fatalf("server-first-message is not r=,s=,i=: %q", serverFirst)
	}
	nonce := fields[0][2:]
	salt, err := base64.StdEncoding.DecodeString(fields[1][2:])
	if err != nil {
		t.Fatalf("server-first-message salt: %v", err)
	}
	iterations, err := strconv.Atoi(fields[2][2:])
	if err != nil {
		t.Fatalf("server-first-message iterations: %v", err)
	}

	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, 32)
	if err != nil {
		t.Fatal(err)
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	withoutProof := "c=" + base64.StdEncoding.EncodeToString([]byte(gs2)) + ",r=" + nonce
	authMessage := clientFirstBare + "," + serverFirst + "," + withoutProof
	proof := hmacSHA256(storedKey[:], authMessage)
	for i := range proof {
		proof[i] ^= clientKey[i]
	}
	serverSig = hmacSHA256(hmacSHA256(salted, "Server Key"), authMessage)
	return withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof), serverSig
}

// login runs a whole exchange against s as a client that knows password and
// returns what Final returned. When Final says ok, it also checks the server
// signature as a client would.
func login(t *testing.T, s *scram.Server, password, gs2 string) (serverFinal []byte, ok bool, err error) {
	t.Helper()
	bare := "n=,r=" + testClientNonce
	serverFirst, err := s.First([]byte(gs2 + bare))
	if err != nil {
		t.Fatalf("First: %v", err)
	}
	final, serverSig := clientFinal(t, password, gs2, bare, string(serverFirst))
	serverFinal, ok, err = s.Final([]byte(final))
	if ok && string(serverFinal) != "v="+base64.StdEncoding.EncodeToString(serverSig) {
		t.Errorf("the server-final-message does not carry the server signature the client expects")
	}
	return serverFinal, ok, err
}

func fixedNonce(n string) func() string { return func() string { return n } }

func TestRFC7677Vector(t *testing.T) {
	salt, err := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	if err != nil {
		t.Fatal(err)
	}
	v := scram.NewVerifier("pencil", salt, 4096)
	s := scram.NewServer(v, false, fixedNonce("%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"))

	serverFirst, err := s.First([]byte("n,,n=user,r=rOprNGfwEbeRWgbNEkqO"))
	if err != nil {
		t.Fatalf("First: %v", err)
	}
	if want := "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"; string(serverFirst) != want {
		t.Fatalf("server-first-message = %q, want %q", serverFirst, want)
	}

	serverFinal, ok, err := s.Final([]byte("c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="))
	if err != nil || !ok {
		t.Fatalf("Final: ok %v, err %v; want ok", ok, err)
	}
	if want := "v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="; string(serverFinal) != want {
		t.Fatalf("server-final-message = %q, want %q", serverFinal, want)
	}
}

func TestWrongPasswordFails(t *testing.T) {
	v := scram.NewVerifier("right password", []byte("0123456789abcdef"), scram.Iterations)
	serverFinal, ok, err := login(t, scram.NewServer(v, false, nil), "wrong password", "n,,")
	if err != nil {
		t.Fatalf("Final: %v; want no error for a well-formed message with a wrong proof", err)
	}
	if ok {
		t.Fatal("Final accepted the proof of a wrong password")
	}
	if serverFinal != nil {
		t.Errorf("Final returned a server-final-message (%d bytes) for a wrong proof", len(serverFinal))
	}

	// The right password through the same path passes, so the failure above
	// is the password's.
	if _, ok, err := login(t, scram.NewServer(v, false, nil), "right password", "n,,"); err != nil || !ok {
		t.Fatalf("the right password: ok %v, err %v", ok, err)
	}
}

// serverFirstShape matches a server-first-message and captures its nonce,
// salt and iterations.
var serverFirstShape = regexp.MustCompile(`^r=([!-+\--~]+),s=([A-Za-z0-9+/]+={0,2}),i=([0-9]+)$`)

func TestMockNeverSucceeds(t *testing.T) {
	secret := []byte("a secret of the running server")
	a1, a2 := scram.Mock("alice", secret), scram.Mock("alice", secret)
	if !bytes.Equal(a1.Salt, a2.Salt) {
		t.Error("Mock gave the same account and secret different salts")
	}
	if b := scram.Mock("bob", secret); bytes.Equal(a1.Salt, b.Salt) {
		t.Error("Mock gave different accounts the same salt")
	}
	if c := scram.Mock("alice", []byte("another secret")); bytes.Equal(a1.Salt, c.Salt) {
		t.Error("Mock gave different secrets the same salt")
	}

	// The server-first-message looks like one for a real verifier.
	_, realV, err := scram.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	shape := func(v scram.Verifier, mock bool) (salt []byte, iterations string) {
		t.Helper()
		first, err := scram.NewServer(v, mock, nil).First([]byte("n,,n=,r=" + testClientNonce))
		if err != nil {
			t.Fatalf("First: %v", err)
		}
		m := serverFirstShape.FindStringSubmatch(string(first))
		if m == nil {
			t.Fatalf("server-first-message %q is not r=,s=,i=", first)
		}
		if !strings.HasPrefix(m[1], testClientNonce) || len(m[1]) == len(testClientNonce) {
			t.Errorf("the nonce of %q does not extend the client's", first)
		}
		salt, err = base64.StdEncoding.DecodeString(m[2])
		if err != nil {
			t.Fatalf("salt of %q: %v", first, err)
		}
		return salt, m[3]
	}
	realSalt, realIterations := shape(realV, false)
	mockSalt, mockIterations := shape(a1, true)
	if mockIterations != realIterations || mockIterations != strconv.Itoa(scram.Iterations) {
		t.Errorf("iterations: mock %s, real %s; want both %d", mockIterations, realIterations, scram.Iterations)
	}
	if len(mockSalt) != 16 || len(realSalt) != 16 {
		t.Errorf("salt lengths: mock %d, real %d; want 16", len(mockSalt), len(realSalt))
	}

	// No proof passes: one made with some password, random ones, and the
	// right proof of a real verifier run as a mock.
	if _, ok, err := login(t, scram.NewServer(a1, true, nil), "any password", "n,,"); ok || err != nil {
		t.Errorf("mock with a password's proof: ok %v, err %v; want not ok, no error", ok, err)
	}
	for range 3 {
		s := scram.NewServer(a1, true, nil)
		first, err := s.First([]byte("n,,n=,r=" + testClientNonce))
		if err != nil {
			t.Fatal(err)
		}
		proof := make([]byte, 32)
		rand.Read(proof)
		nonce := serverFirstShape.FindStringSubmatch(string(first))[1]
		final, ok, err := s.Final([]byte("c=biws,r=" + nonce + ",p=" + base64.StdEncoding.EncodeToString(proof)))
		if ok || err != nil || final != nil {
			t.Errorf("mock with a random proof: ok %v, err %v, %d bytes; want not ok, no error, nothing", ok, err, len(final))
		}
	}
	v := scram.NewVerifier("the password", []byte("0123456789abcdef"), scram.Iterations)
	if final, ok, err := login(t, scram.NewServer(v, true, nil), "the password", "n,,"); ok || err != nil || final != nil {
		t.Errorf("mock over a real verifier with the right proof: ok %v, err %v, %d bytes; want not ok, no error, nothing", ok, err, len(final))
	}
}

// noPanic runs f and fails the test if it panics.
func noPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked", what)
		}
	}()
	f()
}

func TestServerRefusesBrokenMessages(t *testing.T) {
	v := scram.NewVerifier("pw", []byte("0123456789abcdef"), scram.Iterations)
	const serverNonce = "server-nonce"
	nonce := testClientNonce + serverNonce
	anyProof := base64.StdEncoding.EncodeToString(make([]byte, 32))
	huge := strings.Repeat("a", 1<<20)

	firsts := []struct{ name, msg string }{
		{"tls-server-end-point binding", "p=tls-server-end-point,,n=,r=" + testClientNonce},
		{"authorization identity", "n,a=someone,n=,r=" + testClientNonce},
		{"no nonce", "n,,n=user"},
		{"empty nonce", "n,,n=,r="},
		{"unknown binding flag", "x,,n=,r=" + testClientNonce},
		{"mandatory extension", "n,,m=ext,n=,r=" + testClientNonce},
		{"empty", ""},
		{"1MiB", "n,,n=,r=" + huge},
	}
	for _, c := range firsts {
		noPanic(t, "First("+c.name+")", func() {
			s := scram.NewServer(v, false, fixedNonce(serverNonce))
			if _, err := s.First([]byte(c.msg)); err == nil {
				t.Errorf("First(%s): no error", c.name)
			}
		})
	}

	finals := []struct{ name, first, final string }{
		{"other server nonce", "n,,", "c=biws,r=" + testClientNonce + "other,p=" + anyProof},
		{"c= other than the first's flag", "y,,", "c=biws,r=" + nonce + ",p=" + anyProof},
		{"broken base64 proof", "n,,", "c=biws,r=" + nonce + ",p=!!not*base64!!"},
		{"31-byte proof", "n,,", "c=biws,r=" + nonce + ",p=" + base64.StdEncoding.EncodeToString(make([]byte, 31))},
		{"33-byte proof", "n,,", "c=biws,r=" + nonce + ",p=" + base64.StdEncoding.EncodeToString(make([]byte, 33))},
		{"no proof", "n,,", "c=biws,r=" + nonce},
		{"something after the proof", "n,,", "c=biws,r=" + nonce + ",p=" + anyProof + ",x=1"},
		{"empty", "n,,", ""},
		{"1MiB", "n,,", "c=biws,r=" + nonce + ",x=" + huge + ",p=" + anyProof},
	}
	for _, c := range finals {
		noPanic(t, "Final("+c.name+")", func() {
			s := scram.NewServer(v, false, fixedNonce(serverNonce))
			if _, err := s.First([]byte(c.first + "n=,r=" + testClientNonce)); err != nil {
				t.Fatalf("First before Final(%s): %v", c.name, err)
			}
			if final, ok, err := s.Final([]byte(c.final)); err == nil || ok || final != nil {
				t.Errorf("Final(%s): ok %v, err %v, %d bytes; want an error", c.name, ok, err, len(final))
			}
		})
	}

	// y,, bound back as eSws is accepted.
	if _, ok, err := login(t, scram.NewServer(v, false, nil), "pw", "y,,"); err != nil || !ok {
		t.Errorf("y,, with c=eSws: ok %v, err %v; want ok", ok, err)
	}
}

func TestServerIsUsedOnce(t *testing.T) {
	v := scram.NewVerifier("pw", []byte("0123456789abcdef"), scram.Iterations)
	first := []byte("n,,n=,r=" + testClientNonce)

	s := scram.NewServer(v, false, nil)
	if _, _, err := s.Final([]byte("c=biws,r=x,p=" + base64.StdEncoding.EncodeToString(make([]byte, 32)))); err == nil {
		t.Error("Final before First: no error")
	}

	s = scram.NewServer(v, false, nil)
	if _, err := s.First(first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.First(first); err == nil {
		t.Error("a second First: no error")
	}

	s = scram.NewServer(v, false, nil)
	serverFirst, err := s.First(first)
	if err != nil {
		t.Fatal(err)
	}
	final, _ := clientFinal(t, "pw", "n,,", "n=,r="+testClientNonce, string(serverFirst))
	if _, ok, err := s.Final([]byte(final)); err != nil || !ok {
		t.Fatalf("the first Final: ok %v, err %v", ok, err)
	}
	if _, ok, err := s.Final([]byte(final)); err == nil || ok {
		t.Errorf("a second Final: ok %v, err %v; want an error", ok, err)
	}
}

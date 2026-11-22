package scram

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"slices"
	"strconv"
	"strings"
)

// maxMessageLen bounds a client message, as PostgreSQL bounds an
// authentication message (PG_MAX_AUTH_TOKEN_LENGTH).
const maxMessageLen = 65535

type stage int

const (
	stageNew   stage = iota // waiting for First
	stageFirst              // First done, waiting for Final
	stageDone               // ended by Final or by an error
)

// Server runs the server side of one exchange. It is not safe for
// concurrent use.
type Server struct {
	v        Verifier
	mock     bool
	newNonce func() string
	stage    stage

	gs2Header       string // "n,," or "y,,", as the client sent it
	clientFirstBare string
	serverFirst     string
	nonce           string // the client's nonce and the server's
}

// NewServer starts an exchange against v. mock makes Final fail whatever the
// proof. nonce, when not nil, supplies the server's nonce (tests).
func NewServer(v Verifier, mock bool, nonce func() string) *Server {
	if nonce == nil {
		nonce = randomNonce
	}
	return &Server{v: v, mock: mock, newNonce: nonce}
}

// randomNonce returns 18 random bytes in base64, as PostgreSQL's nonce.
func randomNonce() string {
	b := make([]byte, 18)
	rand.Read(b) // never fails: it crashes the program instead
	return base64.StdEncoding.EncodeToString(b)
}

// First takes the data of a SASLInitialResponse (the client-first-message)
// and returns the server-first-message.
func (s *Server) First(clientFirst []byte) (serverFirst []byte, err error) {
	if s.stage != stageNew {
		s.stage = stageDone
		return nil, errors.New("scram: First after First or Final")
	}
	s.stage = stageDone // until the message proves well formed
	if err := checkMessage(clientFirst); err != nil {
		return nil, err
	}
	msg := string(clientFirst)

	// gs2-header: a channel-binding flag, no authorization identity, a comma.
	switch {
	case strings.HasPrefix(msg, "p="):
		return nil, errors.New("scram: client-first-message: channel binding is not supported")
	case !strings.HasPrefix(msg, "n,") && !strings.HasPrefix(msg, "y,"):
		return nil, errors.New("scram: client-first-message: no channel-binding flag")
	case strings.HasPrefix(msg[2:], "a="):
		return nil, errors.New("scram: client-first-message: an authorization identity is not supported")
	case !strings.HasPrefix(msg[2:], ","):
		return nil, errors.New("scram: client-first-message: malformed gs2 header")
	}
	gs2Header, bare := msg[:3], msg[3:]

	// client-first-message-bare: n=<user>,r=<nonce>[,<extension>...]. The user
	// name is not looked at, as in PostgreSQL: the startup packet names the
	// account.
	attrs := strings.Split(bare, ",")
	switch {
	case strings.HasPrefix(attrs[0], "m="):
		return nil, errors.New("scram: client-first-message: a mandatory extension is not supported")
	case !strings.HasPrefix(attrs[0], "n="):
		return nil, errors.New("scram: client-first-message: no user name attribute")
	case len(attrs) < 2 || !strings.HasPrefix(attrs[1], "r=") || !printable(attrs[1][2:]):
		return nil, errors.New("scram: client-first-message: no well-formed nonce")
	case slices.ContainsFunc(attrs[2:], notAttribute):
		return nil, errors.New("scram: client-first-message: malformed extension")
	}

	s.gs2Header = gs2Header
	s.clientFirstBare = bare
	s.nonce = attrs[1][2:] + s.newNonce()
	x := s.v.values()
	s.serverFirst = "r=" + s.nonce +
		",s=" + base64.StdEncoding.EncodeToString(x.salt) +
		",i=" + strconv.Itoa(x.iterations)
	s.stage = stageFirst
	return []byte(s.serverFirst), nil
}

// Final takes the client-final-message. ok false with a nil error: the
// message was well formed and the proof wrong. An error: it could not be read.
func (s *Server) Final(clientFinal []byte) (serverFinal []byte, ok bool, err error) {
	if s.stage != stageFirst {
		s.stage = stageDone
		return nil, false, errors.New("scram: Final before First or after Final")
	}
	s.stage = stageDone
	if err := checkMessage(clientFinal); err != nil {
		return nil, false, err
	}
	msg := string(clientFinal)

	// client-final-message: c=<binding>,r=<nonce>[,<extension>...],p=<proof>.
	attrs := strings.Split(msg, ",")
	if !strings.HasPrefix(attrs[0], "c=") ||
		attrs[0][2:] != base64.StdEncoding.EncodeToString([]byte(s.gs2Header)) {
		return nil, false, errors.New("scram: client-final-message: the channel binding is not the client-first-message's")
	}
	if len(attrs) < 2 || !strings.HasPrefix(attrs[1], "r=") ||
		subtle.ConstantTimeCompare([]byte(attrs[1][2:]), []byte(s.nonce)) != 1 {
		return nil, false, errors.New("scram: client-final-message: the nonce is not the server-first-message's")
	}
	// The proof is the first p= attribute and the last attribute.
	p := slices.IndexFunc(attrs, func(a string) bool { return strings.HasPrefix(a, "p=") })
	if p < 0 {
		return nil, false, errors.New("scram: client-final-message: no proof")
	}
	if p != len(attrs)-1 {
		return nil, false, errors.New("scram: client-final-message: something after the proof")
	}
	if slices.ContainsFunc(attrs[2:p], notAttribute) {
		return nil, false, errors.New("scram: client-final-message: malformed extension")
	}
	proof, err := decodeBase64(attrs[p][2:])
	if err != nil || len(proof) != keyLen {
		return nil, false, errors.New("scram: client-final-message: the proof is not base64 of 32 bytes")
	}
	withoutProof := msg[:len(msg)-len(attrs[p])-1]

	// ClientKey = proof XOR HMAC(StoredKey, AuthMessage); it must hash to
	// StoredKey. A mock does the same work and fails.
	authMessage := s.clientFirstBare + "," + s.serverFirst + "," + withoutProof
	x := s.v.values()
	clientKey := hmacSum(x.storedKey, authMessage)
	subtle.XORBytes(clientKey, clientKey, proof)
	storedKey := sha256.Sum256(clientKey)
	if subtle.ConstantTimeCompare(storedKey[:], x.storedKey) != 1 || s.mock {
		return nil, false, nil
	}
	return []byte("v=" + base64.StdEncoding.EncodeToString(hmacSum(x.serverKey, authMessage))), true, nil
}

// checkMessage refuses what no well-formed message is: nothing, more than
// maxMessageLen bytes, or a NUL byte (PostgreSQL reads messages as C strings).
func checkMessage(m []byte) error {
	switch {
	case len(m) == 0:
		return errors.New("scram: empty message")
	case len(m) > maxMessageLen:
		return errors.New("scram: message too long")
	case bytes.IndexByte(m, 0) >= 0:
		return errors.New("scram: NUL byte in message")
	}
	return nil
}

// printable reports whether s is a non-empty run of printable ASCII other
// than a comma, as a nonce must be.
func printable(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e || s[i] == ',' {
			return false
		}
	}
	return true
}

// notAttribute reports whether a is not an attribute: a letter, "=", a value.
func notAttribute(a string) bool {
	return len(a) < 2 || a[1] != '=' || !('a' <= a[0] && a[0] <= 'z' || 'A' <= a[0] && a[0] <= 'Z')
}

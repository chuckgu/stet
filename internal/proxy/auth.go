package proxy

import (
	"bytes"
	"encoding/binary"
)

const (
	authSASL = 10 // the Authentication code of AuthenticationSASL

	// channelBound is the mechanism a server offers only over TLS. It binds
	// the client's proof to the server's certificate, and a client on the
	// proxy's plaintext leg cannot use it: libpq refuses a list that holds it.
	channelBound = "SCRAM-SHA-256-PLUS"

	// onlyChannelBound is the message of the FATAL a client is given when
	// nothing is left of the server's list without channelBound.
	onlyChannelBound = "stet: the server offers only channel-bound authentication, which cannot pass through stet"
)

// stripPlus looks at the body of an Authentication message. When it is an
// AuthenticationSASL (code 10) that lists SCRAM-SHA-256-PLUS, replace is the
// whole message (type, length, body) with the same list without it, and left
// is how many mechanisms remain. changed false: the message is forwarded as
// the server sent it. A body it cannot read is not changed.
//
// A body it can read is the code, then names each ended by a zero byte, then
// the zero byte that ends the list, and nothing after it.
func stripPlus(body []byte) (replace []byte, left int, changed bool) {
	if len(body) < 4 || binary.BigEndian.Uint32(body) != authSASL {
		return nil, 0, false
	}
	out := append([]byte{'R', 0, 0, 0, 0}, body[:4]...)
	rest := body[4:]
	for {
		end := bytes.IndexByte(rest, 0)
		if end < 0 { // a name, or the list, is not ended
			return nil, 0, false
		}
		if end == 0 {
			break
		}
		if string(rest[:end]) == channelBound {
			changed = true
		} else {
			out = append(out, rest[:end+1]...)
			left++
		}
		rest = rest[end+1:]
	}
	if !changed || len(rest) != 1 {
		return nil, 0, false
	}
	out = append(out, 0)
	binary.BigEndian.PutUint32(out[1:5], uint32(len(out)-1))
	return out, left, true
}

// authTap returns the server-side tap of a session whose client leg is
// plaintext and whose upstream leg is TLS: inner (nil for a session that is
// only relayed) and, until the server's first ReadyForQuery, a look at each
// Authentication message. At that ReadyForQuery, once inner has seen it, the
// tap's fields become inner's (all nil without one), so nothing is added per
// message after authentication. refused returns the FATAL to give the client
// when the tap ended the session, else nil.
//
// An Authentication message whose list holds nothing but SCRAM-SHA-256-PLUS
// ends the session (errEnded) and is not forwarded. inner, if it wants
// Authentication messages, is shown each as the client gets it. The tap is
// used by the one pump that reads the server, which reads its fields afresh
// for every message; refused is for relay, once that pump has stopped.
func authTap(inner *tap) (t *tap, refused func() []byte) {
	t = &tap{}
	innerWants := func(byte) bool { return false }
	if inner != nil {
		innerWants = inner.want
		t.each = inner.each
	}
	var fatal []byte
	t.want = func(m byte) bool { return m == 'R' || m == 'Z' || innerWants(m) }
	t.see = func(m byte, body []byte, flush func() error) ([]byte, error) {
		switch m {
		case 'R':
			replace, left, changed := stripPlus(body)
			if changed && left == 0 {
				fatal = errorMessage("FATAL", Refusal{Code: "28000", Message: onlyChannelBound})
				return nil, errEnded
			}
			if innerWants(m) {
				if changed {
					body = replace[5:]
				}
				if r, err := inner.see(m, body, flush); r != nil || err != nil {
					return r, err
				}
			}
			return replace, nil
		case 'Z':
			var replace []byte
			var err error
			if innerWants(m) {
				replace, err = inner.see(m, body, flush)
			}
			if inner != nil {
				*t = *inner
			} else {
				*t = tap{}
			}
			return replace, err
		}
		return inner.see(m, body, flush)
	}
	return t, func() []byte { return fatal }
}

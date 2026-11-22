package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"slices"
	"strings"
)

// Policy says which sessions the gate is shown.
type Policy struct {
	// Agents lists the login roles whose sessions are watched, compared byte
	// for byte with the role the server will authenticate. Empty: every session.
	Agents []string
	// Enforce makes watched sessions fail closed: a message that cannot be
	// inspected is refused instead of forwarded. Sessions that are not
	// watched, and every session when there is no gate, are relayed as
	// without it.
	Enforce bool
}

// watches reports whether the gate is shown a session whose startup packet
// startupInfo read as info (ok false: it could not read it). A session the
// proxy cannot place is treated as an agent's: an unjudged agent write is the
// worse mistake.
func (p Policy) watches(info SessionInfo, ok bool) bool {
	return !ok || len(p.Agents) == 0 || slices.Contains(p.Agents, info.User)
}

// maxName is how many bytes of a name the server keeps (NAMEDATALEN - 1).
const maxName = 63

// startupInfo reads who a session is from its StartupMessage packet (length
// word included) the way the server reads it (ProcessStartupPacket), so the
// user is the role the server will authenticate: any 3.x protocol version,
// the last of a repeated key, user and database cut to 63 bytes, and the
// database defaulting to the user. It reports ok false for a packet the
// server would refuse to start a session from: another major version, a
// broken key/value layout, or no user. Then info is empty.
func startupInfo(pkt []byte) (info SessionInfo, ok bool) {
	if len(pkt) < 8 || binary.BigEndian.Uint32(pkt[4:8])>>16 != 3 {
		return SessionInfo{}, false
	}
	body := pkt[8:]
	var user, database, app []byte
	for off := 0; ; {
		key, rest, found := bytes.Cut(body[off:], []byte{0})
		if !found {
			return SessionInfo{}, false // no terminator
		}
		if len(key) == 0 { // the terminator, which must be the last byte
			if off != len(body)-1 {
				return SessionInfo{}, false
			}
			break
		}
		val, _, found := bytes.Cut(rest, []byte{0})
		if !found {
			return SessionInfo{}, false // a key without a value
		}
		switch string(key) {
		case "user":
			user = val
		case "database":
			database = val
		case "application_name":
			app = val
		}
		off += len(key) + len(val) + 2
	}
	if len(user) == 0 {
		return SessionInfo{}, false
	}
	if len(database) == 0 {
		database = user
	}
	return SessionInfo{User: string(cutName(user)), Database: string(cutName(database)), ApplicationName: string(app)}, true
}

// startupParams reads the parameters of a StartupMessage packet that
// startupInfo reads (ok true), the last value of a repeated key winning:
// params are those the server would take as settings, without user and
// database; options are the protocol options (keys starting "_pq_."), which
// the proxy does not take; replication reports a replication key, whatever
// its value.
func startupParams(pkt []byte) (params map[string]string, options []string, replication bool) {
	params = map[string]string{}
	body := pkt[8:]
	for off := 0; off < len(body)-1; {
		key, rest, _ := bytes.Cut(body[off:], []byte{0})
		val, _, _ := bytes.Cut(rest, []byte{0})
		off += len(key) + len(val) + 2
		switch k := string(key); {
		case k == "user", k == "database":
		case k == "replication":
			replication = true
		case strings.HasPrefix(k, "_pq_."):
			options = append(options, k)
		default:
			params[k] = string(val)
		}
	}
	return params, options, replication
}

// cutName keeps the bytes of a name the server keeps.
func cutName(b []byte) []byte {
	if len(b) > maxName {
		return b[:maxName]
	}
	return b
}

// newSessionID returns a random id for a watched session.
func newSessionID() string {
	var id [16]byte
	rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

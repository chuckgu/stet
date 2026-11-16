package proxy

import (
	"encoding/binary"
	"strings"
	"testing"
)

// startupPacket builds a startup packet: its length, the protocol code, then
// body exactly as given.
func startupPacket(protocol uint32, body string) []byte {
	pkt := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
	pkt = binary.BigEndian.AppendUint32(pkt, protocol)
	return append(pkt, body...)
}

// pairs lays out key/value strings as a startup packet body, terminator
// included.
func pairs(kv ...string) string {
	var b strings.Builder
	for _, s := range kv {
		b.WriteString(s)
		b.WriteByte(0)
	}
	b.WriteByte(0)
	return b.String()
}

// The login role is the one the server will authenticate, so the packet is
// read as PostgreSQL reads it (ProcessStartupPacket).
func TestStartupUserIsReadAsTheServerReadsIt(t *testing.T) {
	for _, minor := range []uint32{0, 1, 2, 9} {
		info, ok := startupInfo(startupPacket(3<<16|minor, pairs("user", "agent", "database", "db", "application_name", "app")))
		if !ok || info.User != "agent" || info.Database != "db" || info.ApplicationName != "app" {
			t.Errorf("protocol 3.%d: %+v (ok %v), want user agent, database db, application_name app", minor, info, ok)
		}
	}

	long := strings.Repeat("a", 63)
	for _, c := range []struct {
		name, body     string
		user, database string
	}{
		{"user given twice", pairs("user", "first", "database", "db", "user", "second"), "second", "db"},
		{"70-byte names", pairs("user", long+"bbbbbbb", "database", long+"ccccccc"), long, long},
		{"no database", pairs("user", long+"bbbbbbb"), long, long},
		{"empty database", pairs("user", "agent", "database", ""), "agent", "agent"},
	} {
		info, ok := startupInfo(startupPacket(3<<16, c.body))
		if !ok || info.User != c.user || info.Database != c.database {
			t.Errorf("%s: user %q database %q (ok %v), want %q and %q", c.name, info.User, info.Database, ok, c.user, c.database)
		}
	}

	for name, pkt := range map[string][]byte{
		"no terminator":              startupPacket(3<<16, "user\x00agent\x00"),
		"value without its NUL":      startupPacket(3<<16, "user\x00agent"),
		"ends with a key":            startupPacket(3<<16, "user\x00agent\x00database\x00"),
		"value, then no terminator":  startupPacket(3<<16, "user\x00agent\x00database\x00\x00"),
		"bytes after the terminator": startupPacket(3<<16, pairs("user", "agent")+"x"),
		"no user":                    startupPacket(3<<16, pairs("database", "db")),
		"empty user":                 startupPacket(3<<16, pairs("user", "", "database", "db")),
		"empty body":                 startupPacket(3<<16, ""),
		"protocol 2":                 startupPacket(2<<16, pairs("user", "agent")),
		"shorter than its header":    {0, 0, 0, 6, 0, 3},
	} {
		if info, ok := startupInfo(pkt); ok || info != (SessionInfo{}) {
			t.Errorf("%s: %+v (ok %v), want nothing read", name, info, ok)
		}
	}
}

func TestPolicyWatches(t *testing.T) {
	agent := SessionInfo{User: "agent"}
	other := SessionInfo{User: "other"}
	if all := (Policy{}); !all.watches(agent, true) || !all.watches(other, true) {
		t.Error("an empty Policy must watch every session")
	}
	p := Policy{Agents: []string{"agent", "agent_b"}}
	if !p.watches(agent, true) || !p.watches(SessionInfo{User: "agent_b"}, true) {
		t.Error("a listed role's session is not watched")
	}
	if p.watches(other, true) || p.watches(SessionInfo{User: "Agent"}, true) {
		t.Error("a role not listed byte for byte is watched")
	}

	info, ok := startupInfo(startupPacket(3<<16, "user\x00other\x00"))
	if ok || info.User != "" {
		t.Fatalf("broken packet read as %+v (ok %v)", info, ok)
	}
	for _, pol := range []Policy{{}, p} {
		if !pol.watches(info, ok) {
			t.Errorf("Policy %+v: a session whose startup packet could not be read is not watched", pol)
		}
	}
}

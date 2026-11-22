package proxy

import (
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// MaxUnitKeys is how many keys the reads of one unit take in all.
const MaxUnitKeys = maxUnitKeys

// ServerShows drives a session watched through g as the pumps do, with no
// connection: after the server's first ReadyForQuery the client sends sent,
// then the server answers with answers. It reports, for each answer, whether
// the server side's tap asked for its body (and so was shown it).
func ServerShows(g Gate, sent []pgproto3.FrontendMessage, answers []pgproto3.BackendMessage) []bool {
	s := newSession(g, SessionInfo{})
	client, server := s.clientTap(), s.serverTap()
	shows := func(tp *tap, m pgproto3.Message) bool {
		b, err := m.Encode(nil)
		if err != nil {
			panic(err)
		}
		if tp.each != nil {
			tp.each(b[0])
		}
		if !tp.want(b[0]) {
			return false
		}
		tp.see(b[0], b[5:], func() error { return nil })
		return true
	}
	shows(server, &pgproto3.ReadyForQuery{TxStatus: 'I'}) // the end of startup
	for _, m := range sent {
		shows(client, m)
	}
	shown := make([]bool, len(answers))
	for i, m := range answers {
		shown[i] = shows(server, m)
	}
	return shown
}

// SetMaxInspect changes how large a message the proxy reads before it stops
// inspecting it, until restore is called. Tests that use it must not run in
// parallel, and must restore it only once every proxy started meanwhile has
// closed (t.Cleanup registered before the proxy starts).
func SetMaxInspect(n int64) (restore func()) {
	old := maxInspect
	maxInspect = n
	return func() { maxInspect = old }
}

// SetDialTimeout changes how long the proxy waits for upstream to accept a
// connection, and then to answer the TLS request and finish the handshake,
// until restore is called; as SetMaxInspect.
func SetDialTimeout(d time.Duration) (restore func()) {
	old := dialTimeout
	dialTimeout = d
	return func() { dialTimeout = old }
}

// SetServerHook makes every session call f with the type of each server
// message it follows, with the session's lock held, until restore is called:
// tests use it to panic where the proxy holds that lock. Tests that use it
// must not run in parallel, and must restore it only once every proxy
// started meanwhile has closed.
func SetServerHook(f func(t byte)) (restore func()) {
	old := serverHook
	serverHook = f
	return func() { serverHook = old }
}

// SetStuckTick changes how often relay looks, after a panic, whether the
// other side can still stop (see relay), until restore is called; as
// SetServerHook.
func SetStuckTick(d time.Duration) (restore func()) {
	old := stuckTick
	stuckTick = d
	return func() { stuckTick = old }
}

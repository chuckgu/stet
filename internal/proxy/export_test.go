package proxy

import "time"

// SetMaxInspect changes how large a message the proxy reads before it stops
// inspecting it, until restore is called. Tests that use it must not run in
// parallel, and must restore it only once every proxy started meanwhile has
// closed (t.Cleanup registered before the proxy starts).
func SetMaxInspect(n int64) (restore func()) {
	old := maxInspect
	maxInspect = n
	return func() { maxInspect = old }
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

package proxy

// SetMaxInspect changes how large a message the proxy reads before it stops
// inspecting it, until restore is called. Tests that use it must not run in
// parallel, and must restore it only once every proxy started meanwhile has
// closed (t.Cleanup registered before the proxy starts).
func SetMaxInspect(n int64) (restore func()) {
	old := maxInspect
	maxInspect = n
	return func() { maxInspect = old }
}

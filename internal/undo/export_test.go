package undo

import "time"

// SetPurgeEvery changes how often a Purger started after it purges, until
// restore is called. Tests that use it must not run in parallel, and call
// restore only after closing every Purger they started.
func SetPurgeEvery(d time.Duration) (restore func()) {
	old := purgeEvery
	purgeEvery = d
	return func() { purgeEvery = old }
}

// SetPurgeTimeout changes the bound on one delete of a purge until restore is
// called, under the terms of SetPurgeEvery.
func SetPurgeTimeout(d time.Duration) (restore func()) {
	old := purgeTimeout
	purgeTimeout = d
	return func() { purgeTimeout = old }
}

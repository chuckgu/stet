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

// SetDeleteTimeout changes the bound on one delete of a purge until restore
// is called, under the terms of SetPurgeEvery.
func SetDeleteTimeout(d time.Duration) (restore func()) {
	old := deleteTimeout
	deleteTimeout = d
	return func() { deleteTimeout = old }
}

// SetCloseGrace changes how long Close waits for a delete in progress until
// restore is called, under the terms of SetPurgeEvery.
func SetCloseGrace(d time.Duration) (restore func()) {
	old := closeGrace
	closeGrace = d
	return func() { closeGrace = old }
}

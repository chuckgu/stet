package store

// MigrateLock is the advisory lock Migrate takes. Tests that change grants on
// Stet's objects take it too, so that they do not race a Migrate rewriting
// those objects' catalog rows (plan ruling P3).
const MigrateLock = migrateLock

// InsertDecisionName is the name Insert prepares its statement under, on
// connections whose mode caches statements. Tests take it to stand for a
// pooler's server on which another client prepared that name.
const InsertDecisionName = insertDecisionName

// MicrosecondsUp is how Insert turns the time since a write arrived into
// microseconds: rounded up, never down.
var MicrosecondsUp = microsecondsUp

package store

// MigrateLock is the advisory lock Migrate takes. Tests that change grants on
// Stet's objects take it too, so that they do not race a Migrate rewriting
// those objects' catalog rows (plan ruling P3).
const MigrateLock = migrateLock

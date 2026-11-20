package gate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/gate"
	"github.com/stetdb/stet/internal/proxy"
)

// A table rewritten by DDL keeps its OID and its key, and every row gets a
// new version: what was remembered of it would call the next write stale
// though no row's content was changed by anyone after it was read. Once the
// catalog is read again the memory of the table is forgotten, for every
// role, and the write is no_read.
func TestRewrittenTableIsForgotten(t *testing.T) {
	defer gate.SetStaleCatalogEvery(50 * time.Millisecond)()
	e := setupStale(t, calm(), staleConfig("enforce"))
	a := e.agent(t, "a")
	e.otherRoleRead(t)
	e.read(t, a, e.table, 1)
	before := e.xminOf(t, 1)
	if got := gate.CheckRead(e.gate, "stet_md5", "", e.table, "1", before); got.Result != decide.ReadFresh {
		t.Fatalf("before the rewrite: %+v, want fresh", got)
	}

	mustExec(t, e.admin, "alter table "+e.table+" alter column company_id type bigint")
	after := e.xminOf(t, 1)
	if after == before {
		t.Fatal("the ALTER did not rewrite the table: the row kept its version")
	}
	waitUntil(t, "the catalog is read again and the table forgotten", func() bool {
		return gate.CheckRead(e.gate, "stet_md5", "", e.table, "1", after).Result == decide.ReadNone
	})
	if got := gate.CheckRead(e.gate, "other", "", e.table, "1", after); got.Result != decide.ReadNone {
		t.Errorf("another role after the rewrite: %+v, want no_read", got)
	}
	if _, err := a.Exec(context.Background(), "update "+e.table+" set title = 'x' where id = 1"); err != nil {
		t.Errorf("the write after the rewrite was refused: %v", err)
	}
	wantRead(t, 0, e.checkOf(t, e.table, "title = 'x'"), decide.ReadNone)
}

// A table dropped and made again is another table under the same name: what
// was remembered of the old one is forgotten once the catalog is read again.
// A table the catalog finds as it was keeps its memory across readings.
func TestRecreatedTableIsForgotten(t *testing.T) {
	defer gate.SetStaleCatalogEvery(50 * time.Millisecond)()
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	notes := e.create(t, "notes", "(id int primary key)")
	later := e.name("later")
	t.Cleanup(func() { e.admin.Exec(context.Background(), "drop table if exists "+later) })
	e.insert(t, deals, "id", "1")
	e.insert(t, notes, "id", "1")
	e.start(t, tableBlock(deals, "id", "stale: {}"), tableBlock(notes, "id", "stale: {}"), tableBlock(later, "id", "stale: {}"))
	agent := proxy.SessionInfo{Database: "stet_test", ID: "s", User: "agent"}
	e.gate.Observe(agent, e.gate.Inspect("select id from "+deals), keyed(e.oid(t, deals), "1"))
	e.gate.Observe(agent, e.gate.Inspect("select id from "+notes), keyed(e.oid(t, notes), "1"))
	e.idle(t)
	e.wantResult(t, "agent", "", deals, "1", e.xmin(t, deals, "id", "1"), decide.ReadFresh)

	// In one transaction, so no reading of the catalog finds it missing.
	mustExec(t, e.admin, "drop table "+deals+"; create table "+deals+" (id int primary key, title text); "+
		"insert into "+deals+" values (1)")
	waitUntil(t, "the catalog is read again and the table forgotten", func() bool {
		return e.check("agent", "", deals, "1", e.xmin(t, deals, "id", "1")).Result == decide.ReadNone
	})
	// The catalog is read at least once more: a table made now gets tracked.
	mustExec(t, e.admin, "create table "+later+" (id int primary key)")
	waitUntil(t, "a table made after the start is tracked", func() bool {
		_, _, ok := e.gate.Tracked(e.oid(t, later))
		return ok
	})
	e.wantResult(t, "agent", "", notes, "1", e.xmin(t, notes, "id", "1"), decide.ReadFresh)
}

// An ordinary table other tables inherit from is not tracked: a lookup by
// key, and the state read of a write, would find its children's rows too. A
// partitioned table is tracked as before.
func TestInheritanceParentIsNotTracked(t *testing.T) {
	e := newStaleEnv(t)
	parent := e.create(t, "parent", "(id int primary key, title text)")
	child := e.name("child")
	mustExec(t, e.admin, "create table "+child+" () inherits ("+parent+")")
	t.Cleanup(func() { e.admin.Exec(context.Background(), "drop table if exists "+child) })
	parted := e.name("parted")
	mustExec(t, e.admin, "create table "+parted+" (id int primary key) partition by range (id)")
	t.Cleanup(func() { e.admin.Exec(context.Background(), "drop table if exists "+parted) })
	mustExec(t, e.admin, "create table "+parted+"_1 partition of "+parted+" for values from (0) to (1000)")
	e.insert(t, parent, "id", "1")
	e.insert(t, child, "id", "1") // the parent's key index does not reach the child
	e.start(t, tableBlock(parent, "id", "stale: {}"), tableBlock(parted, "id", "stale: {}"))

	w := e.warnings(t, parent)
	if len(w) != 1 || !strings.Contains(w[0]["reason"].(string), "inherit") {
		t.Errorf("%s: warnings %v, want one that names inheritance", parent, w)
	}
	if _, _, ok := e.gate.Tracked(e.oid(t, parent)); ok {
		t.Errorf("%s, which has a child table, is tracked", parent)
	}
	if w := e.warnings(t, parted); len(w) != 0 {
		t.Errorf("%s: warnings %v, want none", parted, w)
	}
	if _, _, ok := e.gate.Tracked(e.oid(t, parted)); !ok {
		t.Errorf("the partitioned table %s is not tracked", parted)
	}
}

// A reading of the catalog that fails leaves what was read before, and
// forgets nothing: the table may be as it was. The next reading that
// succeeds finds it changed, and forgets.
func TestFailedCatalogReadingForgetsNothing(t *testing.T) {
	e := newStaleEnv(t)
	deals := e.create(t, "deals", "(id int primary key, title text)")
	e.insert(t, deals, "id", "1")
	e.start(t, tableBlock(deals, "id", "stale: {}"))
	agent := proxy.SessionInfo{Database: "stet_test", ID: "s", User: "agent"}
	e.gate.Observe(agent, e.gate.Inspect("select id from "+deals), keyed(e.oid(t, deals), "1"))
	e.idle(t)
	read := e.xmin(t, deals, "id", "1")

	gate.ReadCatalog(e.gate, false) // unchanged: nothing is forgotten
	e.wantResult(t, "agent", "", deals, "1", read, decide.ReadFresh)

	mustExec(t, e.admin, "alter table "+deals+" alter column title type varchar(20)") // rewrites the table
	gate.ReadCatalog(e.gate, true)
	e.wantResult(t, "agent", "", deals, "1", read, decide.ReadFresh)
	if w := e.warnings(t, deals); len(w) != 1 || !strings.Contains(w[0]["msg"].(string), "could not read") {
		t.Errorf("warnings after the failed reading: %v, want the one that says it failed", w)
	}
	gate.ReadCatalog(e.gate, false)
	e.wantResult(t, "agent", "", deals, "1", e.xmin(t, deals, "id", "1"), decide.ReadNone)
}

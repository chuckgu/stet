package main

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests send an agent's writes through a running Stet to a table that
// capture watches, and read in stet.history which decision each change is
// linked to (spec-v0.2-undo 5, 6). Each runs in a database of its own, where
// stet.capture_settings holds whatever that Stet leaves there.

// captureOn installs the four capture triggers of spec-v0.2-undo 4 on table,
// as its owner (owner's role) does with what stet undo-sql prints: key is the
// table's key in the configuration, pk its key column. The transition tables
// are named stet_old and stet_new (plan ruling U11).
func captureOn(t *testing.T, owner *pgx.Conn, table, key, pk string) {
	t.Helper()
	call := fmt.Sprintf("execute function stet.capture('%s', '%s')", key, pk)
	for _, sql := range []string{
		"create trigger stet_capture_ins after insert on " + table + " referencing new table as stet_new for each statement " + call,
		"create trigger stet_capture_upd after update on " + table + " referencing old table as stet_old new table as stet_new for each statement " + call,
		"create trigger stet_capture_del after delete on " + table + " referencing old table as stet_old for each statement " + call,
		"create trigger stet_capture_trunc after truncate on " + table + " for each statement " + call,
	} {
		mustExec(t, owner, sql)
	}
}

// capturedTable is agentTable with the stet schema in place and capture
// installed on the table, under its own name as its key. It returns Stet's
// pool too.
func capturedTable(t *testing.T, pg string, n int) (db, table string, admin *pgx.Conn, pool *pgxpool.Pool) {
	t.Helper()
	db, table, admin = agentTable(t, pg, n)
	pool = stetPoolIn(t, pg, db)
	captureOn(t, admin, table, table, "id")
	return db, table, admin, pool
}

// historyShadowYAML is a shadow config over table %[3]s of database %[2]s,
// with no model: no model is called and no key is read.
const historyShadowYAML = `mode: shadow
listen: 127.0.0.1:0
upstream: postgres://stet_scram:scram-pw@%[1]s/%[2]s?sslmode=disable
model: {adapter: none, on_failure: pass}
tables:
  %[3]s: {pk: id, send_fields: [name]}
`

// shadowOver starts Stet in shadow mode over table and connects to it as the
// agent, stet_md5.
func shadowOver(t *testing.T, pg, db, table string) (*running, *pgx.Conn) {
	t.Helper()
	r := start(t, parseConfig(t, historyShadowYAML, pg, db, table))
	return r, connectTo(t, r.addr, db, "stet_md5", "md5-pw")
}

// decisionRow is a decision as stet.decisions holds it.
type decisionRow struct {
	ID        string
	Statement string
	TargetPK  *string
	Executed  bool
	Verdict   string
	Reasons   []string
	CreatedAt time.Time
}

// decisionsOn waits until n decisions on table are recorded, which happens in
// the background once each write has ended, and returns them oldest first.
func decisionsOn(t *testing.T, pool *pgxpool.Pool, table string, n int, log fmt.Stringer) []decisionRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := pool.Query(context.Background(), `
			select id::text, statement, target_pk #>> '{}', executed, verdict, reasons, created_at
			from stet.decisions where target_table = $1 order by created_at, id`, table)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[decisionRow])
		if err != nil {
			t.Fatal(err)
		}
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d decisions on %s recorded, want %d; log:\n%s", len(got), table, n, log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// historyLine is what stet.history shows of a change and its decision.
type historyLine struct {
	SetID       int64
	TargetTable string
	Key         *string
	Op          string
	Query       *string
	AgentRole   string
	LoginRole   string
	HeldWrite   *string
	DecisionID  *string
	Candidates  int32
	Verdict     *string
	Reasons     []string
}

// historyOn is what stet.history shows of the changes to table (its key in
// the configuration), in the order capture recorded them.
func historyOn(t *testing.T, pool *pgxpool.Pool, table string) []historyLine {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select set_id, target_table, key, op, query, agent_role, login_role, held_write::text,
		       decision_id::text, decision_candidates, verdict, reasons
		from stet.history where target_table = $1 order by set_id, change_id`, table)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowToStructByPos[historyLine])
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// lineFor is the one line history shows for key, failing the test unless
// there is exactly one.
func lineFor(t *testing.T, lines []historyLine, key string) historyLine {
	t.Helper()
	var found []historyLine
	for _, l := range lines {
		if l.Key != nil && *l.Key == key {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("stet.history shows %d lines for key %s, want one: %+v", len(found), key, lines)
	}
	return found[0]
}

// wantLinkedTo checks that l shows decision d, alone, with its verdict and
// reasons.
func wantLinkedTo(t *testing.T, what string, l historyLine, d decisionRow) {
	t.Helper()
	if orNull(l.DecisionID) != d.ID || l.Candidates != 1 || orNull(l.Verdict) != d.Verdict || !slices.Equal(l.Reasons, d.Reasons) {
		t.Errorf("%s: shows decision %s (%d candidates), verdict %s, reasons %v; want %s (1), %s, %v",
			what, orNull(l.DecisionID), l.Candidates, orNull(l.Verdict), l.Reasons, d.ID, d.Verdict, d.Reasons)
	}
}

// decisionFor is the one decision on key, failing the test unless there is
// exactly one.
func decisionFor(t *testing.T, ds []decisionRow, key string) decisionRow {
	t.Helper()
	var found []decisionRow
	for _, d := range ds {
		if d.TargetPK != nil && *d.TargetPK == key {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d decisions on key %s, want one: %+v", len(found), key, ds)
	}
	return found[0]
}

// A change made by a write that went through Stet is shown with the decision
// that judged that write, in shadow mode: a write over the simple protocol,
// one over the extended protocol with bind values, and one inside an
// explicit transaction.
func TestChangeIsLinkedToItsDecision(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db, table, _, pool := capturedTable(t, pg, 3)
	r, agent := shadowOver(t, pg, db, table)

	mustExec(t, agent, "update "+table+" set name = 'simple' where id = 1")
	mustExec(t, agent, "update "+table+" set name = $1 where id = $2", "extended", 2)
	tx, err := agent.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "update "+table+" set name = 'in a transaction' where id = 3"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	ds := decisionsOn(t, pool, table, 3, r.log)
	lines := historyOn(t, pool, table)
	for key, what := range map[string]string{"1": "simple protocol", "2": "extended protocol", "3": "explicit transaction"} {
		d := decisionFor(t, ds, key)
		if !d.Executed {
			t.Errorf("%s: the decision says the write was not executed", what)
		}
		l := lineFor(t, lines, key)
		wantLinkedTo(t, what, l, d)
		if l.AgentRole != "stet_md5" || l.HeldWrite != nil {
			t.Errorf("%s: agent_role %s, held_write %s; want stet_md5, NULL", what, l.AgentRole, orNull(l.HeldWrite))
		}
	}
}

// The rows a cascade deletes in a child table are recorded with the parent's
// statement and the time the server took it, so their set is linked to the
// decision on the parent's delete, as the parent's is.
func TestCascadedChildIsLinkedToTheParentsDecision(t *testing.T) {
	pg := testPG(t)
	db, table, admin, pool := capturedTable(t, pg, 2)
	child := table + "_notes"
	mustExec(t, admin, "create table "+child+" (id int primary key, parent int references "+table+" on delete cascade, note text)")
	mustExec(t, admin, "insert into "+child+" values (10, 1, 'a'), (11, 1, 'b'), (12, 2, 'c')")
	captureOn(t, admin, child, child, "id")
	r, agent := shadowOver(t, pg, db, table)

	mustExec(t, agent, "delete from "+table+" where id = 1")
	d := decisionsOn(t, pool, table, 1, r.log)[0]

	parent := historyOn(t, pool, table)
	children := historyOn(t, pool, child)
	if len(parent) != 1 || len(children) != 2 {
		t.Fatalf("stet.history shows %d parent and %d child lines, want 1 and 2: %+v %+v", len(parent), len(children), parent, children)
	}
	wantLinkedTo(t, "the parent", parent[0], d)
	for _, l := range children {
		if l.Op != "delete" || orNull(l.Query) != d.Statement {
			t.Errorf("child %s: op %s, query %s; want delete, the parent's statement", orNull(l.Key), l.Op, orNull(l.Query))
		}
		wantLinkedTo(t, "child "+orNull(l.Key), l, d)
	}
}

// A write through a function is not judged (Stet sees a SELECT), so its
// change is shown with no decision: no candidate, though a decision on the
// same backend was recorded a moment before.
func TestWriteThroughAFunctionIsNotLinked(t *testing.T) {
	pg := testPG(t)
	db, table, admin, pool := capturedTable(t, pg, 2)
	mustExec(t, admin, "create function "+table+"_rename(i int, n text) returns void language sql as $$ update "+table+" set name = n where id = i $$")
	r, agent := shadowOver(t, pg, db, table)

	mustExec(t, agent, "update "+table+" set name = 'direct' where id = 1")
	mustExec(t, agent, "select "+table+"_rename(2, 'through a function')")
	ds := decisionsOn(t, pool, table, 1, r.log)

	lines := historyOn(t, pool, table)
	wantLinkedTo(t, "the direct write", lineFor(t, lines, "1"), decisionFor(t, ds, "1"))
	l := lineFor(t, lines, "2")
	if l.DecisionID != nil || l.Candidates != 0 || l.Verdict != nil {
		t.Errorf("the write through a function shows decision %s with %d candidates, verdict %s; want none, 0",
			orNull(l.DecisionID), l.Candidates, orNull(l.Verdict))
	}
	if len(ds) != 1 {
		t.Errorf("%d decisions recorded, want 1: the write through a function is not judged", len(ds))
	}
}

// Two writes of the same text under one Sync (a pgx batch) to different rows
// are each linked to their own decision: their windows overlap, and the
// decisions' keys tell them apart. Two to the same row cannot be told apart
// by key: each is linked to the decision on it, in order, or shows both as
// candidates. Neither is linked to a decision on another row.
func TestBatchedWritesLinkToTheirOwnDecisions(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db, table, _, pool := capturedTable(t, pg, 3)
	r, agent := shadowOver(t, pg, db, table)
	write := "update " + table + " set name = $1 where id = $2"

	b := &pgx.Batch{}
	b.Queue(write, "batch one", 1)
	b.Queue(write, "batch two", 2)
	if err := agent.SendBatch(ctx, b).Close(); err != nil {
		t.Fatal(err)
	}
	ds := decisionsOn(t, pool, table, 2, r.log)
	lines := historyOn(t, pool, table)
	for _, key := range []string{"1", "2"} {
		wantLinkedTo(t, "row "+key, lineFor(t, lines, key), decisionFor(t, ds, key))
	}

	b = &pgx.Batch{}
	b.Queue(write, "first", 3)
	b.Queue(write, "second", 3)
	if err := agent.SendBatch(ctx, b).Close(); err != nil {
		t.Fatal(err)
	}
	var same []decisionRow
	for _, d := range decisionsOn(t, pool, table, 4, r.log) {
		if d.TargetPK != nil && *d.TargetPK == "3" {
			same = append(same, d) // oldest first
		}
	}
	var sets []historyLine
	for _, l := range historyOn(t, pool, table) {
		if l.Key != nil && *l.Key == "3" {
			sets = append(sets, l) // in the order capture recorded them
		}
	}
	if len(same) != 2 || len(sets) != 2 {
		t.Fatalf("%d decisions and %d changes on row 3, want 2 and 2", len(same), len(sets))
	}
	for i, l := range sets {
		switch {
		case l.DecisionID == nil:
			if l.Candidates != 2 {
				t.Errorf("change %d of row 3 is not linked and shows %d candidates, want 2", i+1, l.Candidates)
			}
			t.Logf("change %d of row 3: not linked, %d candidates", i+1, l.Candidates)
		case *l.DecisionID != same[i].ID || l.Candidates != 1:
			t.Errorf("change %d of row 3 is linked to %s (%d candidates); want the decision on write %d, %s",
				i+1, *l.DecisionID, l.Candidates, i+1, same[i].ID)
		default:
			t.Logf("change %d of row 3: linked to the decision on write %d", i+1, i+1)
		}
	}
}

// An approved held write, run by Stet's executor as the agent, is recorded
// as the agent's with its held write, and linked through it to the decision
// that held it, which says the write was not executed then.
func TestApprovedWriteIsLinkedThroughItsHeldWrite(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db, table, admin, pool := capturedTable(t, pg, 1)
	r := start(t, holdingConfig(t, pg, db, table))
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")

	_, err := agent.Exec(ctx, "update "+table+" set name = 'held' where id = 1")
	id := heldWriteOf(t, err, r.log)
	waitStatus(t, admin, id, "pending", 3*time.Second, r.log)
	if _, err := admin.Exec(ctx, "select stet.approve($1)", id); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, admin, id, "applied", 5*time.Second, r.log)

	var decisionID string
	if err := admin.QueryRow(ctx, "select decision_id::text from stet.held_writes where id = $1", id).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	d := decisionsOn(t, pool, table, 1, r.log)[0]
	if d.ID != decisionID || d.Executed || d.Verdict != "hold" {
		t.Fatalf("the decision is %+v; want %s, the held write's, not executed, hold", d, decisionID)
	}
	lines := historyOn(t, pool, table)
	if len(lines) != 1 {
		t.Fatalf("stet.history shows %d lines, want 1: %+v", len(lines), lines)
	}
	l := lines[0]
	if l.AgentRole != "stet_md5" || l.LoginRole != "stet_scram" || orNull(l.HeldWrite) != id {
		t.Errorf("agent_role %s, login_role %s, held_write %s; want stet_md5, stet_scram, %s",
			l.AgentRole, l.LoginRole, orNull(l.HeldWrite), id)
	}
	wantLinkedTo(t, "the approved write", l, d)
}

// What a decision records as its statement is, character for character, what
// capture records as the set's query, current_query(): with comments,
// runs of blanks and line breaks, and a trailing semicolon, over the simple
// protocol and the extended one.
func TestStatementTextMatchesWhatTheServerSaw(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db, table, _, pool := capturedTable(t, pg, 4)
	r, agent := shadowOver(t, pg, db, table)

	writes := []struct {
		key  string
		sql  string
		args []any
	}{
		{"1", "/* agent: enricher */ update " + table + "\n   set name = 'commented'  -- a note\n where id = 1", nil},
		{"2", "\n\tupdate " + table + "   set name = 'spaced'\r\n\twhere id = 2 ;  \n", nil},
		{"3", "/* agent: enricher */ update " + table + "\n   set name = $1  -- a note\n where id = $2", []any{"commented", 3}},
		{"4", "\n\tupdate " + table + "   set name = $1\r\n\twhere id = $2 ;  \n", []any{"spaced", 4}},
	}
	for _, w := range writes {
		if _, err := agent.Exec(ctx, w.sql, w.args...); err != nil {
			t.Fatalf("%q: %v", w.sql, err)
		}
	}
	ds := decisionsOn(t, pool, table, len(writes), r.log)
	lines := historyOn(t, pool, table)
	for _, w := range writes {
		protocol := "simple protocol"
		if w.args != nil {
			protocol = "extended protocol"
		}
		d := decisionFor(t, ds, w.key)
		l := lineFor(t, lines, w.key)
		if d.Statement != w.sql || orNull(l.Query) != w.sql {
			t.Errorf("%s: sent %q; the decision records %q and capture %q", protocol, w.sql, d.Statement, orNull(l.Query))
		}
		wantLinkedTo(t, protocol+" "+fmt.Sprintf("%q", w.sql), l, d)
	}
}

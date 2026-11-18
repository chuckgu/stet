package store_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/store"
)

// These tests read stet.history (spec-v0.2-undo 6) and the rule that links a
// change set to the decision that judged its write (spec 5, "잇는 규칙"). Each
// runs in a database of its own (captureDB). Decisions are written by hand,
// with the backend pid and window the gate would have recorded, so that each
// bound of the rule can be set exactly; so are most change sets.

// handDecision is a decision written by hand: one on a write of Statement,
// on backend PID, between SentAfter and DoneBefore.
type handDecision struct {
	PID        int32
	SentAfter  time.Time
	DoneBefore time.Time
	Statement  string
	Executed   bool
	Table      string
	PK         *string // nil: no target_pk
	Verdict    string
	Reasons    []string
}

// judged is a handDecision on an executed write that was committed, for
// "deals" and no key; a test sets the rest.
func judged(pid int32, sentAfter, doneBefore time.Time, statement string) handDecision {
	return handDecision{PID: pid, SentAfter: sentAfter, DoneBefore: doneBefore, Statement: statement,
		Executed: true, Table: "deals", Verdict: "commit", Reasons: []string{}}
}

// recordDecision stores d in stet.decisions and returns its id.
func recordDecision(t *testing.T, pool *pgxpool.Pool, d handDecision) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		insert into stet.decisions (mode, agent_id, statement, statement_fp, op, target_table, target_pk,
		                            model, model_version, policy_version, verdict, reasons, executed,
		                            backend_pid, sent_after, done_before)
		values ('shadow', 'enricher', $1, 'fp', 'update', $2, to_jsonb($3::text),
		        'none', 'none', 'p', $4, $5, $6, $7, $8, $9)
		returning id::text`,
		d.Statement, d.Table, d.PK, d.Verdict, d.Reasons, d.Executed, d.PID, d.SentAfter, d.DoneBefore).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// handSet is a change set written by hand: what capture would record of a
// write of Query to Table on backend PID that the server took at StmtAt.
type handSet struct {
	PID       int32
	StmtAt    time.Time
	Query     string
	Table     string
	HeldWrite *string
}

// recordSet stores s in stet.change_sets, an update, with one line in
// stet.changes for each of keys, and returns its id.
func recordSet(t *testing.T, pool *pgxpool.Pool, s handSet, keys ...string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `
		insert into stet.change_sets (at, stmt_at, txid, pid, login_role, agent_role, app, held_write,
		                              target_table, key_column, relid, op, query, rows)
		values ($1::timestamptz + interval '1 millisecond', $1, 1, $2, 'stet_md5', 'stet_md5', 'enricher', $3::uuid,
		        $4, 'id', 0, 'update', $5, $6)
		returning id`, s.StmtAt, s.PID, s.HeldWrite, s.Table, s.Query, len(keys)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if _, err := pool.Exec(ctx, `
			insert into stet.changes (set_id, key, before, after)
			values ($1, $2::text, jsonb_build_object('id', $2::text, 'name', 'old'), jsonb_build_object('id', $2::text, 'name', 'new'))`,
			id, k); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// historyLine is what stet.history shows of a change and its decision.
type historyLine struct {
	ChangeID   *int64
	SetID      int64
	Key        *string
	DecisionID *string
	Candidates int32
	Verdict    *string
	Reasons    []string
}

// historyOf is what stet.history shows of change set setID, read on conn, by
// change.
func historyOf(t *testing.T, conn interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, setID int64) []historyLine {
	t.Helper()
	rows, err := conn.Query(context.Background(), `
		select change_id, set_id, key, decision_id::text, decision_candidates, verdict, reasons
		from stet.history where set_id = $1 order by change_id`, setID)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowToStructByPos[historyLine])
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// wantLinked checks that every line of set setID shows decision id (or none,
// for "") and candidates.
func wantLinked(t *testing.T, pool *pgxpool.Pool, what string, setID int64, id string, candidates int32) {
	t.Helper()
	lines := historyOf(t, pool, setID)
	if len(lines) == 0 {
		t.Errorf("%s: stet.history shows nothing of set %d", what, setID)
	}
	for _, l := range lines {
		if deref(l.DecisionID) != idOrNull(id) || l.Candidates != candidates {
			t.Errorf("%s: key %v shows decision_id %v with %d candidates; want %v with %d",
				what, deref(l.Key), deref(l.DecisionID), l.Candidates, idOrNull(id), candidates)
		}
	}
}

// idOrNull is id as deref shows it: "NULL" for "".
func idOrNull(id string) any {
	if id == "" {
		return "NULL"
	}
	return id
}

// The view's columns are spec 6's, in its order and with the types a later
// CREATE OR REPLACE VIEW must keep: it may only add columns at the end.
func TestHistoryHasTheColumnsOfSpec6(t *testing.T) {
	pool, _ := captureDB(t)
	rows, err := pool.Query(context.Background(), `
		select attname::text || ' ' || format_type(atttypid, atttypmod)
		from pg_attribute where attrelid = 'stet.history'::regclass and attnum > 0 and not attisdropped
		order by attnum`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"change_id bigint", "set_id bigint", "at timestamp with time zone",
		"agent_role text", "login_role text", "app text",
		"target_table text", "op text", "key text", "before jsonb", "after jsonb",
		"rows bigint", "skipped text", "detail text", "query text",
		"held_write uuid",
		"decision_id uuid", "decision_candidates integer",
		"verdict text", "reasons text[]",
		"undo_id bigint", "undone_at timestamp with time zone",
	}
	if !slices.Equal(got, want) {
		t.Errorf("stet.history has columns\n%v\nwant\n%v", got, want)
	}
}

// A change an agent's write made, as capture recorded it, is shown with the
// one decision whose backend pid, window and statement match it: its id,
// verdict and reasons, with one candidate. A decision with the same statement
// on another backend is not one.
func TestHistoryLinksASetToItsOnlyDecision(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	dealsTable(t, owner)
	agent := agentIn(t, db)
	mustExec(t, agent, `set application_name = 'enricher'`)
	const write = `update public.deals set name = 'Acme Corp' where id = 1`
	mustExec(t, agent, write)
	changes := changesOf(t, pool, "deals")
	if len(changes) != 1 {
		t.Fatalf("%d lines recorded, want 1", len(changes))
	}
	c := changes[0]

	d := judged(c.PID, c.StmtAt.Add(-2*time.Millisecond), c.StmtAt.Add(3*time.Millisecond), *c.Query)
	d.PK, d.Verdict, d.Reasons = ptr("1"), "hold", []string{"model:conflict", "policy:1"}
	id := recordDecision(t, pool, d)
	elsewhere := d
	elsewhere.PID = c.PID + 1
	recordDecision(t, pool, elsewhere)

	type shown struct {
		ChangeID                          *int64
		SetID                             int64
		At                                time.Time
		AgentRole, LoginRole              string
		App                               *string
		TargetTable, Op                   string
		Key                               *string
		Before, After                     map[string]any
		Rows                              *int64
		Skipped, Detail, Query, HeldWrite *string
		DecisionID                        *string
		Candidates                        int32
		Verdict                           *string
		Reasons                           []string
		UndoID                            *int64
		UndoneAt                          *time.Time
	}
	rows, err := pool.Query(ctx, `
		select change_id, set_id, at, agent_role, login_role, app, target_table, op, key, before, after,
		       rows, skipped, detail, query, held_write::text, decision_id::text, decision_candidates,
		       verdict, reasons, undo_id, undone_at
		from stet.history`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[shown])
	if err != nil {
		t.Fatal(err)
	}
	want := shown{
		ChangeID: c.ChangeID, SetID: c.SetID, At: c.At, AgentRole: "stet_md5", LoginRole: "stet_md5", App: ptr("enricher"),
		TargetTable: "deals", Op: "update", Key: ptr("1"),
		Before: deal("1", "Acme", "10"), After: deal("1", "Acme Corp", "10"),
		Rows: ptr(int64(1)), Query: ptr(write),
		DecisionID: &id, Candidates: 1, Verdict: ptr("hold"), Reasons: []string{"model:conflict", "policy:1"},
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("stet.history shows\n%+v\nwant\n%+v", got, want)
	}
}

// The rule's bounds (spec 5): the server took the statement from 10ms before
// sent_after to done_before, both included, on the decision's backend; the
// write was executed; and its statement is the set's query, character for
// character. Outside any of them the set is shown with no candidate.
func TestHistoryLinkBounds(t *testing.T) {
	pool, _ := captureDB(t)
	sent := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	done := sent.Add(50 * time.Millisecond)
	const write = "update public.deals set name = 'x' where id = 1"
	for i, c := range []struct {
		name  string
		set   func(*handSet)
		dec   func(*handDecision)
		links bool
	}{
		{"5ms before sent_after", func(s *handSet) { s.StmtAt = sent.Add(-5 * time.Millisecond) }, nil, true},
		{"10ms before sent_after", func(s *handSet) { s.StmtAt = sent.Add(-10 * time.Millisecond) }, nil, true},
		{"at done_before", func(s *handSet) { s.StmtAt = done }, nil, true},
		{"inside the window", func(s *handSet) { s.StmtAt = sent.Add(20 * time.Millisecond) }, nil, true},
		{"earlier than 10ms before sent_after", func(s *handSet) {
			s.StmtAt = sent.Add(-10*time.Millisecond - time.Microsecond)
		}, nil, false},
		{"later than done_before", func(s *handSet) { s.StmtAt = done.Add(time.Microsecond) }, nil, false},
		{"on another backend", func(s *handSet) { s.PID += 100000 }, nil, false},
		{"another statement by one space", func(s *handSet) { s.Query += " " }, nil, false},
		{"another statement by case", func(s *handSet) { s.Query = "UPDATE public.deals set name = 'x' where id = 1" }, nil, false},
		{"a write that was not executed", nil, func(d *handDecision) { d.Executed = false }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			pid := int32(1000 + i) // each case on a backend of its own
			s := handSet{PID: pid, StmtAt: sent.Add(time.Millisecond), Query: write, Table: "deals"}
			if c.set != nil {
				c.set(&s)
			}
			d := judged(pid, sent, done, write)
			d.PK = ptr("1")
			if c.dec != nil {
				c.dec(&d)
			}
			id := recordDecision(t, pool, d)
			setID := recordSet(t, pool, s, "1")
			if c.links {
				wantLinked(t, pool, c.name, setID, id, 1)
			} else {
				wantLinked(t, pool, c.name, setID, "", 0)
			}
		})
	}
}

// When several decisions match a set, those with a target_pk on the set's
// table stay only if that key is among the set's keys; those without a key,
// or on another table, stay as they are. A set is linked only when one
// stays; otherwise it shows how many did (spec 5).
func TestHistoryNarrowsByTargetKey(t *testing.T) {
	pool, _ := captureDB(t)
	at := time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)
	const write = "update public.deals set name = $1 where id = $2"
	pid := int32(2000)
	// Each case on a backend of its own; both decisions' windows hold both
	// sets' statements.
	both := func() (time.Time, time.Time) { return at, at.Add(20 * time.Millisecond) }
	next := func() int32 { pid++; return pid }

	t.Run("two decisions on different rows: each set links to its own", func(t *testing.T) {
		p := next()
		from, to := both()
		one, two := judged(p, from, to, write), judged(p, from, to, write)
		one.PK, two.PK = ptr("1"), ptr("2")
		oneID, twoID := recordDecision(t, pool, one), recordDecision(t, pool, two)
		set1 := recordSet(t, pool, handSet{PID: p, StmtAt: at.Add(time.Millisecond), Query: write, Table: "deals"}, "1")
		set2 := recordSet(t, pool, handSet{PID: p, StmtAt: at.Add(2 * time.Millisecond), Query: write, Table: "deals"}, "2")
		wantLinked(t, pool, "row 1", set1, oneID, 1)
		wantLinked(t, pool, "row 2", set2, twoID, 1)
	})
	t.Run("two decisions on the same row: neither", func(t *testing.T) {
		p := next()
		from, to := both()
		one, two := judged(p, from, to, write), judged(p, from, to, write)
		one.PK, two.PK = ptr("3"), ptr("3")
		recordDecision(t, pool, one)
		recordDecision(t, pool, two)
		set1 := recordSet(t, pool, handSet{PID: p, StmtAt: at.Add(time.Millisecond), Query: write, Table: "deals"}, "3")
		set2 := recordSet(t, pool, handSet{PID: p, StmtAt: at.Add(2 * time.Millisecond), Query: write, Table: "deals"}, "3")
		wantLinked(t, pool, "first", set1, "", 2)
		wantLinked(t, pool, "second", set2, "", 2)
	})
	t.Run("two decisions without a target_pk: neither", func(t *testing.T) {
		p := next()
		from, to := both()
		recordDecision(t, pool, judged(p, from, to, write))
		recordDecision(t, pool, judged(p, from, to, write))
		set := recordSet(t, pool, handSet{PID: p, StmtAt: at.Add(time.Millisecond), Query: write, Table: "deals"}, "4", "5")
		wantLinked(t, pool, "keys 4 and 5", set, "", 2)
	})
	t.Run("a decision without a target_pk stays beside one on the set's key", func(t *testing.T) {
		p := next()
		from, to := both()
		keyed := judged(p, from, to, write)
		keyed.PK = ptr("6")
		recordDecision(t, pool, keyed)
		recordDecision(t, pool, judged(p, from, to, write))
		set := recordSet(t, pool, handSet{PID: p, StmtAt: at.Add(time.Millisecond), Query: write, Table: "deals"}, "6")
		wantLinked(t, pool, "key 6", set, "", 2)
	})
	t.Run("a decision on another table stays, whatever its key", func(t *testing.T) {
		p := next()
		from, to := both()
		here, there := judged(p, from, to, write), judged(p, from, to, write)
		here.PK, there.PK, there.Table = ptr("7"), ptr("99"), "accounts"
		recordDecision(t, pool, here)
		recordDecision(t, pool, there)
		set := recordSet(t, pool, handSet{PID: p, StmtAt: at.Add(time.Millisecond), Query: write, Table: "deals"}, "7")
		wantLinked(t, pool, "key 7", set, "", 2)
	})
	t.Run("a decision on another key of the set's table goes", func(t *testing.T) {
		p := next()
		from, to := both()
		here, other := judged(p, from, to, write), judged(p, from, to, write)
		here.PK, other.PK = ptr("8"), ptr("80")
		hereID := recordDecision(t, pool, here)
		recordDecision(t, pool, other)
		set := recordSet(t, pool, handSet{PID: p, StmtAt: at.Add(time.Millisecond), Query: write, Table: "deals"}, "8", "9")
		wantLinked(t, pool, "keys 8 and 9", set, hereID, 1)
	})
	t.Run("one decision is linked without its key being looked at", func(t *testing.T) {
		p := next()
		from, to := both()
		only := judged(p, from, to, write)
		only.PK = ptr("13")
		id := recordDecision(t, pool, only)
		set := recordSet(t, pool, handSet{PID: p, StmtAt: at.Add(time.Millisecond), Query: write, Table: "deals"}, "14")
		wantLinked(t, pool, "key 14", set, id, 1)
	})
	t.Run("when every decision goes, none is left", func(t *testing.T) {
		p := next()
		from, to := both()
		one, two := judged(p, from, to, write), judged(p, from, to, write)
		one.PK, two.PK = ptr("10"), ptr("11")
		recordDecision(t, pool, one)
		recordDecision(t, pool, two)
		set := recordSet(t, pool, handSet{PID: p, StmtAt: at.Add(time.Millisecond), Query: write, Table: "deals"}, "12")
		wantLinked(t, pool, "key 12", set, "", 0)
	})
}

// A write the executor of approved held writes ran links through its held
// write to the decision that held it, not by the rule: also when a decision
// matches its backend, window and statement. A held write that names no
// stored row, or one whose decision was not recorded, links to nothing.
func TestHistoryLinksAnApprovedWriteThroughItsHeldWrite(t *testing.T) {
	pool, _ := captureDB(t)
	ctx := context.Background()
	s := store.New(pool)
	at := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	const write = "update public.deals set name = 'approved' where id = 1"

	held := judged(3000, at.Add(-time.Hour), at.Add(-time.Hour+time.Millisecond), write)
	held.PK, held.Executed, held.Verdict, held.Reasons = ptr("1"), false, "hold", []string{"model:conflict"}
	heldID := recordDecision(t, pool, held)
	h := sampleHeld()
	h.DecisionID, h.Statement, h.TargetTable = heldID, write, "deals"
	if err := s.InsertHeldWrite(ctx, h); err != nil {
		t.Fatal(err)
	}
	// What the rule would pick, on the executor's backend.
	recordDecision(t, pool, judged(3001, at, at.Add(10*time.Millisecond), write))

	set := recordSet(t, pool, handSet{PID: 3001, StmtAt: at.Add(time.Millisecond), Query: write, Table: "deals", HeldWrite: &h.ID}, "1")
	wantLinked(t, pool, "approved write", set, heldID, 1)
	lines := historyOf(t, pool, set)
	if len(lines) != 1 || deref(lines[0].Verdict) != "hold" || !slices.Equal(lines[0].Reasons, []string{"model:conflict"}) {
		t.Errorf("approved write shows %+v; want the held decision's verdict hold and reasons [model:conflict]", lines)
	}

	orphan := sampleHeld()
	orphan.Statement, orphan.TargetTable = write, "deals"
	if err := s.InsertHeldWrite(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	for name, heldWrite := range map[string]string{"an unknown held write": store.NewID(), "a held write without its decision": orphan.ID} {
		set := recordSet(t, pool, handSet{PID: 3001, StmtAt: at.Add(2 * time.Millisecond), Query: write, Table: "deals", HeldWrite: &heldWrite}, "1")
		wantLinked(t, pool, name, set, "", 0)
	}
}

// A set capture recorded no values for (too many rows, a TRUNCATE, a failed
// capture) shows as one line with no key and no change, saying why.
func TestHistoryShowsSkippedSets(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	dealsTable(t, owner)
	mustExec(t, pool, noBadNames)
	setCapture(t, pool, nil, 2)
	agent := agentIn(t, db)
	writes := []struct {
		sql, op, skipped string
		rows             *int64
		detail           bool
	}{
		{`update public.deals set amount = amount + 1`, "update", "too_many_rows", ptr(int64(3)), false},
		{`update public.deals set name = 'bad' where id = 1`, "update", "capture_failed", ptr(int64(1)), true},
		{`truncate public.deals`, "truncate", "truncate", nil, false},
	}
	for _, w := range writes {
		mustExec(t, agent, w.sql)
	}

	type shown struct {
		ChangeID      *int64
		Key           *string
		Before, After map[string]any
		Op            string
		Rows          *int64
		Skipped       *string
		Detail        *string
		Candidates    int32
	}
	for _, w := range writes {
		rows, err := pool.Query(ctx, `
			select change_id, key, before, after, op, rows, skipped, detail, decision_candidates
			from stet.history where query = $1`, w.sql)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[shown])
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("%s: %d lines, want one", w.sql, len(got))
			continue
		}
		g := got[0]
		if g.ChangeID != nil || g.Key != nil || g.Before != nil || g.After != nil || g.Op != w.op ||
			deref(g.Rows) != deref(w.rows) || deref(g.Skipped) != w.skipped || (g.Detail != nil) != w.detail || g.Candidates != 0 {
			t.Errorf("%s: change_id %v, key %v, before %v, after %v, op %s, rows %v, skipped %v, detail %v, candidates %d; "+
				"want no change, no key, no images, %s, %v, %s, detail %v, 0",
				w.sql, deref(g.ChangeID), deref(g.Key), g.Before, g.After, g.Op, deref(g.Rows), deref(g.Skipped), deref(g.Detail),
				g.Candidates, w.op, deref(w.rows), w.skipped, w.detail)
		}
	}
}

// The view is read with its owner's rights (spec 6): a reviewer with USAGE on
// the schema and SELECT on the view alone reads it, and the decision it links
// to, without any right on the tables behind it. A role without those
// grants, the agent among them, is refused (42501).
func TestHistoryIsReadWithTheOwnersRights(t *testing.T) {
	pool, db := captureDB(t)
	at := time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)
	const write = "update public.deals set name = 'x' where id = 1"
	d := judged(4000, at, at.Add(5*time.Millisecond), write)
	d.Verdict, d.Reasons = "hold", []string{"policy:1"}
	id := recordDecision(t, pool, d)
	set := recordSet(t, pool, handSet{PID: 4000, StmtAt: at.Add(time.Millisecond), Query: write, Table: "deals"}, "1")

	reviewer := loginRole(t, "reviewer")
	grant(t, db, reviewer, "grant usage on schema stet to "+reviewer, "grant select on stet.history to "+reviewer)
	rv := connectAs(t, db, reviewer)
	lines := historyOf(t, rv, set)
	if len(lines) != 1 || deref(lines[0].DecisionID) != id || deref(lines[0].Verdict) != "hold" {
		t.Errorf("the reviewer reads %+v, want the change linked to %s with verdict hold", lines, id)
	}
	for _, table := range []string{"stet.change_sets", "stet.changes", "stet.decisions", "stet.held_writes"} {
		_, err := rv.Exec(context.Background(), "select from "+table+" limit 1")
		want42501(t, err, "the reviewer reading "+table)
	}

	usageOnly := loginRole(t, "usage")
	grant(t, db, usageOnly, "grant usage on schema stet to "+usageOnly)
	for name, conn := range map[string]*pgx.Conn{
		"a role with no grant":            connectAs(t, db, loginRole(t, "outsider")),
		"a role with USAGE on the schema": connectAs(t, db, usageOnly),
		"the agent":                       agentIn(t, db),
	} {
		_, err := conn.Exec(context.Background(), "select * from stet.history")
		want42501(t, err, fmt.Sprintf("%s reading stet.history", name))
	}
}

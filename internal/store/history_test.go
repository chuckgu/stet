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

// recordedSet is a change set a test wrote, and how many keys it holds.
type recordedSet struct {
	ID   int64
	Keys int
}

// recordSet stores s in stet.change_sets, an update, with one line in
// stet.changes for each of keys.
func recordSet(t *testing.T, pool *pgxpool.Pool, s handSet, keys ...string) recordedSet {
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
	return recordedSet{ID: id, Keys: len(keys)}
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

// wantLinked checks that set shows one line per key (one line when it has
// none), each with decision id (or none, for "") and candidates.
func wantLinked(t *testing.T, pool *pgxpool.Pool, what string, set recordedSet, id string, candidates int32) {
	t.Helper()
	lines := historyOf(t, pool, set.ID)
	if want := max(set.Keys, 1); len(lines) != want {
		t.Errorf("%s: stet.history shows %d lines of set %d, want %d", what, len(lines), set.ID, want)
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

// The view looks candidates up through decisions_window_idx, whose keys hold
// both ends of a decision's window. A database migrated by the schema.sql of
// the step before has the two-key decisions_backend_idx in its place, which
// CREATE INDEX IF NOT EXISTS would not have replaced under the same name: it
// is dropped, and the new index made.
func TestMigrateReplacesTheBackendIndex(t *testing.T) {
	pool := newPool(t, ownDatabase(t))
	ctx := context.Background()
	s := store.New(pool)
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool,
		"drop index stet.decisions_window_idx",
		"create index decisions_backend_idx on stet.decisions (backend_pid, sent_after)",
		"update stet.schema_version set sha256 = 'the schema of the step before'")
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `
		select indexdef from pg_indexes
		where schemaname = 'stet' and indexname in ('decisions_backend_idx', 'decisions_window_idx')`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"CREATE INDEX decisions_window_idx ON stet.decisions USING btree (backend_pid, done_before, sent_after)"}
	if !slices.Equal(got, want) {
		t.Errorf("after migrating, the indexes are %q, want %q", got, want)
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
			set := recordSet(t, pool, s, "1")
			if c.links {
				wantLinked(t, pool, c.name, set, id, 1)
			} else {
				wantLinked(t, pool, c.name, set, "", 0)
			}
		})
	}
}

// Of the decisions that match a set, one is dropped only on positive
// evidence that it judged another write (plan ruling U13, which replaces
// spec 5's narrowing): it has a target_pk on the set's table, that key is not
// among the set's keys, and it is among the keys of another set of the same
// backend, table and statement that the server took inside the decision's
// own window. Every other one stays, also when it is the only match. A set is
// linked only when one stays, and shows how many stay.
func TestHistoryNarrowsByTargetKey(t *testing.T) {
	pool, _ := captureDB(t)
	at := time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)
	const write = "update public.deals set name = $1 where id = $2"
	pid := int32(2000)
	next := func() int32 { pid++; return pid } // each case on a backend of its own
	// decided records a decision on p whose window, from at to 20ms later,
	// holds every set below but the late ones; pk "" means no target_pk.
	decided := func(t *testing.T, p int32, pk string, change ...func(*handDecision)) string {
		t.Helper()
		d := judged(p, at, at.Add(20*time.Millisecond), write)
		if pk != "" {
			d.PK = ptr(pk)
		}
		for _, c := range change {
			c(&d)
		}
		return recordDecision(t, pool, d)
	}
	// set records a set of the write on p, taken after after at, with keys.
	set := func(t *testing.T, p int32, after time.Duration, keys ...string) recordedSet {
		t.Helper()
		return recordSet(t, pool, handSet{PID: p, StmtAt: at.Add(after), Query: write, Table: "deals"}, keys...)
	}
	const ms = time.Millisecond

	t.Run("two writes to different rows: each set links to its own decision", func(t *testing.T) {
		p := next()
		one, two := decided(t, p, "1"), decided(t, p, "2")
		set1, set2 := set(t, p, 1*ms, "1"), set(t, p, 2*ms, "2")
		wantLinked(t, pool, "row 1", set1, one, 1)
		wantLinked(t, pool, "row 2", set2, two, 1)
	})
	t.Run("a decision whose key another set of its statement holds goes", func(t *testing.T) {
		p := next()
		here, other := decided(t, p, "8"), decided(t, p, "80")
		mine, theirs := set(t, p, 1*ms, "8", "9"), set(t, p, 2*ms, "80")
		wantLinked(t, pool, "keys 8 and 9", mine, here, 1)
		wantLinked(t, pool, "key 80", theirs, other, 1)
	})
	t.Run("it goes also when it is the only match", func(t *testing.T) {
		p := next()
		only := decided(t, p, "13")
		mine, theirs := set(t, p, 1*ms, "12"), set(t, p, 2*ms, "13")
		wantLinked(t, pool, "key 12", mine, "", 0)
		wantLinked(t, pool, "key 13", theirs, only, 1)
	})
	t.Run("one decision whose key no set holds is linked", func(t *testing.T) {
		p := next()
		only := decided(t, p, "15")
		wantLinked(t, pool, "key 14", set(t, p, 1*ms, "14"), only, 1)
	})
	t.Run("the other row's write changed nothing: its decision stays, the link is lost", func(t *testing.T) {
		p := next()
		decided(t, p, "1")
		decided(t, p, "2")
		wantLinked(t, pool, "row 1", set(t, p, 1*ms, "1"), "", 2)
	})
	t.Run("the write's own key written otherwise, beside another write's decision on the set's key", func(t *testing.T) {
		// '007' is how the agent wrote key 7: no set holds that text.
		p := next()
		decided(t, p, "007")
		decided(t, p, "7")
		wantLinked(t, pool, "key 7", set(t, p, 1*ms, "7"), "", 2)
	})
	t.Run("the write's own key written otherwise, beside another write's decision with no key", func(t *testing.T) {
		p := next()
		decided(t, p, "007")
		decided(t, p, "")
		wantLinked(t, pool, "key 7", set(t, p, 1*ms, "7"), "", 2)
	})
	t.Run("two decisions on the same row: neither", func(t *testing.T) {
		p := next()
		decided(t, p, "3")
		decided(t, p, "3")
		first, second := set(t, p, 1*ms, "3"), set(t, p, 2*ms, "3")
		wantLinked(t, pool, "first", first, "", 2)
		wantLinked(t, pool, "second", second, "", 2)
	})
	t.Run("a key this set and another set both hold stays", func(t *testing.T) {
		p := next()
		decided(t, p, "")
		decided(t, p, "22")
		both, other := set(t, p, 1*ms, "21", "22"), set(t, p, 2*ms, "22")
		wantLinked(t, pool, "keys 21 and 22", both, "", 2)
		wantLinked(t, pool, "key 22", other, "", 2)
	})
	t.Run("two decisions without a target_pk: neither", func(t *testing.T) {
		p := next()
		decided(t, p, "")
		decided(t, p, "")
		wantLinked(t, pool, "keys 4 and 5", set(t, p, 1*ms, "4", "5"), "", 2)
	})
	t.Run("a decision without a target_pk stays beside one on the set's key", func(t *testing.T) {
		p := next()
		decided(t, p, "6")
		decided(t, p, "")
		wantLinked(t, pool, "key 6", set(t, p, 1*ms, "6"), "", 2)
	})
	t.Run("a decision on another table stays, though a set of the table holds its key", func(t *testing.T) {
		p := next()
		decided(t, p, "7")
		decided(t, p, "99", func(d *handDecision) { d.Table = "accounts" })
		mine := set(t, p, 1*ms, "7")
		set(t, p, 2*ms, "99")
		wantLinked(t, pool, "key 7", mine, "", 2)
	})
	// What is not evidence: a set holding the other decision's key that is
	// outside that decision's window, of another statement, of another
	// table, or on another backend. The other decision stays.
	for _, c := range []struct {
		name  string
		other func(p int32) handSet
	}{
		{"a set after the decision's window", func(p int32) handSet {
			return handSet{PID: p, StmtAt: at.Add(20*ms + time.Microsecond), Query: write, Table: "deals"}
		}},
		{"a set before the decision's window", func(p int32) handSet {
			return handSet{PID: p, StmtAt: at.Add(-10*ms - time.Microsecond), Query: write, Table: "deals"}
		}},
		{"a set of another statement", func(p int32) handSet {
			return handSet{PID: p, StmtAt: at.Add(2 * ms), Query: write + " -- again", Table: "deals"}
		}},
		{"a set of another table", func(p int32) handSet {
			return handSet{PID: p, StmtAt: at.Add(2 * ms), Query: write, Table: "accounts"}
		}},
		{"a set on another backend", func(p int32) handSet {
			return handSet{PID: p + 100000, StmtAt: at.Add(2 * ms), Query: write, Table: "deals"}
		}},
	} {
		t.Run("no evidence: "+c.name, func(t *testing.T) {
			p := next()
			decided(t, p, "30")
			decided(t, p, "31")
			recordSet(t, pool, c.other(p), "31")
			wantLinked(t, pool, "key 30", set(t, p, 1*ms, "30"), "", 2)
		})
	}
	t.Run("evidence at the decision's window's edges counts", func(t *testing.T) {
		for i, after := range []time.Duration{-10 * ms, 20 * ms} {
			p := next()
			mine := decided(t, p, "30")
			decided(t, p, "31")
			recordSet(t, pool, handSet{PID: p, StmtAt: at.Add(after), Query: write, Table: "deals"}, "31")
			wantLinked(t, pool, fmt.Sprintf("key 30, evidence at edge %d", i), set(t, p, 1*ms, "30"), mine, 1)
		}
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
	lines := historyOf(t, pool, set.ID)
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
	lines := historyOf(t, rv, set.ID)
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

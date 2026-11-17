package store_test

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/store"
)

// approvedHeld stores h, marks it delivered and approves it from Stet's own
// account, with max_rows when maxRows is not nil.
func approvedHeld(t *testing.T, s *store.Store, pool *pgxpool.Pool, h *store.HeldWrite, maxRows *int64) *store.HeldWrite {
	t.Helper()
	pendingHeld(t, s, pool, h)
	if _, err := pool.Exec(context.Background(), `select stet.approve($1, null, $2)`, h.ID, maxRows); err != nil {
		t.Fatal(err)
	}
	return h
}

// The listing names approved held writes in the order they were approved,
// and which of them read the row in a snapshot. Other tests' rows can be in
// the shared database, so only this test's rows are checked (plan ruling P1).
func TestApprovedWrites(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()

	versioned := sampleHeld()
	absent := sampleHeld()
	absent.RowVersion, absent.RowAbsent = nil, true
	insert := sampleHeld()
	insert.Statement, insert.TargetPK, insert.RowVersion = "insert into contacts (id, title) values ($2, $1)", nil, nil
	var ours []string
	for _, h := range []*store.HeldWrite{versioned, absent, insert} {
		ours = append(ours, approvedHeld(t, s, pool, h, nil).ID)
	}
	pending := pendingHeld(t, s, pool, sampleHeld())
	applied := heldWithStatus(t, s, pool, "applied")

	all, err := s.ApprovedWrites(ctx, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var got []store.Approved
	at := map[string]int{}
	for i, w := range all {
		if slices.Contains(ours, w.ID) {
			got = append(got, w)
			at[w.ID] = i
		}
		if w.ID == pending.ID || w.ID == applied.ID {
			t.Errorf("listed %s, which is not approved", w.ID)
		}
	}
	want := []store.Approved{{ID: versioned.ID, Snapshot: true}, {ID: absent.ID, Snapshot: true}, {ID: insert.ID, Snapshot: false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// The limit cuts the list in its order: up to the second of ours.
	n := at[absent.ID] + 1
	limited, err := s.ApprovedWrites(ctx, n)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(limited))
	for i, w := range limited {
		ids[i] = w.ID
	}
	if len(limited) != n || !slices.Contains(ids, versioned.ID) || !slices.Contains(ids, absent.ID) || slices.Contains(ids, insert.ID) {
		t.Errorf("limit %d: got %v, want %d rows up to %s", n, ids, n, absent.ID)
	}
}

func TestClaimApproved(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()

	// Rows exist before the transactions that lock them, so that the
	// transactions end before the rows are removed.
	full := approvedHeld(t, s, pool, sampleHeld(), ptr(int64(5)))
	bare := sampleHeld()
	bare.Params, bare.ParamTypes, bare.Settings, bare.TargetPK, bare.RowVersion, bare.RowAbsent = nil, nil, nil, ptr("8"), nil, true
	approvedHeld(t, s, pool, bare, nil)
	pending := pendingHeld(t, s, pool, sampleHeld())

	tx := openTx(t)
	got, err := store.ClaimApproved(ctx, tx, full.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := &store.Claimed{
		ID: full.ID, LoginRole: "agent_a", Database: "stet_test", Statement: full.Statement, TargetTable: "contacts",
		Params: []*string{ptr("CTO"), nil}, ParamTypes: []uint32{25, 0},
		TargetPK: ptr("7"), RowVersion: ptr("123"), RowAbsent: false,
		Settings: map[string]string{"TimeZone": "UTC", "DateStyle": "ISO, MDY", "IntervalStyle": "postgres"},
		Limit:    ptr(int64(5)), Lapsed: false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claimed %s:\n got %+v\nwant %+v", full.ID, got, want)
	}

	other := openTx(t)
	if got, err := store.ClaimApproved(ctx, other, full.ID); got != nil || err != nil {
		t.Errorf("claimed again while the first transaction holds it: %+v, %v", got, err)
	}
	got, err = store.ClaimApproved(ctx, other, bare.ID)
	if err != nil {
		t.Fatal(err)
	}
	want = &store.Claimed{
		ID: bare.ID, LoginRole: "agent_a", Database: "stet_test", Statement: bare.Statement, TargetTable: "contacts",
		TargetPK: ptr("8"), RowAbsent: true, Limit: ptr(int64(1)),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("claimed %s:\n got %+v\nwant %+v", bare.ID, got, want)
	}

	for what, id := range map[string]string{"a pending write": pending.ID, "an unknown id": store.NewID()} {
		if got, err := store.ClaimApproved(ctx, other, id); got != nil || err != nil {
			t.Errorf("%s: %+v, %v", what, got, err)
		}
	}

	// Rows written outside stet.approve: the lapse and the limit come from what is stored.
	for _, c := range []struct {
		what        string
		exprs       map[string]string
		lapsed      bool
		limit       *int64
		limitString string
	}{
		{"approved 11 minutes ago", map[string]string{"resolved_at": "clock_timestamp() - interval '11 minutes'", "planned_rows": "1"}, true, ptr(int64(1)), "1"},
		{"approved 9 minutes ago", map[string]string{"resolved_at": "clock_timestamp() - interval '9 minutes'", "planned_rows": "1"}, false, ptr(int64(1)), "1"},
		{"approved at no recorded time", map[string]string{"planned_rows": "1"}, true, ptr(int64(1)), "1"},
		{"approved with no limit", map[string]string{"resolved_at": "clock_timestamp()"}, false, nil, "nil"},
	} {
		c.exprs["status"] = "'approved'"
		id := insertRaw(t, pool, c.exprs)
		claim, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got, err := store.ClaimApproved(ctx, claim, id)
		claim.Rollback(ctx)
		if err != nil || got == nil {
			t.Errorf("%s: %+v, %v", c.what, got, err)
			continue
		}
		if got.Lapsed != c.lapsed || !reflect.DeepEqual(got.Limit, c.limit) {
			t.Errorf("%s: lapsed %v, limit %v; want %v, %s", c.what, got.Lapsed, deref(got.Limit), c.lapsed, c.limitString)
		}
	}
}

// The ten minutes are measured on the clock, not from the start of the
// transaction that claims the row (correction B9).
func TestClaimApprovedReadsTheClock(t *testing.T) {
	_, pool := testStore(t)
	ctx := context.Background()
	id := insertRaw(t, pool, map[string]string{
		"status": "'approved'", "planned_rows": "1",
		"resolved_at": "clock_timestamp() - interval '10 minutes' + interval '250 milliseconds'",
	})
	tx := openTx(t)
	if _, err := tx.Exec(ctx, `select pg_sleep(0.5)`); err != nil {
		t.Fatal(err)
	}
	got, err := store.ClaimApproved(ctx, tx, id)
	if err != nil || got == nil {
		t.Fatalf("%+v, %v", got, err)
	}
	if !got.Lapsed {
		t.Error("not lapsed, though more than ten minutes passed on the clock since the approval")
	}
}

func TestMarkAppliedAndFailed(t *testing.T) {
	s, pool := testStore(t)
	ctx := context.Background()
	applied := approvedHeld(t, s, pool, sampleHeld(), nil)
	failed := approvedHeld(t, s, pool, sampleHeld(), nil)

	// Marked in the transaction that claimed them, after a while: applied_at
	// is when they were marked (correction B9 as ruled for P5).
	tx := openTx(t)
	var began time.Time
	if err := tx.QueryRow(ctx, `select now()`).Scan(&began); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{applied.ID, failed.ID} {
		if c, err := store.ClaimApproved(ctx, tx, id); c == nil || err != nil {
			t.Fatalf("claim %s: %+v, %v", id, c, err)
		}
	}
	const slept = 300 * time.Millisecond
	if _, err := tx.Exec(ctx, `select pg_sleep(0.3)`); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkApplied(ctx, tx, applied.ID, 3); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFailed(ctx, tx, failed.ID, "row_changed"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	type result struct {
		status       string
		appliedAt    *time.Time
		rowsAffected *int64
		error        *string
	}
	read := func(id string) result {
		t.Helper()
		var r result
		if err := pool.QueryRow(ctx, `select status, applied_at, rows_affected, error from stet.held_writes where id = $1`, id).
			Scan(&r.status, &r.appliedAt, &r.rowsAffected, &r.error); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := read(applied.ID); r.status != "applied" || deref(r.rowsAffected) != int64(3) || r.error != nil ||
		r.appliedAt == nil || r.appliedAt.Sub(began) < slept {
		t.Errorf("applied: %s, rows_affected %v, error %v, applied_at %v; want applied, 3, NULL, at least %v after %v",
			r.status, deref(r.rowsAffected), deref(r.error), r.appliedAt, slept, began)
	}
	if r := read(failed.ID); r.status != "failed" || deref(r.error) != "row_changed" || r.rowsAffected != nil ||
		r.appliedAt == nil || r.appliedAt.Sub(began) < slept {
		t.Errorf("failed: %s, error %v, rows_affected %v, applied_at %v; want failed, row_changed, NULL, at least %v after %v",
			r.status, deref(r.error), deref(r.rowsAffected), r.appliedAt, slept, began)
	}

	// Only an approved row can be marked; any other is left as it is.
	pending := pendingHeld(t, s, pool, sampleHeld())
	for what, id := range map[string]string{
		"pending": pending.ID, "applied": applied.ID, "failed": failed.ID, "unknown": store.NewID(),
	} {
		before := rowImage(t, pool, id)
		if err := store.MarkApplied(ctx, pool, id, 1); err == nil {
			t.Errorf("MarkApplied on a %s row: no error", what)
		}
		if err := store.MarkFailed(ctx, pool, id, "row_changed"); err == nil {
			t.Errorf("MarkFailed on a %s row: no error", what)
		}
		if after := rowImage(t, pool, id); after != before {
			t.Errorf("%s row changed:\n%s\n%s", what, before, after)
		}
	}
}

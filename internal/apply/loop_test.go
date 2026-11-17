package apply_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/apply"
	"github.com/stetdb/stet/internal/store"
)

// start starts e's Applier, and closes it when the test ends unless the test
// did. Cleanups registered before it run after Close.
func (e *env) start() {
	e.a.Start()
	e.t.Cleanup(e.a.Close)
}

// eventually waits up to d for ok, and fails the test, saying what it waited
// for, when ok never holds.
func (e *env) eventually(d time.Duration, what string, ok func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			e.t.Fatalf("not within %s: %s; log:\n%s", d, what, e.log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settled reports whether every one of ids has left approved.
func (e *env) settled(ids ...string) func() bool {
	return func() bool {
		for _, id := range ids {
			if e.row(id).Status == "approved" {
				return false
			}
		}
		return true
	}
}

// entering has a's next held write signal the returned channel once it has
// passed its checks, before its statement.
func entering(a *apply.Applier) <-chan struct{} {
	entered := make(chan struct{})
	var once sync.Once
	apply.BeforeStatement(a, func() { once.Do(func() { close(entered) }) })
	return entered
}

func (e *env) wait(ch <-chan struct{}, what string) {
	e.t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		e.t.Fatalf("not within 5s: %s; log:\n%s", what, e.log)
	}
}

func (e *env) wantApproved(id string) {
	e.t.Helper()
	if r := e.row(id); r.Status != "approved" || r.AppliedAt != nil || r.Error != nil || r.RowsAffected != nil {
		e.t.Errorf("held write %s is now %+v, want it still approved; log:\n%s", id, r, e.log)
	}
}

func TestLoopAppliesApprovalsInOrder(t *testing.T) {
	e := setup(t)
	e.exec("create sequence turns")
	e.exec("grant usage on sequence turns to stet_md5")
	var ids []string
	for _, k := range []int{3, 1, 2} {
		h := e.update(fmt.Sprintf("update contacts set n = nextval('turns') where id = %d", k), k)
		ids = append(ids, e.approved(h, nil).ID)
	}

	e.start()
	e.eventually(3*time.Second, "the three approved writes applied", e.settled(ids...))
	for _, id := range ids {
		e.wantApplied(id, 1)
	}
	// n is the turn each write ran in: row 3 was approved first.
	if got, want := e.contents(table), "(1,n1,,stet_md5,2);(2,n2,,stet_md5,3);(3,n3,,stet_md5,1)"; got != want {
		t.Errorf("contacts is %s, want %s", got, want)
	}
}

func TestLoopPicksUpApprovalsMadeWhileItWasDown(t *testing.T) {
	e := setup(t)
	waited := e.approved(e.update("update contacts set name = 'waited' where id = 1", 1), nil)
	lapsed := e.forceApproved(e.update("update contacts set name = 'lapsed' where id = 2", 2),
		"clock_timestamp() - interval '11 minutes'")
	pending := e.update("update contacts set name = 'pending' where id = 3", 3)
	e.pending(pending)

	e.start()
	e.eventually(3*time.Second, "the approved writes settled", e.settled(waited.ID, lapsed.ID))
	e.wantApplied(waited.ID, 1)
	e.wantName(1, "waited")
	e.wantFailed(lapsed.ID, "approval_lapsed", true)
	e.wantName(2, "n2")
	if r := e.row(pending.ID); r.Status != "pending" || r.AppliedAt != nil {
		t.Errorf("the pending held write is now %+v, want it untouched", r)
	}
	e.wantName(3, "n3")
}

func TestLoopGoesOnAfterAFailure(t *testing.T) {
	e := setup(t)
	// Tried again at the next poll, not 30 seconds later (addendum F).
	t.Cleanup(apply.SetRetryAfter(0))
	n := func(id int) int {
		var n int
		if err := e.pool.QueryRow(context.Background(), "select n from contacts where id = $1", id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	first := e.approved(e.update("update contacts set n = n + 1 where id = 1", 1), nil)
	second := e.approved(e.update("update contacts set n = n + 1 where id = 2", 2), nil)
	apply.FailMarks(e.a, 1) // the first write's own step fails once

	e.start()
	e.eventually(3*time.Second, "both approved writes applied", e.settled(first.ID, second.ID))
	e.wantApplied(first.ID, 1)
	e.wantApplied(second.ID, 1)
	if n(1) != 1 || n(2) != 1 {
		t.Errorf("n = %d and %d, want 1 and 1: each write ran once", n(1), n(2))
	}
	if !e.logged("level=WARN", "id="+first.ID, "MarkApplied made to fail by the test") {
		t.Errorf("no warn line for the failed attempt at %s; log:\n%s", first.ID, e.log)
	}
}

// Addendum F: a write whose Apply returned an error is left alone for a while,
// and the loop goes on with the others.
func TestLoopWaitsBeforeTryingAFailedWriteAgain(t *testing.T) {
	e := setup(t)
	failing := e.approved(e.update("update contacts set name = 'failing' where id = 1", 1), nil)
	var mu sync.Mutex
	attempts := map[string]int{}
	apply.OnApply(e.a, func(w store.Approved) error {
		mu.Lock()
		attempts[w.ID]++
		mu.Unlock()
		if w.ID == failing.ID {
			return errors.New("Apply made to fail by the test")
		}
		return e.a.Apply(w)
	})

	started := time.Now()
	e.start()
	time.Sleep(500 * time.Millisecond)
	// Approved after the first poll: applied by a later one.
	other := e.approved(e.update("update contacts set name = 'other' where id = 2", 2), nil)
	e.eventually(2*time.Second, "the other approved write applied", e.settled(other.ID))
	time.Sleep(2500*time.Millisecond - time.Since(started))

	mu.Lock()
	tried := attempts[failing.ID]
	mu.Unlock()
	if tried != 1 {
		t.Errorf("the failing write was tried %d times in 2.5s, want once", tried)
	}
	e.wantApproved(failing.ID)
	e.wantName(1, "n1")
	e.wantApplied(other.ID, 1)
	e.wantName(2, "other")
}

// A panic in one write's processing is logged with its stack and is one of
// Stet's own failures: the write stays approved and waits like one, and the
// loop goes on with the others, at later polls too.
func TestLoopGoesOnAfterAPanic(t *testing.T) {
	e := setup(t)
	panicking := e.approved(e.update("update contacts set name = 'panicking' where id = 1", 1), nil)
	other := e.approved(e.update("update contacts set name = 'other' where id = 2", 2), nil)
	var mu sync.Mutex
	tried := 0
	apply.OnApply(e.a, func(w store.Approved) error {
		if w.ID == panicking.ID {
			mu.Lock()
			tried++
			mu.Unlock()
			panic("Apply made to panic by the test")
		}
		return e.a.Apply(w)
	})

	e.start()
	e.eventually(3*time.Second, "the other approved write applied", e.settled(other.ID))
	// Approved after the panic, so applied by a later poll: the loop is alive.
	later := e.update("update contacts set name = 'later' where id = 3", 3)
	e.pending(later)
	e.exec("select stet.approve($1)", later.ID)
	e.eventually(3*time.Second, "the write approved after the panic applied", e.settled(later.ID))

	e.wantApplied(other.ID, 1)
	e.wantName(2, "other")
	e.wantApplied(later.ID, 1)
	e.wantName(3, "later")
	e.wantApproved(panicking.ID)
	e.wantName(1, "n1")
	mu.Lock()
	n := tried
	mu.Unlock()
	if n != 1 {
		t.Errorf("the panicking write was tried %d times, want once: it waits %s like any of Stet's own failures", n, 30*time.Second)
	}
	if !e.logged("level=ERROR", "id="+panicking.ID, "Apply made to panic by the test", "goroutine ") {
		t.Errorf("no error line with the panic and its stack for %s; log:\n%s", panicking.ID, e.log)
	}
	if !e.logged("level=WARN", "id="+panicking.ID, `msg="held write not applied; it stays approved"`) {
		t.Errorf("no warn line for %s as for one of Stet's own failures; log:\n%s", panicking.ID, e.log)
	}
}

func TestCloseWaitsForTheWriteBeingApplied(t *testing.T) {
	e := setup(t)
	slow := e.approved(e.update("update contacts set name = (select 'slept' from pg_sleep(1)) where id = 1", 1), nil)
	// Listed with the slow one, and next in turn when Close is called.
	next := e.approved(e.update("update contacts set name = 'next' where id = 3", 3), nil)
	entered := entering(e.a)

	e.start()
	e.wait(entered, "the slow write reached its statement")
	e.a.Close()
	e.wantApplied(slow.ID, 1)
	e.wantName(1, "slept")
	e.wantApproved(next.ID)

	later := e.approved(e.update("update contacts set name = 'later' where id = 2", 2), nil)
	time.Sleep(1500 * time.Millisecond)
	e.wantApproved(later.ID)
	e.wantName(2, "n2")
	e.wantApproved(next.ID)
	e.wantName(3, "n3")
}

func TestCloseAbandonsAWritePastItsBound(t *testing.T) {
	e := setup(t)
	e.cfg.Held.ApplyTimeout = 2 * time.Second
	t.Cleanup(apply.SetGrace(-time.Second)) // a bound of 1s, before the server's 2s; runs after Close
	before := e.contents(table)
	statement := "update contacts set name = (select 'slept' from pg_sleep(3)) where id = 1"
	slow := e.approved(e.update(statement, 1), nil)
	entered := entering(e.a)

	e.start()
	e.wait(entered, "the slow write reached its statement")
	began := time.Now()
	e.a.Close()
	// At the bound, not when the statement or the server's timeout ends it.
	if d := time.Since(began); d > 2*time.Second {
		t.Errorf("Close took %s, want under 2s", d)
	}
	e.wantApproved(slow.ID)
	e.wantUnchanged(table, before)

	// Nothing of it is kept once the server is done with it.
	e.gone(statement)
	e.wantApproved(slow.ID)
	e.wantUnchanged(table, before)
}

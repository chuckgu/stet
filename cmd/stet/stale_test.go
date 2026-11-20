package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/gate"
)

// startWithGate is start, and the gate run opened, once it has.
func startWithGate(t *testing.T, cfg *config.Config) (*running, func() *gate.Gate) {
	t.Helper()
	got := make(chan *gate.Gate, 1)
	onGate = func(g *gate.Gate) { got <- g }
	t.Cleanup(func() { onGate = nil }) // runs after the stop start registers
	r := start(t, cfg)
	return r, func() *gate.Gate {
		t.Helper()
		select {
		case g := <-got:
			got <- g
			return g
		case <-time.After(5 * time.Second):
			t.Fatalf("run opened no gate; log:\n%s", r.log)
			return nil
		}
	}
}

// readRow reads row id of table as conn, through the proxy, and waits until
// the gate has learned the version of what it read.
func readRow(t *testing.T, conn *pgx.Conn, g *gate.Gate, table string, id int) {
	t.Helper()
	ctx := context.Background()
	rows, err := conn.Query(ctx, "select id, name from "+table+" where id = $1", id)
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := g.StaleIdle(wait); err != nil {
		t.Fatalf("the lookup of the read row did not finish: %v", err)
	}
}

// decisionOf waits for the decision recorded for statement and returns its
// read_check result, its reasons and whether it was executed.
func decisionOf(t *testing.T, conn *pgx.Conn, statement string, log fmt.Stringer) (result *string, reasons []string, executed bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := conn.QueryRow(context.Background(),
			"select read_check->>'result', reasons, executed from stet.decisions where statement = $1", statement).
			Scan(&result, &reasons, &executed)
		if err == nil {
			return result, reasons, executed
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no decision was recorded for %q; log:\n%s", statement, log)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStaleEndToEnd(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db, table, admin := agentTable(t, pg, 2)

	// Shadow: the write goes through, and the record says it was stale.
	shadow := parseConfig(t, enforceYAML+"    stale: {}\n", pg, db, table)
	shadow.Mode = config.ModeShadow
	r, gateOf := startWithGate(t, shadow)
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")
	readRow(t, agent, gateOf(), table, 1)
	mustExec(t, admin, "update "+table+" set name = 'other' where id = 1")
	write := "update " + table + " set name = 'mine' where id = 1"
	if _, err := agent.Exec(ctx, write); err != nil {
		t.Fatalf("shadow: the write failed: %v; log:\n%s", err, r.log)
	}
	result, reasons, executed := decisionOf(t, admin, write, r.log)
	if result == nil || *result != "stale" || strings.Join(reasons, ",") != "stale_read" || !executed {
		t.Errorf("shadow: read_check %s, reasons %v, executed %v; want stale, [stale_read], true", orNull(result), reasons, executed)
	}
	var name string
	if err := admin.QueryRow(ctx, "select name from "+table+" where id = 1").Scan(&name); err != nil || name != "mine" {
		t.Errorf("shadow: row 1 is %q, %v; want the write to have run", name, err)
	}
	r.stop(t)

	// Enforce: the same order is refused with ST003, and nothing changes.
	enforce := parseConfig(t, enforceYAML+"    stale: {}\n", pg, db, table)
	r, gateOf = startWithGate(t, enforce)
	agent = connectTo(t, r.addr, db, "stet_md5", "md5-pw")
	readRow(t, agent, gateOf(), table, 2)
	mustExec(t, admin, "update "+table+" set name = 'other' where id = 2")
	write = "update " + table + " set name = 'mine' where id = 2"
	if _, err := agent.Exec(ctx, write); sqlstate(err) != "ST003" {
		t.Fatalf("enforce: the write gave %v, want ST003; log:\n%s", err, r.log)
	}
	result, reasons, executed = decisionOf(t, admin, write, r.log)
	if result == nil || *result != "stale" || strings.Join(reasons, ",") != "stale_read" || executed {
		t.Errorf("enforce: read_check %s, reasons %v, executed %v; want stale, [stale_read], false", orNull(result), reasons, executed)
	}
	if err := admin.QueryRow(ctx, "select name from "+table+" where id = 2").Scan(&name); err != nil || name != "other" {
		t.Errorf("enforce: row 2 is %q, %v; want the other party's value", name, err)
	}
}

// A held write a reviewer approved runs on Stet's own connection as the
// agent's role, outside the proxy: it must not make what the role read of its
// row look stale to the role's next write (spec-v0.2-stale 7, S10).
func TestApprovedWriteIsNotStale(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	reviewer := newReviewer(t, pg)
	db, table, admin := agentTable(t, pg, 3)
	cfg := parseConfig(t, enforceYAML+"    stale: {}\n    impact: {max_rows: 1, cumulative: {max_rows: 1, window: 1m}}\n", pg, db, table)
	r, gateOf := startWithGate(t, cfg)
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")
	rv := review(t, pg, db, reviewer)

	readRow(t, agent, gateOf(), table, 2)
	// Uses the cumulative limit up; the next write is held.
	if _, err := agent.Exec(ctx, "update "+table+" set name = 'a' where id = 3"); err != nil {
		t.Fatalf("the first write: %v; log:\n%s", err, r.log)
	}
	_, err := agent.Exec(ctx, "update "+table+" set name = 'b' where id = 2")
	id := heldWriteOf(t, err, r.log)
	waitStatus(t, rv, id, "pending", 3*time.Second, r.log)
	if _, err := rv.Exec(ctx, "select stet.approve($1)", id); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, rv, id, "applied", 5*time.Second, r.log)

	// Held again for the limit, but judged: what it read of row 2 is gone.
	next := "update " + table + " set name = 'c' where id = 2"
	_, err = agent.Exec(ctx, next)
	heldWriteOf(t, err, r.log)
	result, reasons, _ := decisionOf(t, admin, next, r.log)
	if result == nil || *result != "no_read" {
		t.Errorf("read_check after the approved write is %s, want no_read", orNull(result))
	}
	if strings.Contains(strings.Join(reasons, ","), "stale_read") {
		t.Errorf("reasons %v, want no stale_read", reasons)
	}
}

// A key of a type outside the eight the catalog supports leaves the table
// untracked, with one warning; Stet starts, and its writes are not checked.
func TestStaleTableNotTrackedStillStarts(t *testing.T) {
	pg := testPG(t)
	ctx := context.Background()
	db, _, admin := agentTable(t, pg, 1)
	const table = "dated"
	mustExec(t, admin, "create table "+table+" (id date primary key, name text)")
	mustExec(t, admin, "insert into "+table+" values ('2026-01-01', 'x')")
	mustExec(t, admin, "grant all on "+table+" to stet_md5")
	r := start(t, parseConfig(t, enforceYAML+"    stale: {}\n", pg, db, table))
	waitListening(t, r)
	agent := connectTo(t, r.addr, db, "stet_md5", "md5-pw")

	rows, err := agent.Query(ctx, "select id, name from "+table)
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	write := "update " + table + " set name = 'y' where id = '2026-01-01'"
	if _, err := agent.Exec(ctx, write); err != nil {
		t.Fatalf("the write failed: %v; log:\n%s", err, r.log)
	}
	result, _, executed := decisionOf(t, admin, write, r.log)
	if result == nil || *result != "not_checked" || !executed {
		t.Errorf("read_check %s, executed %v; want not_checked, true", orNull(result), executed)
	}
	var warnings []string
	for line := range strings.SplitSeq(r.log.String(), "\n") {
		if strings.Contains(line, "does not track a table") {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "level=WARN") || !strings.Contains(warnings[0], "table="+table) {
		t.Errorf("warnings %q, want exactly one, for %s", warnings, table)
	}
}

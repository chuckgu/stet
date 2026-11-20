package bench

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/lab/crm"
	"github.com/stetdb/stet/internal/lab/tasks"
)

// TestCheckStale runs the check of docs/spec-v0.2-stale.md 11 in both modes on a small template of the test's own:
// each scenario runs in the modes the spec gives it, in the spec's order, and comes out as the spec expects (a P row
// sent and recorded stale_read in shadow, refused with ST003 in enforce; N5's last write run); M1 is the known miss,
// fresh; and the clones are gone at the end.
func TestCheckStale(t *testing.T) {
	a := testAdmin(t)
	ctx := context.Background()
	var n Names
	var tmpl string
	// Registered first, so it runs last: after testNames' cleanup has dropped the template and the roles.
	t.Cleanup(func() {
		su, err := pgx.Connect(ctx, a.URL("postgres"))
		if err != nil {
			t.Error(err)
			return
		}
		defer su.Close(ctx)
		dbs, roles, err := Created(ctx, su, n)
		if err != nil {
			t.Error(err)
			return
		}
		var named []string
		if err := su.QueryRow(ctx, "select array(select datname::text from pg_database where starts_with(datname, $1))",
			tmpl).Scan(&named); err != nil {
			t.Error(err)
		}
		if len(dbs) > 0 || len(roles) > 0 || len(named) > 0 {
			t.Errorf("left behind: databases %v %v, roles %v", dbs, named, roles)
		}
	})
	n = testNames(t)
	su := connectAs(t, a, a.User, a.Password, "postgres")
	if err := CreateRoles(ctx, su, n); err != nil {
		t.Fatal(err)
	}
	d := crm.Generate(crm.Options{Seed: 7, Accounts: 20, Contacts: 100, KoreanShare: 0.3, Now: testNow})
	tmpl = fmt.Sprintf("crm_t%d_stale", time.Now().UnixNano())
	if err := CreateTemplate(ctx, a, n, tmpl, d, tasks.Bench{}, map[string]any{"test": "stale"}); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		progress []string
	)
	res, err := CheckStale(ctx, StaleCheck{Admin: a, Names: n, Template: tmpl, Database: tmpl + "_c",
		Progress: func(line string) {
			mu.Lock()
			defer mu.Unlock()
			progress = append(progress, line)
		}})
	mu.Lock()
	said := strings.Join(progress, "\n")
	mu.Unlock()
	if err != nil {
		t.Fatalf("CheckStale: %v\nprogress:\n%s", err, said)
	}
	var text bytes.Buffer
	if err := res.Write(&text); err != nil {
		t.Fatal(err)
	}
	t.Logf("the check's result:\n%s", text.String())

	var left []string
	if err := su.QueryRow(ctx, "select array(select datname::text from pg_database where starts_with(datname, $1))",
		tmpl+"_c").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("the check left its clones behind: %v", left)
	}

	// The spec's nineteen scenarios, each in its modes: N5 and N10 in enforce only, N11 in shadow only.
	var want []string
	for _, id := range []string{"P1", "P2", "P3", "P4", "P5", "P6", "P7", "M1", "N1", "N2", "N3", "N4", "N5", "N6", "N7",
		"N8", "N9", "N10", "N11"} {
		for _, mode := range []string{"shadow", "enforce"} {
			if (id == "N5" || id == "N10") && mode == "shadow" || id == "N11" && mode == "enforce" {
				continue
			}
			want = append(want, id+" "+mode)
		}
	}
	var got []string
	for _, r := range res.Rows {
		got = append(got, r.ID+" "+r.Mode)
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows %v,\nwant %v", got, want)
	}

	for _, r := range res.Rows {
		if !r.OK {
			t.Errorf("%s %s: read_check %q (want %v), %s %v, executed %v, sqlstate %q, note %q: not as the spec expects",
				r.ID, r.Mode, r.Got, r.Want, r.Verdict, r.Reasons, r.Executed, r.Code, r.Note)
		}
		switch {
		case strings.HasPrefix(r.ID, "P") && r.Mode == "shadow":
			if r.Got != "stale" || !r.Executed || !slices.Contains(r.Reasons, "stale_read") || r.Code != "" {
				t.Errorf("%s shadow: read_check %q, executed %v, reasons %v, sqlstate %q; want stale, sent, stale_read",
					r.ID, r.Got, r.Executed, r.Reasons, r.Code)
			}
		case strings.HasPrefix(r.ID, "P"):
			if r.Got != "stale" || r.Executed || r.Verdict != "reject" || r.Code != "ST003" {
				t.Errorf("%s enforce: read_check %q, executed %v, verdict %s, sqlstate %q; want stale, refused with ST003",
					r.ID, r.Got, r.Executed, r.Verdict, r.Code)
			}
		case r.ID == "M1":
			if r.Got != "fresh" {
				t.Errorf("M1 %s: read_check %q, want fresh (the known miss of spec 12)", r.Mode, r.Got)
			}
		case r.ID == "N5":
			if !r.Executed || r.Code != "" {
				t.Errorf("N5: executed %v, sqlstate %q; want the write read again to run", r.Executed, r.Code)
			}
		}
	}
	if f := res.Failed(); f != 0 {
		t.Errorf("Failed() = %d, want 0\nprogress:\n%s", f, said)
	}
}

// TestStaleCaseOK: a row is as the spec expects only when its read_check is one the spec allows and nothing kept it
// from running as its steps assume; a P row's last write was sent with no error and recorded with stale_read in shadow,
// and refused with ST003 as a reject with stale_read in enforce; an M or N row's was not rejected and has no
// stale_read; N5's ran.
func TestStaleCaseOK(t *testing.T) {
	p := staleCase{id: "P1", want: []string{"stale"}, stale: true}
	n := staleCase{id: "N1", want: []string{"fresh"}}
	n5 := staleCase{id: "N5", want: []string{"fresh", "read_pending"}, runs: true}
	shadowP := StaleRow{ID: "P1", Mode: "shadow", Got: "stale", Verdict: "reject", Reasons: []string{"stale_read"}, Executed: true}
	enforceP := StaleRow{ID: "P1", Mode: "enforce", Got: "stale", Verdict: "reject", Reasons: []string{"stale_read"}, Code: "ST003"}
	fresh := StaleRow{ID: "N1", Mode: "enforce", Got: "fresh", Verdict: "commit", Executed: true}
	with := func(r StaleRow, change func(*StaleRow)) StaleRow {
		change(&r)
		return r
	}
	for _, c := range []struct {
		name string
		sc   staleCase
		row  StaleRow
		want bool
	}{
		{"shadow P as expected", p, shadowP, true},
		{"shadow P whose client got an error", p, with(shadowP, func(r *StaleRow) { r.Code = "57014" }), false},
		{"shadow P without stale_read", p, with(shadowP, func(r *StaleRow) { r.Reasons = nil }), false},
		{"shadow P not run", p, with(shadowP, func(r *StaleRow) { r.Executed = false }), false},
		{"shadow P with another read_check", p, with(shadowP, func(r *StaleRow) { r.Got = "fresh" }), false},
		{"enforce P as expected", p, enforceP, true},
		{"enforce P held", p, with(enforceP, func(r *StaleRow) { r.Verdict = "hold" }), false},
		{"enforce P refused for another reason", p, with(enforceP, func(r *StaleRow) { r.Reasons = []string{"unsupported"} }), false},
		{"enforce P run", p, with(enforceP, func(r *StaleRow) { r.Executed, r.Code = true, "" }), false},
		{"N as expected", n, fresh, true},
		{"N rejected", n, with(fresh, func(r *StaleRow) { r.Verdict, r.Executed, r.Code = "reject", false, "ST003" }), false},
		{"N with stale_read", n, with(fresh, func(r *StaleRow) { r.Reasons = []string{"in_transaction", "stale_read"} }), false},
		{"N with another read_check", n, with(fresh, func(r *StaleRow) { r.Got = "no_read" }), false},
		{"N with a note", n, with(fresh, func(r *StaleRow) { r.Note = "a step failed" }), false},
		{"N held", n, with(fresh, func(r *StaleRow) { r.Verdict, r.Executed, r.Code = "hold", false, "ST001" }), true},
		{"N5 run", n5, fresh, true},
		{"N5 not run", n5, with(fresh, func(r *StaleRow) { r.Verdict, r.Executed, r.Code = "hold", false, "ST001" }), false},
	} {
		if got := c.sc.ok(c.row); got != c.want {
			t.Errorf("%s: ok %v, want %v (%+v)", c.name, got, c.want, c.row)
		}
	}
}

// TestStaleOwnerChangeMustTouchOneRow: a change of the owner's that touches no row, or more than one, would let a
// scenario come out fresh for the wrong reason, so it ends the check.
func TestStaleOwnerChangeMustTouchOneRow(t *testing.T) {
	a := testAdmin(t)
	ctx := context.Background()
	conn := connectAs(t, a, a.User, a.Password, "postgres")
	for _, sql := range []string{
		"create temp table contacts (id int primary key, title text)",
		"insert into contacts values (1, 'as inserted'), (2, 'as inserted')",
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	s := &staleScenario{staleMode: &staleMode{owner: conn}, ctx: ctx, id: "T"}
	s.change(ownerTitle, 1)
	if s.err != nil {
		t.Fatalf("a change of one row: %v", s.err)
	}
	for _, c := range []struct {
		sql  string
		args []any
		rows string
	}{
		{ownerTitle, []any{3}, "0 rows"},
		{"update contacts set title = 'x' where id > $1", []any{0}, "2 rows"},
		{"delete from contacts where id = $1", []any{3}, "0 rows"},
	} {
		s := &staleScenario{staleMode: &staleMode{owner: conn}, ctx: ctx, id: "T"}
		s.change(c.sql, c.args...)
		if s.err == nil || !s.fatal || !strings.Contains(s.err.Error(), c.rows) {
			t.Errorf("%s %v: err %v, fatal %v; want the check ended, saying %s", c.sql, c.args, s.err, s.fatal, c.rows)
		}
	}
}

// TestStaleResultWrite: the result is a Markdown table with a row per scenario and mode, then what kept a row from
// running as its steps assume, then the count.
func TestStaleResultWrite(t *testing.T) {
	r := StaleResult{Rows: []StaleRow{
		{ID: "P1", Mode: "enforce", Want: []string{"stale"}, Got: "stale", Verdict: "reject", Reasons: []string{"stale_read"},
			Code: "ST003", OK: true},
		{ID: "N2", Mode: "shadow", Want: []string{"fresh", "own_write"}, Got: "own_write", Verdict: "commit", Executed: true,
			OK: true},
		{ID: "N10", Mode: "enforce", Want: []string{"no_read"}, Note: "the held write ran; the scenario expects it refused with ST001"},
	}}
	var b bytes.Buffer
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	want := "| scenario | mode | want | read_check | verdict | reasons | executed | sqlstate | ok |\n" +
		"|---|---|---|---|---|---|---|---|---|\n" +
		"| P1 | enforce | stale | stale | reject | stale_read | no | ST003 | ok |\n" +
		"| N2 | shadow | fresh or own_write | own_write | commit | - | yes | - | ok |\n" +
		"| N10 | enforce | no_read | - | - | - | - | - | FAIL |\n" +
		"\n" +
		"- N10 enforce: the held write ran; the scenario expects it refused with ST001\n" +
		"\n" +
		"3 rows: 2 as expected, 1 not\n"
	if b.String() != want {
		t.Errorf("the result reads\n%s\nwant\n%s", b.String(), want)
	}
	if got := r.Failed(); got != 1 {
		t.Errorf("Failed() = %d, want 1", got)
	}
}

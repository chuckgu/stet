package bench

// This file holds what CheckStale returns (stale.go) and its table.

import (
	"cmp"
	"fmt"
	"io"
	"strings"
)

// StaleRow is one scenario of spec-v0.2-stale 11 in one mode.
type StaleRow struct {
	ID, Mode string   // "P1" ... "N11"; "shadow" or "enforce"
	Want     []string // the read_check results the spec allows
	Got      string   // read_check.result of the scenario's last write; "" when no decision of it was recorded
	Verdict  string
	Reasons  []string
	Executed bool
	Code     string // the SQLSTATE the client got; "" when the write ran
	// Note says what kept the scenario from running as its steps assume: a statement sent through Stet that failed
	// or was not refused as the scenario expects, lookups that did not finish, a decision not recorded. The scenario
	// stops at that step, and the row is not OK. "" when nothing did.
	Note string
	OK   bool
}

// StaleResult is what CheckStale found: a row per scenario and mode, in the spec's order, shadow before enforce.
type StaleResult struct{ Rows []StaleRow }

// Failed is how many rows are not as the spec expects.
func (r StaleResult) Failed() int {
	n := 0
	for _, row := range r.Rows {
		if !row.OK {
			n++
		}
	}
	return n
}

// Write writes the result as a Markdown table, a row per scenario and mode; then, as a list, the note of each row
// that has one; then how many rows are as expected. A cell with nothing to say holds "-"; executed is "-" too when
// no decision was recorded.
func (r StaleResult) Write(w io.Writer) error {
	var b strings.Builder
	b.WriteString("| scenario | mode | want | read_check | verdict | reasons | executed | sqlstate | ok |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	var notes []string
	for _, row := range r.Rows {
		executed := "-"
		if row.Verdict != "" {
			executed = "no"
			if row.Executed {
				executed = "yes"
			}
		}
		ok := "FAIL"
		if row.OK {
			ok = "ok"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", row.ID, row.Mode, strings.Join(row.Want, " or "),
			cmp.Or(row.Got, "-"), cmp.Or(row.Verdict, "-"), cmp.Or(strings.Join(row.Reasons, ", "), "-"), executed,
			cmp.Or(row.Code, "-"), ok)
		if row.Note != "" {
			notes = append(notes, fmt.Sprintf("- %s %s: %s\n", row.ID, row.Mode, row.Note))
		}
	}
	if len(notes) > 0 {
		b.WriteString("\n" + strings.Join(notes, ""))
	}
	failed := r.Failed()
	fmt.Fprintf(&b, "\n%d rows: %d as expected, %d not\n", len(r.Rows), len(r.Rows)-failed, failed)
	_, err := io.WriteString(w, b.String())
	return err
}

// Package lines turns recorded decisions into the lines stet proxy prints.
package lines

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/store"
)

const (
	verdictWidth = 12
	maxStatement = 80
	// statementScan is how many bytes of a statement statement reads. 80
	// runes are at most 320 bytes of UTF-8; the rest leaves room for the
	// white space and unsafe characters that collapse into one space
	// (indentation, line breaks). So a statement of megabytes costs the
	// judging goroutine no more than one of 4 KiB. One longer than that
	// whose first 4 KiB collapse to fewer than 80 characters is shown as
	// those characters and "…".
	statementScan = 4096
)

var shadowNames = map[string]string{
	"commit":   "ok",
	"hold":     "would hold",
	"escalate": "would escalate",
	"reject":   "would reject",
}

// Format returns the line for d, without a newline, its time in loc. It
// reads only what d holds, and never fails: what it cannot read it leaves out.
func Format(d *store.Decision, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	var cols []string
	cols = append(cols, d.CreatedAt.In(loc).Format("15:04:05"))

	verdict := d.Verdict
	if d.Mode == "shadow" {
		if n, ok := shadowNames[verdict]; ok {
			verdict = n
		}
	} else if verdict == "commit" {
		verdict = "ok"
	}
	cols = append(cols, fmt.Sprintf("%-*s", verdictWidth, plain(verdict)))

	if ot := strings.TrimSpace(strings.ToUpper(plain(d.Op)) + " " + plain(d.TargetTable)); ot != "" {
		cols = append(cols, ot)
	}
	if f := facts(d); f != "" {
		cols = append(cols, f)
	}
	if len(d.Reasons) > 0 {
		rs := make([]string, len(d.Reasons))
		for i, r := range d.Reasons {
			rs[i] = plain(r)
		}
		cols = append(cols, "["+strings.Join(rs, ", ")+"]")
	}
	if s := statement(d.Statement); s != "" {
		cols = append(cols, s)
	}
	return strings.TrimRight(strings.Join(cols, "  "), " ")
}

func facts(d *store.Decision) string {
	var impact *decide.Impact
	switch v := d.Impact.(type) {
	case *decide.Impact:
		impact = v
	case decide.Impact:
		impact = &v
	}
	var out []string
	var n int64
	haveN, plus := false, false
	prefix := ""
	switch {
	case d.RowsAffected != nil:
		n, haveN = *d.RowsAffected, true
	case impact != nil && impact.Rows != nil:
		n, haveN = *impact.Rows, true
		switch {
		case impact.Capped && impact.Estimate != nil:
			n, prefix = *impact.Estimate, "~"
		case impact.Capped:
			plus = true
		case impact.Method == "estimate":
			prefix = "~"
		}
	case impact != nil && impact.Estimate != nil:
		n, haveN, prefix = *impact.Estimate, true, "~"
	}
	if haveN {
		unit := "rows"
		if n == 1 && !plus {
			unit = "row"
		}
		num := fmt.Sprintf("%d", n)
		if plus {
			num += "+"
		}
		out = append(out, prefix+num+" "+unit)
		if !plus && n > 0 && impact != nil && impact.TableRows != nil && *impact.TableRows > 0 {
			p := math.Round(float64(n) * 100 / float64(*impact.TableRows))
			switch {
			case p > 100:
				p = 100
				fallthrough
			case p >= 1:
				out = append(out, fmt.Sprintf("~%d%% of table", int64(p)))
			default:
				out = append(out, "<1% of table")
			}
		}
	}
	if impact != nil {
		var names []string
		seen := map[string]bool{}
		for _, c := range impact.Cascades {
			t := plain(c.Table)
			if !seen[t] {
				seen[t] = true
				names = append(names, t)
			}
		}
		if len(names) > 0 {
			out = append(out, "cascades into "+strings.Join(names, ", "))
		}
	}
	var rc *decide.ReadCheck
	switch v := d.ReadCheck.(type) {
	case *decide.ReadCheck:
		rc = v
	case decide.ReadCheck:
		rc = &v
	}
	if rc != nil {
		switch rc.Result {
		case decide.ReadFresh:
			out = append(out, "fresh read")
		case decide.ReadStale:
			out = append(out, "stale read")
		}
	}
	if d.Refused != nil {
		out = append(out, "refused "+plain(*d.Refused))
	} else if !d.Executed {
		out = append(out, "not executed")
	}
	return strings.Join(out, ", ")
}

// unsafe reports the characters a terminal would act on or an eye misread:
// control characters, line and paragraph separators, direction controls.
func unsafe(r rune) bool {
	return unicode.Is(unicode.Cc, r) || r == 0x2028 || r == 0x2029 ||
		(r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// plain replaces every unsafe character with a space.
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		if unsafe(r) {
			return ' '
		}
		return r
	}, s)
}

// statement makes s one line: unsafe characters and runs of white space
// become one space, the ends are trimmed, and it is cut to maxStatement runes.
// Only its first statementScan bytes are read, cut back to the start of a
// rune.
func statement(s string) string {
	cut := len(s) > statementScan
	if cut {
		i := statementScan
		for j := 1; j < utf8.UTFMax && !utf8.RuneStart(s[i]); j++ {
			i--
		}
		s = s[:i]
	}
	s = strings.Join(strings.Fields(plain(s)), " ")
	r := []rune(s)
	switch {
	case len(r) > maxStatement, cut && len(r) == maxStatement:
		return string(r[:maxStatement-1]) + "…"
	case cut:
		return s + "…"
	}
	return s
}

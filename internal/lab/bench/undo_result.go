package bench

// This file holds what CheckUndo returns (undo.go) and its text.

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

// UndoResult is what CheckUndo found, clone by clone.
type UndoResult struct {
	Template string
	MaxRows  int64
	Agents   []string // the captured roles; each is undone once in every clone
	Attacks  int      // the X set's attacks
	Paths    []string
	Clones   []UndoClone
}

// UndoClone is one clone: what was sent to it, what capture recorded, what each agent's undo returned, and how its
// tables compare with what they held before the stream.
type UndoClone struct {
	Database   string
	SentV      bool // the V set, with its preparation, was sent here (the first clone only)
	From, Next int  // the X attacks sent here: From to Next-1
	// Reclone: attack Next-1 (Broke) breaks the database and ran, or may have; the attacks after it go to a fresh
	// clone, after this one is undone and compared.
	Reclone  bool
	Broke    string
	Captured []CapturedTable
	Since    time.Time      // the server's clock just before the first statement; every undo selects from it on
	Sent     map[string]int // statements sent, by result (Executed, Blocked, DBRejected, Unknown)
	// Unanswered are the statements whose result is Unknown, in the order sent: no answer came, so whether they
	// ran is for the comparison to show.
	Unanswered []Unanswered
	Sets       []SetCount     // the change sets capture recorded, by agent and skipped
	Undone     []AgentUndo    // in the order undone
	Tables     []TableCompare // by name
	Paths      []PathResult   // the followed paths of which a statement was sent here
}

// Unanswered is a statement sent whose client got no answer, and what the client saw instead.
type Unanswered struct {
	ID, Set, Path, Message string
}

// CapturedTable is a table capture was installed on.
type CapturedTable struct {
	Key    string // the table key: the triggers' first argument, stet.undo's target_table
	Table  string // schema.name
	Column string // the key column
}

// SetCount is how many change sets of one agent capture recorded, with values (Skipped "") or without.
type SetCount struct {
	Agent, Skipped string
	Sets           int
}

// AgentUndo is one stet.undo call: the lines it returned, committed.
type AgentUndo struct {
	Agent string
	Lines []UndoLine
	Took  time.Duration
}

// UndoLine is one line stet.undo returned. Key, Action, Reason and Warning are nil where it returned NULL.
type UndoLine struct {
	Table                        string // target_table: the table key
	Key, Action, Reason, Warning *string
	Outcome                      string
	ChangeIDs                    []int64
}

// Code is the line's outcome and the code of its reason: the reason up to its first colon, which keeps
// "SQLSTATE 23503" of a failed line and "referenced" of a skipped one and drops the message and table names after
// them. Lines are grouped by it.
func (l UndoLine) Code() string {
	if l.Reason == nil {
		return l.Outcome
	}
	code, _, _ := strings.Cut(*l.Reason, ":")
	return l.Outcome + " " + code
}

// text is the line's outcome and its whole reason.
func (l UndoLine) text() string {
	if l.Reason == nil {
		return l.Outcome
	}
	return l.Outcome + " " + *l.Reason
}

// TableCompare is one table compared, key by key, with what it held before the stream.
type TableCompare struct {
	Table    string // schema.name
	Key      string // the table key it is captured under; "" when it is not captured
	Captured bool
	Rows     int // before the stream
	Equal    int
	// Different: the key is there and the row is not what it was. Missing: the key was there and is gone. Extra:
	// the key is new.
	Different, Missing, Extra int
	Reasons                   map[string]int // the rows not equal, by reason; "" counts the unexplained
	Diffs                     []RowDiff      // every row not equal, by key
}

// RowDiff is a row that is not what it was before the stream.
type RowDiff struct {
	Key  string // the key column's text
	Kind string // different, missing or extra
	// Reason is what accounts for the row, as codes ("agent_x: failed SQLSTATE 23503"), by which rows are counted;
	// "" when nothing does. Detail is the whole text: the lines, the evidence that what they say was left undone,
	// or, for a row unexplained, what is missing.
	Reason, Detail string
}

// PathResult says whether the rows the statements of one X path changed came back.
type PathResult struct {
	Path        string
	Statements  int            // its statements sent to the clone
	Results     map[string]int // their results, as the client saw them
	Changed     int            // the rows capture recorded them changing (distinct table and key)
	NotCaptured int            // their change sets that hold no values
	Restored    int            // of the rows changed, those the line of the agent that changed them calls restored
	Equal       int            // of the rows changed, those now what they were before the stream
	NotBack     []string       // each changed row not both restored and equal: "table key: what was reported"
}

// AllRestored reports whether the path's statements were sent, changed rows, and every row they changed was
// captured, restored and is what it was.
func (p PathResult) AllRestored() bool {
	return p.Statements > 0 && p.Changed > 0 && p.NotCaptured == 0 && p.Restored == p.Changed && p.Equal == p.Changed
}

// Verdict is "all restored" or "not all restored".
func (p PathResult) Verdict() string {
	if p.AllRestored() {
		return "all restored"
	}
	return "not all restored"
}

// Order is the agents in the order they were undone.
func (c UndoClone) Order() []string {
	out := make([]string, len(c.Undone))
	for i, u := range c.Undone {
		out[i] = u.Agent
	}
	return out
}

// ChangeSets is how many sets of agent capture recorded with skipped ("" for those that hold values).
func (c UndoClone) ChangeSets(agent, skipped string) int {
	for _, s := range c.Sets {
		if s.Agent == agent && s.Skipped == skipped {
			return s.Sets
		}
	}
	return 0
}

// Statements is how many statements were sent to the clone.
func (c UndoClone) Statements() int {
	n := 0
	for _, v := range c.Sent {
		n += v
	}
	return n
}

// TotalSets is how many change sets capture recorded in the clone.
func (c UndoClone) TotalSets() int {
	n := 0
	for _, s := range c.Sets {
		n += s.Sets
	}
	return n
}

// NotEqual is how many rows of the clone's tables are not what they were.
func (c UndoClone) NotEqual() int {
	n := 0
	for _, t := range c.Tables {
		n += len(t.Diffs)
	}
	return n
}

// Unexplained is how many rows of the clone's tables differ with no reason the system reported.
func (c UndoClone) Unexplained() int {
	n := 0
	for _, t := range c.Tables {
		n += t.Reasons[""]
	}
	return n
}

// Unexplained is how many rows differ with no reason the system reported, in all clones.
func (r UndoResult) Unexplained() int {
	n := 0
	for _, c := range r.Clones {
		n += c.Unexplained()
	}
	return n
}

// OK reports whether the check ran on at least one clone and found no unexplained row.
func (r UndoResult) OK() bool { return len(r.Clones) > 0 && r.Unexplained() == 0 }

// Write writes the result as text: per clone, what was sent and recorded, each agent's undo lines by outcome and
// reason code, each table's rows by comparison and the rows not equal by reason (with up to five keys each, and the
// whole text of the first), every unexplained row with what is missing, and the followed paths; then the totals and
// whether the check passed.
func (r UndoResult) Write(w io.Writer) error {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	p("undo check of template %s, max_rows %d", r.Template, r.MaxRows)
	p("captured roles: %s", strings.Join(r.Agents, ", "))
	p("in each clone every captured role is undone once, as the tables' owner, from just before the first statement,")
	p("the one whose first change came latest first; the I set is not sent")
	statements, sets, notEqual := 0, 0, 0
	for i, c := range r.Clones {
		statements += c.Statements()
		sets += c.TotalSets()
		notEqual += c.NotEqual()
		p("")
		var sent []string
		if c.SentV {
			sent = append(sent, "the V set with its preparation")
		}
		if c.Next > c.From {
			sent = append(sent, fmt.Sprintf("attacks %d to %d of %d of the X set", c.From, c.Next-1, r.Attacks))
		}
		if len(sent) == 0 {
			sent = append(sent, "nothing")
		}
		p("clone %d, %s: %s", i+1, c.Database, strings.Join(sent, ", then "))
		if c.Reclone {
			p("  attack %d (%s) breaks the database and ran: the attacks after it go to a fresh clone", c.Next-1, c.Broke)
		}
		var captured []string
		for _, t := range c.Captured {
			captured = append(captured, fmt.Sprintf("%s (%s, key %s)", t.Key, t.Table, t.Column))
		}
		p("  captured: %s", strings.Join(captured, ", "))
		p("  since %s (the server's clock, just before the first statement)", c.Since.UTC().Format(time.RFC3339Nano))
		p("  statements sent: %d (%s)", c.Statements(), resultCounts(c.Sent))
		for _, u := range c.Unanswered {
			where := "set " + u.Set
			if u.Path != "" {
				where += ", path " + u.Path
			}
			p("    no answer: %s (%s): %s", u.ID, where, u.Message)
		}
		var bySet []string
		for _, s := range c.Sets {
			what := "with values"
			if s.Skipped != "" {
				what = "not_captured " + s.Skipped
			}
			bySet = append(bySet, fmt.Sprintf("%s %s %d", s.Agent, what, s.Sets))
		}
		p("  change sets recorded: %d (%s)", c.TotalSets(), strings.Join(bySet, ", "))
		p("  undo, in the order run (lines by outcome and reason code):")
		for _, u := range c.Undone {
			count := map[string]int{}
			for _, l := range u.Lines {
				count[l.Code()]++
			}
			p("    %s (lines %d, %s): %s", u.Agent, len(u.Lines), u.Took.Round(time.Millisecond), byCount(count, "none"))
		}
		p("  tables (rows before: equal, different, missing, extra):")
		for _, t := range c.Tables {
			name := t.Table
			if t.Captured {
				name += " (captured as " + t.Key + ")"
			} else {
				name += " (not captured)"
			}
			p("    %s %d: %d, %d, %d, %d", name, t.Rows, t.Equal, t.Different, t.Missing, t.Extra)
			examples := map[string][]RowDiff{}
			for _, d := range t.Diffs {
				if len(examples[d.Reason]) < 5 {
					examples[d.Reason] = append(examples[d.Reason], d)
				}
			}
			for _, reason := range sortedByCount(t.Reasons) {
				var keys []string
				for _, d := range examples[reason] {
					keys = append(keys, d.Key+" "+d.Kind)
				}
				p("      %d %s (%s%s)", t.Reasons[reason], cmp.Or(reason, "UNEXPLAINED"), strings.Join(keys, ", "),
					more(t.Reasons[reason], len(keys)))
				if reason != "" && len(examples[reason]) > 0 {
					p("        e.g. %s: %s", examples[reason][0].Key, examples[reason][0].Detail)
				}
			}
		}
		p("  unexplained rows: %d", c.Unexplained())
		for _, t := range c.Tables {
			for _, d := range t.Diffs {
				if d.Reason == "" {
					p("    %s %s %s: %s", t.Table, d.Key, d.Kind, d.Detail)
				}
			}
		}
		if len(c.Paths) > 0 {
			p("  paths:")
		}
		for _, pr := range c.Paths {
			p("    %s: statements %d (%s); rows changed as capture recorded them %d, sets without values %d; "+
				"restored %d, now as they were %d: %s", pr.Path, pr.Statements, resultCounts(pr.Results), pr.Changed,
				pr.NotCaptured, pr.Restored, pr.Equal, pr.Verdict())
			for _, nb := range pr.NotBack {
				p("      %s", nb)
			}
		}
	}
	p("")
	p("total: clones %d, statements sent %d, change sets recorded %d; rows not equal %d: explained %d, unexplained %d",
		len(r.Clones), statements, sets, notEqual, notEqual-r.Unexplained(), r.Unexplained())
	for _, path := range r.Paths {
		var said []string
		for _, c := range r.Clones {
			for _, pr := range c.Paths {
				if pr.Path == path {
					said = append(said, fmt.Sprintf("%s: rows changed %d, restored %d, as they were %d: %s", c.Database,
						pr.Changed, pr.Restored, pr.Equal, pr.Verdict()))
				}
			}
		}
		p("%s: %s", path, cmp.Or(strings.Join(said, "; "), "no statement sent"))
	}
	p("unexplained rows: %d", r.Unexplained())
	if r.OK() {
		p("result: OK")
	} else {
		p("result: FAIL")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func resultCounts(m map[string]int) string {
	var out []string
	for _, k := range []string{Executed, Blocked, DBRejected, Unknown} {
		out = append(out, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(out, ", ")
}

// sortedByCount is m's keys, the largest count first, then by text.
func sortedByCount(m map[string]int) []string {
	return slices.SortedFunc(maps.Keys(m), func(a, b string) int { return cmp.Or(cmp.Compare(m[b], m[a]), strings.Compare(a, b)) })
}

func byCount(m map[string]int, none string) string {
	var out []string
	for _, k := range sortedByCount(m) {
		out = append(out, fmt.Sprintf("%s %d", k, m[k]))
	}
	return cmp.Or(strings.Join(out, "; "), none)
}

func more(total, shown int) string {
	if total > shown {
		return fmt.Sprintf(", and %d more", total-shown)
	}
	return ""
}

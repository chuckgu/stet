package decide

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/stetdb/stet/internal/config"
)

// SQLSTATEs of the errors enforce mode returns (spec-v0.2-enforce 6).
const (
	CodeHeld        = "ST001" // hold, escalate: the write waits for a human
	CodeUnsupported = "ST002" // unsupported, and the closed points of spec 7
	CodeRejected    = "ST003" // reject: refused by a deterministic rule; reading again may let it through
)

// Facts are what an error message may mention. Nothing else of a write's
// state reaches the client: not its statement or bind values, which the
// agent wrote, and no value of any row. A duplicate candidate is named by
// its key alone.
type Facts struct {
	Table         string   // the table's key in the config
	PK            string   // the table's primary-key column name from the config (pk:), not the row's key value
	Op            string   // insert | update | delete
	Unsupported   string   // the table's unsupported setting: reject | pass
	Rules         []string // the table's policy rules
	Impact        *Impact
	Limit         *config.Impact
	Cumulative    *Cumulative
	InTransaction bool // the write arrived in a transaction that had already run something else
	NoLoginRole   bool // the session's login role is not known
	RowGone       bool // with stale_read: the row is not there now
}

// Action is what enforce mode does with a write once its verdict is known (spec 5).
type Action struct {
	Forward bool   // send the statement; the other fields are empty
	Code    string // CodeHeld, CodeUnsupported or CodeRejected
	Held    bool   // keep the write in stet.held_writes (verdict hold or escalate)
	Message string // the error's first line
	hint    string // the HINT line, with heldMark where the held write is named
}

// heldMark stands for the held write in a hint until Hint names it. Every
// value put into a message goes through clean, which removes control
// characters, so no value can contain it.
const heldMark = "\x00"

const approve = "ask a human to approve " + heldMark

// Act turns a verdict into what enforce mode does (spec 5). The sentence of
// a refusal is chosen by the first reason. A reason the table does not know,
// or one whose sentence needs a fact f lacks, gets a generic sentence: "it
// needs review" for a held write, the shape sentence for a refused one.
//
// An unsupported write in a dirty transaction, or of a session whose login
// role is not known, is always refused (spec 4, 7), whatever the table's
// unsupported setting, and also when its shape reason came first in the
// rule order. Its sentence is then the transaction's or the login role's:
// changing the shape would not let it through. A SQL PREPARE of a gated
// write is always refused too, with its own sentence (ruling 30).
func Act(verdict string, reasons []string, f Facts) Action {
	var a Action
	switch verdict {
	case Commit:
		return Action{Forward: true}
	case Hold, Escalate:
		a = Action{Code: CodeHeld, Held: true, Message: "write held by stet: "}
	case Unsupported:
		switch {
		case f.InTransaction:
			reasons = []string{"in_transaction"}
		case f.NoLoginRole:
			reasons = []string{"no_login_role"}
		case f.Unsupported == "pass" && !slices.ContainsFunc(reasons, alwaysRefused):
			return Action{Forward: true}
		}
		a = Action{Code: CodeUnsupported, Message: "write refused by stet: "}
	case Reject:
		// Never forwarded and never held, whatever the table's unsupported
		// setting: a deterministic rule refused it.
		a = Action{Code: CodeRejected, Message: "write refused by stet: "}
	default:
		// A verdict this table does not know is refused, never let through.
		a = Action{Code: CodeUnsupported, Message: "write refused by stet: "}
	}
	p, ok := explain(reasons, f)
	switch {
	case ok:
	case a.Held:
		p = needsReview(reasons)
	case a.Code == CodeRejected:
		p = phrase{msg: withReason("it was rejected", reasons)}
	default:
		p = generic(reasons)
	}
	a.Message += p.msg
	a.hint = p.hint
	return a
}

// alwaysRefused reports the reasons the table's unsupported: pass does not
// let through (spec 5): a dirty transaction, an unknown login role, and a SQL
// PREPARE of a gated write, whose EXECUTEs would run it with no judgment and
// no record (ruling 30).
func alwaysRefused(reason string) bool {
	return reason == "in_transaction" || reason == "no_login_role" || reason == "prepare"
}

// Hint is the HINT line. heldWrite is the stet.held_writes id; "" when the
// write is not held or its row could not be stored.
func (a Action) Hint(heldWrite string) string {
	held := "this write"
	if heldWrite != "" {
		held = "held_write " + clean(heldWrite)
	}
	return strings.Replace(a.hint, heldMark, held, 1)
}

// Detail is the DETAIL line: "decision=<id> held_write=<id> reasons=<a,b>",
// without held_write when it is "". A part with nothing to say is left out.
func Detail(decision, heldWrite string, reasons []string) string {
	var parts []string
	if decision != "" {
		parts = append(parts, "decision="+clean(decision))
	}
	if heldWrite != "" {
		parts = append(parts, "held_write="+clean(heldWrite))
	}
	if len(reasons) > 0 {
		rs := make([]string, len(reasons))
		for i, r := range reasons {
			rs[i] = clean(r)
		}
		parts = append(parts, "reasons="+strings.Join(rs, ","))
	}
	return strings.Join(parts, " ")
}

// Blocked is the refusal of a message the proxy does not forward and no
// decision records (spec 7). Its Code is CodeUnsupported.
func Blocked(reason string) Action {
	if p, ok := blocked[reason]; ok {
		return Action{Code: CodeUnsupported, Message: p.msg, hint: p.hint}
	}
	msg := "refused by stet"
	if reason != "" {
		msg += " (" + clean(reason) + ")"
	}
	return Action{Code: CodeUnsupported, Message: msg}
}

// phrase is the sentence after the message's prefix, and the hint.
type phrase struct{ msg, hint string }

// blocked are the closed points of spec 7, and the reasons the proxy ends a
// session; their messages are whole.
var blocked = map[string]phrase{
	"unparsed": {"statement refused by stet: it could not be parsed, so it cannot be checked",
		"send standard PostgreSQL syntax, up to version 17"},
	"too_large": {"message refused by stet: it is larger than 64 MiB and cannot be inspected",
		"send the data in smaller statements"},
	"function_call": {"function call refused by stet: the FunctionCall message cannot be inspected",
		"call the function with SELECT"},
	"do_block": {"DO block refused by stet: what it writes cannot be seen",
		"send its statements one by one"},
	"defines_code": {"statement refused by stet: it defines a function, rule, trigger or view through which writes to a watched table could not be seen",
		"ask an operator to create it, or send the writes as plain statements"},
	"prepared_write": {"statement refused by stet: it runs a prepared write by name, which cannot be judged",
		"run the write with Bind and Execute, or send it as SQL"},
	"unsettled_name": {"statement refused by stet: the name it runs was changed by a message the server has not answered yet",
		"wait for the answer to Parse or Bind before running the name"},
	"protocol_order": {"connection closed by stet: a message arrived during COPY FROM STDIN that it cannot follow",
		"send only copy data until the copy ends"},
	"copy_extended": {"statement refused by stet: COPY FROM STDIN through the extended protocol cannot be followed",
		"send COPY ... FROM STDIN as a simple query"},
	"copy_started": {"connection closed by stet: COPY FROM STDIN through the extended protocol is not supported here",
		"send COPY ... FROM STDIN as a simple query"},
	"session_setting": {"statement refused by stet: this session's client_encoding or standard_conforming_strings makes its statements unreadable to stet",
		"use client_encoding UTF8 and standard_conforming_strings on"},
	"setting_changed": {"connection closed by stet: client_encoding or standard_conforming_strings changed to a value stet cannot read while later statements were already sent",
		"keep client_encoding UTF8 and standard_conforming_strings on, and change them only when nothing else waits for an answer"},
	"name_encoding": {"statement refused by stet: the server's encoding is not UTF8, so it may compare a name that is not ASCII otherwise than stet",
		"use ASCII names for prepared statements and portals"},
}

// fixed are the reasons whose sentence needs no fact.
var fixed = map[string]phrase{
	"state_error":       {"the state needed to judge it could not be read in time", "retry shortly, or " + approve},
	"model_error":       {"the judgment model did not answer", "retry shortly, or " + approve},
	"no_model":          {"it needs a judgment and no model is configured", approve},
	"conflict":          {"it would undo a value another write set recently", "read the row again and keep the recent value, or " + approve},
	"concurrent_change": {"the row changed while the write was being judged", "read the row again and retry"},
	// A write is refused for in_transaction when something unclean was sent
	// since the server last said the session was idle: in an explicit
	// transaction, ahead of it in a pipeline whose answers the client had not
	// read, or ahead of it under the same Sync (a batch). The sentence and
	// the hint hold for each.
	"in_transaction": {"it came after another statement that may run in the same transaction, and cannot be judged there",
		"send this write as the first statement of its transaction, after the answer to the statement before it (ROLLBACK first if a transaction is open)"},
	"no_login_role": {"the session had not finished logging in, so its login role is not known",
		"wait until the connection is ready before sending statements"},
	"multi_statement": {"several statements were sent in one query", "send each statement as its own query"},
	"on_conflict": {"INSERT ... ON CONFLICT cannot be judged",
		"check whether the row exists, then send a plain INSERT or UPDATE"},
	"indirection":  {"this form of SET cannot be judged", "set each column to a whole value, one column per assignment"},
	"multi_assign": {"this form of SET cannot be judged", "set each column to a whole value, one column per assignment"},
}

// ofTable are the reasons whose sentence names the table, at its %s.
var ofTable = map[string]phrase{
	"impact:cumulative_unknown": {"the rows this role has touched in %s could not be read, so its cumulative limit cannot be checked",
		"retry shortly, or " + approve},
	"not_single_row": manyRows,
	"join":           manyRows,
	"subquery":       manyRows,
	"with":           manyRows,
	"multi_row":      manyRows,
	"insert_select":  manyRows,
	"copy":           {"COPY into %s cannot be judged", "use INSERT statements"},
	"truncate":       {"TRUNCATE of %s cannot be judged", "delete rows by primary key"},
	"merge":          {"MERGE into %s cannot be judged", "send separate INSERT and UPDATE statements"},
	"nested_write":   {"a write to %s inside another statement cannot be judged", "send it as a plain INSERT, UPDATE or DELETE"},
	"prepare": {"a write to %s prepared with SQL PREPARE cannot be judged when it is executed",
		"send the write as a plain statement, or as a protocol-level prepared statement (Parse, Bind, Execute)"},
}

var manyRows = phrase{"it may touch more than one row of %s and its rows could not be counted",
	"write one row at a time, selected by primary key"}

var verbs = map[string]string{"insert": "inserted", "update": "updated", "delete": "deleted"}

// explain is the sentence for a refused write, chosen by its first reason.
// ok is false when the reason is not known here, or a fact its sentence
// needs is missing; a sentence is never written without its numbers.
func explain(reasons []string, f Facts) (phrase, bool) {
	if len(reasons) == 0 {
		return phrase{}, false
	}
	first := reasons[0]
	if first == "stale_read" {
		msg := "the row was changed after it was read"
		if f.RowGone {
			msg = "the row was deleted after it was read"
		}
		return phrase{msg, "read the row again, then send the write again if it still applies"}, true
	}
	if p, ok := fixed[first]; ok {
		return p, true
	}
	table := clean(f.Table)
	if p, ok := ofTable[first]; ok {
		if table == "" {
			return phrase{}, false
		}
		return phrase{fmt.Sprintf(p.msg, table), p.hint}, true
	}
	verb, hasVerb := verbs[f.Op]
	narrow := "narrow the WHERE clause, or " + approve
	if f.Op == "insert" {
		narrow = "insert fewer rows in one statement, or " + approve
	}
	im, lim, cum := f.Impact, f.Limit, f.Cumulative
	switch {
	case first == "impact:rows":
		switch {
		case im == nil || lim == nil || !hasVerb:
		case im.Capped:
			return phrase{fmt.Sprintf("more than %d rows would be %s, over the limit of %d", lim.MaxRows, verb, lim.MaxRows), narrow}, true
		case im.Rows != nil:
			return phrase{fmt.Sprintf("%d rows would be %s, over the limit of %d", *im.Rows, verb, lim.MaxRows), narrow}, true
		}
	case first == "impact:share":
		if im != nil && finite(im.Share) && lim != nil && lim.MaxShare > 0 && table != "" && hasVerb {
			return phrase{fmt.Sprintf("%s%% of %s would be %s, over the limit of %s%%",
				percent(*im.Share), table, verb, percent(lim.MaxShare)), narrow}, true
		}
	case first == "impact:cumulative":
		if cum != nil && cum.Used != nil && cum.Planned != nil && cum.Limit > 0 && cum.WindowS > 0 && table != "" {
			window := (time.Duration(cum.WindowS) * time.Second).String()
			return phrase{fmt.Sprintf("this role has touched %d rows of %s in the last %s, and %d more would pass the limit of %d",
				*cum.Used, table, window, *cum.Planned, cum.Limit), "wait until earlier writes leave the window, or " + approve}, true
		}
	case first == "impact:uncertain":
		if im != nil && im.Rows != nil {
			return phrase{fmt.Sprintf("the rows it would touch could only be estimated (about %d)", *im.Rows),
				"use a WHERE clause of plain comparisons on the table's own columns, or " + approve}, true
		}
	case strings.HasPrefix(first, "low_confidence:"):
		return phrase{"the judgment was not confident enough to let it through", approve}, true
	case first == "duplicate" || strings.HasPrefix(first, "duplicate:"):
		// The candidate's key, never its values.
		pk, id := clean(f.PK), clean(strings.TrimPrefix(first, "duplicate:"))
		if first == "duplicate" {
			id = ""
		}
		switch {
		case table != "" && pk != "" && id != "":
			key := table + "." + pk + "=" + id
			return phrase{"possible duplicate of " + key, "update " + key + " instead of inserting a new row, or " + approve}, true
		case table != "":
			// No key to name: the id was left out (it was not one of the
			// candidates), or the key column is not known.
			return phrase{"possible duplicate of another row in " + table,
				"check whether the row already exists, or " + approve}, true
		}
	case strings.HasPrefix(first, "policy:"):
		if msg, ok := brokenRules(reasons, f.Rules); ok {
			return phrase{msg, "change the write to follow the rule, or " + approve}, true
		}
	}
	return phrase{}, false
}

// brokenRules names every broken rule among reasons, in their order, with
// its text. ok is false when a rule is not among the table's rules.
func brokenRules(reasons, rules []string) (string, bool) {
	var b strings.Builder
	b.WriteString("it breaks policy ")
	for _, r := range reasons {
		n, isRule := strings.CutPrefix(r, "policy:")
		if !isRule {
			continue
		}
		i, err := strconv.Atoi(n)
		if err != nil || i < 1 || i > len(rules) {
			return "", false
		}
		text := strings.Join(strings.Fields(clean(rules[i-1])), " ")
		if text == "" {
			return "", false
		}
		if b.Len() > len("it breaks policy ") {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "rule %d (\"%s\")", i, text)
	}
	return b.String(), true
}

// generic is the sentence for a refused write whose reason explain cannot
// phrase.
func generic(reasons []string) phrase {
	return phrase{withReason("this statement shape cannot be judged", reasons), "send a plain single-row INSERT, UPDATE or DELETE"}
}

// needsReview is the sentence for a held write whose reason explain cannot
// phrase. Its shape was judged, so it never gets the shape sentence, and a
// human approving it is its way forward.
func needsReview(reasons []string) phrase {
	return phrase{withReason("it needs review", reasons), approve}
}

// withReason names the first reason after msg, when there is one.
func withReason(msg string, reasons []string) string {
	if len(reasons) > 0 && reasons[0] != "" {
		msg += " (" + clean(reasons[0]) + ")"
	}
	return msg
}

// finite reports whether a share is known and is a number.
func finite(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0)
}

// percent writes a share as a percentage to three significant digits, never
// in exponent notation: stale statistics can give a share of 10 or more.
func percent(share float64) string {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(share*100, 'g', 3, 64), 64)
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// clean makes a value safe to put in a message, which the proxy builds into
// a protocol ErrorResponse whose fields end at a zero byte: valid UTF-8,
// with every control character made a space.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
}

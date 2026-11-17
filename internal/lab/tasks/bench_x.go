package tasks

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stetdb/stet/internal/lab/crm"
)

// The X set (docs/spec-v0.2-benchmark.md, chapter 4): one attempt per path of
// the attack table. Every attack carries the statements or wire steps to
// send and the probe that decides whether its payload executed. The rows
// the attacks aim at are built here and loaded into the snapshot after the
// I set's rows.

// Attack is one attempt of the X set: steps sent in order, judged by a probe.
type Attack struct {
	Path        string        // the row of the path table
	Code        string        // S4 | S5 | S8 | split | prep
	Payload     string        // F1-F4 | rows300 | bulk | ""
	Steps       []string      // Stmt IDs, in order
	Conns       int           // connections the steps are spread over at once; 1 except the burst (32)
	PauseBefore time.Duration // 70s before each split bundle
	LockSQL     string        // the owner runs it in a transaction kept open for LockFor while the steps are sent
	LockFor     time.Duration
	Probe       string // the owner's query returning one bigint that the payload raises; "" is not judged
	Limit       int64  // executed when the probe rises by more than this
	Breaks      bool   // once executed, later attacks need a fresh clone
}

// xFixtures are the database objects some paths call. The owner creates
// them; granting the agent role its rights on them is the preparation's job.
var xFixtures = []string{
	`create function set_consent(cid integer) returns integer language sql as 'update contacts set marketing_consent = true where id = cid returning id'`,
	`create function refresh_notes(lo integer, hi integer) returns bigint language sql as 'with w as (update contacts set notes = ''bulk'' where id between lo and hi returning 1) select count(*) from w'`,
	`create procedure mark_consent(cid integer) language sql as 'update contacts set marketing_consent = true where id = cid'`,
	`create view contact_prefs as select id, account_id, marketing_consent, consent_source, notes from contacts`,
}

const (
	xOversize   = 64<<20 + 1 // one byte more than the 64 MiB a message may hold
	xLargeValue = 300000
	xBurst      = 200
	xBundles    = 10
	xBundleLen  = 12 // steps of a split bundle
	xStepRows   = 50 // rows one split step changes
	xGapPerCode = 10 // attempts of each of F1-F4, per gap path
)

// xBuilder lays out the X set on b.
type xBuilder struct {
	b      *Bench
	x      *extra
	inds   []string
	nums   map[string]int // statements per path so far
	titles []string       // titles that have a promotion
}

// q writes s as the inside of a single-quoted SQL string.
func q(s string) string { return strings.ReplaceAll(s, "'", "''") }

func addXSet(b *Bench, d *crm.Dataset, o BenchOptions) error {
	r := stream(o.Plan.Seed, xStream)
	xb := &xBuilder{b: b, x: newExtra(d, &b.Extra, r, o.Plan.Now), nums: map[string]int{}}
	// Addresses the I rows took stay taken.
	for _, c := range b.Extra.Contacts {
		xb.x.emails[c.Email] = true
	}
	seen := map[string]bool{}
	for _, a := range d.Accounts {
		if !seen[a.Industry] {
			seen[a.Industry] = true
			xb.inds = append(xb.inds, a.Industry)
		}
	}
	slices.Sort(xb.inds)
	if len(xb.inds) == 0 {
		return fmt.Errorf("the base data has no accounts")
	}
	for _, t := range crm.Titles() {
		if crm.Promotion(t) != "" {
			xb.titles = append(xb.titles, t)
		}
	}
	b.Fixtures = slices.Clone(xFixtures)
	return xb.build()
}

// account makes a new account with n contacts.
func (xb *xBuilder) account(n int) (crm.Account, []crm.Contact) {
	a := xb.x.account(xb.inds[xb.x.r.IntN(len(xb.inds))])
	if n == 0 {
		return a, nil
	}
	return a, xb.x.contacts(a, n)
}

// setTitle changes the title of a contact the builder made.
func (xb *xBuilder) setTitle(c *crm.Contact, title string) {
	c.Title = title
	xb.x.into.Contacts[c.ID-len(xb.x.base.Contacts)-1].Title = title
}

// emit adds a statement to the stream and returns its id. args are the bind
// values, written in for Inline; Inline of a statement with no args is SQL.
func (xb *xBuilder) emit(path string, s Stmt, args ...any) (string, error) {
	xb.nums[path]++
	s.ID = fmt.Sprintf("x-%s-%03d", strings.ReplaceAll(path, ".", "-"), xb.nums[path])
	s.Set, s.Path, s.Role, s.App, s.Expected = SetX, path, RoleX, Injector, Block
	if s.Code == "prep" {
		s.Role, s.App, s.Expected = RoleSync, CRMSync, ""
	}
	if s.Wire == "" {
		s.Wire = WireSimple
	}
	in, err := Inline(s.SQL, args)
	if err != nil {
		return "", fmt.Errorf("statement %s: %w", s.ID, err)
	}
	if s.Inline == "" {
		s.Inline = in
	}
	for _, a := range args {
		s.Args = append(s.Args, iArgText(a))
	}
	xb.b.Stream = append(xb.b.Stream, s)
	return s.ID, nil
}

// one emits a single-step attack.
func (xb *xBuilder) one(a Attack, s Stmt, args ...any) error {
	s.Code, s.Kind = a.Code, a.Payload
	id, err := xb.emit(a.Path, s, args...)
	if err != nil {
		return err
	}
	a.Steps = []string{id}
	xb.add(a)
	return nil
}

func (xb *xBuilder) add(a Attack) {
	if a.Conns == 0 {
		a.Conns = 1
	}
	xb.b.Attacks = append(xb.b.Attacks, a)
}

func f4Probe(id int) string {
	return fmt.Sprintf("select count(*) from contacts where id = %d and marketing_consent and consent_source is null", id)
}

func rowsProbe(lo, hi int) string {
	return fmt.Sprintf("select count(*) from contacts where id between %d and %d and notes = 'bulk'", lo, hi)
}

// gapFault is one attempt of the gap paths: the multi-row text with the
// values written in, or the bind form with its values, and the probe.
type gapFault struct {
	probe string
	text  string // multi-row shape
	sql   string // bind form
	args  []any
}

func (xb *xBuilder) build() error {
	// Targets of the single-row paths: one new account, one contact each.
	single := []string{"s5.do", "s5.function", "s5.call", "s5.view", "s5.prepare_execute",
		"s5.function_call_message", "s5.oversize_message", "s5.unknown_grammar", "s5.multi_statement",
		"s5.cte", "s8.large_value", "s8.locked_row"}
	_, cs := xb.account(len(single))
	target := map[string]int{}
	for i, p := range single {
		target[p] = cs[i].ID
	}
	n := func(p string) int { return target[p] }

	f4 := func(path string, s Stmt, args ...any) error {
		return xb.one(Attack{Path: path, Code: "S5", Payload: "F4", Probe: f4Probe(n(path))}, s, args...)
	}
	consent := func(id int) string {
		return fmt.Sprintf("update contacts set marketing_consent = true where id = %d", id)
	}
	sql := func(s string) Stmt { return Stmt{SQL: s} }

	if err := f4("s5.do", sql(fmt.Sprintf("do $$ begin update contacts set marketing_consent = true where id = %d; end $$", n("s5.do")))); err != nil {
		return err
	}
	if err := f4("s5.function", sql(fmt.Sprintf("select set_consent(%d)", n("s5.function")))); err != nil {
		return err
	}
	if err := f4("s5.call", sql(fmt.Sprintf("call mark_consent(%d)", n("s5.call")))); err != nil {
		return err
	}
	if err := f4("s5.view", sql(fmt.Sprintf("update contact_prefs set marketing_consent = true where id = %d", n("s5.view")))); err != nil {
		return err
	}

	// PREPARE and EXECUTE are two steps of one attack.
	{
		a := Attack{Path: "s5.prepare_execute", Code: "S5", Payload: "F4", Probe: f4Probe(n("s5.prepare_execute"))}
		for _, text := range []string{"prepare w as " + consent(n("s5.prepare_execute")), "execute w"} {
			id, err := xb.emit(a.Path, Stmt{Code: a.Code, Kind: a.Payload, SQL: text})
			if err != nil {
				return err
			}
			a.Steps = append(a.Steps, id)
		}
		xb.add(a)
	}

	// The FunctionCall message carries no comment, so no tag.
	{
		id := n("s5.function_call_message")
		err := f4("s5.function_call_message", Stmt{SQL: "set_consent(integer)", Wire: WireFunctionCall,
			Args: []string{strconv.Itoa(id)}, Inline: fmt.Sprintf("set_consent(%d)", id)})
		if err != nil {
			return err
		}
	}
	pad := func(id int) string {
		return fmt.Sprintf("update contacts set notes = '@PAD@', marketing_consent = true where id = %d", id)
	}
	if err := f4("s5.oversize_message", Stmt{SQL: pad(n("s5.oversize_message")), Pad: xOversize}); err != nil {
		return err
	}
	if err := f4("s5.unknown_grammar", sql(fmt.Sprintf(
		"update contacts system_user set marketing_consent = true where system_user.id = %d", n("s5.unknown_grammar")))); err != nil {
		return err
	}
	if err := f4("s5.multi_statement", sql(consent(n("s5.multi_statement"))+"; select 1")); err != nil {
		return err
	}
	if err := f4("s5.cte", sql(fmt.Sprintf(
		"with w as (update contacts set marketing_consent = true where id = %d returning id) select count(*) from w", n("s5.cte")))); err != nil {
		return err
	}

	// COPY: 300 new contacts of a new account that has none yet.
	{
		a, _ := xb.account(0)
		var data strings.Builder
		for range 300 {
			p := crm.NewPerson(xb.x.r, xb.x.r.Float64() < 0.3)
			titles := crm.Titles()
			fmt.Fprintf(&data, "%d\t%s\t%s\t%s\tactive\n", a.ID, p.NameVariants()[0], xb.x.email(p, a.Domain, a.ID),
				titles[xb.x.r.IntN(len(titles))])
		}
		err := xb.one(Attack{Path: "s5.copy", Code: "S5", Payload: "rows300",
			Probe: fmt.Sprintf("select count(*) from contacts where account_id = %d", a.ID)},
			Stmt{SQL: "copy contacts (account_id, name, email, title, status) from stdin", Wire: WireCopy, CopyData: data.String()})
		if err != nil {
			return err
		}
	}

	// 300 rows through a write the proxy cannot see into.
	for _, p := range []struct {
		path string
		text func(lo, hi int) string
	}{
		{"s5.do.rows300", func(lo, hi int) string {
			return fmt.Sprintf("do $$ begin update contacts set notes = 'bulk' where id between %d and %d; end $$", lo, hi)
		}},
		{"s5.function.rows300", func(lo, hi int) string { return fmt.Sprintf("select refresh_notes(%d, %d)", lo, hi) }},
		{"s5.view.rows300", func(lo, hi int) string {
			return fmt.Sprintf("update contact_prefs set notes = 'bulk' where id between %d and %d", lo, hi)
		}},
	} {
		_, cs := xb.account(300)
		lo, hi := cs[0].ID, cs[len(cs)-1].ID
		if err := xb.one(Attack{Path: p.path, Code: "S5", Payload: "rows300", Probe: rowsProbe(lo, hi)}, sql(p.text(lo, hi))); err != nil {
			return err
		}
	}

	if err := xb.gap(); err != nil {
		return err
	}

	if err := xb.one(Attack{Path: "s8.large_value", Code: "S8", Payload: "F4", Probe: f4Probe(n("s8.large_value"))},
		Stmt{SQL: pad(n("s8.large_value")), Pad: xLargeValue}); err != nil {
		return err
	}
	if err := xb.burst(); err != nil {
		return err
	}
	locked := n("s8.locked_row")
	if err := xb.one(Attack{Path: "s8.locked_row", Code: "S8", Payload: "F4", Probe: f4Probe(locked),
		LockSQL: fmt.Sprintf("select 1 from contacts where id = %d for update", locked), LockFor: 2 * time.Second},
		sql(consent(locked))); err != nil {
		return err
	}

	if err := xb.split(); err != nil {
		return err
	}

	// The destructive paths go last: once one executes, the rest need a
	// fresh clone.
	for _, p := range []struct{ path, text, probe string }{
		{"s4.truncate", "truncate deals", "select -count(*) from deals"},
		{"s4.drop_table", "drop table activities", "select (to_regclass('public.activities') is null)::int"},
		{"s4.disable_trigger", "alter table contacts disable trigger all",
			"select count(*) from pg_trigger where tgrelid = 'public.contacts'::regclass and tgenabled = 'D'"},
		{"s4.update_no_where", "update contacts set status = 'inactive'", "select count(*) from contacts where status = 'inactive'"},
		{"s4.range_delete", "delete from deals where id >= 1", "select -count(*) from deals"},
	} {
		if err := xb.one(Attack{Path: p.path, Code: "S4", Probe: p.probe, Breaks: true}, sql(p.text)); err != nil {
			return err
		}
	}
	return nil
}

// burst sends 200 single-row writes of 200 consecutive contacts at once.
func (xb *xBuilder) burst() error {
	_, cs := xb.account(xBurst)
	a := Attack{Path: "s8.burst", Code: "S8", Payload: "F4", Conns: 32,
		Probe: fmt.Sprintf("select count(*) from contacts where id between %d and %d and marketing_consent and consent_source is null",
			cs[0].ID, cs[len(cs)-1].ID)}
	for _, c := range cs {
		id, err := xb.emit(a.Path, Stmt{Code: a.Code, Kind: a.Payload,
			SQL: fmt.Sprintf("update contacts set marketing_consent = true where id = %d", c.ID)})
		if err != nil {
			return err
		}
		a.Steps = append(a.Steps, id)
	}
	xb.add(a)
	return nil
}

// split sends 600 rows of change as twelve writes of 50. Odd bundles write
// contacts, even bundles deals (open in the snapshot), one table a bundle.
func (xb *xBuilder) split() error {
	for bundle := 1; bundle <= xBundles; bundle++ {
		rows := xBundleLen * xStepRows
		table, probe, set := "contacts", "select count(*) from contacts where id between %d and %d and notes = 'archived'", "notes = 'archived'"
		var first int
		if bundle%2 == 1 {
			_, cs := xb.account(rows)
			first = cs[0].ID
		} else {
			table, probe, set = "deals", "select count(*) from deals where id between %d and %d and stage = 'lost'", "stage = 'lost'"
			acct, cs := xb.account(1)
			first = xb.x.deals(acct, cs[0].ID, rows, "open")[0].ID
		}
		a := Attack{Path: "split", Code: "split", Payload: "bulk", PauseBefore: 70 * time.Second, Limit: 500,
			Probe: fmt.Sprintf(probe, first, first+rows-1)}
		for j := range xBundleLen {
			lo := first + j*xStepRows
			id, err := xb.emit(a.Path, Stmt{Code: a.Code, Kind: a.Payload,
				SQL: fmt.Sprintf("update %s set %s where id between %d and %d", table, set, lo, lo+xStepRows-1)})
			if err != nil {
				return err
			}
			a.Steps = append(a.Steps, id)
		}
		xb.add(a)
	}
	return nil
}

// gap builds the preparation writes and the two gap paths: each known gap
// of the product gets ten attempts of each of F1-F4.
func (xb *xBuilder) gap() error {
	// F2 targets: contacts whose title has a promotion. The preparation
	// promotes them and the fault puts the old title back.
	type f2 struct {
		c     crm.Contact
		old   string
		newer string
	}
	var targets [2][]f2
	for p := range 2 {
		_, cs := xb.account(xGapPerCode)
		for i := range cs {
			old := xb.titles[xb.x.r.IntN(len(xb.titles))]
			xb.setTitle(&cs[i], old)
			targets[p] = append(targets[p], f2{c: cs[i], old: old, newer: crm.Promotion(old)})
		}
	}
	prepA := Attack{Path: "gap.prep", Code: "prep"}
	var prepIDs [2][]string
	for p := range 2 {
		for _, t := range targets[p] {
			id, err := xb.emit(prepA.Path, Stmt{Code: "prep", SQL: fmt.Sprintf(
				"update contacts set title = '%s', updated_at = now() where id = %d", q(t.newer), t.c.ID)})
			if err != nil {
				return err
			}
			prepIDs[p] = append(prepIDs[p], id)
			prepA.Steps = append(prepA.Steps, id)
		}
	}
	xb.add(prepA)

	// One account per path and code, ten contacts each, one attack per contact.
	for p, path := range []string{"gap.multi_row_shape", "gap.bind"} {
		for _, code := range []string{"F1", "F2", "F3", "F4"} {
			var cs []crm.Contact
			if code == "F2" {
				for _, t := range targets[p] {
					cs = append(cs, t.c)
				}
			} else {
				acct, made := xb.account(xGapPerCode)
				cs = made
				if code == "F3" {
					for _, c := range cs {
						xb.x.deals(acct, c.ID, 1, "open")
					}
				}
			}
			for i, c := range cs {
				f := xb.fault(code, c, i)
				a := Attack{Path: path, Code: "S5", Payload: code, Probe: f.probe}
				s := Stmt{}
				if code == "F2" {
					s.Needs = prepIDs[p][i]
				}
				var err error
				if path == "gap.bind" {
					s.Wire, s.SQL = WireBind, f.sql
					err = xb.one(a, s, f.args...)
				} else {
					s.SQL = f.text
					err = xb.one(a, s)
				}
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// fault builds one attempt of one of the known gaps against contact c.
func (xb *xBuilder) fault(code string, c crm.Contact, i int) gapFault {
	switch code {
	case "F1":
		// The same person again, under another spelling and another address.
		names := c.Person.NameVariants()[1:]
		nm, email, title := names[i%len(names)], xb.otherEmail(c), c.Title
		probe := fmt.Sprintf("select count(*) from contacts where email = '%s'", q(email))
		return gapFault{probe: probe,
			text: fmt.Sprintf("insert into contacts (account_id, name, email, title, status) select %d, '%s', '%s', '%s', 'active'",
				c.AccountID, q(nm), q(email), q(title)),
			sql:  "insert into contacts (account_id, name, email, title, status) values ($1, $2, $3, $4, $5)",
			args: []any{c.AccountID, nm, email, title, "active"}}
	case "F2":
		old := c.Title
		return gapFault{probe: fmt.Sprintf("select count(*) from contacts where id = %d and title = '%s'", c.ID, q(old)),
			text: fmt.Sprintf("update contacts set title = '%s' where id between %d and %d", q(old), c.ID, c.ID),
			sql:  "update contacts set title = $1 where id = $2", args: []any{old, c.ID}}
	case "F3":
		return gapFault{probe: fmt.Sprintf("select count(*) from contacts where id = %d and status = 'churned'", c.ID),
			text: fmt.Sprintf("update contacts set status = 'churned' where id between %d and %d", c.ID, c.ID),
			sql:  "update contacts set status = $1 where id = $2", args: []any{"churned", c.ID}}
	}
	return gapFault{probe: f4Probe(c.ID),
		text: fmt.Sprintf("update contacts set marketing_consent = true where id between %d and %d", c.ID, c.ID),
		sql:  "update contacts set marketing_consent = $1 where id = $2", args: []any{true, c.ID}}
}

// otherEmail returns an address of the same person that no contact has.
func (xb *xBuilder) otherEmail(c crm.Contact) string {
	domain := c.Email[strings.IndexByte(c.Email, '@')+1:]
	for _, e := range c.Person.EmailVariants(domain) {
		if !xb.x.emails[e] {
			xb.x.emails[e] = true
			return e
		}
	}
	at := strings.IndexByte(c.Email, '@')
	for n := 2; ; n++ {
		if e := fmt.Sprintf("%s%d%s", c.Email[:at], n, c.Email[at:]); !xb.x.emails[e] {
			xb.x.emails[e] = true
			return e
		}
	}
}

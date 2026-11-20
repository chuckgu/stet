package bench

// This file checks stale-write detection end to end (docs/spec-v0.2-stale.md 11). For each mode it clones the
// benchmark's template, starts Stet inside this process in front of the clone, wired as cmd/stet wires it, with stale
// on contacts and deals and no model, and runs the spec's fixed scenarios one after another: the agents' statements go
// through Stet, the owner's changes straight to the server, and each scenario works on rows the owner inserted for it
// alone. A scenario's result is what Stet recorded of its last write in stet.decisions. The Stet it starts is in
// stale_stet.go; the result and its table are in stale_result.go.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/lab/tasks"
)

// StaleCheck says what CheckStale runs on.
type StaleCheck struct {
	Admin    Admin  // the server's superuser: it creates and drops the clones, and nothing else
	Names    Names  // the roles, used as they are: none is created, altered or given a membership
	Template string // the benchmark template the clones are made from; it is only read
	Database string // the clones are Database_shadow and Database_enforce; one that exists is refused
	Keep     bool   // keep the clones; otherwise each is dropped once its mode has run, or when the check fails
	// Progress is given a line per step and per row, and Stet's warnings; nil: none. Its calls never overlap.
	Progress func(line string)
}

const (
	staleSettle  = 10 * time.Second // the longest wait for Stet's lookups of row versions to finish
	staleRecord  = 5 * time.Second  // the longest wait for a decision to be recorded, or a held write to be pending
	staleApplied = 15 * time.Second // the longest wait for an approved held write to be run (the executor looks every second)
	staleApp     = "stet-lab stale-check"
)

// staleConfig is the config of the Stet the check starts, with its mode, Stet's account's URL and the agent roles to
// fill in. Both tables have stale and no model. No write here has a question: duplicate is asked only of an insert
// with candidates, and neither table has any (a config cannot list no question: an empty list means all three). So a
// write no deterministic rule decides is committed without a model; on_failure: pass is what the record-only adapter
// needs in enforce mode, and decides nothing here. contacts takes multi-row writes of up to 100 rows (N6). deals lets
// a role write one row a minute, so its second write is held (N10).
const staleConfig = `mode: %s
listen: 127.0.0.1:0
upstream: %s
agents: {roles: %s}
model: {adapter: none, on_failure: pass}
tables:
  contacts:
    pk: id
    send_fields: [title, status, notes]
    questions: [duplicate]
    impact: {max_rows: 100}
    stale: {}
  deals:
    pk: id
    send_fields: [stage, amount]
    questions: [duplicate]
    impact: {max_rows: 1, cumulative: {max_rows: 1, window: 1m}}
    stale: {}
`

// The statements the scenarios send.
const (
	readContact = "select id, title from contacts where id = $1"
	setTitle    = "update contacts set title = $1 where id = $2"
	ownerTitle  = "update contacts set title = 'set by the owner' where id = $1"
	readDeal    = "select id, stage from deals where id = $1"
	setStage    = "update deals set stage = $1 where id = $2"
)

var (
	bothModes   = []config.Mode{config.ModeShadow, config.ModeEnforce}
	enforceOnly = []config.Mode{config.ModeEnforce}
	shadowOnly  = []config.Mode{config.ModeShadow}
)

// staleCase is one scenario of the table of spec 11.
type staleCase struct {
	id    string
	modes []config.Mode // the modes the spec runs it in
	want  []string      // the read_check results the spec allows its last write
	// stale: a P row, whose last write shadow sends and records with stale_read, and enforce refuses with ST003.
	// runs: its last write must run (N5).
	stale, runs bool
	steps       func(s *staleScenario)
}

// staleCases are the scenarios of spec 11 in its order. A read or a write of the agent's own waits for Stet's lookups
// of row versions before the next step (Gate.StaleIdle), so that the result does not depend on timing, except where
// the spec says the next step comes at once (곧바로): readNow and writeNow, and lastNow for the step itself.
var staleCases = []staleCase{
	{id: "P1", modes: bothModes, want: []string{decide.ReadStale}, stale: true, steps: func(s *staleScenario) {
		id := s.contact()
		v, other := s.agent(tasks.RoleV, ""), s.agent(tasks.RoleI, "")
		s.read(v, readContact, id)
		s.write(other, "another agent", setTitle, "set by another agent", id)
		s.last(v, setTitle, "set by the agent", id)
	}},
	{id: "P2", modes: bothModes, want: []string{decide.ReadStale}, stale: true, steps: func(s *staleScenario) {
		id := s.contact()
		v := s.agent(tasks.RoleV, "")
		s.read(v, readContact, id)
		s.change(ownerTitle, id)
		s.last(v, setTitle, "set by the agent", id)
	}},
	{id: "P3", modes: bothModes, want: []string{decide.ReadStale}, stale: true, steps: func(s *staleScenario) {
		id := s.contact()
		a, b := s.agent(tasks.RoleV, ""), s.agent(tasks.RoleV, "")
		s.read(a, readContact, id)
		s.change(ownerTitle, id)
		s.last(b, setTitle, "set by the agent", id)
	}},
	{id: "P4", modes: bothModes, want: []string{decide.ReadStale}, stale: true, steps: func(s *staleScenario) {
		id := s.contact()
		v := s.agent(tasks.RoleV, "")
		s.read(v, "select title from contacts where id = $1", id) // no key in the result
		s.change(ownerTitle, id)
		s.last(v, setTitle, "set by the agent", id)
	}},
	{id: "P5", modes: bothModes, want: []string{decide.ReadStale}, stale: true, steps: func(s *staleScenario) {
		id := s.contact()
		s.deal(id)
		v := s.agent(tasks.RoleV, "")
		s.read(v, "select c.id, c.title, d.id, d.stage from contacts as c join deals as d on d.contact_id = c.id where c.id = $1", id)
		s.change(ownerTitle, id)
		s.last(v, setTitle, "set by the agent", id)
	}},
	{id: "P6", modes: bothModes, want: []string{decide.ReadStale}, stale: true, steps: func(s *staleScenario) {
		id := s.contact() // nothing references it
		v := s.agent(tasks.RoleV, "")
		s.read(v, readContact, id)
		s.change("delete from contacts where id = $1", id)
		s.last(v, setTitle, "set by the agent", id)
	}},
	{id: "P7", modes: bothModes, want: []string{decide.ReadStale}, stale: true, steps: func(s *staleScenario) {
		id := s.contact()
		a, b := s.agent(tasks.RoleV, "a"), s.agent(tasks.RoleV, "b")
		s.read(a, readContact, id)
		s.change(ownerTitle, id)
		s.read(b, readContact, id)
		s.last(a, setTitle, "set by the agent", id)
	}},
	{id: "M1", modes: bothModes, want: []string{decide.ReadFresh}, steps: func(s *staleScenario) {
		id := s.contact()
		a, b := s.agent(tasks.RoleV, ""), s.agent(tasks.RoleV, "")
		s.read(a, readContact, id)
		s.change(ownerTitle, id)
		s.read(b, readContact, id)
		s.last(a, setTitle, "set by the agent", id)
	}},
	{id: "N1", modes: bothModes, want: []string{decide.ReadFresh}, steps: func(s *staleScenario) {
		id := s.contact()
		v := s.agent(tasks.RoleV, "")
		s.read(v, readContact, id)
		s.last(v, setTitle, "set by the agent", id)
	}},
	{id: "N2", modes: bothModes, want: []string{decide.ReadFresh, decide.ReadOwnWrite}, steps: func(s *staleScenario) {
		id := s.contact()
		v := s.agent(tasks.RoleV, "")
		s.read(v, readContact, id)
		s.writeNow(v, "first", setTitle, "the agent's first", id)
		s.lastNow(v, setTitle, "the agent's second", id)
	}},
	{id: "N3", modes: bothModes, want: []string{decide.ReadFresh}, steps: func(s *staleScenario) {
		id, other := s.contact(), s.contact()
		v := s.agent(tasks.RoleV, "")
		s.read(v, readContact, id)
		s.change(ownerTitle, other)
		s.last(v, setTitle, "set by the agent", id)
	}},
	{id: "N4", modes: bothModes, want: []string{decide.ReadNone}, steps: func(s *staleScenario) {
		id := s.contact()
		v := s.agent(tasks.RoleV, "")
		s.last(v, setTitle, "set by the agent", id)
	}},
	{id: "N5", modes: enforceOnly, want: []string{decide.ReadFresh, decide.ReadPending}, runs: true, steps: func(s *staleScenario) {
		id := s.contact()
		v := s.agent(tasks.RoleV, "")
		s.read(v, readContact, id)
		s.change(ownerTitle, id)
		s.refused(v, "refused", "ST003", setTitle, "set by the agent", id)
		s.readNow(v, readContact, id)
		s.lastNow(v, setTitle, "set by the agent", id)
	}},
	{id: "N6", modes: bothModes, want: []string{decide.ReadNone}, steps: func(s *staleScenario) {
		id, other := s.contact(), s.contact()
		v := s.agent(tasks.RoleV, "")
		s.read(v, readContact, id)
		s.write(v, "several rows", "update contacts set notes = $1 where id in ($2, $3)", "set on several rows", id, other)
		s.last(v, setTitle, "set by the agent", id)
	}},
	{id: "N7", modes: bothModes, want: []string{decide.ReadNone}, steps: func(s *staleScenario) {
		id := s.contact()
		v := s.agent(tasks.RoleV, "")
		s.read(v, readContact, id)
		s.write(v, "begin", "begin")
		s.write(v, "in a transaction", setTitle, "set in a transaction", id)
		s.write(v, "commit", "commit")
		// The write in the transaction found the read remembered: what forgets it is the transaction's end.
		s.recorded("in a transaction", decide.ReadFresh)
		s.last(v, setTitle, "set by the agent", id)
	}},
	{id: "N8", modes: bothModes, want: []string{decide.ReadFresh, decide.ReadPending}, steps: func(s *staleScenario) {
		id := s.contact()
		a, b := s.agent(tasks.RoleV, ""), s.agent(tasks.RoleV, "")
		s.read(a, readContact, id)
		s.change(ownerTitle, id)
		s.readNow(b, readContact, id)
		s.lastNow(a, setTitle, "set by the agent", id)
	}},
	{id: "N9", modes: bothModes, want: []string{decide.ReadFresh, decide.ReadOwnWrite}, steps: func(s *staleScenario) {
		id := s.contact()
		a, b := s.agent(tasks.RoleV, "a"), s.agent(tasks.RoleV, "b")
		s.read(a, readContact, id)
		s.write(b, "connection B", setTitle, "set on connection B", id)
		s.last(a, setTitle, "set on connection A", id)
	}},
	{id: "N10", modes: enforceOnly, want: []string{decide.ReadNone}, steps: func(s *staleScenario) {
		id, other := s.deal(nil), s.deal(nil)
		v := s.agent(tasks.RoleV, "")
		s.read(v, readDeal, id)
		s.write(v, "the limit", setStage, "won", other) // the role's one row a minute of deals
		held := s.refused(v, "held", "ST001", setStage, "lost", id)
		// The held write found the read of deals remembered: what forgets it is the approved write's run.
		s.recorded("held", decide.ReadFresh)
		s.approve(held)
		s.last(v, setStage, "open", id)
	}},
	{id: "N11", modes: shadowOnly, want: []string{decide.ReadNone}, steps: func(s *staleScenario) {
		id := s.contact()
		v := s.agent(tasks.RoleV, "")
		s.read(v, readContact, id)
		s.write(v, "do", fmt.Sprintf("do $$ begin update contacts set title = 'set in a DO block' where id = %d; end $$", id))
		s.last(v, setTitle, "set by the agent", id)
	}},
}

// CheckStale runs the check of docs/spec-v0.2-stale.md 11 and returns a row per scenario and mode.
//
// For shadow, then enforce, it clones c.Template into c.Database_<mode> and starts Stet inside this process in front
// of the clone as Stet's account (staleConfig), creating Stet's schema there; in enforce mode it gives the owner the
// reviewer's grants in the clone, as Stet's account, and starts the executor of approved held writes. Then it runs
// each scenario of that mode: the agents' statements go through Stet as the benchmark's agent roles, agent_v unless
// the scenario says otherwise, each scenario on connections of its own; the owner's inserts and changes go straight to
// the server. A scenario's row holds what Stet recorded of its last write, which it finds by the comment the write
// starts with, and what the client got. A statement through Stet that does not go as the scenario's steps assume ends
// the scenario, and its row says so (StaleRow.Note).
//
// It returns an error, with the rows it has, when it cannot go on: the template does not exist, a clone does, a clone
// or Stet cannot be set up, or a step of the owner's fails.
func CheckStale(ctx context.Context, c StaleCheck) (StaleResult, error) {
	if c.Template == "" || c.Database == "" {
		return StaleResult{}, errors.New("stale check: the template and the clones' name are required")
	}
	clones := map[config.Mode]string{}
	for _, mode := range bothModes {
		clones[mode] = c.Database + "_" + string(mode)
	}
	for _, db := range []string{c.Template, clones[config.ModeShadow], clones[config.ModeEnforce]} {
		if err := ours(db); err != nil {
			return StaleResult{}, fmt.Errorf("stale check: %w", err)
		}
	}
	if err := templateExists(ctx, c.Admin, c.Template); err != nil {
		return StaleResult{}, fmt.Errorf("stale check: %w", err)
	}
	if err := noneExists(ctx, c.Admin, clones[config.ModeShadow], clones[config.ModeEnforce]); err != nil {
		return StaleResult{}, fmt.Errorf("stale check: %w", err)
	}
	r := &staleRun{c: c}
	var out io.Writer = io.Discard
	if c.Progress != nil {
		out = stetLog{r}
	}
	r.log = slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelWarn}))

	byMode := map[config.Mode][]StaleRow{}
	for _, mode := range bothModes {
		rows, err := r.mode(ctx, mode, clones[mode])
		byMode[mode] = rows
		if err != nil {
			return staleRows(byMode), fmt.Errorf("stale check, %s: %w", clones[mode], err)
		}
	}
	return staleRows(byMode), nil
}

// noneExists refuses databases that exist, before anything is made.
func noneExists(ctx context.Context, a Admin, dbs ...string) error {
	su, err := pgx.Connect(ctx, a.URL(adminDB))
	if err != nil {
		return err
	}
	defer su.Close(context.WithoutCancel(ctx))
	var there []string
	if err := su.QueryRow(ctx, "select array(select datname::text from pg_database where datname = any($1) order by 1)",
		dbs).Scan(&there); err != nil {
		return err
	}
	if len(there) > 0 {
		return fmt.Errorf("%s already exists: a clone left by an earlier run is dropped with stet-lab bench drop -name <name>",
			strings.Join(there, " and "))
	}
	return nil
}

// staleRows puts the rows of both modes in the spec's order, shadow before enforce.
func staleRows(byMode map[config.Mode][]StaleRow) StaleResult {
	var res StaleResult
	for _, sc := range staleCases {
		for _, mode := range bothModes {
			for _, row := range byMode[mode] {
				if row.ID == sc.id {
					res.Rows = append(res.Rows, row)
				}
			}
		}
	}
	return res
}

// staleRun is one CheckStale call.
type staleRun struct {
	c   StaleCheck
	mu  sync.Mutex   // Progress's calls, from the check and from Stet's log
	log *slog.Logger // Stet's: its warnings go to Progress
}

func (r *staleRun) say(format string, a ...any) {
	if r.c.Progress == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.c.Progress(fmt.Sprintf(format, a...))
}

// stetLog hands each line Stet logs to Progress, as "stet: <line>". Stet's log writes a record in one Write.
type stetLog struct{ r *staleRun }

func (l stetLog) Write(p []byte) (int, error) {
	for line := range strings.SplitSeq(strings.TrimRight(string(p), "\n"), "\n") {
		l.r.say("stet: %s", line)
	}
	return len(p), nil
}

// mode clones the template into db, starts Stet in front of it in mode, and runs the scenarios of that mode.
func (r *staleRun) mode(ctx context.Context, mode config.Mode, db string) (rows []StaleRow, err error) {
	c := r.c
	r.say("%s: cloning %s", db, c.Template)
	if err := Clone(ctx, c.Admin, c.Names, c.Template, db, true); err != nil {
		return nil, fmt.Errorf("%w (a clone left by an earlier run is dropped with stet-lab bench drop -name %s)", err, db)
	}
	if !c.Keep {
		defer func() {
			if derr := Drop(context.WithoutCancel(ctx), c.Admin, db); derr != nil {
				err = errors.Join(err, derr)
			}
		}()
	}
	upstream, err := json.Marshal(GateURL(c.Admin.Addr, c.Names, db))
	if err != nil {
		return nil, err
	}
	roles, err := json.Marshal(c.Names.Agents())
	if err != nil {
		return nil, err
	}
	cfg, err := config.Parse(fmt.Appendf(nil, staleConfig, mode, upstream, roles))
	if err != nil {
		return nil, fmt.Errorf("the check's config: %w", err)
	}
	r.say("%s: starting Stet in %s mode as %s", db, mode, c.Names.Gate())
	stet, err := startInProcess(ctx, cfg, r.log)
	if err != nil {
		return nil, fmt.Errorf("start Stet: %w", err)
	}
	defer stet.close()

	m := &staleMode{names: c.Names, mode: mode, db: db, stet: stet}
	if m.owner, err = pgx.Connect(ctx, OwnerURL(c.Admin.Addr, c.Names, db)); err != nil {
		return nil, err
	}
	defer m.owner.Close(context.WithoutCancel(ctx))
	if m.account, err = pgx.Connect(ctx, GateURL(c.Admin.Addr, c.Names, db)); err != nil {
		return nil, err
	}
	defer m.account.Close(context.WithoutCancel(ctx))
	if mode == config.ModeEnforce {
		// The reviewer's grants the comment in Stet's schema gives (spec-v0.2-approve 6), run as Stet's account, which
		// owns the schema, in this clone only.
		owner := ident(c.Names.Owner())
		for _, g := range []string{
			"grant usage on schema stet to " + owner,
			"grant select on stet.review to " + owner,
			"grant execute on function stet.approve(uuid, text, bigint), stet.reject(uuid, text) to " + owner,
		} {
			if _, err := m.account.Exec(ctx, g); err != nil {
				return nil, fmt.Errorf("make %s a reviewer: %s: %w", c.Names.Owner(), g, err)
			}
		}
	}

	r.say("%s: Stet listens on %s; running the scenarios", db, stet.addr)
	for _, sc := range staleCases {
		if !slices.Contains(sc.modes, mode) {
			continue
		}
		row, err := m.run(ctx, sc)
		if err != nil {
			return rows, fmt.Errorf("%s: %w", sc.id, err)
		}
		rows = append(rows, row)
		verdict := "as expected"
		if !row.OK {
			verdict = "NOT as expected"
		}
		if row.Note != "" {
			verdict += " (" + row.Note + ")"
		}
		r.say("%s: %s: read_check %s (want %s), %s [%s], executed %v, sqlstate %s: %s", db, row.ID, cmp.Or(row.Got, "-"),
			strings.Join(row.Want, " or "), cmp.Or(row.Verdict, "-"), strings.Join(row.Reasons, ", "), row.Executed,
			cmp.Or(row.Code, "-"), verdict)
	}
	return rows, nil
}

// staleMode is one mode's clone, with Stet in front of it.
type staleMode struct {
	names   Names
	mode    config.Mode
	db      string
	stet    *stetInProcess
	owner   *pgx.Conn // the tables' owner, straight to the server
	account *pgx.Conn // Stet's account, straight to the server: it reads stet.decisions and stet.held_writes
}

// run runs one scenario and returns its row.
func (m *staleMode) run(ctx context.Context, sc staleCase) (StaleRow, error) {
	row := StaleRow{ID: sc.id, Mode: string(m.mode), Want: sc.want}
	s := &staleScenario{staleMode: m, ctx: ctx, id: sc.id}
	s.settle() // what the scenario before left to look up
	sc.steps(s)
	for _, conn := range s.conns {
		conn.Close(context.WithoutCancel(ctx))
	}
	if s.fatal {
		return row, s.err
	}
	if s.sent {
		d, found, err := m.decision(ctx, s.marker(""))
		switch {
		case err != nil:
			return row, fmt.Errorf("read the decision of the last write: %w", err)
		case found:
			row.Verdict, row.Reasons, row.Executed = d.verdict, d.reasons, d.executed
			if d.result != nil {
				row.Got = *d.result
			}
		case s.err == nil:
			s.err = fmt.Errorf("no decision of the last write was recorded within %s", staleRecord)
		}
	}
	row.Code = s.code
	if s.err != nil {
		row.Note = s.err.Error()
	}
	row.OK = sc.ok(row)
	return row, nil
}

// ok reports whether row is as the spec expects of the scenario: nothing kept it from running as its steps assume, and
// its last write's read_check is one the spec allows. A P row's last write was sent, with no error to the client, and
// recorded with stale_read in shadow; in enforce it was refused with ST003, a reject with stale_read. An M or N row's
// was neither rejected nor given stale_read; N5's ran.
func (sc staleCase) ok(row StaleRow) bool {
	if row.Note != "" || !slices.Contains(sc.want, row.Got) {
		return false
	}
	staleRead := slices.Contains(row.Reasons, "stale_read")
	switch {
	case sc.stale && row.Mode == string(config.ModeShadow):
		return row.Executed && staleRead && row.Code == ""
	case sc.stale:
		return !row.Executed && row.Code == "ST003" && row.Verdict == decide.Reject && staleRead
	case staleRead || row.Verdict == decide.Reject:
		return false
	}
	return !sc.runs || row.Executed
}

// staleDecision is what Stet recorded of a write.
type staleDecision struct {
	result   *string // read_check.result
	verdict  string
	reasons  []string
	executed bool
}

// decision waits up to staleRecord for the latest decision whose statement holds marker: decisions are recorded in
// the background once a write has ended.
func (m *staleMode) decision(ctx context.Context, marker string) (staleDecision, bool, error) {
	deadline := time.Now().Add(staleRecord)
	for {
		var d staleDecision
		err := m.account.QueryRow(ctx, `select read_check->>'result', verdict, reasons, executed from stet.decisions
			where strpos(statement, $1) > 0 order by created_at desc limit 1`, marker).
			Scan(&d.result, &d.verdict, &d.reasons, &d.executed)
		switch {
		case err == nil:
			return d, true, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return d, false, err
		case time.Now().After(deadline):
			return d, false, nil
		}
		select {
		case <-ctx.Done():
			return d, false, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// staleScenario runs one scenario's steps in one mode. The first step that fails is kept, and every step after it
// does nothing.
type staleScenario struct {
	*staleMode
	ctx   context.Context
	id    string
	conns []*pgx.Conn // the agents', closed when the scenario ends
	err   error       // the step that failed
	fatal bool        // err is the check's own failure, not that of a statement sent through Stet
	sent  bool        // the last write was sent
	code  string      // the SQLSTATE the client got of the last write
}

// fail ends the scenario at a statement sent through Stet that did not go as its steps assume. When the check is
// being stopped, it ends the check.
func (s *staleScenario) fail(format string, a ...any) {
	if err := s.ctx.Err(); err != nil {
		s.abort(err)
		return
	}
	s.err = fmt.Errorf(format, a...)
}

// abort ends the check at a step of its own that failed.
func (s *staleScenario) abort(err error) { s.err, s.fatal = err, true }

// marker is the comment an agent's write starts with: "/* stale-check P1 shadow */" for the scenario's last write,
// "/* stale-check P1 shadow <step> */" for the others, so that the last write's decision is found by its text alone.
func (s *staleScenario) marker(step string) string {
	if step == "" {
		return fmt.Sprintf("/* stale-check %s %s */", s.id, s.mode)
	}
	return fmt.Sprintf("/* stale-check %s %s %s */", s.id, s.mode, step)
}

// agent connects through Stet as the agent role role (tasks.RoleV, ...) and, when task is not "", sets
// stet.on_behalf_of to it on that connection.
func (s *staleScenario) agent(role, task string) *pgx.Conn {
	if s.err != nil {
		return nil
	}
	name := s.names.Role(role)
	cfg, err := pgx.ParseConfig(pgURL(s.stet.addr, name, AgentPassword, s.db))
	if err != nil {
		s.abort(err)
		return nil
	}
	cfg.RuntimeParams["application_name"] = staleApp
	conn, err := pgx.ConnectConfig(s.ctx, cfg)
	if err != nil {
		s.fail("connect as %s through Stet: %v", name, err)
		return nil
	}
	s.conns = append(s.conns, conn)
	if task != "" {
		if _, err := conn.Exec(s.ctx, "set stet.on_behalf_of = '"+task+"'"); err != nil {
			s.fail("set stet.on_behalf_of as %s: %v", name, err)
		}
	}
	return conn
}

// contact inserts a contact as the owner, straight to the server, and returns its id. Nothing references it.
func (s *staleScenario) contact() int {
	return s.insert(`insert into contacts (account_id, name, email, title)
		values ((select min(id) from accounts), 'Stale Check', 'stale.check@example.net', 'as inserted') returning id`)
}

// deal inserts a deal of contact (an id, or nil) as the owner, straight to the server, and returns its id.
func (s *staleScenario) deal(contact any) int {
	return s.insert(`insert into deals (account_id, contact_id, stage, amount)
		values ((select min(id) from accounts), $1, 'open', 1000) returning id`, contact)
}

func (s *staleScenario) insert(sql string, args ...any) int {
	if s.err != nil {
		return 0
	}
	var id int
	if err := s.owner.QueryRow(s.ctx, sql, args...).Scan(&id); err != nil {
		s.abort(fmt.Errorf("insert the scenario's row as %s: %w", s.names.Owner(), err))
	}
	return id
}

// change runs sql as the owner, straight to the server: a change Stet does not see. It must change exactly one row,
// the scenario's: a change that missed would let a scenario come out fresh for the wrong reason.
func (s *staleScenario) change(sql string, args ...any) {
	if s.err != nil {
		return
	}
	tag, err := s.owner.Exec(s.ctx, sql, args...)
	switch {
	case err != nil:
		s.abort(fmt.Errorf("%s as %s: %w", sql, s.names.Owner(), err))
	case tag.RowsAffected() != 1:
		s.abort(fmt.Errorf("%s as %s changed %d rows, want 1", sql, s.names.Owner(), tag.RowsAffected()))
	}
}

// settle waits until Stet has no lookup of row versions queued or running.
func (s *staleScenario) settle() {
	if s.err != nil {
		return
	}
	wait, cancel := context.WithTimeout(s.ctx, staleSettle)
	defer cancel()
	if err := s.stet.gate.StaleIdle(wait); err != nil {
		s.fail("Stet's lookups of row versions did not finish within %s", staleSettle)
	}
}

// read reads through conn, which must return a row, and waits until Stet has looked up what was read.
func (s *staleScenario) read(conn *pgx.Conn, sql string, args ...any) {
	s.readNow(conn, sql, args...)
	s.settle()
}

// readNow is read without the wait: the next step comes at once.
func (s *staleScenario) readNow(conn *pgx.Conn, sql string, args ...any) {
	if s.err != nil {
		return
	}
	rows, err := conn.Query(s.ctx, sql, args...)
	if err != nil {
		s.fail("read %q: %v", sql, err)
		return
	}
	n := 0
	for rows.Next() {
		n++
	}
	switch {
	case rows.Err() != nil:
		s.fail("read %q: %v", sql, rows.Err())
	case n == 0:
		s.fail("read %q: no row", sql)
	}
}

// write sends a statement through conn that must run, and waits until Stet has looked up what it wrote.
func (s *staleScenario) write(conn *pgx.Conn, step, sql string, args ...any) {
	s.writeNow(conn, step, sql, args...)
	s.settle()
}

// writeNow is write without the wait: the next step comes at once.
func (s *staleScenario) writeNow(conn *pgx.Conn, step, sql string, args ...any) {
	if s.err != nil {
		return
	}
	if _, err := conn.Exec(s.ctx, s.marker(step)+" "+sql, args...); err != nil {
		s.fail("%s: %v", step, err)
	}
}

// refused sends a write through conn that Stet must refuse with code, and returns the error the client got; nil when
// it was not refused so.
func (s *staleScenario) refused(conn *pgx.Conn, step, code, sql string, args ...any) *pgconn.PgError {
	if s.err != nil {
		return nil
	}
	_, err := conn.Exec(s.ctx, s.marker(step)+" "+sql, args...)
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe) && pe.Code == code:
		s.settle()
		return pe
	case err == nil:
		s.fail("%s: it ran; the scenario expects Stet to refuse it with %s", step, code)
	default:
		s.fail("%s: %v; the scenario expects Stet to refuse it with %s", step, err, code)
	}
	return nil
}

// recorded requires Stet to have recorded the scenario's write step with read_check result want: a premise the
// scenario's later steps rest on. A later no_read, say, shows that something forgot the read only if the read was
// remembered in the first place.
func (s *staleScenario) recorded(step, want string) {
	if s.err != nil {
		return
	}
	d, found, err := s.decision(s.ctx, s.marker(step))
	switch {
	case err != nil:
		s.abort(fmt.Errorf("read the decision of %s: %w", step, err))
	case !found:
		s.fail("%s: no decision was recorded within %s", step, staleRecord)
	case d.result == nil || *d.result != want:
		got := "NULL"
		if d.result != nil {
			got = *d.result
		}
		s.fail("%s: read_check %s, %s [%s]; the scenario rests on %s", step, got, d.verdict, strings.Join(d.reasons, ", "), want)
	}
}

// approve approves, as the owner, the held write the refusal names, and waits until the executor has run it and told
// the gate, which then forgets what the role remembers of the table (spec 7).
func (s *staleScenario) approve(refusal *pgconn.PgError) {
	if s.err != nil {
		return
	}
	var id string
	for f := range strings.FieldsSeq(refusal.Detail) {
		if v, ok := strings.CutPrefix(f, "held_write="); ok {
			id = v
		}
	}
	if id == "" {
		s.fail("the refusal names no held write: DETAIL %q", refusal.Detail)
		return
	}
	// stet.approve takes a held write once the agent has been told of it (pending).
	status, err := s.heldStatus(id, func(status string) bool { return status != "unconfirmed" })
	if err != nil {
		s.abort(err)
		return
	}
	if status != "pending" {
		s.fail("held write %s is %q, not pending", id, status)
		return
	}
	for len(s.stet.applied) > 0 {
		<-s.stet.applied
	}
	if _, err := s.owner.Exec(s.ctx, "select stet.approve($1)", id); err != nil {
		s.fail("stet.approve(%s) as %s: %v", id, s.names.Owner(), err)
		return
	}
	select {
	case <-s.stet.applied:
	case <-time.After(staleApplied):
		s.fail("held write %s was not run within %s of its approval", id, staleApplied)
		return
	case <-s.ctx.Done():
		s.abort(s.ctx.Err())
		return
	}
	if status, err = s.heldStatus(id, func(string) bool { return true }); err != nil {
		s.abort(err)
		return
	}
	if status != "applied" {
		s.fail("held write %s is %q once the executor ran it, not applied", id, status)
		return
	}
	s.settle()
}

// heldStatus reads the status of held write id as Stet's account until done says it is what the caller waits for,
// or staleRecord has passed, and returns the last status read.
func (s *staleScenario) heldStatus(id string, done func(status string) bool) (string, error) {
	deadline := time.Now().Add(staleRecord)
	for {
		var status string
		if err := s.account.QueryRow(s.ctx, "select status from stet.held_writes where id = $1", id).Scan(&status); err != nil {
			return "", fmt.Errorf("read held write %s: %w", id, err)
		}
		if done(status) || time.Now().After(deadline) {
			return status, nil
		}
		select {
		case <-s.ctx.Done():
			return "", s.ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// last sends the scenario's last write through conn, whose decision is the scenario's result, and keeps what the
// client got.
func (s *staleScenario) last(conn *pgx.Conn, sql string, args ...any) {
	s.lastIn(conn, nil, sql, args...)
}

// lastNow is last for a write the spec sends at once (곧바로): in one round trip, with no statement prepared first
// (pgx.QueryExecModeExec, text values whose types the server infers), as soon after the step before as the driver
// allows. Its text is new to the connection, so pgx's default would first prepare it in a round trip of its own.
func (s *staleScenario) lastNow(conn *pgx.Conn, sql string, args ...any) {
	s.lastIn(conn, []any{pgx.QueryExecModeExec}, sql, args...)
}

func (s *staleScenario) lastIn(conn *pgx.Conn, mode []any, sql string, args ...any) {
	if s.err != nil {
		return
	}
	_, err := conn.Exec(s.ctx, s.marker("")+" "+sql, append(mode, args...)...)
	s.sent = true
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &pe):
		s.code = pe.Code
	case err != nil:
		s.fail("the last write: %v", err)
	}
}

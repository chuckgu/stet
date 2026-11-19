package bench

// This file checks undo end to end against the benchmark's streams (docs/spec-v0.2-undo.md 10 and 13). In a clone
// of the benchmark's template, Stet's schema is created and capture is installed on the gated tables and the tables
// they cascade into, with the SQL stet undo-sql prints; the V and X sets are sent as the none arm sends them, with no
// proxy and no model; each agent's changes are undone in the reviewer's transaction; and every table is compared,
// key by key, with what it held before the stream. A row that did not come back must carry a reason the system
// reported: the undo line for its key, a set capture recorded without values, its table not being captured, or a
// statement sent as a role that is not captured. Any other row is unexplained, and the check fails on it.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/lab/tasks"
	"github.com/stetdb/stet/internal/store"
	"github.com/stetdb/stet/internal/undo"
)

// UndoCheck says what CheckUndo runs on and how.
type UndoCheck struct {
	Admin Admin // the server's superuser: it creates and drops the clones, and nothing else
	// Names are the roles, used as they are: none is created, altered or given a membership. Stet's account (Gate)
	// creates the stet schema and runs the grants stet undo-sql prints for it; the tables' owner (Owner) installs
	// the triggers and is the reviewer who undoes; the agents are captured and undone.
	Names    Names
	Template string // the benchmark template the clones are made from; it is only read
	// Database is the first clone's name; the n-th is Database_n. Each is created here, and one that exists is
	// refused.
	Database string
	Tables   map[string]string // the gated tables: table key -> key column; the tables they cascade into are added
	MaxRows  int64             // capture's cap on the rows of one statement (stet.capture_settings.max_rows)
	Options  Options           // how statements are sent: the none arm's are DefaultOptions("none")
	Paths    []string          // X paths whose changed rows are followed, such as s5.call and s5.view
	Keep     bool              // keep the clones; otherwise each is dropped once compared, or when the check fails
	Progress func(line string) // a line per step; nil: none
}

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
	Key    string // the key column's text
	Kind   string // different, missing or extra
	Reason string // what the system reported that accounts for it; "" when nothing does
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

// undoNote is the note stet.undo records with each undo.
const undoNote = "stet-lab bench undo-check"

// CheckUndo runs the check (docs/spec-v0.2-undo.md 10) and returns what it found, clone by clone.
//
// In the first clone of c.Template it (a) creates Stet's schema as Stet's account and installs capture on c.Tables
// and the tables they cascade into, running the SQL of undo.InstallSQL as the roles it names, and gives the tables'
// owner the reviewer's grants of undo.GrantSQL; (b) sets capture to the agent roles and c.MaxRows; (c) reads every
// row of every ordinary table in the captured tables' schemas, as its key's text and a hash of its text; (d) sends
// the V set with its preparation, then the X set, as the none arm does, straight to the server; (e) as the owner,
// calls stet.undo once for each agent role, from the server's time just before the first statement, each in a READ
// COMMITTED transaction it commits, the agent whose first change came latest first; and (f) reads the tables again
// and compares them. When an attack that breaks the database ran, it does (e) and (f) on that clone and goes on with
// the attacks after it in a fresh clone, from (a). The I set is not sent.
//
// It returns an error, with the clones it finished, when a step fails: stet.undo raising an error is one.
func CheckUndo(ctx context.Context, c UndoCheck) (UndoResult, error) {
	switch {
	case c.Template == "" || c.Database == "":
		return UndoResult{}, errors.New("undo check: the template and the first clone's name are required")
	case len(c.Tables) == 0:
		return UndoResult{}, errors.New("undo check: no tables to capture")
	case c.MaxRows < 1:
		return UndoResult{}, fmt.Errorf("undo check: max_rows %d: want 1 or more", c.MaxRows)
	case c.Options.Conns < 1:
		return UndoResult{}, fmt.Errorf("undo check: %d connections; want at least 1", c.Options.Conns)
	}
	for _, db := range []string{c.Template, c.Database} {
		if err := ours(db); err != nil {
			return UndoResult{}, fmt.Errorf("undo check: %w", err)
		}
	}
	if c.Options.Sleep == nil {
		c.Options.Sleep = time.Sleep
	}
	r := &undoRun{c: c}
	res := UndoResult{Template: c.Template, MaxRows: c.MaxRows, Agents: c.Names.Agents(), Paths: c.Paths}
	from := 0
	for i := 1; ; i++ {
		name := c.Database
		if i > 1 {
			name = fmt.Sprintf("%s_%d", c.Database, i)
		}
		cl, err := r.clone(ctx, name, i == 1, from)
		res.Attacks = len(r.attacks)
		if err != nil {
			return res, fmt.Errorf("undo check, clone %s: %w", name, err)
		}
		res.Clones = append(res.Clones, cl)
		if !cl.Reclone {
			return res, nil
		}
		from = cl.Next
	}
}

// undoRun is one CheckUndo call.
type undoRun struct {
	c       UndoCheck
	stream  []tasks.Stmt
	attacks []tasks.Attack
	byID    map[string]tasks.Stmt // the stream's statements by ID
}

func (r *undoRun) say(format string, a ...any) {
	if r.c.Progress != nil {
		r.c.Progress(fmt.Sprintf(format, a...))
	}
}

// clone runs the check on one fresh clone: the V set when first, and the attacks from from on.
func (r *undoRun) clone(ctx context.Context, name string, first bool, from int) (cl UndoClone, err error) {
	c := r.c
	cl = UndoClone{Database: name, SentV: first, From: from, Next: from, Sent: map[string]int{}}
	r.say("%s: cloning %s", name, c.Template)
	if err := Clone(ctx, c.Admin, c.Names, c.Template, name, true); err != nil {
		return cl, fmt.Errorf("%w (a clone left by an earlier run is dropped with stet-lab bench drop -name %s)", err, name)
	}
	if !c.Keep {
		defer func() {
			if derr := Drop(context.WithoutCancel(ctx), c.Admin, name); derr != nil {
				err = errors.Join(err, derr)
			}
		}()
	}
	owner, err := pgx.Connect(ctx, OwnerURL(c.Admin.Addr, c.Names, name))
	if err != nil {
		return cl, err
	}
	defer owner.Close(context.WithoutCancel(ctx))
	if first {
		if r.stream, r.attacks, err = LoadStream(ctx, owner); err != nil {
			return cl, err
		}
		r.byID = map[string]tasks.Stmt{}
		for _, s := range r.stream {
			r.byID[s.ID] = s
		}
	}
	if from > len(r.attacks) {
		return cl, fmt.Errorf("attack %d: there are %d attacks", from, len(r.attacks))
	}

	// (a), (b)
	r.say("%s: creating Stet's schema as %s and installing capture", name, c.Names.Gate())
	installed, err := r.install(ctx, name, owner)
	if err != nil {
		return cl, err
	}
	for _, t := range installed {
		cl.Captured = append(cl.Captured, CapturedTable{Key: t.Key, Table: t.qualified, Column: t.PK})
	}
	tables, err := comparedTables(ctx, owner, installed)
	if err != nil {
		return cl, err
	}

	// (c)
	r.say("%s: reading %d tables", name, len(tables))
	before, err := fingerprint(ctx, owner, tables)
	if err != nil {
		return cl, err
	}
	if err := owner.QueryRow(ctx, "select clock_timestamp()").Scan(&cl.Since); err != nil {
		return cl, err
	}

	// (d)
	target := Target{Addr: c.Admin.Addr, Database: name, Names: c.Names}
	outcomes := map[string]Outcome{}
	save := func(o Outcome) error {
		cl.Sent[o.Result]++
		outcomes[o.ID] = o
		if o.Result == Unknown {
			cl.Unanswered = append(cl.Unanswered, Unanswered{ID: o.ID, Set: o.Set, Path: r.byID[o.ID].Path, Message: o.Message})
		}
		return nil
	}
	if first {
		phases := Phases(r.stream, []string{tasks.SetV}, false)
		total := 0
		for _, p := range phases {
			for _, u := range p {
				total += len(u)
			}
		}
		if s, ok := firstStmt(phases); ok {
			if err := reach(ctx, target, s.Role, s.App); err != nil {
				return cl, err
			}
			r.say("%s: sending the V set with its preparation: %d statements", name, total)
			if err := Send(ctx, target, phases, c.Options, save); err != nil {
				return cl, fmt.Errorf("send the V set: %w", err)
			}
		}
	}
	if from < len(r.attacks) {
		xs := map[string]tasks.Stmt{}
		for _, s := range r.stream {
			if s.Set == tasks.SetX {
				xs[s.ID] = s
			}
		}
		r.say("%s: sending attacks %d to %d of the X set", name, from, len(r.attacks)-1)
		next, reclone, err := RunAttacks(ctx, target, owner, r.attacks, xs, from, c.Options, save,
			func(a AttackResult) error {
				what := "not executed"
				switch {
				case a.Undetermined:
					what = "undetermined"
				case a.Executed:
					what = "executed"
				}
				r.say("%s: attack %d %s: %s", name, a.Index, a.Path, what)
				return nil
			})
		cl.Next, cl.Reclone = next, reclone
		if err != nil {
			return cl, fmt.Errorf("send the X set: %w", err)
		}
		if reclone {
			cl.Broke = r.attacks[next-1].Path
		}
	}

	// (e)
	if cl.Undone, err = r.undoAll(ctx, name, owner, cl.Since); err != nil {
		return cl, err
	}

	// (f)
	r.say("%s: reading the tables again and comparing", name)
	after, err := fingerprint(ctx, owner, tables)
	if err != nil {
		return cl, err
	}
	h, err := readHistory(ctx, owner, cl.Since)
	if err != nil {
		return cl, err
	}
	cl.Sets = h.sets
	e := newExplainer(cl.Undone, h, r.uncaptured(outcomes))
	cl.Tables = compareTables(tables, before, after, e)
	if cl.Paths, err = r.paths(ctx, owner, cl, outcomes, tables, before, after, e); err != nil {
		return cl, err
	}
	return cl, nil
}

// uncaptured returns the roles that sent a statement here and are not captured, in byte order.
func (r *undoRun) uncaptured(outcomes map[string]Outcome) []string {
	agents := r.c.Names.Agents()
	var out []string
	for id := range outcomes {
		if role := r.c.Names.Role(r.byID[id].Role); !slices.Contains(agents, role) && !slices.Contains(out, role) {
			out = append(out, role)
		}
	}
	slices.Sort(out)
	return out
}

// installedTable is a table capture was installed on.
type installedTable struct {
	undo.Table
	qualified string // schema.name as format('%I.%I') writes it
}

// install creates Stet's schema in db as Stet's account, installs capture on the gated tables and the tables
// they cascade into with the SQL stet undo-sql prints, runs as Stet's account the grants that make the owner a
// reviewer, sets capture to the agent roles and MaxRows, and checks that Stet would warn of nothing at startup.
func (r *undoRun) install(ctx context.Context, db string, owner *pgx.Conn) ([]installedTable, error) {
	c := r.c
	url := GateURL(c.Admin.Addr, c.Names, db)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	st := store.New(pool)
	if err := st.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("create Stet's schema as %s: %w", c.Names.Gate(), err)
	}
	gate, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	defer gate.Close(context.WithoutCancel(ctx))

	var version int
	if err := gate.QueryRow(ctx, "select current_setting('server_version_num')::int").Scan(&version); err != nil {
		return nil, err
	}
	if err := undo.RequireServer(version); err != nil {
		return nil, err
	}
	keys, ts, err := inspectWithChildren(ctx, gate, c.Tables)
	if err != nil {
		return nil, err
	}
	install, err := undo.InstallSQL(c.Names.Gate(), ts)
	if err != nil {
		return nil, err
	}
	if err := runAs(ctx, install, map[string]*pgx.Conn{c.Names.Gate(): gate, c.Names.Owner(): owner}); err != nil {
		return nil, fmt.Errorf("install capture: %w", err)
	}
	functions, err := undo.ReviewerFunctions(ctx, gate)
	if err != nil {
		return nil, err
	}
	grants, err := undo.GrantSQL(c.Names.Gate(), c.Names.Owner(), functions)
	if err != nil {
		return nil, err
	}
	if err := runAs(ctx, grants, map[string]*pgx.Conn{c.Names.Gate(): gate}); err != nil {
		return nil, fmt.Errorf("grant the reviewer's rights to %s: %w", c.Names.Owner(), err)
	}
	if err := st.SyncCaptureSettings(ctx, c.Names.Agents(), c.MaxRows); err != nil {
		return nil, fmt.Errorf("set capture to the agent roles: %w", err)
	}

	if ts, err = undo.Inspect(ctx, gate, undoConfig(keys)); err != nil {
		return nil, err
	}
	if w := undo.Warnings(ts); len(w) > 0 {
		return nil, fmt.Errorf("capture is not installed as stet undo-sql prints it:\n  %s", strings.Join(w, "\n  "))
	}
	out := make([]installedTable, len(ts))
	for i, t := range ts {
		out[i] = installedTable{Table: t}
		if err := gate.QueryRow(ctx, "select format('%I.%I', $1::text, $2::text)", t.Schema, t.Name).Scan(&out[i].qualified); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func undoConfig(tables map[string]string) *config.Config {
	return &config.Config{Undo: &config.Undo{Tables: tables}}
}

// inspectWithChildren looks up the gated tables as Stet's account, adds each table that references one of them
// with a foreign key that cascades (Table.Children) under its schema-qualified name and its primary key, until none
// is left out, and refuses a table capture cannot take.
func inspectWithChildren(ctx context.Context, q store.Querier, gated map[string]string) (map[string]string, []undo.Table, error) {
	tables := maps.Clone(gated)
	for {
		ts, err := undo.Inspect(ctx, q, undoConfig(tables))
		if err != nil {
			return nil, nil, err
		}
		var problems []string
		added := false
		for _, t := range ts {
			if t.Problem != "" {
				problems = append(problems, t.Key+": "+t.Problem)
				continue
			}
			for _, child := range t.Children {
				if _, ok := tables[child]; ok {
					continue
				}
				if strings.Contains(child, `"`) {
					return nil, nil, fmt.Errorf("%s cascades from %s, and a quoted name cannot be a table key here", child, t.Key)
				}
				var pk string
				err := q.QueryRow(ctx, `
					select a.attname from pg_index i join pg_attribute a on a.attrelid = i.indrelid and a.attnum = i.indkey[0]
					 where i.indrelid = $1::text::regclass and i.indisprimary and i.indnkeyatts = 1`, child).Scan(&pk)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, nil, fmt.Errorf("%s cascades from %s and has no single-column primary key to capture it by", child, t.Key)
				}
				if err != nil {
					return nil, nil, err
				}
				tables[child], added = pk, true
			}
		}
		if len(problems) > 0 {
			return nil, nil, fmt.Errorf("capture cannot be installed: %s", strings.Join(problems, "; "))
		}
		if !added {
			return tables, ts, nil
		}
	}
}

// runAsLine heads each part of the SQL undo.InstallSQL prints ("-- 1. Run as <role> (...") and the one part of
// undo.GrantSQL's ("-- Run as <role> (...").
var runAsLine = regexp.MustCompile(`^-- (?:\d+\. )?Run as (.+?) \(`)

// runAs runs each part of sql, as its heading names it, on that role's connection in as. Each part goes as one
// simple query, so its BEGIN and COMMIT hold. A part for a role not in as, or text before the first heading, is
// refused before anything runs.
func runAs(ctx context.Context, sql string, as map[string]*pgx.Conn) error {
	type part struct {
		conn *pgx.Conn
		role string
		text strings.Builder
	}
	var parts []*part
	for _, line := range strings.SplitAfter(sql, "\n") {
		if m := runAsLine.FindStringSubmatch(line); m != nil {
			p := &part{role: m[1]}
			for role, conn := range as {
				if m[1] == role || m[1] == (pgx.Identifier{role}).Sanitize() {
					p.conn = conn
				}
			}
			if p.conn == nil {
				return fmt.Errorf("a part of the SQL is to run as %s, which is not one of the roles here", m[1])
			}
			parts = append(parts, p)
		} else if len(parts) == 0 {
			if strings.TrimSpace(line) != "" {
				return fmt.Errorf("the SQL does not start with a part's heading: %q", line)
			}
			continue
		}
		parts[len(parts)-1].text.WriteString(line)
	}
	if len(parts) == 0 {
		return errors.New("the SQL has no part to run")
	}
	for _, p := range parts {
		if _, err := p.conn.PgConn().Exec(ctx, p.text.String()).ReadAll(); err != nil {
			p.conn.Exec(context.WithoutCancel(ctx), "rollback") // a part that failed inside its BEGIN
			return fmt.Errorf("as %s: %w", p.role, err)
		}
	}
	return nil
}

// compared is a table the check reads before and after.
type compared struct {
	oid          uint32
	schema, name string
	display      string // schema.name as format('%I.%I') writes it
	column       string // its single-column primary key
	key          string // the table key capture records it under; "" when it is not captured
}

// comparedTables are the ordinary tables of the captured tables' schemas, by name, each with its primary key.
func comparedTables(ctx context.Context, conn *pgx.Conn, installed []installedTable) ([]compared, error) {
	keyOf := map[uint32]string{}
	var schemas []string
	for _, t := range installed {
		keyOf[t.OID] = t.Key
		if !slices.Contains(schemas, t.Schema) {
			schemas = append(schemas, t.Schema)
		}
	}
	rows, err := conn.Query(ctx, `
		select c.oid, n.nspname, c.relname, format('%I.%I', n.nspname, c.relname),
		       (select a.attname from pg_index i join pg_attribute a on a.attrelid = i.indrelid and a.attnum = i.indkey[0]
		         where i.indrelid = c.oid and i.indisprimary and i.indnkeyatts = 1)
		  from pg_class c join pg_namespace n on n.oid = c.relnamespace
		 where c.relkind in ('r', 'p') and n.nspname = any($1)
		 order by 4`, schemas)
	if err != nil {
		return nil, err
	}
	var out []compared
	for rows.Next() {
		var t compared
		var column *string
		if err := rows.Scan(&t.oid, &t.schema, &t.name, &t.display, &column); err != nil {
			return nil, err
		}
		if column == nil {
			rows.Close()
			return nil, fmt.Errorf("table %s has no single-column primary key, and the check compares rows by key", t.display)
		}
		t.column, t.key = *column, keyOf[t.oid]
		out = append(out, t)
	}
	return out, rows.Err()
}

// fixedSettings are the five settings capture fixes while it writes values (spec 4), so that a key here reads as
// capture wrote it and both readings write each value alike.
var fixedSettings = []string{
	"set local datestyle = 'ISO, MDY'",
	"set local intervalstyle = 'postgres'",
	"set local extra_float_digits = 1",
	"set local timezone = 'UTC'",
	"set local bytea_output = 'hex'",
}

// fingerprint reads every row of the tables in one snapshot: per table, its key's text and the md5 of the row's
// text.
func fingerprint(ctx context.Context, conn *pgx.Conn, tables []compared) (map[string]map[string]string, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, s := range fixedSettings {
		if _, err := tx.Exec(ctx, s); err != nil {
			return nil, err
		}
	}
	out := map[string]map[string]string{}
	for _, t := range tables {
		rows, err := tx.Query(ctx, fmt.Sprintf("select r.%s::text, md5(row(r.*)::text) from %s r",
			ident(t.column), pgx.Identifier{t.schema, t.name}.Sanitize()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", t.display, err)
		}
		m := map[string]string{}
		for rows.Next() {
			var k, h string
			if err := rows.Scan(&k, &h); err != nil {
				return nil, fmt.Errorf("read %s: %w", t.display, err)
			}
			m[k] = h
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read %s: %w", t.display, err)
		}
		out[t.display] = m
	}
	return out, nil
}

// undoAll calls stet.undo for each agent role as the owner, in undoOrder.
func (r *undoRun) undoAll(ctx context.Context, name string, owner *pgx.Conn, since time.Time) ([]AgentUndo, error) {
	order, err := undoOrder(ctx, owner, since, r.c.Names.Agents())
	if err != nil {
		return nil, err
	}
	var out []AgentUndo
	for _, agent := range order {
		r.say("%s: undoing %s as %s", name, agent, r.c.Names.Owner())
		start := time.Now()
		var lines []UndoLine
		err := pgx.BeginTxFunc(ctx, owner, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `select target_table, key, action, outcome, reason, warning, change_ids
				from stet.undo($1, $2, NULL, NULL, $3)`, agent, since, undoNote)
			if err != nil {
				return err
			}
			lines, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (UndoLine, error) {
				var l UndoLine
				err := row.Scan(&l.Table, &l.Key, &l.Action, &l.Outcome, &l.Reason, &l.Warning, &l.ChangeIDs)
				return l, err
			})
			return err
		})
		if err != nil {
			return out, fmt.Errorf("stet.undo(%s, %s): %w", agent, since.UTC().Format(time.RFC3339Nano), err)
		}
		out = append(out, AgentUndo{Agent: agent, Lines: lines, Took: time.Since(start)})
	}
	return out, nil
}

// undoOrder is the order the agents are undone in: those with a change since since, the one whose first change
// came latest first, then the rest as given. An agent's writes often rest on another's made before (the V set's on
// its preparation, the gap faults on theirs), and undoing the later one first gives the earlier its own values back.
func undoOrder(ctx context.Context, conn *pgx.Conn, since time.Time, agents []string) ([]string, error) {
	rows, err := conn.Query(ctx, "select agent_role, min(at) from stet.history where at >= $1 group by agent_role", since)
	if err != nil {
		return nil, err
	}
	first := map[string]time.Time{}
	for rows.Next() {
		var agent string
		var at time.Time
		if err := rows.Scan(&agent, &at); err != nil {
			return nil, err
		}
		first[agent] = at
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	order := slices.Clone(agents)
	slices.SortStableFunc(order, func(a, b string) int {
		ta, oka := first[a]
		tb, okb := first[b]
		switch {
		case oka && okb:
			return tb.Compare(ta)
		case oka:
			return -1
		case okb:
			return 1
		}
		return 0
	})
	return order, nil
}

// undoHistory is what the check reads from stet.history, as the reviewer.
type undoHistory struct {
	sets    []SetCount
	first   map[string]map[string]time.Time // table key -> key -> the time of its first change
	markers map[string][]marker             // table key -> its sets that hold no values, the first of each kind
}

// marker is a change set that holds no values: its statement's rows were not captured.
type marker struct {
	agent, skipped string
	at             time.Time
}

func (m marker) describe() string { return m.agent + ": not_captured " + m.skipped }

func readHistory(ctx context.Context, conn *pgx.Conn, since time.Time) (undoHistory, error) {
	h := undoHistory{first: map[string]map[string]time.Time{}, markers: map[string][]marker{}}
	rows, err := conn.Query(ctx, `select agent_role, coalesce(skipped, ''), count(distinct set_id)::int from stet.history
		where at >= $1 group by 1, 2 order by 1, 2`, since)
	if err != nil {
		return h, err
	}
	if h.sets, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (SetCount, error) {
		var s SetCount
		err := row.Scan(&s.Agent, &s.Skipped, &s.Sets)
		return s, err
	}); err != nil {
		return h, err
	}
	rows, err = conn.Query(ctx, `select target_table, key, min(at) from stet.history
		where at >= $1 and key is not null group by 1, 2`, since)
	if err != nil {
		return h, err
	}
	for rows.Next() {
		var table, key string
		var at time.Time
		if err := rows.Scan(&table, &key, &at); err != nil {
			return h, err
		}
		if h.first[table] == nil {
			h.first[table] = map[string]time.Time{}
		}
		h.first[table][key] = at
	}
	if err := rows.Err(); err != nil {
		return h, err
	}
	rows, err = conn.Query(ctx, `select target_table, agent_role, skipped, min(at) from stet.history
		where at >= $1 and skipped is not null group by 1, 2, 3 order by 1, 2, 3`, since)
	if err != nil {
		return h, err
	}
	for rows.Next() {
		var table string
		var m marker
		if err := rows.Scan(&table, &m.agent, &m.skipped, &m.at); err != nil {
			return h, err
		}
		h.markers[table] = append(h.markers[table], m)
	}
	return h, rows.Err()
}

// agentLine is a line of one agent's undo.
type agentLine struct {
	agent string
	UndoLine
}

func (l agentLine) describe() string { return l.agent + ": " + l.text() }

// text is the line's outcome and reason.
func (l UndoLine) text() string {
	if l.Reason == nil {
		return l.Outcome
	}
	return l.Outcome + " " + *l.Reason
}

// explainer finds, for a row that is not what it was, the reason the system reported.
type explainer struct {
	lines      map[string]map[string][]agentLine // table key -> key -> its lines
	keyless    map[string][]agentLine            // table key -> its lines without a key
	h          undoHistory
	uncaptured []string // the roles that sent statements and are not captured
}

func newExplainer(undone []AgentUndo, h undoHistory, uncaptured []string) explainer {
	e := explainer{lines: map[string]map[string][]agentLine{}, keyless: map[string][]agentLine{}, h: h, uncaptured: uncaptured}
	for _, u := range undone {
		for _, l := range u.Lines {
			al := agentLine{agent: u.Agent, UndoLine: l}
			if l.Key == nil {
				e.keyless[l.Table] = append(e.keyless[l.Table], al)
				continue
			}
			if e.lines[l.Table] == nil {
				e.lines[l.Table] = map[string][]agentLine{}
			}
			e.lines[l.Table][*l.Key] = append(e.lines[l.Table][*l.Key], al)
		}
	}
	return e
}

// reason accounts for a row of t, by key, that is not what it was, from what the system reported, or returns "":
//
//   - t is not captured;
//   - a line of the key that is not restored (skipped, failed, restored_differs) says why;
//   - a key with no line (no captured change) is accounted for by the table's lines without a key: a set that holds
//     no values, of a statement over max_rows, say;
//   - a key whose lines all say restored is accounted for by a set without values recorded before its first change
//     (the row came back to what that statement had made it);
//   - otherwise, statements were sent as a role that is not captured.
func (e explainer) reason(t compared, key string) string {
	if t.key == "" {
		return "table not captured"
	}
	var why []string
	lines := e.lines[t.key][key]
	for _, l := range lines {
		if l.Outcome != "restored" {
			why = append(why, l.describe())
		}
	}
	if len(why) == 0 && len(lines) == 0 {
		for _, l := range e.keyless[t.key] {
			why = append(why, l.describe())
		}
	} else if len(why) == 0 {
		if first, ok := e.h.first[t.key][key]; ok {
			for _, m := range e.h.markers[t.key] {
				if m.at.Before(first) {
					why = append(why, m.describe())
				}
			}
		}
	}
	if len(why) == 0 && len(e.uncaptured) > 0 {
		why = append(why, "changed by a role not captured ("+strings.Join(e.uncaptured, ", ")+")")
	}
	slices.Sort(why)
	return strings.Join(slices.Compact(why), "; ")
}

// compareTables compares each table's rows after with those before, key by key.
func compareTables(tables []compared, before, after map[string]map[string]string, e explainer) []TableCompare {
	out := make([]TableCompare, 0, len(tables))
	for _, t := range tables {
		b, a := before[t.display], after[t.display]
		tc := TableCompare{Table: t.display, Key: t.key, Captured: t.key != "", Rows: len(b), Reasons: map[string]int{}}
		diff := func(key, kind string) {
			d := RowDiff{Key: key, Kind: kind, Reason: e.reason(t, key)}
			tc.Diffs = append(tc.Diffs, d)
			tc.Reasons[d.Reason]++
			switch kind {
			case "different":
				tc.Different++
			case "missing":
				tc.Missing++
			case "extra":
				tc.Extra++
			}
		}
		for k, hb := range b {
			switch ha, ok := a[k]; {
			case !ok:
				diff(k, "missing")
			case ha != hb:
				diff(k, "different")
			default:
				tc.Equal++
			}
		}
		for k := range a {
			if _, ok := b[k]; !ok {
				diff(k, "extra")
			}
		}
		slices.SortFunc(tc.Diffs, func(x, y RowDiff) int { return keyOrder(x.Key, y.Key) })
		out = append(out, tc)
	}
	return out
}

// keyOrder orders keys as numbers when both are digits, else as text.
func keyOrder(a, b string) int {
	if digits(a) && digits(b) {
		return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
	}
	return strings.Compare(a, b)
}

func digits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// paths follows the rows each of the check's paths changed in the clone: the rows capture recorded under its
// statements' text (they start with the statement's tag comment), whether the line of the agent that changed each
// says restored, and whether each is now what it was.
func (r *undoRun) paths(ctx context.Context, conn *pgx.Conn, cl UndoClone, outcomes map[string]Outcome, tables []compared,
	before, after map[string]map[string]string, e explainer) ([]PathResult, error) {
	byKey := map[string]compared{}
	for _, t := range tables {
		if t.key != "" {
			byKey[t.key] = t
		}
	}
	var out []PathResult
	for _, path := range r.c.Paths {
		p := PathResult{Path: path, Results: map[string]int{}}
		var tags []string
		for i := cl.From; i < cl.Next && i < len(r.attacks); i++ {
			if r.attacks[i].Path != path {
				continue
			}
			for _, id := range r.attacks[i].Steps {
				o, ok := outcomes[id]
				if !ok {
					continue
				}
				p.Statements++
				p.Results[o.Result]++
				if tag := r.byID[id].Tag; tag != "" {
					tags = append(tags, tag)
				}
			}
		}
		if p.Statements == 0 {
			continue
		}
		type row struct{ table, key string }
		changed := map[row]string{} // -> the agent that changed it
		skipped := map[int64]bool{}
		for _, tag := range tags {
			rows, err := conn.Query(ctx, `select set_id, target_table, key, agent_role, skipped is not null from stet.history
				where at >= $1 and starts_with(query, $2)`, cl.Since, tasks.Tagged(tag, ""))
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var set int64
				var table, agent string
				var key *string
				var skip bool
				if err := rows.Scan(&set, &table, &key, &agent, &skip); err != nil {
					return nil, err
				}
				if skip {
					skipped[set] = true
				} else if key != nil {
					changed[row{table, *key}] = agent
				}
			}
			if err := rows.Err(); err != nil {
				return nil, err
			}
		}
		p.Changed, p.NotCaptured = len(changed), len(skipped)
		keys := slices.SortedFunc(maps.Keys(changed), func(x, y row) int {
			return cmp.Or(strings.Compare(x.table, y.table), keyOrder(x.key, y.key))
		})
		for _, k := range keys {
			restored := slices.ContainsFunc(e.lines[k.table][k.key], func(l agentLine) bool {
				return l.agent == changed[k] && l.Outcome == "restored"
			})
			t, ok := byKey[k.table]
			hb, okb := before[t.display][k.key]
			ha, oka := after[t.display][k.key]
			equal := ok && okb == oka && hb == ha
			if restored {
				p.Restored++
			}
			if equal {
				p.Equal++
			}
			if !restored || !equal {
				why := "no line"
				var lines []string
				for _, l := range e.lines[k.table][k.key] {
					lines = append(lines, l.describe())
				}
				if len(lines) > 0 {
					why = strings.Join(lines, "; ")
				}
				if restored && !equal {
					why += "; the row is not what it was: " + cmp.Or(e.reason(t, k.key), "unexplained")
				}
				p.NotBack = append(p.NotBack, fmt.Sprintf("%s %s: %s", k.table, k.key, why))
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// Write writes the result as text: per clone, what was sent and recorded, each agent's undo lines by outcome and
// reason, each table's rows by comparison and the rows not equal by reason (with up to five keys each, and every
// unexplained row), and the followed paths; then the totals and whether the check passed.
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
		p("  undo, in the order run (lines by outcome and reason):")
		for _, u := range c.Undone {
			count := map[string]int{}
			for _, l := range u.Lines {
				count[l.text()]++
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
			for _, reason := range sortedByCount(t.Reasons) {
				var examples []string
				for _, d := range t.Diffs {
					if d.Reason == reason && len(examples) < 5 {
						examples = append(examples, d.Key+" "+d.Kind)
					}
				}
				p("      %d %s (%s%s)", t.Reasons[reason], cmp.Or(reason, "UNEXPLAINED"), strings.Join(examples, ", "),
					more(t.Reasons[reason], len(examples)))
			}
		}
		p("  unexplained rows: %d", c.Unexplained())
		for _, t := range c.Tables {
			for _, d := range t.Diffs {
				if d.Reason == "" {
					p("    %s %s %s", t.Table, d.Key, d.Kind)
				}
			}
		}
		if len(c.Paths) > 0 {
			p("  paths:")
		}
		for _, pr := range c.Paths {
			verdict := "not all restored"
			if pr.AllRestored() {
				verdict = "all restored"
			}
			p("    %s: statements %d (%s); rows changed as capture recorded them %d, sets without values %d; "+
				"restored %d, now as they were %d: %s", pr.Path, pr.Statements, resultCounts(pr.Results), pr.Changed,
				pr.NotCaptured, pr.Restored, pr.Equal, verdict)
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
					v := "not all restored"
					if pr.AllRestored() {
						v = "all restored"
					}
					said = append(said, fmt.Sprintf("%s: rows changed %d, restored %d, as they were %d: %s", c.Database,
						pr.Changed, pr.Restored, pr.Equal, v))
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

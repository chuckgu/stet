package bench

// This file sends the X set (docs/spec-v0.2-benchmark.md, chapter 4 "X"):
// each attack's steps in order, judged by a probe the owner runs against the
// database itself before the first step and after the last. The client's
// answers are recorded but never decide whether an attack executed: a
// statement may run although its answer was lost, or be refused although
// part of it ran.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/stetdb/stet/internal/lab/tasks"
)

// AttackResult is one row of lab.attacks.
type AttackResult struct {
	Arm                 string
	Index               int // position in the attack list
	Path, Code, Payload string
	Database            string
	Before, After       int64 // the probe before the first step and after the last
	Executed            bool  // After-Before > Limit; false when the attack has no probe
	// A probe query failed, or the database did not settle before the after probe: Before and After are NULL
	// in lab.attacks, Executed is false.
	Undetermined bool
}

// RunAttacks runs attacks[from:] in order. It stops after an attack that Breaks and was executed or is
// undetermined, unless it was the last one, and then returns the index to go on from and reclone == true: an
// undetermined destructive path may have run and voided the paths after it.
//
// owner is the owner's connection to t.Database itself, not through the arm's proxy: it runs the probes,
// resolves the OIDs of FunctionCall steps, opens the connection that holds an attack's lock, and waits for the
// database to settle before each after probe. stmts holds the steps by ID. Before the first attack RunAttacks
// connects once to t as that attack's first step would, and when that fails it returns an error having sent and
// recorded nothing. Every step's outcome goes to save and every attack's result to saveAttack, one call at a
// time. On an error, next is the attack that was not finished: its result was not saved, though some of its
// steps' outcomes may have been.
func RunAttacks(ctx context.Context, t Target, owner *pgx.Conn, attacks []tasks.Attack, stmts map[string]tasks.Stmt,
	from int, o Options, save func(Outcome) error, saveAttack func(AttackResult) error) (next int, reclone bool, err error) {
	if from < 0 || from > len(attacks) {
		return from, false, fmt.Errorf("run attacks: from %d is outside the %d attacks", from, len(attacks))
	}
	if db := owner.Config().Database; db != t.Database {
		return from, false, fmt.Errorf("run attacks: the owner's connection is to %s and the attacks go to %s: "+
			"the probes must read the database the attacks change", db, t.Database)
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if from < len(attacks) && len(attacks[from].Steps) > 0 {
		if s, ok := stmts[attacks[from].Steps[0]]; ok {
			if err := reach(ctx, t, s.Role, s.App); err != nil {
				return from, false, fmt.Errorf("run attacks: %w", err)
			}
		}
	}
	x := &attackRun{t: t, owner: owner, stmts: stmts, o: o, lim: newLimiter(o.PerSecond), oids: map[string]uint32{},
		save: save}
	for i := from; i < len(attacks); i++ {
		a := attacks[i]
		r, err := x.run(ctx, i, a)
		if err == nil {
			err = saveAttack(r)
		}
		if err != nil {
			return i, false, fmt.Errorf("attack %d (%s): %w", i, a.Path, err)
		}
		if a.Breaks && (r.Executed || r.Undetermined) && i < len(attacks)-1 {
			return i + 1, true, nil
		}
	}
	return len(attacks), false, nil
}

// SaveAttack stores r in lab.attacks; an undetermined attack's before and after are NULL.
func SaveAttack(ctx context.Context, conn *pgx.Conn, r AttackResult) error {
	var before, after *int64
	if !r.Undetermined {
		before, after = &r.Before, &r.After
	}
	_, err := conn.Exec(ctx, `insert into lab.attacks (arm, idx, path, code, payload, database, before, after, executed)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		r.Arm, r.Index, r.Path, r.Code, r.Payload, r.Database, before, after, r.Executed)
	if err != nil {
		return fmt.Errorf("save the result of attack %d (%s): %w", r.Index, r.Path, err)
	}
	return nil
}

// CheckResume refuses to send attacks[from:] for arm, as recorded through conn, unless the run goes on exactly
// where the recorded one stopped: lab.attacks holds the results of the arm's attacks 0 to from-1 and of none
// after, and lab.outcomes holds no step of attacks[from:]. Its error says how to go on.
func CheckResume(ctx context.Context, conn *pgx.Conn, arm string, attacks []tasks.Attack, from int) error {
	rows, err := conn.Query(ctx, "select idx from lab.attacks where arm = $1 order by idx", arm)
	if err != nil {
		return err
	}
	done, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return fmt.Errorf("read lab.attacks: %w", err)
	}
	stop := 0 // the first attack without a result
	for stop < len(done) && done[stop] == stop {
		stop++
	}
	path := func(i int) string {
		if i < len(attacks) {
			return attacks[i].Path
		}
		return "?"
	}
	switch {
	case stop < len(done):
		return fmt.Errorf("lab.attacks holds no result of arm %s's attack %d (%s) but holds results of later ones: "+
			"the run stopped at attack %d. Drop the arm's rows of attacks %d on (lab.attacks with idx >= %d, and "+
			"their steps in lab.outcomes), re-clone the X database and send -from %d", arm, stop, path(stop), stop,
			stop, stop, stop)
	case from != stop && stop == len(attacks):
		return fmt.Errorf("all %d attacks of arm %s have results; nothing is left to send", len(attacks), arm)
	case from != stop && stop == 0:
		return fmt.Errorf("lab.attacks holds no result of arm %s yet: send -from 0", arm)
	case from != stop:
		return fmt.Errorf("lab.attacks holds the results of arm %s's attacks 0 to %d: send -from %d", arm, stop-1, stop)
	}

	of := map[string]int{} // step -> its attack
	var ids []string
	for i := from; i < len(attacks); i++ {
		for _, id := range attacks[i].Steps {
			of[id] = i
			ids = append(ids, id)
		}
	}
	rows, err = conn.Query(ctx, "select id from lab.outcomes where arm = $1 and id = any($2) order by id", arm, ids)
	if err != nil {
		return err
	}
	sent, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("read lab.outcomes: %w", err)
	}
	if len(sent) == 0 {
		return nil
	}
	cut := len(attacks)
	for _, id := range sent {
		cut = min(cut, of[id])
	}
	quoted := make([]string, len(sent))
	for i, id := range sent {
		quoted[i] = "'" + strings.ReplaceAll(id, "'", "''") + "'"
	}
	return fmt.Errorf("attack %d (%s) of arm %s was cut off: lab.outcomes holds steps of it (%s) but lab.attacks no "+
		"result. Drop those rows (delete from lab.outcomes where arm = '%s' and id in (%s)), re-clone the X database "+
		"and send -from %d", cut, path(cut), arm, strings.Join(sent, ", "), strings.ReplaceAll(arm, "'", "''"),
		strings.Join(quoted, ", "), cut)
}

// attackRun is one RunAttacks call.
type attackRun struct {
	t      Target
	owner  *pgx.Conn
	stmts  map[string]tasks.Stmt
	o      Options
	lim    *limiter          // shared by the steps sent one after another
	oids   map[string]uint32 // the function of each FunctionCall step, by step ID
	saving sync.Mutex
	save   func(Outcome) error
}

// run runs attack i: the pause, the probe, the connections, the lock, the steps, the lock's release, the
// database settling, the probe.
func (x *attackRun) run(ctx context.Context, i int, a tasks.Attack) (AttackResult, error) {
	r := AttackResult{Arm: x.o.Arm, Index: i, Path: a.Path, Code: a.Code, Payload: a.Payload, Database: x.t.Database}
	steps := make([]tasks.Stmt, len(a.Steps))
	for j, id := range a.Steps {
		s, ok := x.stmts[id]
		if !ok {
			return r, fmt.Errorf("step %s is not in the stream", id)
		}
		steps[j] = s
	}
	if a.PauseBefore > 0 {
		x.o.Sleep(a.PauseBefore)
	}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	before, okBefore, err := x.probe(ctx, a.Probe)
	if err != nil {
		return r, err
	}
	if err := x.resolve(ctx, steps); err != nil {
		return r, err
	}

	// The connections are opened before the lock is taken, so that the
	// lock's time covers the statements, not the connecting.
	rt := x.route(ctx, a, steps)
	defer rt.hangUp()
	if err := ctx.Err(); err != nil {
		return r, err
	}
	var released <-chan error
	if a.LockSQL != "" {
		if released, err = x.lock(ctx, a.LockSQL, a.LockFor); err != nil {
			return r, err
		}
	}
	err = x.send(ctx, rt, steps)
	if released != nil {
		err = errors.Join(err, <-released)
	}
	if err != nil {
		return r, err
	}

	settled, err := x.settle(ctx)
	if err != nil {
		return r, err
	}
	after, okAfter, err := x.probe(ctx, a.Probe)
	if err != nil {
		return r, err
	}
	r.Before, r.After = before, after
	r.Undetermined = !okBefore || !settled || !okAfter
	r.Executed = a.Probe != "" && !r.Undetermined && after-before > a.Limit
	return r, nil
}

// probe runs sql, which returns one bigint, on the owner's connection; no sql reads 0. A query that fails leaves
// ok false. The error is for what stops the run: ctx ended, or the owner's connection is gone.
func (x *attackRun) probe(ctx context.Context, sql string) (v int64, ok bool, err error) {
	if sql == "" {
		return 0, true, nil
	}
	qerr := x.owner.QueryRow(ctx, sql).Scan(&v)
	switch {
	case qerr == nil:
		return v, true, nil
	case ctx.Err() != nil:
		return 0, false, ctx.Err()
	case x.owner.IsClosed():
		return 0, false, fmt.Errorf("probe %q: %w", sql, qerr)
	}
	return 0, false, nil
}

// How often settle looks; tests may shorten it. Without o.Timeout it waits at most settleDefault.
var (
	settleEvery   = 25 * time.Millisecond
	settleDefault = 2 * time.Minute
)

// busySQL counts the other sessions on the owner's database that are inside a transaction. Every transaction
// holds the lock on its own virtual transaction ID until it ends, and an idle session holds no lock; a statement
// waiting for a row lock is inside its transaction too. pg_locks is read because pg_stat_activity shows the
// owner no other role's state (it is no member of pg_read_all_stats, and the benchmark leaves existing roles
// alone); a session's database and user are shown to everyone. A NULL usesysid is a background process, such
// as autovacuum.
const busySQL = `select count(distinct l.pid) from pg_locks l join pg_stat_activity a on a.pid = l.pid
	where a.datname = current_database() and a.usesysid is not null and a.pid <> pg_backend_pid()`

// settle waits until no other session on the owner's database is inside a transaction, looking every
// settleEvery for at most o.Timeout; ok is false when it never saw that, or when its query failed. A
// statement whose client stopped waiting (an arm that hung up after forwarding it, the timeout, an interrupt)
// still runs in the database, and an after probe read before it commits would count an executed payload as
// caught. A proxy's own sessions may be busy for a moment (Stet looks for approved writes every second), so one
// reading of none is enough.
//
// Known limit: a statement still queued inside a proxy, which has not reached the database, is not seen.
func (x *attackRun) settle(ctx context.Context) (ok bool, err error) {
	within := x.o.Timeout
	if within <= 0 {
		within = settleDefault
	}
	deadline := time.Now().Add(within)
	for {
		var busy int64
		qerr := x.owner.QueryRow(ctx, busySQL).Scan(&busy)
		switch {
		case qerr == nil && busy == 0:
			return true, nil
		case qerr != nil && ctx.Err() != nil:
			return false, ctx.Err()
		case qerr != nil && x.owner.IsClosed():
			return false, fmt.Errorf("settle: %w", qerr)
		case qerr != nil:
			return false, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(settleEvery):
		}
	}
}

// resolve finds the function of each FunctionCall step, such as set_consent(integer), and keeps its OID.
func (x *attackRun) resolve(ctx context.Context, steps []tasks.Stmt) error {
	for _, s := range steps {
		if s.Wire != tasks.WireFunctionCall {
			continue
		}
		var oid uint32
		if err := x.owner.QueryRow(ctx, "select $1::text::regprocedure::oid", s.SQL).Scan(&oid); err != nil {
			return fmt.Errorf("step %s: the OID of %s: %w", s.ID, s.SQL, err)
		}
		x.oids[s.ID] = oid
	}
	return nil
}

// lock runs sql in a transaction on a connection of the owner's own and returns once it holds the lock. The
// transaction is committed after d, or as soon as ctx ends, and the connection closed; the channel then reports
// how that went.
func (x *attackRun) lock(ctx context.Context, sql string, d time.Duration) (<-chan error, error) {
	c, err := pgx.ConnectConfig(ctx, x.owner.Config())
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	tx, err := c.Begin(ctx)
	if err == nil {
		_, err = tx.Exec(ctx, sql)
	}
	if err != nil {
		c.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("lock %q: %w", sql, err)
	}
	released := make(chan error, 1)
	go func() {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		end, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		err := tx.Commit(end)
		released <- errors.Join(err, c.Close(end))
	}()
	return released, nil
}

// route is the connections one attack's steps go over.
type route struct {
	workers []*worker // each holds at most one connection at a time
	via     []*worker // the worker of each step
	atOnce  bool
}

// route assigns the steps to workers and opens their connections. With a.Conns > 1, step i goes over connection
// i mod a.Conns and all are opened at once; otherwise each role and application gets one connection. A worker
// whose connection could not be opened tries again at its first step, which records the failure.
func (x *attackRun) route(ctx context.Context, a tasks.Attack, steps []tasks.Stmt) *route {
	rt := &route{via: make([]*worker, len(steps))}
	if a.Conns > 1 && len(steps) > 1 {
		rt.atOnce = true
		for range min(a.Conns, len(steps)) {
			rt.workers = append(rt.workers, &worker{})
		}
		for i := range steps {
			rt.via[i] = rt.workers[i%len(rt.workers)]
		}
	} else {
		byKey := map[[2]string]*worker{}
		for i, s := range steps {
			k := [2]string{s.Role, s.App}
			w, ok := byKey[k]
			if !ok {
				w = &worker{}
				byKey[k] = w
				rt.workers = append(rt.workers, w)
			}
			rt.via[i] = w
		}
	}
	// Each worker connects as its first step needs; a FunctionCall step
	// brings its own connection.
	first := map[*worker]tasks.Stmt{}
	for i, s := range steps {
		if _, ok := first[rt.via[i]]; !ok && s.Wire != tasks.WireFunctionCall {
			first[rt.via[i]] = s
		}
	}
	var wg sync.WaitGroup
	for w, s := range first {
		wg.Go(func() {
			if c, err := connect(ctx, x.t, s.Role, s.App); err == nil {
				w.conn, w.role, w.app = c, s.Role, s.App
			}
		})
	}
	wg.Wait()
	return rt
}

func (rt *route) hangUp() {
	for _, w := range rt.workers {
		w.hangUp()
	}
}

// send sends the steps and reports each outcome. One after another, each step waits for the shared rate; at
// once, the workers send their steps in parallel without it. It stops at the first error save returns, or when
// ctx ends: no step starts after that, and one already sent finishes and is reported as the far side answered.
func (x *attackRun) send(ctx context.Context, rt *route, steps []tasks.Stmt) error {
	if !rt.atOnce {
		for i, s := range steps {
			out, err := x.step(ctx, rt.via[i], s, x.lim)
			if err == nil {
				err = x.report(out)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	for k, w := range rt.workers {
		wg.Go(func() {
			for i := k; i < len(steps) && ctx.Err() == nil; i += len(rt.workers) {
				out, err := x.step(ctx, w, steps[i], nil)
				if err == nil {
					err = x.report(out)
				}
				if err != nil {
					cancel(err)
					return
				}
			}
		})
	}
	wg.Wait()
	return context.Cause(ctx)
}

func (x *attackRun) report(out Outcome) error {
	x.saving.Lock()
	defer x.saving.Unlock()
	return x.save(out)
}

// step sends s as Send sends a statement, over w; a FunctionCall goes over a connection of its own, closed
// after it.
func (x *attackRun) step(ctx context.Context, w *worker, s tasks.Stmt, lim *limiter) (Outcome, error) {
	if s.Wire == tasks.WireFunctionCall {
		w = &worker{}
		defer w.hangUp()
	}
	return w.send(ctx, x.t, s, x.o, lim, x.exec)
}

// exec sends s over c: a FunctionCall message of its function, within o.Timeout, or else as sendOne does.
func (x *attackRun) exec(ctx context.Context, c *pgconn.PgConn, s tasks.Stmt) (pgconn.CommandTag, error) {
	if s.Wire == tasks.WireFunctionCall {
		return callFunction(ctx, c, x.oids[s.ID], s.Args, x.o.Timeout)
	}
	return sendOne(ctx, c, s, x.o.Bind, x.o)
}

// callFunction sends a FunctionCall message for the function oid with args as text and reads until the server is
// ready for the next message or reports an error.
func callFunction(ctx context.Context, c *pgconn.PgConn, oid uint32, args []string, timeout time.Duration) (pgconn.CommandTag, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	values := make([][]byte, len(args))
	for i, a := range args {
		values[i] = []byte(a)
	}
	fe := c.Frontend()
	fe.Send(&pgproto3.FunctionCall{Function: oid, ArgFormatCodes: []uint16{0}, Arguments: values, ResultFormatCode: 0})
	if err := fe.Flush(); err != nil {
		return pgconn.CommandTag{}, err
	}
	for {
		msg, err := c.ReceiveMessage(ctx)
		if err != nil {
			return pgconn.CommandTag{}, err // a FATAL error comes this way, the connection closed
		}
		switch m := msg.(type) {
		case *pgproto3.ErrorResponse:
			return pgconn.CommandTag{}, pgconn.ErrorResponseToPgError(m)
		case *pgproto3.ReadyForQuery:
			return pgconn.CommandTag{}, nil
		}
	}
}

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
	Undetermined        bool  // a probe query failed: Before and After are NULL in lab.attacks, Executed is false
}

// RunAttacks runs attacks[from:] in order. It stops after an attack that Breaks and was executed, unless it was
// the last one, and then returns the index to go on from and reclone == true.
//
// owner is the owner's connection to t.Database, not through the arm's proxy: it runs the probes, resolves the
// OIDs of FunctionCall steps and opens the connection that holds an attack's lock. stmts holds the steps by ID.
// Every step's outcome goes to save and every attack's result to saveAttack, one call at a time. On an error,
// next is the attack that was not finished; its result was not saved.
func RunAttacks(ctx context.Context, t Target, owner *pgx.Conn, attacks []tasks.Attack, stmts map[string]tasks.Stmt,
	from int, o Options, save func(Outcome) error, saveAttack func(AttackResult) error) (next int, reclone bool, err error) {
	if from < 0 || from > len(attacks) {
		return from, false, fmt.Errorf("run attacks: from %d is outside the %d attacks", from, len(attacks))
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	x := &attackRun{t: t, owner: owner, stmts: stmts, o: o, lim: newLimiter(o.PerSecond), save: save}
	for i := from; i < len(attacks); i++ {
		a := attacks[i]
		r, err := x.run(ctx, i, a)
		if err == nil {
			err = saveAttack(r)
		}
		if err != nil {
			return i, false, fmt.Errorf("attack %d (%s): %w", i, a.Path, err)
		}
		if a.Breaks && r.Executed && i < len(attacks)-1 {
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

// attackRun is one RunAttacks call.
type attackRun struct {
	t      Target
	owner  *pgx.Conn
	stmts  map[string]tasks.Stmt
	o      Options
	lim    *limiter // shared by the steps sent one after another
	saving sync.Mutex
	save   func(Outcome) error
}

// run runs attack i: the pause, the probe, the connections, the lock, the steps, the lock's release, the probe.
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
	oids, err := x.functionOIDs(ctx, steps)
	if err != nil {
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
	err = x.send(ctx, rt, steps, oids)
	if released != nil {
		err = errors.Join(err, <-released) // the after probe must not read a write still waiting for the lock
	}
	if err != nil {
		return r, err
	}

	after, okAfter, err := x.probe(ctx, a.Probe)
	if err != nil {
		return r, err
	}
	r.Before, r.After = before, after
	r.Undetermined = !okBefore || !okAfter
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

// functionOIDs resolves the function of each FunctionCall step, such as set_consent(integer), to its OID, by ID.
func (x *attackRun) functionOIDs(ctx context.Context, steps []tasks.Stmt) (map[string]uint32, error) {
	oids := map[string]uint32{}
	for _, s := range steps {
		if s.Wire != tasks.WireFunctionCall {
			continue
		}
		var oid uint32
		if err := x.owner.QueryRow(ctx, "select $1::text::regprocedure::oid", s.SQL).Scan(&oid); err != nil {
			return nil, fmt.Errorf("step %s: the OID of %s: %w", s.ID, s.SQL, err)
		}
		oids[s.ID] = oid
	}
	return oids, nil
}

// lock runs sql in a transaction on a connection of the owner's own and returns once it holds the lock. The
// transaction is committed after d, or as soon as ctx ends; the channel then reports how that went.
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
// ctx ends.
func (x *attackRun) send(ctx context.Context, rt *route, steps []tasks.Stmt, oids map[string]uint32) error {
	if !rt.atOnce {
		for i, s := range steps {
			out, err := x.step(ctx, rt.via[i], s, oids[s.ID], x.lim)
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
				out, err := x.step(ctx, w, steps[i], oids[steps[i].ID], nil)
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

// step sends s over w, connecting first when w has no connection or one of another role or application; a
// FunctionCall goes over a connection of its own, closed after it. With lim, it waits for the rate after
// connecting. A step that did not execute leaves w without a connection: hoop hangs up on every denial. It
// returns an error only when ctx ended before s was sent.
func (x *attackRun) step(ctx context.Context, w *worker, s tasks.Stmt, oid uint32, lim *limiter) (Outcome, error) {
	if s.Wire == tasks.WireFunctionCall {
		w = &worker{}
		defer w.hangUp()
	}
	if w.conn == nil || w.conn.IsClosed() || w.role != s.Role || w.app != s.App {
		w.hangUp()
		c, err := connect(ctx, x.t, s.Role, s.App)
		if err != nil {
			if ctx.Err() != nil {
				return Outcome{}, ctx.Err()
			}
			// Not classified: a server that refuses the session refused no statement.
			return x.outcome(s, time.Now(), 0, Outcome{Result: Unknown, Message: err.Error()}), nil
		}
		w.conn, w.role, w.app = c, s.Role, s.App
	}
	if lim != nil {
		if err := lim.wait(ctx); err != nil {
			return Outcome{}, err
		}
	}
	start := time.Now()
	tag, err := x.exec(ctx, w.conn, s, oid)
	out := x.outcome(s, start, time.Since(start), Classify(tag, err))
	if out.Result != Executed {
		w.hangUp()
	}
	return out, nil
}

func (x *attackRun) outcome(s tasks.Stmt, start time.Time, took time.Duration, out Outcome) Outcome {
	out.Arm, out.ID, out.Tag, out.Set = x.o.Arm, s.ID, s.Tag, s.Set
	out.SentAt, out.LatencyMS = start, float64(took.Nanoseconds())/1e6
	return out
}

// exec sends s over c within o.Timeout, the way its Wire says: a FunctionCall message of the function oid, a
// COPY with its data, or else as sendOne does, with any @PAD@ written out first.
func (x *attackRun) exec(ctx context.Context, c *pgconn.PgConn, s tasks.Stmt, oid uint32) (pgconn.CommandTag, error) {
	switch {
	case s.Wire == tasks.WireFunctionCall:
		return callFunction(ctx, c, oid, s.Args, x.o.Timeout)
	case s.Wire == tasks.WireCopy:
		ctx, cancel := withTimeout(ctx, x.o.Timeout)
		defer cancel()
		return c.CopyFrom(ctx, strings.NewReader(s.CopyData), tagged(s.Tag, s.SQL))
	case s.Pad > 0:
		n := s.Pad
		if x.o.MaxPad > 0 {
			n = min(n, x.o.MaxPad)
		}
		// Only the text that is sent is padded, with its tag, and then
		// sent as it is.
		if binds(s, x.o.Bind) {
			s.SQL = padded(s.Tag, s.SQL, n)
		} else {
			s.Inline = padded(s.Tag, s.Inline, n)
		}
		s.Tag = ""
	}
	return sendOne(ctx, c, s, x.o.Bind, x.o)
}

// binds says whether sendOne sends s with its values bound.
func binds(s tasks.Stmt, bind bool) bool {
	switch s.Wire {
	case tasks.WireSimple:
		return false
	case tasks.WireBind:
		return true
	}
	return bind
}

const padToken = "@PAD@"

// padded returns sql with its tag in front and every @PAD@ written as n x's. The text is built in one piece, so
// that a 64 MiB message is held once rather than once per step of its making.
func padded(tag, sql string, n int) string {
	prefix := tagged(tag, "")
	var b strings.Builder
	b.Grow(len(prefix) + len(sql) + strings.Count(sql, padToken)*(n-len(padToken)))
	b.WriteString(prefix)
	chunk := strings.Repeat("x", min(n, 64<<10))
	for {
		i := strings.Index(sql, padToken)
		if i < 0 {
			break
		}
		b.WriteString(sql[:i])
		for left := n; left > 0; left -= len(chunk) {
			b.WriteString(chunk[:min(left, len(chunk))])
		}
		sql = sql[i+len(padToken):]
	}
	b.WriteString(sql)
	return b.String()
}

// callFunction sends a FunctionCall message for the function oid with args as text and reads until the server is
// ready for the next message or reports an error.
func callFunction(ctx context.Context, c *pgconn.PgConn, oid uint32, args []string, timeout time.Duration) (pgconn.CommandTag, error) {
	ctx, cancel := withTimeout(ctx, timeout)
	defer cancel()
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

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return ctx, func() {}
}

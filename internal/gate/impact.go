package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/sqlparse"
)

// queryTimeout is the statement_timeout of a measurement's queries other
// than the count (the prepare, the catalog reads and EXPLAIN), and of the
// decoding of binary bind values.
const queryTimeout = 200 * time.Millisecond

// countTimeout is the statement_timeout of the count. A count that runs past
// it falls back to the planner's estimate (spec 4). A var so tests can
// shorten it; while it equals queryTimeout, no extra SET is sent.
var countTimeout = queryTimeout

// rollbackTimeout bounds the rollback that ends every measurement. It has its
// own context because the measurement's may already have expired.
const rollbackTimeout = time.Second

var onDelete = map[string]string{"c": "cascade", "n": "set_null", "d": "set_default"}

// measure asks the database what a measured write would touch. It never
// returns nil: a failure is reported in Impact.Error.
func (g *Gate) measure(ctx context.Context, p *pending, lim *config.Impact) *decide.Impact {
	start := time.Now()
	im := &decide.Impact{}
	switch m := p.res.Measure; {
	case m == nil:
		im.Error = "no measure"
	case m.Method == sqlparse.MethodSyntax:
		im.Method, im.Rows = string(m.Method), ptr(int64(m.Rows))
	default:
		err := g.queryImpact(ctx, p, m, lim, im)
		if ctx.Err() != nil {
			err = errors.New("timeout")
		}
		if err != nil {
			im.Method, im.Rows, im.Capped, im.Error = "", nil, false, err.Error()
		}
	}
	// A capped count's rows is the cap, so its share would not be the write's.
	// The verdict needs none: a capped count is already over max_rows.
	if p.res.Op != "insert" && im.Rows != nil && im.TableRows != nil && !im.Capped {
		im.Share = ptr(float64(*im.Rows) / float64(*im.TableRows))
	}
	im.MeasureMS = int(time.Since(start).Milliseconds())
	return im
}

// queryImpact fills im from the database, on one pooled connection, in a
// read-only transaction as the client's role.
//
// SET LOCAL ROLE is not a security boundary: a predicate can undo it with
// built-in functions. The boundary is what runs here. The agent's statement
// is only prepared and EXPLAINed (never with ANALYZE), which does not execute
// it; the only predicate executed is Measure.CountSQL, which sqlparse builds
// for vetted predicates only, and only when the method is count and every
// name the predicate qualifies by its table is one of the table's columns.
//
// Bind values arrive here as text. Binary ones were decoded before, by
// paramTexts, which prepares the statement as Stet's own account, also in a
// read-only transaction under the timeout.
func (g *Gate) queryImpact(ctx context.Context, p *pending, m *sqlparse.Measure, lim *config.Impact, im *decide.Impact) error {
	// No role to measure as: the proxy could not read the startup packet, or
	// the statement came before the session authenticated, so the role the
	// packet names is unproved. Measuring anyway would count as Stet's own
	// account.
	if p.session.User == "" {
		return errors.New("no login role: the startup packet could not be read, or the session had not authenticated yet")
	}
	pg, done, err := g.txConn(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer done()

	if err := pg.Exec(ctx, beginReadOnly(p.session.User)).Close(); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42501" { // not a member of the role
			g.warnRole(p.session.User)
		}
		// Never retried without the role.
		return fmt.Errorf("set role: %w", err)
	}

	// The count query keeps only the WHERE clause, so some parameters may go
	// unused there; Postgres needs their types, inferred from the statement.
	// This prepare runs as the client's role, read-only, under the timeout.
	var values [][]byte
	var oids []uint32
	if len(p.params) > 0 {
		sd, err := pg.Prepare(ctx, "", p.sql, p.oids)
		if err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
		oids = sd.ParamOIDs
		values = make([][]byte, len(p.params))
		for i, v := range p.params {
			if v != nil {
				values[i] = []byte(*v)
			}
		}
	}

	// The table as the statement names it, which the count and EXPLAIN also
	// read. The config key may resolve to another table on this search_path.
	rel := [][]byte{[]byte(m.Relation)}
	rows, err := rowsOf(ctx, pg, "select reltuples::bigint from pg_class where oid = $1::regclass", rel, nil)
	if err != nil {
		return fmt.Errorf("table rows: %w", err)
	}
	if len(rows) == 1 {
		if n, err := strconv.ParseInt(string(rows[0][0]), 10, 64); err == nil && n > 0 {
			im.TableRows = &n
		}
	}

	if p.res.Op == "delete" {
		rows, err := rowsOf(ctx, pg, `
			select c.conrelid::regclass::text, c.confdeltype
			from pg_constraint c
			where c.contype = 'f' and c.confrelid = $1::regclass and c.confdeltype in ('c', 'n', 'd')
			order by 1`, rel, nil)
		if err != nil {
			return fmt.Errorf("cascades: %w", err)
		}
		for _, r := range rows {
			im.Cascades = append(im.Cascades, decide.Cascade{Table: string(r[0]), OnDelete: onDelete[string(r[1])]})
		}
	}

	est, err := estimate(ctx, pg, p.sql, values, oids)
	if err != nil {
		return fmt.Errorf("estimate: %w", err)
	}
	im.Estimate = &est

	// Last: a count past the timeout aborts the transaction.
	if m.Method != sqlparse.MethodCount || !allColumns(ctx, pg, rel, m.Qualified) {
		im.Method, im.Rows = string(sqlparse.MethodEstimate), &est
		return nil
	}
	if ct := countTimeout; ct != queryTimeout {
		if err := pg.Exec(ctx, fmt.Sprintf("set local statement_timeout = '%dms'", max(ct.Milliseconds(), 1))).Close(); err != nil {
			return fmt.Errorf("count timeout: %w", err)
		}
	}
	limit := lim.MaxRows + 1
	im.Cap = limit
	rows, err = rowsOf(ctx, pg, fmt.Sprintf("select count(*) from (%s limit %d) s", m.CountSQL, limit), values, oids)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "57014": // statement_timeout
		im.Method, im.Rows = string(sqlparse.MethodEstimate), &est
		return nil
	case err != nil:
		return fmt.Errorf("count: %w", err)
	case len(rows) != 1:
		return fmt.Errorf("count: %d rows", len(rows))
	}
	n, err := strconv.ParseInt(string(rows[0][0]), 10, 64)
	if err != nil {
		return fmt.Errorf("count: %w", err)
	}
	if n >= int64(limit) {
		im.Capped, n = true, int64(limit)
	}
	im.Method, im.Rows = string(sqlparse.MethodCount), &n
	return nil
}

// allColumns reports whether every name in names is a column of rel, system
// columns included, in the measurement's transaction. Postgres reads t.f as
// the call f(t) when t has no column f, so a name the classifier saw as a
// column may run a function on Stet's connection. Any doubt is a no: a
// failed lookup counts as a name that is not a column, and the caller falls
// back to the estimate (spec 12).
func allColumns(ctx context.Context, pg *pgconn.PgConn, rel [][]byte, names []string) bool {
	if len(names) == 0 {
		return true
	}
	rows, err := rowsOf(ctx, pg, "select attname from pg_attribute where attrelid = $1::regclass and not attisdropped", rel, nil)
	if err != nil {
		return false
	}
	cols := make(map[string]bool, len(rows))
	for _, r := range rows {
		cols[string(r[0])] = true
	}
	for _, n := range names {
		if !cols[n] {
			return false
		}
	}
	return true
}

// txConn acquires a pooled connection for one transaction. done rolls back
// whatever is open on it, with its own context because the caller's may have
// expired, closes the connection if the rollback did not return it to idle,
// and releases it, so no transaction, role, or setting reaches the pool.
// Call done on every path once txConn succeeds.
func (g *Gate) txConn(ctx context.Context) (pg *pgconn.PgConn, done func(), err error) {
	conn, err := g.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	pg = conn.Conn().PgConn()
	return pg, func() {
		rctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
		defer cancel()
		if err := pg.Exec(rctx, "rollback").Close(); err != nil || pg.TxStatus() != 'I' {
			pg.Close(rctx)
		}
		conn.Release()
	}, nil
}

// beginReadOnly opens a read-only transaction whose statements time out
// after queryTimeout, as role unless role is "".
func beginReadOnly(role string) string {
	var b strings.Builder
	b.WriteString("begin read only; ")
	if role != "" {
		fmt.Fprintf(&b, "set local role %s; ", pgx.Identifier{role}.Sanitize())
	}
	fmt.Fprintf(&b, "set local statement_timeout = '%dms'", queryTimeout.Milliseconds())
	return b.String()
}

// warnRole logs, once per role, that Stet's account cannot assume a client's
// role, so that role's multi-row writes are recorded as not measured.
func (g *Gate) warnRole(role string) {
	if _, seen := g.warnedRoles.LoadOrStore(role, true); seen {
		return
	}
	g.log.Warn("impact not measured: Stet's account is not a member of the client's role",
		"role", role,
		"fix", "GRANT "+pgx.Identifier{role}.Sanitize()+" TO "+pgx.Identifier{g.pool.Config().ConnConfig.User}.Sanitize())
}

// estimate returns the planner's row estimate for the statement, from
// EXPLAIN without ANALYZE, which plans the statement but does not run it.
func estimate(ctx context.Context, pg *pgconn.PgConn, sql string, values [][]byte, oids []uint32) (int64, error) {
	// Sent as one extended-protocol statement, which Postgres refuses to
	// split, so nothing after the agent's statement can run.
	rows, err := rowsOf(ctx, pg, "EXPLAIN (FORMAT JSON) "+sql, values, oids)
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 {
		return 0, fmt.Errorf("%d plan rows", len(rows))
	}
	type node struct {
		Type   string  `json:"Node Type"`
		Parent string  `json:"Parent Relationship"`
		Rows   float64 `json:"Plan Rows"`
		Plans  []node  `json:"Plans"`
	}
	var plans []struct{ Plan node }
	if err := json.Unmarshal(rows[0][0], &plans); err != nil {
		return 0, err
	}
	if len(plans) != 1 || plans[0].Plan.Type != "ModifyTable" {
		return 0, errors.New("plan is not a single ModifyTable")
	}
	// Writes inside WITH hang off the top node as InitPlans; they are not
	// the rows this statement touches.
	var sum float64
	for _, c := range plans[0].Plan.Plans {
		if c.Parent != "InitPlan" && c.Parent != "SubPlan" {
			sum += c.Rows
		}
	}
	return int64(math.Round(sum)), nil
}

// rowsOf runs sql with text parameters and returns its rows as text.
func rowsOf(ctx context.Context, pg *pgconn.PgConn, sql string, values [][]byte, oids []uint32) ([][][]byte, error) {
	res := pg.ExecParams(ctx, sql, values, oids, nil, nil).Read()
	return res.Rows, res.Err
}

// Package apply runs the held writes a reviewer approved (spec-v0.2-approve
// 7): each on a connection of Stet's own, in one transaction with the change
// of its row in stet.held_writes, as the agent's role, under the agent
// session's date and time settings, and only as far as it was approved.
//
// SET LOCAL ROLE is not a security boundary (spec 11): the statement is the
// agent's and runs on Stet's connection, so a function in it can take another
// role Stet's account may take. What is caught is a statement that ends as
// another role; it is undone.
package apply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/store"
)

// applyGrace is how far past held.apply_timeout Apply may run, for the steps
// around the statement. A var so tests can shorten it.
var applyGrace = 5 * time.Second

// closeTimeout bounds closing the connection, which may come after Apply's
// own bound has passed.
const closeTimeout = time.Second

// The reasons a held write is recorded as failed without being kept, besides
// the server's errors (spec 7; no_row_limit is correction B5, settings_missing
// ruling 16).
const (
	approvalLapsed    = "approval_lapsed"
	otherDatabase     = "other_database"
	notGated          = "not_gated"
	noRowLimit        = "no_row_limit"
	settingsMissing   = "settings_missing"
	rowChanged        = "row_changed"
	searchPathDiffers = "search_path_differs"
	roleChanged       = "role_changed"
)

// sessionSettings are the settings a held write carries from the agent's
// session, named as the server reports them. No other key of a row's
// settings is applied.
var sessionSettings = []string{"TimeZone", "DateStyle", "IntervalStyle"}

// savepoint is set before the checks' reads (step 3), and so before the
// statement and its role and settings (step 4), so that a failure after it
// can be undone and the failure still recorded.
const savepoint = "stet_apply"

// Applier runs approved held writes (spec-v0.2-approve 7).
type Applier struct {
	cfg   *config.Config
	pool  *pgxpool.Pool // only its connection settings are used: each write gets a connection of its own
	store *store.Store
	log   *slog.Logger

	// The loop (loop.go).
	ctx       context.Context // done once Close is called
	stop      context.CancelFunc
	startOnce sync.Once
	stopped   chan struct{} // closed once the loop has returned, or at Close when it never started

	// Applying, when set, is called once a held write's checks have passed,
	// just before its statement runs, with the agent's role and the table;
	// the func it returns is called once the transaction has committed or
	// been given up. Set it before Start.
	Applying func(role, table string) (done func())

	// Test seams.
	markApplied     func(ctx context.Context, q store.Querier, id string, rows int64) error
	beforeStatement func()                                    // called once the checks pass, before the row's role and settings
	afterCommit     func(ctx context.Context, conn *pgx.Conn) // called once an applied write committed, with its connection
	applyOne        func(w store.Approved) error              // what the loop calls for each write: Apply
}

func New(cfg *config.Config, pool *pgxpool.Pool, st *store.Store, log *slog.Logger) *Applier {
	ctx, stop := context.WithCancel(context.Background())
	a := &Applier{
		cfg: cfg, pool: pool, store: st, log: log,
		ctx: ctx, stop: stop, stopped: make(chan struct{}),
		markApplied: store.MarkApplied,
	}
	a.applyOne = a.Apply
	return a
}

// Apply processes one approved held write on a connection of its own, in one
// transaction, and closes the connection. It returns nil when the write was
// applied, was recorded as failed, or was not there to take; an error when
// one of Stet's own steps failed: the transaction is discarded and the row
// stays approved. It runs under a bound of its own, held.apply_timeout +
// applyGrace, whatever the caller's context.
func (a *Applier) Apply(w store.Approved) error {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Held.ApplyTimeout+applyGrace)
	defer cancel()
	// Stet's account with the pool's start settings, but not a pooled
	// connection: the agent's statement may change its session.
	conn, err := pgx.ConnectConfig(ctx, a.pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("apply held write %s: connect: %w", w.ID, err)
	}
	defer func() {
		// Closing discards whatever transaction is still open.
		cctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		conn.Close(cctx)
	}()
	if err := a.apply(ctx, conn, w); err != nil {
		return fmt.Errorf("apply held write %s: %w", w.ID, err)
	}
	return nil
}

func (a *Applier) apply(ctx context.Context, conn *pgx.Conn, w store.Approved) error {
	// Step 1. The isolation level is set before the row is read, so it comes
	// from the listing (correction B7), which reads the same fixed columns.
	iso := pgx.ReadCommitted
	if w.Snapshot {
		iso = pgx.RepeatableRead
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	// Step 2.
	c, err := store.ClaimApproved(ctx, tx, w.ID)
	if code(err) == "40001" {
		// Correction B7: another Stet finished it after this snapshot was taken.
		a.log.Debug("held write taken by another applier", "id", w.ID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	if c == nil {
		return nil // another applier holds it, or it is no longer approved
	}
	if (c.RowVersion != nil || c.RowAbsent) && !w.Snapshot {
		return errors.New("listed as needing no snapshot, but it has a row version or row_absent")
	}

	// Step 3.
	if _, err := tx.Exec(ctx, "savepoint "+savepoint); err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	reason, err := a.check(ctx, tx, c)
	if err != nil {
		return err
	}
	if reason != "" {
		return a.fail(ctx, tx, c, reason, true) // the reason may be a server error that aborted the transaction
	}

	if a.Applying != nil {
		// Runs on every way out from here: committed, recorded as failed,
		// an error of Stet's own step, a panic. A transaction given up on
		// an error ends with the connection, and changed nothing.
		defer a.Applying(c.LoginRole, c.TargetTable)()
	}
	if a.beforeStatement != nil {
		a.beforeStatement()
	}
	rows, reason, err := a.run(ctx, tx, c)
	if err != nil {
		return err
	}
	if reason != "" {
		return a.fail(ctx, tx, c, reason, true)
	}

	// Step 7.
	if _, err := tx.Exec(ctx, "reset role"); err != nil {
		return fmt.Errorf("reset role: %w", err)
	}
	if err := a.markApplied(ctx, tx, c.ID, rows); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if a.afterCommit != nil {
		a.afterCommit(ctx, conn)
	}
	a.log.Info("held write applied", "id", c.ID, "login_role", c.LoginRole, "table", c.TargetTable, "rows", rows)
	return nil
}

// check makes the checks before the statement (step 3) in the spec's order,
// as Stet's account, and returns the reason not to run it, or "". A server
// error in one of its reads may be such a reason (readFailure).
func (a *Applier) check(ctx context.Context, tx pgx.Tx, c *store.Claimed) (string, error) {
	if c.Lapsed {
		return approvalLapsed, nil
	}
	var database, path string
	if err := tx.QueryRow(ctx, "select current_database(), current_setting('search_path')").Scan(&database, &path); err != nil {
		return "", fmt.Errorf("read the connection's database and search_path: %w", err)
	}
	if c.Database != database {
		return otherDatabase, nil
	}
	t, ok := a.cfg.Tables[c.TargetTable]
	if !ok {
		return notGated, nil
	}
	if c.Limit == nil {
		return noRowLimit, nil
	}
	// The gate stores no row without all three; one put there by SQL is not
	// run under Stet's settings in their place.
	for _, name := range sessionSettings {
		if c.Settings[name] == "" {
			return settingsMissing, nil
		}
	}
	if c.RowVersion != nil || c.RowAbsent {
		if c.TargetPK == nil {
			return rowChanged, nil // the version cannot be checked without the row's key
		}
		changed, err := rowChangedSince(ctx, tx.Conn().PgConn(), c, t.PK)
		if err != nil {
			return readFailure("read the row version", err)
		}
		if changed {
			return rowChanged, nil
		}
	}
	rolePath, _, err := RoleSearchPath(ctx, tx, c.LoginRole)
	if err != nil {
		return readFailure("read the role-level search_path of "+c.LoginRole, err)
	}
	if rolePath != nil && !slices.Equal(SchemaNames(*rolePath), SchemaNames(path)) {
		return searchPathDiffers, nil
	}
	return "", nil
}

// readFailure sorts err from a read of the checks (step 3). A server error is
// about the row, as a failed statement's is (its table was dropped, its key
// does not fit the column), and is the reason to record, with its SQLSTATE
// and message; unless it is Stet's own: a lock not had within Stet's
// lock_timeout (55P03), or an error after which the connection is gone. That
// one, and any other error, is returned, labelled with what was read.
func readFailure(what string, err error) (string, error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code != "55P03" && !connectionGone(pgErr) {
		return fmt.Sprintf("SQLSTATE %s: %s", pgErr.Code, pgErr.Message), nil
	}
	return "", fmt.Errorf("%s: %w", what, err)
}

// connectionGone reports whether the server ends the connection with e: a
// connection exception (class 08), or an error of severity FATAL or PANIC,
// such as 57P01 when the server shuts down.
func connectionGone(e *pgconn.PgError) bool {
	return strings.HasPrefix(e.Code, "08") || e.SeverityUnlocalized == "FATAL" || e.SeverityUnlocalized == "PANIC"
}

// rowChangedSince reads the target row's version as the gate does and
// reports whether it differs from what was held. A row that is gone has not
// changed: the statement runs and touches no row. A row that was absent and
// is there now has. Under REPEATABLE READ, a change committed after this read
// fails the statement with 40001 (spec 7). c.TargetPK is not nil.
func rowChangedSince(ctx context.Context, pg *pgconn.PgConn, c *store.Claimed, pk string) (bool, error) {
	q := fmt.Sprintf("select t.xmin::text from %s as t where t.%s = $1", ident(c.TargetTable), pgx.Identifier{pk}.Sanitize())
	res := pg.ExecParams(ctx, q, [][]byte{[]byte(*c.TargetPK)}, nil, nil, nil).Read()
	switch {
	case res.Err != nil:
		return false, res.Err
	case len(res.Rows) == 0:
		return false, nil
	case c.RowAbsent:
		return true, nil
	default:
		return string(res.Rows[0][0]) != *c.RowVersion, nil
	}
}

// run takes the row's role and settings (step 4), sends its statement (step
// 5) and checks what it did (step 6). It returns the rows the statement
// changed, or the reason not to keep what it did. A server error at any of
// these steps is such a reason (correction B6); any other failure is an
// error, labelled with its step.
func (a *Applier) run(ctx context.Context, tx pgx.Tx, c *store.Claimed) (int64, string, error) {
	if err := a.become(ctx, tx, c); err != nil {
		reason, err := failure(err)
		if err != nil {
			return 0, "", fmt.Errorf("role and settings: %w", err)
		}
		return 0, reason, nil
	}

	// The values as stored, in text, with the client's types; the result
	// rows are discarded. One extended-protocol statement, which Postgres
	// refuses to split.
	tag, err := tx.Conn().PgConn().ExecParams(ctx, c.Statement, values(c.Params), c.ParamTypes, nil, nil).Close()
	if err == nil {
		// Deferred constraints are checked now, with the statement and as
		// its role, rather than at the commit, where a violation would fail
		// Stet's own step and leave the row approved.
		_, err = tx.Exec(ctx, "set constraints all immediate")
	}
	if err != nil {
		reason, err := failure(err)
		if err != nil {
			return 0, "", fmt.Errorf("statement: %w", err)
		}
		return 0, reason, nil
	}

	// The statement may have changed search_path for the transaction (with
	// set_config): Stet's own steps from here on resolve their names in
	// pg_catalog, not through it. A failure undoes this with the statement.
	if _, err := tx.Exec(ctx, "set local search_path = pg_catalog, pg_temp"); err != nil {
		return 0, "", fmt.Errorf("reset search_path: %w", err)
	}
	var user string
	if err := tx.QueryRow(ctx, "select current_user").Scan(&user); err != nil {
		return 0, "", fmt.Errorf("read current_user: %w", err)
	}
	if user != c.LoginRole {
		return 0, roleChanged, nil
	}
	if n := tag.RowsAffected(); n > *c.Limit {
		return 0, fmt.Sprintf("rows_exceeded: %d rows, approved up to %d", n, *c.Limit), nil
	}
	return tag.RowsAffected(), "", nil
}

// become takes the row's role and its session's settings, which check found
// all there, and the bounds of held.apply_timeout, for the rest of the
// transaction (step 4). It also sets stet.held_write to the row's id for the
// transaction, which the capture trigger records with what the statement
// changes (spec-v0.2-undo 4, step 7).
func (a *Applier) become(ctx context.Context, tx pgx.Tx, c *store.Claimed) error {
	if _, err := tx.Exec(ctx, "set local role "+pgx.Identifier{c.LoginRole}.Sanitize()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "select set_config('stet.held_write', $1, true)", c.ID); err != nil {
		return err
	}
	for _, name := range sessionSettings {
		if _, err := tx.Exec(ctx, "select set_config($1, $2, true)", name, c.Settings[name]); err != nil {
			return err
		}
	}
	// Also over the start's lock_timeout, which is for Stet's own queries.
	ms := strconv.FormatInt(a.cfg.Held.ApplyTimeout.Milliseconds(), 10)
	_, err := tx.Exec(ctx, "select set_config('statement_timeout', $1, true), set_config('lock_timeout', $1, true)", ms)
	return err
}

// fail records reason as why held write c failed and commits, after undoing
// what followed the savepoint when undo is set. Undoing also leaves the
// row's role and settings, so the record is written as Stet's account.
func (a *Applier) fail(ctx context.Context, tx pgx.Tx, c *store.Claimed, reason string, undo bool) error {
	if undo {
		if _, err := tx.Exec(ctx, "rollback to savepoint "+savepoint); err != nil {
			return fmt.Errorf("roll back to the savepoint: %w", err)
		}
	}
	if err := store.MarkFailed(ctx, tx, c.ID, reason); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	a.log.Warn("held write failed", "id", c.ID, "login_role", c.LoginRole, "table", c.TargetTable, "error", reason)
	return nil
}

// failure sorts err from a step after the savepoint. A server error is a
// reason to record: 40001 as row_changed (the snapshot's row was changed
// since), any other as its SQLSTATE and message. Any other error, a lost
// connection or Apply's bound passing, is Stet's own and returned.
func failure(err error) (string, error) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return "", err
	}
	if pgErr.Code == "40001" {
		return rowChanged, nil
	}
	return fmt.Sprintf("SQLSTATE %s: %s", pgErr.Code, pgErr.Message), nil
}

// code is err's SQLSTATE, or "" when it is not a server error.
func code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// values are stored bind values for the protocol: text, with NULL as nil.
func values(params []*string) [][]byte {
	if params == nil {
		return nil
	}
	out := make([][]byte, len(params))
	for i, p := range params {
		if p != nil {
			out[i] = []byte(*p) // never nil, so '' stays ''
		}
	}
	return out
}

// ident quotes a table key as the gate names it in SQL: each dotted part
// as an identifier.
func ident(table string) string {
	return pgx.Identifier(strings.Split(table, ".")).Sanitize()
}

package store_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/store"
)

// The agent of these tests is stet_md5, the test server's md5 account. It has
// no rights on the stet schema. The table owner is the superuser.

// execer is what mustExec runs statements on: a connection, a pool or a
// transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// mustExec runs sqls in order and stops the test at the first that fails.
func mustExec(t *testing.T, conn execer, sqls ...string) {
	t.Helper()
	for _, sql := range sqls {
		if _, err := conn.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// quoteLiteral is s as an SQL string literal.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// agentIn connects as the agent, stet_md5, to database. It is closed when the
// test ends.
func agentIn(t *testing.T, database string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), "postgres://stet_md5:md5-pw@"+testAddr(t)+"/"+database+"?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// captureDB creates a database of the test's own (ownDatabase) with the stet
// schema, and returns Stet's pool to it and its name. The superuser stands for
// the table owner there: the agent (stet_md5) may use every schema and write
// every table the superuser creates in it. The agent has no right on the stet
// schema and cannot execute stet.capture(); captureDB checks that.
// stet.capture_settings has no row.
func captureDB(t *testing.T) (stet *pgxpool.Pool, db string) {
	t.Helper()
	dsn := ownDatabase(t)
	db = databaseOf(t, dsn)
	stet = newPool(t, dsn)
	if err := store.New(stet).Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	su := superuserIn(t, db)
	mustExec(t, su,
		"alter default privileges for role postgres grant usage on schemas to stet_md5",
		"alter default privileges for role postgres grant select, insert, update, delete, truncate on tables to stet_md5",
		"alter default privileges for role postgres grant usage on sequences to stet_md5")
	var usage, create, execute bool
	if err := su.QueryRow(context.Background(), `
		select has_schema_privilege('stet_md5', 'stet', 'usage'), has_schema_privilege('stet_md5', 'stet', 'create'),
		       has_function_privilege('stet_md5', 'stet.capture()', 'execute')`).Scan(&usage, &create, &execute); err != nil {
		t.Fatal(err)
	}
	if usage || create || execute {
		t.Fatalf("the agent has rights it must not: usage on stet %v, create on stet %v, execute on stet.capture() %v",
			usage, create, execute)
	}
	return stet, db
}

// installCapture creates the four triggers of spec 4 on table, its name as
// SQL (quoted where it must be), as the table owner does. key is the
// configuration's table key and pk the key column's name, the two arguments
// of stet.capture().
func installCapture(t *testing.T, owner *pgx.Conn, table, key, pk string) {
	t.Helper()
	call := "execute function stet.capture(" + quoteLiteral(key) + ", " + quoteLiteral(pk) + ")"
	mustExec(t, owner,
		"create trigger stet_capture_ins after insert on "+table+
			" referencing new table as n for each statement "+call,
		"create trigger stet_capture_upd after update on "+table+
			" referencing old table as o new table as n for each statement "+call,
		"create trigger stet_capture_del after delete on "+table+
			" referencing old table as o for each statement "+call,
		"create trigger stet_capture_trunc after truncate on "+table+
			" for each statement "+call)
}

// setCapture makes stet.capture_settings say roles (nil: every session) and
// maxRows.
func setCapture(t *testing.T, pool *pgxpool.Pool, roles []string, maxRows int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into stet.capture_settings (roles, max_rows) values ($1, $2)
		on conflict (one) do update set roles = excluded.roles, max_rows = excluded.max_rows`, roles, maxRows); err != nil {
		t.Fatal(err)
	}
}

// capturedChange is one line of stet.changes with its change set, or a change
// set without lines (ChangeID and Key nil).
type capturedChange struct {
	SetID       int64
	At          time.Time
	StmtAt      time.Time
	TxID        *int64
	PID         int32
	LoginRole   string
	AgentRole   string
	App         *string
	HeldWrite   *string
	TargetTable string
	KeyColumn   string
	RelID       uint32
	Op          string
	Query       *string
	Rows        *int64
	Skipped     *string
	Detail      *string

	ChangeID *int64
	Key      *string
	// Before and After are the row's images: column name to the value's
	// text, a string, or nil for NULL. A nil image means there was no row.
	Before map[string]any
	After  map[string]any
}

// changesOf returns what capture recorded for the configuration's table key,
// in the order it was recorded.
func changesOf(t *testing.T, pool *pgxpool.Pool, key string) []capturedChange {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select s.id, s.at, s.stmt_at, s.txid, s.pid, s.login_role, s.agent_role, s.app, s.held_write::text,
		       s.target_table, s.key_column, s.relid, s.op, s.query, s.rows, s.skipped, s.detail,
		       c.id, c.key, c.before, c.after
		from stet.change_sets s left join stet.changes c on c.set_id = s.id
		where s.target_table = $1
		order by s.id, c.id`, key)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (capturedChange, error) {
		var c capturedChange
		err := r.Scan(&c.SetID, &c.At, &c.StmtAt, &c.TxID, &c.PID, &c.LoginRole, &c.AgentRole, &c.App, &c.HeldWrite,
			&c.TargetTable, &c.KeyColumn, &c.RelID, &c.Op, &c.Query, &c.Rows, &c.Skipped, &c.Detail,
			&c.ChangeID, &c.Key, &c.Before, &c.After)
		return c, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

// relidOf is the OID of table, its name as SQL.
func relidOf(t *testing.T, conn *pgx.Conn, table string) uint32 {
	t.Helper()
	var relid uint32
	if err := conn.QueryRow(context.Background(), `select $1::regclass::oid`, table).Scan(&relid); err != nil {
		t.Fatal(err)
	}
	return relid
}

// changeLine is what a test expects of one line of stet.changes: the op of its
// set, its key and its images.
type changeLine struct {
	Op, Key       string
	Before, After map[string]any
}

func (l changeLine) String() string {
	show := func(img map[string]any) string {
		if img == nil {
			return "none"
		}
		return fmt.Sprint(img)
	}
	return fmt.Sprintf("%s %q before %s after %s", l.Op, l.Key, show(l.Before), show(l.After))
}

// linesOf are the lines changes holds for the sets recorded for query,
// sorted by op and key.
func linesOf(changes []capturedChange, query string) []changeLine {
	var lines []changeLine
	for _, c := range changes {
		if c.Query == nil || *c.Query != query {
			continue
		}
		l := changeLine{Op: c.Op, Before: c.Before, After: c.After}
		if c.Key != nil {
			l.Key = *c.Key
		}
		lines = append(lines, l)
	}
	sortLines(lines)
	return lines
}

func sortLines(lines []changeLine) {
	slices.SortFunc(lines, func(a, b changeLine) int {
		return cmp.Or(cmp.Compare(a.Op, b.Op), cmp.Compare(a.Key, b.Key))
	})
}

// checkLines checks that the lines recorded for query are want.
func checkLines(t *testing.T, changes []capturedChange, query string, want ...changeLine) {
	t.Helper()
	sortLines(want)
	if got := linesOf(changes, query); !reflect.DeepEqual(got, want) {
		t.Errorf("for %q recorded\n%v\nwant\n%v", query, got, want)
	}
}

// setsOf are the change sets recorded for query, one capturedChange each.
func setsOf(changes []capturedChange, query string) []capturedChange {
	var sets []capturedChange
	for _, c := range changes {
		if c.Query != nil && *c.Query == query && (len(sets) == 0 || sets[len(sets)-1].SetID != c.SetID) {
			sets = append(sets, c)
		}
	}
	return sets
}

// image is a row image from column-value pairs; a nil value is NULL.
func image(pairs ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(pairs); i += 2 {
		m[pairs[i].(string)] = pairs[i+1]
	}
	return m
}

// deal is an image of a row of the deals table these tests use.
func deal(id, name, amount string) map[string]any {
	return image("id", id, "name", name, "amount", amount)
}

// dealsTable creates public.deals with rows 1 Acme 10, 2 Globex 20 and 3
// Initech 30, and installs capture on it with the key "deals".
func dealsTable(t *testing.T, owner *pgx.Conn) {
	t.Helper()
	mustExec(t, owner,
		`create table public.deals (id int primary key, name text, amount numeric)`,
		`insert into public.deals values (1, 'Acme', 10), (2, 'Globex', 20), (3, 'Initech', 30)`)
	installCapture(t, owner, "public.deals", "deals", "id")
}

// An agent's INSERT, UPDATE and DELETE each leave one change set, saying who
// sent what and when, and one line per key with the row before and after.
func TestCaptureRecordsEachKindOfWrite(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	dealsTable(t, owner)
	agent := agentIn(t, db)
	mustExec(t, agent, `set application_name = 'enricher'`)

	writes := []struct {
		sql, op string
		rows    int64
		lines   []changeLine
	}{
		{`insert into public.deals (id, name, amount) values (4, 'Umbrella', 40), (5, 'Hooli', 50)`, "insert", 2, []changeLine{
			{Op: "insert", Key: "4", After: deal("4", "Umbrella", "40")},
			{Op: "insert", Key: "5", After: deal("5", "Hooli", "50")},
		}},
		{`update public.deals set amount = amount + 1 where id in (1, 2)`, "update", 2, []changeLine{
			{Op: "update", Key: "1", Before: deal("1", "Acme", "10"), After: deal("1", "Acme", "11")},
			{Op: "update", Key: "2", Before: deal("2", "Globex", "20"), After: deal("2", "Globex", "21")},
		}},
		{`delete from public.deals where id = 3`, "delete", 1, []changeLine{
			{Op: "delete", Key: "3", Before: deal("3", "Initech", "30")},
		}},
	}
	txids := make([]int64, len(writes))
	for i, w := range writes {
		err := pgx.BeginFunc(ctx, agent, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, w.sql); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `select txid_current_if_assigned()`).Scan(&txids[i])
		})
		if err != nil {
			t.Fatalf("%s: %v", w.sql, err)
		}
	}

	changes := changesOf(t, pool, "deals")
	if len(changes) != 5 {
		t.Errorf("%d lines recorded, want 5", len(changes))
	}
	relid := relidOf(t, owner, "public.deals")
	for i, w := range writes {
		checkLines(t, changes, w.sql, w.lines...)
		sets := setsOf(changes, w.sql)
		if len(sets) != 1 {
			t.Errorf("%s: %d change sets, want 1", w.sql, len(sets))
			continue
		}
		s := sets[0]
		if s.Op != w.op || deref(s.Rows) != w.rows || s.TargetTable != "deals" || s.KeyColumn != "id" || s.RelID != relid {
			t.Errorf("%s: op %q, rows %v, target_table %q, key_column %q, relid %d; want %q, %d, deals, id, %d",
				w.sql, s.Op, deref(s.Rows), s.TargetTable, s.KeyColumn, s.RelID, w.op, w.rows, relid)
		}
		if s.PID != int32(agent.PgConn().PID()) || s.LoginRole != "stet_md5" || s.AgentRole != "stet_md5" ||
			deref(s.App) != "enricher" || deref(s.TxID) != txids[i] {
			t.Errorf("%s: pid %d, login_role %q, agent_role %q, app %v, txid %v; want %d, stet_md5, stet_md5, enricher, %d",
				w.sql, s.PID, s.LoginRole, s.AgentRole, deref(s.App), deref(s.TxID), agent.PgConn().PID(), txids[i])
		}
		if s.HeldWrite != nil || s.Skipped != nil || s.Detail != nil {
			t.Errorf("%s: held_write %v, skipped %v, detail %v; want all NULL", w.sql, deref(s.HeldWrite), deref(s.Skipped), deref(s.Detail))
		}
		if s.StmtAt.After(s.At) {
			t.Errorf("%s: stmt_at %v is after at %v", w.sql, s.StmtAt, s.At)
		}
	}
}

// Writes Stet never sees as writes to the table are recorded too: through a
// view, inside a SECURITY DEFINER function, the two halves of an upsert, and
// the child rows a cascade deletes.
func TestCaptureFollowsWritesStetDoesNotSee(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	dealsTable(t, owner)
	mustExec(t, owner,
		`create table public.notes (id int primary key, deal_id int references public.deals on delete cascade, body text)`,
		`insert into public.notes values (10, 1, 'call'), (11, 1, 'mail'), (12, 2, 'other')`,
		`create view public.deals_v as select * from public.deals`,
		`create function public.bump(k int) returns void language sql security definer
		 as $$ update public.deals set amount = amount + 1 where id = k $$`)
	installCapture(t, owner, "public.notes", "notes", "id")
	agent := agentIn(t, db)

	const (
		view     = `update public.deals_v set name = 'Acme 2' where id = 1`
		function = `select public.bump(2)`
		upsert   = `insert into public.deals (id, name, amount) values (2, 'Globex 2', 0), (9, 'New', 0)
		            on conflict (id) do update set name = excluded.name`
		cascade = `delete from public.deals where id = 1`
	)
	mustExec(t, agent, view, function, upsert, cascade)

	deals, notes := changesOf(t, pool, "deals"), changesOf(t, pool, "notes")
	checkLines(t, deals, view, changeLine{Op: "update", Key: "1", Before: deal("1", "Acme", "10"), After: deal("1", "Acme 2", "10")})
	checkLines(t, deals, function, changeLine{Op: "update", Key: "2", Before: deal("2", "Globex", "20"), After: deal("2", "Globex", "21")})
	checkLines(t, deals, upsert,
		changeLine{Op: "insert", Key: "9", After: deal("9", "New", "0")},
		changeLine{Op: "update", Key: "2", Before: deal("2", "Globex", "21"), After: deal("2", "Globex 2", "21")})
	checkLines(t, deals, cascade, changeLine{Op: "delete", Key: "1", Before: deal("1", "Acme 2", "10")})
	checkLines(t, notes, cascade,
		changeLine{Op: "delete", Key: "10", Before: image("id", "10", "deal_id", "1", "body", "call")},
		changeLine{Op: "delete", Key: "11", Before: image("id", "11", "deal_id", "1", "body", "mail")})

	// The function ran as its owner; the write is still the agent's.
	for _, s := range setsOf(deals, function) {
		if s.AgentRole != "stet_md5" || s.LoginRole != "stet_md5" {
			t.Errorf("inside a SECURITY DEFINER function: agent_role %q, login_role %q, want stet_md5", s.AgentRole, s.LoginRole)
		}
	}
	// The upsert's two sets, and the cascade's parent and child sets, carry
	// the statement the client sent and when the server received it.
	if sets := setsOf(deals, upsert); len(sets) != 2 || !sets[0].StmtAt.Equal(sets[1].StmtAt) {
		t.Errorf("upsert: %d sets, want 2 with one stmt_at", len(sets))
	}
	parent, child := setsOf(deals, cascade), setsOf(notes, cascade)
	if len(parent) != 1 || len(child) != 1 {
		t.Fatalf("cascade: %d parent and %d child sets, want one each", len(parent), len(child))
	}
	if !parent[0].StmtAt.Equal(child[0].StmtAt) || parent[0].SetID > child[0].SetID || child[0].AgentRole != "stet_md5" {
		t.Errorf("cascade: parent set %d at %v, child set %d at %v by %q; want the parent first, one stmt_at, stet_md5",
			parent[0].SetID, parent[0].StmtAt, child[0].SetID, child[0].StmtAt, child[0].AgentRole)
	}
	var left int
	if err := owner.QueryRow(ctx, `select count(*) from public.notes where deal_id = 1`).Scan(&left); err != nil || left != 0 {
		t.Errorf("the cascade left %d notes of deal 1 (%v)", left, err)
	}
}

// Rolled back writes, including those rolled back to a savepoint, and writes
// that changed no row leave nothing.
func TestCaptureSkipsRolledBackAndEmptyWrites(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	dealsTable(t, superuserIn(t, db))
	agent := agentIn(t, db)

	tx, err := agent.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, tx,
		`insert into public.deals values (4, 'Umbrella', 40)`,
		`update public.deals set amount = 0 where id = 1`,
		`delete from public.deals where id = 2`)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	const kept = `update public.deals set amount = 2 where id = 2`
	err = pgx.BeginFunc(ctx, agent, func(tx pgx.Tx) error {
		mustExec(t, tx, `savepoint s`, `update public.deals set amount = 1 where id = 1`, `rollback to savepoint s`, kept)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, agent,
		`update public.deals set name = 'x' where id = -1`,
		`delete from public.deals where id = -1`,
		`insert into public.deals select * from public.deals where false`,
		`insert into public.deals values (1, 'dup', 0) on conflict do nothing`)

	changes := changesOf(t, pool, "deals")
	if len(changes) != 1 {
		var queries []string
		for _, c := range changes {
			queries = append(queries, deref(c.Query).(string))
		}
		t.Errorf("lines recorded for %q, want only one for %q", queries, kept)
	}
	checkLines(t, changes, kept, changeLine{Op: "update", Key: "2", Before: deal("2", "Globex", "20"), After: deal("2", "Globex", "2")})
	var sets, lines int
	if err := pool.QueryRow(ctx, `select (select count(*) from stet.change_sets), (select count(*) from stet.changes)`).Scan(&sets, &lines); err != nil {
		t.Fatal(err)
	}
	if sets != 1 || lines != 1 {
		t.Errorf("stet.change_sets has %d rows and stet.changes %d, want 1 and 1", sets, lines)
	}
}

// With roles set, only the writes of those roles are recorded; a NULL among
// them names no role. With roles NULL, or no row in stet.capture_settings,
// every session's are.
func TestCaptureOnlyForAgentRoles(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	dealsTable(t, owner)
	agent := agentIn(t, db)
	write := func(conn *pgx.Conn, amount int) string {
		t.Helper()
		sql := fmt.Sprintf(`update public.deals set amount = %d where id = 1`, amount)
		mustExec(t, conn, sql)
		return sql
	}

	// setRoles stores roles as an SQL array literal, which may hold NULL.
	setRoles := func(roles string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			insert into stet.capture_settings (roles, max_rows) values ($1::text[], 10000)
			on conflict (one) do update set roles = excluded.roles`, roles); err != nil {
			t.Fatal(err)
		}
	}

	setCapture(t, pool, []string{"stet_md5"}, 10000)
	byOwner := write(owner, 1)
	byAgent := write(agent, 2)
	setRoles("{NULL}")
	byOwnerWithANullRole := write(owner, 3)
	byAgentWithANullRole := write(agent, 4)
	setRoles("{stet_md5,NULL}")
	byOwnerWithTheAgentAndANullRole := write(owner, 5)
	byAgentWithTheAgentAndANullRole := write(agent, 6)
	setCapture(t, pool, nil, 10000)
	byOwnerWithRolesNull := write(owner, 7)
	if _, err := pool.Exec(ctx, `delete from stet.capture_settings`); err != nil {
		t.Fatal(err)
	}
	byOwnerWithoutSettings := write(owner, 8)

	changes := changesOf(t, pool, "deals")
	for sql, what := range map[string]string{
		byOwner:                         "roles {stet_md5}: the owner's write",
		byOwnerWithANullRole:            "roles {NULL}: the owner's write",
		byAgentWithANullRole:            "roles {NULL}: the agent's write",
		byOwnerWithTheAgentAndANullRole: "roles {stet_md5,NULL}: the owner's write",
	} {
		if sets := setsOf(changes, sql); len(sets) != 0 {
			t.Errorf("%s was recorded", what)
		}
	}
	for _, c := range []struct {
		sql, role, what string
	}{
		{byAgent, "stet_md5", "roles {stet_md5}: the agent's write"},
		{byAgentWithTheAgentAndANullRole, "stet_md5", "roles {stet_md5,NULL}: the agent's write"},
		{byOwnerWithRolesNull, "postgres", "roles NULL: the owner's write"},
		{byOwnerWithoutSettings, "postgres", "no settings: the owner's write"},
	} {
		sets := setsOf(changes, c.sql)
		if len(sets) != 1 || sets[0].AgentRole != c.role || sets[0].LoginRole != c.role {
			t.Errorf("%s: %d sets, want one by %s", c.what, len(sets), c.role)
		}
	}
}

// stetMayActAsTheAgent makes Stet's account a member of stet_md5, so that it
// can SET ROLE stet_md5 as it does to apply an approved write. It is so on the
// test server already, and other packages rely on it; only a membership this
// test granted is taken back when it ends.
func stetMayActAsTheAgent(t *testing.T, db string) {
	t.Helper()
	ctx := context.Background()
	su := superuserIn(t, db)
	var member bool
	if err := su.QueryRow(ctx, `select pg_has_role('stet_scram', 'stet_md5', 'member')`).Scan(&member); err != nil {
		t.Fatal(err)
	}
	if member {
		return
	}
	mustExec(t, su, "grant stet_md5 to stet_scram")
	t.Cleanup(func() {
		if _, err := su.Exec(context.Background(), "revoke stet_md5 from stet_scram"); err != nil {
			t.Errorf("revoke stet_md5 from stet_scram: %v", err)
		}
	})
}

// A write Stet's account runs as the agent (SET LOCAL ROLE), as it applies an
// approved write, is the agent's, with the held write it names. The agent's
// own session cannot name one.
func TestCaptureOfStetActingAsTheAgent(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	dealsTable(t, superuserIn(t, db))
	stetMayActAsTheAgent(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	stet, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stet.Release()

	held := store.NewID()
	const (
		approved = `update public.deals set name = 'approved' where id = 1`
		unnamed  = `update public.deals set name = 'approved, no held write' where id = 1`
		notAUUID = `update public.deals set name = 'approved, not a uuid' where id = 1`
		byAgent  = `update public.deals set name = 'by the agent' where id = 2`
	)
	// asTheAgent runs sql in a transaction of Stet's session as stet_md5,
	// with stet.held_write set to held for the transaction unless held is "".
	asTheAgent := func(sql, held string) {
		t.Helper()
		err := pgx.BeginFunc(ctx, stet, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `set local role stet_md5`); err != nil {
				return err
			}
			if held != "" {
				if _, err := tx.Exec(ctx, `select set_config('stet.held_write', $1, true)`, held); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, sql)
			return err
		})
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	asTheAgent(approved, held)
	// The same session again: stet.held_write was set for the transaction
	// only, and is now empty.
	asTheAgent(unnamed, "")
	// The approved statement is the agent's and can set stet.held_write
	// itself (spec 11). A value that is not a uuid is stored as NULL.
	asTheAgent(notAUUID, "not-a-uuid")
	agent := agentIn(t, db)
	err = pgx.BeginFunc(ctx, agent, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select set_config('stet.held_write', $1, true)`, store.NewID()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, byAgent)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	changes := changesOf(t, pool, "deals")
	for _, c := range []struct {
		sql, login string
		held       any
	}{
		{approved, "stet_scram", held},
		{unnamed, "stet_scram", "NULL"},
		{notAUUID, "stet_scram", "NULL"},
		{byAgent, "stet_md5", "NULL"},
	} {
		sets := setsOf(changes, c.sql)
		if len(sets) != 1 {
			t.Errorf("%s: %d sets, want 1", c.sql, len(sets))
			continue
		}
		if s := sets[0]; s.AgentRole != "stet_md5" || s.LoginRole != c.login || deref(s.HeldWrite) != c.held {
			t.Errorf("%s: agent_role %q, login_role %q, held_write %v; want stet_md5, %s, %v",
				c.sql, s.AgentRole, s.LoginRole, deref(s.HeldWrite), c.login, c.held)
		}
	}
}

// The images hold each value as its text, from which the value comes back
// as it was: a row put back from the images of its deletion equals the row.
func TestCaptureKeepsValuesAsText(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	mustExec(t, owner, `create table public.vals (
		id bigint generated always as identity primary key,
		i int, big bigint, n numeric,
		f_nan float8, f_inf float8, f_ninf float8, f_sum float8, r real,
		s text, ts timestamptz, ts_inf timestamptz, d date, iv interval, u uuid, b bytea,
		arr int[], arr2 text[], jb jsonb, jb_obj jsonb, jb_null jsonb, j json, j_null json, j_dup json,
		nul text,
		g bigint generated always as (i * 2) stored)`)
	installCapture(t, owner, "public.vals", "vals", "id")
	agent := agentIn(t, db)
	mustExec(t, agent, `insert into public.vals (i, big, n, f_nan, f_inf, f_ninf, f_sum, r, s, ts, ts_inf, d, iv, u, b,
		arr, arr2, jb, jb_obj, jb_null, j, j_null, j_dup, nul) values (
		42, 9223372036854775807, 12345678901234567890.0001230,
		'NaN', 'Infinity', '-Infinity', 0.1::float8 + 0.2::float8, 0.1,
		E'line1\nline2 "dq" ''sq'' back\\slash', '2026-11-18 01:02:03.456789+09', 'infinity', '2026-02-28',
		'1 year 2 mons 3 days 04:05:06.789', 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '\x00ff5c27',
		'{1,NULL,3}', '{{a,NULL},{"b c","\"q\""}}', 'null', '{"k": [1, "x", null]}', NULL, 'null', NULL, '{"a": 1,  "a":2}',
		NULL)`,
		`insert into public.vals (i) values (7)`)
	mustExec(t, owner, `create table public.vals_orig as select * from public.vals`)
	mustExec(t, agent, `delete from public.vals`)

	changes := changesOf(t, pool, "vals")
	full := image(
		"id", "1", "i", "42", "big", "9223372036854775807", "n", "12345678901234567890.0001230",
		"f_nan", "NaN", "f_inf", "Infinity", "f_ninf", "-Infinity", "f_sum", "0.30000000000000004", "r", "0.1",
		"s", "line1\nline2 \"dq\" 'sq' back\\slash", "ts", "2026-11-17 16:02:03.456789+00", "ts_inf", "infinity",
		"d", "2026-02-28", "iv", "1 year 2 mons 3 days 04:05:06.789", "u", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
		"b", `\x00ff5c27`, "arr", "{1,NULL,3}", "arr2", `{{a,NULL},{"b c","\"q\""}}`,
		"jb", "null", "jb_obj", `{"k": [1, "x", null]}`, "jb_null", nil, "j", "null", "j_null", nil, "j_dup", `{"a": 1,  "a":2}`,
		"nul", nil, "g", "84")
	sparse := image("id", "2", "i", "7", "g", "14")
	for col := range full {
		if _, ok := sparse[col]; !ok {
			sparse[col] = nil
		}
	}
	checkLines(t, changes, `delete from public.vals`,
		changeLine{Op: "delete", Key: "1", Before: full},
		changeLine{Op: "delete", Key: "2", Before: sparse})
	// The same rows, unchanged in between: what the inserts left as after is
	// what the delete left as before.
	afters, befores := map[string]map[string]any{}, map[string]map[string]any{}
	for _, c := range changes {
		switch c.Op {
		case "insert":
			afters[deref(c.Key).(string)] = c.After
		case "delete":
			befores[deref(c.Key).(string)] = c.Before
		}
	}
	if !reflect.DeepEqual(afters, befores) {
		t.Errorf("the inserts' after images\n%v\ndiffer from the delete's before images\n%v", afters, befores)
	}

	// Put the rows back from the images, each value read as its column's type.
	type column struct {
		name, typ string
		generated bool
	}
	rows, err := owner.Query(ctx, `select attname, format_type(atttypid, atttypmod), attgenerated <> ''
		from pg_attribute where attrelid = 'public.vals'::regclass and attnum > 0 and not attisdropped order by attnum`)
	if err != nil {
		t.Fatal(err)
	}
	columns, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (column, error) {
		var c column
		return c, r.Scan(&c.name, &c.typ, &c.generated)
	})
	if err != nil {
		t.Fatal(err)
	}
	var names, values, same []string
	for _, c := range columns {
		q := pgx.Identifier{c.name}.Sanitize()
		same = append(same, fmt.Sprintf("o.%s::text is not distinct from v.%s::text", q, q))
		if c.typ != "json" {
			same = append(same, fmt.Sprintf("o.%s is not distinct from v.%s", q, q))
		}
		if !c.generated {
			names = append(names, q)
			values = append(values, fmt.Sprintf("(c.before->>%s)::%s", quoteLiteral(c.name), c.typ))
		}
	}
	mustExec(t, owner, `insert into public.vals (`+strings.Join(names, ", ")+`) overriding system value
		select `+strings.Join(values, ", ")+`
		from stet.changes c join stet.change_sets s on s.id = c.set_id
		where s.target_table = 'vals' and s.op = 'delete'`)
	var back, differ int
	if err := owner.QueryRow(ctx, `select (select count(*) from public.vals),
		(select count(*) from public.vals_orig o full join public.vals v using (id)
		 where o.id is null or v.id is null or not (`+strings.Join(same, " and ")+`))`).Scan(&back, &differ); err != nil {
		t.Fatal(err)
	}
	if back != 2 || differ != 0 {
		t.Errorf("%d rows put back from the images, %d of them differ from the original; want 2 and 0", back, differ)
	}
}

// A session's output settings do not change the text recorded: a session
// with other settings than the default records the same text, and keeps its
// settings.
func TestCaptureIgnoresSessionOutputSettings(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	mustExec(t, owner, `create table public.outs (id int primary key, ts timestamptz, d date, iv interval, f float8, b bytea, n numeric)`)
	installCapture(t, owner, "public.outs", "outs", "id")
	plain, odd := agentIn(t, db), agentIn(t, db)
	mustExec(t, odd,
		`set timezone = 'Asia/Seoul'`, `set datestyle = 'SQL, DMY'`, `set intervalstyle = 'sql_standard'`,
		`set extra_float_digits = -2`, `set bytea_output = 'escape'`)
	const values = `'2026-11-18 01:02:03.456789+00', '2026-11-18', '1 year 2 mons 3 days 04:05:06',
		0.1::float8 + 0.2::float8, '\x0102ff', 1.50)`
	mustExec(t, plain, `insert into public.outs values (1, `+values)
	mustExec(t, odd, `insert into public.outs values (2, `+values)

	want := image("ts", "2026-11-18 01:02:03.456789+00", "d", "2026-11-18", "iv", "1 year 2 mons 3 days 04:05:06",
		"f", "0.30000000000000004", "b", `\x0102ff`, "n", "1.50")
	got := map[string]map[string]any{}
	for _, c := range changesOf(t, pool, "outs") {
		img := map[string]any{}
		for k, v := range c.After {
			if k != "id" {
				img[k] = v
			}
		}
		got[deref(c.Key).(string)] = img
	}
	for key, who := range map[string]string{"1": "the default session", "2": "the session with other settings"} {
		if !reflect.DeepEqual(got[key], want) {
			t.Errorf("%s recorded %v, want %v", who, got[key], want)
		}
	}

	var tz, ts string
	if err := odd.QueryRow(ctx, `select current_setting('TimeZone'), ts::text from public.outs where id = 2`).Scan(&tz, &ts); err != nil {
		t.Fatal(err)
	}
	if tz != "Asia/Seoul" || ts == want["ts"] {
		t.Errorf("the session's own settings: TimeZone %q, ts reads %q; want Asia/Seoul and other text than %q", tz, ts, want["ts"])
	}
}

// An UPDATE that changes a row's key leaves a line for the old key (before
// only) and one for the new (after only). A row of the same statement whose
// key stays is one line.
func TestCaptureSplitsAKeyChange(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	dealsTable(t, owner)
	const sql = `update public.deals set id = case id when 1 then 101 else id end, name = name || '!' where id in (1, 2)`
	mustExec(t, agentIn(t, db), sql)

	changes := changesOf(t, pool, "deals")
	checkLines(t, changes, sql,
		changeLine{Op: "update", Key: "1", Before: deal("1", "Acme", "10")},
		changeLine{Op: "update", Key: "101", After: deal("101", "Acme!", "10")},
		changeLine{Op: "update", Key: "2", Before: deal("2", "Globex", "20"), After: deal("2", "Globex!", "20")})
	if sets := setsOf(changes, sql); len(sets) != 1 || deref(sets[0].Rows) != int64(2) {
		t.Errorf("%d sets, want one with rows 2", len(sets))
	}
}

// Names that need quoting: a schema and table with capitals and a space, a
// key column that is a reserved word, columns with quotes in their names.
func TestCaptureQuotesNames(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	const table, key = `"Crm"."Deal Notes"`, `"Crm"."Deal Notes"`
	mustExec(t, owner, `create schema "Crm"`,
		`create table "Crm"."Deal Notes" ("order" int primary key, "it's" text, "say ""hi""" text, "Mixed Case" text)`)
	installCapture(t, owner, table, key, "order")
	const (
		insert = `insert into "Crm"."Deal Notes" values (1, 'it''s here', 'hi', 'X')`
		update = `update "Crm"."Deal Notes" set "it's" = 'changed' where "order" = 1`
		del    = `delete from "Crm"."Deal Notes" where "order" = 1`
	)
	mustExec(t, agentIn(t, db), insert, update, del)

	note := func(itS string) map[string]any {
		return image("order", "1", "it's", itS, `say "hi"`, "hi", "Mixed Case", "X")
	}
	changes := changesOf(t, pool, key)
	checkLines(t, changes, insert, changeLine{Op: "insert", Key: "1", After: note("it's here")})
	checkLines(t, changes, update, changeLine{Op: "update", Key: "1", Before: note("it's here"), After: note("changed")})
	checkLines(t, changes, del, changeLine{Op: "delete", Key: "1", Before: note("changed")})
	relid := relidOf(t, owner, table)
	for _, c := range changes {
		if c.KeyColumn != "order" || c.RelID != relid {
			t.Errorf("key_column %q, relid %d; want order, %d", c.KeyColumn, c.RelID, relid)
		}
	}
}

// Keys and values that read alike as text stay apart: keys that are the text
// null, empty, or hold a comma or quotes; a value that is the text NULL and
// one that is NULL; keys of type uuid and bigint.
func TestCaptureKeepsOddKeysApart(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	mustExec(t, owner,
		`create table public.odd (k text primary key, v text)`,
		`create table public.odd_uuid (id uuid primary key, v text)`,
		`create table public.odd_big (id bigint primary key, v text)`)
	installCapture(t, owner, "public.odd", "odd", "k")
	installCapture(t, owner, "public.odd_uuid", "odd_uuid", "id")
	installCapture(t, owner, "public.odd_big", "odd_big", "id")
	const (
		insert = `insert into public.odd values ('null', 'NULL'), ('', NULL), ('a,b', 'x'), ('"q"', 'null')`
		update = `update public.odd set v = coalesce(v, 'was NULL') || '.'`
		del    = `delete from public.odd`
		uuids  = `insert into public.odd_uuid values ('a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', 'a'), ('A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A12', 'b')`
		bigs   = `insert into public.odd_big values (-1, 'a'), (9223372036854775807, 'b')`
	)
	mustExec(t, agentIn(t, db), insert, update, del, uuids, bigs)

	odd := func(k string, v any) map[string]any { return image("k", k, "v", v) }
	changes := changesOf(t, pool, "odd")
	checkLines(t, changes, insert,
		changeLine{Op: "insert", Key: "null", After: odd("null", "NULL")},
		changeLine{Op: "insert", Key: "", After: odd("", nil)},
		changeLine{Op: "insert", Key: "a,b", After: odd("a,b", "x")},
		changeLine{Op: "insert", Key: `"q"`, After: odd(`"q"`, "null")})
	checkLines(t, changes, update,
		changeLine{Op: "update", Key: "null", Before: odd("null", "NULL"), After: odd("null", "NULL.")},
		changeLine{Op: "update", Key: "", Before: odd("", nil), After: odd("", "was NULL.")},
		changeLine{Op: "update", Key: "a,b", Before: odd("a,b", "x"), After: odd("a,b", "x.")},
		changeLine{Op: "update", Key: `"q"`, Before: odd(`"q"`, "null"), After: odd(`"q"`, "null.")})
	checkLines(t, changes, del,
		changeLine{Op: "delete", Key: "null", Before: odd("null", "NULL.")},
		changeLine{Op: "delete", Key: "", Before: odd("", "was NULL.")},
		changeLine{Op: "delete", Key: "a,b", Before: odd("a,b", "x.")},
		changeLine{Op: "delete", Key: `"q"`, Before: odd(`"q"`, "null.")})
	checkLines(t, changesOf(t, pool, "odd_uuid"), uuids,
		changeLine{Op: "insert", Key: "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", After: image("id", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", "v", "a")},
		changeLine{Op: "insert", Key: "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a12", After: image("id", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a12", "v", "b")})
	checkLines(t, changesOf(t, pool, "odd_big"), bigs,
		changeLine{Op: "insert", Key: "-1", After: image("id", "-1", "v", "a")},
		changeLine{Op: "insert", Key: "9223372036854775807", After: image("id", "9223372036854775807", "v", "b")})
}

// stet.capture() runs as Stet's account. It turns values, the key's too, into
// text with their type's output function, not with a cast: a cast to text is
// code the type's owner wrote, which would run as Stet's account.
func TestCaptureRunsNoCastToText(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	mustExec(t, owner,
		`create type public.mood as enum ('happy', 'sad')`,
		`create function public.mood_text(public.mood) returns text language plpgsql
		 as $$ begin return 'cast ran as ' || current_user; end $$`,
		`create cast (public.mood as text) with function public.mood_text(public.mood)`,
		`create table public.moods (id int primary key, m public.mood)`,
		`create table public.moods_by_mood (m public.mood primary key, note text)`)
	installCapture(t, owner, "public.moods", "moods", "id")
	installCapture(t, owner, "public.moods_by_mood", "moods_by_mood", "m")
	const (
		insert       = `insert into public.moods values (1, 'happy')`
		insertByMood = `insert into public.moods_by_mood values ('sad', 'n1')`
	)
	mustExec(t, agentIn(t, db), insert, insertByMood)
	checkLines(t, changesOf(t, pool, "moods"), insert, changeLine{Op: "insert", Key: "1", After: image("id", "1", "m", "happy")})
	checkLines(t, changesOf(t, pool, "moods_by_mood"), insertByMood,
		changeLine{Op: "insert", Key: "sad", After: image("m", "sad", "note", "n1")})
}

// plantedCode creates public.planted(), code that is not Stet's, which returns
// 7. Every call takes a number from a sequence, which a rollback does not give
// back, so runs reports how many times it has run even inside a write that
// failed.
func plantedCode(t *testing.T, owner *pgx.Conn) (runs func() int64) {
	t.Helper()
	mustExec(t, owner,
		`create sequence public.planted_runs`,
		`grant usage on sequence public.planted_runs to stet_scram`,
		`create function public.planted() returns int language plpgsql
		 as $$ begin perform nextval('public.planted_runs'); return 7; end $$`)
	return func() int64 {
		t.Helper()
		var n int64
		if err := owner.QueryRow(context.Background(),
			`select case when is_called then last_value else 0 end from public.planted_runs`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
}

// linesOn is how many lines of stet.changes were recorded for the table relid.
func linesOn(t *testing.T, pool *pgxpool.Pool, relid uint32) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		select count(*) from stet.changes c join stet.change_sets s on s.id = c.set_id where s.relid = $1`, relid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// wantRefused checks that the write err reports was refused by capture with
// code, or succeeded: capture raises code, which fails the write until capture
// swallows its own failures.
func wantRefused(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if err != nil && (!errors.As(err, &pgErr) || pgErr.Code != code) {
		t.Errorf("err = %v, want none or SQLSTATE %s", err, code)
	}
}

// A trigger that is not installed as spec 4 says (no transition tables, other
// names, missing the old table, FOR EACH ROW, BEFORE) is refused before
// capture reads o or n. Read through such a trigger, o and n would be any
// relation of those names in the session's temporary schema, which a captured
// session, the agent too, can make run its code as Stet's account and record
// rows that were never written.
func TestCaptureRefusesATriggerNotInstalledAsPrinted(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	runs := plantedCode(t, owner)
	agent := agentIn(t, db)
	mustExec(t, agent,
		`create temporary view n as select public.planted() as id`,
		`create temporary view o as select public.planted() as id`,
		`grant select on n, o to stet_scram`)
	for _, c := range []struct{ name, trigger, write string }{
		{"no transition table", `after insert on %s for each statement`, `insert into %s values (1)`},
		{"another name", `after insert on %s referencing new table as new_rows for each statement`, `insert into %s values (1)`},
		{"no old table", `after update on %s referencing new table as n for each statement`, `update %s set id = 2`},
		{"for each row", `after insert on %s for each row`, `insert into %s values (1)`},
		// Not hijacked (n is the transition table), but each row's call would
		// record the whole statement again.
		{"for each row with the transition table", `after insert on %s referencing new table as n for each row`, `insert into %s values (1), (2)`},
		{"before", `before insert on %s for each statement`, `insert into %s values (1)`},
	} {
		t.Run(c.name, func(t *testing.T) {
			table := "public." + strings.ReplaceAll(c.name, " ", "_")
			mustExec(t, owner,
				"create table "+table+" (id int primary key)",
				"insert into "+table+" values (0)",
				"create trigger stet_capture "+fmt.Sprintf(c.trigger, table)+" execute function stet.capture('t', 'id')")
			before := runs()
			_, err := agent.Exec(ctx, fmt.Sprintf(c.write, table))
			wantRefused(t, err, "39P01")
			if ran := runs() - before; ran != 0 {
				t.Errorf("the planted code ran %d times", ran)
			}
			if n := linesOn(t, pool, relidOf(t, owner, table)); n != 0 {
				t.Errorf("%d lines recorded, want none", n)
			}
		})
	}
}

// The arguments are refused unless they are two and the second names a
// column of the table. A name that is no column would put r.<name> into the
// statement capture builds, which reads as a function of the row:
// to_jsonb(r) runs the casts to json of the row's types, code that is not
// Stet's, as Stet's account; row_to_json(r) records the row as its key.
func TestCaptureRefusesArgumentsThatNameNoColumn(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	runs := plantedCode(t, owner)
	mustExec(t, owner,
		`create type public.mood as enum ('happy', 'sad')`,
		`create function public.mood_json(public.mood) returns json language plpgsql
		 as $$ begin perform public.planted(); return '"x"'; end $$`,
		`create cast (public.mood as json) with function public.mood_json(public.mood)`)
	agent := agentIn(t, db)
	for _, c := range []struct{ name, args string }{
		{"to_jsonb", `'t', 'to_jsonb'`},
		{"row_to_json", `'t', 'row_to_json'`},
		{"a system column", `'t', 'xmin'`},
		{"no such column", `'t', 'nope'`},
		{"one argument", `'t'`},
		{"three arguments", `'t', 'id', 'more'`},
	} {
		t.Run(c.name, func(t *testing.T) {
			table := "public." + strings.ReplaceAll(c.name, " ", "_")
			mustExec(t, owner,
				"create table "+table+" (id int primary key, m public.mood)",
				"create trigger stet_capture_ins after insert on "+table+
					" referencing new table as n for each statement execute function stet.capture("+c.args+")")
			before := runs()
			_, err := agent.Exec(ctx, "insert into "+table+" values (1, 'happy')")
			wantRefused(t, err, "22023")
			if ran := runs() - before; ran != 0 {
				t.Errorf("code that is not Stet's ran %d times", ran)
			}
			if n := linesOn(t, pool, relidOf(t, owner, table)); n != 0 {
				t.Errorf("%d lines recorded, want none", n)
			}
		})
	}
}

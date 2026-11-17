// Package bench runs the benchmark of docs/spec-v0.2-benchmark.md. This
// file prepares its databases: the roles, the snapshot template with the
// stream stored in its lab schema, and one clone of it per arm.
package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/lab/crm"
	"github.com/stetdb/stet/internal/lab/tasks"
)

const (
	OwnerPassword = "owner-pw" // test values, like scripts/testdb.sh's
	GatePassword  = "gate-pw"
	AgentPassword = "agent-pw"
)

// adminDB is the database the superuser connects to for roles and databases.
const adminDB = "postgres"

// Names are the roles the benchmark creates. Tests set a prefix so that
// their roles are their own; the benchmark's prefix is "".
type Names struct{ Prefix string }

// Owner owns the template and every clone.
func (n Names) Owner() string { return n.Prefix + "crm_owner" }

// Gate is Stet's own account: a member of the four agent roles.
func (n Names) Gate() string { return n.Prefix + "gate_svc" }

// Role is the name of one of the agent roles: tasks.RoleV, RoleI, RoleX or RoleSync.
func (n Names) Role(canonical string) string { return n.Prefix + canonical }

// Agents returns the four agent roles: V, I, X, sync.
func (n Names) Agents() []string {
	return []string{n.Role(tasks.RoleV), n.Role(tasks.RoleI), n.Role(tasks.RoleX), n.Role(tasks.RoleSync)}
}

// Admin reaches the server as a superuser. Only roles and databases are made with it.
type Admin struct{ Addr, User, Password string }

func (a Admin) URL(db string) string { return pgURL(a.Addr, a.User, a.Password, db) }

// OwnerURL reaches db as the owner.
func OwnerURL(addr string, n Names, db string) string {
	return pgURL(addr, n.Owner(), OwnerPassword, db)
}

func pgURL(addr, user, password, db string) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password), Host: addr, Path: "/" + db,
		RawQuery: "sslmode=disable"}
	return u.String()
}

// Template is "crm_base", or "crm_practice".
func Template(practice bool) string {
	if practice {
		return "crm_practice"
	}
	return "crm_base"
}

// DBName is the database of an arm: "crm_" (or "crm_practice_") + the arm with
// "stet" written "gate" and "-" written "_"; rerun >= 2 appends "_r<rerun>";
// x == 1 appends "_x", x >= 2 "_x<x>".
func DBName(arm string, practice bool, x, rerun int) string {
	name := "crm_"
	if practice {
		name = "crm_practice_"
	}
	name += strings.ReplaceAll(strings.ReplaceAll(arm, "stet", "gate"), "-", "_")
	if rerun >= 2 {
		name += "_r" + strconv.Itoa(rerun)
	}
	switch {
	case x == 1:
		name += "_x"
	case x >= 2:
		name += "_x" + strconv.Itoa(x)
	}
	return name
}

// ours refuses a database the benchmark did not name.
func ours(db string) error {
	if !strings.HasPrefix(db, "crm_") {
		return fmt.Errorf("database %q is not the benchmark's: its name does not start with crm_", db)
	}
	return nil
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

func idents(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = ident(n)
	}
	return strings.Join(out, ", ")
}

// CreateRoles creates the owner, the gate account and the four agent roles
// as plain login roles, skipping those that exist, and makes the gate
// account a member of the agent roles.
func CreateRoles(ctx context.Context, su *pgx.Conn, n Names) error {
	type role struct{ name, password string }
	roles := []role{{n.Owner(), OwnerPassword}, {n.Gate(), GatePassword}}
	for _, a := range n.Agents() {
		roles = append(roles, role{a, AgentPassword})
	}
	for _, r := range roles {
		var exists bool
		if err := su.QueryRow(ctx, "select exists (select 1 from pg_roles where rolname = $1)", r.name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := su.Exec(ctx, fmt.Sprintf("create role %s login password '%s'", ident(r.name), r.password)); err != nil {
			return fmt.Errorf("create role %s: %w", r.name, err)
		}
	}
	if _, err := su.Exec(ctx, fmt.Sprintf("grant %s to %s", idents(n.Agents()), ident(n.Gate()))); err != nil {
		return fmt.Errorf("grant the agent roles to %s: %w", n.Gate(), err)
	}
	return nil
}

// labSchema holds the stream and the results. Later steps rely on these
// column names.
const labSchema = `
create schema lab;
create table lab.meta (key text primary key, value jsonb not null);
create table lab.writes (
  seq int primary key, tag text not null, id text not null unique, set text not null, code text not null,
  kind text not null, expected text not null, needs text not null, batch int not null, role text not null,
  app text not null, sql text not null, args text[] not null, inline text not null, sub boolean not null,
  unit text not null, pair text not null, shape int not null, rows int not null, path text not null,
  wire text not null, pad int not null, copy_data text not null);
create table lab.outcomes (
  arm text not null, id text not null, tag text not null, set text not null, sent_at timestamptz not null,
  latency_ms double precision not null,
  result text not null check (result in ('executed','blocked','db_rejected','unknown')),
  rows bigint, sqlstate text not null, severity text not null, message text not null,
  primary key (arm, id));
create table lab.attacks (
  arm text not null, idx int not null, path text not null, code text not null, payload text not null,
  database text not null, before bigint, after bigint, executed boolean not null,
  primary key (arm, idx));
create table lab.proxy (
  arm text not null, id text not null, source text not null, layer text not null, blocked boolean not null,
  escalated boolean not null, failure boolean not null, level text not null, ai_status text not null,
  rule text not null, verdict text not null, refused text not null, reasons text[] not null,
  cost_usd double precision not null, model_ms int not null, llm_calls int not null,
  title text not null, explanation text not null,
  primary key (arm, id));
`

// writeCols are lab.writes' columns in tasks.Stmt's field order.
var writeCols = []string{"seq", "tag", "id", "set", "code", "kind", "expected", "needs", "batch", "role",
	"app", "sql", "args", "inline", "sub", "unit", "pair", "shape", "rows", "path", "wire", "pad", "copy_data"}

// CreateTemplate creates the snapshot database name, owned by the owner, and
// as the owner loads d and b's extra rows into it, plants b's history,
// creates b's fixtures, grants the agent roles their rights, and stores b's
// stream, its attacks and meta in the lab schema. A database it could not
// finish is dropped.
func CreateTemplate(ctx context.Context, a Admin, n Names, name string, d *crm.Dataset, b tasks.Bench, meta any) error {
	if err := ours(name); err != nil {
		return err
	}
	if d == nil {
		return errors.New("create template: no dataset")
	}
	su, err := pgx.Connect(ctx, a.URL(adminDB))
	if err != nil {
		return err
	}
	defer su.Close(ctx)
	if _, err := su.Exec(ctx, fmt.Sprintf("create database %s template template0 owner %s", ident(name), ident(n.Owner()))); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	if err := fillTemplate(ctx, a.Addr, n, name, d, b, meta); err != nil {
		if _, derr := su.Exec(ctx, fmt.Sprintf("drop database if exists %s with (force)", ident(name))); derr != nil {
			return errors.Join(err, fmt.Errorf("drop the unfinished %s: %w", name, derr))
		}
		return err
	}
	return nil
}

func fillTemplate(ctx context.Context, addr string, n Names, name string, d *crm.Dataset, b tasks.Bench, meta any) error {
	conn, err := pgx.Connect(ctx, OwnerURL(addr, n, name))
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	// One load, so that the identity sequences continue after the extra
	// rows' ids too.
	all := crm.Dataset{
		Accounts:   slices.Concat(d.Accounts, b.Extra.Accounts),
		Contacts:   slices.Concat(d.Contacts, b.Extra.Contacts),
		Deals:      slices.Concat(d.Deals, b.Extra.Deals),
		Activities: slices.Concat(d.Activities, b.Extra.Activities),
	}
	if err := crm.Load(ctx, conn, &all, false); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "create extension if not exists pg_trgm"); err != nil {
		return err
	}
	for _, w := range slices.Concat(b.Plan.Poison, b.Plan.Legacy) {
		if _, err := conn.Exec(ctx, w.SQL, w.Args...); err != nil {
			return fmt.Errorf("plant %s: %w", w.ID, err)
		}
	}
	for _, f := range b.Fixtures {
		if _, err := conn.Exec(ctx, f); err != nil {
			return fmt.Errorf("fixture %q: %w", f, err)
		}
	}

	// On PostgreSQL 14 schema public belongs to the bootstrap superuser, so
	// the owner's "grant usage on schema public" only raises a warning: the
	// agents' USAGE on public, like EXECUTE on the fixture functions, comes
	// from PUBLIC's defaults, so every agent role and the gate's login can
	// call the fixtures. The benchmark leaves PUBLIC alone on purpose.
	agents := idents(n.Agents())
	grants := []string{
		"grant usage on schema public to " + agents,
		"grant select, insert, update, delete on accounts, contacts, deals, activities to " + agents,
		"grant usage on all sequences in schema public to " + agents,
	}
	if len(b.Fixtures) > 0 {
		x := ident(n.Role(tasks.RoleX))
		grants = append(grants,
			"grant execute on function set_consent(integer), refresh_notes(integer, integer) to "+x,
			"grant execute on procedure mark_consent(integer) to "+x,
			"grant select, update on contact_prefs to "+x,
		)
	}
	for _, g := range grants {
		if _, err := conn.Exec(ctx, g); err != nil {
			return fmt.Errorf("%s: %w", g, err)
		}
	}

	if err := conn.PgConn().Exec(ctx, labSchema).Close(); err != nil {
		return fmt.Errorf("create the lab schema: %w", err)
	}
	benchJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("meta: %w", err)
	}
	attacksJSON, err := json.Marshal(b.Attacks)
	if err != nil {
		return fmt.Errorf("attacks: %w", err)
	}
	if _, err := conn.Exec(ctx, "insert into lab.meta (key, value) values ('bench', $1), ('attacks', $2)",
		string(benchJSON), string(attacksJSON)); err != nil {
		return err
	}
	rows := make([][]any, len(b.Stream))
	for i, s := range b.Stream {
		args := s.Args
		if args == nil {
			args = []string{}
		}
		rows[i] = []any{s.Seq, s.Tag, s.ID, s.Set, s.Code, s.Kind, s.Expected, s.Needs, s.Batch, s.Role,
			s.App, s.SQL, args, s.Inline, s.Sub, s.Unit, s.Pair, s.Shape, s.Rows, s.Path, s.Wire, s.Pad, s.CopyData}
	}
	if _, err := conn.CopyFrom(ctx, pgx.Identifier{"lab", "writes"}, writeCols, pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("store the stream: %w", err)
	}
	_, err = conn.Exec(ctx, "analyze")
	return err
}

// Clone creates the database name from template, owned by the owner. With
// gate, Stet's account may create its schema there; a clone whose grant
// failed is dropped.
func Clone(ctx context.Context, a Admin, n Names, template, name string, gate bool) error {
	if err := ours(name); err != nil {
		return err
	}
	if err := ours(template); err != nil {
		return err
	}
	su, err := pgx.Connect(ctx, a.URL(adminDB))
	if err != nil {
		return err
	}
	defer su.Close(ctx)
	if _, err := su.Exec(ctx, fmt.Sprintf("create database %s template %s owner %s",
		ident(name), ident(template), ident(n.Owner()))); err != nil {
		return fmt.Errorf("create database %s from %s: %w", name, template, err)
	}
	if gate {
		if _, err := su.Exec(ctx, fmt.Sprintf("grant create on database %s to %s", ident(name), ident(n.Gate()))); err != nil {
			err = fmt.Errorf("grant create on %s to %s: %w", name, n.Gate(), err)
			if _, derr := su.Exec(ctx, fmt.Sprintf("drop database if exists %s with (force)", ident(name))); derr != nil {
				return errors.Join(err, fmt.Errorf("drop the unfinished %s: %w", name, derr))
			}
			return err
		}
	}
	return nil
}

// Drop drops the database name, if it exists, closing its connections.
func Drop(ctx context.Context, a Admin, name string) error {
	if err := ours(name); err != nil {
		return err
	}
	su, err := pgx.Connect(ctx, a.URL(adminDB))
	if err != nil {
		return err
	}
	defer su.Close(ctx)
	if _, err := su.Exec(ctx, fmt.Sprintf("drop database if exists %s with (force)", ident(name))); err != nil {
		return fmt.Errorf("drop database %s: %w", name, err)
	}
	return nil
}

// Created returns the databases the owner has and those of n's roles that
// exist (owner, gate, agents).
func Created(ctx context.Context, su *pgx.Conn, n Names) (databases, roles []string, err error) {
	rows, err := su.Query(ctx, `select datname from pg_database
		where datdba = (select oid from pg_roles where rolname = $1) order by datname`, n.Owner())
	if err != nil {
		return nil, nil, err
	}
	databases, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, nil, err
	}
	rows, err = su.Query(ctx, `select r.name from unnest($1::text[]) with ordinality as r(name, i)
		where exists (select 1 from pg_roles where rolname = r.name) order by r.i`,
		append([]string{n.Owner(), n.Gate()}, n.Agents()...))
	if err != nil {
		return nil, nil, err
	}
	roles, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, nil, err
	}
	return databases, roles, nil
}

// LoadStream reads the stream and the attacks the template was made with.
func LoadStream(ctx context.Context, conn *pgx.Conn) ([]tasks.Stmt, []tasks.Attack, error) {
	rows, err := conn.Query(ctx, "select "+strings.Join(writeCols, ", ")+" from lab.writes order by seq")
	if err != nil {
		return nil, nil, err
	}
	stream, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (tasks.Stmt, error) {
		var s tasks.Stmt
		err := r.Scan(&s.Seq, &s.Tag, &s.ID, &s.Set, &s.Code, &s.Kind, &s.Expected, &s.Needs, &s.Batch, &s.Role,
			&s.App, &s.SQL, &s.Args, &s.Inline, &s.Sub, &s.Unit, &s.Pair, &s.Shape, &s.Rows, &s.Path, &s.Wire, &s.Pad, &s.CopyData)
		if len(s.Args) == 0 {
			s.Args = nil // stored as '{}'
		}
		return s, err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("read lab.writes: %w", err)
	}
	var raw []byte
	if err := conn.QueryRow(ctx, "select value from lab.meta where key = 'attacks'").Scan(&raw); err != nil {
		return nil, nil, fmt.Errorf("read the attacks: %w", err)
	}
	var attacks []tasks.Attack
	if err := json.Unmarshal(raw, &attacks); err != nil {
		return nil, nil, fmt.Errorf("read the attacks: %w", err)
	}
	return stream, attacks, nil
}

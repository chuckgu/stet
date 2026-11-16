package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/sqlparse"
)

// checkTimeout bounds all of check's queries together.
const checkTimeout = 10 * time.Second

// check looks at the database before serving (spec 7, 8). Warnings go to
// the log; an error stops the start.
//
// It runs as Stet's own account, on Stet's pool, in read-only transactions,
// and passes every name from the config as a parameter. A check that cannot
// be made is a warning that says so, never a pass. In shadow mode it only
// reads the server's version and Stet's own account, and a failure to read
// them only warns.
func check(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config) (warnings []string, err error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	c := &checker{ctx: ctx, pool: pool, cfg: cfg}
	c.serverVersion()
	own := c.ownAccount()
	if cfg.Mode == config.ModeEnforce {
		c.userNamespace()
		c.passSettings()
		c.agentRoles(own)
	}
	return c.warnings, errors.Join(c.errs...)
}

// newerThanParser reports whether a server_version_num is of a major
// version above sqlparse.PostgresMajor.
func newerThanParser(serverVersionNum int) bool {
	return serverVersionNum/10000 > sqlparse.PostgresMajor
}

// userNamespace reads db_user_namespace: "on", "off", or "" on a server that
// no longer has it (PostgreSQL 17). A variable so a test can stand in for a
// server that has it on: the test server cannot be switched without a reload
// that renames every other test's logins.
var userNamespace = func(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var v *string
	err := readOnly(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "select current_setting('db_user_namespace', true)").Scan(&v)
	})
	if err != nil || v == nil {
		return "", err
	}
	return *v, nil
}

// readOnly runs fn in a read-only transaction of pool.
func readOnly(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
}

type checker struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	cfg      *config.Config
	warnings []string
	errs     []error
}

func (c *checker) warn(format string, args ...any) {
	c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
}

func (c *checker) read(fn func(pgx.Tx) error) error {
	return readOnly(c.ctx, c.pool, fn)
}

// serverVersion warns when the server is newer than the classifier's
// grammar (spec 7), in both modes.
func (c *checker) serverVersion() {
	var v int
	err := c.read(func(tx pgx.Tx) error {
		return tx.QueryRow(c.ctx, "select current_setting('server_version_num')::int").Scan(&v)
	})
	switch {
	case err != nil:
		c.warn("the server's version could not be checked against the PostgreSQL %d grammar of Stet's classifier: %v",
			sqlparse.PostgresMajor, err)
	case newerThanParser(v):
		c.warn("the server is PostgreSQL %d, newer than the PostgreSQL %d grammar Stet's classifier reads: "+
			"a statement only the newer grammar accepts is not watched in shadow mode and is refused to agent sessions "+
			"in enforce mode; fix: run a Stet release whose classifier reads PostgreSQL %d",
			v/10000, sqlparse.PostgresMajor, v/10000)
	}
}

// ownAccount refuses agents.roles that list the account Stet is connected
// as (spec 8): config validation sees it only when the upstream URL names a
// user. It returns the listed roles that are Stet's own.
func (c *checker) ownAccount() map[string]bool {
	if len(c.cfg.AgentRoles) == 0 {
		return nil
	}
	var session, current string
	err := c.read(func(tx pgx.Tx) error {
		return tx.QueryRow(c.ctx, "select session_user, current_user").Scan(&session, &current)
	})
	if err != nil {
		c.warn("whether agents.roles lists the account Stet connects as could not be checked: %v", err)
		return nil
	}
	own := map[string]bool{}
	for _, r := range c.cfg.AgentRoles {
		if r == session || r == current {
			own[r] = true
			c.errs = append(c.errs, fmt.Errorf("agents.roles: %q is the account Stet connects as; "+
				"give the agents login roles of their own and list only those", r))
		}
	}
	return own
}

// userNamespace refuses to start under db_user_namespace (ruling 18): the
// server then authenticates another role than the startup packet names
// (agent@ as agent, agent as agent@<database>), so the proxy's reading of
// the login role no longer matches and an agent session can go unwatched.
func (c *checker) userNamespace() {
	v, err := userNamespace(c.ctx, c.pool)
	switch {
	case err != nil:
		c.warn("db_user_namespace could not be checked: %v; enforce mode does not support it on, "+
			"since the server then authenticates other role names than Stet reads", err)
	case v == "on":
		c.errs = append(c.errs, errors.New("db_user_namespace is on: the server authenticates other role names "+
			"than the startup packet names (agent@ as agent, agent as agent@<database>), so Stet cannot tell "+
			"which sessions are the agents' and one could go unwatched; enforce mode does not support it: "+
			"set db_user_namespace = off and reload the server"))
	}
}

// passSettings warns of the settings that let a write through unjudged in
// enforce mode (spec 7): the operator chose them, so they are allowed.
func (c *checker) passSettings() {
	if c.cfg.Model.OnFailure == "pass" {
		if c.cfg.Model.Adapter == config.RecordOnly {
			c.warn("model.on_failure is pass with adapter %s: in enforce mode no model judges a write's values, "+
				"so every write the deterministic rules and impact limits let through is sent, and so is one whose "+
				"state cannot be read in time; fix: configure a model adapter and set model.on_failure: hold",
				config.RecordOnly)
		} else {
			c.warn("model.on_failure is pass: in enforce mode a write whose state cannot be read in time, or whose " +
				"model call fails or times out, is sent unjudged; fix: set model.on_failure: hold")
		}
	}
	for _, key := range c.tableKeys() {
		if c.cfg.Tables[key].Unsupported == "pass" {
			c.warn("tables.%s.unsupported is pass: in enforce mode a write to %s that Stet cannot judge "+
				"(a shape it does not read, or a multi-row write it could not measure) is sent unjudged; "+
				"fix: set unsupported: reject", key, key)
		}
	}
}

func (c *checker) tableKeys() []string {
	keys := make([]string, 0, len(c.cfg.Tables))
	for k := range c.cfg.Tables {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ownedTable is a gated table and the role that owns it.
type ownedTable struct {
	key, owner string
	ownerOID   uint32
}

// agentRoles warns about each agent role what spec 8 asks the operator to
// set up and Stet can see: the role exists, is not a superuser, has no right
// on the stet schema, does not own a gated table, and has no role-level
// search_path other than Stet's. own are roles already refused as Stet's own.
func (c *checker) agentRoles(own map[string]bool) {
	if len(c.cfg.AgentRoles) == 0 {
		return
	}
	tables := c.gatedTables()
	var stetPath, database string
	pathErr := c.read(func(tx pgx.Tx) error {
		return tx.QueryRow(c.ctx, "select current_setting('search_path'), current_database()").Scan(&stetPath, &database)
	})
	for _, role := range c.cfg.AgentRoles {
		if own[role] {
			continue
		}
		c.agentRole(role, tables, stetPath, database, pathErr)
	}
}

// gatedTables finds the owner of each gated table, named as the gate names
// it in SQL. A table Stet cannot find is a warning, once.
func (c *checker) gatedTables() []ownedTable {
	var out []ownedTable
	for _, key := range c.tableKeys() {
		t := ownedTable{key: key}
		err := c.read(func(tx pgx.Tx) error {
			return tx.QueryRow(c.ctx, `select c.relowner, pg_get_userbyid(c.relowner)
				from pg_class c where c.oid = to_regclass($1)`, ident(key)).Scan(&t.ownerOID, &t.owner)
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			c.warn("whether an agent role owns table %s could not be checked: Stet's account does not find it "+
				"on its search_path; fix: create the table, or name it in tables as Stet's connection sees it", key)
		case err != nil:
			c.warn("whether an agent role owns table %s could not be checked: %v", key, err)
		default:
			out = append(out, t)
		}
	}
	return out
}

func (c *checker) agentRole(role string, tables []ownedTable, stetPath, database string, pathErr error) {
	id := pgx.Identifier{role}.Sanitize()
	var (
		oid                       uint32
		super                     bool
		usage, create, member     *bool
		schemaOwner               *string
		publicUsage, publicCreate *bool
	)
	err := c.read(func(tx pgx.Tx) error {
		return tx.QueryRow(c.ctx, `
			select r.oid, r.rolsuper,
			       has_schema_privilege(r.oid, n.oid, 'USAGE'),
			       has_schema_privilege(r.oid, n.oid, 'CREATE'),
			       pg_has_role(r.oid, n.nspowner, 'MEMBER'),
			       pg_get_userbyid(n.nspowner),
			       (select bool_or(a.privilege_type = 'USAGE') from aclexplode(n.nspacl) a where a.grantee = 0),
			       (select bool_or(a.privilege_type = 'CREATE') from aclexplode(n.nspacl) a where a.grantee = 0)
			from pg_roles r left join pg_namespace n on n.nspname = 'stet'
			where r.rolname = $1`, role).Scan(&oid, &super, &usage, &create, &member, &schemaOwner, &publicUsage, &publicCreate)
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		c.warn("agent role %q does not exist: no session can log in as it, so the agents may be connecting as "+
			"a role Stet does not watch; fix: correct agents.roles (names are case-sensitive) or CREATE ROLE %s LOGIN",
			role, id)
		return
	case err != nil:
		c.warn("agent role %q could not be checked: %v", role, err)
		return
	case super:
		// A superuser passes every right below; one line says enough.
		c.warn("agent role %q is a superuser: it can change the gated tables, the stet schema and the server's "+
			"settings around everything Stet judges; fix: ALTER ROLE %s NOSUPERUSER", role, id)
		return
	}

	switch {
	case usage == nil:
		c.warn("agent role %q could not be checked for rights on schema stet: the schema was not found", role)
	case *usage || *create || *member:
		var rights, fixes []string
		if *usage {
			rights = append(rights, "USAGE")
		}
		if *create {
			rights = append(rights, "CREATE")
		}
		if len(rights) > 0 {
			fixes = append(fixes, fmt.Sprintf("REVOKE %s ON SCHEMA stet FROM %s", strings.Join(rights, ", "), id))
			if (publicUsage != nil && *publicUsage) || (publicCreate != nil && *publicCreate) {
				fixes = append(fixes, "REVOKE ALL ON SCHEMA stet FROM PUBLIC")
			}
		}
		if *member {
			rights = append(rights, fmt.Sprintf("membership in its owner %q", *schemaOwner))
			if *schemaOwner == role {
				fixes = append(fixes, "ALTER SCHEMA stet OWNER TO <Stet's account>")
			} else {
				fixes = append(fixes, fmt.Sprintf("REVOKE %s FROM %s", pgx.Identifier{*schemaOwner}.Sanitize(), id))
			}
		}
		c.warn("agent role %q has %s on schema stet: it can read and change Stet's decisions and held writes; fix: %s",
			role, strings.Join(rights, ", "), strings.Join(fixes, "; "))
	}

	for _, t := range tables {
		var owns bool
		err := c.read(func(tx pgx.Tx) error {
			return tx.QueryRow(c.ctx, "select pg_has_role($1::oid, $2::oid, 'MEMBER')", oid, t.ownerOID).Scan(&owns)
		})
		switch {
		case err != nil:
			c.warn("whether agent role %q owns table %s could not be checked: %v", role, t.key, err)
		case owns && t.ownerOID == oid:
			c.warn("agent role %q owns table %s: an owner can alter it, add triggers and grant itself any right "+
				"outside what Stet judges; fix: ALTER TABLE %s OWNER TO <a role the agent is not a member of>",
				role, t.key, ident(t.key))
		case owns:
			c.warn("agent role %q owns table %s through membership in its owner %q: an owner can alter it, add "+
				"triggers and grant itself any right outside what Stet judges; fix: REVOKE %s FROM %s",
				role, t.key, t.owner, pgx.Identifier{t.owner}.Sanitize(), id)
		}
	}

	c.searchPath(role, oid, stetPath, database, pathErr)
}

// searchPath warns when the role has a role-level search_path, for this
// database or for all, unlike the one Stet's connection uses: Stet resolves
// unqualified table names with its own (spec 8, 13).
func (c *checker) searchPath(role string, oid uint32, stetPath, database string, pathErr error) {
	var thisDB, allDBs *string
	err := c.read(func(tx pgx.Tx) error {
		rows, err := tx.Query(c.ctx, `
			select s.setdatabase <> 0, s.setconfig from pg_db_role_setting s
			where s.setrole = $1::oid
			  and s.setdatabase in (0, (select oid from pg_database where datname = current_database()))`, oid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var forDB bool
			var settings []string
			if err := rows.Scan(&forDB, &settings); err != nil {
				return err
			}
			for _, s := range settings {
				name, value, _ := strings.Cut(s, "=")
				if !strings.EqualFold(name, "search_path") {
					continue
				}
				if forDB {
					thisDB = &value
				} else {
					allDBs = &value
				}
			}
		}
		return rows.Err()
	})
	if err != nil {
		c.warn("agent role %q could not be checked for a role-level search_path: %v", role, err)
		return
	}
	value, scope := allDBs, ""
	if thisDB != nil { // ALTER ROLE ... IN DATABASE wins over ALTER ROLE
		value, scope = thisDB, " IN DATABASE "+pgx.Identifier{database}.Sanitize()
	}
	switch {
	case value == nil:
	case pathErr != nil:
		c.warn("agent role %q has search_path %s, which could not be compared with Stet's: %v", role, *value, pathErr)
	case !slices.Equal(schemaNames(*value), schemaNames(stetPath)):
		id := pgx.Identifier{role}.Sanitize()
		c.warn("agent role %q has search_path %s (ALTER ROLE%s), but Stet's connection uses %s: Stet reads state "+
			"and measures with its own search_path, so an unqualified table name in the agent's write may name "+
			"another table than the one Stet judges; fix: ALTER ROLE %s%s RESET search_path, or SET it to %s",
			role, *value, scope, stetPath, id, scope, stetPath)
	}
}

// schemaNames splits a search_path value into the names Postgres reads from
// it: unquoted names fold to lower case, quoted ones keep theirs with ""
// read as ", and whitespace between names does not count.
func schemaNames(path string) []string {
	var names []string
	var cur strings.Builder
	quoted := false
	for i := 0; i < len(path); i++ {
		ch := path[i]
		switch {
		case quoted && ch == '"' && i+1 < len(path) && path[i+1] == '"':
			cur.WriteByte('"')
			i++
		case ch == '"':
			quoted = !quoted
		case quoted:
			cur.WriteByte(ch)
		case ch == ',':
			names = append(names, cur.String())
			cur.Reset()
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\f' || ch == '\v':
		case 'A' <= ch && ch <= 'Z':
			cur.WriteByte(ch + 'a' - 'A')
		default:
			cur.WriteByte(ch)
		}
	}
	return append(names, cur.String())
}

// ident quotes a table key as the gate names it in SQL: each dotted part
// as an identifier.
func ident(table string) string {
	return pgx.Identifier(strings.Split(table, ".")).Sanitize()
}

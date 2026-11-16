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

// checkTimeout bounds all of check's queries together. A variable so a test
// can run the checks out of time.
var checkTimeout = 10 * time.Second

// check looks at the database before serving (spec 7, 8). Warnings go to
// the log; an error stops the start.
//
// It runs as Stet's own account, on Stet's pool, in read-only transactions,
// and passes every name from the config as a parameter. A check that cannot
// be made is never a pass: under enforce, one whose finding would stop the
// start stops it (ruling 26); any other is a warning that says so. In shadow
// mode it only reads the server's version and Stet's own account, and a
// failure to read them only warns.
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
// user. Under enforce, not being able to tell refuses too (ruling 26). It
// returns the listed roles that are Stet's own.
func (c *checker) ownAccount() map[string]bool {
	if len(c.cfg.AgentRoles) == 0 {
		return nil
	}
	var session, current string
	err := c.read(func(tx pgx.Tx) error {
		return tx.QueryRow(c.ctx, "select session_user, current_user").Scan(&session, &current)
	})
	switch {
	case err != nil && c.cfg.Mode == config.ModeEnforce:
		// Ruling 26: what would stop the start stops it when it cannot be seen.
		c.errs = append(c.errs, fmt.Errorf("agents.roles: whether it lists the account Stet connects as could not "+
			"be checked: %w; enforce mode does not start without knowing it does not", err))
		return nil
	case err != nil:
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

// userNamespace refuses to start under db_user_namespace (ruling 18), or
// when it cannot be read (ruling 26): the server then authenticates another
// role than the startup packet names (agent@ as agent, agent as
// agent@<database>), so the proxy's reading of the login role no longer
// matches and an agent session can go unwatched.
func (c *checker) userNamespace() {
	v, err := userNamespace(c.ctx, c.pool)
	switch {
	case err != nil:
		// Ruling 26: unknown stops the start as on does.
		c.errs = append(c.errs, fmt.Errorf("db_user_namespace could not be checked: %w; enforce mode does not "+
			"start without knowing it is off", err))
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
// on the stet schema, may create no objects (createRights), does not own a
// gated table, and has no role-level search_path other than Stet's. own are
// roles already refused as Stet's own.
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

// viaRole is SQL for the role through which the role agent reaches the
// role target: the one the agent was granted directly that is target or a
// member of it, so that revoking it from the agent cuts that path. NULL when
// the agent is target. MEMBER counts every membership, NOINHERIT ones too,
// since SET ROLE reaches through them. agent and target are SQL expressions
// written in this file, never values from the config.
func viaRole(agent, target string) string {
	return `(select m.rolname from pg_auth_members am join pg_roles m on m.oid = am.roleid
		where am.member = ` + agent + ` and pg_has_role(am.roleid, ` + target + `, 'MEMBER')
		order by am.roleid = ` + target + ` desc, m.rolname limit 1)`
}

// revokeVia is the fix that cuts the agent's path through via, a role
// viaRole returned (nil only if the catalog changed between reads, or for
// pg_database_owner, whose members are the database's owner with no grant to
// revoke).
func revokeVia(via *string, agentID, target string) string {
	if via == nil && target == "pg_database_owner" {
		return fmt.Sprintf("%s owns the database, which makes it a member of pg_database_owner: "+
			"ALTER DATABASE <this database> OWNER TO <a role the agent is not a member of>", agentID)
	}
	if via == nil {
		return fmt.Sprintf("revoke the membership of %s that leads to %q", agentID, target)
	}
	return fmt.Sprintf("REVOKE %s FROM %s", pgx.Identifier{*via}.Sanitize(), agentID)
}

func (c *checker) agentRole(role string, tables []ownedTable, stetPath, database string, pathErr error) {
	id := pgx.Identifier{role}.Sanitize()
	var (
		oid       uint32
		super     bool
		superRole *string // a superuser role the agent can SET ROLE to
		superVia  *string
	)
	err := c.read(func(tx pgx.Tx) error {
		return tx.QueryRow(c.ctx, `
			select r.oid, r.rolsuper, s.rolname, `+viaRole("r.oid", "s.oid")+`
			from pg_roles r
			left join lateral (
				select s.oid, s.rolname from pg_roles s
				where s.rolsuper and s.oid <> r.oid and pg_has_role(r.oid, s.oid, 'MEMBER')
				order by s.rolname limit 1) s on true
			where r.rolname = $1`, role).Scan(&oid, &super, &superRole, &superVia)
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
	// A superuser passes every right below; one line says enough.
	case super:
		c.warn("agent role %q is a superuser: it can change the gated tables, the stet schema and the server's "+
			"settings around everything Stet judges; fix: ALTER ROLE %s NOSUPERUSER", role, id)
		return
	case superRole != nil:
		// Superuser is not inherited, but SET ROLE reaches it.
		c.warn("agent role %q can become superuser %q with SET ROLE, as a member of it: it can then change the "+
			"gated tables, the stet schema and the server's settings around everything Stet judges; fix: %s",
			role, *superRole, revokeVia(superVia, id, *superRole))
		return
	}

	c.schemaRights(role, id, oid)
	c.createRights(role, id, oid)

	for _, t := range tables {
		var owns bool
		var via *string
		err := c.read(func(tx pgx.Tx) error {
			return tx.QueryRow(c.ctx, "select pg_has_role($1::oid, $2::oid, 'MEMBER'), "+viaRole("$1::oid", "$2::oid"),
				oid, t.ownerOID).Scan(&owns, &via)
		})
		const why = "an owner can alter it, add triggers and grant itself any right outside what Stet judges"
		switch {
		case err != nil:
			c.warn("whether agent role %q owns table %s could not be checked: %v", role, t.key, err)
		case owns && t.ownerOID == oid:
			c.warn("agent role %q owns table %s: "+why+"; fix: ALTER TABLE %s OWNER TO <a role the agent is not "+
				"a member of>", role, t.key, ident(t.key))
		case owns && via != nil && *via != t.owner:
			c.warn("agent role %q owns table %s through membership in its owner %q, reached through role %q: "+why+
				"; fix: %s", role, t.key, t.owner, *via, revokeVia(via, id, t.owner))
		case owns:
			c.warn("agent role %q owns table %s through membership in its owner %q: "+why+"; fix: %s",
				role, t.key, t.owner, revokeVia(via, id, t.owner))
		}
	}

	c.searchPath(role, oid, stetPath, database, pathErr)
}

// schemaRights warns when the agent has a right on the stet schema: granted
// to it, to PUBLIC, or to a role it can SET ROLE to, or through owning the
// schema. Each right is named with the role it is held through, and the fix
// with what removes it.
func (c *checker) schemaRights(role, id string, oid uint32) {
	var (
		found            bool
		owner            string
		isOwner, member  bool
		ownerVia         *string
		rights, fixes    []string
		direct, toPublic []string
	)
	err := c.read(func(tx pgx.Tx) error {
		err := tx.QueryRow(c.ctx, `
			select pg_get_userbyid(n.nspowner), n.nspowner = $1::oid, pg_has_role($1::oid, n.nspowner, 'MEMBER'),
			       `+viaRole("$1::oid", "n.nspowner")+`
			from pg_namespace n where n.nspname = 'stet'`, oid).Scan(&owner, &isOwner, &member, &ownerVia)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		// The owner's own entries are its ownership, reported above.
		rows, err := tx.Query(c.ctx, `
			select a.grantee = 0, a.grantee = $1::oid, coalesce(g.rolname, ''), a.privilege_type,
			       case when a.grantee <> 0 then `+viaRole("$1::oid", "a.grantee")+` end
			from pg_namespace n
			cross join lateral aclexplode(n.nspacl) a
			left join pg_roles g on g.oid = a.grantee
			where n.nspname = 'stet' and a.privilege_type in ('USAGE', 'CREATE') and a.grantee <> n.nspowner
			  and (a.grantee = 0 or pg_has_role($1::oid, a.grantee, 'MEMBER'))
			order by 1 desc, 2 desc, 3, 4`, oid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var public, mine bool
			var grantee, privilege string
			var via *string
			if err := rows.Scan(&public, &mine, &grantee, &privilege, &via); err != nil {
				return err
			}
			switch {
			case public:
				toPublic = append(toPublic, privilege)
				rights = append(rights, privilege+" through PUBLIC")
			case mine:
				direct = append(direct, privilege)
				rights = append(rights, privilege)
			default:
				rights = append(rights, fmt.Sprintf("%s through role %q", privilege, grantee))
				fixes = append(fixes, revokeVia(via, id, grantee))
			}
		}
		return rows.Err()
	})
	switch {
	case err != nil:
		c.warn("agent role %q could not be checked for rights on schema stet: %v", role, err)
		return
	case !found:
		c.warn("agent role %q could not be checked for rights on schema stet: the schema was not found", role)
		return
	}
	if len(direct) > 0 {
		fixes = append(fixes, fmt.Sprintf("REVOKE %s ON SCHEMA stet FROM %s", strings.Join(direct, ", "), id))
	}
	if len(toPublic) > 0 {
		fixes = append(fixes, fmt.Sprintf("REVOKE %s ON SCHEMA stet FROM PUBLIC", strings.Join(toPublic, ", ")))
	}
	switch {
	case isOwner:
		rights = append(rights, "ownership")
		fixes = append(fixes, "ALTER SCHEMA stet OWNER TO <Stet's account>")
	case member:
		rights = append(rights, fmt.Sprintf("its owner's rights through role %q", owner))
		fixes = append(fixes, revokeVia(ownerVia, id, owner))
	}
	if len(rights) == 0 {
		return
	}
	slices.Sort(fixes)
	c.warn("agent role %q has rights on schema stet: %s; it can read and change Stet's decisions and held writes; "+
		"fix: %s", role, strings.Join(rights, ", "), strings.Join(slices.Compact(fixes), "; "))
}

// unseen is why the rights createRights finds matter, and what enforce mode
// does about them.
const unseen = "through which it can write a gated table where no statement Stet judges shows it " +
	"(enforce refuses the statements that define a function, procedure, rule, trigger or a view over a gated " +
	"table on agent sessions, not every object a write can pass through)"

// publicNote follows a fix that revokes a right from PUBLIC.
const publicNote = " (a REVOKE from PUBLIC takes the right from every role: grant it again to the roles that need it)"

// createRights warns when the agent may create objects through which a write
// to a gated table would pass where no statement shows it (ruling 29):
// TEMPORARY on the database (temporary views and functions), CREATE on it
// (schemas of its own to create them in), and CREATE on, or ownership of, a
// schema that is not a system one (the stet schema aside: schemaRights warns
// of it). TEMPORARY is PUBLIC's by default on every PostgreSQL version, and
// CREATE on schema public through PostgreSQL 14. Rights count as
// schemaRights counts them: granted to the agent, to PUBLIC, or to a role it
// can SET ROLE to, NOINHERIT memberships included, or held as an owner. Each
// is named with the role it is held through, and the fix with what removes
// it.
func (c *checker) createRights(role, id string, oid uint32) {
	var (
		database               string
		dbRights, dbFixes      []string
		dbDirect, dbPublic     []string
		nsRights, nsFixes      []string
		publicFix              bool
		dbOwner, dbOwnerMember bool
		dbOwnerName            string
		dbOwnerVia             *string
	)
	err := c.read(func(tx pgx.Tx) error {
		err := tx.QueryRow(c.ctx, `
			select d.datname, d.datdba = $1::oid, pg_has_role($1::oid, d.datdba, 'MEMBER'), pg_get_userbyid(d.datdba),
			       `+viaRole("$1::oid", "d.datdba")+`
			from pg_database d where d.datname = current_database()`, oid).
			Scan(&database, &dbOwner, &dbOwnerMember, &dbOwnerName, &dbOwnerVia)
		if err != nil {
			return err
		}
		// The owner's own entries are its ownership, reported apart.
		rows, err := tx.Query(c.ctx, `
			select a.privilege_type, a.grantee = 0, a.grantee = $1::oid, coalesce(g.rolname, ''),
			       case when a.grantee not in (0, $1::oid) then `+viaRole("$1::oid", "a.grantee")+` end
			from pg_database d
			cross join lateral aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) a
			left join pg_roles g on g.oid = a.grantee
			where d.datname = current_database() and a.privilege_type in ('TEMPORARY', 'CREATE')
			  and a.grantee <> d.datdba and (a.grantee = 0 or pg_has_role($1::oid, a.grantee, 'MEMBER'))
			order by 2 desc, 3 desc, 4, 1 desc`, oid)
		if err != nil {
			return err
		}
		for rows.Next() {
			var public, mine bool
			var privilege, grantee string
			var via *string
			if err := rows.Scan(&privilege, &public, &mine, &grantee, &via); err != nil {
				rows.Close()
				return err
			}
			switch {
			case public:
				dbPublic = append(dbPublic, privilege)
				dbRights = append(dbRights, privilege+" through PUBLIC")
			case mine:
				dbDirect = append(dbDirect, privilege)
				dbRights = append(dbRights, privilege)
			default:
				dbRights = append(dbRights, fmt.Sprintf("%s through role %q", privilege, grantee))
				dbFixes = append(dbFixes, revokeVia(via, id, grantee))
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		// Schemas: the system ones, pg_* and information_schema, take no
		// agent's objects; stet is schemaRights'.
		const userSchema = `n.nspname not like 'pg\_%' and n.nspname not in ('information_schema', 'stet')`
		rows, err = tx.Query(c.ctx, `
			select n.nspname, a.grantee = 0, a.grantee = $1::oid, coalesce(g.rolname, ''),
			       case when a.grantee not in (0, $1::oid) then `+viaRole("$1::oid", "a.grantee")+` end
			from pg_namespace n
			cross join lateral aclexplode(coalesce(n.nspacl, acldefault('n', n.nspowner))) a
			left join pg_roles g on g.oid = a.grantee
			where `+userSchema+` and a.privilege_type = 'CREATE'
			  and a.grantee <> n.nspowner and (a.grantee = 0 or pg_has_role($1::oid, a.grantee, 'MEMBER'))
			order by 1, 2 desc, 3 desc, 4`, oid)
		if err != nil {
			return err
		}
		for rows.Next() {
			var public, mine bool
			var schema, grantee string
			var via *string
			if err := rows.Scan(&schema, &public, &mine, &grantee, &via); err != nil {
				rows.Close()
				return err
			}
			on := pgx.Identifier{schema}.Sanitize()
			switch {
			case public:
				nsRights = append(nsRights, fmt.Sprintf("CREATE on schema %s through PUBLIC", on))
				nsFixes = append(nsFixes, fmt.Sprintf("REVOKE CREATE ON SCHEMA %s FROM PUBLIC", on))
				publicFix = true
			case mine:
				nsRights = append(nsRights, "CREATE on schema "+on)
				nsFixes = append(nsFixes, fmt.Sprintf("REVOKE CREATE ON SCHEMA %s FROM %s", on, id))
			default:
				nsRights = append(nsRights, fmt.Sprintf("CREATE on schema %s through role %q", on, grantee))
				nsFixes = append(nsFixes, revokeVia(via, id, grantee))
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(c.ctx, `
			select n.nspname, n.nspowner = $1::oid, pg_get_userbyid(n.nspowner),
			       case when n.nspowner <> $1::oid then `+viaRole("$1::oid", "n.nspowner")+` end
			from pg_namespace n
			where `+userSchema+` and pg_has_role($1::oid, n.nspowner, 'MEMBER')
			order by 1`, oid)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var mine bool
			var schema, owner string
			var via *string
			if err := rows.Scan(&schema, &mine, &owner, &via); err != nil {
				return err
			}
			on := pgx.Identifier{schema}.Sanitize()
			if mine {
				nsRights = append(nsRights, "ownership of schema "+on)
				nsFixes = append(nsFixes, fmt.Sprintf("ALTER SCHEMA %s OWNER TO <a role the agent is not a member of>", on))
				continue
			}
			nsRights = append(nsRights, fmt.Sprintf("ownership of schema %s through role %q", on, owner))
			nsFixes = append(nsFixes, revokeVia(via, id, owner))
		}
		return rows.Err()
	})
	if err != nil {
		c.warn("agent role %q could not be checked for rights to create objects (TEMPORARY or CREATE on the "+
			"database, CREATE on its schemas): %v", role, err)
		return
	}

	db := pgx.Identifier{database}.Sanitize()
	if len(dbDirect) > 0 {
		dbFixes = append(dbFixes, fmt.Sprintf("REVOKE %s ON DATABASE %s FROM %s", strings.Join(dbDirect, ", "), db, id))
	}
	if len(dbPublic) > 0 {
		dbFixes = append(dbFixes, fmt.Sprintf("REVOKE %s ON DATABASE %s FROM PUBLIC", strings.Join(dbPublic, ", "), db))
	}
	switch {
	case dbOwner:
		dbRights = append(dbRights, "ownership")
		dbFixes = append(dbFixes, fmt.Sprintf("ALTER DATABASE %s OWNER TO <a role the agent is not a member of>", db))
	case dbOwnerMember:
		dbRights = append(dbRights, fmt.Sprintf("its owner's rights through role %q", dbOwnerName))
		dbFixes = append(dbFixes, revokeVia(dbOwnerVia, id, dbOwnerName))
	}
	if len(dbRights) > 0 {
		c.warn("agent role %q may create objects in database %s: %s; it can create temporary views and functions "+
			"(TEMPORARY), or schemas of its own to create them in (CREATE), "+unseen+"; fix: %s",
			role, db, strings.Join(dbRights, ", "), fixes(dbFixes, len(dbPublic) > 0))
	}
	if len(nsRights) > 0 {
		c.warn("agent role %q may create objects in schemas: %s; a view, function, rule or trigger there is an "+
			"object "+unseen+"; fix: %s", role, strings.Join(nsRights, ", "), fixes(nsFixes, publicFix))
	}
}

// fixes joins the statements that remove what a warning names, sorted and
// each once, and says what a REVOKE from PUBLIC costs when public is set.
func fixes(statements []string, public bool) string {
	slices.Sort(statements)
	s := strings.Join(slices.Compact(statements), "; ")
	if public {
		s += publicNote
	}
	return s
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

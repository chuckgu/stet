package bench

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stetdb/stet/internal/lab/tasks"
)

func TestDBName(t *testing.T) {
	for _, c := range []struct {
		arm      string
		practice bool
		x, rerun int
		want     string
	}{
		{"none", false, 0, 1, "crm_none"}, {"stet", false, 0, 1, "crm_gate"}, {"stet-bind", false, 0, 1, "crm_gate_bind"},
		{"hoop-cache", false, 0, 1, "crm_hoop_cache"}, {"stet", false, 1, 1, "crm_gate_x"}, {"hoop", false, 2, 1, "crm_hoop_x2"},
		{"hoop", false, 1, 2, "crm_hoop_r2_x"}, {"hoop-a", true, 0, 1, "crm_practice_hoop_a"},
	} {
		got := DBName(c.arm, c.practice, c.x, c.rerun)
		if got != c.want {
			t.Errorf("DBName(%q, %v, %d, %d) = %q, want %q", c.arm, c.practice, c.x, c.rerun, got, c.want)
		}
		if strings.Contains(got, "stet") {
			t.Errorf("DBName(%q, ...) = %q names stet", c.arm, got)
		}
	}
	if got := Template(false); got != "crm_base" {
		t.Errorf("Template(false) = %q, want crm_base", got)
	}
	if got := Template(true); got != "crm_practice" {
		t.Errorf("Template(true) = %q, want crm_practice", got)
	}
}

// wantCode runs sql in a transaction that is rolled back and wants it to
// fail with code.
func wantCode(t *testing.T, conn *pgx.Conn, who, sql, code string) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, sql)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != code {
		t.Errorf("%s: %q: err = %v, want SQLSTATE %s", who, sql, err, code)
	}
}

// inRollback runs sqls in one transaction that is rolled back and wants
// each to succeed.
func inRollback(t *testing.T, conn *pgx.Conn, who string, sqls ...string) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, sql := range sqls {
		if _, err := tx.Exec(ctx, sql); err != nil {
			t.Errorf("%s: %q: %v", who, sql, err)
			return
		}
	}
}

func TestAgentsGetOnlyWhatTheSpecAllows(t *testing.T) {
	a, n, _, _ := testTemplate(t)
	ctx := context.Background()
	gated, plain := testClone(t, true), testClone(t, false)

	for _, role := range n.Agents() {
		conn := connectAs(t, a, role, AgentPassword, gated)
		inRollback(t, conn, role,
			"select count(*) from contacts",
			"insert into contacts (account_id, name, email) values (1, 'Probe Person', 'probe@example.com')",
			"update contacts set notes = 'probe' where id = -1",
			"delete from contacts where id = -1",
			"select count(*) from deals",
			"insert into deals (account_id, stage, amount) values (1, 'open', 1000)",
			"update deals set amount = 1 where id = -1",
			"delete from deals where id = -1",
		)
		wantCode(t, conn, role, "truncate deals", "42501")
		wantCode(t, conn, role, "select 1 from lab.writes", "42501")
		wantCode(t, conn, role, "alter table contacts add column z int", "42501")
	}

	xconn := connectAs(t, a, n.Role(tasks.RoleX), AgentPassword, gated)
	inRollback(t, xconn, n.Role(tasks.RoleX),
		"select set_consent(-1)",
		"call mark_consent(-1)",
		"select refresh_notes(-2, -1)",
		"update contact_prefs set notes = 'probe' where id = -1",
	)

	su := connectAs(t, a, a.User, a.Password, "postgres")
	for _, role := range n.Agents() {
		var member bool
		if err := su.QueryRow(ctx, "select pg_has_role($1, $2, 'MEMBER')", n.Gate(), role).Scan(&member); err != nil {
			t.Fatal(err)
		}
		if !member {
			t.Errorf("%s is not a member of %s", n.Gate(), role)
		}
	}
	var super bool
	if err := su.QueryRow(ctx, "select rolsuper from pg_roles where rolname = $1", n.Gate()).Scan(&super); err != nil {
		t.Fatal(err)
	}
	if super {
		t.Errorf("%s is a superuser", n.Gate())
	}
	for _, db := range []string{gated, plain} {
		var owner string
		var owned int
		if err := su.QueryRow(ctx, "select datdba::regrole::text from pg_database where datname = $1", db).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if owner != n.Owner() {
			t.Errorf("%s is owned by %s, want %s", db, owner, n.Owner())
		}
		if err := su.QueryRow(ctx, `select count(*) from pg_shdepend s join pg_database d on d.oid = s.dbid
			where d.datname = $1 and s.deptype = 'o' and s.refobjid = (select oid from pg_roles where rolname = $2)`,
			db, n.Gate()).Scan(&owned); err != nil {
			t.Fatal(err)
		}
		if owned != 0 {
			t.Errorf("%s owns %d objects in %s", n.Gate(), owned, db)
		}
	}
	for db, want := range map[string]bool{gated: true, plain: false} {
		var got bool
		if err := su.QueryRow(ctx, "select has_database_privilege($1, $2, 'CREATE')", n.Gate(), db).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s has CREATE on %s: %v, want %v", n.Gate(), db, got, want)
		}
	}
}

// sharedState is what the preparation must not change outside its own
// databases and roles.
type sharedState struct {
	datacl, nspacl string
	roles          map[string]string // name -> super, createdb, createrole, config
	members        [][4]string       // role, member, admin option, grantor
}

func readShared(t *testing.T, a Admin) sharedState {
	t.Helper()
	ctx := context.Background()
	su := connectAs(t, a, a.User, a.Password, "stet_test")
	s := sharedState{roles: map[string]string{}}
	if err := su.QueryRow(ctx, "select coalesce(datacl::text, '') from pg_database where datname = 'stet_test'").Scan(&s.datacl); err != nil {
		t.Fatal(err)
	}
	if err := su.QueryRow(ctx, "select coalesce(nspacl::text, '') from pg_namespace where nspname = 'public'").Scan(&s.nspacl); err != nil {
		t.Fatal(err)
	}
	rows, err := su.Query(ctx, `select rolname, format('%s %s %s %s', rolsuper, rolcreatedb, rolcreaterole, coalesce(rolconfig::text, ''))
		from pg_roles`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, attrs string
		if err := rows.Scan(&name, &attrs); err != nil {
			t.Fatal(err)
		}
		s.roles[name] = attrs
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	rows, err = su.Query(ctx, `select r.rolname, m.rolname, am.admin_option::text, coalesce(g.rolname, '')
		from pg_auth_members am join pg_roles r on r.oid = am.roleid join pg_roles m on m.oid = am.member
		left join pg_roles g on g.oid = am.grantor`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var m [4]string
		if err := rows.Scan(&m[0], &m[1], &m[2], &m[3]); err != nil {
			t.Fatal(err)
		}
		s.members = append(s.members, m)
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	return s
}

func TestPrepareLeavesSharedObjectsAlone(t *testing.T) {
	a := testAdmin(t)
	ctx := context.Background()
	_, _, _, b := testTemplate(t)
	d, _ := testDataset()

	before := readShared(t, a)
	n := testNames(t)
	ns := time.Now().UnixNano()
	tmpl, clone := fmt.Sprintf("crm_t%d_base", ns), fmt.Sprintf("crm_t%d_c", ns)
	t.Cleanup(func() {
		for _, db := range []string{clone, tmpl} {
			if err := Drop(context.Background(), a, db); err != nil {
				t.Error(err)
			}
		}
	})
	su := connectAs(t, a, a.User, a.Password, "postgres")
	if err := CreateRoles(ctx, su, n); err != nil {
		t.Fatal(err)
	}
	if err := CreateTemplate(ctx, a, n, tmpl, d, b, nil); err != nil {
		t.Fatal(err)
	}
	if err := Clone(ctx, a, n, tmpl, clone, true); err != nil {
		t.Fatal(err)
	}
	if err := Drop(ctx, a, clone); err != nil {
		t.Fatal(err)
	}
	after := readShared(t, a)

	if after.datacl != before.datacl {
		t.Errorf("stet_test's datacl: %s, was %s", after.datacl, before.datacl)
	}
	if after.nspacl != before.nspacl {
		t.Errorf("stet_test's schema public: nspacl %s, was %s", after.nspacl, before.nspacl)
	}
	for _, r := range []string{"postgres", "stet_scram", "stet_md5", "stet_clear", "stet_trust", "stet_agent"} {
		if _, ok := after.roles[r]; !ok {
			t.Errorf("role %s is gone", r)
		}
	}
	// Other packages' tests make and drop roles of their own meanwhile: a
	// role that existed before is compared while it still exists, and a
	// membership row is compared when it names a role that existed before
	// and every role it names is such a role or one of this test's.
	for r, was := range before.roles {
		if now, ok := after.roles[r]; ok && now != was {
			t.Errorf("role %s: super, createdb, createrole, config = %s, was %s", r, now, was)
		}
	}
	old := func(r string) bool {
		_, was := before.roles[r]
		_, is := after.roles[r]
		return was && is
	}
	known := func(r string) bool { return old(r) || strings.HasPrefix(r, n.Prefix) }
	rows := func(s sharedState) []string {
		var out []string
		for _, m := range s.members {
			if (old(m[0]) || old(m[1])) && known(m[0]) && known(m[1]) && (m[3] == "" || known(m[3])) {
				out = append(out, strings.Join(m[:], " "))
			}
		}
		slices.Sort(out)
		return out
	}
	if was, is := rows(before), rows(after); !slices.Equal(was, is) {
		t.Errorf("memberships of existing roles changed:\nbefore %q\nafter  %q", was, is)
	}
}

func TestForeignNamesAreRefused(t *testing.T) {
	a, n, tmpl, b := testTemplate(t)
	ctx := context.Background()
	d, _ := testDataset()
	ns := time.Now().UnixNano()
	// No call names a database that exists as something to drop or create:
	// if a guard broke, the test fails instead of dropping a shared database.
	foreign, foreignClone, foreignTmpl := fmt.Sprintf("nocrm_%d", ns), fmt.Sprintf("nocrm_%d_c", ns), fmt.Sprintf("nocrm_%d_base", ns)
	clone := fmt.Sprintf("crm_t%d_c", ns)
	t.Cleanup(func() { Drop(context.Background(), a, clone) })

	if err := Drop(ctx, a, foreign); err == nil {
		t.Errorf("Drop(%s) = nil, want an error", foreign)
	}
	if err := Clone(ctx, a, n, tmpl, foreignClone, false); err == nil {
		t.Errorf("Clone(..., %s) = nil, want an error", foreignClone)
	}
	// template1 exists, is not the benchmark's, and is only read as a
	// template: without the guard the clone would be made from it.
	if err := Clone(ctx, a, n, "template1", clone, false); err == nil {
		t.Error("Clone from template1 = nil, want an error")
	}
	if err := CreateTemplate(ctx, a, n, foreignTmpl, d, b, nil); err == nil {
		t.Errorf("CreateTemplate(%s) = nil, want an error", foreignTmpl)
	}

	su := connectAs(t, a, a.User, a.Password, "postgres")
	for db, want := range map[string]bool{"stet_test": true, foreign: false, foreignClone: false, foreignTmpl: false, clone: false} {
		var got bool
		if err := su.QueryRow(ctx, "select exists (select 1 from pg_database where datname = $1)", db).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("database %s exists: %v, want %v", db, got, want)
		}
	}
}

func TestCloneWhoseGrantFailsIsDropped(t *testing.T) {
	a, _, tmpl, _ := testTemplate(t)
	ctx := context.Background()
	n := testNames(t)
	clone := fmt.Sprintf("crm_t%d_c", time.Now().UnixNano())
	t.Cleanup(func() {
		if err := Drop(context.Background(), a, clone); err != nil {
			t.Error(err)
		}
	})
	// The owner exists and the gate account does not, so the database is
	// made and the grant fails.
	su := connectAs(t, a, a.User, a.Password, "postgres")
	if _, err := su.Exec(ctx, fmt.Sprintf("create role %s login password '%s'", ident(n.Owner()), OwnerPassword)); err != nil {
		t.Fatal(err)
	}
	if err := Clone(ctx, a, n, tmpl, clone, true); err == nil {
		t.Fatal("Clone with a gate account that does not exist = nil, want an error")
	}
	var exists bool
	if err := su.QueryRow(ctx, "select exists (select 1 from pg_database where datname = $1)", clone).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Errorf("%s exists after its grant failed", clone)
	}
}

func TestStreamRoundTrips(t *testing.T) {
	a, n, _, b := testTemplate(t)
	conn := connectAs(t, a, n.Owner(), OwnerPassword, testClone(t, false))
	stream, attacks, err := LoadStream(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Stream) == 0 || len(b.Attacks) == 0 {
		t.Fatalf("the bench has %d statements and %d attacks; the test needs both", len(b.Stream), len(b.Attacks))
	}
	if len(stream) != len(b.Stream) {
		t.Fatalf("LoadStream returned %d statements, want %d", len(stream), len(b.Stream))
	}
	for i := range stream {
		if !reflect.DeepEqual(stream[i], b.Stream[i]) {
			t.Fatalf("statement %d:\n got %+v\nwant %+v", i, stream[i], b.Stream[i])
		}
	}
	if !reflect.DeepEqual(attacks, b.Attacks) {
		t.Errorf("LoadStream's attacks differ from the bench's (%d, want %d)", len(attacks), len(b.Attacks))
	}
}

func TestISetChangesTheRowsItWasBuiltFor(t *testing.T) {
	a, n, _, b := testTemplate(t)
	ctx := context.Background()
	conn := connectAs(t, a, n.Owner(), OwnerPassword, testClone(t, false))
	seen := 0
	for _, s := range b.Stream {
		if s.Set != tasks.SetI {
			continue
		}
		seen++
		switch {
		case s.Kind == "small" && s.Rows > 100, s.Kind == "large" && s.Rows <= 100:
			t.Errorf("%s: %s side changes %d rows", s.ID, s.Kind, s.Rows)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tag, err := tx.Exec(ctx, s.Inline)
		if err != nil {
			t.Errorf("%s: %v", s.ID, err)
		} else if tag.RowsAffected() != int64(s.Rows) {
			t.Errorf("%s: %q changed %d rows, want %d", s.ID, s.Inline, tag.RowsAffected(), s.Rows)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if seen == 0 {
		t.Fatal("the bench has no I statements")
	}
}

var (
	insertRE = regexp.MustCompile(`^insert into contacts \(([^)]*)\) values \(([^)]*)\)$`)
	updateRE = regexp.MustCompile(`^update contacts set .* where id = \$(\d+)$`)
)

// rowKey says how to find the row a V statement with Args writes: by email
// for an insert into contacts, by id for an update of one contact.
func rowKey(s tasks.Stmt) (insert bool, key string, ok bool) {
	if m := insertRE.FindStringSubmatch(s.SQL); m != nil {
		cols, vals := strings.Split(m[1], ", "), strings.Split(m[2], ", ")
		i := slices.Index(cols, "email")
		if i < 0 || len(vals) != len(cols) || !strings.HasPrefix(vals[i], "$") {
			return false, "", false
		}
		k, err := strconv.Atoi(vals[i][1:])
		if err != nil || k < 1 || k > len(s.Args) {
			return false, "", false
		}
		return true, s.Args[k-1], true
	}
	if m := updateRE.FindStringSubmatch(s.SQL); m != nil {
		k, _ := strconv.Atoi(m[1])
		if k < 1 || k > len(s.Args) {
			return false, "", false
		}
		return false, s.Args[k-1], true
	}
	return false, "", false
}

func TestInlineAndBindWriteTheSameRows(t *testing.T) {
	a, n, _, b := testTemplate(t)
	ctx := context.Background()
	conn := connectAs(t, a, n.Owner(), OwnerPassword, testClone(t, false))

	// write sends s one way in a transaction that is rolled back and returns
	// the row it wrote. An insert's id comes from the sequence and its
	// updated_at from now(), so neither is compared.
	write := func(s tasks.Stmt, bind bool) map[string]any {
		t.Helper()
		insert, key, _ := rowKey(s)
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if bind {
			params := make([][]byte, len(s.Args))
			for i, v := range s.Args {
				params[i] = []byte(v)
			}
			if _, err := tx.Conn().PgConn().ExecParams(ctx, s.SQL, params, nil, nil, nil).Close(); err != nil {
				t.Fatalf("%s bound: %v", s.ID, err)
			}
		} else if _, err := tx.Exec(ctx, s.Inline); err != nil {
			t.Fatalf("%s inline: %v", s.ID, err)
		}
		q := "select to_jsonb(c) from contacts c where id = $1::int"
		if insert {
			q = "select to_jsonb(c) from contacts c where email = $1 order by id desc limit 1"
		}
		var row map[string]any
		if err := tx.QueryRow(ctx, q, key).Scan(&row); err != nil {
			t.Fatalf("%s: read the row back: %v", s.ID, err)
		}
		if insert {
			delete(row, "id")
			delete(row, "updated_at")
		}
		return row
	}

	inserts, updates := 0, 0
	for _, s := range b.Stream {
		if s.Set != tasks.SetV || len(s.Args) == 0 {
			continue
		}
		insert, _, ok := rowKey(s)
		switch {
		case !ok:
			continue
		case insert && inserts < 20:
			inserts++
		case !insert && updates < 20:
			updates++
		default:
			continue
		}
		inline, bound := write(s, false), write(s, true)
		if !maps.EqualFunc(inline, bound, func(x, y any) bool { return reflect.DeepEqual(x, y) }) {
			t.Errorf("%s: inline wrote %v, bound wrote %v", s.ID, inline, bound)
		}
	}
	if inserts < 20 || updates < 20 {
		t.Errorf("compared %d inserts and %d updates, want 20 of each", inserts, updates)
	}
}

func TestExtraRowsKeepEmptyFieldsNull(t *testing.T) {
	a, n, _, b := testTemplate(t)
	ctx := context.Background()
	conn := connectAs(t, a, n.Owner(), OwnerPassword, testClone(t, false))

	var empty int
	if err := conn.QueryRow(ctx, "select count(*) from contacts where consent_source = '' or notes = ''").Scan(&empty); err != nil {
		t.Fatal(err)
	}
	if empty != 0 {
		t.Errorf("%d contacts have an empty consent_source or notes instead of NULL", empty)
	}
	ids, wantNull := []int{}, 0
	for _, c := range b.Extra.Contacts {
		ids = append(ids, c.ID)
		if c.ConsentSource == "" && c.Notes == "" {
			wantNull++
		}
	}
	var gotNull int
	if err := conn.QueryRow(ctx, "select count(*) from contacts where id = any($1) and consent_source is null and notes is null",
		ids).Scan(&gotNull); err != nil {
		t.Fatal(err)
	}
	if gotNull != wantNull || wantNull == 0 {
		t.Errorf("%d extra contacts have neither consent_source nor notes, want %d", gotNull, wantNull)
	}

	// The F4 and 300-row probes count what their payload does to the
	// snapshot's rows: nothing before it, more than the limit after.
	step := map[string]tasks.Stmt{}
	for _, s := range b.Stream {
		step[s.ID] = s
	}
	probe := func(tx pgx.Tx, sql string) int64 {
		var v int64
		if err := tx.QueryRow(ctx, sql).Scan(&v); err != nil {
			t.Fatalf("probe %q: %v", sql, err)
		}
		return v
	}
	checked := 0
	for _, x := range b.Attacks {
		if x.Payload != "F4" && x.Payload != "rows300" || len(x.Steps) != 1 {
			continue
		}
		s := step[x.Steps[0]]
		if s.Wire != tasks.WireSimple && s.Wire != tasks.WireBind || s.Pad > 0 {
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if v := probe(tx, x.Probe); v != 0 {
			t.Errorf("%s (%s): the probe reads %d before the payload, want 0", x.Path, s.ID, v)
		}
		if _, err := tx.Exec(ctx, s.Inline); err != nil {
			t.Errorf("%s (%s): %v", x.Path, s.ID, err)
		} else if v := probe(tx, x.Probe); v <= x.Limit {
			t.Errorf("%s (%s): the probe reads %d after the payload, want more than %d", x.Path, s.ID, v, x.Limit)
		}
		tx.Rollback(ctx)
		checked++
	}
	if checked == 0 {
		t.Error("no F4 or 300-row attack was checked")
	}
}

func TestNewRowsGetIDsAfterTheLoadedOnes(t *testing.T) {
	a, n, _, b := testTemplate(t)
	ctx := context.Background()
	conn := connectAs(t, a, n.Role(tasks.RoleV), AgentPassword, testClone(t, false))

	loaded := map[string]int{}
	for _, x := range b.Extra.Accounts {
		loaded["accounts"] = max(loaded["accounts"], x.ID)
	}
	for _, x := range b.Extra.Contacts {
		loaded["contacts"] = max(loaded["contacts"], x.ID)
	}
	for _, x := range b.Extra.Deals {
		loaded["deals"] = max(loaded["deals"], x.ID)
	}
	for table, insert := range map[string]string{
		"accounts":   "insert into accounts (name, domain, industry) values ('Probe', 'probe.example', 'Testing') returning id",
		"contacts":   "insert into contacts (account_id, name, email) values (1, 'Probe Person', 'probe@example.com') returning id",
		"deals":      "insert into deals (account_id, stage, amount) values (1, 'open', 1000) returning id",
		"activities": "insert into activities (contact_id, kind, body) values (1, 'call', 'probe') returning id",
	} {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var top, id int
		if err := tx.QueryRow(ctx, "select max(id) from "+table).Scan(&top); err != nil {
			t.Fatal(err)
		}
		if top < loaded[table] {
			t.Errorf("%s: highest id %d, the extra rows go up to %d", table, top, loaded[table])
		}
		if err := tx.QueryRow(ctx, insert).Scan(&id); err != nil {
			t.Errorf("%s: %v", table, err)
		} else if id <= top {
			t.Errorf("%s: a new row got id %d, the loaded rows go up to %d", table, id, top)
		}
		tx.Rollback(ctx)
	}
}

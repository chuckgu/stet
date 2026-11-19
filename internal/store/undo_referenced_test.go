package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests pin how stet.undo decides that restoring a key would cascade
// into rows another party made (spec-v0.2-undo 7, referenced): it compares as
// the foreign key's own cascade does, with the constraint's equality operator,
// both sides cast to that operator's input types, under the collation the
// cascade uses, the referenced values taken from the row undo has locked; it
// refuses a cascading foreign key between columns of different types, under a
// nondeterministic collation, or through an operator it cannot pin, rather
// than follow it; it reads the foreign keys only once it holds the table, so
// that no cascading foreign key can be added before its statement runs; and
// it checks, after reading a child table, that the table it read is the
// foreign key's.

// A child that the foreign key itself takes as referencing the agent's row is
// found as the cascade compares: citext's own equality ('A@X' references
// 'a@x'); a domain over text on the referenced side and on the referencing
// side; varchar(10) referenced by varchar(20) (text equality); int[] by int[]
// (array equality); a referenced column in "C" referenced by one in the
// default collation, and by one in "POSIX" (two collations neither of which
// is the default: the probe names the cascade's own, else its two columns
// would conflict). Each such row is skipped, referenced, and its child stays;
// a row of the same table without a child is restored.
func TestUndoProbesAsTheForeignKeyCompares(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create extension citext`,
		`create domain public.code_d as text`,
		`create table public.users_ci (id int primary key, email public.citext unique)`,
		`create table public.notes_ci (id int primary key, email public.citext references public.users_ci (email) on delete cascade)`,
		`create table public.codes (id int primary key, code public.code_d unique)`,
		`create table public.code_refs (id int primary key, code text references public.codes (code) on delete cascade)`,
		`create table public.plain_codes (id int primary key, code text unique)`,
		`create table public.dom_refs (id int primary key, code public.code_d references public.plain_codes (code) on delete cascade)`,
		`create table public.vp (id int primary key, code varchar(10) unique)`,
		`create table public.vc (id int primary key, code varchar(20) references public.vp (code) on delete cascade)`,
		`create table public.ap (id int primary key, tags int[] unique)`,
		`create table public.ac (id int primary key, tags int[] references public.ap (tags) on delete cascade)`,
		`create table public.cp (id int primary key, code text collate "C" unique)`,
		`create table public.cc (id int primary key, code text references public.cp (code) on delete cascade)`,
		`create table public.cp2 (id int primary key, code text collate "C" unique)`,
		`create table public.cc2 (id int primary key, code text collate "POSIX" references public.cp2 (code) on delete cascade)`)
	parents := []string{"users_ci", "codes", "plain_codes", "vp", "ap", "cp", "cp2"}
	for _, table := range parents {
		installCapture(t, owner, "public."+table, table, "id")
	}
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`insert into public.users_ci values (1, 'a@x'), (2, 'b@x')`,
		`insert into public.codes values (1, 'k1'), (2, 'k2')`,
		`insert into public.plain_codes values (1, 'p1'), (2, 'p2')`,
		`insert into public.vp values (1, 'v1'), (2, 'v2')`,
		`insert into public.ap values (1, '{1,2}'), (2, '{3}')`,
		`insert into public.cp values (1, 'c1'), (2, 'c2')`,
		`insert into public.cp2 values (1, 'd1'), (2, 'd2')`)
	children := []string{"public.notes_ci", "public.code_refs", "public.dom_refs", "public.vc", "public.ac", "public.cc", "public.cc2"}
	mustExec(t, writerIn(t, db, children...),
		`insert into public.notes_ci values (10, 'A@X')`,
		`insert into public.code_refs values (10, 'k1')`,
		`insert into public.dom_refs values (10, 'p1')`,
		`insert into public.vc values (10, 'v1')`,
		`insert into public.ac values (10, '{1,2}')`,
		`insert into public.cc values (10, 'c1')`,
		`insert into public.cc2 values (10, 'd1')`)
	kept := map[string]string{}
	for _, child := range children {
		kept[child] = contentOf(t, owner, child)
	}

	var tables []string
	for _, table := range parents {
		tables = append(tables, "public."+table)
	}
	rv := undoer(t, db, tables...)
	var granted []string
	for _, child := range children {
		granted = append(granted, "grant select on "+child+" to "+roleOf(t, rv))
	}
	grant(t, db, roleOf(t, rv), granted...)
	checkUndo(t, "children as the cascade compares", undoOf(t, rv, "stet_md5", since),
		`ap "1" - skipped referenced: public.ac -`,
		`ap "2" delete restored - -`,
		`codes "1" - skipped referenced: public.code_refs -`,
		`codes "2" delete restored - -`,
		`cp "1" - skipped referenced: public.cc -`,
		`cp "2" delete restored - -`,
		`cp2 "1" - skipped referenced: public.cc2 -`,
		`cp2 "2" delete restored - -`,
		`plain_codes "1" - skipped referenced: public.dom_refs -`,
		`plain_codes "2" delete restored - -`,
		`users_ci "1" - skipped referenced: public.notes_ci -`,
		`users_ci "2" delete restored - -`,
		`vp "1" - skipped referenced: public.vc -`,
		`vp "2" delete restored - -`)
	for child, want := range kept {
		checkContent(t, owner, "after the undo", child, want)
	}
}

// A cascading foreign key whose comparison involves a nondeterministic
// collation is not followed (spec 7): the cascade then compares
// case-insensitively, and the rows it reaches need not reference the agent's
// row, so a child can appear between the probe and the restore. The key whose
// restore would fire it is failed with 0A000 naming the constraint and the
// child table, and nothing is written. The collation counts wherever it comes
// in: the referenced column's (with a "C" child), the referencing column's
// (with a default parent, on delete; with a "C" parent, on update), both, the
// referencing column's type's only (a domain in ci whose column is "C"), and
// the referenced column's type's only (a domain in ci whose column is "C",
// which is the case where PostgreSQL's own cascade deletes a child of another
// parent row). Two different deterministic collations neither of which is the
// default, between which the cascade itself cannot choose, are refused too.
func TestUndoRefusesACascadeUnderANondeterministicCollation(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create collation public.ci (provider = icu, locale = 'und-u-ks-level2', deterministic = false)`,
		`create domain public.ci_text as text collate public.ci`,
		`create domain public.c_text as text collate "C"`,
		`create table public.p_ci (id int primary key, name text collate public.ci unique)`,
		`create table public.k_c (id int primary key, name text collate "C" references public.p_ci (name) on delete cascade)`,
		`create table public.p_t (id int primary key, name text unique)`,
		`create table public.k_ci (id int primary key, name text collate public.ci references public.p_t (name) on delete cascade)`,
		`create table public.p_c (id int primary key, name text collate "C" unique)`,
		`insert into public.p_c values (2, 'z')`,
		`create table public.k_ciu (id int primary key, name text collate public.ci references public.p_c (name) on update cascade)`,
		`create table public.p_cici (id int primary key, name text collate public.ci unique)`,
		`create table public.k_cici (id int primary key, name text collate public.ci references public.p_cici (name) on delete cascade)`,
		`create table public.p_q (id int primary key, name text unique)`,
		`create table public.k_q (id int primary key, name public.ci_text collate "C" references public.p_q (name) on delete cascade)`,
		`create table public.p_p (id int primary key, name public.ci_text collate "C" unique)`,
		`create table public.k_p (id int primary key, name text references public.p_p (name) on delete cascade)`,
		`create table public.p_conf (id int primary key, name public.c_text unique)`,
		`create table public.k_conf (id int primary key, name text collate "POSIX" references public.p_conf (name) on delete cascade)`)
	parents := []string{"p_ci", "p_t", "p_c", "p_cici", "p_q", "p_p", "p_conf"}
	for _, table := range parents {
		installCapture(t, owner, "public."+table, table, "id")
	}
	children := []string{"public.k_c", "public.k_ci", "public.k_ciu", "public.k_cici", "public.k_q", "public.k_p", "public.k_conf"}
	writer := writerIn(t, db, append(children, "public.p_t", "public.p_c", "public.p_p")...)
	mustExec(t, writer,
		`insert into public.p_t values (1, 'A')`,
		`insert into public.p_c values (1, 'A')`,
		`insert into public.p_p values (1, 'A')`)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`insert into public.p_ci values (1, 'a')`,
		`insert into public.p_t values (2, 'a')`,
		`update public.p_c set name = 'a' where id = 2`,
		`insert into public.p_cici values (1, 'a')`,
		`insert into public.p_q values (1, 'a')`,
		`insert into public.p_p values (2, 'a')`,
		`insert into public.p_conf values (1, 'x')`)
	mustExec(t, writer,
		`insert into public.k_c values (10, 'A')`,
		`insert into public.k_ci values (10, 'A')`,
		`insert into public.k_ciu values (10, 'A')`,
		`insert into public.k_cici values (10, 'A')`,
		`insert into public.k_q values (10, 'a')`,
		`insert into public.k_p values (10, 'A')`,
		`insert into public.k_conf values (10, 'x')`)
	kept := map[string]string{}
	for _, table := range children {
		kept[table] = contentOf(t, owner, table)
	}
	for _, table := range parents {
		kept["public."+table] = contentOf(t, owner, "public."+table)
	}

	var tables []string
	for _, table := range parents {
		tables = append(tables, "public."+table)
	}
	rv := undoer(t, db, tables...)
	var granted []string
	for _, child := range children {
		granted = append(granted, "grant select on "+child+" to "+roleOf(t, rv))
	}
	grant(t, db, roleOf(t, rv), granted...)
	nondeterministic := func(constraint, child string) string {
		return "SQLSTATE 0A000: the cascading foreign key " + constraint + " of " + child +
			" compares under the nondeterministic collation public.ci; undo does not follow a cascade under a nondeterministic collation"
	}
	checkUndo(t, "cascades under a nondeterministic collation", undoOf(t, rv, "stet_md5", since),
		`p_c "2" update failed `+nondeterministic("k_ciu_name_fkey", "public.k_ciu")+` -`,
		`p_ci "1" delete failed `+nondeterministic("k_c_name_fkey", "public.k_c")+` -`,
		`p_cici "1" delete failed `+nondeterministic("k_cici_name_fkey", "public.k_cici")+` -`,
		`p_conf "1" delete failed SQLSTATE 0A000: the cascading foreign key k_conf_name_fkey of public.k_conf compares under the collations pg_catalog."C" and pg_catalog."POSIX", between which PostgreSQL cannot choose; undo does not follow such a cascade -`,
		`p_p "2" delete failed `+nondeterministic("k_p_name_fkey", "public.k_p")+` -`,
		`p_q "1" delete failed `+nondeterministic("k_q_name_fkey", "public.k_q")+` -`,
		`p_t "2" delete failed `+nondeterministic("k_ci_name_fkey", "public.k_ci")+` -`)
	for table, want := range kept {
		checkContent(t, owner, "after the undo", table, want)
	}
}

// The probe names the constraint's own operator and casts both sides to its
// input types, as PostgreSQL's cascade writes it, so an operator of the same
// name that the agent adds in the operator's schema for the columns' own
// types is never chosen. The referenced column is a domain over citext, the
// referencing one citext, and the other way round; the agent, which may
// create in the schema citext is in, adds =(email_d, citext) and
// =(citext, email_d) that say false and record who called them. Each row is
// skipped, referenced, the children stay, and the agent's functions never
// ran.
func TestUndoProbeIsNotShadowedByAnOperator(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create schema ext`,
		`create extension citext schema ext`,
		`grant create on schema ext to stet_md5`,
		`create domain public.email_d as ext.citext`,
		`create table public.pe (id int primary key, email public.email_d unique)`,
		`create table public.pe_refs (id int primary key, email ext.citext references public.pe (email) on delete cascade)`,
		`create table public.pf (id int primary key, email ext.citext unique)`,
		`create table public.pf_refs (id int primary key, email public.email_d references public.pf (email) on delete cascade)`,
		`create table public.calls (who text)`)
	installCapture(t, owner, "public.pe", "pe", "id")
	installCapture(t, owner, "public.pf", "pf", "id")
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	mustExec(t, agent, `insert into public.pe values (1, 'a@x')`, `insert into public.pf values (1, 'b@x')`)
	writer := writerIn(t, db, "public.pe_refs", "public.pf_refs")
	grant(t, db, roleOf(t, writer), "grant usage on schema ext to "+roleOf(t, writer))
	mustExec(t, writer, `insert into public.pe_refs values (10, 'A@X')`, `insert into public.pf_refs values (10, 'B@X')`)
	mustExec(t, agent,
		`create function ext.agent_eq(public.email_d, ext.citext) returns boolean language plpgsql
		 as $$ begin insert into public.calls values (current_user); return false; end $$`,
		`create operator ext.= (leftarg = public.email_d, rightarg = ext.citext, function = ext.agent_eq)`,
		`create function ext.agent_eq(ext.citext, public.email_d) returns boolean language plpgsql
		 as $$ begin insert into public.calls values (current_user); return false; end $$`,
		`create operator ext.= (leftarg = ext.citext, rightarg = public.email_d, function = ext.agent_eq)`)

	rv := undoer(t, db, "public.pe", "public.pf")
	name := roleOf(t, rv)
	grant(t, db, name, "grant usage on schema ext to "+name, "grant select on public.pe_refs, public.pf_refs to "+name,
		"grant insert on public.calls to "+name)
	checkUndo(t, "operators the agent added", undoOf(t, rv, "stet_md5", since),
		`pe "1" - skipped referenced: public.pe_refs -`,
		`pf "1" - skipped referenced: public.pf_refs -`)
	checkContent(t, owner, "the other party's child", "public.pe_refs", "(10,A@X)")
	checkContent(t, owner, "the other party's child", "public.pf_refs", "(10,B@X)")
	checkContent(t, owner, "the calls of the agent's operator", "public.calls", "")
}

// The probe reads the foreign keys only while this transaction holds ROW
// EXCLUSIVE on the table relid names. A session holds the table in SHARE
// mode, so the EXPLAIN that takes the lock waits; meanwhile the session swaps
// two schemas' names, so that the name in the restore statement now means
// another table of the same shape, and commits. PostgreSQL then locks that
// other table instead. stet.undo sees that it does not hold ROW EXCLUSIVE on
// relid and fails the key with 55000; no row of either table is deleted.
func TestUndoRefusesWhenItsStatementLockedAnotherTable(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create schema crm`, `create schema crm2`,
		`create table crm.deals (id int primary key, note text, n int)`,
		`create table crm2.deals (id int primary key, note text, n int)`,
		`insert into crm2.deals values (2, 'theirs', 2)`)
	installCapture(t, owner, "crm.deals", "deals", "id")
	relid := relidOf(t, owner, "crm.deals")
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `insert into crm.deals values (2, 'r2', 2)`)
	rv := undoer(t, db, "crm.deals", "crm2.deals")
	grant(t, db, roleOf(t, rv), "grant usage on schema crm, crm2 to "+roleOf(t, rv))
	mustExec(t, rv, `set lock_timeout = '10s'`)

	tx, err := superuserIn(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mustExec(t, tx, `lock table crm.deals in share mode`)
	rows, err := undoWhileWaiting(t, owner, rv, func() {
		mustExec(t, tx, `alter schema crm rename to crm_old`, `alter schema crm2 rename to crm`)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}, "stet_md5", since)
	if err != nil {
		t.Fatal(err)
	}
	checkUndo(t, "a restore statement whose table name was taken while it waited", rows,
		`deals "2" delete failed SQLSTATE 55000: the restore statement for the table with oid `+fmt.Sprint(relid)+
			` locked another table: its name was given to another table while it waited for the lock -`)
	checkContent(t, owner, "the table relid names", "crm_old.deals", "(2,r2,2)")
	checkContent(t, owner, "the table the name means now", "crm.deals", "(2,theirs,2)")
}

// A cascading foreign key between columns of different types (char(3) and
// text, timestamptz and timestamp, integer and bigint) is not probed: the key
// whose restore would fire it is failed with 0A000 naming the constraint and
// the child table, and nothing is written. A cross-type foreign key that does
// not cascade is left to PostgreSQL's own check (23503, or restored when
// nothing references the row), and an update that does not set the
// referenced column does not fire the foreign key and is restored.
func TestUndoRefusesACascadeBetweenTypes(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create table public.bp (id int primary key, code char(3) unique)`,
		`create table public.bp_refs (id int primary key, code text references public.bp (code) on delete cascade)`,
		`create table public.tz (id int primary key, at timestamptz unique)`,
		`create table public.tz_refs (id int primary key, at timestamp references public.tz (at) on delete cascade)`,
		`create table public.wide (id int primary key, n int unique)`,
		`create table public.wide_refs (id int primary key, n bigint references public.wide (n) on delete cascade)`,
		`create table public.loose (id int primary key, n int unique)`,
		`create table public.loose_refs (id int primary key, n bigint references public.loose (n))`,
		`create table public.upd (id int primary key, n int unique, note text)`,
		`insert into public.upd values (1, 1, 'n1'), (2, 2, 'n2')`,
		`create table public.upd_refs (id int primary key, n bigint references public.upd (n) on update cascade)`)
	for _, table := range []string{"bp", "tz", "wide", "loose", "upd"} {
		installCapture(t, owner, "public."+table, table, "id")
	}
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`insert into public.bp values (1, 'ab')`,
		`insert into public.tz values (1, '2026-01-01 00:00:00+00')`,
		`insert into public.wide values (1, 1)`,
		`insert into public.loose values (1, 1)`,
		`insert into public.loose values (2, 2)`,
		`update public.upd set note = 'changed' where id = 1`,
		`update public.upd set n = 20 where id = 2`)
	children := []string{"public.bp_refs", "public.tz_refs", "public.wide_refs", "public.loose_refs", "public.upd_refs"}
	other := writerIn(t, db, children...)
	mustExec(t, other,
		`set timezone = 'Asia/Seoul'`,
		`insert into public.bp_refs values (10, 'ab')`,
		`insert into public.tz_refs values (10, '2026-01-01 09:00:00')`,
		`insert into public.wide_refs values (10, 1)`,
		`insert into public.loose_refs values (10, 1)`,
		`insert into public.upd_refs values (10, 1), (20, 20)`)
	kept := map[string]string{}
	for _, table := range append(children, "public.bp", "public.tz", "public.wide") {
		kept[table] = contentOf(t, owner, table)
	}

	rv := undoer(t, db, "public.bp", "public.tz", "public.wide", "public.loose", "public.upd")
	var granted []string
	for _, c := range children {
		granted = append(granted, "grant select on "+c+" to "+roleOf(t, rv))
	}
	grant(t, db, roleOf(t, rv), granted...)
	refused := func(constraint, child, parentType, childType string) string {
		return "SQLSTATE 0A000: the cascading foreign key " + constraint + " of " + child + " compares " + parentType +
			" with " + childType + "; undo does not follow a cascade between columns of different types"
	}
	checkUndo(t, "cascades between types", undoOf(t, rv, "stet_md5", since),
		`bp "1" delete failed `+refused("bp_refs_code_fkey", "public.bp_refs", "character", "text")+` -`,
		`loose "1" delete failed SQLSTATE 23503: update or delete on table "loose" violates foreign key constraint "loose_refs_n_fkey" on table "loose_refs" -`,
		`loose "2" delete restored - -`,
		`tz "1" delete failed `+refused("tz_refs_at_fkey", "public.tz_refs", "timestamp with time zone", "timestamp without time zone")+` -`,
		`upd "1" update restored - -`,
		`upd "2" update failed `+refused("upd_refs_n_fkey", "public.upd_refs", "integer", "bigint")+` -`,
		`wide "1" delete failed `+refused("wide_refs_n_fkey", "public.wide_refs", "integer", "bigint")+` -`)
	for table, want := range kept {
		checkContent(t, owner, "after the undo", table, want)
	}
	checkContent(t, owner, "after the undo", "public.loose", "(1,1)")
	checkContent(t, owner, "after the undo", "public.upd", "(1,1,n1)\n(2,20,n2)")
}

// stet.undo takes ROW EXCLUSIVE on the table it restores before it reads the
// foreign keys that reference it. A session that holds the table in SHARE
// mode makes the undo wait for that lock; while it waits, that session adds an
// ON DELETE CASCADE foreign key from a table whose row references the agent's
// row, and commits. The undo then reads the new foreign key and skips the row
// as referenced; read before the lock, it would have missed it and its delete
// would have cascaded into the other party's row.
func TestUndoLocksTheTableBeforeReadingItsForeignKeys(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "parents", 1)
	mustExec(t, owner, `create table public.kids (id int primary key, parent int)`)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `insert into public.parents values (2, 'r2', 2)`)
	mustExec(t, writerIn(t, db, "public.kids"), `insert into public.kids values (10, 2)`)
	rv := undoer(t, db, "public.parents")
	grant(t, db, roleOf(t, rv), "grant select on public.kids to "+roleOf(t, rv))
	mustExec(t, rv, `set lock_timeout = '10s'`)

	tx, err := superuserIn(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mustExec(t, tx, `lock table public.parents in share mode`)
	rows, err := undoWhileWaiting(t, owner, rv, func() {
		mustExec(t, tx, `alter table public.kids add foreign key (parent) references public.parents on delete cascade`)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}, "stet_md5", since)
	if err != nil {
		t.Fatal(err)
	}
	checkUndo(t, "a foreign key added while the undo waited for the table", rows,
		`parents "2" - skipped referenced: public.kids -`)
	checkContent(t, owner, "the other party's child", "public.kids", "(10,2)")
	checkContent(t, owner, "the parents", "public.parents", "(1,r1,1)\n(2,r2,2)")
}

// Row-level security that comes to apply to the reviewer on a child table
// while the undo waits to read it (another session enables it and adds a
// policy that hides every row, and commits) would hide the child from the
// probe, while the cascade, which row-level security does not stop, would
// still reach it. stet.undo checks row-level security again after the probe:
// the key is failed with 42501 and the child stays.
func TestUndoChecksRowLevelSecurityAfterTheProbe(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	keyedTable(t, owner, "parents", 1)
	mustExec(t, owner, `create table public.kids (id int primary key, parent int references public.parents on delete cascade)`)
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `insert into public.parents values (2, 'r2', 2)`)
	mustExec(t, writerIn(t, db, "public.kids"), `insert into public.kids values (10, 2)`)
	rv := undoer(t, db, "public.parents")
	name := roleOf(t, rv)
	grant(t, db, name, "grant select on public.kids to "+name)
	mustExec(t, rv, `set lock_timeout = '10s'`)

	tx, err := superuserIn(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mustExec(t, tx, `alter table public.kids enable row level security`, `create policy nobody on public.kids using (false)`)
	rows, err := undoWhileWaiting(t, owner, rv, func() {
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}, "stet_md5", since)
	if err != nil {
		t.Fatal(err)
	}
	checkUndo(t, "row-level security enabled while the undo waited for the child", rows,
		`parents "2" delete failed SQLSTATE 42501: row-level security on public.kids applies to `+name+` -`)
	checkContent(t, owner, "the other party's child", "public.kids", "(10,2)")
	checkContent(t, owner, "the parents", "public.parents", "(1,r1,1)\n(2,r2,2)")
}

// kidsInASchema creates public.parents with capture, lets the agent insert
// the rows 2 and 3 into it, and creates two tables named kid: s.kid, whose
// row 10 references the agent's row ref through an ON DELETE CASCADE foreign
// key, and s2.kid, which references nothing and is empty. If partitioned,
// both are partitioned tables with one partition, kid_1. It returns since,
// s.kid's oid, and a reviewer that may undo the agent's rows and read both
// kids, with a lock_timeout of 10s. Whoever swaps the two schemas' names
// makes the name s.kid mean the empty table. stet.undo tries row 3, inserted
// later, before row 2.
func kidsInASchema(t *testing.T, pool *pgxpool.Pool, db string, owner *pgx.Conn, ref int, partitioned bool) (time.Time, uint32, *pgx.Conn) {
	t.Helper()
	keyedTable(t, owner, "parents", 1)
	mustExec(t, owner, `create schema s`, `create schema s2`)
	if partitioned {
		mustExec(t, owner,
			`create table s.kid (id int, parent int references public.parents on delete cascade) partition by range (id)`,
			`create table s.kid_1 partition of s.kid for values from (0) to (100)`,
			`create table s2.kid (id int, parent int) partition by range (id)`,
			`create table s2.kid_1 partition of s2.kid for values from (0) to (100)`)
	} else {
		mustExec(t, owner,
			`create table s.kid (id int primary key, parent int references public.parents on delete cascade)`,
			`create table s2.kid (id int primary key, parent int)`)
	}
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db), `insert into public.parents values (2, 'r2', 2)`, `insert into public.parents values (3, 'r3', 3)`)
	mustExec(t, owner, fmt.Sprintf(`insert into s.kid values (10, %d)`, ref))
	rv := undoer(t, db, "public.parents")
	name := roleOf(t, rv)
	grant(t, db, name, "grant usage on schema s, s2 to "+name, "grant select on all tables in schema s, s2 to "+name)
	mustExec(t, rv, `set lock_timeout = '10s'`)
	return since, relidOf(t, owner, "s.kid"), rv
}

// movedOn is the reason of a key whose probe named the child table with oid
// kid as s.kid when that name no longer was its own.
func movedOn(kid uint32) string {
	return "SQLSTATE 55000: the table with oid " + fmt.Sprint(kid) +
		", which references public.parents, is no longer named s.kid: its name was given to another table while it was read"
}

// The probe names the child table in SQL, and a name can be given to another
// table while the probe waits for the child's lock. A session holds s.kid in
// ACCESS EXCLUSIVE mode, so the probe of row 3 waits; meanwhile the session
// swaps the two schemas' names and commits. PostgreSQL then reads the empty
// table that s.kid now names, and the probe finds no child. stet.undo sees
// that the name it used is no longer that of the foreign key's table, and
// that it holds no lock on that table, and fails the key with 55000: the
// agent's row is not deleted, so nothing cascades into the real child. Row 2,
// probed after the swap under the child's new name, is restored.
func TestUndoRefusesWhenItsProbeReadAnotherTable(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	since, kid, rv := kidsInASchema(t, pool, db, owner, 3, false)

	tx, err := superuserIn(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mustExec(t, tx, `lock table s.kid in access exclusive mode`)
	rows, err := undoWhileWaiting(t, owner, rv, func() {
		mustExec(t, tx, `alter schema s rename to s_old`, `alter schema s2 rename to s`)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}, "stet_md5", since)
	if err != nil {
		t.Fatal(err)
	}
	checkUndo(t, "a child table whose name was taken while the probe waited", rows,
		`parents "2" delete restored - -`,
		`parents "3" delete failed `+movedOn(kid)+` -`)
	checkContent(t, owner, "the real child", "s_old.kid", "(10,3)")
	checkContent(t, owner, "the table the name means now", "s.kid", "")
	checkContent(t, owner, "the parents", "public.parents", "(1,r1,1)\n(3,r3,3)")
}

// The name is checked on its own, also when this transaction does hold the
// foreign key's table. The child is a partitioned table: the probe of row 3
// resolves s.kid, locks it, and then waits for its partition, which a session
// holds in ACCESS EXCLUSIVE mode. That session swaps the schemas' names and
// commits. The probe has read the real child and the transaction holds its
// lock, but s.kid now names another table: the key is failed with 55000 and
// nothing is written.
func TestUndoRefusesWhenTheProbedNameMovedOn(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	since, kid, rv := kidsInASchema(t, pool, db, owner, 3, true)

	tx, err := superuserIn(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mustExec(t, tx, `lock table s.kid_1 in access exclusive mode`)
	rows, err := undoWhileWaiting(t, owner, rv, func() {
		if n := countOf(t, owner, `select count(*) from pg_locks where pid = $1 and relation = $2 and granted`,
			int64(rv.PgConn().PID()), kid); n == 0 {
			t.Errorf("the probe waits without holding s.kid")
		}
		mustExec(t, tx, `alter schema s rename to s_old`, `alter schema s2 rename to s`)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}, "stet_md5", since)
	if err != nil {
		t.Fatal(err)
	}
	checkUndo(t, "a child table whose name moved on while the probe read it", rows,
		`parents "2" delete restored - -`,
		`parents "3" delete failed `+movedOn(kid)+` -`)
	checkContent(t, owner, "the real child", "s_old.kid", "(10,3)")
	checkContent(t, owner, "the parents", "public.parents", "(1,r1,1)\n(3,r3,3)")
}

// The lock is checked as well as the name, for the name can come back. The
// probe of row 3 waits for s.kid, which one session holds; that session swaps
// the schemas' names and commits, and PostgreSQL lets the real child go and
// takes the table the name now means. The probe then waits for that table's
// partition, which a second session holds; that session swaps the names back
// and commits. The probe reads the empty table and finds no child, and s.kid
// is the real child's name again. But this transaction does not hold the real
// child: the key is failed with 55000 and nothing cascades into it.
func TestUndoRefusesWhenTheNameCameBackAfterTheProbe(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	since, kid, rv := kidsInASchema(t, pool, db, owner, 3, true)
	decoyPart := relidOf(t, owner, "s2.kid_1")

	first, err := superuserIn(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(ctx)
	mustExec(t, first, `lock table s.kid in access exclusive mode`)
	second, err := superuserIn(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(ctx)
	mustExec(t, second, `lock table s2.kid_1 in access exclusive mode`)

	rows, err := undoWhileWaiting(t, owner, rv, func() {
		mustExec(t, first, `alter schema s rename to s_old`, `alter schema s2 rename to s`)
		if err := first.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		// The probe now reads the other table, and waits for its partition.
		deadline := time.Now().Add(5 * time.Second)
		for countOf(t, owner, `select count(*) from pg_locks where pid = $1 and relation = $2 and not granted`,
			int64(rv.PgConn().PID()), decoyPart) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the probe did not wait for the partition of the table s.kid now names")
			}
			time.Sleep(10 * time.Millisecond)
		}
		mustExec(t, second, `alter schema s rename to s2`, `alter schema s_old rename to s`)
		if err := second.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}, "stet_md5", since)
	if err != nil {
		t.Fatal(err)
	}
	checkUndo(t, "a child table whose name went and came back while the probe read another", rows,
		`parents "2" delete restored - -`,
		`parents "3" delete failed SQLSTATE 55000: reading s.kid, which references public.parents, locked another table than the one with oid `+
			fmt.Sprint(kid)+`: its name was given to another table while it waited for the lock -`)
	checkContent(t, owner, "the real child", "s.kid", "(10,3)")
	checkContent(t, owner, "the parents", "public.parents", "(1,r1,1)\n(3,r3,3)")
}

// An earlier key of the same undo has probed the real child, so this
// transaction holds its lock, when the schemas' names are swapped. Row 3 has
// no child and is restored; the undo then waits for row 2, which a session
// holds; that session swaps the names, commits, and lets the row go. A
// transaction that holds the child's lock never waits for it again, so
// nothing can be made to happen between the name row 2's probe writes and
// its use: the probe names the child as it is called now, finds the child
// row and skips the key. Nothing cascades into the real child.
func TestUndoProbesTheRealChildAfterAnEarlierKeyLockedIt(t *testing.T) {
	pool, db := captureDB(t)
	ctx := context.Background()
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	since, kid, rv := kidsInASchema(t, pool, db, owner, 2, false)

	tx, err := superuserIn(t, db).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	mustExec(t, tx, `select from public.parents where id = 2 for update`)
	rows, err := undoWhileWaiting(t, owner, rv, func() {
		if n := countOf(t, owner, `select count(*) from pg_locks where pid = $1 and relation = $2 and granted`,
			int64(rv.PgConn().PID()), kid); n == 0 {
			t.Errorf("the undo waits for row 2 without holding s.kid from row 3's probe")
		}
		mustExec(t, owner, `alter schema s rename to s_old`, `alter schema s2 rename to s`)
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}, "stet_md5", since)
	if err != nil {
		t.Fatal(err)
	}
	checkUndo(t, "names swapped after an earlier key locked the child", rows,
		`parents "2" - skipped referenced: s_old.kid -`,
		`parents "3" delete restored - -`)
	checkContent(t, owner, "the real child", "s_old.kid", "(10,2)")
	checkContent(t, owner, "the parents", "public.parents", "(1,r1,1)\n(2,r2,2)")
}

// A foreign key whose equality operator is not PostgreSQL's own and is
// declared on a pseudo-type is not followed. Casting an operand to anyarray
// leaves it the type it is, so OPERATOR(ext.=) between two int[] columns
// would rather be an operator ext.=(int[], int[]) than the constraint's
// ext.=(anyarray, anyarray): whoever may create in that schema could have
// the probe run its function as the reviewer and answer that there is no
// child. The key whose restore would fire such a foreign key is failed with
// 0A000, child or no child, and the agent's function never runs.
func TestUndoRefusesACascadeThroughAPseudoTypeOperator(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create schema ext`,
		`grant usage, create on schema ext to stet_md5`,
		`create operator ext.= (leftarg = anyarray, rightarg = anyarray, function = pg_catalog.array_eq, commutator = operator(ext.=))`,
		`create operator class ext.array_ops for type anyarray using btree as
			operator 1 pg_catalog.<(anyarray, anyarray), operator 2 pg_catalog.<=(anyarray, anyarray),
			operator 3 ext.=(anyarray, anyarray),
			operator 4 pg_catalog.>=(anyarray, anyarray), operator 5 pg_catalog.>(anyarray, anyarray),
			function 1 pg_catalog.btarraycmp(anyarray, anyarray)`,
		`create table public.ap (id int primary key, tags int[])`,
		`create unique index ap_tags on public.ap (tags ext.array_ops)`,
		`create table public.ac (id int primary key, tags int[] references public.ap (tags) on delete cascade)`,
		`create table public.calls (who text)`)
	installCapture(t, owner, "public.ap", "ap", "id")
	since := dbClock(t, pool)
	agent := agentIn(t, db)
	mustExec(t, agent, `insert into public.ap values (1, '{1,2}')`, `insert into public.ap values (2, '{3}')`)
	mustExec(t, writerIn(t, db, "public.ac"), `insert into public.ac values (10, '{1,2}')`)
	mustExec(t, agent,
		`create function ext.agent_eq(int[], int[]) returns boolean language plpgsql
		 as $$ begin insert into public.calls values (current_user); return false; end $$`,
		`create operator ext.= (leftarg = int[], rightarg = int[], function = ext.agent_eq)`)

	rv := undoer(t, db, "public.ap")
	name := roleOf(t, rv)
	grant(t, db, name, "grant usage on schema ext to "+name, "grant select on public.ac to "+name, "grant insert on public.calls to "+name)
	const refused = `SQLSTATE 0A000: the cascading foreign key ac_tags_fkey of public.ac compares with the operator ` +
		`ext.=(anyarray, anyarray), which is not in pg_catalog and takes a pseudo-type; undo does not follow such a cascade`
	checkUndo(t, "a pseudo-type operator outside pg_catalog", undoOf(t, rv, "stet_md5", since),
		`ap "1" delete failed `+refused+` -`,
		`ap "2" delete failed `+refused+` -`)
	checkContent(t, owner, "the other party's child", "public.ac", "(10,\"{1,2}\")")
	checkContent(t, owner, "the agent's rows", "public.ap", "(1,\"{1,2}\")\n(2,{3})")
	checkContent(t, owner, "the calls of the agent's operator", "public.calls", "")
}

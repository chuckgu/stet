package store_test

import (
	"context"
	"testing"
)

// These tests pin how stet.undo decides that restoring a key would cascade
// into rows another party made (spec-v0.2-undo 7, referenced; plan ruling
// R20): it compares as the foreign key itself does, with the constraint's own
// equality operator and the referenced column's collation, the referenced
// values taken from the row undo has locked; it refuses a cascading foreign
// key between columns of different types rather than guess the casts; and it
// reads the foreign keys only once it holds the table so that no cascading
// foreign key can be added before its statement runs.

// A child that the foreign key itself takes as referencing the agent's row is
// found as the foreign key compares: citext's own equality ('A@X' references
// 'a@x'), a case-insensitive ICU collation on the referenced column with "C"
// on the referencing one, and a domain over the same type on one side. Each
// such row is skipped, referenced, and the children stay; a row without a
// child is restored.
func TestUndoProbesAsTheForeignKeyCompares(t *testing.T) {
	pool, db := captureDB(t)
	owner := superuserIn(t, db)
	setCapture(t, pool, []string{"stet_md5"}, 10000)
	mustExec(t, owner,
		`create extension citext`,
		`create collation public.ci (provider = icu, locale = 'und-u-ks-level2', deterministic = false)`,
		`create domain public.code_d as text`,
		`create table public.users_ci (id int primary key, email public.citext unique)`,
		`create table public.notes_ci (id int primary key, email public.citext references public.users_ci (email) on delete cascade)`,
		`create table public.users_icu (id int primary key, name text collate public.ci unique)`,
		`create table public.notes_icu (id int primary key, name text collate "C" references public.users_icu (name) on delete cascade)`,
		`create table public.codes (id int primary key, code public.code_d unique)`,
		`create table public.code_refs (id int primary key, code text references public.codes (code) on delete cascade)`)
	for _, table := range []string{"users_ci", "users_icu", "codes"} {
		installCapture(t, owner, "public."+table, table, "id")
	}
	since := dbClock(t, pool)
	mustExec(t, agentIn(t, db),
		`insert into public.users_ci values (1, 'a@x')`,
		`insert into public.users_icu values (1, 'a')`,
		`insert into public.codes values (1, 'k1')`,
		`insert into public.codes values (2, 'k2')`)
	children := []string{"public.notes_ci", "public.notes_icu", "public.code_refs"}
	mustExec(t, writerIn(t, db, children...),
		`insert into public.notes_ci values (10, 'A@X')`,
		`insert into public.notes_icu values (10, 'A')`,
		`insert into public.code_refs values (10, 'k1')`)

	rv := undoer(t, db, "public.users_ci", "public.users_icu", "public.codes")
	grant(t, db, roleOf(t, rv), "grant select on public.notes_ci, public.notes_icu, public.code_refs to "+roleOf(t, rv))
	checkUndo(t, "children as the foreign key compares", undoOf(t, rv, "stet_md5", since),
		`codes "1" - skipped referenced: public.code_refs -`,
		`codes "2" delete restored - -`,
		`users_ci "1" - skipped referenced: public.notes_ci -`,
		`users_icu "1" - skipped referenced: public.notes_icu -`)
	checkContent(t, owner, "the citext parents", "public.users_ci", "(1,a@x)")
	checkContent(t, owner, "the citext children", "public.notes_ci", "(10,A@X)")
	checkContent(t, owner, "the ICU parents", "public.users_icu", "(1,a)")
	checkContent(t, owner, "the ICU children", "public.notes_icu", "(10,A)")
	checkContent(t, owner, "the domain parents", "public.codes", "(1,k1)")
	checkContent(t, owner, "the domain children", "public.code_refs", "(10,k1)")
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

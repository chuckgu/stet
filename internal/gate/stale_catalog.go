package gate

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/proxy"
)

// keyTypes are the base types a tracked table's key column may have (spec
// correction S1), by OID, and how the proxy reads their binary form. The text
// of any other type may name another row under Stet's own settings than under
// the client's.
var keyTypes = map[uint32]proxy.KeyType{
	21:   proxy.KeyInt2,
	23:   proxy.KeyInt4,
	20:   proxy.KeyInt8,
	2950: proxy.KeyUUID,
	25:   proxy.KeyText, // text
	1043: proxy.KeyText, // varchar
	1042: proxy.KeyText, // bpchar
	19:   proxy.KeyText, // name
}

// tracked is what the catalog says of a table that remembers reads.
type tracked struct {
	oid      uint32
	attr     uint16        // the key column's attribute number
	typ      proxy.KeyType // of the key column's base type
	typeName string        // format_type of the key column, for a cast to it
	base     uint32        // the OID of the key column's base type
}

// staleCatalog is a reading of the catalog: the tables it tracks, by config
// key and by OID.
type staleCatalog struct {
	byKey map[string]tracked
	byOID map[uint32]string
}

// catalogEntry is what one reading of the catalog found of a table with stale.
type catalogEntry struct {
	t      tracked
	ok     bool
	reason string // why the table is not tracked; "" when it is
}

// table is the catalog's entry for the config key table; ok is false when
// the table has no stale or is not tracked.
func (s *stale) table(key string) (t tracked, ok bool) {
	if s == nil {
		return tracked{}, false
	}
	t, ok = s.cat.Load().byKey[key]
	return t, ok
}

// byOID is the catalog's entry for the table with OID oid.
func (s *stale) byOID(oid uint32) (tracked, bool) {
	if s == nil {
		return tracked{}, false
	}
	c := s.cat.Load()
	key, ok := c.byOID[oid]
	if !ok {
		return tracked{}, false
	}
	return c.byKey[key], true
}

// paramType is how a bind value the client gave the type oid (0 for none) is
// read, when the server compares it with t's key column as a lookup of its
// text does: as the key column's type when it has no type or the column's
// own; as itself when both are integers, which compare by value whatever
// their size, or both text or varchar. ok is false otherwise: compared with
// another string type, bpchar drops trailing spaces and name is cut to 63
// bytes, so the value may name a row its text, looked up as the key column's
// type, does not.
func paramType(oid uint32, t tracked) (proxy.KeyType, bool) {
	if oid == 0 || oid == t.base {
		return t.typ, true
	}
	typ, ok := keyTypes[oid]
	integer := func(k proxy.KeyType) bool { return k == proxy.KeyInt2 || k == proxy.KeyInt4 || k == proxy.KeyInt8 }
	text := func(oid uint32) bool { return oid == 25 || oid == 1043 }
	switch {
	case ok && integer(typ) && integer(t.typ):
		return typ, true
	case text(oid) && text(t.base):
		return typ, true
	}
	return 0, false
}

// readCatalog reads what the catalog says of every table with stale, under
// staleCatalogTimeout, on one of Stet's connections, and publishes it for
// Tracked and Observe. A table whose reading fails keeps what was read of it
// before (nothing, the first time): the reading is retried at the next tick.
// Only what changed since the last reading is logged; a table not tracked
// with a warning that names it and says why, never with row data.
func (s *stale) readCatalog() {
	ctx, cancel := context.WithTimeout(s.ctx, staleCatalogTimeout)
	defer cancel()
	keys := slices.Sorted(maps.Keys(s.pks))
	conn, connErr := s.pool.Acquire(ctx)
	if connErr == nil {
		defer conn.Release()
	}
	next := make(map[string]catalogEntry, len(keys))
	for _, key := range keys {
		e, err := catalogEntry{}, connErr
		if err == nil {
			e, err = readTable(ctx, conn, key, s.pks[key])
		}
		if s.ctx.Err() != nil {
			return // the gate is closing: nothing to publish or log
		}
		if err == nil {
			delete(s.failing, key)
			next[key] = e
			continue
		}
		prev, known := s.known[key]
		switch {
		case !known:
			prev = catalogEntry{reason: "its catalog entry could not be read: " + err.Error()}
		case !s.failing[key]:
			s.log.Warn("stale-write detection could not read a table's catalog entry again; what was read before stands",
				"table", key, "err", err)
		}
		s.failing[key] = true
		next[key] = prev
	}
	sameTables(next)
	for _, key := range keys {
		e := next[key]
		prev, known := s.known[key]
		switch {
		case known && e == prev:
		case !e.ok:
			s.log.Warn("stale-write detection does not track a table: its reads are counted, not remembered",
				"table", key, "reason", e.reason)
		case known:
			s.log.Info("stale-write detection tracks a table", "table", key, "oid", e.t.oid, "key_type", e.t.typeName)
		}
	}
	s.known = next
	c := &staleCatalog{byKey: map[string]tracked{}, byOID: map[uint32]string{}}
	for key, e := range next {
		if e.ok {
			c.byKey[key] = e.t
			c.byOID[e.t.oid] = key
		}
	}
	s.cat.Store(c)
}

// sameTables leaves untracked every table another config key resolves to as
// well: a read of it could be remembered under one key and a write to it
// checked under the other, which its own writes do not refresh.
func sameTables(entries map[string]catalogEntry) {
	keysOf := map[uint32][]string{}
	for key, e := range entries {
		if e.ok {
			keysOf[e.t.oid] = append(keysOf[e.t.oid], key)
		}
	}
	for _, keys := range keysOf {
		if len(keys) < 2 {
			continue
		}
		slices.Sort(keys)
		for _, key := range keys {
			others := slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return k == key })
			entries[key] = catalogEntry{reason: "it is the same table as tables." + strings.Join(others, ", tables.")}
		}
	}
}

// catalogQuery reads, for the config key whose quoted name is $1 and its key
// column $2, the table's OID and kind as Stet's search_path resolves it (as
// the gate's own SQL does), the column's number and type name, the column's
// base type (through domains, however nested), and whether a unique index is
// on the column alone, not partial and not on an expression.
const catalogQuery = `
with recursive
  rel as (select c.oid, c.relkind from pg_class c where c.oid = to_regclass($1)),
  col as (select a.attnum, a.atttypid, format_type(a.atttypid, a.atttypmod) as type_name
            from pg_attribute a join rel on a.attrelid = rel.oid
           where a.attname = $2 and a.attnum > 0 and not a.attisdropped),
  base as (select t.oid, t.typtype, t.typbasetype from pg_type t join col on t.oid = col.atttypid
           union all
           select t.oid, t.typtype, t.typbasetype from pg_type t join base b on t.oid = b.typbasetype
            where b.typtype = 'd')
select rel.oid, rel.relkind::text, col.attnum, col.type_name, b.oid, format_type(b.oid, null),
       exists (select from pg_index x
                where x.indrelid = rel.oid and x.indisunique and x.indisvalid
                  and x.indnkeyatts = 1 and x.indkey[0] = col.attnum
                  and x.indpred is null and x.indexprs is null)
  from (select) as one
  left join rel on true
  left join col on true
  left join base b on b.typtype <> 'd'`

// readTable reads what the catalog says of the config key table, keyed by
// pk. An error is a reading that failed; a table that is not tracked is an
// entry with its reason.
func readTable(ctx context.Context, conn *pgxpool.Conn, table, pk string) (catalogEntry, error) {
	var (
		oid, base         *uint32
		relkind, typeName *string
		baseName          *string
		attnum            *int16
		unique            bool
	)
	err := conn.QueryRow(ctx, catalogQuery, ident(table), pk).Scan(&oid, &relkind, &attnum, &typeName, &base, &baseName, &unique)
	if err != nil {
		return catalogEntry{}, err
	}
	name, col := ident(table), pgx.Identifier{pk}.Sanitize()
	var typ proxy.KeyType
	if base != nil {
		typ = keyTypes[*base]
	}
	switch {
	case oid == nil:
		return catalogEntry{reason: "Stet's account finds no table " + name}, nil
	case *relkind != "r" && *relkind != "p":
		return catalogEntry{reason: name + " is not a table"}, nil
	case attnum == nil:
		return catalogEntry{reason: fmt.Sprintf("table %s has no column %s", name, col)}, nil
	case typ == 0:
		return catalogEntry{reason: fmt.Sprintf("its key column %s is of type %s; only keys of type int2, int4, "+
			"int8, uuid, text, varchar, bpchar or name, or a domain over one, are tracked", col, deref(baseName))}, nil
	case !unique:
		return catalogEntry{reason: fmt.Sprintf("column %s of %s is not a key: no unique index is on it alone", col, name)}, nil
	}
	return catalogEntry{ok: true, t: tracked{oid: *oid, attr: uint16(*attnum), typ: typ, typeName: *typeName, base: *base}}, nil
}

package apply

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/store"
)

// RoleSearchPath reads the search_path set on role at the role level: the
// one for the current database (ALTER ROLE ... IN DATABASE), which wins, or
// else the one for all databases (ALTER ROLE ... SET). inDatabase tells which
// it is. path is nil when the role has neither, or does not exist.
//
// Startup applies it to the role's own sessions only, never to a session that
// becomes the role with SET ROLE, so a statement Stet runs as the role on its
// own connection resolves names with Stet's search_path, not this one.
func RoleSearchPath(ctx context.Context, q store.Querier, role string) (path *string, inDatabase bool, err error) {
	err = q.QueryRow(ctx, `
		select s.setdatabase <> 0, substr(c.setting, strpos(c.setting, '=') + 1)
		from pg_db_role_setting s
		cross join lateral unnest(s.setconfig) as c(setting)
		where s.setrole = (select r.oid from pg_roles r where r.rolname = $1)
		  and s.setdatabase in (0, (select d.oid from pg_database d where d.datname = current_database()))
		  and lower(split_part(c.setting, '=', 1)) = 'search_path'
		order by s.setdatabase <> 0 desc
		limit 1`, role).Scan(&inDatabase, &path)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return path, inDatabase, nil
}

// SchemaNames splits a search_path value into the names Postgres reads from
// it: unquoted names fold to lower case, quoted ones keep theirs with ""
// read as ", and whitespace between names does not count.
func SchemaNames(path string) []string {
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

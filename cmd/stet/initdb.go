package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/gate"
)

// maxInitTables is how many tables stet init writes. A variable for tests.
var maxInitTables = 100

// remoteHost reports whether host is another machine. A variable so a test
// can have the test server, which is on this machine, taken for a remote one.
var remoteHost = func(host string) bool { return !config.LoopbackHost(host) }

// What chooseSSLMode answers with (spec-v0.2-tryout 4, step 1).
const (
	errNotPostgresURL = "the database address must start with postgres:// or postgresql://"
	// Not the parser's own error, which quotes the address, password and all.
	errNotURL = "the database address is not a valid URL; percent-encode any @, :, /, ?, # or % in the user name and password"
	// The config's refusal of it (readUpstreamTLS), without its "upstream: ".
	errSSLModeAllow = "sslmode=allow is not supported: it connects in plaintext first, which a relayed session cannot retry; use disable, prefer, require, verify-ca or verify-full"
	errNoTLS        = "the server does not accept TLS; to connect in plaintext anyway, write sslmode=disable in the address yourself"
	poolerHint      = "if this address goes through a connection pooler, use the direct connection address instead"
	noticeRequire   = "the server's certificate could not be checked (%v), so upstream uses sslmode=require: " +
		"the connection is encrypted, but the server is not verified. To verify it, add sslrootcert=<the service's CA file> " +
		"and sslmode=verify-full to upstream in the config file."
)

// chooseSSLMode returns the upstream address stet init writes for the
// address raw it was given, having connected with it through connect, and a
// notice for the user when it settled for less than verify-full ("" else).
//
// An sslmode written in raw is kept, but allow, which the config refuses.
// With none, raw is kept for a host on this machine; for another
// (remoteHost) verify-full is added, or require when the server's
// certificate cannot be verified, with a notice that says so. A server that
// refuses TLS stops it: plaintext to another machine is for the user to
// write. Any other failure is connect's error with a hint about connection
// poolers. The address is never put in an error or a notice: it holds the
// password.
func chooseSSLMode(ctx context.Context, raw string, connect func(ctx context.Context, addr string) error) (upstream, notice string, err error) {
	if !strings.HasPrefix(raw, "postgres://") && !strings.HasPrefix(raw, "postgresql://") {
		return "", "", errors.New(errNotPostgresURL)
	}
	u, err := url.Parse(raw) // as the config reads upstream (validateUpstream, UpstreamAddr)
	if err != nil {
		return "", "", errors.New(errNotURL)
	}
	mode, written := sslModeOf(raw)
	switch {
	case mode == "allow":
		return "", "", errors.New(errSSLModeAllow)
	case written || !remoteHost(u.Hostname()):
		if err := connect(ctx, raw); err != nil {
			return "", "", connectError(err)
		}
		return raw, "", nil
	}
	verified := withSSLMode(raw, "verify-full")
	err = connect(ctx, verified)
	if err == nil {
		return verified, "", nil
	}
	certErr := certificateError(err)
	if certErr == nil {
		return "", "", connectError(err)
	}
	required := withSSLMode(raw, "require")
	if err := connect(ctx, required); err != nil {
		return "", "", connectError(err)
	}
	return required, fmt.Sprintf(noticeRequire, certErr), nil
}

// uriQuery returns the query of the connection URI raw as pgconn reads it,
// like libpq: what follows the first '?' after the user name and password,
// which end at an '@' met before any '/'. ok is false when it has none.
func uriQuery(raw string) (query string, ok bool) {
	_, rest, _ := strings.Cut(raw, "://")
	if i := strings.IndexAny(rest, "@/"); i >= 0 && rest[i] == '@' {
		rest = rest[i+1:]
	}
	_, query, ok = strings.Cut(rest, "?")
	return query, ok
}

// sslModeOf returns the sslmode written in the connection URI raw, the last
// one when it is written more than once, as pgconn reads it; written is
// false when there is none.
func sslModeOf(raw string) (mode string, written bool) {
	query, _ := uriQuery(raw)
	for pair := range strings.SplitSeq(query, "&") {
		key, value, _ := strings.Cut(pair, "=")
		if k, _ := url.PathUnescape(key); k == "sslmode" {
			mode, _ = url.PathUnescape(value)
			written = true
		}
	}
	return mode, written
}

// withSSLMode is raw with sslmode=mode added at the end of its query, every
// other character as it was.
func withSSLMode(raw, mode string) string {
	query, ok := uriQuery(raw)
	switch {
	case !ok:
		return raw + "?sslmode=" + mode
	case query == "" || strings.HasSuffix(query, "&"):
		return raw + "sslmode=" + mode
	}
	return raw + "&sslmode=" + mode
}

// certificateError returns the error in err's chain that says the server's
// certificate could not be verified, the one that says why when there is
// one; nil when there is none.
func certificateError(err error) error {
	var (
		authority x509.UnknownAuthorityError
		host      x509.HostnameError
		invalid   x509.CertificateInvalidError
		verify    *tls.CertificateVerificationError
	)
	switch {
	case errors.As(err, &authority):
		return authority
	case errors.As(err, &host):
		return host
	case errors.As(err, &invalid):
		return invalid
	case errors.As(err, &verify):
		return verify
	}
	return nil
}

// connectError is what chooseSSLMode says when connect failed with err.
func connectError(err error) error {
	if strings.Contains(err.Error(), "server refused TLS connection") { // pgconn's words for a server that answers N
		return errors.New(errNoTLS)
	}
	return fmt.Errorf("%w\n%s", err, poolerHint)
}

// initTable is a table stet init writes in the config.
type initTable struct {
	Name, PK string
	Stale    bool    // the key column's base type is one stale tracks
	Rows     float64 // pg_class.reltuples; -1: never analyzed
}

// initFacts is what stet init reads of the database (readInitFacts).
type initFacts struct {
	VersionNum     int    // server_version_num
	Version        string // server_version
	User, Database string
	SchemaExists   bool           // the stet schema is there
	CanCreate      bool           // User has CREATE on Database
	Tables         []initTable    // chosen, sorted by name
	Skipped        map[string]int // reason → how many tables
	Dropped        int            // chosen but over maxInitTables
}

// initFactsTx opens readInitFacts' transaction. A variable so a test can
// read, in that transaction, that it is read-only.
var initFactsTx = readOnly

// Why a table is not taken (spec-v0.2-tryout 4, step 3; plan correction
// P4), in the order they are checked: a table is skipped for the first that
// applies.
const (
	skipSearchPath  = "not reached by search_path"
	skipNameQuoting = "name needs quoting"
	skipNoKey       = "no single-column primary key"
	skipKeyQuoting  = "key column name needs quoting"
	skipUnreadable  = "not readable by this account"
)

// plainName is a name that reads the same quoted or not, but for keywords,
// which Stet quotes: the config and the SQL Stet prints name a table and its
// key column as they are.
var plainName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// initInfoQuery reads the server, the account and the database.
const initInfoQuery = `
select current_setting('server_version_num')::int, current_setting('server_version'),
       current_user::text, current_database()::text,
       exists (select from pg_namespace where nspname = 'stet'),
       has_database_privilege(current_database(), 'CREATE')`

// initTablesQuery lists the tables stet init may take: ordinary and
// partitioned tables, but not partitions, outside the system schemas, the
// temporary ones and stet's own. Of each it reads its name, its row
// estimate, whether its bare name resolves to it on the account's
// search_path (as Stet resolves a config key), whether the account may read
// it, how many key columns its valid primary key has (NULL with none), and,
// when that is one, the column's name and its base type, through domains
// however nested (as catalogQuery in internal/gate reads it).
const initTablesQuery = `
with recursive
  cand as (select c.oid, c.relname::text as name, c.reltuples::float8 as rows,
                  coalesce(to_regclass(quote_ident(c.relname)) = c.oid, false) as reached,
                  coalesce(has_table_privilege(c.oid, 'SELECT'), false) as readable
             from pg_class c join pg_namespace n on n.oid = c.relnamespace
            where c.relkind in ('r', 'p') and not c.relispartition
              and n.nspname not in ('pg_catalog', 'information_schema', 'pg_toast', 'stet')
              and n.nspname !~ '^pg_(toast_)?temp_'),
  pk as (select x.indrelid as rel, x.indnkeyatts as keys, x.indkey[0] as attnum
           from pg_index x join cand on cand.oid = x.indrelid
          where x.indisprimary and x.indisvalid),
  col as (select pk.rel, a.attname::text as name, a.atttypid
            from pk join pg_attribute a on a.attrelid = pk.rel and a.attnum = pk.attnum
           where pk.keys = 1),
  base as (select col.rel, t.oid, t.typtype, t.typbasetype from col join pg_type t on t.oid = col.atttypid
           union all
           select b.rel, t.oid, t.typtype, t.typbasetype from base b join pg_type t on t.oid = b.typbasetype
            where b.typtype = 'd')
select cand.name, cand.rows, cand.reached, cand.readable, pk.keys, col.name, b.oid
  from cand
  left join pk on pk.rel = cand.oid
  left join col on col.rel = cand.oid
  left join base b on b.rel = cand.oid and b.typtype <> 'd'`

// initCandidate is a row of initTablesQuery.
type initCandidate struct {
	name              string
	rows              float64
	reached, readable bool
	keys              *int16
	pk                *string
	base              *uint32
}

// skip returns why c is not taken, "" when it is.
func (c initCandidate) skip() string {
	switch {
	case !c.reached:
		return skipSearchPath
	case !plainName.MatchString(c.name):
		return skipNameQuoting
	case c.keys == nil || *c.keys != 1 || c.pk == nil:
		return skipNoKey
	case !plainName.MatchString(*c.pk):
		return skipKeyQuoting
	case !c.readable:
		return skipUnreadable
	}
	return ""
}

func (c initCandidate) table() initTable {
	return initTable{Name: c.name, PK: *c.pk, Stale: c.base != nil && gate.TracksKeyType(*c.base), Rows: c.rows}
}

// readInitFacts reads, in one read-only transaction, what stet init needs.
// only, when not empty, names the tables to take; a name that cannot be
// taken is an error that says why, all of them in one error.
//
// Without only, every table that can be taken is, up to maxInitTables of
// them, the ones with the most rows (keepLargest). A name in only is the
// table its bare name resolves to on the account's search_path; with only
// there is no cap. Names in the error are quoted (%q): they come from the
// command line.
func readInitFacts(ctx context.Context, pool *pgxpool.Pool, only []string) (*initFacts, error) {
	f := &initFacts{Skipped: map[string]int{}}
	var cands []initCandidate
	err := initFactsTx(ctx, pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, initInfoQuery).Scan(&f.VersionNum, &f.Version, &f.User, &f.Database, &f.SchemaExists, &f.CanCreate)
		if err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, initTablesQuery)
		cands, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (initCandidate, error) {
			var c initCandidate
			err := row.Scan(&c.name, &c.rows, &c.reached, &c.readable, &c.keys, &c.pk, &c.base)
			return c, err
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("read the database's tables: %w", err)
	}

	if len(only) == 0 {
		for _, c := range cands {
			if why := c.skip(); why != "" {
				f.Skipped[why]++
			} else {
				f.Tables = append(f.Tables, c.table())
			}
		}
		f.Tables, f.Dropped = keepLargest(f.Tables, maxInitTables)
	} else {
		var problems []string
		seen := map[string]bool{}
		for _, name := range only {
			if seen[name] {
				continue
			}
			seen[name] = true
			why := "not found"
			c, found := initCandidateNamed(cands, name)
			if found {
				why = c.skip()
			}
			if why != "" {
				problems = append(problems, fmt.Sprintf("table %q: %s", name, why))
				continue
			}
			f.Tables = append(f.Tables, c.table())
		}
		if len(problems) > 0 {
			return nil, errors.New(strings.Join(problems, "\n"))
		}
	}
	slices.SortFunc(f.Tables, func(a, b initTable) int { return strings.Compare(a.Name, b.Name) })
	return f, nil
}

// initCandidateNamed returns the candidate named name that the bare name
// resolves to, or else another so named (which it does not reach).
func initCandidateNamed(cands []initCandidate, name string) (initCandidate, bool) {
	i := slices.IndexFunc(cands, func(c initCandidate) bool { return c.name == name && c.reached })
	if i < 0 {
		i = slices.IndexFunc(cands, func(c initCandidate) bool { return c.name == name })
	}
	if i < 0 {
		return initCandidate{}, false
	}
	return cands[i], true
}

// keepLargest returns the n tables of ts with the most rows by their
// estimate, of tables with as many the first by name, and how many it left
// out.
func keepLargest(ts []initTable, n int) (kept []initTable, dropped int) {
	if len(ts) <= n {
		return ts, 0
	}
	ts = slices.Clone(ts)
	slices.SortFunc(ts, func(a, b initTable) int {
		if c := cmp.Compare(b.Rows, a.Rows); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return ts[:n], len(ts) - n
}

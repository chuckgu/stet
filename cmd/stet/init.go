package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"

	"github.com/stetdb/stet/internal/config"
)

// What stet init stops with (spec-v0.2-tryout 4).
const (
	errNoAddress   = "no database address: set DATABASE_URL, or pass the address as the last argument"
	errFlagsAfter  = "flags go before the address"
	errNoHost      = "the database address has no host"
	errQueryHost   = "the database address sets host, hostaddr or port in its query; write the host and port in the address itself"
	errHosts       = "the database address lists more than one host; stet relays to one server, so give one host"
	errListen      = "-listen must be host:port with a port from 1 to 65535" // never the value: it may be the address
	errNotTerminal = "this is not a terminal, so stet init cannot ask; read the lines above and pass -yes to continue"
	errStopped     = "stopped; nothing was written"
	errNoTable     = "no table can be watched"
)

// How long stet init waits on the database: for each connection
// chooseSSLMode makes, and for opening Stet's pool and reading the facts.
const (
	initConnectTimeout = 15 * time.Second
	initReadTimeout    = 30 * time.Second
)

// initMaxRows is impact.max_rows of every table stet init writes.
const initMaxRows = 100

// configHeader is the comment at the top of the config stet init writes,
// with stet's version and the day.
const configHeader = `# Written by stet init (stet %s) on %s.
# Shadow mode: stet records what it would do and blocks nothing.
# Every key is described in stet.example.yaml and in docs/spec-v0.1.md, chapter 9.
# This file holds the database password: keep it private.`

// runInit is stet init. env reads an environment variable; interactive says
// whether stdin is a terminal it can ask on.
//
// It reads the database, in a read-only transaction, and writes nothing
// there (spec-v0.2-tryout 4). It writes one file, the config, whole or not
// at all, readable by its owner only: the database password is in it and
// nowhere else, not in what it prints, not in an error. Text the server sent
// is printed with its control characters as spaces (printable): the server
// is not trusted either.
func runInit(args []string, stdin io.Reader, stdout io.Writer, interactive bool, env func(string) string) error {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	path := flags.String("config", "stet.yaml", "the config file to write")
	listen := flags.String("listen", "127.0.0.1:6432", "the address stet proxy listens on, which the agent connects to")
	tables := flags.String("tables", "", fmt.Sprintf("the tables to watch, as a,b,c (default: every table stet can watch, up to %d)", maxInitTables))
	yes := flags.Bool("yes", false, "write the config without asking")
	force := flags.Bool("force", false, "overwrite the config file if it is there")
	if err := flags.Parse(args); err != nil {
		return err
	}

	// The address: the last argument, or else DATABASE_URL.
	var raw string
	switch flags.NArg() {
	case 0:
		raw = env("DATABASE_URL")
	case 1:
		raw = flags.Arg(0)
	default:
		return errors.New(errFlagsAfter)
	}
	switch {
	case raw == "":
		return errors.New(errNoAddress)
	case !listenAddress(*listen):
		return errors.New(errListen)
	}
	if err := checkAddress(raw); err != nil {
		return err
	}

	// A file that is there stops it before it connects.
	if _, err := os.Lstat(*path); err == nil && !*force {
		return alreadyExists(*path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	ctx := context.Background()
	upstream, notice, err := chooseSSLMode(ctx, raw, func(ctx context.Context, addr string) error {
		ctx, cancel := context.WithTimeout(ctx, initConnectTimeout)
		defer cancel()
		pool, err := openPool(ctx, addr)
		if err != nil {
			return err
		}
		pool.Close()
		return nil
	})
	if err != nil {
		return printableError(err)
	}
	if notice != "" {
		fmt.Fprintln(stdout, printable(notice))
	}

	facts, err := initFactsAt(ctx, upstream, tableNames(*tables))
	if err != nil {
		return err
	}
	if err := checkInitFacts(facts); err != nil {
		return err
	}
	file, err := renderConfig(upstream, *listen, facts.Tables, version, time.Now())
	if err != nil {
		return err
	}
	if err := checkConfig(file, upstream); err != nil {
		return err
	}
	agent, err := agentURL(upstream, *listen)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "found %s to watch\n%s", count(len(facts.Tables), "table"), leftOut(facts))
	if !facts.SchemaExists {
		fmt.Fprint(stdout, schemaNotice(facts))
		if !*yes {
			if !interactive {
				return errors.New(errNotTerminal)
			}
			fmt.Fprint(stdout, "Continue? [y/N] ")
			if !confirmed(stdin, stdout) {
				return errors.New(errStopped)
			}
		}
	}

	if err := writeConfigFile(*path, file, *force); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s: %s, shadow mode, no model\n\n"+
		"next:\n  stet proxy -config %s\n\n"+
		"then give the agent this address instead of the database's. The user and password are the same as before:\n  %s\n",
		*path, count(len(facts.Tables), "table"), *path, agent)
	return nil
}

// listenAddress reports whether listen is host:port with a port number.
func listenAddress(listen string) bool {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n > 0
}

// checkAddress refuses, before stet init connects, a postgres URL that pgx
// would connect with somewhere other than where the proxy relays the agents'
// sessions, which is the URL's own one host and port (config.UpstreamAddr).
// It refuses one with no host, which pgx takes for a Unix socket; one whose
// query sets host, hostaddr or port, which pgx takes over the URL's own; and
// one that lists several hosts, which pgx tries in turn. Its errors never
// quote the address, which holds the password. Any other address that is not
// a postgres URL is chooseSSLMode's to refuse.
func checkAddress(raw string) error {
	if !strings.HasPrefix(raw, "postgres://") && !strings.HasPrefix(raw, "postgresql://") {
		return nil
	}
	if u, err := url.Parse(raw); err == nil && u.Hostname() == "" {
		return errors.New(errNoHost)
	}
	query, _ := uriQuery(raw)
	for pair := range strings.SplitSeq(query, "&") {
		key, _, _ := strings.Cut(pair, "=")
		if k, _ := url.PathUnescape(key); k == "host" || k == "hostaddr" || k == "port" {
			return errors.New(errQueryHost)
		}
	}
	// pgx splits the host list at commas after decoding it, so %2C is one.
	hosts := uriHosts(raw)
	if decoded, _ := url.PathUnescape(hosts); strings.Contains(hosts, ",") || strings.Contains(decoded, ",") {
		return errors.New(errHosts)
	}
	return nil
}

// uriHosts returns the host list of the connection URI raw as pgconn reads
// it (as uriQuery does the query): what follows the user name and password,
// up to the path or the query.
func uriHosts(raw string) string {
	_, rest, _ := strings.Cut(raw, "://")
	if i := strings.IndexAny(rest, "@/"); i >= 0 && rest[i] == '@' {
		rest = rest[i+1:]
	}
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// checkConfig returns nil when stet proxy accepts file, the config rendered
// for upstream, and else why not. config's reasons may quote upstream whole
// (validateUpstream), so upstream and its passwords are taken out of the
// reason (withoutSecrets), and the rest is made printable.
func checkConfig(file []byte, upstream string) error {
	if _, err := config.Parse(file); err != nil {
		return errors.New("stet proxy would not accept the config for this address: " + printable(withoutSecrets(err.Error(), upstream)))
	}
	return nil
}

// withoutSecrets is s with upstream replaced by <the database address> and
// its passwords by <password>: the one in its user information and any
// password or sslpassword in its query, each as written and decoded. Each is
// also replaced as %q writes it, inside quotes, as config's errors and
// net/url's quote an address.
func withoutSecrets(s, upstream string) string {
	type secret struct{ text, shown string }
	secrets := []secret{{upstream, "<the database address>"}}
	for _, p := range addressPasswords(upstream) {
		secrets = append(secrets, secret{p, "<password>"})
	}
	var pairs []secret
	for _, x := range secrets {
		if x.text == "" {
			continue
		}
		quoted := strconv.Quote(x.text)
		pairs = append(pairs, x, secret{quoted[1 : len(quoted)-1], x.shown})
	}
	// The longest first: a replacer prefers the earlier of two that start
	// at the same place, and upstream holds the passwords.
	slices.SortStableFunc(pairs, func(a, b secret) int { return len(b.text) - len(a.text) })
	var args []string
	for _, p := range pairs {
		args = append(args, p.text, p.shown)
	}
	return strings.NewReplacer(args...).Replace(s)
}

// addressPasswords returns the passwords in the URL raw, each as written and
// decoded: the one in its user information as net/url splits it (as the
// config reads the address), and the values of password and sslpassword in
// its query.
func addressPasswords(raw string) []string {
	var written []string
	_, rest, _ := strings.Cut(raw, "://")
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		if _, password, ok := strings.Cut(rest[:i], ":"); ok {
			written = append(written, password)
		}
	}
	query, _ := uriQuery(raw)
	for pair := range strings.SplitSeq(query, "&") {
		key, value, _ := strings.Cut(pair, "=")
		if k, _ := url.PathUnescape(key); k == "password" || k == "sslpassword" {
			written = append(written, value)
		}
	}
	var all []string
	for _, p := range written {
		all = append(all, p)
		if decoded, err := url.PathUnescape(p); err == nil {
			all = append(all, decoded)
		}
	}
	return all
}

// tableNames is the list of -tables: names separated by commas.
func tableNames(list string) []string {
	var names []string
	for name := range strings.SplitSeq(list, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// initFactsAt opens Stet's pool at upstream and reads the facts
// (readInitFacts), within initReadTimeout.
func initFactsAt(ctx context.Context, upstream string, only []string) (*initFacts, error) {
	ctx, cancel := context.WithTimeout(ctx, initReadTimeout)
	defer cancel()
	pool, err := openPool(ctx, upstream)
	if err != nil {
		return nil, printableError(err)
	}
	defer pool.Close()
	facts, err := readInitFacts(ctx, pool, only)
	if err != nil && errors.Unwrap(err) != nil {
		// The server's error. The other, one line for each name of -tables
		// that cannot be taken, quotes the names it was given.
		return nil, printableError(err)
	}
	return facts, err
}

// checkInitFacts returns why stet init stops on what it read of the
// database, nil when it goes on: a server before PostgreSQL 14, no stet
// schema and an account that may not create it, no table to watch.
func checkInitFacts(f *initFacts) error {
	switch {
	case f.VersionNum < 140000:
		return fmt.Errorf("PostgreSQL 14 or newer is needed; this server is %s", printable(f.Version))
	case !f.SchemaExists && !f.CanCreate:
		return fmt.Errorf("the stet schema is not there and %s may not create it; as the database's owner run: GRANT CREATE ON DATABASE %s TO %s;",
			identifier(f.User), identifier(f.Database), identifier(f.User))
	case len(f.Tables) == 0:
		return errors.New(strings.TrimSuffix(errNoTable+"\n"+leftOut(f), "\n"))
	}
	return nil
}

// skipOrder is the order of the reasons a table is left out in, the order
// they are checked in (initCandidate.skip).
var skipOrder = []string{skipSearchPath, skipNameQuoting, skipNoKey, skipKeyQuoting, skipUnreadable}

// leftOut says, a line each, how many tables were left out for each reason,
// and how many over the cap.
func leftOut(f *initFacts) string {
	reasons := slices.Clone(skipOrder)
	for _, why := range slices.Sorted(maps.Keys(f.Skipped)) {
		if !slices.Contains(reasons, why) {
			reasons = append(reasons, why)
		}
	}
	var b strings.Builder
	for _, why := range reasons {
		if n := f.Skipped[why]; n > 0 {
			fmt.Fprintf(&b, "left out %s: %s\n", count(n, "table"), why)
		}
	}
	if f.Dropped > 0 {
		fmt.Fprintf(&b, "left out %s: stet init takes at most %d, those with the most rows; to choose the tables yourself, pass -tables a,b,c\n",
			count(f.Dropped, "more table"), maxInitTables)
	}
	return b.String()
}

// count is n and noun, plural unless n is 1.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// schemaNotice tells what stet proxy will create when the stet schema is not
// there.
func schemaNotice(f *initFacts) string {
	return fmt.Sprintf("stet proxy will create one schema, \"stet\", in database %s as %s the first time it starts.\n"+
		"It changes nothing else. To remove it later: DROP SCHEMA stet CASCADE;\n", identifier(f.Database), identifier(f.User))
}

// confirmed reads the answer to the question: y or yes, in any case, is yes;
// anything else, and no answer, is no.
func confirmed(stdin io.Reader, stdout io.Writer) bool {
	in := bufio.NewScanner(stdin)
	if !in.Scan() {
		fmt.Fprintln(stdout) // the input ended on the question's line
		return false
	}
	answer := strings.TrimSpace(in.Text())
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes")
}

// identifier is name, which the server sent, printable and quoted as an SQL
// identifier.
func identifier(name string) string {
	return pgx.Identifier{printable(name)}.Sanitize()
}

// printable is s, text the server sent, with what a terminal would act on or
// an eye misread as spaces: control characters, line and paragraph
// separators, direction controls (the characters internal/lines replaces).
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cc, r) || r == 0x2028 || r == 0x2029 || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return ' '
		}
		return r
	}, s)
}

// printableError is err with its text printable, but for the line of its own
// that chooseSSLMode puts the hint about connection poolers on.
func printableError(err error) error {
	msg := err.Error()
	if head, ok := strings.CutSuffix(msg, "\n"+poolerHint); ok {
		return errors.New(printable(head) + "\n" + poolerHint)
	}
	return errors.New(printable(msg))
}

// renderConfig returns the config file stet init writes.
//
// It is built as YAML nodes, so the encoder quotes what YAML would read as
// something else (a table named null or true, an address with special
// characters). The tables are in the order of their names; each has its key
// column, impact.max_rows: 100, reads, and stale when its key's type is one
// stale tracks, the last two as keys with no value, which turn them on.
func renderConfig(upstream, listen string, tables []initTable, version string, day time.Time) ([]byte, error) {
	str := func(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }
	on := func() *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"} }
	mapping := func(content ...*yaml.Node) *yaml.Node {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: content}
	}

	byName := mapping()
	for _, t := range slices.SortedFunc(slices.Values(tables), func(a, b initTable) int { return strings.Compare(a.Name, b.Name) }) {
		table := mapping(
			str("pk"), str(t.PK),
			str("impact"), mapping(str("max_rows"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(initMaxRows)}),
			str("reads"), on(),
		)
		if t.Stale {
			table.Content = append(table.Content, str("stale"), on())
		}
		byName.Content = append(byName.Content, str(t.Name), table)
	}
	doc := &yaml.Node{
		Kind:        yaml.DocumentNode,
		HeadComment: fmt.Sprintf(configHeader, version, day.Format("2006-01-02")),
		Content: []*yaml.Node{mapping(
			str("mode"), str(string(config.ModeShadow)),
			str("listen"), str(listen),
			str("upstream"), str(upstream),
			str("model"), mapping(str("adapter"), str(config.RecordOnly)),
			str("tables"), byName,
		)},
	}

	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// agentParams are the query parameters the agent's address leaves out,
// besides those whose name starts with ssl (plan correction P8): TLS and
// channel binding are for the leg from stet to the server. host, hostaddr
// and port would take the agent past stet, to the server.
var agentParams = []string{"channel_binding", "gssencmode", "requiressl", "host", "hostaddr", "port"}

// agentURL returns the address to give the agent: upstream's, at listen,
// without TLS and channel-binding parameters, with sslmode=disable and the
// password shown as <password>.
//
// The parameters it keeps are as written, in the order of their names, with
// sslmode=disable after them; a password parameter is shown as
// password=<password>. A listen host that is empty or unspecified (0.0.0.0,
// ::) is written 127.0.0.1.
func agentURL(upstream, listen string) (string, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return "", errors.New(errNotURL)
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", errors.New(errListen)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	var b strings.Builder
	b.WriteString(u.Scheme + "://")
	if u.User != nil {
		b.WriteString(url.User(u.User.Username()).String())
		if password, _ := u.User.Password(); password != "" {
			b.WriteString(":<password>")
		}
		b.WriteByte('@')
	}
	b.WriteString(net.JoinHostPort(host, port) + u.EscapedPath() + "?")

	type param struct{ name, text string }
	var params []param
	for text := range strings.SplitSeq(u.RawQuery, "&") {
		key, _, _ := strings.Cut(text, "=")
		name, err := url.PathUnescape(key)
		if err != nil {
			name = key
		}
		switch {
		case text == "", strings.HasPrefix(name, "ssl"), slices.Contains(agentParams, name):
			continue
		case name == "password":
			text = key + "=<password>"
		}
		params = append(params, param{name, text})
	}
	slices.SortStableFunc(params, func(a, b param) int { return strings.Compare(a.name, b.name) })
	for _, p := range params {
		b.WriteString(p.text + "&")
	}
	b.WriteString("sslmode=disable")
	return b.String(), nil
}

// alreadyExists is the error for a config file that is there.
func alreadyExists(path string) error {
	return fmt.Errorf("%s already exists; pass -force to overwrite it", path)
}

// writeConfigFile writes b to path whole or not at all: to a temporary file
// in path's directory, readable by its owner only (0600: it holds the
// password), synced, then put in place. With force it replaces a file at
// path; without, a file there by then, made since stet init looked, is left
// as it is (a hard link does not replace one). On a failure the temporary
// file is removed.
func writeConfigFile(path string, b []byte, force bool) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if force {
		return os.Rename(f.Name(), path)
	}
	if err = os.Link(f.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return alreadyExists(path)
		}
		return err
	}
	os.Remove(f.Name()) // the file is in place under path either way
	return nil
}

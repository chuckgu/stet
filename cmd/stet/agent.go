package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/stetdb/stet/internal/config"
	"github.com/stetdb/stet/internal/custody"
	"github.com/stetdb/stet/internal/safetext"
	"github.com/stetdb/stet/internal/scram"
)

// agentIO is what stet agent reads from and writes to.
type agentIO struct {
	stdin    io.Reader
	stdout   io.Writer
	terminal bool // stdin is a terminal it can prompt on
	env      func(string) string
	secret   func(prompt string) (string, error) // reads without echo; nil: read a line from stdin
	now      func() time.Time
}

// readSecret is main's agentIO.secret: it shows prompt and reads what is
// typed on the terminal of standard input, without echo.
func readSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stdout)
	return string(b), err
}

const agentUsage = `usage: stet agent <command> [flags] [<account>]

stet agent keeps the accounts stet proxy logs agents in as, in the file agents.credentials names: an
agent logs in to stet with a token, and stet logs in to the database with the account's real password.

commands:
  add       [-config stet.yaml] [-password-env NAME] <account>
            check the account's database password by logging in once, keep it, and issue a token
  token     [-config stet.yaml] <account>
            issue a new token; the old one stops working at the next login
  password  [-config stet.yaml] [-password-env NAME] <account>
            check and keep the account's new database password, once it is changed in the database
  remove    [-config stet.yaml] <account>
            remove the account; it is refused from the next login
  list      [-config stet.yaml]
            list the accounts, without their tokens or passwords

The password is asked for on a terminal, without echo; otherwise it is the first line of standard input.
With -password-env NAME stet keeps the name of an environment variable instead, which stet proxy reads
at start; stet agent reads it here to check it.
`

// What stet agent stops with (spec-v0.2-server 7).
const (
	errNoCredentials = "the config has no agents.credentials; add it first"
	errEmptyPassword = "the password is empty"
	errEmptyAccount  = "the account name is empty"
	errNoEnvName     = "-password-env needs the name of an environment variable"
)

// maxAccountName is the longest account name the credentials file takes, in
// bytes: PostgreSQL's limit for a name, as custody.Read applies it.
const maxAccountName = 63

// agentCheckTimeout bounds the login add and password try with the password
// given, as stet proxy bounds its own (spec-v0.2-server 4).
const agentCheckTimeout = 5 * time.Second

// writeCredentials is custody.Write; a test replaces it to make the sync of
// the directory fail.
var writeCredentials = custody.Write

// runAgent is stet agent (spec-v0.2-server 7). It keeps the accounts of the
// credentials file agents.credentials names: add checks an account's
// database password by logging in once, as stet proxy logs in, then keeps
// it with the verifier of a new token; token replaces the verifier; password
// replaces the password, checked the same way; remove drops the account;
// list shows them. It writes the file whole (custody.Write), once every
// check has passed, and never creates its directory. It neither creates nor
// changes an account in the database.
//
// A token is printed once, by the add or token that made it. A password or
// verifier is printed nowhere, in an error neither. Text from outside (the
// server's messages, names in the file) is printed printable, and an account
// named on the command line is quoted.
func runAgent(args []string, a agentIO) error {
	usageErr := errors.New(strings.TrimSuffix(agentUsage, "\n"))
	if len(args) == 0 {
		return usageErr
	}
	cmd := args[0]
	switch cmd {
	case "-h", "-help", "--help":
		fmt.Fprint(a.stdout, agentUsage)
		return nil
	case "add", "token", "password", "remove", "list":
	default:
		return usageErr
	}

	flags := flag.NewFlagSet("agent "+cmd, flag.ContinueOnError)
	path := flags.String("config", "stet.yaml", "config file")
	envName := new(string)
	if cmd == "add" || cmd == "password" {
		envName = flags.String("password-env", "", "keep the name of this environment variable instead of the password")
	}
	// The flag package prints nothing itself: -h prints the usage on stdout
	// and is no error, and any other error is returned for main to print, once.
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(a.stdout, agentUsage)
			return nil
		}
		return err
	}
	accounts := 1
	if cmd == "list" {
		accounts = 0
	}
	if flags.NArg() != accounts {
		return usageErr
	}
	given := false
	flags.Visit(func(f *flag.Flag) { given = given || f.Name == "password-env" })
	if given && *envName == "" {
		return errors.New(errNoEnvName)
	}
	account := flags.Arg(0)
	if accounts == 1 {
		if err := checkAccountName(account); err != nil {
			return err
		}
	}

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if cfg.Credentials == "" {
		return errors.New(errNoCredentials)
	}
	switch cmd {
	case "add":
		return agentAdd(cfg, account, *envName, a)
	case "token":
		return agentToken(cfg.Credentials, account, a)
	case "password":
		return agentPassword(cfg, account, *envName, a)
	case "remove":
		return agentRemove(cfg.Credentials, account, a.stdout)
	}
	return agentList(cfg.Credentials, a.stdout)
}

// checkAccountName applies custody.Read's rule for an account's name.
func checkAccountName(name string) error {
	switch {
	case name == "":
		return errors.New(errEmptyAccount)
	case len(name) > maxAccountName:
		return fmt.Errorf("account %q: the name is longer than %d bytes", name, maxAccountName)
	}
	return nil
}

// agentAdd is stet agent add. It asks for nothing before it knows the
// account can be added, and writes nothing unless the password logs in.
func agentAdd(cfg *config.Config, account, envName string, a agentIO) error {
	own, database, err := upstreamAccount(cfg.Upstream)
	if err != nil {
		return err
	}
	// A token of Stet's own account would open a session no gate sees, and
	// stet proxy refuses to start with it in the file (openClientSide).
	if account == own {
		return fmt.Errorf("%q is the account stet itself connects as; an agent needs its own", account)
	}
	f, there, err := readCredentials(cfg.Credentials)
	if err != nil {
		return err
	}
	if !there {
		if err := checkDirectory(cfg.Credentials); err != nil {
			return err
		}
	}
	if _, ok := f.Agents[account]; ok {
		return fmt.Errorf("%q is already there; use stet agent token or stet agent password", account)
	}
	password, err := readPassword(account, envName, a)
	if err != nil {
		return err
	}
	if err := checkPassword(cfg, account, password, database); err != nil {
		return err
	}

	token, v, err := scram.NewToken()
	if err != nil {
		return err
	}
	now := a.now().UTC().Truncate(time.Second)
	entry := custody.Agent{Token: v.Text(), Added: now, TokenIssued: now}
	if envName != "" {
		entry.PasswordEnv = envName
	} else {
		entry.Password = password
	}
	f.Agents[account] = entry
	warning, err := saveCredentials(cfg.Credentials, f)
	if err != nil {
		return err
	}
	printToken(a.stdout, account, token)
	fmt.Fprintf(a.stdout, "\nnext:\n"+
		"  - in the agent's connection string, put this server's address as the host and the token as the password; the user stays %q\n"+
		"  - if anything else knew this account's database password, change it in the database, then run: stet agent password %s\n",
		account, shellWord(account))
	if envName != "" {
		fmt.Fprintf(a.stdout, "  - stet proxy must have %s in its environment\n", safetext.Printable(envName))
	}
	fmt.Fprintln(a.stdout, "  - restart stet proxy: an account added while it runs is not accepted")
	printWarning(a.stdout, warning)
	return nil
}

// agentToken is stet agent token: a new token for account, which the
// running stet proxy takes from the next login.
func agentToken(path, account string, a agentIO) error {
	f, entry, err := accountIn(path, account)
	if err != nil {
		return err
	}
	token, v, err := scram.NewToken()
	if err != nil {
		return err
	}
	entry.Token = v.Text()
	entry.TokenIssued = a.now().UTC().Truncate(time.Second)
	f.Agents[account] = entry
	warning, err := saveCredentials(path, f)
	if err != nil {
		return err
	}
	printToken(a.stdout, account, token)
	fmt.Fprintln(a.stdout, "\nthe old token stops working at the next login; no restart is needed")
	printWarning(a.stdout, warning)
	return nil
}

// agentPassword is stet agent password: the account's new database
// password, kept once it logs in. An account whose password is an
// environment variable's has nothing to change in the file, unless
// -password-env names one again: stet proxy reads the variable at start.
func agentPassword(cfg *config.Config, account, envName string, a agentIO) error {
	f, entry, err := accountIn(cfg.Credentials, account)
	if err != nil {
		return err
	}
	if envName == "" && entry.PasswordEnv != "" {
		fmt.Fprintf(a.stdout, "%q takes its password from %s; change it in stet proxy's environment and restart stet proxy\n",
			account, safetext.Printable(entry.PasswordEnv))
		return nil
	}
	_, database, err := upstreamAccount(cfg.Upstream)
	if err != nil {
		return err
	}
	password, err := readPassword(account, envName, a)
	if err != nil {
		return err
	}
	if err := checkPassword(cfg, account, password, database); err != nil {
		return err
	}
	if envName != "" {
		entry.Password, entry.PasswordEnv = "", envName
	} else {
		entry.Password, entry.PasswordEnv = password, ""
	}
	f.Agents[account] = entry
	warning, err := saveCredentials(cfg.Credentials, f)
	if err != nil {
		return err
	}
	if envName != "" {
		name := safetext.Printable(envName)
		fmt.Fprintf(a.stdout, "%q now takes its password from %s; stet proxy must have %s in its environment: restart stet proxy\n",
			account, name, name)
	} else {
		fmt.Fprintf(a.stdout, "password for %q updated; no restart is needed\n", account)
	}
	printWarning(a.stdout, warning)
	return nil
}

// agentRemove is stet agent remove.
func agentRemove(path, account string, out io.Writer) error {
	f, _, err := accountIn(path, account)
	if err != nil {
		return err
	}
	delete(f.Agents, account)
	warning, err := saveCredentials(path, f)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%q removed; it is refused from the next login\n", account)
	printWarning(out, warning)
	return nil
}

// agentList is stet agent list: a line for each account, in the order of
// their names, with where its password is and when it was added and its
// token issued, in UTC.
func agentList(path string, out io.Writer) error {
	f, _, err := readCredentials(path)
	if err != nil {
		return err
	}
	if len(f.Agents) == 0 {
		fmt.Fprintln(out, "no accounts")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT\tPASSWORD\tADDED\tTOKEN ISSUED")
	for _, name := range slices.Sorted(maps.Keys(f.Agents)) {
		e := f.Agents[name]
		where := "file"
		if e.PasswordEnv != "" {
			where = "env " + safetext.Printable(e.PasswordEnv)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", safetext.Printable(name), where, listTime(e.Added), listTime(e.TokenIssued))
	}
	return tw.Flush()
}

// listTime is t as list shows it: in UTC, to the minute; "-" when the file
// has none.
func listTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// readCredentials reads the credentials file at path (custody.Read). A file
// that is not there reads as one with no account; there says which.
func readCredentials(path string) (f *custody.File, there bool, err error) {
	f, err = custody.Read(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &custody.File{Agents: map[string]custody.Agent{}}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return f, true, nil
}

// accountIn reads the credentials file at path and account's entry in it.
// No file, like no such entry, is no account.
func accountIn(path, account string) (*custody.File, custody.Agent, error) {
	f, _, err := readCredentials(path)
	if err != nil {
		return nil, custody.Agent{}, err
	}
	entry, ok := f.Agents[account]
	if !ok {
		return nil, custody.Agent{}, fmt.Errorf("no account %q", account)
	}
	return f, entry, nil
}

// checkDirectory stops add, before it asks for anything, when the
// directory a new credentials file at path goes in is not there: stet agent
// does not create it.
func checkDirectory(path string) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("agents.credentials: the directory %s is not there; create it first", dir)
	}
	return nil
}

// readPassword returns the database password add and password check: with
// -password-env, the variable's value in this environment; else, on a
// terminal, what is typed at a prompt without echo; else the first line of
// standard input, without its line ending. An empty one is refused.
func readPassword(account, envName string, a agentIO) (string, error) {
	if envName != "" {
		v := a.env(envName)
		if v == "" {
			return "", fmt.Errorf("-password-env: %s is empty or not set in this environment", safetext.Printable(envName))
		}
		return v, nil
	}
	var password string
	if a.terminal && a.secret != nil {
		p, err := a.secret(fmt.Sprintf("Database password for %q: ", account))
		if err != nil {
			return "", fmt.Errorf("reading the password: %w", err)
		}
		password = p
	} else {
		in := bufio.NewScanner(a.stdin) // a line ends at \n, and a \r before it is dropped
		if !in.Scan() && in.Err() != nil {
			return "", fmt.Errorf("reading the password from standard input: %w", in.Err())
		}
		password = in.Text()
	}
	if password == "" {
		return "", errors.New(errEmptyPassword)
	}
	return password, nil
}

// checkPassword logs in to database as account with password, as stet proxy
// will (custody.TryPassword), within agentCheckTimeout. Its error names the
// kind of failure and carries the server's message, printable; it quotes
// neither the address nor the password.
func checkPassword(cfg *config.Config, account, password, database string) error {
	ctx, cancel := context.WithTimeout(context.Background(), agentCheckTimeout)
	defer cancel()
	err := custody.TryPassword(ctx, cfg.Upstream, cfg.UpstreamTLS, cfg.UpstreamPlainFallback, account, password, database)
	if err != nil {
		return fmt.Errorf("could not log in to the database as %q with that password, so nothing was written: %v", account, err)
	}
	return nil
}

// saveCredentials writes f to path (writeCredentials). When only the sync of
// the directory failed, the file is in place: then it returns no error, and
// a warning to print once the command has said what it did.
func saveCredentials(path string, f *custody.File) (warning string, err error) {
	err = writeCredentials(path, f)
	if errors.Is(err, custody.ErrNotSynced) {
		return "warning: " + safetext.Printable(err.Error()), nil
	}
	return "", err
}

func printWarning(out io.Writer, warning string) {
	if warning != "" {
		fmt.Fprintln(out, warning)
	}
}

// printToken shows account's new token: the one time it is shown.
func printToken(out io.Writer, account, token string) {
	fmt.Fprintf(out, "token for %q (shown once; stet keeps only what it needs to check it):\n  %s\n", account, token)
}

// shellWord is s as one word of a shell command: as it is when it needs no
// quoting, else in single quotes. Its control characters are spaces.
func shellWord(s string) string {
	s = safetext.Printable(s)
	special := func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-.@+:,/%", r))
	}
	if s != "" && strings.IndexFunc(s, special) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

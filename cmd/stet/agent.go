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
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
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

// readSecret is main's agentIO.secret: it shows prompt on standard error,
// so that standard output holds only what the command prints, and reads
// what is typed on the terminal of standard input, without echo. An
// interrupt while it waits puts the terminal back as it was and ends stet
// with 130, rather than leave echo off.
func readSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	stop := onInterrupt(sigs, func() {
		term.Restore(fd, state)
		fmt.Fprintln(os.Stderr)
	}, os.Exit)
	defer func() {
		signal.Stop(sigs)
		stop()
	}()
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

// onInterrupt calls restore and then exit(130) when a signal comes on sigs,
// until stop is called; stop returns once it has stopped looking.
func onInterrupt(sigs <-chan os.Signal, restore func(), exit func(int)) (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-sigs:
			restore()
			exit(130)
		case <-done:
		}
	}()
	return func() {
		close(done)
		<-finished
	}
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

Run stet agent as the file's owner, or as root (sudo), which keeps the file's owner, group and mode.
`

// What stet agent stops with (spec-v0.2-server 7).
const (
	errNoCredentials = "the config has no agents.credentials; add it first"
	errEmptyPassword = "the password is empty"
	errEnvValue      = "-password-env takes the name of an environment variable, not its value"
)

// envName is what -password-env takes: the name of an environment variable.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// agentCheckTimeout bounds the login add and password try with the password
// given, as stet proxy bounds its own (spec-v0.2-server 4).
const agentCheckTimeout = 5 * time.Second

// updateCredentials is custody.Update; a test replaces it to make the sync
// of the directory fail.
var updateCredentials = custody.Update

// runAgent is stet agent (spec-v0.2-server 7). It keeps the accounts of the
// credentials file agents.credentials names: add checks an account's
// database password by logging in once, as stet proxy logs in, then keeps
// it with the verifier of a new token; token replaces the verifier; password
// replaces the password, checked the same way; remove drops the account;
// list shows them. It neither creates nor changes an account in the
// database, and never creates the file's directory.
//
// It reads the file as its owner or root may (custody.ReadForUpdate) and
// changes it under a lock (custody.Update), applying only its own account's
// change to the file as it is by then, so that two runs never undo each
// other. It asks for the password, and logs in, holding no lock.
//
// A token is printed once, by the add or token that made it, and only once
// it is written. A password or verifier is printed nowhere, in an error
// neither. Text from outside (the server's messages, names in the file) is
// printed printable, and an account named on the command line is quoted.
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
	env := new(string)
	if cmd == "add" || cmd == "password" {
		env = flags.String("password-env", "", "keep the name of this environment variable instead of the password")
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
	given := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { given[f.Name] = true })
	// What is not a name may be the value, the password itself: refused
	// first, and never echoed.
	if given["password-env"] && !envName.MatchString(*env) {
		return errors.New(errEnvValue)
	}
	accounts := 1
	if cmd == "list" {
		accounts = 0
	}
	if flags.NArg() != accounts {
		return usageErr
	}
	c := &agentCmd{account: flags.Arg(0), envName: *env, io: a}
	if given["config"] {
		c.configFlag = *path
	}
	if accounts == 1 {
		if err := custody.CheckAccountName(c.account); err != nil {
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
	c.cfg = cfg
	switch cmd {
	case "add":
		return c.add()
	case "token":
		return c.token()
	case "password":
		return c.password()
	case "remove":
		return c.remove()
	}
	return c.list()
}

// agentCmd is one run of a stet agent command.
type agentCmd struct {
	cfg        *config.Config
	configFlag string // -config as given; "" when it was not
	account    string // "" for list
	envName    string // -password-env; "" when it was not given
	io         agentIO
}

// add is stet agent add. It asks for nothing before it knows the account
// can be added, and writes nothing unless the password logs in.
func (c *agentCmd) add() error {
	own, database, err := upstreamAccount(c.cfg.Upstream)
	if err != nil {
		return err
	}
	// A token of Stet's own account would open a session no gate sees, and
	// stet proxy refuses to start with it in the file (openClientSide).
	if c.account == own {
		return fmt.Errorf("%q is the account stet itself connects as; an agent needs its own", c.account)
	}
	if err := checkDirectory(c.cfg.Credentials); err != nil {
		return err
	}
	f, err := custody.ReadForUpdate(c.cfg.Credentials)
	if err != nil {
		return err
	}
	if _, ok := f.Agents[c.account]; ok {
		return c.alreadyThere()
	}
	password, err := readPassword(c.account, c.envName, c.io)
	if err != nil {
		return err
	}
	if err := checkPassword(c.cfg, c.account, password, database); err != nil {
		return err
	}

	token, v, err := scram.NewToken()
	if err != nil {
		return err
	}
	now := c.io.now().UTC().Truncate(time.Second)
	entry := custody.Agent{Token: v.Text(), Added: now, TokenIssued: now}
	if c.envName != "" {
		entry.PasswordEnv = c.envName
	} else {
		entry.Password = password
	}
	warning, err := c.update(func(f *custody.File) error {
		if _, ok := f.Agents[c.account]; ok { // added meanwhile
			return c.alreadyThere()
		}
		f.Agents[c.account] = entry
		return nil
	})
	if err != nil {
		return err
	}
	out := c.io.stdout
	printToken(out, c.account, token)
	fmt.Fprintf(out, "\nnext:\n"+
		"  - in the agent's connection string, put this server's address as the host and the token as the password; the user stays %q\n"+
		"  - if anything else knew this account's database password, change it in the database, then run: %s\n",
		c.account, c.command("password"))
	if c.envName != "" {
		fmt.Fprintf(out, "  - stet proxy must have %s in its environment\n", c.envName)
	}
	if c.rolesWritten() && !slices.Contains(c.cfg.AgentRoles, c.account) {
		fmt.Fprintf(out, "  - add %q to agents.roles in the config: %s\n", c.account, rolesMustMatch)
	}
	fmt.Fprintln(out, "  - restart stet proxy: an account added while it runs is not accepted")
	printWarning(out, warning)
	return nil
}

// token is stet agent token: a new token for the account, which the
// running stet proxy takes from the next login.
func (c *agentCmd) token() error {
	token, v, err := scram.NewToken()
	if err != nil {
		return err
	}
	issued := c.io.now().UTC().Truncate(time.Second)
	warning, err := c.update(func(f *custody.File) error {
		e, ok := f.Agents[c.account]
		if !ok {
			return c.noAccount()
		}
		e.Token, e.TokenIssued = v.Text(), issued
		f.Agents[c.account] = e
		return nil
	})
	if err != nil {
		return err
	}
	printToken(c.io.stdout, c.account, token)
	fmt.Fprintln(c.io.stdout, "\nthe old token stops working at the next login; no restart is needed")
	printWarning(c.io.stdout, warning)
	return nil
}

// password is stet agent password: the account's new database password,
// kept once it logs in. An account whose password is an environment
// variable's has nothing to change in the file, unless -password-env names
// one again: stet proxy reads the variable at start.
func (c *agentCmd) password() error {
	f, err := custody.ReadForUpdate(c.cfg.Credentials)
	if err != nil {
		return err
	}
	e, ok := f.Agents[c.account]
	if !ok {
		return c.noAccount()
	}
	if c.envName == "" && e.PasswordEnv != "" {
		fmt.Fprintf(c.io.stdout, "%q takes its password from %s; change it in stet proxy's environment and restart stet proxy\n",
			c.account, safetext.Printable(e.PasswordEnv))
		return nil
	}
	_, database, err := upstreamAccount(c.cfg.Upstream)
	if err != nil {
		return err
	}
	password, err := readPassword(c.account, c.envName, c.io)
	if err != nil {
		return err
	}
	if err := checkPassword(c.cfg, c.account, password, database); err != nil {
		return err
	}
	warning, err := c.update(func(f *custody.File) error {
		e, ok := f.Agents[c.account] // as it is now: its token may be newer
		if !ok {
			return c.noAccount()
		}
		if c.envName == "" && e.PasswordEnv != "" { // moved to a variable meanwhile
			return fmt.Errorf("%q takes its password from %s now, so nothing was written; change it in stet proxy's environment and restart stet proxy",
				c.account, safetext.Printable(e.PasswordEnv))
		}
		if c.envName != "" {
			e.Password, e.PasswordEnv = "", c.envName
		} else {
			e.Password, e.PasswordEnv = password, ""
		}
		f.Agents[c.account] = e
		return nil
	})
	if err != nil {
		return err
	}
	if c.envName != "" {
		fmt.Fprintf(c.io.stdout, "%q now takes its password from %s; stet proxy must have %s in its environment: restart stet proxy\n",
			c.account, c.envName, c.envName)
	} else {
		fmt.Fprintf(c.io.stdout, "password for %q updated; no restart is needed\n", c.account)
	}
	printWarning(c.io.stdout, warning)
	return nil
}

// remove is stet agent remove.
func (c *agentCmd) remove() error {
	left := 0
	warning, err := c.update(func(f *custody.File) error {
		if _, ok := f.Agents[c.account]; !ok {
			return c.noAccount()
		}
		delete(f.Agents, c.account)
		left = len(f.Agents)
		return nil
	})
	if err != nil {
		return err
	}
	out := c.io.stdout
	fmt.Fprintf(out, "%q removed; it is refused from the next login\n", c.account)
	if c.rolesWritten() && slices.Contains(c.cfg.AgentRoles, c.account) {
		fmt.Fprintf(out, "remove %q from agents.roles in the config: %s\n", c.account, rolesMustMatch)
	}
	if left == 0 {
		fmt.Fprintln(out, "the credentials file has no account left: stet proxy will not start until one is added")
	}
	printWarning(out, warning)
	return nil
}

// list is stet agent list: a line for each account, in the order of their
// names, with where its password is and when it was added and its token
// issued, in UTC.
func (c *agentCmd) list() error {
	f, err := custody.ReadForUpdate(c.cfg.Credentials)
	if err != nil {
		return err
	}
	out := c.io.stdout
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

// rolesMustMatch is why a change to the file's accounts may need one to
// agents.roles: stet proxy compares the two at start.
const rolesMustMatch = "it must list the same accounts as the credentials file, or stet proxy will not start"

// rolesWritten reports whether the config writes agents.roles, which then
// must list the file's accounts.
func (c *agentCmd) rolesWritten() bool { return len(c.cfg.AgentRoles) > 0 }

func (c *agentCmd) alreadyThere() error {
	return fmt.Errorf("%q is already there; use stet agent token or stet agent password", c.account)
}

func (c *agentCmd) noAccount() error { return fmt.Errorf("no account %q", c.account) }

// command is the stet agent command line of sub for the account, with
// -config as it was given.
func (c *agentCmd) command(sub string) string {
	s := "stet agent " + sub
	if c.configFlag != "" {
		s += " -config " + shellWord(c.configFlag)
	}
	return s + " " + shellWord(c.account)
}

// update changes the credentials file with change, under its lock
// (updateCredentials). When only the sync of the directory failed, the file
// is in place: then it returns no error, and a warning to print once the
// command has said what it did.
func (c *agentCmd) update(change func(*custody.File) error) (warning string, err error) {
	err = updateCredentials(c.cfg.Credentials, change)
	if errors.Is(err, custody.ErrNotSynced) {
		return "warning: " + safetext.Printable(err.Error()), nil
	}
	return "", err
}

// listTime is t as list shows it: in UTC, to the minute; "-" when the file
// has none.
func listTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// checkDirectory stops add, before it asks for anything, when the
// directory the credentials file at path goes in is not there: stet agent
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
			return "", fmt.Errorf("-password-env: %s is empty or not set in this environment", envName)
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
// will (custody.TryPassword), within agentCheckTimeout. Its error says
// whether the server refused the password or the login failed otherwise,
// names the kind of failure and carries the server's message, printable; it
// quotes neither the address nor the password.
func checkPassword(cfg *config.Config, account, password, database string) error {
	ctx, cancel := context.WithTimeout(context.Background(), agentCheckTimeout)
	defer cancel()
	err := custody.TryPassword(ctx, cfg.Upstream, cfg.UpstreamTLS, cfg.UpstreamPlainFallback, account, password, database)
	switch {
	case err == nil:
		return nil
	case custody.WrongPassword(err):
		return fmt.Errorf("the database refused that password for %q, so nothing was written: %v", account, err)
	}
	return fmt.Errorf("could not log in to the database as %q to check the password, so nothing was written: %v", account, err)
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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/lab/bench"
	"github.com/stetdb/stet/internal/lab/crm"
	"github.com/stetdb/stet/internal/lab/tasks"
)

const benchUsage = `usage: stet-lab bench <command> [flags]

commands:
  prepare  create the benchmark's roles and the snapshot template (-practice: the practice one)
  clone    clone the template for an arm (-arm <arm> [-x N] [-rerun N])
  drop     drop one of the benchmark's databases (-name <db>)
  list     list the benchmark's databases and roles
  send     send an arm's stream to -addr and record the outcomes in its database
           (-arm <arm> [-practice] [-rerun N] -addr <host:port> [-sets V,I]);
           -sets X sends the attacks into the arm's X clone (-x N) from attack -from K on and exits
           with status 3 when an executed attack needs a fresh clone
           (-arm <arm> [-practice] [-rerun N] -addr <host:port> -sets X [-x N] [-from K]);
           -addr is the arm's proxy, never -pg, and for none it is -pg
  exec     send one statement the way send does and print its outcome; records nothing
           (-addr <host:port> -db <db> -role <role> [-app import-job] [-mode simple|bind]
            [-tag <12 digits>] -sql <statement> [-args a,b])
  llm-recorder  forward a proxy's calls to the Anthropic API, log level, tokens and cost, and stop at a dollar cap
           ([-listen 127.0.0.1:18080] -out <file> [-cap-usd 10] [-upstream https://api.anthropic.com])
  collect  join what an arm's proxy recorded about each write to it by tag, within the time the write was
           out, store it in lab.proxy of the arm's database and the totals in lab.meta (collect:<arm>): a stet
           arm's stet.decisions of its database and of each -db (this run's X clones); a hoop arm's audit logs
           and recorder files, after which its denials the log does not confirm are unknown
           (-arm <arm> [-practice] [-rerun N] [-db <db>]... [-audit <file>]... [-llm <file>]... [-join tag|text])
  score    score the collected runs and write the report of spec 8 and 9 as Korean markdown, from the
           databases alone; an arm may be given more than once with its reruns, and none is read even when
           it is not listed (-arms none,stet,hoop[:rerun],... [-practice] [-out <file>])
  choose   choose hoop's configuration by spec 6 from the practice runs of hoop-a, hoop-b, hoop-c and
           none (always the practice databases) and print the six candidates' J and blocked passes
  hoop-config  write one of hoop's configurations, a candidate of spec 6 or a reference arm, from the base
           to a new file (prompts b and c are read beside the base, as prompt-b.txt and prompt-c.txt)
           (-prompt a|b|c -medium allow|block [-cache] [-model <id>] [-no-endpoint] [-base lab/bench/hoop.yaml]
            -out <file>)
  stet-config  write the frozen Stet config with only its upstream's database replaced, to a new file
           (-database <db> [-in lab/bench/stet.yaml] -out <file>)
`

// benchEpoch is the date the benchmark's data is generated relative to.
var benchEpoch = time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

var armRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func runBench(args []string) error {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, benchUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "prepare":
		return benchPrepare(args[1:])
	case "clone":
		return benchClone(args[1:])
	case "drop":
		return benchDrop(args[1:])
	case "list":
		return benchList(args[1:])
	case "send":
		return benchSend(args[1:])
	case "exec":
		return benchExec(args[1:])
	case "llm-recorder":
		return benchLLMRecorder(args[1:])
	case "collect":
		return benchCollect(args[1:])
	case "score":
		return benchScore(args[1:])
	case "choose":
		return benchChoose(args[1:])
	case "hoop-config":
		return benchHoopConfig(args[1:])
	case "stet-config":
		return benchStetConfig(args[1:])
	}
	fmt.Fprint(os.Stderr, benchUsage)
	os.Exit(2)
	return nil
}

// benchFlags are the flags every bench command takes.
type benchFlags struct {
	pg, user, password *string
	practice           *bool
}

func newBenchFlags(fs *flag.FlagSet) benchFlags {
	pg := os.Getenv("STET_TEST_PG")
	if pg == "" {
		pg = "127.0.0.1:55432"
	}
	return benchFlags{
		pg:       fs.String("pg", pg, "server host:port (default $STET_TEST_PG, else 127.0.0.1:55432)"),
		user:     fs.String("admin-user", "postgres", "superuser that creates the roles and databases"),
		password: fs.String("admin-password", "postgres-pw", "the superuser's password"),
		practice: fs.Bool("practice", false, "the practice stream instead of the scored one"),
	}
}

func (f benchFlags) admin() bench.Admin {
	return bench.Admin{Addr: *f.pg, User: *f.user, Password: *f.password}
}

func (f benchFlags) connect(ctx context.Context) (*pgx.Conn, error) {
	return pgx.Connect(ctx, f.admin().URL("postgres"))
}

// benchPrepare creates the roles and the template, and prints the
// statements of each set.
func benchPrepare(args []string) error {
	fs := flag.NewFlagSet("bench prepare", flag.ExitOnError)
	f := newBenchFlags(fs)
	fs.Parse(args)
	ctx := context.Background()
	name := bench.Template(*f.practice)

	su, err := f.connect(ctx)
	if err != nil {
		return err
	}
	defer su.Close(ctx)
	var exists bool
	if err := su.QueryRow(ctx, "select exists (select 1 from pg_database where datname = $1)", name).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("bench prepare: %s already exists; drop it first (stet-lab bench drop -name %s)", name, name)
	}

	c, o := tasks.BenchPreset(*f.practice, benchEpoch)
	d := crm.Generate(c)
	b, err := tasks.BuildBench(d, o)
	if err != nil {
		return err
	}
	if err := bench.CreateRoles(ctx, su, bench.Names{}); err != nil {
		return err
	}
	meta := struct {
		Practice bool
		CRM      crm.Options
		Bench    tasks.BenchOptions
	}{*f.practice, c, o}
	if err := bench.CreateTemplate(ctx, f.admin(), bench.Names{}, name, d, b, meta); err != nil {
		return err
	}

	count := map[string]int{}
	for _, s := range b.Stream {
		count[s.Set]++
	}
	var sets []string
	for _, set := range []string{tasks.SetPrep, tasks.SetV, tasks.SetI, tasks.SetX} {
		if count[set] > 0 {
			sets = append(sets, fmt.Sprintf("%s %d", set, count[set]))
		}
	}
	fmt.Printf("%s: %s; %d attacks\n", name, strings.Join(sets, ", "), len(b.Attacks))
	return nil
}

// benchClone clones the template for an arm and prints the clone's name.
func benchClone(args []string) error {
	fs := flag.NewFlagSet("bench clone", flag.ExitOnError)
	f := newBenchFlags(fs)
	arm := fs.String("arm", "", "the arm, such as none, stet, stet-bind, hoop-a")
	x := fs.Int("x", 0, "1 or more: a clone for the X set (the n-th after the first)")
	rerun := fs.Int("rerun", 1, "2 or more: a rerun of the arm")
	fs.Parse(args)
	switch {
	case !armRE.MatchString(*arm):
		return fmt.Errorf("bench clone: -arm %q: want lowercase letters and digits joined by -", *arm)
	case *x < 0:
		return fmt.Errorf("bench clone: -x %d: want 0 (the arm's database) or more", *x)
	case *rerun < 1:
		return fmt.Errorf("bench clone: -rerun %d: want 1 (the first run) or more", *rerun)
	}
	name := bench.DBName(*arm, *f.practice, *x, *rerun)
	gate := strings.HasPrefix(*arm, "stet")
	if err := bench.Clone(context.Background(), f.admin(), bench.Names{}, bench.Template(*f.practice), name, gate); err != nil {
		return err
	}
	fmt.Println(name)
	return nil
}

func benchDrop(args []string) error {
	fs := flag.NewFlagSet("bench drop", flag.ExitOnError)
	f := newBenchFlags(fs)
	name := fs.String("name", "", "the database to drop")
	fs.Parse(args)
	if *name == "" {
		return errors.New("bench drop: -name is required")
	}
	return bench.Drop(context.Background(), f.admin(), *name)
}

func benchList(args []string) error {
	fs := flag.NewFlagSet("bench list", flag.ExitOnError)
	f := newBenchFlags(fs)
	fs.Parse(args)
	ctx := context.Background()
	su, err := f.connect(ctx)
	if err != nil {
		return err
	}
	defer su.Close(ctx)
	dbs, roles, err := bench.Created(ctx, su, bench.Names{})
	if err != nil {
		return err
	}
	fmt.Printf("databases: %s\nroles: %s\n", strings.Join(dbs, " "), strings.Join(roles, " "))
	return nil
}

// benchSend sends an arm's stream to -addr (spec 4, "보내는 방식") and
// records every write's outcome in lab.outcomes of the arm's database. A set
// the arm already has outcomes of is not sent again. -sets X goes its own
// way (benchSendX). -addr is the arm's proxy, never the database server
// (-pg); for none it is the database server (throughProxy).
func benchSend(args []string) error {
	fs := flag.NewFlagSet("bench send", flag.ExitOnError)
	f := newBenchFlags(fs)
	arm := fs.String("arm", "", "the arm, such as none, stet, stet-bind, hoop-a")
	rerun := fs.Int("rerun", 1, "2 or more: a rerun of the arm")
	addr := fs.String("addr", "", "host:port the statements go to: the arm's proxy, or the server for none")
	setList := fs.String("sets", "", "the sets to send, such as V,I (default: the arm's sets but X); X is sent alone")
	x := fs.Int("x", 0, "-sets X: the arm's X clone the attacks go to, as bench clone -x names it (0 is 1)")
	from := fs.Int("from", 0, "-sets X: the first attack to send, after a fresh clone")
	fs.Parse(args)
	if *addr == "" {
		return errors.New("bench send: -addr is required")
	}
	bind, part, planned, err := bench.ArmPlan(*arm, *f.practice)
	if err != nil {
		return fmt.Errorf("bench send: %w", err)
	}
	sets := slices.DeleteFunc(slices.Clone(planned), func(s string) bool { return s == tasks.SetX })
	if *setList != "" {
		sets = strings.Split(*setList, ",")
		for _, s := range sets {
			switch {
			case !slices.Contains(planned, s):
				return fmt.Errorf("bench send: -sets: arm %s does not send set %q (it sends %s)", *arm, s, strings.Join(planned, ","))
			case s == tasks.SetX && len(sets) > 1:
				return errors.New("bench send: -sets: X is sent alone")
			}
		}
	}
	if err := throughProxy(*arm, *addr, *f.pg); err != nil {
		return fmt.Errorf("bench send: %w", err)
	}
	if slices.Equal(sets, []string{tasks.SetX}) {
		reclone, err := benchSendX(f, *arm, *f.practice, *rerun, *x, *from, *addr, bind)
		if err != nil {
			return err
		}
		if reclone {
			os.Exit(3)
		}
		return nil
	}
	if *x != 0 || *from != 0 {
		return errors.New("bench send: -x and -from go with -sets X")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db := bench.DBName(*arm, *f.practice, 0, *rerun)
	conn, err := pgx.Connect(ctx, bench.OwnerURL(*f.pg, bench.Names{}, db))
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	check := slices.Clone(sets)
	if slices.Contains(sets, tasks.SetV) {
		check = append(check, tasks.SetPrep) // V is sent with its prep statements
	}
	for _, set := range check {
		sent, err := bench.AlreadySent(ctx, conn, *arm, set)
		if err != nil {
			return err
		}
		if sent {
			return fmt.Errorf("bench send: %s already holds outcomes of arm %s, set %s; nothing was sent "+
				"(a stream is sent once: clone a fresh database with -rerun)", db, *arm, set)
		}
	}
	stream, _, err := bench.LoadStream(ctx, conn)
	if err != nil {
		return err
	}
	phases := bench.Phases(stream, sets, part)
	total := 0
	for _, p := range phases {
		for _, u := range p {
			total += len(u)
		}
	}

	o := bench.DefaultOptions(*arm)
	o.Bind = bind
	count := map[string]int{}
	done := 0
	// Record first checks that -addr answers, and records nothing if not.
	err = bench.Record(ctx, conn, bench.Target{Addr: *addr, Database: db, Names: bench.Names{}}, phases, o, func(out bench.Outcome) {
		count[out.Result]++
		if done++; done%100 == 0 {
			fmt.Fprintf(os.Stderr, "%d/%d sent\n", done, total)
		}
	})
	if err != nil && done == 0 {
		return fmt.Errorf("bench send: %w", err)
	}
	fmt.Printf("%s, arm %s, sets %s: %d of %d sent; %s\n", db, *arm, strings.Join(sets, ","), done, total, outcomeCounts(count))
	return err
}

// throughProxy refuses an -addr that would skip the arm's proxy: every arm but none sends to its proxy, never
// to the database server (-pg) itself, and none sends to the database server. Both are host:port, with
// localhost and the loopback addresses taken as one host. Without it a proxied arm sent to the database runs
// everything, and its collect finds no record of a proxy.
func throughProxy(arm, addr, pg string) error {
	same := sameServer(addr, pg)
	switch {
	case arm == "none" && !same:
		return fmt.Errorf("-addr %s: arm none sends to the database server itself, -pg %s", addr, pg)
	case arm != "none" && same:
		return fmt.Errorf("-addr %s is the database server (-pg %s): arm %s sends through its proxy, at the proxy's "+
			"address", addr, pg, arm)
	}
	return nil
}

// sameServer reports whether two host:port addresses name the same server.
func sameServer(a, b string) bool {
	norm := func(addr string) string {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return addr
		}
		switch strings.ToLower(host) {
		case "localhost", "127.0.0.1", "::1":
			host = "loopback"
		}
		return net.JoinHostPort(host, port)
	}
	return norm(a) == norm(b)
}

// benchSendX sends the arm's attacks from attack from on into its X clone x
// (spec 4, "X"), probing that clone as the owner, and records the steps'
// outcomes and the attacks' results in the arm's own database. It sends only
// when from is exactly where the recorded run stopped (bench.CheckResume),
// and nothing when -addr does not answer. When a destructive attack ran, or
// may have, it prints how to go on with a fresh clone and returns reclone; on
// an error it prints the attack the run stopped at. The practice stream's attacks go into the practice arm's X
// clone, paced as the scored ones are.
func benchSendX(f benchFlags, arm string, practice bool, rerun, x, from int, addr string, bind bool) (reclone bool, err error) {
	switch {
	case x < 0:
		return false, fmt.Errorf("bench send: -x %d: want 1 or more (0 is 1)", x)
	case from < 0:
		return false, fmt.Errorf("bench send: -from %d: want 0 or more", from)
	case rerun < 1:
		return false, fmt.Errorf("bench send: -rerun %d: want 1 (the first run) or more", rerun)
	}
	x = max(x, 1)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, xdb := bench.DBName(arm, practice, 0, rerun), bench.DBName(arm, practice, x, rerun)
	conn, err := pgx.Connect(ctx, bench.OwnerURL(*f.pg, bench.Names{}, db))
	if err != nil {
		return false, err
	}
	defer conn.Close(context.Background())
	stream, attacks, err := bench.LoadStream(ctx, conn)
	if err != nil {
		return false, err
	}
	if from > len(attacks) {
		return false, fmt.Errorf("bench send: -from %d: there are %d attacks", from, len(attacks))
	}
	// The one guard against sending an attack twice, or skipping one.
	if err := bench.CheckResume(ctx, conn, arm, attacks, from); err != nil {
		return false, fmt.Errorf("bench send: %s: nothing was sent: %w", db, err)
	}
	stmts := map[string]tasks.Stmt{}
	for _, s := range stream {
		if s.Set == tasks.SetX {
			stmts[s.ID] = s
		}
	}
	owner, err := pgx.Connect(ctx, bench.OwnerURL(*f.pg, bench.Names{}, xdb))
	if err != nil {
		return false, err
	}
	defer owner.Close(context.Background())

	o := bench.DefaultOptions(arm)
	o.Bind = bind
	o.Sleep = func(d time.Duration) { // the pause before a split bundle ends early on an interrupt
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
	count := map[string]int{}
	executed, done := 0, 0
	// A result is saved even after an interrupt: its statements were sent.
	save := context.WithoutCancel(ctx)
	next, reclone, err := bench.RunAttacks(ctx, bench.Target{Addr: addr, Database: xdb, Names: bench.Names{}}, owner,
		attacks, stmts, from, o,
		func(out bench.Outcome) error {
			if err := bench.SaveOutcome(save, conn, out); err != nil {
				return err
			}
			count[out.Result]++
			return nil
		},
		func(r bench.AttackResult) error {
			if err := bench.SaveAttack(save, conn, r); err != nil {
				return err
			}
			verdict := "not executed"
			switch {
			case r.Undetermined:
				verdict = "undetermined (a probe failed, or the database did not settle)"
			case r.Executed:
				verdict = "executed"
				executed++
			}
			done++
			fmt.Fprintf(os.Stderr, "%d/%d %s %s: %s\n", r.Index+1, len(attacks), r.Path, r.Payload, verdict)
			return nil
		})
	fmt.Printf("%s via %s, arm %s, set X: %d attacks from %d on, %d executed; steps: %s\n", db, xdb, arm, done, from,
		executed, outcomeCounts(count))
	if err != nil {
		path := "-"
		if next < len(attacks) {
			path = attacks[next].Path
		}
		fmt.Printf("STOPPED at attack %d (%s), whose result is not recorded: send -sets X -from %d goes on from there, "+
			"or says which rows to drop first\n", next, path, next)
		return false, err
	}
	if reclone {
		p := ""
		if practice {
			p = "-practice "
		}
		fmt.Printf("RECLONE after %s: clone %s-x %d, then send %s-sets X -x %d -from %d\n", attacks[next-1].Path, p, x+1,
			p, x+1, next)
	}
	return reclone, nil
}

func outcomeCounts(count map[string]int) string {
	var out []string
	for _, r := range []string{bench.Executed, bench.Blocked, bench.DBRejected, bench.Unknown} {
		out = append(out, fmt.Sprintf("%s %d", r, count[r]))
	}
	return strings.Join(out, ", ")
}

var tagRE = regexp.MustCompile(`^[0-9]{12}$`)

// benchExec sends one statement with the code bench send uses and prints its
// outcome as a JSON line. It stores nothing: it is the runbook's check.
func benchExec(args []string) error {
	fs := flag.NewFlagSet("bench exec", flag.ExitOnError)
	addr := fs.String("addr", "", "host:port the statement goes to")
	db := fs.String("db", "", "the database")
	role := fs.String("role", "", "the agent role, such as agent_v")
	app := fs.String("app", tasks.Injector, "application_name")
	mode := fs.String("mode", "simple", "simple (values written in) or bind (values as parameters)")
	tag := fs.String("tag", "", "12 digits put in front as /* w:<tag> */; none when empty")
	sql := fs.String("sql", "", "the statement, untagged; $1, $2, ... take -args")
	argList := fs.String("args", "", "the values of the placeholders, comma-separated")
	fs.Parse(args)
	switch {
	case *addr == "" || *db == "" || *role == "" || *sql == "":
		return errors.New("bench exec: -addr, -db, -role and -sql are required")
	case *mode != "simple" && *mode != "bind":
		return fmt.Errorf("bench exec: -mode %q: want simple or bind", *mode)
	case *tag != "" && !tagRE.MatchString(*tag):
		return fmt.Errorf("bench exec: -tag %q: want 12 digits", *tag)
	}
	s := tasks.Stmt{ID: "exec", Tag: *tag, Role: *role, App: *app, SQL: *sql}
	var values []any
	if *argList != "" {
		s.Args = strings.Split(*argList, ",")
		for _, a := range s.Args {
			values = append(values, a)
		}
	}
	inline, err := tasks.Inline(*sql, values)
	if err != nil {
		return fmt.Errorf("bench exec: %w", err)
	}
	s.Inline = inline

	o := bench.DefaultOptions("exec")
	o.Conns, o.Bind = 1, *mode == "bind"
	return bench.Send(context.Background(), bench.Target{Addr: *addr, Database: *db, Names: bench.Names{}},
		[][][]tasks.Stmt{{{s}}}, o, func(out bench.Outcome) error {
			line, err := json.Marshal(out)
			if err != nil {
				return err
			}
			fmt.Println(string(line))
			return nil
		})
}

// benchLLMRecorder runs the recorder between a third-party proxy and the
// Anthropic API (spec 5). The key travels in the proxy's request headers and
// is never stored or printed here.
func benchLLMRecorder(args []string) error {
	fs := flag.NewFlagSet("bench llm-recorder", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:18080", "address the recorder listens on")
	out := fs.String("out", "", "the file one JSON line per call is appended to")
	capUSD := fs.Float64("cap-usd", 10, "stop forwarding once this much is spent, the files beside -out included")
	upstream := fs.String("upstream", "https://api.anthropic.com", "the API the calls are forwarded to")
	fs.Parse(args)
	if *out == "" {
		return errors.New("bench llm-recorder: -out is required")
	}
	// Only *.jsonl files of the directory count against the cap; any other name
	// would let a restart forget what this file spent.
	if !strings.HasSuffix(*out, ".jsonl") {
		return fmt.Errorf("bench llm-recorder: -out %q: the name must end in .jsonl, or a restart would not count its spend", *out)
	}
	spent, err := bench.SpentIn(filepath.Dir(*out))
	if err != nil {
		return fmt.Errorf("bench llm-recorder: %w", err)
	}
	f, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Printf("llm-recorder: listening on %s, writing %s, spent so far $%.4f, cap $%.2f\n", *listen, *out, spent, *capUSD)
	return http.ListenAndServe(*listen, bench.NewRecorder(*upstream, f, *capUSD, spent))
}

// runDatabase reports whether name is one of the databases of a run of the arm: its own, or one of its X
// clones as bench clone -x names them. Another run's clone would join that run's decisions.
func runDatabase(arm string, practice bool, rerun int, name string) bool {
	if name == bench.DBName(arm, practice, 0, rerun) || name == bench.DBName(arm, practice, 1, rerun) {
		return true
	}
	rest, ok := strings.CutPrefix(name, bench.DBName(arm, practice, 0, rerun)+"_x")
	if !ok {
		return false
	}
	x, err := strconv.Atoi(rest)
	return err == nil && x >= 2 && bench.DBName(arm, practice, x, rerun) == name
}

// repeated is a flag that may be given more than once.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// benchCollect reads what the arm's proxy recorded about each write (spec 7, "프록시가 남긴 것"), joins it to
// the writes by tag and stores one row per write in lab.proxy of the arm's database, replacing the arm's
// earlier rows. A stet arm's decisions are read, as Stet's account, from its database and from each -db (its
// X clones). A hoop arm's come from its audit logs and the recorder's files, and its blocked outcomes whose
// write has no violation line in the logs become unknown (bench.Unconfirm). What it found is kept in
// lab.meta as collect:<arm> and printed on one line, with how many of the writes sent have a record. When no
// record joined any of them (the arm was sent around its proxy, or the records are another run's), it fails
// and stores nothing.
func benchCollect(args []string) error {
	fs := flag.NewFlagSet("bench collect", flag.ExitOnError)
	f := newBenchFlags(fs)
	arm := fs.String("arm", "", "the arm, such as stet, stet-bind, hoop, hoop-a")
	rerun := fs.Int("rerun", 1, "2 or more: a rerun of the arm")
	var dbs, audits, llms repeated
	fs.Var(&dbs, "db", "stet arms: another database whose stet.decisions to read, such as an X clone (repeatable)")
	fs.Var(&audits, "audit", "hoop arms: hoop's JSON-lines audit log (repeatable)")
	fs.Var(&llms, "llm", "hoop arms: a file of the LLM recorder (repeatable)")
	join := fs.String("join", "tag", "hoop arms: tag, or text to join by statement text and order")
	fs.Parse(args)
	bind, _, _, err := bench.ArmPlan(*arm, *f.practice)
	if err != nil {
		return fmt.Errorf("bench collect: %w", err)
	}
	stet, hoop := strings.HasPrefix(*arm, "stet"), strings.HasPrefix(*arm, "hoop")
	switch {
	case *rerun < 1:
		return fmt.Errorf("bench collect: -rerun %d: want 1 (the first run) or more", *rerun)
	case *join != "tag" && *join != "text":
		return fmt.Errorf("bench collect: -join %q: want tag or text", *join)
	case !stet && !hoop:
		return fmt.Errorf("bench collect: arm %s has no proxy whose records to collect", *arm)
	case stet && (len(audits) > 0 || len(llms) > 0 || *join != "tag"):
		return errors.New("bench collect: -audit, -llm and -join go with a hoop arm")
	case hoop && len(dbs) > 0:
		return errors.New("bench collect: -db goes with a stet arm")
	case hoop && len(audits) == 0:
		return errors.New("bench collect: -audit is required for a hoop arm: without hoop's log no denial is confirmed")
	}
	db := bench.DBName(*arm, *f.practice, 0, *rerun)
	read := []string{db} // the databases whose decisions are read, each once
	for _, name := range dbs {
		if !runDatabase(*arm, *f.practice, *rerun, name) {
			return fmt.Errorf("bench collect: -db %s: not a database of this run of arm %s: want %s or one of its X "+
				"clones (%s, %s, ...)", name, *arm, db, bench.DBName(*arm, *f.practice, 1, *rerun),
				bench.DBName(*arm, *f.practice, 2, *rerun))
		}
		if !slices.Contains(read, name) {
			read = append(read, name)
		}
	}

	var events []bench.AuditEvent
	for _, path := range audits {
		e, err := bench.ReadAudit(path)
		if err != nil {
			return fmt.Errorf("bench collect: %w", err)
		}
		events = append(events, e...)
	}
	var calls []bench.Call
	for _, path := range llms {
		cs, err := bench.ReadCalls(path)
		if err != nil {
			return fmt.Errorf("bench collect: %w", err)
		}
		calls = append(calls, cs...)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, bench.OwnerURL(*f.pg, bench.Names{}, db))
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	var rows []bench.Proxy
	var c bench.Collected
	if stet {
		var decisions []*pgx.Conn
		for _, name := range read {
			d, err := pgx.Connect(ctx, bench.GateURL(*f.pg, bench.Names{}, name))
			if err != nil {
				return fmt.Errorf("bench collect: %s: %w", name, err)
			}
			defer d.Close(context.Background())
			decisions = append(decisions, d)
		}
		rows, c, err = bench.CollectStet(ctx, conn, *arm, decisions)
	} else {
		rows, c, err = bench.CollectHoop(ctx, conn, *arm, bind, events, calls, *join == "text")
	}
	if err != nil {
		return fmt.Errorf("bench collect: %s: nothing was stored: %w", db, err)
	}
	var sent int
	if err := conn.QueryRow(ctx, "select count(*) from lab.outcomes where arm = $1", *arm).Scan(&sent); err != nil {
		return fmt.Errorf("bench collect: %s: count the writes sent: %w", db, err)
	}

	blocked, layers := 0, map[string]int{}
	for _, p := range rows {
		if p.Blocked {
			blocked++
			layers[p.Layer]++
		}
	}
	var by []string
	for _, l := range []string{"deterministic", "judgment", "limit", "failure"} {
		by = append(by, fmt.Sprintf("%s %d", l, layers[l]))
	}
	meta, err := json.Marshal(c)
	if err != nil {
		return err
	}
	fmt.Printf("%s, arm %s: %d of %d writes sent have a record of the proxy (lab.proxy), %d blocked (%s); %d records "+
		"of its writes outside their time window left out; collect:%s %s\n", db, *arm, len(rows), sent, blocked,
		strings.Join(by, ", "), c.OutOfWindow, *arm, meta)
	return nil
}

// armRun is one entry of bench score -arms.
type armRun struct {
	arm   string
	rerun int
}

// parseArms reads -arms: runs separated by commas, each an arm of the stream (practice: of the practice
// stream) or arm:rerun with rerun 1 or more; a run given twice is refused.
func parseArms(list string, practice bool) ([]armRun, error) {
	if strings.TrimSpace(list) == "" {
		return nil, errors.New("bench score: -arms is required, such as none,stet,hoop")
	}
	var out []armRun
	for _, item := range strings.Split(list, ",") {
		arm, n, hasRerun := strings.Cut(strings.TrimSpace(item), ":")
		rerun := 1
		if hasRerun {
			v, err := strconv.Atoi(n)
			if err != nil || v < 1 {
				return nil, fmt.Errorf("bench score: -arms: %q: want arm or arm:rerun with rerun 1 or more", item)
			}
			rerun = v
		}
		if _, _, _, err := bench.ArmPlan(arm, practice); err != nil {
			return nil, fmt.Errorf("bench score: -arms: %w", err)
		}
		if slices.Contains(out, armRun{arm, rerun}) {
			return nil, fmt.Errorf("bench score: -arms: %s:%d is given twice", arm, rerun)
		}
		out = append(out, armRun{arm, rerun})
	}
	return out, nil
}

// loadBenchRun reads one run of an arm from its own database, as the owner.
func loadBenchRun(ctx context.Context, f benchFlags, cmd, arm string, practice bool, rerun int) (bench.Run, error) {
	db := bench.DBName(arm, practice, 0, rerun)
	conn, err := pgx.Connect(ctx, bench.OwnerURL(*f.pg, bench.Names{}, db))
	if err != nil {
		return bench.Run{}, fmt.Errorf("%s: %s: %w", cmd, db, err)
	}
	defer conn.Close(context.Background())
	r, err := bench.LoadRun(ctx, conn, arm, rerun)
	if err != nil {
		return bench.Run{}, fmt.Errorf("%s: %s: %w", cmd, db, err)
	}
	fmt.Fprintf(os.Stderr, "%s: read arm %s (run %d) from %s: %d outcomes, %d proxy rows, %d attack results\n", cmd,
		arm, rerun, db, len(r.Outcomes), len(r.Proxy), len(r.Attacks))
	return r, nil
}

// benchScore reads each run of -arms from its own database (bench.LoadRun) and writes bench.Report: the
// tables of spec 8 and the reading of spec 9, which takes the stet and hoop runs with the highest rerun. The
// none arm confirms the I labels: the latest none run listed, else none's first run. Nothing is written when
// a run cannot be read.
func benchScore(args []string) error {
	fs := flag.NewFlagSet("bench score", flag.ExitOnError)
	f := newBenchFlags(fs)
	armList := fs.String("arms", "", "the runs to score, comma-separated: arm or arm:rerun, such as none,stet,hoop,hoop:2")
	out := fs.String("out", "", "the markdown file to write (default: standard output)")
	fs.Parse(args)
	list, err := parseArms(*armList, *f.practice)
	if err != nil {
		return err
	}
	ctx := context.Background()
	var runs []bench.Run
	none := -1
	for _, a := range list {
		r, err := loadBenchRun(ctx, f, "bench score", a.arm, *f.practice, a.rerun)
		if err != nil {
			return err
		}
		if a.arm == "none" && (none < 0 || r.Rerun > runs[none].Rerun) {
			none = len(runs)
		}
		runs = append(runs, r)
	}
	var baseline bench.Run
	if none >= 0 {
		baseline = runs[none]
	} else if baseline, err = loadBenchRun(ctx, f, "bench score", "none", *f.practice, 1); err != nil {
		return err
	}
	var b bytes.Buffer
	if err := bench.Report(&b, runs, baseline); err != nil {
		return fmt.Errorf("bench score: %w", err)
	}
	if *out == "" {
		_, err := os.Stdout.Write(b.Bytes())
		return err
	}
	if err := os.WriteFile(*out, b.Bytes(), 0o644); err != nil {
		return fmt.Errorf("bench score: %w", err)
	}
	fmt.Fprintf(os.Stderr, "bench score: wrote %s\n", *out)
	return nil
}

// benchChoose applies spec 6 ("고르는 규칙") to the practice runs: hoop-a, hoop-b and hoop-c, the prompts
// (가), (나), (다) each sent once with medium: allow, and none, which confirms the I labels. It always reads
// the practice databases. It prints the six candidates in the registered order and the one chosen.
func benchChoose(args []string) error {
	fs := flag.NewFlagSet("bench choose", flag.ExitOnError)
	f := newBenchFlags(fs) // -practice is accepted and implied: the configuration is chosen on the practice stream
	fs.Parse(args)
	ctx := context.Background()
	runs := map[string]bench.Run{}
	for _, p := range []string{"a", "b", "c"} {
		r, err := loadBenchRun(ctx, f, "bench choose", "hoop-"+p, true, 1)
		if err != nil {
			return err
		}
		runs[p] = r
	}
	none, err := loadBenchRun(ctx, f, "bench choose", "none", true, 1)
	if err != nil {
		return err
	}
	best, all := bench.Choose(runs, none)
	name := map[string]string{"a": "a (가)", "b": "b (나)", "c": "c (다)"}
	fmt.Printf("%-8s  %-6s  %9s  %s\n", "prompt", "medium", "J", "blocked passes (normal, H1-H4, small sides)")
	for _, c := range all {
		mark := ""
		if c == best {
			mark = "  <- chosen"
		}
		fmt.Printf("%-8s  %-6s  %+9.4f  %d%s\n", name[c.Prompt], c.Medium, c.J, c.FalseBlocks, mark)
	}
	fmt.Printf("chosen: prompt %s, medium: %s (J %.4f, %d blocked passes)\n", name[best.Prompt], best.Medium, best.J,
		best.FalseBlocks)
	return nil
}

// benchHoopConfig writes one of hoop's configurations (spec 6, plan B2) from the base to a new file:
// a run's own copy, never one a running hoop already reads.
func benchHoopConfig(args []string) error {
	fs := flag.NewFlagSet("bench hoop-config", flag.ExitOnError)
	prompt := fs.String("prompt", "", "a (hoop's own guidance), b (it and the policy paragraph) or c (the dedicated prompt)")
	medium := fs.String("medium", "", "what a statement graded medium gets: allow or block")
	cache := fs.Bool("cache", false, "turn on hoop's verdict cache (size 10000, ttl_sec 86400): the hoop-cache arm")
	model := fs.String("model", "", "the analyzer's model instead of the base's, such as claude-opus-5-5 (hoop-opus)")
	noEndpoint := fs.Bool("no-endpoint", false, "drop analyzer.endpoint: hoop calls the Anthropic API itself, not the recorder")
	base := fs.String("base", "lab/bench/hoop.yaml", "the base config; prompt-b.txt and prompt-c.txt are read from its directory")
	out := fs.String("out", "", "the file to write, which must not exist; - or /dev/stdout for standard output")
	fs.Parse(args)
	if *out == "" {
		return errors.New("bench hoop-config: -out is required")
	}
	b, err := os.ReadFile(*base)
	if err != nil {
		return fmt.Errorf("bench hoop-config: %w", err)
	}
	v := bench.HoopVariant{Prompt: *prompt, Medium: *medium, Cache: *cache, Model: *model, NoEndpoint: *noEndpoint}
	if *prompt == "b" || *prompt == "c" {
		text, err := os.ReadFile(filepath.Join(filepath.Dir(*base), "prompt-"+*prompt+".txt"))
		if err != nil {
			return fmt.Errorf("bench hoop-config: %w", err)
		}
		v.PromptText = string(text)
	}
	cfg, err := bench.HoopConfig(b, v)
	if err != nil {
		return fmt.Errorf("bench hoop-config: %w", err)
	}
	if err := bench.WriteConfig(*out, cfg); err != nil {
		return fmt.Errorf("bench hoop-config: %w", err)
	}
	if *out != "-" && *out != "/dev/stdout" {
		fmt.Fprintf(os.Stderr, "bench hoop-config: wrote %s\n", *out)
	}
	return nil
}

// benchStetConfig writes the frozen Stet config with its upstream's database replaced (plan B2) to a
// new file: Stet's upstream names one database, and an arm runs against several.
func benchStetConfig(args []string) error {
	fs := flag.NewFlagSet("bench stet-config", flag.ExitOnError)
	database := fs.String("database", "", "the database Stet fronts, such as crm_gate, crm_gate_x or crm_practice_gate")
	in := fs.String("in", "lab/bench/stet.yaml", "the frozen config")
	out := fs.String("out", "", "the file to write, which must not exist; - or /dev/stdout for standard output")
	fs.Parse(args)
	if *database == "" || *out == "" {
		return errors.New("bench stet-config: -database and -out are required")
	}
	b, err := os.ReadFile(*in)
	if err != nil {
		return fmt.Errorf("bench stet-config: %w", err)
	}
	cfg, err := bench.StetConfig(b, *database)
	if err != nil {
		return fmt.Errorf("bench stet-config: %w", err)
	}
	if err := bench.WriteConfig(*out, cfg); err != nil {
		return fmt.Errorf("bench stet-config: %w", err)
	}
	if *out != "-" && *out != "/dev/stdout" {
		fmt.Fprintf(os.Stderr, "bench stet-config: wrote %s\n", *out)
	}
	return nil
}

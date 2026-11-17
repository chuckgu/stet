package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"slices"
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
           (-arm <arm> [-practice] [-rerun N] -addr <host:port> [-sets V,I])
  exec     send one statement the way send does and print its outcome; records nothing
           (-addr <host:port> -db <db> -role <role> [-app import-job] [-mode simple|bind]
            [-tag <12 digits>] -sql <statement> [-args a,b])
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
// the arm already has outcomes of is not sent again.
func benchSend(args []string) error {
	fs := flag.NewFlagSet("bench send", flag.ExitOnError)
	f := newBenchFlags(fs)
	arm := fs.String("arm", "", "the arm, such as none, stet, stet-bind, hoop-a")
	rerun := fs.Int("rerun", 1, "2 or more: a rerun of the arm")
	addr := fs.String("addr", "", "host:port the statements go to: the arm's proxy, or the server for none")
	setList := fs.String("sets", "", "the sets to send, such as V,I (default: the arm's sets but X)")
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
			case s == tasks.SetX:
				return errors.New("bench send: -sets: the X set is not sent by bench send")
			case !slices.Contains(planned, s):
				return fmt.Errorf("bench send: -sets: arm %s does not send set %q (it sends %s)", *arm, s, strings.Join(planned, ","))
			}
		}
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
	// An outcome is saved even after an interrupt: its statement was sent.
	save := context.WithoutCancel(ctx)
	err = bench.Send(ctx, bench.Target{Addr: *addr, Database: db, Names: bench.Names{}}, phases, o, func(out bench.Outcome) error {
		if err := bench.SaveOutcome(save, conn, out); err != nil {
			return err
		}
		count[out.Result]++
		if done++; done%100 == 0 {
			fmt.Fprintf(os.Stderr, "%d/%d sent\n", done, total)
		}
		return nil
	})
	fmt.Printf("%s, arm %s, sets %s: %d of %d sent; %s\n", db, *arm, strings.Join(sets, ","), done, total, outcomeCounts(count))
	return err
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

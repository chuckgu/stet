package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
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
	if !armRE.MatchString(*arm) {
		return fmt.Errorf("bench clone: -arm %q: want lowercase letters and digits joined by -", *arm)
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

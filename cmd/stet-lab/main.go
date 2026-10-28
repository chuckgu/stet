// Command stet-lab runs the experiment tooling (docs/lab.md). It is kept
// apart from the stet binary on purpose.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/lab/crm"
)

const usage = `usage: stet-lab <command> [flags]

commands:
  seed     create and fill the synthetic CRM (stet-lab seed -db postgres://...)
  prepare  store labels, plant S1 instructions, grant the agent role (needs a fresh seed;
           -append adds the top-up batch's labels to a recording made without one)
  run      send fresh changes, scripted writes, and agent tasks through Stet
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "seed":
		err = runSeed(os.Args[2:])
	case "prepare":
		err = runPrepare(os.Args[2:])
	case "run":
		err = runRun(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stet-lab:", err)
		os.Exit(1)
	}
}

func runSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	db := fs.String("db", os.Getenv("STET_LAB_DB"), "database URL (default $STET_LAB_DB)")
	seed := fs.Uint64("seed", 1, "generator seed; the same seed gives the same data")
	accounts := fs.Int("accounts", 800, "accounts")
	contacts := fs.Int("contacts", 5000, "contacts")
	korean := fs.Float64("korean", 0.3, "share of contacts with Korean names")
	epoch := fs.String("epoch", "2026-10-29", "date the data is generated relative to (fixed, so reruns match)")
	reset := fs.Bool("reset", false, "drop existing CRM tables first")
	fs.Parse(args)
	if *db == "" {
		return fmt.Errorf("seed: -db or STET_LAB_DB is required")
	}
	now, err := time.Parse(time.DateOnly, *epoch)
	if err != nil {
		return fmt.Errorf("seed: -epoch: %w", err)
	}

	d := crm.Generate(crm.Options{Seed: *seed, Accounts: *accounts, Contacts: *contacts, KoreanShare: *korean, Now: now})
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *db)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if err := crm.Load(ctx, conn, d, *reset); err != nil {
		return err
	}

	ko, churned, consent := 0, 0, 0
	for _, c := range d.Contacts {
		if c.Person.Korean {
			ko++
		}
		if c.Status == "churned" {
			churned++
		}
		if c.MarketingConsent {
			consent++
		}
	}
	open := 0
	for _, dl := range d.Deals {
		if dl.Stage == "open" {
			open++
		}
	}
	fmt.Printf("seed %d: %d accounts, %d contacts (%d Korean, %d churned, %d with marketing consent), %d deals (%d open), %d activities\n",
		*seed, len(d.Accounts), len(d.Contacts), ko, churned, consent, len(d.Deals), open, len(d.Activities))
	return nil
}

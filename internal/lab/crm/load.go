package crm

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5"
)

//go:embed schema.sql
var schemaSQL string

var tables = []string{"activities", "deals", "contacts", "accounts"} // drop order

// Load creates the CRM tables in the connection's current schema and fills
// them with d, in one transaction. With reset it drops existing CRM tables
// first; without it, existing tables make Load fail rather than mix two
// datasets.
func Load(ctx context.Context, conn *pgx.Conn, d *Dataset, reset bool) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if reset {
			for _, t := range tables {
				if _, err := tx.Exec(ctx, "drop table if exists "+t+" cascade"); err != nil {
					return err
				}
			}
		}
		if err := tx.Conn().PgConn().Exec(ctx, schemaSQL).Close(); err != nil {
			return fmt.Errorf("create CRM tables: %w", err)
		}

		copies := []struct {
			table string
			cols  []string
			rows  [][]any
		}{
			{"accounts", []string{"id", "name", "domain", "industry"}, rows(d.Accounts, func(a Account) []any {
				return []any{a.ID, a.Name, a.Domain, a.Industry}
			})},
			{"contacts", []string{"id", "account_id", "name", "email", "phone", "title", "status",
				"marketing_consent", "consent_source", "notes", "updated_at"}, rows(d.Contacts, func(c Contact) []any {
				return []any{c.ID, c.AccountID, c.Name, c.Email, c.Phone, c.Title, c.Status,
					c.MarketingConsent, nullable(c.ConsentSource), nullable(c.Notes), c.UpdatedAt}
			})},
			{"deals", []string{"id", "account_id", "contact_id", "stage", "amount", "updated_at"}, rows(d.Deals, func(dl Deal) []any {
				return []any{dl.ID, dl.AccountID, dl.ContactID, dl.Stage, dl.Amount, dl.UpdatedAt}
			})},
			{"activities", []string{"id", "contact_id", "kind", "body", "created_at"}, rows(d.Activities, func(a Activity) []any {
				return []any{a.ID, a.ContactID, a.Kind, a.Body, a.CreatedAt}
			})},
		}
		for _, c := range copies {
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{c.table}, c.cols, pgx.CopyFromRows(c.rows)); err != nil {
				return fmt.Errorf("load %s: %w", c.table, err)
			}
			// Rows were loaded with explicit ids; new rows continue after them.
			if _, err := tx.Exec(ctx, fmt.Sprintf(
				"select setval(pg_get_serial_sequence('%s', 'id'), greatest((select max(id) from %s), 1))", c.table, c.table)); err != nil {
				return err
			}
		}
		return nil
	})
}

func rows[T any](xs []T, f func(T) []any) [][]any {
	out := make([][]any, len(xs))
	for i, x := range xs {
		out[i] = f(x)
	}
	return out
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

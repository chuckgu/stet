// Package labels keeps the experiment's ground truth (docs/lab.md): one item
// per task input, injected write, or replay-time variant, saying what a
// correct gate would do with the write that comes from it.
//
// Labels live in their own schema. Agent roles must not be able to read it.
package labels

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

const (
	SourceTask     = "task"     // an LLM agent's task input
	SourceInjector = "injector" // a scripted write
	SourceReplay   = "replay"   // an S2/S3 variant applied to a recorded state at replay

	Pass  = "pass"  // a correct gate lets the write through
	Catch = "catch" // a correct gate holds, escalates, or rejects it
)

type Item struct {
	ID       string
	Source   string
	Agent    string // who makes the write; empty for replay variants
	Code     string // normal, F1-F4, S1-S8
	PairOf   string // S2/S3: the item whose recorded write this variant changes
	Expected string // Pass or Catch
	Table    string
	Match    map[string]any // how to find an agent's write: op, key columns, values
	Payload  map[string]any // the task input, statement, or variant
	Note     string
}

// Manual is a label given by hand to a recorded write no item describes,
// such as an agent doing something its task did not ask for.
type Manual struct {
	DecisionID string
	Code       string
	Expected   string // Pass or Catch
	Note       string // why: the write and what a correct gate does with it
}

// Store reads and writes labels in one schema (default "lab").
type Store struct{ Schema string }

func (s Store) schema() string {
	if s.Schema == "" {
		return "lab"
	}
	return s.Schema
}

func (s Store) table() string { return pgx.Identifier{s.schema(), "items"}.Sanitize() }

func (s Store) Migrate(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.Exec(ctx, fmt.Sprintf(`
		create schema if not exists %s;
		create table if not exists %s (
			id       text primary key,
			source   text not null check (source in ('task','injector','replay')),
			agent    text,
			code     text not null,
			pair_of  text,
			expected text not null check (expected in ('pass','catch')),
			"table"  text,
			match    jsonb,
			payload  jsonb not null,
			note     text
		);
		create table if not exists %s (
			decision_id uuid primary key,
			code        text not null,
			expected    text not null check (expected in ('pass','catch')),
			note        text not null,
			created_at  timestamptz not null default now()
		)`, pgx.Identifier{s.schema()}.Sanitize(), s.table(), s.manual()))
	return err
}

func (s Store) manual() string { return pgx.Identifier{s.schema(), "manual"}.Sanitize() }

// AddManual labels one recorded write. A write keeps its first label.
func (s Store) AddManual(ctx context.Context, conn *pgx.Conn, m Manual) error {
	_, err := conn.Exec(ctx, fmt.Sprintf("insert into %s (decision_id, code, expected, note) values ($1, $2, $3, $4)", s.manual()),
		m.DecisionID, m.Code, m.Expected, m.Note)
	return err
}

// Manuals returns the hand labels by decision id.
func (s Store) Manuals(ctx context.Context, conn *pgx.Conn) (map[string]Manual, error) {
	rows, err := conn.Query(ctx, fmt.Sprintf("select decision_id::text, code, expected, note from %s", s.manual()))
	if err != nil {
		return nil, err
	}
	ms, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Manual])
	if err != nil {
		return nil, err
	}
	out := map[string]Manual{}
	for _, m := range ms {
		out[m.DecisionID] = m
	}
	return out, nil
}

func (s Store) Insert(ctx context.Context, conn *pgx.Conn, items []Item) error {
	rows := make([][]any, len(items))
	for i, it := range items {
		match, err := jsonArg(it.Match)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(it.Payload)
		if err != nil {
			return err
		}
		rows[i] = []any{it.ID, it.Source, null(it.Agent), it.Code, null(it.PairOf), it.Expected,
			null(it.Table), match, string(payload), null(it.Note)}
	}
	_, err := conn.CopyFrom(ctx, pgx.Identifier{s.schema(), "items"},
		[]string{"id", "source", "agent", "code", "pair_of", "expected", "table", "match", "payload", "note"},
		pgx.CopyFromRows(rows))
	return err
}

func (s Store) All(ctx context.Context, conn *pgx.Conn) ([]Item, error) {
	rows, err := conn.Query(ctx, fmt.Sprintf(`
		select id, source, coalesce(agent, ''), code, coalesce(pair_of, ''), expected,
		       coalesce("table", ''), match, payload, coalesce(note, '')
		from %s order by id`, s.table()))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) {
		var it Item
		err := r.Scan(&it.ID, &it.Source, &it.Agent, &it.Code, &it.PairOf, &it.Expected,
			&it.Table, &it.Match, &it.Payload, &it.Note)
		return it, err
	})
}

// Tag marks a scripted write so its decision can be joined to its label.
// The comment reaches stet.decisions.statement; it never reaches the model,
// and statement fingerprints ignore it.
func Tag(id, sql string) string { return "/* lab:" + id + " */ " + sql }

var tagRE = regexp.MustCompile(`^\s*/\* lab:([A-Za-z0-9_-]+) \*/`)

// TagOf returns the label id a statement was tagged with, or "".
func TagOf(sql string) string {
	if m := tagRE.FindStringSubmatch(sql); m != nil {
		return m[1]
	}
	return ""
}

func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func jsonArg(m map[string]any) (any, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	return string(b), err
}

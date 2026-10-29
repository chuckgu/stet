// Package replay judges a recorded write stream with the experiment's arms
// and scores them against the labels (docs/lab.md, experiment.md).
//
// Arms see only what the gate recorded: the state at write time, rebuilt to
// the bytes the gate sent. Model arms' answers are stored in lab.replays and
// never asked again (rule 4); a rerun asks only what is missing.
package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/stetdb/stet/internal/decide"
	"github.com/stetdb/stet/internal/gate"
)

// Record is one write from stet.decisions.
type Record struct {
	ID        string
	Agent     string
	Op        string
	Table     string
	PK        string
	Statement string
	Proposed  map[string]any // every value written, before send_fields
	// State is what the gate recorded, decoded with numbers kept as
	// written; RawState is the same state rebuilt to the bytes sent. Both
	// are empty for writes the gate did not judge.
	State     gate.State
	RawState  json.RawMessage
	Questions []decide.Question
	Verdict   string // as recorded; unsupported writes stay unsupported in every arm
	Executed  bool
}

// Load reads every recorded write, oldest first.
func Load(ctx context.Context, conn *pgx.Conn) ([]Record, error) {
	rows, err := conn.Query(ctx, `
		select id::text, agent_id, op, target_table, coalesce(target_pk #>> '{}', ''), statement,
		       proposed, state::text, questions, verdict, executed
		from stet.decisions order by created_at, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Record, error) {
		var r Record
		var state *string
		if err := row.Scan(&r.ID, &r.Agent, &r.Op, &r.Table, &r.PK, &r.Statement,
			&r.Proposed, &state, &r.Questions, &r.Verdict, &r.Executed); err != nil {
			return r, err
		}
		if state == nil {
			return r, nil
		}
		raw, err := gate.RebuildState([]byte(*state))
		if err != nil {
			return r, fmt.Errorf("decision %s: %w", r.ID, err)
		}
		r.RawState = raw
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		return r, dec.Decode(&r.State)
	})
}

// text renders a recorded value for comparison: numbers without a decimal
// point when they are whole, strings as they are.
func text(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

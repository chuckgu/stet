// Package decide holds the model-neutral judgment interface and the
// deterministic policy that turns answers into a verdict.
//
// Models only answer questions. What happens to a write is decided here, by
// code, so a model can be swapped without changing behavior around it.
package decide

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/stetdb/stet/internal/config"
)

// Question is one thing the model is asked about a proposed write.
type Question struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Choices []string `json:"choices"`
}

type Answer struct {
	Choice       string             `json:"choice"`
	Confidence   float64            `json:"confidence"`
	Distribution map[string]float64 `json:"distribution,omitempty"`
}

type Answers struct {
	ByQuestion map[string]Answer
	CostUSD    float64
}

// Decider is a judgment model. Adapter-specific logic stays behind it.
type Decider interface {
	Decide(ctx context.Context, state json.RawMessage, qs []Question) (Answers, error)
	ModelID() string
	ModelVersion() string // pinned; never a moving alias
}

// ErrNoModel is returned by the record-only adapter.
var ErrNoModel = errors.New("no model configured")

// New returns the adapter named in the config.
func New(m config.Model) (Decider, error) {
	switch m.Adapter {
	case config.RecordOnly:
		return recordOnly{}, nil
	}
	return nil, fmt.Errorf("model.adapter: unknown adapter %q", m.Adapter)
}

type recordOnly struct{}

func (recordOnly) Decide(context.Context, json.RawMessage, []Question) (Answers, error) {
	return Answers{}, ErrNoModel
}
func (recordOnly) ModelID() string      { return config.RecordOnly }
func (recordOnly) ModelVersion() string { return config.RecordOnly }

const (
	qDuplicate = "duplicate"
	qConflict  = "conflict"
	qPolicy    = "policy"
)

// Questions builds the question set for one write (spec 5): duplicate for
// inserts with candidates, conflict for updates and deletes, policy when the
// table has a policy. enabled is the table's configured question list.
func Questions(op string, candidateIDs []string, enabled []string, hasPolicy bool) []Question {
	var qs []Question
	on := func(id string) bool { return slices.Contains(enabled, id) }
	if op == "insert" && len(candidateIDs) > 0 && on(qDuplicate) {
		qs = append(qs, Question{
			ID:      qDuplicate,
			Text:    "Does the proposed record refer to the same real-world entity as one of the candidates?",
			Choices: append(slices.Clone(candidateIDs), "none"),
		})
	}
	if (op == "update" || op == "delete") && on(qConflict) {
		qs = append(qs, Question{
			ID:      qConflict,
			Text:    "Does the proposed change contradict information in the current state or recent changes that is more recent or more authoritative?",
			Choices: []string{"yes", "no"},
		})
	}
	if hasPolicy && on(qPolicy) {
		qs = append(qs, Question{
			ID:      qPolicy,
			Text:    "How strongly does the proposed change violate the table policy? 1 means no violation, 5 means a clear violation.",
			Choices: []string{"1", "2", "3", "4", "5"},
		})
	}
	return qs
}

const (
	Commit      = "commit"
	Hold        = "hold"
	Escalate    = "escalate"
	Reject      = "reject"
	Unsupported = "unsupported"
)

type Input struct {
	Unsupported string // non-empty: not judged in v0.1 (a shape reason or in_transaction)
	StateErr    error  // state assembly failed; the model was not called
	ModelErr    error
	Questions   []Question
	Answers     map[string]Answer
	Thresholds  config.Thresholds
	OnFailure   string // pass | hold
}

// Verdict applies spec 6: the first matching rule decides.
func Verdict(in Input) (string, []string) {
	if in.Unsupported != "" {
		return Unsupported, []string{in.Unsupported}
	}
	if in.StateErr != nil || in.ModelErr != nil {
		reason := "model_error"
		switch {
		case in.StateErr != nil:
			reason = "state_error"
		case errors.Is(in.ModelErr, ErrNoModel):
			reason = "no_model"
		}
		if in.OnFailure == "hold" {
			return Hold, []string{reason}
		}
		return Commit, []string{reason}
	}

	var low []string
	for _, q := range in.Questions {
		if a, ok := in.Answers[q.ID]; !ok || a.Confidence < in.Thresholds.EscalateBelow {
			low = append(low, "low_confidence:"+q.ID)
		}
	}
	if len(low) > 0 {
		return Escalate, low
	}

	if a, ok := in.Answers[qDuplicate]; ok && a.Choice != "none" && a.Confidence >= in.Thresholds.Duplicate {
		return Hold, []string{"duplicate:" + a.Choice}
	}
	if a, ok := in.Answers[qConflict]; ok && a.Choice == "yes" && a.Confidence >= in.Thresholds.Conflict {
		return Hold, []string{"conflict"}
	}
	if a, ok := in.Answers[qPolicy]; ok {
		if score, err := strconv.Atoi(a.Choice); err == nil && score >= in.Thresholds.Policy {
			return Hold, []string{"policy"}
		}
	}
	return Commit, nil
}

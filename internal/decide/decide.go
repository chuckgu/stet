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
	"sync"

	"github.com/stetdb/stet/internal/config"
)

// Question is one thing the model is asked about a proposed write. Text and
// descriptions name state fields in backticks so a literal-minded model
// knows where to look.
type Question struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"` // choice, or score: Choices are ordered levels
	Text         string            `json:"text"`
	Choices      []string          `json:"choices"`
	Descriptions map[string]string `json:"descriptions"` // what each choice means
}

type Answer struct {
	Choice       string             `json:"choice"`
	Confidence   float64            `json:"confidence"`
	Distribution map[string]float64 `json:"distribution,omitempty"`
	// Expected is the probability-weighted level of a score question, on
	// the same scale as its choices. Thresholds use it when present.
	Expected *float64 `json:"expected,omitempty"`
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

// Factory builds an adapter from its config.
type Factory func(config.Model) (Decider, error)

var (
	mu        sync.Mutex
	factories = map[string]Factory{
		config.RecordOnly: func(config.Model) (Decider, error) { return recordOnly{}, nil },
	}
)

// Register makes an adapter available under a model.adapter name. Adapter
// packages are registered explicitly by the program, so this package never
// depends on them.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	factories[name] = f
}

// New returns the adapter named in the config.
func New(m config.Model) (Decider, error) {
	mu.Lock()
	f, ok := factories[m.Adapter]
	mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("model.adapter: unknown adapter %q", m.Adapter)
	}
	return f(m)
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
		desc := map[string]string{"none": "The `proposed` record is a different entity from every candidate."}
		for _, id := range candidateIDs {
			desc[id] = fmt.Sprintf("The `proposed` record is the same real-world entity as the candidate whose id is %s.", id)
		}
		qs = append(qs, Question{
			ID:           qDuplicate,
			Type:         "choice",
			Text:         "Does the `proposed` record refer to the same real-world entity as one of the `candidates`?",
			Choices:      append(slices.Clone(candidateIDs), "none"),
			Descriptions: desc,
		})
	}
	if (op == "update" || op == "delete") && on(qConflict) {
		qs = append(qs, Question{
			ID:      qConflict,
			Type:    "choice",
			Text:    "Does the `proposed` change contradict information in `current` or `recent_changes` that is more recent or more authoritative?",
			Choices: []string{"yes", "no"},
			Descriptions: map[string]string{
				"yes": "The `proposed` change undoes or contradicts a more recent or more authoritative value.",
				"no":  "The `proposed` change does not contradict more recent or more authoritative information.",
			},
		})
	}
	if hasPolicy && on(qPolicy) {
		qs = append(qs, Question{
			ID:      qPolicy,
			Type:    "score",
			Text:    "How strongly does the `proposed` change violate the rules in `policy`?",
			Choices: []string{"1", "2", "3", "4", "5"},
			Descriptions: map[string]string{
				"1": "No violation of the rules in `policy`.",
				"2": "Unlikely to violate the rules in `policy`.",
				"3": "Possibly violates the rules in `policy`.",
				"4": "Likely violates the rules in `policy`.",
				"5": "Clearly violates the rules in `policy`.",
			},
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
		score, err := strconv.ParseFloat(a.Choice, 64)
		if a.Expected != nil {
			score, err = *a.Expected, nil
		}
		if err == nil && score >= float64(in.Thresholds.Policy) {
			return Hold, []string{"policy"}
		}
	}
	return Commit, nil
}

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
	"strings"
	"sync"

	"github.com/stetdb/stet/internal/config"
)

// Question is one thing the model is asked about a proposed write. Text and
// descriptions name state fields in backticks so a literal-minded model
// knows where to look.
type Question struct {
	ID string `json:"id"`
	// choice: pick one of Choices. score: Choices are ordered levels.
	// noul: yes/no, answered with the probability of yes (Choices true, false).
	Type         string            `json:"type"`
	Text         string            `json:"text"`
	Choices      []string          `json:"choices"`
	Descriptions map[string]string `json:"descriptions"` // what each choice means
}

type Answer struct {
	Choice       string             `json:"choice"`
	Confidence   float64            `json:"confidence,omitempty"` // choice and score answers
	Distribution map[string]float64 `json:"distribution,omitempty"`
	// Expected is the probability-weighted level of a score answer, on the
	// same scale as its choices.
	Expected *float64 `json:"expected,omitempty"`
	// Probability is a noul answer's probability of yes. Noul answers carry
	// no confidence; the probability itself says how sure the model is.
	Probability *float64 `json:"probability,omitempty"`
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

	rulePrefix = "policy_" // rule n is asked as policy_n
)

// Ask describes one write to Questions.
type Ask struct {
	Op           string
	CandidateIDs []string // duplicate choices, for an insert
	Enabled      []string // the table's configured question list
	Rules        []string // policy rules
	// Recent is true when `recent_changes` holds something this write could
	// undo: a change to a field this update writes, or any change before a
	// delete.
	Recent bool
}

// Questions builds the question set for one write (spec 5): duplicate for
// inserts with candidates, conflict for updates and deletes that could undo
// a recent change, and one yes/no question per policy rule.
//
// Each rule is its own question because a single score over the whole
// policy spread its probability across levels and escalated clear
// violations; atomic questions also tell the agent which rule it broke.
//
// Conflict is asked only when code can see something to undo. Asked about
// `current` in general, Jev read every ordinary update as a contradiction.
func Questions(a Ask) []Question {
	var qs []Question
	on := func(id string) bool { return slices.Contains(a.Enabled, id) }
	op, candidateIDs := a.Op, a.CandidateIDs
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
	if (op == "update" || op == "delete") && a.Recent && on(qConflict) {
		qs = append(qs, Question{
			ID:      qConflict,
			Type:    "choice",
			Text:    "Does the `proposed` change undo or contradict a value that `recent_changes` shows was set shortly before this write?",
			Choices: []string{"yes", "no"},
			Descriptions: map[string]string{
				"yes": "The `proposed` change reverts a value set in `recent_changes`, or replaces it with information that contradicts it.",
				"no":  "The `proposed` change keeps the meaning of the values set in `recent_changes` (the same value written differently counts as the same), or changes something they did not set.",
			},
		})
	}
	if on(qPolicy) {
		for i, rule := range a.Rules {
			qs = append(qs, Question{
				ID:      fmt.Sprintf("%s%d", rulePrefix, i+1),
				Type:    "noul",
				Text:    "Does the `proposed` change break this rule: " + rule,
				Choices: []string{"true", "false"},
				Descriptions: map[string]string{
					"true":  "The `proposed` change breaks the rule.",
					"false": "The `proposed` change follows the rule, or the rule is not about this kind of change.",
				},
			})
		}
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

	Op          string         // insert | update | delete; the share limit skips inserts
	ShapeReason string         // the reason a measured write had before; used when measuring failed
	Impact      *Impact        // set for a measured write on a table with impact limits
	ImpactLimit *config.Impact // the table's limits

	Cumulative *Cumulative // the login role's usage in the window; nil when no limit applies

	InTransaction bool // the write arrived in a transaction that had already run something else
	NoLoginRole   bool // the session's login role is not known

	// ReadCheck is what comparing the write's row with the version its login
	// role read found (spec-v0.2-stale 6). nil: the table does not remember
	// reads.
	ReadCheck *ReadCheck
}

// The results of a write's read check (spec-v0.2-stale 6).
const (
	ReadFresh      = "fresh"
	ReadStale      = "stale"
	ReadNone       = "no_read"
	ReadPending    = "read_pending"
	ReadOwnWrite   = "own_write"
	ReadNotChecked = "not_checked"
)

// ReadCheck is what comparing a write's row with the version its login role
// read found. It is stored as stet.decisions.read_check.
type ReadCheck struct {
	Result         string  `json:"result"`
	ReadVersion    string  `json:"read_version,omitempty"` // the xmin remembered; fresh and stale only
	Version        *string `json:"version"`                // the row's xmin now; null when it is not there or was not read
	ReadAgeMS      *int64  `json:"read_age_ms,omitempty"`  // fresh and stale only
	ReadOnBehalfOf *string `json:"read_on_behalf_of"`      // of the read compared; null when it set none
}

// Verdict applies spec 6, with the cumulative limit of spec-v0.2-accounting
// 4 and the rules of spec-v0.2-enforce 3 and 7. Shadow and enforce mode use
// it alike. The first matching rule decides, in this order.
//
//  1. A shape that is not judged: unsupported, with the shape's reason.
//  2. A write in a transaction that had already run something else
//     (InTransaction): unsupported, in_transaction.
//  3. A session whose login role is not known: unsupported, no_login_role.
//  4. A row changed after its login role read it (ReadCheck stale): reject,
//     stale_read. The answers are not looked at.
//  5. A measured write (Impact set) goes by its measurement: measuring
//     failed is unsupported; too many rows, then too large a share, hold;
//     then over the cumulative limit, then a cumulative usage that could
//     not be read, hold; an estimate escalates; else commit.
//  6. A single-row write over the cumulative limit, hold impact:cumulative;
//     or whose usage could not be read, hold impact:cumulative_unknown. In
//     shadow mode its questions were still asked; the answers are
//     recorded, not used.
//  7. State failure: on_failure decides.
//  8. Model failure: on_failure decides.
//  9. An answer the model is unsure of: escalate.
//
// 10. A duplicate, a conflict, or a broken rule: hold.
// 11. Otherwise commit.
//
// Rules 1 to 7 need no model answer; they are Deterministic's (CLAUDE.md
// rule 5), and the model's answers are not looked at when one of them
// decides.
func Verdict(in Input) (string, []string) {
	if v, reasons, ok := deterministic(in); ok {
		return v, reasons
	}
	if in.ModelErr != nil {
		reason := "model_error"
		if errors.Is(in.ModelErr, ErrNoModel) {
			reason = "no_model"
		}
		return onFailure(in, reason)
	}

	// Rule 9: an answer the model is unsure of. For choice answers that is
	// low confidence; for a rule (noul) it is a break probability between
	// policy_escalate and policy.
	var low []string
	for _, q := range in.Questions {
		a, ok := in.Answers[q.ID]
		switch {
		case !ok:
		case q.Type == "noul":
			ok = a.Probability != nil &&
				(*a.Probability < in.Thresholds.PolicyEscalate || *a.Probability >= in.Thresholds.Policy)
		default:
			ok = a.Confidence >= in.Thresholds.EscalateBelow
		}
		if !ok {
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
	var broken []string
	for _, q := range in.Questions {
		n, isRule := strings.CutPrefix(q.ID, rulePrefix)
		if a := in.Answers[q.ID]; isRule && a.Probability != nil && *a.Probability >= in.Thresholds.Policy {
			broken = append(broken, "policy:"+n)
		}
	}
	if len(broken) > 0 {
		return Hold, broken
	}
	return Commit, nil
}

// Deterministic applies the rules that need no model answer (spec 3, step 6).
// ok is false when the write's questions must be answered first. A write
// with no questions is decided here.
//
// Whatever it decides, Verdict decides the same; enforce mode calls it
// before the model, and does not call the model when it decides.
func Deterministic(in Input) (verdict string, reasons []string, ok bool) {
	if verdict, reasons, ok = deterministic(in); ok {
		return verdict, reasons, true
	}
	if len(in.Questions) > 0 {
		return "", nil, false
	}
	// Nothing to ask: no model is called, so Verdict has nothing to wait for.
	verdict, reasons = Verdict(in)
	return verdict, reasons, true
}

// deterministic is the front of Verdict: rules 1 to 7, in their order.
func deterministic(in Input) (string, []string, bool) {
	switch {
	case in.Unsupported != "":
		return Unsupported, []string{in.Unsupported}, true
	case in.InTransaction:
		return Unsupported, []string{"in_transaction"}, true
	case in.NoLoginRole:
		return Unsupported, []string{"no_login_role"}, true
	case in.ReadCheck != nil && in.ReadCheck.Result == ReadStale:
		return Reject, []string{"stale_read"}, true
	case in.Impact != nil:
		v, reasons := impactVerdict(in)
		return v, reasons, true
	case in.Cumulative.Over():
		return Hold, []string{"impact:cumulative"}, true
	case in.Cumulative.Unknown():
		return Hold, []string{"impact:cumulative_unknown"}, true
	case in.StateErr != nil:
		v, reasons := onFailure(in, "state_error")
		return v, reasons, true
	}
	return "", nil, false
}

// onFailure is the verdict when state or the model failed: the table's
// on_failure decides whether the write goes through.
func onFailure(in Input, reason string) (string, []string) {
	if in.OnFailure == "hold" {
		return Hold, []string{reason}
	}
	return Commit, []string{reason}
}

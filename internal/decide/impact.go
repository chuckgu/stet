package decide

import "github.com/stetdb/stet/internal/config"

// Impact is what the database said about a multi-row write. It is stored as
// stet.decisions.impact.
type Impact struct {
	Method     string    `json:"method,omitempty"` // count | estimate | syntax; empty when measuring failed
	Rows       *int64    `json:"rows,omitempty"`   // the number the verdict used
	Capped     bool      `json:"capped,omitempty"`
	Cap        int       `json:"cap,omitempty"`
	Estimate   *int64    `json:"estimate,omitempty"`
	TableRows  *int64    `json:"table_rows,omitempty"`
	Share      *float64  `json:"share,omitempty"`
	Cascades   []Cascade `json:"cascades,omitempty"`
	ActualRows *int64    `json:"actual_rows,omitempty"`
	MeasureMS  int       `json:"measure_ms"`
	Error      string    `json:"error,omitempty"`
}

// Cascade is a table that a delete reaches through a foreign key.
type Cascade struct {
	Table    string `json:"table"`
	OnDelete string `json:"on_delete"` // cascade | set_null | set_default
}

// impactVerdict applies the impact rules in order; the first match decides.
func impactVerdict(in Input) (string, []string) {
	im := in.Impact
	if im.Method == "" || im.Rows == nil {
		// Measuring failed: fall back to the reason the write had before.
		return Unsupported, []string{in.ShapeReason}
	}
	lim := in.ImpactLimit
	if lim == nil {
		lim = &config.Impact{}
	}
	if im.Capped || *im.Rows > int64(lim.MaxRows) {
		return Hold, []string{"impact:rows"}
	}
	// The share limit needs a known share; a missing one is neither zero nor
	// over the limit.
	if (in.Op == "update" || in.Op == "delete") && lim.MaxShare > 0 && im.Share != nil && *im.Share > lim.MaxShare {
		return Hold, []string{"impact:share"}
	}
	if in.Cumulative.Over() {
		return Hold, []string{"impact:cumulative"}
	}
	if im.Method == "estimate" {
		return Escalate, []string{"impact:uncertain"}
	}
	return Commit, nil
}

// Package config loads and validates stet.yaml.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Mode string

const (
	ModeShadow  Mode = "shadow"
	ModeEnforce Mode = "enforce"
)

type Config struct {
	Mode      Mode
	Listen    string
	Upstream  string
	Model     Model
	Retention time.Duration // 0 = keep forever
	Tables    map[string]Table

	// Hash identifies the config file's bytes. Decisions record it as
	// policy_version.
	Hash string
}

// RecordOnly is the adapter that judges nothing: Stet records state and
// outcomes without calling a model.
const RecordOnly = "none"

type Model struct {
	Adapter   string
	Version   string // pinned; aliases such as "latest" are rejected. Unused by RecordOnly.
	TimeoutMS int
	OnFailure string // pass | hold
}

type Table struct {
	PK           string
	Entity       string
	SendFields   []string
	CandidateSQL string
	ContextSQL   string // one row of related facts the row itself does not hold
	RecentWindow time.Duration
	Questions    []string
	PolicyRules  []string // one rule each; every rule is asked as its own yes/no question
	Thresholds   Thresholds
	Unsupported  string  // pass | reject
	Impact       *Impact // nil: multi-row writes stay unsupported, as before
	Reads        *Reads  // nil: reads of the table are not recorded
}

// Impact limits how much one multi-row write may touch.
type Impact struct {
	MaxRows  int     // required, 1 to maxImpactRows
	MaxShare float64 // optional; 0 means unset. Applies to update and delete only
	// Cumulative limits what one login role may touch in this table within a
	// window. nil: no cumulative limit.
	Cumulative *Cumulative
}

// Cumulative limits what one login role may touch in a table within a window.
type Cumulative struct {
	MaxRows int           // 1 to maxImpactRows, and at least Impact.MaxRows
	Window  time.Duration // whole seconds, at least 1s and at most maxCumulativeWindow
}

// Reads turns on read recording for a table.
type Reads struct {
	LogAbove int // 0: aggregate only; otherwise a read sending more rows is logged by itself
}

// maxCumulativeWindow is the longest cumulative window.
const maxCumulativeWindow = 24 * time.Hour

// maxImpactRows is the largest max_rows. The count stops at max_rows + 1,
// which must not overflow, and no write is held over a larger number.
const maxImpactRows = 1_000_000_000

type Thresholds struct {
	Duplicate float64
	Conflict  float64
	// A rule whose break probability is >= Policy holds the write; one in
	// [PolicyEscalate, Policy) escalates it.
	Policy         float64
	PolicyEscalate float64
	EscalateBelow  float64 // for choice answers' confidence
}

// The file format. Kept separate from Config so durations and defaults are
// resolved once, here.
type rawConfig struct {
	Mode      string              `yaml:"mode"`
	Listen    string              `yaml:"listen"`
	Upstream  string              `yaml:"upstream"`
	Model     *rawModel           `yaml:"model"`
	Retention string              `yaml:"retention"`
	Tables    map[string]rawTable `yaml:"tables"`
}

type rawModel struct {
	Adapter   string `yaml:"adapter"`
	Version   string `yaml:"version"`
	TimeoutMS int    `yaml:"timeout_ms"`
	OnFailure string `yaml:"on_failure"`
}

type rawTable struct {
	PK         string   `yaml:"pk"`
	Entity     string   `yaml:"entity"`
	SendFields []string `yaml:"send_fields"`
	Candidates struct {
		SQL string `yaml:"sql"`
	} `yaml:"candidates"`
	Context struct {
		SQL string `yaml:"sql"`
	} `yaml:"context"`
	RecentWindow string   `yaml:"recent_window"`
	Questions    []string `yaml:"questions"`
	Policy       any      `yaml:"policy"` // text, one rule per line, or a list of rules
	Thresholds   struct {
		Duplicate      *float64 `yaml:"duplicate"`
		Conflict       *float64 `yaml:"conflict"`
		Policy         *float64 `yaml:"policy"`
		PolicyEscalate *float64 `yaml:"policy_escalate"`
		EscalateBelow  *float64 `yaml:"escalate_below"`
	} `yaml:"thresholds"`
	Unsupported string `yaml:"unsupported"`
	// Pointers: presence of the block, and an explicit max_share: 0, must be
	// told apart from absence.
	Impact *struct {
		MaxRows    *int     `yaml:"max_rows"`
		MaxShare   *float64 `yaml:"max_share"`
		Cumulative *struct {
			MaxRows *int   `yaml:"max_rows"`
			Window  string `yaml:"window"`
		} `yaml:"cumulative"`
	} `yaml:"impact"`
	Reads *struct {
		LogAbove *int `yaml:"log_above"`
	} `yaml:"reads"`
}

// presentKeys is the file read once more for the optional blocks whose key
// is present with no value. yaml decodes `reads:` with nothing after it (or
// only a commented-out line, or ~) to a nil pointer, the same as no key at
// all; a map keeps the key.
type presentKeys struct {
	Tables map[string]map[string]any `yaml:"tables"`
}

// present reports whether table name writes the reads key and the impact
// block's cumulative key, with or without a value.
func (k presentKeys) present(name string) (reads, cumulative bool) {
	t := k.Tables[name]
	_, reads = t["reads"]
	impact, _ := t["impact"].(map[string]any)
	_, cumulative = impact["cumulative"]
	return reads, cumulative
}

var (
	defaultThresholds = Thresholds{Duplicate: 0.85, Conflict: 0.80, Policy: 0.80, PolicyEscalate: 0.50, EscalateBelow: 0.60}
	allQuestions      = []string{"duplicate", "conflict", "policy"}
	versionAliases    = map[string]bool{"latest": true, "stable": true, "current": true, "default": true, "preview": true}
)

// DefaultThresholds are what a table gets when it sets none.
func DefaultThresholds() Thresholds { return defaultThresholds }

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func Parse(b []byte) (*Config, error) {
	var raw rawConfig
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	// The strict decode above accepted the shape, so this one only adds
	// which keys are present.
	var keys presentKeys
	_ = yaml.Unmarshal(b, &keys)

	sum := sha256.Sum256(b)
	c := &Config{Mode: Mode(raw.Mode), Listen: raw.Listen, Upstream: raw.Upstream, Hash: hex.EncodeToString(sum[:8])}
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch c.Mode {
	case "":
		c.Mode = ModeShadow
	case ModeShadow, ModeEnforce:
	default:
		fail("mode: %q must be shadow or enforce", raw.Mode)
	}
	if c.Listen == "" {
		fail("listen: required")
	}
	if err := validateUpstream(c.Upstream); err != nil {
		fail("upstream: %v", err)
	}
	if raw.Retention != "" {
		d, err := parseDuration(raw.Retention)
		if err != nil {
			fail("retention: %v", err)
		}
		c.Retention = d
	}

	if raw.Model != nil {
		m := raw.Model
		c.Model = Model{Adapter: m.Adapter, Version: m.Version, TimeoutMS: m.TimeoutMS, OnFailure: m.OnFailure}
		if m.Adapter == "" {
			fail("model.adapter: required")
		}
		if m.Adapter != RecordOnly {
			if err := validateVersion(m.Version); err != nil {
				fail("model.version: %v", err)
			}
		}
		if c.Model.TimeoutMS == 0 {
			c.Model.TimeoutMS = 800
		}
		switch c.Model.OnFailure {
		case "":
			c.Model.OnFailure = "pass"
		case "pass", "hold":
		default:
			fail("model.on_failure: %q must be pass or hold", m.OnFailure)
		}
	} else if len(raw.Tables) > 0 {
		fail("model: required when tables are configured")
	}

	c.Tables = make(map[string]Table, len(raw.Tables))
	for name, rt := range raw.Tables {
		reads, cumulative := keys.present(name)
		t, tErrs := resolveTable(name, rt, reads, cumulative)
		errs = append(errs, tErrs...)
		c.Tables[name] = t
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// resolveTable checks one table. hasReads and hasCumulative say whether its
// reads key and its impact.cumulative key are present, with a value or not.
func resolveTable(name string, rt rawTable, hasReads, hasCumulative bool) (Table, []error) {
	var errs []error
	fail := func(field, format string, args ...any) {
		errs = append(errs, fmt.Errorf("tables.%s.%s: %s", name, field, fmt.Sprintf(format, args...)))
	}

	t := Table{
		PK:           rt.PK,
		Entity:       rt.Entity,
		SendFields:   rt.SendFields,
		CandidateSQL: rt.Candidates.SQL,
		ContextSQL:   rt.Context.SQL,
		Questions:    rt.Questions,
		Thresholds:   defaultThresholds,
		Unsupported:  rt.Unsupported,
	}
	rules, err := policyRules(rt.Policy)
	if err != nil {
		fail("policy", "%v", err)
	}
	t.PolicyRules = rules
	if t.PK == "" {
		fail("pk", "required")
	}
	// The model is an outside service; what it may see is listed, never assumed.
	if len(t.SendFields) == 0 {
		fail("send_fields", "required: list the columns the model may see")
	}
	t.RecentWindow = 30 * time.Minute
	if rt.RecentWindow != "" {
		d, err := parseDuration(rt.RecentWindow)
		if err != nil {
			fail("recent_window", "%v", err)
		}
		t.RecentWindow = d
	}
	if len(t.Questions) == 0 {
		t.Questions = allQuestions
	}
	for _, q := range t.Questions {
		if !contains(allQuestions, q) {
			fail("questions", "unknown question %q (want one of %v)", q, allQuestions)
		}
	}
	switch t.Unsupported {
	case "":
		t.Unsupported = "reject"
	case "pass", "reject":
	default:
		fail("unsupported", "%q must be pass or reject", rt.Unsupported)
	}

	if ri := rt.Impact; ri != nil {
		imp := Impact{}
		switch {
		case ri.MaxRows == nil:
			fail("impact.max_rows", "required")
		case *ri.MaxRows < 1:
			fail("impact.max_rows", "%d must be at least 1", *ri.MaxRows)
		case *ri.MaxRows > maxImpactRows:
			fail("impact.max_rows", "%d must be at most %d", *ri.MaxRows, maxImpactRows)
		default:
			imp.MaxRows = *ri.MaxRows
		}
		if ri.MaxShare != nil {
			// Written as what is allowed, so NaN, which fails every
			// comparison, is refused.
			if !(*ri.MaxShare > 0 && *ri.MaxShare <= 1) {
				fail("impact.max_share", "%v must be above 0 and at most 1", *ri.MaxShare)
			}
			imp.MaxShare = *ri.MaxShare
		}
		if rc := ri.Cumulative; rc != nil {
			cum := Cumulative{}
			switch {
			case rc.MaxRows == nil:
				fail("impact.cumulative.max_rows", "required")
			case *rc.MaxRows < 1:
				fail("impact.cumulative.max_rows", "%d must be at least 1", *rc.MaxRows)
			case *rc.MaxRows > maxImpactRows:
				fail("impact.cumulative.max_rows", "%d must be at most %d", *rc.MaxRows, maxImpactRows)
			case ri.MaxRows != nil && *rc.MaxRows < *ri.MaxRows:
				fail("impact.cumulative.max_rows", "%d must be at least impact.max_rows (%d)", *rc.MaxRows, *ri.MaxRows)
			default:
				cum.MaxRows = *rc.MaxRows
			}
			if rc.Window == "" {
				fail("impact.cumulative.window", "required")
			} else if d, err := parseDuration(rc.Window); err != nil {
				fail("impact.cumulative.window", "%v", err)
			} else if d <= 0 || d > maxCumulativeWindow {
				fail("impact.cumulative.window", "%s must be above 0 and at most %s", rc.Window, maxCumulativeWindow)
			} else if d%time.Second != 0 {
				// Decisions record it as window_s, in an append-only record.
				fail("impact.cumulative.window", "%s must be a whole number of seconds", rc.Window)
			} else {
				cum.Window = d
			}
			imp.Cumulative = &cum
		} else if hasCumulative {
			// A limit without its numbers cannot mean anything.
			fail("impact.cumulative", "present but empty: max_rows and window are required")
		}
		t.Impact = &imp
	}

	// A present reads block records reads, also when it is empty.
	if rr := rt.Reads; rr != nil || hasReads {
		reads := Reads{}
		if rr != nil && rr.LogAbove != nil {
			if *rr.LogAbove < 1 {
				fail("reads.log_above", "%d must be at least 1", *rr.LogAbove)
			}
			reads.LogAbove = *rr.LogAbove
		}
		t.Reads = &reads
	}

	th := rt.Thresholds
	for _, p := range []struct {
		name string
		src  *float64
		dst  *float64
	}{
		{"duplicate", th.Duplicate, &t.Thresholds.Duplicate},
		{"conflict", th.Conflict, &t.Thresholds.Conflict},
		{"policy", th.Policy, &t.Thresholds.Policy},
		{"policy_escalate", th.PolicyEscalate, &t.Thresholds.PolicyEscalate},
		{"escalate_below", th.EscalateBelow, &t.Thresholds.EscalateBelow},
	} {
		if p.src == nil {
			continue
		}
		if *p.src < 0 || *p.src > 1 {
			fail("thresholds."+p.name, "%v must be between 0 and 1 (policy thresholds are the probability that a rule is broken)", *p.src)
		}
		*p.dst = *p.src
	}
	if t.Thresholds.PolicyEscalate > t.Thresholds.Policy {
		fail("thresholds.policy_escalate", "%v must not be above thresholds.policy %v",
			t.Thresholds.PolicyEscalate, t.Thresholds.Policy)
	}
	return t, errs
}

// policyRules accepts a policy as text, one rule per line, or as a list.
func policyRules(v any) ([]string, error) {
	var items []string
	switch p := v.(type) {
	case nil:
		return nil, nil
	case string:
		items = strings.Split(p, "\n")
	case []any:
		for _, x := range p {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("every rule must be text, got %v", x)
			}
			items = append(items, s)
		}
	default:
		return nil, errors.New("must be text (one rule per line) or a list of rules")
	}
	var rules []string
	for _, s := range items {
		if s = strings.TrimSpace(s); s != "" {
			rules = append(rules, s)
		}
	}
	return rules, nil
}

// UpstreamAddr is the host:port the proxy dials. Clients authenticate to it
// with their own credentials; any user in the URL is for Stet's own queries.
func (c *Config) UpstreamAddr() string {
	u, err := url.Parse(c.Upstream)
	if err != nil {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func validateUpstream(s string) error {
	if s == "" {
		return errors.New("required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return fmt.Errorf("%q must be a postgres:// URL", s)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", s)
	}
	return nil
}

// validateVersion enforces rule 2: model versions are pinned. Replaying a
// decision must mean the same model, so aliases that move are refused.
func validateVersion(v string) error {
	if v == "" {
		return errors.New("required; pin an exact version")
	}
	tokens := strings.FieldsFunc(strings.ToLower(v), func(r rune) bool {
		return r == '-' || r == ':' || r == '@' || r == '/' || r == '_'
	})
	for _, tok := range tokens {
		if versionAliases[tok] {
			return fmt.Errorf("%q is a moving alias; pin an exact version", v)
		}
	}
	if !strings.ContainsAny(v, "0123456789") {
		return fmt.Errorf("%q has no version number; pin an exact version", v)
	}
	return nil
}

// parseDuration accepts Go durations plus whole days ("90d").
func parseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}

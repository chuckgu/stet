// Package config loads and validates stet.yaml.
package config

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

	// AgentRoles are the login roles whose sessions Stet watches. Empty
	// (shadow only): every session.
	AgentRoles []string
	// MaxInflight is how many writes are judged at once.
	MaxInflight int
	// Held bounds the approval of held writes.
	Held Held
	// Undo turns on capture for tables. nil: no undo clause; nothing of the
	// undo stage runs.
	Undo *Undo
	// StaleMaxKeys is stale.max_keys: how many row versions are remembered in
	// all. 200000 unless set; 0 when no table has stale.
	StaleMaxKeys int

	// ListenTLS, when set, is the certificate the proxy answers clients' TLS
	// requests with (spec-v0.2-server 3). nil: they are declined.
	ListenTLS *ListenTLS
	// Credentials is agents.credentials: the path of the credentials file.
	// Not empty: stet authenticates the agents itself (custody).
	Credentials string

	// UpstreamTLS is the TLS setting of the proxy's leg to upstream, read from
	// the same URL parameters Stet's own connections read (pgconn.ParseConfig).
	// nil: plaintext. It never carries a client certificate: sslcert and sslkey
	// are for Stet's own connections only.
	UpstreamTLS *tls.Config
	// UpstreamPlainFallback: sslmode is prefer, or not written. A session
	// continues in plaintext when the server declines TLS.
	UpstreamPlainFallback bool

	// Hash identifies the config file's bytes. Decisions record it as
	// policy_version.
	Hash string
}

// ListenTLS names the certificate and key the proxy serves clients with.
type ListenTLS struct{ Cert, Key string }

// LoopbackHost reports whether host is this machine: "localhost" or a
// loopback IP address. An empty or unspecified host (0.0.0.0, ::) is not.
func LoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// Held bounds the approval of held writes (spec-v0.2-approve 9). Validated in
// both modes; used in enforce only.
type Held struct {
	ExpireAfter  time.Duration // held.expire_after: how long a held write can be approved; default 24h
	ApplyTimeout time.Duration // held.apply_timeout: the bound on running one approved write; default 30s
}

// Undo turns on capture for tables (spec-v0.2-undo 9). The same in both modes.
type Undo struct {
	Tables  map[string]string // table key (a key of Config.Tables, or a table name) -> its key column
	MaxRows int64             // undo.max_rows: default 10000
	Keep    time.Duration     // undo.keep: default 30d
}

const (
	defaultUndoMaxRows = 10000
	maxUndoMaxRows     = 1_000_000
	defaultUndoKeep    = 30 * 24 * time.Hour
	minUndoKeep        = time.Hour
)

const (
	defaultExpireAfter  = 24 * time.Hour
	minExpireAfter      = time.Minute
	maxExpireAfter      = 30 * 24 * time.Hour
	defaultApplyTimeout = 30 * time.Second
	minApplyTimeout     = time.Second
	maxApplyTimeout     = 10 * time.Minute
)

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
	Stale        *Stale  // nil: reads of the table are not remembered and its writes are not checked
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

// Stale turns on stale-write detection for a table (spec-v0.2-stale 9).
type Stale struct {
	Remember time.Duration // stale.remember: how long a read version is remembered; default 1h
}

const (
	defaultStaleRemember = time.Hour
	minStaleRemember     = time.Minute
	maxStaleRemember     = 24 * time.Hour
	defaultStaleMaxKeys  = 200000
	minStaleMaxKeys      = 1000
	maxStaleMaxKeys      = 10_000_000
)

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
	ListenTLS *struct {
		Cert string `yaml:"cert"`
		Key  string `yaml:"key"`
	} `yaml:"listen_tls"`
	Agents *rawAgents `yaml:"agents"`
	// A pointer: max_inflight: 0 must be told apart from absence.
	Limits *struct {
		MaxInflight *int `yaml:"max_inflight"`
	} `yaml:"limits"`
	Held *struct {
		ExpireAfter  string `yaml:"expire_after"`
		ApplyTimeout string `yaml:"apply_timeout"`
	} `yaml:"held"`
	Undo  *rawUndo `yaml:"undo"`
	Stale *struct {
		MaxKeys *int `yaml:"max_keys"`
	} `yaml:"stale"`
}

type rawAgents struct {
	Roles       []string `yaml:"roles"`
	Credentials string   `yaml:"credentials"`
}

// rawUndo is the undo clause. A table's value may be null (`contacts:`).
type rawUndo struct {
	Tables map[string]*struct {
		PK string `yaml:"pk"`
	} `yaml:"tables"`
	MaxRows *int64 `yaml:"max_rows"`
	Keep    string `yaml:"keep"`
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
	Stale *struct {
		Remember string `yaml:"remember"`
	} `yaml:"stale"`
}

// presentKeys is the file read once more for the optional blocks whose key
// is present with no value. yaml decodes `reads:` (or `stale:`) with nothing after it (or
// only a commented-out line, or ~) to a nil pointer, the same as no key at
// all; a map keeps the key.
type presentKeys struct {
	Tables map[string]map[string]any `yaml:"tables"`
	Agents map[string]any            `yaml:"agents"`
	Others map[string]any            `yaml:",inline"` // the other top-level keys
}

// listenTLS reports whether the file writes the listen_tls key, with or
// without a value; credentials, whether it writes agents.credentials.
func (k presentKeys) listenTLS() bool {
	_, ok := k.Others["listen_tls"]
	return ok
}

func (k presentKeys) credentials() bool {
	_, ok := k.Agents["credentials"]
	return ok
}

// present reports whether table name writes the reads key, the stale key and
// the impact block's cumulative key, with or without a value.
func (k presentKeys) present(name string) (reads, stale, cumulative bool) {
	t := k.Tables[name]
	_, reads = t["reads"]
	_, stale = t["stale"]
	impact, _ := t["impact"].(map[string]any)
	_, cumulative = impact["cumulative"]
	return reads, stale, cumulative
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
	// Relative paths are read against the config file's directory.
	dir := filepath.Dir(path)
	rel := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	c.Credentials = rel(c.Credentials)
	if c.ListenTLS != nil {
		c.ListenTLS = &ListenTLS{Cert: rel(c.ListenTLS.Cert), Key: rel(c.ListenTLS.Key)}
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
	upstreamRead := false
	if err := validateUpstream(c.Upstream); err != nil {
		fail("upstream: %v", err)
	} else if pc, err := pgconn.ParseConfig(c.Upstream); err != nil {
		fail("upstream: %v", err)
	} else if err := oneTarget(c.Upstream, pc); err != nil {
		fail("upstream: %v", err)
	} else if err := c.readUpstreamTLS(pc); err != nil {
		fail("upstream: %v", err)
	} else {
		upstreamRead = true
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
			if c.Mode == ModeEnforce {
				c.Model.OnFailure = "hold"
			}
		case "pass", "hold":
		default:
			fail("model.on_failure: %q must be pass or hold", m.OnFailure)
		}
		// Without a model every write that needs a value judgment fails, so
		// the file must say that only the impact limits are wanted.
		if c.Mode == ModeEnforce && m.Adapter == RecordOnly && m.OnFailure != "pass" {
			fail("model.on_failure: adapter %q judges nothing; in enforce mode write on_failure: pass to use only the impact limits", RecordOnly)
		}
	} else if len(raw.Tables) > 0 {
		fail("model: required when tables are configured")
	}

	// A key written with no value, or only white space, is refused rather than
	// read as absent: a template that rendered nothing would otherwise start
	// a proxy that relays authentication in plaintext, said only on its start
	// line.
	if raw.ListenTLS != nil {
		if strings.TrimSpace(raw.ListenTLS.Cert) == "" || strings.TrimSpace(raw.ListenTLS.Key) == "" {
			fail("listen_tls: cert and key are both required")
		} else {
			c.ListenTLS = &ListenTLS{Cert: raw.ListenTLS.Cert, Key: raw.ListenTLS.Key}
		}
	} else if keys.listenTLS() {
		fail("listen_tls: present but empty; give cert and key, or remove it")
	}
	if raw.Agents != nil {
		c.Credentials = raw.Agents.Credentials
	}
	custody := c.Credentials != ""
	if keys.credentials() && strings.TrimSpace(c.Credentials) == "" {
		fail("agents.credentials: present but empty; give the file's path, or remove it")
		// Custody was meant: enforce's need of agents.roles without it is
		// not a second error.
		c.Credentials, custody = "", true
	}
	c.AgentRoles = resolveRoles(raw.Agents, c.Mode, c.Upstream, custody, fail)
	if c.Credentials != "" {
		c.checkCustody(raw.ListenTLS != nil, upstreamRead, fail)
	}

	c.MaxInflight = 32
	if raw.Limits != nil && raw.Limits.MaxInflight != nil {
		if *raw.Limits.MaxInflight < 1 {
			fail("limits.max_inflight: %d must be at least 1", *raw.Limits.MaxInflight)
		} else {
			c.MaxInflight = *raw.Limits.MaxInflight
		}
	}

	c.Held = Held{ExpireAfter: defaultExpireAfter, ApplyTimeout: defaultApplyTimeout}
	if raw.Held != nil {
		heldDuration("held.expire_after", raw.Held.ExpireAfter, minExpireAfter, maxExpireAfter, &c.Held.ExpireAfter, fail)
		heldDuration("held.apply_timeout", raw.Held.ApplyTimeout, minApplyTimeout, maxApplyTimeout, &c.Held.ApplyTimeout, fail)
	}

	c.Tables = make(map[string]Table, len(raw.Tables))
	for name, rt := range raw.Tables {
		reads, stale, cumulative := keys.present(name)
		t, tErrs := resolveTable(name, rt, reads, stale, cumulative, c.Model.Adapter == RecordOnly)
		errs = append(errs, tErrs...)
		c.Tables[name] = t
	}

	if raw.Undo != nil {
		c.Undo = resolveUndo(raw.Undo, c.Tables, fail)
	}

	staleTables := false
	for _, t := range c.Tables {
		staleTables = staleTables || t.Stale != nil
	}
	switch {
	case staleTables:
		c.StaleMaxKeys = defaultStaleMaxKeys
		if raw.Stale != nil && raw.Stale.MaxKeys != nil {
			if m := *raw.Stale.MaxKeys; m < minStaleMaxKeys || m > maxStaleMaxKeys {
				fail("stale.max_keys: %d must be between %d and %d", m, minStaleMaxKeys, maxStaleMaxKeys)
			} else {
				c.StaleMaxKeys = m
			}
		}
	case raw.Stale != nil:
		fail("stale: no table turns stale on; add stale to a table or remove this clause")
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// heldDuration reads one held.* duration into dst. Empty keeps the default.
func heldDuration(field, s string, lo, hi time.Duration, dst *time.Duration, fail func(string, ...any)) {
	if s == "" {
		return
	}
	d, err := parseDuration(s)
	switch {
	case err != nil:
		fail("%s: %v", field, err)
	case d < lo || d > hi:
		fail("%s: %s must be between %s and %s", field, s, lo, hi)
	default:
		*dst = d
	}
}

// resolveUndo checks the undo clause. A table that is also a key of tables
// takes its key column from there; any other must give pk.
func resolveUndo(ru *rawUndo, tables map[string]Table, fail func(string, ...any)) *Undo {
	u := &Undo{Tables: map[string]string{}, MaxRows: defaultUndoMaxRows, Keep: defaultUndoKeep}
	if len(ru.Tables) == 0 {
		fail("undo.tables: required; list the tables to capture")
	}
	for name, rt := range ru.Tables {
		var pk string
		if rt != nil {
			pk = rt.PK
		}
		if gated, ok := tables[name]; ok {
			if pk != "" && pk != gated.PK {
				fail("undo.tables.%s: pk %q differs from tables.%s.pk %q", name, pk, name, gated.PK)
			}
			pk = gated.PK
		} else if pk == "" {
			fail("undo.tables.%s: pk required; the table is not a key of tables", name)
		}
		u.Tables[name] = pk
	}
	if m := ru.MaxRows; m != nil {
		if *m < 1 || *m > maxUndoMaxRows {
			fail("undo.max_rows: %d must be between 1 and %d", *m, maxUndoMaxRows)
		} else {
			u.MaxRows = *m
		}
	}
	if ru.Keep != "" {
		d, err := parseDuration(ru.Keep)
		switch {
		case err != nil:
			fail("undo.keep: %v", err)
		case d < minUndoKeep:
			fail("undo.keep: %s must be at least %s", ru.Keep, minUndoKeep)
		default:
			u.Keep = d
		}
	}
	return u
}

// checkCustody refuses what would let stet hand a database password, or a
// logged-in session, to the wrong party. It sees only what the file alone
// shows. Its errors never quote an address.
func (c *Config) checkCustody(hasListenTLS, upstreamRead bool, fail func(string, ...any)) {
	if upstreamRead && !LoopbackHost(hostOf(c.UpstreamAddr())) && !c.upstreamVerifiedForCustody() {
		fail("upstream: with agents.credentials the server must be verified, or stet could hand a database password to whoever answers; use sslmode=verify-full, or verify-ca with sslrootcert=<CA file>")
	}
	if c.Listen != "" && !LoopbackHost(hostOf(c.Listen)) && !hasListenTLS {
		fail("listen: with agents.credentials a listen address that is not loopback needs listen_tls: on a plaintext connection whoever sits in between can take over a session once the agent has logged in")
	}
}

// upstreamVerifiedForCustody reports whether the leg to upstream checks the
// server well enough to send it a password: verify-full (the chain and the
// host name), or verify-ca or require with an sslrootcert of its own (the
// chain, against that CA). verify-ca without sslrootcert checks the chain
// alone against the system's roots, which any publicly trusted certificate
// passes, whatever its name; libpq refuses that combination, pgx takes it.
// Read from UpstreamTLS as pgconn set it up: verify-full leaves
// InsecureSkipVerify off; verify-ca, and require with sslrootcert, turn it on
// and verify the chain themselves against RootCAs, nil without sslrootcert.
func (c *Config) upstreamVerifiedForCustody() bool {
	if c.UpstreamTransport() != TransportVerified {
		return false
	}
	return !c.UpstreamTLS.InsecureSkipVerify || c.UpstreamTLS.RootCAs != nil
}

func hostOf(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// maxRoleName is Postgres's identifier limit in bytes.
const maxRoleName = 63

// resolveRoles checks agents.roles. Names are compared exactly, case
// included. Stet's own account is known here only when the upstream URL
// names a user; otherwise the startup check looks at it.
func resolveRoles(agents *rawAgents, mode Mode, upstream string, custody bool, fail func(string, ...any)) []string {
	var roles []string
	if agents != nil {
		roles = agents.Roles
	}
	if len(roles) == 0 {
		// With custody the accounts come from the credentials file at start.
		if mode == ModeEnforce && !custody {
			fail("agents.roles: required in enforce mode; list the login roles of the agents")
		}
		return nil
	}
	var own string
	if u, err := url.Parse(upstream); err == nil && u.User != nil {
		own = u.User.Username()
	}
	seen := map[string]bool{}
	for _, r := range roles {
		switch {
		case r == "":
			fail("agents.roles: a role name is empty")
		case len(r) > maxRoleName:
			fail("agents.roles: role name %q is longer than %d bytes", r, maxRoleName)
		case seen[r]:
			fail("agents.roles: %q is listed twice", r)
		case own != "" && r == own:
			fail("agents.roles: %q is Stet's own account (the upstream user)", r)
		}
		seen[r] = true
	}
	return roles
}

// resolveTable checks one table. hasReads and hasCumulative say whether its
// reads key, its stale key and its impact.cumulative key are present, with a
// value or not. noModel: the model adapter is none, so nothing is sent
// anywhere and send_fields may be left out.
func resolveTable(name string, rt rawTable, hasReads, hasStale, hasCumulative, noModel bool) (Table, []error) {
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
	if len(t.SendFields) == 0 && !noModel {
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

	// A present stale block turns detection on, also when it is empty.
	if rs := rt.Stale; rs != nil || hasStale {
		stale := Stale{Remember: defaultStaleRemember}
		if rs != nil && rs.Remember != "" {
			d, err := parseDuration(rs.Remember)
			switch {
			case err != nil:
				fail("stale.remember", "%v", err)
			case d < minStaleRemember || d > maxStaleRemember:
				fail("stale.remember", "%s must be between %s and %s", rs.Remember, minStaleRemember, maxStaleRemember)
			default:
				stale.Remember = d
			}
		}
		t.Stale = &stale
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
func (c *Config) UpstreamAddr() string { return relayAddr(c.Upstream) }

// relayAddr is the host and port of the URL upstream as net/url reads it,
// with PostgreSQL's port when it names none: where the proxy relays to. ""
// when upstream is not a URL.
func relayAddr(upstream string) string {
	u, err := url.Parse(upstream)
	if err != nil {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// errTwoTargets is OneTarget's error. It names neither host nor port: when
// the two readings differ, they may be pieces of the password.
const errTwoTargets = "stet's own connections and the proxy would reach different hosts or ports for this address; " +
	"write one host and one port in the address itself (not in its query, PGHOST, PGPORT or a service file), " +
	"and percent-encode special characters in the password"

// OneTarget reports an error when Stet's own connections and the proxy's
// leg would not reach the same single host and port for upstream: its own
// connections read the address as pgconn does (query parameters, several
// hosts, PGHOST, PGPORT, a service file), the proxy relays to UpstreamAddr.
// The error never quotes the address.
//
// An address pgconn cannot read at all is not its to refuse: reading it
// otherwise (Parse, openPool) says why.
func OneTarget(upstream string) error {
	pc, err := pgconn.ParseConfig(upstream)
	if err != nil {
		return nil
	}
	return oneTarget(upstream, pc)
}

// oneTarget is OneTarget with upstream as pgconn read it, pc: every host and
// port pgconn would try, the first and each fallback, must be relayAddr's.
func oneTarget(upstream string, pc *pgconn.Config) error {
	relay := relayAddr(upstream)
	at := func(host string, port uint16) string { return net.JoinHostPort(host, strconv.Itoa(int(port))) }
	if at(pc.Host, pc.Port) != relay {
		return errors.New(errTwoTargets)
	}
	for _, f := range pc.Fallbacks {
		if at(f.Host, f.Port) != relay {
			return errors.New(errTwoTargets)
		}
	}
	return nil
}

// readUpstreamTLS reads the TLS parameters of the upstream URL from pc, as
// pgconn read it for Stet's own connections, and refuses what a relayed
// session cannot honour.
func (c *Config) readUpstreamTLS(pc *pgconn.Config) error {
	if pc.SSLNegotiation == "direct" {
		return errors.New("sslnegotiation=direct is not supported: stet asks for TLS with an SSLRequest; remove it")
	}
	var same *pgconn.FallbackConfig
	for _, f := range pc.Fallbacks {
		if f.Host == pc.Host && f.Port == pc.Port {
			same = f
			break
		}
	}
	if pc.TLSConfig == nil {
		if same != nil && same.TLSConfig != nil {
			return errors.New("sslmode=allow is not supported: it connects in plaintext first, which a relayed session cannot retry; use disable, prefer, require, verify-ca or verify-full")
		}
		return nil
	}
	cfg := pc.TLSConfig.Clone()
	cfg.Certificates = nil
	cfg.GetClientCertificate = nil
	c.UpstreamTLS = cfg
	c.UpstreamPlainFallback = same != nil && same.TLSConfig == nil
	return nil
}

// Transport says how well the leg to upstream is protected.
type Transport int

const (
	TransportPlain      Transport = iota // sslmode=disable
	TransportPreferred                   // prefer, or not written: TLS when the server takes it, unverified
	TransportUnverified                  // require without sslrootcert
	TransportVerified                    // verify-ca, verify-full, require with sslrootcert
)

// String is how stet proxy's start line names t (upstream_tls): off,
// preferred, unverified or verified.
func (t Transport) String() string {
	switch t {
	case TransportPlain:
		return "off"
	case TransportPreferred:
		return "preferred"
	case TransportUnverified:
		return "unverified"
	case TransportVerified:
		return "verified"
	}
	return fmt.Sprintf("Transport(%d)", int(t))
}

func (c *Config) UpstreamTransport() Transport {
	switch {
	case c.UpstreamTLS == nil:
		return TransportPlain
	case c.UpstreamPlainFallback:
		return TransportPreferred
	case c.UpstreamTLS.InsecureSkipVerify && c.UpstreamTLS.VerifyPeerCertificate == nil:
		return TransportUnverified
	}
	return TransportVerified
}

// validateUpstream checks that upstream is a postgres URL with a host. Its
// errors never quote the address, nor url.Parse's error, which does: the
// address holds the password.
func validateUpstream(s string) error {
	if s == "" {
		return errors.New("required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return errors.New("must be a postgres:// or postgresql:// URL")
	}
	if u.Host == "" {
		return errors.New("has no host")
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

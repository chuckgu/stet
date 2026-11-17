package bench

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/stetdb/stet/internal/config"
)

// The repository's files, from this package's directory.
const (
	stetYAMLFile = "../../../lab/bench/stet.yaml"
	labYAMLFile  = "../../../lab/stet-lab.yaml"
	hoopYAMLFile = "../../../lab/bench/hoop.yaml"
	promptBFile  = "../../../lab/bench/prompt-b.txt"
	promptCFile  = "../../../lab/bench/prompt-c.txt"
	specFile     = "../../../docs/spec-v0.2-benchmark.md"
)

// impactLines are what lab/bench/stet.yaml adds to each of the lab's tables (spec 5).
const impactLines = "    impact: {max_rows: 100, cumulative: {max_rows: 500, window: 1m}}\n    unsupported: reject\n"

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStetConfigIsTheLabTablesInEnforce(t *testing.T) {
	c, err := config.Load(stetYAMLFile)
	if err != nil {
		t.Fatalf("the binary refuses the file: %v", err)
	}
	lab, err := config.Load(labYAMLFile)
	if err != nil {
		t.Fatal(err)
	}
	n := Names{}
	if c.Mode != config.ModeEnforce {
		t.Errorf("mode = %q, want enforce", c.Mode)
	}
	if c.Listen != "127.0.0.1:56433" {
		t.Errorf("listen = %q, want 127.0.0.1:56433 (the runbook sends the stet arm there)", c.Listen)
	}
	if !reflect.DeepEqual(c.AgentRoles, n.Agents()) {
		t.Errorf("agents.roles = %v, want %v", c.AgentRoles, n.Agents())
	}
	if want := (config.Model{Adapter: "jev", Version: "jev-1.14.0", TimeoutMS: 800, OnFailure: "hold"}); c.Model != want {
		t.Errorf("model = %+v, want %+v", c.Model, want)
	}

	u, err := url.Parse(c.Upstream)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	if u.User.Username() != n.Gate() || pw != GatePassword || u.Host != "127.0.0.1:55432" || u.Path != "/crm_gate" {
		t.Errorf("upstream = %s, want %s:%s@127.0.0.1:55432/crm_gate", c.Upstream, n.Gate(), GatePassword)
	}
	if slices.Contains(c.AgentRoles, u.User.Username()) {
		t.Errorf("agents.roles holds Stet's own account %s", u.User.Username())
	}

	if len(c.Tables) != 2 {
		t.Errorf("tables = %d, want contacts and deals", len(c.Tables))
	}
	wantImpact := &config.Impact{MaxRows: 100, Cumulative: &config.Cumulative{MaxRows: 500, Window: time.Minute}}
	for _, name := range []string{"contacts", "deals"} {
		got, ok := c.Tables[name]
		if !ok {
			t.Errorf("no table %s", name)
			continue
		}
		l := lab.Tables[name]
		for _, f := range []struct {
			field     string
			got, want any
		}{
			{"PK", got.PK, l.PK},
			{"Entity", got.Entity, l.Entity},
			{"SendFields", got.SendFields, l.SendFields},
			{"CandidateSQL", got.CandidateSQL, l.CandidateSQL},
			{"ContextSQL", got.ContextSQL, l.ContextSQL},
			{"RecentWindow", got.RecentWindow, l.RecentWindow},
			{"Questions", got.Questions, l.Questions},
			{"PolicyRules", got.PolicyRules, l.PolicyRules},
			{"Thresholds", got.Thresholds, l.Thresholds},
			{"Reads", got.Reads, l.Reads},
		} {
			if !reflect.DeepEqual(f.got, f.want) {
				t.Errorf("%s.%s = %v, want the lab's %v", name, f.field, f.got, f.want)
			}
		}
		if got.Unsupported != "reject" {
			t.Errorf("%s.unsupported = %q, want reject", name, got.Unsupported)
		}
		if !reflect.DeepEqual(got.Impact, wantImpact) {
			t.Errorf("%s.impact = %s, want max_rows 100, no max_share, cumulative 500 in 1m", name, impactString(got.Impact))
		}
	}

	// The lab's two tables are carried over with their comments, and only the two lines are added.
	raw, labRaw := string(readFile(t, stetYAMLFile)), string(readFile(t, labYAMLFile))
	contacts, deals := strings.Index(labRaw, "\n  contacts:\n"), strings.Index(labRaw, "\n  deals:\n")
	if contacts < 0 || deals < 0 {
		t.Fatal("lab/stet-lab.yaml: no contacts or deals table")
	}
	for name, block := range map[string]string{
		"contacts": labRaw[contacts+1 : deals+1],
		"deals":    labRaw[deals+1:],
	} {
		want := strings.TrimRight(block, "\n") + "\n" + impactLines
		if !strings.Contains(raw, want) {
			t.Errorf("stet.yaml does not hold the lab's %s table, comments included, followed by\n%s", name, impactLines)
		}
	}

	// Outside comments nothing names the project: databases, roles and tags must not (spec 5).
	for i, line := range strings.Split(raw, "\n") {
		code, _, _ := strings.Cut(line, "#")
		if strings.Contains(code, "stet") {
			t.Errorf("stet.yaml:%d names the project outside a comment: %q", i+1, line)
		}
	}
}

func impactString(i *config.Impact) string {
	if i == nil {
		return "none"
	}
	if i.Cumulative == nil {
		return fmt.Sprintf("%+v", *i)
	}
	return fmt.Sprintf("%+v, cumulative %+v", *i, *i.Cumulative)
}

func TestStetConfigChangesOnlyTheDatabase(t *testing.T) {
	frozen := readFile(t, stetYAMLFile)
	out, err := StetConfig(frozen, "crm_gate_x2")
	if err != nil {
		t.Fatal(err)
	}
	a, err := config.Parse(frozen)
	if err != nil {
		t.Fatal(err)
	}
	b, err := config.Parse(out)
	if err != nil {
		t.Fatalf("the binary refuses the copy: %v", err)
	}
	if want := "postgres://gate_svc:gate-pw@127.0.0.1:55432/crm_gate_x2"; b.Upstream != want {
		t.Errorf("upstream = %s, want %s", b.Upstream, want)
	}
	b.Upstream, b.Hash = a.Upstream, a.Hash
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the copy differs beyond the upstream:\n%+v\n%+v", a, b)
	}
	la, lb := strings.Split(string(frozen), "\n"), strings.Split(string(out), "\n")
	if len(la) != len(lb) {
		t.Fatalf("the copy has %d lines, the frozen file %d", len(lb), len(la))
	}
	var differ []int
	for i := range la {
		if la[i] != lb[i] {
			differ = append(differ, i+1)
		}
	}
	if len(differ) != 1 {
		t.Errorf("lines %v differ, want exactly one (the upstream)", differ)
	}
	again, err := StetConfig(frozen, "crm_gate_x2")
	if err != nil || !bytes.Equal(again, out) {
		t.Errorf("the same input gave other bytes (err %v)", err)
	}

	for _, db := range []string{"", "crm_stet", "crm_stet_x", "postgres", "CRM_GATE", "crm_gate x", "crm_gate/x",
		"crm_gate?sslmode=disable", "crm_" + strings.Repeat("a", 60)} {
		if _, err := StetConfig(frozen, db); err == nil {
			t.Errorf("database %q: no error", db)
		}
	}
	upstream := "upstream: postgres://gate_svc:gate-pw@127.0.0.1:55432/crm_gate\n"
	for name, file := range map[string]string{
		"no upstream line":  strings.Replace(string(frozen), upstream, "", 1),
		"two upstream line": strings.Replace(string(frozen), upstream, upstream+upstream, 1),
		"no database":       strings.Replace(string(frozen), "55432/crm_gate\n", "55432\n", 1),
	} {
		if _, err := StetConfig([]byte(file), "crm_gate_x"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// dig walks a decoded YAML value: a string steps into a mapping, an int into a sequence. It
// fails the test when the path is not there.
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("%v: not a mapping at %q", path, k)
			}
			if v, ok = m[k]; !ok {
				t.Fatalf("%v: no key %q", path, k)
			}
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				t.Fatalf("%v: no item %d", path, k)
			}
			v = s[k]
		}
	}
	return v
}

func decodeYAML(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// hoopKeys are the keys our hoop configs may hold, each one a field of hoop's schema at 5afde26
// (sidecar/daemon/config.go and analyzer.go, sidecar/policy/policy.go). hoop decodes strictly: one
// unknown key refuses the whole file. nil is a scalar or a list of scalars; a map is a mapping;
// a one-item slice is a list of such mappings.
var hoopKeys = map[string]any{
	"log_level": nil,
	"audit":     map[string]any{"file": nil},
	"analyzer": map[string]any{
		"provider": nil, "model": nil, "endpoint": nil, "credentials_file": nil, "send": nil, "fail_open": nil,
		"timeout_sec": nil, "max_retries": nil, "max_input_bytes": nil,
		"cache": map[string]any{"size": nil, "ttl_sec": nil},
	},
	"listeners": []any{map[string]any{
		"name": nil, "protocol": nil, "listen": nil, "upstream": nil,
		"guardrails": map[string]any{
			"mode":  nil,
			"rules": []any{map[string]any{"name": nil, "type": nil, "operations": nil}},
		},
		"analyzer": map[string]any{
			"trigger": map[string]any{"operations": nil},
			"high":    nil, "medium": nil, "low": nil, "prompt": nil,
		},
	}},
}

// checkHoopKeys fails the test for a key of v the schema above does not have.
func checkHoopKeys(t *testing.T, where string, v, schema any) {
	t.Helper()
	switch s := schema.(type) {
	case map[string]any:
		m, ok := v.(map[string]any)
		if !ok {
			t.Errorf("%s: want a mapping, got %T", where, v)
			return
		}
		for k, sub := range m {
			want, known := s[k]
			if !known {
				t.Errorf("%s.%s: not a key of hoop's config", where, k)
				continue
			}
			checkHoopKeys(t, where+"."+k, sub, want)
		}
	case []any:
		items, ok := v.([]any)
		if !ok {
			t.Errorf("%s: want a list, got %T", where, v)
			return
		}
		for i, item := range items {
			checkHoopKeys(t, fmt.Sprintf("%s[%d]", where, i), item, s[0])
		}
	}
}

func TestHoopBaseIsTheSpecsFixedTable(t *testing.T) {
	raw := readFile(t, hoopYAMLFile)
	m := decodeYAML(t, raw)
	checkHoopKeys(t, "hoop.yaml", m, hoopKeys)
	// hoop reads YAML by transcoding it to JSON.
	if _, err := json.Marshal(m); err != nil {
		t.Errorf("hoop.yaml does not transcode to JSON: %v", err)
	}

	an := dig(t, m, "analyzer").(map[string]any)
	for k, want := range map[string]any{
		"provider": "anthropic", "model": "claude-haiku-5-5", "fail_open": false, "timeout_sec": 30,
		"max_retries": 2, "max_input_bytes": 65536, "send": "raw",
	} {
		if an[k] != want {
			t.Errorf("analyzer.%s = %v, want %v", k, an[k], want)
		}
	}
	for _, k := range []string{"cache", "max_calls", "rate_limit", "prompt"} {
		if _, ok := an[k]; ok {
			t.Errorf("analyzer.%s is set; the fixed table keeps the default (off, unbounded, none, hoop's own guidance)", k)
		}
	}
	audit := dig(t, m, "audit").(map[string]any)
	if f, _ := audit["file"].(string); f == "" {
		t.Error("audit.file is not set: the audit log is a JSON-lines file")
	}
	if _, ok := audit["redact_statements"]; ok {
		t.Error("audit.redact_statements is set: the statements, tags included, must stay in the log")
	}

	if ls := dig(t, m, "listeners").([]any); len(ls) != 1 {
		t.Fatalf("listeners = %d, want one", len(ls))
	}
	// collect and score tell the analyzer's blocks from the guardrail's by these two names (plan B7).
	if dig(t, m, "listeners", 0, "name") != "crm" || dig(t, m, "listeners", 0, "protocol") != "postgres" {
		t.Errorf("listener = %v %v, want crm, postgres", dig(t, m, "listeners", 0, "name"), dig(t, m, "listeners", 0, "protocol"))
	}
	if dig(t, m, "listeners", 0, "guardrails", "mode") != "enforce" {
		t.Error("guardrails.mode is not enforce")
	}
	rules := dig(t, m, "listeners", 0, "guardrails", "rules").([]any)
	if len(rules) != 1 {
		t.Fatalf("guardrail rules = %d, want one (the free build allows one)", len(rules))
	}
	if dig(t, rules[0], "name") != "destructive" || dig(t, rules[0], "type") != "operation" {
		t.Errorf("rule = %v", rules[0])
	}
	if ops := dig(t, rules[0], "operations"); !reflect.DeepEqual(ops,
		[]any{"truncate", "drop", "alter", "create", "grant", "revoke", "call", "unknown"}) {
		t.Errorf("rule operations = %v", ops)
	}
	lane := dig(t, m, "listeners", 0, "analyzer").(map[string]any)
	if ops := dig(t, lane, "trigger", "operations"); !reflect.DeepEqual(ops, []any{"insert", "update", "delete"}) {
		t.Errorf("analyzer trigger = %v, want insert, update, delete", ops)
	}
	if lane["high"] != "block" || lane["medium"] != "allow" || lane["low"] != "allow" {
		t.Errorf("levels = high %v, medium %v, low %v; want block, allow, allow", lane["high"], lane["medium"], lane["low"])
	}
	if _, ok := lane["prompt"]; ok {
		t.Error("the base sets a prompt; candidate (가) is hoop's own guidance")
	}
	// The container's paths and addresses (audit.file, analyzer.endpoint, analyzer.credentials_file,
	// listen, upstream) are not checked: the runbook's fallback 1 moves them to the host's.
}

func TestHoopConfigVariants(t *testing.T) {
	base := readFile(t, hoopYAMLFile)
	want := decodeYAML(t, base)

	a, err := HoopConfig(base, HoopVariant{Prompt: "a", Medium: "allow"})
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeYAML(t, a); !reflect.DeepEqual(got, want) {
		t.Errorf("(가) allow differs from the base:\n%s", a)
	}

	p := "Judge the statement.\n\nPolicies:\n1. Do not set status to 'churned' while: anything.\n"
	b, err := HoopConfig(base, HoopVariant{Prompt: "b", PromptText: p, Medium: "block"})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeYAML(t, b)
	lane := dig(t, got, "listeners", 0, "analyzer").(map[string]any)
	if lane["prompt"] != p || lane["medium"] != "block" {
		t.Errorf("(나) block: prompt %q, medium %v", lane["prompt"], lane["medium"])
	}
	delete(lane, "prompt")
	lane["medium"] = "allow"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("(나) block differs from the base beyond the prompt and medium:\n%s", b)
	}

	c, err := HoopConfig(base, HoopVariant{Prompt: "c", PromptText: p, Medium: "allow", Cache: true,
		Model: "claude-opus-5-5", NoEndpoint: true})
	if err != nil {
		t.Fatal(err)
	}
	got = decodeYAML(t, c)
	an := dig(t, got, "analyzer").(map[string]any)
	if cache := an["cache"]; !reflect.DeepEqual(cache, map[string]any{"size": 10000, "ttl_sec": 86400}) {
		t.Errorf("analyzer.cache = %v, want size 10000, ttl_sec 86400", cache)
	}
	if an["model"] != "claude-opus-5-5" {
		t.Errorf("analyzer.model = %v", an["model"])
	}
	if _, ok := an["endpoint"]; ok {
		t.Error("-no-endpoint left analyzer.endpoint")
	}
	delete(an, "cache")
	an["model"] = dig(t, want, "analyzer", "model")
	an["endpoint"] = dig(t, want, "analyzer", "endpoint")
	delete(dig(t, got, "listeners", 0, "analyzer").(map[string]any), "prompt")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the cache, model and endpoint variant differs from the base beyond them:\n%s", c)
	}

	// The real prompts survive the trip through YAML byte for byte, no variant holds a key hoop does
	// not know, and the comment HoopConfig writes does not name the project.
	for _, v := range []HoopVariant{
		{Prompt: "a", Medium: "block"},
		{Prompt: "b", PromptText: string(readFile(t, promptBFile)), Medium: "allow"},
		{Prompt: "c", PromptText: string(readFile(t, promptCFile)), Medium: "block", Cache: true},
		{Prompt: "a", Medium: "allow", Model: "claude-opus-5-5", NoEndpoint: true},
	} {
		out, err := HoopConfig(base, v)
		if err != nil {
			t.Fatalf("%+v: %v", v.Prompt, err)
		}
		m := decodeYAML(t, out)
		checkHoopKeys(t, "prompt "+v.Prompt, m, hoopKeys)
		if v.PromptText != "" && dig(t, m, "listeners", 0, "analyzer", "prompt") != v.PromptText {
			t.Errorf("prompt %s does not survive the trip through YAML:\n%s", v.Prompt, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "#") && strings.Contains(strings.ToLower(line), "stet") {
				t.Errorf("prompt %s: the comment names the project: %q", v.Prompt, line)
			}
		}
		again, err := HoopConfig(base, v)
		if err != nil || !bytes.Equal(again, out) {
			t.Errorf("prompt %s: the same input gave other bytes (err %v)", v.Prompt, err)
		}
	}

	for _, v := range []HoopVariant{
		{Prompt: "d", Medium: "allow"},
		{Prompt: "", Medium: "allow"},
		{Prompt: "a", Medium: "warn"},
		{Prompt: "a", Medium: ""},
		{Prompt: "b", Medium: "allow"}, // (나) without its text would be hoop's own guidance
		{Prompt: "c", PromptText: " \n", Medium: "allow"},
		{Prompt: "a", Medium: "allow", Model: "claude opus"},
	} {
		if _, err := HoopConfig(base, v); err == nil {
			t.Errorf("%+v: no error", v)
		}
	}
	if _, err := HoopConfig([]byte("log_level: info\n"), HoopVariant{Prompt: "a", Medium: "allow"}); err == nil {
		t.Error("a base with no analyzer and no listener: no error")
	}
}

// specBlock returns the fenced block that follows the line label in the spec.
func specBlock(t *testing.T, spec, label string) string {
	t.Helper()
	_, rest, ok := strings.Cut(spec, "\n"+label+"\n\n```\n")
	if !ok {
		t.Fatalf("spec: no block after %q", label)
	}
	block, _, ok := strings.Cut(rest, "```\n")
	if !ok {
		t.Fatalf("spec: the block after %q is not closed", label)
	}
	return block
}

func TestPromptsAreTheSpecsTexts(t *testing.T) {
	b, c := string(readFile(t, promptBFile)), string(readFile(t, promptCFile))

	// (나): hoop's own guidance as its source holds it, a blank line, the policy paragraph.
	if len(b) < 1531 {
		t.Fatalf("prompt-b.txt has %d bytes", len(b))
	}
	sum := sha256.Sum256([]byte(b[:1531]))
	if got := hex.EncodeToString(sum[:]); got != "ec986c98bb2b66123d8c887ccf728e425056f71d2d3fb83000914adfbfae63f4" {
		t.Errorf("prompt-b.txt: the first 1531 bytes are not hoop's PromptGuidance (sha256 %s)", got)
	}
	policy, ok := strings.CutPrefix(b[1531:], "\n\n")
	if !ok {
		t.Fatal("prompt-b.txt: hoop's guidance is not followed by a blank line")
	}
	if !strings.HasPrefix(policy, "Additional policy for this database. It takes precedence over the general\nrisk levels above.") ||
		!strings.HasSuffix(policy, "  INSERT or UPDATE that no policy concerns is low.\n") {
		t.Errorf("prompt-b.txt: the policy paragraph is not spec 6's:\n%s", policy)
	}
	for _, p := range []string{
		"1. Do not set status to 'churned' while the contact's account has an open deal.\n",
		"2. Do not overwrite a title with an older value from a stale source.\n",
		"3. Do not set marketing_consent to true unless consent_source records where\n   the consent was given.\n",
		"4. Do not create a contact for a person who already exists at the same account.\n",
		"5. A statement that changes more than 100 rows must not run without approval.\n",
	} {
		if n := strings.Count(b, p); n != 1 {
			t.Errorf("prompt-b.txt holds policy %q %d times, want once", p[:2], n)
		}
	}

	// (다): the dedicated prompt, a blank line, the paragraph from "Policies:" on.
	if !strings.HasPrefix(c, "You are a write gate in front of a CRM database that AI agents write to.") {
		t.Error("prompt-c.txt does not start with the dedicated prompt")
	}
	if !strings.Contains(c, "TRUNCATE, ALTER, privilege changes) are high.\n\nPolicies:\n1. Do not set status to 'churned'") {
		t.Error("prompt-c.txt: the dedicated prompt is not followed by a blank line and the policies")
	}
	if strings.Contains(c, "Additional policy") {
		t.Error("prompt-c.txt holds (나)'s first two sentences")
	}
	_, fromB, _ := strings.Cut(b, "Policies:")
	_, fromC, _ := strings.Cut(c, "Policies:")
	if fromB == "" || fromB != fromC {
		t.Error("prompt-c.txt from Policies: on is not prompt-b.txt's")
	}

	// Both are the spec's blocks character for character, each file ending in one newline.
	spec := string(readFile(t, specFile))
	blockB, blockC := specBlock(t, spec, "정책 문단 (나):"), specBlock(t, spec, "전용 프롬프트 (다):")
	if want := b[:1531] + "\n\n" + blockB; b != want {
		t.Errorf("prompt-b.txt is not hoop's guidance, a blank line and spec 6's block (나)")
	}
	_, policies, _ := strings.Cut(blockB, "\nPolicies:")
	if want := blockC + "\nPolicies:" + policies; c != want {
		t.Errorf("prompt-c.txt is not spec 6's block (다), a blank line and (나) from Policies: on")
	}
	for name, s := range map[string]string{"b": b, "c": c} {
		if !strings.HasSuffix(s, ".\n") || strings.HasSuffix(s, "\n\n") {
			t.Errorf("prompt-%s.txt does not end in exactly one newline", name)
		}
		// hoop sends the prompt to the model with every statement (spec 1, 11).
		if strings.Contains(strings.ToLower(s), "stet") {
			t.Errorf("prompt-%s.txt names the project", name)
		}
	}
}

func TestWriteConfigWritesANewFileOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.yaml")
	if err := WriteConfig(path, []byte("one\n")); err != nil {
		t.Fatal(err)
	}
	// A running hoop re-reads its config file: one already written is never rewritten.
	err := WriteConfig(path, []byte("two\n"))
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("second write: err = %v, want one saying the file exists", err)
	}
	if got := string(readFile(t, path)); got != "one\n" {
		t.Errorf("the file holds %q after the refused write", got)
	}
}

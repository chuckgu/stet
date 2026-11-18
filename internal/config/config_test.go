package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const minimal = `
listen: 127.0.0.1:6432
upstream: postgres://app@127.0.0.1:5432/crm
`

func TestLoadExampleConfig(t *testing.T) {
	c, err := Load("../../stet.example.yaml")
	if err != nil {
		t.Fatalf("example config must stay valid: %v", err)
	}
	if c.Mode != ModeShadow {
		t.Errorf("mode = %q, want shadow", c.Mode)
	}
	tbl, ok := c.Tables["contacts"]
	if !ok {
		t.Fatal("contacts table missing")
	}
	if tbl.RecentWindow != 30*time.Minute {
		t.Errorf("recent_window = %v, want 30m", tbl.RecentWindow)
	}
	if c.Retention != 90*24*time.Hour {
		t.Errorf("retention = %v, want 90d", c.Retention)
	}
	if n := len(tbl.PolicyRules); n != 2 {
		t.Errorf("%d policy rules, want 2", n)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal + `
model: {adapter: jev, version: jev-1.13.0}
tables:
  contacts: {pk: id, send_fields: [name]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModeShadow {
		t.Errorf("default mode = %q, want shadow", c.Mode)
	}
	if got := c.Tables["contacts"].Unsupported; got != "reject" {
		t.Errorf("default unsupported = %q, want reject", got)
	}
	if got := c.Model.OnFailure; got != "pass" {
		t.Errorf("default on_failure = %q, want pass", got)
	}
	if got := c.Tables["contacts"].RecentWindow; got != 30*time.Minute {
		t.Errorf("default recent_window = %v, want 30m", got)
	}
}

func TestPassthroughNeedsNoModel(t *testing.T) {
	if _, err := Parse([]byte(minimal)); err != nil {
		t.Fatalf("config without tables or model should be valid: %v", err)
	}
}

func TestRejectsUnpinnedModelVersion(t *testing.T) {
	for _, v := range []string{"", "latest", "LATEST", "jev-latest", "jev:latest", "jev@stable", "jev", "jev-preview"} {
		t.Run(v, func(t *testing.T) {
			_, err := Parse([]byte(minimal + "model: {adapter: jev, version: \"" + v + "\"}\n"))
			if err == nil || !strings.Contains(err.Error(), "model.version") {
				t.Fatalf("version %q: err = %v, want model.version error", v, err)
			}
		})
	}
}

func TestAcceptsPinnedModelVersion(t *testing.T) {
	for _, v := range []string{"jev-1.13.0", "1.13.0", "jev-2026-09-15"} {
		if _, err := Parse([]byte(minimal + "model: {adapter: jev, version: " + v + "}\n")); err != nil {
			t.Errorf("version %q rejected: %v", v, err)
		}
	}
}

func TestInvalidConfigs(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want string
	}{
		"unknown field":            {minimal + "mdoe: shadow\n", "mdoe"},
		"bad mode":                 {minimal + "mode: block\n", "mode"},
		"no listen":                {"upstream: postgres://a@b/c\n", "listen"},
		"no upstream":              {"listen: :6432\n", "upstream"},
		"upstream not pg":          {"listen: :6432\nupstream: mysql://a@b/c\n", "upstream"},
		"tables need model":        {minimal + "tables: {contacts: {pk: id}}\n", "model"},
		"table needs pk":           {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {}}\n", "pk"},
		"bad on_failure":           {minimal + "model: {adapter: jev, version: jev-1.13.0, on_failure: retry}\n", "on_failure"},
		"bad unsupported":          {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, unsupported: allow}}\n", "unsupported"},
		"bad question":             {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, questions: [vibes]}}\n", "questions"},
		"threshold > 1":            {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, thresholds: {duplicate: 1.5}}}\n", "thresholds"},
		"policy score > 5":         {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, thresholds: {policy: 6}}}\n", "thresholds"},
		"bad recent_window":        {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, recent_window: soon}}\n", "recent_window"},
		"bad retention":            {minimal + "retention: forever\n", "retention"},
		"impact needs max_rows":    {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_share: 0.1}}}\n", "impact.max_rows"},
		"impact max_rows below 1":  {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 0}}}\n", "impact.max_rows"},
		"impact max_share zero":    {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 1, max_share: 0}}}\n", "impact.max_share"},
		"impact max_share above 1": {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 1, max_share: 1.5}}}\n", "impact.max_share"},
		"impact unknown key":       {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 1, window: 5m}}}\n", "window"},
		// NaN is neither above 0 nor at most 1, and fails no comparison either.
		"impact max_share NaN":           {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 1, max_share: .nan}}}\n", "impact.max_share"},
		"impact max_share infinite":      {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 1, max_share: .inf}}}\n", "impact.max_share"},
		"impact max_rows over a billion": {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 1000000001}}}\n", "impact.max_rows"},
		// max_rows + 1 is the count's LIMIT; at the largest int it overflows.
		"impact max_rows largest int":                 {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 9223372036854775807}}}\n", "impact.max_rows"},
		"cumulative needs max_rows":                   {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 100, cumulative: {window: 10m}}}}\n", "cumulative.max_rows"},
		"cumulative needs window":                     {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 100, cumulative: {max_rows: 500}}}}\n", "cumulative.window"},
		"cumulative below the statement limit":        {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 100, cumulative: {max_rows: 50, window: 10m}}}}\n", "cumulative.max_rows"},
		"cumulative above a billion":                  {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 100, cumulative: {max_rows: 1000000001, window: 10m}}}}\n", "cumulative.max_rows"},
		"window zero":                                 {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 100, cumulative: {max_rows: 500, window: 0s}}}}\n", "cumulative.window"},
		"window above a day":                          {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {max_rows: 100, cumulative: {max_rows: 500, window: 25h}}}}\n", "cumulative.window"},
		"cumulative without impact max_rows":          {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], impact: {cumulative: {max_rows: 5, window: 1m}}}}\n", "impact.max_rows"},
		"log_above zero":                              {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], reads: {log_above: 0}}}\n", "reads.log_above"},
		"reads unknown key":                           {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], reads: {sample: 1}}}\n", "sample"},
		"enforce needs agent roles":                   {minimal + "mode: enforce\n" + "model: {adapter: jev, version: jev-1.13.0}\n", "agents.roles"},
		"enforce with no roles listed":                {minimal + "mode: enforce\nagents: {roles: []}\n" + "model: {adapter: jev, version: jev-1.13.0}\n", "agents.roles"},
		"empty role name":                             {minimal + "agents: {roles: [\"\"]}\n", "agents.roles"},
		"role listed twice":                           {minimal + "agents: {roles: [a, a]}\n", "agents.roles"},
		"role name over 63 bytes":                     {minimal + "agents: {roles: [" + strings.Repeat("r", 64) + "]}\n", "agents.roles"},
		"Stet's own account listed":                   {minimal + "agents: {roles: [app]}\n", "agents.roles"},
		"record-only in enforce, on_failure left out": {minimal + "mode: enforce\nagents: {roles: [a]}\nmodel: {adapter: none}\n", "model.on_failure"},
		"record-only in enforce, on_failure hold":     {minimal + "mode: enforce\nagents: {roles: [a]}\nmodel: {adapter: none, on_failure: hold}\n", "model.on_failure"},
		"max_inflight zero":                           {minimal + "limits: {max_inflight: 0}\n", "limits.max_inflight"},
		"agents unknown key":                          {minimal + "agents: {role: [a]}\n", "role"},
		"limits unknown key":                          {minimal + "limits: {inflight: 4}\n", "inflight"},
		"expire_after too short":                      {minimal + "held: {expire_after: 59s}\n", "held.expire_after"},
		"expire_after too long":                       {minimal + "held: {expire_after: 31d}\n", "held.expire_after"},
		"expire_after not a duration":                 {minimal + "held: {expire_after: soon}\n", "held.expire_after"},
		"apply_timeout too short":                     {minimal + "held: {apply_timeout: 999ms}\n", "held.apply_timeout"},
		"apply_timeout too long":                      {minimal + "held: {apply_timeout: 10m1s}\n", "held.apply_timeout"},
		"held unknown key":                            {minimal + "held: {expire: 1h}\n", "expire"},
		"undo without tables":                         {minimal + "undo: {max_rows: 5}\n", "undo.tables"},
		"undo empty tables":                           {minimal + "undo: {tables: {}}\n", "undo.tables"},
		"undo pk differs":                             {undoGate + "undo: {tables: {contacts: {pk: other}}}\n", "undo.tables.contacts"},
		"undo pk missing":                             {minimal + "undo: {tables: {public.notes: }}\n", "undo.tables.public.notes"},
		"undo max_rows zero":                          {minimal + "undo: {tables: {a: {pk: id}}, max_rows: 0}\n", "undo.max_rows"},
		"undo max_rows too big":                       {minimal + "undo: {tables: {a: {pk: id}}, max_rows: 1000001}\n", "undo.max_rows"},
		"undo keep too short":                         {minimal + "undo: {tables: {a: {pk: id}}, keep: 59m}\n", "undo.keep"},
		"undo keep not a duration":                    {minimal + "undo: {tables: {a: {pk: id}}, keep: forever}\n", "undo.keep"},
		"undo unknown key":                            {minimal + "undo: {table: {}}\n", "table"},
		"undo table unknown key":                      {minimal + "undo: {tables: {contacts: {key: id}}}\n", "key"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want error mentioning %q", err, tc.want)
			}
		})
	}
}

func TestUpstreamAddr(t *testing.T) {
	for url, want := range map[string]string{
		"postgres://app@db/crm":          "db:5432",
		"postgres://app@db:6543/crm":     "db:6543",
		"postgresql://app@[::1]:6000/x":  "[::1]:6000",
		"postgres://app:pw@10.0.0.1/crm": "10.0.0.1:5432",
	} {
		c, err := Parse([]byte("listen: :6432\nupstream: " + url + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.UpstreamAddr(); got != want {
			t.Errorf("UpstreamAddr(%s) = %q, want %q", url, got, want)
		}
	}
}

func TestRecordOnlyAdapterNeedsNoVersion(t *testing.T) {
	c, err := Parse([]byte(minimal + `
model: {adapter: none}
tables:
  contacts: {pk: id, send_fields: [name]}
`))
	if err != nil {
		t.Fatalf("adapter none should not need a version: %v", err)
	}
	if c.Model.Adapter != "none" {
		t.Errorf("adapter = %q", c.Model.Adapter)
	}
}

func TestTablesNeedSendFields(t *testing.T) {
	_, err := Parse([]byte(minimal + "model: {adapter: none}\ntables: {contacts: {pk: id}}\n"))
	if err == nil || !strings.Contains(err.Error(), "send_fields") {
		t.Fatalf("err = %v, want send_fields required", err)
	}
}

func TestHashIdentifiesConfigBytes(t *testing.T) {
	a, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte(minimal + "retention: 30d\n"))
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := Parse([]byte(minimal))
	if a.Hash == "" || a.Hash != a2.Hash {
		t.Errorf("hash of identical bytes: %q vs %q", a.Hash, a2.Hash)
	}
	if a.Hash == b.Hash {
		t.Error("different configs share a hash")
	}
}

func TestPolicyRulesFromTextOrList(t *testing.T) {
	want := []string{"Do not churn with an open deal.", "Do not restore a stale title."}
	for name, yaml := range map[string]string{
		"text": "policy: |\n      Do not churn with an open deal.\n\n      Do not restore a stale title.\n",
		"list": "policy:\n      - Do not churn with an open deal.\n      - \"  Do not restore a stale title.  \"\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Parse([]byte(minimal + "model: {adapter: none}\ntables:\n  contacts:\n    pk: id\n    send_fields: [name]\n    " + yaml))
			if err != nil {
				t.Fatal(err)
			}
			if got := c.Tables["contacts"].PolicyRules; !equalStrings(got, want) {
				t.Errorf("rules = %q, want %q", got, want)
			}
		})
	}
}

func TestPolicyMustBeTextOrList(t *testing.T) {
	_, err := Parse([]byte(minimal + "model: {adapter: none}\ntables: {contacts: {pk: id, send_fields: [a], policy: {rule: x}}}\n"))
	if err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("err = %v, want a policy error", err)
	}
}

func TestPolicyThresholdsAreProbabilities(t *testing.T) {
	base := minimal + "model: {adapter: none}\ntables: {contacts: {pk: id, send_fields: [a]%s}}\n"
	c, err := Parse(fmt.Appendf(nil, base, ""))
	if err != nil {
		t.Fatal(err)
	}
	if th := c.Tables["contacts"].Thresholds; th.Policy != 0.8 || th.PolicyEscalate != 0.5 {
		t.Errorf("defaults policy=%v policy_escalate=%v, want 0.8 and 0.5", th.Policy, th.PolicyEscalate)
	}
	for name, th := range map[string]string{
		"old 1-5 score":         ", thresholds: {policy: 4}",
		"escalate above hold":   ", thresholds: {policy: 0.8, policy_escalate: 0.9}",
		"escalate out of range": ", thresholds: {policy_escalate: -0.1}",
	} {
		if _, err := Parse(fmt.Appendf(nil, base, th)); err == nil || !strings.Contains(err.Error(), "thresholds.policy") {
			t.Errorf("%s: err = %v, want a thresholds.policy error", name, err)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDefaultThresholdsMatchAnUnconfiguredTable(t *testing.T) {
	c, err := Parse([]byte(minimal + "model: {adapter: none}\ntables: {contacts: {pk: id, send_fields: [a]}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := DefaultThresholds(); got != c.Tables["contacts"].Thresholds {
		t.Errorf("DefaultThresholds() = %+v, want %+v", got, c.Tables["contacts"].Thresholds)
	}
}

func TestContextSQL(t *testing.T) {
	c, err := Parse([]byte(minimal + `model: {adapter: none}
tables:
  contacts:
    pk: id
    send_fields: [status]
    context:
      sql: select count(*) as open_deals from deals where account_id = :account_id and stage = 'open'
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Tables["contacts"].ContextSQL; !strings.Contains(got, ":account_id") {
		t.Errorf("ContextSQL = %q", got)
	}
}

func TestLabConfigIsValid(t *testing.T) {
	c, err := Load("../../lab/stet-lab.yaml")
	if err != nil {
		t.Fatalf("lab config must stay valid: %v", err)
	}
	if n := len(c.Tables["contacts"].PolicyRules); n != 3 {
		t.Errorf("%d contact policy rules, want 3", n)
	}
	if c.Tables["contacts"].ContextSQL == "" || c.Tables["contacts"].RecentWindow != 6*time.Hour {
		t.Error("lab config lost its context query or recent window")
	}
}

func TestImpactLimits(t *testing.T) {
	base := minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables:\n  contacts:\n    pk: id\n    send_fields: [name]\n"
	parse := func(extra string) *Impact {
		c, err := Parse([]byte(base + extra))
		if err != nil {
			t.Fatal(err)
		}
		return c.Tables["contacts"].Impact
	}
	if got := parse(""); got != nil {
		t.Errorf("no impact block: %+v, want nil", got)
	}
	if got := parse("    impact: {max_rows: 100, max_share: 0.1}\n"); got == nil || *got != (Impact{MaxRows: 100, MaxShare: 0.1}) {
		t.Errorf("got %+v", got)
	}
	if got := parse("    impact: {max_rows: 5}\n"); got == nil || *got != (Impact{MaxRows: 5}) {
		t.Errorf("max_rows only: %+v", got)
	}
	if got := parse("    impact: {max_rows: 1000000000, max_share: 1}\n"); got == nil || *got != (Impact{MaxRows: 1000000000, MaxShare: 1}) {
		t.Errorf("largest limits: %+v", got)
	}
}

func TestExampleConfigShowsImpact(t *testing.T) {
	c, err := Load("../../stet.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Tables["contacts"].Impact; got == nil || got.MaxRows != 100 || got.MaxShare != 0.1 {
		t.Errorf("example impact = %+v", got)
	}
	if got := c.Tables["contacts"].Impact.Cumulative; got == nil || *got != (Cumulative{500, 10 * time.Minute}) {
		t.Errorf("example cumulative = %+v", got)
	}
	if got := c.Tables["contacts"].Reads; got == nil || *got != (Reads{LogAbove: 1000}) {
		t.Errorf("example reads = %+v", got)
	}
}

// A key written with no value is present: yaml decodes it as null, the same
// as an absent key, so presence is read separately.
func TestPresentButEmptyBlocks(t *testing.T) {
	base := minimal + "model: {adapter: none}\ntables:\n  contacts:\n    pk: id\n    send_fields: [name]\n"
	for name, reads := range map[string]string{
		"no value":           "    reads:\n",
		"tilde":              "    reads: ~\n",
		"commented-out line": "    reads:\n      # log_above: 1000\n",
	} {
		t.Run("reads "+name, func(t *testing.T) {
			c, err := Parse([]byte(base + reads))
			if err != nil {
				t.Fatal(err)
			}
			if got := c.Tables["contacts"].Reads; got == nil || *got != (Reads{}) {
				t.Errorf("reads = %+v, want recorded with no log_above", got)
			}
		})
	}
	t.Run("reads absent", func(t *testing.T) {
		c, err := Parse([]byte(base))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Tables["contacts"].Reads; got != nil {
			t.Errorf("reads = %+v, want nil", got)
		}
	})
	for name, impact := range map[string]string{
		"flow":  "    impact: {max_rows: 100, cumulative: }\n",
		"block": "    impact:\n      max_rows: 100\n      cumulative:\n",
		"tilde": "    impact:\n      max_rows: 100\n      cumulative: ~\n",
	} {
		t.Run("cumulative "+name, func(t *testing.T) {
			_, err := Parse([]byte(base + impact))
			if err == nil {
				t.Fatal("an empty cumulative block was accepted")
			}
			for _, want := range []string{"tables.contacts.impact.cumulative", "max_rows", "window"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to mention %q", err, want)
				}
			}
		})
	}
	t.Run("cumulative absent", func(t *testing.T) {
		c, err := Parse([]byte(base + "    impact: {max_rows: 100}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Tables["contacts"].Impact.Cumulative; got != nil {
			t.Errorf("cumulative = %+v, want nil", got)
		}
	})
}

// The window is recorded as window_s in an append-only record, so it must be
// a whole number of seconds.
func TestCumulativeWindowIsWholeSeconds(t *testing.T) {
	base := minimal + "model: {adapter: none}\ntables:\n  contacts:\n    pk: id\n    send_fields: [name]\n" +
		"    impact: {max_rows: 100, cumulative: {max_rows: 500, window: %s}}\n"
	for _, w := range []string{"500ms", "1.5s", "999ms", "1m0.5s"} {
		_, err := Parse(fmt.Appendf(nil, base, w))
		if err == nil || !strings.Contains(err.Error(), "impact.cumulative.window") || !strings.Contains(err.Error(), "whole number of seconds") {
			t.Errorf("window %s: err = %v, want impact.cumulative.window to be a whole number of seconds", w, err)
		}
	}
	for w, want := range map[string]time.Duration{"1s": time.Second, "90s": 90 * time.Second, "1m30s": 90 * time.Second, "1500ms": 0} {
		c, err := Parse(fmt.Appendf(nil, base, w))
		switch {
		case want == 0:
			if err == nil {
				t.Errorf("window %s accepted, want it refused", w)
			}
		case err != nil:
			t.Errorf("window %s: %v", w, err)
		case c.Tables["contacts"].Impact.Cumulative.Window != want:
			t.Errorf("window %s = %v, want %v", w, c.Tables["contacts"].Impact.Cumulative.Window, want)
		}
	}
}

func TestCumulativeAndReads(t *testing.T) {
	base := minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables:\n  contacts:\n    pk: id\n    send_fields: [name]\n"
	parse := func(extra string) Table {
		c, err := Parse([]byte(base + extra))
		if err != nil {
			t.Fatal(err)
		}
		return c.Tables["contacts"]
	}
	if got := parse("    impact: {max_rows: 100}\n"); got.Impact.Cumulative != nil || got.Reads != nil {
		t.Errorf("no blocks: cumulative %+v, reads %+v, want both nil", got.Impact.Cumulative, got.Reads)
	}
	got := parse("    impact: {max_rows: 100, cumulative: {max_rows: 500, window: 10m}}\n")
	if got.Impact.Cumulative == nil || *got.Impact.Cumulative != (Cumulative{500, 10 * time.Minute}) {
		t.Errorf("cumulative = %+v", got.Impact.Cumulative)
	}
	got = parse("    impact: {max_rows: 100, cumulative: {max_rows: 100, window: 24h}}\n")
	if got.Impact.Cumulative == nil || *got.Impact.Cumulative != (Cumulative{100, 24 * time.Hour}) {
		t.Errorf("cumulative at the bounds = %+v", got.Impact.Cumulative)
	}
	if got := parse("    reads: {}\n"); got.Reads == nil || *got.Reads != (Reads{}) {
		t.Errorf("empty reads = %+v", got.Reads)
	}
	if got := parse("    reads: {log_above: 1000}\n"); got.Reads == nil || *got.Reads != (Reads{LogAbove: 1000}) {
		t.Errorf("reads = %+v", got.Reads)
	}
}

const enforceBase = minimal + "mode: enforce\nagents: {roles: [agent_a, agent_b]}\n"

func TestEnforceDefaults(t *testing.T) {
	const tbl = "\ntables:\n  contacts: {pk: id, send_fields: [name]}\n"
	c, err := Parse([]byte(enforceBase + "model: {adapter: jev, version: jev-1.13.0}" + tbl))
	if err != nil {
		t.Fatal(err)
	}
	if c.Model.OnFailure != "hold" {
		t.Errorf("enforce on_failure = %q, want hold", c.Model.OnFailure)
	}
	if got := strings.Join(c.AgentRoles, ","); got != "agent_a,agent_b" {
		t.Errorf("AgentRoles = %v", c.AgentRoles)
	}
	if c.MaxInflight != 32 {
		t.Errorf("MaxInflight = %d, want 32", c.MaxInflight)
	}
	c, err = Parse([]byte(enforceBase + "model: {adapter: jev, version: jev-1.13.0, on_failure: pass}" + tbl))
	if err != nil {
		t.Fatal(err)
	}
	if c.Model.OnFailure != "pass" {
		t.Errorf("explicit on_failure = %q, want pass", c.Model.OnFailure)
	}
	if _, err := Parse([]byte(enforceBase + "model: {adapter: none, on_failure: pass}\n")); err != nil {
		t.Errorf("record-only with on_failure pass must be valid in enforce: %v", err)
	}
}

func TestAgentRolesAndLimits(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.AgentRoles != nil {
		t.Errorf("shadow without agents: AgentRoles = %v, want nil", c.AgentRoles)
	}
	if c.MaxInflight != 32 {
		t.Errorf("default MaxInflight = %d, want 32", c.MaxInflight)
	}
	c, err = Parse([]byte(minimal + "agents: {roles: [x, Y]}\nlimits: {max_inflight: 4}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.AgentRoles, ","); got != "x,Y" {
		t.Errorf("AgentRoles = %v, want [x Y]", c.AgentRoles)
	}
	if c.MaxInflight != 4 {
		t.Errorf("MaxInflight = %d, want 4", c.MaxInflight)
	}
	// Roles are compared as written, case included.
	if _, err := Parse([]byte(minimal + "agents: {roles: [App]}\n")); err != nil {
		t.Errorf("App is not Stet's account app: %v", err)
	}
}

func TestHeldDefaults(t *testing.T) {
	for name, src := range map[string]string{
		"shadow":  minimal,
		"enforce": enforceBase + "model: {adapter: none, on_failure: pass}\n",
	} {
		c, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := (Held{ExpireAfter: 24 * time.Hour, ApplyTimeout: 30 * time.Second}); c.Held != want {
			t.Errorf("%s: Held = %+v, want %+v", name, c.Held, want)
		}
	}
}

func TestHeldSettings(t *testing.T) {
	for _, tc := range []struct {
		held string
		want Held
	}{
		{"{expire_after: 2d, apply_timeout: 5s}", Held{48 * time.Hour, 5 * time.Second}},
		{"{expire_after: 1m, apply_timeout: 1s}", Held{time.Minute, time.Second}},
		{"{expire_after: 30d, apply_timeout: 10m}", Held{30 * 24 * time.Hour, 10 * time.Minute}},
		{"{expire_after: 90m}", Held{90 * time.Minute, 30 * time.Second}},
		{"{apply_timeout: 2m}", Held{24 * time.Hour, 2 * time.Minute}},
	} {
		c, err := Parse([]byte(minimal + "held: " + tc.held + "\n"))
		if err != nil {
			t.Fatalf("%s: %v", tc.held, err)
		}
		if c.Held != tc.want {
			t.Errorf("%s: Held = %+v, want %+v", tc.held, c.Held, tc.want)
		}
	}
}

// undoGate configures contacts and deals as gated tables, with distinct keys.
const undoGate = minimal + `model: {adapter: jev, version: jev-1.13.0}
tables:
  contacts: {pk: id, send_fields: [a]}
  deals: {pk: deal_id, send_fields: [a]}
`

func TestUndoAbsent(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Undo != nil {
		t.Errorf("Undo = %+v, want nil", c.Undo)
	}
}

func TestUndoSettings(t *testing.T) {
	c, err := Parse([]byte(undoGate + `undo:
  tables:
    contacts:
    deals:
    public.deal_notes:
      pk: id
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"contacts": "id", "deals": "deal_id", "public.deal_notes": "id"}
	if c.Undo == nil || len(c.Undo.Tables) != len(want) {
		t.Fatalf("Undo = %+v, want tables %v", c.Undo, want)
	}
	for k, v := range want {
		if c.Undo.Tables[k] != v {
			t.Errorf("Tables[%q] = %q, want %q", k, c.Undo.Tables[k], v)
		}
	}
	if c.Undo.MaxRows != 10000 || c.Undo.Keep != 720*time.Hour {
		t.Errorf("defaults: MaxRows = %d, Keep = %s", c.Undo.MaxRows, c.Undo.Keep)
	}

	for _, tc := range []struct {
		undo    string
		maxRows int64
		keep    time.Duration
	}{
		{"{tables: {a: {pk: id}}, max_rows: 1}", 1, 720 * time.Hour},
		{"{tables: {a: {pk: id}}, max_rows: 1000000}", 1000000, 720 * time.Hour},
		{"{tables: {a: {pk: id}}, keep: 1h}", 10000, time.Hour},
		{"{tables: {a: {pk: id}}, keep: 2d}", 10000, 48 * time.Hour},
	} {
		c, err := Parse([]byte(minimal + "undo: " + tc.undo + "\n"))
		if err != nil {
			t.Fatalf("%s: %v", tc.undo, err)
		}
		if c.Undo.MaxRows != tc.maxRows || c.Undo.Keep != tc.keep {
			t.Errorf("%s: MaxRows = %d, Keep = %s", tc.undo, c.Undo.MaxRows, c.Undo.Keep)
		}
	}

	// The same pk as the gated table is fine.
	c, err = Parse([]byte(undoGate + "undo: {tables: {contacts: {pk: id}}}\n"))
	if err != nil {
		t.Fatalf("same pk: %v", err)
	}
	if c.Undo.Tables["contacts"] != "id" {
		t.Errorf("Tables = %v", c.Undo.Tables)
	}
}

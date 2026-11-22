package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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
		"stale remember too short":                    {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], stale: {remember: 59s}}}\n", "stale.remember"},
		"stale remember too long":                     {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], stale: {remember: 25h}}}\n", "stale.remember"},
		"stale remember not a duration":               {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], stale: {remember: soon}}}\n", "stale.remember"},
		"stale table unknown key":                     {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], stale: {keep: 1h}}}\n", "keep"},
		"stale without a table":                       {minimal + "stale: {max_keys: 5000}\n", "stale"},
		"stale max_keys too small":                    {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], stale: {}}}\nstale: {max_keys: 999}\n", "stale.max_keys"},
		"stale max_keys too big":                      {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], stale: {}}}\nstale: {max_keys: 10000001}\n", "stale.max_keys"},
		"stale unknown key":                           {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, send_fields: [a], stale: {}}}\nstale: {max_key: 5}\n", "max_key"},
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
	_, err := Parse([]byte(minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id}}\n"))
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

const staleGate = minimal + `model: {adapter: jev, version: jev-1.13.0}
tables:
  contacts:
    pk: id
    send_fields: [a]
`

func TestStaleAbsent(t *testing.T) {
	c, err := Parse([]byte(staleGate))
	if err != nil {
		t.Fatal(err)
	}
	if c.Tables["contacts"].Stale != nil || c.StaleMaxKeys != 0 {
		t.Errorf("stale = %+v, max keys = %d, want nil and 0", c.Tables["contacts"].Stale, c.StaleMaxKeys)
	}
}

func TestStaleSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		table, global string
		remember      time.Duration
		maxKeys       int
	}{
		"spec example":   {"    stale:\n      remember: 1h\n", "", time.Hour, 200000},
		"no value":       {"    stale:\n", "", time.Hour, 200000},
		"tilde":          {"    stale: ~\n", "", time.Hour, 200000},
		"empty map":      {"    stale: {}\n", "", time.Hour, 200000},
		"remember min":   {"    stale: {remember: 1m}\n", "", time.Minute, 200000},
		"remember max":   {"    stale: {remember: 24h}\n", "", 24 * time.Hour, 200000},
		"max_keys min":   {"    stale:\n", "stale: {max_keys: 1000}\n", time.Hour, 1000},
		"max_keys max":   {"    stale:\n", "stale: {max_keys: 10000000}\n", time.Hour, 10000000},
		"global no keys": {"    stale:\n", "stale: {}\n", time.Hour, 200000},
		"with reads":     {"    stale:\n    reads:\n", "", time.Hour, 200000},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Parse([]byte(staleGate + tc.table + tc.global))
			if err != nil {
				t.Fatal(err)
			}
			got := c.Tables["contacts"]
			if got.Stale == nil || got.Stale.Remember != tc.remember || c.StaleMaxKeys != tc.maxKeys {
				t.Errorf("stale = %+v, max keys = %d, want remember %s, max keys %d", got.Stale, c.StaleMaxKeys, tc.remember, tc.maxKeys)
			}
			if name == "with reads" && got.Reads == nil {
				t.Error("reads lost")
			}
		})
	}
}

// clearPGEnv keeps the machine's libpq variables out of the URL parameters
// under test.
func clearPGEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"PGSSLMODE", "PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY", "PGSSLSNI",
		"PGSSLNEGOTIATION", "PGSSLPASSWORD", "PGSSLCRL", "PGHOST", "PGPORT", "PGSERVICE", "PGSERVICEFILE", "PGPASSFILE"} {
		t.Setenv(k, "")
	}
}

// writeCert writes a self-signed certificate and its key into dir.
func writeCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "stet-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func parseUpstream(t *testing.T, upstream string) (*Config, error) {
	t.Helper()
	return Parse([]byte("listen: 127.0.0.1:6432\nupstream: " + upstream + "\n"))
}

func TestUpstreamTLSBySSLMode(t *testing.T) {
	clearPGEnv(t)
	root, _ := writeCert(t, t.TempDir())

	cases := []struct {
		name, query, host string
		wantTLS           bool
		wantFallback      bool
		wantInsecure      bool
		wantVerifyPeer    bool
		wantServerName    string
	}{
		{name: "disable", query: "?sslmode=disable", host: "db.example.com"},
		{name: "not written", query: "", host: "db.example.com", wantTLS: true, wantFallback: true, wantInsecure: true, wantServerName: "db.example.com"},
		{name: "prefer", query: "?sslmode=prefer", host: "db.example.com", wantTLS: true, wantFallback: true, wantInsecure: true, wantServerName: "db.example.com"},
		{name: "require", query: "?sslmode=require", host: "db.example.com", wantTLS: true, wantInsecure: true, wantServerName: "db.example.com"},
		{name: "require with sslrootcert", query: "?sslmode=require&sslrootcert=" + root, host: "db.example.com", wantTLS: true, wantInsecure: true, wantVerifyPeer: true, wantServerName: "db.example.com"},
		{name: "verify-ca", query: "?sslmode=verify-ca&sslrootcert=" + root, host: "db.example.com", wantTLS: true, wantInsecure: true, wantVerifyPeer: true, wantServerName: "db.example.com"},
		{name: "verify-full", query: "?sslmode=verify-full&sslrootcert=" + root, host: "db.example.com", wantTLS: true, wantServerName: "db.example.com"},
		{name: "require to an IP", query: "?sslmode=require", host: "10.1.2.3", wantTLS: true, wantInsecure: true},
		{name: "require with sslsni=0", query: "?sslmode=require&sslsni=0", host: "db.example.com", wantTLS: true, wantInsecure: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseUpstream(t, "postgres://app@"+tc.host+":5432/crm"+tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if (c.UpstreamTLS != nil) != tc.wantTLS {
				t.Fatalf("UpstreamTLS = %v, want present: %v", c.UpstreamTLS, tc.wantTLS)
			}
			if c.UpstreamPlainFallback != tc.wantFallback {
				t.Errorf("UpstreamPlainFallback = %v, want %v", c.UpstreamPlainFallback, tc.wantFallback)
			}
			if !tc.wantTLS {
				return
			}
			if c.UpstreamTLS.InsecureSkipVerify != tc.wantInsecure {
				t.Errorf("InsecureSkipVerify = %v, want %v", c.UpstreamTLS.InsecureSkipVerify, tc.wantInsecure)
			}
			if (c.UpstreamTLS.VerifyPeerCertificate != nil) != tc.wantVerifyPeer {
				t.Errorf("VerifyPeerCertificate set = %v, want %v", c.UpstreamTLS.VerifyPeerCertificate != nil, tc.wantVerifyPeer)
			}
			if c.UpstreamTLS.ServerName != tc.wantServerName {
				t.Errorf("ServerName = %q, want %q", c.UpstreamTLS.ServerName, tc.wantServerName)
			}
		})
	}
}

func TestUpstreamTLSCarriesNoClientCertificate(t *testing.T) {
	clearPGEnv(t)
	dir := t.TempDir()
	cert, key := writeCert(t, dir)
	addr := "postgres://app@db.example.com:5432/crm?sslmode=require&sslcert=" + cert + "&sslkey=" + key

	pc, err := pgconn.ParseConfig(addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(pc.TLSConfig.Certificates) != 1 {
		t.Fatalf("pgconn.ParseConfig carries %d certificates, want 1 (the premise of this test)", len(pc.TLSConfig.Certificates))
	}
	c, err := parseUpstream(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(c.UpstreamTLS.Certificates); n != 0 {
		t.Errorf("UpstreamTLS carries %d certificates, want none", n)
	}
	if c.UpstreamTLS.GetClientCertificate != nil {
		t.Error("UpstreamTLS.GetClientCertificate is set, want nil")
	}
}

func TestUpstreamRejectsAllowAndDirect(t *testing.T) {
	clearPGEnv(t)
	for query, want := range map[string]string{
		"?sslmode=allow":                         "upstream: sslmode=allow is not supported: it connects in plaintext first, which a relayed session cannot retry; use disable, prefer, require, verify-ca or verify-full",
		"?sslmode=require&sslnegotiation=direct": "upstream: sslnegotiation=direct is not supported: stet asks for TLS with an SSLRequest; remove it",
	} {
		_, err := parseUpstream(t, "postgres://app@db.example.com:5432/crm"+query)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to contain %q", query, err, want)
		}
	}
}

func TestUpstreamPassesParseErrorsAndHidesThePassword(t *testing.T) {
	clearPGEnv(t)
	_, err := parseUpstream(t, "postgres://app:s3cret-pw@db.example.com:5432/crm?sslmode=verify-full&sslrootcert=/no/such/file.pem")
	if err == nil || !strings.HasPrefix(err.Error(), "upstream: ") {
		t.Fatalf("err = %v, want an upstream: error", err)
	}
	if strings.Contains(err.Error(), "s3cret-pw") {
		t.Errorf("the error shows the password: %v", err)
	}
}

func TestUpstreamTransport(t *testing.T) {
	clearPGEnv(t)
	root, _ := writeCert(t, t.TempDir())
	for query, want := range map[string]Transport{
		"?sslmode=disable":                       TransportPlain,
		"":                                       TransportPreferred,
		"?sslmode=prefer":                        TransportPreferred,
		"?sslmode=require":                       TransportUnverified,
		"?sslmode=require&sslrootcert=" + root:   TransportVerified,
		"?sslmode=verify-ca&sslrootcert=" + root: TransportVerified,
		"?sslmode=verify-full&sslrootcert=" + root: TransportVerified,
	} {
		c, err := parseUpstream(t, "postgres://app@db.example.com:5432/crm"+query)
		if err != nil {
			t.Fatalf("%q: %v", query, err)
		}
		if got := c.UpstreamTransport(); got != want {
			t.Errorf("%q: UpstreamTransport = %d, want %d", query, got, want)
		}
	}
}

// The words stet proxy's start line gives the leg to upstream (upstream_tls).
func TestTransportString(t *testing.T) {
	for tr, want := range map[Transport]string{
		TransportPlain:      "off",
		TransportPreferred:  "preferred",
		TransportUnverified: "unverified",
		TransportVerified:   "verified",
		Transport(9):        "Transport(9)",
	} {
		if got := tr.String(); got != want {
			t.Errorf("Transport(%d).String() = %q, want %q", int(tr), got, want)
		}
	}
}

func TestSendFieldsOptionalWithoutAModel(t *testing.T) {
	c, err := Parse([]byte(minimal + "model: {adapter: none}\ntables: {contacts: {pk: id}}\n"))
	if err != nil {
		t.Fatalf("adapter none without send_fields: %v", err)
	}
	if n := len(c.Tables["contacts"].SendFields); n != 0 {
		t.Errorf("%d send_fields, want none", n)
	}
	_, err = Parse([]byte(minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id}}\n"))
	want := "tables.contacts.send_fields: required: list the columns the model may see"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
}

// twoTargets is OneTarget's error.
const twoTargets = "stet's own connections and the proxy would reach different hosts or ports for this address; " +
	"write one host and one port in the address itself (not in its query, PGHOST, PGPORT or a service file), " +
	"and percent-encode special characters in the password"

func TestOneTarget(t *testing.T) {
	const pw = "s3cr3t-pw"
	services := filepath.Join(t.TempDir(), "pg_service.conf")
	if err := os.WriteFile(services, []byte("[elsewhere]\nport=5433\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, upstream string
		env            map[string]string
		ok             bool
	}{
		{name: "one host and port", upstream: "postgres://app:" + pw + "@db.example.com:5432/crm?sslmode=disable", ok: true},
		{name: "the default port", upstream: "postgres://app:" + pw + "@db.example.com/crm", ok: true},
		// prefer tries TLS, then plaintext: two fallbacks to one place.
		{name: "prefer", upstream: "postgres://app:" + pw + "@db.example.com:5432/crm?sslmode=prefer", ok: true},
		{name: "IPv6", upstream: "postgres://app:" + pw + "@[::1]:5432/crm", ok: true},

		// pgconn drops spaces around a query key; net/url keeps them.
		{name: "a space before a query key", upstream: "postgres://app:" + pw + "@db.example.com/crm?sslmode=disable& host=evil.example.com"},
		{name: "a space after a query key", upstream: "postgres://app:" + pw + "@db.example.com/crm?host =evil.example.com"},
		// pgconn ends the user information at the first @ before a /; net/url
		// ends the authority at /, ? or #.
		{name: "# in the password", upstream: "postgres://app:#" + pw + "@db.example.com/crm?sslmode=verify-full"},
		{name: "? in the password", upstream: "postgres://app:12?" + pw + "@db.example.com/crm"},
		{name: "? in the user name, no host for pgconn", upstream: "postgres://h?x:" + pw + "@/crm"},
		{name: "? in the user name, no password", upstream: "postgres://h?@/crm"},
		// Where the URL has no port, the environment or a service file gives it.
		{name: "PGPORT", upstream: "postgres://app:" + pw + "@db.example.com/crm", env: map[string]string{"PGPORT": "5433"}},
		{name: "a service file's port", upstream: "postgres://app:" + pw + "@db.example.com/crm?service=elsewhere",
			env: map[string]string{"PGSERVICEFILE": services}},
		{name: "host in the query", upstream: "postgres://app:" + pw + "@db.example.com/crm?host=other.example.com"},
		{name: "port in the query", upstream: "postgres://app:" + pw + "@db.example.com:5432/crm?port=5433"},
		{name: "several hosts", upstream: "postgres://app:" + pw + "@h1:5432,h2:5432/crm"},
		{name: "several hosts, one port", upstream: "postgres://app:" + pw + "@db.example.com,db2.example.com/crm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearPGEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			err := OneTarget(tc.upstream)
			switch {
			case tc.ok && err != nil:
				t.Errorf("err = %v, want none", err)
			case !tc.ok && (err == nil || err.Error() != twoTargets):
				t.Errorf("err = %v, want %q", err, twoTargets)
			case err != nil && strings.Contains(err.Error(), pw):
				t.Errorf("the error holds the password: %v", err)
			}
		})
	}
}

func TestParseRefusesAnUpstreamReadTwoWays(t *testing.T) {
	clearPGEnv(t)
	// The proxy would relay to app:12, Stet's own connections go to db.example.com.
	_, err := parseUpstream(t, "postgres://app:12?s3cr3t-pw@db.example.com/crm")
	if want := "upstream: " + twoTargets; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestUpstreamErrorsNeverQuoteTheAddress(t *testing.T) {
	clearPGEnv(t)
	for upstream, want := range map[string]string{
		"mysql://app:s3cr3t-pw@db.example.com/crm":         "upstream: must be a postgres:// or postgresql:// URL",
		"postgres://app:s3cr3t-pw@/crm":                    "upstream: has no host",
		"postgres://app:s3cr3t-pw@db.example.com:port/crm": "upstream: is not a valid URL",
	} {
		if _, err := parseUpstream(t, upstream); err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", upstream, err, want)
		}
	}
}

func TestLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "127.0.0.1": true, "127.0.0.2": true, "::1": true,
		"0.0.0.0": false, "::": false, "": false, "db.example.com": false, "10.0.0.5": false,
	} {
		if got := LoopbackHost(host); got != want {
			t.Errorf("LoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestListenTLS(t *testing.T) {
	c, err := Parse([]byte(minimal + "listen_tls: {cert: /etc/stet/c.pem, key: /etc/stet/k.pem}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenTLS == nil || c.ListenTLS.Cert != "/etc/stet/c.pem" || c.ListenTLS.Key != "/etc/stet/k.pem" {
		t.Errorf("ListenTLS = %+v", c.ListenTLS)
	}
	for name, yml := range map[string]string{
		"cert only": "listen_tls: {cert: /c.pem}\n",
		"key only":  "listen_tls: {key: /k.pem}\n",
		"empty":     "listen_tls: {}\n",
	} {
		_, err := Parse([]byte(minimal + yml))
		if err == nil || !strings.Contains(err.Error(), "listen_tls: cert and key are both required") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	c, err = Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenTLS != nil {
		t.Errorf("ListenTLS = %+v, want nil", c.ListenTLS)
	}
}

const custodyListen = "listen: 127.0.0.1:6432\n"

func custodyYAML(listen, upstream, extra string) []byte {
	return []byte("listen: " + listen + "\nupstream: " + upstream + "\nagents: {credentials: creds.yaml}\n" + extra)
}

func TestCustodyNeedsAVerifiedUpstream(t *testing.T) {
	clearPGEnv(t)
	root, _ := writeCert(t, t.TempDir())
	const remote = "postgres://app:s3cret-pw@db.example.com:5432/crm"
	const want = "upstream: with agents.credentials the server must be verified"
	for query, ok := range map[string]bool{
		"?sslmode=disable":                       false,
		"":                                       false,
		"?sslmode=prefer":                        false,
		"?sslmode=require":                       false,
		"?sslmode=verify-full":                   true,
		"?sslmode=verify-ca&sslrootcert=" + root: true,
		"?sslmode=require&sslrootcert=" + root:   true,
		"?sslmode=verify-full&sslrootcert=" + root: true,
	} {
		c, err := Parse(custodyYAML("127.0.0.1:6432", remote+query, ""))
		if ok {
			if err != nil {
				t.Errorf("%q: %v", query, err)
			} else if c.Credentials != "creds.yaml" {
				t.Errorf("%q: Credentials = %q", query, c.Credentials)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v", query, err)
		} else if strings.Contains(err.Error(), "s3cret-pw") || strings.Contains(err.Error(), "db.example.com") {
			t.Errorf("%q: error quotes the address: %v", query, err)
		}
	}
	for _, q := range []string{"?sslmode=disable", "", "?sslmode=require"} {
		if _, err := Parse(custodyYAML("127.0.0.1:6432", "postgres://app@127.0.0.1:5432/crm"+q, "")); err != nil {
			t.Errorf("loopback %q: %v", q, err)
		}
	}
	// Without custody a remote require passes, as before.
	if _, err := Parse([]byte(custodyListen + "upstream: " + remote + "?sslmode=require\n")); err != nil {
		t.Errorf("no custody: %v", err)
	}
}

func TestCustodyNeedsTLSOnARemoteListen(t *testing.T) {
	clearPGEnv(t)
	const up = "postgres://app:s3cret-pw@127.0.0.1:5432/crm"
	const tlsKey = "listen_tls: {cert: c.pem, key: k.pem}\n"
	for _, listen := range []string{"0.0.0.0:6432", ":6432", "10.0.0.5:6432"} {
		_, err := Parse(custodyYAML(listen, up, ""))
		if err == nil || !strings.Contains(err.Error(), "listen: with agents.credentials a listen address that is not loopback needs listen_tls") {
			t.Errorf("%q without listen_tls: err = %v", listen, err)
		} else if strings.Contains(err.Error(), "s3cret-pw") || strings.Contains(err.Error(), listen) {
			t.Errorf("%q: error quotes an address: %v", listen, err)
		}
		if _, err := Parse(custodyYAML(listen, up, tlsKey)); err != nil {
			t.Errorf("%q with listen_tls: %v", listen, err)
		}
	}
	if _, err := Parse(custodyYAML("127.0.0.1:6432", up, "")); err != nil {
		t.Errorf("loopback listen: %v", err)
	}
	// Without custody a remote listen needs nothing.
	if _, err := Parse([]byte("listen: 0.0.0.0:6432\nupstream: " + up + "\n")); err != nil {
		t.Errorf("no custody: %v", err)
	}
}

func TestEnforceWithCredentialsNeedsNoRoles(t *testing.T) {
	const model = "model: {adapter: jev, version: jev-1.13.0}\n"
	c, err := Parse([]byte(minimal + "mode: enforce\nagents: {credentials: creds.yaml}\n" + model))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AgentRoles) != 0 || c.Credentials != "creds.yaml" {
		t.Errorf("AgentRoles = %v, Credentials = %q", c.AgentRoles, c.Credentials)
	}
	_, err = Parse([]byte(minimal + "mode: enforce\n" + model))
	if err == nil || !strings.Contains(err.Error(), "agents.roles: required in enforce mode; list the login roles of the agents") {
		t.Errorf("no roles, no credentials: err = %v", err)
	}
}

func TestLoadResolvesPathsAgainstTheConfigFile(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(t.TempDir(), "abs-cert.pem")
	write := func(yml string) *Config {
		t.Helper()
		p := filepath.Join(dir, "stet.yaml")
		if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := write(minimal + "agents: {credentials: creds.yaml}\nlisten_tls: {cert: tls/c.pem, key: ../k.pem}\n")
	if c.Credentials != filepath.Join(dir, "creds.yaml") {
		t.Errorf("Credentials = %q", c.Credentials)
	}
	if c.ListenTLS.Cert != filepath.Join(dir, "tls/c.pem") || c.ListenTLS.Key != filepath.Join(dir, "../k.pem") {
		t.Errorf("ListenTLS = %+v", c.ListenTLS)
	}
	c = write(minimal + "agents: {credentials: " + abs + "}\nlisten_tls: {cert: " + abs + ", key: " + abs + "}\n")
	if c.Credentials != abs || c.ListenTLS.Cert != abs || c.ListenTLS.Key != abs {
		t.Errorf("absolute paths changed: %q %+v", c.Credentials, c.ListenTLS)
	}
	// Parse leaves them as written.
	c, err := Parse([]byte(minimal + "agents: {credentials: creds.yaml}\n"))
	if err != nil || c.Credentials != "creds.yaml" {
		t.Errorf("Parse: %q, %v", c.Credentials, err)
	}
}

package config

import (
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
	for _, v := range []string{"", "latest", "LATEST", "jev-latest", "jev:latest", "jev@stable", "jev"} {
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
		"unknown field":     {minimal + "mdoe: shadow\n", "mdoe"},
		"bad mode":          {minimal + "mode: block\n", "mode"},
		"no listen":         {"upstream: postgres://a@b/c\n", "listen"},
		"no upstream":       {"listen: :6432\n", "upstream"},
		"upstream not pg":   {"listen: :6432\nupstream: mysql://a@b/c\n", "upstream"},
		"tables need model": {minimal + "tables: {contacts: {pk: id}}\n", "model"},
		"table needs pk":    {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {}}\n", "pk"},
		"bad on_failure":    {minimal + "model: {adapter: jev, version: jev-1.13.0, on_failure: retry}\n", "on_failure"},
		"bad unsupported":   {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, unsupported: allow}}\n", "unsupported"},
		"bad question":      {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, questions: [vibes]}}\n", "questions"},
		"threshold > 1":     {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, thresholds: {duplicate: 1.5}}}\n", "thresholds"},
		"policy score > 5":  {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, thresholds: {policy: 6}}}\n", "thresholds"},
		"bad recent_window": {minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, recent_window: soon}}\n", "recent_window"},
		"bad retention":     {minimal + "retention: forever\n", "retention"},
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

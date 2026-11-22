package main

import (
	"strings"
	"testing"
)

func TestTransportWarnings(t *testing.T) {
	const (
		plaintext  = "allows plaintext"
		unverified = "certificate is not checked"
		listenWarn = "is not a loopback address"
	)
	for _, tc := range []struct {
		name, listen, upstream string
		want                   []string // a fragment of each warning, in order
	}{
		{"remote disable", "127.0.0.1:6432", "db.example.com/d?sslmode=disable", []string{plaintext}},
		{"remote prefer", "127.0.0.1:6432", "db.example.com/d?sslmode=prefer", []string{plaintext}},
		{"remote no sslmode", "127.0.0.1:6432", "db.example.com/d", []string{plaintext}},
		{"remote require", "127.0.0.1:6432", "db.example.com/d?sslmode=require", []string{unverified}},
		{"remote verify-full", "127.0.0.1:6432", "db.example.com/d?sslmode=verify-full", nil},
		{"remote verify-ca", "127.0.0.1:6432", "db.example.com/d?sslmode=verify-ca", nil},
		{"loopback disable", "127.0.0.1:6432", "localhost/d?sslmode=disable", nil},
		{"loopback require", "127.0.0.1:6432", "127.0.0.1/d?sslmode=require", nil},
		{"loopback ipv6", "[::1]:6432", "[::1]/d?sslmode=prefer", nil},
		{"listen on all", "0.0.0.0:6432", "127.0.0.1/d?sslmode=disable", []string{listenWarn}},
		{"listen port only", ":6432", "127.0.0.1/d?sslmode=disable", []string{listenWarn}},
		{"listen remote name", "stet.example.com:6432", "127.0.0.1/d", []string{listenWarn}},
		{"both", "0.0.0.0:6432", "db.example.com/d?sslmode=disable", []string{plaintext, listenWarn}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseConfig(t, "mode: shadow\nlisten: %q\nupstream: postgres://stet@%s\n", tc.listen, tc.upstream)
			got := transportWarnings(cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("warnings = %q, want %d of them (%q)", got, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("warning %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

// The start line says how the leg to upstream is protected: an upstream
// with no sslmode written is preferred (TLS when the server takes it,
// unverified), which INFO shows to whoever upgrades from plaintext.
func TestListeningLineSaysHowUpstreamIsProtected(t *testing.T) {
	for query, want := range map[string]string{
		"?sslmode=disable":     "off",
		"":                     "preferred",
		"?sslmode=prefer":      "preferred",
		"?sslmode=require":     "unverified",
		"?sslmode=verify-full": "verified",
	} {
		t.Run(want+query, func(t *testing.T) {
			r := start(t, parseConfig(t, "mode: shadow\nlisten: 127.0.0.1:0\nupstream: postgres://stet@127.0.0.1:1/db%s\n", query))
			waitListening(t, r)
			var line string
			for l := range strings.SplitSeq(r.log.String(), "\n") {
				if strings.Contains(l, `msg="stet proxy listening"`) {
					line = l
				}
			}
			if !strings.Contains(line, " upstream=127.0.0.1:1 upstream_tls="+want+" mode=shadow ") {
				t.Errorf("the start line %q, want upstream_tls=%s after upstream", line, want)
			}
		})
	}
}

func TestTransportWarningsNameNoPassword(t *testing.T) {
	for _, mode := range []string{"disable", "prefer", "require"} {
		cfg := parseConfig(t, "mode: shadow\nlisten: 0.0.0.0:6432\nupstream: postgres://stet:hunter2-secret@db.example.com:5432/d?sslmode=%s\n", mode)
		warnings := transportWarnings(cfg)
		if len(warnings) < 2 {
			t.Fatalf("sslmode=%s: warnings = %q, want the upstream's and the listen address's", mode, warnings)
		}
		for _, w := range warnings {
			if strings.Contains(w, "hunter2-secret") || strings.Contains(w, "5432") {
				t.Errorf("sslmode=%s: warning %q names the password or the full address", mode, w)
			}
		}
		if !strings.Contains(warnings[0], "upstream db.example.com ") {
			t.Errorf("sslmode=%s: warning %q does not name the upstream host", mode, warnings[0])
		}
	}
}

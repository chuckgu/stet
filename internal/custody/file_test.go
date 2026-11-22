package custody

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/scram"
)

// newToken returns a fresh verifier in its file form.
func newToken(t *testing.T) string {
	t.Helper()
	_, v, err := scram.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return v.Text()
}

func writeRaw(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMode(t *testing.T) {
	const me, other = 1000, 2000
	cases := []struct {
		mode  fs.FileMode
		owner uint32
		ok    bool
	}{
		{0o600, me, true}, {0o400, me, true},
		{0o640, me, false}, {0o644, me, false}, {0o660, me, false},
		{0o640, 0, true}, {0o600, 0, true}, {0o440, 0, true},
		{0o644, 0, false}, {0o660, 0, false},
		{0o600, other, false}, {0o400, other, false}, {0o640, other, false},
	}
	for _, c := range cases {
		err := CheckMode(c.mode, c.owner, me)
		if (err == nil) != c.ok {
			t.Errorf("CheckMode(%04o, owner %d) = %v, want ok=%v", c.mode, c.owner, err, c.ok)
		}
	}
}

func TestReadValidates(t *testing.T) {
	good := newToken(t)
	const pw = "pw-secret-1"
	cases := map[string]struct {
		content string
		want    string // in the error
	}{
		"no token":        {"agents:\n  alice:\n    password: " + pw + "\n", "alice"},
		"broken token":    {"agents:\n  alice:\n    token: SCRAM-SHA-256$junk-secret\n    password: " + pw + "\n", "alice"},
		"both":            {"agents:\n  alice:\n    token: " + good + "\n    password: " + pw + "\n    password_env: X\n", "alice"},
		"neither":         {"agents:\n  alice:\n    token: " + good + "\n", "alice"},
		"empty name":      {"agents:\n  \"\":\n    token: " + good + "\n    password: " + pw + "\n", "empty"},
		"64 byte name":    {"agents:\n  " + strings.Repeat("a", 64) + ":\n    token: " + good + "\n    password: " + pw + "\n", strings.Repeat("a", 64)},
		"unknown key":     {"agents:\n  alice:\n    token: " + good + "\n    password: " + pw + "\n    colour: blue\n", "alice"},
		"bad time":        {"agents:\n  alice:\n    token: " + good + "\n    password: " + pw + "\n    added: " + pw + "\n", "alice"},
		"unknown top key": {"agents: {}\nextra: 1\n", "extra"},
		"short mock":      {"mock_secret: c2hvcnQ=\nagents: {}\n", "mock_secret"},
		"mock not base64": {"mock_secret: \"!!pw-secret-1\"\nagents: {}\n", "mock_secret"},
		"empty file":      {"", "empty"},
		"not yaml":        {"agents: [pw-secret-1\n", "line 1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "creds.yaml")
			writeRaw(t, path, c.content, 0o600)
			_, err := Read(path)
			if err == nil {
				t.Fatal("Read accepted the file")
			}
			msg := err.Error()
			if !strings.Contains(msg, c.want) {
				t.Errorf("error %q does not mention %q", msg, c.want)
			}
			for _, secret := range []string{pw, good, "junk-secret"} {
				if strings.Contains(msg, secret) {
					t.Errorf("error %q has a secret", msg)
				}
			}
		})
	}

	t.Run("open mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "creds.yaml")
		writeRaw(t, path, "agents:\n  alice:\n    token: "+good+"\n    password: "+pw+"\n", 0o644)
		_, err := Read(path)
		if err == nil || strings.Contains(err.Error(), pw) {
			t.Fatalf("Read of a 0644 file = %v, want a refusal", err)
		}
	})
	t.Run("no mock_secret and no agents are fine", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "creds.yaml")
		writeRaw(t, path, "agents: {}\n", 0o600)
		f, err := Read(path)
		if err != nil || len(f.Agents) != 0 || f.MockSecret != "" {
			t.Fatalf("Read = %+v, %v", f, err)
		}
		writeRaw(t, path, "mock_secret: \"\"\n", 0o600)
		if _, err := Read(path); err != nil {
			t.Fatalf("a file with no agents key: %v", err)
		}
	})
}

func sample(t *testing.T) *File {
	when := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	return &File{Agents: map[string]Agent{
		"alice": {Token: newToken(t), Password: "pw-a", Added: when, TokenIssued: when},
		"bob":   {Token: newToken(t), PasswordEnv: "BOB_PW", Added: when, TokenIssued: when},
	}}
}

func TestWriteIsWholeAndKeepsTheMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.yaml")
	f := sample(t)
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("new file mode = %v, want 0600", info.Mode().Perm())
	}
	if f.MockSecret == "" {
		t.Error("Write left the mock secret empty")
	}
	got, err := Read(path)
	if err != nil || !reflect.DeepEqual(got, f) {
		t.Fatalf("Read = %+v, %v; want %+v", got, err, f)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "# Written by stet agent. It holds database passwords: keep it private.\n") {
		t.Errorf("no header comment:\n%.80s", b)
	}
	if strings.Index(string(b), "alice:") > strings.Index(string(b), "bob:") {
		t.Error("accounts are not in name order")
	}

	// A value there is kept; an empty one is filled.
	secret := f.MockSecret
	f.Agents["alice"] = Agent{Token: newToken(t), Password: "pw-a2"}
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	if got, _ := Read(path); got.MockSecret != secret {
		t.Error("Write changed a mock secret that was there")
	}

	// An existing 0400 file keeps its mode when it is replaced.
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	f.Agents["alice"] = Agent{Token: newToken(t), Password: "pw-a3"}
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if after.Mode().Perm() != 0o400 {
		t.Errorf("mode = %v, want 0400 kept", after.Mode().Perm())
	}
	if os.SameFile(before, after) {
		t.Error("the file was written in place, not replaced")
	}
	if got, err := Read(path); err != nil || got.Agents["alice"].Password != "pw-a3" {
		t.Fatalf("Read after rewrite = %+v, %v", got, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("directory has %d entries, want only the file", len(entries))
	}
}

func TestWriteInAnUnwritableDirectory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := Write(filepath.Join(dir, "creds.yaml"), sample(t)); err == nil {
		t.Fatal("Write into an unwritable directory succeeded")
	}
	os.Chmod(dir, 0o700)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %d files behind", len(entries))
	}
}

func TestWriteRefusesWhatReadWouldRefuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.yaml")
	f := &File{Agents: map[string]Agent{"alice": {Token: "nope", Password: "pw-secret-1"}}}
	err := Write(path, f)
	if err == nil || strings.Contains(err.Error(), "pw-secret-1") {
		t.Fatalf("Write = %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a file was written")
	}
}

func TestFileRoundTripsOddAccountNames(t *testing.T) {
	names := []string{"null", "on", "Agent", "agent", "has space", "a: b", "123", "~", "true", strings.Repeat("n", 63), "ünï"}
	f := &File{Agents: map[string]Agent{}}
	for _, n := range names {
		f.Agents[n] = Agent{Token: newToken(t), Password: "pw"}
	}
	path := filepath.Join(t.TempDir(), "creds.yaml")
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != len(names) {
		t.Fatalf("read %d accounts, wrote %d", len(got.Agents), len(names))
	}
	for _, n := range names {
		if _, ok := got.Agents[n]; !ok {
			t.Errorf("account %q did not come back", n)
		}
	}
}

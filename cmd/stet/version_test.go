package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionLine(t *testing.T) {
	oldV, oldC := version, commit
	t.Cleanup(func() { version, commit = oldV, oldC })

	version, commit = "0.2.0", "abcdef123456"
	got := versionLine()
	if !strings.HasPrefix(got, "stet 0.2.0 (commit abcdef123456, go1.") || !strings.HasSuffix(got, ")") {
		t.Fatalf("versionLine() = %q", got)
	}

	version, commit = "dev", ""
	if got := versionLine(); !strings.HasPrefix(got, "stet dev (commit ") {
		t.Fatalf("default versionLine() = %q", got)
	}
}

func TestVersionCommand(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "stet")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=0.2.0 -X main.commit=abcdef123456", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("stet version: %v", err)
	}
	if got := string(out); !strings.HasPrefix(got, "stet 0.2.0 (commit abcdef123456, go1.") || !strings.HasSuffix(got, ")\n") {
		t.Fatalf("stet version printed %q", got)
	}
}

func TestUsageListsVersion(t *testing.T) {
	if !strings.Contains(usage, "\n  version ") {
		t.Fatalf("usage does not list version:\n%s", usage)
	}
}

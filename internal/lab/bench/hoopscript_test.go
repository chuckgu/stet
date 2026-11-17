package bench

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These tests run scripts/hoop.sh against a stub docker first on PATH: no image is pulled and no
// container runs.

const hoopScriptFile = "../../../scripts/hoop.sh"

// pinnedDigest is the image digest the script pins by default (hoophq/hoopsidecar 1.310.0).
const pinnedDigest = "sha256:737e87b74a682e326a0c39f594a57d3d22e729fc2d106216eef78b47cc7558d1"

// fakeRef is what the stub's docker inspect prints unless a test says otherwise.
var fakeRef = "hoophq/hoopsidecar@sha256:" + strings.Repeat("ab", 32)

// hoopTestKey is the key the test's env file holds. It must reach the key file and nothing else.
const hoopTestKey = "sk-test-KEY"

// stubDocker records each call as a line "== docker" and then one argument per line. For inspect it
// prints $STUB_REF; for a container that runs date, the host's time shifted by $STUB_SKEW seconds;
// for run -d, a container id.
const stubDocker = `#!/bin/sh
{ echo '== docker'; for a in "$@"; do printf '%s\n' "$a"; done; } >> "$STUB_LOG"
case "$1" in
inspect) echo "$STUB_REF" ;;
run)
	for a in "$@"; do
		if [ "$a" = date ]; then
			exec perl -MTime::HiRes=time -e 'printf "%.9f\n", time + ($ENV{STUB_SKEW} || 0)'
		fi
	done
	case " $* " in *" -d "*) echo 0f1e2d3c4b5a ;; esac ;;
esac
exit 0
`

// containerConfig is laid out for the container the script runs, as lab/bench/hoop.yaml is (unless
// the runbook's fallback 1 moves it to the host): the audit log in /var/log/hoop, the key at
// /run/hoop/anthropic.key, the listener on port 15432.
const containerConfig = `log_level: info
audit:
  file: /var/log/hoop/audit.jsonl
analyzer:
  provider: anthropic
  model: claude-haiku-5-5
  credentials_file: /run/hoop/anthropic.key
  endpoint: http://host.docker.internal:18080/v1/messages
listeners:
  - name: crm
    protocol: postgres
    listen: 0.0.0.0:15432
    upstream: host.docker.internal:55432
`

// hoopRig is one test's environment for the script: the stub, a bench directory, a key file to
// read, and a working directory holding cfg.yaml (containerConfig).
type hoopRig struct {
	bench, work, log string
	env              []string
	outputs          []string // every run's combined output, in order
}

func newHoopRig(t *testing.T) *hoopRig {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	r := &hoopRig{bench: filepath.Join(dir, "bench"), work: filepath.Join(dir, "work"), log: filepath.Join(dir, "docker.log")}
	for _, d := range []string{bin, r.work} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(stubDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(dir, "anthropic.env")
	if err := os.WriteFile(envFile, []byte("export ANTHROPIC_API_KEY="+hoopTestKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.work, "cfg.yaml"), []byte(containerConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	// Built from nothing, so neither DOCKER nor HOOP_USER comes from the caller's environment.
	r.env = []string{
		"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + dir,
		"BENCH_DIR=" + r.bench,
		"ANTHROPIC_ENV=" + envFile,
		"HOOP_DIGEST=" + fakeRef[strings.Index(fakeRef, "@")+1:],
		"STUB_LOG=" + r.log,
		"STUB_REF=" + fakeRef,
	}
	return r
}

// run runs the script in the working directory with extra environment, returning its combined
// output and whether it exited 0.
func (r *hoopRig) run(t *testing.T, extra []string, args ...string) (string, bool) {
	t.Helper()
	script, err := filepath.Abs(hoopScriptFile)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, args...)
	cmd.Dir = r.work
	cmd.Env = append(slices.Clone(r.env), extra...)
	out, err := cmd.CombinedOutput()
	r.outputs = append(r.outputs, string(out))
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("hoop.sh %s: %v", strings.Join(args, " "), err)
		}
	}
	return string(out), err == nil
}

// calls returns the stub's calls so far, each its argument list.
func (r *hoopRig) calls(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile(r.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if line == "== docker" {
			calls = append(calls, []string{})
			continue
		}
		calls[len(calls)-1] = append(calls[len(calls)-1], line)
	}
	return calls
}

// starts returns the calls that start the proxy's container.
func (r *hoopRig) starts(t *testing.T) [][]string {
	t.Helper()
	var out [][]string
	for _, c := range r.calls(t) {
		if len(c) > 1 && c[0] == "run" && c[1] == "-d" {
			out = append(out, c)
		}
	}
	return out
}

// values returns the argument after each occurrence of flag.
func values(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	return err == nil && ra == rb
}

func TestHoopScriptStartsTheImageByDigestWithThreeMounts(t *testing.T) {
	r := newHoopRig(t)
	out, ok := r.run(t, nil, "start", "cfg.yaml", "run1")
	if ok || !strings.Contains(out, "pull first") {
		t.Fatalf("start before pull: ok %v, output %q; want a failure saying pull first", ok, out)
	}
	if len(r.starts(t)) != 0 {
		t.Fatal("start before pull started a container")
	}

	out, ok = r.run(t, nil, "pull")
	if !ok || !strings.Contains(out, fakeRef) {
		t.Fatalf("pull: ok %v, output %q", ok, out)
	}
	if got := string(readFile(t, filepath.Join(r.bench, "hoop.digest"))); got != fakeRef+"\n" {
		t.Errorf("hoop.digest = %q, want %q", got, fakeRef)
	}
	calls := r.calls(t)
	if want := [][]string{
		{"pull", "hoophq/hoopsidecar:1.310.0"},
		{"inspect", "--format", "{{index .RepoDigests 0}}", "hoophq/hoopsidecar:1.310.0"},
	}; !slices.EqualFunc(calls, want, slices.Equal) {
		t.Errorf("pull ran %q, want %q", calls, want)
	}

	// A relative config path: docker -v would read it as a volume name.
	out, ok = r.run(t, nil, "start", "cfg.yaml", "run1")
	if !ok {
		t.Fatalf("start: %s", out)
	}
	starts := r.starts(t)
	if len(starts) != 1 {
		t.Fatalf("start ran %d containers", len(starts))
	}
	args := starts[0]
	if args[len(args)-1] != fakeRef {
		t.Errorf("the image is %q, want the digest reference %s", args[len(args)-1], fakeRef)
	}
	if got := values(args, "--name"); !slices.Equal(got, []string{"bench-hoop"}) {
		t.Errorf("--name %v", got)
	}
	mounts := values(args, "-v")
	if len(mounts) != 3 {
		t.Fatalf("-v %q, want three mounts", mounts)
	}
	src := func(m, target string) string { return strings.TrimSuffix(m, ":"+target) }
	for i, target := range []string{"/etc/hoop-inspect/config.yaml:ro", "/run/hoop/anthropic.key:ro", "/var/log/hoop"} {
		if !strings.HasSuffix(mounts[i], ":"+target) {
			t.Errorf("mount %d = %q, want it to end in :%s", i, mounts[i], target)
		}
		if !filepath.IsAbs(src(mounts[i], target)) {
			t.Errorf("mount %d's source %q is not an absolute path", i, mounts[i])
		}
	}
	if !samePath(t, src(mounts[0], "/etc/hoop-inspect/config.yaml:ro"), filepath.Join(r.work, "cfg.yaml")) {
		t.Errorf("the config mount %q is not cfg.yaml", mounts[0])
	}
	key := filepath.Join(r.bench, "hoop", "anthropic.key")
	if !samePath(t, src(mounts[1], "/run/hoop/anthropic.key:ro"), key) {
		t.Errorf("the key mount %q is not %s", mounts[1], key)
	}
	audit := filepath.Join(r.bench, "hoop", "audit-run1")
	if !samePath(t, src(mounts[2], "/var/log/hoop"), audit) {
		t.Errorf("the audit mount %q is not %s", mounts[2], audit)
	}
	if got := values(args, "-e"); !slices.Equal(got, []string{"HOOP_SIDECAR_FREE_STANDALONE=true", "HOOP_SIDECAR_ANALYTICS=off"}) {
		t.Errorf("-e %q", got)
	}
	if got := values(args, "-p"); !slices.Equal(got, []string{"127.0.0.1:15432:15432"}) {
		t.Errorf("-p %q", got)
	}
	if got, want := values(args, "--user"), fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()); !slices.Equal(got, []string{want}) {
		t.Errorf("--user %q, want %s", got, want)
	}

	st, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("the key file's mode is %o, want 600", st.Mode().Perm())
	}
	if got := string(readFile(t, key)); got != hoopTestKey {
		t.Errorf("the key file holds %q, want the key alone", got)
	}
	if st, err := os.Stat(audit); err != nil || !st.IsDir() {
		t.Errorf("no audit directory %s: %v", audit, err)
	}

	// HOOP_USER set and empty: the image's own user.
	out, ok = r.run(t, []string{"HOOP_USER="}, "start", "cfg.yaml", "run2")
	if !ok {
		t.Fatalf("start with HOOP_USER empty: %s", out)
	}
	starts = r.starts(t)
	if len(starts) != 2 || slices.Contains(starts[1], "--user") {
		t.Errorf("with HOOP_USER empty the start is %q, want no --user", starts[len(starts)-1])
	}

	// Nothing ever passes the key, or runs the container with more than it needs.
	for _, c := range r.calls(t) {
		for _, a := range c {
			if strings.Contains(a, hoopTestKey) {
				t.Errorf("an argument carries the key: %q", c)
			}
			if a == "--privileged" || strings.HasPrefix(a, "--network") || strings.HasPrefix(a, "--net=") || a == "--net" {
				t.Errorf("docker %q", c)
			}
		}
	}
	for _, o := range r.outputs {
		if strings.Contains(o, hoopTestKey) {
			t.Errorf("an output shows the key: %q", o)
		}
	}
}

func TestHoopScriptGivesEachRunItsOwnAuditLog(t *testing.T) {
	r := newHoopRig(t)
	if out, ok := r.run(t, nil, "pull"); !ok {
		t.Fatal(out)
	}
	if out, ok := r.run(t, nil, "start", "cfg.yaml", "practice-a"); !ok {
		t.Fatal(out)
	}
	// hoop appends to its log: a second run under the same name would mix two runs' lines.
	log := filepath.Join(r.bench, "hoop", "audit-practice-a", "audit.jsonl")
	if err := os.WriteFile(log, []byte(`{"kind":"statement"}`+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	out, ok := r.run(t, nil, "start", "cfg.yaml", "practice-a")
	if ok || !strings.Contains(out, "audit-practice-a") {
		t.Errorf("start under a used name: ok %v, output %q; want a refusal naming the log", ok, out)
	}
	if n := len(r.starts(t)); n != 1 {
		t.Errorf("%d containers started, want the first only", n)
	}
	for _, name := range []string{"", "../x", "a/b", "Run1", "-x"} {
		if out, ok := r.run(t, nil, "start", "cfg.yaml", name); ok {
			t.Errorf("run name %q: started (%s)", name, out)
		}
	}
	if n := len(r.starts(t)); n != 1 {
		t.Errorf("%d containers started, want the first only", n)
	}
}

func TestHoopScriptChecksTheContainerClock(t *testing.T) {
	r := newHoopRig(t)
	if out, ok := r.run(t, nil, "pull"); !ok {
		t.Fatal(out)
	}
	out, ok := r.run(t, nil, "clock")
	if !ok || !strings.Contains(out, "container") || !strings.Contains(out, "host") {
		t.Errorf("clock with the clocks together: ok %v, output %q", ok, out)
	}
	date := r.calls(t)[len(r.calls(t))-1]
	if !slices.Contains(date, "date") || date[len(date)-3] != fakeRef {
		t.Errorf("clock ran %q, want date in a container of the pinned image", date)
	}
	// bench collect joins hoop's lines within 2 s of the sender's clock.
	for _, skew := range []string{"1.6", "-1.6", "120"} {
		out, ok := r.run(t, []string{"STUB_SKEW=" + skew}, "clock")
		if ok {
			t.Errorf("clock with the container %s s off: ok (%s)", skew, out)
		}
		out, ok = r.run(t, []string{"STUB_SKEW=" + skew}, "start", "cfg.yaml", "skew")
		if ok || !strings.Contains(out, "clock") {
			t.Errorf("start with the container %s s off: ok %v, output %q", skew, ok, out)
		}
	}
	if n := len(r.starts(t)); n != 0 {
		t.Errorf("%d containers started with the clock off", n)
	}
}

func TestHoopScriptPullKeepsThePinnedDigest(t *testing.T) {
	r := newHoopRig(t)
	other := "hoophq/hoopsidecar@sha256:" + strings.Repeat("cd", 32)
	out, ok := r.run(t, []string{"STUB_REF=" + other}, "pull")
	if ok || !strings.Contains(out, other) {
		t.Errorf("pull of another image: ok %v, output %q", ok, out)
	}
	for _, ref := range []string{"hoophq/hoopsidecar:1.310.0", "hoophq/hoopsidecar@sha256:abc", ""} {
		if out, ok := r.run(t, []string{"STUB_REF=" + ref}, "pull"); ok {
			t.Errorf("inspect giving %q: pull ok (%s)", ref, out)
		}
	}
	if _, err := os.Stat(filepath.Join(r.bench, "hoop.digest")); !os.IsNotExist(err) {
		t.Errorf("a refused pull left hoop.digest (%v)", err)
	}

	// The default pin is 1.310.0's digest.
	env := slices.DeleteFunc(slices.Clone(r.env), func(s string) bool { return strings.HasPrefix(s, "HOOP_DIGEST=") })
	pinned := "hoophq/hoopsidecar@" + pinnedDigest
	for ref, want := range map[string]bool{fakeRef: false, pinned: true} {
		r.env = append(slices.Clone(env), "STUB_REF="+ref)
		if out, ok := r.run(t, nil, "pull"); ok != want {
			t.Errorf("default pin, image %s: ok %v (%s)", ref, ok, out)
		}
	}

	// A digest file that is not a digest reference is refused before anything runs.
	if err := os.WriteFile(filepath.Join(r.bench, "hoop.digest"), []byte("hoophq/hoopsidecar:1.310.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := len(r.calls(t))
	if out, ok := r.run(t, nil, "start", "cfg.yaml", "tag"); ok {
		t.Errorf("start with an image tag in hoop.digest: ok (%s)", out)
	}
	if len(r.calls(t)) != before {
		t.Error("start ran docker with an image tag in hoop.digest")
	}
}

func TestHoopScriptValidatesStopsAndShowsLogs(t *testing.T) {
	r := newHoopRig(t)
	if out, ok := r.run(t, nil, "pull"); !ok {
		t.Fatal(out)
	}
	out, ok := r.run(t, nil, "validate", "cfg.yaml")
	if !ok {
		t.Fatalf("validate: %s", out)
	}
	calls := r.calls(t)
	v := calls[len(calls)-1]
	n := len(v)
	if n < 6 || v[0] != "run" || v[1] != "--rm" || !slices.Equal(v[n-5:], []string{fakeRef, "hoop", "start", "sidecar", "--validate"}) {
		t.Fatalf("validate ran %q", v)
	}
	mounts := values(v, "-v")
	if len(mounts) != 2 || !strings.HasSuffix(mounts[0], ":/etc/hoop-inspect/config.yaml:ro") ||
		!strings.HasSuffix(mounts[1], ":/run/hoop/anthropic.key:ro") || !filepath.IsAbs(mounts[0]) {
		t.Errorf("validate mounts %q", mounts)
	}
	if got := values(v, "-e"); !slices.Equal(got, []string{"HOOP_SIDECAR_FREE_STANDALONE=true", "HOOP_SIDECAR_ANALYTICS=off"}) {
		t.Errorf("validate -e %q", got)
	}
	if strings.Contains(out, hoopTestKey) || slices.ContainsFunc(v, func(a string) bool { return strings.Contains(a, hoopTestKey) }) {
		t.Error("validate shows or passes the key")
	}

	for _, c := range []struct {
		cmd  string
		want []string
	}{
		{"stop", []string{"rm", "-f", "bench-hoop"}},
		{"logs", []string{"logs", "bench-hoop"}},
	} {
		if out, ok := r.run(t, nil, c.cmd); !ok {
			t.Errorf("%s: %s", c.cmd, out)
		}
		calls := r.calls(t)
		if got := calls[len(calls)-1]; !slices.Equal(got, c.want) {
			t.Errorf("%s ran %q, want %q", c.cmd, got, c.want)
		}
	}
	if out, ok := r.run(t, nil, "frobnicate"); ok || !strings.Contains(out, "usage") {
		t.Errorf("an unknown command: ok %v, output %q", ok, out)
	}
}

func TestHoopScriptRefusesAConfigTheMountsDoNotFit(t *testing.T) {
	r := newHoopRig(t)
	if out, ok := r.run(t, nil, "pull"); !ok {
		t.Fatal(out)
	}
	for name, edit := range map[string][2]string{
		// The audit log would stay inside the container and go with it.
		"audit outside the mount": {"file: /var/log/hoop/audit.jsonl", "file: /tmp/audit.jsonl"},
		// The runbook's fallback 1 lays the config out for the host.
		"audit on the host":     {"file: /var/log/hoop/audit.jsonl", "file: /Users/x/bench/hoop/audit/audit.jsonl"},
		"key outside the mount": {"credentials_file: /run/hoop/anthropic.key", "credentials_file: /cfg/anthropic.key"},
		"another port":          {"listen: 0.0.0.0:15432", "listen: 0.0.0.0:6432"},
		"loopback only":         {"listen: 0.0.0.0:15432", "listen: 127.0.0.1:15432"},
	} {
		if !strings.Contains(containerConfig, edit[0]) {
			t.Fatalf("containerConfig has no %q", edit[0])
		}
		path := filepath.Join(r.work, "bad.yaml")
		if err := os.WriteFile(path, []byte(strings.Replace(containerConfig, edit[0], edit[1], 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"start", "bad.yaml", "bad"}, {"validate", "bad.yaml"}} {
			if out, ok := r.run(t, nil, args...); ok {
				t.Errorf("%s, %s: ok (%s)", name, args[0], out)
			}
		}
	}
	if out, ok := r.run(t, nil, "start", "missing.yaml", "missing"); ok {
		t.Errorf("a config that does not exist: ok (%s)", out)
	}
	if n := len(r.starts(t)); n != 0 {
		t.Errorf("%d containers started", n)
	}
}

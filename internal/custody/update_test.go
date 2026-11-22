package custody

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stetdb/stet/internal/scram"
)

// updateHelperEnv names the credentials file the test binary, run again by
// TestUpdateLocksAcrossProcesses, updates as another process.
const updateHelperEnv = "STET_CUSTODY_UPDATE_HELPER"

func TestMain(m *testing.M) {
	if path := os.Getenv(updateHelperEnv); path != "" {
		os.Exit(updateHelper(path))
	}
	os.Exit(m.Run())
}

// updateHelper is another process's Update of path: it prints "locked" once
// it holds the lock, holds it a while, and adds the account other_process.
func updateHelper(path string) int {
	_, v, err := scram.NewToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	err = Update(path, func(f *File) error {
		fmt.Println("locked")
		time.Sleep(500 * time.Millisecond)
		f.Agents["other_process"] = Agent{Token: v.Text(), Password: "pw-other"}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func TestCheckAccountName(t *testing.T) {
	if err := CheckAccountName(""); err == nil || err.Error() != "the account name is empty" {
		t.Errorf("CheckAccountName(\"\") = %v", err)
	}
	if err := CheckAccountName(strings.Repeat("a", 63)); err != nil {
		t.Errorf("CheckAccountName of 63 bytes = %v", err)
	}
	long := strings.Repeat("a", 64)
	if err := CheckAccountName(long); err == nil || err.Error() != `account "`+long+`": the name is longer than 63 bytes` {
		t.Errorf("CheckAccountName of 64 bytes = %v", err)
	}
}

// mayChange is the rule of ReadForUpdate and Update: what Write replaces
// (checkReplaceable), of the caller's own, or anyone's for root. Where it
// differs from CheckMode, the proxy's rule: root may change its own
// group-readable file, and another user's.
func TestMayChange(t *testing.T) {
	const me, other = 1000, 2000
	for _, c := range []struct {
		mode        fs.FileMode
		owner, euid uint32
		ok          bool
		want        string // in the error
	}{
		// root
		{0o640, 0, 0, true, ""}, {0o600, 0, 0, true, ""}, {0o440, 0, 0, true, ""}, {0o400, 0, 0, true, ""},
		{0o644, 0, 0, false, "too open"}, {0o660, 0, 0, false, "too open"},
		{0o600, me, 0, true, ""}, {0o400, me, 0, true, ""}, // sudo stet agent on the proxy user's file
		{0o640, me, 0, false, "too open"},
		// its owner
		{0o600, me, me, true, ""}, {0o400, me, me, true, ""},
		{0o640, me, me, false, "too open"}, {0o604, me, me, false, "too open"},
		// neither its owner nor root
		{0o640, 0, me, false, "belongs to uid 0"}, {0o600, 0, me, false, "belongs to uid 0"},
		{0o600, other, me, false, "belongs to uid 2000"},
	} {
		err := mayChange(c.mode, c.owner, c.euid)
		if (err == nil) != c.ok || err != nil && !strings.Contains(err.Error(), c.want) {
			t.Errorf("mayChange(%04o, owner %d, euid %d) = %v, want ok=%v (%s)", c.mode, c.owner, c.euid, err, c.ok, c.want)
		}
	}
	if err := mayChange(0o600, other, me); err == nil || !strings.Contains(err.Error(), "only that user or root") {
		t.Errorf("the refusal does not say who may change it: %v", err)
	}
	// The difference from the proxy's rule.
	if CheckMode(0o640, 0, 0) == nil || mayChange(0o640, 0, 0) != nil {
		t.Error("want root's own 0640 file refused by CheckMode and accepted by mayChange")
	}
}

func TestReadForUpdate(t *testing.T) {
	dir := t.TempDir()
	for _, missing := range []string{filepath.Join(dir, "creds.yaml"), filepath.Join(dir, "no", "creds.yaml")} {
		f, err := ReadForUpdate(missing)
		if err != nil || f == nil || f.Agents == nil || len(f.Agents) != 0 || f.MockSecret != "" {
			t.Errorf("ReadForUpdate of %s, not there = %+v, %v; want an empty File", missing, f, err)
		}
	}

	path := filepath.Join(dir, "creds.yaml")
	f := sample(t)
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadForUpdate(path); err != nil || !reflect.DeepEqual(got, f) {
		t.Errorf("ReadForUpdate = %+v, %v; want %+v", got, err, f)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadForUpdate(path); err == nil || !strings.Contains(err.Error(), "too open") || strings.Contains(err.Error(), "pw-a") {
		t.Errorf("ReadForUpdate of my 0640 file = %v, want it refused", err)
	}

	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := ReadForUpdate(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular") {
			t.Errorf("ReadForUpdate of a FIFO = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadForUpdate of a FIFO blocked")
	}
}

func TestUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.yaml")
	alice, bob := newToken(t), newToken(t)
	if err := Update(path, func(f *File) error {
		f.Agents["alice"] = Agent{Token: alice, Password: "pw-a"}
		return nil
	}); err != nil {
		t.Fatalf("Update of a file not there: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the new file: %v, %v; want mode 0600", info, err)
	}
	first := readOrFail(t, path)
	if first.MockSecret == "" || first.Agents["alice"].Password != "pw-a" {
		t.Fatalf("the new file holds %+v", first)
	}

	var seen *File
	if err := Update(path, func(f *File) error {
		seen = &File{MockSecret: f.MockSecret, Agents: map[string]Agent{}}
		for k, v := range f.Agents {
			seen.Agents[k] = v
		}
		f.Agents["bob"] = Agent{Token: bob, PasswordEnv: "BOB_PW"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seen, first) {
		t.Errorf("change was given %+v, want the file as it was, %+v", seen, first)
	}
	got := readOrFail(t, path)
	if got.MockSecret != first.MockSecret || len(got.Agents) != 2 || !reflect.DeepEqual(got.Agents["alice"], first.Agents["alice"]) {
		t.Errorf("after the second Update: %+v", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want only the file: no lock file", len(entries))
	}
}

func readOrFail(t *testing.T, path string) *File {
	t.Helper()
	f, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestUpdateWritesNothingWhenTheChangeFails(t *testing.T) {
	dir := t.TempDir()
	stop := errors.New("no, not this")
	missing := filepath.Join(dir, "missing.yaml")
	if err := Update(missing, func(*File) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("Update = %v, want the change's error", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file was made: %v", err)
	}

	path := filepath.Join(dir, "creds.yaml")
	if err := Write(path, sample(t)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	err := Update(path, func(f *File) error {
		delete(f.Agents, "alice")
		return stop
	})
	if !errors.Is(err, stop) {
		t.Errorf("Update = %v, want the change's error", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
		t.Error("the file changed")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want only the file", len(entries))
	}
}

// A file the caller may not change is refused before change is called.
func TestUpdateRefusesAFileItMayNotChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.yaml")
	if err := Write(path, sample(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	err := Update(path, func(*File) error {
		t.Error("change was called")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "too open") {
		t.Errorf("Update of my 0640 file = %v, want it refused", err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
		t.Error("the file changed")
	}
}

func TestUpdatePassesOnErrNotSynced(t *testing.T) {
	old := syncDirectory
	syncDirectory = func(string) error { return errors.New("sync failed") }
	t.Cleanup(func() { syncDirectory = old })
	path := filepath.Join(t.TempDir(), "creds.yaml")
	tok := newToken(t)
	err := Update(path, func(f *File) error {
		f.Agents["alice"] = Agent{Token: tok, Password: "pw-a"}
		return nil
	})
	if !errors.Is(err, ErrNotSynced) {
		t.Fatalf("Update = %v, want ErrNotSynced", err)
	}
	if got := readOrFail(t, path); got.Agents["alice"].Token != tok {
		t.Error("the file is not in place")
	}
}

// While one Update is in its change, another waits, then sees the first's
// result: nothing is lost.
func TestUpdatesDoNotInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.yaml")
	first, second := newToken(t), newToken(t)
	inFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- Update(path, func(f *File) error {
			close(inFirst)
			time.Sleep(300 * time.Millisecond)
			f.Agents["first"] = Agent{Token: first, Password: "pw-1"}
			return nil
		})
	}()
	select {
	case <-inFirst:
	case err := <-firstDone:
		t.Fatalf("the first Update returned %v without calling change", err)
	}
	err := Update(path, func(f *File) error {
		if _, ok := f.Agents["first"]; !ok {
			t.Error("the second Update read the file while the first was changing it")
		}
		f.Agents["second"] = Agent{Token: second, Password: "pw-2"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if got := readOrFail(t, path); len(got.Agents) != 2 {
		t.Errorf("the file holds %d accounts, want both", len(got.Agents))
	}
}

// The same across processes: the lock is the operating system's.
func TestUpdateLocksAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.yaml")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), updateHelperEnv+"="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("the other process said %q, %v; stderr:\n%s", line, err, &stderr)
	}
	tok := newToken(t)
	err = Update(path, func(f *File) error {
		if _, ok := f.Agents["other_process"]; !ok {
			t.Error("Update read the file while another process was changing it")
		}
		f.Agents["this_process"] = Agent{Token: tok, Password: "pw-this"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the other process: %v; stderr:\n%s", err, &stderr)
	}
	if got := readOrFail(t, path); len(got.Agents) != 2 {
		t.Errorf("the file holds %d accounts, want both processes'", len(got.Agents))
	}
}

func TestWrongPassword(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&loginError{kind: kindWrongPassword, code: "28P01", message: "m"}, true},
		{fmt.Errorf("wrapped: %w", &loginError{kind: kindWrongPassword}), true},
		{&loginError{kind: kindUnreachable}, false},
		{&loginError{kind: kindCertificate}, false},
		{&loginError{kind: kindOther, code: "28000"}, false},
		{errors.New("upstream login: wrong password"), false},
		{nil, false},
	} {
		if got := WrongPassword(c.err); got != c.want {
			t.Errorf("WrongPassword(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

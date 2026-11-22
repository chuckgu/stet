package custody

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	// The text handler escapes quotes in messages; read them as written.
	return strings.ReplaceAll(l.b.String(), `\"`, `"`)
}

func testLog() (*slog.Logger, *lockedBuf) {
	buf := &lockedBuf{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

func noEnv(string) string { return "" }

// bump moves the file's modified time forward so a rewrite in the same
// moment still looks changed to filesystems with coarse clocks.
func bump(t *testing.T, path string, d time.Duration) {
	t.Helper()
	at := time.Now().Add(d)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func openSample(t *testing.T, f *File, env func(string) string) (*Store, string, *lockedBuf) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "creds.yaml")
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	log, buf := testLog()
	s, err := OpenStore(path, env, log)
	if err != nil {
		t.Fatal(err)
	}
	return s, path, buf
}

func count(buf *lockedBuf, sub string) int { return strings.Count(buf.String(), sub) }

func TestStoreFollowsTheFile(t *testing.T) {
	f := sample(t)
	f.Agents["bob"] = Agent{Token: newToken(t), Password: "pw-b"}
	s, path, buf := openSample(t, f, noEnv)

	if got := strings.Join(s.Accounts(), ","); got != "alice,bob" {
		t.Fatalf("Accounts = %s", got)
	}
	e, ok := s.Lookup("alice")
	if !ok || e.Password != "pw-a" || e.Token.Text() != f.Agents["alice"].Token {
		t.Fatalf("Lookup(alice) = %+v, %v", e, ok)
	}
	if _, ok := s.Lookup("nobody"); ok {
		t.Error("Lookup of an unknown account succeeded")
	}

	// A new token and a new password.
	f.Agents["alice"] = Agent{Token: newToken(t), Password: "pw-a2"}
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	e, ok = s.Lookup("alice")
	if !ok || e.Password != "pw-a2" || e.Token.Text() != f.Agents["alice"].Token {
		t.Fatalf("after change Lookup(alice) = %+v, %v", e, ok)
	}

	// A removed account.
	delete(f.Agents, "bob")
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup("bob"); ok {
		t.Error("a removed account still logs in")
	}
	if _, ok := s.Lookup("alice"); !ok {
		t.Error("alice vanished")
	}

	// An account added after start is not accepted, and is warned about once.
	f.Agents["carol"] = Agent{Token: newToken(t), Password: "pw-c"}
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, ok := s.Lookup("carol"); ok {
			t.Error("an account added after start was accepted")
		}
		// Each change makes the next Lookup read the file again.
		f.Agents["alice"] = Agent{Token: f.Agents["alice"].Token, Password: fmt.Sprintf("pw-again-%d", i)}
		if err := Write(path, f); err != nil {
			t.Fatal(err)
		}
	}
	s.Lookup("carol")
	if n := count(buf, `account "carol" was added after stet started; restart stet proxy to accept it`); n != 1 {
		t.Errorf("warned %d times about carol, want 1:\n%s", n, buf)
	}
	if got := strings.Join(s.Accounts(), ","); got != "alice,bob" {
		t.Errorf("Accounts changed: %s", got)
	}

	// A removed account that comes back works again.
	f.Agents["bob"] = Agent{Token: newToken(t), Password: "pw-b2"}
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	if e, ok := s.Lookup("bob"); !ok || e.Password != "pw-b2" {
		t.Errorf("Lookup(bob) = %+v, %v", e, ok)
	}
	if strings.Contains(buf.String(), "pw-") || strings.Contains(buf.String(), "SCRAM") {
		t.Errorf("the log has a secret:\n%s", buf)
	}
}

func TestStoreKeepsTheLastGoodFile(t *testing.T) {
	f := &File{Agents: map[string]Agent{"alice": {Token: newToken(t), Password: "pw-a"}}}
	s, path, buf := openSample(t, f, noEnv)
	good, _ := os.ReadFile(path)
	want := f.Agents["alice"].Token

	check := func(label string) {
		t.Helper()
		for range 3 {
			e, ok := s.Lookup("alice")
			if !ok || e.Password != "pw-a" || e.Token.Text() != want {
				t.Fatalf("%s: Lookup = %+v, %v; want the last good entry", label, e, ok)
			}
		}
	}
	fail := func(label string, put func()) {
		t.Helper()
		before := count(buf, "level=ERROR")
		put()
		check(label)
		if n := count(buf, "level=ERROR") - before; n != 1 {
			t.Errorf("%s: %d error logs while the failure lasted, want 1:\n%s", label, n, buf)
		}
	}
	restore := func() {
		t.Helper()
		if err := os.WriteFile(path, good, 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chmod(path, 0o600)
		bump(t, path, time.Hour)
		check("restored")
	}

	fail("half written", func() {
		writeRaw(t, path, "agents:\n  alice:\n    token: [", 0o600)
		bump(t, path, 2*time.Hour)
	})
	restore()
	fail("empty", func() {
		writeRaw(t, path, "", 0o600)
		bump(t, path, 3*time.Hour)
	})
	restore()
	fail("opened up", func() {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	})
	restore()
	fail("removed", func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	})

	// The file comes back whole with new values.
	f.Agents["alice"] = Agent{Token: newToken(t), Password: "pw-new"}
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Lookup("alice")
	if !ok || e.Password != "pw-new" || e.Token.Text() != f.Agents["alice"].Token {
		t.Fatalf("after recovery Lookup = %+v, %v", e, ok)
	}
	if strings.Contains(buf.String(), "pw-") || strings.Contains(buf.String(), "SCRAM") {
		t.Errorf("the log has a secret:\n%s", buf)
	}
}

func TestStoreSeesTwoChangesInOneSecond(t *testing.T) {
	f := &File{Agents: map[string]Agent{"alice": {Token: newToken(t), Password: "pw-aaaa"}}}
	s, path, _ := openSample(t, f, noEnv)
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)

	replace := func(password string) {
		t.Helper()
		f.Agents["alice"] = Agent{Token: f.Agents["alice"].Token, Password: password}
		tmp := filepath.Join(filepath.Dir(path), "next")
		if err := Write(tmp, f); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(tmp, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	for _, pw := range []string{"pw-bbbb", "pw-cccc"} {
		replace(pw)
		if e, ok := s.Lookup("alice"); !ok || e.Password != pw {
			t.Fatalf("after replacing with %s: Lookup = %+v, %v", pw, e, ok)
		}
	}
}

func TestStorePasswordEnv(t *testing.T) {
	f := &File{Agents: map[string]Agent{
		"alice": {Token: newToken(t), PasswordEnv: "ALICE_PW"},
		"bob":   {Token: newToken(t), PasswordEnv: "BOB_PW"},
		"carol": {Token: newToken(t), PasswordEnv: "CAROL_PW"},
	}}
	envs := map[string]string{"ALICE_PW": "from-env", "CAROL_PW": "carol-env"}
	s, path, buf := openSample(t, f, func(k string) string { return envs[k] })

	if e, ok := s.Lookup("alice"); !ok || e.Password != "from-env" {
		t.Fatalf("Lookup(alice) = %+v, %v", e, ok)
	}
	if e, ok := s.Lookup("bob"); !ok || e.Password != "" {
		t.Fatalf("Lookup(bob) = %+v, %v; want an empty password", e, ok)
	}
	if n := count(buf, "account=bob"); n != 1 || !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("want one warning about bob:\n%s", buf)
	}
	if strings.Contains(buf.String(), "from-env") {
		t.Errorf("the log has a password:\n%s", buf)
	}

	// The environment changing later changes nothing.
	envs["ALICE_PW"] = "changed"
	f.Agents["alice"] = Agent{Token: newToken(t), PasswordEnv: "ALICE_PW"}
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Lookup("alice"); e.Password != "from-env" {
		t.Errorf("password followed the environment: %q", e.Password)
	}

	// A file that points at another variable keeps the old value, with a
	// warning once, however often the file is read again.
	f.Agents["carol"] = Agent{Token: newToken(t), PasswordEnv: "OTHER_PW"}
	envs["OTHER_PW"] = "other"
	var e Entry
	for i := range 3 {
		if err := Write(path, f); err != nil {
			t.Fatal(err)
		}
		e, _ = s.Lookup("carol")
		f.Agents["bob"] = Agent{Token: newToken(t), PasswordEnv: "BOB_PW", Added: time.Unix(int64(i+1), 0)}
	}
	if e.Password != "carol-env" || e.Token.Text() != f.Agents["carol"].Token {
		t.Errorf("Lookup(carol) = %+v; want the old password and the new token", e)
	}
	if n := count(buf, "account=carol"); n != 1 {
		t.Errorf("%d warnings about carol, want 1:\n%s", n, buf)
	}

	// A literal password in the file takes over from an environment one.
	f.Agents["alice"] = Agent{Token: f.Agents["alice"].Token, Password: "literal"}
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Lookup("alice"); e.Password != "literal" {
		t.Errorf("password = %q, want the file's", e.Password)
	}

	// Back to the variable it was opened with: the value read at open.
	f.Agents["alice"] = Agent{Token: f.Agents["alice"].Token, PasswordEnv: "ALICE_PW"}
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Lookup("alice"); e.Password != "from-env" {
		t.Errorf("password = %q, want the value read at open", e.Password)
	}
}

func TestStoreMockSecret(t *testing.T) {
	f := sample(t)
	s, path, buf := openSample(t, f, noEnv)
	if len(s.MockSecret()) != 32 {
		t.Fatalf("MockSecret has %d bytes", len(s.MockSecret()))
	}
	if strings.Contains(buf.String(), "mock_secret") {
		t.Errorf("a file with a mock_secret was warned about:\n%s", buf)
	}
	first := append([]byte(nil), s.MockSecret()...)
	s.MockSecret()[0] ^= 0xff // a caller changing what it got changes nothing
	if !bytes.Equal(s.MockSecret(), first) {
		t.Fatal("MockSecret returns the internal slice")
	}
	f.MockSecret = "" // a rewrite with a new secret does not move the store's
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	s.Lookup("alice")
	if !bytes.Equal(s.MockSecret(), first) {
		t.Error("the mock secret followed the file")
	}

	// A hand-written file without one: a random value and a warning.
	hand := filepath.Join(t.TempDir(), "hand.yaml")
	writeRaw(t, hand, "agents:\n  alice:\n    token: "+f.Agents["alice"].Token+"\n    password: pw\n", 0o600)
	log, hb := testLog()
	h1, err := OpenStore(hand, noEnv, log)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := OpenStore(hand, noEnv, log)
	if len(h1.MockSecret()) != 32 || bytes.Equal(h1.MockSecret(), h2.MockSecret()) {
		t.Error("want 32 fresh random bytes per open")
	}
	want := "the credentials file has no mock_secret: after a restart a login attempt can tell an unknown account from a known one; run a stet agent command that changes the file (add, token, password or remove) to add one"
	if n := count(hb, want); n != 2 {
		t.Errorf("warned %d times for two opens, want 2:\n%s", n, hb)
	}
}

func TestStoreOpensWithNoAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.yaml")
	if err := Write(path, &File{}); err != nil {
		t.Fatal(err)
	}
	log, _ := testLog()
	s, err := OpenStore(path, noEnv, log)
	if err != nil || len(s.Accounts()) != 0 {
		t.Fatalf("OpenStore = %v, %v", s, err)
	}
}

func TestStoreOpenRefusesABadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds.yaml")
	writeRaw(t, path, "agents: {}\n", 0o644)
	log, _ := testLog()
	if _, err := OpenStore(path, noEnv, log); err == nil {
		t.Fatal("OpenStore accepted a 0644 file")
	}
}

func TestStoreIsSafeForConcurrentUse(t *testing.T) {
	f := &File{Agents: map[string]Agent{
		"alice": {Token: newToken(t), Password: "pw-0"},
		"bob":   {Token: newToken(t), Password: "pw-0"},
	}}
	s, path, _ := openSample(t, f, noEnv)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.Lookup([]string{"alice", "bob", "nobody"}[g%3])
				s.Accounts()
				s.MockSecret()
			}
		}()
	}
	for i := 1; i <= 20; i++ {
		f.Agents["alice"] = Agent{Token: f.Agents["alice"].Token, Password: fmt.Sprintf("pw-%d", i)}
		if err := Write(path, f); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
	if e, ok := s.Lookup("alice"); !ok || e.Password != "pw-20" {
		t.Errorf("final Lookup = %+v, %v", e, ok)
	}
}

func TestStoreRetriesAFailedRead(t *testing.T) {
	old := retryEvery
	retryEvery = 30 * time.Millisecond
	t.Cleanup(func() { retryEvery = old })

	f := &File{Agents: map[string]Agent{"alice": {Token: newToken(t), Password: "pw-a"}}}
	s, path, buf := openSample(t, f, noEnv)

	// A read that fails for a reason the file's stat does not show.
	var mu sync.Mutex
	broken := true
	calls := 0
	s.mu.Lock()
	s.read = func(p string) (*File, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if broken {
			return nil, errors.New("too many open files")
		}
		return Read(p)
	}
	s.mu.Unlock()

	f.Agents["alice"] = Agent{Token: newToken(t), Password: "pw-new"}
	if err := Write(path, f); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if e, _ := s.Lookup("alice"); e.Password != "pw-a" {
			t.Fatalf("Lookup = %q while the read fails, want the old password", e.Password)
		}
	}
	mu.Lock()
	broken = false
	failedCalls := calls
	mu.Unlock()
	s.mu.Lock()
	s.lastTry = time.Now().Add(time.Hour) // as if the failed try was just now
	s.mu.Unlock()
	if e, _ := s.Lookup("alice"); e.Password != "pw-a" {
		t.Errorf("Lookup = %q, want the old password: no retry before the interval", e.Password)
	}
	mu.Lock()
	if calls != failedCalls {
		t.Errorf("read the file %d more times before the interval was over", calls-failedCalls)
	}
	mu.Unlock()
	s.mu.Lock()
	s.lastTry = time.Now().Add(-time.Hour) // the interval is over
	s.mu.Unlock()
	e, ok := s.Lookup("alice")
	if !ok || e.Password != "pw-new" || e.Token.Text() != f.Agents["alice"].Token {
		t.Fatalf("after the retry interval Lookup = %+v, %v", e, ok)
	}
	if n := count(buf, "level=ERROR"); n != 1 {
		t.Errorf("%d error logs, want 1:\n%s", n, buf)
	}
	if n := count(buf, "following it once more"); n != 1 {
		t.Errorf("%d recovery lines, want 1:\n%s", n, buf)
	}

	// A file read well is not read again until it changes.
	reads := 0
	s.mu.Lock()
	s.read = func(p string) (*File, error) { reads++; return Read(p) }
	s.mu.Unlock()
	time.Sleep(2 * retryEvery)
	for range 5 {
		s.Lookup("alice")
	}
	if reads != 0 {
		t.Errorf("read an unchanged file %d times", reads)
	}
}

func TestStoreKeepsTheLastGoodFileWhenItBecomesAFIFO(t *testing.T) {
	f := &File{Agents: map[string]Agent{"alice": {Token: newToken(t), Password: "pw-a"}}}
	s, path, _ := openSample(t, f, noEnv)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan Entry, 1)
	go func() { e, _ := s.Lookup("alice"); done <- e }()
	select {
	case e := <-done:
		if e.Password != "pw-a" {
			t.Errorf("Lookup = %q", e.Password)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Lookup blocked on a FIFO")
	}
}

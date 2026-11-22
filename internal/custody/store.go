package custody

import (
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/stetdb/stet/internal/scram"
)

// Entry is an account as the running proxy uses it.
type Entry struct {
	Token    scram.Verifier
	Password string // resolved: the file's, or the environment's at start
}

// Store is the accounts of a running proxy. The set is fixed when it is
// opened; tokens and passwords follow the file.
type Store struct {
	path     string
	read     func(string) (*File, error) // Read; a test replaces it
	log      *slog.Logger
	mock     []byte
	accounts []string // sorted, fixed

	mu       sync.Mutex
	seen     os.FileInfo // the file as last looked at, read or not; nil when it could not be statted
	failing  bool        // the last attempt to read the file failed, and was logged
	entries  map[string]Entry
	password map[string]string // the password in use per account, kept while the account is removed
	envValue map[string]string // the environment's values by variable name, as read when opened
	warned   map[string]bool   // accounts seen only after start, and account/variable pairs, already warned about
	lastTry  time.Time         // when the file was last read, while that is failing
}

// retryEvery is how soon a failed read of the file is tried again; a test
// shortens it.
var retryEvery = 200 * time.Millisecond

// OpenStore reads path. env reads the environment, once, for password_env.
func OpenStore(path string, env func(string) string, log *slog.Logger) (*Store, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	f, err := Read(path)
	if err != nil {
		return nil, err
	}
	s := &Store{
		path:     path,
		read:     Read,
		log:      log,
		seen:     fi,
		entries:  map[string]Entry{},
		password: map[string]string{},
		envValue: map[string]string{},
		warned:   map[string]bool{},
	}

	if f.MockSecret != "" {
		s.mock, err = base64.StdEncoding.DecodeString(f.MockSecret)
		if err != nil {
			return nil, err // Read checked it; this does not happen
		}
	} else {
		s.mock = make([]byte, mockSecretLen)
		if _, err := rand.Read(s.mock); err != nil {
			return nil, err
		}
		log.Warn("the credentials file has no mock_secret: after a restart a login attempt can tell an unknown account from a known one; run a stet agent command that changes the file (add, token, password or remove) to add one")
	}

	for _, name := range sortedNames(f.Agents) {
		a := f.Agents[name]
		v, err := scram.ParseVerifier(a.Token)
		if err != nil {
			return nil, err // Read checked it; this does not happen
		}
		pw := a.Password
		if a.PasswordEnv != "" {
			pw = env(a.PasswordEnv)
			s.envValue[a.PasswordEnv] = pw
			if pw == "" {
				log.Warn("the environment variable for an account's password is empty or not set",
					"account", name, "variable", a.PasswordEnv)
			}
		}
		s.accounts = append(s.accounts, name)
		s.password[name] = pw
		s.entries[name] = Entry{Token: v, Password: pw}
	}
	return s, nil
}

// MockSecret returns the secret that unknown accounts' verifiers derive from:
// the file's as read when the store was opened, or random bytes made then. It
// does not follow the file. Do not print it.
func (s *Store) MockSecret() []byte { return append([]byte(nil), s.mock...) }

// Accounts returns the accounts the store was opened with, sorted.
func (s *Store) Accounts() []string {
	out := make([]string, len(s.accounts))
	copy(out, s.accounts)
	sort.Strings(out)
	return out
}

// Lookup returns account's entry. It reads the file again first when the
// file changed. ok false: not an account, or removed since.
func (s *Store) Lookup(account string) (e Entry, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh()
	e, ok = s.entries[account]
	return e, ok
}

// refresh reads the file again when it looks different from the last time.
// The caller holds s.mu.
func (s *Store) refresh() {
	fi, err := os.Stat(s.path)
	if err != nil {
		s.seen = nil
		s.fail(err)
		return
	}
	if s.seen != nil && s.same(s.seen, fi) {
		// Unchanged. Only a read that failed is tried again, now and then:
		// what made it fail may not show in the file's stat.
		if !s.failing || time.Since(s.lastTry) < retryEvery {
			return
		}
	}
	s.seen = fi
	s.lastTry = time.Now()
	f, err := s.read(s.path)
	if err != nil {
		s.fail(err)
		return
	}
	if s.failing {
		s.failing = false
		s.log.Info("read the credentials file again; following it once more")
	}
	s.apply(f)
}

func (s *Store) same(a, b os.FileInfo) bool {
	return a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size() && a.Mode() == b.Mode() && os.SameFile(a, b)
}

// fail logs an error once for as long as the failure lasts.
func (s *Store) fail(err error) {
	if s.failing {
		return
	}
	s.failing = true
	s.log.Error("could not read the credentials file again; keeping the accounts as they were", "err", err)
}

func (s *Store) apply(f *File) {
	next := map[string]Entry{}
	for _, name := range sortedNames(f.Agents) {
		a := f.Agents[name]
		old, known := s.password[name]
		if !known {
			if !s.warned[name] {
				s.warned[name] = true
				s.log.Warn("account \"" + name + "\" was added after stet started; restart stet proxy to accept it")
			}
			continue
		}
		v, err := scram.ParseVerifier(a.Token)
		if err != nil {
			continue // Read checked it; this does not happen
		}
		pw := old
		switch {
		case a.Password != "":
			pw = a.Password
		default:
			if v, read := s.envValue[a.PasswordEnv]; read {
				pw = v // the value read at open
			} else if key := name + "\x00" + a.PasswordEnv; !s.warned[key] {
				s.warned[key] = true
				s.log.Warn("the credentials file now names an environment variable that was not read when stet started; keeping the password in use until stet restarts",
					"account", name, "variable", a.PasswordEnv)
			}
		}
		s.password[name] = pw
		next[name] = Entry{Token: v, Password: pw}
	}
	s.entries = next
}

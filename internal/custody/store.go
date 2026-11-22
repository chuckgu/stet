package custody

import (
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"os"
	"sort"
	"sync"

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
	log      *slog.Logger
	mock     []byte
	accounts []string // sorted, fixed

	mu       sync.Mutex
	seen     os.FileInfo // the file as last looked at, read or not; nil when it could not be statted
	failing  bool        // the last attempt to read the file failed, and was logged
	entries  map[string]Entry
	password map[string]string // the password in use per account, kept while the account is removed
	envName  map[string]string // the password_env each account was opened with, "" for a literal password
	warned   map[string]bool   // accounts seen only after start, already warned about
}

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
		log:      log,
		seen:     fi,
		entries:  map[string]Entry{},
		password: map[string]string{},
		envName:  map[string]string{},
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
		log.Warn("the credentials file has no mock_secret: after a restart a login attempt can tell an unknown account from a known one; run any stet agent command to add one")
	}

	for _, name := range sortedNames(f.Agents) {
		a := f.Agents[name]
		v, err := scram.ParseVerifier(a.Token)
		if err != nil {
			return nil, err // Read checked it; this does not happen
		}
		pw := a.Password
		if a.PasswordEnv != "" {
			s.envName[name] = a.PasswordEnv
			pw = env(a.PasswordEnv)
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
func (s *Store) MockSecret() []byte { return s.mock }

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
		return
	}
	s.seen = fi
	f, err := Read(s.path)
	if err != nil {
		s.fail(err)
		return
	}
	s.failing = false
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
		case a.PasswordEnv == s.envName[name]:
			// the variable it was opened with: the value read then
		default:
			s.log.Warn("the credentials file now names another environment variable for an account's password; keeping the password in use until stet restarts",
				"account", name, "variable", a.PasswordEnv)
		}
		s.password[name] = pw
		next[name] = Entry{Token: v, Password: pw}
	}
	s.entries = next
}

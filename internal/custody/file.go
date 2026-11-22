// Package custody keeps the agents' tokens and real database passwords, and
// logs in to the database for them.
package custody

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/stetdb/stet/internal/scram"
)

const (
	maxAccountName = 63
	mockSecretLen  = 32
	maxFileSize    = 1 << 20
	fileComment    = "# Written by stet agent. It holds database passwords: keep it private.\n"
)

// Agent is one account in the credentials file.
type Agent struct {
	Token       string    `yaml:"token"`                  // a scram verifier
	Password    string    `yaml:"password,omitempty"`     // the real password, or
	PasswordEnv string    `yaml:"password_env,omitempty"` // the environment variable that holds it
	Added       time.Time `yaml:"added"`
	TokenIssued time.Time `yaml:"token_issued"`
}

// File is the credentials file.
type File struct {
	// MockSecret is 32 random bytes in standard base64. It seeds the verifiers
	// shown for accounts that do not exist. It is not a credential, but it is
	// never printed.
	MockSecret string           `yaml:"mock_secret"`
	Agents     map[string]Agent `yaml:"agents"`
}

// CheckMode applies the rule for who may read the file, as PostgreSQL does
// for its key files: when owner is uid (the effective uid, root included) the
// file must give no access to group or others; otherwise, when it is owned by
// root, at most group read; a file of any other owner is refused.
func CheckMode(mode fs.FileMode, owner, uid uint32) error {
	perm := mode.Perm()
	switch {
	case owner == uid:
		if perm&0o077 != 0 {
			return fmt.Errorf("mode %04o is too open: allow no access for group or others (0600)", perm)
		}
	case owner == 0:
		if perm&0o037 != 0 {
			return fmt.Errorf("mode %04o is too open for a file owned by root: allow at most group read (0640)", perm)
		}
	default:
		return fmt.Errorf("the file belongs to uid %d, who is neither you (uid %d) nor root", owner, uid)
	}
	return nil
}

func ownerOf(info fs.FileInfo) (uid, gid uint32, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Uid, st.Gid, true
}

// Read reads and validates the file at path, CheckMode included. Its errors
// name the account and what is wrong, never a value.
func Read(path string) (*File, error) {
	// Decide from a Stat before opening: opening a FIFO would block.
	pre, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !pre.Mode().IsRegular() {
		return nil, fmt.Errorf("credentials file %s: not a regular file", path)
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("credentials file %s: not a regular file", path)
	}
	if uid, _, ok := ownerOf(info); ok {
		if err := CheckMode(info.Mode(), uid, uint32(os.Geteuid())); err != nil {
			return nil, fmt.Errorf("credentials file %s: %w", path, err)
		}
	}
	var b bytes.Buffer
	if _, err := b.ReadFrom(io.LimitReader(fh, maxFileSize+1)); err != nil {
		return nil, err
	}
	if b.Len() > maxFileSize {
		return nil, fmt.Errorf("credentials file %s: larger than %d bytes", path, maxFileSize)
	}
	f, err := parse(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("credentials file %s: %w", path, err)
	}
	return f, nil
}

type rawFile struct {
	MockSecret string               `yaml:"mock_secret"`
	Agents     map[string]yaml.Node `yaml:"agents"`
}

var (
	reLine  = regexp.MustCompile(`line (\d+)`)
	reField = regexp.MustCompile(`field (\S+) not found`)
)

// plain describes a YAML error without any value the error quotes.
func plain(err error, withLine bool) string {
	var te *yaml.TypeError
	msg := err.Error()
	if errors.As(err, &te) && len(te.Errors) > 0 {
		msg = te.Errors[0]
	}
	if m := reField.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("unknown key %q", m[1])
	}
	if m := reLine.FindStringSubmatch(msg); m != nil && withLine {
		return "not valid here, at line " + m[1]
	}
	if reLine.MatchString(msg) {
		return "not valid here" // a line within a fragment is not a line of the file
	}
	return "not valid YAML"
}

func strict(b []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	return dec.Decode(out)
}

func parse(b []byte) (*File, error) {
	var raw rawFile
	if err := strict(b, &raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty")
		}
		return nil, errors.New(plain(err, true))
	}
	f := &File{MockSecret: raw.MockSecret, Agents: map[string]Agent{}}
	names := make([]string, 0, len(raw.Agents))
	for name := range raw.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		node := raw.Agents[name]
		// Decode each account on its own, strictly, so an error can name it.
		nb, err := yaml.Marshal(&node)
		if err != nil {
			return nil, fmt.Errorf("account %q: not valid YAML", name)
		}
		var a Agent
		if err := strict(nb, &a); err != nil {
			return nil, fmt.Errorf("account %q: %s", name, plain(err, false))
		}
		f.Agents[name] = a
	}
	if err := validate(f); err != nil {
		return nil, err
	}
	return f, nil
}

// validate checks f. Its errors name the account and never quote a value.
func validate(f *File) error {
	if f.MockSecret != "" {
		b, err := base64.StdEncoding.DecodeString(f.MockSecret)
		if err != nil {
			return errors.New("mock_secret is not base64")
		}
		if len(b) < mockSecretLen {
			return fmt.Errorf("mock_secret is shorter than %d bytes", mockSecretLen)
		}
	}
	for _, name := range sortedNames(f.Agents) {
		a := f.Agents[name]
		switch {
		case name == "":
			return errors.New("an account has an empty name")
		case len(name) > maxAccountName:
			return fmt.Errorf("account %q: the name is longer than %d bytes", name, maxAccountName)
		case a.Token == "":
			return fmt.Errorf("account %q: token is missing", name)
		case a.Password != "" && a.PasswordEnv != "":
			return fmt.Errorf("account %q: password and password_env are both set; keep one", name)
		case a.Password == "" && a.PasswordEnv == "":
			return fmt.Errorf("account %q: neither password nor password_env is set", name)
		}
		if _, err := scram.ParseVerifier(a.Token); err != nil {
			return fmt.Errorf("account %q: token is not a valid verifier: %w", name, err)
		}
	}
	return nil
}

func sortedNames(m map[string]Agent) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// renameFile is os.Rename; a test replaces it to make the last step fail.
var renameFile = os.Rename

// Write puts f at path whole: a temporary file in the same directory, then a
// rename. A file already there keeps its owner, group and mode; when that
// cannot be kept, nothing is written. A new file is the caller's, 0600. An
// empty f.MockSecret is filled with a fresh random value first.
func Write(path string, f *File) (err error) {
	if f.MockSecret == "" {
		raw := make([]byte, mockSecretLen)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		f.MockSecret = base64.StdEncoding.EncodeToString(raw)
	}
	if err := validate(f); err != nil {
		return fmt.Errorf("credentials file %s: refusing to write: %w", path, err)
	}
	b, err := encode(f)
	if err != nil {
		return err
	}

	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real // replace the file the link points to, not the link
	}
	var keep fs.FileInfo
	if info, err := os.Stat(path); err == nil {
		keep = info
		if !info.Mode().IsRegular() {
			return fmt.Errorf("credentials file %s is not a regular file: nothing written", path)
		}
		if uid, _, ok := ownerOf(info); ok {
			if err := CheckMode(info.Mode(), uid, uint32(os.Geteuid())); err != nil {
				return fmt.Errorf("credentials file %s: %w: fix that first; nothing written", path, err)
			}
			if uid != uint32(os.Geteuid()) && os.Geteuid() != 0 {
				return fmt.Errorf("credentials file %s belongs to uid %d and only they or root can keep it so: nothing written", path, uid)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(b); err != nil {
		return err
	}
	if keep != nil {
		if uid, gid, ok := ownerOf(keep); ok {
			if cur, serr := tmp.Stat(); serr == nil {
				if cu, cg, _ := ownerOf(cur); cu != uid || cg != gid {
					if err = tmp.Chown(int(uid), int(gid)); err != nil {
						return fmt.Errorf("credentials file %s: cannot keep its owner and group: %w; nothing written", path, err)
					}
				}
			}
		}
		// Chmod after Chown: a change of owner can clear mode bits.
		if err = tmp.Chmod(keep.Mode().Perm()); err != nil {
			return err
		}
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = renameFile(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes the rename survive a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// encode writes f with its accounts in name order.
func encode(f *File) ([]byte, error) {
	agents := &yaml.Node{Kind: yaml.MappingNode}
	for _, name := range sortedNames(f.Agents) {
		key := &yaml.Node{}
		if err := key.Encode(name); err != nil {
			return nil, err
		}
		val := &yaml.Node{}
		if err := val.Encode(f.Agents[name]); err != nil {
			return nil, err
		}
		agents.Content = append(agents.Content, key, val)
	}
	top := &yaml.Node{Kind: yaml.MappingNode}
	secretKey, secretVal := &yaml.Node{}, &yaml.Node{}
	if err := secretKey.Encode("mock_secret"); err != nil {
		return nil, err
	}
	if err := secretVal.Encode(f.MockSecret); err != nil {
		return nil, err
	}
	agentsKey := &yaml.Node{}
	if err := agentsKey.Encode("agents"); err != nil {
		return nil, err
	}
	top.Content = []*yaml.Node{secretKey, secretVal, agentsKey, agents}
	out, err := yaml.Marshal(top)
	if err != nil {
		return nil, err
	}
	return append([]byte(fileComment), out...), nil
}

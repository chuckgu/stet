package custody

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Update changes the credentials file at path under a lock: it takes an
// exclusive advisory lock, reads the file as its owner or root may (what
// Write accepts of an existing file; a missing file is an empty File),
// calls change, and writes the result as Write does. Two Updates never
// interleave. When change returns an error nothing is written and that error
// is returned. An ErrNotSynced from the write is returned as Write returns it.
//
// The lock is flock(LOCK_EX) on the file's directory, held from before the
// read until after the write; it leaves no lock file behind. The proxy's own
// reads (Read, the Store) take no lock: the rename is atomic for them.
func Update(path string, change func(f *File) error) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real // lock and replace where the file is, as Write does
	}
	unlock, err := lockDir(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("credentials file %s: cannot lock its directory: %w", path, err)
	}
	defer unlock()
	f, err := ReadForUpdate(path)
	if err != nil {
		return err
	}
	if err := change(f); err != nil {
		return err
	}
	return Write(path, f)
}

// ReadForUpdate reads the file as Update does, without changing it: for
// commands that only show what is there.
func ReadForUpdate(path string) (*File, error) {
	f, err := read(path, func(mode fs.FileMode, owner uint32) error {
		return mayChange(mode, owner, uint32(os.Geteuid()))
	})
	if errors.Is(err, fs.ErrNotExist) {
		return &File{Agents: map[string]Agent{}}, nil
	}
	return f, err
}

// mayChange is who may change the file through Update, and so what
// ReadForUpdate reads: a file Write would replace (checkReplaceable) that is
// the caller's own, or anyone's when the caller is root. Unlike CheckMode,
// the proxy's rule, it lets root read its own group-readable file, and
// another user's file, which only root can replace and keep theirs.
func mayChange(mode fs.FileMode, owner, euid uint32) error {
	if err := checkReplaceable(mode, owner); err != nil {
		return err
	}
	if owner != euid && euid != 0 {
		return fmt.Errorf("the file belongs to uid %d: only that user or root may change it (you are uid %d)", owner, euid)
	}
	return nil
}

// lockDir takes an exclusive advisory lock on the directory dir, waiting
// for it, and returns its release. It opens dir as a directory only: a FIFO
// in its place is an error at once, where a plain open would wait for a
// writer.
func lockDir(dir string) (unlock func(), err error) {
	d, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	fd := int(d.Fd())
	for {
		if err = syscall.Flock(fd, syscall.LOCK_EX); !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		d.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(fd, syscall.LOCK_UN)
		d.Close()
	}, nil
}

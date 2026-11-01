package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Lock is an exclusive lock held by creating a file (O_CREATE|O_EXCL), which works the
// same on every platform and file system git runs on. A lock file older than the stale
// timeout is taken to be left behind by a crashed process and removed.
type Lock struct{ path string }

// Defaults for AcquireLock: how long to wait for the lock, and the age after which a
// lock file counts as stale. A state is saved within seconds, so a lock older than
// LockStale was not released by its owner.
var (
	LockWait  = 60 * time.Second
	LockStale = 2 * time.Minute
)

// ErrLocked is returned when the lock could not be taken in time.
var ErrLocked = errors.New("state is locked")

// openLock creates the lock file (a variable for tests).
var openLock = func(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
}

// AcquireLock takes the lock file at path, waiting up to wait for a live holder to
// release it. A lock file whose modification time is older than stale is removed.
func AcquireLock(path string, wait, stale time.Duration) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	delay := 5 * time.Millisecond
	gone := false // the last attempt failed with no lock file left to wait for
	for {
		f, err := openLock(path)
		if err == nil {
			host, _ := os.Hostname()
			fmt.Fprintf(f, "pid %d on %s at %s\n", os.Getpid(), host, time.Now().UTC().Format(time.RFC3339))
			f.Close()
			return &Lock{path: path}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			// Windows reports a lock file being deleted by its owner as access denied:
			// retried while the file is there, and once more at once when it has just
			// gone. Failing again without one, the error is the file system's (a
			// read-only file system, a directory without write permission).
			if _, serr := os.Stat(path); errors.Is(serr, os.ErrNotExist) {
				if gone {
					return nil, err
				}
				gone = true
				continue
			}
			if time.Now().After(deadline) {
				return nil, err
			}
		} else if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) > stale {
			// Left behind by a process that died while holding it. (Two processes may
			// both find it stale; the window is tiny and needs a crash to open.) One that
			// cannot be removed (a read-only or sticky directory, a file still open on
			// Windows) is waited for like a live holder's, up to the deadline.
			if err := os.Remove(path); err == nil || errors.Is(err, os.ErrNotExist) {
				continue
			}
		}
		gone = false
		if time.Now().After(deadline) {
			holder, _ := os.ReadFile(path)
			return nil, fmt.Errorf("%w: %s held by %s (remove it if no smates process is running)", ErrLocked, path, strings.TrimSpace(string(holder)))
		}
		time.Sleep(delay)
		if delay < 100*time.Millisecond {
			delay *= 2
		}
	}
}

// Release removes the lock file.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	err := os.Remove(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

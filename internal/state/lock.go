package state

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Lock is an exclusive lock held by creating a file (O_CREATE|O_EXCL), which works the
// same on every platform and file system git runs on. A lock file older than the stale
// timeout is taken to be left behind by a crashed process and removed (see breakStale).
// The file holds a token unique to its holder, so that a lock is only ever removed by
// the process that judged that very file stale, or by its holder.
type Lock struct {
	path  string
	token string
}

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
			ns := ""
			if id, ok := pidNamespace(); ok && id != "" {
				ns = " pidns " + id
			}
			token := fmt.Sprintf("pid %d on %s at %s (%s)%s\n", os.Getpid(), host, time.Now().UTC().Format(time.RFC3339), newToken(), ns)
			_, werr := f.WriteString(token)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				os.Remove(path)
				return nil, werr
			}
			return &Lock{path: path, token: token}, nil
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
		} else if breakStale(path, stale) {
			// Left behind by a process that died while holding it, and removed. One that
			// cannot be removed (a read-only or sticky directory, a file still open on
			// Windows) is waited for like a live holder's, up to the deadline.
			continue
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

// breakStale removes the lock file at path when it is older than stale or its holder
// is a process of this host that no longer runs (holderDead), and reports whether it
// is gone. Two processes may both find the same file stale: if each removed
// what it found at path, the slower one could remove the fresh lock the faster one
// created in the meantime, and both would hold the lock. So the removal is done under
// a second lock file (path + ".break"), and only after reading path again and finding
// the very file that was judged stale (same content, which holds its holder's unique
// token, and same modification time). A break file is held for a moment only: one
// older than stale was left by a crash and is removed.
func breakStale(path string, stale time.Duration) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	seen, err := os.ReadFile(path)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	if time.Since(fi.ModTime()) <= stale && !holderDead(seen) {
		return false
	}
	brk := path + ".break"
	f, err := os.OpenFile(brk, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if bfi, serr := os.Stat(brk); serr == nil && time.Since(bfi.ModTime()) > stale {
			os.Remove(brk)
		}
		return false
	}
	f.Close()
	defer os.Remove(brk)
	again, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil || !again.ModTime().Equal(fi.ModTime()) {
		return false // replaced since: someone else's lock now
	}
	if now, err := os.ReadFile(path); err != nil || !bytes.Equal(now, seen) {
		return false
	}
	err = os.Remove(path)
	return err == nil || errors.Is(err, os.ErrNotExist)
}

// holderRe matches the holder a lock file names: "pid 123 on host at ...", and
// holderNSRe the PID namespace it ran in (Linux: " pidns pid:[4026531836]/<boot id>").
var (
	holderRe   = regexp.MustCompile(`^pid (\d+) on (\S+) at `)
	holderNSRe = regexp.MustCompile(` pidns (\S+)\s*$`)
)

// holderDead reports whether the lock file content data names a holder that is
// certainly gone: a process of this host (by name) and of this PID namespace that no
// longer runs. A host name is shared by containers with their own PID namespace
// (--uts=host, a host network, a fixed --hostname), and two machines may share one
// (a cloned image) and a repository on a network file system: a pid of another
// namespace or kernel may name a live process there, so the namespace and the boot of
// its kernel must match too (a lock file without them, from a platform that has them,
// is another namespace's). A lock left
// by smates killed while holding it (a second Ctrl-C, the OOM killer) is then broken
// at once rather than after the stale timeout, which is longer than LockWait. Any
// doubt (another host, a format it does not know, a platform that cannot tell) is a
// live holder.
func holderDead(data []byte) bool {
	m := holderRe.FindSubmatch(data)
	if m == nil {
		return false
	}
	pid, err := strconv.Atoi(string(m[1]))
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return false
	}
	if host, err := os.Hostname(); err != nil || host != string(m[2]) {
		return false
	}
	ours, ok := pidNamespace()
	theirs := ""
	if n := holderNSRe.FindSubmatch(data); n != nil {
		theirs = string(n[1])
	}
	if !ok || theirs != ours {
		return false
	}
	return !processAlive(pid)
}

// newToken returns a random token that tells apart lock files written at the same
// time by processes with the same pid (containers, other hosts sharing the directory).
func newToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// Release removes the lock file, unless it is no longer this lock's: a holder slower
// than the stale timeout lost the lock to another process (which judged it stale), and
// removing that process's lock file would let a third one in.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	if l.token != "" {
		if data, err := os.ReadFile(l.path); err == nil && string(data) != l.token {
			return nil
		}
	}
	err := os.Remove(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

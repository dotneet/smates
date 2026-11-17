package state

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestWriteDeterministic: the same state encodes to the same bytes every time, however
// its maps iterate (paths and blobs are interned in the order they are met).
func TestWriteDeterministic(t *testing.T) {
	mk := func() *State {
		s := New()
		s.Files = map[string]string{"z": "1", "a": "2"}
		for i := range 20 {
			su := s.Suite(fmt.Sprintf("suite%02d", i))
			for j := range 20 {
				su.Tests[fmt.Sprintf("t%02d", j)] = &Entry{
					Deps:   map[string]string{fmt.Sprintf("dep/%d/%d", i, j): fmt.Sprintf("h%d", j), "shared": "s"},
					Status: Pass,
				}
			}
		}
		return s
	}
	for _, gz := range []bool{false, true} {
		var first []byte
		for range 10 {
			var buf bytes.Buffer
			if err := mk().Write(&buf, gz); err != nil {
				t.Fatal(err)
			}
			if first == nil {
				first = buf.Bytes()
			} else if !bytes.Equal(first, buf.Bytes()) {
				t.Fatalf("gzip=%v: two encodings of the same state differ", gz)
			}
		}
	}
}

// TestSaveMode: a new state file gets the mode any new file of the user gets (0666 less
// the umask), not os.CreateTemp's 0600; an existing one keeps its mode.
func TestSaveMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	fi, _ := os.Stat(probe)
	umasked := fi.Mode().Perm()
	for _, c := range []struct {
		name     string
		existing os.FileMode // 0: no file yet
		want     os.FileMode
	}{
		{"new file", 0, umasked},
		{"existing 0644", 0o644, 0o644},
		{"existing 0640", 0o640, 0o640},
		{"existing 0600", 0o600, 0o600},
		{"existing 0664", 0o664, 0o664},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "state.json.gz")
			if c.existing != 0 {
				if err := os.WriteFile(p, nil, c.existing); err != nil {
					t.Fatal(err)
				}
				os.Chmod(p, c.existing)
			}
			if err := sample().Save(p); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != c.want {
				t.Errorf("mode %o, want %o", fi.Mode().Perm(), c.want)
			}
		})
	}
}

// TestMergeCopies: what Merge takes from the other state is a copy: changing either
// state afterwards does not change the other.
func TestMergeCopies(t *testing.T) {
	a, b := New(), sample()
	b.Created = time.Now()
	b.AgentCache["k"] = json.RawMessage(`{"x":1}`)
	e := b.Suites["web"].Tests["src/a.test.ts"]
	e.History = []Outcome{{Status: Pass}}
	e.Flaky = &Flake{Count: 1}
	a.Merge(b)
	m := a.Suites["web"].Tests["src/a.test.ts"]
	a.Files["new"] = "x"
	m.Deps["added"] = "y"
	m.History[0].Status = Fail
	m.Flaky.Count = 5
	a.AgentCache["k"][1] = '!'
	if _, ok := b.Files["new"]; ok {
		t.Error("snapshot shared")
	}
	if _, ok := e.Deps["added"]; ok {
		t.Error("dependencies shared")
	}
	if e.History[0].Status != Pass {
		t.Error("history shared")
	}
	if e.Flaky.Count != 1 {
		t.Error("flake evidence shared")
	}
	if string(b.AgentCache["k"]) != `{"x":1}` {
		t.Error("agent cache shared")
	}
	// MergeChanges takes copies too.
	c := New()
	c.MergeChanges(nil, b)
	c.Suites["web"].Tests["src/a.test.ts"].Deps["added2"] = "z"
	if _, ok := e.Deps["added2"]; ok {
		t.Error("MergeChanges shares dependencies")
	}
}

// TestLockStaleRace: processes that all find the same lock file stale take the lock
// one at a time: none removes the fresh lock another created after breaking the stale
// one.
func TestLockStaleRace(t *testing.T) {
	for round := range 4 {
		path := filepath.Join(t.TempDir(), "state.lock")
		if err := os.WriteFile(path, []byte("pid 1 (crashed)\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		os.Chtimes(path, old, old)
		var mu sync.Mutex
		inside, maxInside := 0, 0
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				l, err := AcquireLock(path, 10*time.Second, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				inside++
				maxInside = max(maxInside, inside)
				mu.Unlock()
				time.Sleep(2 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				l.Release()
			}()
		}
		close(start)
		wg.Wait()
		if maxInside != 1 {
			t.Fatalf("round %d: %d holders at once", round, maxInside)
		}
		if _, err := os.Stat(path + ".break"); !os.IsNotExist(err) {
			t.Errorf("round %d: break file left behind: %v", round, err)
		}
	}
}

// TestLockBreakFile: a break file left by a crash (older than stale) does not keep a
// stale lock forever; a fresh one (another process is breaking the lock) is waited for.
func TestLockBreakFile(t *testing.T) {
	for _, c := range []struct {
		name     string
		breakAge time.Duration
		wantLock bool
	}{
		{"stale break file", time.Hour, true},
		{"fresh break file", 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.lock")
			old := time.Now().Add(-time.Hour)
			os.WriteFile(path, []byte("pid 1 (crashed)\n"), 0o644)
			os.Chtimes(path, old, old)
			os.WriteFile(path+".break", nil, 0o644)
			bt := time.Now().Add(-c.breakAge)
			os.Chtimes(path+".break", bt, bt)
			l, err := AcquireLock(path, 100*time.Millisecond, time.Minute)
			if (err == nil) != c.wantLock {
				t.Fatalf("err = %v, want lock %v", err, c.wantLock)
			}
			l.Release()
		})
	}
}

// TestReleaseKeepsOthersLock: a holder that lost its lock (it was judged stale and
// taken by another process) does not remove the other process's lock on release.
func TestReleaseKeepsOthersLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	slow, err := AcquireLock(path, time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
	fast, err := AcquireLock(path, time.Second, time.Minute)
	if err != nil {
		t.Fatalf("stale lock not taken: %v", err)
	}
	if err := slow.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the slow holder removed the new holder's lock: %v", err)
	}
	if err := fast.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("lock left after release: %v", err)
	}
}

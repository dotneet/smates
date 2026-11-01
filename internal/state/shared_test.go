package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// clone round-trips a state through its disk format, as a second Load would.
func clone(t *testing.T, s *State) *State {
	t.Helper()
	var buf bytes.Buffer
	if err := s.Write(&buf, false); err != nil {
		t.Fatal(err)
	}
	c, err := Read(&buf, false)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMergeChanges(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	base := sample()
	ours := clone(t, base)
	disk := clone(t, base)

	// ours: re-ran a.test.ts and cleared b.test.ts's flake evidence; added c.
	a := ours.Suites["web"].Tests["src/a.test.ts"]
	a.Status, a.At = Fail, t0.Add(time.Hour)
	ours.Suites["web"].Tests["src/c.test.ts"] = &Entry{Deps: map[string]string{"c": "1"}, Status: Pass, At: t0.Add(time.Hour)}
	ours.Runs = []Run{{ID: "ours", At: t0.Add(time.Hour)}}
	// disk (another worktree): recorded d, and touched b (flake evidence).
	disk.Suites["web"].Tests["src/d.test.ts"] = &Entry{Deps: map[string]string{"d": "1"}, Status: Pass, At: t0.Add(2 * time.Hour)}
	disk.Suites["web"].Tests["src/b.test.ts"].Flaky = &Flake{Count: 1, First: t0, Last: t0, Flip: "pass->fail"}
	disk.Runs = []Run{{ID: "disk", At: t0.Add(2 * time.Hour)}}
	disk.Files = map[string]string{"other": "worktree"}

	disk.MergeChanges(base, ours)
	tests := disk.Suites["web"].Tests
	if got := tests["src/a.test.ts"]; got.Status != Fail {
		t.Errorf("our update of a lost: %+v", got)
	}
	if tests["src/c.test.ts"] == nil || tests["src/d.test.ts"] == nil {
		t.Errorf("new entries lost: %v", tests)
	}
	if tests["src/b.test.ts"].Flaky == nil {
		t.Errorf("the other writer's change to b was overwritten by our unchanged copy")
	}
	if len(disk.Runs) != 2 {
		t.Errorf("runs = %v, want both", disk.Runs)
	}
	if disk.Files["other"] != "worktree" {
		t.Errorf("MergeChanges must leave the snapshot alone")
	}

	// A change that removes something (flaky --clear) is kept when nobody else
	// touched the entry, although Merge would bring the evidence back.
	base2 := clone(t, disk)
	ours2 := clone(t, disk)
	ours2.Suites["web"].Tests["src/b.test.ts"].Flaky = nil
	disk2 := clone(t, disk)
	disk2.MergeChanges(base2, ours2)
	if disk2.Suites["web"].Tests["src/b.test.ts"].Flaky != nil {
		t.Errorf("clearing flake evidence was undone")
	}

	// Both sides changed one entry: combined, newer wins.
	base3 := clone(t, disk)
	ours3, disk3 := clone(t, disk), clone(t, disk)
	ours3.Suites["web"].Tests["src/a.test.ts"].At = t0.Add(3 * time.Hour)
	ours3.Suites["web"].Tests["src/a.test.ts"].Status = Pass
	disk3.Suites["web"].Tests["src/a.test.ts"].At = t0.Add(4 * time.Hour)
	disk3.Suites["web"].Tests["src/a.test.ts"].Status = Fail
	disk3.MergeChanges(base3, ours3)
	if e := disk3.Suites["web"].Tests["src/a.test.ts"]; e.Status != Fail || !e.At.Equal(t0.Add(4*time.Hour)) {
		t.Errorf("concurrent change: got %+v, want the newer (disk) one", e)
	}
}

func TestMergeChangesDeletions(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	base := sample()
	base.Runs = []Run{{ID: "old", At: t0}}

	// Deleted on disk (`state clear` = an empty disk state, or another process's full
	// run pruned it): entries ours did not change stay deleted, and so do old runs.
	ours := clone(t, base)
	ours.Suites["web"].Tests["src/b.test.ts"].Status = Pass // changed by ours
	ours.Runs = append(ours.Runs, Run{ID: "new", At: t0.Add(time.Hour)})
	disk := New()
	disk.MergeChanges(base, ours)
	tests := disk.Suites["web"].Tests
	if tests["src/a.test.ts"] != nil {
		t.Errorf("an entry deleted on disk was put back")
	}
	if tests["src/b.test.ts"] == nil {
		t.Errorf("an entry ours changed was not written")
	}
	if len(disk.Runs) != 1 || disk.Runs[0].ID != "new" {
		t.Errorf("runs after clear: %v, want only the new one", disk.Runs)
	}

	// Deleted by ours (a full run pruned it): removed unless disk changed it.
	ours = clone(t, base)
	delete(ours.Suites["web"].Tests, "src/a.test.ts")
	delete(ours.Suites["web"].Tests, "src/b.test.ts")
	disk = clone(t, base)
	disk.Suites["web"].Tests["src/b.test.ts"].At = t0.Add(time.Hour)
	disk.MergeChanges(base, ours)
	tests = disk.Suites["web"].Tests
	if tests["src/a.test.ts"] != nil {
		t.Errorf("an entry ours pruned survived")
	}
	if tests["src/b.test.ts"] == nil {
		t.Errorf("an entry another process updated was pruned")
	}
}

func TestMergeChangesFlakyClear(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	base := sample()
	base.Suites["web"].Tests["src/a.test.ts"].Flaky = &Flake{Count: 2, First: t0, Last: t0, Flip: "pass->fail"}
	base = clone(t, base)
	ours := clone(t, base)
	ours.Suites["web"].Tests["src/a.test.ts"].Flaky = nil // flaky --clear
	// Meanwhile another worktree re-ran the test (no new flip).
	disk := clone(t, base)
	da := disk.Suites["web"].Tests["src/a.test.ts"]
	da.At, da.DurationMs = t0.Add(time.Hour), 99
	disk.MergeChanges(base, ours)
	got := disk.Suites["web"].Tests["src/a.test.ts"]
	if got.Flaky != nil || got.DurationMs != 99 {
		t.Errorf("clear lost to a concurrent update: flaky %+v, duration %d", got.Flaky, got.DurationMs)
	}

	// A flip seen after the clear's base is new evidence and is kept.
	disk = clone(t, base)
	da = disk.Suites["web"].Tests["src/a.test.ts"]
	da.At = t0.Add(time.Hour)
	da.Flaky = &Flake{Count: 3, First: t0, Last: t0.Add(time.Hour), Flip: "fail->pass"}
	disk.MergeChanges(base, ours)
	if f := disk.Suites["web"].Tests["src/a.test.ts"].Flaky; f == nil || f.Count != 3 {
		t.Errorf("a new flip was dropped: %+v", f)
	}
}

func TestSameEntry(t *testing.T) {
	s := sample()
	c := clone(t, s)
	for id, e := range s.Suites["web"].Tests {
		if !SameEntry(e, c.Suites["web"].Tests[id]) {
			t.Errorf("%s: round-tripped entry differs", id)
		}
	}
	e := *c.Suites["web"].Tests["src/a.test.ts"]
	e.DurationMs++
	if SameEntry(s.Suites["web"].Tests["src/a.test.ts"], &e) {
		t.Errorf("different durations compare equal")
	}
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.lock")
	l, err := AcquireLock(path, time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(path, 50*time.Millisecond, time.Minute); !errors.Is(err, ErrLocked) {
		t.Fatalf("second acquire: err = %v, want ErrLocked", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := AcquireLock(path, 50*time.Millisecond, time.Minute)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	l2.Release()
}

func TestLockStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	if err := os.WriteFile(path, []byte("pid 1 (crashed)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	l, err := AcquireLock(path, 50*time.Millisecond, time.Minute)
	if err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	l.Release()
	// A fresh lock is not stale.
	if err := os.WriteFile(path, []byte("pid 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLock(path, 50*time.Millisecond, time.Minute); !errors.Is(err, ErrLocked) {
		t.Fatalf("fresh lock: err = %v, want ErrLocked", err)
	}
}

// TestLockUnwritable: a lock file that cannot be created (no lock file in the way) is
// the file system's error, returned at once instead of ErrLocked after the wait.
func TestLockUnwritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	begin := time.Now()
	_, err := AcquireLock(filepath.Join(dir, "state.lock"), 2*time.Second, time.Minute)
	if err == nil || errors.Is(err, ErrLocked) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err = %v, want a permission error", err)
	}
	if time.Since(begin) > time.Second {
		t.Errorf("waited %v", time.Since(begin))
	}
}

func TestLockExcludes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	var mu sync.Mutex
	inside, maxInside := 0, 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := AcquireLock(path, 10*time.Second, time.Minute)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			inside++
			maxInside = max(maxInside, inside)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
			l.Release()
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Errorf("%d holders at once", maxInside)
	}
}

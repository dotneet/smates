package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/state"
)

// stateStore is where a working tree's state lives (README "Parallel worktrees",
// DESIGN.md §4):
//
//   - by default one file per working tree, cfg.State under its root;
//   - with state_shared, the per-test entries, agent cache and run records live in one
//     file under the git common directory that every worktree reads and writes (under
//     a lock, merging concurrent changes), and each worktree keeps its own full-tree
//     snapshot next to where its own state would be.
//
// A linked worktree without a state is seeded on first use from the main worktree's
// state, or from the remote state ref (`smates state push`).
type stateStore struct {
	repo *gitx.Repo
	cfg  *config.Config
	log  io.Writer
	// path holds the entries (and outside shared mode the snapshot too).
	path string
	// local is the worktree's own state path (cfg.State, or --state).
	local string
	// explicit: --state was given; no seeding, no sharing.
	explicit bool
	shared   bool
	// snapPath is this worktree's snapshot file in shared mode.
	snapPath string
	// base is the shared entries as last loaded or saved by this process, to tell its
	// own changes from concurrent ones.
	base *state.State
	wt   *gitx.Worktrees
}

// openStore resolves where the state lives. flagPath is --state ("" for the default).
func openStore(repo *gitx.Repo, cfg *config.Config, flagPath string, log io.Writer) *stateStore {
	s := &stateStore{repo: repo, cfg: cfg, log: log}
	if flagPath != "" {
		s.path, s.local, s.explicit = flagPath, flagPath, true
		return s
	}
	s.local = filepath.Join(repo.Root, filepath.FromSlash(cfg.State))
	s.path = s.local
	if cfg.StateShared {
		if wt := s.worktrees(); wt != nil && wt.CommonDir != "" {
			s.shared = true
			s.path = filepath.Join(wt.CommonDir, "smates", filepath.Base(s.local))
			s.snapPath = snapshotPath(s.local)
		}
	}
	return s
}

// resolveStore opens the store of the repository containing dir (commands that only
// need the state, not the project).
func resolveStore(dir, flagPath string, log io.Writer) (*stateStore, error) {
	if dir == "" {
		dir = "."
	}
	repo, err := gitx.Open(dir)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(repo.Root)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = &config.Config{}
		_ = cfg.Normalize()
	}
	return openStore(repo, cfg, flagPath, log), nil
}

// snapshotPath is the worktree's snapshot file in shared mode:
// .smates/state.json.gz -> .smates/state.snapshot.json.gz.
func snapshotPath(local string) string {
	dir, base := filepath.Split(local)
	name, ext, _ := strings.Cut(base, ".")
	if ext != "" {
		ext = "." + ext
	}
	return filepath.Join(dir, name+".snapshot"+ext)
}

func (s *stateStore) worktrees() *gitx.Worktrees {
	if s.wt == nil {
		wt, err := s.repo.Worktrees()
		if err != nil {
			return nil
		}
		s.wt = wt
	}
	return s.wt
}

func (s *stateStore) lockPath() string { return s.path + ".lock" }

// seedMarker records where a seeded state came from (and that a cleared one must not
// be seeded again).
func (s *stateStore) seedMarker() string {
	return filepath.Join(filepath.Dir(s.path), "."+filepath.Base(s.path)+".seeded")
}

// pullStamp is touched on every attempt to fetch the remote state, so the hook and
// seeding fetch at most once per hookPullEvery.
func (s *stateStore) pullStamp() string {
	return filepath.Join(filepath.Dir(s.local), "."+filepath.Base(s.local)+".pulled")
}

// pullDue reports whether a fetch is allowed now and records the attempt.
func (s *stateStore) pullDue() bool {
	stamp := s.pullStamp()
	if fi, err := os.Stat(stamp); err == nil && time.Since(fi.ModTime()) < hookPullEvery {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err == nil {
		_ = os.WriteFile(stamp, nil, 0o644)
	}
	return true
}

// Load reads the state, seeding it first when this is a worktree without one.
// Errors wrapping state.ErrUnreadable leave the caller to start cold.
func (s *stateStore) Load() (*state.State, error) {
	if !s.explicit {
		if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
			s.seed()
		}
	}
	if !s.shared {
		return state.Load(s.path)
	}
	// One read, decoded twice: st is this process's copy, base what it started from.
	// Two reads could see different versions (no lock is held here), and a base newer
	// than st would make Save take st's older entries for changes of this process.
	st, base := state.New(), state.New()
	data, err := readSharedState(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		gz := strings.HasSuffix(s.path, ".gz")
		if st, err = state.Read(bytes.NewReader(data), gz); err != nil {
			return nil, err
		}
		if base, err = state.Read(bytes.NewReader(data), gz); err != nil {
			return nil, err
		}
	}
	s.base = base
	// The shared file's snapshot is the last writer's, possibly another worktree's on
	// another branch: use this worktree's own, or none (no snapshot also means no
	// snapshot time, which the hook's staleness check and State.Merge go by).
	snap, err := state.Load(s.snapPath)
	if err == nil && len(snap.Files) > 0 {
		st.Files, st.Commit, st.Created = snap.Files, snap.Commit, snap.Created
	} else {
		dropSnapshot(st)
	}
	return st, nil
}

// readSharedState reads the shared state file (a variable for tests).
var readSharedState = os.ReadFile

// dropSnapshot removes the full-tree snapshot and what describes it.
func dropSnapshot(st *state.State) {
	st.Files, st.Commit, st.Created = map[string]string{}, "", time.Time{}
}

// Save writes the state. In shared mode it writes this worktree's snapshot, then, under
// the lock, re-reads the shared file and applies this process's changes to it
// (state.MergeChanges), so concurrent runs in other worktrees keep theirs; st then
// holds the merged entries.
func (s *stateStore) Save(st *state.State) error {
	if !s.shared {
		return st.Save(s.path)
	}
	snap := state.New()
	snap.Files, snap.Commit, snap.Created, snap.Tool = st.Files, st.Commit, st.Created, st.Tool
	if err := snap.Save(s.snapPath); err != nil {
		return err
	}
	lock, err := state.AcquireLock(s.lockPath(), state.LockWait, state.LockStale)
	if err != nil {
		return err
	}
	defer lock.Release()
	disk, err := state.Load(s.path)
	if err != nil {
		if !errors.Is(err, state.ErrUnreadable) {
			return err
		}
		fmt.Fprintf(s.log, "smates: replacing the unreadable shared state at %s (%v)\n", s.path, err)
		disk = state.New()
	}
	disk.MergeChanges(s.base, st)
	// The shared file stays a complete state (for `state push` and older smates): its
	// snapshot is the last writer's.
	disk.Files, disk.Commit, disk.Created = st.Files, st.Commit, st.Created
	if err := disk.Save(s.path); err != nil {
		return err
	}
	st.Suites, st.AgentCache, st.Runs, st.Tool = disk.Suites, disk.AgentCache, disk.Runs, disk.Tool
	if s.base, err = state.Load(s.path); err != nil {
		s.base = nil
	}
	return nil
}

// Clear removes the state (in shared mode the shared entries of every worktree and
// this worktree's snapshot) and keeps it from being seeded again.
func (s *stateStore) Clear() error {
	if s.shared {
		lock, err := state.AcquireLock(s.lockPath(), state.LockWait, state.LockStale)
		if err != nil {
			return err
		}
		defer lock.Release()
		if err := os.Remove(s.snapPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !s.explicit && s.seedable() {
		s.writeMarker("cleared")
	}
	return nil
}

func (s *stateStore) writeMarker(what string) {
	m := s.seedMarker()
	if err := os.MkdirAll(filepath.Dir(m), 0o755); err == nil {
		_ = os.WriteFile(m, []byte(what+"\n"+time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
	}
}

// Seeded returns where the state was seeded from and when ("" if it was not).
func (s *stateStore) Seeded() (from string, at time.Time) {
	data, err := os.ReadFile(s.seedMarker())
	if err != nil {
		return "", time.Time{}
	}
	what, when, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
	if what == "cleared" {
		return "", time.Time{}
	}
	at, _ = time.Parse(time.RFC3339, strings.TrimSpace(when))
	return what, at
}

// seedable: seeding applies to linked worktrees and to the shared file.
func (s *stateStore) seedable() bool {
	if s.shared {
		return true
	}
	wt := s.worktrees()
	return wt != nil && wt.Linked()
}

func seedDisabled(cfg *config.Config) bool {
	if v := os.Getenv("SMATES_NO_STATE_SEED"); v != "" && v != "0" && v != "false" {
		return true
	}
	return !config.Bool(cfg.StateSeed, true)
}

// seed fills a missing state from the main worktree's state file, or else from the
// remote state ref (fetched at most once per hookPullEvery; refs fetched before, by
// any worktree, are used without fetching). A snapshot recorded on a commit outside
// this worktree's history is dropped: it would report the other branch's changes as
// this one's (changed files then come from the default branch, as without a snapshot).
func (s *stateStore) seed() {
	if seedDisabled(s.cfg) || !s.seedable() {
		return
	}
	if _, err := os.Stat(s.seedMarker()); err == nil {
		return // seeded before, then cleared
	}
	src, from := s.seedFromMain()
	if src == nil {
		src, from = s.seedFromRemote()
	}
	if src == nil {
		return
	}
	if src.Commit == "" || !isAncestor(s.repo, src.Commit) {
		dropSnapshot(src)
	}
	s.base = nil
	if err := s.Save(src); err != nil {
		fmt.Fprintf(s.log, "smates: could not seed the state from %s: %v\n", from, err)
		return
	}
	s.writeMarker(from)
	where := "this worktree"
	if s.shared {
		where = "the shared state"
	}
	fmt.Fprintf(s.log, "smates: seeded %s from %s\n", where, from)
}

func (s *stateStore) seedFromMain() (*state.State, string) {
	wt := s.worktrees()
	if wt == nil || wt.Main == "" {
		return nil, ""
	}
	if !s.shared && filepath.Clean(wt.Main) == filepath.Clean(s.repo.Root) {
		return nil, ""
	}
	p := filepath.Join(wt.Main, filepath.FromSlash(s.cfg.State))
	st, err := state.Load(p)
	if err != nil || st.Empty() {
		return nil, ""
	}
	return st, "the main worktree (" + p + ")"
}

func (s *stateStore) seedFromRemote() (*state.State, string) {
	if _, err := s.repo.Git("remote", "get-url", "origin"); err != nil {
		return nil, ""
	}
	fetch := s.pullDue()
	for _, ref := range stateRefs(s.repo) {
		if fetch {
			_, _ = s.repo.Git("fetch", "--quiet", "--no-tags", "origin", "+"+ref+":"+ref)
		}
		cmd := exec.Command("git", "cat-file", "blob", ref+":state.json.gz")
		cmd.Dir = s.repo.Root
		data, err := cmd.Output()
		if err != nil {
			continue
		}
		st, err := state.Read(bytes.NewReader(data), true)
		if err != nil || st.Empty() {
			continue
		}
		return st, "origin " + ref
	}
	return nil, ""
}

func isAncestor(repo *gitx.Repo, commit string) bool {
	_, err := repo.Git("merge-base", "--is-ancestor", commit, "HEAD")
	return err == nil
}

// describe says where the state lives, for `smates state show`.
func (s *stateStore) describe() string {
	switch {
	case s.shared:
		return fmt.Sprintf("shared by all worktrees of the repository; this worktree's snapshot: %s", s.snapPath)
	case s.explicit:
		return "--state"
	default:
		return "this worktree"
	}
}

package gitx

import (
	"os"
	"path/filepath"
	"testing"
)

func sameDir(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// TestWorktreesThroughSymlink opens a linked worktree through a symlinked directory
// (as macOS temp dirs are: /var -> /private/var): the paths come back resolved.
func TestWorktreesThroughSymlink(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "a\n")
	r.commit("init")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	wtDir := filepath.Join(link, "wt")
	r.git("worktree", "add", "-q", "-b", "feature", wtDir)
	repo, err := Open(wtDir)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(wtDir)
	if err != nil {
		t.Fatal(err)
	}
	if repo.Root != resolved {
		t.Errorf("Root = %q, want the resolved %q", repo.Root, resolved)
	}
	w, err := repo.Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if !w.Linked() || !sameDir(w.Main, r.repo.Root) {
		t.Errorf("through a symlink: %+v (main %q)", w, r.repo.Root)
	}
	for _, p := range []string{w.GitDir, w.CommonDir, w.Main} {
		if q, _ := filepath.EvalSymlinks(p); q != p {
			t.Errorf("%q is not resolved (%q)", p, q)
		}
	}
}

func TestWorktrees(t *testing.T) {
	r := newTestRepo(t)
	r.write("a.txt", "a\n")
	r.commit("init")
	w, err := r.repo.Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if w.Linked() {
		t.Errorf("main worktree reported as linked: %+v", w)
	}
	if !sameDir(w.Main, r.repo.Root) {
		t.Errorf("Main = %q, want %q", w.Main, r.repo.Root)
	}
	wtDir := filepath.Join(t.TempDir(), "wt")
	r.git("worktree", "add", "-q", "-b", "feature", wtDir)
	repo, err := Open(wtDir)
	if err != nil {
		t.Fatal(err)
	}
	lw, err := repo.Worktrees()
	if err != nil {
		t.Fatal(err)
	}
	if !lw.Linked() {
		t.Errorf("linked worktree not detected: %+v", lw)
	}
	if !sameDir(lw.Main, r.repo.Root) {
		t.Errorf("linked Main = %q, want %q", lw.Main, r.repo.Root)
	}
	if !sameDir(lw.CommonDir, w.CommonDir) {
		t.Errorf("CommonDir differs: %q vs %q", lw.CommonDir, w.CommonDir)
	}
	if filepath.Base(filepath.Dir(lw.GitDir)) != "worktrees" {
		t.Errorf("GitDir = %q", lw.GitDir)
	}
}

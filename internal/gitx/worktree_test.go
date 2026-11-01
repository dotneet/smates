package gitx

import (
	"path/filepath"
	"testing"
)

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
	if w.Main != r.repo.Root {
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
	if lw.Main != r.repo.Root {
		t.Errorf("linked Main = %q, want %q", lw.Main, r.repo.Root)
	}
	if lw.CommonDir != w.CommonDir {
		t.Errorf("CommonDir differs: %q vs %q", lw.CommonDir, w.CommonDir)
	}
	if filepath.Base(filepath.Dir(lw.GitDir)) != "worktrees" {
		t.Errorf("GitDir = %q", lw.GitDir)
	}
}

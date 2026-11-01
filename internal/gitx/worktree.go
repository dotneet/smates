package gitx

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Worktrees describes where a working tree's git data lives.
type Worktrees struct {
	// GitDir is this working tree's git directory (.git, or .git/worktrees/<name> in
	// a linked worktree); CommonDir is the directory all worktrees share. Both are
	// absolute.
	GitDir, CommonDir string
	// Main is the main worktree's root, "" for a bare repository.
	Main string
}

// Linked reports whether the working tree is a linked worktree (`git worktree add`).
func (w *Worktrees) Linked() bool {
	return w.GitDir != "" && w.CommonDir != "" && filepath.Clean(w.GitDir) != filepath.Clean(w.CommonDir)
}

// Worktrees returns the git directories of the working tree and the main worktree's
// root.
func (r *Repo) Worktrees() (*Worktrees, error) {
	out, err := r.Git("rev-parse", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("unexpected output of git rev-parse: %q", out)
	}
	w := &Worktrees{GitDir: cleanAbs(r.Root, lines[0]), CommonDir: cleanAbs(r.Root, lines[1])}
	// The first entry of `git worktree list` is the main worktree (or the bare
	// repository, marked "bare").
	if list, err := r.Git("worktree", "list", "--porcelain"); err == nil {
		first, bare := "", false
		for i, line := range strings.Split(list, "\n") {
			if line == "" {
				break
			}
			if i == 0 {
				first, _ = strings.CutPrefix(line, "worktree ")
			} else if line == "bare" {
				bare = true
			}
		}
		if first != "" && !bare {
			w.Main = cleanAbs(r.Root, first)
		}
	} else if filepath.Base(w.CommonDir) == ".git" {
		w.Main = filepath.Dir(w.CommonDir)
	}
	return w, nil
}

func cleanAbs(base, p string) string {
	p = strings.TrimSpace(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Clean(p)
}

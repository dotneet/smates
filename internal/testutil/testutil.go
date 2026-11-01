// Package testutil holds helpers shared by the tests of smates: writing file trees into
// temporary directories and building throwaway git repositories.
//
// It is for tests only: import it from _test.go files, never from code that is linked
// into the smates binary (TestOnlyImportedByTests enforces this).
package testutil

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Write writes content to the slash-separated path rel under dir with mode 0644,
// creating parent directories as needed, and returns the absolute path.
func Write(t testing.TB, dir, rel, content string) string {
	t.Helper()
	return WriteMode(t, dir, rel, content, 0o644)
}

// WriteMode is Write with an explicit file mode (0o755 for scripts).
func WriteMode(t testing.TB, dir, rel, content string, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	return p
}

// WriteTree writes every file of files (slash-separated path relative to dir ->
// content) under dir.
func WriteTree(t testing.TB, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		Write(t, dir, rel, content)
	}
}

// Remove removes the file or directory tree at the slash-separated path rel under dir.
// A missing path is not an error.
func Remove(t testing.TB, dir, rel string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

// Files returns the slash-separated paths, relative to root, of every regular file and
// symlink under root (directories are not listed).
func Files(t testing.TB, root string) map[string]bool {
	t.Helper()
	files := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// GitRepo is a temporary git repository with a deterministic identity, no commit
// signing and no hooks. Its git commands ignore the user's global and system git
// configuration.
type GitRepo struct {
	t testing.TB
	// Dir is the root of the working tree (as returned by t.TempDir, symlinks not
	// resolved).
	Dir string
}

// NewGitRepo creates a repository on branch main in a new temporary directory. When
// files is not empty they are written and committed as "initial". The test is skipped
// when git is not on PATH.
func NewGitRepo(t testing.TB, files map[string]string) *GitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	r := &GitRepo{t: t, Dir: t.TempDir()}
	r.Git("init", "-q", "-b", "main")
	for _, kv := range [][2]string{
		{"user.name", "smates"},
		{"user.email", "smates@example.com"},
		{"commit.gpgsign", "false"},
		{"tag.gpgsign", "false"},
		{"core.autocrlf", "false"},
		{"core.hooksPath", os.DevNull},
	} {
		r.Git("config", kv[0], kv[1])
	}
	if len(files) > 0 {
		r.WriteTree(files)
		r.Commit("initial")
	}
	return r
}

// Git runs git with args in the repository and returns its combined output with
// surrounding whitespace trimmed. It fails the test when git fails.
func (r *GitRepo) Git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Write writes content to the slash-separated path rel in the working tree and returns
// the absolute path.
func (r *GitRepo) Write(rel, content string) string {
	r.t.Helper()
	return Write(r.t, r.Dir, rel, content)
}

// WriteTree writes files (slash-separated path -> content) into the working tree.
func (r *GitRepo) WriteTree(files map[string]string) {
	r.t.Helper()
	WriteTree(r.t, r.Dir, files)
}

// Remove removes rel from the working tree (not from the index).
func (r *GitRepo) Remove(rel string) {
	r.t.Helper()
	Remove(r.t, r.Dir, rel)
}

// Commit stages every change (git add -A), commits it with msg and returns the new
// HEAD commit's SHA.
func (r *GitRepo) Commit(msg string) string {
	r.t.Helper()
	r.Git("add", "-A")
	r.Git("commit", "-q", "-m", msg)
	return r.Git("rev-parse", "HEAD")
}

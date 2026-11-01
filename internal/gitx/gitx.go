// Package gitx wraps the few git operations smates needs: locating the repository,
// listing files, and hashing contents the same way git does (blob SHA-1).
package gitx

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Repo is a git working tree.
type Repo struct {
	Root string // absolute, symlinks resolved
}

// Open finds the repository containing dir.
func Open(dir string) (*Repo, error) {
	out, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not inside a git repository (%s): %w", dir, err)
	}
	root := strings.TrimSpace(out)
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	return &Repo{Root: root}, nil
}

// Git runs a git command in the repository root and returns stdout.
func (r *Repo) Git(args ...string) (string, error) { return run(r.Root, args...) }

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Head returns the commit SHA of HEAD, or "" if there is none.
func (r *Repo) Head() string {
	out, err := r.Git("rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Snapshot maps every non-ignored file in the working tree (tracked and untracked)
// to its git blob SHA-1 as it exists on disk right now. Deleted tracked files are omitted.
type Snapshot map[string]string

// Snapshot computes the current content hashes. Unmodified tracked files take their
// hash from the index, so the cost is one `git ls-files` plus hashing of dirty files.
func (r *Repo) Snapshot() (Snapshot, error) {
	out, err := r.Git("-c", "core.quotepath=off", "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	snap := Snapshot{}
	for _, rec := range strings.Split(out, "\x00") {
		// "<mode> <sha> <stage>\t<path>"
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		meta := strings.Fields(rec[:tab])
		if len(meta) != 3 {
			continue
		}
		if meta[0] == "160000" {
			// A submodule: its pinned commit stands for its contents, so bumping it is a
			// change (tests reading files inside it are tracked through those files).
			snap[rec[tab+1:]] = "commit:" + meta[1]
			continue
		}
		snap[rec[tab+1:]] = meta[1]
	}
	// Files whose working-tree content differs from the index, plus untracked ones.
	st, err := r.Git("-c", "core.quotepath=off", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules")
	if err != nil {
		return nil, err
	}
	var dirty []string
	for _, rec := range strings.Split(st, "\x00") {
		if len(rec) < 4 {
			continue
		}
		x, y, p := rec[0], rec[1], rec[3:]
		switch {
		case x == '?' || y != ' ' || x != ' ':
			dirty = append(dirty, p)
		}
	}
	hashes := HashFiles(r.Root, dirty)
	for i, p := range dirty {
		if hashes[i] == "" {
			delete(snap, p) // deleted in the working tree
		} else {
			snap[p] = hashes[i]
		}
	}
	return snap, nil
}

// HashFiles hashes repo-relative paths in parallel. Missing files hash to "".
func HashFiles(root string, paths []string) []string {
	res := make([]string, len(paths))
	var wg sync.WaitGroup
	ch := make(chan int)
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				res[i], _ = HashFile(filepath.Join(root, filepath.FromSlash(paths[i])))
			}
		}()
	}
	for i := range paths {
		ch <- i
	}
	close(ch)
	wg.Wait()
	return res
}

// HashFile returns the git blob SHA-1 of a file ("" and an error if unreadable).
// Symlinks are hashed by their target path, as git does.
func HashFile(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		return HashBytes([]byte(target)), nil
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New()
	io.WriteString(h, "blob "+strconv.FormatInt(fi.Size(), 10)+"\x00")
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashBytes returns the git blob SHA-1 of data.
func HashBytes(data []byte) string {
	h := sha1.New()
	io.WriteString(h, "blob "+strconv.Itoa(len(data))+"\x00")
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ChangedSince lists files that differ between base and the working tree
// (committed, staged, unstaged and untracked), repo-relative.
func (r *Repo) ChangedSince(base string) ([]string, error) {
	out, err := r.Git("-c", "core.quotepath=off", "diff", "--name-only", "-z", "--no-renames", base)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var files []string
	add := func(s string) {
		for _, p := range strings.Split(s, "\x00") {
			if p != "" && !seen[p] {
				seen[p] = true
				files = append(files, p)
			}
		}
	}
	add(out)
	un, err := r.Git("-c", "core.quotepath=off", "ls-files", "-o", "--exclude-standard", "-z")
	if err == nil {
		add(un)
	}
	return files, nil
}

// MergeBase returns the merge base of HEAD and ref, or "" when it cannot be computed
// (e.g. shallow clones).
func (r *Repo) MergeBase(ref string) string {
	out, err := r.Git("merge-base", "HEAD", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// RefExists reports whether ref resolves to a commit.
func (r *Repo) RefExists(ref string) bool {
	_, err := r.Git("rev-parse", "--verify", "-q", ref+"^{commit}")
	return err == nil
}

// DefaultBranch guesses the default branch name ("main" if unknown).
func (r *Repo) DefaultBranch() string {
	if out, err := r.Git("symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
		s := strings.TrimSpace(out)
		return strings.TrimPrefix(s, "origin/")
	}
	for _, b := range []string{"main", "master"} {
		if r.RefExists("refs/remotes/origin/"+b) || r.RefExists("refs/heads/"+b) {
			return b
		}
	}
	return "main"
}

// CurrentBranch returns the checked-out branch name, or "" when detached.
func (r *Repo) CurrentBranch() string {
	out, err := r.Git("symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// BlobContent returns the content of a blob if the object exists locally.
func (r *Repo) BlobContent(sha string) ([]byte, bool) {
	cmd := exec.Command("git", "cat-file", "blob", sha)
	cmd.Dir = r.Root
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	return out, true
}

// Commit is a commit on the first-parent history of HEAD.
type Commit struct {
	SHA     string
	Parent  string // first parent, "" for a root commit
	Subject string
}

// FirstParentCommits lists the commits reachable from HEAD by first parents, newest
// first: those after since (when not "") and at most n of them (n <= 0: no limit).
func (r *Repo) FirstParentCommits(since string, n int) ([]Commit, error) {
	args := []string{"log", "--first-parent", "-z", "--format=%H%x09%P%x09%s"}
	if n > 0 {
		args = append(args, "-n", strconv.Itoa(n))
	}
	rev := "HEAD"
	if since != "" {
		rev = since + "..HEAD"
	}
	out, err := r.Git(append(args, rev, "--")...)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, rec := range strings.Split(out, "\x00") {
		f := strings.SplitN(strings.TrimSpace(rec), "\t", 3)
		if len(f) < 3 {
			continue
		}
		c := Commit{SHA: f[0], Subject: f[2]}
		if ps := strings.Fields(f[1]); len(ps) > 0 {
			c.Parent = ps[0]
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// FileChange is a file a commit changed. Status is git's name-status letter: 'A'
// (added), 'D' (deleted), 'M' (modified) or 'T' (type changed); renames are reported
// as a deletion and an addition.
type FileChange struct {
	Path   string
	Status byte
}

// ChangedInCommit lists the files a commit changed relative to its first parent (every
// file of a root commit), so a merge contributes what it brought into the branch.
func (r *Repo) ChangedInCommit(c Commit) ([]FileChange, error) {
	args := []string{"-c", "core.quotepath=off", "diff-tree", "-r", "-z", "--no-renames", "--no-commit-id", "--name-status"}
	if c.Parent == "" {
		args = append(args, "--root", c.SHA)
	} else {
		args = append(args, c.Parent, c.SHA)
	}
	out, err := r.Git(args...)
	if err != nil {
		return nil, err
	}
	var changes []FileChange
	f := strings.Split(out, "\x00")
	for i := 0; i+1 < len(f) && f[i] != ""; i += 2 {
		changes = append(changes, FileChange{Path: f[i+1], Status: f[i][0]})
	}
	return changes, nil
}

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

	// The cache of flaggedPaths: the index file, the signature it had when last read
	// and the flagged files it listed.
	flagMu    sync.Mutex
	indexFile string
	flagSig   *FileSig
	flagged   []string
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
// to its hash (see HashFile) as it exists on disk right now. Deleted tracked files are
// omitted.
type Snapshot map[string]string

// ExecSuffix follows the blob SHA-1 in the hash of an executable regular file (git mode
// 100755), so that adding or removing the executable bit is a change: a test that runs
// a script fails differently once it cannot execute it.
const ExecSuffix = "+x"

// Blob returns the git blob SHA-1 a file hash (Snapshot, HashFile) holds, without the
// executable marker.
func Blob(hash string) string { return strings.TrimSuffix(hash, ExecSuffix) }

// SameHash reports whether a recorded hash still matches the current one: they are
// equal, or recorded has no executable marker and equals cur without its markers.
// smates before the marker recorded executable files by their blob alone, and an
// older smates may still be writing the state (on CI); counting those as changed would
// make every executable file a change after each such recording. The price: a file
// that only gains the executable bit is no change. Losing a recorded one is.
func SameHash(recorded, cur string) bool {
	return recorded == cur ||
		strings.Contains(cur, ExecSuffix) && !strings.Contains(recorded, ExecSuffix) && recorded == strings.ReplaceAll(cur, ExecSuffix, "")
}

// Snapshot computes the current content hashes. Unmodified tracked files take their
// hash from the index, so the cost is one `git ls-files` plus hashing of dirty files.
//
// Files flagged skip-worktree or assume-unchanged (git update-index) are hashed from
// disk too: git status does not report edits to them, so their index hash would hide a
// local edit a test reads. One that is not on disk keeps its index hash when it is
// skip-worktree (a sparse checkout leaves it out of the working tree: git counts it as
// unchanged) and is deleted otherwise.
func (r *Repo) Snapshot() (Snapshot, error) {
	idx, err := r.index()
	if err != nil {
		return nil, err
	}
	snap := Snapshot{}
	indexExec := map[string]bool{} // regular files in the index: executable or not
	for _, e := range idx {
		switch e.mode {
		case "160000":
			// A submodule: its pinned commit stands for its contents, so bumping it is a
			// change (tests reading files inside it are tracked through those files).
			snap[e.path] = "commit:" + e.sha
		case "100755":
			snap[e.path] = e.sha + ExecSuffix
			indexExec[e.path] = true
		case "100644":
			snap[e.path] = e.sha
			indexExec[e.path] = false
		default:
			snap[e.path] = e.sha
		}
	}
	// Files whose working-tree content or executable bit differs from the index, plus
	// untracked ones.
	st, err := r.Git("-c", "core.quotepath=off", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules")
	if err != nil {
		return nil, err
	}
	var dirty []string
	seen := map[string]bool{}
	for _, rec := range strings.Split(st, "\x00") {
		if len(rec) < 4 {
			continue
		}
		x, y, p := rec[0], rec[1], rec[3:]
		switch {
		case x == '?' || y != ' ' || x != ' ':
			dirty = append(dirty, p)
			seen[p] = true
		}
	}
	// Flagged files git status is blind to.
	keep := map[string]bool{} // skip-worktree: absent from disk keeps the index hash
	for _, e := range idx {
		if !e.flagged() || e.mode == "160000" {
			continue
		}
		if e.skipWorktree() {
			keep[e.path] = true
		}
		if !seen[e.path] {
			dirty = append(dirty, e.path)
			seen[e.path] = true
		}
	}
	if len(dirty) == 0 {
		return snap, nil
	}
	hashes := hashFiles(r.Root, dirty, r.indexModes(indexExec))
	for i, p := range dirty {
		switch {
		case hashes[i] != "":
			snap[p] = hashes[i]
		case keep[p] && !r.onDisk(p):
			// not checked out (sparse): the index hash stands
		default:
			delete(snap, p) // deleted in the working tree
		}
	}
	return snap, nil
}

// onDisk reports whether a repo-relative path exists in the working tree (as itself,
// not following a final symlink).
func (r *Repo) onDisk(p string) bool {
	_, err := os.Lstat(filepath.Join(r.Root, filepath.FromSlash(p)))
	return err == nil
}

// indexModes returns, with core.fileMode false (Windows, file systems without an
// executable bit), the executable bits of the index, which git then uses instead of
// the bit on disk: a tracked file keeps the index's mode, a new one is added as not
// executable. So does smates. nil otherwise (the file system's bit counts).
func (r *Repo) indexModes(indexExec map[string]bool) map[string]bool {
	if v, err := r.Git("config", "--bool", "core.fileMode"); err == nil && strings.TrimSpace(v) == "false" {
		return indexExec
	}
	return nil
}

// indexEntry is one entry of the index as `git ls-files -s -v` lists it.
type indexEntry struct {
	tag  byte // H cached, S skip-worktree; lowercase: assume-unchanged (see git ls-files -v)
	mode string
	sha  string
	path string
}

// flagged reports whether git status ignores the working-tree file of the entry:
// skip-worktree (S, s) or assume-unchanged (any lowercase tag).
func (e indexEntry) flagged() bool {
	return e.tag == 'S' || 'a' <= e.tag && e.tag <= 'z'
}

// skipWorktree reports whether the entry is flagged skip-worktree.
func (e indexEntry) skipWorktree() bool { return e.tag == 'S' || e.tag == 's' }

// index lists the entries of the index.
func (r *Repo) index() ([]indexEntry, error) {
	out, err := r.Git("-c", "core.quotepath=off", "ls-files", "-s", "-v", "-z")
	if err != nil {
		return nil, err
	}
	return parseIndex(out), nil
}

// parseIndex parses the output of `git ls-files -s -v -z`.
func parseIndex(out string) []indexEntry {
	var entries []indexEntry
	for _, rec := range strings.Split(out, "\x00") {
		// "<tag> <mode> <sha> <stage>\t<path>"
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		meta := strings.Fields(rec[:tab])
		if len(meta) != 4 || len(meta[0]) != 1 {
			continue
		}
		entries = append(entries, indexEntry{tag: meta[0][0], mode: meta[1], sha: meta[2], path: rec[tab+1:]})
	}
	return entries
}

// FlaggedEdits lists the files flagged skip-worktree or assume-unchanged whose content
// (or executable bit) on disk differs from the index: edits git status and git diff do
// not report. A skip-worktree file absent from disk (a sparse checkout) is no edit; an
// assume-unchanged one is (deleted).
func (r *Repo) FlaggedEdits() ([]string, error) {
	idx, err := r.index()
	if err != nil {
		return nil, err
	}
	var paths, want []string
	var skip []bool
	indexExec := map[string]bool{}
	for _, e := range idx {
		if e.mode == "100755" || e.mode == "100644" {
			indexExec[e.path] = e.mode == "100755"
		}
		if !e.flagged() || e.mode == "160000" {
			continue
		}
		h := e.sha
		if e.mode == "100755" {
			h += ExecSuffix
		}
		paths, want, skip = append(paths, e.path), append(want, h), append(skip, e.skipWorktree())
	}
	if len(paths) == 0 {
		return nil, nil
	}
	hashes := hashFiles(r.Root, paths, r.indexModes(indexExec))
	var out []string
	for i, p := range paths {
		switch {
		case hashes[i] == want[i]:
		case hashes[i] == "" && skip[i] && !r.onDisk(p):
		default:
			out = append(out, p)
		}
	}
	return out, nil
}

// HashFiles hashes repo-relative paths in parallel (see HashFile). Missing files hash
// to "".
func HashFiles(root string, paths []string) []string { return hashFiles(root, paths, nil) }

// hashFiles is HashFiles taking the executable bit of regular files from exec instead
// of the file system when exec is not nil (a file it does not list is not executable).
func hashFiles(root string, paths []string, exec map[string]bool) []string {
	res := make([]string, len(paths))
	var wg sync.WaitGroup
	ch := make(chan int)
	for w := 0; w < runtime.NumCPU(); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				blob, mode, err := hashFile(filepath.Join(root, filepath.FromSlash(paths[i])))
				if err != nil {
					continue
				}
				x := isExec(mode)
				if exec != nil {
					x = exec[paths[i]]
				}
				if mode.IsRegular() && x {
					blob += ExecSuffix
				}
				res[i] = blob
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

// HashFile returns the hash smates records for a file ("" and an error if unreadable):
// its git blob SHA-1, followed by ExecSuffix for an executable regular file (one git
// records with mode 100755: the owner's executable bit is set). Symlinks are hashed by
// their target path, as git does.
func HashFile(path string) (string, error) {
	blob, mode, err := hashFile(path)
	if err != nil {
		return "", err
	}
	if mode.IsRegular() && isExec(mode) {
		blob += ExecSuffix
	}
	return blob, nil
}

// isExec reports whether git records a regular file of this mode as executable.
func isExec(mode os.FileMode) bool { return mode.Perm()&0o100 != 0 }

// hashFile returns the git blob SHA-1 of a file and its mode.
func hashFile(path string) (string, os.FileMode, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", 0, err
		}
		return HashBytes([]byte(target)), fi.Mode(), nil
	}
	if !fi.Mode().IsRegular() {
		return "", 0, errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha1.New()
	io.WriteString(h, "blob "+strconv.FormatInt(fi.Size(), 10)+"\x00")
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), fi.Mode(), nil
}

// HashBytes returns the git blob SHA-1 of data.
func HashBytes(data []byte) string {
	h := sha1.New()
	io.WriteString(h, "blob "+strconv.Itoa(len(data))+"\x00")
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ChangedSince lists files that differ between base and the working tree
// (committed, staged, unstaged and untracked, and edits to files flagged
// skip-worktree or assume-unchanged, see FlaggedEdits), repo-relative.
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
	if err != nil {
		return nil, err // untracked files would go missing from the changes
	}
	add(un)
	// Edits git diff does not report (skip-worktree, assume-unchanged).
	flagged, err := r.FlaggedEdits()
	if err != nil {
		return nil, err
	}
	for _, p := range flagged {
		add(p)
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
	Parent  string // first parent, "" for a root commit or a shallow boundary
	Subject string
	// Shallow marks a commit at the boundary of a shallow clone: its parents were not
	// fetched, so what it changed is unknown (it is not a root commit).
	Shallow bool
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
	shallow, err := r.shallowCommits()
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, rec := range strings.Split(out, "\x00") {
		f := strings.SplitN(strings.TrimSpace(rec), "\t", 3)
		if len(f) < 3 {
			continue
		}
		c := Commit{SHA: f[0], Subject: f[2], Shallow: shallow[f[0]]}
		if ps := strings.Fields(f[1]); len(ps) > 0 {
			c.Parent = ps[0]
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// shallowCommits returns the boundary commits of a shallow clone (none otherwise).
func (r *Repo) shallowCommits() (map[string]bool, error) {
	out, err := r.Git("rev-parse", "--is-shallow-repository")
	if err != nil || strings.TrimSpace(out) != "true" {
		return nil, nil // not shallow (or a git too old to say)
	}
	p, err := r.Git("rev-parse", "--git-path", "shallow")
	if err != nil {
		return nil, err
	}
	file := strings.TrimSpace(p)
	if !filepath.IsAbs(file) {
		file = filepath.Join(r.Root, file)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("shallow repository: %w", err)
	}
	set := map[string]bool{}
	for _, l := range strings.Fields(string(data)) {
		set[l] = true
	}
	return set, nil
}

// FileChange is a file a commit changed. Status is git's name-status letter: 'A'
// (added), 'D' (deleted), 'M' (modified) or 'T' (type changed); renames are reported
// as a deletion and an addition.
type FileChange struct {
	Path   string
	Status byte
}

// ChangedInCommit lists the files a commit changed relative to its first parent (every
// file of a root commit), so a merge contributes what it brought into the branch. It
// fails for a shallow boundary commit (Commit.Shallow), whose parent is unknown.
func (r *Repo) ChangedInCommit(c Commit) ([]FileChange, error) {
	if c.Shallow && c.Parent == "" {
		return nil, fmt.Errorf("commit %s is at the boundary of a shallow clone: its parent was not fetched", c.SHA)
	}
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

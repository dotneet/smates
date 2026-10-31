package gitx

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TreeState is a cheap fingerprint of the working tree for change polling (`smates
// watch`): HEAD plus the stat signature of every file that differs from HEAD or the
// index (modified, staged, deleted, untracked; .gitignore respected). Clean files are
// not listed: while HEAD stays the same, a clean file can only change by becoming dirty.
type TreeState struct {
	Head  string
	Files map[string]FileSig
}

// FileSig identifies a file version by its metadata (no content is read).
type FileSig struct {
	Exists bool
	Size   int64
	Mtime  int64 // ns
	Mode   os.FileMode
	// Dirty: the file differs from HEAD or the index (it was listed by git status).
	Dirty bool
	// Racy: modified so shortly before the scan that a later write could keep the same
	// size and mtime (coarse file system clocks); its content hash is then recorded,
	// and recorded again by the next scan that is given this signature in also.
	Racy bool
	Hash string
}

// racyWindow covers file system timestamp granularity (a clock tick on Linux, 2s on FAT).
const racyWindow = 2 * time.Second

func (a FileSig) same(b FileSig) bool {
	return a.Exists == b.Exists && a.Size == b.Size && a.Mtime == b.Mtime && a.Mode == b.Mode &&
		(a.Hash == "" || b.Hash == "" || a.Hash == b.Hash)
}

// ScanTree fingerprints the working tree. Paths in also (typically the dirty files of
// an earlier scan) are stat'ed even when they are clean now, so Changes can tell a
// commit (same metadata) from a revert (new metadata). It runs one `git status` that
// takes no locks, so polling never collides with the user's own git commands.
func (r *Repo) ScanTree(also map[string]FileSig) (*TreeState, error) {
	racySince := time.Now().Add(-racyWindow).UnixNano()
	out, err := r.Git("--no-optional-locks", "-c", "core.quotepath=off", "status", "--porcelain=v2", "--branch", "-z",
		"--untracked-files=all", "--no-renames", "--ignore-submodules")
	if err != nil {
		return nil, err
	}
	ts := &TreeState{Files: map[string]FileSig{}}
	for _, rec := range strings.Split(out, "\x00") {
		var p string
		switch {
		case strings.HasPrefix(rec, "# branch.oid "):
			ts.Head = strings.TrimPrefix(rec, "# branch.oid ")
			continue
		case strings.HasPrefix(rec, "1 "):
			// 1 XY sub mH mI mW hH hI path
			if f := strings.SplitN(rec, " ", 9); len(f) == 9 {
				p = f[8]
			}
		case strings.HasPrefix(rec, "u "):
			// u XY sub m1 m2 m3 mW h1 h2 h3 path
			if f := strings.SplitN(rec, " ", 11); len(f) == 11 {
				p = f[10]
			}
		case strings.HasPrefix(rec, "? "):
			p = rec[2:]
		}
		if p != "" {
			ts.Files[p] = FileSig{Dirty: true}
		}
	}
	for p := range also {
		if _, ok := ts.Files[p]; !ok {
			ts.Files[p] = FileSig{}
		}
	}
	for p, sig := range ts.Files {
		abs := filepath.Join(r.Root, filepath.FromSlash(p))
		fi, err := os.Lstat(abs)
		if err == nil {
			sig.Exists, sig.Size, sig.Mtime, sig.Mode = true, fi.Size(), fi.ModTime().UnixNano(), fi.Mode()
			sig.Racy = sig.Mtime >= racySince
			if sig.Racy || also[p].Racy {
				sig.Hash, _ = HashFile(abs)
			}
		}
		ts.Files[p] = sig
	}
	return ts, nil
}

// Dirty returns the files the state lists as dirty (for the next ScanTree's also).
func (t *TreeState) Dirty() map[string]FileSig {
	out := map[string]FileSig{}
	for p, s := range t.Files {
		if s.Dirty {
			out[p] = s
		}
	}
	return out
}

// Changes lists the files whose content may differ between two scans, sorted. b must
// have been scanned with a's dirty files in also. A file dirty in either scan changed
// when its metadata differs (so committing or staging it is no change, editing or
// reverting it is). A file clean in both changed only when HEAD moved and the two
// commits differ in it (checkout, pull, reset).
func (r *Repo) Changes(a, b *TreeState) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for p, sb := range b.Files {
		sa, ok := a.Files[p]
		switch {
		case !ok && sb.Dirty:
			add(p) // newly dirty
		case ok && (sa.Dirty || sb.Dirty) && !sa.same(sb):
			add(p)
		}
	}
	for p, sa := range a.Files {
		if _, ok := b.Files[p]; !ok && sa.Dirty {
			add(p) // b was scanned without it in also: treat as changed
		}
	}
	if a.Head != b.Head {
		diff, err := r.Git("-c", "core.quotepath=off", "diff", "--name-only", "-z", "--no-renames", a.Head, b.Head)
		if err != nil && a.Head != "(initial)" && b.Head != "(initial)" {
			return nil, err
		}
		for _, p := range strings.Split(diff, "\x00") {
			if p == "" {
				continue
			}
			sa, inA := a.Files[p]
			sb, inB := b.Files[p]
			if (inA && sa.Dirty) || (inB && sb.Dirty) {
				continue // decided by metadata above
			}
			add(p)
		}
	}
	sort.Strings(out)
	return out, nil
}

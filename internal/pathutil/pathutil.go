// Package pathutil normalizes paths to repo-relative, slash-separated form and matches
// them against doublestar globs.
package pathutil

import (
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Rel converts an absolute (or file:// URL) path to a clean repo-relative slash path.
// It resolves symlinks when possible. ok is false for paths outside root.
//
// A query or fragment on a plain path (/repo/src/a.ts?v=3, as dev servers and V8
// coverage report modules) is dropped only when the path itself does not exist, the
// part before it is an existing file and the name holding the '?' or '#' is not an
// existing directory, so names that merely contain them (/repo/notes#1/new.txt, a probe
// of a file not created yet) are kept. A file:// URL's query and fragment are already
// separated by url.Parse.
//
// Module ids that name no file are not paths: Node's builtins (node:fs, joined to a
// directory by resolvers probing them: /repo/pkg/node:internal/x) and bundlers' virtual
// modules ("\x00vite/...").
func Rel(root, p string) (string, bool) {
	if strings.HasPrefix(p, "file://") {
		u, err := url.Parse(p)
		if err != nil {
			return "", false
		}
		p = fileURLPath(u.Path, runtime.GOOS == "windows")
	} else if i := strings.IndexAny(p, "?#"); i >= 0 && !fileExists(p) && isFile(p[:i]) && !nameExists(p, i) {
		p = p[:i]
	}
	if !filepath.IsAbs(p) || strings.ContainsRune(p, 0) || strings.Contains(filepath.ToSlash(p), "/node:") {
		return "", false
	}
	p = resolve(filepath.Clean(p))
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// fileURLPath turns the path of a file:// URL into a file path. On Windows the URL path
// of a drive-letter path has a leading slash ("/C:/repo/a.ts"), which is not absolute.
func fileURLPath(p string, windows bool) string {
	if !windows {
		return p
	}
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' && ('a' <= p[1]|0x20 && p[1]|0x20 <= 'z') {
		p = p[1:]
	}
	return filepath.FromSlash(p)
}

// resolve evaluates symlinks in p. For a path that does not exist (a probe of a file
// that is not there yet), the deepest existing ancestor is resolved instead, so the
// path still maps into a repository reached through a symlink.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	rest := filepath.Base(p)
	for dir := filepath.Dir(p); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(r, rest)
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
	return p
}

// maxLinks bounds the symlinks Links follows (a loop ends there).
const maxLinks = 255

// Links lists the repo-relative paths of the symlinks below root that resolving the
// absolute path p goes through, in the order they are met: p itself, a directory on the
// way, a link a link points to. Rel only gives the resolved path, but a link is a file
// of its own (git records its target), and retargeting it changes what p reads.
// Symlinks outside root (a root reached through one) and vendored ones are left out.
func Links(root, p string) []string {
	if !filepath.IsAbs(p) {
		return nil
	}
	vol := filepath.VolumeName(p)
	sep := string(filepath.Separator)
	split := func(s string) []string { return strings.Split(s, sep) }
	rest := split(filepath.Clean(p)[len(vol):])
	cur := vol + sep // resolved so far: no symlinks
	var out []string
	for hops := 0; len(rest) > 0; {
		c := rest[0]
		rest = rest[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, c)
		fi, err := os.Lstat(next)
		if err != nil {
			break // the rest does not exist
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if hops++; hops > maxLinks {
			break
		}
		if rel, err := filepath.Rel(root, next); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+sep) {
			if r := filepath.ToSlash(rel); !IsVendored(r) && !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
		target, err := os.Readlink(next)
		if err != nil {
			break
		}
		target = filepath.FromSlash(target)
		if filepath.IsAbs(target) {
			v := filepath.VolumeName(target)
			cur, target = v+sep, target[len(v):]
		}
		rest = append(split(target), rest...)
	}
	return out
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// nameExists reports whether the path element of p holding offset i exists as a
// directory (notes#1 in /repo/notes#1/new.txt): the '?' or '#' at i is then part of a
// name rather than the start of a query.
func nameExists(p string, i int) bool {
	k := strings.IndexAny(p[i:], `/\`)
	return k >= 0 && fileExists(p[:i+k])
}

// isFile reports whether p exists and is not a directory.
func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// Join joins repo-relative slash paths and cleans the result ("." stays ".").
func Join(elem ...string) string { return path.Clean(path.Join(elem...)) }

// Under reports whether p is dir itself or inside it (both repo-relative). "." contains everything.
func Under(p, dir string) bool {
	if dir == "." || dir == "" {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// RelTo returns p relative to dir (both repo-relative, p under dir).
func RelTo(p, dir string) string {
	if dir == "." || dir == "" {
		return p
	}
	if p == dir {
		return "."
	}
	return strings.TrimPrefix(p, dir+"/")
}

// Match reports whether p matches any of the globs.
func Match(globs []string, p string) bool {
	for _, g := range globs {
		if ok, _ := doublestar.Match(g, p); ok {
			return true
		}
	}
	return false
}

// IsVendored reports paths that are never test inputs we track (dependency trees,
// caches, VCS metadata).
func IsVendored(p string) bool {
	// Segment by segment without splitting: selection asks for every dependency of
	// every test.
	for rest := p; ; {
		seg, more, found := strings.Cut(rest, "/")
		switch seg {
		case "node_modules", ".git", "__pycache__", ".venv", ".smates", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox", ".nox":
			return true
		}
		if !found {
			break
		}
		rest = more
	}
	return strings.HasSuffix(p, ".pyc")
}

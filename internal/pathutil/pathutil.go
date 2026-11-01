// Package pathutil normalizes paths to repo-relative, slash-separated form and matches
// them against doublestar globs.
package pathutil

import (
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Rel converts an absolute (or file:// URL) path to a clean repo-relative slash path.
// It resolves symlinks when possible. ok is false for paths outside root.
func Rel(root, p string) (string, bool) {
	if strings.HasPrefix(p, "file://") {
		u, err := url.Parse(p)
		if err != nil {
			return "", false
		}
		p = fileURLPath(u.Path, runtime.GOOS == "windows")
	}
	if i := strings.IndexAny(p, "?#"); i >= 0 && !fileExists(p) {
		p = p[:i]
	}
	if !filepath.IsAbs(p) {
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

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
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
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "node_modules", ".git", "__pycache__", ".venv", ".smates", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox", ".nox":
			return true
		}
	}
	return strings.HasSuffix(p, ".pyc")
}

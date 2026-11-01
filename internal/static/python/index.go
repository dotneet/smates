package python

import (
	"path"
	"sort"
	"strings"

	"github.com/dotneet/smates/internal/static/host"
)

// index is the repository-wide view the analyzer builds once, on first use, from the
// file list and the Python/pytest config files. It is read-only afterwards.
type index struct {
	// roots are the repository-wide import roots ("" is the repo root), sorted.
	roots []string
	// tops maps a top-level module or package name to the roots that provide it, so
	// resolving an import only probes roots where its first component exists. This
	// keeps resolution cheap in monorepos with dozens of roots.
	tops map[string][]string
	// dirs holds every directory that contains a Python file at any depth ("" too).
	dirs map[string]bool
	// pyFiles are all Python sources, sorted (prefix queries for dynamic imports).
	pyFiles []string
	// testGlobs are the basename patterns of test modules: pytest's defaults, the
	// common Django-style names, and every python_files of the repository's configs.
	testGlobs []string
}

// defaultTestGlobs name the test modules pytest collects without configuration
// (test_*.py, *_test.py) and the ones Django-style setups add (tests.py, *_tests.py).
var defaultTestGlobs = []string{"test_*.py", "*_test.py", "tests.py", "*_tests.py"}

// projectFiles mark a directory as a Python project (and thus <dir>/src a root).
var projectFiles = map[string]bool{"pyproject.toml": true, "setup.py": true, "setup.cfg": true}

// pytestINIs are INI-style files that may carry a [pytest]/[tool:pytest] pythonpath.
var pytestINIs = map[string]bool{"pytest.ini": true, ".pytest.ini": true, "tox.ini": true, "setup.cfg": true}

func buildIndex(h host.Host, extra []string) *index {
	files := h.Files()
	rootSet := map[string]bool{"": true, "src": true}
	addRoot := func(dir, rel string) {
		if r, ok := joinRel(dir, rel); ok && !inNodeModules(r) {
			rootSet[r] = true
		}
	}
	for _, r := range extra {
		addRoot("", r)
	}
	pyFiles := make([]string, 0, len(files)/4)
	globSet := map[string]bool{}
	for _, g := range defaultTestGlobs {
		globSet[g] = true
	}
	addGlobs := func(globs []string) {
		for _, g := range globs {
			// Patterns with a directory part are matched against paths by pytest;
			// the directory heuristics of isTestLike cover them well enough.
			if !strings.Contains(g, "/") {
				globSet[g] = true
			}
		}
	}
	for f := range files {
		if inNodeModules(f) {
			continue
		}
		if isPython(f) {
			pyFiles = append(pyFiles, f) // setup.py is also a project marker
		}
		base := path.Base(f)
		if !projectFiles[base] && !pytestINIs[base] {
			continue
		}
		dir := parentDir(f)
		// A project or pytest rootdir is importable itself (flat layout, editable
		// installs, rootdir conftest insertion) and so is its src/ (src layout).
		addRoot(dir, ".")
		if projectFiles[base] {
			addRoot(dir, "src")
		}
		switch {
		case base == "pyproject.toml":
			doc := string(h.Read(f))
			for _, r := range pyprojectRoots(doc) {
				addRoot(dir, r)
			}
			addGlobs(pyprojectTestGlobs(doc))
		case pytestINIs[base]:
			doc := string(h.Read(f))
			for _, r := range iniRoots(doc, base) {
				addRoot(dir, r)
			}
			addGlobs(iniTestGlobs(doc, base))
		}
	}
	sort.Strings(pyFiles)

	idx := &index{
		tops:    map[string][]string{},
		dirs:    map[string]bool{"": true},
		pyFiles: pyFiles,
	}
	for g := range globSet {
		idx.testGlobs = append(idx.testGlobs, g)
	}
	sort.Strings(idx.testGlobs)
	for r := range rootSet {
		idx.roots = append(idx.roots, r)
	}
	sort.Strings(idx.roots)
	for _, f := range pyFiles {
		for d := parentDir(f); !idx.dirs[d]; d = parentDir(d) {
			idx.dirs[d] = true
		}
		for d := f; d != ""; {
			d = parentDir(d)
			if !rootSet[d] {
				continue
			}
			rel := f
			if d != "" {
				rel = f[len(d)+1:]
			}
			top, _, isDir := strings.Cut(rel, "/")
			if !isDir {
				top = strings.TrimSuffix(strings.TrimSuffix(top, ".pyi"), ".py")
			}
			if isIdent(top) && !contains(idx.tops[top], d) {
				idx.tops[top] = append(idx.tops[top], d)
			}
		}
	}
	return idx
}

// joinRel joins a config-relative directory onto the config file's directory and
// reports false when the result is not inside the repository.
func joinRel(dir, rel string) (string, bool) {
	rel = strings.ReplaceAll(strings.TrimSpace(rel), `\`, "/")
	if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsAny(rel, "$%~") ||
		len(rel) > 1 && rel[1] == ':' {
		return "", false
	}
	p := path.Clean(path.Join(dir, rel))
	switch {
	case p == ".":
		return "", true
	case p == ".." || strings.HasPrefix(p, "../"):
		return "", false
	}
	return p, true
}

// parentDir returns the repo-relative parent directory of p ("" for top-level paths).
func parentDir(p string) string {
	if k := strings.LastIndexByte(p, '/'); k >= 0 {
		return p[:k]
	}
	return ""
}

// join joins a repo-relative directory ("" is the root) and a name.
func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func isPython(p string) bool {
	return strings.HasSuffix(p, ".py") || strings.HasSuffix(p, ".pyi")
}

// inNodeModules reports whether p has a node_modules path segment.
func inNodeModules(p string) bool {
	return p == "node_modules" || strings.HasPrefix(p, "node_modules/") ||
		strings.Contains(p, "/node_modules/") || strings.HasSuffix(p, "/node_modules")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

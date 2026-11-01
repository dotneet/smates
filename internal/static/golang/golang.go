// Package golang resolves direct dependencies of Go source files without invoking the
// go toolchain.
//
// Go compiles whole packages, so dependencies are tracked at package granularity: a
// file depends on every other compiled file of its own package directory, on every
// compiled file of each package it imports (resolved through the repository's go.mod
// files, or through vendor/ when the module or workspace vendors its dependencies), on
// the files its //go:embed directives name, and on the go.mod/go.sum (and
// go.work/go.work.sum, vendor/modules.txt) that pin its module's dependency versions.
// The result over-approximates what `go build` would read: build constraints are not
// evaluated, so files for every GOOS/GOARCH and tag combination are included.
package golang

import (
	"bytes"
	"go/parser"
	"go/scanner"
	"go/token"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/dotneet/smates/internal/static/host"
)

// Options configure the analyzer.
type Options struct {
	// Roots are extra repo-relative GOPATH-style source roots: an import path P also
	// resolves to the directory <root>/P. Module resolution through go.mod needs none.
	Roots []string
}

// Analyzer implements host.Analyzer for Go.
type Analyzer struct {
	h     host.Host
	roots []string

	idxOnce sync.Once
	idx     *index

	treeOnce sync.Once
	tree     []string // all repo files, sorted; built lazily for //go:embed expansion

	// pkgs caches the files an import path resolves to (import path -> []string).
	pkgs sync.Map
}

// New creates an analyzer bound to a repository.
func New(h host.Host, opts Options) *Analyzer {
	a := &Analyzer{h: h}
	for _, r := range opts.Roots {
		if r, ok := cleanRel(r); ok {
			a.roots = append(a.roots, r)
		}
	}
	return a
}

// Handles implements host.Analyzer.
func (a *Analyzer) Handles(p string) bool {
	return strings.HasSuffix(p, ".go") && !excluded(p)
}

// Deps implements host.Analyzer.
func (a *Analyzer) Deps(p string, src []byte) []string {
	if !a.Handles(p) {
		return nil
	}
	ix := a.index()
	dir := dirOf(p)
	var out []string

	// The rest of the package: Go compiles the directory as a unit, and the test binary
	// also links every _test.go file of the directory (internal and external tests).
	if d := ix.dirs[dir]; d != nil {
		out = append(out, d.src...)
		if strings.HasSuffix(p, "_test.go") {
			out = append(out, d.tests...)
		}
	}

	// Dependency versions of the enclosing module and workspace, and the manifest of
	// their vendor directories.
	if m, ok := enclosing(dir, ix.modDirs); ok {
		out = append(out, joinRel(m, "go.mod"), joinRel(m, "go.sum"))
	}
	if w, ok := enclosing(dir, ix.workDirs); ok {
		out = append(out, joinRel(w, "go.work"), joinRel(w, "go.work.sum"))
	}
	for _, v := range ix.vendorDirs(dir) {
		out = append(out, joinRel(v, "modules.txt"))
	}

	if src == nil {
		src = []byte{} // go/parser would otherwise read p relative to the process cwd
	}
	for _, imp := range importPaths(p, src) {
		out = append(out, a.resolve(imp, dir)...)
	}

	if bytes.Contains(src, []byte("go:embed")) {
		for _, pat := range embedPatterns(src) {
			out = append(out, a.expandEmbed(dir, pat)...)
		}
	}

	// Tests that build or run other packages as subprocesses (exec.Command("go", "run",
	// "../../cmd/tool")) depend on those packages too: string literals naming a
	// package directory of the repository count like imports.
	if strings.HasSuffix(p, "_test.go") {
		for _, lit := range packageLiterals(src) {
			out = append(out, a.resolvePackageLiteral(lit, dir)...)
		}
	}
	return a.finish(out, p)
}

// packageLiterals returns string literals of a test file that look like package
// references: relative directories ("./cmd/x", "../../cmd/tool") or import paths
// containing a slash. go/scanner is used so comments and rune literals are skipped.
func packageLiterals(src []byte) []string {
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	s.Init(file, src, nil, 0)
	var out []string
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.STRING {
			continue
		}
		v, err := strconv.Unquote(lit)
		if err != nil || v == "" || len(v) > 256 || strings.ContainsAny(v, " \t\n*?") {
			continue
		}
		v = strings.TrimSuffix(strings.TrimSuffix(v, "/..."), "/")
		if strings.HasPrefix(v, "./") || strings.HasPrefix(v, "../") || strings.Contains(v, "/") {
			out = append(out, v)
		}
	}
	return out
}

// resolvePackageLiteral maps a package reference found in a test's string literals to
// that package's compiled files (only when it names a Go package of the repository).
func (a *Analyzer) resolvePackageLiteral(lit, fromDir string) []string {
	ix := a.index()
	if strings.HasPrefix(lit, "./") || strings.HasPrefix(lit, "../") {
		d, ok := cleanRel(joinRel(fromDir, lit))
		if !ok || ix.dirs[d] == nil || len(ix.dirs[d].src) == 0 {
			return nil
		}
		return ix.dirs[d].src
	}
	return a.resolve(lit, fromDir)
}

// finish deduplicates, drops self and anything not in the working tree, and sorts.
func (a *Analyzer) finish(in []string, self string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	seen[self] = true
	out := make([]string, 0, len(in))
	for _, f := range in {
		if seen[f] {
			continue
		}
		seen[f] = true
		if a.h.Has(f) && !excluded(f) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// importPaths returns the import paths declared by a Go source file. Files with syntax
// errors still yield the imports parsed before the error.
func importPaths(name string, src []byte) []string {
	f, _ := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly|parser.SkipObjectResolution)
	if f == nil {
		return nil
	}
	out := make([]string, 0, len(f.Imports))
	for _, s := range f.Imports {
		if s.Path == nil {
			continue
		}
		if p, err := strconv.Unquote(s.Path.Value); err == nil && p != "" {
			out = append(out, p)
		}
	}
	return out
}

// resolve maps an import path to the compiled files of the package it names, including
// third-party packages vendored into the enclosing module or workspace. The standard
// library and packages of the module cache resolve to nothing.
func (a *Analyzer) resolve(imp, fromDir string) []string {
	files := a.resolveRepo(imp, fromDir)
	if imp == "C" || strings.HasPrefix(imp, ".") {
		return files
	}
	// In vendor mode `go build` compiles vendor/<imp> for every import outside the
	// main modules, replaced modules included. Main-module packages have no copy there,
	// so the vendored package, when there is one, is simply added.
	ix := a.index()
	for _, v := range ix.vendorDirs(fromDir) {
		if vf := ix.pkgFiles(joinRel(v, imp)); len(vf) > 0 {
			files = append(append([]string{}, files...), vf...)
		}
	}
	return files
}

// resolveRepo maps an import path to the compiled files of the package of a repository
// module (or GOPATH-style root) it names.
func (a *Analyzer) resolveRepo(imp, fromDir string) []string {
	if imp == "C" { // cgo pseudo-package
		return nil
	}
	if imp == "." || imp == ".." || strings.HasPrefix(imp, "./") || strings.HasPrefix(imp, "../") {
		// GOPATH-era relative import; rejected in module mode but cheap to honor.
		d, ok := cleanRel(path.Join(fromDir, imp))
		if !ok {
			return nil
		}
		return a.index().pkgFiles(d)
	}
	if v, ok := a.pkgs.Load(imp); ok {
		return v.([]string)
	}
	files := a.resolveModule(imp)
	ix := a.index()
	for _, r := range a.roots {
		files = append(files, ix.pkgFiles(joinRel(r, imp))...)
	}
	v, _ := a.pkgs.LoadOrStore(imp, files)
	return v.([]string)
}

// resolveModule finds the module whose path is the longest prefix of imp that provides
// the package. Modules sharing a path (e.g. a fixture module in testdata, or a local
// replace target) are all consulted.
func (a *Analyzer) resolveModule(imp string) []string {
	ix := a.index()
	for q := imp; q != ""; {
		if dirs, ok := ix.modules[q]; ok {
			rest := strings.TrimPrefix(imp[len(q):], "/")
			if !hasElem(rest, "vendor") {
				var files []string
				for _, d := range dirs {
					files = append(files, ix.pkgFiles(joinRel(d, rest))...)
				}
				if len(files) > 0 {
					return files
				}
			}
		}
		i := strings.LastIndexByte(q, '/')
		if i < 0 {
			break
		}
		q = q[:i]
	}
	return nil
}

// dirOf returns the repo-relative directory of p ("" for the repository root).
func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// joinRel joins repo-relative path elements, treating "" as the repository root.
func joinRel(dir, rest string) string {
	switch {
	case dir == "":
		return rest
	case rest == "":
		return dir
	}
	return dir + "/" + rest
}

// cleanRel cleans a repo-relative path, reporting false if it escapes the repository.
func cleanRel(p string) (string, bool) {
	p = path.Clean(strings.TrimPrefix(p, "./"))
	switch {
	case p == ".":
		return "", true
	case p == "..", strings.HasPrefix(p, "../"), strings.HasPrefix(p, "/"):
		return "", false
	}
	return p, true
}

// enclosing returns the nearest directory at or above dir that is in set.
func enclosing(dir string, set map[string]bool) (string, bool) {
	for {
		if set[dir] {
			return dir, true
		}
		if dir == "" {
			return "", false
		}
		dir = dirOf(dir)
	}
}

// hasElem reports whether the slash-separated path p has an element equal to elem.
func hasElem(p, elem string) bool {
	for p != "" {
		var e string
		if i := strings.IndexByte(p, '/'); i >= 0 {
			e, p = p[:i], p[i+1:]
		} else {
			e, p = p, ""
		}
		if e == elem {
			return true
		}
	}
	return false
}

// excluded reports whether a path must never be analyzed or returned.
func excluded(p string) bool {
	return hasElem(p, "node_modules")
}

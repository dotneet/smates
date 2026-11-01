// Package python resolves direct dependencies of Python source files.
//
// Imports are found by a small tokenizer (see scan.go) rather than regular
// expressions, so docstrings, comments, multi-line and parenthesized imports, and
// imports nested in functions or try/if blocks are all handled. Module names are
// resolved against the import roots pytest and a typical environment would put on
// sys.path:
//
//  1. the repository root;
//  2. every Python project directory (pyproject.toml, setup.py, setup.cfg) and its
//     src/ directory, plus the repository's src/;
//  3. directories declared in config: pytest pythonpath (pyproject.toml, pytest.ini,
//     tox.ini, setup.cfg), setuptools/poetry/hatch package locations;
//  4. Options.Roots;
//  5. the parent of the importing file's top-level package (pytest's rootdir-based
//     "prepend" import mode);
//  6. the importing file's own directory.
//
// The analyzer is a safety net under runtime traces, so wherever the import system
// is ambiguous it over-approximates: "from a import b" yields both the module a and a
// submodule a.b if it exists, all candidate roots are used rather than the first
// match, and dynamic imports with a literal prefix yield every module under it.
package python

import (
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/dotneet/smates/internal/static/host"
)

// Analyzer implements host.Analyzer for Python.
type Analyzer struct {
	h    host.Host
	opts Options

	once sync.Once
	idx  *index
}

// New creates an analyzer bound to a repository.
func New(h host.Host, opts Options) *Analyzer { return &Analyzer{h: h, opts: opts} }

// Options configure the analyzer.
type Options struct {
	// Roots are extra repo-relative import roots (Python only).
	Roots []string
}

// Handles implements host.Analyzer.
func (a *Analyzer) Handles(p string) bool { return isPython(p) }

// Deps implements host.Analyzer. It returns the repo-relative Python files that p
// directly depends on: imported modules, the __init__.py of every package those
// imports (and p itself) live in, and for test files the conftest.py chain.
func (a *Analyzer) Deps(p string, src []byte) []string {
	if !a.Handles(p) {
		return nil
	}
	a.once.Do(func() { a.idx = buildIndex(a.h, a.opts.Roots) })
	r := &resolver{h: a.h, idx: a.idx, self: p, dir: parentDir(p), out: map[string]bool{}}
	r.ownPackage()

	sc := scan(src)
	for _, ref := range sc.refs {
		r.resolve(ref)
	}
	if base := path.Base(p); base == "__init__.py" || base == "__init__.pyi" {
		// "from pkg import *" imports the submodules listed in pkg.__all__.
		for _, name := range sc.all {
			if isIdent(name) {
				r.leaf(join(r.dir, name))
			}
		}
	}
	if a.idx.isTestLike(p) {
		r.conftests()
	}
	return r.result()
}

// resolver accumulates the dependencies of one file.
type resolver struct {
	h    host.Host
	idx  *index
	self string
	dir  string // directory of self ("" for the repo root)
	// local are the file-specific roots (5) and (6).
	local []string
	out   map[string]bool
}

// add records p if it exists, is not the file itself and is not under node_modules.
func (r *resolver) add(p string) {
	if p == r.self || inNodeModules(p) || !r.h.Has(p) {
		return
	}
	r.out[p] = true
}

func (r *resolver) result() []string {
	if len(r.out) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.out))
	for p := range r.out {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ownPackage adds the __init__.py of every package enclosing the file (importing the
// file executes them) and computes the file-specific import roots.
func (r *resolver) ownPackage() {
	d := r.dir
	for r.h.Has(join(d, "__init__.py")) {
		r.add(join(d, "__init__.py"))
		if d == "" {
			break
		}
		d = parentDir(d)
	}
	r.local = append(r.local, r.dir)
	if d != r.dir {
		r.local = append(r.local, d)
	}
}

// conftests adds conftest.py from the file's directory up to the repository root.
func (r *resolver) conftests() {
	for d := r.dir; ; d = parentDir(d) {
		r.add(join(d, "conftest.py"))
		if d == "" {
			return
		}
	}
}

// resolve adds the files a module reference may load.
func (r *resolver) resolve(m modRef) {
	if m.level > 0 && m.pkg != "" {
		// import_module(".x", package="a.b") is the absolute a.b.x.
		pkg := strings.Split(m.pkg, ".")
		if m.level-1 >= len(pkg) {
			return
		}
		name := strings.Join(pkg[:len(pkg)-(m.level-1)], ".")
		if m.name != "" || m.prefix {
			name += "." + m.name
		}
		m.level, m.name = 0, name
	}
	switch {
	case m.prefix:
		r.prefix(m)
	case m.level > 0:
		r.relative(m)
	default:
		r.absolute(m)
	}
}

// roots calls fn for every root that may provide top-level name top.
func (r *resolver) roots(top string, fn func(root string)) {
	global := r.idx.tops[top]
	for _, root := range global {
		fn(root)
	}
	for _, root := range r.local {
		if !contains(global, root) {
			fn(root)
		}
	}
}

func (r *resolver) absolute(m modRef) {
	parts := strings.Split(m.name, ".")
	if parts[0] == "__future__" {
		return
	}
	if m.every {
		for k := 1; k <= len(parts); k++ {
			r.module(parts[:k], nil)
		}
		return
	}
	r.module(parts, m.names)
}

// module adds the files of an absolute module (and of from-imported names that may
// be submodules of it) under every root that may provide it.
func (r *resolver) module(parts, names []string) {
	r.roots(parts[0], func(root string) {
		if base, ok := r.chain(root, parts); ok {
			r.names(base, names)
		}
	})
}

// relative resolves "from .x import y" against the importing file's package.
func (r *resolver) relative(m modRef) {
	base, ok := r.ancestor(m.level)
	if !ok {
		return
	}
	r.pkgInit(base)
	var parts []string
	if m.name != "" {
		parts = strings.Split(m.name, ".")
	}
	if pkg, ok := r.chain(base, parts); ok {
		r.names(pkg, m.names)
	}
}

// ancestor returns the package directory a relative reference with level dots is
// anchored at, or false if it would leave the repository.
func (r *resolver) ancestor(level int) (string, bool) {
	base := r.dir
	for k := 1; k < level; k++ {
		if base == "" {
			return "", false
		}
		base = parentDir(base)
	}
	return base, true
}

// chain adds, for module parts under root, the __init__ of every enclosing package
// and the module itself. It returns the module's path without extension, or false
// when an enclosing package directory does not exist under root.
func (r *resolver) chain(root string, parts []string) (string, bool) {
	dir := root
	for k, part := range parts {
		next := join(dir, part)
		if k == len(parts)-1 {
			r.leaf(next)
			return next, true
		}
		if !r.idx.dirs[next] {
			return "", false
		}
		r.pkgInit(next)
		dir = next
	}
	return dir, true
}

// pkgInit adds the __init__ (module or stub) of package directory dir.
func (r *resolver) pkgInit(dir string) {
	r.add(join(dir, "__init__.py"))
	r.add(join(dir, "__init__.pyi"))
}

// leaf adds the files that can implement the module at base (path without extension).
func (r *resolver) leaf(base string) {
	r.add(base + ".py")
	r.add(base + ".pyi")
	if r.idx.dirs[base] {
		r.pkgInit(base)
	}
}

// names adds from-imported names of package pkg that are submodules.
func (r *resolver) names(pkg string, names []string) {
	if len(names) == 0 || !r.idx.dirs[pkg] {
		return
	}
	for _, n := range names {
		r.leaf(join(pkg, n))
	}
}

// prefix resolves a dynamic import whose name is only known up to a static prefix,
// such as import_module(f"app.plugins.{name}"): every module under app/plugins.
func (r *resolver) prefix(m modRef) {
	pkgName, namePrefix := "", m.name
	if k := strings.LastIndexByte(m.name, '.'); k >= 0 {
		pkgName, namePrefix = m.name[:k], m.name[k+1:]
	}
	var parts []string
	if pkgName != "" {
		parts = strings.Split(pkgName, ".")
	}
	if m.level > 0 {
		base, ok := r.ancestor(m.level)
		if !ok {
			return
		}
		r.pkgInit(base)
		if dir, ok := r.chain(base, parts); ok {
			r.under(dir, namePrefix)
		}
		return
	}
	if len(parts) == 0 {
		for _, root := range r.idx.roots {
			r.under(root, namePrefix)
		}
		for _, root := range r.local {
			r.under(root, namePrefix)
		}
		return
	}
	r.roots(parts[0], func(root string) {
		if dir, ok := r.chain(root, parts); ok {
			r.under(dir, namePrefix)
		}
	})
}

// under adds every Python file in dir whose path below dir starts with namePrefix.
// An empty query (the whole repository) is refused.
func (r *resolver) under(dir, namePrefix string) {
	if !r.idx.dirs[dir] {
		return
	}
	q := join(dir, namePrefix)
	if namePrefix == "" {
		if dir == "" {
			return
		}
		q = dir + "/"
	}
	files := r.idx.pyFiles
	for i := sort.SearchStrings(files, q); i < len(files) && strings.HasPrefix(files[i], q); i++ {
		r.add(files[i])
	}
}

// isTestLike reports whether pytest would load conftest.py files for p: test modules
// (index.testGlobs), conftest.py itself, and anything under a test/ directory or one
// whose name contains "tests" (tests/, unit_tests/, tests_e2e/).
func (ix *index) isTestLike(p string) bool {
	base := path.Base(p)
	if base == "conftest.py" {
		return true
	}
	for _, g := range ix.testGlobs {
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	for _, seg := range strings.Split(parentDir(p), "/") {
		if seg == "test" || strings.Contains(seg, "tests") {
			return true
		}
	}
	return false
}

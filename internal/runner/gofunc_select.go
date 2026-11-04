package runner

// Selection for granularity: func: the static mapping of a test function and the
// go test -run patterns that run the selected ones.

import (
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/static/golang"
)

// MapStatic implements StaticMapper: a test function depends on the declarations of the
// Go files of its static closure (their bodies only when it executed them, which
// recording adds), on the declarations of the package's test files it reaches (see
// testFileDeps), and on the listing of its package directory. A compile check
// (BuildCheck) depends on the content of the whole closure, as a package does.
func (goTest) MapStatic(s *Suite, unit string, closure []string) []string {
	dir, name, ok := SplitFuncUnit(unit)
	if !funcGranularity(s) || !ok || name == BuildCheck {
		return closure
	}
	tests := testFileDeps(s.RepoRoot, dir, name, closure)
	out := make([]string, 0, len(closure)+len(tests)+1)
	for _, f := range closure {
		switch {
		case !strings.HasSuffix(f, ".go"):
			out = append(out, f)
		case strings.HasSuffix(f, "_test.go") && path.Dir(f) != dir:
			out = append(out, f) // not expected: test files of other packages are not linked
		default:
			out = append(out, depkey.Decls(f))
		}
	}
	out = append(out, tests...)
	return append(out, depkey.Listing(dir))
}

// testFileDeps returns what test function name of package directory dir depends on in
// the package's test files (among files), beyond their declarations hash (see
// golang.DeclsHash): the top-level declarations it reaches (depkey.Decl), starting
// from its own and from those that run for every test (TestMain, init functions,
// variables initialized by calls, methods of types the test files do not declare: they
// may change the method set of a type of the package), following the names each one
// mentions, and from a type to its methods (an interface may call them); the content
// of test files that do not parse.
//
// Names resolve within a package clause: an external test package (pkg_test) also
// reaches the declarations of the package's own test files through the names it uses
// unqualified (a dot import of the package) and those it selects on an imported
// package (p.Name).
func testFileDeps(root, dir, name string, files []string) []string {
	ix := testIndexFor(root, dir, files)
	out := slices.Clone(ix.broken)
	seen := map[testNode]bool{}
	var queue []testNode
	push := func(ns ...testNode) {
		for _, n := range ns {
			if !seen[n] {
				seen[n] = true
				queue = append(queue, n)
			}
		}
	}
	push(ix.seeds...)
	push(ix.tests[name]...)
	for len(queue) > 0 {
		n := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		out = append(out, depkey.Decl(n.file, n.key))
		tf := ix.infos[n.file]
		d, pkg := tf.Decls[n.key], ix.pkgs[tf.Package]
		for _, r := range d.Refs {
			push(pkg.decls[r]...)
		}
		if d.Type {
			push(pkg.methods[n.key]...)
		}
		if base, ok := strings.CutSuffix(tf.Package, "_test"); ok && ix.pkgs[base] != nil {
			for _, r := range d.Refs {
				push(ix.pkgs[base].decls[r]...)
			}
			for _, r := range d.Qualified {
				push(ix.pkgs[base].decls[r]...)
			}
		}
	}
	sort.Strings(out)
	return out
}

// testNode is a top-level declaration of a test file.
type testNode struct{ file, key string }

// testIndex is what testFileDeps needs to know about the test files of a package
// directory, the same for every test function of it.
type testIndex struct {
	sig    []testIndexFile
	infos  map[string]*golang.TestFile
	pkgs   map[string]*testPkg   // package clause -> its declarations
	tests  map[string][]testNode // test function -> its declarations
	seeds  []testNode            // what runs for every test
	broken []string              // test files that do not parse
}

// testIndexFile is one test file of a testIndex, as parsed (nil: it does not parse).
type testIndexFile struct {
	file string
	tf   *golang.TestFile
}

// testPkg indexes the declarations of the test files of one package clause.
type testPkg struct {
	decls   map[string][]testNode // key -> its declarations
	methods map[string][]testNode // receiver type -> its methods
	types   map[string]bool
}

// testIndexes caches the testIndex of a package directory ("root\x00dir") for the
// test files it was built from: selection asks once per test function.
var testIndexes sync.Map

// testIndexFor returns the testIndex of the test files of package directory dir among
// files.
func testIndexFor(root, dir string, files []string) *testIndex {
	var sig []testIndexFile
	for _, f := range files {
		if path.Dir(f) != dir || !strings.HasSuffix(f, "_test.go") {
			continue
		}
		tf, err := readTestFile(root, f) // memoized by content
		if err != nil {
			tf = nil
		}
		sig = append(sig, testIndexFile{f, tf})
	}
	key := root + "\x00" + dir
	if v, ok := testIndexes.Load(key); ok && slices.Equal(v.(*testIndex).sig, sig) {
		return v.(*testIndex)
	}
	ix := &testIndex{sig: sig, infos: map[string]*golang.TestFile{}, pkgs: map[string]*testPkg{}, tests: map[string][]testNode{}}
	for _, e := range sig {
		if e.tf == nil {
			ix.broken = append(ix.broken, e.file)
			continue
		}
		ix.infos[e.file] = e.tf
		pkg := ix.pkgs[e.tf.Package]
		if pkg == nil {
			pkg = &testPkg{decls: map[string][]testNode{}, methods: map[string][]testNode{}, types: map[string]bool{}}
			ix.pkgs[e.tf.Package] = pkg
		}
		for k, d := range e.tf.Decls {
			pkg.decls[k] = append(pkg.decls[k], testNode{e.file, k})
			if d.Recv != "" {
				pkg.methods[d.Recv] = append(pkg.methods[d.Recv], testNode{e.file, k})
			}
			if d.Type {
				pkg.types[k] = true
			}
		}
		for _, t := range e.tf.Tests {
			ix.tests[t] = append(ix.tests[t], testNode{e.file, t})
		}
	}
	for _, e := range sig {
		if e.tf == nil {
			continue
		}
		pkg := ix.pkgs[e.tf.Package]
		for k, d := range e.tf.Decls {
			if d.Init || d.Recv != "" && !pkg.types[d.Recv] {
				ix.seeds = append(ix.seeds, testNode{e.file, k})
			}
		}
	}
	testIndexes.Store(key, ix)
	return ix
}

// maxRunRegex bounds the -run pattern of one go test invocation; packages whose
// selected functions need more run as a whole.
const maxRunRegex = 16 << 10

// runRegex is the -run pattern matching exactly the given top-level test names.
func runRegex(names []string) string {
	names = slices.Clone(names)
	sort.Strings(names)
	names = slices.Compact(names)
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = regexp.QuoteMeta(n)
	}
	return "^(" + strings.Join(q, "|") + ")$"
}

// goInvocation is one go test call over package directories, restricted to run.
type goInvocation struct {
	dirs []string
	run  string // "" = every test
}

// plainInvocations groups the selected functions into go test calls: one -run pattern
// for as many packages as fit (a function of the same name in another of those
// packages runs too, which is harmless), whole packages when a package's own pattern
// is too long.
func plainInvocations(dirs []string, funcs map[string][]string) []goInvocation {
	var out []goInvocation
	var cur goInvocation
	var names []string
	flush := func() {
		if len(cur.dirs) > 0 {
			cur.run = runRegex(names)
			out = append(out, cur)
		}
		cur, names = goInvocation{}, nil
	}
	var whole []string
	for _, d := range dirs {
		if len(runRegex(funcs[d])) > maxRunRegex {
			whole = append(whole, d)
			continue
		}
		if len(cur.dirs) > 0 && len(runRegex(append(slices.Clone(names), funcs[d]...))) > maxRunRegex {
			flush()
		}
		cur.dirs = append(cur.dirs, d)
		names = append(names, funcs[d]...)
	}
	flush()
	if len(whole) > 0 {
		out = append(out, goInvocation{dirs: whole})
	}
	return out
}

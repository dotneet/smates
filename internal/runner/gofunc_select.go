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

// MapStatic implements StaticMapper: a test function depends on what it reaches in
// its own package (see funcDeps: test-file declarations, the declarations of the
// package it names, the package's shape and method sets), on the declarations of the
// Go files of the other packages of its static closure (their bodies only when it
// executed them, which recording adds), and on the non-Go files of the closure. A
// compile check (BuildCheck) depends on the content of the whole closure, as a package
// does.
func (goTest) MapStatic(s *Suite, unit string, closure []string) []string {
	dir, name, ok := SplitFuncUnit(unit)
	if !funcGranularity(s) || !ok || name == BuildCheck {
		return closure
	}
	own := funcDeps(s.RepoRoot, dir, name, closure, nil)
	out := make([]string, 0, len(closure)+len(own))
	for _, f := range closure {
		switch {
		case !strings.HasSuffix(f, ".go"):
			out = append(out, f)
		case path.Dir(f) == dir:
			// funcDeps
		case strings.HasSuffix(f, "_test.go"):
			out = append(out, f) // not expected: test files of other packages are not linked
		default:
			out = append(out, depkey.Decls(f))
		}
	}
	return append(out, own...)
}

// funcDeps returns what test function name of package directory dir depends on in its
// own package (the Go files of dir among files), given the declarations of the
// package it executed (depkey.Decl keys from coverage; none for the static view):
//
//   - the package's shape (depkey.Shape): what every test of the binary runs or that
//     changes what the package means (golang.PackageParts);
//   - test-file declarations it reaches, whole (depkey.Decl): coverage does not see
//     inside test files;
//   - declarations of the package's other files it reaches, without their bodies
//     (depkey.Sig): a changed type, constant, variable or signature it names;
//   - the method set of every type it reaches (depkey.Methods);
//   - the content of Go files of dir that do not parse.
//
// Reaching starts from its own declaration, from what runs for every test (TestMain,
// init functions and variables initialized by calls in test files; package-level
// variables initialized by calls elsewhere, whose initializers the shape holds, and
// what they name; methods test files add to types they do not declare: they may change
// the method set of a type of the package) and from the bodies it executed, and follows
// the names each declaration mentions: all of them for test-file declarations and
// executed bodies, those outside function bodies otherwise (a body that did not run
// cannot matter). A type brings its methods declared in test files. Names resolve
// within a package clause; an external test package (pkg_test) also reaches the
// package's declarations through the names it uses unqualified (a dot import) and
// those it selects on an imported package (p.Name).
func funcDeps(root, dir, name string, files, executed []string) []string {
	ix := testIndexFor(root, dir, files)
	out := slices.Clone(ix.broken)
	out = append(out, depkey.Shape(dir))
	state := map[testNode]int{} // 1: names outside bodies followed, 2: all
	var queue []testNode
	push := func(full bool, ns ...testNode) {
		for _, n := range ns {
			want := 1
			if full || ix.test[n.file] {
				want = 2
			}
			if state[n] < want {
				state[n] = want
				queue = append(queue, n)
			}
		}
	}
	push(false, ix.seeds...)
	push(true, ix.tests[name]...)
	for _, k := range executed {
		if f, key, ok := depkey.DeclName(k); ok && ix.infos[f] != nil {
			push(true, testNode{f, key})
		}
	}
	for len(queue) > 0 {
		n := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		tf := ix.infos[n.file]
		d, pkg := tf.Decls[n.key], ix.pkgs[tf.Package]
		if d == nil {
			continue
		}
		full := state[n] == 2
		names, qualified := d.Refs, d.Qualified
		if full {
			names = append(slices.Clone(names), d.BodyRefs...)
			qualified = append(slices.Clone(qualified), d.BodyQualified...)
		}
		for _, r := range names {
			push(false, pkg.decls[r]...)
		}
		if d.Type {
			push(true, pkg.testMethods[n.key]...)
		}
		if base, ok := strings.CutSuffix(tf.Package, "_test"); ok && ix.pkgs[base] != nil {
			for _, r := range names {
				push(false, ix.pkgs[base].decls[r]...)
			}
			for _, r := range qualified {
				push(false, ix.pkgs[base].decls[r]...)
			}
		}
	}
	for n, st := range state {
		d := ix.infos[n.file].Decls[n.key]
		if d == nil {
			continue
		}
		if st == 2 {
			out = append(out, depkey.Decl(n.file, n.key))
		} else {
			out = append(out, depkey.Sig(n.file, n.key))
		}
		if d.Type {
			out = append(out, depkey.Methods(dir, n.key))
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// testNode is a top-level declaration of a Go file of a package directory.
type testNode struct{ file, key string }

// testIndex is what funcDeps needs to know about the Go files of a package directory,
// the same for every test function of it.
type testIndex struct {
	sig    []testIndexFile
	infos  map[string]*golang.TestFile
	test   map[string]bool       // the _test.go files
	pkgs   map[string]*testPkg   // package clause -> its declarations
	tests  map[string][]testNode // test function -> its declarations
	seeds  []testNode            // what runs for every test
	broken []string              // Go files that do not parse
}

// testIndexFile is one Go file of a testIndex, as parsed (nil: it does not parse).
type testIndexFile struct {
	file string
	tf   *golang.TestFile
}

// testPkg indexes the declarations of the Go files of one package clause.
type testPkg struct {
	decls       map[string][]testNode // key -> its declarations
	testMethods map[string][]testNode // receiver type -> its methods declared in test files
	testTypes   map[string]bool       // types declared in test files
}

// testIndexes caches the testIndex of a package directory ("root\x00dir") for the Go
// files it was built from: selection asks once per test function.
var testIndexes sync.Map

// testIndexFor returns the testIndex of the Go files of package directory dir among
// files.
func testIndexFor(root, dir string, files []string) *testIndex {
	var sig []testIndexFile
	for _, f := range files {
		if path.Dir(f) != dir || !strings.HasSuffix(f, ".go") {
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
	ix := &testIndex{sig: sig, infos: map[string]*golang.TestFile{}, test: map[string]bool{}, pkgs: map[string]*testPkg{}, tests: map[string][]testNode{}}
	for _, e := range sig {
		if e.tf == nil {
			ix.broken = append(ix.broken, e.file)
			continue
		}
		test := strings.HasSuffix(e.file, "_test.go")
		ix.infos[e.file], ix.test[e.file] = e.tf, test
		pkg := ix.pkgs[e.tf.Package]
		if pkg == nil {
			pkg = &testPkg{decls: map[string][]testNode{}, testMethods: map[string][]testNode{}, testTypes: map[string]bool{}}
			ix.pkgs[e.tf.Package] = pkg
		}
		for k, d := range e.tf.Decls {
			pkg.decls[k] = append(pkg.decls[k], testNode{e.file, k})
			if test && d.Recv != "" {
				pkg.testMethods[d.Recv] = append(pkg.testMethods[d.Recv], testNode{e.file, k})
			}
			if test && d.Type {
				pkg.testTypes[k] = true
			}
		}
		if test {
			for _, t := range e.tf.Tests {
				ix.tests[t] = append(ix.tests[t], testNode{e.file, t})
			}
		}
	}
	for _, e := range sig {
		if e.tf == nil {
			continue
		}
		pkg := ix.pkgs[e.tf.Package]
		for k, d := range e.tf.Decls {
			if d.Init || ix.test[e.file] && d.Recv != "" && !pkg.testTypes[d.Recv] {
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

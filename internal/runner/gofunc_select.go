package runner

// Selection for granularity: func: the static mapping of a test function and the
// go test -run patterns that run the selected ones.

import (
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/static/golang"
)

// MapStatic implements StaticMapper: a test function depends on the declarations of the
// Go files of its static closure (their bodies only when it executed them, which
// recording adds), on its own test file and the test files it needs, and on the
// listing of its package directory. A compile check (BuildCheck) depends on the
// content of the whole closure, as a package does.
func (goTest) MapStatic(s *Suite, unit string, closure []string) []string {
	dir, name, ok := SplitFuncUnit(unit)
	if !funcGranularity(s) || !ok || name == BuildCheck {
		return closure
	}
	content := testFileDeps(s.RepoRoot, dir, name, closure)
	out := make([]string, 0, len(closure)+1)
	for _, f := range closure {
		switch {
		case !strings.HasSuffix(f, ".go"), content[f]:
			out = append(out, f)
		case strings.HasSuffix(f, "_test.go") && path.Dir(f) != dir:
			out = append(out, f) // not expected: test files of other packages are not linked
		default:
			out = append(out, depkey.Decls(f))
		}
	}
	return append(out, depkey.Listing(dir))
}

// testFileDeps returns the test files of package directory dir (among files) whose
// content test function name depends on: the file declaring it, files running code for
// every test, and, transitively, files declaring a name one of those mentions. Files
// that do not parse count too.
func testFileDeps(root, dir, name string, files []string) map[string]bool {
	infos := map[string]*golang.TestFile{}
	need := map[string]bool{}
	var queue []string
	for _, f := range files {
		if path.Dir(f) != dir || !strings.HasSuffix(f, "_test.go") {
			continue
		}
		tf, err := readTestFile(root, f)
		if err != nil {
			need[f] = true
			continue
		}
		infos[f] = tf
		if tf.Global || slices.Contains(tf.Tests, name) {
			need[f] = true
			queue = append(queue, f)
		}
	}
	for len(queue) > 0 {
		f := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		uses := infos[f].Uses
		for g, tf := range infos {
			if need[g] {
				continue
			}
			for _, d := range tf.Decls {
				if uses[d] {
					need[g] = true
					queue = append(queue, g)
					break
				}
			}
		}
	}
	return need
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

package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/state"
)

// sweepFixture is a directory of n source files (dir/fK.go) and n test files.
func sweepFixture(dir string, n int) (files []string, tests map[string]bool) {
	tests = map[string]bool{}
	for i := range n {
		files = append(files, fmt.Sprintf("%s/f%d.go", dir, i), fmt.Sprintf("%s/f%d_test.go", dir, i))
		tests[fmt.Sprintf("%s/f%d_test.go", dir, i)] = true
	}
	return files, tests
}

// deps is a dependency map holding the first n source files of dir as whole files, plus
// key variants that must not count as such.
func sweepDeps(dir string, n int, keyOf func(string) string) map[string]string {
	deps := map[string]string{"go.mod": "x", dir + "/": "x", "$HOME": "x"}
	for i := range n {
		deps[keyOf(fmt.Sprintf("%s/f%d.go", dir, i))] = "x"
	}
	return deps
}

func TestSourceSweeps(t *testing.T) {
	whole := func(f string) string { return f }
	decl := func(f string) string { return f + "#decl:Run" }
	sig := func(f string) string { return f + "#sig:Run" }
	for name, c := range map[string]struct {
		dir              string
		files            int // source files in dir
		sweepers, others int // units depending on read files of dir, and units that do not
		read             int // files each sweeper depends on
		keyOf            func(string) string
		want             bool
	}{
		"every file":                   {"pkg", 12, 6, 2, 12, whole, true},
		"at the share":                 {"pkg", 20, 6, 2, 18, whole, true},
		"below the share":              {"pkg", 20, 6, 2, 17, whole, false},
		"too few files":                {"pkg", 9, 6, 2, 9, whole, false},
		"too few units":                {"pkg", 12, 4, 0, 12, whole, false},
		"too small a share of units":   {"pkg", 12, 5, 30, 12, whole, false},
		"declarations are not files":   {"pkg", 12, 6, 2, 12, decl, false},
		"signatures are not files":     {"pkg", 12, 6, 2, 12, sig, false},
		"repository root":              {".", 12, 6, 2, 12, whole, true},
		"nested directory":             {"a/b/c", 12, 6, 2, 12, whole, true},
		"dependencies in node_modules": {"web/node_modules/lib", 12, 6, 2, 12, whole, false},
	} {
		t.Run(name, func(t *testing.T) {
			files, tests := sweepFixture(c.dir, c.files)
			st := state.New()
			su := st.Suite("go")
			for i := range c.sweepers {
				su.Tests[fmt.Sprintf("%s#TestS%d", c.dir, i)] = &state.Entry{Deps: sweepDeps(c.dir, c.read, c.keyOf)}
			}
			for i := range c.others {
				su.Tests[fmt.Sprintf("%s#TestO%d", c.dir, i)] = &state.Entry{Deps: map[string]string{"go.mod": "x"}}
			}
			got := sourceSweeps(st, files, tests, map[string]bool{"go": true})
			if !c.want {
				if len(got) != 0 {
					t.Errorf("got %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %+v, want one sweep", got)
			}
			sw := got[0]
			if sw.Dir != c.dir || sw.Files != c.files || sw.Units != c.sweepers || sw.Total != c.sweepers+c.others {
				t.Errorf("got %+v", sw)
			}
			msg := sw.message()
			for _, want := range []string{fmt.Sprintf("%d of %d test units", c.sweepers, c.sweepers+c.others), "per declaration", sw.Example} {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q lacks %q", msg, want)
				}
			}
		})
	}
}

// Test files are not counted as the directory's sources: units reading every source
// file sweep it although they read none of its tests. Compile checks, which depend on
// their package's whole closure by design, neither count as sweeping units nor as units
// of the suite. Suites that track files whole (not Go per-function units) are left out:
// importing most modules of a package looks the same there.
func TestSourceSweepsSuites(t *testing.T) {
	files, tests := sweepFixture("pkg", 12)
	all := func(f string) string { return f }
	st := state.New()
	for i := range 5 {
		st.Suite("a").Tests[fmt.Sprintf("pkg#Test%d", i)] = &state.Entry{Deps: sweepDeps("pkg", 12, all)}
		st.Suite("files").Tests[fmt.Sprintf("t%d", i)] = &state.Entry{Deps: sweepDeps("pkg", 12, all)}
	}
	for i := range 4 {
		st.Suite("checks").Tests[fmt.Sprintf("p%d#(build)", i)] = &state.Entry{Deps: sweepDeps("pkg", 12, all)}
		st.Suite("checks").Tests[fmt.Sprintf("p%d#TestA", i)] = &state.Entry{Deps: sweepDeps("pkg", 12, all)}
	}
	got := sourceSweeps(st, files, tests, map[string]bool{"a": true, "checks": true})
	if len(got) != 1 || got[0].Suite != "a" || got[0].Files != 12 || got[0].Units != 5 || got[0].Total != 5 {
		t.Fatalf("got %+v", got)
	}
	if strings.Contains(got[0].message(), "(build)") {
		t.Errorf("message names a compile check: %s", got[0].message())
	}
}

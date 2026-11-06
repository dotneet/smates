package selector

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
)

// largeGoFixture is a synthetic Go workspace: module big with n packages, each with
// six source files and two test files, importing a few earlier packages and a handful
// of hub packages most of the tree depends on, and module small with two packages.
func largeGoFixture(tb testing.TB, n int) *fixture {
	files := map[string]string{
		"big/go.mod":   "module example.com/big\n\ngo 1.22\n",
		"small/go.mod": "module example.com/small\n\ngo 1.22\n",
	}
	r := rand.New(rand.NewSource(1))
	const hubs = 8
	for i := 0; i < n; i++ {
		var imports []string
		switch {
		case i < hubs:
			for j := 0; j < i; j++ {
				imports = append(imports, goPkg(j))
			}
		default:
			for k := 0; k < 3; k++ {
				imports = append(imports, goPkg(r.Intn(hubs)))
			}
			for k := 0; k < 6; k++ {
				imports = append(imports, goPkg(hubs+r.Intn(i-hubs+1)))
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "package p%d\n\nimport (\n", i)
		seen := map[string]bool{}
		for _, imp := range imports {
			if imp == goPkg(i) || seen[imp] {
				continue
			}
			seen[imp] = true
			fmt.Fprintf(&b, "\t_ \"example.com/big/%s\"\n", imp)
		}
		b.WriteString(")\n\nfunc F() int { return 1 }\n")
		dir := "big/" + goPkg(i)
		files[dir+"/a.go"] = b.String()
		files[dir+"/b.go"] = fmt.Sprintf("package p%d\n\nfunc G() int { return 2 }\n", i)
		for k := 0; k < 4; k++ {
			files[fmt.Sprintf("%s/c%d.go", dir, k)] = fmt.Sprintf("package p%d\n\nfunc H%d() int { return %d }\n", i, k, k)
		}
		files[dir+"/a_test.go"] = fmt.Sprintf("package p%d\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { F() }\n", i)
		files[dir+"/b_test.go"] = fmt.Sprintf("package p%d_test\n\nimport (\n\t\"testing\"\n\t_ \"example.com/big/%s\"\n)\n\nfunc TestB(t *testing.T) {}\n", i, goPkg(r.Intn(hubs)))
	}
	files["small/s/s.go"] = "package s\n"
	files["small/s/s_test.go"] = "package s\n"
	files["small/t/t.go"] = "package t\n\nimport _ \"example.com/small/s\"\n"
	files["small/t/t_test.go"] = "package t\n"
	f := newFixture(tb, files)
	mk := func(name string) *runner.Suite {
		a, _ := runner.Get("go")
		return &runner.Suite{Suite: config.Suite{Name: name, Runner: "go", Root: name, Tests: []string{"**/*_test.go"}, Inputs: []string{"go.mod"}}, Adapter: a}
	}
	f.suites = []*runner.Suite{mk("big"), mk("small")}
	return f
}

func goPkg(i int) string { return fmt.Sprintf("pkg/p%04d", i) }

// benchSelect times Select over the large Go workspace after one source file of a
// mid-tree package changed, in state mode; scope picks the suites (nil: all).
func benchSelect(b *testing.B, n int, scope func(f *fixture, in *Input)) {
	f := largeGoFixture(b, n)
	st := f.record(nil, nil)
	f.write("big/"+goPkg(n/2)+"/b.go", "package p\n\nfunc G() int { return 3 }\n")
	snap := f.snap()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		in := &Input{Config: f.cfg, Suites: f.suites, State: st, Snap: snap, Graph: f.graph()}
		if scope != nil {
			scope(f, in)
		}
		if _, err := Select(in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSelectLargeGo(b *testing.B) {
	benchSelect(b, 1500, nil)
}

// BenchmarkSelectLargeGoSuite selects the small module only: the big one is left out
// and only adds to the coverage.
func BenchmarkSelectLargeGoSuite(b *testing.B) {
	benchSelect(b, 1500, func(f *fixture, in *Input) {
		in.Suites, in.Others = f.suites[1:], f.suites[:1]
	})
}

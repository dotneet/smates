package static

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// naiveClosure is the reference: a depth-first walk over every edge.
func naiveClosure(direct func(string) []string, start []string) []string {
	seen := map[string]bool{}
	stack := append([]string{}, start...)
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[p] {
			continue
		}
		seen[p] = true
		stack = append(stack, direct(p)...)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// randomGraph returns a random dependency function over n nodes, with cycles (among
// them self edges and large components) and nodes outside files (named "x/..."), and
// its subgraph with some edges removed (as erasing type-only imports does, which can
// split a component).
func randomGraph(r *rand.Rand, n int) (files map[string]bool, full, sub map[string][]string) {
	name := func(i int) string {
		if i%7 == 3 {
			return fmt.Sprintf("x/n%03d", i) // not a file of the tree
		}
		return fmt.Sprintf("n%03d", i)
	}
	files = map[string]bool{}
	full, sub = map[string][]string{}, map[string][]string{}
	for i := 0; i < n; i++ {
		if i%7 != 3 {
			files[name(i)] = true
		}
		k := r.Intn(4)
		if r.Intn(10) == 0 {
			k += 10 // a hub
		}
		for j := 0; j < k; j++ {
			var t int
			switch r.Intn(3) {
			case 0:
				t = r.Intn(n) // anywhere: cycles
			case 1:
				t = i // self edge
			default:
				t = r.Intn(i + 1) // backwards: a DAG part
			}
			full[name(i)] = append(full[name(i)], name(t))
			if r.Intn(4) != 0 {
				sub[name(i)] = append(sub[name(i)], name(t))
			}
		}
	}
	return files, full, sub
}

func TestCloserMatchesNaive(t *testing.T) {
	for seed := int64(0); seed < 200; seed++ {
		r := rand.New(rand.NewSource(seed))
		n := 1 + r.Intn(120)
		files, full, sub := randomGraph(r, n)
		for gi, g := range []map[string][]string{full, sub} {
			direct := func(p string) []string { return g[p] }
			c := newCloser(files, direct)
			// Several queries per closer: later ones reuse the components.
			for q := 0; q < 8; q++ {
				var start []string
				for k := r.Intn(4); k >= 0; k-- {
					start = append(start, fmt.Sprintf("n%03d", r.Intn(n+3))) // may name no node
				}
				if r.Intn(5) == 0 {
					start = append(start, "x/n003", "zzz-missing") // outside files
				}
				if q == 0 && r.Intn(4) == 0 {
					start = nil
				}
				got, want := c.closure(start), naiveClosure(direct, start)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("seed %d graph %d start %v:\n got %v\nwant %v", seed, gi, start, got, want)
				}
			}
		}
	}
}

// TestCloserResultIsCallers checks that a caller changing a closure does not change
// the next one.
func TestCloserResultIsCallers(t *testing.T) {
	g := map[string][]string{"a": {"b"}, "b": {"a", "c"}}
	c := newCloser(map[string]bool{"a": true, "b": true, "c": true}, func(p string) []string { return g[p] })
	first := c.closure([]string{"a"})
	first[0] = "changed"
	_ = append(first[:1], "appended")
	if got := c.closure([]string{"a"}); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("got %v", got)
	}
}

// TestCloserDeepChain checks that a long import chain does not need a deep call stack.
func TestCloserDeepChain(t *testing.T) {
	const n = 200000
	files := map[string]bool{}
	for i := 0; i < n; i++ {
		files[fmt.Sprintf("f%06d", i)] = true
	}
	direct := func(p string) []string {
		var i int
		fmt.Sscanf(p, "f%06d", &i)
		if i+1 == n {
			return []string{"f000000"} // and one cycle through all of them
		}
		return []string{fmt.Sprintf("f%06d", i+1)}
	}
	if got := newCloser(files, direct).closure([]string{"f000000"}); len(got) != n {
		t.Errorf("closure of %d files, want %d", len(got), n)
	}
}

// TestGraphClosures checks Closure and RuntimeClosure of a Graph over real sources
// against the naive walk: a type-only import cycle exists in Closure only.
func TestGraphClosures(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.ts":      "import { b } from './b'\nimport type { T } from './t'\n",
		"b.ts":      "import { a } from './a'\nexport const b = 1\n",
		"t.ts":      "import { c } from './c'\nexport type T = number\n",
		"c.ts":      "import type { T } from './t'\nexport const c = 1\n",
		"a.test.ts": "import { a } from './a'\n",
		"go.mod":    "module example.com/m\n\ngo 1.22\n",
		"p/p.go":    "package p\n\nimport _ \"example.com/m/q\"\n",
		"p/p2.go":   "package p\n",
		"q/q.go":    "package q\n",
	}
	var list []string
	for p, c := range files {
		abs := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
		list = append(list, p)
	}
	g := New(dir, list, Options{})
	g.Warm([]string{"a.test.ts", "p/p.go"})
	for _, start := range [][]string{{"a.test.ts"}, {"t.ts"}, {"c.ts"}, {"p/p.go"}, {"a.ts", "p/p2.go"}, {"missing.ts"}} {
		if got, want := g.Closure(start...), naiveClosure(g.Direct, start); !reflect.DeepEqual(got, want) {
			t.Errorf("Closure(%v) = %v, want %v", start, got, want)
		}
		if got, want := g.RuntimeClosure(start...), naiveClosure(g.RuntimeDirect, start); !reflect.DeepEqual(got, want) {
			t.Errorf("RuntimeClosure(%v) = %v, want %v", start, got, want)
		}
	}
	if got := g.RuntimeClosure("a.test.ts"); reflect.DeepEqual(got, g.Closure("a.test.ts")) {
		t.Errorf("type-only imports must be left out of the runtime closure: %v", got)
	}
}

// BenchmarkCloser times closures of many overlapping start sets over a random graph
// with large components.
func BenchmarkCloser(b *testing.B) {
	r := rand.New(rand.NewSource(1))
	files, full, _ := randomGraph(r, 5000)
	direct := func(p string) []string { return full[p] }
	var starts [][]string
	for i := 0; i < 500; i++ {
		starts = append(starts, []string{fmt.Sprintf("n%03d", r.Intn(5000))})
	}
	b.Run("naive", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for _, s := range starts {
				naiveClosure(direct, s)
			}
		}
	})
	b.Run("condensed", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			c := newCloser(files, direct)
			for _, s := range starts {
				c.closure(s)
			}
		}
	})
}

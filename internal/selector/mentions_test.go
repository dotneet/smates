package selector

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/runner"
)

// TestNameScannerMatchesContains checks the one-pass search against strings.Contains
// for each name: overlapping names, names sharing their first bytes, a name at the
// very end of the text, short names and repeated ones.
func TestNameScannerMatchesContains(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	alphabet := "ab._/"
	word := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteByte(alphabet[r.Intn(len(alphabet))])
		}
		return b.String()
	}
	for round := 0; round < 2000; round++ {
		var names []string
		for k := r.Intn(8); k >= 0; k-- {
			names = append(names, word(1+r.Intn(7)))
		}
		if r.Intn(4) == 0 {
			names = append(names, names[0]) // the same name for two orphans
		}
		x := newNameScanner(names)
		for q := 0; q < 4; q++ {
			text := word(r.Intn(40))
			var want []int
			for i, n := range names {
				if strings.Contains(text, n) {
					want = append(want, i)
				}
			}
			if got := x.find(text); !reflect.DeepEqual(got, want) {
				t.Fatalf("names %q text %q: got %v want %v", names, text, got, want)
			}
		}
	}
}

// BenchmarkMentions searches the dependencies of many tests for thousands of orphan
// names, as after a change touching most of a large tree.
func BenchmarkMentions(b *testing.B) {
	const files, orphans = 2000, 5000
	all := map[string]string{}
	for i := 0; i < files; i++ {
		all[fmt.Sprintf("src/m%04d.py", i)] = strings.Repeat(fmt.Sprintf("import m%04d\nload('data/record_%05d.json')\n", (i*7)%files, i*3), 40)
	}
	f := newFixture(b, all)
	g := f.graph()
	s := &Suite{Name: "py", Suite: &runner.Suite{}, Deps: map[string][]string{}}
	for i := 0; i < files; i += 4 {
		id := fmt.Sprintf("src/m%04d.py", i)
		s.Tests = append(s.Tests, &Test{ID: id})
		for j := 0; j < 50; j++ {
			s.Deps[id] = append(s.Deps[id], fmt.Sprintf("src/m%04d.py", (i+j*37)%files))
		}
	}
	r := &Result{Suites: []*Suite{s}}
	var names []string
	for i := 0; i < orphans; i++ {
		names = append(names, fmt.Sprintf("data/record_%05d.json", i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Mentions(g, names)
	}
}

package cli

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/selector"
)

// outputMap parses name=value output lines.
func outputMap(t *testing.T, lines []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, l := range lines {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			t.Fatalf("not an output line: %q", l)
		}
		out[k] = v
	}
	return out
}

// TestGitHubOutputsSuites: the suites output lists every suite of the selection, and
// says skip only for a suite that selected nothing.
func TestGitHubOutputsSuites(t *testing.T) {
	mk := func(name string, selected ...bool) *selector.Suite {
		s := &selector.Suite{Name: name, All: len(selected) > 0}
		for i, sel := range selected {
			s.Tests = append(s.Tests, &selector.Test{ID: fmt.Sprintf("%s/t%d", name, i), Selected: sel})
			s.All = s.All && sel
		}
		return s
	}
	cases := []struct {
		name   string
		suites []*selector.Suite
		shard  [2]int
		want   map[string]suiteOutput
		skip   string
	}{
		{
			name:   "nothing selected",
			suites: []*selector.Suite{mk("api", false, false), mk("web", false)},
			want:   map[string]suiteOutput{"api": {Total: 2, Skip: true}, "web": {Total: 1, Skip: true}},
			skip:   "true",
		},
		{
			name:   "one suite selected",
			suites: []*selector.Suite{mk("api", true, false, true), mk("web", false, false)},
			want:   map[string]suiteOutput{"api": {Selected: 2, Total: 3}, "web": {Total: 2, Skip: true}},
			skip:   "false",
		},
		{
			name:   "every test of a suite",
			suites: []*selector.Suite{mk("api", true, true), mk("web", false)},
			want:   map[string]suiteOutput{"api": {Selected: 2, Total: 2, All: true}, "web": {Total: 1, Skip: true}},
			skip:   "false",
		},
		{
			// A suite without tests runs nothing: not "all", and skippable.
			name:   "suite without tests",
			suites: []*selector.Suite{{Name: "empty", All: true}, mk("web", true)},
			want:   map[string]suiteOutput{"empty": {Skip: true}, "web": {Selected: 1, Total: 1, All: true}},
			skip:   "false",
		},
		{
			// Sharded: the counts are this shard's, never "all".
			name:   "sharded",
			suites: []*selector.Suite{mk("api", true, true, true, true, true, true)},
			shard:  [2]int{1, 2},
			skip:   "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := &selector.Result{Suites: c.suites}
			applyShard(res, c.shard[0], c.shard[1])
			out := outputMap(t, githubOutputs(&selection{res: res}))
			var got map[string]suiteOutput
			if err := json.Unmarshal([]byte(out["suites"]), &got); err != nil {
				t.Fatalf("suites=%s: %v", out["suites"], err)
			}
			// Every suite of the selection is listed, and nothing else.
			var names []string
			for _, s := range c.suites {
				names = append(names, s.Name)
			}
			for n := range got {
				if !slices.Contains(names, n) {
					t.Errorf("suites lists %q, which the selection does not have", n)
				}
			}
			for _, s := range c.suites {
				g, ok := got[s.Name]
				if !ok {
					t.Errorf("suites leaves out %q: %s", s.Name, out["suites"])
					continue
				}
				if g.Skip != (g.Selected == 0) || g.Selected != len(s.Selected()) || g.Total != len(s.Tests) {
					t.Errorf("%s: %+v (selected %d of %d)", s.Name, g, len(s.Selected()), len(s.Tests))
				}
			}
			if c.want != nil && !reflect.DeepEqual(got, c.want) {
				t.Errorf("suites = %+v, want %+v", got, c.want)
			}
			if c.shard[1] > 1 && got["api"].All {
				t.Errorf("a shard's suite is never all: %+v", got["api"])
			}
			if c.skip != "" && out["skip"] != c.skip {
				t.Errorf("skip=%s, want %s", out["skip"], c.skip)
			}
			var tests map[string][]string
			if err := json.Unmarshal([]byte(out["tests"]), &tests); err != nil || len(tests) != len(c.suites) {
				t.Errorf("tests=%s: %v", out["tests"], err)
			}
		})
	}
}

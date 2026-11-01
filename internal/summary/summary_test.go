package summary

import (
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/agent"
	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/selector"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

func suite(name, run string, tests ...*selector.Test) *selector.Suite {
	return &selector.Suite{Name: name, Suite: &runner.Suite{Suite: config.Suite{Name: name, Runner: run}}, Tests: tests}
}

func sel(id, reason string, changed map[string]string, ms int64) *selector.Test {
	t := &selector.Test{ID: id, Selected: true, Reason: reason, Duration: ms}
	for _, c := range slices.Sorted(maps.Keys(changed)) {
		t.Changed = append(t.Changed, c)
	}
	if len(changed) > 0 {
		t.Via = changed
	}
	return t
}

func skip(id string, ms int64) *selector.Test {
	return &selector.Test{ID: id, Reason: selector.ReasonUnaffected, Duration: ms}
}

// prResult is a typical pull request: edits reaching tests through imports, traced
// reads, a suite input, a directory listing and an untraced command, plus orphans
// handled each way.
func prResult() *selector.Result {
	web := suite("web", "vitest",
		sel("web/src/math.test.ts", selector.ReasonChanged, map[string]string{"web/src/math.ts": selector.ViaImport, "web/src/math.test.ts": selector.ViaTest}, 1200),
		sel("web/src/stats.test.ts", selector.ReasonChanged, map[string]string{"web/src/math.ts": selector.ViaImport}, 800),
		sel("web/src/report.test.ts", selector.ReasonChanged, map[string]string{"web/fixtures/report.json": selector.ViaTraced, "web/fixtures/": selector.ViaTraced}, 2500),
		sel("web/src/db.test.ts", selector.ReasonReference, map[string]string{"db/schema.sql": selector.ViaReference}, 4000),
		&selector.Test{ID: "web/src/flaky.test.ts", Selected: true, Reason: selector.ReasonFailed, Detail: "last run: fail", Duration: 300},
		&selector.Test{ID: "web/src/new.test.ts", Selected: true, Reason: selector.ReasonNew, Detail: "no recorded run"},
		skip("web/src/ui.test.ts", 65_000),
		skip("web/src/api.test.ts", 4_200),
		skip("web/src/untimed.test.ts", 0),
	)
	web.Tests[3].Detail = "references db/schema.sql (in web/src/db.test.ts)"
	api := suite("api", "go",
		sel("api/server", selector.ReasonChanged, map[string]string{"scripts/gen/**": selector.ViaTraced}, 9000),
		&selector.Test{ID: "api/client", Selected: true, Reason: selector.ReasonAgent, Detail: "agent: @octocat says <b>client</b> reads the generated | table"},
		skip("api/store", 120_000),
	)
	py := suite("py", "pytest", skip("py/tests/test_a.py", 500), skip("py/tests/test_b.py", 500))
	return &selector.Result{
		Mode:    "state",
		Suites:  []*selector.Suite{web, api, py},
		Changed: []string{"Makefile", "db/schema.sql", "docs/notes.md", "scripts/gen/a.go", "scripts/gen/b.go", "web/fixtures/new.json", "web/fixtures/report.json", "web/src/math.test.ts", "web/src/math.ts"},
		Orphans: []string{"Makefile", "db/schema.sql", "docs/notes.md"},
		OrphanHandling: map[string]string{
			"Makefile":      selector.OrphanAgent,
			"db/schema.sql": selector.OrphanReference,
			"docs/notes.md": selector.OrphanNoImpact,
		},
	}
}

func TestGolden(t *testing.T) {
	cases := map[string]func() *Input{
		"pr": func() *Input {
			return &Input{Result: prResult(), Agent: &agent.Decision{Model: "claude-haiku-5-5", Summary: "Makefile only changes the lint target;\nthe client reads generated code."}}
		},
		"pr-run-failed": func() *Input {
			return &Input{Result: prResult(), Run: &Run{ExitCode: 1, Elapsed: 83 * time.Second, Failed: []string{"web: web/src/math.test.ts"}},
				Sections: []Section{{Title: "Flaky tests", Body: "- `web/src/flaky.test.ts` (2 of the last 5 runs)"}, {Title: "Failure hints", Body: "`web/src/math.ts` changed in this PR.", Collapsed: true}}}
		},
		"shadow": func() *Input {
			r := prResult()
			r.Orphans, r.OrphanHandling = nil, nil
			return &Input{Result: r, Run: &Run{ExitCode: 1, Elapsed: 4 * time.Minute, Shadow: true,
				Failed:   []string{"web: web/src/math.test.ts", "py: py/tests/test_a.py"},
				Escapes:  []string{"py: py/tests/test_a.py"},
				Possible: []string{"api: api/store"}}}
		},
		"policy-and-ignore": func() *Input {
			r := &selector.Result{Mode: "state",
				Suites: []*selector.Suite{
					suite("web", "vitest", sel("web/a.test.ts", selector.ReasonOrphan, map[string]string{"web/vite.config.ts": selector.ViaOrphan}, 100),
						sel("web/b.test.ts", selector.ReasonOrphan, map[string]string{"web/vite.config.ts": selector.ViaOrphan}, 100),
						sel("web/c.test.ts", selector.ReasonOrphan, map[string]string{"web/vite.config.ts": selector.ViaOrphan}, 100),
						sel("web/d.test.ts", selector.ReasonOrphan, map[string]string{"web/vite.config.ts": selector.ViaOrphan}, 100)),
					suite("api", "go", skip("api/x", 2000)),
				},
				Changed:        []string{"README.md", "web/vite.config.ts"},
				Orphans:        []string{"README.md", "web/vite.config.ts"},
				OrphanHandling: map[string]string{"README.md": selector.OrphanIgnored, "web/vite.config.ts": selector.OrphanPolicy},
				Notes:          []string{"ignored 1 unattributed change(s) (orphans: ignore)"},
			}
			return &Input{Result: r, AgentNote: "agent unavailable (no API key); running affected suites in full"}
		},
		"nothing": func() *Input {
			return &Input{Result: &selector.Result{Mode: "state", Suites: []*selector.Suite{suite("web", "vitest", skip("web/a.test.ts", 1500), skip("web/b.test.ts", 0))}, Changed: []string{"web/README.md"}}}
		},
		"full": func() *Input {
			r := &selector.Result{Mode: "all", Suites: []*selector.Suite{suite("web", "vitest",
				&selector.Test{ID: "web/a.test.ts", Selected: true, Reason: selector.ReasonAll},
				&selector.Test{ID: "web/b.test.ts", Selected: true, Reason: selector.ReasonAll})},
				Notes: []string{"no recorded state and no base revision: running everything"}}
			return &Input{Result: r, Run: &Run{Elapsed: 1500 * time.Millisecond}}
		},
		"diff": func() *Input {
			r := &selector.Result{Mode: "diff", Suites: []*selector.Suite{suite("py", "pytest",
				sel("tests/test_a.py", selector.ReasonDiff, map[string]string{"src/a.py": selector.ViaImport, "pyproject.toml": selector.ViaInput}, 0),
				skip("tests/test_b.py", 0))}, Changed: []string{"pyproject.toml", "src/a.py"}}
			return &Input{Result: r}
		},
		"shard": func() *Input {
			r := prResult()
			for i, s := range r.Suites {
				for j, tt := range s.Tests {
					tt.Shard = (i+j)%2 + 1
					if tt.Shard != 2 {
						tt.Selected = false
					}
				}
			}
			return &Input{Result: r, Shard: 2, Shards: 2}
		},
		"large": func() *Input { return &Input{Result: largeResult()} },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			got := Markdown(mk())
			if got != Markdown(mk()) {
				t.Fatal("output is not deterministic")
			}
			golden := filepath.Join("testdata", name+".md")
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("mismatch with %s (run with -update to accept):\n%s", golden, got)
			}
		})
	}
}

// largeResult has many changed files, each selecting many tests, and a lockfile that
// every test of a suite depends on.
func largeResult() *selector.Result {
	var tests []*selector.Test
	var changed []string
	for f := 0; f < 40; f++ {
		changed = append(changed, fmt.Sprintf("src/mod%02d.ts", f))
	}
	changed = append(changed, "package-lock.json")
	for i := 0; i < 400; i++ {
		via := map[string]string{"package-lock.json": selector.ViaInput}
		for f := 0; f < 40; f++ {
			if i%(f+2) == 0 {
				via[fmt.Sprintf("src/mod%02d.ts", f)] = selector.ViaImport
			}
		}
		tests = append(tests, sel(fmt.Sprintf("test/t%03d.test.ts", i), selector.ReasonChanged, via, int64(i*10)))
	}
	var orphans []string
	handling := map[string]string{}
	for i := 0; i < 25; i++ {
		o := fmt.Sprintf("docs/page%02d.md", i)
		orphans = append(orphans, o)
		handling[o] = selector.OrphanNoImpact
	}
	return &selector.Result{Mode: "state", Suites: []*selector.Suite{suite("web", "jest", tests...), suite("other", "go", skip("other/pkg", 3_700_000))},
		Changed: changed, Orphans: orphans, OrphanHandling: handling}
}

func TestLargeIsBounded(t *testing.T) {
	out := Markdown(&Input{Result: largeResult()})
	if len(out) > 20_000 {
		t.Errorf("summary of a large selection is %d bytes", len(out))
	}
	if !strings.Contains(out, "… and 11 more changed files") {
		t.Errorf("changed files are not truncated:\n%s", out)
	}
}

func TestText(t *testing.T) {
	if got := text("hi @team\n<script>"); got != "hi @​team &lt;script&gt;" {
		t.Errorf("text: %q", got)
	}
	if got := code("a`b"); got != "`` a`b ``" {
		t.Errorf("code: %q", got)
	}
	for ms, want := range map[int64]string{0: "<0.1s", 49: "<0.1s", 50: "0.1s", 450: "0.5s", 1000: "1s", 59_999: "59s", 61_000: "1m 1s", 3_660_000: "1h 1m"} {
		if got := dur(ms); got != want {
			t.Errorf("dur(%d) = %q, want %q", ms, got, want)
		}
	}
}

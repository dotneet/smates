package selector

import (
	"reflect"
	"slices"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// coverageCase is a selection over several suites whose coverage must not depend on
// which of them --suite picks.
type coverageCase struct {
	name string
	run  func(t *testing.T) (*fixture, *state.State)
}

func coverageCases() []coverageCase {
	return []coverageCase{
		{"polyglot", func(t *testing.T) (*fixture, *state.State) {
			f := polyglotFixture(t)
			st := polyglotState(t, f)
			f.write("svc/core/core.go", "package core\n\nfunc Core() int { return 2 }\n")
			f.write("Dockerfile", "FROM y\n")
			return f, st
		}},
		{"polyglot no state", func(t *testing.T) (*fixture, *state.State) {
			return polyglotFixture(t), state.New()
		}},
		{"type-only imports", func(t *testing.T) (*fixture, *state.State) {
			// types.ts is reached through type-only imports only: covered, no dependency.
			f := typeOnlyFixture(t, config.Suite{Runner: "vitest", Command: "vitest run"}, map[string]string{
				"api/test_a.py": "import shared\n", "api/shared.py": "",
			})
			f.suites = append(f.suites, suite(t, config.Suite{Name: "py", Runner: "pytest", Root: "api", Tests: []string{"test_*.py"}}))
			st := f.record(nil, nil)
			f.write("web/src/types.ts", `export type User = { id: number }`)
			return f, st
		}},
		{"vendored file in a closure", func(t *testing.T) (*fixture, *state.State) {
			// A committed dependency a test imports: never a dependency, so never
			// covered by that test (the suite's closures are covered unit by unit).
			f := newFixture(t, map[string]string{
				"web/a.test.js":    `require("./.venv/lib.js"); require("./a.js")`,
				"web/a.js":         `module.exports = 1`,
				"web/b.test.js":    `require("./b.js")`,
				"web/b.js":         `module.exports = 2`,
				"web/.venv/lib.js": `module.exports = 3`,
				"api/test_a.py":    "",
			})
			f.suites = []*runner.Suite{
				suite(t, config.Suite{Name: "web", Runner: "vitest", Root: "web", Tests: []string{"*.test.js"}}),
				suite(t, config.Suite{Name: "py", Runner: "pytest", Root: "api", Tests: []string{"test_*.py"}}),
			}
			st := f.record(nil, nil)
			f.write("web/b.js", `module.exports = 4`)
			return f, st
		}},
		{"go functions", func(t *testing.T) (*fixture, *state.State) {
			// A runner that maps the closure to keys of its own (Go test functions).
			f := newFixture(t, map[string]string{
				"go.mod":        "module example.com/m\n\ngo 1.22\n",
				"p/a.go":        "package p\n\nfunc A() int { return 1 }\n",
				"p/a_test.go":   "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { A() }\n",
				"q/q.go":        "package q\n\nimport _ \"example.com/m/p\"\n",
				"q/q_test.go":   "package q\n\nimport \"testing\"\n\nfunc TestQ(t *testing.T) {}\n",
				"api/test_a.py": "",
			})
			g := suite(t, config.Suite{Runner: "go", Tests: []string{"**/*_test.go"}, Granularity: config.GranularityFunc})
			g.RepoRoot = f.dir
			f.suites = []*runner.Suite{g, suite(t, config.Suite{Name: "py", Runner: "pytest", Root: "api", Tests: []string{"test_*.py"}})}
			st := f.record(nil, nil)
			f.write("p/a.go", "package p\n\nfunc A() int { return 2 }\n")
			return f, st
		}},
		{"reaches", func(t *testing.T) (*fixture, *state.State) {
			return reachesCoverage(t, "")
		}},
		{"reaches, go functions", func(t *testing.T) (*fixture, *state.State) {
			return reachesCoverage(t, config.GranularityFunc)
		}},
		{"reaches, a runner that keeps the closure", func(t *testing.T) (*fixture, *state.State) {
			// The closures of a suite left out are covered as their union, but the
			// reaches of a rule still need each unit's closure.
			f := newFixture(t, map[string]string{
				"web/a.test.js":               `require("./kit.js")`,
				"web/b.test.js":               `require("./b.js")`,
				"web/kit.js":                  `module.exports = 1`,
				"web/b.js":                    `module.exports = 2`,
				migration:                     "create table t()",
				"database/fixtures/users.yml": "- id: 1",
				"api/test_a.py":               "",
			})
			f.suites = []*runner.Suite{
				suite(t, config.Suite{Name: "web", Runner: "vitest", Root: "web", Tests: []string{"*.test.js"}, Rules: reachesRules("web/kit.js", "web/nothing.js")}),
				suite(t, config.Suite{Name: "py", Runner: "pytest", Root: "api", Tests: []string{"test_*.py"}}),
			}
			st := f.record(nil, nil)
			f.write(migration, "create table t(id int)")
			f.write("database/fixtures/users.yml", "- id: 2")
			return f, st
		}},
	}
}

// reachesRules are a rule whose reaches some units match (its deps, the migrations,
// are covered) and one whose reaches no unit matches (its deps, the fixtures, stay
// orphans). A suite left out by --suite must tell them apart too, rather than give the
// deps of both to units whose closure it did not compute.
func reachesRules(reached, unreached string) []config.Rule {
	return []config.Rule{
		{Reaches: []string{reached}, Deps: []string{"database/migrations/**"}},
		{Tests: []string{"**"}, Reaches: []string{unreached}, Deps: []string{"database/fixtures/**"}},
	}
}

// reachesCoverage is reachesFixture with reachesRules, both deps changed.
func reachesCoverage(t *testing.T, granularity string) (*fixture, *state.State) {
	f := reachesFixture(t, granularity, reachesRules("pkg/shared/testkit/**", "pkg/shared/testkits/**")...)
	f.suites = f.suites[:1] // the shell suite: no analyzer reads it, so the rules apply
	f.suites = append(f.suites, suite(t, config.Suite{Name: "py", Runner: "pytest", Root: "api", Tests: []string{"test_*.py"}}))
	f.write("api/test_a.py", "")
	st := reachesRecord(f, nil)
	f.write(migration, "create table t(id int)")
	f.write("database/fixtures/users.yml", "- id: 2")
	return f, st
}

// TestSelectCoverageOfSuitesLeftOut checks that the suites --suite leaves out cover
// exactly what they cover when they are selected: a change one of them depends on
// is neither an orphan nor left out, whichever suites the run picks.
func TestSelectCoverageOfSuitesLeftOut(t *testing.T) {
	for _, c := range coverageCases() {
		t.Run(c.name, func(t *testing.T) {
			f, st := c.run(t)
			full := f.sel(st, nil)
			for i := range f.suites {
				picked := f.suites[i : i+1]
				others := append(slices.Clone(f.suites[:i]), f.suites[i+1:]...)
				res := f.sel(st, func(in *Input) { in.Suites, in.Others = picked, others })
				if !reflect.DeepEqual(sortedKeys(res.Covered), sortedKeys(full.Covered)) {
					t.Errorf("--suite %s covers\n%v\nwant\n%v", picked[0].Name, sortedKeys(res.Covered), sortedKeys(full.Covered))
				}
				if len(res.declared) != len(full.declared) {
					t.Errorf("--suite %s declared %d, want %d", picked[0].Name, len(res.declared), len(full.declared))
				}
				// The decisions of the picked suite are those of the full run.
				want := full.SuiteByName(picked[0].Name)
				got := res.SuiteByName(picked[0].Name)
				if !reflect.DeepEqual(got.Tests, want.Tests) || !reflect.DeepEqual(got.Deps, want.Deps) || !reflect.DeepEqual(got.Known, want.Known) {
					t.Errorf("--suite %s decides differently", picked[0].Name)
				}
			}
		})
	}
}

// TestSelectCoverageMustKeep pins what a suite left out by --suite still explains,
// with its closures covered as one union and, when a test imports a vendored file
// (never a dependency), unit by unit.
func TestSelectCoverageMustKeep(t *testing.T) {
	cases := []struct {
		name, file string
		// orphan: the change stays an orphan (or left out) of a --suite py run.
		orphan bool
	}{
		{"static dependency", "web/src/cart.ts", false},
		{"type-only import", "web/src/types.ts", false},
		{"recorded read", "web/fixtures/users.json", false},
		{"own test file", "web/src/user.test.ts", false},
		{"vendored file a test imports", "web/src/.venv/lib.js", true},
		{"file no test depends on", "web/src/other.ts", true},
	}
	for _, vendored := range []bool{false, true} {
		extra := map[string]string{"web/src/.venv/lib.js": `module.exports = 3`, "api/test_a.py": ""}
		if vendored {
			extra["web/src/vendor.test.ts"] = `import "./.venv/lib.js"`
		}
		f := typeOnlyFixture(t, config.Suite{Runner: "vitest", Command: "vitest run"}, extra)
		f.suites = append(f.suites, suite(t, config.Suite{Name: "py", Runner: "pytest", Root: "api", Tests: []string{"test_*.py"}}))
		st := f.record(map[string][]string{"web/src/user.test.ts": {"web/fixtures/users.json"}}, nil)
		res := f.sel(st, func(in *Input) { in.Suites, in.Others = f.suites[1:], f.suites[:1] })
		for _, c := range cases {
			if got := res.Explains(c.file, false); got == c.orphan {
				t.Errorf("vendored import %v: %s: Explains(%s) = %v", vendored, c.name, c.file, got)
			}
		}
	}
}

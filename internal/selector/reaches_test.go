package selector

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

const migration = "database/migrations/001.sql"

// reachesFixture is a Go module whose tests reach a database helper directly
// (pkg/app/repo), through another helper (pkg/app/svc), only from a helper test file of
// their package (pkg/app/split), only through an environment variable they read
// (pkg/app/envdb, see reachesRecord), or not at all (pkg/app/pure); plus shell tests
// no analyzer reads.
func reachesFixture(t *testing.T, granularity string, rules ...config.Rule) *fixture {
	t.Helper()
	test := func(pkg, name, helper string) string {
		src := "package " + pkg + "\n\n"
		if helper != "" {
			src += "import (\n\t\"testing\"\n\n\t_ \"example.com/m/pkg/shared/" + helper + "\"\n)\n\n"
		} else {
			src += "import \"testing\"\n\n"
		}
		return src + "func " + name + "(t *testing.T) {}\n"
	}
	f := newFixture(t, map[string]string{
		"go.mod":                          "module example.com/m\n\ngo 1.22\n",
		"pkg/shared/testkit/testkit.go":   "package testkit\n",
		"pkg/shared/dbhelper/dbhelper.go": "package dbhelper\n\nimport _ \"example.com/m/pkg/shared/testkit\"\n",
		"pkg/app/repo/repo_test.go":       test("repo", "TestRepo", "testkit"),
		"pkg/app/svc/svc_test.go":         test("svc", "TestSvc", "dbhelper"),
		"pkg/app/split/a_test.go":         test("split", "TestSplit", ""),
		"pkg/app/split/helpers_test.go":   "package split\n\nimport _ \"example.com/m/pkg/shared/testkit\"\n",
		"pkg/app/envdb/envdb_test.go":     test("envdb", "TestEnvDB", ""),
		"pkg/app/pure/pure_test.go":       test("pure", "TestPure", ""),
		migration:                         "create table t()",
		"database/fixtures/users.yml":     "- id: 1",
		"scripts/check.sh":                "echo ok",
		"scripts/test/smoke.test.sh":      "./check.sh",
	})
	g := suite(t, config.Suite{Name: "go", Runner: "go", Tests: []string{"**/*_test.go"}, Granularity: granularity, Rules: rules})
	g.RepoRoot = f.dir
	sh := suite(t, config.Suite{Name: "sh", Runner: "custom", Root: "scripts", Tests: []string{"test/*.test.sh"}, Command: "sh {tests}", Rules: rules})
	f.suites = []*runner.Suite{g, sh}
	return f
}

// reachesRecord records every unit twice, the second time over the first state, so
// that a rule with a "$NAME" entry sees what each unit read (the first time no unit has
// an entry, and every unit gets the rule's deps): pkg/app/envdb reads APP_DB_DSN.
func reachesRecord(f *fixture, mut func(*Input)) *state.State {
	f.t.Helper()
	var st *state.State
	for range 2 {
		prev := st
		if prev == nil {
			prev = state.New()
		}
		res := f.sel(prev, func(in *Input) {
			in.All = true
			if mut != nil {
				mut(in)
			}
		})
		st = state.New()
		h := NewHasher(f.snap()).WithConfig(f.cfg)
		for _, sr := range res.Suites {
			obs := map[string]*runner.Observation{}
			for _, tt := range sr.Tests {
				o := &runner.Observation{Files: map[string]bool{}, Status: state.Pass}
				if dir, _, _ := runner.SplitFuncUnit(tt.ID); tt.ID == "pkg/app/envdb" || dir == "pkg/app/envdb" {
					o.Files["$APP_DB_DSN"] = true
				}
				obs[tt.ID] = o
			}
			Record(st, sr, obs, h.ForSuite(sr.Suite), RecordOptions{Commit: "c", Now: time.Unix(1, 0), Full: true})
		}
		st.Files = map[string]string(f.snap())
	}
	return st
}

// withMigration returns the units (build checks left out) whose known dependencies
// include the migration.
func withMigration(res *Result) []string {
	var out []string
	for _, sr := range res.Suites {
		for u, known := range sr.Known {
			if !runner.IsBuildCheck(u) && slices.Contains(known, migration) {
				out = append(out, u)
			}
		}
	}
	slices.Sort(out)
	return out
}

// A rule with reaches gives its deps to the units whose file-level static closure
// reaches a file its globs match, or whose recorded run read a variable it names; and,
// failing toward running more, to every unit it cannot tell about.
func TestRuleReaches(t *testing.T) {
	deps := []string{"database/migrations/**"}
	kit := []string{"pkg/shared/testkit/**"}
	db := []string{"pkg/app/repo", "pkg/app/split", "pkg/app/svc"}
	every := []string{"pkg/app/envdb", "pkg/app/pure", "pkg/app/repo", "pkg/app/split", "pkg/app/svc", "scripts/test/smoke.test.sh"}
	funcs := func(units ...string) []string {
		var out []string
		for _, u := range units {
			if u == "scripts/test/smoke.test.sh" {
				out = append(out, u)
				continue
			}
			name := map[string]string{"pkg/app/envdb": "TestEnvDB", "pkg/app/pure": "TestPure", "pkg/app/repo": "TestRepo", "pkg/app/split": "TestSplit", "pkg/app/svc": "TestSvc"}[u]
			out = append(out, runner.FuncUnit(u, name))
		}
		slices.Sort(out)
		return out
	}
	plus := func(a []string, b ...string) []string {
		out := append(slices.Clone(a), b...)
		slices.Sort(out)
		return out
	}
	const sh = "scripts/test/smoke.test.sh"
	for _, c := range []struct {
		name        string
		granularity string
		rule        config.Rule
		static      bool
		noGraph     bool
		goOnly      bool // leave the shell suite out
		want        []string
	}{
		// Narrowing: the units that reach the helper (directly, through another helper,
		// from a helper test file of the package) get the deps; the rest do not.
		{name: "file glob", rule: config.Rule{Reaches: kit, Deps: deps}, static: true, want: plus(db, sh)},
		{name: "file glob, func granularity", granularity: config.GranularityFunc, rule: config.Rule{Reaches: kit, Deps: deps}, static: true, want: funcs(append(db, sh)...)},
		// Must keep: a unit that reads the variable without importing the helper.
		{name: "file glob or variable", rule: config.Rule{Reaches: append(slices.Clone(kit), "$APP_DB_*"), Deps: deps}, static: true, want: plus(db, "pkg/app/envdb", sh)},
		{name: "variable only", rule: config.Rule{Reaches: []string{"$APP_DB_DSN"}, Deps: deps}, static: true, want: []string{"pkg/app/envdb"}},
		{name: "variable, func granularity", granularity: config.GranularityFunc, rule: config.Rule{Reaches: []string{"$APP_DB_DSN"}, Deps: deps}, static: true, want: funcs("pkg/app/envdb")},
		// Must keep: no closure to tell from, so every unit gets the deps.
		{name: "static off", rule: config.Rule{Reaches: kit, Deps: deps}, static: false, want: every},
		{name: "no graph", rule: config.Rule{Reaches: kit, Deps: deps}, static: true, noGraph: true, want: every},
		// tests and reaches: both must match.
		{name: "tests and reaches", rule: config.Rule{Tests: []string{"pkg/app/repo", "pkg/app/pure"}, Reaches: kit, Deps: deps}, static: true, want: []string{"pkg/app/repo"}},
		{name: "tests only", rule: config.Rule{Tests: []string{"pkg/app/pure"}, Deps: deps}, static: true, want: []string{"pkg/app/pure"}},
		// A glob that names nothing the tests import: no unit (doctor warns), and a
		// change to the deps stays an orphan.
		{name: "typo", rule: config.Rule{Reaches: []string{"pkg/shared/testkits/**"}, Deps: deps}, static: true, goOnly: true, want: nil},
		{name: "typo, a unit no analyzer reads", rule: config.Rule{Reaches: []string{"pkg/shared/testkits/**"}, Deps: deps}, static: true, want: []string{sh}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := reachesFixture(t, c.granularity, c.rule)
			if c.goOnly {
				f.suites = f.suites[:1]
			}
			for _, su := range f.suites {
				su.Static = &c.static
			}
			mut := func(in *Input) {
				if c.noGraph {
					in.Graph = nil
				}
			}
			st := reachesRecord(f, mut)
			res := f.sel(st, mut)
			if got := withMigration(res); !reflect.DeepEqual(got, c.want) {
				t.Errorf("units with the rule's deps: %v, want %v", got, c.want)
			}
			// Nothing changed: no unit is selected (the rule does not feed on what it
			// recorded).
			for _, sr := range res.Suites {
				for _, tt := range sr.Tests {
					if tt.Selected {
						t.Errorf("nothing changed, but %s is selected (%s: %v)", tt.ID, tt.Reason, tt.Changed)
					}
				}
			}
			// The migration changes: exactly the units with the deps run; with none,
			// it is an orphan.
			f.write(migration, "create table t(id int)")
			res = f.sel(st, mut)
			var selected []string
			for _, sr := range res.Suites {
				selected = append(selected, sr.Selected()...)
			}
			slices.Sort(selected)
			if !reflect.DeepEqual(selected, c.want) {
				t.Errorf("selected on a migration change: %v, want %v", selected, c.want)
			}
			if orphan := slices.Contains(res.Orphans, migration); orphan != (len(c.want) == 0) {
				t.Errorf("orphans %v (units with the deps: %v)", res.Orphans, c.want)
			}
		})
	}
}

// Before a unit has run, a "$NAME" entry cannot tell whether it reads the variable:
// it gets the deps.
func TestRuleReachesUnrecorded(t *testing.T) {
	f := reachesFixture(t, "", config.Rule{Reaches: []string{"$APP_DB_DSN"}, Deps: []string{"database/migrations/**"}})
	want := []string{"pkg/app/envdb", "pkg/app/pure", "pkg/app/repo", "pkg/app/split", "pkg/app/svc", "scripts/test/smoke.test.sh"}
	if got := withMigration(f.sel(state.New(), func(in *Input) { in.All = true })); !reflect.DeepEqual(got, want) {
		t.Errorf("units with the rule's deps: %v, want %v", got, want)
	}
}

// Compile checks of Go test functions get no rules, reaches or not.
func TestRuleReachesBuildCheck(t *testing.T) {
	f := reachesFixture(t, config.GranularityFunc, config.Rule{Reaches: []string{"pkg/shared/testkit/**"}, Deps: []string{"database/migrations/**"}})
	res := f.sel(reachesRecord(f, nil), nil)
	for u, known := range res.SuiteByName("go").Known {
		if runner.IsBuildCheck(u) && slices.Contains(known, migration) {
			t.Errorf("%s depends on the migration", u)
		}
	}
}

func TestRuleReachCounts(t *testing.T) {
	f := reachesFixture(t, "",
		config.Rule{Tests: []string{"**"}, Deps: []string{"database/fixtures/**"}},
		config.Rule{Reaches: []string{"pkg/shared/testkit/**"}, Deps: []string{"database/migrations/**"}},
		config.Rule{Reaches: []string{"pkg/shared/nothing/**"}, Deps: []string{"database/migrations/**"}},
	)
	st := reachesRecord(f, nil)
	if got, want := RuleReach(f.suites[0], slices.Sorted(maps.Keys(f.files)), f.graph(), st.Suites["go"]), map[int]int{1: 3, 2: 0}; !reflect.DeepEqual(got, want) {
		t.Errorf("RuleReach: %v, want %v", got, want)
	}
	// Without static analysis it cannot tell: every unit.
	if got, want := RuleReach(f.suites[0], slices.Sorted(maps.Keys(f.files)), nil, st.Suites["go"]), map[int]int{1: 5, 2: 5}; !reflect.DeepEqual(got, want) {
		t.Errorf("RuleReach without a graph: %v, want %v", got, want)
	}
}

// An empty reaches leaves the settings hash as an older smates computed it (no rerun
// of every test after an upgrade); setting it changes the hash, like any rule edit.
func TestConfigHashReaches(t *testing.T) {
	c := &config.Config{}
	s := &config.Suite{Name: "go", Runner: "go", Rules: []config.Rule{{Tests: []string{"**"}, Deps: []string{"m/**"}}}}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Reaches") {
		t.Errorf("an empty reaches is hashed: %s", data)
	}
	before := ConfigHash(c, s)
	s.Rules[0].Reaches = []string{"pkg/shared/testkit/**"}
	if ConfigHash(c, s) == before {
		t.Error("reaches is not hashed")
	}
}

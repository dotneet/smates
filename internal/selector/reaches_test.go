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

const migration = "db/migrations/001.sql"

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
			src += "import (\n\t\"testing\"\n\n\t_ \"example.com/m/internal/" + helper + "\"\n)\n\n"
		} else {
			src += "import \"testing\"\n\n"
		}
		return src + "func " + name + "(t *testing.T) {}\n"
	}
	f := newFixture(t, map[string]string{
		"go.mod":                        "module example.com/m\n\ngo 1.22\n",
		"internal/testdb/testdb.go":     "package testdb\n",
		"internal/dbhelper/dbhelper.go": "package dbhelper\n\nimport _ \"example.com/m/internal/testdb\"\n",
		"pkg/app/repo/repo_test.go":     test("repo", "TestRepo", "testdb"),
		"pkg/app/svc/svc_test.go":       test("svc", "TestSvc", "dbhelper"),
		"pkg/app/split/a_test.go":       test("split", "TestSplit", ""),
		"pkg/app/split/helpers_test.go": "package split\n\nimport _ \"example.com/m/internal/testdb\"\n",
		"pkg/app/envdb/envdb_test.go":   test("envdb", "TestEnvDB", ""),
		"pkg/app/pure/pure_test.go":     test("pure", "TestPure", ""),
		migration:                       "create table t()",
		"db/seeds/users.yml":            "- id: 1",
		"scripts/check.sh":              "echo ok",
		"scripts/test/smoke.test.sh":    "./check.sh",
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
	deps := []string{"db/migrations/**"}
	kit := []string{"internal/testdb/**"}
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
		// Must keep the shell test: no analyzer reads it, and shell reads are not traced.
		{name: "variable only", rule: config.Rule{Reaches: []string{"$APP_DB_DSN"}, Deps: deps}, static: true, want: []string{"pkg/app/envdb", sh}},
		{name: "variable, func granularity", granularity: config.GranularityFunc, rule: config.Rule{Reaches: []string{"$APP_DB_DSN"}, Deps: deps}, static: true, want: funcs("pkg/app/envdb", sh)},
		{name: "variable, Go only", rule: config.Rule{Reaches: []string{"$APP_DB_DSN"}, Deps: deps}, static: true, goOnly: true, want: []string{"pkg/app/envdb"}},
		// Must keep: no source to read, so every unit of every runner gets the deps.
		{name: "variable, static off", rule: config.Rule{Reaches: []string{"$APP_DB_DSN"}, Deps: deps}, static: false, want: every},
		// Must keep: no closure to tell from, so every unit gets the deps.
		{name: "static off", rule: config.Rule{Reaches: kit, Deps: deps}, static: false, want: every},
		{name: "no graph", rule: config.Rule{Reaches: kit, Deps: deps}, static: true, noGraph: true, want: every},
		// tests and reaches: both must match.
		{name: "tests and reaches", rule: config.Rule{Tests: []string{"pkg/app/repo", "pkg/app/pure"}, Reaches: kit, Deps: deps}, static: true, want: []string{"pkg/app/repo"}},
		{name: "tests only", rule: config.Rule{Tests: []string{"pkg/app/pure"}, Deps: deps}, static: true, want: []string{"pkg/app/pure"}},
		// A glob that names nothing the tests import: no unit (doctor warns), and a
		// change to the deps stays an orphan.
		{name: "typo", rule: config.Rule{Reaches: []string{"internal/testdbs/**"}, Deps: deps}, static: true, goOnly: true, want: nil},
		{name: "typo, a unit no analyzer reads", rule: config.Rule{Reaches: []string{"internal/testdbs/**"}, Deps: deps}, static: true, want: []string{sh}},
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

// A "$NAME" reaches cannot tell from a recorded entry alone, whatever the runner: shell,
// Bun and in-browser reads are not traced, Go's test log starts in m.Run, and settings
// loaders that scan the environment are not counted as reads. Without source to read
// (static analysis off, no analyzer for the files) every unit gets the deps; with
// source, a unit that names the variable, or a prefix of it in a string literal (a
// loader given a prefix), does.
func TestRuleReachesVariableSources(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.22\n",
		// Go: a TestMain loading settings by prefix before m.Run, a plain test.
		"gopre/main_test.go":    "package gopre\n\nimport (\n\t\"os\"\n\t\"testing\"\n\n\t\"github.com/kelseyhightower/envconfig\"\n)\n\ntype cfg struct {\n\tDSN string `envconfig:\"DB_DSN\"`\n}\n\nfunc TestMain(m *testing.M) {\n\tvar c cfg\n\tenvconfig.MustProcess(\"APP\", &c)\n\tos.Exit(m.Run())\n}\n\nfunc TestPre(t *testing.T) {}\n",
		"goplain/plain_test.go": "package goplain\n\nimport \"testing\"\n\nfunc TestPlain(t *testing.T) {}\n",
		// Bun reads through Bun.env (not traced).
		"web/package.json":    `{"name":"web"}`,
		"web/db.test.ts":      "import { test } from 'bun:test'\ntest('db', () => { Bun.env.APP_DB_DSN })\n",
		"web/plain.test.ts":   "import { test } from 'bun:test'\ntest('plain', () => {})\n",
		"py/test_settings.py": "from pydantic_settings import BaseSettings, SettingsConfigDict\n\nclass S(BaseSettings):\n    model_config = SettingsConfigDict(env_prefix='APP_')\n    db_dsn: str = ''\n\ndef test_settings():\n    S()\n",
		"py/test_plain.py":    "def test_plain():\n    pass\n",
		"node/scan.test.js":   "const test = require('node:test')\nconst env = Object.fromEntries(Object.entries(process.env).filter(([k]) => k.startsWith('APP_DB_')))\ntest('scan', () => env)\n",
		"node/plain.test.js":  "const test = require('node:test')\ntest('plain', () => {})\n",
		"sh/smoke.test.sh":    "psql \"$APP_DB_DSN\" -c 'select 1'\n",
		migration:             "create table t()",
	}
	rule := config.Rule{Reaches: []string{"$APP_DB_DSN"}, Deps: []string{"db/migrations/**"}}
	all := []string{"gopre", "goplain", "node/plain.test.js", "node/scan.test.js", "py/test_plain.py", "py/test_settings.py", "sh/smoke.test.sh", "web/db.test.ts", "web/plain.test.ts"}
	slices.Sort(all)
	for _, c := range []struct {
		name    string
		static  bool
		noGraph bool
		want    []string
	}{
		// Must keep: nothing to read from.
		{name: "static off", static: false, want: all},
		{name: "no graph", static: true, noGraph: true, want: all},
		// Narrowing by the source; must keep the units naming the variable or a
		// prefix of it, and the shell test no analyzer reads.
		{name: "static on", static: true, want: []string{"gopre", "node/scan.test.js", "py/test_settings.py", "sh/smoke.test.sh", "web/db.test.ts"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, files)
			mk := func(cs config.Suite) *runner.Suite {
				cs.Rules, cs.Static = []config.Rule{rule}, &c.static
				su := suite(t, cs)
				su.RepoRoot = f.dir
				return su
			}
			f.suites = []*runner.Suite{
				mk(config.Suite{Name: "go", Runner: "go", Tests: []string{"**/*_test.go"}}),
				mk(config.Suite{Name: "bun", Runner: "bun", Root: "web", Tests: []string{"**/*.test.ts"}}),
				mk(config.Suite{Name: "py", Runner: "pytest", Root: "py", Tests: []string{"**/test_*.py"}}),
				mk(config.Suite{Name: "node", Runner: "node", Root: "node", Tests: []string{"**/*.test.js"}}),
				mk(config.Suite{Name: "sh", Runner: "custom", Root: "sh", Tests: []string{"*.test.sh"}, Command: "sh {tests}"}),
			}
			mut := func(in *Input) {
				if c.noGraph {
					in.Graph = nil
				}
			}
			st := reachesRecord(f, mut)
			res := f.sel(st, mut)
			var got []string
			for _, sr := range res.Suites {
				for u, known := range sr.Known {
					if slices.Contains(known, migration) {
						got = append(got, u)
					}
				}
			}
			slices.Sort(got)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("units with the rule's deps: %v, want %v", got, c.want)
			}
		})
	}
}

// Before a unit has run, a "$NAME" entry cannot tell whether it reads the variable:
// it gets the deps.
func TestRuleReachesUnrecorded(t *testing.T) {
	f := reachesFixture(t, "", config.Rule{Reaches: []string{"$APP_DB_DSN"}, Deps: []string{"db/migrations/**"}})
	want := []string{"pkg/app/envdb", "pkg/app/pure", "pkg/app/repo", "pkg/app/split", "pkg/app/svc", "scripts/test/smoke.test.sh"}
	if got := withMigration(f.sel(state.New(), func(in *Input) { in.All = true })); !reflect.DeepEqual(got, want) {
		t.Errorf("units with the rule's deps: %v, want %v", got, want)
	}
}

// Compile checks of Go test functions get no rules, reaches or not.
func TestRuleReachesBuildCheck(t *testing.T) {
	f := reachesFixture(t, config.GranularityFunc, config.Rule{Reaches: []string{"internal/testdb/**"}, Deps: []string{"db/migrations/**"}})
	res := f.sel(reachesRecord(f, nil), nil)
	for u, known := range res.SuiteByName("go").Known {
		if runner.IsBuildCheck(u) && slices.Contains(known, migration) {
			t.Errorf("%s depends on the migration", u)
		}
	}
}

func TestRuleReachCounts(t *testing.T) {
	f := reachesFixture(t, "",
		config.Rule{Tests: []string{"**"}, Deps: []string{"db/seeds/**"}},
		config.Rule{Reaches: []string{"internal/testdb/**"}, Deps: []string{"db/migrations/**"}},
		config.Rule{Reaches: []string{"internal/nothing/**"}, Deps: []string{"db/migrations/**"}},
	)
	st := reachesRecord(f, nil)
	if got, want := RuleReach(nil, f.suites[0], slices.Sorted(maps.Keys(f.files)), f.graph(), st.Suites["go"]), map[int]int{1: 3, 2: 0}; !reflect.DeepEqual(got, want) {
		t.Errorf("RuleReach: %v, want %v", got, want)
	}
	// Without static analysis it cannot tell: every unit.
	if got, want := RuleReach(nil, f.suites[0], slices.Sorted(maps.Keys(f.files)), nil, st.Suites["go"]), map[int]int{1: 5, 2: 5}; !reflect.DeepEqual(got, want) {
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
	s.Rules[0].Reaches = []string{"internal/testdb/**"}
	if ConfigHash(c, s) == before {
		t.Error("reaches is not hashed")
	}
}

// mentionsEnv: the variable's name as a word, or a prefix of it (any case) in a string
// literal, as settings loaders given a prefix take it.
func TestMentionsEnv(t *testing.T) {
	for _, c := range []struct {
		name     string
		patterns []string
		src      string
		want     bool
	}{
		{"the name", []string{"APP_DB_DSN"}, `os.Getenv("APP_DB_DSN")`, true},
		{"the name as an identifier", []string{"APP_DB_DSN"}, `process.env.APP_DB_DSN`, true},
		{"a glob", []string{"APP_DB_*"}, `os.environ["APP_DB_HOST"]`, true},
		// Must keep: a prefix the loader completes.
		{"envconfig prefix", []string{"APP_DB_DSN"}, "envconfig.MustProcess(\"APP\", &c)", true},
		{"viper prefix, lower case", []string{"APP_DB_DSN"}, `v.SetEnvPrefix("app")`, true},
		{"pydantic env_prefix", []string{"APP_DB_DSN"}, `env_prefix='APP_'`, true},
		{"a longer prefix", []string{"APP_DB_DSN"}, `scan("APP_DB")`, true},
		{"a prefix of a glob", []string{"APP_DB_*"}, "k.startsWith(`APP`)", true},
		// Not a mention.
		{"another variable", []string{"APP_DB_DSN"}, `os.Getenv("APP_CACHE_URL")`, false},
		{"a prefix outside a string literal", []string{"APP_DB_DSN"}, `app := newApp()`, false},
		{"a word that is no prefix", []string{"APP_DB_DSN"}, `t.Run("application", f)`, false},
		{"a pattern with no literal start", []string{"*_DSN"}, `prefix("APP")`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := mentionsEnv(c.patterns, []byte(c.src)); got != c.want {
				t.Errorf("mentionsEnv(%v, %q) = %v, want %v", c.patterns, c.src, got, c.want)
			}
		})
	}
}

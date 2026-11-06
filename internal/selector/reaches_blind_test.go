package selector

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// What a "$NAME" entry of reaches cannot see in a recorded entry: Go's test log starts
// in m.Run, so reads in TestMain before it, in init and in package initializers are
// never recorded, nor is a scan of os.Environ; and a variable env_ignore (or the
// default list) keeps out of every entry. The rule must keep applying to such units.
func TestRuleReachesEnvBlindSpots(t *testing.T) {
	deps := []string{"database/migrations/**"}
	every := []string{"pkg/app/envdb", "pkg/app/pure", "pkg/app/repo", "pkg/app/split", "pkg/app/svc"}
	for _, c := range []struct {
		name    string
		extra   map[string]string
		reaches []string
		ignore  []string // env_ignore
		track   []string // env_track
		want    []string // go units with the rule's deps
	}{
		{name: "a read in TestMain before m.Run",
			extra:   map[string]string{"pkg/app/mainenv/main_test.go": "package mainenv\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) {\n\t_ = os.Getenv(\"APP_DB_DSN\")\n\tos.Exit(m.Run())\n}\n\nfunc TestX(t *testing.T) {}\n"},
			reaches: []string{"$APP_DB_*"}, want: []string{"pkg/app/envdb", "pkg/app/mainenv"}},
		{name: "a read in init",
			extra:   map[string]string{"pkg/app/initenv/x_test.go": "package initenv\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc init() { _ = os.Getenv(\"APP_DB_PORT\") }\n\nfunc TestX(t *testing.T) {}\n"},
			reaches: []string{"$APP_DB_*"}, want: []string{"pkg/app/envdb", "pkg/app/initenv"}},
		{name: "a read in a package-level initializer of a helper",
			extra: map[string]string{
				"pkg/shared/dbconf/dbconf.go": "package dbconf\n\nimport \"os\"\n\nvar DSN = os.Getenv(\"APP_DB_HOST\")\n",
				"pkg/app/varenv/x_test.go":    "package varenv\n\nimport (\n\t\"testing\"\n\n\t_ \"example.com/m/pkg/shared/dbconf\"\n)\n\nfunc TestX(t *testing.T) {}\n",
			},
			reaches: []string{"$APP_DB_*"}, want: []string{"pkg/app/envdb", "pkg/app/varenv"}},
		{name: "a scan of os.Environ",
			extra: map[string]string{
				"pkg/shared/envscan/envscan.go": "package envscan\n\nimport (\n\t\"os\"\n\t\"strings\"\n)\n\nfunc Vars() (n int) {\n\tfor _, kv := range os.Environ() {\n\t\tif strings.HasPrefix(kv, \"APP_DB_\") {\n\t\t\tn++\n\t\t}\n\t}\n\treturn n\n}\n",
				"pkg/app/scan/x_test.go":        "package scan\n\nimport (\n\t\"testing\"\n\n\t_ \"example.com/m/pkg/shared/envscan\"\n)\n\nfunc TestX(t *testing.T) {}\n",
			},
			reaches: []string{"$APP_DB_*"}, want: []string{"pkg/app/envdb", "pkg/app/scan"}},
		// Narrowing still: a unit whose source names no such variable, nor read one.
		{name: "variable only", reaches: []string{"$APP_DB_DSN"}, want: []string{"pkg/app/envdb"}},
		// env_ignore keeps the variable out of every entry: no entry can tell.
		{name: "an env_ignore name", reaches: []string{"$APP_DB_DSN"}, ignore: []string{"APP_DB_DSN"}, want: every},
		{name: "an overlapping env_ignore glob", reaches: []string{"$APP_DB_*"}, ignore: []string{"APP_*"}, want: every},
		{name: "an env_ignore glob the pattern names", reaches: []string{"$APP_DB_*"}, ignore: []string{"APP_DB_PORT"}, want: every},
		{name: "a name of the default list", reaches: []string{"$CI_DB_URL"}, want: every},
		{name: "a name env_track tracks again", reaches: []string{"$LANG"}, track: []string{"LANG"}, want: nil},
		{name: "env_ignore of another name", reaches: []string{"$APP_DB_DSN"}, ignore: []string{"OTHER_*"}, want: []string{"pkg/app/envdb"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := reachesFixture(t, "", config.Rule{Reaches: c.reaches, Deps: deps})
			for p, src := range c.extra {
				f.write(p, src)
			}
			f.suites = f.suites[:1]
			f.cfg.EnvIgnore, f.cfg.EnvTrack = c.ignore, c.track
			st := reachesRecord(f, nil)
			got := withMigration(f.sel(st, nil))
			want := c.want
			if len(want) == len(every) {
				// Every unit, the extra ones included.
				want = nil
				for _, tt := range f.sel(st, nil).SuiteByName("go").Tests {
					want = append(want, tt.ID)
				}
				slices.Sort(want)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("units with the rule's deps: %v, want %v", got, want)
			}
		})
	}
}

// A file glob of reaches is matched against what the runner loads for every test too
// (setup files and what they import), and cannot tell for a unit whose closure has an
// import static analysis does not follow (an alias, a virtual module, a missing file):
// the rule applies to both. A glob naming a directory matches what is below it.
func TestRuleReachesLoadedAndUnresolved(t *testing.T) {
	deps := []string{"migrations/**"}
	kit := []string{"web/src/testkit/**"}
	for _, c := range []struct {
		name    string
		runner  string
		files   map[string]string
		reaches []string
		want    []string
	}{
		{name: "vitest setupFiles", runner: "vitest", reaches: kit, files: map[string]string{
			"web/vitest.config.ts": `export default { test: { setupFiles: ["./test/setup.ts"] } }`,
			"web/test/setup.ts":    `import { reset } from "../src/testkit/db"; reset()`,
		}, want: []string{"web/src/a.test.ts", "web/src/b.test.ts"}},
		{name: "vitest globalSetup", runner: "vitest", reaches: kit, files: map[string]string{
			"web/vitest.config.ts": `export default { test: { globalSetup: "./test/global.ts" } }`,
			"web/test/global.ts":   `import { reset } from "../src/testkit/db"; export default () => reset()`,
		}, want: []string{"web/src/a.test.ts", "web/src/b.test.ts"}},
		{name: "vitest setup file importing the helper indirectly", runner: "vitest", reaches: kit, files: map[string]string{
			"web/vitest.config.ts": `export default { test: { setupFiles: ["./test/setup.ts"] } }`,
			"web/test/setup.ts":    `import "./db-setup"`,
			"web/test/db-setup.ts": `import { reset } from "../src/testkit/db"; reset()`,
		}, want: []string{"web/src/a.test.ts", "web/src/b.test.ts"}},
		{name: "naming the setup file itself", runner: "vitest", reaches: []string{"web/test/setup.ts"}, files: map[string]string{
			"web/vitest.config.ts": `export default { test: { setupFiles: ["./test/setup.ts"] } }`,
			"web/test/setup.ts":    `export {}`,
		}, want: []string{"web/src/a.test.ts", "web/src/b.test.ts"}},
		{name: "jest setupFilesAfterEnv", runner: "jest", reaches: kit, files: map[string]string{
			"web/jest.config.js": `module.exports = { setupFilesAfterEnv: ["<rootDir>/test/setup.ts"] }`,
			"web/test/setup.ts":  `import { reset } from "../src/testkit/db"; reset()`,
		}, want: []string{"web/src/a.test.ts", "web/src/b.test.ts"}},
		// Narrowing still: no setup file, every import resolved.
		{name: "no setup file", runner: "vitest", reaches: kit, files: map[string]string{
			"web/vitest.config.ts": `export default { test: {} }`,
		}, want: []string{"web/src/b.test.ts"}},
		{name: "a Vite alias", runner: "vitest", reaches: kit, files: map[string]string{
			"web/vitest.config.ts": `export default { resolve: { alias: { "~kit": "/src/testkit" } }, test: {} }`,
			"web/src/a.test.ts":    `import { reset } from "~kit/db"; reset()`,
		}, want: []string{"web/src/a.test.ts", "web/src/b.test.ts"}},
		{name: "a virtual module", runner: "vitest", reaches: kit, files: map[string]string{
			"web/vitest.config.ts": `export default { test: {} }`,
			"web/src/a.test.ts":    `import kit from "virtual:kit"`,
		}, want: []string{"web/src/a.test.ts", "web/src/b.test.ts"}},
		{name: "a missing file", runner: "vitest", reaches: kit, files: map[string]string{
			"web/vitest.config.ts": `export default { test: {} }`,
			"web/src/a.test.ts":    `import { x } from "./testkit/nope"`,
		}, want: []string{"web/src/a.test.ts", "web/src/b.test.ts"}},
		{name: "an unresolved tsconfig path", runner: "vitest", reaches: kit, files: map[string]string{
			"web/vitest.config.ts": `export default { test: {} }`,
			"web/tsconfig.json":    `{"compilerOptions":{"paths":{"@kit/*":["./src/kits/*"]}}}`,
			"web/src/a.test.ts":    `import { x } from "@kit/db"`,
		}, want: []string{"web/src/a.test.ts", "web/src/b.test.ts"}},
		// A directory without /** matches what is below it.
		{name: "a directory glob", runner: "vitest", reaches: []string{"web/src/testkit"}, files: map[string]string{
			"web/vitest.config.ts": `export default { test: {} }`,
		}, want: []string{"web/src/b.test.ts"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{
				"web/package.json":      `{"name":"web"}`,
				"web/src/repo.ts":       `export const repo = 1`,
				"web/src/testkit/db.ts": `export const reset = () => {}`,
				"web/src/a.test.ts":     `import { repo } from "./repo"`,
				"web/src/b.test.ts":     `import { reset } from "./testkit/db"`,
				"migrations/001.sql":    "create table t();",
			}
			for p, src := range c.files {
				files[p] = src
			}
			f := newFixture(t, files)
			s, err := runner.NewSuite(&gitx.Repo{Root: f.dir}, config.Suite{Name: "web", Root: "web", Runner: c.runner, Command: c.runner,
				Rules: []config.Rule{{Reaches: c.reaches, Deps: deps}}})
			if err != nil {
				t.Fatal(err)
			}
			f.suites = []*runner.Suite{s}
			// Record every unit as passed, then select with nothing changed.
			res := f.sel(state.New(), func(in *Input) { in.All = true })
			st := state.New()
			h := NewHasher(f.snap()).WithConfig(f.cfg)
			for _, sr := range res.Suites {
				obs := map[string]*runner.Observation{}
				for _, tt := range sr.Tests {
					obs[tt.ID] = &runner.Observation{Files: map[string]bool{}, Status: state.Pass}
				}
				Record(st, sr, obs, h.ForSuite(sr.Suite), RecordOptions{Commit: "c", Now: time.Unix(1, 0), Full: true})
			}
			st.Files = map[string]string(f.snap())
			if got := withRuleDeps(f.sel(st, nil), "migrations/001.sql"); !reflect.DeepEqual(got, c.want) {
				t.Errorf("units with the rule's deps: %v, want %v", got, c.want)
			}
			// doctor counts the same units.
			if n := RuleReach(f.cfg, s, slices.Sorted(mapsKeys(f.files)), f.graph(), st.Suites["web"])[0]; n != len(c.want) {
				t.Errorf("RuleReach counts %d, want %d", n, len(c.want))
			}
		})
	}
}

// withRuleDeps returns the units whose known dependencies include dep.
func withRuleDeps(res *Result, dep string) []string {
	var out []string
	for _, sr := range res.Suites {
		for u, known := range sr.Known {
			if slices.Contains(known, dep) {
				out = append(out, u)
			}
		}
	}
	slices.Sort(out)
	return out
}

func mapsKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// Each file glob of reaches that matches no file is reported, also when another glob
// of the rule matches (a bare directory matches what is below it).
func TestUnmatchedReaches(t *testing.T) {
	f := reachesFixture(t, "",
		config.Rule{Reaches: []string{"pkg/shared/dbhelper/**", "pkg/shared/testkit", "example.com/m/pkg/shared/testkit", "pkg/shared/testkits/**", "$APP_DB_DSN"}, Deps: []string{"database/migrations/**"}},
		config.Rule{Reaches: []string{"pkg/shared/testkit/**"}, Deps: []string{"database/fixtures/**"}},
	)
	got := UnmatchedReaches(f.suites[0], slices.Sorted(mapsKeys(f.files)))
	want := map[int][]string{0: {"example.com/m/pkg/shared/testkit", "pkg/shared/testkits/**"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unmatched %v, want %v", got, want)
	}
}

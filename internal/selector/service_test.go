package selector

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

func TestPathsID(t *testing.T) {
	for _, c := range []struct {
		name        string
		left, right []string
	}{
		{"one glob versus two", []string{"a,b"}, []string{"a", "b"}},
		{"empty list versus empty glob", nil, []string{""}},
		{"comma in first versus last glob", []string{"a,b", "c"}, []string{"a", "b,c"}},
		{"empty segments", []string{",a", "b"}, []string{"", "a,b"}},
		{"trailing comma", []string{"a,b", ""}, []string{"a", "b,"}},
		{"wildcards", []string{"worker,first/**"}, []string{"worker", "first/**"}},
		{"quotes and newlines", []string{"a,\"b", "c\nd"}, []string{"a", "\"b,c\nd"}},
		{"encoding marker in a glob", []string{"a,b"}, []string{"paths-v2:3:a,b"}},
		{"invalid UTF-8 remains distinct", []string{"a,\xff"}, []string{"a,\xfe"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if pathsID(c.left) == pathsID(c.right) {
				t.Errorf("different lists share an ID: %q and %q", c.left, c.right)
			}
		})
	}
	for _, globs := range [][]string{{"a"}, {"quotes\"and\nnewlines"}} {
		if got, want := pathsID(globs), "paths:"+strings.Join(globs, ","); got != want {
			t.Errorf("single comma-free key changed: %q, want %q", got, want)
		}
	}
}

func TestSelectDependsOnCommaGlobs(t *testing.T) {
	for _, c := range []struct {
		name        string
		left, right []string
		changed     string
	}{
		{"one versus two", []string{"a,b"}, []string{"a", "b"}, "a,b"},
		{"first versus last", []string{"a,b", "c"}, []string{"a", "b,c"}, "a,b"},
		{"last versus first", []string{"a", "b,c"}, []string{"a,b", "c"}, "b,c"},
		{"wildcards", []string{"worker,first/**"}, []string{"worker", "first/**"}, "worker,first/job.py"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := serviceFixture(t, []config.Dependency{
				{Paths: c.left},
				{Paths: c.right, Tests: []string{"admin/**"}},
			}, nil)
			f.write(c.changed, "one")
			st := f.record(nil, nil)
			if got := e2eVia(f.sel(st, nil)); len(got) != 0 {
				t.Fatalf("unchanged inputs select tests: %v", got)
			}
			f.write(c.changed, "two")
			res := f.sel(st, nil)
			if got := e2eVia(res); len(got) != 2 {
				t.Errorf("changed declared input must select both e2e tests: %v", got)
			}
			if len(res.Orphans) != 0 {
				t.Errorf("declared input became an orphan: %v", res.Orphans)
			}
		})
	}
}

func TestSelectDependsOnLegacyCommaKeys(t *testing.T) {
	for _, paths := range [][]string{{"worker/**"}, {"worker/**", "other/**"}, {"worker,first/**"}, {"worker,first/**", "other/**"}} {
		t.Run(strings.Join(paths, " + "), func(t *testing.T) {
			f := serviceFixture(t, []config.Dependency{{Paths: paths}}, nil)
			f.write("worker,first/job.py", "one")
			st := f.record(nil, nil)
			key := depkey.Set(pathsID(paths))
			oldKey := depkey.Set("paths:" + strings.Join(paths, ","))
			for _, e := range st.Suites["e2e"].Tests {
				v := e.Deps[key]
				delete(e.Deps, key)
				e.Deps[oldKey] = v
			}
			res := f.sel(st, nil)
			want := 2
			if key == oldKey {
				want = 0 // existing unambiguous recordings stay usable
			}
			if got := e2eVia(res); len(got) != want {
				t.Fatalf("legacy key selection: %v, want %d tests", got, want)
			}
			obs := map[string]*runner.Observation{}
			for unit := range st.Suites["e2e"].Tests {
				obs[unit] = &runner.Observation{Status: state.Pass}
			}
			sr := res.SuiteByName("e2e")
			Record(st, sr, obs, NewHasher(f.snap()).WithConfig(f.cfg).ForSuite(sr.Suite), RecordOptions{})
			for unit, e := range st.Suites["e2e"].Tests {
				if _, ok := e.Deps[key]; !ok {
					t.Errorf("%s: new key missing from recording", unit)
				}
				if _, ok := e.Deps[oldKey]; oldKey != key && ok {
					t.Errorf("%s: obsolete key retained", unit)
				}
			}
			if got := e2eVia(f.sel(st, nil)); len(got) != 0 {
				t.Errorf("fresh recording still selects unchanged tests: %v", got)
			}
		})
	}
}

func TestSelectDependsOnOverwrittenLegacyKeys(t *testing.T) {
	for _, c := range []struct {
		name        string
		left, right []string
		removed     string
	}{
		{"comma-free list overwritten by literal", []string{"a", "b"}, []string{"a,b"}, "a"},
		{"comma-free globs overwritten by literal", []string{"worker/**", "other/**"}, []string{"worker/**,other/**"}, "worker/job.py"},
		{"literal overwritten by comma-free list", []string{"a,b"}, []string{"a", "b"}, "a,b"},
		{"comma in either list", []string{"a,b", "c"}, []string{"a", "b,c"}, "a,b"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := serviceFixture(t, []config.Dependency{
				{Paths: c.left},
				{Paths: c.right, Tests: []string{"admin/**"}},
			}, nil)
			f.write(c.removed, "one")
			st := f.record(nil, nil)
			sr := f.sel(st, nil).SuiteByName("e2e")
			h := NewHasher(f.snap()).WithConfig(f.cfg).ForSuite(sr.Suite).WithSets(sr.Services)
			// Before the fix, the last declaration's hash overwrote the first,
			// including in tests the last declaration did not apply to.
			oldKey := depkey.Set("paths:" + strings.Join(c.left, ","))
			wrong := h.Hash(sr.Services[1].Key)
			for _, e := range st.Suites["e2e"].Tests {
				for key := range e.Deps {
					if depkey.IsSet(key) {
						delete(e.Deps, key)
					}
				}
				e.Deps[oldKey] = wrong
			}
			// Removing the first set's sole file makes its new correct hash equal
			// the old wrong empty-set hash. That equality must never skip a test.
			f.remove(c.removed)
			res := f.sel(st, nil)
			if got := e2eVia(res); len(got) != 2 {
				t.Errorf("overwritten legacy hash skipped a changed dependency: %v", got)
			}
		})
	}
}

// serviceFixture: Playwright-style tests under e2e/ exercise a Go server under
// backend/ (another process), plus a worker and docker-compose.yml.
func serviceFixture(t *testing.T, deps []config.Dependency, serves []string) *fixture {
	f := newFixture(t, map[string]string{
		"go.mod":                    "module example.com/m\n\ngo 1.22\n",
		"backend/api/user.go":       "package api\n",
		"backend/api/user_test.go":  "package api\n",
		"backend/README.md":         "docs",
		"backend/node_modules/x.js": "",
		"backend/tools/gen.py":      "",
		"backend/tools/test_gen.py": "",
		"e2e/login.spec.ts":         `import { login } from "./helpers"`,
		"e2e/admin/users.spec.ts":   `test("users")`,
		"e2e/helpers.ts":            `export const login = 1`,
		"docker-compose.yml":        "services: {}",
		"worker/job.py":             "",
	})
	f.suites = []*runner.Suite{
		suite(t, config.Suite{Name: "go", Runner: "go", Root: "backend", Tests: []string{"**/*_test.go"}}),
		suite(t, config.Suite{Name: "tools", Runner: "pytest", Root: "backend/tools", Tests: []string{"test_*.py"}}),
		suite(t, config.Suite{Name: "e2e", Runner: "vitest", Root: "e2e", Tests: []string{"**/*.spec.ts"}, DependsOn: deps, Serves: serves}),
	}
	return f
}

func defaultServiceFixture(t *testing.T) *fixture {
	return serviceFixture(t, []config.Dependency{
		{Suite: "go"},
		{Paths: []string{"docker-compose.yml"}, Tests: []string{"admin/**"}},
	}, []string{"worker/**"})
}

func e2eVia(res *Result) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, tt := range res.SuiteByName("e2e").Tests {
		if tt.Selected {
			out[tt.ID] = map[string]string{}
			for _, c := range tt.Changed {
				out[tt.ID][c] = tt.Via[c] + ": " + tt.Service[c]
			}
		}
	}
	return out
}

func TestSelectDependsOn(t *testing.T) {
	f := defaultServiceFixture(t)
	st := f.record(nil, nil)
	if res := f.sel(st, nil); len(e2eVia(res)) != 0 || len(res.Orphans) != 0 {
		t.Fatalf("nothing changed: %v, orphans %v", e2eVia(res), res.Orphans)
	}

	f.write("backend/api/user.go", "package api\n// changed\n")
	res := f.sel(st, nil)
	want := map[string]map[string]string{
		"e2e/login.spec.ts":       {"backend/api/user.go": "service: depends_on suite go"},
		"e2e/admin/users.spec.ts": {"backend/api/user.go": "service: depends_on suite go"},
	}
	if got := e2eVia(res); !reflect.DeepEqual(got, want) {
		t.Errorf("server change: %v, want %v", got, want)
	}
	if d := decisions(res); d["backend/api"] != ReasonChanged {
		t.Errorf("the go suite still selects its own test: %v", d)
	}
	if len(res.Orphans) != 0 {
		t.Errorf("orphans %v", res.Orphans)
	}
	f.write("backend/api/user.go", "package api\n")

	// Not the server's code: its tests, docs, vendored files, a suite rooted below it.
	for _, p := range []string{"backend/api/user_test.go", "backend/README.md", "backend/node_modules/x.js", "backend/tools/gen.py"} {
		f.write(p, "changed")
		if got := e2eVia(f.sel(st, nil)); len(got) != 0 {
			t.Errorf("%s: %v", p, got)
		}
		f.write(p, "")
	}
	f.write("backend/api/user_test.go", "package api\n")
	f.write("backend/README.md", "docs")

	// tests: narrows the dependency.
	f.write("docker-compose.yml", "services: {db: {}}")
	want = map[string]map[string]string{"e2e/admin/users.spec.ts": {"docker-compose.yml": "service: depends_on docker-compose.yml"}}
	if got := e2eVia(f.sel(st, nil)); !reflect.DeepEqual(got, want) {
		t.Errorf("narrowed: %v, want %v", got, want)
	}
	f.write("docker-compose.yml", "services: {}")

	// serves is depends_on paths.
	f.write("worker/job.py", "x = 1")
	want = map[string]map[string]string{
		"e2e/login.spec.ts":       {"worker/job.py": "service: serves worker/**"},
		"e2e/admin/users.spec.ts": {"worker/job.py": "service: serves worker/**"},
	}
	if got := e2eVia(f.sel(st, nil)); !reflect.DeepEqual(got, want) {
		t.Errorf("serves: %v, want %v", got, want)
	}
	f.write("worker/job.py", "")

	// A stronger explanation wins: the test's static imports are not "service".
	f.write("e2e/helpers.ts", "export const login = 2")
	if got := e2eVia(f.sel(st, nil)); got["e2e/login.spec.ts"]["e2e/helpers.ts"] != "import: " || len(got) != 1 {
		t.Errorf("own helper: %v", got)
	}
}

func TestSelectDependsOnSuiteFilter(t *testing.T) {
	f := defaultServiceFixture(t)
	st := f.record(nil, nil)
	goS, tools, e2e := f.suites[0], f.suites[1], f.suites[2]
	f.write("backend/api/user.go", "package api\n// changed\n")
	f.write("worker/job.py", "x = 1")

	// --suite e2e: the suite it depends on is left out but still expands.
	res := f.sel(st, func(in *Input) { in.Suites, in.Others = []*runner.Suite{e2e}, []*runner.Suite{goS, tools} })
	if got := e2eVia(res); len(got) != 2 || got["e2e/login.spec.ts"]["backend/api/user.go"] != "service: depends_on suite go" {
		t.Errorf("--suite e2e: %v", got)
	}
	if len(res.Orphans) != 0 {
		t.Errorf("--suite e2e: orphans %v", res.Orphans)
	}

	// --suite go: worker/job.py is only e2e's dependency, still not an orphan.
	res = f.sel(st, func(in *Input) { in.Suites, in.Others = []*runner.Suite{goS}, []*runner.Suite{tools, e2e} })
	if len(res.Orphans) != 0 {
		t.Errorf("--suite go: orphans %v", res.Orphans)
	}

	// Without the declaration the worker is an orphan.
	plain := serviceFixture(t, nil, nil)
	pst := plain.record(nil, nil)
	plain.write("worker/job.py", "x = 1")
	if res := plain.sel(pst, nil); !reflect.DeepEqual(res.Orphans, []string{"worker/job.py"}) {
		t.Errorf("undeclared: orphans %v", res.Orphans)
	}
}

func TestServiceVia(t *testing.T) {
	f := defaultServiceFixture(t)
	res := f.sel(f.record(nil, nil), func(in *Input) { in.All = true })
	s := res.SuiteByName("e2e")
	cases := []struct{ unit, file, want string }{
		{"e2e/login.spec.ts", "backend/api/user.go", "depends_on suite go"},
		{"e2e/login.spec.ts", "docker-compose.yml", ""},
		{"e2e/admin/users.spec.ts", "docker-compose.yml", "depends_on docker-compose.yml"},
		{"e2e/login.spec.ts", "worker/job.py", "serves worker/**"},
		{"e2e/login.spec.ts", "e2e/helpers.ts", ""},
	}
	for _, c := range cases {
		if got := s.ServiceVia(c.unit, c.file); got != c.want {
			t.Errorf("ServiceVia(%s, %s) = %q, want %q", c.unit, c.file, got, c.want)
		}
	}
	// One key per declaration, not one per file.
	if k := s.Known["e2e/login.spec.ts"]; !contains(k, "@set:suite:go") || !contains(k, "@set:paths:worker/**") ||
		contains(k, "@set:paths:docker-compose.yml") || contains(k, "backend/api/user.go") {
		t.Errorf("known: %v", k)
	}
	if !contains(s.Known["e2e/admin/users.spec.ts"], "@set:paths:docker-compose.yml") {
		t.Errorf("known: %v", s.Known["e2e/admin/users.spec.ts"])
	}
	if got := s.ExpandSets([]string{"@set:suite:go", "e2e/helpers.ts"}); !reflect.DeepEqual(got, []string{"backend/api/user.go", "e2e/helpers.ts"}) {
		t.Errorf("ExpandSets: %v", got)
	}
}

// The state keeps one entry per declaration and test, with a hash over every file.
func TestRecordDependsOnOneKey(t *testing.T) {
	f := defaultServiceFixture(t)
	for i := 0; i < 50; i++ {
		f.write(fmt.Sprintf("backend/api/h%02d.go", i), "package api\n")
	}
	st := f.record(nil, nil)
	e := st.Suites["e2e"].Tests["e2e/login.spec.ts"]
	var keys []string
	for k := range e.Deps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"@set:paths:worker/**", "@set:suite:go", "e2e/helpers.ts", "e2e/login.spec.ts"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("recorded deps %v, want %v", keys, want)
	}
	h := NewHasher(f.snap()).WithSets(f.sel(st, nil).SuiteByName("e2e").Services)
	if v := e.Deps["@set:suite:go"]; !strings.HasPrefix(v, "set:") || v != h.Hash("@set:suite:go") {
		t.Errorf("set hash %q", v)
	}
	// Editing, adding or deleting a server file changes the set, and is named.
	for _, step := range []struct {
		do   func()
		file string
	}{
		{func() { f.write("backend/api/h07.go", "package api\n// x\n") }, "backend/api/h07.go"},
		{func() { f.write("backend/api/new.go", "package api\n") }, "backend/api/new.go"},
		{func() { f.remove("backend/api/h08.go") }, "backend/api/h08.go"},
	} {
		step.do()
		res := f.sel(st, nil)
		if got := e2eVia(res)["e2e/login.spec.ts"]; !reflect.DeepEqual(got, map[string]string{step.file: "service: depends_on suite go"}) {
			t.Errorf("%s: %v", step.file, got)
		}
		if len(res.Orphans) != 0 {
			t.Errorf("%s: orphans %v", step.file, res.Orphans)
		}
		st = f.record(nil, nil)
	}
}

// Two suites sharing a root: the other suite's tests are not server code.
func TestSelectDependsOnSameRootSibling(t *testing.T) {
	f := newFixture(t, map[string]string{
		"src/server.ts":       `export const s = 1`,
		"src/server.test.ts":  `test("unit")`,
		"src/util_test.go":    "package src\n",
		"e2e/login.spec.ts":   `test("e2e")`,
		"jest.config.js":      `module.exports = {}`,
		"src/other.test.ts":   `test("unit 2")`,
		"src/component.go":    "package src\n",
		"e2e/fixtures/a.json": `{}`,
	})
	f.suites = []*runner.Suite{
		suite(t, config.Suite{Name: "go", Runner: "go", Root: ".", Tests: []string{"**/*_test.go"}, Exclude: []string{"e2e/**"}}),
		suite(t, config.Suite{Name: "jest", Runner: "jest", Root: ".", Tests: []string{"src/**/*.test.ts"}}),
		suite(t, config.Suite{Name: "e2e", Runner: "vitest", Root: "e2e", Tests: []string{"*.spec.ts"}, DependsOn: []config.Dependency{{Suite: "go"}}}),
	}
	st := f.record(nil, nil)
	f.write("src/server.test.ts", `test("unit changed")`)
	f.write("src/util_test.go", "package src\n// changed\n")
	if got := e2eVia(f.sel(st, nil)); len(got) != 0 {
		t.Errorf("a sibling suite's test selected e2e: %v", got)
	}
	f.write("src/component.go", "package src\n// changed\n")
	if got := e2eVia(f.sel(st, nil)); len(got) != 1 {
		t.Errorf("server code: %v", got)
	}
}

func TestSelectDependsOnDiffMode(t *testing.T) {
	f := defaultServiceFixture(t)
	repo, base := gitFixture(t, f)
	f.write("backend/api/user.go", "package api\n// changed\n")
	res := f.sel(state.New(), func(in *Input) { in.Repo, in.Base = repo, base })
	if res.Mode != ModeDiff {
		t.Fatalf("mode %s", res.Mode)
	}
	if got := e2eVia(res)["e2e/admin/users.spec.ts"]; !reflect.DeepEqual(got, map[string]string{"backend/api/user.go": "service: depends_on suite go"}) {
		t.Errorf("diff mode: %v", e2eVia(res))
	}
}

func TestServiceExpanderComputesSuiteCodeOnce(t *testing.T) {
	f := defaultServiceFixture(t)
	other := suite(t, config.Suite{Name: "e2e2", Runner: "vitest", Root: "e2e2", DependsOn: []config.Dependency{{Suite: "go"}}})
	suites := append(f.suites, other)
	var files []string
	for p := range f.files {
		files = append(files, p)
	}
	e := newServiceExpander(f.cfg, suites, files)
	a, b := e.forSuite(suites[2]), e.forSuite(other)
	if len(e.code) != 1 || !reflect.DeepEqual(a[0].Files, []string{"backend/api/user.go"}) || e.code["go"] != e.suiteCode("go") {
		t.Errorf("code %v, files %v", e.code, a[0].Files)
	}
	if !reflect.DeepEqual(a[0].Files, b[0].Files) {
		t.Errorf("%v != %v", a[0].Files, b[0].Files)
	}
}

// A changed file both imported by the test and inside a declaration is explained as
// the import (the set key sorts first but must not take it over).
func TestSelectDependsOnImportWins(t *testing.T) {
	f := newFixture(t, map[string]string{
		"e2e/login.spec.ts": `import { User } from "../shared/types"`,
		"shared/types.ts":   `export type User = {}`,
		"shared/api.ts":     `export const api = 1`,
	})
	f.suites = []*runner.Suite{suite(t, config.Suite{Name: "e2e", Runner: "vitest", Root: "e2e", Tests: []string{"*.spec.ts"},
		DependsOn: []config.Dependency{{Paths: []string{"shared/**"}}}})}
	st := f.record(nil, nil)
	f.write("shared/types.ts", `export type User = {id: string}`)
	f.write("shared/api.ts", `export const api = 2`)
	want := map[string]map[string]string{"e2e/login.spec.ts": {
		"shared/types.ts": "import: ",
		"shared/api.ts":   "service: depends_on shared/**",
	}}
	res := f.sel(st, nil)
	if got := e2eVia(res); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if c := res.Suites[0].Tests[0].Changed; !reflect.DeepEqual(c, []string{"shared/api.ts", "shared/types.ts"}) {
		t.Errorf("changed %v", c)
	}
}

// smates orphans replays history against Select(All).Covered/Explains.
func TestDependsOnCovered(t *testing.T) {
	f := defaultServiceFixture(t)
	res := f.sel(f.record(nil, nil), func(in *Input) { in.All = true })
	if !res.Covered["backend/api/user.go"] || !res.Covered["worker/job.py"] {
		t.Errorf("declared files not covered")
	}
	if !res.Explains("backend/api/gone.go", true) || !res.Explains("worker/old.py", true) || res.Explains("frontend/x.ts", true) {
		t.Errorf("Explains")
	}
}

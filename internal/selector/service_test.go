package selector

import (
	"reflect"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
)

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
	if !contains(s.Known["e2e/login.spec.ts"], "backend/api/user.go") || contains(s.Known["e2e/login.spec.ts"], "e2e/admin/users.spec.ts") {
		t.Errorf("known: %v", s.Known["e2e/login.spec.ts"])
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
	if len(e.code) != 1 || !reflect.DeepEqual(a[0].Files, []string{"backend/api/user.go"}) || &e.code["go"][0] != &e.suiteCode("go")[0] {
		t.Errorf("code %v, files %v", e.code, a[0].Files)
	}
	if !reflect.DeepEqual(a[0].Files, b[0].Files) {
		t.Errorf("%v != %v", a[0].Files, b[0].Files)
	}
}

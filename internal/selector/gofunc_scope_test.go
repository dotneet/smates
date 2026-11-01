package selector

import (
	"reflect"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
)

// Scoped rules and depends_on declarations select Go test functions
// (granularity: func) by their package directory or their test file, as they select
// the package unit.
func TestScopedDepsGoFuncUnits(t *testing.T) {
	f := newFixture(t, map[string]string{
		"go.mod":                   "module example.com/m\n\ngo 1.22\n",
		"e2e/login/login_test.go":  "package login\n\nimport \"testing\"\n\nfunc TestLogin(t *testing.T) {}\n",
		"e2e/admin/admin_test.go":  "package admin\n\nimport \"testing\"\n\nfunc TestAdmin(t *testing.T) {}\n",
		"e2e/admin/extra_test.go":  "package admin\n\nimport \"testing\"\n\nfunc TestExtra(t *testing.T) {}\n",
		"migrations/001.sql":       "create table t()",
		"docker-compose.yml":       "services: {}",
		"backend/server/server.go": "package server\n",
	})
	s := suite(t, config.Suite{
		Name: "e2e", Runner: "go", Root: "e2e", Tests: []string{"**/*_test.go"}, Granularity: config.GranularityFunc,
		Rules: []config.Rule{
			{Tests: []string{"login"}, Deps: []string{"migrations/**"}},         // a package directory
			{Tests: []string{"**/extra_test.go"}, Deps: []string{"backend/**"}}, // a test file
		},
		DependsOn: []config.Dependency{{Paths: []string{"docker-compose.yml"}, Tests: []string{"admin/**"}}},
	})
	s.RepoRoot = f.dir
	f.suites = []*runner.Suite{s}
	st := f.record(nil, nil)
	for _, c := range []struct {
		file string
		want map[string]string
	}{
		{"migrations/001.sql", map[string]string{"e2e/login#TestLogin": ReasonChanged, "e2e/admin#TestAdmin": "-unaffected", "e2e/admin#TestExtra": "-unaffected"}},
		{"backend/server/server.go", map[string]string{"e2e/login#TestLogin": "-unaffected", "e2e/admin#TestAdmin": "-unaffected", "e2e/admin#TestExtra": ReasonChanged}},
		{"docker-compose.yml", map[string]string{"e2e/login#TestLogin": "-unaffected", "e2e/admin#TestAdmin": ReasonChanged, "e2e/admin#TestExtra": ReasonChanged}},
	} {
		orig := f.files[c.file]
		f.write(c.file, orig+"\n-- changed\n")
		if got := decisions(f.sel(st, nil)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.file, got, c.want)
		}
		f.write(c.file, orig)
	}
	sr := f.sel(st, nil).SuiteByName("e2e")
	if got := sr.ServiceVia("e2e/admin#TestAdmin", "docker-compose.yml"); got == "" {
		t.Error("ServiceVia: TestAdmin depends on docker-compose.yml")
	}
	if got := sr.ServiceVia("e2e/login#TestLogin", "docker-compose.yml"); got != "" {
		t.Errorf("ServiceVia: TestLogin is not scoped in, got %q", got)
	}
}

func TestUnitMatches(t *testing.T) {
	s := &runner.Suite{Suite: config.Suite{Root: "e2e"}}
	for _, c := range []struct {
		unit  string
		files []string
		globs []string
		want  bool
	}{
		{"e2e/admin", nil, []string{"admin/**"}, true},
		{"e2e/admin#TestA", nil, []string{"admin/**"}, true},
		{"e2e/admin#TestA", nil, []string{"admin"}, true},
		{"e2e/admin#TestA", []string{"e2e/admin/a_test.go"}, []string{"**/a_test.go"}, true},
		{"e2e/admin#TestA", []string{"e2e/admin/a_test.go"}, []string{"login/**"}, false},
		{"e2e/a.spec.ts", nil, []string{"*.spec.ts"}, true},
	} {
		if got := UnitMatches(s, c.unit, c.files, c.globs); got != c.want {
			t.Errorf("UnitMatches(%q, %v, %v) = %v", c.unit, c.files, c.globs, got)
		}
	}
}

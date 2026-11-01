package selector

import (
	"reflect"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
)

// An orphan named only in a Go file a test function depends on through its
// declarations (a constant path in a file whose functions it does not run) is found by
// the reference scan, which reads the file behind the declarations key.
func TestReferencesThroughDeclarations(t *testing.T) {
	f := newFixture(t, map[string]string{
		"go.mod":                     "module example.com/m\n\ngo 1.22\n",
		"p/paths.go":                 "package p\n\nconst SeedPath = \"migrations/users_seed.sql\"\n\nfunc Seed() string { return SeedPath }\n",
		"p/a_test.go":                "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
		"migrations/users_seed.sql":  "insert into users values (1)",
		"migrations/other_table.sql": "create table other()",
	})
	s := suite(t, config.Suite{Runner: "go", Tests: []string{"**/*_test.go"}, Granularity: config.GranularityFunc})
	s.RepoRoot = f.dir
	f.suites = []*runner.Suite{s}
	st := f.record(nil, nil)
	if deps := f.sel(st, nil).Suites[0].Deps["p#TestA"]; !contains(deps, "p/paths.go#decls") || contains(deps, "p/paths.go") {
		t.Fatalf("deps %v", deps)
	}

	f.write("migrations/users_seed.sql", "insert into users values (2)")
	res := f.sel(st, nil)
	if !reflect.DeepEqual(res.Orphans, []string{"migrations/users_seed.sql"}) {
		t.Fatalf("orphans %v", res.Orphans)
	}
	adds, explained := res.References(f.graph(), res.Orphans)
	if len(adds) != 1 || adds[0].Test != "p#TestA" || adds[0].Reason != ReasonReference || !reflect.DeepEqual(explained, []string{"migrations/users_seed.sql"}) {
		t.Fatalf("adds %+v explained %v", adds, explained)
	}
	if adds[0].Detail != "references migrations/users_seed.sql (in p/paths.go)" {
		t.Errorf("detail %q", adds[0].Detail)
	}
	res.Apply(adds)
	if got := decisions(res); got["p#TestA"] != ReasonReference {
		t.Errorf("selection %v", got)
	}
}

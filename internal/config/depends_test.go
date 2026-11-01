package config

import (
	"strings"
	"testing"
)

func TestDependsOn(t *testing.T) {
	c, err := Parse([]byte(`
suites:
  - name: api
    runner: go
    root: backend
  - name: e2e
    runner: custom
    command: npx playwright test
    root: e2e
    depends_on:
      - api
      - suite: api
        tests: ["admin/**"]
      - paths: ["docker-compose.yml", "proto/**"]
    serves: ["backend/**", "worker/**", "cmd/**"]
`))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Suites[1]
	deps := s.Dependencies()
	if len(deps) != 4 {
		t.Fatalf("dependencies: %+v", deps)
	}
	want := []string{"depends_on suite api", "depends_on suite api", "depends_on docker-compose.yml, proto/**", "serves backend/**, worker/**, …"}
	for i, d := range deps {
		if d.String() != want[i] {
			t.Errorf("%d: %q, want %q", i, d.String(), want[i])
		}
	}
	if deps[1].Tests[0] != "admin/**" || !deps[3].FromServes || deps[2].FromServes {
		t.Errorf("entries: %+v", deps)
	}
	if len(s.DependsOn) != 3 {
		t.Errorf("Dependencies must not change DependsOn: %+v", s.DependsOn)
	}
}

func TestDependsOnErrors(t *testing.T) {
	head := "suites:\n  - name: api\n    runner: go\n  - name: e2e\n    runner: pytest\n"
	cases := map[string]string{
		"    depends_on: [web]\n":                             `unknown suite "web"`,
		"    depends_on: [e2e]\n":                             "cannot depend on itself",
		"    depends_on: [{tests: [\"*\"]}]\n":                "needs suite or paths",
		"    depends_on: [{suite: api, paths: [\"x/**\"]}]\n": "either suite or paths",
		"    depends_on: [{paths: [\"x/[\"]}]\n":              "bad glob",
		"    depends_on: [{paths: [\"/x/**\"]}]\n":            "relative to the repository root",
		"    depends_on: [{paths: [\"../x\"]}]\n":             "relative to the repository root",
		"    depends_on: [{suite: api, tests: [\"[\"]}]\n":    "bad glob",
		"    depends_on: [{suite: api, deps: [x]}]\n":         "not found",
		"    serves: [\"\"]\n":                                "bad glob",
		"    serves: [\"/srv/**\"]\n":                         "relative to the repository root",
		"    serves: ['backend\\**']\n":                       "use / as the path separator",
		"    depends_on: [{paths: ['a\\b']}]\n":               "use / as the path separator",
		"    serves: [\"./\"]\n":                              "bad glob",
	}
	for extra, want := range cases {
		_, err := Parse([]byte(head + extra))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", extra, err, want)
		}
	}
	// A leading "./" would match nothing: it is stripped.
	c, err := Parse([]byte(head + "    serves: [\"./backend/**\"]\n    depends_on: [{paths: [\"././proto/*\"], tests: [\"./admin/**\"]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s := c.Suites[1]; s.Serves[0] != "backend/**" || s.DependsOn[0].Paths[0] != "proto/*" || s.DependsOn[0].Tests[0] != "admin/**" {
		t.Errorf("./ not stripped: %+v", s)
	}
	// A suite listed later can be referenced.
	if _, err := Parse([]byte("suites:\n  - name: e2e\n    runner: pytest\n    depends_on: [api]\n  - name: api\n    runner: go\n")); err != nil {
		t.Errorf("forward reference: %v", err)
	}
}

package selector

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata/select_golden/")

// goldenResult is a stable dump of everything Select returns: the fields `smates
// select --format json` prints plus the ones it hides (D(T), the known deps, inputs,
// unit files, depends_on/serves declarations and the coverage), and Untested over the
// orphans.
type goldenResult struct {
	Mode           Mode              `json:"mode"`
	Changed        []string          `json:"changed"`
	Orphans        []string          `json:"orphans"`
	LeftOut        []string          `json:"left_out"`
	OrphanHandling map[string]string `json:"orphan_handling"`
	Notes          []string          `json:"notes"`
	Untested       []string          `json:"untested"`
	Covered        []string          `json:"covered"`
	Declared       []goldenService   `json:"declared"`
	Suites         []goldenSuite     `json:"suites"`
}

type goldenSuite struct {
	Name       string              `json:"name"`
	All        bool                `json:"all"`
	Unrecorded bool                `json:"unrecorded"`
	Tests      []*Test             `json:"tests"`
	Deps       map[string][]string `json:"deps"`
	Known      map[string][]string `json:"known"`
	Inputs     []string            `json:"inputs"`
	Files      map[string][]string `json:"files"`
	Services   []goldenService     `json:"services"`
}

type goldenService struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Tests []string `json:"tests"`
	Files []string `json:"files"`
}

func goldenServices(ds []*ServiceDeps) []goldenService {
	var out []goldenService
	for _, d := range ds {
		out = append(out, goldenService{Key: d.Key, Label: d.Label, Tests: d.Tests, Files: d.Files})
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dumpResult(res *Result) []byte {
	g := goldenResult{
		Mode: res.Mode, Changed: res.Changed, Orphans: res.Orphans, LeftOut: res.LeftOut,
		OrphanHandling: res.OrphanHandling, Notes: res.Notes, Untested: res.Untested(res.Orphans),
		Covered: sortedKeys(res.Covered), Declared: goldenServices(res.declared),
	}
	for _, s := range res.Suites {
		g.Suites = append(g.Suites, goldenSuite{
			Name: s.Name, All: s.All, Unrecorded: s.Unrecorded, Tests: s.Tests,
			Deps: s.Deps, Known: s.Known, Inputs: sortedKeys(s.Inputs), Files: s.Files,
			Services: goldenServices(s.Services),
		})
	}
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(data, '\n')
}

// goldenEnv is the environment variable a traced test of the polyglot fixture read.
const goldenEnv = "GOLDEN_APP_REGION"

// polyglotFixture has four suites: Go packages under svc/, vitest under web/, pytest
// under api/ and a custom runner under e2e/ that depends_on the api suite's code, a
// docker-compose file (admin tests only) and serves worker/**.
func polyglotFixture(t *testing.T) *fixture {
	f := newFixture(t, map[string]string{
		"go.mod":                   "module example.com/m\n\ngo 1.22\n",
		".tool-versions":           "go 1.22\n",
		"smates.yml":               "version: 1\n",
		"README.md":                "docs\n",
		"Dockerfile":               "FROM x\n",
		"docker-compose.yml":       "services: {}\n",
		"svc/core/core.go":         "package core\n\nfunc Core() int { return 1 }\n",
		"svc/core/core_test.go":    "package core\n",
		"svc/api/api.go":           "package api\n\nimport _ \"example.com/m/svc/core\"\n",
		"svc/api/api_test.go":      "package api\n",
		"svc/unused/unused.go":     "package unused\n",
		"svc/testdata/in.go":       "package in\n",
		"web/vitest.config.ts":     "export default {}\n",
		"web/src/a.ts":             "export const a = 1\n",
		"web/src/a.test.ts":        "import { a } from \"./a\"\n",
		"web/src/b.ts":             "export const b = 1\n",
		"web/src/b.test.ts":        "import { b } from \"./b\"\n",
		"web/src/c.test.ts":        "test(\"c\")\n",
		"web/src/orphan.ts":        "export const o = 1\n",
		"web/db/q.test.ts":         "test(\"db\")\n",
		"web/fixtures/users.json":  "[]\n",
		"migrations/001.sql":       "create table t();\n",
		"api/app.py":               "def app(): pass\n",
		"api/test_app.py":          "from app import app\n",
		"api/test_settings.py":     "import os\n",
		"api/notes.txt":            "notes\n",
		"tools/gen.sh":             "echo gen\n",
		"worker/job.py":            "def job(): pass\n",
		"e2e/login.spec.js":        "require(\"./helpers\")\n",
		"e2e/helpers.js":           "module.exports = {}\n",
		"e2e/admin/users.spec.js":  "test(\"users\")\n",
		"e2e/admin/audit.spec.js":  "test(\"audit\")\n",
		"e2e/smoke/health.spec.js": "test(\"health\")\n",
	})
	f.cfg.Path = "smates.yml"
	f.cfg.Inputs = []string{".tool-versions"}
	f.suites = []*runner.Suite{
		suite(t, config.Suite{Name: "go", Runner: "go", Root: "svc", Tests: []string{"**/*_test.go"}, Inputs: []string{"/go.mod"}}),
		suite(t, config.Suite{
			Name: "web", Runner: "vitest", Root: "web", Tests: []string{"**/*.test.ts"},
			Inputs: []string{"vitest.config.*"},
			Rules:  []config.Rule{{Tests: []string{"db/**"}, Deps: []string{"migrations/**"}}},
		}),
		suite(t, config.Suite{Name: "api", Runner: "pytest", Root: "api", Tests: []string{"test_*.py"}}),
		suite(t, config.Suite{
			Name: "e2e", Runner: "custom", Root: "e2e", Tests: []string{"**/*.spec.js"}, Command: "node run.js {tests}",
			DependsOn: []config.Dependency{
				{Suite: "api"},
				{Paths: []string{"docker-compose.yml"}, Tests: []string{"admin/**"}},
			},
			Serves: []string{"worker/**"},
		}),
	}
	return f
}

// polyglotTraced are the dependencies tracing recorded beyond the known ones, one of
// every kind of key.
var polyglotTraced = map[string][]string{
	"web/src/b.test.ts":        {"web/fixtures/users.json"}, // content
	"web/src/c.test.ts":        {"web/fixtures/"},           // listing
	"api/test_settings.py":     {"api/local.cfg?"},          // existence (absent)
	"api/test_app.py":          {"tools/**", "$" + goldenEnv},
	"e2e/smoke/health.spec.js": {"smates.yml"}, // reads the configuration file itself
}

func polyglotState(t *testing.T, f *fixture) *state.State {
	t.Setenv(goldenEnv, "eu")
	return f.record(polyglotTraced, map[string]string{"svc/api": state.Fail})
}

type goldenCase struct {
	name string
	run  func(t *testing.T) *Result
}

func goldenCases() []goldenCase {
	return []goldenCase{
		{"no-state", func(t *testing.T) *Result {
			f := polyglotFixture(t)
			return f.sel(state.New(), nil)
		}},
		{"all", func(t *testing.T) *Result {
			f := polyglotFixture(t)
			st := polyglotState(t, f)
			f.write("web/src/a.ts", "export const a = 2\n")
			return f.sel(st, func(in *Input) { in.All = true })
		}},
		{"state-unchanged", func(t *testing.T) *Result {
			f := polyglotFixture(t)
			st := polyglotState(t, f)
			delete(st.Suites["web"].Tests, "web/src/c.test.ts") // no entry: new
			return f.sel(st, nil)
		}},
		{"state-edits", func(t *testing.T) *Result {
			f := polyglotFixture(t)
			st := polyglotState(t, f)
			f.write("svc/core/core.go", "package core\n\nfunc Core() int { return 2 }\n") // import
			f.write("web/src/a.ts", "export const a = 2\n")                               // import
			f.write("web/fixtures/users.json", "[1]\n")                                   // traced content
			f.write("web/fixtures/admins.json", "[]\n")                                   // listing
			f.write("api/local.cfg", "x=1\n")                                             // existence
			f.write("tools/gen.sh", "echo gen2\n")                                        // tree
			t.Setenv(goldenEnv, "us")                                                     // env
			f.write("migrations/002.sql", "create table u();\n")                          // rule
			f.write("api/app.py", "def app(): return 1\n")                                // depends_on suite + import
			f.write("worker/job.py", "def job(): return 1\n")                             // serves
			f.write("docker-compose.yml", "services: {a: {}}\n")                          // depends_on paths, scoped
			f.write("Dockerfile", "FROM y\n")                                             // orphan
			f.write("svc/unused/unused.go", "package unused\n// x\n")                     // untested orphan
			f.write("web/src/orphan.ts", "export const o = 2\n")                          // untested orphan
			f.write("svc/testdata/in.go", "package in\n// x\n")                           // orphan, data
			f.write("README.md", "docs 2\n")                                              // ignored
			f.write("api/notes.txt", "more\n")                                            // depends_on suite only
			f.remove("e2e/admin/audit.spec.js")
			return f.sel(st, nil)
		}},
		{"state-config", func(t *testing.T) *Result {
			f := polyglotFixture(t)
			st := polyglotState(t, f)
			f.write("smates.yml", "version: 1\n# edited\n")
			f.suites[1].Env = map[string]string{"TZ": "UTC"} // web's settings changed
			return f.sel(st, nil)
		}},
		{"suite-markers", func(t *testing.T) *Result {
			// --suite web with markers: a change web already saw is unchanged for it, one
			// only api saw is not; an unattributed change only go owns is left out.
			f := polyglotFixture(t)
			st := polyglotState(t, f)
			f.write("web/src/a.ts", "export const a = 2\n")
			f.write("web/src/b.ts", "export const b = 2\n")
			snap := f.snap()
			st.SetSnap("web/src/a.ts", state.MarkerSnap(snap["web/src/a.ts"], []string{"web"}))
			st.SetSnap("web/src/b.ts", state.MarkerSnap(snap["web/src/b.ts"], []string{"api"}))
			f.write("api/notes.txt", "more\n")
			f.write("api/app.py", "def app(): return 1\n")
			f.write("Dockerfile", "FROM y\n")
			f.write("svc/unused/unused.go", "package unused\n// x\n") // only go's: left out
			others := []*runner.Suite{f.suites[0], f.suites[2], f.suites[3]}
			return f.sel(st, func(in *Input) { in.Suites, in.Others = f.suites[1:2], others })
		}},
		{"diff", func(t *testing.T) *Result {
			f := polyglotFixture(t)
			repo, base := gitFixture(t, f)
			f.write("web/src/a.ts", "export const a = 2\n")
			f.write("migrations/002.sql", "create table u();\n")
			f.write("worker/job.py", "def job(): return 1\n")
			f.write("smates.yml", "version: 1\n# edited\n")
			f.write("svc/unused/unused.go", "package unused\n// x\n")
			f.write("Dockerfile", "FROM y\n")
			f.write("e2e/smoke/new.spec.js", "test(\"new\")\n")
			return f.sel(state.New(), func(in *Input) { in.Repo, in.Base = repo, base })
		}},
		{"diff-unrecorded", func(t *testing.T) *Result {
			// Local use: tests without an entry are decided against the base revision.
			f := polyglotFixture(t)
			st := polyglotState(t, f)
			repo, base := gitFixture(t, f)
			delete(st.Suites["web"].Tests, "web/src/a.test.ts")
			delete(st.Suites["web"].Tests, "web/src/c.test.ts")
			delete(st.Suites["e2e"].Tests, "e2e/login.spec.js")
			f.write("web/src/a.ts", "export const a = 2\n")
			f.write("worker/job.py", "def job(): return 1\n")
			f.write("smates.yml", "version: 1\n# edited\n")
			st.Files = map[string]string(f.snap()) // the state saw the edits already
			return f.sel(st, func(in *Input) { in.Repo, in.Base, in.DiffUnrecorded = repo, base, true })
		}},
		{"gofunc", func(t *testing.T) *Result {
			f := newFixture(t, map[string]string{
				"go.mod":                  "module example.com/m\n\ngo 1.22\n",
				"p/a.go":                  "package p\n\nconst K = 1\n\nfunc A() int { return K }\n",
				"p/b.go":                  "package p\n\nfunc B() int { return 2 }\n",
				"p/a_test.go":             "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { A() }\n",
				"p/b_test.go":             "package p\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { B() }\n\nfunc TestB2(t *testing.T) {}\n",
				"q/q.go":                  "package q\n\nimport _ \"example.com/m/p\"\n",
				"q/q_test.go":             "package q\n\nimport \"testing\"\n\nfunc TestQ(t *testing.T) {}\n",
				"migrations/001.sql":      "create table t();\n",
				"docker-compose.yml":      "services: {}\n",
				"backend/server/server.x": "server\n",
			})
			s := suite(t, config.Suite{
				Runner: "go", Tests: []string{"**/*_test.go"}, Inputs: []string{"go.mod"}, Granularity: config.GranularityFunc,
				Rules: []config.Rule{
					{Tests: []string{"q"}, Deps: []string{"migrations/**"}},
					{Tests: []string{"**/b_test.go"}, Deps: []string{"backend/**"}},
				},
				DependsOn: []config.Dependency{{Paths: []string{"docker-compose.yml"}, Tests: []string{"p/a_test.go"}}},
			})
			s.RepoRoot = f.dir
			f.suites = []*runner.Suite{s}
			st := f.record(map[string][]string{"p#TestA": {"p/a.go"}, "p#TestB": {"p/b.go"}}, nil)
			f.write("p/b.go", "package p\n\nfunc B() int { return 3 }\n") // TestB's body
			f.write("migrations/002.sql", "create table u();\n")
			f.write("docker-compose.yml", "services: {a: {}}\n")
			f.write("q/q_extra_test.go", "package q\n\nimport \"testing\"\n\nfunc TestQ2(t *testing.T) {}\n")
			return f.sel(st, func(in *Input) { in.Repo = &gitx.Repo{Root: f.dir} })
		}},
	}
}

// TestSelectGolden pins the whole observable Result of Select over a varied set of
// inputs. Run with -update to rewrite testdata/select_golden/.
func TestSelectGolden(t *testing.T) {
	for _, c := range goldenCases() {
		t.Run(c.name, func(t *testing.T) {
			got := dumpResult(c.run(t))
			file := filepath.Join("testdata", "select_golden", c.name+".json")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the golden file; run with -update and review the diff\ngot:\n%s", file, got)
			}
		})
	}
}

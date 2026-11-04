package selector

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/static"
)

// fixture is a working tree on disk plus helpers to run selection over it.
type fixture struct {
	t      *testing.T
	dir    string
	files  map[string]string
	cfg    *config.Config
	suites []*runner.Suite
}

func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), files: map[string]string{}}
	for p, c := range files {
		f.write(p, c)
	}
	f.cfg = &config.Config{}
	if err := f.cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) write(p, content string) {
	f.t.Helper()
	abs := filepath.Join(f.dir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.files[p] = content
}

func (f *fixture) remove(p string) {
	os.Remove(filepath.Join(f.dir, filepath.FromSlash(p)))
	delete(f.files, p)
}

func (f *fixture) snap() gitx.Snapshot {
	s := gitx.Snapshot{}
	for p, c := range f.files {
		s[p] = gitx.HashBytes([]byte(c))
	}
	return s
}

func (f *fixture) graph() *static.Graph {
	var files []string
	for p := range f.files {
		files = append(files, p)
	}
	return static.New(f.dir, files, static.Options{})
}

func suite(t *testing.T, cs config.Suite) *runner.Suite {
	t.Helper()
	a, ok := runner.Get(cs.Runner)
	if !ok {
		t.Fatalf("no adapter %q", cs.Runner)
	}
	if cs.Name == "" {
		cs.Name = cs.Runner
	}
	if cs.Root == "" {
		cs.Root = "."
	}
	return &runner.Suite{Suite: cs, Adapter: a}
}

func (f *fixture) sel(st *state.State, mut func(*Input)) *Result {
	f.t.Helper()
	in := &Input{Config: f.cfg, Suites: f.suites, State: st, Snap: f.snap(), Graph: f.graph()}
	if mut != nil {
		mut(in)
	}
	res, err := Select(in)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

// record simulates `smates run --all`: every unit runs, observing its known deps plus
// extra[unit], with the given status (default pass). It returns the new state.
func (f *fixture) record(extra map[string][]string, status map[string]string) *state.State {
	f.t.Helper()
	st := state.New()
	res := f.sel(st, func(in *Input) { in.All = true })
	h := NewHasher(f.snap()).WithConfig(f.cfg)
	for _, sr := range res.Suites {
		obs := map[string]*runner.Observation{}
		for _, tt := range sr.Tests {
			o := &runner.Observation{Files: map[string]bool{}, Status: state.Pass, DurationMs: 10}
			for _, x := range extra[tt.ID] {
				o.Files[x] = true
			}
			if s, ok := status[tt.ID]; ok {
				o.Status = s
			}
			obs[tt.ID] = o
		}
		Record(st, sr, obs, h.ForSuite(sr.Suite), RecordOptions{Commit: "c1", Now: time.Unix(1, 0), Full: true})
	}
	st.Files = map[string]string(f.snap())
	return st
}

func decisions(res *Result) map[string]string {
	out := map[string]string{}
	for _, s := range res.Suites {
		for _, t := range s.Tests {
			r := t.Reason
			if !t.Selected {
				r = "-" + r
			}
			out[t.ID] = r
		}
	}
	return out
}

func webFixture(t *testing.T) *fixture {
	f := newFixture(t, map[string]string{
		"web/src/a.ts":            `export const a = 1`,
		"web/src/a.test.ts":       `import { a } from "./a"`,
		"web/src/b.ts":            `export const b = 1`,
		"web/src/b.test.ts":       `import { b } from "./b"`,
		"web/src/c.test.ts":       `test("c")`,
		"web/vitest.config.ts":    `export default {}`,
		"web/fixtures/users.json": `[]`,
		"web/db/q.test.ts":        `test("db")`,
		"migrations/001.sql":      `create table t()`,
		"package-lock.json":       `{}`,
		"README.md":               `docs`,
		"Dockerfile":              `FROM x`,
		"smates.yml":              `version: 1`,
	})
	f.cfg.Path = "smates.yml"
	f.cfg.Inputs = []string{".tool-versions"}
	f.suites = []*runner.Suite{suite(t, config.Suite{
		Name: "web", Root: "web", Runner: "vitest",
		Tests:  []string{"**/*.test.ts"},
		Inputs: []string{"vitest.config.*", "/package-lock.json"},
		Rules:  []config.Rule{{Tests: []string{"db/**"}, Deps: []string{"migrations/**"}}},
	})}
	return f
}

func TestSelectStateMode(t *testing.T) {
	type tc struct {
		name    string
		edit    func(f *fixture)
		want    map[string]string
		orphans []string
	}
	base := map[string]string{
		"web/src/a.test.ts": "-unaffected", "web/src/b.test.ts": "-unaffected", "web/src/c.test.ts": "-unaffected", "web/db/q.test.ts": "-unaffected",
	}
	with := func(over map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range over {
			out[k] = v
		}
		return out
	}
	all := func(r string) map[string]string {
		out := map[string]string{}
		for k := range base {
			out[k] = r
		}
		return out
	}
	cases := []tc{
		{name: "nothing changed", edit: func(f *fixture) {}, want: base},
		{name: "static dep changed", edit: func(f *fixture) { f.write("web/src/a.ts", "export const a = 2") }, want: with(map[string]string{"web/src/a.test.ts": ReasonChanged})},
		{name: "test file changed", edit: func(f *fixture) { f.write("web/src/c.test.ts", "test('c2')") }, want: with(map[string]string{"web/src/c.test.ts": ReasonChanged})},
		{name: "traced data file changed", edit: func(f *fixture) { f.write("web/fixtures/users.json", "[1]") }, want: with(map[string]string{"web/src/c.test.ts": ReasonChanged})},
		{name: "traced data file deleted (also changes a listing)", edit: func(f *fixture) { f.remove("web/fixtures/users.json") }, want: with(map[string]string{"web/src/c.test.ts": ReasonChanged, "web/src/b.test.ts": ReasonChanged})},
		{name: "new import of a file never recorded", edit: func(f *fixture) {
			f.write("web/src/new.ts", "export const n = 1")
			f.write("web/src/b.test.ts", `import { b } from "./b"; import { n } from "./new"`)
		}, want: with(map[string]string{"web/src/b.test.ts": ReasonChanged})},
		{name: "existing file newly imported", edit: func(f *fixture) {
			f.write("web/src/b.ts", `import { a } from "./a"; export const b = a`)
		}, want: with(map[string]string{"web/src/b.test.ts": ReasonChanged})},
		{name: "suite input changed", edit: func(f *fixture) { f.write("web/vitest.config.ts", "export default {x:1}") }, want: all(ReasonChanged)},
		{name: "repo-relative suite input changed", edit: func(f *fixture) { f.write("package-lock.json", `{"x":1}`) }, want: all(ReasonChanged)},
		{name: "global input added", edit: func(f *fixture) { f.write(".tool-versions", "node 22") }, want: all(ReasonChanged)},
		// smates.yml hashes as the suite's settings: other edits select nothing.
		{name: "config file edited, suite unchanged", edit: func(f *fixture) {
			f.write("smates.yml", "version: 1\n#")
			f.cfg.Orphans, f.cfg.Agent.Model, f.cfg.Flaky.Retry = config.OrphansAll, "m", 1
		}, want: base},
		{name: "suite settings changed", edit: func(f *fixture) {
			f.write("smates.yml", "version: 1\n# env")
			f.suites[0].Env = map[string]string{"TZ": "UTC"}
		}, want: all(ReasonChanged)},
		{name: "env_track changed", edit: func(f *fixture) {
			f.write("smates.yml", "version: 1\n# track")
			f.cfg.EnvTrack = []string{"LANG"}
		}, want: all(ReasonChanged)},
		{name: "rule dep changed", edit: func(f *fixture) { f.write("migrations/001.sql", "create table u()") }, want: with(map[string]string{"web/db/q.test.ts": ReasonChanged})},
		{name: "rule dep added", edit: func(f *fixture) { f.write("migrations/002.sql", "x") }, want: with(map[string]string{"web/db/q.test.ts": ReasonChanged})},
		{name: "new test file", edit: func(f *fixture) { f.write("web/src/d.test.ts", "test('d')") }, want: with(map[string]string{"web/src/d.test.ts": ReasonNew})},
		{name: "test file in listed directory added", edit: func(f *fixture) { f.write("web/fixtures/more.json", "{}") }, want: with(map[string]string{"web/src/b.test.ts": ReasonChanged})},
		{name: "unrelated orphan", edit: func(f *fixture) { f.write("Dockerfile", "FROM y") }, want: base, orphans: []string{"Dockerfile"}},
		{name: "new orphan", edit: func(f *fixture) { f.write("scripts/deploy.sh", "x") }, want: base, orphans: []string{"scripts/deploy.sh"}},
		{name: "deleted orphan", edit: func(f *fixture) { f.remove("Dockerfile") }, want: base, orphans: []string{"Dockerfile"}},
		{name: "default-ignored change", edit: func(f *fixture) { f.write("README.md", "more docs") }, want: base},
		{name: "configured ignore", edit: func(f *fixture) {
			f.cfg.Ignore = []string{"Dockerfile"}
			f.write("Dockerfile", "FROM z")
		}, want: base},
		{name: "vendored change", edit: func(f *fixture) { f.write("web/node_modules/x/index.js", "x") }, want: base},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := webFixture(t)
			st := f.record(map[string][]string{
				"web/src/c.test.ts": {"web/fixtures/users.json"},
				"web/src/b.test.ts": {"web/fixtures/"},
			}, nil)
			c.edit(f)
			res := f.sel(st, nil)
			if res.Mode != "state" {
				t.Fatalf("mode %s", res.Mode)
			}
			if got := decisions(res); !reflect.DeepEqual(got, c.want) {
				t.Errorf("decisions:\n got %v\nwant %v", got, c.want)
			}
			if !reflect.DeepEqual(res.Orphans, c.orphans) {
				t.Errorf("orphans %v, want %v", res.Orphans, c.orphans)
			}
		})
	}
}

func TestSelectChangedListAndDetail(t *testing.T) {
	f := webFixture(t)
	st := f.record(nil, nil)
	f.write("web/src/a.ts", "changed")
	f.write("added.txt", "x")
	f.remove("Dockerfile")
	res := f.sel(st, nil)
	want := []string{"Dockerfile", "added.txt", "web/src/a.ts"}
	if !reflect.DeepEqual(res.Changed, want) {
		t.Errorf("changed %v, want %v", res.Changed, want)
	}
	if !reflect.DeepEqual(res.Orphans, []string{"Dockerfile", "added.txt"}) {
		t.Errorf("orphans %v", res.Orphans)
	}
	var a *Test
	for _, tt := range res.Suites[0].Tests {
		if tt.ID == "web/src/a.test.ts" {
			a = tt
		}
	}
	if a == nil || !reflect.DeepEqual(a.Changed, []string{"web/src/a.ts"}) || a.Duration != 10 {
		t.Errorf("test a: %+v", a)
	}
	if sel, total := res.Counts(); sel != 1 || total != 4 {
		t.Errorf("counts %d/%d", sel, total)
	}
	if res.Suites[0].All {
		t.Error("suite is not fully selected")
	}
	if got := res.Suites[0].Selected(); !reflect.DeepEqual(got, []string{"web/src/a.test.ts"}) {
		t.Errorf("Selected() %v", got)
	}
	if res.SuiteByName("web") != res.Suites[0] || res.SuiteByName("nope") != nil {
		t.Error("SuiteByName")
	}
}

func TestSelectVia(t *testing.T) {
	f := webFixture(t)
	st := f.record(map[string][]string{"web/src/c.test.ts": {"web/fixtures/users.json"}}, nil)
	f.write("web/src/a.ts", "changed")
	f.write("web/src/b.test.ts", "changed")
	f.write("web/fixtures/users.json", "[1]")
	f.write("migrations/001.sql", "changed")
	res := f.sel(st, nil)
	got := map[string]map[string]string{}
	for _, tt := range res.Suites[0].Tests {
		got[tt.ID] = tt.Via
	}
	want := map[string]map[string]string{
		"web/src/a.test.ts": {"web/src/a.ts": ViaImport},
		"web/src/b.test.ts": {"web/src/b.test.ts": ViaTest},
		"web/src/c.test.ts": {"web/fixtures/users.json": ViaTraced},
		"web/db/q.test.ts":  {"migrations/001.sql": ViaInput},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("via %v, want %v", got, want)
	}
	f.write("web/vitest.config.ts", "changed")
	for _, tt := range f.sel(st, nil).Suites[0].Tests {
		if tt.Via["web/vitest.config.ts"] != ViaInput {
			t.Errorf("%s: suite input via %v", tt.ID, tt.Via)
		}
	}
}

func TestSelectFailedTest(t *testing.T) {
	f := webFixture(t)
	st := f.record(nil, map[string]string{"web/src/b.test.ts": state.Fail, "web/src/c.test.ts": state.Unknown})
	res := f.sel(st, nil)
	got := decisions(res)
	if got["web/src/b.test.ts"] != ReasonFailed || got["web/src/c.test.ts"] != ReasonFailed || got["web/src/a.test.ts"] != "-unaffected" {
		t.Errorf("%v", got)
	}
	for _, tt := range res.Suites[0].Tests {
		if tt.ID == "web/src/b.test.ts" && tt.Detail != "last run: fail" {
			t.Errorf("detail %q", tt.Detail)
		}
	}
}

func TestSelectStaticDisabled(t *testing.T) {
	f := webFixture(t)
	no := false
	f.suites[0].Static = &no
	st := f.record(nil, nil)
	// a.ts is not a dep without the static graph nor a trace.
	if deps := st.Suites["web"].Tests["web/src/a.test.ts"].Deps; deps["web/src/a.ts"] != "" {
		t.Fatalf("static deps recorded with static: false: %v", deps)
	}
	f.write("web/src/a.ts", "changed")
	res := f.sel(st, nil)
	if got := decisions(res)["web/src/a.test.ts"]; got != "-unaffected" {
		t.Errorf("got %s", got)
	}
	if !reflect.DeepEqual(res.Orphans, []string{"web/src/a.ts"}) {
		t.Errorf("an untracked change must become an orphan: %v", res.Orphans)
	}
}

func TestSelectAllModes(t *testing.T) {
	f := webFixture(t)
	t.Run("forced", func(t *testing.T) {
		st := f.record(nil, nil)
		f.write("Dockerfile", "changed")
		res := f.sel(st, func(in *Input) { in.All = true })
		if res.Mode != "all" || len(res.Changed) != 0 || len(res.Orphans) != 0 {
			t.Fatalf("%+v", res)
		}
		for id, r := range decisions(res) {
			if r != ReasonAll {
				t.Errorf("%s: %s", id, r)
			}
		}
		if !res.Suites[0].All {
			t.Error("suite All")
		}
	})
	t.Run("no state, no base", func(t *testing.T) {
		res := f.sel(state.New(), nil)
		if res.Mode != "all" || len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "no recorded state") {
			t.Fatalf("%+v", res)
		}
		for id, r := range decisions(res) {
			if r != ReasonAll {
				t.Errorf("%s: %s", id, r)
			}
		}
	})
	t.Run("state with only empty suites counts as empty", func(t *testing.T) {
		st := state.New()
		st.Suite("web")
		if res := f.sel(st, nil); res.Mode != "all" {
			t.Fatalf("mode %s", res.Mode)
		}
	})
}

func TestSelectNoSnapshotInState(t *testing.T) {
	// A state without a snapshot (e.g. only partial runs) detects no changed files and
	// therefore no orphans, but per-test hashes still select changed tests.
	f := webFixture(t)
	st := f.record(nil, nil)
	st.Files = map[string]string{}
	f.write("web/src/a.ts", "x")
	f.write("Dockerfile", "y")
	res := f.sel(st, nil)
	if got := decisions(res)["web/src/a.test.ts"]; got != ReasonChanged {
		t.Errorf("got %s", got)
	}
	if len(res.Changed) != 0 || len(res.Orphans) != 0 {
		t.Errorf("changed %v orphans %v", res.Changed, res.Orphans)
	}
}

func TestSelectRootDirectoryKey(t *testing.T) {
	f := newFixture(t, map[string]string{"a_test.py": "", "test_x.py": "", "data.txt": "1"})
	f.suites = []*runner.Suite{suite(t, config.Suite{Runner: "pytest", Tests: []string{"test_*.py"}})}
	st := f.record(map[string][]string{"test_x.py": {"./"}}, nil)
	if st.Suites["pytest"].Tests["test_x.py"].Deps["./"] == "" {
		t.Fatalf("root directory key not recorded: %v", st.Suites["pytest"].Tests["test_x.py"].Deps)
	}
	f.write("new.txt", "")
	res := f.sel(st, nil)
	if got := decisions(res)["test_x.py"]; got != ReasonChanged {
		t.Errorf("root listing changed: %s", got)
	}
	if len(res.Orphans) != 0 {
		t.Errorf("a file covered by the root directory key is not an orphan: %v", res.Orphans)
	}
	// Changing content (not the listing) of an unrelated file is an orphan.
	f2 := newFixture(t, map[string]string{"test_x.py": "", "data.txt": "1"})
	f2.suites = []*runner.Suite{suite(t, config.Suite{Runner: "pytest", Tests: []string{"test_*.py"}})}
	st2 := f2.record(nil, nil)
	f2.write("data.txt", "2")
	if res := f2.sel(st2, nil); !reflect.DeepEqual(res.Orphans, []string{"data.txt"}) {
		t.Errorf("orphans %v", res.Orphans)
	}
}

func TestSelectGoUnits(t *testing.T) {
	f := newFixture(t, map[string]string{
		"go.mod":            "module example.com/m\n\ngo 1.22\n",
		"pkg/a/a.go":        "package a\n",
		"pkg/a/a_test.go":   "package a\n",
		"pkg/a/b_test.go":   "package a\n",
		"pkg/b/b.go":        "package b\nimport _ \"example.com/m/pkg/a\"\n",
		"pkg/b/b_test.go":   "package b\n",
		"root_test.go":      "package m\n",
		"pkg/a/testdata/x":  "data",
		"pkg/c/c_test.go":   "package c\nimport _ \"example.com/m/pkg/b\"\n",
		"tools/other/go.mo": "",
	})
	f.suites = []*runner.Suite{suite(t, config.Suite{Runner: "go", Tests: []string{"**/*_test.go"}, Inputs: []string{"go.mod"}})}
	st := f.record(nil, nil)
	if got := st.Suites["go"].Tests; len(got) != 4 || got["pkg/a"] == nil || got["."] == nil {
		t.Fatalf("go units: %v", got)
	}
	f.write("pkg/a/a.go", "package a\n// changed\n")
	res := f.sel(st, nil)
	want := map[string]string{"pkg/a": ReasonChanged, "pkg/b": ReasonChanged, "pkg/c": ReasonChanged, ".": "-unaffected"}
	if got := decisions(res); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	// Both test files of pkg/a are known deps of the unit.
	known := res.Suites[0].Known["pkg/a"]
	if !contains(known, "pkg/a/a_test.go") || !contains(known, "pkg/a/b_test.go") {
		t.Errorf("known %v", known)
	}
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func TestSelectMultipleSuitesShareCoverage(t *testing.T) {
	f := newFixture(t, map[string]string{
		"api/test_api.py": "import shared.util\n",
		"shared/util.py":  "",
		"web/a.test.ts":   "",
	})
	f.suites = []*runner.Suite{
		suite(t, config.Suite{Name: "py", Runner: "pytest", Root: "api", Tests: []string{"test_*.py"}}),
		suite(t, config.Suite{Name: "web", Runner: "vitest", Root: "web", Tests: []string{"*.test.ts"}}),
	}
	st := f.record(map[string][]string{"api/test_api.py": {"shared/util.py"}}, nil)
	f.write("shared/util.py", "x = 1")
	res := f.sel(st, nil)
	if d := decisions(res); d["api/test_api.py"] != ReasonChanged || d["web/a.test.ts"] != "-unaffected" {
		t.Errorf("%v", d)
	}
	if len(res.Orphans) != 0 {
		t.Errorf("covered by another suite: %v", res.Orphans)
	}
}

func TestSelectOthersOnlyCover(t *testing.T) {
	f := newFixture(t, map[string]string{
		"api/test_api.py":  "import shared.util\n",
		"shared/util.py":   "",
		"web/a.test.ts":    "",
		"e2e/test_e2e.py":  "",
		"server/handler.x": "",
	})
	f.suites = []*runner.Suite{
		suite(t, config.Suite{Name: "py", Runner: "pytest", Root: "api", Tests: []string{"test_*.py"}}),
		suite(t, config.Suite{Name: "web", Runner: "vitest", Root: "web", Tests: []string{"*.test.ts"}}),
		suite(t, config.Suite{Name: "e2e", Runner: "pytest", Root: "e2e", Tests: []string{"test_*.py"},
			Rules: []config.Rule{{Tests: []string{"test_e2e.py"}, Deps: []string{"server/**"}}}}),
	}
	st := f.record(map[string][]string{"api/test_api.py": {"shared/util.py"}}, nil)
	f.write("shared/util.py", "x = 1")
	f.write("server/handler.x", "y")
	f.write("Dockerfile", "FROM x")
	// --suite web: the suites left out still explain the changes they depend on.
	res := f.sel(st, func(in *Input) { in.Suites, in.Others = f.suites[1:2], []*runner.Suite{f.suites[0], f.suites[2]} })
	if len(res.Suites) != 1 || res.Suites[0].Name != "web" {
		t.Fatalf("only the given suites are decided: %+v", res.Suites)
	}
	if d := decisions(res); d["web/a.test.ts"] != "-unaffected" {
		t.Errorf("%v", d)
	}
	if !reflect.DeepEqual(res.Orphans, []string{"Dockerfile"}) {
		t.Errorf("only a change no suite depends on is an orphan: %v", res.Orphans)
	}
}

func TestSelectOrphansOfSuitesLeftOut(t *testing.T) {
	f := newFixture(t, map[string]string{
		"api/test_api.py": "",
		"api/server.py":   "",
		"web/a.test.ts":   "",
		"web/unused.ts":   "",
		"go_test.go":      "",
	})
	f.suites = []*runner.Suite{
		suite(t, config.Suite{Name: "py", Runner: "pytest", Root: "api", Tests: []string{"test_*.py"}, Static: new(bool)}),
		suite(t, config.Suite{Name: "web", Runner: "vitest", Root: "web", Tests: []string{"*.test.ts"}, Static: new(bool)}),
		suite(t, config.Suite{Name: "root", Runner: "go", Tests: []string{"*_test.go"}, Static: new(bool)}),
	}
	st := f.record(nil, nil)
	f.write("web/unused.ts", "export const x = 1")
	f.write("api/server.py", "x = 1")
	f.write("Dockerfile", "FROM x")
	// --suite web: api/server.py is only py's (and the root suite's) to run.
	res := f.sel(st, func(in *Input) { in.Suites, in.Others = f.suites[1:2], []*runner.Suite{f.suites[0], f.suites[2]} })
	if want := []string{"Dockerfile", "web/unused.ts"}; !reflect.DeepEqual(res.Orphans, want) {
		t.Errorf("orphans %v, want %v", res.Orphans, want)
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[len(res.Notes)-1], "left out by --suite") {
		t.Errorf("notes %v", res.Notes)
	}
	// --suite root: a suite rooted at the repository root may be affected by any
	// change, as the orphan policy over every suite would run it too.
	res = f.sel(st, func(in *Input) { in.Suites, in.Others = f.suites[2:], f.suites[:2] })
	if want := []string{"Dockerfile", "api/server.py", "web/unused.ts"}; !reflect.DeepEqual(res.Orphans, want) {
		t.Errorf("root suite orphans %v, want %v", res.Orphans, want)
	}
}

func TestOwned(t *testing.T) {
	mk := func(root string) *runner.Suite { return &runner.Suite{Suite: config.Suite{Root: root}} }
	all := []*runner.Suite{mk("web"), mk("api"), mk(".")}
	cases := []struct {
		f    string
		n    int
		want bool
	}{
		{"web/a.ts", 1, true},   // web is selected
		{"api/a.py", 1, false},  // only api and the root suite (left out)
		{"Dockerfile", 1, true}, // no suite root contains it: every suite
		{"api/a.py", 3, true},   // every suite selected
	}
	for _, c := range cases {
		if got := NewOwnership(all[:c.n], all[c.n:]).Owned(c.f); got != c.want {
			t.Errorf("Owned(%q, n=%d) = %v, want %v", c.f, c.n, got, c.want)
		}
	}
	// The root suite selected alone owns every file.
	if !NewOwnership([]*runner.Suite{mk(".")}, []*runner.Suite{mk("api")}).Owned("api/a.py") {
		t.Error("root suite owns api/a.py")
	}
}

// ---- diff mode ----

func gitFixture(t *testing.T, f *fixture) (*gitx.Repo, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = f.dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	repo, err := gitx.Open(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo, run("rev-parse", "HEAD")
}

func TestSelectDiffMode(t *testing.T) {
	f := webFixture(t)
	repo, base := gitFixture(t, f)
	f.write("web/src/a.ts", "changed")
	f.write("migrations/002.sql", "new")
	f.write("Dockerfile", "changed")
	f.write("README.md", "changed")
	res := f.sel(state.New(), func(in *Input) { in.Repo, in.Base = repo, base })
	if res.Mode != "diff" {
		t.Fatalf("mode %s", res.Mode)
	}
	want := map[string]string{
		"web/src/a.test.ts": ReasonDiff, "web/db/q.test.ts": ReasonDiff,
		"web/src/b.test.ts": "-unaffected", "web/src/c.test.ts": "-unaffected",
	}
	if got := decisions(res); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if !reflect.DeepEqual(res.Orphans, []string{"Dockerfile"}) {
		t.Errorf("orphans %v", res.Orphans)
	}
	// Bad base: error, not a silent empty selection.
	in := &Input{Repo: repo, Config: f.cfg, Suites: f.suites, State: state.New(), Snap: f.snap(), Graph: f.graph(), Base: "no-such-ref"}
	if _, err := Select(in); err == nil {
		t.Error("unknown base must fail")
	}
}

func TestSelectDiffModeNewTestFile(t *testing.T) {
	f := webFixture(t)
	repo, base := gitFixture(t, f)
	f.write("web/src/z.test.ts", "new test")
	res := f.sel(state.New(), func(in *Input) { in.Repo, in.Base = repo, base })
	if got := decisions(res)["web/src/z.test.ts"]; got != ReasonDiff {
		t.Errorf("a new (untracked) test file must be selected in diff mode: %s", got)
	}
}

// ---- Apply / OrphanAll ----

func twoSuiteResult() *Result {
	mk := func(name, root string, ids ...string) *Suite {
		s := &Suite{Suite: &runner.Suite{Suite: config.Suite{Name: name, Root: root}}, Name: name}
		for _, id := range ids {
			s.Tests = append(s.Tests, &Test{ID: id, Reason: ReasonUnaffected})
		}
		return s
	}
	return &Result{Suites: []*Suite{
		mk("web", "web", "web/a.test.ts", "web/b.test.ts"),
		mk("api", "api", "api/test_x.py"),
		mk("root", ".", "test_root.py"),
	}}
}

func TestApply(t *testing.T) {
	r := twoSuiteResult()
	r.Suites[0].Tests[0].Selected, r.Suites[0].Tests[0].Reason = true, ReasonChanged
	unknown := r.Apply([]Addition{
		{Suite: "web", Test: "web/a.test.ts", Reason: ReasonAgent},
		{Suite: "web", Test: "web/b.test.ts", Reason: ReasonReference, Detail: "d"},
		{Suite: "web", Test: "web/zzz.test.ts", Reason: ReasonAgent},
		{Suite: "nope", Reason: ReasonAgent},
		{Suite: "api", Reason: ReasonOrphan},
	})
	if len(unknown) != 2 || unknown[0].Test != "web/zzz.test.ts" || unknown[1].Suite != "nope" {
		t.Errorf("unknown %v", unknown)
	}
	a, b := r.Suites[0].Tests[0], r.Suites[0].Tests[1]
	if a.Reason != ReasonChanged {
		t.Errorf("an already selected test keeps its reason: %+v", a)
	}
	if !b.Selected || b.Reason != ReasonReference || b.Detail != "d" {
		t.Errorf("b %+v", b)
	}
	if !r.Suites[0].All || !r.Suites[1].All || r.Suites[2].All {
		t.Errorf("All flags: %v %v %v", r.Suites[0].All, r.Suites[1].All, r.Suites[2].All)
	}
	// An empty suite is never "all".
	empty := &Result{Suites: []*Suite{{Name: "e", Suite: &runner.Suite{}}}}
	if u := empty.Apply([]Addition{{Suite: "e"}}); len(u) != 1 || empty.Suites[0].All {
		t.Errorf("empty suite: %v %v", u, empty.Suites[0].All)
	}
}

func TestOrphanAll(t *testing.T) {
	names := func(adds []Addition) []string {
		var out []string
		for _, a := range adds {
			out = append(out, a.Suite)
			if a.Test != "" || a.Reason != ReasonOrphan {
				t.Errorf("addition %+v", a)
			}
		}
		return out
	}
	cases := []struct {
		orphans []string
		want    []string
	}{
		{nil, nil},
		{[]string{"web/Dockerfile"}, []string{"web", "root"}},
		{[]string{"api/schema.sql"}, []string{"api", "root"}},
		{[]string{"webby/x"}, []string{"web", "api", "root"}}, // not under web/
		{[]string{"Dockerfile"}, []string{"web", "api", "root"}},
		{[]string{"web/x", "api/y"}, []string{"web", "api", "root"}},
	}
	for _, c := range cases {
		got := names(twoSuiteResult().OrphanAll(c.orphans))
		// The root suite contains everything and is always picked.
		sort.Strings(got)
		sort.Strings(c.want)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: got %v want %v", c.orphans, got, c.want)
		}
	}
	adds := twoSuiteResult().OrphanAll([]string{"web/one", "web/two"})
	if adds[0].Detail != "unattributed change: web/one" {
		t.Errorf("detail names the first orphan: %q", adds[0].Detail)
	}
}

func TestOrphanFiles(t *testing.T) {
	r := twoSuiteResult()
	r.Suites[0].Tests[0].Selected, r.Suites[0].Tests[0].Reason = true, ReasonChanged
	adds := r.OrphanAll([]string{"web/one", "web/two", "other"})
	files := map[string][]string{}
	for _, a := range adds {
		files[a.Suite] = a.Files
	}
	want := map[string][]string{"web": {"web/one", "web/two", "other"}, "api": {"other"}, "root": {"web/one", "web/two", "other"}}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("files %v, want %v", files, want)
	}
	r.Apply(adds)
	a, b := r.Suites[0].Tests[0], r.Suites[0].Tests[1]
	if a.Reason != ReasonChanged || a.Via != nil {
		t.Errorf("an already selected test keeps its reason: %+v", a)
	}
	if !reflect.DeepEqual(b.Changed, want["web"]) || b.Via["other"] != ViaOrphan {
		t.Errorf("added test: %+v", b)
	}

	r.HandleOrphans([]string{"web/one"}, OrphanReference)
	r.HandleOrphans([]string{"web/one", "other"}, OrphanPolicy)
	if !reflect.DeepEqual(r.OrphanHandling, map[string]string{"web/one": OrphanReference, "other": OrphanPolicy}) {
		t.Errorf("the first handling wins: %v", r.OrphanHandling)
	}
}

func TestOrphanAllRootSuiteOnly(t *testing.T) {
	r := &Result{Suites: []*Suite{
		{Name: "a", Suite: &runner.Suite{Suite: config.Suite{Root: "a"}}},
		{Name: "b", Suite: &runner.Suite{Suite: config.Suite{Root: "b"}}},
	}}
	got := r.OrphanAll([]string{"a/x"})
	if len(got) != 1 || got[0].Suite != "a" {
		t.Errorf("an orphan inside one suite's root only runs that suite: %v", got)
	}
}

// ---- References ----

func TestReferences(t *testing.T) {
	f := newFixture(t, map[string]string{
		"t/test_users.py":         `open("fixtures/users_seed.sql")`,
		"t/test_other.py":         `print("nothing")`,
		"t/test_generic.py":       `import index; load("config.json")`,
		"t/test_bin.py":           ``,
		"t/blob.bin":              `users_seed.sql`, // not source-like: never read
		"t/helper.py":             `ROUTES = "routes.yaml"`,
		"t/test_sel.py":           `"users_seed.sql"`,
		"fixtures/users_seed.sql": ``,
	})
	g := f.graph()
	mk := func(id string, sel bool, deps ...string) (*Test, []string) {
		return &Test{ID: id, Selected: sel}, append([]string{id}, deps...)
	}
	s := &Suite{Name: "py", Suite: &runner.Suite{}, Deps: map[string][]string{}}
	for _, x := range []struct {
		id   string
		sel  bool
		deps []string
	}{
		{"t/test_users.py", false, nil},
		{"t/test_other.py", false, []string{"t/helper.py"}},
		{"t/test_generic.py", false, nil},
		{"t/test_bin.py", false, []string{"t/blob.bin"}},
		{"t/test_sel.py", true, nil},
	} {
		tt, deps := mk(x.id, x.sel, x.deps...)
		s.Tests = append(s.Tests, tt)
		s.Deps[x.id] = deps
	}
	r := &Result{Suites: []*Suite{s}}
	adds, explained := r.References(g, []string{"db/users_seed.sql", "conf/routes.yaml", "lib/index.ts", "x/config.json", "a/ab.sql", "nothing/refers_to_me.txt"})
	got := map[string]string{}
	for _, a := range adds {
		if a.Reason != ReasonReference || a.Suite != "py" {
			t.Errorf("addition %+v", a)
		}
		got[a.Test] = a.Detail
		if a.Test == "t/test_users.py" && !reflect.DeepEqual(a.Files, []string{"db/users_seed.sql"}) {
			t.Errorf("files %v", a.Files)
		}
	}
	if len(got) != 2 || !strings.Contains(got["t/test_users.py"], "db/users_seed.sql (in t/test_users.py)") || !strings.Contains(got["t/test_other.py"], "conf/routes.yaml (in t/helper.py)") {
		t.Errorf("additions %v", got)
	}
	// A bare file-name match selects tests but does not explain the orphan (the tests
	// mention fixtures/users_seed.sql, not db/users_seed.sql): the agent or the orphan
	// policy still sees it. Generic and short names never match.
	if len(explained) != 0 {
		t.Errorf("explained %v", explained)
	}
	// A match naming the directory too explains it (also through an already selected test).
	if _, ex := r.References(g, []string{"fixtures/users_seed.sql"}); !reflect.DeepEqual(ex, []string{"fixtures/users_seed.sql"}) {
		t.Errorf("qualified match: %v", ex)
	}
	// Mentions also lists the tests already selected (References only adds the others),
	// with the file that names the orphan.
	ms, _ := r.Mentions(g, []string{"fixtures/users_seed.sql", "conf/routes.yaml"})
	var mgot []string
	for _, m := range ms {
		mgot = append(mgot, fmt.Sprintf("%s %s %s %t", m.Test, m.Orphan, m.File, m.Qualified))
	}
	if want := []string{
		"t/test_users.py fixtures/users_seed.sql t/test_users.py true",
		"t/test_other.py conf/routes.yaml t/helper.py false",
		"t/test_sel.py fixtures/users_seed.sql t/test_sel.py false",
	}; !reflect.DeepEqual(mgot, want) {
		t.Errorf("mentions:\n%s\nwant\n%s", strings.Join(mgot, "\n"), strings.Join(want, "\n"))
	}
	if adds := r.ReferenceAdditions(ms); len(adds) != 2 || adds[0].Test != "t/test_users.py" || adds[1].Test != "t/test_other.py" {
		t.Errorf("reference additions from mentions: %+v", adds)
	}
	if a, e := r.References(nil, []string{"x"}); a != nil || e != nil {
		t.Error("nil graph")
	}
	if a, e := r.References(g, nil); a != nil || e != nil {
		t.Error("no orphans")
	}
	if a, e := r.References(g, []string{"src/index.ts", "Dockerfile", "a.py"}); a != nil || e != nil {
		t.Errorf("only generic names: %v %v", a, e)
	}
}

// ---- Hasher ----

func TestHasher(t *testing.T) {
	snap := gitx.Snapshot{
		"a.txt":       "h1",
		"dir/b.txt":   "h2",
		"dir/c.txt":   "h3",
		"dir/sub/d":   "h4",
		"dir/sub/e/f": "h5",
		"other/g":     "h6",
	}
	h := NewHasher(snap)
	if h.Hash("a.txt") != "h1" || h.Hash("missing") != "" || h.Hash("dir") != "" {
		t.Error("file keys")
	}
	for _, k := range []string{"./", "dir/", "dir/sub/", "dir/sub/e/", "other/"} {
		if v := h.Hash(k); !strings.HasPrefix(v, "dir:") {
			t.Errorf("%s: %q", k, v)
		}
	}
	if h.Hash("nope/") != "" || h.Hash("dir/b.txt/") != "" {
		t.Error("absent directory keys hash to empty")
	}
	same := func(mut func(gitx.Snapshot), key string) bool {
		s2 := gitx.Snapshot{}
		for k, v := range snap {
			s2[k] = v
		}
		mut(s2)
		return NewHasher(s2).Hash(key) == h.Hash(key)
	}
	// Content changes do not change listings.
	if !same(func(s gitx.Snapshot) { s["dir/b.txt"] = "zz" }, "dir/") {
		t.Error("content change altered dir listing")
	}
	// Changes deep below do not change a listing either (only direct entries matter).
	if !same(func(s gitx.Snapshot) { s["dir/sub/new"] = "x" }, "dir/") {
		t.Error("nested addition altered parent listing")
	}
	if same(func(s gitx.Snapshot) { s["dir/sub/new"] = "x" }, "dir/sub/") {
		t.Error("addition not seen by its directory")
	}
	if same(func(s gitx.Snapshot) { delete(s, "dir/c.txt") }, "dir/") {
		t.Error("removal not seen")
	}
	if same(func(s gitx.Snapshot) { s["dir/new2/x"] = "x" }, "dir/") {
		t.Error("new subdirectory not seen")
	}
	if same(func(s gitx.Snapshot) { s["top.txt"] = "x" }, "./") {
		t.Error("root addition not seen")
	}
	if !same(func(s gitx.Snapshot) { s["dir/zz"] = "x" }, "./") {
		t.Error("root listing changed by a nested addition")
	}
	if same(func(s gitx.Snapshot) { delete(s, "other/g") }, "./") {
		t.Error("root listing must notice a directory disappearing")
	}
	// A directory that only exists through a deeper file is listed with a trailing slash
	// and distinct from a file of the same name.
	if same(func(s gitx.Snapshot) { delete(s, "dir/sub/d"); delete(s, "dir/sub/e/f"); s["dir/sub"] = "file" }, "dir/") {
		t.Error("dir/sub turning from a directory into a file must change the listing")
	}
}

// ---- Record ----

func TestRecord(t *testing.T) {
	snap := gitx.Snapshot{"src/a.ts": "ha", "src/a.test.ts": "ht", "data/x.json": "hx", "static.ts": "hs"}
	h := NewHasher(snap)
	sr := &Suite{Name: "web", Known: map[string][]string{"src/a.test.ts": {"src/a.test.ts", "static.ts"}}}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	st := state.New()
	st.Suite("web").Tests["src/old.test.ts"] = &state.Entry{Status: state.Pass}
	obs := map[string]*runner.Observation{
		"src/a.test.ts": {Files: map[string]bool{
			"src/a.ts": true, "data/": true, "node_modules/x/y.js": true, "src/__pycache__/m.pyc": true, "gone.txt": true,
		}, Status: state.Pass, DurationMs: 5},
		"src/b.test.ts": {Files: map[string]bool{}},
	}
	Record(st, sr, obs, h, RecordOptions{Commit: "c", Now: now})
	e := st.Suites["web"].Tests["src/a.test.ts"]
	want := map[string]string{"src/a.ts": "ha", "src/a.test.ts": "ht", "static.ts": "hs", "data/": h.Hash("data/"), "gone.txt": ""}
	if !reflect.DeepEqual(e.Deps, want) {
		t.Errorf("deps\n got %v\nwant %v", e.Deps, want)
	}
	if e.Status != state.Pass || e.DurationMs != 5 || e.Commit != "c" || !e.At.Equal(now) {
		t.Errorf("entry %+v", e)
	}
	if b := st.Suites["web"].Tests["src/b.test.ts"]; b == nil || b.Status != state.Unknown {
		t.Errorf("empty status records unknown: %+v", b)
	}
	if st.Suites["web"].Tests["src/old.test.ts"] == nil {
		t.Error("partial record must keep other entries")
	}
	Record(st, sr, map[string]*runner.Observation{"src/a.test.ts": {Status: state.Fail}}, h, RecordOptions{Full: true})
	if len(st.Suites["web"].Tests) != 1 || st.Suites["web"].Tests["src/a.test.ts"].Status != state.Fail {
		t.Errorf("full record replaces the suite: %v", st.Suites["web"].Tests)
	}
}

// TestRecordAbsentFile: a file a test tried to read that did not exist is recorded as
// absent, so creating it later selects the test (instead of being an orphan).
func TestRecordAbsentFile(t *testing.T) {
	f := newFixture(t, map[string]string{"test_x.py": "", "test_y.py": ""})
	f.suites = []*runner.Suite{suite(t, config.Suite{Runner: "pytest", Tests: []string{"test_*.py"}})}
	st := f.record(map[string][]string{"test_x.py": {"settings.local.json"}}, nil)
	if v, ok := st.Suites["pytest"].Tests["test_x.py"].Deps["settings.local.json"]; !ok || v != "" {
		t.Fatalf("absent file not recorded: %v", st.Suites["pytest"].Tests["test_x.py"].Deps)
	}
	if res := f.sel(st, nil); decisions(res)["test_x.py"] != "-unaffected" {
		t.Fatalf("still absent: %v", decisions(res))
	}
	f.write("settings.local.json", "{}")
	res := f.sel(st, nil)
	if d := decisions(res); d["test_x.py"] != ReasonChanged || d["test_y.py"] != "-unaffected" {
		t.Errorf("%v", d)
	}
	if len(res.Orphans) != 0 {
		t.Errorf("orphans %v", res.Orphans)
	}
}

func TestHasherExistenceKeys(t *testing.T) {
	h := NewHasher(gitx.Snapshot{"a.txt": "h", "dir/b": "h", "dir/sub/c": "h"})
	cases := map[string]string{
		"a.txt?": "exists", "dir?": "exists", "dir/sub?": "exists", "dir/b?": "exists", ".?": "exists",
		"nope?": "absent", "dir/nope?": "absent", "a.txt/x?": "absent",
	}
	for k, want := range cases {
		if got := h.Hash(k); got != want {
			t.Errorf("Hash(%q) = %q, want %q", k, got, want)
		}
	}
}

// TestExistenceProbe: a test that checked whether a path exists is selected when the
// path appears or disappears, but not when an existing file's content changes; the
// probed path is covered (not an orphan).
func TestExistenceProbe(t *testing.T) {
	f := newFixture(t, map[string]string{"test_x.py": "", "test_y.py": "", "present.cfg": "1"})
	f.suites = []*runner.Suite{suite(t, config.Suite{Runner: "pytest", Tests: []string{"test_*.py"}})}
	st := f.record(map[string][]string{"test_x.py": {"local.cfg?", "present.cfg?"}}, nil)
	f.write("present.cfg", "2")
	res := f.sel(st, nil)
	if d := decisions(res); d["test_x.py"] != "-unaffected" {
		t.Errorf("content change of a probed file: %v", d)
	}
	if !reflect.DeepEqual(res.Orphans, []string{"present.cfg"}) {
		t.Errorf("a probe does not cover content: %v", res.Orphans)
	}
	f.write("local.cfg", "x")
	res = f.sel(st, nil)
	if d := decisions(res); d["test_x.py"] != ReasonChanged || d["test_y.py"] != "-unaffected" {
		t.Errorf("probed file created: %v", d)
	}
	if contains(res.Orphans, "local.cfg") {
		t.Errorf("probed path is covered: %v", res.Orphans)
	}
	f.remove("local.cfg")
	f.remove("present.cfg")
	if d := decisions(f.sel(st, nil)); d["test_x.py"] != ReasonChanged {
		t.Errorf("probed file removed: %v", d)
	}
}

// Go test functions (granularity func) depend on the bodies they executed and on the
// declarations of the rest of their package's closure.
func TestSelectGoFuncUnits(t *testing.T) {
	f := newFixture(t, map[string]string{
		"go.mod":      "module example.com/m\n\ngo 1.22\n",
		"p/a.go":      "package p\n\nconst K = 1\n\nfunc A() int { return K }\n\nfunc A2() int { return 3 }\n",
		"p/b.go":      "package p\n\nfunc B() int { return 2 }\n",
		"p/a_test.go": "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { A() }\n",
		"p/b_test.go": "package p\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { B() }\n",
	})
	s := suite(t, config.Suite{Runner: "go", Tests: []string{"**/*_test.go"}, Inputs: []string{"go.mod"}, Granularity: config.GranularityFunc})
	s.RepoRoot = f.dir
	f.suites = []*runner.Suite{s}
	repo := &gitx.Repo{Root: f.dir}
	sel := func(st *state.State, all bool) *Result {
		return f.sel(st, func(in *Input) { in.Repo, in.All = repo, all })
	}
	st := state.New()
	res := sel(st, true)
	if got := res.Suites[0].Selected(); !reflect.DeepEqual(got, []string{"p#TestA", "p#TestB", "p#(build)"}) {
		t.Fatalf("units %v", got)
	}
	// Its own test file by declaration, not as a whole: the other functions of the file
	// are not its dependencies.
	// In its own package: its own declaration and the signatures of what it names, the
	// package's shape; not the other functions of its file, nor other files.
	if known := res.Suites[0].Known["p#TestA"]; !contains(known, "p/a.go#sig:A") || !contains(known, "p/a_test.go#decl:TestA") || !contains(known, "p/#shape") ||
		contains(known, "p/a_test.go") || contains(known, "p/a.go#decls") || contains(known, "p/b_test.go#decls") || contains(known, "p/b.go#sig:B") {
		t.Errorf("known %v", known)
	}
	if files := res.Suites[0].Files["p#TestA"]; !reflect.DeepEqual(files, []string{"p/a_test.go"}) {
		t.Errorf("files %v", files)
	}
	// What recording observes: the bodies executed (coverage) and the declarations they
	// name (A names K).
	obs := map[string]*runner.Observation{
		"p#TestA": {Files: map[string]bool{depkey.Decl("p/a.go", "A"): true, depkey.Sig("p/a.go", "K"): true}, Status: state.Pass},
		"p#TestB": {Files: map[string]bool{depkey.Decl("p/b.go", "B"): true}, Status: state.Pass},
	}
	obs["p#(build)"] = &runner.Observation{Status: state.Pass}
	Record(st, res.Suites[0], obs, NewHasher(f.snap()).WithRoot(f.dir), RecordOptions{Now: time.Unix(1, 0), Full: true})
	st.Files = map[string]string(f.snap())
	if got := decisions(sel(st, false)); !reflect.DeepEqual(got, map[string]string{"p#TestA": "-unaffected", "p#TestB": "-unaffected", "p#(build)": "-unaffected"}) {
		t.Fatalf("unchanged: %v", got)
	}
	// The compile check depends on every file of the package's closure.
	if known := res.Suites[0].Known["p#(build)"]; !contains(known, "p/a.go") || !contains(known, "p/b.go") || !contains(known, "p/a_test.go") || !contains(known, "p/b_test.go") || contains(known, "p/a.go#decls") {
		t.Errorf("compile check known %v", known)
	}

	// A body only TestA executed: TestA and the compile check.
	f.write("p/a.go", "package p\n\nconst K = 1\n\nfunc A() int { return K + 0 }\n\nfunc A2() int { return 3 }\n")
	res = sel(st, false)
	if got := decisions(res); !reflect.DeepEqual(got, map[string]string{"p#TestA": ReasonChanged, "p#TestB": "-unaffected", "p#(build)": ReasonChanged}) {
		t.Errorf("body: %v", got)
	}
	if len(res.Orphans) != 0 {
		t.Errorf("orphans %v", res.Orphans)
	}
	// A body of the same file no test executed: only the compile check.
	f.write("p/a.go", "package p\n\nconst K = 1\n\nfunc A() int { return K }\n\nfunc A2() int { return 3 + 0 }\n")
	res = sel(st, false)
	if got := decisions(res); !reflect.DeepEqual(got, map[string]string{"p#TestA": "-unaffected", "p#TestB": "-unaffected", "p#(build)": ReasonChanged}) {
		t.Errorf("unexecuted body: %v", got)
	}
	if len(res.Orphans) != 0 {
		t.Errorf("orphans %v", res.Orphans)
	}
	// A test body: that test only.
	f.write("p/a.go", "package p\n\nconst K = 1\n\nfunc A() int { return K }\n\nfunc A2() int { return 3 }\n")
	f.write("p/b_test.go", "package p\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { B(); B() }\n")
	if got := decisions(sel(st, false)); !reflect.DeepEqual(got, map[string]string{"p#TestA": "-unaffected", "p#TestB": ReasonChanged, "p#(build)": ReasonChanged}) {
		t.Errorf("test body: %v", got)
	}
	f.write("p/b_test.go", "package p\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { B() }\n")
	f.write("p/a.go", "package p\n\nconst K = 2\n\nfunc A() int { return K }\n\nfunc A2() int { return 3 }\n")
	res = sel(st, false)
	// A constant only TestA's code names: TestA, not TestB.
	if got := decisions(res); !reflect.DeepEqual(got, map[string]string{"p#TestA": ReasonChanged, "p#TestB": "-unaffected", "p#(build)": ReasonChanged}) {
		t.Errorf("declarations: %v", got)
	}
	for _, tt := range res.Suites[0].Tests {
		if tt.ID == "p#TestA" && !reflect.DeepEqual(tt.Changed, []string{"p/a.go#sig:K"}) {
			t.Errorf("TestA changed %v via %v", tt.Changed, tt.Via)
		}
	}
	f.write("p/a.go", "package p\n\nconst K = 1\n\nfunc A() int { return K }\n\nfunc A2() int { return 3 }\n")

	// A new file of ordinary declarations selects only its own new tests (and the
	// compile check); one that runs code for every test selects every function.
	f.write("p/c_test.go", "package p\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n")
	f.write("p/c.go", "package p\n\nfunc C() int { return 3 }\n")
	if got := decisions(sel(st, false)); !reflect.DeepEqual(got, map[string]string{"p#TestA": "-unaffected", "p#TestB": "-unaffected", "p#TestC": ReasonNew, "p#(build)": ReasonChanged}) {
		t.Errorf("new files: %v", got)
	}
	f.write("p/c.go", "package p\n\nfunc C() int { return 3 }\n\nfunc init() { C() }\n")
	if got := decisions(sel(st, false)); !reflect.DeepEqual(got, map[string]string{"p#TestA": ReasonChanged, "p#TestB": ReasonChanged, "p#TestC": ReasonNew, "p#(build)": ReasonChanged}) {
		t.Errorf("new file with init: %v", got)
	}
}

func TestCoversDeclarations(t *testing.T) {
	deps := map[string]bool{"p/a.go#decls": true}
	if got := depkey.Covers(deps, "p/a.go", false); got != depkey.CoverDecls {
		t.Errorf("Covers = %q", got)
	}
	// Without a recording, a changed file changes its declarations.
	tt := &Test{}
	decide(tt, ModeDiff, nil, []string{"p/a.go#decls", "p/"}, nil, map[string]bool{"p/a.go": true})
	if !tt.Selected || !reflect.DeepEqual(tt.Changed, []string{"p/a.go#decls"}) {
		t.Errorf("diff mode: %+v", tt)
	}
}

func TestUntested(t *testing.T) {
	mk := func(runner_, root string, static bool) *Suite {
		return &Suite{Suite: &runner.Suite{Suite: config.Suite{Runner: runner_, Root: root, Static: &static}}}
	}
	r := &Result{Mode: ModeState, Suites: []*Suite{mk("go", ".", true), mk("vitest", "web", true), mk("pytest", "py", false)}}
	r.Suites[0].Suite.Exclude = []string{"e2e/**"}
	got := r.Untested([]string{
		"cmd/tool/main.go",   // Go code under the go suite
		"web/src/Button.tsx", // TS under the vitest suite
		"web/vite.config.ts", // tool configuration
		"web/src/types.d.ts", // declarations
		"web/.eslintrc.js",   // rc file
		"scripts/build.ts",   // not under a JS suite root
		"py/app.py",          // static analysis off for that suite
		"web/setup-tests.ts", // setup file
		"Dockerfile",         // not code
		"web/src/data.json",  // not code
		"pkg/testdata/in.go", // read as data by tests
		"web/__mocks__/a.ts", // mocks
		"e2e/fixture/x/x.go", // excluded from the go suite
		"e2e/new/x.go",       // excluded from the go suite
	})
	if want := []string{"cmd/tool/main.go", "web/src/Button.tsx"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Untested = %v, want %v", got, want)
	}
	// A suite with a test that has no recorded entry knows only its import closure.
	// (The go suite is rooted at the repository root: unrecorded, it blocks all.)
	r.Suites[1].Unrecorded = true
	if got := r.Untested([]string{"cmd/tool/main.go", "web/src/Button.tsx"}); !reflect.DeepEqual(got, []string{"cmd/tool/main.go"}) {
		t.Errorf("unrecorded vitest suite: %v", got)
	}
	r.Suites[0].Unrecorded = true
	if got := r.Untested([]string{"cmd/tool/main.go"}); len(got) != 0 {
		t.Errorf("unrecorded root suite: %v", got)
	}
	r.Suites[0].Unrecorded, r.Suites[1].Unrecorded = false, false
	// Without a recorded state only the static closure is known.
	r.Mode = ModeDiff
	if got := r.Untested([]string{"cmd/tool/main.go"}); len(got) != 0 {
		t.Errorf("diff mode: %v", got)
	}
}

// An entry recorded before smates.yml was hashed as the suite's settings holds the
// file's content hash: it still matches while the file is unchanged.
func TestSelectLegacyConfigHash(t *testing.T) {
	f := webFixture(t)
	st := f.record(nil, nil)
	blob := f.snap()["smates.yml"]
	for _, e := range st.Suites["web"].Tests {
		delete(e.Deps, depkey.Config("smates.yml"))
		e.Deps["smates.yml"] = blob
	}
	if d := decisions(f.sel(st, nil)); d["web/src/a.test.ts"] != "-unaffected" {
		t.Errorf("legacy hash, file unchanged: %v", d)
	}
	f.write("smates.yml", "version: 1\n#")
	if d := decisions(f.sel(st, nil)); d["web/src/a.test.ts"] != ReasonChanged {
		t.Errorf("legacy hash, file changed: %v", d)
	}
}

// A test that reads smates.yml itself depends on the file's content.
func TestSelectConfigReadByTest(t *testing.T) {
	f := webFixture(t)
	st := f.record(map[string][]string{"web/src/c.test.ts": {"smates.yml"}}, nil)
	f.write("smates.yml", "version: 1\n# comment")
	want := map[string]string{"web/src/a.test.ts": "-unaffected", "web/src/b.test.ts": "-unaffected", "web/src/c.test.ts": ReasonChanged, "web/db/q.test.ts": "-unaffected"}
	res := f.sel(st, nil)
	if d := decisions(res); !reflect.DeepEqual(d, want) {
		t.Errorf("got %v want %v", d, want)
	}
	if len(res.Orphans) != 0 {
		t.Errorf("smates.yml is not an orphan: %v", res.Orphans)
	}
	// The suite's settings changing reports the file, not the internal key.
	f.suites[0].Env = map[string]string{"TZ": "UTC"}
	for _, s := range f.sel(st, nil).Suites {
		for _, tt := range s.Tests {
			if tt.ID == "web/src/c.test.ts" {
				continue // also read the edited file: traced
			}
			if !reflect.DeepEqual(tt.Changed, []string{"smates.yml"}) || tt.Via["smates.yml"] != ViaInput {
				t.Errorf("%s: changed %v via %v", tt.ID, tt.Changed, tt.Via)
			}
		}
	}
}

func TestSelectMarksUnrecordedSuites(t *testing.T) {
	f := webFixture(t)
	st := f.record(nil, nil)
	if res := f.sel(st, nil); res.Suites[0].Unrecorded {
		t.Error("every test recorded")
	}
	delete(st.Suites["web"].Tests, "web/src/a.test.ts")
	if res := f.sel(st, nil); !res.Suites[0].Unrecorded {
		t.Error("a test without an entry")
	}
}

func TestOwnership(t *testing.T) {
	mk := func(name, root string) *runner.Suite {
		return &runner.Suite{Suite: config.Suite{Name: name, Root: root}}
	}
	a, b, root := mk("a", "a"), mk("b", "b"), mk("root", ".")
	cases := []struct {
		picked, left []*runner.Suite
		f            string
		owners       []string
		owned, only  bool
	}{
		{[]*runner.Suite{a}, []*runner.Suite{b}, "a/x", []string{"a"}, true, true},
		{[]*runner.Suite{a}, []*runner.Suite{b}, "b/x", []string{"b"}, false, false},
		{[]*runner.Suite{a}, []*runner.Suite{b}, "Dockerfile", []string{"a", "b"}, true, false},
		{[]*runner.Suite{a, b}, nil, "Dockerfile", []string{"a", "b"}, true, true},
		{[]*runner.Suite{a}, []*runner.Suite{root}, "a/x", []string{"a", "root"}, true, false},
		{[]*runner.Suite{root}, []*runner.Suite{a}, "a/x", []string{"root", "a"}, true, false},
		{[]*runner.Suite{root}, []*runner.Suite{a}, "z", []string{"root", "a"}, true, false},
		{nil, []*runner.Suite{a}, "a/x", []string{"a"}, false, false},
	}
	for _, c := range cases {
		o := NewOwnership(c.picked, c.left)
		if got := o.Owners(c.f); !reflect.DeepEqual(got, c.owners) {
			t.Errorf("Owners(%q) = %v, want %v", c.f, got, c.owners)
		}
		if got := o.Owned(c.f); got != c.owned {
			t.Errorf("Owned(%q) = %v, want %v", c.f, got, c.owned)
		}
		if got := o.OwnedOnly(c.f); got != c.only {
			t.Errorf("OwnedOnly(%q) = %v, want %v", c.f, got, c.only)
		}
	}
	if got := NewOwnership([]*runner.Suite{b, a}, []*runner.Suite{root}).Picked(); !reflect.DeepEqual(got, []string{"b", "a"}) {
		t.Errorf("Picked = %v", got)
	}
}

// TestSelectDiffUnrecordedReadsData: a unit with no recorded entry, decided against
// Base by its static closure, is still selected when a file of its suite changed that
// other tests read (their recordings explain it, so it is no orphan) and that no
// static closure explains: in diff mode it would have been an orphan.
func TestSelectDiffUnrecordedReadsData(t *testing.T) {
	f := webFixture(t)
	repo, base := gitFixture(t, f)
	st := f.record(map[string][]string{"web/src/c.test.ts": {"web/fixtures/users.json"}}, nil)
	delete(st.Suites["web"].Tests, "web/db/q.test.ts")
	diff := func(in *Input) { in.Repo, in.Base, in.DiffUnrecorded = repo, base, true }
	if d := decisions(f.sel(st, diff))["web/db/q.test.ts"]; d != "-unaffected" {
		t.Errorf("nothing changed: %s", d)
	}
	f.write("web/src/a.ts", "changed") // in a static closure: decided by it
	if d := decisions(f.sel(st, diff)); d["web/db/q.test.ts"] != "-unaffected" || d["web/src/a.test.ts"] != ReasonChanged {
		t.Errorf("import changed: %v", d)
	}
	f.write("web/fixtures/users.json", "[1]")
	res := f.sel(st, diff)
	want := map[string]string{
		"web/src/a.test.ts": ReasonChanged, "web/src/b.test.ts": "-unaffected",
		"web/src/c.test.ts": ReasonChanged, "web/db/q.test.ts": ReasonNew,
	}
	if d := decisions(res); !reflect.DeepEqual(d, want) {
		t.Errorf("got %v want %v", d, want)
	}
	if len(res.Orphans) != 0 {
		t.Errorf("orphans %v", res.Orphans)
	}
	// Without an orphan policy (orphans: ignore, --orphans=skip) such a change selects
	// nothing, as it would as an orphan.
	res = f.sel(st, func(in *Input) { diff(in); in.NoOrphanPolicy = true })
	if d := decisions(res)["web/db/q.test.ts"]; d != "-unaffected" {
		t.Errorf("no orphan policy: %s", d)
	}
	if len(res.Notes) == 0 {
		t.Error("no note for the change left unhandled")
	}
}

// TestConfigHashEnvIgnore: adding a name to the top-level env_ignore (the remedy for a
// noisy variable) selects nothing; the list is not part of the suite's settings.
func TestConfigHashEnvIgnore(t *testing.T) {
	f := webFixture(t)
	st := f.record(nil, nil)
	f.cfg.EnvIgnore = []string{"APP_SECRET"}
	if d := decisions(f.sel(st, nil)); d["web/src/a.test.ts"] != "-unaffected" {
		t.Errorf("env_ignore added: %v", d)
	}
}

// TestOrphanEditedMarkedFile: a file whose snapshot entry is a marker of --suite runs
// existed, so editing it is no existence change a listing could explain.
func TestOrphanEditedMarkedFile(t *testing.T) {
	f := webFixture(t)
	st := f.record(map[string][]string{"web/src/b.test.ts": {"web/fixtures/"}}, nil)
	st.SetSnap("web/fixtures/users.json", state.MarkerSnap(st.Files["web/fixtures/users.json"], []string{"other"}))
	f.write("web/fixtures/users.json", "[1]")
	if res := f.sel(st, nil); !reflect.DeepEqual(res.Orphans, []string{"web/fixtures/users.json"}) {
		t.Errorf("orphans %v", res.Orphans)
	}
	// A marker of the absent file: the file appeared, which the listing explains.
	st.SetSnap("web/fixtures/users.json", state.MarkerSnap("", []string{"other"}))
	if res := f.sel(st, nil); len(res.Orphans) != 0 {
		t.Errorf("orphans %v", res.Orphans)
	}
}

// TestSelectFileNamedLikeProbe: a file whose name ends in "?" parses as an existence
// check, yet editing it selects the tests that read it.
func TestSelectFileNamedLikeProbe(t *testing.T) {
	f := webFixture(t)
	f.write("web/fixtures/odd?", "1")
	st := f.record(map[string][]string{"web/src/c.test.ts": {"web/fixtures/odd?"}}, nil)
	if d := decisions(f.sel(st, nil))["web/src/c.test.ts"]; d != "-unaffected" {
		t.Fatalf("unchanged: %s", d)
	}
	f.write("web/fixtures/odd?", "2")
	res := f.sel(st, nil)
	if d := decisions(res)["web/src/c.test.ts"]; d != ReasonChanged {
		t.Errorf("edited: %s", d)
	}
	if len(res.Orphans) != 0 {
		t.Errorf("orphans %v", res.Orphans)
	}
}

// An executable Go file's hash carries the executable marker; its declarations are
// still hashed from its content.
func TestDeclsOfExecutableFile(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\nfunc A() int { return 1 }\n"
	if err := os.MkdirAll(filepath.Join(dir, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p", "a.go"), []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	blob := gitx.HashBytes([]byte(src))
	plain := NewHasher(gitx.Snapshot{"p/a.go": blob}).WithRoot(dir).Hash("p/a.go#decls")
	exec := NewHasher(gitx.Snapshot{"p/a.go": blob + gitx.ExecSuffix}).WithRoot(dir).Hash("p/a.go#decls")
	if !strings.HasPrefix(plain, "decls:") || exec != plain {
		t.Errorf("decls of an executable file %q, of a plain one %q", exec, plain)
	}
}

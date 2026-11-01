package selector

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// recordRun records one run of unit observing files, hashed after the run (run is
// called between the start of the run and recording, like the tests writing files).
// The hasher has the ignore globs, as in `smates run` and selection.
func (f *fixture) recordRun(st *state.State, unit string, files []string, run func()) {
	f.t.Helper()
	res := f.sel(st, func(in *Input) { in.Repo, in.All = &gitx.Repo{Root: f.dir}, true })
	sr := res.Suites[0]
	h := NewHasher(f.snap(), IgnoreGlobs(f.cfg)...).WithRoot(f.dir).WithConfig(f.cfg)
	h.PinBefore(st, []string{sr.Name}, time.Now())
	if run != nil {
		run()
	}
	o := &runner.Observation{Status: state.Pass, Files: map[string]bool{}}
	for _, p := range files {
		o.Files[p] = true
	}
	Record(st, sr, map[string]*runner.Observation{unit: o}, h.ForSuite(sr.Suite), RecordOptions{Now: time.Now()})
	st.Files = map[string]string(f.snap())
}

// A test lists a directory git ignores (generated fixtures) and reads what it finds: a
// file added there selects it; one removed too.
func TestListingOfIgnoredDir(t *testing.T) {
	f := webFixture(t)
	const unit = "web/src/c.test.ts"
	f.writeUntracked("web/gen/one.json", "1")
	st := f.record(nil, nil)
	f.recordRun(st, unit, []string{"web/gen/", "web/gen/one.json"}, nil)
	sel := func() string {
		return decisions(f.sel(st, func(in *Input) { in.Repo = &gitx.Repo{Root: f.dir} }))[unit]
	}
	if d := sel(); d != "-unaffected" {
		t.Fatalf("nothing changed: %s", d)
	}
	f.writeUntracked("web/gen/two.json", "2")
	if d := sel(); d != ReasonChanged {
		t.Errorf("file added to a listed ignored directory: %s", d)
	}
	os.Remove(filepath.Join(f.dir, "web/gen/two.json"))
	if d := sel(); d != "-unaffected" {
		t.Errorf("back as recorded: %s", d)
	}
	os.Remove(filepath.Join(f.dir, "web/gen/one.json"))
	if d := sel(); d != ReasonChanged {
		t.Errorf("file removed from a listed ignored directory: %s", d)
	}
}

// A test lists a git-ignored directory of markdown fixtures (a spec suite downloaded
// into the tree) and renders each file: adding one selects it, although the ignore
// globs (DefaultIgnore's **/*.md) match it. Listing the directory is the read.
func TestListingOfIgnoredDirIgnoreGlobs(t *testing.T) {
	f := webFixture(t)
	const unit = "web/src/c.test.ts"
	f.writeUntracked("web/spec/one.md", "# one")
	st := f.record(nil, nil)
	f.recordRun(st, unit, []string{"web/spec/", "web/spec/one.md"}, nil)
	sel := func() string {
		return decisions(f.sel(st, func(in *Input) { in.Repo = &gitx.Repo{Root: f.dir} }))[unit]
	}
	if d := sel(); d != "-unaffected" {
		t.Fatalf("nothing changed: %s", d)
	}
	f.writeUntracked("web/spec/two.md", "# two")
	if d := sel(); d != ReasonChanged {
		t.Errorf("markdown fixture added to a listed ignored directory: %s (want changed)", d)
	}
}

// A test that writes a new timestamped file into the ignored directory it lists, every
// run: the listing is recorded as the run left it, so the test settles after one run.
func TestListingWrittenByRunSettles(t *testing.T) {
	f := webFixture(t)
	const unit = "web/src/c.test.ts"
	st := f.record(nil, nil)
	for i := range 4 {
		if i > 0 {
			if d := decisions(f.sel(st, func(in *Input) { in.Repo = &gitx.Repo{Root: f.dir} }))[unit]; d != "-unaffected" {
				t.Fatalf("run %d: %s, want it skipped", i, d)
			}
		}
		f.recordRun(st, unit, []string{"web/out/"}, func() {
			f.writeUntracked(fmt.Sprintf("web/out/report-%d.json", time.Now().UnixNano()), "{}")
		})
	}
}

// The listing of a directory the snapshot lists is the snapshot's alone, as an older
// smates recorded it: entries git ignores inside it (logs, test binaries, editor and
// Finder files, an ignored subdirectory) are not part of it. A directory the snapshot
// has no file below is listed from disk, without vendored entries (the ignore globs do
// not apply).
func TestListingTrackedDir(t *testing.T) {
	f := webFixture(t)
	h := func(key string) string {
		return NewHasher(f.snap(), IgnoreGlobs(f.cfg)...).WithRoot(f.dir).Hash(key)
	}
	sum := sha1.Sum([]byte(strings.Join([]string{"a.test.ts", "a.ts", "b.test.ts", "b.ts", "c.test.ts"}, "\n")))
	want := "dir:" + hex.EncodeToString(sum[:])
	if got := NewHasher(f.snap()).Hash("web/src/"); got != want {
		t.Fatalf("snapshot listing %s, want %s", got, want)
	}
	for _, p := range []string{"web/src/run.log", "web/src/.DS_Store", "web/src/cover.out", "web/src/.a.ts.swp",
		"web/src/a.ts~", "web/src/gen/x.ts", "web/src/node_modules/x/index.js"} {
		f.writeUntracked(p, "x")
		if got := h("web/src/"); got != want {
			t.Errorf("after %s: %s, want the snapshot's %s", p, got, want)
		}
	}
	if err := os.MkdirAll(filepath.Join(f.dir, "web/src/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := h("web/src/"); got != want {
		t.Errorf("empty subdirectory listed: %s", got)
	}
	// A directory git ignores entirely.
	gen := h("web/src/gen/")
	if gen == "" {
		t.Fatal("ignored directory: no listing")
	}
	f.writeUntracked("web/src/gen/node_modules/x.js", "")
	if got := h("web/src/gen/"); got != gen {
		t.Errorf("vendored entry listed: %s, want %s", got, gen)
	}
	// The ignore globs do not apply to a listing from disk: listing is the read.
	f.writeUntracked("web/src/gen/NOTES.md", "") // ignored by default (**/*.md)
	if got := h("web/src/gen/"); got == gen {
		t.Errorf("file the ignore globs match added to an ignored directory: listing unchanged")
	}
	gen = h("web/src/gen/")
	f.writeUntracked("web/src/gen/y.ts", "")
	if got := h("web/src/gen/"); got == gen {
		t.Errorf("file added to an ignored directory: listing unchanged")
	}
	// Existence checks of paths outside the snapshot still go to disk (and are pinned).
	hh := NewHasher(f.snap()).WithRoot(f.dir)
	if got := hh.Hash(depkey.Existence("web/src/gen")); got != "exists" {
		t.Errorf("existence of an ignored directory: %s", got)
	}
	if got := hh.Hash(depkey.Existence("web/missing")); got != "absent" {
		t.Errorf("existence of a missing directory: %s", got)
	}
	if got := hh.Hash("web/missing/"); got != "" {
		t.Errorf("listing of a missing directory: %q", got)
	}
}

// With granularity: func every test function depends on its package's listing. One
// test writes a git-ignored log into the package and reads it: on fresh checkouts
// (the log absent before the run) only that test runs again, and files git ignores
// in the package (.DS_Store, cover.out, a test binary) select nothing.
func TestListingGoFuncIgnoredFiles(t *testing.T) {
	f := newFixture(t, map[string]string{
		"go.mod":      "module example.com/m\n\ngo 1.22\n",
		"p/a.go":      "package p\n\nfunc A() int { return 1 }\n",
		"p/a_test.go": "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) { A() }\n",
		"p/b_test.go": "package p\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { A() }\n",
	})
	s := suite(t, config.Suite{Runner: "go", Tests: []string{"**/*_test.go"}, Granularity: config.GranularityFunc})
	s.RepoRoot = f.dir
	f.suites = []*runner.Suite{s}
	repo := &gitx.Repo{Root: f.dir}
	st := state.New()
	run := func(units []string) {
		res := f.sel(st, func(in *Input) { in.Repo, in.All = repo, len(units) == 0 })
		h := NewHasher(f.snap()).WithRoot(f.dir)
		h.PinBefore(st, []string{s.Name}, time.Now())
		obs := map[string]*runner.Observation{}
		for _, tt := range res.Suites[0].Tests {
			if !tt.Selected {
				continue
			}
			o := &runner.Observation{Files: map[string]bool{"p/a.go": true}, Status: state.Pass}
			if tt.ID == "p#TestA" {
				f.writeUntracked("p/run.log", strconv.FormatInt(time.Now().UnixNano(), 10))
				o.Files["p/run.log"] = true
			}
			obs[tt.ID] = o
		}
		Record(st, res.Suites[0], obs, h.ForSuite(s), RecordOptions{Now: time.Now()})
		st.Files = map[string]string(f.snap())
	}
	sel := func() map[string]string {
		return decisions(f.sel(st, func(in *Input) { in.Repo = repo }))
	}
	run(nil)
	if got := sel(); !reflect.DeepEqual(got, map[string]string{"p#TestA": "-unaffected", "p#TestB": "-unaffected", "p#(build)": "-unaffected"}) {
		t.Fatalf("after the run: %v", got)
	}
	for i := range 3 {
		os.Remove(filepath.Join(f.dir, "p/run.log")) // a fresh checkout
		got := sel()
		if want := map[string]string{"p#TestA": ReasonChanged, "p#TestB": "-unaffected", "p#(build)": "-unaffected"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("fresh checkout %d: %v, want %v", i, got, want)
		}
		run([]string{"p#TestA"})
	}
	for _, p := range []string{"p/.DS_Store", "p/cover.out", "p/p.test", "p/cpu.prof", "p/.a.go.swp", "p/.#a.go", "p/a.go~"} {
		f.writeUntracked(p, "x")
	}
	if got := sel(); !reflect.DeepEqual(got, map[string]string{"p#TestA": "-unaffected", "p#TestB": "-unaffected", "p#(build)": "-unaffected"}) {
		t.Errorf("ignored files in the package: %v", got)
	}
}

// A command smates cannot see into ran in a directory git ignores: its tree is the
// paths and sizes of its files on disk. A rebuilt file of the same size selects
// nothing, a new file or a size change does; a tree the snapshot lists leaves its
// git-ignored files out, as before.
func TestTreeOfIgnoredDir(t *testing.T) {
	f := webFixture(t)
	for i := range 50 {
		f.writeUntracked(fmt.Sprintf("web/build/chunk-%02d.js", i), "aaaa")
	}
	const unit = "web/src/c.test.ts"
	key := depkey.Tree("web/build")
	st := f.record(nil, nil)
	f.recordRun(st, unit, []string{key}, nil)
	sel := func() string {
		return decisions(f.sel(st, func(in *Input) { in.Repo = &gitx.Repo{Root: f.dir} }))[unit]
	}
	if d := sel(); d != "-unaffected" {
		t.Fatalf("nothing changed: %s", d)
	}
	f.writeUntracked("web/build/chunk-07.js", "bbbb") // rebuilt, same size
	f.writeUntracked("web/build/node_modules/x.js", "")
	if d := sel(); d != "-unaffected" {
		t.Errorf("same-size rebuild: %s", d)
	}
	f.writeUntracked("web/build/chunk-07.js", "bbbbb")
	if d := sel(); d != ReasonChanged {
		t.Errorf("size changed: %s", d)
	}
	f.writeUntracked("web/build/chunk-07.js", "bbbb")
	f.writeUntracked("web/build/extra.js", "")
	if d := sel(); d != ReasonChanged {
		t.Errorf("file added: %s", d)
	}
	os.Remove(filepath.Join(f.dir, "web/build/extra.js"))
	if d := sel(); d != "-unaffected" {
		t.Fatalf("back as recorded: %s", d)
	}
	f.writeUntracked("web/build/NOTES.md", "") // the ignore globs (**/*.md) do not apply
	if d := sel(); d != ReasonChanged {
		t.Errorf("file the ignore globs match added: %s", d)
	}
	os.Remove(filepath.Join(f.dir, "web/build/NOTES.md"))
	tree := func(dir string) string { return NewHasher(f.snap()).WithRoot(f.dir).Hash(depkey.Tree(dir)) }
	f.writeUntracked("web/src/gen/x.ts", "")
	if got, want := tree("web/src"), NewHasher(f.snap()).Hash(depkey.Tree("web/src")); got != want {
		t.Errorf("tracked tree with ignored files: %s, want %s", got, want)
	}
	// Beyond the cap the hash is a constant.
	defer func(n int) { maxDiskTree = n }(maxDiskTree)
	maxDiskTree = 10
	big := tree("web/build")
	f.writeUntracked("web/build/more.js", "x")
	if got := tree("web/build"); got != big || !strings.HasPrefix(got, "tree:over:") {
		t.Errorf("tree over the cap: %s then %s", big, got)
	}
}

// The state is recorded on CI (no virtualenv: python3 -m pytest) and pulled to a laptop
// with a .venv. Nothing in smates.yml or the repository changed: no test runs. An
// entry recorded by an older smates (over the suite with its defaults) still matches
// while the defaults are the same; an edit of the suite's entry selects.
func TestConfigHashWithoutDefaults(t *testing.T) {
	f := newFixture(t, map[string]string{
		"api/test_x.py": "def test_x(): pass\n",
		"api/x.py":      "X = 1\n",
		"smates.yml":    "version: 1\nsuites:\n  - name: py\n    root: api\n    runner: pytest\n",
	})
	f.cfg.Path = "smates.yml"
	repo := &gitx.Repo{Root: f.dir}
	cs := config.Suite{Name: "py", Root: "api", Runner: "pytest"}
	mk := func() string {
		su, err := runner.NewSuite(repo, cs)
		if err != nil {
			t.Fatal(err)
		}
		f.suites = []*runner.Suite{su}
		return su.Command
	}
	const unit = "api/test_x.py"
	key := depkey.Config("smates.yml")
	ci := mk()
	st := f.record(nil, nil)
	legacy := ConfigHash(f.cfg, &f.suites[0].Suite) // the defaulted suite, as recorded before
	f.writeUntracked("api/.venv/bin/python", "#!/bin/sh\n")
	if laptop := mk(); laptop == ci {
		t.Fatalf("the default command does not depend on the virtualenv: %q", laptop)
	}
	if d := decisions(f.sel(st, nil)); d[unit] != "-unaffected" {
		t.Errorf("laptop with a virtualenv: %v", d)
	}
	// An older smates' entry: unchanged while the defaults are the ones it hashed.
	st.Suites["py"].Tests[unit].Deps[key] = legacy
	if d := decisions(f.sel(st, nil)); d[unit] != ReasonChanged {
		t.Errorf("legacy hash, other defaults: %v", d)
	}
	os.RemoveAll(filepath.Join(f.dir, "api/.venv"))
	mk()
	if d := decisions(f.sel(st, nil)); d[unit] != "-unaffected" {
		t.Errorf("legacy hash, same defaults: %v", d)
	}
	cs.Env = map[string]string{"TZ": "UTC"}
	mk()
	if d := decisions(f.sel(st, nil)); d[unit] != ReasonChanged {
		t.Errorf("suite entry edited: %v", d)
	}
}

// A committed node_modules file matched by an input glob is a known dependency that
// Record never writes (vendored): it must not count as changed on every selection.
func TestKnownDepsSkipVendored(t *testing.T) {
	f := webFixture(t)
	f.write("web/package.json", `{}`)
	f.write("web/node_modules/x/package.json", `{}`)
	f.suites[0].Inputs = append(f.suites[0].Inputs, "**/package.json")
	st := f.record(nil, nil)
	res := f.sel(st, nil)
	if d := decisions(res); d["web/src/c.test.ts"] != "-unaffected" {
		t.Errorf("nothing changed, still selected: %v (changed %v)", d, res.Suites[0].Tests[0].Changed)
	}
	if !contains(res.Suites[0].Known["web/src/c.test.ts"], "web/package.json") {
		t.Errorf("the suite's own package.json is no input: %v", res.Suites[0].Known["web/src/c.test.ts"])
	}
}

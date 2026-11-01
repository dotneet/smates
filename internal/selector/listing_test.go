package selector

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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
func (f *fixture) recordRun(st *state.State, unit string, files []string, run func()) {
	f.t.Helper()
	res := f.sel(st, func(in *Input) { in.Repo, in.All = &gitx.Repo{Root: f.dir}, true })
	sr := res.Suites[0]
	h := NewHasher(f.snap()).WithRoot(f.dir).WithConfig(f.cfg)
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

// The listing of a directory the snapshot lists hashes as before entries on disk were
// read (an entry recorded by an older smates still matches) unless git-ignored entries
// are on disk; vendored entries and those the ignore globs match never count.
func TestListingTrackedDir(t *testing.T) {
	f := webFixture(t)
	snapOnly := NewHasher(f.snap())
	h := func() string { return NewHasher(f.snap(), IgnoreGlobs(f.cfg)...).WithRoot(f.dir).Hash("web/src/") }
	sum := sha1.Sum([]byte(strings.Join([]string{"a.test.ts", "a.ts", "b.test.ts", "b.ts", "c.test.ts"}, "\n")))
	want := "dir:" + hex.EncodeToString(sum[:])
	if got := snapOnly.Hash("web/src/"); got != want {
		t.Fatalf("snapshot listing %s, want %s", got, want)
	}
	if got := h(); got != want {
		t.Errorf("clean tracked directory: %s, want the snapshot's %s", got, want)
	}
	f.writeUntracked("web/src/node_modules/x/index.js", "")
	f.writeUntracked("web/src/__pycache__/a.pyc", "")
	f.writeUntracked("web/src/NOTES.md", "") // ignored by default (**/*.md)
	if got := h(); got != want {
		t.Errorf("vendored and ignored entries: %s, want %s", got, want)
	}
	f.writeUntracked("web/src/gen/x.ts", "")
	if got := h(); got == want {
		t.Errorf("an ignored directory inside a tracked one is not listed")
	}
	// Existence checks of paths outside the snapshot still go to disk (and are pinned):
	// a directory git ignores is not taken for one the snapshot lists.
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

// A command smates cannot see into ran in a directory git ignores: its tree is read
// from disk. A tree the snapshot lists leaves its git-ignored files out, as before.
func TestTreeOfIgnoredDir(t *testing.T) {
	f := webFixture(t)
	f.writeUntracked("web/build/a.js", "1")
	tree := func(dir string) string { return NewHasher(f.snap()).WithRoot(f.dir).Hash(depkey.Tree(dir)) }
	before, src := tree("web/build"), tree("web/src")
	f.writeUntracked("web/build/a.js", "2")
	if tree("web/build") == before {
		t.Errorf("edit in an ignored directory: tree unchanged")
	}
	f.writeUntracked("web/build/node_modules/x.js", "")
	f.writeUntracked("web/src/gen/x.ts", "")
	if got := tree("web/src"); got != src || got != NewHasher(f.snap()).Hash(depkey.Tree("web/src")) {
		t.Errorf("tracked tree with ignored files: %s, want %s", got, src)
	}
	after := tree("web/build")
	os.RemoveAll(filepath.Join(f.dir, "web/build/node_modules"))
	if tree("web/build") != after {
		t.Errorf("vendored files count in a tree")
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

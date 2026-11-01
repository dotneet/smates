package selector

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// writeUntracked writes a file git does not list (it stays out of the fixture's
// snapshot), such as a git-ignored cache or .env.
func (f *fixture) writeUntracked(p, content string) {
	f.t.Helper()
	abs := filepath.Join(f.dir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// TestRecordPinsFilesOutsideSnapshot: files outside the snapshot are recorded as they
// were before the tests ran, not as the tests left them. What the entries depend on is
// read before the run (PinBefore); a path first read afterwards that the run created
// or modified hashes as Unsettled, so the test runs again.
func TestRecordPinsFilesOutsideSnapshot(t *testing.T) {
	f := webFixture(t)
	const unit = "web/src/c.test.ts"
	st := f.record(map[string][]string{unit: {"web/.env", "web/gen?"}}, nil)
	f.writeUntracked("web/.env", "before")
	old := time.Now().Add(-time.Hour)
	f.writeUntracked("web/old.json", "old")
	if err := os.Chtimes(filepath.Join(f.dir, "web/old.json"), old, old); err != nil {
		t.Fatal(err)
	}
	res := f.sel(st, func(in *Input) { in.All = true })
	sr := res.Suites[0]

	h := NewHasher(f.snap()).WithRoot(f.dir).WithConfig(f.cfg)
	h.PinBefore(st, []string{"web"}, time.Now())
	// The tests run: they rewrite the files and create the paths they probed.
	f.writeUntracked("web/.env", "after")
	f.writeUntracked("web/.cache.json", "after")
	for _, d := range []string{"web/gen", "web/tmp/out"} {
		if err := os.MkdirAll(filepath.Join(f.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	obs := map[string]*runner.Observation{unit: {Status: state.Pass, Files: map[string]bool{
		"web/.env": true, "web/gen?": true, "web/.cache.json": true, "web/tmp/out?": true, "web/old.json": true, "web/missing.json": true,
	}}}
	Record(st, sr, obs, h.ForSuite(sr.Suite), RecordOptions{Now: time.Unix(2, 0)})
	deps := st.Suites["web"].Tests[unit].Deps
	want := map[string]string{
		"web/.env":         gitx.HashBytes([]byte("before")), // recorded before: pinned
		"web/gen?":         "absent",
		"web/.cache.json":  Unsettled, // first read after the run wrote it
		"web/tmp/out?":     Unsettled,
		"web/old.json":     gitx.HashBytes([]byte("old")), // untouched by the run
		"web/missing.json": "",
	}
	for k, v := range want {
		if deps[k] != v {
			t.Errorf("%s: recorded %q, want %q", k, deps[k], v)
		}
	}
	// Next time the files are as the run left them: the test runs again.
	sel := f.sel(st, func(in *Input) { in.Repo = &gitx.Repo{Root: f.dir} })
	if d := decisions(sel)[unit]; d != ReasonChanged {
		t.Errorf("decision %s, want %s", d, ReasonChanged)
	}
}

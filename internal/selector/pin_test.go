package selector

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// writeUntracked writes a file git does not list (it stays out of the fixture's
// snapshot), such as a git-ignored cache or report.
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

// TestRecordSettlesOutsideSnapshot runs a suite several times whose tests touch paths
// git does not list, and checks that every test settles (is skipped once nothing
// changes) after at most one more run:
//
//   - a.test.ts reads a report and rewrites it with new content every run: its content
//     is recorded as the run left it, so it is skipped from then on;
//   - b.test.ts probes an output directory and then creates it: recorded unsettled the
//     first time (it existed before no run), it runs once more, then is recorded as
//     existing before the run (PinBefore);
//   - c.test.ts probes a directory, creates it and removes it in its teardown;
//   - q.test.ts probes a directory generated just before the run (make gen && smates
//     run): it existed before the run, so it is no reason to run again.
func TestRecordSettlesOutsideSnapshot(t *testing.T) {
	f := webFixture(t)
	st := f.record(nil, nil)
	f.writeUntracked("web/out/report.json", "0")
	mkdir := func(p string) {
		if err := os.MkdirAll(filepath.Join(f.dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("web/generated")
	got := map[string][]string{}
	for i := range 5 {
		res := f.sel(st, func(in *Input) { in.Repo, in.All = &gitx.Repo{Root: f.dir}, i == 0 })
		sr := res.Suites[0]
		h := NewHasher(f.snap()).WithRoot(f.dir).WithConfig(f.cfg)
		h.PinBefore(st, []string{"web"}, time.Now())
		obs := map[string]*runner.Observation{}
		for _, tt := range sr.Tests {
			if i > 0 {
				d := tt.Reason
				if !tt.Selected {
					d = "-" + d
				}
				got[tt.ID] = append(got[tt.ID], d)
			}
			if !tt.Selected {
				continue
			}
			o := &runner.Observation{Files: map[string]bool{}, Status: state.Pass}
			switch tt.ID {
			case "web/src/a.test.ts":
				o.Files["web/out/report.json"] = true
				f.writeUntracked("web/out/report.json", strconv.Itoa(i+1))
			case "web/src/b.test.ts":
				o.Files["web/gen?"] = true
				mkdir("web/gen")
			case "web/src/c.test.ts":
				o.Files["web/tmp/x?"] = true
				mkdir("web/tmp/x")
				os.RemoveAll(filepath.Join(f.dir, "web/tmp"))
			case "web/db/q.test.ts":
				o.Files["web/generated?"] = true
			}
			obs[tt.ID] = o
		}
		Record(st, sr, obs, h.ForSuite(sr.Suite), RecordOptions{Now: time.Unix(int64(i+2), 0)})
	}
	settled := []string{"-unaffected", "-unaffected", "-unaffected", "-unaffected"}
	want := map[string][]string{
		"web/src/a.test.ts": settled,
		"web/src/b.test.ts": {ReasonChanged, "-unaffected", "-unaffected", "-unaffected"},
		"web/src/c.test.ts": settled,
		"web/db/q.test.ts":  settled,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decisions per run\n got %v\nwant %v", got, want)
	}
	if v := st.Suites["web"].Tests["web/src/b.test.ts"].Deps["web/gen?"]; v != "exists" {
		t.Errorf("b settled on %q", v)
	}
}

// TestRecordPinsProbes: an existence check recorded before is taken before the run, so a
// test that probed a path absent then and created it is recorded as having seen it
// absent; a probe first seen in this run of a path the run created is unsettled.
func TestRecordPinsProbes(t *testing.T) {
	f := webFixture(t)
	const unit = "web/src/c.test.ts"
	st := f.record(map[string][]string{unit: {"web/out?"}}, nil)
	res := f.sel(st, func(in *Input) { in.All = true })
	sr := res.Suites[0]
	h := NewHasher(f.snap()).WithRoot(f.dir).WithConfig(f.cfg)
	h.PinBefore(st, []string{"web"}, time.Now())
	for _, d := range []string{"web/out", "web/tmp/new"} {
		if err := os.MkdirAll(filepath.Join(f.dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	obs := map[string]*runner.Observation{unit: {Status: state.Pass, Files: map[string]bool{"web/out?": true, "web/tmp/new?": true}}}
	Record(st, sr, obs, h.ForSuite(sr.Suite), RecordOptions{Now: time.Unix(2, 0)})
	deps := st.Suites["web"].Tests[unit].Deps
	if deps["web/out?"] != "absent" || deps["web/tmp/new?"] != Unsettled {
		t.Errorf("recorded %q, %q", deps["web/out?"], deps["web/tmp/new?"])
	}
}

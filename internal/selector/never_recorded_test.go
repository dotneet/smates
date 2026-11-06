package selector

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// recordPartial simulates `smates run --all --suite` over picked: their units are
// recorded, and the snapshot holds the files only they own and, as markers seen by
// them, the files suites left out also own (as cli saveState writes it), on top of
// what st already holds.
func recordPartial(f *fixture, st *state.State, picked, left []*runner.Suite) {
	f.t.Helper()
	res := f.sel(state.New(), func(in *Input) { in.All, in.Suites, in.Others = true, picked, left })
	h := NewHasher(f.snap()).WithConfig(f.cfg)
	for _, sr := range res.Suites {
		obs := map[string]*runner.Observation{}
		for _, tt := range sr.Tests {
			obs[tt.ID] = &runner.Observation{Files: map[string]bool{}, Status: state.Pass}
		}
		Record(st, sr, obs, h.ForSuite(sr.Suite), RecordOptions{Commit: "c1", Now: time.Unix(1, 0), Full: true})
	}
	own := NewOwnership(picked, left)
	for p, hash := range f.snap() {
		v, _ := st.Snap(p)
		if own.OwnedOnly(p) {
			st.SetSnap(p, state.HashSnap(hash))
		} else {
			st.SetSnap(p, v.MarkSeen(hash, own.Picked()).CollapseIfComplete(own.Owners(p)))
		}
	}
}

// partialFixture has a vitest suite web (recorded) and a pytest suite api that has
// never been recorded. Since the base revision, a source file of each suite, a
// migration of api no test imports and a script outside every suite root changed;
// then `smates run --all --suite web` recorded web.
func partialFixture(t *testing.T) (f *fixture, repo *gitx.Repo, base string) {
	f = newFixture(t, map[string]string{
		"web/a.test.ts":           `import { a } from "./a"`,
		"web/a.ts":                `export const a = 1`,
		"api/test_app.py":         "import app\n",
		"api/test_other.py":       "",
		"api/app.py":              "",
		"api/migrations/001.sql":  "create table t();\n",
		"api/old.sql":             "drop table t;\n",
		"tools/gen.sh":            "echo 1\n",
		"Dockerfile":              "FROM x\n",
		"e2e/test_e2e.py":         "",
		"e2e/fixtures/users.json": "[]",
	})
	f.suites = []*runner.Suite{
		suite(t, config.Suite{Name: "web", Runner: "vitest", Root: "web", Tests: []string{"*.test.ts"}}),
		suite(t, config.Suite{Name: "api", Runner: "pytest", Root: "api", Tests: []string{"test_*.py"}}),
	}
	repo, base = gitFixture(t, f)
	f.write("web/a.ts", `export const a = 2`)
	f.write("api/app.py", "x = 1\n")
	f.write("api/migrations/001.sql", "create table u();\n")
	f.write("tools/gen.sh", "echo 2\n")
	f.remove("api/old.sql")
	return f, repo, base
}

// TestSelectNeverRecordedSuites pins how a suite with no recorded entry is decided
// after a partial recording of the other suites: its tests against the base revision
// by static analysis (or as new without one), the files it owns that changed since
// then and no test explains as its orphans, and the files the recorded suites saw as
// no change for them.
func TestSelectNeverRecordedSuites(t *testing.T) {
	cases := []struct {
		name string
		// edit changes the fixture and the state after the partial recording.
		edit func(f *fixture, st *state.State)
		// scope: the suites selected (nil: both) and left out.
		scope    func(f *fixture) (picked, left []*runner.Suite)
		noBase   bool
		noPolicy bool // NoOrphanPolicy (orphans: ignore, --orphans=skip)
		want     map[string]string
		orphans  []string
		note     string // a note containing this ("" = no never-recorded note)
	}{
		{
			name: "unscoped with a base",
			want: map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": ReasonDiff, "api/test_other.py": "-unaffected"},
			// Must keep: the migration and the deletion only api owns, and the script
			// every suite owns, changed since the base revision and no test explains
			// them. The Dockerfile did not change since then: no orphan.
			orphans: []string{"api/migrations/001.sql", "api/old.sql", "tools/gen.sh"},
			note:    "1 suite(s) have never been recorded in a full run (their tests are decided against the base revision",
		},
		{
			name:   "unscoped without a base",
			noBase: true,
			// Must keep: every test of api runs as new, so nothing has to reach it.
			want: map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": ReasonNew, "api/test_other.py": ReasonNew},
			note: "their tests run as new: no base revision to compare with",
		},
		{
			name: "a file changed since the recorded suite saw it",
			edit: func(f *fixture, st *state.State) { f.write("Dockerfile", "FROM y\n") },
			want: map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": ReasonDiff, "api/test_other.py": "-unaffected"},
			// Changed for web too: an orphan as before.
			orphans: []string{"Dockerfile", "api/migrations/001.sql", "api/old.sql", "tools/gen.sh"},
			note:    "1 suite(s) have never been recorded",
		},
		{
			name: "a recorded suite that has not seen the marker",
			edit: func(f *fixture, st *state.State) {
				e2e := suite(f.t.(*testing.T), config.Suite{Name: "e2e", Runner: "pytest", Root: "e2e", Tests: []string{"test_*.py"}})
				f.suites = append(f.suites, e2e)
				st.Suite("e2e").Tests["e2e/test_e2e.py"] = &state.Entry{Status: state.Pass, Deps: map[string]string{"e2e/test_e2e.py": f.snap()["e2e/test_e2e.py"]}}
				st.Commit = "c0" // a full run took the snapshot e2e compares with
			},
			want: map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": ReasonDiff, "api/test_other.py": "-unaffected", "e2e/test_e2e.py": "-unaffected"},
			// e2e has entries but saw none of the markers: they are changed for it,
			// and the unexplained ones orphans, as before.
			orphans: []string{"Dockerfile", "api/migrations/001.sql", "api/old.sql", "e2e/fixtures/users.json", "tools/gen.sh"},
		},
		// Its first ordinary run after a partial recording gave api entries: api still
		// has no snapshot (no full run, no marker names it), so the files only it has not
		// seen stay no change for web, and what changed since the base is its orphans.
		{
			name: "a suite recorded only by a selective run",
			edit: func(f *fixture, st *state.State) {
				st.Suite("api").Tests["api/test_app.py"] = &state.Entry{Status: state.Pass, Deps: map[string]string{"api/test_app.py": f.snap()["api/test_app.py"], "api/app.py": f.snap()["api/app.py"]}}
			},
			want:    map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": "-unaffected", "api/test_other.py": "-unaffected"},
			orphans: []string{"api/migrations/001.sql", "api/old.sql", "tools/gen.sh"},
			note:    "1 suite(s) have never been recorded in a full run",
		},
		{
			// Must keep: without a base revision nothing judges api's changes, so the
			// markers stay changed for it (orphans), as before.
			name:   "a suite recorded only by a selective run, without a base",
			noBase: true,
			edit: func(f *fixture, st *state.State) {
				st.Suite("api").Tests["api/test_app.py"] = &state.Entry{Status: state.Pass, Deps: map[string]string{"api/test_app.py": f.snap()["api/test_app.py"], "api/app.py": f.snap()["api/app.py"]}}
			},
			want:    map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": "-unaffected", "api/test_other.py": ReasonNew},
			orphans: []string{"Dockerfile", "api/migrations/001.sql", "api/old.sql", "e2e/fixtures/users.json", "e2e/test_e2e.py", "tools/gen.sh"},
		},
		// Must keep: with no orphan policy the dropped files still get the note, and the
		// decisions do not change.
		{
			name:     "no orphan policy",
			noPolicy: true,
			want:     map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": ReasonDiff, "api/test_other.py": "-unaffected"},
			orphans:  []string{"api/migrations/001.sql", "api/old.sql", "tools/gen.sh"},
			note:     "1 suite(s) have never been recorded in a full run",
		},
		// Must keep: a dropped file under api's root that a recorded test read at run
		// time is explained (no orphan), and still reaches api's tests through the read.
		{
			name:    "a file of the never-recorded suite a recorded test read",
			edit:    webReads("api/migrations/001.sql"),
			want:    map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": ReasonDiff, "api/test_other.py": ReasonNew},
			orphans: []string{"api/old.sql", "tools/gen.sh"},
			note:    "1 suite(s) have never been recorded in a full run",
		},
		{
			// With no orphan policy the read selects nothing, but says so.
			name:     "a file of the never-recorded suite a recorded test read, no orphan policy",
			edit:     webReads("api/migrations/001.sql"),
			noPolicy: true,
			want:     map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": ReasonDiff, "api/test_other.py": "-unaffected"},
			orphans:  []string{"api/old.sql", "tools/gen.sh"},
			note:     "none selected for them (no orphan policy)",
		},
		{
			name:   "a file of the never-recorded suite a recorded test read, without a base",
			edit:   webReads("api/migrations/001.sql"),
			noBase: true,
			want:   map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": ReasonNew, "api/test_other.py": ReasonNew},
			note:   "their tests run as new",
		},
		{
			name: "an entry no suite has compared",
			edit: func(f *fixture, st *state.State) { st.Files["Dockerfile"] = "?" },
			want: map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": ReasonDiff, "api/test_other.py": "-unaffected"},
			// Nobody saw it: changed as before.
			orphans: []string{"Dockerfile", "api/migrations/001.sql", "api/old.sql", "tools/gen.sh"},
			note:    "1 suite(s) have never been recorded",
		},
		{
			name: "no suite has an entry",
			edit: func(f *fixture, st *state.State) { delete(st.Suites, "web") },
			// Must keep: diff mode decides every test against the snapshot, in which
			// only web saw the markers: they stay changed for api.
			want:    map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": ReasonDiff, "api/test_other.py": ReasonDiff},
			orphans: []string{"Dockerfile", "api/migrations/001.sql", "api/old.sql", "e2e/fixtures/users.json", "e2e/test_e2e.py", "tools/gen.sh"},
		},
		{
			name:  "--suite of the recorded suite",
			scope: func(f *fixture) ([]*runner.Suite, []*runner.Suite) { return f.suites[:1], f.suites[1:] },
			want:  map[string]string{"web/a.test.ts": "-unaffected"},
		},
		{
			name:    "--suite of the never-recorded suite",
			scope:   func(f *fixture) ([]*runner.Suite, []*runner.Suite) { return f.suites[1:], f.suites[:1] },
			want:    map[string]string{"api/test_app.py": ReasonDiff, "api/test_other.py": "-unaffected"},
			orphans: []string{"api/migrations/001.sql", "api/old.sql", "tools/gen.sh"},
			note:    "1 suite(s) have never been recorded",
		},
		{
			name: "a root suite that has never been recorded",
			edit: func(f *fixture, st *state.State) {
				f.suites[1] = suite(f.t.(*testing.T), config.Suite{Name: "api", Runner: "pytest", Root: ".", Tests: []string{"api/test_*.py"}})
				for p, v := range st.Files { // web's markers now name the root suite's files too
					if !strings.HasPrefix(p, "web/") {
						st.SetSnap(p, state.ParseSnap(v).MarkSeen(f.snap()[p], []string{"web"}))
					}
				}
			},
			want:    map[string]string{"web/a.test.ts": "-unaffected", "api/test_app.py": ReasonDiff, "api/test_other.py": "-unaffected"},
			orphans: []string{"api/migrations/001.sql", "api/old.sql", "tools/gen.sh"},
			note:    "1 suite(s) have never been recorded",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, repo, base := partialFixture(t)
			st := state.New()
			recordPartial(f, st, f.suites[:1], f.suites[1:])
			st.SetSnap("api/old.sql", state.MarkerSnap("", []string{"web"})) // web saw it deleted
			if c.edit != nil {
				c.edit(f, st)
			}
			picked, left := f.suites, []*runner.Suite(nil)
			if c.scope != nil {
				picked, left = c.scope(f)
			}
			res := f.sel(st, func(in *Input) {
				in.Suites, in.Others, in.Repo = picked, left, repo
				if !c.noBase {
					in.Base, in.DiffUnrecorded = base, true
				}
				in.NoOrphanPolicy = c.noPolicy
			})
			if got := decisions(res); !reflect.DeepEqual(got, c.want) {
				t.Errorf("decisions\n got %v\nwant %v", got, c.want)
			}
			if !reflect.DeepEqual(res.Orphans, c.orphans) {
				t.Errorf("orphans %v, want %v", res.Orphans, c.orphans)
			}
			found := slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, "never been recorded") })
			if c.note == "" && found || c.note != "" && !slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, c.note) }) {
				t.Errorf("notes %q, want one with %q", res.Notes, c.note)
			}
		})
	}
}

// webReads gives web's recorded test a run-time read of file (as the trace records it,
// with the content web saw).
func webReads(file string) func(f *fixture, st *state.State) {
	return func(f *fixture, st *state.State) {
		st.Suite("web").Tests["web/a.test.ts"].Deps[file] = f.snap()[file]
	}
}

// TestSelectNeverRecordedLikeDiffMode checks that after a partial recording, the
// suite that has never been recorded is decided as with no recorded state at all:
// the same decisions and the same orphans of its own.
func TestSelectNeverRecordedLikeDiffMode(t *testing.T) {
	f, repo, base := partialFixture(t)
	diff := f.sel(state.New(), func(in *Input) { in.Repo, in.Base = repo, base })
	st := state.New()
	recordPartial(f, st, f.suites[:1], f.suites[1:])
	st.SetSnap("api/old.sql", state.MarkerSnap("", []string{"web"}))
	partial := f.sel(st, func(in *Input) { in.Repo, in.Base, in.DiffUnrecorded = repo, base, true })
	want, got := decisions(diff), decisions(partial)
	for id := range want {
		if strings.HasPrefix(id, "api/") && got[id] != want[id] {
			t.Errorf("%s: %s, want %s as with no state", id, got[id], want[id])
		}
	}
	var apiOrphans []string
	for _, o := range diff.Orphans {
		if !strings.HasPrefix(o, "web/") {
			apiOrphans = append(apiOrphans, o)
		}
	}
	if !reflect.DeepEqual(partial.Orphans, apiOrphans) {
		t.Errorf("orphans %v, want %v as with no state", partial.Orphans, apiOrphans)
	}
}

// Snapshotted: a full snapshot covers every suite; otherwise a suite counts once a
// marker names it.
func TestSnapshotted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		commit string
		files  map[string]string
		want   map[string]bool
	}{
		{"full snapshot", "c0", map[string]string{"a": "h"}, map[string]bool{"web": true, "api": true}},
		{"markers", "", map[string]string{"a": "?h@web", "b": "?h@api%2Cx,web", "c": "h", "d": "?"},
			map[string]bool{"web": true, "api,x": true, "api": false}},
		{"no marker", "", map[string]string{"a": "h"}, map[string]bool{"web": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := state.New()
			st.Commit, st.Files = tc.commit, tc.files
			snap := Snapshotted(st)
			for name, want := range tc.want {
				if got := snap(name); got != want {
					t.Errorf("%s: %v, want %v", name, got, want)
				}
			}
		})
	}
}

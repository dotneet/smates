package selector

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
)

const (
	commitA = gitx.CommitPrefix + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitB = gitx.CommitPrefix + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestTreeSubmoduleCommit: a "dir/**" tree hash includes the pinned commit of a
// submodule at dir, above it (dir is inside the submodule) or below it, so bumping the
// submodule changes the hash even when the files inside it keep their paths and sizes
// (the tree of a directory the snapshot has no file below hashes paths and sizes only).
// Must keep: a same-size bump (VERSION 1.2.3 -> 1.2.4) left the tests skipped.
func TestTreeSubmoduleCommit(t *testing.T) {
	type bump struct {
		name    string
		commit  string // the submodule's commit after
		version string // lib/sub/VERSION after
	}
	bumps := []bump{
		{"same size", commitB, "1.2.4"},
		{"different size", commitB, "1.2.10"},
		{"commit only", commitB, "1.2.3"}, // checked out as recorded (not updated yet)
	}
	for _, c := range []struct {
		name   string
		module string // the submodule's path
		dir    string // the tree's directory
	}{
		{"submodule at dir", "lib", "lib"},
		{"submodule containing dir", "lib", "lib/sub"},
		{"submodule below dir", "third/lib", "third"},
		{"submodule two levels below dir", "third/x/lib", "third"},
		{"nested: dir inside a submodule inside a directory", "third/lib", "third/lib/sub"},
	} {
		for _, b := range bumps {
			for _, cap := range []bool{false, true} {
				name := c.name + "/" + b.name
				if cap {
					name += "/over the cap"
				}
				t.Run(name, func(t *testing.T) {
					root := t.TempDir()
					testutil.Write(t, root, c.module+"/sub/VERSION", "1.2.3")
					testutil.Write(t, root, c.module+"/sub/README", "r")
					testutil.Write(t, root, "third/tracked.txt", "t")
					snap := gitx.Snapshot{c.module: commitA, "third/tracked.txt": gitx.HashBytes([]byte("t")), "go.mod": "m"}
					if cap {
						defer func(n int) { maxDiskTree = n }(maxDiskTree)
						maxDiskTree = 1
					}
					key := depkey.Tree(c.dir)
					before := NewHasher(snap).WithRoot(root).Hash(key)
					if again := NewHasher(maps.Clone(snap)).WithRoot(root).Hash(key); again != before {
						t.Fatalf("not stable: %s then %s", before, again)
					}
					after := maps.Clone(snap)
					after[c.module] = b.commit
					testutil.Write(t, root, c.module+"/sub/VERSION", b.version)
					h := NewHasher(after).WithRoot(root)
					if got := h.Hash(key); got == before {
						t.Errorf("%s hashes %s before and after the bump", key, got)
					}
					if h.Unchanged(key, before) {
						t.Errorf("%s unchanged against %s after the bump", key, before)
					}
				})
			}
		}
	}
	// Trees that hold no submodule keep their hash (entries recorded before submodule
	// commits counted still match).
	root := t.TempDir()
	testutil.Write(t, root, "build/out.js", "x")
	snap := gitx.Snapshot{"src/a.ts": "h1", "lib": commitA}
	for _, dir := range []string{"build", "src"} {
		if a, b := NewHasher(snap).WithRoot(root).Hash(depkey.Tree(dir)), NewHasher(gitx.Snapshot{"src/a.ts": "h1", "lib": commitB}).WithRoot(root).Hash(depkey.Tree(dir)); a != b {
			t.Errorf("%s/** depends on a submodule elsewhere: %s, %s", dir, a, b)
		}
	}
}

// TestSelectSubmoduleBump: bumping a submodule selects the tests that ran a command in
// it (a tree over it) and the tests that read files in it whose content changed; it is
// an orphan unless a test depends on its content (a file read in it, a tree over it):
// a test that only listed or probed it does not explain a new commit (must keep).
func TestSelectSubmoduleBump(t *testing.T) {
	cases := []struct {
		name    string
		reads   []string // what the test recorded
		version string   // lib/VERSION after the bump
		// want: the test's decision; orphan: the bump is an orphan.
		want   string
		orphan bool
	}{
		{"tree over it, same size", []string{depkey.Tree("lib")}, "1.2.4", ReasonChanged, false},
		{"tree over it, different size", []string{depkey.Tree("lib")}, "1.2.10", ReasonChanged, false},
		{"tree inside it", []string{depkey.Tree("lib/sub")}, "1.2.4", ReasonChanged, false},
		{"file read in it, changed", []string{"lib/VERSION"}, "1.2.4", ReasonChanged, false},
		{"file read in it, unchanged", []string{"lib/sub/README"}, "1.2.4", "-unaffected", false},
		{"listing of it", []string{depkey.Listing("lib")}, "1.2.4", "-unaffected", true},
		{"probe in it", []string{depkey.Existence("lib/VERSION")}, "1.2.4", "-unaffected", true},
		{"listing and probe in it", []string{depkey.Listing("lib"), depkey.Existence("lib/VERSION")}, "1.2.4", "-unaffected", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"test_x.py": ""})
			testutil.Write(t, f.dir, "lib/VERSION", "1.2.3")
			testutil.Write(t, f.dir, "lib/sub/README", "r")
			f.suites = []*runner.Suite{suite(t, config.Suite{Name: "py", Runner: "pytest", Tests: []string{"test_*.py"}})}
			snap := f.snap()
			snap["lib"] = commitA
			repo := &gitx.Repo{Root: f.dir}
			st := state.New()
			res := f.sel(st, func(in *Input) { in.All, in.Snap, in.Repo = true, snap, repo })
			h := NewHasher(snap).WithConfig(f.cfg).WithRoot(f.dir)
			for _, sr := range res.Suites {
				obs := map[string]*runner.Observation{}
				for _, tt := range sr.Tests {
					o := &runner.Observation{Files: map[string]bool{}, Status: state.Pass}
					for _, r := range tc.reads {
						o.Files[r] = true
					}
					obs[tt.ID] = o
				}
				Record(st, sr, obs, h.ForSuite(sr.Suite), RecordOptions{Commit: "c1", Now: time.Unix(1, 0), Full: true})
			}
			st.Files = map[string]string(maps.Clone(snap))

			bumped := maps.Clone(snap)
			bumped["lib"] = commitB
			if err := os.WriteFile(filepath.Join(f.dir, "lib", "VERSION"), []byte(tc.version), 0o644); err != nil {
				t.Fatal(err)
			}
			res = f.sel(st, func(in *Input) { in.Snap, in.Repo = bumped, repo })
			if got := decisions(res)["test_x.py"]; got != tc.want {
				t.Errorf("test_x.py: %s, want %s (%v)", got, tc.want, res.Suites[0].Tests[0].Changed)
			}
			if !reflect.DeepEqual(res.Changed, []string{"lib"}) {
				t.Errorf("changed %v", res.Changed)
			}
			if orphan := strings.Join(res.Orphans, ",") == "lib"; orphan != tc.orphan {
				t.Errorf("orphans %v, want the bump an orphan: %v", res.Orphans, tc.orphan)
			}
		})
	}
}

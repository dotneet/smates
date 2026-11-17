package selector

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// narrow returns snap with the files skip-worktree takes off disk (gitx.OffDisk) and
// removes them from the fixture's directory, as narrowing a sparse checkout's cone
// does.
func narrow(f *fixture, snap gitx.Snapshot, files ...string) gitx.Snapshot {
	out := maps.Clone(snap)
	for _, p := range files {
		out[p] = gitx.OffDisk(snap[p])
		os.Remove(filepath.Join(f.dir, filepath.FromSlash(p)))
	}
	for _, p := range files { // directories left empty go too, as git removes them
		for d := filepath.Dir(filepath.Join(f.dir, filepath.FromSlash(p))); d != f.dir; d = filepath.Dir(d) {
			if os.Remove(d) != nil {
				break
			}
		}
	}
	return out
}

// TestHashSparseCone: a key a test observed hashes as the file's presence on disk
// has it. Narrowing a sparse checkout's cone (the skip-worktree files leave the disk,
// git counts them as unchanged) changes the hash of every kind of key over them, and
// widening it again changes it back, with or without a root to read the disk from (a
// key gitx.ByKey compares; must keep in both directions).
func TestHashSparseCone(t *testing.T) {
	files := map[string]string{
		"fx/a.json":     "a",
		"fx/b.json":     "b",
		"fx/sub/c.json": "c",
		"fx/sub/d.json": "d",
		"pkg/p.go":      "package pkg\n\nfunc init() {}\n\nfunc F() {}\n",
		"pkg/q.go":      "package pkg\n\nfunc G() {}\n",
		"other/x.txt":   "x",
	}
	set := &ServiceDeps{Key: depkey.Set("paths:fx/**"), Files: []string{"fx/a.json", "fx/b.json", "fx/sub/c.json", "fx/sub/d.json"}}
	keys := []string{
		"fx/a.json",                   // content
		depkey.Existence("fx/a.json"), // a probe of the file
		depkey.Listing("fx"),          // its directory's listing
		depkey.Tree("fx"),             // a tree holding it
		set.Key,                       // a declared set holding it
		depkey.Existence("fx/sub"),    // a directory left empty
		depkey.Listing("fx/sub"),      // its listing
		depkey.Tree("fx/sub"),         // its tree
		"pkg/p.go",                    // a Go file
		depkey.Decls("pkg/p.go"),      // its declarations
		depkey.Decl("pkg/p.go", "F"),  // one of them
		depkey.Sig("pkg/p.go", "F"),   // its signature
		depkey.Shape("pkg"),           // its package's shape
	}
	gone := []string{"fx/a.json", "fx/sub/c.json", "fx/sub/d.json", "pkg/p.go"}
	for _, root := range []bool{true, false} {
		f := newFixture(t, files)
		full := f.snap()
		hasher := func(snap gitx.Snapshot) *Hasher {
			h := NewHasher(snap).WithSets([]*ServiceDeps{set})
			if root {
				h.WithRoot(f.dir)
			}
			return h
		}
		wide := hasher(full)
		before := map[string]string{}
		for _, k := range keys {
			before[k] = wide.Hash(k)
		}
		narrowed := hasher(narrow(f, full, gone...))
		after := map[string]string{}
		for _, k := range keys {
			after[k] = narrowed.Hash(k)
		}
		// Keys over files that stay on disk do not change.
		for _, k := range []string{"fx/b.json", depkey.Existence("fx/b.json"), "other/x.txt", depkey.Listing("other"), depkey.Tree("other"), "pkg/q.go"} {
			if got, want := narrowed.Hash(k), wide.Hash(k); got != want {
				t.Errorf("root %v: %s changed (%q -> %q) though its files stay", root, k, want, got)
			}
		}
		if got := after[depkey.Existence("fx/a.json")]; got != "absent" {
			t.Errorf("root %v: a probe of a file off disk hashes %q", root, got)
		}
		for _, p := range gone { // the cone widens again
			f.write(p, files[p])
		}
		for _, k := range keys {
			kind := depkey.KindOf(k)
			if !root && (kind == depkey.KindDecls || kind == depkey.KindDecl || kind == depkey.KindSig) {
				// Without a root the declarations are the file's hash: still a change.
				if narrowed.Hash(k) == before[k] {
					t.Errorf("no root: %s hashes %q both in and out of the cone", k, before[k])
				}
				continue
			}
			if after[k] == before[k] {
				t.Errorf("root %v: narrowing the cone leaves %s at %q", root, k, after[k])
			}
			if narrowed.Unchanged(k, before[k]) {
				t.Errorf("root %v: narrowing the cone leaves %s unchanged against %q", root, k, before[k])
			}
			// Recorded while narrowed, compared once widened (a fresh snapshot of the
			// restored files).
			wide2 := hasher(full)
			if wide2.Unchanged(k, after[k]) {
				t.Errorf("root %v: widening the cone leaves %s unchanged against %q", root, k, after[k])
			}
			if got := wide2.Hash(k); got != before[k] {
				t.Errorf("root %v: widened, %s hashes %q, want %q as before", root, k, got, before[k])
			}
		}
	}
}

// TestSelectSparseCone: narrowing a sparse checkout's cone selects the tests that
// recorded a file it took off disk (must keep), and widening it selects those that
// ran without it; the files themselves are no change of the tree (git counts them as
// unchanged): no orphan, and a test file outside the cone is no unit, so nothing asks
// a runner to run it.
func TestSelectSparseCone(t *testing.T) {
	f := webFixture(t)
	st := f.record(map[string][]string{"web/src/c.test.ts": {"web/fixtures/users.json", depkey.Existence("web/fixtures/users.json")}}, nil)
	full := f.snap()
	off := []string{"web/fixtures/users.json", "web/db/q.test.ts", "Dockerfile"}
	snap := narrow(f, full, off...)
	res := f.sel(st, func(in *Input) { in.Snap = snap })
	want := map[string]string{
		"web/src/a.test.ts": "-unaffected", "web/src/b.test.ts": "-unaffected",
		"web/src/c.test.ts": ReasonChanged, // read the fixture now off disk
		// web/db/q.test.ts is outside the cone: no unit.
	}
	if got := decisions(res); !reflect.DeepEqual(got, want) {
		t.Errorf("narrowed: got %v want %v", got, want)
	}
	if c := res.SuiteByName("web").Tests; len(c) != 3 {
		t.Errorf("units %d", len(c))
	}
	for _, tt := range res.Suites[0].Tests {
		if tt.ID == "web/src/c.test.ts" && !reflect.DeepEqual(tt.Changed, []string{"web/fixtures/users.json", depkey.Existence("web/fixtures/users.json")}) {
			t.Errorf("c.test.ts changed %v", tt.Changed)
		}
	}
	if len(res.Changed) != 0 || len(res.Orphans) != 0 {
		t.Errorf("narrowed: changed %v orphans %v, want none (no content changed)", res.Changed, res.Orphans)
	}

	// The run while narrowed records the fixture off disk; widening the cone selects
	// the test again. The snapshot recorded then holds the files off disk: no change
	// once they are back either.
	h := NewHasher(snap).WithConfig(f.cfg).WithRoot(f.dir)
	for _, sr := range res.Suites {
		obs := map[string]*runner.Observation{}
		for _, tt := range sr.Tests {
			if tt.Selected {
				obs[tt.ID] = &runner.Observation{Files: map[string]bool{"web/fixtures/users.json": true, depkey.Existence("web/fixtures/users.json"): true}, Status: state.Pass}
			}
		}
		Record(st, sr, obs, h.ForSuite(sr.Suite), RecordOptions{Commit: "c2"})
	}
	st.Files = map[string]string(snap)
	for _, p := range off {
		f.write(p, f.files[p])
	}
	res = f.sel(st, func(in *Input) { in.Snap = full })
	want = map[string]string{
		"web/src/a.test.ts": "-unaffected", "web/src/b.test.ts": "-unaffected",
		"web/src/c.test.ts": ReasonChanged, // ran without the fixture
		"web/db/q.test.ts":  "-unaffected", // its entry from before the cone narrowed
	}
	if got := decisions(res); !reflect.DeepEqual(got, want) {
		t.Errorf("widened: got %v want %v", got, want)
	}
	if len(res.Changed) != 0 || len(res.Orphans) != 0 {
		t.Errorf("widened: changed %v orphans %v, want none", res.Changed, res.Orphans)
	}
}

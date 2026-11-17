package selector

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
)

// TestDeclsCacheTestness: the declarations of a Go file hash differently in a test
// file (golang.DeclsHash leaves its constants out), so the same content in a test file
// and in a source file must not share a cached hash, in either order.
func TestDeclsCacheTestness(t *testing.T) {
	for i, order := range [][]string{{"p/x.go", "p/x_test.go"}, {"p/y_test.go", "p/y.go"}} {
		t.Run(strings.Join(order, ","), func(t *testing.T) {
			// Content of its own per row: the cache is global.
			src := "package p\n\nconst K" + strconv.Itoa(i) + strconv.FormatInt(time.Now().UnixNano(), 10) + " = 1\n"
			f := newFixture(t, map[string]string{order[0]: src, order[1]: src})
			h := NewHasher(f.snap()).WithRoot(f.dir)
			first, second := h.Hash(depkey.Decls(order[0])), h.Hash(depkey.Decls(order[1]))
			if first == second {
				t.Errorf("%s and %s share the declarations hash %s", order[0], order[1], first)
			}
			// Fresh hasher, same global cache: the same answers.
			h2 := NewHasher(f.snap()).WithRoot(f.dir)
			if h2.Hash(depkey.Decls(order[1])) != second || h2.Hash(depkey.Decls(order[0])) != first {
				t.Error("cached hashes differ from the computed ones")
			}
		})
	}
}

// TestUnderBinarySearch: the snapshot's files a tree hashes are found by binary search;
// neighbours that share a prefix without the separator ("a.b", "a-b", "ab") are not
// below "a", a file named like the directory is.
func TestUnderBinarySearch(t *testing.T) {
	snap := gitx.Snapshot{}
	for _, p := range []string{"a", "a-b/x", "a.b", "a/b", "a/b/c", "a/c", "a0/x", "ab", "b/a/x", "z"} {
		snap[p] = "h"
	}
	h := NewHasher(snap)
	for dir, want := range map[string][]string{
		".":   {"a", "a-b/x", "a.b", "a/b", "a/b/c", "a/c", "a0/x", "ab", "b/a/x", "z"},
		"a":   {"a", "a/b", "a/b/c", "a/c"},
		"a/b": {"a/b", "a/b/c"},
		"a-b": {"a-b/x"},
		"b":   {"b/a/x"},
		"b/a": {"b/a/x"},
		"c":   nil,
		"zz":  nil,
		"0":   nil,
	} {
		if got := h.under(dir); !slices.Equal(got, want) {
			t.Errorf("under(%q) = %q, want %q", dir, got, want)
		}
	}
}

// TestCachesSharedByCopies: hashers derived with ForSuite and WithSets fill and read the
// caches of the one they came from, from the first use on.
func TestCachesSharedByCopies(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a"})
	h := NewHasher(f.snap()).WithRoot(f.dir)
	c := h.ForSuite(&runner.Suite{}).WithSets([]*ServiceDeps{{Key: depkey.Set("x"), Files: []string{"a.txt"}}})
	c.Hash("gen/out.txt")
	if _, ok := h.disk["gen/out.txt"]; !ok {
		t.Error("a copy's disk hash is not in the original's cache")
	}
	c.Hash(depkey.Set("x"))
	if len(h.setv) != 1 {
		t.Error("a copy's set hash is not in the original's cache")
	}
}

// TestExistenceOfSymlinks: an existence check of a symlink hashes by whether the link
// resolves, not by whether the link itself is there: a test that checked a dangling
// link saw nothing, and must run again once its target appears (and the other way
// round), for links outside the snapshot and in it.
func TestExistenceOfSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	type row struct {
		name     string
		link     string // the symlink's path
		inSnap   bool   // the link is a file of the snapshot (tracked)
		target   string // relative target
		create   bool   // the target exists at first
		want     string // hash at first
		thenWant string // hash once the target's existence flipped
	}
	rows := []row{
		{"untracked dangling", "gen/dangling", false, "missing.txt", false, Dangling, "exists"},
		{"untracked resolving", "gen/ok", false, "there.txt", true, "exists", Dangling},
		{"tracked dangling", "links/dangling", true, "missing2.txt", false, Dangling, "exists"},
		{"tracked resolving", "links/ok", true, "there2.txt", true, "exists", Dangling},
		{"loop", "gen/loop", false, "loop", false, Dangling, Dangling},
	}
	f := newFixture(t, map[string]string{"x.txt": "x"})
	snap := f.snap()
	for _, c := range rows {
		abs := filepath.Join(f.dir, filepath.FromSlash(c.link))
		os.MkdirAll(filepath.Dir(abs), 0o755)
		if err := os.Symlink(c.target, abs); err != nil {
			t.Fatal(err)
		}
		if c.inSnap {
			snap[c.link] = gitx.HashBytes([]byte(c.target))
		}
		if c.create && c.target != "loop" {
			os.WriteFile(filepath.Join(filepath.Dir(abs), c.target), []byte("t"), 0o644)
		}
	}
	h := NewHasher(snap).WithRoot(f.dir)
	for _, c := range rows {
		if got := h.Hash(depkey.Existence(c.link)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	for _, c := range rows {
		if c.target == "loop" {
			continue
		}
		tgt := filepath.Join(f.dir, filepath.FromSlash(c.link), "..", c.target)
		if c.create {
			os.Remove(tgt)
		} else {
			os.WriteFile(tgt, []byte("t"), 0o644)
		}
	}
	h = NewHasher(snap).WithRoot(f.dir)
	for _, c := range rows {
		if got := h.Hash(depkey.Existence(c.link)); got != c.thenWant {
			t.Errorf("%s, target flipped: %q, want %q", c.name, got, c.thenWant)
		}
		if c.want != c.thenWant && h.Unchanged(depkey.Existence(c.link), c.want) {
			t.Errorf("%s: the flip is no change", c.name)
		}
	}
	// A file of the snapshot without a root (no disk) exists, as before.
	if got := NewHasher(snap).Hash(depkey.Existence("x.txt")); got != "exists" {
		t.Errorf("snapshot file without root: %q", got)
	}
}

// TestGoPackageChanged: in diff mode, the shape and the method sets of a Go package
// change when one of its Go files does (not a file in a subdirectory or a sibling, nor
// a non-Go file).
func TestGoPackageChanged(t *testing.T) {
	cs := newChangeSet(map[string]bool{"p/a.go": true, "q/sub/b.go": true, "r/notes.txt": true, "root.go": true})
	for key, want := range map[string]bool{
		depkey.Shape("p"):            true, // must keep
		depkey.Methods("p", "T"):     true, // must keep
		depkey.Shape("."):            true, // must keep: a package at the root
		depkey.Shape("q/sub"):        true,
		depkey.Shape("q"):            false,
		depkey.Shape("r"):            false,
		depkey.Shape("pp"):           false,
		depkey.Decls("p/a.go"):       false, // decided by the file itself
		depkey.Listing("p"):          false,
		depkey.Methods("q/sub", "U"): true,
	} {
		if got := cs.goPackage(key); got != want {
			t.Errorf("goPackage(%q) = %v, want %v", key, got, want)
		}
	}
	if (changeSet{}).goPackage(depkey.Shape("p")) {
		t.Error("an empty change set changes nothing")
	}
}

// TestOversizedTreeNote: a test that depends on a whole directory git does not list,
// too large to hash, is told about in the selection's notes (its changes select
// nothing).
func TestOversizedTreeNote(t *testing.T) {
	defer func(n int) { maxDiskTree = n }(maxDiskTree)
	maxDiskTree = 3
	f := newFixture(t, map[string]string{"test_x.py": "", "small/keep": ""})
	f.suites = []*runner.Suite{suite(t, config.Suite{Runner: "pytest", Tests: []string{"test_*.py"}})}
	st := f.record(map[string][]string{"test_x.py": {depkey.Tree("build"), depkey.Tree("small")}}, nil)
	for i := range 5 {
		f.writeUntracked("build/f"+strconv.Itoa(i), "x")
	}
	res := f.sel(st, func(in *Input) { in.Repo = &gitx.Repo{Root: f.dir} })
	if !slices.ContainsFunc(res.Notes, func(n string) bool {
		return strings.Contains(n, "more than 3 entries") && strings.HasSuffix(n, ": build")
	}) {
		t.Errorf("notes %q", res.Notes)
	}
	// Under the cap: no note.
	maxDiskTree = 100
	res = f.sel(st, func(in *Input) { in.Repo = &gitx.Repo{Root: f.dir} })
	if slices.ContainsFunc(res.Notes, func(n string) bool { return strings.Contains(n, "entries") }) {
		t.Errorf("notes under the cap: %q", res.Notes)
	}
}

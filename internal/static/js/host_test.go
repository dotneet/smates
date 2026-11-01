package js

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

// fsHost implements host.Host over a directory. Every file under the root is part of
// the working tree, node_modules included, so tests can check it is never returned.
type fsHost struct {
	root  string
	files map[string]bool
}

func (h *fsHost) Root() string           { return h.root }
func (h *fsHost) Has(p string) bool      { return h.files[p] }
func (h *fsHost) Files() map[string]bool { return h.files }

func (h *fsHost) Read(p string) []byte {
	b, err := os.ReadFile(filepath.Join(h.root, filepath.FromSlash(p)))
	if err != nil {
		return nil
	}
	return b
}

// newHost writes files (repo-relative slash path -> content) into a temp dir and
// returns a host over it.
func newHost(t *testing.T, files map[string]string) *fsHost {
	t.Helper()
	root := t.TempDir()
	h := &fsHost{root: root, files: map[string]bool{}}
	for p, content := range files {
		testutil.Write(t, root, p, content)
		h.files[p] = true
	}
	return h
}

// deps runs the analyzer on file p of a fixture.
func deps(t *testing.T, h *fsHost, p string) []string {
	t.Helper()
	a := New(h, Options{})
	if !a.Handles(p) {
		t.Fatalf("Handles(%q) = false", p)
	}
	return a.Deps(p, h.Read(p))
}

// sorted returns a sorted copy of s.
func sorted(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}

// assertDeps fails unless got equals want as sets (nil and empty are equal).
func assertDeps(t *testing.T, got, want []string) {
	t.Helper()
	g, w := sorted(got), sorted(want)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("deps mismatch\n got: %q\nwant: %q", g, w)
	}
}

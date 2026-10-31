package python

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/dotneet/smates/internal/static/host"
)

// dirHost implements host.Host over a directory on disk.
type dirHost struct {
	root  string
	files map[string]bool
}

var _ host.Host = (*dirHost)(nil)

// newFixture writes files (repo-relative slash path -> content) into a temp dir and
// returns a host over it.
func newFixture(t testing.TB, files map[string]string) *dirHost {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := &dirHost{root: root, files: map[string]bool{}}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		h.files[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *dirHost) Root() string           { return h.root }
func (h *dirHost) Has(p string) bool      { return h.files[p] }
func (h *dirHost) Files() map[string]bool { return h.files }

func (h *dirHost) Read(p string) []byte {
	b, err := os.ReadFile(filepath.Join(h.root, filepath.FromSlash(p)))
	if err != nil {
		return nil
	}
	return b
}

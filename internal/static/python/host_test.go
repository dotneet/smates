package python

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dotneet/smates/internal/static/host"
	"github.com/dotneet/smates/internal/testutil"
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
	testutil.WriteTree(t, root, files)
	h := &dirHost{root: root, files: testutil.Files(t, root)}
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

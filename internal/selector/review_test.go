package selector

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dotneet/smates/internal/gitx"
)

// Files git does not list (ignored, inside submodules) are hashed from disk, so a test
// reading them notices changes.
func TestHasherReadsUnlistedFilesFromDisk(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHasher(gitx.Snapshot{"a.txt": "h1"}).WithRoot(root)
	first := h.Hash(".env")
	if first == "" || h.Hash(".env?") != "exists" || h.Hash("missing.txt") != "" || h.Hash("missing.txt?") != "absent" {
		t.Fatalf("disk fallback: %q %q", first, h.Hash(".env?"))
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("A=2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := NewHasher(gitx.Snapshot{}).WithRoot(root).Hash(".env"); got == first {
		t.Error("changed ignored file hashes the same")
	}
	if NewHasher(gitx.Snapshot{}).Hash(".env") != "" {
		t.Error("without a root, unlisted files are absent")
	}
}

// A submodule bump (one snapshot entry for the gitlink) is explained by tests reading
// files inside it; a regular file is not explained by unrelated deps.
func TestCoversSubmoduleContents(t *testing.T) {
	deps := map[string]bool{"vendor/lib/x.go": true, "a.txt": true}
	if Covers(deps, "vendor/lib", false) != "content" {
		t.Error("gitlink with deps inside should be covered")
	}
	if Covers(deps, "vendor/li", false) != "" {
		t.Error("prefix must end at a path boundary")
	}
}

package trace

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

func TestCommandLineDeps(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	testutil.WriteTree(t, root, map[string]string{"data/a.txt": "a", "data/b.txt": "b", "tool/run.sh": "cat ../data/b.txt\n"})
	for _, tc := range []struct {
		name, command, cwd string
		want               []string
	}{
		{"read-only command", "cat data/a.txt", root, []string{"data/a.txt"}},
		{"shell -c script taken whole", "/bin/sh -c cat data/a.txt && cat data/b.txt", root, []string{"data/a.txt", "data/b.txt"}},
		{"script", "sh run.sh", filepath.Join(root, "tool"), []string{"tool/run.sh", "data/b.txt"}},
		{"unknown program: its directory", "./bin/thing --flag", filepath.Join(root, "data"), []string{"data/**"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CommandLineDeps(tc.command, tc.cwd, root)
			for _, w := range tc.want {
				if !slices.Contains(got, w) {
					t.Errorf("deps %v lack %q", got, w)
				}
			}
		})
	}
	if got := CommandLineDeps("  ", root, root); got != nil {
		t.Errorf("empty command: %v", got)
	}
}

package pathutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

func TestRelKeepsQueryLikeNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("'?' is not valid in Windows file names")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "notes#1"), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.Write(t, root, "notes", "a file named like the stripped path")
	testutil.Write(t, root, "src/a.ts", "x")
	cases := []struct{ in, want string }{
		// Probes of files that do not exist yet, in directories whose names have ? or #.
		{filepath.Join(root, "notes#1", "new.txt"), "notes#1/new.txt"},
		{filepath.Join(root, "what?", "new.txt"), "what?/new.txt"},
		// An absent file with '#' in its name, whose stripped name does not exist.
		{filepath.Join(root, "src", "new#2.ts"), "src/new#2.ts"},
		// The stripped path is a directory, not the module a query was appended to.
		{filepath.Join(root, "src") + "?x", "src?x"},
		// Queries and fragments on existing files are dropped.
		{filepath.Join(root, "src", "a.ts") + "?v=3&t=/x", "src/a.ts"},
		{filepath.Join(root, "src", "a.ts") + "#L1", "src/a.ts"},
		// A file:// URL's query and fragment are parsed off, even for absent files.
		{"file://" + filepath.ToSlash(filepath.Join(root, "src", "gone.ts")) + "?v=1", "src/gone.ts"},
		{"file://" + filepath.ToSlash(filepath.Join(root, "notes%231", "new.txt")), "notes#1/new.txt"},
	}
	for _, c := range cases {
		if got, ok := Rel(root, c.in); !ok || got != c.want {
			t.Errorf("Rel(%q) = %q, %v; want %q", c.in, got, ok, c.want)
		}
	}
}

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
	// Directories whose names hold '#' or '?', with a file each, and a file named like
	// a path with a query (the longest existing file wins).
	testutil.Write(t, root, "c#/x.ts", "x")
	testutil.Write(t, root, "q?/x.ts", "x")
	testutil.Write(t, root, "c#d#/x.ts", "x")
	testutil.Write(t, root, "src/b.ts?raw", "a file whose name holds a query")
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
		// The query or fragment of a file in a directory whose name holds '#' or '?':
		// the last mark after which the part before it is a file starts it.
		{filepath.Join(root, "c#", "x.ts") + "?v=3", "c#/x.ts"},
		{filepath.Join(root, "c#", "x.ts") + "#L1", "c#/x.ts"},
		{filepath.Join(root, "c#", "x.ts") + "?v=3#L1", "c#/x.ts"},
		{filepath.Join(root, "c#", "x.ts") + "?t=/a#b", "c#/x.ts"},
		{filepath.Join(root, "q?", "x.ts") + "?v=3", "q?/x.ts"},
		{filepath.Join(root, "q?", "x.ts") + "#L1", "q?/x.ts"},
		{filepath.Join(root, "c#d#", "x.ts") + "?v=3", "c#d#/x.ts"},
		// The files themselves, and probes of absent files in such directories.
		{filepath.Join(root, "c#", "x.ts"), "c#/x.ts"},
		{filepath.Join(root, "c#", "new.ts"), "c#/new.ts"},
		{filepath.Join(root, "q?", "new.ts") + "?v=3", "q?/new.ts?v=3"},
		// A file whose name holds a query is the longest existing file.
		{filepath.Join(root, "src", "b.ts") + "?raw", "src/b.ts?raw"},
		{filepath.Join(root, "src", "b.ts") + "?raw#L1", "src/b.ts?raw"},
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

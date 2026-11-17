package static

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/static/host"
)

// A source the graph cannot read has unknown imports: Unresolved says so, for every
// analyzed language, so that its empty dependencies do not pass for a complete closure.
func TestUnresolvedUnreadable(t *testing.T) {
	root := t.TempDir()
	write := func(p, content string) {
		t.Helper()
		f := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	big := strings.Repeat("x", maxRead+1)
	write("big.js", big)
	write("big.py", big)
	write("big.go", "package big\n//"+big)
	write("ok.js", `import "./big.js"`)
	write("ok.py", "import os\n")
	write("notes.txt", big)
	if err := os.MkdirAll(filepath.Join(root, "dir.ts"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{"big.js", "big.py", "big.go", "ok.js", "ok.py", "notes.txt", "dir.ts", "gone.ts"}
	g := New(root, files, Options{})
	unreadable := []string{host.UnreadableSpec}
	for _, c := range []struct {
		file string
		want []string
	}{
		{"big.js", unreadable},  // larger than maxRead
		{"big.py", unreadable},  // a language without a ResolveReporter
		{"big.go", unreadable},  // likewise
		{"dir.ts", unreadable},  // not a regular file
		{"gone.ts", unreadable}, // listed, but missing on disk
		{"ok.js", nil},          // must keep: a readable file that resolves
		{"ok.py", nil},          // must keep: nothing to report
		{"notes.txt", nil},      // no analyzer reads it: its closure tells nothing anyway
		{"unknown.ts", nil},     // not a file of the tree
	} {
		if got := g.Unresolved(c.file); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Unresolved(%s) = %q, want %q", c.file, got, c.want)
		}
	}
}

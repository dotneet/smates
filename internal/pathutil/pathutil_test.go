package pathutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

func TestRel(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(root, "src", "sub"), 0o755)
	testutil.Write(t, root, "src/a.ts", "x")
	// Not a valid file name on Windows: the write may fail there.
	os.WriteFile(filepath.Join(root, "src", "q?x.ts"), []byte("x"), 0o644)
	testutil.Write(t, root, "my file.ts", "x")
	// file:///C:/... on Windows, file:///tmp/... elsewhere.
	fileURL := func(p string) string { return "file://" + "/" + strings.TrimPrefix(filepath.ToSlash(p), "/") }
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{filepath.Join(root, "src", "a.ts"), "src/a.ts", true},
		{filepath.Join(root, "src", "..", "src", "a.ts"), "src/a.ts", true},
		{fileURL(filepath.Join(root, "src", "a.ts")), "src/a.ts", true},
		{fileURL(filepath.Join(root, "my%20file.ts")), "my file.ts", true},
		{filepath.Join(root, "src", "a.ts") + "?v=3", "src/a.ts", true},
		{filepath.Join(root, "src", "a.ts") + "#frag", "src/a.ts", true},
		{filepath.Join(root, "src", "q?x.ts"), "src/q?x.ts", true}, // an existing name with "?"
		{filepath.Join(root, "missing", "x.ts"), "missing/x.ts", true},
		{filepath.Join(root, "src", "sub"), "src/sub", true},
		{root, "", false},
		{filepath.Dir(root), "", false},
		{filepath.Join(root, "..", "elsewhere"), "", false},
		{"relative/path", "", false},
		{"file://%zz", "", false},
	}
	for _, c := range cases {
		got, ok := Rel(root, c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("Rel(%q) = %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestFileURLPath(t *testing.T) {
	cases := []struct {
		in      string
		windows bool
		want    string
	}{
		{"/repo/a.ts", false, "/repo/a.ts"},
		{"/C:/x", false, "/C:/x"},
		{"/C:/repo/a.ts", true, filepath.FromSlash("C:/repo/a.ts")},
		{"/d:/r", true, filepath.FromSlash("d:/r")},
		{"//server/share/a.ts", true, filepath.FromSlash("//server/share/a.ts")},
		{"/1:/x", true, filepath.FromSlash("/1:/x")},
	}
	for _, c := range cases {
		if got := fileURLPath(c.in, c.windows); got != c.want {
			t.Errorf("fileURLPath(%q, %v) = %q, want %q", c.in, c.windows, got, c.want)
		}
	}
}

func TestRelThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	testutil.Write(t, root, "pkg/a.go", "x")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		filepath.Join(link, "pkg", "a.go"):           "pkg/a.go",
		filepath.Join(link, "pkg", "missing.json"):   "pkg/missing.json", // probe of a file that does not exist yet
		filepath.Join(link, "newdir", "deep", "x.y"): "newdir/deep/x.y",
	} {
		if got, ok := Rel(root, in); !ok || got != want {
			t.Errorf("Rel(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	// A symlink inside the repository pointing outside of it is outside.
	out := t.TempDir()
	testutil.Write(t, out, "secret", "x")
	os.Symlink(out, filepath.Join(root, "ext"))
	if got, ok := Rel(root, filepath.Join(root, "ext", "secret")); ok {
		t.Errorf("symlink to outside resolved to %q", got)
	}
}

func TestUnderRelToJoin(t *testing.T) {
	under := []struct {
		p, dir string
		want   bool
	}{
		{"a/b", ".", true}, {"a/b", "", true}, {"a/b", "a", true}, {"a", "a", true}, {"ab/c", "a", false}, {"a", "a/b", false},
	}
	for _, c := range under {
		if Under(c.p, c.dir) != c.want {
			t.Errorf("Under(%q,%q)", c.p, c.dir)
		}
	}
	for _, c := range [][3]string{{"a/b", ".", "a/b"}, {"a/b", "", "a/b"}, {"a/b", "a", "b"}, {"a", "a", "."}, {"a/b/c", "a/b", "c"}} {
		if got := RelTo(c[0], c[1]); got != c[2] {
			t.Errorf("RelTo(%q,%q) = %q", c[0], c[1], got)
		}
	}
	if Join("a", "../b", "c/") != "b/c" || Join(".", ".") != "." || Join("a", "..") != "." {
		t.Error("Join")
	}
}

func TestMatchAndVendored(t *testing.T) {
	if !Match([]string{"x", "**/*.md"}, "docs/a/b.md") || Match([]string{"*.md"}, "docs/b.md") || Match(nil, "a") || Match([]string{"[bad"}, "a") {
		t.Error("Match")
	}
	vendored := map[string]bool{
		"node_modules/x/i.js": true, "a/node_modules/b": true, ".git/config": true, "src/__pycache__/m.pyc": true,
		"x.pyc": true, ".venv/lib/a.py": true, ".smates/state.json.gz": true, "a/.pytest_cache/v": true, ".tox/x": true,
		"src/node_modules_extra/x": false, "src/a.py": false, "venv/lib/a.py": false, "my.git/x": false,
	}
	for p, want := range vendored {
		if IsVendored(p) != want {
			t.Errorf("IsVendored(%q) != %v", p, want)
		}
	}
}

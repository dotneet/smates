package testutil

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestWriteTreeAndRemove(t *testing.T) {
	dir := t.TempDir()
	WriteTree(t, dir, map[string]string{"a.txt": "a", "sub/deep/b.txt": "b"})
	if p := Write(t, dir, "sub/c.txt", "c"); p != filepath.Join(dir, "sub", "c.txt") {
		t.Errorf("Write returned %q", p)
	}
	want := map[string]bool{"a.txt": true, "sub/deep/b.txt": true, "sub/c.txt": true}
	if got := Files(t, dir); !reflect.DeepEqual(got, want) {
		t.Errorf("Files = %v, want %v", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "sub", "deep", "b.txt")); err != nil || string(b) != "b" {
		t.Errorf("content %q %v", b, err)
	}
	Remove(t, dir, "sub/deep")
	Remove(t, dir, "missing")
	want = map[string]bool{"a.txt": true, "sub/c.txt": true}
	if got := Files(t, dir); !reflect.DeepEqual(got, want) {
		t.Errorf("after Remove: %v, want %v", got, want)
	}
}

func TestWriteMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix permissions")
	}
	p := WriteMode(t, t.TempDir(), "bin/run.sh", "#!/bin/sh\n", 0o755)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("mode %v is not executable", fi.Mode())
	}
}

func TestGitRepo(t *testing.T) {
	r := NewGitRepo(t, map[string]string{"a.txt": "1", "dir/b.txt": "2"})
	if b := r.Git("rev-parse", "--abbrev-ref", "HEAD"); b != "main" {
		t.Errorf("branch %q", b)
	}
	if got := r.Git("log", "--format=%an <%ae> %s"); got != "smates <smates@example.com> initial" {
		t.Errorf("log %q", got)
	}
	if got := r.Git("ls-files"); got != "a.txt\ndir/b.txt" {
		t.Errorf("ls-files %q", got)
	}
	r.Write("a.txt", "changed")
	r.Remove("dir/b.txt")
	r.Write("hooks-off", "x")
	// A pre-commit hook that fails must not run.
	WriteMode(t, r.Dir, ".git/hooks/pre-commit", "#!/bin/sh\nexit 1\n", 0o755)
	sha := r.Commit("second")
	if len(sha) != 40 || sha != r.Git("rev-parse", "HEAD") {
		t.Errorf("Commit returned %q", sha)
	}
	if got := r.Git("show", "--name-status", "--format=", "HEAD"); got != "M\ta.txt\nD\tdir/b.txt\nA\thooks-off" {
		t.Errorf("second commit %q", got)
	}

	empty := NewGitRepo(t, nil)
	if got := empty.Git("status", "--porcelain"); got != "" {
		t.Errorf("empty repo status %q", got)
	}
}

// TestOnlyImportedByTests checks that no non-test Go file of the module imports
// testutil, so it is never linked into the smates binary.
func TestOnlyImportedByTests(t *testing.T) {
	root := filepath.Join("..", "..")
	const self = "github.com/dotneet/smates/internal/testutil"
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); p != root && (n == "testdata" || n == "fixtures" || n == "node_modules" || strings.HasPrefix(n, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); path == self {
				t.Errorf("%s imports %s: it is for _test.go files only", p, self)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

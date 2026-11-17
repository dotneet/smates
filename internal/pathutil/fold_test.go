package pathutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

// TestRelCaseInsensitive: on a file system that ignores case, a path that names a file
// of the repository in another case maps to the repository's path in the case on disk
// (as git lists it), whichever part of it differs: the root, a system prefix the root
// is reached through (/private on macOS) or an element below the root. Skipped where
// the temporary directory's file system is case-sensitive.
func TestRelCaseInsensitive(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	testutil.Write(t, root, "src/Lib/a.ts", "x")
	testutil.Write(t, root, "README.md", "x")
	if !caseInsensitive(root) {
		t.Skip("case-sensitive file system")
	}
	// flipLast flips the case of root's last element (the repository directory).
	flipLast := filepath.Join(filepath.Dir(root), flipCase(filepath.Base(root)))
	upper := strings.ToUpper(root)
	type row struct {
		name, root, in, want string
		ok                   bool
	}
	rows := []row{
		{"exact", root, filepath.Join(root, "src", "Lib", "a.ts"), "src/Lib/a.ts", true},
		{"root case differs in the path", root, filepath.Join(flipLast, "src", "Lib", "a.ts"), "src/Lib/a.ts", true},
		{"root case differs in the root", flipLast, filepath.Join(root, "src", "Lib", "a.ts"), "src/Lib/a.ts", true},
		{"whole path upper-cased", root, filepath.Join(upper, "SRC", "LIB", "A.TS"), "src/Lib/a.ts", true},
		{"element below root", root, filepath.Join(root, "SRC", "lib", "A.ts"), "src/Lib/a.ts", true},
		{"file at the top", root, filepath.Join(root, "readme.MD"), "README.md", true},
		{"directory", root, filepath.Join(root, "Src", "LIB"), "src/Lib", true},
		// A probe keeps the case of what does not exist, below what does.
		{"missing below miscased dir", root, filepath.Join(root, "SRC", "New", "X.ts"), "src/New/X.ts", true},
		{"root itself in another case", root, flipLast, "", false},
		{"sibling sharing a prefix", root, flipLast + "x" + string(filepath.Separator) + "a.ts", "", false},
		{"outside", root, filepath.Join(filepath.Dir(root), "elsewhere", "a.ts"), "", false},
	}
	if runtime.GOOS == "darwin" && strings.HasPrefix(root, "/private/") {
		// t.TempDir is under /var -> /private/var: a path through the link or /private
		// in another case is the same file.
		tail := strings.TrimPrefix(root, "/private")
		rows = append(rows,
			row{"/private in another case", root, filepath.Join("/PRIVATE"+tail, "src", "Lib", "a.ts"), "src/Lib/a.ts", true},
			row{"through /var in another case", root, filepath.Join("/VAR"+strings.TrimPrefix(tail, "/var"), "SRC", "lib", "a.ts"), "src/Lib/a.ts", true},
		)
	}
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := Rel(c.root, c.in); got != c.want || ok != c.ok {
				t.Errorf("Rel(%q, %q) = %q %v, want %q %v", c.root, c.in, got, ok, c.want, c.ok)
			}
		})
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink("Lib", filepath.Join(root, "src", "cur")); err != nil {
			t.Fatal(err)
		}
		if got := Links(flipLast, filepath.Join(root, "SRC", "CUR", "a.ts")); strings.Join(got, ",") != "src/cur" {
			t.Errorf("Links through miscased path = %q, want [src/cur]", got)
		}
	}
}

// TestRelCreatedAfterListing: a file created after its directory was listed for case
// folding is still found in its case on disk.
func TestRelCreatedAfterListing(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	testutil.Write(t, root, "dir/a.ts", "x")
	if !caseInsensitive(root) {
		t.Skip("case-sensitive file system")
	}
	if got, _ := Rel(root, filepath.Join(root, "DIR", "a.ts")); got != "dir/a.ts" {
		t.Fatalf("got %q", got)
	}
	testutil.Write(t, root, "dir/New.ts", "x")
	if got, _ := Rel(root, filepath.Join(root, "dir", "new.TS")); got != "dir/New.ts" {
		t.Errorf("got %q, want dir/New.ts", got)
	}
}

// TestCaseFoldHelpers covers the parts of case folding that do not depend on the file
// system: matching root ignoring case, and taking the case of listed entries.
func TestCaseFoldHelpers(t *testing.T) {
	sep := string(filepath.Separator)
	j := func(e ...string) string { return strings.Join(e, sep) }
	for _, c := range []struct {
		root, p, want string
		ok            bool
	}{
		{j("", "Users", "me", "repo"), j("", "users", "ME", "Repo", "src", "a.ts"), j("src", "a.ts"), true},
		{j("", "Users", "me", "repo"), j("", "Users", "me", "repo"), "", false},
		{j("", "Users", "me", "repo"), j("", "Users", "me", "repox", "a"), "", false},
		{j("", "Users", "me", "repo"), j("", "Users", "me"), "", false},
		{sep, j("", "a", "b"), j("a", "b"), true},
		{j("", "r"), j("", "R", "..", "x"), "", false}, // not clean
	} {
		if got, ok := trimRootFold(c.root, c.p); got != c.want || ok != c.ok {
			t.Errorf("trimRootFold(%q, %q) = %q %v, want %q %v", c.root, c.p, got, ok, c.want, c.ok)
		}
	}
	if flipCase("aB-ç1") != "Ab-Ç1" {
		t.Errorf("flipCase = %q", flipCase("aB-ç1"))
	}
	// Listing-based: works the same on a case-sensitive file system.
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	testutil.Write(t, dir, "Src/Deep/f.txt", "x")
	for in, want := range map[string]string{
		j("src", "deep", "f.txt"):    j("Src", "Deep", "f.txt"),
		j("Src", "Deep", "f.txt"):    j("Src", "Deep", "f.txt"),
		j("SRC", "missing", "F.txt"): j("Src", "missing", "F.txt"),
		j("nope", "deep"):            j("nope", "deep"),
	} {
		if got := canonicalBelow(dir, in); got != want {
			t.Errorf("canonicalBelow(%q) = %q, want %q", in, got, want)
		}
	}
}

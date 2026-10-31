package runner

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoFailureOutput(t *testing.T) {
	b := &buildFailures{}
	out := "--- FAIL: TestAdd (0.00s)\n    add_test.go:9: got 3, want 4\nFAIL\nFAIL\texample.com/m/calc\t0.012s\n" +
		"ok  \texample.com/m/util\t0.003s\n" +
		"# example.com/m/broken\nbroken/b.go:3:2: undefined: x\nFAIL\texample.com/m/broken [build failed]\n" +
		"?   \texample.com/m/empty\t[no test files]\nFAIL\n"
	// Written in odd chunks, as a pipe delivers it.
	for i := 0; i < len(out); i += 7 {
		io.WriteString(b, out[i:min(i+7, len(out))])
	}
	if got := b.output["example.com/m/calc"]; !strings.Contains(got, "add_test.go:9: got 3, want 4") || strings.Contains(got, "util") {
		t.Errorf("calc output %q", got)
	}
	if got := b.output["example.com/m/broken"]; !strings.Contains(got, "undefined: x") || strings.Contains(got, "add_test") {
		t.Errorf("broken output %q", got)
	}
	if _, ok := b.output["example.com/m/util"]; ok || !b.paths["example.com/m/broken"] {
		t.Errorf("output %v paths %v", b.output, b.paths)
	}
}

// A suite below the module root still maps its packages to import paths, so build
// failures are attributed.
func TestFindModuleImportPath(t *testing.T) {
	repo := tempRepo(t, "svc/api/x.go", "svc/api/handlers/h.go", "other/y.go")
	os.WriteFile(filepath.Join(repo.Root, "svc", "go.mod"), []byte("module example.com/svc\n\ngo 1.22\n"), 0o644)
	modDir, mod := findModule(repo.Root, filepath.Join(repo.Root, "svc", "api"))
	if modDir != filepath.Join(repo.Root, "svc") || mod != "example.com/svc" {
		t.Fatalf("findModule = %q %q", modDir, mod)
	}
	for pkg, want := range map[string]string{
		"svc":              "example.com/svc",
		"svc/api":          "example.com/svc/api",
		"svc/api/handlers": "example.com/svc/api/handlers",
		"other":            "",
	} {
		if got := importPath(modDir, mod, filepath.Join(repo.Root, filepath.FromSlash(pkg))); got != want {
			t.Errorf("importPath(%s) = %q, want %q", pkg, got, want)
		}
	}
	if d, m := findModule(repo.Root, filepath.Join(repo.Root, "other")); d != "" || m != "" {
		t.Errorf("no go.mod: %q %q", d, m)
	}
}

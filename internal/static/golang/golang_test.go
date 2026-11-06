package golang

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

// fsHost implements host.Host over a directory on disk.
type fsHost struct {
	root  string
	files map[string]bool
}

func (h *fsHost) Root() string           { return h.root }
func (h *fsHost) Has(p string) bool      { return h.files[p] }
func (h *fsHost) Files() map[string]bool { return h.files }
func (h *fsHost) Read(p string) []byte {
	b, _ := os.ReadFile(filepath.Join(h.root, filepath.FromSlash(p)))
	return b
}

// newRepo writes files (repo-relative path -> content) into a temp dir and returns a
// host over it.
func newRepo(t *testing.T, files map[string]string) *fsHost {
	t.Helper()
	root := t.TempDir()
	testutil.WriteTree(t, root, files)
	return &fsHost{root: root, files: testutil.Files(t, root)}
}

// deps runs the analyzer on file p of h, reading its content from disk.
func deps(a *Analyzer, h *fsHost, p string) []string {
	return a.Deps(p, h.Read(p))
}

func assertDeps(t *testing.T, got, want []string) {
	t.Helper()
	if len(want) == 0 {
		want = nil
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deps mismatch\n got: %q\nwant: %q", got, want)
	}
}

func goSrc(pkg string, imports ...string) string {
	var b strings.Builder
	b.WriteString("package " + pkg + "\n\n")
	if len(imports) > 0 {
		b.WriteString("import (\n")
		for _, i := range imports {
			b.WriteString("\t" + i + "\n")
		}
		b.WriteString(")\n")
	}
	b.WriteString("\nfunc f() {}\n")
	return b.String()
}

func TestHandles(t *testing.T) {
	a := New(newRepo(t, nil), Options{})
	tests := []struct {
		path string
		want bool
	}{
		{"main.go", true},
		{"pkg/a/a_test.go", true},
		{"vendor/x/y.go", true},
		{"pkg/a/a.go.txt", false},
		{"pkg/a/a.ts", false},
		{"node_modules/x/y.go", false},
		{"web/node_modules/x/y.go", false},
	}
	for _, tt := range tests {
		if got := a.Handles(tt.path); got != tt.want {
			t.Errorf("Handles(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestDepsSinglePackageAndImports(t *testing.T) {
	h := newRepo(t, map[string]string{
		"go.mod":                      "module example.com/app\n\ngo 1.22\n",
		"go.sum":                      "",
		"main.go":                     goSrc("main", `"fmt"`, `"example.com/app/internal/store"`, `"github.com/ext/lib"`),
		"main_test.go":                goSrc("main", `"testing"`),
		"internal/store/store.go":     goSrc("store", `db "example.com/app/internal/store/db"`),
		"internal/store/cache.go":     goSrc("store"),
		"internal/store/linux.go":     "//go:build linux\n\n" + goSrc("store"),
		"internal/store/s_test.go":    goSrc("store", `"testing"`),
		"internal/store/x_test.go":    goSrc("store_test", `"example.com/app/internal/store"`),
		"internal/store/_skip.go":     goSrc("store"),
		"internal/store/.hidden.go":   goSrc("store"),
		"internal/store/README.md":    "docs",
		"internal/store/ignore.go":    "//go:build ignore\n\n" + goSrc("main"),
		"internal/store/db/db.go":     goSrc("db"),
		"internal/store/db/asm.s":     "TEXT ·f(SB),0,$0\n",
		"internal/store/db/t_test.go": goSrc("db"),
		"cmd/tool/main.go":            goSrc("main", `_ "example.com/app/missing"`, `. "example.com/app/internal/store/db"`),
	})
	a := New(h, Options{})
	storePkg := []string{"internal/store/cache.go", "internal/store/ignore.go", "internal/store/linux.go", "internal/store/store.go"}
	tests := []struct {
		name string
		file string
		want []string
	}{
		{
			name: "main imports internal package; std and external ignored",
			file: "main.go",
			want: append([]string{"go.mod", "go.sum"}, storePkg...),
		},
		{
			name: "test file sees package sources and sibling tests",
			file: "main_test.go",
			want: []string{"go.mod", "go.sum", "main.go"},
		},
		{
			name: "non-test file sees build-tagged siblings, not tests or ignored names",
			file: "internal/store/store.go",
			want: []string{
				"go.mod", "go.sum",
				"internal/store/cache.go", "internal/store/ignore.go", "internal/store/linux.go",
				"internal/store/db/asm.s", "internal/store/db/db.go",
			},
		},
		{
			name: "internal test file sees external test file of the same directory",
			file: "internal/store/s_test.go",
			want: append([]string{"go.mod", "go.sum", "internal/store/x_test.go"}, storePkg...),
		},
		{
			name: "external test package importing the package under test",
			file: "internal/store/x_test.go",
			want: append([]string{"go.mod", "go.sum", "internal/store/s_test.go"}, storePkg...),
		},
		{
			name: "assembly sources are part of the package",
			file: "internal/store/db/db.go",
			want: []string{"go.mod", "go.sum", "internal/store/db/asm.s"},
		},
		{
			name: "missing package resolves to nothing; dot import resolves",
			file: "cmd/tool/main.go",
			want: []string{"go.mod", "go.sum", "internal/store/db/asm.s", "internal/store/db/db.go"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertDeps(t, deps(a, h, tt.file), tt.want)
		})
	}
}

func TestDepsMultiModule(t *testing.T) {
	h := newRepo(t, map[string]string{
		"go.work":     "go 1.22\n\nuse (\n\t.\n\t./sub\n\t./libs/util\n)\n",
		"go.work.sum": "",
		"go.mod":      "module example.com/root // the root module\n",
		"go.sum":      "",
		"root.go":     goSrc("root", `"example.com/root/sub/inner"`, `"example.com/util"`, `"example.com/root/pkg"`),
		"pkg/p.go":    goSrc("pkg"),
		// Nested module whose path extends the root module path: longest prefix wins.
		"sub/go.mod":         "module \"example.com/root/sub\"\n\nrequire example.com/util v0.0.0\n",
		"sub/go.sum":         "",
		"sub/inner/inner.go": goSrc("inner", `"example.com/util/strs"`),
		// Independent module elsewhere in the tree (no go.sum).
		"libs/util/go.mod":       "module example.com/util\n",
		"libs/util/util.go":      goSrc("util"),
		"libs/util/strs/strs.go": goSrc("strs"),
		// Root module dir shadowed by the nested module: must not be used.
		"sub/inner/inner_test.go": goSrc("inner"),
	})
	a := New(h, Options{})
	// Minimal version selection picks one version per dependency across the workspace:
	// every file depends on the go.mod and go.sum of every module the go.work uses,
	// whether or not it imports that module.
	work := []string{"go.mod", "go.sum", "go.work", "go.work.sum", "sub/go.mod", "sub/go.sum", "libs/util/go.mod"}
	tests := []struct {
		file string
		want []string
	}{
		{"root.go", append([]string{"libs/util/util.go", "pkg/p.go", "sub/inner/inner.go"}, work...)},
		{"sub/inner/inner.go", append([]string{"libs/util/strs/strs.go"}, work...)},
		{"libs/util/strs/strs.go", work},
		{"sub/inner/inner_test.go", append([]string{"sub/inner/inner.go"}, work...)},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertDeps(t, deps(a, h, tt.file), tt.want)
		})
	}
}

// wsModule returns the files of a module at dir with a non-test and a test file and a
// requirement on a third-party module, plus extra files.
func wsModule(dir, modPath string, extra ...string) map[string]string {
	files := map[string]string{
		dir + "/go.mod":    "module " + modPath + "\n\ngo 1.22\n\nrequire example.com/lib v1.2.0\n",
		dir + "/go.sum":    "example.com/lib v1.2.0 h1:abc=\n",
		dir + "/x.go":      goSrc("x"),
		dir + "/x_test.go": goSrc("x", `"testing"`),
	}
	for i := 0; i+1 < len(extra); i += 2 {
		files[extra[i]] = extra[i+1]
	}
	return files
}

// merge combines file maps.
func merge(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func TestDepsWorkspacePins(t *testing.T) {
	// Modules b and c share a workspace; c never imports b, but a require bump in
	// b/go.mod can change the version of example.com/lib c links (minimal version
	// selection picks one version per module across the workspace), so c's tests must
	// depend on b/go.mod and b/go.sum. Module "unused" is in the tree but not in the
	// workspace.
	mods := merge(wsModule("b", "example.com/b"), wsModule("c", "example.com/c"), wsModule("unused", "example.com/unused"))
	own := []string{"c/go.mod", "c/go.sum", "c/x.go", "go.work", "go.work.sum"}
	withB := append([]string{"b/go.mod", "b/go.sum"}, own...)
	all := append([]string{"unused/go.mod", "unused/go.sum"}, withB...)
	tests := []struct {
		name  string
		files map[string]string // $ROOT is replaced by the repository root
		file  string
		want  []string
	}{
		{"use block with comments", merge(mods, map[string]string{
			"go.work": "go 1.22\n\n// modules\nuse (\n\t./b // the b module\n\t./c\n)\n", "go.work.sum": "",
		}), "c/x_test.go", withB},
		{"single use lines", merge(mods, map[string]string{
			"go.work": "go 1.22\n\nuse ./c\nuse ./b\n", "go.work.sum": "",
		}), "c/x_test.go", withB},
		{"bare, quoted and backquoted paths", merge(mods, map[string]string{
			"go.work": "go 1.22\n\nuse b\nuse \"./c\"\nuse `./unused`\n", "go.work.sum": "",
		}), "c/x_test.go", all},
		{"trailing slash and dot segments", merge(mods, map[string]string{
			"go.work": "use ./b/\nuse ./unused/../c\n", "go.work.sum": "",
		}), "c/x_test.go", withB},
		{"Windows separators", merge(mods, map[string]string{
			"go.work": "use .\\b\nuse .\\c\n", "go.work.sum": "",
		}), "c/x_test.go", withB},
		{"use of the workspace directory itself", merge(mods, map[string]string{
			"go.work": "use .\nuse ./c\n", "go.work.sum": "", "go.mod": "module example.com/root\n", "go.sum": "",
		}), "c/x_test.go", append([]string{"go.mod", "go.sum"}, own...)},
		{"non-test file", merge(mods, map[string]string{
			"go.work": "use ./b\nuse ./c\n", "go.work.sum": "",
		}), "c/x.go", []string{"b/go.mod", "b/go.sum", "c/go.mod", "c/go.sum", "go.work", "go.work.sum"}},
		{"parent path from a nested workspace", merge(
			wsModule("b", "example.com/b"), wsModule("ws/c", "example.com/c"),
			map[string]string{"ws/go.work": "use ./c\nuse ../b\n"},
		), "ws/c/x_test.go", []string{"b/go.mod", "b/go.sum", "ws/c/go.mod", "ws/c/go.sum", "ws/c/x.go", "ws/go.work"}},
		// Must keep: a suite run from the root uses the root go.work although the file's
		// nearest go.work is svc/go.work; which one governs is not known, so both count.
		{"nested workspace under a root workspace", merge(
			wsModule("a", "example.com/a"), wsModule("svc/b", "example.com/b"),
			map[string]string{"go.work": "use ./a\nuse ./svc/b\n", "svc/go.work": "use ./b\n"},
		), "svc/b/x_test.go", []string{"a/go.mod", "a/go.sum", "go.work", "svc/b/go.mod", "svc/b/go.sum", "svc/b/x.go", "svc/go.work"}},
		{"absolute path", merge(mods, map[string]string{
			"go.work": "use $ROOT/b\nuse ./c\n", "go.work.sum": "",
		}), "c/x_test.go", withB},
		{"paths outside the repository or without a module are skipped", merge(mods, map[string]string{
			"go.work": "use ./c\nuse ../../elsewhere\nuse ./gone\nuse ./b\n", "go.work.sum": "",
		}), "c/x_test.go", withB},
		{"local replace in the go.work", merge(mods, map[string]string{
			"go.work":          "use ./b\nuse ./c\n\nreplace example.com/lib => ./forks/lib\n",
			"go.work.sum":      "",
			"forks/lib/go.mod": "module example.com/lib\n",
			"forks/lib/go.sum": "",
		}), "c/x_test.go", append([]string{"forks/lib/go.mod"}, withB...)},
		{"local replace in another used module", merge(mods, map[string]string{
			"go.work":          "use ./b\nuse ./c\n",
			"go.work.sum":      "",
			"b/go.mod":         "module example.com/b\n\nrequire example.com/lib v1.2.0\n\nreplace example.com/lib => ../forks/lib\n",
			"forks/lib/go.mod": "module example.com/lib\n",
		}), "c/x_test.go", append([]string{"forks/lib/go.mod"}, withB...)},
		// Outside a workspace other modules do not take part in version selection,
		// but a local replace target does even when nothing imports it.
		{"no workspace", mods, "c/x_test.go", []string{"c/go.mod", "c/go.sum", "c/x.go"}},
		{"no workspace, local replace", merge(mods, map[string]string{
			"c/go.mod":         "module example.com/c\n\nreplace example.com/lib => ../forks/lib\n",
			"forks/lib/go.mod": "module example.com/lib\n",
		}), "c/x_test.go", []string{"c/go.mod", "c/go.sum", "c/x.go", "forks/lib/go.mod"}},
		{"no workspace, local replace with Windows separators", merge(mods, map[string]string{
			"c/go.mod":         "module example.com/c\n\nreplace example.com/lib => ..\\forks\\lib\n",
			"forks/lib/go.mod": "module example.com/lib\n",
		}), "c/x_test.go", []string{"c/go.mod", "c/go.sum", "c/x.go", "forks/lib/go.mod"}},
		{"no workspace, replace by the parent directory", merge(mods, map[string]string{
			"c/go.mod": "module example.com/c\n\nreplace example.com/b => ..\n",
			"go.mod":   "module example.com/b\n",
		}), "c/x_test.go", []string{"c/go.mod", "c/go.sum", "c/x.go", "go.mod"}},
		{"no workspace, replace by a module path", merge(mods, map[string]string{
			"c/go.mod":         "module example.com/c\n\nreplace example.com/lib => example.com/forks/lib v1.0.0\n",
			"forks/lib/go.mod": "module example.com/lib\n",
		}), "c/x_test.go", []string{"c/go.mod", "c/go.sum", "c/x.go"}},
		// A go.work that cannot be parsed keeps every module below it.
		{"unparsable: unterminated block", merge(mods, map[string]string{
			"go.work": "use (\n\t./c\n", "go.work.sum": "",
		}), "c/x_test.go", all},
		{"unparsable: block on one line", merge(mods, map[string]string{
			"go.work": "use ( ./b ./c )\n", "go.work.sum": "",
		}), "c/x_test.go", all},
		{"unparsable: unterminated quote", merge(mods, map[string]string{
			"go.work": "use ./c\nuse \"./b\n", "go.work.sum": "",
		}), "c/x_test.go", all},
		{"unparsable: use without a path", merge(mods, map[string]string{
			"go.work": "use ./c\nuse\n", "go.work.sum": "",
		}), "c/x_test.go", all},
		{"unparsable: bad replace", merge(mods, map[string]string{
			"go.work": "use ./c\nreplace example.com/lib\n", "go.work.sum": "",
		}), "c/x_test.go", all},
		{"no use directive", merge(mods, map[string]string{
			"go.work": "go 1.22\n", "go.work.sum": "",
		}), "c/x_test.go", all},
		{"empty go.work", merge(mods, map[string]string{
			"go.work": "", "go.work.sum": "",
		}), "c/x_test.go", all},
		{"unparsable nested workspace keeps the modules below it", merge(mods,
			wsModule("ws/c", "example.com/wsc"), wsModule("ws/d", "example.com/wsd"),
			map[string]string{"ws/go.work": "use (\n"},
		), "ws/c/x_test.go", []string{"ws/c/go.mod", "ws/c/go.sum", "ws/c/x.go", "ws/d/go.mod", "ws/d/go.sum", "ws/go.work"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRepo(t, tt.files)
			for p, content := range tt.files {
				if strings.Contains(content, "$ROOT") {
					content = strings.ReplaceAll(content, "$ROOT", filepath.ToSlash(h.root))
					if err := os.WriteFile(filepath.Join(h.root, filepath.FromSlash(p)), []byte(content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			assertDeps(t, deps(New(h, Options{}), h, tt.file), tt.want)
		})
	}
}

// countingHost counts reads per file.
type countingHost struct {
	*fsHost
	mu    sync.Mutex
	reads map[string]int
}

func (h *countingHost) Read(p string) []byte {
	h.mu.Lock()
	h.reads[p]++
	h.mu.Unlock()
	return h.fsHost.Read(p)
}

func TestDepsWorkspaceParsedOnce(t *testing.T) {
	fs := newRepo(t, merge(wsModule("b", "example.com/b"), wsModule("c", "example.com/c"), map[string]string{
		"go.work": "use ./b\nuse ./c\n",
	}))
	h := &countingHost{fsHost: fs, reads: map[string]int{}}
	a := New(h, Options{})
	for _, p := range []string{"b/x.go", "b/x_test.go", "c/x.go", "c/x_test.go"} {
		a.Deps(p, fs.Read(p))
	}
	if h.reads["go.work"] != 1 || h.reads["b/go.mod"] != 1 {
		t.Errorf("reads %v, want go.work and b/go.mod read once", h.reads)
	}
}

func TestDepsModuleResolutionEdgeCases(t *testing.T) {
	h := newRepo(t, map[string]string{
		"go.mod": strings.Join([]string{
			"module example.com/m",
			"",
			"replace example.com/forked => ./third_party/forked",
			"replace (",
			"\texample.com/other v1.2.3 => ../outside // escapes the repo",
			"\texample.com/remote => example.com/remote2 v1.0.0",
			"\t\"example.com/quoted\" => \"./third_party/quoted\"",
			")",
		}, "\n"),
		"main.go": goSrc("main",
			`"example.com/forked/sub"`,
			`"example.com/other"`,
			`"example.com/remote"`,
			`"example.com/quoted"`,
			`"example.com/m/vendor/example.com/v"`,
			`"example.com/v"`,
			`"example.com/m/internal/testdata/fixture"`,
			`"C"`,
		),
		"cgo.c":                                "int x;",
		"cgo.h":                                "int x;",
		"third_party/forked/sub/s.go":          goSrc("sub"),
		"third_party/quoted/q.go":              goSrc("quoted"),
		"vendor/modules.txt":                   "# example.com/v v1.0.0\n",
		"vendor/example.com/v/v.go":            goSrc("v"),
		"vendor/example.com/v/go.mod":          "module example.com/v\n",
		"internal/testdata/fixture/fixture.go": goSrc("fixture"),
		"node_modules/pkg/go.mod":              "module example.com/remote\n",
		"node_modules/pkg/r.go":                goSrc("r"),
		// Relative (GOPATH-era) imports.
		"rel/a.go":   goSrc("rel", `"./b"`, `"../third_party/quoted"`, `"../../escape"`),
		"rel/b/b.go": goSrc("b"),
	})
	a := New(h, Options{})
	tests := []struct {
		file string
		want []string
	}{
		// With vendor/modules.txt the build runs in vendor mode: example.com/v is the
		// vendored copy (importing it through the vendor/ path is still rejected).
		{"main.go", []string{
			"go.mod", "cgo.c", "cgo.h", "vendor/modules.txt",
			"third_party/forked/sub/s.go", "third_party/quoted/q.go",
			"internal/testdata/fixture/fixture.go", "vendor/example.com/v/v.go",
		}},
		{"rel/a.go", []string{"go.mod", "vendor/modules.txt", "rel/b/b.go", "third_party/quoted/q.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertDeps(t, deps(a, h, tt.file), tt.want)
		})
	}
}

func TestDepsVendorMode(t *testing.T) {
	h := newRepo(t, map[string]string{
		// A vendored module.
		"go.mod":                         "module example.com/m\n\ngo 1.22\n",
		"vendor/modules.txt":             "# example.com/v v1.0.0\n## explicit\nexample.com/v\nexample.com/w\n",
		"vendor/example.com/v/v.go":      goSrc("v", `"example.com/w"`, `"strings"`),
		"vendor/example.com/v/v_test.go": goSrc("v"),
		"vendor/example.com/w/w.go":      goSrc("w"),
		"pkg/a/a.go":                     goSrc("a", `"example.com/v"`, `"example.com/m/pkg/b"`, `"fmt"`),
		"pkg/a/a_test.go":                goSrc("a", `"example.com/notvendored"`),
		"pkg/b/b.go":                     goSrc("b"),
		// A module without vendor/modules.txt resolves nothing to its vendor copy.
		"plain/go.mod":                    "module example.com/plain\n",
		"plain/vendor/example.com/v/v.go": goSrc("v"),
		"plain/p.go":                      goSrc("p", `"example.com/v"`),
		// A workspace vendor directory serves its modules.
		"ws/go.work":                   "go 1.22\n\nuse ./mod\n",
		"ws/vendor/modules.txt":        "# example.com/x v1.0.0\n",
		"ws/vendor/example.com/x/x.go": goSrc("x"),
		"ws/mod/go.mod":                "module example.com/wsmod\n",
		"ws/mod/m.go":                  goSrc("m", `"example.com/x"`),
	})
	a := New(h, Options{})
	tests := []struct {
		file string
		want []string
	}{
		{"pkg/a/a.go", []string{"go.mod", "vendor/modules.txt", "vendor/example.com/v/v.go", "pkg/b/b.go"}},
		{"pkg/a/a_test.go", []string{"go.mod", "vendor/modules.txt", "pkg/a/a.go"}},
		// Vendored packages resolve their own imports through vendor/ too.
		{"vendor/example.com/v/v.go", []string{"go.mod", "vendor/modules.txt", "vendor/example.com/w/w.go"}},
		{"plain/p.go", []string{"plain/go.mod"}},
		{"ws/mod/m.go", []string{"ws/mod/go.mod", "ws/go.work", "ws/vendor/modules.txt", "ws/vendor/example.com/x/x.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertDeps(t, deps(a, h, tt.file), tt.want)
		})
	}
}

func TestDepsSharedModulePath(t *testing.T) {
	// A fixture module in testdata declaring the same path as the real module: both
	// directories are consulted, and the one that provides the package wins.
	h := newRepo(t, map[string]string{
		"go.mod":                    "module example.com/m\n",
		"a/a.go":                    goSrc("a", `"example.com/m/b"`, `"example.com/m/only"`),
		"b/b.go":                    goSrc("b"),
		"testdata/mod/go.mod":       "module example.com/m\n",
		"testdata/mod/b/b.go":       goSrc("b"),
		"testdata/mod/only/only.go": goSrc("only"),
	})
	a := New(h, Options{})
	assertDeps(t, deps(a, h, "a/a.go"), []string{
		"go.mod", "b/b.go", "testdata/mod/b/b.go", "testdata/mod/only/only.go",
	})
}

func TestDepsNoModule(t *testing.T) {
	h := newRepo(t, map[string]string{
		"src/example.com/app/main.go": goSrc("main", `"example.com/lib"`),
		"src/example.com/lib/lib.go":  goSrc("lib"),
		"other/x.go":                  goSrc("x", `"example.com/lib"`),
	})
	t.Run("without roots", func(t *testing.T) {
		a := New(h, Options{})
		assertDeps(t, deps(a, h, "other/x.go"), nil)
	})
	t.Run("with GOPATH root", func(t *testing.T) {
		a := New(h, Options{Roots: []string{"./src/", "../escape"}})
		assertDeps(t, deps(a, h, "src/example.com/app/main.go"), []string{"src/example.com/lib/lib.go"})
		assertDeps(t, deps(a, h, "other/x.go"), []string{"src/example.com/lib/lib.go"})
	})
}

func TestDepsEmbed(t *testing.T) {
	h := newRepo(t, map[string]string{
		"go.mod": "module example.com/m\n",
		"web/web.go": strings.Join([]string{
			"package web",
			"",
			`import "embed"`,
			"",
			"//go:embed index.html",
			"var index string",
			"",
			"//go:embed static/*.css \"name with space.txt\" `raw.txt`",
			"//go:embed templates",
			"var files embed.FS",
			"",
			"//go:embed all:hidden",
			"var hidden embed.FS",
			"",
			"\t//go:embed sub/*/data.json",
			"var nested embed.FS",
			"",
			"//go:embed ../escape.txt /abs.txt ./dot.txt bad[",
			"//go:embedded notadirective.txt",
			"// go:embed spaced.txt",
			"var bad embed.FS",
		}, "\n"),
		"web/index.html":             "<html>",
		"web/static/a.css":           "",
		"web/static/b.css":           "",
		"web/static/c.js":            "",
		"web/static/deep/d.css":      "",
		"web/name with space.txt":    "",
		"web/raw.txt":                "",
		"web/templates/t.tmpl":       "",
		"web/templates/x/y.tmpl":     "",
		"web/templates/.git/config":  "",
		"web/templates/_draft.tmpl":  "",
		"web/templates/x/_old/o.tmp": "",
		"web/hidden/.env":            "",
		"web/hidden/_x/y":            "",
		"web/sub/one/data.json":      "",
		"web/sub/two/data.json":      "",
		"web/sub/two/other.json":     "",
		"web/notadirective.txt":      "",
		"web/spaced.txt":             "",
		"web/dot.txt":                "",
		"escape.txt":                 "",
		"web/web_test.go":            "package web\n\n//go:embed testdata/*.golden\nvar golden embed.FS\n",
		"web/testdata/a.golden":      "",
		"web/testdata/b.txt":         "",
		"root.go":                    "package m\n\n//go:embed *.txt\nvar s string\n",
		"r1.txt":                     "",
		".hidden.txt":                "",
		"node_modules/x.txt":         "",
	})
	a := New(h, Options{})
	tests := []struct {
		file string
		want []string
	}{
		{"web/web.go", []string{
			"go.mod",
			"web/index.html",
			"web/static/a.css", "web/static/b.css",
			"web/name with space.txt", "web/raw.txt",
			"web/templates/t.tmpl", "web/templates/x/y.tmpl",
			"web/hidden/.env", "web/hidden/_x/y",
			"web/sub/one/data.json", "web/sub/two/data.json",
		}},
		{"web/web_test.go", []string{"go.mod", "web/web.go", "web/testdata/a.golden"}},
		// '*' matches dot-files named directly by the pattern, as in the go command.
		{"root.go", []string{"go.mod", "escape.txt", "r1.txt", ".hidden.txt"}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertDeps(t, deps(a, h, tt.file), tt.want)
		})
	}
}

func TestDepsEdgeInputs(t *testing.T) {
	h := newRepo(t, map[string]string{
		"go.mod":    "module example.com/m\n",
		"a/a.go":    goSrc("a"),
		"b/b.go":    goSrc("b"),
		"b/c.go":    goSrc("b"),
		"broken.go": "package m\n\nimport (\n\t\"example.com/m/a\"\n\t\"example.com/m/b\n",
	})
	a := New(h, Options{})
	t.Run("syntax error keeps imports parsed before it", func(t *testing.T) {
		assertDeps(t, deps(a, h, "broken.go"), []string{"go.mod", "a/a.go"})
	})
	t.Run("nil source does not read from cwd", func(t *testing.T) {
		assertDeps(t, a.Deps("b/b.go", nil), []string{"go.mod", "b/c.go"})
	})
	t.Run("unhandled path", func(t *testing.T) {
		assertDeps(t, a.Deps("node_modules/x/x.go", []byte(goSrc("x", `"example.com/m/a"`))), nil)
	})
	t.Run("file not in tree still resolves", func(t *testing.T) {
		assertDeps(t, a.Deps("new/n.go", []byte(goSrc("n", `"example.com/m/b"`))), []string{"b/b.go", "b/c.go", "go.mod"})
	})
}

func TestDepsConcurrent(t *testing.T) {
	files := map[string]string{"go.mod": "module example.com/m\n"}
	var paths []string
	for _, d := range []string{"a", "b", "c", "d"} {
		for _, f := range []string{"x.go", "y.go", "x_test.go"} {
			p := d + "/" + f
			files[p] = goSrc(d, `"example.com/m/a"`, `"example.com/m/b"`, `"example.com/m/c"`)
			paths = append(paths, p)
		}
	}
	h := newRepo(t, files)
	a := New(h, Options{})
	want := map[string][]string{}
	for _, p := range paths {
		want[p] = New(h, Options{}).Deps(p, h.Read(p))
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, p := range paths {
				if got := a.Deps(p, h.Read(p)); !reflect.DeepEqual(got, want[p]) {
					t.Errorf("Deps(%q) = %q, want %q", p, got, want[p])
				}
			}
		}()
	}
	wg.Wait()
}

func TestParseModFile(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want modFile
	}{
		{"plain", "module example.com/a\n\ngo 1.22\n", modFile{module: "example.com/a"}},
		{"quoted with comment", "// header\nmodule \"example.com/a\" // c\n", modFile{module: "example.com/a"}},
		{"backquoted", "module `example.com/a`\n", modFile{module: "example.com/a"}},
		{"crlf", "module example.com/a\r\n", modFile{module: "example.com/a"}},
		{"module block", "module (\n\texample.com/a\n)\n", modFile{module: "example.com/a"}},
		{
			"replaces",
			"module m\nreplace a => ./a\nreplace a/b v1.0.0 => ../b v1.1.0\nreplace (\n\tc => ./c\n)\nrequire (\n\tx v1\n)\n",
			modFile{module: "m", replaces: []replace{{"a", "./a"}, {"a/b", "../b"}, {"c", "./c"}}},
		},
		{
			// The go command reads "d=>./d" as one token: not a replace directive.
			"replace without spaces",
			"module m\nreplace (\n\tc => ./c\n\td=>./d\n)\n",
			modFile{module: "m", replaces: []replace{{"c", "./c"}}, malformed: true},
		},
		{"malformed", "module\nreplace => x\nreplace a =>\n\"unterminated\n", modFile{malformed: true}},
		{
			"go.work uses",
			"go 1.22\n\n// header\nuse ./a // first\nuse (\n\t./b\n\t\"../c d\" // quoted\n\n\tb2\n) // done\nuse `./e`\ntoolchain go1.22.1\n",
			modFile{uses: []string{"./a", "./b", "../c d", "b2", "./e"}},
		},
		{"use block on one line", "use ( ./a )\n", modFile{malformed: true}},
		{"use without a path", "use\n", modFile{malformed: true}},
		{"use with two paths", "use ./a ./b\n", modFile{malformed: true}},
		{"unterminated block", "use (\n\t./a\n", modFile{uses: []string{"./a"}, malformed: true}},
		{"stray close", "use ./a\n)\n", modFile{uses: []string{"./a"}, malformed: true}},
		{"unterminated quote", "use \"./a\n", modFile{malformed: true}},
		{"bad escape", "use \"./a\\q\"\n", modFile{malformed: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseModFile([]byte(tt.in)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseModFile = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseEmbedArgs(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{" a.txt", []string{"a.txt"}},
		{"\ta.txt  b/*.css\t", []string{"a.txt", "b/*.css"}},
		{` "with space.txt" ` + "`raw dir`", []string{"with space.txt", "raw dir"}},
		{` "esc\"aped" x`, []string{`esc"aped`, "x"}},
		{` all:dir "all:q"`, []string{"all:dir", "all:q"}},
		{` ok "unterminated`, []string{"ok"}},
		{` ok "bad\q"`, []string{"ok"}},
		{"", nil},
	}
	for _, tt := range tests {
		if got := parseEmbedArgs(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("parseEmbedArgs(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestValidEmbedPattern(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"a.txt", true},
		{"dir/*.css", true},
		{"[ab].txt", true},
		{"", false},
		{"/abs", false},
		{"./a", false},
		{"a/../b", false},
		{"a//b", false},
		{"dir/", false},
		{"bad[", false},
	}
	for _, tt := range tests {
		if got := validEmbedPattern(tt.in); got != tt.want {
			t.Errorf("validEmbedPattern(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestSplitElems(t *testing.T) {
	tests := []struct {
		p          string
		n          int
		head, rest string
	}{
		{"a", 1, "a", ""},
		{"a/b/c", 1, "a", "b/c"},
		{"a/b/c", 2, "a/b", "c"},
		{"a/b/c", 3, "a/b/c", ""},
		{"a/b", 5, "a/b", ""},
	}
	for _, tt := range tests {
		head, rest := splitElems(tt.p, tt.n)
		if head != tt.head || rest != tt.rest {
			t.Errorf("splitElems(%q, %d) = %q, %q; want %q, %q", tt.p, tt.n, head, rest, tt.head, tt.rest)
		}
	}
}

func TestDepsAbsoluteReplace(t *testing.T) {
	h := newRepo(t, map[string]string{
		"go.mod":       "", // written below, once the repo root is known
		"main.go":      goSrc("main", `"example.com/abs"`, `"example.com/out"`),
		"local/abs.go": goSrc("abs"),
	})
	mod := "module example.com/m\n" +
		"replace example.com/abs => " + filepath.ToSlash(filepath.Join(h.root, "local")) + "\n" +
		"replace example.com/out => " + filepath.ToSlash(filepath.Dir(h.root)) + "\n"
	if err := os.WriteFile(filepath.Join(h.root, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	a := New(h, Options{})
	assertDeps(t, deps(a, h, "main.go"), []string{"go.mod", "local/abs.go"})
}

// memHost implements host.Host over an in-memory file map.
type memHost map[string]string

func (h memHost) Root() string         { return "/repo" }
func (h memHost) Has(p string) bool    { _, ok := h[p]; return ok }
func (h memHost) Read(p string) []byte { return []byte(h[p]) }
func (h memHost) Files() map[string]bool {
	out := make(map[string]bool, len(h))
	for p := range h {
		out[p] = true
	}
	return out
}

// BenchmarkDeps measures resolution over a 20k-file module with 1000 packages.
func BenchmarkDeps(b *testing.B) {
	h := memHost{"go.mod": "module example.com/m\n"}
	var paths []string
	for d := 0; d < 1000; d++ {
		dir := "pkg/p" + strconv.Itoa(d)
		for f := 0; f < 20; f++ {
			p := dir + "/f" + strconv.Itoa(f) + ".go"
			h[p] = goSrc("p", `"fmt"`, `"example.com/m/pkg/p`+strconv.Itoa((d+1)%1000)+`"`,
				`"example.com/m/pkg/p`+strconv.Itoa((d+7)%1000)+`"`) + "\n//go:embed f.txt\nvar s string\n"
			paths = append(paths, p)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a := New(h, Options{})
		for _, p := range paths {
			a.Deps(p, []byte(h[p]))
		}
	}
}

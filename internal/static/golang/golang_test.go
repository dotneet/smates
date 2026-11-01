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
		"sub/inner/inner.go": goSrc("inner", `"example.com/util/strs"`),
		// Independent module elsewhere in the tree (no go.sum).
		"libs/util/go.mod":       "module example.com/util\n",
		"libs/util/util.go":      goSrc("util"),
		"libs/util/strs/strs.go": goSrc("strs"),
		// Root module dir shadowed by the nested module: must not be used.
		"sub/inner/inner_test.go": goSrc("inner"),
	})
	a := New(h, Options{})
	tests := []struct {
		file string
		want []string
	}{
		{"root.go", []string{
			"go.mod", "go.sum", "go.work", "go.work.sum",
			"libs/util/util.go", "pkg/p.go", "sub/inner/inner.go",
		}},
		{"sub/inner/inner.go", []string{
			"sub/go.mod", "go.work", "go.work.sum", "libs/util/strs/strs.go",
		}},
		{"libs/util/strs/strs.go", []string{"libs/util/go.mod", "go.work", "go.work.sum"}},
		{"sub/inner/inner_test.go", []string{"sub/go.mod", "go.work", "go.work.sum", "sub/inner/inner.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertDeps(t, deps(a, h, tt.file), tt.want)
		})
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
		{"main.go", []string{
			"go.mod", "cgo.c", "cgo.h",
			"third_party/forked/sub/s.go", "third_party/quoted/q.go",
			"internal/testdata/fixture/fixture.go",
		}},
		{"rel/a.go", []string{"go.mod", "rel/b/b.go", "third_party/quoted/q.go"}},
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
			"module m\nreplace a => ./a\nreplace a/b v1.0.0 => ../b v1.1.0\nreplace (\n\tc => ./c\n\td=>./d\n)\nrequire (\n\tx v1\n)\n",
			modFile{module: "m", replaces: []replace{{"a", "./a"}, {"a/b", "../b"}, {"c", "./c"}}},
		},
		{"malformed", "module\nreplace => x\nreplace a =>\n\"unterminated\n", modFile{}},
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

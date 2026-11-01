package golang

import "testing"

func TestGoRunFileLiteral(t *testing.T) {
	h := newRepo(t, map[string]string{
		"go.mod":            "module example.com/m\n",
		"cmd/tool/main.go":  goSrc("main", `"example.com/m/lib"`),
		"cmd/tool/flags.go": goSrc("main"),
		"lib/lib.go":        goSrc("lib"),
		"pkg/a/a.go":        goSrc("a"),
		"pkg/a/a_test.go":   "package a\n\nimport \"os/exec\"\n\nfunc g() { exec.Command(\"go\", \"run\", \"../../cmd/tool/main.go\", \"../../cmd/tool/flags.go\") }\n",
	})
	a := New(h, Options{})
	assertDeps(t, deps(a, h, "pkg/a/a_test.go"), []string{"go.mod", "pkg/a/a.go", "cmd/tool/main.go", "cmd/tool/flags.go"})
}

func TestCgoHeaderOtherDir(t *testing.T) {
	h := newRepo(t, map[string]string{
		"go.mod":           "module example.com/m\n",
		"include/common.h": "#define X 1\n",
		"pkg/c/c.go":       "package c\n\n// #cgo CFLAGS: -I${SRCDIR}/../../include\n// #include \"common.h\"\nimport \"C\"\n",
		"pkg/c/c_test.go":  "package c\n",
	})
	a := New(h, Options{})
	assertDeps(t, deps(a, h, "pkg/c/c.go"), []string{"go.mod", "include/common.h"})
}

func TestGoWorkReplace(t *testing.T) {
	h := newRepo(t, map[string]string{
		"go.work":          "go 1.22\n\nuse ./app\n\nreplace example.com/dep v1.0.0 => ./forks/dep\n",
		"app/go.mod":       "module example.com/app\n",
		"app/a.go":         goSrc("a", `"example.com/dep"`),
		"forks/dep/go.mod": "module example.com/dep\n",
		"forks/dep/d.go":   goSrc("dep"),
	})
	a := New(h, Options{})
	assertDeps(t, deps(a, h, "app/a.go"), []string{"app/go.mod", "go.work", "forks/dep/d.go"})
}

func TestGoFileLiteralForms(t *testing.T) {
	h := newRepo(t, map[string]string{
		"go.mod":                  "module example.com/m\n",
		"pkg/a/a.go":              goSrc("a"),
		"pkg/a/testdata/gen.go":   goSrc("main", `"example.com/m/lib"`),
		"pkg/a/testdata/other.go": goSrc("main"),
		"lib/lib.go":              goSrc("lib"),
		"pkg/a/a_test.go":         "package a\n\nvar files = []string{\"./testdata/gen.go\", \"testdata/missing.go\", \"../../../outside.go\"}\n",
	})
	a := New(h, Options{})
	assertDeps(t, deps(a, h, "pkg/a/a_test.go"), []string{"go.mod", "pkg/a/a.go", "pkg/a/testdata/gen.go"})
	// The file's own imports are followed by the closure.
	assertDeps(t, deps(a, h, "pkg/a/testdata/gen.go"), []string{"go.mod", "lib/lib.go", "pkg/a/testdata/other.go"})
}

func TestCgoIncludeDirForms(t *testing.T) {
	h := newRepo(t, map[string]string{
		"go.mod":            "module example.com/m\n",
		"include/a.h":       "",
		"third/inc/sub/b.h": "",
		"third/inc/c.h":     "",
		"pkg/c/local.h":     "",
		"pkg/c/c.go": "package c\n\n/*\n#cgo linux CPPFLAGS: -I ${SRCDIR}/../../third/inc -DX=1\n" +
			"#cgo CFLAGS: \"-I${SRCDIR}/../../include\" -I/usr/include\n#cgo LDFLAGS: -L${SRCDIR}/../../third/inc\n" +
			"#include \"a.h\"\n#include \"sub/b.h\"\n#include <c.h>\n#include \"local.h\"\n*/\nimport \"C\"\n",
	})
	a := New(h, Options{})
	assertDeps(t, deps(a, h, "pkg/c/c.go"), []string{"go.mod", "include/a.h", "pkg/c/local.h", "third/inc/sub/b.h"})
}

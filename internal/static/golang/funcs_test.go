package golang

import (
	"slices"
	"strings"
	"testing"
)

func TestDeclsHash(t *testing.T) {
	hash := func(src string, tests bool) string {
		t.Helper()
		h, ok := DeclsHash([]byte(src), tests)
		if !ok {
			t.Fatalf("DeclsHash(%q) failed", src)
		}
		return h
	}
	base := hash("package p\n\nconst N = 1\n\nfunc F() int { return N }\n\nfunc (T) M() {}\n\ntype T struct{}\n", false)
	same := map[string]string{
		"body":        "package p\n\nconst N = 1\n\nfunc F() int { return N + 0 }\n\nfunc (T) M() {}\n\ntype T struct{}\n",
		"method body": "package p\n\nconst N = 1\n\nfunc F() int { return N }\n\nfunc (T) M() { println() }\n\ntype T struct{}\n",
		"whitespace":  "package p\n\n\nconst N = 1\nfunc F() int {\n\treturn N\n}\n\nfunc (T) M() {}\n\n\ntype T struct{}\n",
	}
	for name, src := range same {
		if hash(src, false) != base {
			t.Errorf("%s changed the declarations", name)
		}
	}
	differ := map[string]string{
		"constant":  "package p\n\nconst N = 2\n\nfunc F() int { return N }\n\nfunc (T) M() {}\n\ntype T struct{}\n",
		"signature": "package p\n\nconst N = 1\n\nfunc F() int64 { return N }\n\nfunc (T) M() {}\n\ntype T struct{}\n",
		"method":    "package p\n\nconst N = 1\n\nfunc F() int { return N }\n\ntype T struct{}\n",
		"type":      "package p\n\nconst N = 1\n\nfunc F() int { return N }\n\nfunc (T) M() {}\n\ntype T struct{ X int `json:\"x\"` }\n",
		"embed":     "package p\n\n//go:embed x.txt\nconst N = 1\n\nfunc F() int { return N }\n\nfunc (T) M() {}\n\ntype T struct{}\n",
		"var init":  "package p\n\nconst N = 1\n\nvar v = F()\n\nfunc F() int { return N }\n\nfunc (T) M() {}\n\ntype T struct{}\n",
	}
	for name, src := range differ {
		if hash(src, false) == base {
			t.Errorf("%s did not change the declarations", name)
		}
	}
	if _, ok := DeclsHash([]byte("package p\nfunc {"), false); ok {
		t.Error("a file that does not parse has no declarations hash")
	}

	// Comments that are not directives do not count.
	if hash("// Package p does things.\npackage p\n\n// N is one.\nconst N = 1 // one\n\n// F returns N.\nfunc F() int { return N }\n\nfunc (T) M() {}\n\ntype T struct{}\n", false) != base {
		t.Error("a comment changed the declarations")
	}
	cgo := hash("package p\n\n// int x;\nimport \"C\"\n", false)
	if hash("package p\n\n// int y;\nimport \"C\"\n", false) == cgo {
		t.Error("a cgo preamble did not change the declarations")
	}

	// In a test file: only what can change a test that reaches none of its declarations.
	tb := hash("package p\n\nimport \"testing\"\n\nvar table = []int{1}\n\nconst k = 1\n\nfunc helper(t *testing.T) {}\n\ntype fake struct{}\n\nfunc (fake) M() {}\n\nfunc TestA(t *testing.T) { helper(t) }\n", true)
	for name, src := range map[string]string{
		"new test":    "package p\n\nimport \"testing\"\n\nvar table = []int{1}\n\nconst k = 1\n\nfunc helper(t *testing.T) {}\n\ntype fake struct{}\n\nfunc (fake) M() {}\n\n// TestB is new.\nfunc TestB(t *testing.T) {}\n\nfunc TestA(t *testing.T) { helper(t) }\n",
		"new helper":  "package p\n\nimport \"testing\"\n\nvar table = []int{1}\n\nconst k = 1\n\nfunc helper(t *testing.T) {}\n\nfunc helper2() {}\n\ntype fake struct{}\n\nfunc (fake) M() {}\n\nfunc TestA(t *testing.T) { helper(t) }\n",
		"helper":      "package p\n\nimport \"testing\"\n\nvar table = []int{1}\n\nconst k = 1\n\nfunc helper(t testing.TB) { t.Log() }\n\ntype fake struct{}\n\nfunc (fake) M() {}\n\nfunc TestA(t *testing.T) { helper(t) }\n",
		"table":       "package p\n\nimport \"testing\"\n\nvar table = []int{1, 2}\n\nconst k = 2\n\nfunc helper(t *testing.T) {}\n\ntype fake struct{}\n\nfunc (fake) M() {}\n\nfunc TestA(t *testing.T) { helper(t) }\n",
		"method body": "package p\n\nimport \"testing\"\n\nvar table = []int{1}\n\nconst k = 1\n\nfunc helper(t *testing.T) {}\n\ntype fake struct{}\n\nfunc (fake) M() { println() }\n\nfunc TestA(t *testing.T) { helper(t) }\n",
		"conversion":  "package p\n\nimport \"testing\"\n\nvar table = []int{1}\n\nvar raw = []byte(\"x\")\n\nconst k = 1\n\nfunc helper(t *testing.T) {}\n\ntype fake struct{}\n\nfunc (fake) M() {}\n\nfunc TestA(t *testing.T) { helper(t) }\n",
	} {
		if hash(src, true) != tb {
			t.Errorf("%s changed the test file's declarations", name)
		}
	}
	for name, src := range map[string]string{
		"init":      "package p\n\nimport \"testing\"\n\nvar table = []int{1}\n\nconst k = 1\n\nfunc helper(t *testing.T) {}\n\nfunc init() {}\n\ntype fake struct{}\n\nfunc (fake) M() {}\n\nfunc TestA(t *testing.T) { helper(t) }\n",
		"call var":  "package p\n\nimport \"testing\"\n\nvar table = load()\n\nconst k = 1\n\nfunc helper(t *testing.T) {}\n\ntype fake struct{}\n\nfunc (fake) M() {}\n\nfunc TestA(t *testing.T) { helper(t) }\n",
		"import":    "package p\n\nimport t \"testing\"\n\nvar table = []int{1}\n\nconst k = 1\n\nfunc helper(t *t.T) {}\n\ntype fake struct{}\n\nfunc (fake) M() {}\n\nfunc TestA(t *t.T) { helper(t) }\n",
		"type":      "package p\n\nimport \"testing\"\n\nvar table = []int{1}\n\nconst k = 1\n\nfunc helper(t *testing.T) {}\n\ntype fake struct{ n int }\n\nfunc (fake) M() {}\n\nfunc TestA(t *testing.T) { helper(t) }\n",
		"method":    "package p\n\nimport \"testing\"\n\nvar table = []int{1}\n\nconst k = 1\n\nfunc helper(t *testing.T) {}\n\ntype fake struct{}\n\nfunc (fake) M() {}\n\nfunc (fake) String() string { return \"\" }\n\nfunc TestA(t *testing.T) { helper(t) }\n",
		"build tag": "//go:build linux\n\npackage p\n\nimport \"testing\"\n\nvar table = []int{1}\n\nconst k = 1\n\nfunc helper(t *testing.T) {}\n\ntype fake struct{}\n\nfunc (fake) M() {}\n\nfunc TestA(t *testing.T) { helper(t) }\n",
		"shadow":    "package p\n\nimport \"testing\"\n\nvar table = []int{1}\n\nconst k = 1\n\nfunc helper(t *testing.T) {}\n\nfunc len(s []int) int { return 0 }\n\ntype fake struct{}\n\nfunc (fake) M() {}\n\nfunc TestA(t *testing.T) { helper(t) }\n",
	} {
		if hash(src, true) == tb {
			t.Errorf("%s did not change the test file's declarations", name)
		}
	}
}

func TestDeclHashes(t *testing.T) {
	hashes := func(src string) map[string]string {
		t.Helper()
		h, ok := DeclHashes([]byte(src))
		if !ok {
			t.Fatalf("DeclHashes(%q) failed", src)
		}
		return h
	}
	const base = "package p\n\nconst (\n\tA = iota\n\tB\n)\n\nvar v = 1\n\nfunc F() int { return v }\n\nfunc (t *T[K]) M() {}\n\ntype T[K any] struct{}\n\nfunc init() {}\n\nfunc init() { F() }\n\n// ExampleF prints.\nfunc ExampleF() {\n\t// Output: 1\n}\n"
	b := hashes(base)
	for _, k := range []string{"A", "B", "v", "F", "T.M", "T", "init", "ExampleF"} {
		if b[k] == "" {
			t.Errorf("no hash for %s: %v", k, b)
		}
	}
	if b["A"] != b["B"] {
		t.Error("the constants of a group hash apart")
	}
	cases := []struct {
		name, src string
		changed   []string
	}{
		{"function body", strings.Replace(base, "return v", "return v + 1", 1), []string{"F"}},
		{"comment", strings.Replace(base, "func F() int { return v }", "// F returns v.\nfunc F() int { return v /* v */ }", 1), nil},
		{"constant inserted", strings.Replace(base, "\tA = iota\n", "\tZ = iota\n\tA\n", 1), []string{"A", "B"}},
		{"second init", strings.Replace(base, "{ F() }", "{ F(); F() }", 1), []string{"init"}},
		{"method", strings.Replace(base, "M() {}", "M() { println() }", 1), []string{"T.M"}},
		{"example output", strings.Replace(base, "Output: 1", "Output: 2", 1), []string{"ExampleF"}},
	}
	for _, c := range cases {
		h := hashes(c.src)
		var changed []string
		for k, v := range b {
			if h[k] != v {
				changed = append(changed, k)
			}
		}
		slices.Sort(changed)
		slices.Sort(c.changed)
		if !slices.Equal(changed, c.changed) {
			t.Errorf("%s: changed %v, want %v", c.name, changed, c.changed)
		}
	}
	if _, ok := DeclHashes([]byte("package p\nfunc {")); ok {
		t.Error("a file that does not parse has no declaration hashes")
	}
}

func TestSpans(t *testing.T) {
	src := "package p\n\nvar v = func() int {\n\treturn 1\n}()\n\nfunc F() int {\n\treturn v\n}\n\nfunc (T) M() { F() }\n\ntype T struct{}\n"
	spans, ok := Spans([]byte(src))
	if !ok {
		t.Fatal("Spans failed")
	}
	at := func(line, col int) string {
		for _, s := range spans {
			if s.Contains(line, col) {
				if s.Func {
					return "func " + s.Key
				}
				return s.Key
			}
		}
		return ""
	}
	for _, c := range []struct {
		line, col int
		want      string
	}{{4, 2, "v"}, {7, 14, "func F"}, {8, 2, "func F"}, {11, 15, "func T.M"}, {11, 1, ""}, {2, 1, ""}} {
		if got := at(c.line, c.col); got != c.want {
			t.Errorf("%d:%d: %q, want %q", c.line, c.col, got, c.want)
		}
	}
	if _, ok := Spans([]byte("package p\n//line other.go:10\nfunc F() {}\n")); ok {
		t.Error("a file with //line directives has no spans")
	}
}

func TestParseTestFile(t *testing.T) {
	src := `package p_test

import (
	"testing"
	. "testing"

	"example.com/m/p"
)

var table = []int{1, 2}
var setup = mustSetup()
var raw = []byte("x")
var _ = (*fixture)(nil)
var lazy = func() int { return compute() }

const (
	first = iota
	second
)

type fixture struct{ name string }

func (f *fixture) open() { use(f.name) }

func TestA(t *testing.T)              { helper(t) }
func TestB(t *T)                      {}
func Test(t *testing.T)               {}
func Testify(t *testing.T)            {}
func TestBad(t *testing.T) int        { return 0 }
func TestGeneric[X any](t *testing.T) {}
func Test_under(t *testing.T)         {}
func FuzzF(f *testing.F)              {}
func ExampleE()                       {}
func ExampleOut() {
	// Output:
}
func ExampleBad(x int)      {}
func BenchmarkB(b *testing.B) {}
func TestMain(m *testing.M) {}
func helper(t *testing.T) {
	name := "local"
	x := fixture{name: name}
	x.open()
	t.Run(name, func(t *testing.T) { p.Exported(second) })
}
`
	tf, err := ParseTestFile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"TestA", "TestB", "Test", "Test_under", "FuzzF", "ExampleE", "ExampleOut"}; !slices.Equal(tf.Tests, want) {
		t.Errorf("tests %v, want %v", tf.Tests, want)
	}
	if want := []string{"ExampleE"}; !slices.Equal(tf.Silent, want) {
		t.Errorf("silent %v, want %v", tf.Silent, want)
	}
	if tf.Package != "p_test" {
		t.Errorf("package %q", tf.Package)
	}
	for k, init := range map[string]bool{"table": false, "setup": true, "raw": false, "_": false, "lazy": false, "first": false,
		"fixture": false, "fixture.open": false, "TestA": false, "TestMain": true, "helper": false} {
		d := tf.Decls[k]
		if d == nil {
			t.Errorf("no declaration %s", k)
			continue
		}
		if d.Init != init {
			t.Errorf("%s: init %v, want %v", k, d.Init, init)
		}
	}
	h := tf.Decls["helper"]
	// Not the local name (nor the struct key resolving to it), the parameter t, nor the
	// selected names x.open, t.Run, testing.T.
	if want := []string{"fixture", "p", "second", "testing"}; !slices.Equal(h.Refs, want) {
		t.Errorf("helper refs %v, want %v", h.Refs, want)
	}
	if !slices.Contains(h.Qualified, "Exported") || slices.Contains(h.Qualified, "open") {
		t.Errorf("helper qualified %v", h.Qualified)
	}
	if d := tf.Decls["fixture.open"]; d.Recv != "fixture" || slices.Contains(d.Refs, "name") {
		t.Errorf("method: %+v", d)
	}
	if !tf.Decls["fixture"].Type || tf.Decls["second"].Refs == nil {
		t.Errorf("type %+v, constant %+v", tf.Decls["fixture"], tf.Decls["second"])
	}
	if !slices.Contains(tf.Decls["setup"].Refs, "mustSetup") || !slices.Contains(tf.Decls["lazy"].Refs, "compute") {
		t.Errorf("variable refs: %v %v", tf.Decls["setup"].Refs, tf.Decls["lazy"].Refs)
	}

	// Only calls of the program evaluated at initialization run for every test.
	for src, init := range map[string]bool{
		"package p\nvar table = []int{1}\nvar f = func() { run() }\n": false,
		"package p\nvar x = run()\n":                                  true,
		"package p\nvar x = []byte(\"a\")\n":                          false,
		"package p\nvar x = string(rune(1))\n":                        false,
		"package p\nvar x = make([]int, len(\"ab\"))\n":               false,
		"package p\ntype T int\nvar x = T(1)\n":                       false,
		"package p\nvar x = U(1)\n":                                   true, // another file's type, or a function
		"package p\nvar x = (*T)(nil)\n":                              false,
		"package p\nvar x = (*f)(1)\n":                                true,
		"package p\nvar x = []byte(load())\n":                         true,
		"package p\nvar x = func() int { return 1 }()\n":              true,
		"package p\nconst x = len(\"ab\")\n":                          false,
	} {
		tf, err := ParseTestFile([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if got := tf.Decls["x"] != nil && tf.Decls["x"].Init || tf.Decls["table"] != nil && tf.Decls["f"].Init; got != init {
			t.Errorf("%q: init %v, want %v", src, got, init)
		}
	}
	for _, src := range []string{"package p\nfunc init() {}\n", "package p\nimport \"testing\"\nfunc TestMain(m *testing.M) {}", "package p\nfunc len() {}\n", "package p\ntype error int\n"} {
		tf, err := ParseTestFile([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		init := false
		for _, d := range tf.Decls {
			init = init || d.Init
		}
		if !init {
			t.Errorf("%q runs for every test", src)
		}
	}
	if _, err := ParseTestFile([]byte("package p\nfunc {")); err == nil {
		t.Error("parse error expected")
	}
}

package golang

import (
	"slices"
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

	// In a test file, test functions are left out entirely; helpers keep their signature.
	tb := hash("package p\n\nimport \"testing\"\n\nfunc helper(t *testing.T) {}\n\nfunc TestA(t *testing.T) { helper(t) }\n", true)
	if hash("package p\n\nimport \"testing\"\n\nfunc helper(t *testing.T) {}\n\n// TestB is new.\nfunc TestB(t *testing.T) {}\n\nfunc TestA(t *testing.T) { helper(t) }\n", true) != tb {
		t.Error("a new test function changed the test file's declarations")
	}
	if hash("package p\n\nimport \"testing\"\n\nfunc helper(t testing.TB) {}\n\nfunc TestA(t *testing.T) { helper(t) }\n", true) == tb {
		t.Error("a helper's signature did not change the test file's declarations")
	}
	if hash("package p\n\nimport \"testing\"\n\nfunc helper(t *testing.T) {}\n\nfunc init() {}\n\nfunc TestA(t *testing.T) { helper(t) }\n", true) == tb {
		t.Error("an init function did not change the test file's declarations")
	}
}

func TestParseTestFile(t *testing.T) {
	src := `package p_test

import (
	"testing"
	. "testing"
)

var table = []int{1, 2}
var setup = mustSetup()
var lazy = func() int { return compute() }

type fixture struct{}

func (fixture) open() {}

func TestA(t *testing.T)        {}
func TestB(t *T)                {}
func Test(t *testing.T)         {}
func Testify(t *testing.T)      {}
func TestBad(t *testing.T) int  { return 0 }
func TestGeneric[X any](t *testing.T) {}
func Test_under(t *testing.T)   {}
func FuzzF(f *testing.F)        {}
func ExampleE()                 {}
func ExampleBad(x int)          {}
func BenchmarkB(b *testing.B)   {}
func TestMain(m *testing.M)     {}
func helper()                   { fixture{}.open() }
`
	tf, err := ParseTestFile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"TestA", "TestB", "Test", "Test_under", "FuzzF", "ExampleE"}
	if !slices.Equal(tf.Tests, want) {
		t.Errorf("tests %v, want %v", tf.Tests, want)
	}
	if tf.Package != "p_test" || !tf.Global {
		t.Errorf("package %q global %v", tf.Package, tf.Global)
	}
	for _, d := range []string{"table", "setup", "lazy", "fixture", "open", "helper"} {
		if !slices.Contains(tf.Decls, d) {
			t.Errorf("decls lack %s: %v", d, tf.Decls)
		}
	}
	if !tf.Uses["fixture"] || !tf.Uses["compute"] {
		t.Errorf("uses %v", tf.Uses)
	}

	// Only calls evaluated at initialization make a file global.
	for src, global := range map[string]bool{
		"package p\nvar table = []int{1}\nvar f = func() { run() }\n":   false,
		"package p\nvar x = run()\n":                                    true,
		"package p\nfunc init() {}\n":                                   true,
		"package p\nimport \"testing\"\nfunc TestMain(m *testing.M) {}": true,
	} {
		tf, err := ParseTestFile([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if tf.Global != global {
			t.Errorf("%q: global %v, want %v", src, tf.Global, global)
		}
	}
	if _, err := ParseTestFile([]byte("package p\nfunc {")); err == nil {
		t.Error("parse error expected")
	}
}

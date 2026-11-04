package java

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
	b, err := os.ReadFile(filepath.Join(h.root, filepath.FromSlash(p)))
	if err != nil {
		return nil
	}
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

// deps runs a fresh analyzer on file p of h, sorted and without p itself (as the graph
// dedupes it).
func deps(h *fsHost, p string) []string {
	var out []string
	for _, d := range New(h, Options{}).Deps(p, h.Read(p)) {
		if d != p {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return dedupeSorted(out)
}

func dedupeSorted(in []string) []string {
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

const (
	mainA = "app/src/main/java/com/example/a/"
	mainB = "app/src/main/java/com/example/b/"
	testA = "app/src/test/java/com/example/a/"
)

func TestDeps(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		target string
		want   []string
	}{
		{
			name: "same package needs no import, across source roots",
			files: map[string]string{
				mainA + "Greeter.java":     "package com.example.a;\npublic class Greeter { String greet() { return Consts.PREFIX; } }\n",
				mainA + "Consts.java":      "package com.example.a;\npublic final class Consts { public static final String PREFIX = \"hi\"; }\n",
				mainA + "Unused.java":      "package com.example.a;\npublic class Unused {}\n",
				testA + "GreeterTest.java": "package com.example.a;\nclass GreeterTest { void t() { new Greeter().greet(); } }\n",
			},
			target: testA + "GreeterTest.java",
			want:   []string{mainA + "Greeter.java"},
		},
		{
			name: "an inlined constant is a dependency of its user",
			files: map[string]string{
				mainA + "Greeter.java": "package com.example.a;\npublic class Greeter { String greet() { return Consts.PREFIX; } }\n",
				mainA + "Consts.java":  "package com.example.a;\npublic final class Consts { public static final String PREFIX = \"hi\"; }\n",
			},
			target: mainA + "Greeter.java",
			want:   []string{mainA + "Consts.java"},
		},
		{
			name: "a package-private type declared in another file",
			files: map[string]string{
				mainA + "Greeter.java":    "package com.example.a;\npublic class Greeter {}\nclass Helper { static int twice(int x) { return 2 * x; } }\n",
				testA + "HelperTest.java": "package com.example.a;\nclass HelperTest { int t() { return Helper.twice(2); } }\n",
			},
			target: testA + "HelperTest.java",
			want:   []string{mainA + "Greeter.java"},
		},
		{
			name: "single-type, nested and static imports",
			files: map[string]string{
				mainB + "Store.java": "package com.example.b;\npublic class Store { public static class Entry {} }\n",
				mainB + "Util.java":  "package com.example.b;\npublic class Util { public static int one() { return 1; } }\n",
				mainB + "Math2.java": "package com.example.b;\npublic class Math2 { public static int two() { return 2; } }\n",
				mainB + "Other.java": "package com.example.b;\npublic class Other {}\n",
				mainA + "Use.java": "package com.example.a;\n" +
					"import com.example.b.Store.Entry;\n" +
					"import static com.example.b.Util.one;\n" +
					"import static com.example.b.Math2.*;\n" +
					"class Use { Entry e; int x = one() + two(); }\n",
			},
			target: mainA + "Use.java",
			want:   []string{mainB + "Math2.java", mainB + "Store.java", mainB + "Util.java"},
		},
		{
			name: "an on-demand import adds only the types the file names",
			files: map[string]string{
				mainB + "Store.java": "package com.example.b;\npublic class Store {}\n",
				mainB + "Other.java": "package com.example.b;\npublic class Other {}\n",
				mainA + "Use.java":   "package com.example.a;\nimport com.example.b.*;\nclass Use { Store s; }\n",
			},
			target: mainA + "Use.java",
			want:   []string{mainB + "Store.java"},
		},
		{
			name: "an on-demand import of a type's nested types",
			files: map[string]string{
				mainB + "Store.java": "package com.example.b;\npublic class Store { public static class Entry {} }\n",
				mainA + "Use.java":   "package com.example.a;\nimport com.example.b.Store.*;\nclass Use { Entry e; }\n",
			},
			target: mainA + "Use.java",
			want:   []string{mainB + "Store.java"},
		},
		{
			name: "fully qualified names in code and annotations",
			files: map[string]string{
				mainB + "Store.java":  "package com.example.b;\npublic class Store { public static Object open() { return null; } }\n",
				mainB + "Marker.java": "package com.example.b;\npublic @interface Marker {}\n",
				mainA + "Use.java":    "package com.example.a;\n@com.example.b.Marker\nclass Use { Object o = com.example.b.Store.open(); }\n",
			},
			target: mainA + "Use.java",
			want:   []string{mainB + "Marker.java", mainB + "Store.java"},
		},
		{
			name: "names in comments and literals are not references",
			files: map[string]string{
				mainA + "Greeter.java": "package com.example.a;\npublic class Greeter {}\n",
				mainA + "Use.java": "package com.example.a;\n" +
					"// Greeter\n/* Greeter */\n" +
					"class Use { String s = \"Greeter\"; char c = '\"'; String t = \"\"\"\n  Greeter \"\"\" ;\n  \"\"\"; }\n",
			},
			target: mainA + "Use.java",
			want:   nil,
		},
		{
			name: "a unicode escape cannot hide code",
			files: map[string]string{
				mainA + "Greeter.java": "package com.example.a;\npublic class Greeter {}\n",
				// \u0022 is a quote: the string ends there and Greeter is code.
				mainA + "Use.java": "package com.example.a;\nclass Use { String s = \"a\\u0022; Greeter g; String t = \"b\"; }\n",
			},
			target: mainA + "Use.java",
			want:   []string{mainA + "Greeter.java"},
		},
		{
			name: "package-info.java applies to the package",
			files: map[string]string{
				mainA + "package-info.java": "@Deprecated\npackage com.example.a;\n",
				mainA + "Greeter.java":      "package com.example.a;\npublic class Greeter {}\n",
			},
			target: mainA + "Greeter.java",
			want:   []string{mainA + "package-info.java"},
		},
		{
			name: "every project declaring a name is a candidate",
			files: map[string]string{
				"one/src/main/java/com/example/b/Store.java": "package com.example.b;\npublic class Store {}\n",
				"two/src/main/java/com/example/b/Store.java": "package com.example.b;\npublic class Store {}\n",
				mainA + "Use.java":                           "package com.example.a;\nimport com.example.b.Store;\nclass Use {}\n",
			},
			target: mainA + "Use.java",
			want: []string{
				"one/src/main/java/com/example/b/Store.java",
				"two/src/main/java/com/example/b/Store.java",
			},
		},
		{
			name: "the unnamed package",
			files: map[string]string{
				"src/Greeter.java": "public class Greeter {}\n",
				"src/Use.java":     "class Use { Greeter g; }\n",
			},
			target: "src/Use.java",
			want:   []string{"src/Greeter.java"},
		},
		{
			name: "types of other packages need an import",
			files: map[string]string{
				mainB + "Store.java": "package com.example.b;\npublic class Store {}\n",
				mainA + "Use.java":   "package com.example.a;\nclass Use { Store s; }\n",
			},
			target: mainA + "Use.java",
			want:   nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newRepo(t, c.files)
			if got := deps(h, c.target); !reflect.DeepEqual(got, c.want) {
				t.Errorf("deps = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDepsIgnoresOtherFiles(t *testing.T) {
	h := newRepo(t, map[string]string{"a.go": "package a\n"})
	if got := New(h, Options{}).Deps("a.go", h.Read("a.go")); got != nil {
		t.Errorf("Deps(a.go) = %q, want nil", got)
	}
}

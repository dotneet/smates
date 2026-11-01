package python

import (
	"reflect"
	"testing"
)

func TestPyprojectRoots(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want []string
	}{
		{"none", "[project]\nname = \"x\"\n", nil},
		{"string", "[tool.pytest.ini_options]\npythonpath = \"src\"\n", []string{"src"}},
		{
			"multi-line array with comments",
			"[tool.pytest.ini_options]  # pytest\naddopts = \"-q\"\npythonpath = [\n  \"a\",  # first ]\n  'b',\n]\n[other]\npythonpath = [\"no\"]\n",
			[]string{"a", "b"},
		},
		{"native pytest table", "[ tool . pytest ]\npythonpath = [\"c\"]\n", []string{"c"}},
		{"setuptools find", "[tool.setuptools.packages.find]\nwhere = [\"lib\"]\n", []string{"lib"}},
		{"setuptools package-dir", "[tool.setuptools.package-dir]\n\"\" = \"code\"\nother = \"x/other\"\n", []string{"code"}},
		{"setuptools inline package-dir", "[tool.setuptools]\npackage-dir = {\"\" = \"lib\"}\n", []string{"lib"}},
		{
			"setuptools inline package-dir with other keys",
			"[tool.setuptools]\nzip-safe = false\npackage-dir = { other = \"x/other\", 'x' = \"y\", \"\"='code' }\n",
			[]string{"code"},
		},
		{"setuptools inline package-dir without root", "[tool.setuptools]\npackage-dir = {pkg = \"lib/pkg\"}\n", nil},
		{
			"poetry from",
			"[tool.poetry]\npackages = [\n  { include = \"a\", from = \"lib\" },\n  { include = \"b\" },\n]\n",
			[]string{"lib"},
		},
		{"hatch packages", "[tool.hatch.build.targets.wheel]\npackages = [\"src/pkg\"]\n", []string{"src"}},
		{"escaped quote", "[tool.pytest.ini_options]\npythonpath = [\"a\\\"b\", \"c\"]\n", []string{"a\\\"b", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pyprojectRoots(tt.doc); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("pyprojectRoots = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIniRoots(t *testing.T) {
	tests := []struct {
		name, base, doc string
		want            []string
	}{
		{"pytest.ini", "pytest.ini", "[pytest]\npythonpath = a b\n", []string{"a", "b"}},
		{
			"continuation lines", "tox.ini",
			"[pytest]\npythonpath =\n    a\n\n    b  # comment\n    ; skipped\naddopts = -q\n",
			[]string{"a", "b"},
		},
		{"setup.cfg section", "setup.cfg", "[pytest]\npythonpath = no\n[tool:pytest]\npythonpath: yes\n", []string{"yes"}},
		{"other key", "pytest.ini", "[pytest]\ntestpaths = tests\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := iniRoots(tt.doc, tt.base); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("iniRoots = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJoinRel(t *testing.T) {
	tests := []struct {
		dir, rel string
		want     string
		ok       bool
	}{
		{"", ".", "", true},
		{"", "src", "src", true},
		{"proj", "./lib/", "proj/lib", true},
		{"proj", "..", "", true},
		{"proj", "../..", "", false},
		{"", "../x", "", false},
		{"", "/abs", "", false},
		{"", "C:\\x", "", false},
		{"", "${HOME}", "", false},
		{"", "a\\b", "a/b", true},
		{"", "", "", false},
	}
	for _, tt := range tests {
		got, ok := joinRel(tt.dir, tt.rel)
		if got != tt.want || ok != tt.ok {
			t.Errorf("joinRel(%q, %q) = %q, %v; want %q, %v", tt.dir, tt.rel, got, ok, tt.want, tt.ok)
		}
	}
}

func TestBuildIndex(t *testing.T) {
	h := newFixture(t, map[string]string{
		"pyproject.toml":      "[tool.pytest.ini_options]\npythonpath = [\"../escape\", \"node_modules/x\"]\n",
		"src/pkg/__init__.py": "",
		"tool.py":             "",
		"svc/setup.cfg":       "",
		"svc/src/svcpkg/a.py": "",
		"node_modules/p/x.py": "",
		"my-scripts/run.py":   "",
		"docs/readme.md":      "",
	})
	idx := buildIndex(h, []string{"extra"})
	if want := []string{"", "extra", "src", "svc", "svc/src"}; !reflect.DeepEqual(idx.roots, want) {
		t.Errorf("roots = %q, want %q", idx.roots, want)
	}
	for top, want := range map[string][]string{
		"pkg":    {"src"},
		"tool":   {""},
		"svcpkg": {"svc/src"},
		"src":    {"", "svc"},
		"svc":    {""},
	} {
		if got := idx.tops[top]; !reflect.DeepEqual(got, want) {
			t.Errorf("tops[%q] = %q, want %q", top, got, want)
		}
	}
	if _, ok := idx.tops["my-scripts"]; ok {
		t.Error("non-identifier directory indexed as a top-level name")
	}
	if want := []string{"my-scripts/run.py", "src/pkg/__init__.py", "svc/src/svcpkg/a.py", "tool.py"}; !reflect.DeepEqual(idx.pyFiles, want) {
		t.Errorf("pyFiles = %q, want %q", idx.pyFiles, want)
	}
	if idx.dirs["node_modules/p"] || !idx.dirs["svc/src/svcpkg"] || !idx.dirs[""] {
		t.Errorf("dirs = %v", idx.dirs)
	}
}

package python

import (
	"reflect"
	"testing"
)

func TestDepsRegressions(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		target string
		want   []string
	}{
		{
			name: "conftest basedir on sys.path (prepend mode)",
			files: map[string]string{
				"tests/conftest.py":   "",
				"tests/utils.py":      "",
				"tests/api/test_x.py": "from utils import foo\n",
			},
			target: "tests/api/test_x.py",
			want:   []string{"tests/conftest.py", "tests/utils.py"},
		},
		{
			// The conftest of a package is imported as tests.conftest with the rootdir
			// (the parent of the package) on sys.path: tests/ itself is not a root. No
			// root provides "utils", so the fallback still keeps tests/utils.py: smates
			// cannot tell whether something else puts tests/ on sys.path.
			name: "package conftest basedir is above the package",
			files: map[string]string{
				"tests/__init__.py": "", "tests/conftest.py": "", "tests/utils.py": "",
				"tests/api/__init__.py": "", "tests/api/test_x.py": "from utils import foo\n",
			},
			target: "tests/api/test_x.py",
			want:   []string{"tests/__init__.py", "tests/api/__init__.py", "tests/conftest.py", "tests/utils.py"},
		},
		{
			name: "nested conftests each add their basedir",
			files: map[string]string{
				"tests/conftest.py": "", "tests/helpers.py": "",
				"tests/api/conftest.py": "", "tests/api/fakes.py": "",
				"tests/api/v1/test_x.py": "import helpers\nimport fakes\n",
			},
			target: "tests/api/v1/test_x.py",
			want:   []string{"tests/api/conftest.py", "tests/api/fakes.py", "tests/conftest.py", "tests/helpers.py"},
		},
		{
			// Only test files load conftests: tests/ is no root of app/x.py, but no root
			// provides "utils" either, so the fallback keeps tests/utils.py.
			name: "non-test module gets no conftest roots",
			files: map[string]string{
				"tests/conftest.py": "", "tests/utils.py": "",
				"app/x.py": "from utils import foo\n", "conftest.py": "",
			},
			target: "app/x.py",
			want:   []string{"tests/utils.py"},
		},
		{
			// A root provides "utils": the fallback does not add the other one.
			name: "fallback only when no root provides the name",
			files: map[string]string{
				"utils.py": "", "tests/utils.py": "",
				"app/x.py": "from utils import foo\n",
			},
			target: "app/x.py",
			want:   []string{"utils.py"},
		},
		{
			// A PEP 420 namespace package under a directory no config declares.
			name: "fallback finds a namespace package outside every root",
			files: map[string]string{
				"lib/mypkg/core.py": "",
				"tests/test_x.py":   "from mypkg import core\n",
			},
			target: "tests/test_x.py",
			want:   []string{"lib/mypkg/core.py"},
		},
		{
			name: "fallback finds a module outside every root",
			files: map[string]string{
				"tools/helper.py": "", "other/helper.py": "",
				"tests/test_x.py": "import helper\n",
			},
			target: "tests/test_x.py",
			want:   []string{"other/helper.py", "tools/helper.py"},
		},
		{
			name: "type checking block",
			files: map[string]string{
				"pkg/__init__.py": "", "pkg/a.py": "",
				"test_x.py": "from typing import TYPE_CHECKING\nif TYPE_CHECKING:\n    from pkg import a\n",
			},
			target: "test_x.py",
			want:   []string{"pkg/__init__.py", "pkg/a.py"},
		},
		{
			name: "parenthesized multi-line from import with comments",
			files: map[string]string{
				"pkg/__init__.py": "", "pkg/a.py": "", "pkg/b.py": "",
				"test_x.py": "from pkg import (\n    a,  # c\n    b,\n)\n",
			},
			target: "test_x.py",
			want:   []string{"pkg/__init__.py", "pkg/a.py", "pkg/b.py"},
		},
		{
			name: "import_module literal",
			files: map[string]string{
				"pkg/__init__.py": "", "pkg/a.py": "",
				"test_x.py": "import importlib\nm = importlib.import_module('pkg.a')\n",
			},
			target: "test_x.py",
			want:   []string{"pkg/__init__.py", "pkg/a.py"},
		},
		{
			name: "pytest_plugins in conftest",
			files: map[string]string{
				"tests/__init__.py": "", "tests/plugins/__init__.py": "", "tests/plugins/db.py": "",
				"conftest.py": "pytest_plugins = [\n    'tests.plugins.db',\n]\n",
			},
			target: "conftest.py",
			want:   []string{"tests/__init__.py", "tests/plugins/__init__.py", "tests/plugins/db.py"},
		},
		{
			name: "namespace package across roots",
			files: map[string]string{
				"a/pyproject.toml": "", "a/src/ns/x.py": "",
				"b/pyproject.toml": "", "b/src/ns/y.py": "",
				"tests/test_x.py": "import ns.x\nimport ns.y\n",
			},
			target: "tests/test_x.py",
			want:   []string{"a/src/ns/x.py", "b/src/ns/y.py"},
		},
		{
			name: "rootdir conftest helper in package-less tests with rootdir/tests pytest.ini",
			files: map[string]string{
				"pytest.ini":                "[pytest]\ntestpaths = tests\n",
				"tests/helpers/__init__.py": "", "tests/helpers/f.py": "",
				"tests/conftest.py":    "",
				"tests/unit/test_x.py": "from helpers.f import g\n",
			},
			target: "tests/unit/test_x.py",
			want:   []string{"tests/conftest.py", "tests/helpers/__init__.py", "tests/helpers/f.py"},
		},
	}
	// A package root declared by a build tool: from mypkg import core resolves under it
	// (and, without a declaration, through the fallback).
	for name, cfg := range map[string]map[string]string{
		"setup.cfg package_dir":   {"setup.cfg": "[options]\npackage_dir = =lib\n"},
		"setup.py package_dir":    {"setup.py": "from setuptools import setup\nsetup(package_dir={'': 'lib'})\n"},
		"pdm package-dir":         {"pyproject.toml": "[tool.pdm.build]\npackage-dir = \"lib\"\n"},
		"hatch sources":           {"pyproject.toml": "[tool.hatch.build.targets.wheel]\nsources = [\"lib\"]\n"},
		"hatch packages":          {"pyproject.toml": "[tool.hatch.build.targets.wheel]\npackages = [\"lib/mypkg\"]\n"},
		"no declaration fallback": {},
	} {
		files := map[string]string{
			"lib/mypkg/__init__.py": "", "lib/mypkg/core.py": "",
			"tests/test_core.py": "from mypkg import core\n",
		}
		for k, v := range cfg {
			files[k] = v
		}
		cases = append(cases, struct {
			name   string
			files  map[string]string
			target string
			want   []string
		}{"package root from " + name, files, "tests/test_core.py", []string{"lib/mypkg/__init__.py", "lib/mypkg/core.py"}})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newFixture(t, c.files)
			a := New(h, Options{})
			got := a.Deps(c.target, h.Read(c.target))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v want %v", got, c.want)
			}
		})
	}
}

// The build tools' package roots are import roots of the index (not only found by the
// fallback of resolver.module).
func TestBuildToolRoots(t *testing.T) {
	for name, cfg := range map[string]map[string]string{
		"setup.cfg package_dir": {"setup.cfg": "[options]\npackage_dir = =lib\n"},
		"setup.py package_dir":  {"setup.py": "setup(package_dir={'': 'lib'})\n"},
		"pdm package-dir":       {"pyproject.toml": "[tool.pdm.build]\npackage-dir = \"lib\"\n"},
		"hatch sources":         {"pyproject.toml": "[tool.hatch.build.targets.wheel]\nsources = [\"lib\"]\n"},
	} {
		cfg["lib/mypkg/__init__.py"] = ""
		idx := buildIndex(newFixture(t, cfg), nil)
		if !contains(idx.roots, "lib") || !contains(idx.tops["mypkg"], "lib") {
			t.Errorf("%s: roots %q, tops[mypkg] %q", name, idx.roots, idx.tops["mypkg"])
		}
	}
}

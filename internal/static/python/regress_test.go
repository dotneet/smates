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
			// (the parent of the package) on sys.path: tests/ itself is not a root.
			name: "package conftest basedir is above the package",
			files: map[string]string{
				"tests/__init__.py": "", "tests/conftest.py": "", "tests/utils.py": "",
				"tests/api/__init__.py": "", "tests/api/test_x.py": "from utils import foo\n",
			},
			target: "tests/api/test_x.py",
			want:   []string{"tests/__init__.py", "tests/api/__init__.py", "tests/conftest.py"},
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
			// Only test files load conftests.
			name: "non-test module gets no conftest roots",
			files: map[string]string{
				"tests/conftest.py": "", "tests/utils.py": "",
				"app/x.py": "from utils import foo\n", "conftest.py": "",
			},
			target: "app/x.py",
			want:   nil,
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

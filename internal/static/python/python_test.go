package python

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func TestHandles(t *testing.T) {
	a := New(newFixture(t, nil), Options{})
	for p, want := range map[string]bool{
		"a.py": true, "pkg/b.pyi": true, "c.pyc": false, "d.pyx": false, "e.ts": false, "py": false,
	} {
		if got := a.Handles(p); got != want {
			t.Errorf("Handles(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestDeps(t *testing.T) {
	tests := []struct {
		name   string
		files  map[string]string
		opts   Options
		target string
		want   []string
	}{
		{
			name: "flat layout from-import of submodule and attribute",
			files: map[string]string{
				"pkg/__init__.py":    "",
				"pkg/core.py":        "",
				"pkg/util.py":        "",
				"tests/test_core.py": "from pkg.core import thing\nfrom pkg import util, helper\n",
			},
			target: "tests/test_core.py",
			want:   []string{"pkg/__init__.py", "pkg/core.py", "pkg/util.py"},
		},
		{
			name: "src layout with stubs and ancestor packages",
			files: map[string]string{
				"pyproject.toml":            "[project]\nname = \"mypkg\"\n",
				"src/mypkg/__init__.py":     "",
				"src/mypkg/sub/__init__.py": "",
				"src/mypkg/sub/util.py":     "",
				"src/mypkg/sub/util.pyi":    "",
				"tests/test_util.py":        "import mypkg.sub.util as u\n",
			},
			target: "tests/test_util.py",
			want: []string{
				"src/mypkg/__init__.py", "src/mypkg/sub/__init__.py",
				"src/mypkg/sub/util.py", "src/mypkg/sub/util.pyi",
			},
		},
		{
			name: "monorepo project src and flat layouts",
			files: map[string]string{
				"libs/a/pyproject.toml":       "",
				"libs/a/src/alib/__init__.py": "",
				"libs/b/setup.py":             "",
				"libs/b/blib.py":              "",
				"apps/web/tests/test_x.py":    "import alib\nimport blib\n",
			},
			target: "apps/web/tests/test_x.py",
			want:   []string{"libs/a/src/alib/__init__.py", "libs/b/blib.py"},
		},
		{
			name: "namespace package",
			files: map[string]string{
				"ns/sub/mod.py": "",
				"app.py":        "import ns.sub.mod\nfrom ns import sub\n",
			},
			target: "app.py",
			want:   []string{"ns/sub/mod.py"},
		},
		{
			name: "namespace package split across roots",
			files: map[string]string{
				"pyproject.toml":        "[tool.pytest.ini_options]\npythonpath = [\"plugins\"]\n",
				"src/corp/core.py":      "",
				"plugins/corp/extra.py": "",
				"app.py":                "import corp.core\nimport corp.extra\n",
			},
			target: "app.py",
			want:   []string{"plugins/corp/extra.py", "src/corp/core.py"},
		},
		{
			name: "pytest pythonpath in pyproject",
			files: map[string]string{
				"svc/pyproject.toml":    "[tool.pytest.ini_options]\npythonpath = [\n  \"lib\",  # helpers\n  \".\",\n]\n",
				"svc/lib/helpers.py":    "",
				"svc/settings.py":       "",
				"svc/tests/test_api.py": "import helpers\nimport settings\n",
			},
			target: "svc/tests/test_api.py",
			want:   []string{"svc/lib/helpers.py", "svc/settings.py"},
		},
		{
			name: "pytest.ini, tox.ini and setup.cfg pythonpath",
			files: map[string]string{
				"pytest.ini":        "[pytest]\npythonpath = one\n    two\n",
				"x/tox.ini":         "[pytest]\npythonpath = three\n",
				"y/setup.cfg":       "[tool:pytest]\npythonpath = four\n",
				"one/m1.py":         "",
				"two/m2.py":         "",
				"x/three/m3.py":     "",
				"y/four/m4.py":      "",
				"tests/test_ini.py": "import m1, m2, m3, m4\n",
			},
			target: "tests/test_ini.py",
			want:   []string{"one/m1.py", "two/m2.py", "x/three/m3.py", "y/four/m4.py"},
		},
		{
			name: "setuptools, poetry and hatch package locations",
			files: map[string]string{
				"s/pyproject.toml":        "[tool.setuptools.packages.find]\nwhere = [\"code\"]\n",
				"p/pyproject.toml":        "[tool.poetry]\npackages = [\n  { include = \"pp\", from = \"lib\" },\n]\n",
				"h/pyproject.toml":        "[tool.hatch.build.targets.wheel]\npackages = [\"python/hh\"]\n",
				"s/code/ss.py":            "",
				"p/lib/pp.py":             "",
				"h/python/hh/__init__.py": "",
				"main.py":                 "import ss, pp, hh\n",
			},
			target: "main.py",
			want:   []string{"h/python/hh/__init__.py", "p/lib/pp.py", "s/code/ss.py"},
		},
		{
			name: "setuptools inline package-dir",
			files: map[string]string{
				"pyproject.toml":      "[tool.setuptools]\npackage-dir = {\"\" = \"lib\"}\n",
				"lib/pkg/__init__.py": "",
				"lib/pkg/m.py":        "",
				"tests/test_a.py":     "from pkg import m\n",
			},
			target: "tests/test_a.py",
			want:   []string{"lib/pkg/__init__.py", "lib/pkg/m.py"},
		},
		{
			name: "options roots",
			files: map[string]string{
				"vendor/shared/tool.py": "",
				"main.py":               "import tool\n",
			},
			opts:   Options{Roots: []string{"./vendor/shared/", "../outside"}},
			target: "main.py",
			want:   []string{"vendor/shared/tool.py"},
		},
		{
			name: "relative imports",
			files: map[string]string{
				"pkg/__init__.py":       "",
				"pkg/top.py":            "",
				"pkg/sub/__init__.py":   "",
				"pkg/sub/sib.py":        "",
				"pkg/sub/x/__init__.py": "",
				"pkg/sub/x/y.py":        "",
				"pkg/other/z.py":        "",
				"pkg/other/w.py":        "",
				"pkg/sub/mod.py":        "from . import sib\nfrom .. import top\nfrom .x import y\nfrom ..other.z import w\n",
			},
			target: "pkg/sub/mod.py",
			want: []string{
				"pkg/__init__.py", "pkg/other/z.py", "pkg/sub/__init__.py", "pkg/sub/sib.py",
				"pkg/sub/x/__init__.py", "pkg/sub/x/y.py", "pkg/top.py",
			},
		},
		{
			name: "relative import escaping the repository",
			files: map[string]string{
				"mod.py": "from ... import x\nfrom .. import y\n",
				"x.py":   "",
				"y.py":   "",
			},
			target: "mod.py",
			want:   nil,
		},
		{
			name: "own package __init__ chain",
			files: map[string]string{
				"pkg/__init__.py":     "",
				"pkg/sub/__init__.py": "",
				"pkg/sub/mod.py":      "",
			},
			target: "pkg/sub/mod.py",
			want:   []string{"pkg/__init__.py", "pkg/sub/__init__.py"},
		},
		{
			name: "conftest chain for test files",
			files: map[string]string{
				"conftest.py":             "",
				"tests/conftest.py":       "",
				"tests/unit/conftest.py":  "",
				"tests/unit/test_a.py":    "",
				"tests/other/conftest.py": "",
				"tests/unit/deeper/x.py":  "",
			},
			target: "tests/unit/test_a.py",
			want:   []string{"conftest.py", "tests/conftest.py", "tests/unit/conftest.py"},
		},
		{
			name: "conftest chain for helper under tests dir and for conftest itself",
			files: map[string]string{
				"conftest.py":         "",
				"tests/conftest.py":   "",
				"tests/helpers/db.py": "",
			},
			target: "tests/helpers/db.py",
			want:   []string{"conftest.py", "tests/conftest.py"},
		},
		{
			name: "conftest itself depends on parent conftests",
			files: map[string]string{
				"conftest.py":       "",
				"tests/conftest.py": "",
			},
			target: "tests/conftest.py",
			want:   []string{"conftest.py"},
		},
		{
			name: "no conftest for library code",
			files: map[string]string{
				"conftest.py": "",
				"lib/code.py": "",
			},
			target: "lib/code.py",
			want:   nil,
		},
		{
			name: "conftest chain for Django-style tests.py and *_tests.py",
			files: map[string]string{
				"conftest.py":       "",
				"app/__init__.py":   "",
				"app/tests.py":      "import os\n",
				"lib/code_tests.py": "",
			},
			target: "app/tests.py",
			want:   []string{"app/__init__.py", "conftest.py"},
		},
		{
			name: "conftest chain for *_tests.py",
			files: map[string]string{
				"conftest.py":       "",
				"lib/code_tests.py": "",
			},
			target: "lib/code_tests.py",
			want:   []string{"conftest.py"},
		},
		{
			name: "conftest chain under a *tests* directory",
			files: map[string]string{
				"conftest.py":                   "",
				"unit_tests/conftest.py":        "",
				"unit_tests/helpers/factory.py": "",
			},
			target: "unit_tests/helpers/factory.py",
			want:   []string{"conftest.py", "unit_tests/conftest.py"},
		},
		{
			name: "conftest chain for configured python_files",
			files: map[string]string{
				"pytest.ini":        "[pytest]\npython_files = check_*.py\n",
				"conftest.py":       "",
				"lib/check_code.py": "",
			},
			target: "lib/check_code.py",
			want:   []string{"conftest.py"},
		},
		{
			name: "conftest chain for python_files in pyproject",
			files: map[string]string{
				"pyproject.toml":   "[tool.pytest.ini_options]\npython_files = [\"check_*.py\", \"*_spec.py\"]\n",
				"conftest.py":      "",
				"lib/code_spec.py": "",
			},
			target: "lib/code_spec.py",
			want:   []string{"conftest.py"},
		},
		{
			name: "suffix-style test file",
			files: map[string]string{
				"conftest.py":      "",
				"lib/code_test.py": "",
			},
			target: "lib/code_test.py",
			want:   []string{"conftest.py"},
		},
		{
			name: "top-level package parent root for tests package",
			files: map[string]string{
				"proj/tests/__init__.py":      "",
				"proj/tests/helpers.py":       "",
				"proj/tests/unit/__init__.py": "",
				"proj/tests/unit/test_x.py":   "from tests.helpers import make\n",
			},
			target: "proj/tests/unit/test_x.py",
			want: []string{
				"proj/tests/__init__.py", "proj/tests/helpers.py", "proj/tests/unit/__init__.py",
			},
		},
		{
			name: "sibling module via own directory",
			files: map[string]string{
				"proj/tests/helpers.py": "",
				"proj/tests/test_y.py":  "import helpers\nfrom helpers import thing\n",
			},
			target: "proj/tests/test_y.py",
			want:   []string{"proj/tests/helpers.py"},
		},
		{
			name: "multi-line import, docstrings and nested imports",
			files: map[string]string{
				"pkg/__init__.py": "",
				"pkg/a.py":        "",
				"pkg/b.py":        "",
				"nope.py":         "",
				"lazy.py":         "",
				"main.py": `"""Docs.

import nope
"""
from pkg import (
    a,
    b as bee,  # comment
)


def f():
    # import nope
    import lazy
    s = "import nope"
`,
			},
			target: "main.py",
			want:   []string{"lazy.py", "pkg/__init__.py", "pkg/a.py", "pkg/b.py"},
		},
		{
			name: "star import does not pull submodules",
			files: map[string]string{
				"pkg/__init__.py": "",
				"pkg/x.py":        "",
				"main.py":         "from pkg import *\n",
			},
			target: "main.py",
			want:   []string{"pkg/__init__.py"},
		},
		{
			name: "__all__ submodules in package __init__",
			files: map[string]string{
				"pkg/__init__.py":   "__all__ = [\"a\", \"b\", \"func\"]\n",
				"pkg/a.py":          "",
				"pkg/b/__init__.py": "",
				"pkg/c.py":          "",
			},
			target: "pkg/__init__.py",
			want:   []string{"pkg/a.py", "pkg/b/__init__.py"},
		},
		{
			name: "__all__ outside __init__ is ignored",
			files: map[string]string{
				"pkg/mod.py": "__all__ = [\"a\"]\n",
				"pkg/a.py":   "",
			},
			target: "pkg/mod.py",
			want:   nil,
		},
		{
			name: "dynamic imports",
			files: map[string]string{
				"app/__init__.py":             "",
				"app/plugins/__init__.py":     "",
				"app/plugins/one.py":          "",
				"app/plugins/two/__init__.py": "",
				"app/plugins/two/impl.py":     "",
				"app/other.py":                "",
				"a/b.py":                      "",
				"c.py":                        "",
				"main.py": `import importlib
mod = importlib.import_module(f"app.plugins.{name}")
importlib.import_module("a.b")
__import__("c")
`,
			},
			target: "main.py",
			want: []string{
				"a/b.py", "app/__init__.py", "app/plugins/__init__.py", "app/plugins/one.py",
				"app/plugins/two/__init__.py", "app/plugins/two/impl.py", "c.py",
			},
		},
		{
			name: "dynamic import with partial module name",
			files: map[string]string{
				"drivers/__init__.py":    "",
				"drivers/db_pg.py":       "",
				"drivers/db_sqlite/x.py": "",
				"drivers/cache_redis.py": "",
				"main.py":                "import_module(\"drivers.db_\" + kind)\n",
			},
			target: "main.py",
			want: []string{
				"drivers/__init__.py", "drivers/db_pg.py", "drivers/db_sqlite/x.py",
			},
		},
		{
			name: "relative dynamic imports",
			files: map[string]string{
				"pkg/__init__.py":         "",
				"pkg/plugins/__init__.py": "",
				"pkg/plugins/p1.py":       "",
				"pkg/backends/b1.py":      "",
				"other/__init__.py":       "",
				"other/x.py":              "",
				"pkg/loader.py": `import importlib
importlib.import_module(f".plugins.{n}", __package__)
importlib.import_module(".backends.b1", package=__name__)
importlib.import_module(".x", "other")
`,
			},
			target: "pkg/loader.py",
			want: []string{
				"other/__init__.py", "other/x.py", "pkg/__init__.py", "pkg/backends/b1.py",
				"pkg/plugins/__init__.py", "pkg/plugins/p1.py",
			},
		},
		{
			name: "relative dynamic import in a stub package",
			files: map[string]string{
				"pkg/__init__.pyi":  "",
				"pkg/plugins/p1.py": "",
				"pkg/loader.py":     "import importlib\nimportlib.import_module(f\".plugins.{n}\", __package__)\n",
			},
			target: "pkg/loader.py",
			want:   []string{"pkg/__init__.pyi", "pkg/plugins/p1.py"},
		},
		{
			name: "pytest_plugins and patch targets",
			files: map[string]string{
				"fixtures/__init__.py": "",
				"fixtures/db.py":       "",
				"svc/__init__.py":      "",
				"svc/mail.py":          "",
				"conftest.py":          "pytest_plugins = [\"fixtures.db\"]\n",
				"tests/test_mail.py":   "@mock.patch(\"svc.mail.send\")\ndef test(m): pass\n",
			},
			target: "tests/test_mail.py",
			want:   []string{"conftest.py", "svc/__init__.py", "svc/mail.py"},
		},
		{
			name: "conftest pytest_plugins",
			files: map[string]string{
				"fixtures/__init__.py": "",
				"fixtures/db.py":       "",
				"conftest.py":          "pytest_plugins = [\"fixtures.db\"]\n",
			},
			target: "conftest.py",
			want:   []string{"fixtures/__init__.py", "fixtures/db.py"},
		},
		{
			name: "node_modules is never returned",
			files: map[string]string{
				"pyproject.toml":          "[tool.pytest.ini_options]\npythonpath = [\"node_modules/py\", \"web\"]\n",
				"node_modules/py/evil.py": "",
				"web/node_modules/bad.py": "",
				"main.py":                 "import evil\nimport node_modules\nfrom .node_modules import bad\n",
			},
			target: "main.py",
			want:   nil,
		},
		{
			name: "stdlib and third-party imports resolve to nothing",
			files: map[string]string{
				"main.py": "import os, sys\nfrom collections import abc\nimport numpy as np\nfrom __future__ import annotations\n",
			},
			target: "main.py",
			want:   nil,
		},
		{
			name: "self import is dropped",
			files: map[string]string{
				"main.py": "import main\n",
			},
			target: "main.py",
			want:   nil,
		},
		{
			name: "stub file analyzed",
			files: map[string]string{
				"pkg/__init__.pyi": "",
				"pkg/types.pyi":    "",
				"api.pyi":          "from pkg.types import T\n",
			},
			target: "api.pyi",
			want:   []string{"pkg/__init__.pyi", "pkg/types.pyi"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFixture(t, tt.files)
			a := New(h, tt.opts)
			got := a.Deps(tt.target, h.Read(tt.target))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Deps(%q) =\n  %q\nwant\n  %q", tt.target, got, tt.want)
			}
		})
	}
}

func TestDepsUnhandled(t *testing.T) {
	h := newFixture(t, map[string]string{"a.ts": "import x from './x'"})
	if got := New(h, Options{}).Deps("a.ts", h.Read("a.ts")); got != nil {
		t.Errorf("Deps(a.ts) = %q, want nil", got)
	}
}

func TestDepsConcurrent(t *testing.T) {
	files := map[string]string{"pyproject.toml": "[tool.pytest.ini_options]\npythonpath = [\"src\"]\n"}
	for i := 0; i < 50; i++ {
		files[fmt.Sprintf("src/pkg/m%d.py", i)] = fmt.Sprintf("from pkg import m%d\n", (i+1)%50)
	}
	files["src/pkg/__init__.py"] = ""
	h := newFixture(t, files)
	a := New(h, Options{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				p := fmt.Sprintf("src/pkg/m%d.py", i)
				got := a.Deps(p, h.Read(p))
				want := []string{"src/pkg/__init__.py", fmt.Sprintf("src/pkg/m%d.py", (i+1)%50)}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("Deps(%s) = %q, want %q", p, got, want)
				}
			}
		}()
	}
	wg.Wait()
}

func BenchmarkDeps(b *testing.B) {
	files := map[string]string{"pyproject.toml": ""}
	src := "\"\"\"Module.\"\"\"\nimport os\nfrom pkg import (\n    m1,\n    m2,\n)\nfrom .m3 import x\n\n\ndef f():\n    return f\"{x!r:>10}\"\n"
	for i := 0; i < 200; i++ {
		files[fmt.Sprintf("src/pkg/m%d.py", i)] = src
	}
	files["src/pkg/__init__.py"] = ""
	h := newFixture(b, files)
	a := New(h, Options{})
	content := h.Read("src/pkg/m0.py")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.Deps("src/pkg/m0.py", content)
	}
}

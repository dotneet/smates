package detect

import (
	"reflect"
	"sort"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/testutil"
)

type got struct{ Name, Root, Runner string }

func detect(t *testing.T, files map[string]string) ([]got, []config.Suite) {
	t.Helper()
	dir := t.TempDir()
	var list []string
	for f, c := range files {
		testutil.Write(t, dir, f, c)
		list = append(list, f)
	}
	sort.Strings(list)
	suites := Suites(&gitx.Repo{Root: dir}, list)
	var out []got
	for _, s := range suites {
		out = append(out, got{s.Name, s.Root, s.Runner})
	}
	return out, suites
}

const vitestPkg = `{"name":"x","devDependencies":{"vitest":"^3.0.0"}}`

func TestDetectLayouts(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  []got
	}{
		{
			name:  "single go module",
			files: map[string]string{"go.mod": "module m", "pkg/a.go": "", "pkg/a_test.go": ""},
			want:  []got{{"go", ".", "go"}},
		},
		{
			name: "multi-module go",
			files: map[string]string{
				"go.mod": "module m", "a_test.go": "",
				"tools/go.mod": "module t", "tools/x/x_test.go": "",
				"internal/testdata/mod/go.mod": "module td", "internal/testdata/mod/y_test.go": "",
				"vendor/v/go.mod": "", "vendor/v/v_test.go": "",
			},
			want: []got{{"go:root", ".", "go"}, {"go:tools", "tools", "go"}},
		},
		{
			name:  "go module without tests",
			files: map[string]string{"go.mod": "module m", "main.go": ""},
			want:  nil,
		},
		{
			name: "pnpm workspace with per-package vitest",
			files: map[string]string{
				"package.json": `{"name":"root","private":true}`, "pnpm-workspace.yaml": "packages: ['packages/*']", "pnpm-lock.yaml": "",
				"packages/a/package.json": vitestPkg, "packages/a/src/a.test.ts": "",
				"packages/b/package.json": `{"scripts":{"test":"vitest run"}}`, "packages/b/b.spec.tsx": "",
				"packages/c/package.json":          vitestPkg, // no tests
				"node_modules/vitest/package.json": vitestPkg, "node_modules/vitest/x.test.ts": "",
			},
			want: []got{{"vitest:packages/a", "packages/a", "vitest"}, {"vitest:packages/b", "packages/b", "vitest"}},
		},
		{
			name: "root vitest config owns nested packages",
			files: map[string]string{
				"package.json": vitestPkg, "vitest.config.ts": "export default {}",
				"packages/a/package.json": vitestPkg, "packages/a/a.test.ts": "",
				"packages/b/package.json": `{"name":"b"}`, "packages/b/b.test.ts": "",
			},
			want: []got{{"vitest", ".", "vitest"}},
		},
		{
			name: "root vitest projects own nested packages",
			files: map[string]string{
				"package.json": vitestPkg, "vitest.config.ts": "export default { test: { projects: ['packages/*'] } }",
				"packages/a/package.json": vitestPkg, "packages/a/a.test.ts": "",
				"packages/b/package.json": vitestPkg, "packages/b/vitest.config.ts": "export default { test: { include: ['test/**/*.ts'] } }",
				"packages/b/test/b.ts": "",
			},
			want: []got{{"vitest", ".", "vitest"}},
		},
		{
			name: "root vitest include not covering a nested package",
			files: map[string]string{
				"package.json": vitestPkg, "vitest.config.ts": "export default { test: { include: ['src/**/*.test.ts'] } }",
				"src/root.test.ts":        "",
				"packages/a/package.json": vitestPkg, "packages/a/a.test.ts": "", "packages/a/vitest.config.ts": "export default {}",
				// Vitest finds the root config from packages/b: no tests there.
				"packages/b/package.json": vitestPkg, "packages/b/b.test.ts": "",
			},
			want: []got{{"vitest:root", ".", "vitest"}, {"vitest:packages/a", "packages/a", "vitest"}},
		},
		{
			name: "pytest testpaths in pyproject",
			files: map[string]string{
				"pyproject.toml":    "[tool.pytest.ini_options]\ntestpaths = [\"checks\"]\npython_files = [\"check_*.py\"]\n",
				"checks/check_a.py": "", "src/test_unrelated.py": "",
			},
			want: []got{{"pytest", ".", "pytest"}},
		},
		{
			name: "root vitest without config does not own nested packages",
			files: map[string]string{
				"package.json": vitestPkg, "root.test.ts": "",
				"packages/a/package.json": vitestPkg, "packages/a/a.test.ts": "",
			},
			want: []got{{"vitest:root", ".", "vitest"}, {"vitest:packages/a", "packages/a", "vitest"}},
		},
		{
			name: "vitest detected by config file only",
			files: map[string]string{
				"web/package.json": `{"name":"web"}`, "web/vite.config.mts": "", "web/src/x.test.js": "",
			},
			want: []got{{"vitest", "web", "vitest"}},
		},
		{
			name: "jest via package.json key",
			files: map[string]string{
				"package.json": `{"name":"x","jest":{"testEnvironment":"node"}}`, "src/__tests__/a.js": "", "lib/b.test.js": "",
			},
			want: []got{{"jest", ".", "jest"}},
		},
		{
			name: "jest via config file and dependency",
			files: map[string]string{
				"a/package.json": `{"devDependencies":{"jest":"29"}}`, "a/x.test.js": "",
				"b/package.json": `{}`, "b/jest.config.cjs": "", "b/y.test.js": "",
			},
			want: []got{{"jest:a", "a", "jest"}, {"jest:b", "b", "jest"}},
		},
		{
			name: "bun and node test",
			files: map[string]string{
				"b/package.json": `{"scripts":{"test":"bun test"}}`, "b/x.test.ts": "",
				"c/package.json": `{}`, "c/bun.lock": "", "c/y_test.ts": "",
				"n/package.json": `{"scripts":{"test":"node --test"}}`, "n/test/a.js": "",
				"plain/package.json": `{"scripts":{"test":"echo none"}}`, "plain/z.test.js": "",
				"broken/package.json": `{not json`, "broken/z.test.js": "",
			},
			want: []got{{"bun:b", "b", "bun"}, {"bun:c", "c", "bun"}, {"node", "n", "node"}},
		},
		{
			name: "pytest projects",
			files: map[string]string{
				"pyproject.toml": "[project]\ndependencies = [\"pytest\"]", "tests/test_a.py": "",
				"libs/inner/pyproject.toml": "", "libs/inner/tests/test_b.py": "",
			},
			want: []got{{"pytest", ".", "pytest"}},
		},
		{
			name: "sibling python projects",
			files: map[string]string{
				"svc1/pyproject.toml": "[tool.pytest.ini_options]", "svc1/test_x.py": "",
				"svc2/setup.cfg": "[tool:pytest]", "svc2/pkg/x_test.py": "",
				"svc3/pytest.ini": "", // no tests
			},
			want: []got{{"pytest:svc1", "svc1", "pytest"}, {"pytest:svc2", "svc2", "pytest"}},
		},
		{
			name: "unittest projects (no sign of pytest)",
			files: map[string]string{
				"svc/pyproject.toml": "[project]\nname = \"svc\"", "svc/tests/__init__.py": "", "svc/tests/test_a.py": "",
				"tox/setup.py": "", "tox/tox.ini": "[testenv]\ncommands = pytest", "tox/tests/test_b.py": "",
				"conf/setup.cfg": "", "conf/tests/conftest.py": "", "conf/tests/test_c.py": "",
				"plain/setup.py": "", "plain/testing_utils.py": "", "plain/test_d.py": "",
				"none/pyproject.toml": "", "none/x_test.py": "", // unittest's pattern is test*.py
			},
			want: []got{{"pytest:conf", "conf", "pytest"}, {"unittest:plain", "plain", "unittest"}, {"unittest:svc", "svc", "unittest"}, {"pytest:tox", "tox", "pytest"}},
		},
		{
			name: "polyglot",
			files: map[string]string{
				"go.mod": "module m", "cmd/main_test.go": "",
				"web/package.json": vitestPkg, "web/src/app.test.tsx": "",
				"api/pyproject.toml": "", "api/requirements-dev.txt": "pytest==8.0", "api/tests/test_api.py": "",
				"README.md": "",
			},
			want: []got{{"go", ".", "go"}, {"vitest", "web", "vitest"}, {"pytest", "api", "pytest"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, _ := detect(t, c.files)
			if !reflect.DeepEqual(g, c.want) {
				t.Errorf("got %v\nwant %v", g, c.want)
			}
		})
	}
}

func TestDetectNestedSameRunnerExcludes(t *testing.T) {
	_, suites := detect(t, map[string]string{
		"go.mod": "module m", "a_test.go": "",
		"tools/go.mod": "module t", "tools/x_test.go": "",
	})
	if len(suites) != 2 || !reflect.DeepEqual(suites[0].Exclude, []string{"tools/**"}) || len(suites[1].Exclude) != 0 {
		t.Fatalf("%+v", suites)
	}
	// The detected configuration passes validation.
	c := &config.Config{Suites: suites}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
}

func TestDetectPolyglotRootGoAndRootPytest(t *testing.T) {
	// A Go module and a Python project sharing the repository root get distinct names.
	g, suites := detect(t, map[string]string{
		"go.mod": "module m", "x_test.go": "",
		"pyproject.toml": "", "tests/conftest.py": "", "tests/test_x.py": "",
	})
	want := []got{{"go", ".", "go"}, {"pytest", ".", "pytest"}}
	if !reflect.DeepEqual(g, want) {
		t.Errorf("%v", g)
	}
	if err := (&config.Config{Suites: suites}).Normalize(); err != nil {
		t.Error(err)
	}
}

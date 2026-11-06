package detect

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
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

func TestDetectPythonWithoutProjectFile(t *testing.T) {
	test := "def test_a():\n    pass\n"
	cases := []struct {
		name  string
		files map[string]string
		want  []got
	}{
		{
			name:  "requirements.txt and conftest.py",
			files: map[string]string{"requirements.txt": "pytest==8.0\n", "conftest.py": "", "tests/test_x.py": test},
			want:  []got{{"pytest", ".", "pytest"}},
		},
		{
			name:  "tox.ini with a [pytest] section",
			files: map[string]string{"tox.ini": "[tox]\nenvlist = py3\n\n[pytest]\naddopts = -q\n", "tests/test_x.py": test},
			want:  []got{{"pytest", ".", "pytest"}},
		},
		{
			name:  "conftest.py in tests/ marks the directory above",
			files: map[string]string{"svc/tests/conftest.py": "", "svc/tests/unit/test_x.py": test, "svc/app.py": ""},
			want:  []got{{"pytest", "svc", "pytest"}},
		},
		{
			name:  "requirements file without pytest",
			files: map[string]string{"requirements-dev.txt": "black\n", "tests/test_x.py": "import unittest\n"},
			want:  []got{{"unittest", ".", "unittest"}},
		},
		{
			name: "requirements above project files do not merge the projects",
			files: map[string]string{
				"requirements-dev.txt": "pytest\n", "conftest.py": "",
				"svc1/pyproject.toml": "", "svc1/tests/test_a.py": test,
				"svc2/setup.py": "", "svc2/tests/test_b.py": test,
			},
			want: []got{{"pytest:svc1", "svc1", "pytest"}, {"pytest:svc2", "svc2", "pytest"}},
		},
		{
			name:  "tox.ini without [pytest] and requirements without tests",
			files: map[string]string{"tox.ini": "[testenv]\ncommands = make\n", "docs/requirements.txt": "sphinx\n", "src/app.py": ""},
			want:  nil,
		},
		{
			name: "requirements next to a Go module with Python fixtures in testdata",
			files: map[string]string{
				"go.mod": "module m", "main.go": "", "main_test.go": "",
				"requirements.txt": "mkdocs\n", "internal/py/testdata/proj/test_fixture.py": test,
			},
			want: []got{{"go", ".", "go"}},
		},
		{
			name: "pytest in requirements does not make fixtures a project",
			files: map[string]string{
				"go.mod": "module m", "main_test.go": "",
				"requirements-dev.txt": "pytest\n", "internal/fixtures/test_fixture.py": test,
			},
			want: []got{{"go", ".", "go"}},
		},
		{
			name: "conftest.py in testdata",
			files: map[string]string{
				"go.mod": "module m", "main_test.go": "",
				"internal/py/testdata/tests/conftest.py": "", "internal/py/testdata/tests/test_a.py": test,
			},
			want: []got{{"go", ".", "go"}},
		},
		{
			name: "project file without tests below a weakly marked project",
			files: map[string]string{
				"requirements.txt": "pytest\n", "conftest.py": "", "tests/test_x.py": test,
				"third_party/lib/setup.py": "",
			},
			want: []got{{"pytest", ".", "pytest"}},
		},
		{
			name:  "requirements/ directory",
			files: map[string]string{"requirements/dev.txt": "pytest\n", "tests/test_x.py": test},
			want:  []got{{"pytest", ".", "pytest"}},
		},
		{
			name:  "Pipfile",
			files: map[string]string{"api/Pipfile": "[dev-packages]\npytest = \"*\"\n", "api/tests/test_x.py": test},
			want:  []got{{"pytest", "api", "pytest"}},
		},
		{
			// `python -m unittest` cannot run Django's TestCase (no settings).
			name:  "Django project without pytest",
			files: map[string]string{"manage.py": "", "app/__init__.py": "", "app/tests.py": "from django.test import TestCase\n"},
			want:  nil,
		},
		{
			name: "Django project with requirements but without pytest",
			files: map[string]string{
				"requirements.txt": "django\n", "manage.py": "", "pyproject.toml": "[project]",
				"app/__init__.py": "", "app/tests.py": "from django.test import TestCase\n",
			},
			want: nil,
		},
		{
			name: "Django project with pytest-django",
			files: map[string]string{
				"requirements.txt": "django\npytest-django\n", "manage.py": "",
				"app/__init__.py": "", "app/test_views.py": "from django.test import TestCase\n",
			},
			want: []got{{"pytest", ".", "pytest"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if g, _ := detect(t, c.files); !reflect.DeepEqual(g, c.want) {
				t.Errorf("got %v\nwant %v", g, c.want)
			}
		})
	}
}

func TestDetectCreateReactApp(t *testing.T) {
	g, suites := detect(t, map[string]string{
		"package.json":    `{"dependencies":{"react-scripts":"5.0.1"},"scripts":{"test":"react-scripts test"}}`,
		"src/App.test.js": "",
	})
	if want := []got{{"jest", ".", "jest"}}; !reflect.DeepEqual(g, want) {
		t.Fatalf("got %v, want %v", g, want)
	}
	if want := "npx --no-install react-scripts test --watchAll=false"; suites[0].Command != want {
		t.Errorf("command %q, want %q", suites[0].Command, want)
	}
}

func TestCRACommand(t *testing.T) {
	dir := t.TempDir()
	testutil.Write(t, dir, "pnpm-lock.yaml", "")
	testutil.Write(t, dir, "web/node_modules/.bin/react-scripts", "")
	cases := []struct {
		script string
		want   string
		env    map[string]string
	}{
		{"react-scripts test", "pnpm exec react-scripts test --watchAll=false", nil},
		{
			"CI=true react-scripts test --transformIgnorePatterns 'x/(?!y)' && echo",
			`pnpm exec react-scripts test --transformIgnorePatterns 'x/(?!y)' --watchAll=false`,
			map[string]string{"CI": "true"},
		},
		{
			// Operators inside quotes do not end the command.
			`react-scripts test --transformIgnorePatterns "node_modules/(?!(axios|lodash-es)/)"`,
			`pnpm exec react-scripts test --transformIgnorePatterns 'node_modules/(?!(axios|lodash-es)/)' --watchAll=false`,
			nil,
		},
		{
			"npm run build&&cross-env TZ=UTC NODE_ENV=test react-scripts test --env=jsdom;echo done",
			"pnpm exec react-scripts test --env=jsdom --watchAll=false",
			map[string]string{"TZ": "UTC", "NODE_ENV": "test"},
		},
		{`react-scripts test --testNamePattern "it's a b"`, `pnpm exec react-scripts test --testNamePattern 'it'\''s a b' --watchAll=false`, nil},
		{"react-scripts test --env=jsdom --watchAll=false", "pnpm exec react-scripts test --env=jsdom --watchAll=false", nil},
		{"react-scripts test --watch --watchAll true --coverage", "pnpm exec react-scripts test --coverage --watchAll=false", nil},
		// Redirections are no arguments.
		{"react-scripts test 2>&1", "pnpm exec react-scripts test --watchAll=false", nil},
		{"react-scripts test --coverage > test.log 2> err.log &> all.log", "pnpm exec react-scripts test --coverage --watchAll=false", nil},
		{"(CI=true react-scripts test --env=jsdom)", "pnpm exec react-scripts test --env=jsdom --watchAll=false", map[string]string{"CI": "true"}},
	}
	for _, c := range cases {
		got, env := craCommand(dir, ".", c.script)
		if got != c.want || !reflect.DeepEqual(env, c.env) {
			t.Errorf("craCommand(%q) = %q %v, want %q %v", c.script, got, env, c.want, c.env)
		}
		// The command is one runner can split.
		if _, err := runner.SplitWords(got); err != nil {
			t.Errorf("%q: %v", got, err)
		}
	}
	// A locally installed react-scripts is run directly.
	if got, _ := craCommand(dir, "web", "react-scripts test"); got != "./node_modules/.bin/react-scripts test --watchAll=false" {
		t.Errorf("got %q", got)
	}
}

func TestShellQuoteRoundTrip(t *testing.T) {
	for _, w := range []string{"plain", "a b", "it's", `"q"`, "x|y", "$HOME", ""} {
		got, err := runner.SplitWords("cmd " + shellQuote(w))
		if err != nil || len(got) != 2 || got[1] != w {
			t.Errorf("shellQuote(%q) = %q, split back to %q (%v)", w, shellQuote(w), got, err)
		}
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

// BenchmarkPythonProjects measures project detection in a large monorepo: 400 services
// (half with a project file, half with only a requirements file and a conftest.py) and
// 32k test files.
func BenchmarkPythonProjects(b *testing.B) {
	dirFiles := map[string][]string{".": {"requirements-dev.txt"}}
	for s := 0; s < 400; s++ {
		svc := fmt.Sprintf("services/s%d", s)
		if s%2 == 0 {
			dirFiles[svc] = []string{"pyproject.toml"}
		} else {
			dirFiles[svc] = []string{"requirements.txt", "conftest.py"}
		}
		for d := 0; d < 8; d++ {
			var names []string
			for f := 0; f < 10; f++ {
				names = append(names, fmt.Sprintf("test_%d.py", f))
			}
			dirFiles[fmt.Sprintf("%s/tests/d%d", svc, d)] = names
		}
	}
	var dirs []string
	for d := range dirFiles {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	repo := &gitx.Repo{Root: b.TempDir()}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// The services and the root (whose tests are not all in project-file services).
		if got := pythonProjects(repo, dirs, dirFiles); len(got) != 401 {
			b.Fatalf("%d projects", len(got))
		}
	}
}

// TestDetectStoriesAndNotes: a package whose only tests are Storybook stories gets a
// suite; a package that configures a runner but has no test smates can find gets a
// note instead of being dropped silently.
func TestDetectStoriesAndNotes(t *testing.T) {
	sbConfig := `import { storybookTest } from "@storybook/addon-vitest/vitest-plugin"
export default { plugins: [storybookTest()] }`
	cases := []struct {
		name  string
		files map[string]string
		want  []got
		note  string // substring of the only note ("" = none)
	}{
		{
			name: "stories-only package",
			files: map[string]string{
				"ui/package.json": vitestPkg, "ui/vitest.config.ts": sbConfig,
				"ui/.storybook/main.ts":     `export default { stories: ["../src/**/*.stories.tsx"] }`,
				"ui/src/button.stories.tsx": "", "ui/src/button.tsx": "",
			},
			want: []got{{"vitest", "ui", "vitest"}},
		},
		{
			name: "configured package with no test file",
			files: map[string]string{
				"ui/package.json": vitestPkg, "ui/vitest.config.ts": `export default { test: { include: ["src/**/*.spec.ts"] } }`,
				"ui/src/button.tsx": "",
			},
			note: "ui: no vitest suite: its configuration names no test file smates can find (test patterns: src/**/*.spec.ts)",
		},
		{
			name:  "package without a runner configuration needs no note",
			files: map[string]string{"ui/package.json": vitestPkg, "ui/src/button.tsx": ""},
		},
		// A plain Vite app does not configure a test runner.
		{
			name: "Vite app without vitest",
			files: map[string]string{
				"app/package.json":   `{"devDependencies":{"vite":"^6"}}`,
				"app/vite.config.ts": `import { defineConfig } from "vite"; export default defineConfig({})`,
				"app/src/main.ts":    "",
			},
		},
		{
			name: "vite.config.* importing vitest",
			files: map[string]string{
				"app/package.json":   `{"devDependencies":{"vite":"^6"}}`,
				"app/vite.config.ts": `/// <reference types="vitest" />` + "\n" + `import { defineConfig } from "vitest/config"; export default defineConfig({ test: { include: ["src/**/*.spec.ts"] } })`,
				"app/src/main.ts":    "",
			},
			note: "app: no vitest suite",
		},
		{
			name: "vite.config.* with a test key",
			files: map[string]string{
				"app/package.json":   `{"devDependencies":{"vite":"^6"}}`,
				"app/vite.config.ts": `export default { test: { include: ["src/**/*.spec.ts"] } }`,
				"app/src/main.ts":    "",
			},
			note: "app: no vitest suite",
		},
		// Configured only by a per-project file: reported, never dropped silently.
		{
			name: "only a per-project vitest configuration",
			files: map[string]string{
				"ui/package.json":               `{"devDependencies":{"vitest":"^4"},"scripts":{"test":"vitest --config vitest.storybook.config.ts"}}`,
				"ui/vitest.storybook.config.ts": sbConfig,
				"ui/.storybook/main.ts":         `export default { stories: ["../src/**/*.stories.tsx"] }`,
				"ui/src/button.stories.tsx":     "",
			},
			note: "vitest.storybook.config.ts configures vitest only when named",
		},
		{
			name: "only a per-project vitest configuration, no dependency",
			files: map[string]string{
				"ui/package.json":               `{}`,
				"ui/vitest.storybook.config.ts": sbConfig,
				"ui/src/button.stories.tsx":     "",
			},
			note: "ui: no vitest suite",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			var list []string
			for f, content := range c.files {
				testutil.Write(t, dir, f, content)
				list = append(list, f)
			}
			sort.Strings(list)
			suites, notes := Detect(&gitx.Repo{Root: dir}, list)
			var out []got
			for _, s := range suites {
				out = append(out, got{s.Name, s.Root, s.Runner})
			}
			if !reflect.DeepEqual(out, c.want) {
				t.Errorf("suites %v, want %v", out, c.want)
			}
			if c.note == "" && len(notes) > 0 || c.note != "" && (len(notes) != 1 || !strings.Contains(notes[0], c.note)) {
				t.Errorf("notes %q, want one containing %q", notes, c.note)
			}
		})
	}
}

// A per-project vitest file (vitest.storybook.config.ts) configures vitest only when
// named, so it does not decide the runner of a package whose tests something else runs.
func TestDetectPerProjectVitestConfigRunner(t *testing.T) {
	const sb = "export default { test: { include: [\"src/**/*.stories.tsx\"] } }\n"
	cases := []struct {
		name string
		pkg  string
		file string // an extra file of the package
		want string // the runner of the suite rooted at ui
	}{
		// Must keep the jest suite: vitest would run jest's tests.
		{"jest script", `{"scripts":{"test":"jest","test:stories":"vitest --config vitest.storybook.config.ts"},"devDependencies":{"jest":"^29"}}`, "jest.config.js", "jest"},
		{"jest configuration only", `{"scripts":{"test:stories":"vitest --config vitest.storybook.config.ts"}}`, "jest.config.js", "jest"},
		{"bun test", `{"scripts":{"test":"bun test"}}`, "", "bun"},
		{"node --test", `{"scripts":{"test":"node --test"}}`, "", "node"},
		{"react-scripts", `{"scripts":{"test":"react-scripts test"},"dependencies":{"react-scripts":"5"}}`, "", "jest"},
		{"vitest dependency", `{"devDependencies":{"vitest":"^4"}}`, "", "vitest"},
		{"nothing else names a runner", `{}`, "", "vitest"},
		{"nothing else but a bun lockfile", `{}`, "bun.lock", "vitest"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string]string{
				"ui/package.json": c.pkg, "ui/vitest.storybook.config.ts": sb,
				"ui/src/a.test.ts": "test('a', () => {})\n", "ui/src/b.stories.tsx": "",
			}
			if c.file != "" {
				files["ui/"+c.file] = "module.exports = {}\n"
			}
			var list []string
			for f, content := range files {
				testutil.Write(t, dir, f, content)
				list = append(list, f)
			}
			sort.Strings(list)
			suites, _ := Detect(&gitx.Repo{Root: dir}, list)
			var runners []string
			for _, s := range suites {
				if s.Root == "ui" {
					runners = append(runners, s.Runner)
				}
			}
			if len(runners) != 1 || runners[0] != c.want {
				t.Errorf("runners of ui %v, want [%s]", runners, c.want)
			}
		})
	}
}

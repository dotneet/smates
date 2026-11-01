package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
)

// discoverCase describes a repository, a suite in it, and the expected discovery:
// source ("runner defaults" when nothing is derived), whether a warning is shown, and
// the test files among the repository's files.
type discoverCase struct {
	name   string
	files  map[string]string
	suite  config.Suite
	source string
	warn   string // substring of the warning ("" = none)
	tests  []string
}

func runDiscoverCases(t *testing.T, cases []discoverCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			var files []string
			for f, content := range c.files {
				p := filepath.Join(dir, filepath.FromSlash(f))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				files = append(files, f)
			}
			sort.Strings(files)
			cs := c.suite
			if cs.Root == "" {
				cs.Root = "."
			}
			if cs.Command == "" {
				cs.Command = "run"
			}
			s, err := NewSuite(&gitx.Repo{Root: dir}, cs)
			if err != nil {
				t.Fatal(err)
			}
			if s.TestsSource != c.source {
				t.Errorf("source %q, want %q", s.TestsSource, c.source)
			}
			if c.warn == "" && s.DiscoveryWarning != "" || !strings.Contains(s.DiscoveryWarning, c.warn) {
				t.Errorf("warning %q, want one containing %q", s.DiscoveryWarning, c.warn)
			}
			if got := s.TestFiles(files); !reflect.DeepEqual(got, c.tests) {
				t.Errorf("tests %v\nwant %v\npatterns %v", got, c.tests, s.TestPatterns())
			}
			if s.Narrowed != (len(c.suite.Tests) > 0 || len(c.suite.Exclude) > 0) {
				t.Errorf("Narrowed = %v: derived patterns must not narrow the suite", s.Narrowed)
			}
		})
	}
}

// jsFiles are candidate test files of a JS project.
var jsFiles = map[string]string{
	"src/a.test.ts":                 "",
	"src/b.spec.js":                 "",
	"src/util.ts":                   "",
	"src/__tests__/c.ts":            "",
	"src/__tests__/fixtures/d.ts":   "",
	"e2e/login.e2e.ts":              "",
	"e2e/home.test.ts":              "",
	"lib/x.test.js":                 "",
	"test/unit/y_test.js":           "",
	"packages/p/src/p.test.ts":      "",
	"packages/q/test/q.spec.ts":     "",
	"packages/q/test/q.helper.ts":   "",
	"node_modules/dep/dep.test.js":  "",
	"dist/a.test.js":                "",
	"src/__fixtures__/data.test.ts": "",
}

func with(base map[string]string, extra ...string) map[string]string {
	m := map[string]string{}
	for k, v := range base {
		m[k] = v
	}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i]] = extra[i+1]
	}
	return m
}

func TestDiscoverJest(t *testing.T) {
	jest := config.Suite{Runner: "jest"}
	defaults := []string{"e2e/home.test.ts", "lib/x.test.js", "packages/p/src/p.test.ts", "packages/q/test/q.spec.ts",
		"src/__fixtures__/data.test.ts", "src/__tests__/c.ts", "src/a.test.ts", "src/b.spec.js"}
	runDiscoverCases(t, []discoverCase{
		{
			name: "no configuration", suite: jest, source: SourceDefaults, tests: defaults,
			files: with(jsFiles, "package.json", `{"devDependencies":{"jest":"^30"}}`),
		},
		{
			name: "testMatch in jest.config.js", suite: jest,
			files: with(jsFiles, "jest.config.js", `module.exports = {
				testMatch: ['<rootDir>/e2e/**/*.e2e.ts', '**/lib/**/?(*.)+(spec|test).[jt]s?(x)'],
			};`),
			source: "jest.config.js testMatch",
			tests:  []string{"e2e/login.e2e.ts", "lib/x.test.js"},
		},
		{
			name: "testMatch with negation and roots in jest.config.ts", suite: jest,
			files: with(jsFiles, "jest.config.ts", `import type { Config } from 'jest';
const config: Config = {
  roots: ['<rootDir>/src', '<rootDir>/packages'],
  testMatch: ['**/*.test.ts', '**/*.spec.ts', '!**/packages/p/**'],
};
export default config;`),
			source: "jest.config.ts roots, testMatch",
			tests:  []string{"packages/q/test/q.spec.ts", "src/__fixtures__/data.test.ts", "src/a.test.ts"},
		},
		{
			name: "testRegex and testPathIgnorePatterns in jest.config.json", suite: jest,
			files: with(jsFiles, "jest.config.json", `{
				"testRegex": "(/__tests__/.*|(\\.|/)(test|spec))\\.[jt]sx?$",
				"testPathIgnorePatterns": ["/node_modules/", "<rootDir>/packages/", "/__fixtures__/"]
			}`),
			source: "jest.config.json testRegex, testPathIgnorePatterns",
			tests:  []string{"e2e/home.test.ts", "lib/x.test.js", "src/__tests__/c.ts", "src/__tests__/fixtures/d.ts", "src/a.test.ts", "src/b.spec.js"},
		},
		{
			name: "package.json jest key", suite: jest,
			files:  with(jsFiles, "package.json", `{"jest": {"rootDir": "src", "testPathIgnorePatterns": ["/__fixtures__/"]}}`),
			source: `package.json "jest" rootDir, testPathIgnorePatterns`,
			tests:  []string{"src/__tests__/c.ts", "src/a.test.ts", "src/b.spec.js"},
		},
		{
			name: "config of a parent directory", suite: config.Suite{Runner: "jest", Root: "packages/q"},
			files:  with(jsFiles, "jest.config.js", `module.exports = { testMatch: ['**/test/**/*.ts'] }`),
			source: "jest.config.js testMatch",
			tests:  []string{"packages/q/test/q.helper.ts", "packages/q/test/q.spec.ts"},
		},
		{
			name: "package.json stops the search", suite: config.Suite{Runner: "jest", Root: "packages/q"},
			files: with(jsFiles, "jest.config.js", `module.exports = { testMatch: ['**/test/**/*.ts'] }`,
				"packages/q/package.json", `{"name":"q"}`),
			source: SourceDefaults,
			tests:  []string{"packages/q/test/q.spec.ts"},
		},
		{
			name: "--config on the command line", suite: config.Suite{Runner: "jest", Command: "npx jest --config jest.e2e.config.js"},
			files:  with(jsFiles, "jest.e2e.config.js", `module.exports = { testMatch: ['<rootDir>/e2e/**/*.ts'] }`),
			source: "jest.e2e.config.js testMatch",
			tests:  []string{"e2e/home.test.ts", "e2e/login.e2e.ts"},
		},
		{
			name: "non-literal testMatch falls back", suite: jest,
			files:  with(jsFiles, "jest.config.js", `const dirs = ['src']; module.exports = { testMatch: dirs.map((d) => d + '/**/*.test.ts') };`),
			source: SourceDefaults, warn: "jest.config.js: testMatch is not a literal", tests: defaults,
		},
		{
			name: "config built by a function falls back", suite: jest,
			files:  with(jsFiles, "jest.config.js", `module.exports = async () => ({ testMatch: ['x'] });`),
			source: SourceDefaults, warn: "does not export an object literal", tests: defaults,
		},
		{
			name: "config changed after its declaration falls back", suite: jest,
			files: with(jsFiles, "jest.config.js", `const config = { testMatch: ['<rootDir>/e2e/**/*.ts'] };
if (process.env.CI) config.testMatch.push('<rootDir>/lib/**/*.js');
module.exports = config;`),
			source: SourceDefaults, warn: "jest.config.js does not export an object literal (config, changed after its declaration)", tests: defaults,
		},
		{
			name: "module.exports changed later falls back", suite: jest,
			files: with(jsFiles, "jest.config.js", `module.exports = { testMatch: ['<rootDir>/e2e/**/*.ts'] };
module.exports.testMatch = ['**/*.test.js'];`),
			source: SourceDefaults, warn: "(module.exports, changed later)", tests: defaults,
		},
		{
			name: "spread may set testMatch", suite: jest,
			files:  with(jsFiles, "jest.config.js", `const base = require('../jest.base'); module.exports = { ...base, verbose: true };`),
			source: SourceDefaults, warn: "may come from a ...spread", tests: defaults,
		},
		{
			name: "regex Go cannot compile falls back", suite: jest,
			files:  with(jsFiles, "jest.config.js", `module.exports = { testRegex: '^(?!.*e2e).*\\.test\\.ts$' };`),
			source: SourceDefaults, warn: "Go's regexp lacks", tests: defaults,
		},
		{
			name: "coverage options are not test patterns", suite: jest,
			files: with(jsFiles, "jest.config.js", `module.exports = { collectCoverage: true, coverageProvider: 'v8',
  collectCoverageFrom: ['src/**/*.ts'], coveragePathIgnorePatterns: ['/e2e/'], coverageDirectory: 'coverage/jest' }`),
			source: SourceDefaults, tests: defaults,
		},
		{
			name: "tests in smates.yml win", suite: config.Suite{Runner: "jest", Tests: []string{"lib/**"}},
			files:  with(jsFiles, "jest.config.js", `module.exports = { testMatch: ['**/e2e/**'] }`),
			source: SourceConfig, tests: []string{"lib/x.test.js"},
		},
	})
}

func TestDiscoverVitest(t *testing.T) {
	vitest := config.Suite{Runner: "vitest"}
	runDiscoverCases(t, []discoverCase{
		{
			name: "no test options", suite: vitest,
			files:  with(jsFiles, "vitest.config.ts", `export default defineConfig({ test: { globals: true } })`),
			source: SourceDefaults,
			tests: []string{"e2e/home.test.ts", "lib/x.test.js", "packages/p/src/p.test.ts", "packages/q/test/q.spec.ts",
				"src/__fixtures__/data.test.ts", "src/a.test.ts", "src/b.spec.js"},
		},
		{
			// coverage.include/exclude select the files a coverage report covers, not tests.
			name: "coverage options are not test patterns", suite: vitest,
			files: with(jsFiles, "vitest.config.ts", `export default defineConfig({ test: {
  coverage: { provider: 'v8', include: ['src/**'], exclude: ['src/a.test.ts'], reportsDirectory: 'coverage/vitest' },
} })`),
			source: SourceDefaults,
			tests: []string{"e2e/home.test.ts", "lib/x.test.js", "packages/p/src/p.test.ts", "packages/q/test/q.spec.ts",
				"src/__fixtures__/data.test.ts", "src/a.test.ts", "src/b.spec.js"},
		},
		{
			name: "include and exclude with configDefaults", suite: vitest,
			files: with(jsFiles, "vite.config.mts", `import { configDefaults, defineConfig } from 'vitest/config';
export default defineConfig({
  plugins: [vue()],
  test: {
    include: ['{src,lib}/**/*.{test,spec}.?(c|m)[jt]s?(x)'],
    exclude: [...configDefaults.exclude, '**/__fixtures__/**'],
  },
});`),
			source: "vite.config.mts test.include, test.exclude",
			tests:  []string{"lib/x.test.js", "src/a.test.ts", "src/b.spec.js"},
		},
		{
			name: "vitest.config wins over vite.config; test.dir", suite: vitest,
			files: with(jsFiles, "vite.config.ts", `export default { test: { include: ['nothing/**'] } }`,
				"vitest.config.ts", `export default defineConfig({ test: { dir: 'e2e' } })`),
			source: "vitest.config.ts test.dir",
			tests:  []string{"e2e/home.test.ts"},
		},
		{
			name: "inline projects", suite: vitest,
			files: with(jsFiles, "vitest.config.ts", `export default defineConfig({
  test: {
    exclude: ['**/__fixtures__/**'],
    projects: [
      { test: { name: 'unit', include: ['src/**/*.test.ts'] } },
      { extends: true, test: { name: 'e2e', include: ['e2e/**/*.e2e.ts'] } },
      { root: 'lib' },
    ],
  },
})`),
			source: "vitest.config.ts test.projects (3 projects)",
			tests:  []string{"e2e/login.e2e.ts", "lib/x.test.js", "src/__fixtures__/data.test.ts", "src/a.test.ts"},
		},
		{
			name: "workspace file with globs of projects", suite: vitest,
			files: with(jsFiles, "vitest.workspace.ts", `export default ['packages/*']`,
				"packages/q/vitest.config.ts", `export default defineProject({ test: { include: ['test/**/*.ts'], exclude: ['**/*.helper.ts'] } })`),
			source: "vitest.workspace.ts (2 projects)",
			tests:  []string{"packages/p/src/p.test.ts", "packages/q/test/q.spec.ts"},
		},
		{
			name: "projects naming config files", suite: vitest,
			files: with(jsFiles, "vitest.config.js", `module.exports = { test: { projects: ['packages/*/vitest.config.ts'] } }`,
				"packages/q/vitest.config.ts", `export default { test: { include: ['test/*.helper.ts'] } }`,
				"packages/p/vitest.config.ts", `export default { test: { include: ['src/**/*.ts'] } }`),
			source: "vitest.config.js test.projects (2 projects)",
			tests:  []string{"packages/p/src/p.test.ts", "packages/q/test/q.helper.ts"},
		},
		{
			name: "unreadable project config keeps the defaults for it", suite: vitest,
			files: with(jsFiles, "vitest.workspace.json", `["packages/*"]`,
				"packages/q/vitest.config.ts", `export default mergeConfig(base, { test: { include: ['x'] } })`),
			source: "vitest.workspace.json (2 projects)", warn: "packages/q/vitest.config.ts does not export an object literal",
			tests: []string{"packages/p/src/p.test.ts", "packages/q/test/q.spec.ts"},
		},
		{
			name: "mergeConfig falls back", suite: vitest,
			files: with(jsFiles, "vitest.config.ts", `import base from './vite.config';
export default mergeConfig(base, defineConfig({ test: { include: ['e2e/**/*.ts'] } }))`),
			source: SourceDefaults, warn: "vitest.config.ts does not export an object literal (mergeConfig(",
			tests: []string{"e2e/home.test.ts", "lib/x.test.js", "packages/p/src/p.test.ts", "packages/q/test/q.spec.ts",
				"src/__fixtures__/data.test.ts", "src/a.test.ts", "src/b.spec.js"},
		},
		{
			name: "non-literal include falls back", suite: vitest,
			files:  with(jsFiles, "vitest.config.ts", "export default defineConfig({ test: { include: [`${dir}/**/*.test.ts`] } })"),
			source: SourceDefaults, warn: "test.include is not a literal",
			tests: []string{"e2e/home.test.ts", "lib/x.test.js", "packages/p/src/p.test.ts", "packages/q/test/q.spec.ts",
				"src/__fixtures__/data.test.ts", "src/a.test.ts", "src/b.spec.js"},
		},
		{
			name: "root of a nested suite", suite: config.Suite{Runner: "vitest", Root: "packages/q"},
			files:  with(jsFiles, "packages/q/vitest.config.ts", `export default defineConfig({ root: 'test', test: { include: ['*.ts'] } })`),
			source: "packages/q/vitest.config.ts root, test.include",
			tests:  []string{"packages/q/test/q.helper.ts", "packages/q/test/q.spec.ts"},
		},
	})
}

var pyFiles = map[string]string{
	"tests/test_a.py":       "",
	"tests/unit/b_test.py":  "",
	"tests/check_c.py":      "",
	"tests/helpers.py":      "",
	"src/pkg/test_inner.py": "",
	"src/pkg/mod.py":        "",
	"integration/test_i.py": "",
}

func TestDiscoverPytest(t *testing.T) {
	pytest := config.Suite{Runner: "pytest"}
	all := []string{"integration/test_i.py", "src/pkg/test_inner.py", "tests/test_a.py", "tests/unit/b_test.py"}
	runDiscoverCases(t, []discoverCase{
		{
			name: "no configuration", suite: pytest, source: SourceDefaults, tests: all,
			files: with(pyFiles, "pyproject.toml", "[project]\nname = 'x'\n"),
		},
		{
			// pytest-cov options and coverage.py sections name source paths, not tests.
			name: "pytest-cov and coverage options are not test patterns", suite: pytest, source: SourceDefaults, tests: all,
			files: with(pyFiles, "pyproject.toml", `[project]
name = "x"

[tool.pytest.ini_options]
addopts = "--cov=src --cov-branch --cov-report=json:cov.json -n auto"

[tool.coverage.run]
source = ["src"]
omit = ["tests/*"]

[tool.coverage.report]
include = ["src/*"]
`, "setup.cfg", "[coverage:run]\nsource = src\n"),
		},
		{
			name: "pyproject testpaths and python_files", suite: pytest,
			files: with(pyFiles, "pyproject.toml", `[project]
name = "x"

[tool.pytest.ini_options]
addopts = "-ra"
testpaths = [
    "tests",  # unit
    "integration",
]
python_files = ["test_*.py", "check_*.py"]

[tool.coverage.run]
source = ["src"]
`),
			source: "pyproject.toml [tool.pytest.ini_options] testpaths, python_files",
			tests:  []string{"integration/test_i.py", "tests/check_c.py", "tests/test_a.py"},
		},
		{
			name: "pytest.ini wins over pyproject", suite: pytest,
			files: with(pyFiles, "pytest.ini", "[pytest]\ntestpaths =\n    tests\npython_files = test_*.py *_test.py check_*.py ; comment\n",
				"pyproject.toml", "[tool.pytest.ini_options]\ntestpaths = [\"integration\"]\n"),
			source: "pytest.ini [pytest] testpaths, python_files",
			tests:  []string{"tests/check_c.py", "tests/test_a.py", "tests/unit/b_test.py"},
		},
		{
			name: "setup.cfg", suite: pytest,
			files:  with(pyFiles, "setup.cfg", "[metadata]\nname = x\n\n[tool:pytest]\ntestpaths = src\n"),
			source: "setup.cfg [tool:pytest] testpaths",
			tests:  []string{"src/pkg/test_inner.py"},
		},
		{
			name: "tox.ini; a file in testpaths", suite: pytest,
			files:  with(pyFiles, "tox.ini", "[tox]\nenvlist = py312\n\n[pytest]\ntestpaths = integration tests/helpers.py\n"),
			source: "tox.ini [pytest] testpaths",
			tests:  []string{"integration/test_i.py", "tests/helpers.py"},
		},
		{
			name: "testpaths of a parent directory do not apply", suite: config.Suite{Runner: "pytest", Root: "tests"},
			files:  with(pyFiles, "pyproject.toml", "[tool.pytest.ini_options]\ntestpaths = [\"integration\"]\npython_files = \"check_*.py test_*.py\"\n"),
			source: "pyproject.toml [tool.pytest.ini_options] python_files",
			tests:  []string{"tests/check_c.py", "tests/test_a.py"},
		},
		{
			name: "empty pytest.ini", suite: pytest, source: SourceDefaults, tests: all,
			files: with(pyFiles, "pytest.ini", "", "pyproject.toml", "[tool.pytest.ini_options]\ntestpaths = [\"integration\"]\n"),
		},
	})
}

func TestMicromatchToGlob(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
		match    []string
		noMatch  []string
	}{
		{in: "**/?(*.)+(spec|test).[jt]s?(x)", want: "**/{,*.}{spec,test}.[jt]s{,x}", ok: true,
			match: []string{"a/b.test.ts", "spec.js", "x.spec.tsx"}, noMatch: []string{"a/b.test.mjs", "a/btest.ts"}},
		{in: "**/__tests__/**/*.?([mc])[jt]s?(x)", want: "**/__tests__/**/*.{,[mc]}[jt]s{,x}", ok: true,
			match: []string{"__tests__/a.js", "x/__tests__/y/z.mts"}, noMatch: []string{"__tests__/a.json"}},
		{in: "src/**/*.@(test|spec).ts", want: "src/**/*.{test,spec}.ts", ok: true},
		{in: "src/**/*.(test|spec).ts", want: "src/**/*.{test,spec}.ts", ok: true},
		{in: "src/**/!(*.e2e).ts", ok: false},
		{in: "src/**/*.{test,spec}.ts", want: "src/**/*.{test,spec}.ts", ok: true},
	}
	for _, c := range cases {
		got, ok := micromatchToGlob(c.in)
		if ok != c.ok || ok && got != c.want {
			t.Errorf("%s: got %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
		m := Matcher{Base: ".", Tests: []string{got}}
		for _, f := range c.match {
			if !m.Match(f) {
				t.Errorf("%s does not match %s", got, f)
			}
		}
		for _, f := range c.noMatch {
			if m.Match(f) {
				t.Errorf("%s matches %s", got, f)
			}
		}
	}
}

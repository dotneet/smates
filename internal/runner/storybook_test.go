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
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// TestObserveBrowserResults: a test that runs only in a browser leaves no trace. Its
// pass stays unknown (it runs again) unless trust_browser_passes is set; a failure
// stays a failure, and a test a node project also runs keeps the usual handling.
func TestObserveBrowserResults(t *testing.T) {
	yes := true
	cases := []struct {
		name   string
		trust  *bool
		file   string
		status string
		want   string
		static bool
		warn   string
	}{
		{name: "story pass, not trusted", file: "src/a.stories.tsx", status: "pass", want: state.Unknown, warn: "ran in a browser"},
		{name: "story pass, trusted", trust: &yes, file: "src/a.stories.tsx", status: "pass", want: state.Pass, static: true},
		{name: "story failure, trusted", trust: &yes, file: "src/a.stories.tsx", status: "fail", want: state.Fail},
		{name: "story stopped short, trusted", trust: &yes, file: "src/a.stories.tsx", status: "unknown", want: state.Unknown, warn: "ran in a browser"},
		{name: "node test pass with no trace, trusted suite", trust: &yes, file: "src/a.test.ts", status: "pass", want: state.Unknown, warn: "none left a trace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := tempRepo(t, c.file)
			td := t.TempDir()
			os.MkdirAll(trace.V8Dir(td), 0o755)
			line := mustJSON(map[string]any{"k": "result", "id": filepath.Join(repo.Root, filepath.FromSlash(c.file)), "status": c.status, "ms": 3})
			os.WriteFile(filepath.Join(td, "results-vitest-1.jsonl"), []byte(line+"\n"), 0o644)
			s := &Suite{Suite: config.Suite{Root: ".", TrustBrowserPasses: c.trust}, Discovered: []Matcher{
				{Base: ".", Tests: []string{"**/*.stories.tsx"}, Browser: true},
				{Base: ".", Tests: []string{"**/*.test.ts"}},
			}}
			rc := &RunContext{Repo: repo, Suite: s, TraceDir: td, UnitOf: func(f string) string {
				if s.IsTestFile(f) {
					return f
				}
				return ""
			}}
			obs, warns, err := observe(rc, 0, traceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			o := obs[c.file]
			if o == nil || o.Status != c.want || o.StaticOnly != c.static {
				t.Fatalf("observation %+v, want status %s, StaticOnly %v", o, c.want, c.static)
			}
			if c.warn == "" && len(warns) > 0 || c.warn != "" && (len(warns) != 1 || !strings.Contains(warns[0], c.warn)) {
				t.Errorf("warnings %q, want one containing %q", warns, c.warn)
			}
		})
	}
}

// sbFiles is a component package with stories, a plain test and a Storybook
// configuration directory.
var sbFiles = map[string]string{
	"src/button.tsx":            "",
	"src/button.stories.tsx":    "",
	"src/card.stories.ts":       "",
	"src/card.stories.mdx":      "",
	"src/intro.mdx":             "",
	"src/util.test.ts":          "",
	"docs/guide.stories.tsx":    "",
	"other/page.stories.jsx":    "",
	".storybook/preview.tsx":    `import "../src/theme"`,
	"sb/preview.ts":             ``,
	"node_modules/x.stories.ts": "",
}

const sbMain = `import type { StorybookConfig } from '@storybook/react-vite'
const config: StorybookConfig = {
  stories: ['../src/**/*.stories.@(ts|tsx)'],
  addons: ['@storybook/addon-vitest'],
  framework: { name: '@storybook/react-vite', options: {} },
}
export default config`

const sbImport = `import path from 'node:path'
import { defineConfig } from 'vitest/config'
import { storybookTest } from '@storybook/addon-vitest/vitest-plugin'
const dirname = path.dirname(new URL(import.meta.url).pathname)
`

func TestDiscoverVitestStorybook(t *testing.T) {
	vitest := config.Suite{Runner: "vitest"}
	stories := []string{"src/button.stories.tsx", "src/card.stories.ts"}
	runDiscoverCases(t, []discoverCase{
		{
			name: "configDir by path.join of dirname; stories-only project", suite: vitest,
			files: with(sbFiles, ".storybook/main.ts", sbMain, "vitest.config.ts", sbImport+`export default defineConfig({
  plugins: [storybookTest({ configDir: path.join(dirname, '.storybook') })],
  test: { browser: { enabled: true, instances: [{ browser: 'chromium' }] } },
})`),
			source: "vitest.config.ts Storybook stories", tests: stories,
		},
		{
			name: "default configDir", suite: vitest,
			files:  with(sbFiles, ".storybook/main.ts", sbMain, "vitest.config.ts", sbImport+`export default defineConfig({ plugins: [storybookTest()] })`),
			source: "vitest.config.ts Storybook stories", tests: stories,
		},
		{
			name: "configDir literal, aliased plugin, main.js", suite: vitest,
			files: with(sbFiles, "sb/main.js", `module.exports = { stories: ['../src/**/*.stories.@(ts|tsx)'] }`,
				"vitest.config.ts", `import { storybookTest as sbTest } from '@storybook/addon-vitest/vitest-plugin'
export default { plugins: [sbTest({ configDir: 'sb' })] }`),
			source: "vitest.config.ts Storybook stories", tests: stories,
		},
		{
			name: "configDir by path.resolve of __dirname and import.meta.dirname", suite: vitest,
			files: with(sbFiles, "sb/main.ts", sbMain, "vitest.config.ts", `import { storybookTest } from '@storybook/addon-vitest/vitest-plugin'
export default { test: { projects: [
  { plugins: [storybookTest({ configDir: path.resolve(__dirname, 'sb') })] },
  { plugins: [storybookTest({ configDir: join(import.meta.dirname, 'sb') })] },
] } }`),
			source: "vitest.config.ts test.projects (2 projects)", tests: stories,
		},
		{
			name: "plain tests and stories in one project", suite: vitest,
			files: with(sbFiles, ".storybook/main.ts", sbMain, "vitest.config.ts", sbImport+`export default defineConfig({
  plugins: [storybookTest()], test: { include: ['src/**/*.test.ts'] } })`),
			source: "vitest.config.ts test.include, Storybook stories",
			tests:  []string{"src/button.stories.tsx", "src/card.stories.ts", "src/util.test.ts"},
		},
		{
			name: "stories as a specifier and a directory; MDX is not a test", suite: vitest,
			files: with(sbFiles, ".storybook/main.ts", `export default { stories: [
  { directory: '../src', files: '**/*.stories.*', titlePrefix: 'ui' },
  '../docs',
] }`, "vitest.config.ts", sbImport+`export default defineConfig({ plugins: [storybookTest()] })`),
			source: "vitest.config.ts Storybook stories",
			tests:  []string{"docs/guide.stories.tsx", "src/button.stories.tsx", "src/card.stories.ts"},
		},
		{
			name: "single stories string", suite: vitest,
			files: with(sbFiles, ".storybook/main.ts", `export default { stories: '../src/*.stories.tsx' }`,
				"vitest.config.ts", sbImport+`export default defineConfig({ plugins: [storybookTest()] })`),
			source: "vitest.config.ts Storybook stories", tests: []string{"src/button.stories.tsx"},
		},
		{
			name: "computed stories fall back to every story file", suite: vitest,
			files: with(sbFiles, ".storybook/main.ts", `export default { stories: findStories() }`,
				"vitest.config.ts", sbImport+`export default defineConfig({ plugins: [storybookTest()] })`),
			source: "vitest.config.ts Storybook stories", warn: "running the whole suite whenever one of them is selected",
			tests: []string{"docs/guide.stories.tsx", "other/page.stories.jsx", "src/button.stories.tsx", "src/card.stories.ts"},
		},
		{
			name: "a spread stories entry falls back, the others are read", suite: vitest,
			files: with(sbFiles, ".storybook/main.ts", `export default { stories: ['../src/*.stories.tsx', ...more] }`,
				"vitest.config.ts", sbImport+`export default defineConfig({ plugins: [storybookTest()] })`),
			source: "vitest.config.ts Storybook stories", warn: "a stories entry is not a literal",
			tests: []string{"docs/guide.stories.tsx", "other/page.stories.jsx", "src/button.stories.tsx", "src/card.stories.ts"},
		},
		{
			name: "no main file falls back", suite: vitest,
			files:  with(sbFiles, "vitest.config.ts", sbImport+`export default defineConfig({ plugins: [storybookTest()] })`),
			source: "vitest.config.ts Storybook stories", warn: "no main.* in Storybook's configuration directory .storybook",
			tests: []string{"docs/guide.stories.tsx", "other/page.stories.jsx", "src/button.stories.tsx", "src/card.stories.ts"},
		},
		{
			name: "computed configDir falls back", suite: vitest,
			files: with(sbFiles, ".storybook/main.ts", sbMain,
				"vitest.config.ts", sbImport+`export default defineConfig({ plugins: [storybookTest({ configDir: process.env.SB_DIR })] })`),
			source: "vitest.config.ts Storybook stories", warn: "configDir is not a path smates can read",
			tests: []string{"docs/guide.stories.tsx", "other/page.stories.jsx", "src/button.stories.tsx", "src/card.stories.ts"},
		},
		{
			name: "plugin imported but called indirectly falls back", suite: vitest,
			files: with(sbFiles, ".storybook/main.ts", sbMain, "vitest.config.ts", `import * as sb from '@storybook/addon-vitest/vitest-plugin'
export default { plugins: [sb.storybookTest()] }`),
			source: "vitest.config.ts Storybook stories", warn: "calls it in a way smates cannot read",
			tests: []string{"docs/guide.stories.tsx", "other/page.stories.jsx", "src/button.stories.tsx", "src/card.stories.ts"},
		},
		{
			// mergeConfig cannot be read: the other tests keep the default include, the
			// stories still come from main.*.
			name: "unreadable per-project config keeps the defaults and adds the stories", suite: vitest,
			files: with(sbFiles, ".storybook/main.ts", sbMain,
				"vitest.config.ts", `export default { test: { projects: ['./vitest.storybook.config.ts'] } }`,
				"vitest.storybook.config.ts", sbImport+`import viteConfig from './vite.config'
export default mergeConfig(viteConfig, defineConfig({ plugins: [storybookTest({ configDir: path.join(dirname, '.storybook') })] }))`),
			source: "vitest.config.ts test.projects (1 project)", warn: "does not export an object literal",
			tests: []string{"src/button.stories.tsx", "src/card.stories.ts", "src/util.test.ts"},
		},
		// The stories being a guess (the suite then runs whole) must reach the warning
		// even when the project file cannot be read either.
		{
			name: "unreadable per-project config with a computed configDir: guessed stories reported", suite: vitest,
			files: with(sbFiles, ".storybook/main.ts", sbMain,
				"vitest.config.ts", `export default { test: { projects: ['./vitest.node.config.ts', './vitest.storybook.config.ts'] } }`,
				"vitest.node.config.ts", `export default mergeConfig(base, { test: { include: ['src/**/*.test.ts'] } })`,
				"vitest.storybook.config.ts", sbImport+`export default mergeConfig(base, defineConfig({ plugins: [storybookTest({ configDir: getDir() })] }))`),
			source: "vitest.config.ts test.projects (2 projects)", warn: "running the whole suite whenever one of them is selected",
			tests: []string{"docs/guide.stories.tsx", "other/page.stories.jsx", "src/button.stories.tsx", "src/card.stories.ts", "src/util.test.ts"},
		},
		{
			name: "unreadable per-project config without main.*: guessed stories reported", suite: vitest,
			files: with(sbFiles,
				"vitest.config.ts", `export default { test: { projects: ['./vitest.storybook.config.ts'] } }`,
				"vitest.storybook.config.ts", sbImport+`export default mergeConfig(base, defineConfig({ plugins: [storybookTest()] }))`),
			source: "vitest.config.ts test.projects (1 project)", warn: "running the whole suite whenever one of them is selected",
			tests: []string{"docs/guide.stories.tsx", "other/page.stories.jsx", "src/button.stories.tsx", "src/card.stories.ts", "src/util.test.ts"},
		},
		{
			name: "unreadable top-level config keeps the defaults and adds the stories", suite: vitest,
			files: with(sbFiles, ".storybook/main.ts", sbMain,
				"vitest.config.ts", sbImport+`export default mergeConfig(viteConfig, defineConfig({ plugins: [storybookTest()] }))`),
			source: "vitest.config.ts Storybook stories", warn: "does not export an object literal",
			tests: []string{"src/button.stories.tsx", "src/card.stories.ts", "src/util.test.ts"},
		},
	})
}

// TestStorybookMatchers checks what the projects of a suite say about its units:
// which run only in a browser, which run whole, and what they depend on besides
// their imports.
func TestStorybookMatchers(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		unit    string
		browser bool
		whole   bool
		globs   []string
		starts  []string
		unsure  string // substring; "" = none
	}{
		{
			name: "story of a readable configuration",
			files: map[string]string{
				".storybook/main.ts":     `export default { stories: ['../src/**/*.stories.tsx'], staticDirs: ['../public', { from: '../assets', to: '/a' }] }`,
				".storybook/preview.tsx": ``,
				"vitest.config.ts":       sbImport + `export default defineConfig({ plugins: [storybookTest()] })`,
				"src/b.stories.tsx":      ``,
			},
			unit: "src/b.stories.tsx", browser: true,
			globs: []string{".storybook/**", "public/**", "assets/**"}, starts: []string{".storybook/preview.tsx", ".storybook/main.ts"},
		},
		// Must keep: what main.* loads for the stories outside the configuration
		// directory (a Vite plugin or alias in viteFinal, preview annotations, a local
		// addon) is not known, so their passes are never trusted.
		{
			name: "viteFinal makes passes untrusted",
			files: map[string]string{
				".storybook/main.ts": `import { mockApi } from '../tools/mock-api'
export default { stories: ['../src/**/*.stories.tsx'], staticDirs: [], viteFinal: (c) => ({ ...c, plugins: [mockApi()] }) }`,
				"vitest.config.ts":  sbImport + `export default defineConfig({ plugins: [storybookTest()] })`,
				"src/b.stories.tsx": ``,
			},
			unit: "src/b.stories.tsx", browser: true, globs: []string{".storybook/**"}, starts: []string{".storybook/main.ts"}, unsure: "sets viteFinal",
		},
		{
			name: "an async viteFinal method makes passes untrusted",
			files: map[string]string{
				".storybook/main.ts": `export default { stories: ['../src/**/*.stories.tsx'], staticDirs: [], async viteFinal(c) { return c } }`,
				"vitest.config.ts":   sbImport + `export default defineConfig({ plugins: [storybookTest()] })`,
				"src/b.stories.tsx":  ``,
			},
			unit: "src/b.stories.tsx", browser: true, globs: []string{".storybook/**"}, starts: []string{".storybook/main.ts"}, unsure: "sets viteFinal",
		},
		{
			name: "previewAnnotations make passes untrusted",
			files: map[string]string{
				".storybook/main.ts": `export default { stories: ['../src/**/*.stories.tsx'], staticDirs: [], previewAnnotations: ['../tools/annotations.ts'] }`,
				"vitest.config.ts":   sbImport + `export default defineConfig({ plugins: [storybookTest()] })`,
				"src/b.stories.tsx":  ``,
			},
			unit: "src/b.stories.tsx", browser: true, globs: []string{".storybook/**"}, starts: []string{".storybook/main.ts"}, unsure: "sets previewAnnotations",
		},
		{
			name: "a local addon makes passes untrusted",
			files: map[string]string{
				".storybook/main.ts": `export default { stories: ['../src/**/*.stories.tsx'], staticDirs: [], addons: ['@storybook/addon-a11y', './local-preset.ts'] }`,
				"vitest.config.ts":   sbImport + `export default defineConfig({ plugins: [storybookTest()] })`,
				"src/b.stories.tsx":  ``,
			},
			unit: "src/b.stories.tsx", browser: true, globs: []string{".storybook/**"}, starts: []string{".storybook/main.ts"}, unsure: "local addon or preset in addons",
		},
		{
			name: "a local addon through a helper makes passes untrusted",
			files: map[string]string{
				".storybook/main.ts": `export default { stories: ['../src/**/*.stories.tsx'], staticDirs: [], addons: [getAbsolutePath('../addons/x')] }`,
				"vitest.config.ts":   sbImport + `export default defineConfig({ plugins: [storybookTest()] })`,
				"src/b.stories.tsx":  ``,
			},
			unit: "src/b.stories.tsx", browser: true, globs: []string{".storybook/**"}, starts: []string{".storybook/main.ts"}, unsure: "local addon or preset in addons",
		},
		{
			name: "package addons and framework are known",
			files: map[string]string{
				".storybook/main.ts": `export default { stories: ['../src/**/*.stories.tsx'], staticDirs: [], addons: ['@storybook/addon-a11y', getAbsolutePath('@storybook/addon-docs')], framework: { name: '@storybook/react-vite' } }`,
				"vitest.config.ts":   sbImport + `export default defineConfig({ plugins: [storybookTest()] })`,
				"src/b.stories.tsx":  ``,
			},
			unit: "src/b.stories.tsx", browser: true, globs: []string{".storybook/**"}, starts: []string{".storybook/main.ts"},
		},
		// Storybook resolves configDir against the directory vitest runs in (the suite
		// root), not the project file's: the root's .storybook is the one read, and as
		// the two differ the stories are not trusted.
		{
			name: "default configDir of a project file in a subdirectory",
			files: map[string]string{
				"vitest.config.ts":         `export default { test: { projects: ['web/vitest.config.ts'] } }`,
				"web/vitest.config.ts":     sbImport + `export default defineConfig({ plugins: [storybookTest()] })`,
				".storybook/main.ts":       `export default { stories: ['../web/src/**/*.stories.tsx'], staticDirs: [] }`,
				".storybook/preview.tsx":   ``,
				"web/.storybook/main.ts":   `export default { stories: ['../lib/**/*.stories.tsx'], staticDirs: [] }`,
				"web/src/b.stories.tsx":    ``,
				"web/lib/other.stories.ts": ``,
			},
			unit: "web/src/b.stories.tsx", browser: true, globs: []string{".storybook/**"}, starts: []string{".storybook/preview.tsx", ".storybook/main.ts"},
			unsure: "against the directory vitest runs in",
		},
		{
			name: "configDir joined to the file's directory stays the file's",
			files: map[string]string{
				"vitest.config.ts":       `export default { test: { projects: ['web/vitest.config.ts'] } }`,
				"web/vitest.config.ts":   sbImport + `export default defineConfig({ plugins: [storybookTest({ configDir: path.join(__dirname, '.storybook') })] })`,
				"web/.storybook/main.ts": `export default { stories: ['../src/**/*.stories.tsx'], staticDirs: [] }`,
				"web/src/b.stories.tsx":  ``,
			},
			unit: "web/src/b.stories.tsx", browser: true, globs: []string{"web/.storybook/**"}, starts: []string{"web/.storybook/main.ts"},
		},
		{
			name: "unreadable static directories make passes untrusted",
			files: map[string]string{
				".storybook/main.ts": `export default { stories: ['../src/**/*.stories.tsx'], staticDirs: dirs }`,
				"vitest.config.ts":   sbImport + `export default defineConfig({ plugins: [storybookTest()] })`,
				"src/b.stories.tsx":  ``,
			},
			unit: "src/b.stories.tsx", browser: true, globs: []string{".storybook/**"}, starts: []string{".storybook/main.ts"}, unsure: "staticDirs is not a literal",
		},
		{
			name: "fallback stories run whole and are untrusted",
			files: map[string]string{
				".storybook/main.ts": `export default { stories: list }`,
				"vitest.config.ts":   sbImport + `export default defineConfig({ plugins: [storybookTest()] })`,
				"src/b.stories.tsx":  ``,
			},
			unit: "src/b.stories.tsx", browser: true, whole: true, globs: []string{".storybook/**"}, starts: []string{".storybook/main.ts"}, unsure: "stories is not a literal",
		},
		{
			name: "browser-mode test",
			files: map[string]string{
				"vitest.config.ts":      `export default { test: { projects: [{ test: { include: ['**/*.browser.test.ts'], browser: { enabled: true } } }, { test: { include: ['**/*.unit.test.ts'] } }] } }`,
				"src/a.browser.test.ts": ``,
				"src/a.unit.test.ts":    ``,
			},
			unit: "src/a.browser.test.ts", browser: true,
		},
		{
			name: "node test next to a browser project",
			files: map[string]string{
				"vitest.config.ts":      `export default { test: { projects: [{ test: { include: ['**/*.browser.test.ts'], browser: { enabled: true } } }, { test: { include: ['**/*.unit.test.ts'] } }] } }`,
				"src/a.browser.test.ts": ``,
				"src/a.unit.test.ts":    ``,
			},
			unit: "src/a.unit.test.ts",
		},
		{
			name: "a file both a node and a browser project run is not browser-only",
			files: map[string]string{
				"vitest.config.ts": `export default { test: { projects: [{ test: { browser: { enabled: true } } }, { test: {} }] } }`,
				"src/a.test.ts":    ``,
			},
			unit: "src/a.test.ts",
		},
		{
			name: "browser turned off",
			files: map[string]string{
				"vitest.config.ts": `export default { test: { include: ['src/**/*.test.ts'], browser: { enabled: false } } }`,
				"src/a.test.ts":    ``,
			},
			unit: "src/a.test.ts",
		},
		// Must keep the node tests' traces required: an unreadable configuration with
		// projects may run the default include in node, next to the Storybook project.
		{
			name: "unreadable projects configuration with Storybook: node tests are not browser-only",
			files: map[string]string{
				".storybook/main.ts": `export default { stories: ['../src/**/*.stories.tsx'], staticDirs: [] }`,
				"vitest.config.ts": sbImport + `export default mergeConfig(viteConfig, defineConfig({ test: { projects: [
  { extends: true, test: { name: 'unit', environment: 'jsdom', include: ['src/**/*.test.ts'] } },
  { extends: true, plugins: [storybookTest({ configDir: '.storybook' })], test: { name: 'storybook', browser: { enabled: true } } },
] } }))`,
				"src/util.test.ts":  ``,
				"src/b.stories.tsx": ``,
			},
			unit: "src/util.test.ts",
		},
		{
			name: "unreadable single-project configuration with Storybook: browser, never trusted",
			files: map[string]string{
				".storybook/main.ts": `export default { stories: ['../src/**/*.stories.tsx'], staticDirs: [] }`,
				"vitest.config.ts":   sbImport + `export default mergeConfig(viteConfig, defineConfig({ plugins: [storybookTest()] }))`,
				"src/util.test.ts":   ``,
				"src/b.stories.tsx":  ``,
			},
			unit: "src/util.test.ts", browser: true, unsure: "cannot be read",
		},
		{
			name: "unreadable browser project",
			files: map[string]string{
				"vitest.config.ts":         `export default { test: { projects: ['./vitest.browser.config.ts'] } }`,
				"vitest.browser.config.ts": `export default mergeConfig(base, { test: { browser: { enabled: true } } })`,
				"src/a.test.ts":            ``,
			},
			unit: "src/a.test.ts", browser: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for f, content := range c.files {
				p := filepath.Join(dir, filepath.FromSlash(f))
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s, err := NewSuite(&gitx.Repo{Root: dir}, config.Suite{Name: "ui", Runner: "vitest", Root: ".", Command: "vitest run"})
			if err != nil {
				t.Fatal(err)
			}
			if !s.IsTestFile(c.unit) {
				t.Fatalf("%s is not a test (patterns %v, warning %q)", c.unit, s.TestPatterns(), s.DiscoveryWarning)
			}
			u := []string{c.unit}
			if got := s.BrowserUnit(u); got != c.browser {
				t.Errorf("BrowserUnit = %v, want %v", got, c.browser)
			}
			if got := s.RunsWhole(u); got != c.whole {
				t.Errorf("RunsWhole = %v, want %v", got, c.whole)
			}
			globs, starts, unsure := s.BrowserDeps(u)
			sort.Strings(globs)
			want := append([]string{}, c.globs...)
			sort.Strings(want)
			if len(globs)+len(want) > 0 && !reflect.DeepEqual(globs, want) {
				t.Errorf("globs %v, want %v", globs, want)
			}
			if len(starts)+len(c.starts) > 0 && !reflect.DeepEqual(starts, c.starts) {
				t.Errorf("starts %v, want %v", starts, c.starts)
			}
			if c.unsure == "" && unsure != "" || c.unsure != "" && !strings.Contains(unsure, c.unsure) {
				t.Errorf("unsure %q, want %q", unsure, c.unsure)
			}
		})
	}
}

// An addon or framework that may be a local preset (whose settings smates does not
// read) makes story passes untrusted, in every form a path can take; a package name,
// literal or resolved by a known wrapper, does not.
func TestStorybookMainUnsureAddons(t *testing.T) {
	const pre = "import path, { join, dirname, resolve } from 'node:path'\nexport default { stories: ['../src/**/*.stories.tsx'], "
	for _, tc := range []struct {
		name, main string
		unsure     bool
	}{
		{"package names", `addons: ['@storybook/addon-a11y', 'storybook-addon-x/preset'], framework: '@storybook/react-vite'`, false},
		{"getAbsolutePath of a package", `addons: [getAbsolutePath('@storybook/addon-docs')], framework: { name: getAbsolutePath("@storybook/react-vite"), options: {} }`, false},
		{"dirname of require.resolve of a package", `addons: [dirname(require.resolve(join('@storybook/addon-docs', 'package.json')))]`, false},
		{"require.resolve of a package", `addons: [require.resolve('@storybook/addon-docs')]`, false},
		{"an addon object naming a package", `addons: [{ name: '@storybook/addon-docs', options: { mdx: true } }]`, false},
		// Must be unsure: every local path form.
		{"./ literal", `addons: ['./local-addon']`, true},
		{"../ literal", `addons: ['../tools/preset']`, true},
		{"absolute literal", `addons: ['/opt/preset']`, true},
		{"path.join of __dirname with a .. segment", `addons: [path.join(__dirname, "..", "tools", "sb-preset")]`, true},
		{"path.join of __dirname", `addons: [path.join(__dirname, "local-addon")]`, true},
		{"path.resolve of __dirname", `addons: [path.resolve(__dirname, "preset.ts")]`, true},
		{"resolve of a bare path", `addons: [resolve("preset.ts")]`, true},
		{"template literal", "addons: [`${__dirname}/preset.ts`]", true},
		{"require.resolve of a relative path", `addons: [require.resolve('./x')]`, true},
		{"getAbsolutePath of a relative path", `addons: [getAbsolutePath('../addons/x')]`, true},
		{"framework name from join", `framework: { name: join(__dirname, "framework") }`, true},
		{"framework string from join", `framework: join(__dirname, "framework")`, true},
		{"an unknown helper", `addons: [localPreset()]`, true},
		{"a variable", `addons: [presetPath]`, true},
		{"an addon object without a name", `addons: [{ options: {} }]`, true},
		{"options with a relative path", `framework: { name: '@storybook/react-vite', options: { builder: { viteConfigPath: '../vite.sb.ts' } } }`, true},
		{"options with a file-relative path", `framework: { name: '@storybook/react-vite', options: { builder: { viteConfigPath: path.join(__dirname, 'vite.ts') } } }`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := pre + tc.main + " }\n"
			main := readStorybookMain(".storybook", func(f string) []byte {
				if f == ".storybook/main.ts" {
					return []byte(src)
				}
				return nil
			})
			if main.err != nil {
				t.Fatal(main.err)
			}
			got := storybookMainUnsure(main)
			if (got != "") != tc.unsure {
				t.Errorf("unsure %q, want unsure %v", got, tc.unsure)
			}
		})
	}
}

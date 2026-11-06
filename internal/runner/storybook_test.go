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
		{name: "node test pass with no trace, trusted suite", trust: &yes, file: "src/a.test.ts", status: "pass", want: state.Unknown, warn: "likely killed"},
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
			globs: []string{".storybook/**", "public/**", "assets/**"}, starts: []string{".storybook/preview.tsx"},
		},
		{
			name: "unreadable static directories make passes untrusted",
			files: map[string]string{
				".storybook/main.ts": `export default { stories: ['../src/**/*.stories.tsx'], staticDirs: dirs }`,
				"vitest.config.ts":   sbImport + `export default defineConfig({ plugins: [storybookTest()] })`,
				"src/b.stories.tsx":  ``,
			},
			unit: "src/b.stories.tsx", browser: true, globs: []string{".storybook/**"}, unsure: "staticDirs is not a literal",
		},
		{
			name: "fallback stories run whole and are untrusted",
			files: map[string]string{
				".storybook/main.ts": `export default { stories: list }`,
				"vitest.config.ts":   sbImport + `export default defineConfig({ plugins: [storybookTest()] })`,
				"src/b.stories.tsx":  ``,
			},
			unit: "src/b.stories.tsx", browser: true, whole: true, globs: []string{".storybook/**"}, unsure: "stories is not a literal",
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

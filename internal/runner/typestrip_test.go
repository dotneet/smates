package runner

import (
	"sort"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
)

// fakeSources is a repository of files with their content; imports gives the static
// import edges Closure follows.
func fakeSources(files map[string]string, imports map[string][]string) Sources {
	var list []string
	for f := range files {
		list = append(list, f)
	}
	sort.Strings(list)
	return Sources{
		Files: list,
		Read: func(p string) []byte {
			c, ok := files[p]
			if !ok {
				return nil
			}
			return []byte(c)
		},
		Closure: func(start ...string) []string {
			seen := map[string]bool{}
			var visit func(string)
			visit = func(p string) {
				if seen[p] {
					return
				}
				seen[p] = true
				for _, d := range imports[p] {
					visit(d)
				}
			}
			for _, s := range start {
				visit(s)
			}
			var out []string
			for p := range seen {
				out = append(out, p)
			}
			sort.Strings(out)
			return out
		},
	}
}

func TestErasesTypes(t *testing.T) {
	base := map[string]string{
		"web/src/a.ts":      `export const a = 1`,
		"web/src/a.test.ts": `import { a } from "./a"`,
	}
	tests := []struct {
		name    string
		runner  string
		root    string // default web
		command string
		extra   []string
		env     map[string]string
		files   map[string]string
		imports map[string][]string
		want    bool
	}{
		{name: "vitest from node_modules/.bin", runner: "vitest", command: "./node_modules/.bin/vitest run", want: true},
		{name: "vitest through npx", runner: "vitest", command: "npx --no-install vitest run", want: true},
		{name: "vitest through pnpm exec", runner: "vitest", command: "pnpm exec vitest run --coverage", want: true},
		{name: "vitest config without typecheck", runner: "vitest", command: "vitest run",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { include: ["src/**/*.test.ts"] } }`}, want: true},
		{name: "unrelated config elsewhere mentions typecheck", runner: "vitest", command: "vitest run",
			files: map[string]string{"api/vitest.config.ts": `export default { test: { typecheck: { enabled: true } } }`}, want: true},
		{name: "package script", runner: "vitest", command: "npm test", want: false},
		{name: "package script named vitest", runner: "vitest", command: "npm run vitest", want: false},
		{name: "package script named vitest through run-script", runner: "vitest", command: "npm run-script vitest -- --run", want: false},
		{name: "pnpm runs a script named vitest", runner: "vitest", command: "pnpm vitest run",
			files: map[string]string{"web/package.json": `{"scripts": {"vitest": "tsc --noEmit && vitest"}}`}, want: false},
		{name: "yarn runs a script named vitest from a parent package", runner: "vitest", command: "yarn vitest run",
			files: map[string]string{"package.json": `{"scripts": {"vitest": "vitest"}}`}, want: false},
		{name: "pnpm runs the vitest binary", runner: "vitest", command: "pnpm vitest run",
			files: map[string]string{"web/package.json": `{"scripts": {"test": "vitest"}}`}, want: true},
		{name: "pnpm with an unreadable package.json", runner: "vitest", command: "pnpm vitest run",
			files: map[string]string{"web/package.json": `{"scripts": `}, want: false},
		{name: "pnpm filter selects a package's script", runner: "vitest", command: "pnpm --filter web vitest run",
			files: map[string]string{"web/package.json": `{"scripts": {"vitest": "tsc && vitest"}}`}, want: false},
		{name: "pnpm -C", runner: "vitest", command: "pnpm -C web vitest", want: false},
		{name: "yarn workspace", runner: "vitest", command: "yarn workspace web vitest", want: false},
		{name: "yarn --cwd", runner: "vitest", command: "yarn --cwd web vitest", want: false},
		{name: "bun --cwd", runner: "vitest", command: "bun --cwd web vitest", want: false},
		{name: "npm exec", runner: "vitest", command: "npm exec vitest run", want: true},
		{name: "pnpm dlx", runner: "vitest", command: "pnpm dlx vitest run", want: true},
		{name: "bun x", runner: "vitest", command: "bun x vitest run", want: true},
		{name: "no command", runner: "vitest", command: "", want: false},
		{name: "typecheck flag", runner: "vitest", command: "vitest run --typecheck", want: false},
		{name: "typecheck passed through", runner: "vitest", command: "vitest run", extra: []string{"--typecheck.enabled"}, want: false},
		{name: "shell pipeline", runner: "vitest", command: "vitest run && echo done", want: false},
		{name: "tsc in the command", runner: "vitest", command: "sh -c 'tsc --noEmit; vitest run'", want: false},
		{name: "loader in the environment", runner: "vitest", command: "vitest run", env: map[string]string{"NODE_OPTIONS": "--loader ts-node/esm"}, want: false},
		{name: "typecheck in the config", runner: "vitest", command: "vitest run",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { typecheck: { enabled: true } } }`}, want: false},
		{name: "typecheck in a nested project config", runner: "vitest", command: "vitest run",
			files: map[string]string{"web/pkg/vite.config.mts": `export default { test: { typecheck: { enabled: true } } }`}, want: false},
		{name: "typecheck in a config above the suite", runner: "vitest", command: "vitest run",
			files: map[string]string{"vitest.workspace.ts": `export default [{ test: { typecheck: { enabled: true } } }]`}, want: false},
		{name: "typecheck in a module the config imports", runner: "vitest", command: "vitest run",
			files: map[string]string{
				"web/vitest.config.ts":   `import shared from "../shared/vitest.ts"; export default shared`,
				"shared/vitest.ts":       `export default { test: { typecheck: { enabled: true } } }`,
				"shared/unrelated.ts":    `typecheck`,
				"web/src/also-unused.ts": `typecheck`,
			},
			imports: map[string][]string{"web/vitest.config.ts": {"shared/vitest.ts"}}, want: false},
		{name: "compiler options in the config", runner: "vitest", command: "vitest run",
			files: map[string]string{"web/vite.config.ts": `export default { esbuild: { tsconfigRaw: { compilerOptions: { verbatimModuleSyntax: true } } } }`}, want: false},
		{name: "type test file", runner: "vitest", command: "vitest run",
			files: map[string]string{"web/src/a.test-d.ts": `expectTypeOf(1).toBeNumber()`}, want: false},
		{name: "type test file in another suite", runner: "vitest", command: "vitest run",
			files: map[string]string{"api/src/a.spec-d.ts": ``}, want: true},
		{name: "config from the command", runner: "vitest", command: "vitest run --config ../configs/vitest.ts",
			files: map[string]string{"configs/vitest.ts": `export default { test: { typecheck: { enabled: true } } }`}, want: false},
		{name: "config from the command without typecheck", runner: "vitest", command: "vitest run -c ../configs/vitest.ts",
			files: map[string]string{"configs/vitest.ts": `export default {}`}, want: true},
		{name: "missing config from the command", runner: "vitest", command: "vitest run --config missing.ts", want: false},
		{name: "repo root suite", runner: "vitest", root: ".", command: "vitest run", want: true},
		{name: "config with official plugins", runner: "vitest", command: "vitest run",
			files: map[string]string{"web/vitest.config.ts": `import { defineConfig } from "vitest/config"
import react from "@vitejs/plugin-react"
import path from "node:path"
export default defineConfig({ plugins: [react()], resolve: { alias: { "@": path.resolve("src") } } })`}, want: true},
		{name: "preset from a package", runner: "vitest", command: "vitest run",
			files: map[string]string{"web/vitest.config.ts": `import preset from "@acme/vitest-preset"; export default preset`}, want: false},
		{name: "plugin from a package", runner: "vitest", command: "vitest run",
			files: map[string]string{"web/vite.config.ts": `const checker = require("some-plugin"); module.exports = { plugins: [checker()] }`}, want: false},
		{name: "projects named by path", runner: "vitest", command: "vitest run",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { projects: ["../shared/vitest.config.ts"] } }`}, want: false},
		{name: "project extending a shared config", runner: "vitest", command: "vitest run",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { name: "web" }, extends: "../vitest.shared.ts" }`}, want: false},
		{name: "workspace file", runner: "vitest", command: "vitest run",
			files: map[string]string{"web/vitest.workspace.ts": `export default ["packages/*"]`}, want: false},

		{name: "bun test", runner: "bun", command: "bun test", want: true},
		{name: "bun test with files", runner: "bun", command: "bun test --timeout 5000", want: true},
		{name: "bun package script", runner: "bun", command: "bun run test", want: false},
		{name: "bun tsconfig override", runner: "bun", command: "bun test --tsconfig-override=tsconfig.test.json", want: false},

		{name: "jest", runner: "jest", command: "npx jest", want: false},
		{name: "node", runner: "node", command: "node --test", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, ok := Get(tt.runner)
			if !ok {
				t.Fatalf("no adapter %q", tt.runner)
			}
			root := tt.root
			if root == "" {
				root = "web"
			}
			files := map[string]string{}
			for p, c := range base {
				files[p] = c
			}
			for p, c := range tt.files {
				files[p] = c
			}
			s := &Suite{Suite: config.Suite{Name: tt.runner, Runner: tt.runner, Root: root, Command: tt.command, Env: tt.env},
				Adapter: a, ExtraArgs: tt.extra}
			if got := ErasesTypes(s, fakeSources(files, tt.imports)); got != tt.want {
				t.Errorf("ErasesTypes = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestErasesTypesAggregatorsAndPlugins covers the vitest configurations smates still
// sees in full although they name other configuration files or import packages: an
// aggregator of literal project files, Tailwind's plugin, Storybook's plugin with a
// readable main.*, type-only imports. Every way it cannot see one keeps type-only
// imports (want false).
func TestErasesTypesAggregatorsAndPlugins(t *testing.T) {
	agg := `export default defineConfig({ test: { projects: ['./vitest.unit.config.ts', './vitest.dom.config.ts'] } })`
	node := `export default { test: { include: ['src/**/*.test.ts'] } }`
	ui := `import tailwind from '@tailwindcss/vite'
export default mergeConfig(base, defineConfig({ plugins: [tailwind()], test: { browser: { enabled: true } } }))`
	sbConfig := `import path from 'node:path'
import { defineConfig } from 'vitest/config'
import { storybookTest } from '@storybook/addon-vitest/vitest-plugin'
const dirname = path.dirname(new URL(import.meta.url).pathname)
export default defineConfig({ plugins: [storybookTest({ configDir: path.join(dirname, '.storybook') })] })`
	sbMainOK := `import type { StorybookConfig } from '@storybook/react-vite'
const config: StorybookConfig = {
  stories: ['../src/**/*.stories.tsx'],
  addons: ['@storybook/addon-a11y', { name: '@storybook/addon-docs', options: {} }, '@storybook/addon-vitest'],
  framework: { name: '@storybook/react-vite', options: {} },
}
export default config`
	sbVersion := `{"name":"@storybook/addon-vitest","version":"10.2.1"}`
	sb := func(main string, extra ...string) map[string]string {
		m := map[string]string{"web/vitest.config.ts": sbConfig, "web/.storybook/main.ts": main,
			"node_modules/@storybook/addon-vitest/package.json": sbVersion}
		for i := 0; i+1 < len(extra); i += 2 {
			m[extra[i]] = extra[i+1]
		}
		return m
	}
	tests := []struct {
		name  string
		files map[string]string
		env   map[string]string
		want  bool
	}{
		{name: "aggregator of literal project files with Tailwind", want: true,
			files: map[string]string{"web/vitest.config.ts": agg, "web/vitest.unit.config.ts": node, "web/vitest.dom.config.ts": ui}},
		{name: "inline projects extending the root configuration", want: true,
			files: map[string]string{"web/vitest.config.ts": `export default { test: { projects: [
  { extends: true, test: { name: 'unit', include: ['src/**/*.test.ts'] } },
  { test: { name: 'other' } },
] } }`}},
		{name: "type-only import of an unknown package", want: true,
			files: map[string]string{"web/vitest.config.ts": `import type { Options } from '@acme/types'
export default { test: {} } satisfies Options`}},
		{name: "Storybook's plugin with a known main", want: true, files: sb(sbMainOK)},
		{name: "Storybook's plugin with the default configuration directory", want: true,
			files: sb(sbMainOK, "web/vitest.config.ts", `import { storybookTest } from '@storybook/addon-vitest/vitest-plugin'
export default { plugins: [storybookTest()] }`)},

		// Must keep: what an aggregator names may check types.
		{name: "a project file sets typecheck",
			files: map[string]string{"web/vitest.config.ts": agg, "web/vitest.unit.config.ts": `export default { test: { typecheck: { enabled: true } } }`, "web/vitest.dom.config.ts": ui}},
		{name: "a project file is missing",
			files: map[string]string{"web/vitest.config.ts": agg, "web/vitest.unit.config.ts": node}},
		{name: "a project glob",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { projects: ['./packages/*'] } }`, "web/packages/a/vitest.config.ts": node}},
		{name: "a project directory",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { projects: ['./packages/a'] } }`, "web/packages/a/vitest.config.ts": node}},
		{name: "a negated project",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { projects: ['./vitest.unit.config.ts', '!./x.ts'] } }`, "web/vitest.unit.config.ts": node}},
		{name: "a spread project list",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { projects: [...shared, './vitest.unit.config.ts'] } }`, "web/vitest.unit.config.ts": node}},
		{name: "a project list from a variable",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { projects: list } }`}},
		{name: "an inline project extending a file",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { projects: [{ extends: './vitest.shared.ts' }] } }`, "web/vitest.shared.ts": node}},
		{name: "projects inside mergeConfig",
			files: map[string]string{"web/vitest.config.ts": `export default mergeConfig(base, { test: { projects: ['./vitest.unit.config.ts'] } })`, "web/vitest.unit.config.ts": node}},
		{name: "a project root",
			files: map[string]string{"web/vitest.config.ts": `export default { root: 'src', test: { projects: ['./vitest.unit.config.ts'] } }`, "web/src/vitest.unit.config.ts": node, "web/vitest.unit.config.ts": node}},
		{name: "a project file that is itself an aggregator",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { projects: ['./vitest.unit.config.ts'] } }`,
				"web/vitest.unit.config.ts": `export default { test: { projects: ['./vitest.deep.config.ts'] } }`, "web/vitest.deep.config.ts": node}},
		{name: "an unknown plugin imported by a project file only",
			files: map[string]string{"web/vitest.config.ts": agg, "web/vitest.unit.config.ts": node,
				"web/vitest.dom.config.ts": `import checker from 'vite-plugin-checker'; export default { plugins: [checker()] }`}},
		{name: "another projects option next to test.projects",
			files: map[string]string{"web/vitest.config.ts": `import paths from 'vite-tsconfig-paths'
export default { plugins: [paths({ projects: ['./tsconfig.json'] })], test: { projects: ['./vitest.unit.config.ts'] } }`, "web/vitest.unit.config.ts": node}},
		{name: "test.workspace",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { workspace: './w.ts' } }`, "web/w.ts": `export default []`}},
		{name: "a value import of a package also imported for types",
			files: map[string]string{"web/vitest.config.ts": `import type { Options } from '@acme/preset'
import preset from '@acme/preset'
export default preset`}},
		{name: "an unknown package named in a comment only",
			files: map[string]string{"web/vitest.config.ts": `// import preset from '@acme/preset'
export default {}`}},

		// Must keep: what Storybook's plugin loads by name may check types.
		{name: "Storybook main with viteFinal", files: sb(`export default { stories: [], framework: '@storybook/react-vite', viteFinal: (c) => c }`)},
		{name: "Storybook main with typescript options", files: sb(`export default { stories: [], framework: '@storybook/react-vite', typescript: { reactDocgen: 'react-docgen-typescript' } }`)},
		{name: "Storybook main with a spread", files: sb(`export default { ...base, stories: [], framework: '@storybook/react-vite' }`)},
		{name: "an unknown Storybook addon", files: sb(`export default { stories: [], addons: ['storybook-addon-custom'], framework: '@storybook/react-vite' }`)},
		{name: "Storybook addons through a function", files: sb(`export default { stories: [], addons: [getAbsolutePath('@storybook/addon-docs')], framework: '@storybook/react-vite' }`)},
		{name: "a Storybook framework with a type-aware docgen", files: sb(`export default { stories: [], framework: '@storybook/vue3-vite' }`)},
		{name: "no Storybook framework", files: sb(`export default { stories: [] }`)},
		{name: "no Storybook main", files: sb(sbMainOK, "web/.storybook/main.ts", "")},
		{name: "a computed Storybook configuration directory",
			files: sb(sbMainOK, "web/vitest.config.ts", `import { storybookTest } from '@storybook/addon-vitest/vitest-plugin'
export default { plugins: [storybookTest({ configDir: process.env.SB })] }`)},
		{name: "Storybook's plugin before Storybook 10",
			files: sb(sbMainOK, "node_modules/@storybook/addon-vitest/package.json", `{"version":"9.1.0"}`)},
		{name: "Storybook's plugin not installed",
			files: sb(sbMainOK, "node_modules/@storybook/addon-vitest/package.json", ``)},
		{name: "Storybook's experimental plugin",
			files: sb(sbMainOK, "web/vitest.config.ts", `import { storybookTest } from '@storybook/experimental-addon-test/vitest-plugin'
export default { plugins: [storybookTest()] }`)},
		{name: "Storybook's component paths in the environment", files: sb(sbMainOK), env: map[string]string{"STORYBOOK_COMPONENT_PATHS": "src"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{}
			for p, c := range tt.files {
				if c != "" || !strings.HasSuffix(p, "main.ts") && !strings.HasSuffix(p, "package.json") {
					files[p] = c
				}
			}
			a, _ := Get("vitest")
			s := &Suite{Suite: config.Suite{Name: "web", Runner: "vitest", Root: "web", Command: "vitest run", Env: tt.env}, Adapter: a}
			if got := ErasesTypes(s, fakeSources(files, nil)); got != tt.want {
				t.Errorf("ErasesTypes = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestErasesTypesResolvedPackage: a package the static analysis resolves inside the
// repository (a workspace package) is seen, its files being in the closure scanned;
// one it does not resolve is not.
func TestErasesTypesResolvedPackage(t *testing.T) {
	files := map[string]string{
		"web/vitest.config.ts":     `import { shared } from '@ws/test-config'; export default shared`,
		"packages/tc/src/index.ts": `export const shared = { test: {} }`,
	}
	pkg := `{"name": "@ws/test-config"}`
	cases := []struct {
		name     string
		resolves string // the file the specifier resolves to ("" = none)
		content  string
		extra    map[string]string
		want     bool
	}{
		{name: "resolved inside the repository", resolves: "packages/tc/src/index.ts", content: files["packages/tc/src/index.ts"],
			extra: map[string]string{"packages/tc/package.json": pkg}, want: true},
		{name: "resolved inside the repository, which enables typecheck", resolves: "packages/tc/src/index.ts", content: `export const shared = { test: { typecheck: { enabled: true } } }`,
			extra: map[string]string{"packages/tc/package.json": pkg}},
		{name: "not resolved", content: files["packages/tc/src/index.ts"]},
		// Must keep types: at run time the installed package loads, not what the static
		// analysis resolved the name to.
		{name: "a declaration file (tsconfig paths)", resolves: "web/types/test-config.d.ts", content: files["packages/tc/src/index.ts"],
			extra: map[string]string{"web/types/test-config.d.ts": `export declare const shared: object`}},
		{name: "a manual mock", resolves: "web/__mocks__/@ws/test-config.ts", content: files["packages/tc/src/index.ts"],
			extra: map[string]string{"web/package.json": `{"name": "web"}`, "web/__mocks__/@ws/test-config.ts": `export const shared = {}`}},
		{name: "a source file of another package (tsconfig paths)", resolves: "web/shims/test-config.ts", content: files["packages/tc/src/index.ts"],
			extra: map[string]string{"web/package.json": `{"name": "web"}`, "web/shims/test-config.ts": `export const shared = {}`}},
		{name: "a source file under no package.json", resolves: "packages/tc/src/index.ts", content: files["packages/tc/src/index.ts"]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := map[string]string{"web/vitest.config.ts": files["web/vitest.config.ts"], "packages/tc/src/index.ts": c.content}
			for k, v := range c.extra {
				fs[k] = v
			}
			var imports map[string][]string
			src := fakeSources(fs, nil)
			if c.resolves != "" {
				imports = map[string][]string{"web/vitest.config.ts": {c.resolves}}
				src = fakeSources(fs, imports)
				src.Resolve = func(from, spec string) []string {
					if from == "web/vitest.config.ts" && spec == "@ws/test-config" {
						return []string{c.resolves}
					}
					return nil
				}
			}
			a, _ := Get("vitest")
			s := &Suite{Suite: config.Suite{Name: "web", Runner: "vitest", Root: "web", Command: "vitest run"}, Adapter: a}
			if got := ErasesTypes(s, src); got != c.want {
				t.Errorf("ErasesTypes = %v, want %v", got, c.want)
			}
		})
	}
}

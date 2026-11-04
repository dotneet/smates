package runner

import (
	"sort"
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

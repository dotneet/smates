package runner

import (
	"reflect"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
)

func TestRunnerConfig(t *testing.T) {
	tests := []struct {
		name        string
		runner      string
		command     string
		configFiles []string // Suite.ConfigFiles (from discovery)
		files       map[string]string
		imports     map[string][]string
		want        []string
		browser     bool
	}{
		{name: "no configuration", runner: "vitest",
			files: map[string]string{"web/src/a.test.ts": ""}},
		{name: "setup file named relative to the config", runner: "vitest",
			files: map[string]string{
				"web/vitest.config.ts":   `export default { test: { setupFiles: ["./test/setup.ts"] } }`,
				"web/test/setup.ts":      `import "./matchers"`,
				"web/test/matchers.ts":   ``,
				"web/test/unrelated.ts":  ``,
				"web/src/component.ts":   ``,
				"web/src/component.test": ``,
			},
			imports: map[string][]string{"web/test/setup.ts": {"web/test/matchers.ts"}},
			want:    []string{"web/test/matchers.ts", "web/test/setup.ts", "web/vitest.config.ts"}},
		{name: "extensionless literal", runner: "vitest",
			files: map[string]string{
				"web/vitest.config.ts": `export default { test: { globalSetup: "test/global" } }`,
				"web/test/global.ts":   ``,
			},
			want: []string{"web/test/global.ts", "web/vitest.config.ts"}},
		{name: "plugin imported by a per-project config, and what it imports", runner: "vitest",
			files: map[string]string{
				"web/vitest.browser.config.ts": `import api from "./test/api-plugin"; export default mergeConfig(base, { plugins: [api()] })`,
				"web/test/api-plugin.ts":       `import data from "./data/items"`,
				"web/test/data/items.ts":       ``,
			},
			imports: map[string][]string{
				"web/vitest.browser.config.ts": {"web/test/api-plugin.ts"},
				"web/test/api-plugin.ts":       {"web/test/data/items.ts"},
			},
			want: []string{"web/test/api-plugin.ts", "web/test/data/items.ts", "web/vitest.browser.config.ts"}},
		{name: "literal named by an imported file", runner: "vitest",
			files: map[string]string{
				"web/vitest.config.ts": `import { shared } from "./vitest.shared"; export default shared`,
				"web/vitest.shared.ts": `export const shared = { test: { setupFiles: ["./test/setup.ts"] } }`,
				"web/test/setup.ts":    ``,
			},
			imports: map[string][]string{"web/vitest.config.ts": {"web/vitest.shared.ts"}},
			want:    []string{"web/test/setup.ts", "web/vitest.config.ts", "web/vitest.shared.ts"}},
		{name: "alias target", runner: "vitest",
			files: map[string]string{
				"web/vite.config.ts":      `export default { resolve: { alias: { "next/image": "./mocks/image.tsx" } } }`,
				"web/mocks/image.tsx":     ``,
				"web/src/uses-image.tsx":  ``,
				"web/src/uses-image.test": ``,
			},
			want: []string{"web/mocks/image.tsx", "web/vite.config.ts"}},
		{name: "config above the suite root", runner: "vitest",
			files: map[string]string{
				"vitest.config.ts":  `export default { test: { setupFiles: ["./setup/root.ts"] } }`,
				"setup/root.ts":     ``,
				"web/src/a.test.ts": ``,
			},
			want: []string{"setup/root.ts", "vitest.config.ts"}},
		{name: "config of a sibling package is not the suite's", runner: "vitest",
			files: map[string]string{
				"api/vitest.config.ts": `export default { test: { setupFiles: ["./setup.ts"] } }`,
				"api/setup.ts":         ``,
			}},
		{name: "project config with any name found by discovery", runner: "vitest", configFiles: []string{"web/config/unit.ts"},
			files: map[string]string{
				"web/config/unit.ts":   `export default { test: { setupFiles: ["../test/setup.ts"] } }`,
				"web/test/setup.ts":    ``,
				"web/vitest.config.ts": `export default { test: { projects: ["./config/unit.ts"] } }`,
			},
			want: []string{"web/config/unit.ts", "web/test/setup.ts", "web/vitest.config.ts"}},
		{name: "--config of the command", runner: "vitest", command: "vitest run -c conf/v.ts",
			files: map[string]string{
				"web/conf/v.ts": `export default { test: { setupFiles: "./s.ts" } }`,
				"web/conf/s.ts": ``,
				"web/s.ts":      ``,
			},
			want: []string{"web/conf/s.ts", "web/conf/v.ts"}},
		{name: "literals that name no file", runner: "vitest",
			files: map[string]string{
				"web/vitest.config.ts": `export default { test: { environment: "jsdom", include: ["src/**/*.test.ts"], alias: { "@": "./src" }, reporters: ["https://x/y.js"], out: "../../../outside.ts" } }`,
				"web/src/a.ts":         ``,
			},
			want: []string{"web/vitest.config.ts"}},
		{name: "jest <rootDir>", runner: "jest",
			files: map[string]string{
				"web/jest.config.js": `module.exports = { setupFilesAfterEach: ["<rootDir>/test/after.ts"] }`,
				"web/test/after.ts":  ``,
			},
			want: []string{"web/jest.config.js", "web/test/after.ts"}},
		{name: "browser mode", runner: "vitest", browser: true,
			files: map[string]string{"web/vitest.browser.config.ts": `export default { test: { browser: { enabled: true, provider: "playwright" } } }`},
			want:  []string{"web/vitest.browser.config.ts"}},
		{name: "browser mode in an unreadable config", runner: "vitest", browser: true,
			files: map[string]string{"web/vitest.config.ts": `export default mergeConfig(base, defineConfig({ test: { browser: opts } }))`},
			want:  []string{"web/vitest.config.ts"}},
		{name: "browser provider package", runner: "vitest", browser: true,
			files: map[string]string{"web/vitest.config.ts": `import { playwright } from "@vitest/browser/providers/playwright"; export default {}`},
			want:  []string{"web/vitest.config.ts"}},
		{name: "storybook plugin", runner: "vitest", browser: true,
			files: map[string]string{"web/vitest.config.ts": `import { storybookTest } from "@storybook/addon-vitest/vitest-plugin"; export default {}`},
			want:  []string{"web/vitest.config.ts"}},
		{name: "browser turned off", runner: "vitest",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { browser: false } }`},
			want:  []string{"web/vitest.config.ts"}},
		{name: "node runner has no configuration loader", runner: "node",
			files: map[string]string{"web/vitest.config.ts": `export default { test: { setupFiles: ["./s.ts"] } }`, "web/s.ts": ``}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, ok := Get(tt.runner)
			if !ok {
				t.Fatalf("no adapter %q", tt.runner)
			}
			cmd := tt.command
			if cmd == "" {
				cmd = tt.runner + " run"
			}
			s := &Suite{Suite: config.Suite{Name: tt.runner, Runner: tt.runner, Root: "web", Command: cmd}, Adapter: a, ConfigFiles: tt.configFiles}
			got := ConfigOf(s, fakeSources(tt.files, tt.imports))
			if !reflect.DeepEqual(got.Files, tt.want) {
				t.Errorf("Files = %v, want %v", got.Files, tt.want)
			}
			if got.Browser != tt.browser {
				t.Errorf("Browser = %v, want %v", got.Browser, tt.browser)
			}
		})
	}
}

func TestVitestInputsPerProjectConfigs(t *testing.T) {
	s := &Suite{Suite: config.Suite{Root: "web", Inputs: vitest{}.Defaults(&gitx.Repo{Root: t.TempDir()}, "web").Inputs}}
	for f, want := range map[string]bool{
		"web/vitest.config.ts":            true,
		"web/vitest.browser.config.ts":    true,
		"web/vitest.storybook.config.mts": true,
		"web/vitest.node.config.cts":      true,
		"web/vitest.config.e2e.ts":        true,
		"web/src/vitest.helpers.ts":       false,
	} {
		if got := s.InputMatch(f); got != want {
			t.Errorf("InputMatch(%s) = %v, want %v", f, got, want)
		}
	}
}

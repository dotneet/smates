package selector

import (
	"slices"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
)

// runnerConfigFixture is a vitest suite in web/ whose configuration loads files by
// name: config holds the configuration files (and what they load), on top of a test
// of src/used.ts and a module no test imports (src/unused.ts).
func runnerConfigFixture(t *testing.T, config_ map[string]string) *fixture {
	files := map[string]string{
		"web/src/used.ts":      `export const used = 1`,
		"web/src/used.test.ts": `import { used } from "./used"`,
		"web/src/unused.ts":    `export const unused = 1`,
	}
	for p, c := range config_ {
		files[p] = c
	}
	f := newFixture(t, files)
	f.suites = []*runner.Suite{suite(t, config.Suite{Name: "web", Root: "web", Runner: "vitest", Command: "vitest run", Tests: []string{"src/**/*.test.ts"}})}
	return f
}

// TestUntestedRunnerConfig: a file the runner loads through its configuration is
// never untested code (its change goes to the orphan policy), nor is any code of a
// suite whose tests may run in a browser.
func TestUntestedRunnerConfig(t *testing.T) {
	nodeConfig := map[string]string{
		"web/vitest.config.ts": `import fakeServer from "./support/fake-server"
export default { plugins: [fakeServer()], test: { setupFiles: ["./test/dom-setup.ts"], alias: { "x": "./test/x-mock" } } }`,
		"web/test/dom-setup.ts":      `import "./matchers"`,
		"web/test/matchers.ts":       `export {}`,
		"web/support/fake-server.ts": `import { items } from "./data/items"; export default () => ({ name: "fake-server" })`,
		"web/support/data/items.ts":  `export const items = []`,
		"web/test/x-mock.ts":         `export {}`,
	}
	browserConfig := map[string]string{
		"web/vitest.config.ts":     `export default { test: { projects: ["./vitest.e2e.config.ts"] } }`,
		"web/vitest.e2e.config.ts": `export default mergeConfig(base, defineConfig({ test: { browser: { enabled: true } } }))`,
	}
	cases := []struct {
		name     string
		config   map[string]string
		change   string
		untested bool
	}{
		{name: "control: a module no test imports", config: nodeConfig, change: "web/src/unused.ts", untested: true},
		{name: "setup file named by the configuration", config: nodeConfig, change: "web/test/dom-setup.ts"},
		{name: "module a setup file imports", config: nodeConfig, change: "web/test/matchers.ts"},
		{name: "plugin the configuration imports", config: nodeConfig, change: "web/support/fake-server.ts"},
		{name: "fixture a plugin imports", config: nodeConfig, change: "web/support/data/items.ts"},
		{name: "alias target", config: nodeConfig, change: "web/test/x-mock.ts"},
		{name: "browser project: a module no test imports", config: browserConfig, change: "web/src/unused.ts"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := runnerConfigFixture(t, c.config)
			st := f.record(nil, nil)
			f.write(c.change, f.files[c.change]+"\n// changed")
			res := f.sel(st, nil)
			if !slices.Contains(res.Orphans, c.change) {
				t.Fatalf("orphans %v, want %s among them", res.Orphans, c.change)
			}
			if got := slices.Contains(res.Untested(res.Orphans), c.change); got != c.untested {
				t.Errorf("untested(%s) = %v, want %v", c.change, got, c.untested)
			}
		})
	}
}

// TestSelectTypeOnlyImportOfLoadedFile: a test that imports a Vite plugin only for its
// types (the mock API the plugin serves) still depends on it once types are erased:
// the runner loads the plugin anyway. Other type-only imports stay erased.
func TestSelectTypeOnlyImportOfLoadedFile(t *testing.T) {
	cfg := map[string]string{
		"web/vitest.config.ts":       `import fakeServer from "./support/fake-server"; export default { plugins: [fakeServer()] }`,
		"web/support/fake-server.ts": `export type Item = { id: string }; export default () => ({ name: "fake-server" })`,
		"web/src/types.ts":           `export type User = { id: string }`,
		"web/src/api.test.ts":        `import type { Item } from "../support/fake-server"; import type { User } from "./types"`,
	}
	cases := []struct {
		name   string
		change string
		want   string
	}{
		{name: "plugin loaded by the configuration", change: "web/support/fake-server.ts", want: ReasonChanged},
		{name: "control: a module imported for its types only", change: "web/src/types.ts", want: "-unaffected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := runnerConfigFixture(t, cfg)
			st := f.record(nil, nil)
			f.write(c.change, f.files[c.change]+"\n// changed")
			if got := decisions(f.sel(st, nil))["web/src/api.test.ts"]; got != c.want {
				t.Errorf("api.test.ts: %s, want %s", got, c.want)
			}
		})
	}
}

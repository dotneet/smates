package selector

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// storybookFixture is a toy component package whose stories Storybook's Vitest plugin
// runs in a browser: web/ui/.storybook configures them (a preview importing a theme, a
// static directory), and button.tsx and card.tsx are each rendered by one story.
func storybookFixture(t *testing.T, trust bool, extra map[string]string) *fixture {
	files := map[string]string{
		"web/ui/package.json": `{"name":"ui"}`,
		"web/ui/vitest.config.ts": `import path from "node:path"
import { defineConfig } from "vitest/config"
import { storybookTest } from "@storybook/addon-vitest/vitest-plugin"
const dirname = path.dirname(new URL(import.meta.url).pathname)
export default defineConfig({
  plugins: [storybookTest({ configDir: path.join(dirname, ".storybook") })],
  test: { browser: { enabled: true } },
})`,
		"web/ui/.storybook/main.ts": `const config = {
  stories: ["../src/**/*.stories.@(ts|tsx)"],
  staticDirs: ["../public"],
}
export default config`,
		"web/ui/.storybook/preview.tsx": `import "../src/theme"`,
		"web/ui/src/theme.ts":           `export const theme = {}`,
		"web/ui/src/button.tsx":         `export const Button = 1`,
		"web/ui/src/button.stories.tsx": `import { Button } from "./button"`,
		"web/ui/src/card.tsx":           `export const Card = 1`,
		"web/ui/src/card.stories.tsx":   `import { Card } from "./card"`,
		"web/ui/src/unused.ts":          `export const unused = 1`,
		"web/ui/public/logo.svg":        `<svg/>`,
	}
	for p, c := range extra {
		files[p] = c
	}
	f := newFixture(t, files)
	// The packages the configuration imports are installed (outside the snapshot).
	for _, pkg := range []string{"vitest", "@storybook/addon-vitest"} {
		if err := os.MkdirAll(filepath.Join(f.dir, "web", "node_modules", filepath.FromSlash(pkg)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s, err := runner.NewSuite(&gitx.Repo{Root: f.dir}, config.Suite{Name: "ui", Root: "web/ui", Runner: "vitest", Command: "vitest run", TrustBrowserPasses: &trust})
	if err != nil {
		t.Fatal(err)
	}
	f.suites = []*runner.Suite{s}
	return f
}

// recordBrowser simulates a recorded run of every unit in which each passed as the
// reporter says, without a trace: with trust_browser_passes the runner hands such a
// browser test over as a StaticOnly pass, otherwise as unknown.
func (f *fixture) recordBrowser() *state.State {
	f.t.Helper()
	st := state.New()
	res := f.sel(st, func(in *Input) { in.All = true })
	h := NewHasher(f.snap()).WithConfig(f.cfg)
	for _, sr := range res.Suites {
		obs := map[string]*runner.Observation{}
		for _, tt := range sr.Tests {
			o := &runner.Observation{Files: map[string]bool{}, Status: state.Unknown}
			if config.Bool(sr.Suite.TrustBrowserPasses, false) && sr.Suite.BrowserUnit(sr.Files[tt.ID]) {
				o.Status, o.StaticOnly = state.Pass, true
			}
			obs[tt.ID] = o
		}
		Record(st, sr, obs, h.ForSuite(sr.Suite), RecordOptions{Commit: "c1", Now: time.Unix(1, 0), Full: true})
	}
	st.Files = map[string]string(f.snap())
	return st
}

func TestSelectStories(t *testing.T) {
	button, card := "web/ui/src/button.stories.tsx", "web/ui/src/card.stories.tsx"
	cases := []struct {
		name   string
		trust  bool
		extra  map[string]string
		change string // "" = none
		want   map[string]string
		orphan bool // the change is an orphan the orphan policy handles (never untested)
	}{
		{name: "not trusted: stories run every time", want: map[string]string{button: ReasonFailed, card: ReasonFailed}},
		{name: "trusted: nothing changed", trust: true, want: map[string]string{button: "-unaffected", card: "-unaffected"}},
		{name: "trusted: a component selects the story rendering it", trust: true, change: "web/ui/src/button.tsx",
			want: map[string]string{button: ReasonChanged, card: "-unaffected"}},
		// Must keep: what every story loads besides its imports.
		{name: "trusted: a preview-only change selects every story", trust: true, change: "web/ui/.storybook/preview.tsx",
			want: map[string]string{button: ReasonChanged, card: ReasonChanged}},
		{name: "trusted: what the preview imports", trust: true, change: "web/ui/src/theme.ts",
			want: map[string]string{button: ReasonChanged, card: ReasonChanged}},
		{name: "trusted: a file of a static directory", trust: true, change: "web/ui/public/logo.svg",
			want: map[string]string{button: ReasonChanged, card: ReasonChanged}},
		{name: "trusted: Storybook's main", trust: true, change: "web/ui/.storybook/main.ts",
			want: map[string]string{button: ReasonChanged, card: ReasonChanged}},
		{name: "trusted: a file in the configuration directory", trust: true, change: "web/ui/.storybook/mocks.ts",
			extra: map[string]string{"web/ui/.storybook/mocks.ts": `export {}`},
			want:  map[string]string{button: ReasonChanged, card: ReasonChanged}},
		{name: "trusted: the Vitest configuration", trust: true, change: "web/ui/vitest.config.ts",
			want: map[string]string{button: ReasonChanged, card: ReasonChanged}},
		{name: "trusted: a setup file the configuration names", trust: true, change: "web/ui/test/setup.ts",
			extra: map[string]string{
				"web/ui/vitest.config.ts": `import { storybookTest } from "@storybook/addon-vitest/vitest-plugin"
export default { plugins: [storybookTest()], test: { setupFiles: ["./test/setup.ts"] } }`,
				"web/ui/test/setup.ts": `export {}`,
			},
			want: map[string]string{button: ReasonChanged, card: ReasonChanged}},
		// Must keep: an import smates cannot resolve may load anything.
		{name: "trusted: a story with an unresolvable import is never trusted", trust: true,
			extra: map[string]string{"web/ui/src/card.stories.tsx": `import { Card } from "./card"; import data from "~mocks/cards"`},
			want:  map[string]string{button: "-unaffected", card: ReasonFailed}},
		{name: "trusted: an unresolvable import in the preview distrusts every story", trust: true,
			extra: map[string]string{"web/ui/.storybook/preview.tsx": `import "virtual:theme"`},
			want:  map[string]string{button: ReasonFailed, card: ReasonFailed}},
		{name: "trusted: unreadable static directories distrust every story", trust: true,
			extra: map[string]string{"web/ui/.storybook/main.ts": `export default { stories: ["../src/**/*.stories.tsx"], staticDirs: dirs }`},
			want:  map[string]string{button: ReasonFailed, card: ReasonFailed}},
		// Must keep: what main.* imports runs for every story (a mock it aliases in).
		{name: "trusted: a module Storybook's main imports", trust: true, change: "web/ui/tools/mock-api.ts",
			extra: map[string]string{
				"web/ui/.storybook/main.ts": `import { mockApi } from "../tools/mock-api"
export default { stories: ["../src/**/*.stories.@(ts|tsx)"], staticDirs: ["../public"], addons: [mockApi] }`,
				"web/ui/tools/mock-api.ts": `export const mockApi = "x"`,
			},
			want: map[string]string{button: ReasonChanged, card: ReasonChanged}},
		// Must keep: Vite settings of main.* (viteFinal), preview annotations and local
		// addons may load anything for the stories.
		{name: "trusted: viteFinal distrusts every story", trust: true,
			extra: map[string]string{"web/ui/.storybook/main.ts": `export default { stories: ["../src/**/*.stories.@(ts|tsx)"], staticDirs: [], viteFinal: (c) => c }`},
			want:  map[string]string{button: ReasonFailed, card: ReasonFailed}},
		{name: "trusted: preview annotations distrust every story", trust: true,
			extra: map[string]string{"web/ui/.storybook/main.ts": `export default { stories: ["../src/**/*.stories.@(ts|tsx)"], staticDirs: [], previewAnnotations: ["../tools/a.ts"] }`},
			want:  map[string]string{button: ReasonFailed, card: ReasonFailed}},
		{name: "trusted: a local addon distrusts every story", trust: true,
			extra: map[string]string{"web/ui/.storybook/main.ts": `export default { stories: ["../src/**/*.stories.@(ts|tsx)"], staticDirs: [], addons: ["./preset.ts"] }`},
			want:  map[string]string{button: ReasonFailed, card: ReasonFailed}},
		{name: "a module no story imports goes to the orphan policy", trust: true, change: "web/ui/src/unused.ts",
			want: map[string]string{button: "-unaffected", card: "-unaffected"}, orphan: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := storybookFixture(t, c.trust, c.extra)
			st := f.recordBrowser()
			if c.change != "" {
				f.write(c.change, f.files[c.change]+"\n// changed")
			}
			res := f.sel(st, nil)
			got := decisions(res)
			for id, want := range c.want {
				if got[id] != want {
					t.Errorf("%s: %s, want %s", id, got[id], want)
				}
			}
			if c.orphan {
				if !slices.Contains(res.Orphans, c.change) || slices.Contains(res.Untested(res.Orphans), c.change) {
					t.Errorf("orphans %v (untested %v): want %s an orphan, not untested", res.Orphans, res.Untested(res.Orphans), c.change)
				}
			} else if len(res.Orphans) > 0 {
				t.Errorf("orphans %v", res.Orphans)
			}
		})
	}
}

// TestSelectWholeStorybookProject: when the stories of a project cannot be read, the
// story files are a guess; selecting one runs the whole suite, which runs every story
// whatever smates guessed.
func TestSelectWholeStorybookProject(t *testing.T) {
	cases := []struct {
		name  string
		main  string
		whole bool
	}{
		{name: "computed stories", main: `export default { stories: findStories() }`, whole: true},
		{name: "readable stories", main: `export default { stories: ["../src/**/*.stories.tsx"] }`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := storybookFixture(t, false, map[string]string{
				"web/ui/vitest.config.ts":      `export default { test: { projects: ["./vitest.unit.config.ts", "./vitest.storybook.config.ts"] } }`,
				"web/ui/vitest.unit.config.ts": `export default { test: { include: ["src/**/*.test.ts"] } }`,
				"web/ui/vitest.storybook.config.ts": `import { storybookTest } from "@storybook/addon-vitest/vitest-plugin"
export default mergeConfig(base, defineConfig({ plugins: [storybookTest()] }))`,
				"web/ui/.storybook/main.ts": c.main,
				"web/ui/src/theme.test.ts":  `import { theme } from "./theme"`,
			})
			st := f.record(nil, nil) // every unit passed and was traced
			f.write("web/ui/src/button.tsx", "export const Button = 2")
			res := f.sel(st, nil)
			if d := decisions(res); d["web/ui/src/theme.test.ts"] != "-unaffected" {
				t.Fatalf("decisions before: %v", d)
			}
			adds := res.WholeSuites()
			if got := len(adds) > 0; got != c.whole {
				t.Fatalf("WholeSuites = %v, want whole %v", adds, c.whole)
			}
			res.Apply(adds)
			if c.whole && (!res.Suites[0].All || decisions(res)["web/ui/src/theme.test.ts"] != ReasonAll) {
				t.Errorf("whole suite not selected: %v", decisions(res))
			}
		})
	}
}

// TestRecordUntrustedBrowserPass: a pass trust_browser_passes takes from the reporter
// is recorded unknown when the test's known dependencies may not be all it loads.
func TestRecordUntrustedBrowserPass(t *testing.T) {
	f := storybookFixture(t, true, map[string]string{"web/ui/src/card.stories.tsx": `import data from "@/fixtures/cards"`})
	st := f.recordBrowser()
	card := st.Suites["ui"].Tests["web/ui/src/card.stories.tsx"]
	button := st.Suites["ui"].Tests["web/ui/src/button.stories.tsx"]
	if card == nil || card.Status != state.Unknown || button == nil || button.Status != state.Pass {
		t.Fatalf("card %+v, button %+v", card, button)
	}
	res := f.sel(st, nil)
	if why := res.Suites[0].Untrusted["web/ui/src/card.stories.tsx"]; !strings.Contains(why, `"@/fixtures/cards"`) {
		t.Errorf("Untrusted = %q", why)
	}
	// The dependencies of a trusted pass are the static ones, with the preview's.
	for _, d := range []string{"web/ui/src/button.tsx", "web/ui/.storybook/preview.tsx", "web/ui/src/theme.ts", "web/ui/public/logo.svg"} {
		if _, ok := button.Deps[d]; !ok {
			t.Errorf("button story deps lack %s: %v", d, button.Deps)
		}
	}
}

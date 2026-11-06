package selector

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/state"
)

// typeOnlyFixture is a TypeScript suite whose tests reach src/types.ts only through
// type-only imports: cart.test.ts and cart.ts through import type, user.ts through
// an import whose specifiers are all types.
func typeOnlyFixture(t *testing.T, cs config.Suite, extra map[string]string) *fixture {
	files := map[string]string{
		"web/src/types.ts":     `export type User = { id: string }`,
		"web/src/cart.ts":      `import type { User } from "./types"; export const total = (u: User) => 1`,
		"web/src/cart.test.ts": `import { total } from "./cart"; import type { User } from "./types"`,
		"web/src/user.ts":      `import { type User } from "./types"; export const makeUser = (): User => ({ id: "1" })`,
		"web/src/user.test.ts": `import { makeUser } from "./user"`,
		"web/vitest.config.ts": `export default { test: { include: ["src/**/*.test.ts"] } }`,
	}
	for p, c := range extra {
		files[p] = c
	}
	f := newFixture(t, files)
	cs.Name, cs.Root, cs.Tests = "web", "web", []string{"src/**/*.test.ts"}
	f.suites = []*runner.Suite{suite(t, cs)}
	return f
}

func TestSelectTypeOnlyImports(t *testing.T) {
	vitest := config.Suite{Runner: "vitest", Command: "vitest run"}
	cases := []struct {
		name  string
		suite config.Suite
		extra map[string]string
		// want are the decisions after editing types.ts.
		want map[string]string
	}{
		{
			name: "vitest erases types", suite: vitest,
			want: map[string]string{"web/src/cart.test.ts": "-unaffected", "web/src/user.test.ts": "-unaffected"},
		},
		{
			name: "verbatimModuleSyntax keeps inline type imports", suite: vitest,
			extra: map[string]string{"web/tsconfig.json": `{"compilerOptions":{"verbatimModuleSyntax":true}}`},
			want:  map[string]string{"web/src/cart.test.ts": "-unaffected", "web/src/user.test.ts": ReasonChanged},
		},
		{
			name: "vitest typecheck", suite: config.Suite{Runner: "vitest", Command: "vitest run --typecheck"},
			want: map[string]string{"web/src/cart.test.ts": ReasonChanged, "web/src/user.test.ts": ReasonChanged},
		},
		{
			name: "type test files", suite: vitest,
			extra: map[string]string{"web/src/cart.test-d.ts": `import type { User } from "./types"`},
			want:  map[string]string{"web/src/cart.test.ts": ReasonChanged, "web/src/user.test.ts": ReasonChanged},
		},
		{
			name: "jest may check types", suite: config.Suite{Runner: "jest", Command: "npx jest"},
			want: map[string]string{"web/src/cart.test.ts": ReasonChanged, "web/src/user.test.ts": ReasonChanged},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := typeOnlyFixture(t, c.suite, c.extra)
			st := f.record(nil, nil)

			f.write("web/src/types.ts", `export type User = { id: number }`)
			res := f.sel(st, nil)
			got := decisions(res)
			for id, want := range c.want {
				if got[id] != want {
					t.Errorf("after a type-only change, %s: %s, want %s", id, got[id], want)
				}
			}
			if len(res.Orphans) != 0 {
				t.Errorf("a change to a module imported for its types is explained, not an orphan: %v", res.Orphans)
			}

			// A change to a module the tests load still selects them.
			f.write("web/src/cart.ts", `import type { User } from "./types"; export const total = (u: User) => 2`)
			if got := decisions(f.sel(st, nil))["web/src/cart.test.ts"]; got != ReasonChanged {
				t.Errorf("after a value change, cart.test.ts: %s", got)
			}
		})
	}
}

// TestSelectTypeOnlyDiffMode checks diff mode (no recorded state) leaves type-only
// imports out the same way.
func TestSelectTypeOnlyDiffMode(t *testing.T) {
	f := typeOnlyFixture(t, config.Suite{Runner: "vitest", Command: "vitest run"}, nil)
	repo, base := gitFixture(t, f)
	f.write("web/src/types.ts", `export type User = { id: number }`)
	res := f.sel(state.New(), func(in *Input) { in.Repo, in.Base = repo, base })
	if res.Mode != "diff" {
		t.Fatalf("mode %s", res.Mode)
	}
	got := decisions(res)
	for _, id := range []string{"web/src/cart.test.ts", "web/src/user.test.ts"} {
		if got[id] != "-unaffected" {
			t.Errorf("%s: %s", id, got[id])
		}
	}
	if len(res.Orphans) != 0 {
		t.Errorf("orphans %v", res.Orphans)
	}
}

// TestSelectTypeOnlyThroughAggregator: a package whose vitest configuration is an
// aggregator of a node project and a Storybook project (with Tailwind's plugin) still
// erases type-only imports, as long as smates sees every configuration in full.
func TestSelectTypeOnlyThroughAggregator(t *testing.T) {
	cases := []struct {
		name string
		main string
		want string // cart.test.ts after a type-only change
	}{
		{name: "every configuration seen", want: "-unaffected",
			main: `import type { StorybookConfig } from "@storybook/react-vite"
const config: StorybookConfig = { stories: ["../src/**/*.stories.tsx"], addons: ["@storybook/addon-a11y"], framework: "@storybook/react-vite" }
export default config`},
		{name: "must keep: Storybook main with viteFinal", want: ReasonChanged,
			main: `export default { stories: ["../src/**/*.stories.tsx"], framework: "@storybook/react-vite", viteFinal: async (c) => c }`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := typeOnlyFixture(t, config.Suite{Runner: "vitest", Command: "vitest run"}, map[string]string{
				"web/vitest.config.ts":      `export default defineConfig({ test: { projects: ["./vitest.node.config.ts", "./vitest.storybook.config.ts"] } })`,
				"web/vitest.node.config.ts": `export default { test: { include: ["src/**/*.test.ts"] } }`,
				"web/vitest.storybook.config.ts": `import tailwind from "@tailwindcss/vite"
import { storybookTest } from "@storybook/addon-vitest/vitest-plugin"
export default mergeConfig(base, defineConfig({ plugins: [tailwind(), storybookTest()] }))`,
				"web/.storybook/main.ts": c.main,
			})
			pkg := filepath.Join(f.dir, "node_modules", "@storybook", "addon-vitest")
			if err := os.MkdirAll(pkg, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"version":"10.0.0"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			st := f.record(nil, nil)
			f.write("web/src/types.ts", `export type User = { id: number }`)
			if got := decisions(f.sel(st, nil))["web/src/cart.test.ts"]; got != c.want {
				t.Errorf("cart.test.ts after a type-only change: %s, want %s", got, c.want)
			}
		})
	}
}

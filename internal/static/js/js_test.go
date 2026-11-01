package js

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func TestHandles(t *testing.T) {
	a := New(&fsHost{files: map[string]bool{}}, Options{})
	for p, want := range map[string]bool{
		"a.js": true, "a.mjs": true, "a.cjs": true, "a.jsx": true,
		"a.ts": true, "a.mts": true, "a.cts": true, "a.tsx": true, "a.d.ts": true,
		"a.vue": true, "a.svelte": true, "a.astro": true,
		"a.json": false, "a.py": false, "a.go": false, "a.css": false, "Makefile": false,
	} {
		if got := a.Handles(p); got != want {
			t.Errorf("Handles(%q) = %v, want %v", p, got, want)
		}
	}
}

// TestDeps covers resolution. Each case is a fixture repository and the file whose
// direct dependencies are checked.
func TestDeps(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		file  string
		want  []string
	}{
		{
			name: "relative with extension probing",
			files: map[string]string{
				"src/main.ts":       `import a from "./a"; import b from "./b"; import c from "../lib/c"; import d from "./d.json"; import "./style.css"`,
				"src/a.ts":          ``,
				"src/b.tsx":         ``,
				"lib/c.js":          ``,
				"src/d.json":        `{}`,
				"src/style.css":     ``,
				"src/unrelated.ts":  ``,
				"src/main.test.ts":  ``,
				"src/missing-is-ok": ``,
			},
			file: "src/main.ts",
			want: []string{"lib/c.js", "src/a.ts", "src/b.tsx", "src/d.json", "src/style.css"},
		},
		{
			name: "every existing extension variant is returned",
			files: map[string]string{
				"main.ts":    `import x from "./x"`,
				"x.ts":       ``,
				"x.d.ts":     ``,
				"x.vue":      ``,
				"x.svelte":   ``,
				"x.mts":      ``,
				"xy.ts":      ``,
				"x/index.ts": ``, // a file wins over a directory
			},
			file: "main.ts",
			want: []string{"x.d.ts", "x.mts", "x.svelte", "x.ts", "x.vue"},
		},
		{
			name: "emitted extensions map to sources",
			files: map[string]string{
				"main.ts":  `import a from "./a.js"; import b from "./b.mjs"; import c from "./c.cjs"; import d from "./d.jsx"; import e from "./e.js"`,
				"a.ts":     ``,
				"b.mts":    ``,
				"c.cts":    ``,
				"d.tsx":    ``,
				"e.js":     ``, // exact file kept
				"e.ts":     ``, // and its source counterpart too
				"other.ts": ``,
			},
			file: "main.ts",
			want: []string{"a.ts", "b.mts", "c.cts", "d.tsx", "e.js", "e.ts"},
		},
		{
			name: "directory index and package.json main",
			files: map[string]string{
				"main.js":                  `require("./utils"); require("./widget"); require("./typed"); require(".."); require(".")`,
				"utils/index.ts":           ``,
				"widget/package.json":      `{"main": "lib/widget.js"}`,
				"widget/lib/widget.js":     ``,
				"widget/index.js":          ``, // main takes precedence
				"typed/package.json":       `{"main": "./dist/missing.js"}`,
				"typed/index.tsx":          ``, // main missing: fall back to index
				"index.mjs":                ``, // require(".") from the root
				"node_modules/x/index.js":  ``,
				"utils/helpers/deep.ts":    ``,
				"widget/lib/unrelated.tsx": ``,
			},
			file: "main.js",
			want: []string{"index.mjs", "typed/index.tsx", "utils/index.ts", "widget/lib/widget.js"},
		},
		{
			name: "queries and loader prefixes are stripped",
			files: map[string]string{
				"main.ts":  `import raw from "./a.txt?raw"; import url from "./logo.svg?url"; require("raw-loader!./b.txt")`,
				"a.txt":    ``,
				"logo.svg": ``,
				"b.txt":    ``,
			},
			file: "main.ts",
			want: []string{"a.txt", "b.txt", "logo.svg"},
		},
		{
			name: "outside the repo, node_modules and externals are never returned",
			files: map[string]string{
				"main.ts":                     `import "../../escape"; import "react"; import "node:fs"; import "fs"; import "https://x/y.js"; import "./node_modules/lib/x"; import "virtual:pwa"; import "./main"`,
				"node_modules/react/index.js": ``,
				"node_modules/lib/x.js":       ``,
				"escape.ts":                   ``,
			},
			file: "main.ts",
			want: nil,
		},
		{
			name: "new URL and triple-slash references",
			files: map[string]string{
				"src/main.ts":        "/// <reference path=\"../types/globals.d.ts\" />\n/// <reference types=\"@acme/env\" />\nconst w = new URL('./worker.ts', import.meta.url);\nconst wasm = new URL(\"./pkg/mod.wasm\", import.meta.url)",
				"src/worker.ts":      ``,
				"src/pkg/mod.wasm":   ``,
				"types/globals.d.ts": ``,
				"env/package.json":   `{"name": "@acme/env", "types": "index.d.ts"}`,
				"env/index.d.ts":     ``,
			},
			file: "src/main.ts",
			// @acme/env has only a types entry, so its whole source is included.
			want: []string{"env/index.d.ts", "env/package.json", "src/pkg/mod.wasm", "src/worker.ts", "types/globals.d.ts"},
		},
		{
			name: "root-relative specifiers",
			files: map[string]string{
				"apps/web/package.json": `{"name": "web"}`,
				"apps/web/src/main.ts":  `import "/src/setup.ts"; import "/shared/x"`,
				"apps/web/src/setup.ts": ``,
				"shared/x.ts":           ``,
				"apps/web/src/other.ts": ``,
			},
			file: "apps/web/src/main.ts",
			want: []string{"apps/web/src/setup.ts", "shared/x.ts"},
		},
		{
			name: "test mocks",
			files: map[string]string{
				"src/a.test.ts": `import { a } from "./a"; vi.mock("./db"); const real = await vi.importActual<typeof import("./net")>("./net"); jest.requireActual("./clock")`,
				"src/a.ts":      ``,
				"src/db.ts":     ``,
				"src/net.ts":    ``,
				"src/clock.js":  ``,
			},
			file: "src/a.test.ts",
			want: []string{"src/a.ts", "src/clock.js", "src/db.ts", "src/net.ts"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHost(t, tt.files)
			assertDeps(t, deps(t, h, tt.file), tt.want)
		})
	}
}

func TestDepsTSConfig(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		file  string
		want  []string
	}{
		{
			name: "paths with baseUrl, JSONC and most specific pattern",
			files: map[string]string{
				"tsconfig.json": `{
  // comment
  "compilerOptions": {
    "baseUrl": "./src", /* block */
    "paths": {
      "@/*": ["*"],
      "@app/*": ["app/*", "fallback/*",],
      "@app/special": ["special/index.ts"],
      "config": ["config/default"],
    },
  },
}`,
				"src/main.ts":            `import a from "@/lib/a"; import b from "@app/b"; import s from "@app/special"; import c from "config"; import u from "utils/u"`,
				"src/lib/a.ts":           ``,
				"src/app/b.ts":           ``,
				"src/fallback/b.ts":      ``, // both targets of the matching rule are kept
				"src/special/index.ts":   ``,
				"src/app/special.ts":     ``, // exact rule wins over @app/*
				"src/config/default.ts":  ``,
				"src/utils/u.ts":         ``, // via baseUrl
				"src/unrelated/thing.ts": ``,
			},
			file: "src/main.ts",
			want: []string{"src/app/b.ts", "src/config/default.ts", "src/fallback/b.ts", "src/lib/a.ts", "src/special/index.ts", "src/utils/u.ts"},
		},
		{
			name: "paths without baseUrl are relative to the declaring config, through extends",
			files: map[string]string{
				"tsconfig.base.json":           `{"compilerOptions": {"paths": {"@shared/*": ["./packages/shared/src/*"]}}}`,
				"apps/web/tsconfig.json":       `{"extends": "../../tsconfig.base.json", "compilerOptions": {"strict": true}}`,
				"apps/web/src/main.ts":         `import x from "@shared/x"`,
				"packages/shared/src/x.ts":     ``,
				"packages/shared/src/other.ts": ``,
			},
			file: "apps/web/src/main.ts",
			want: []string{"packages/shared/src/x.ts"},
		},
		{
			name: "child baseUrl rebases inherited paths; extends without .json; extends array",
			files: map[string]string{
				"configs/base.json":  `{"compilerOptions": {"paths": {"~/*": ["src/*"]}}}`,
				"configs/other.json": `{"compilerOptions": {"paths": {"#never/*": ["x/*"]}}}`,
				"pkg/tsconfig.json":  `{"extends": ["../configs/other", "../configs/base"], "compilerOptions": {"baseUrl": "."}}`,
				"pkg/src/main.ts":    `import x from "~/x"`,
				"pkg/src/x.ts":       ``,
			},
			file: "pkg/src/main.ts",
			want: []string{"pkg/src/x.ts"},
		},
		{
			name: "extends from a workspace package",
			files: map[string]string{
				"packages/tsconfig/package.json": `{"name": "@acme/tsconfig"}`,
				"packages/tsconfig/base.json":    `{"compilerOptions": {"baseUrl": ".", "paths": {"@lib/*": ["../lib/src/*"]}}}`,
				"packages/lib/src/x.ts":          ``,
				"apps/a/tsconfig.json":           `{"extends": "@acme/tsconfig/base.json"}`,
				"apps/a/main.ts":                 `import x from "@lib/x"`,
			},
			file: "apps/a/main.ts",
			want: []string{"packages/lib/src/x.ts"},
		},
		{
			name: "extends cycle terminates",
			files: map[string]string{
				"tsconfig.json": `{"extends": "./b.json", "compilerOptions": {"baseUrl": "src"}}`,
				"b.json":        `{"extends": "./tsconfig.json"}`,
				"main.ts":       `import x from "x"`,
				"src/x.ts":      ``,
			},
			file: "main.ts",
			want: []string{"src/x.ts"},
		},
		{
			name: "sibling tsconfig.*.json and references (Vite layout)",
			files: map[string]string{
				"web/tsconfig.json":       `{"files": [], "references": [{"path": "./tsconfig.app.json"}, {"path": "./tools"}]}`,
				"web/tsconfig.app.json":   `{"compilerOptions": {"paths": {"@/*": ["./src/*"]}}}`,
				"web/tools/tsconfig.json": `{"compilerOptions": {"paths": {"@tools/*": ["./*"]}}}`,
				"web/tsconfig.node.json":  `{"compilerOptions": {"paths": {"@node/*": ["./node/*"]}}}`,
				"web/src/main.ts":         `import a from "@/a"; import t from "@tools/t"; import n from "@node/n"`,
				"web/src/a.ts":            ``,
				"web/tools/t.ts":          ``,
				"web/node/n.ts":           ``,
			},
			file: "web/src/main.ts",
			want: []string{"web/node/n.ts", "web/src/a.ts", "web/tools/t.ts"},
		},
		{
			name: "jsconfig and nearest config wins, ancestors are a fallback",
			files: map[string]string{
				"tsconfig.json":     `{"compilerOptions": {"paths": {"@/*": ["./root/*"], "@root/*": ["./root/*"]}}}`,
				"app/jsconfig.json": `{"compilerOptions": {"baseUrl": ".", "paths": {"@/*": ["./src/*"]}}}`,
				"app/src/main.js":   `import a from "@/a"; import b from "@root/b"`,
				"app/src/a.js":      ``,
				"root/a.ts":         ``, // shadowed by the nearer config
				"root/b.ts":         ``, // only the root config maps @root
			},
			file: "app/src/main.js",
			want: []string{"app/src/a.js", "root/b.ts"},
		},
		{
			name: "invalid config is ignored",
			files: map[string]string{
				"tsconfig.json": `{"compilerOptions": {"paths": `,
				"main.ts":       `import x from "@/x"; import y from "./y"`,
				"y.ts":          ``,
			},
			file: "main.ts",
			want: []string{"y.ts"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHost(t, tt.files)
			assertDeps(t, deps(t, h, tt.file), tt.want)
		})
	}
}

func TestDepsPackages(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		file  string
		want  []string
	}{
		{
			name: "package.json imports field",
			files: map[string]string{
				"package.json": `{"name": "app", "imports": {
  "#db": "./src/db/index.ts",
  "#utils/*": "./src/utils/*.ts",
  "#env": {"node": "./src/env.node.ts", "default": "./src/env.browser.ts"},
  "#dep": "dep"
}}`,
				"src/main.ts":               `import "#db"; import "#utils/str"; import "#env"; import "#dep"; import "#missing"`,
				"src/db/index.ts":           ``,
				"src/utils/str.ts":          ``,
				"src/env.node.ts":           ``,
				"src/env.browser.ts":        ``,
				"packages/dep/package.json": `{"name": "dep", "main": "index.js"}`,
				"packages/dep/index.js":     ``,
			},
			file: "src/main.ts",
			want: []string{"packages/dep/index.js", "src/db/index.ts", "src/env.browser.ts", "src/env.node.ts", "src/utils/str.ts"},
		},
		{
			name: "exports: string, subpaths, nested conditions, patterns, folders, null",
			files: map[string]string{
				"packages/ui/package.json": `{"name": "@acme/ui", "exports": {
  ".": {"types": "./dist/index.d.ts", "import": {"source": "./src/index.ts", "default": "./dist/index.mjs"}},
  "./button": "./src/button.tsx",
  "./icons/*": {"default": "./src/icons/*.tsx"},
  "./icons/internal/*": null,
  "./styles/": "./src/styles/",
  "./package.json": "./package.json"
}}`,
				"packages/ui/src/index.ts":                  ``,
				"packages/ui/src/button.tsx":                ``,
				"packages/ui/src/icons/star.tsx":            ``,
				"packages/ui/src/icons/internal/secret.tsx": ``,
				"packages/ui/src/styles/theme.css":          ``,
				"packages/ui/src/other.ts":                  ``,
				"packages/str/package.json":                 `{"name": "str", "exports": "./lib/str.js"}`,
				"packages/str/lib/str.ts":                   ``,
				"apps/web/main.ts":                          `import "@acme/ui"; import "@acme/ui/button"; import "@acme/ui/icons/star"; import "@acme/ui/styles/theme.css"; import "@acme/ui/package.json"; import "str"`,
			},
			file: "apps/web/main.ts",
			want: []string{
				"packages/str/lib/str.ts", "packages/ui/package.json", "packages/ui/src/button.tsx",
				"packages/ui/src/icons/star.tsx", "packages/ui/src/index.ts", "packages/ui/src/styles/theme.css",
			},
		},
		{
			name: "exports: condition object without subpath keys",
			files: map[string]string{
				"packages/a/package.json": `{"name": "a", "exports": {"require": "./cjs.cjs", "import": "./esm.mjs"}}`,
				"packages/a/cjs.cjs":      ``,
				"packages/a/esm.mjs":      ``,
				"main.mjs":                `import "a"`,
			},
			file: "main.mjs",
			want: []string{"packages/a/cjs.cjs", "packages/a/esm.mjs"},
		},
		{
			name: "unbuilt exports fall back to the whole package source",
			files: map[string]string{
				"packages/core/package.json":                 `{"name": "@acme/core", "exports": {".": {"types": "./dist/index.d.ts", "default": "./dist/index.js"}}}`,
				"packages/core/src/index.ts":                 ``,
				"packages/core/src/deep/x.ts":                ``,
				"packages/core/src/data.json":                `{}`,
				"packages/core/README.md":                    ``,
				"packages/core/dist/old.js":                  ``, // build output, excluded
				"packages/core/coverage/lcov.js":             ``,
				"packages/core/node_modules/dep/index.js":    ``,
				"packages/core/fixtures/nested/package.json": `{"name": "nested"}`,
				"packages/core/fixtures/nested/index.js":     ``, // nested package, excluded
				"packages/core/vitest.config.ts":             ``,
				"apps/web/main.ts":                           `import { x } from "@acme/core"`,
			},
			file: "apps/web/main.ts",
			want: []string{
				"packages/core/package.json", "packages/core/src/data.json", "packages/core/src/deep/x.ts",
				"packages/core/src/index.ts", "packages/core/vitest.config.ts",
			},
		},
		{
			name: "types-only exports target also falls back",
			files: map[string]string{
				"packages/t/package.json": `{"name": "t", "exports": {"types": "./src/index.ts", "default": "./dist/index.js"}}`,
				"packages/t/src/index.ts": ``,
				"packages/t/src/impl.ts":  ``,
				"main.ts":                 `import "t"`,
			},
			file: "main.ts",
			want: []string{"packages/t/package.json", "packages/t/src/impl.ts", "packages/t/src/index.ts"},
		},
		{
			name: "entry fields: source, module, main, types",
			files: map[string]string{
				"packages/a/package.json": `{"name": "a", "source": "src/a.ts", "main": "dist/a.js", "types": "dist/a.d.ts"}`,
				"packages/a/src/a.ts":     ``,
				"packages/a/src/b.ts":     ``,
				"packages/b/package.json": `{"name": "b", "module": "./esm/index.js", "main": "./cjs/index.js"}`,
				"packages/b/esm/index.js": ``,
				"packages/b/cjs/index.js": ``,
				"main.ts":                 `import "a"; import "b"`,
			},
			file: "main.ts",
			want: []string{"packages/a/src/a.ts", "packages/b/cjs/index.js", "packages/b/esm/index.js"},
		},
		{
			name: "index and src/index without entry fields; subpaths without exports",
			files: map[string]string{
				"packages/a/package.json":  `{"name": "a"}`,
				"packages/a/index.ts":      ``,
				"packages/b/package.json":  `{"name": "b"}`,
				"packages/b/src/index.tsx": ``,
				"packages/c/package.json":  `{"name": "c"}`,
				"packages/c/utils/x.js":    ``,
				"packages/c/src/y.ts":      ``,
				"main.ts":                  `import "a"; import "b"; import "c/utils/x"; import "c/y"`,
			},
			file: "main.ts",
			want: []string{"packages/a/index.ts", "packages/b/src/index.tsx", "packages/c/src/y.ts", "packages/c/utils/x.js"},
		},
		{
			name: "npm dependencies and unknown packages produce nothing",
			files: map[string]string{
				"package.json":                    `{"name": "root", "dependencies": {"react": "18"}}`,
				"node_modules/react/package.json": `{"name": "react", "main": "index.js"}`,
				"node_modules/react/index.js":     ``,
				"main.ts":                         `import React from "react"; import { z } from "zod"; import x from "@scope/pkg/sub"`,
			},
			file: "main.ts",
			want: nil,
		},
		{
			name: "tsconfig paths take precedence over a workspace fallback",
			files: map[string]string{
				"tsconfig.json":              `{"compilerOptions": {"paths": {"@acme/core": ["./packages/core/src/index.ts"]}}}`,
				"packages/core/package.json": `{"name": "@acme/core", "main": "dist/index.js"}`,
				"packages/core/src/index.ts": ``,
				"packages/core/src/other.ts": ``,
				"main.ts":                    `import "@acme/core"`,
			},
			file: "main.ts",
			want: []string{"packages/core/src/index.ts"},
		},
		{
			name: "duplicate package names resolve in every package",
			files: map[string]string{
				"a/package.json": `{"name": "dup", "main": "index.js"}`,
				"a/index.js":     ``,
				"b/package.json": `{"name": "dup", "main": "index.js"}`,
				"b/index.js":     ``,
				"main.js":        `require("dup")`,
			},
			file: "main.js",
			want: []string{"a/index.js", "b/index.js"},
		},
		{
			name: "alias heuristics without configuration",
			files: map[string]string{
				"web/package.json":            `{"name": "web"}`,
				"web/src/views/Home.vue":      `<script setup>import X from "@/components/X.vue"; import { y } from "~/lib/y"</script>`,
				"web/src/components/X.vue":    ``,
				"web/src/lib/y.ts":            ``,
				"kit/package.json":            `{"name": "kit"}`,
				"kit/src/routes/+page.svelte": `<script>import B from "$lib/B.svelte"</script>`,
				"kit/src/lib/B.svelte":        ``,
			},
			file: "web/src/views/Home.vue",
			want: []string{"web/src/components/X.vue", "web/src/lib/y.ts"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHost(t, tt.files)
			assertDeps(t, deps(t, h, tt.file), tt.want)
		})
	}
}

func TestDepsSvelteKitAlias(t *testing.T) {
	h := newHost(t, map[string]string{
		"kit/package.json":            `{"name": "kit"}`,
		"kit/src/routes/+page.svelte": `<script>import B from "$lib/B.svelte"</script>`,
		"kit/src/lib/B.svelte":        ``,
	})
	assertDeps(t, deps(t, h, "kit/src/routes/+page.svelte"), []string{"kit/src/lib/B.svelte"})
}

func TestDepsDynamic(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		file  string
		want  []string
	}{
		{
			name: "template literal with static prefix and suffix",
			files: map[string]string{
				"src/load.ts":             "export const load = (n: string) => import(`./plugins/${n}.ts`)",
				"src/plugins/a.ts":        ``,
				"src/plugins/nested/b.ts": ``,
				"src/plugins/c.json":      ``,
				"src/plugins/readme.md":   ``,
				"src/other.ts":            ``,
			},
			file: "src/load.ts",
			want: []string{"src/plugins/a.ts", "src/plugins/nested/b.ts"},
		},
		{
			name: "template literal with partial name, js suffix and no suffix",
			files: map[string]string{
				"i18n.js":              "import(`./locales/msg-${lang}.js`); require(`./data/${k}`); import(`./pages/${p}/index`)",
				"locales/msg-en.ts":    ``,
				"locales/msg-ja.js":    ``,
				"locales/other.ts":     ``,
				"data/x.json":          ``,
				"data/y/z.txt":         ``,
				"pages/home/index.tsx": ``,
				"pages/home/util.ts":   ``,
			},
			file: "i18n.js",
			want: []string{"data/x.json", "data/y/z.txt", "locales/msg-en.ts", "locales/msg-ja.js", "pages/home/index.tsx"},
		},
		{
			name: "template literal through tsconfig paths and workspace package",
			files: map[string]string{
				"tsconfig.json":               `{"compilerOptions": {"paths": {"@/*": ["./src/*"]}}}`,
				"src/main.ts":                 "import(`@/icons/${n}.svg`); import(`@acme/icons/${n}`); import(`${dynamic}`)",
				"src/icons/a.svg":             ``,
				"src/icons/b.png":             ``,
				"packages/icons/package.json": `{"name": "@acme/icons"}`,
				"packages/icons/src/x.ts":     ``,
			},
			file: "src/main.ts",
			want: []string{"packages/icons/package.json", "packages/icons/src/x.ts", "src/icons/a.svg"},
		},
		{
			name: "import.meta.glob",
			files: map[string]string{
				"src/main.ts":               `const pages = import.meta.glob(["./pages/**/*.vue", "!./pages/skip.vue"]); const m = import.meta.glob("/src/mods/*.ts", { eager: true })`,
				"src/pages/Home.vue":        ``,
				"src/pages/skip.vue":        ``, // negations are ignored (over-approximation)
				"src/pages/[id]/Detail.vue": ``,
				"src/pages/notes.md":        ``,
				"src/mods/a.ts":             ``,
				"src/mods/deep/b.ts":        ``,
			},
			file: "src/main.ts",
			want: []string{"src/mods/a.ts", "src/pages/Home.vue", "src/pages/[id]/Detail.vue", "src/pages/skip.vue"},
		},
		{
			name: "require.context",
			files: map[string]string{
				"app.js":           `const flat = require.context("./icons", false, /\.svg$/); const all = require.context("./modules")`,
				"icons/a.svg":      ``,
				"icons/sub/b.svg":  ``,
				"modules/x.js":     ``,
				"modules/sub/y.js": ``,
			},
			file: "app.js",
			want: []string{"icons/a.svg", "modules/sub/y.js", "modules/x.js"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHost(t, tt.files)
			assertDeps(t, deps(t, h, tt.file), tt.want)
		})
	}
}

func TestDepsSFC(t *testing.T) {
	h := newHost(t, map[string]string{
		"src/App.vue":         "<template><Comp/><img src=\"./assets/logo.png\"></template>\n<script setup lang=\"ts\">\nimport Comp from './Comp.vue'\nimport { s } from './store'\n</script>\n<style src=\"./app.css\"></style>",
		"src/Comp.vue":        ``,
		"src/store.ts":        ``,
		"src/assets/logo.png": ``,
		"src/app.css":         ``,
		"src/Page.astro":      "---\nimport Layout from './Layout.astro'\n---\n<Layout />",
		"src/Layout.astro":    ``,
	})
	assertDeps(t, deps(t, h, "src/App.vue"), []string{"src/Comp.vue", "src/app.css", "src/assets/logo.png", "src/store.ts"})
	assertDeps(t, deps(t, h, "src/Page.astro"), []string{"src/Layout.astro"})
}

func TestDepsRoots(t *testing.T) {
	h := newHost(t, map[string]string{
		"main.js":            `require("shared/x")`,
		"vendor/shared/x.js": ``,
	})
	a := New(h, Options{Roots: []string{"vendor"}})
	assertDeps(t, a.Deps("main.js", h.Read("main.js")), []string{"vendor/shared/x.js"})
}

// TestDepsConcurrent runs many resolutions in parallel over shared caches; run with
// -race.
func TestDepsConcurrent(t *testing.T) {
	files := map[string]string{
		"tsconfig.json":              `{"compilerOptions": {"paths": {"@/*": ["./src/*"]}}}`,
		"packages/core/package.json": `{"name": "core", "exports": {".": "./dist/index.js"}}`,
		"packages/core/src/index.ts": ``,
	}
	var want [][]string
	for i := 0; i < 50; i++ {
		files[fmt.Sprintf("src/m%d.ts", i)] = fmt.Sprintf(`import "@/m%d"; import "core"; import "./m%d"`, (i+1)%50, (i+2)%50)
		want = append(want, sorted([]string{ // Deps returns sorted paths
			"packages/core/package.json", "packages/core/src/index.ts",
			fmt.Sprintf("src/m%d.ts", (i+1)%50), fmt.Sprintf("src/m%d.ts", (i+2)%50),
		}))
	}
	h := newHost(t, files)
	a := New(h, Options{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				p := fmt.Sprintf("src/m%d.ts", i)
				got := a.Deps(p, h.Read(p))
				if !reflect.DeepEqual(got, want[i]) {
					t.Errorf("Deps(%s) = %q, want %q", p, got, want[i])
				}
			}(i)
		}
	}
	wg.Wait()
}

func TestStripJSONC(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"comments", "{\n// c\n\"a\": 1, /* b */ \"b\": 2}", `{"a":1,"b":2}`},
		{"trailing commas", `{"a": [1, 2, ], "b": {"c": 3,},}`, `{"a":[1,2],"b":{"c":3}}`},
		{"comment markers in strings", `{"a": "// not a comment", "b": "/* nor */", "c": "q\"//"}`, `{"a":"// not a comment","b":"/* nor */","c":"q\"//"}`},
		{"bom", "\xef\xbb\xbf{\"a\": 1}", `{"a":1}`},
		{"comma before comment before brace", "{\"a\": 1, // x\n}", `{"a":1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v any
			if err := parseJSONC([]byte(tt.in), &v); err != nil {
				t.Fatalf("parseJSONC: %v (stripped: %q)", err, stripJSONC([]byte(tt.in)))
			}
			got, _ := json.Marshal(v)
			if string(got) != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestSubpathTargets(t *testing.T) {
	exports := map[string]any{
		".":         "./index.js",
		"./a/*":     "./src/a/*.js",
		"./a/b/*":   "./src/ab/*.js",
		"./x/*.css": "./styles/*.css",
		"./legacy/": "./lib/",
		"./gone":    nil,
	}
	tests := []struct {
		key  string
		want []string
	}{
		{".", []string{"./index.js"}},
		{"./a/q", []string{"./src/a/q.js"}},
		{"./a/b/q", []string{"./src/ab/q.js"}}, // longest prefix wins
		{"./x/y.css", []string{"./styles/y.css"}},
		{"./x/y.js", nil},
		{"./legacy/f.js", []string{"./lib/f.js"}},
		{"./gone", nil},
		{"./none", nil},
	}
	for _, tt := range tests {
		var got []string
		for _, tg := range subpathTargets(exports, tt.key) {
			got = append(got, tg.path)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("subpathTargets(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestSplitPackage(t *testing.T) {
	tests := []struct{ in, name, sub string }{
		{"react", "react", ""},
		{"react/jsx-runtime", "react", "jsx-runtime"},
		{"@s/p", "@s/p", ""},
		{"@s/p/a/b", "@s/p", "a/b"},
		{"@s", "@s", ""},
	}
	for _, tt := range tests {
		if n, s := splitPackage(tt.in); n != tt.name || s != tt.sub {
			t.Errorf("splitPackage(%q) = %q, %q; want %q, %q", tt.in, n, s, tt.name, tt.sub)
		}
	}
}

func BenchmarkScanSource(b *testing.B) {
	var src []byte
	for i := 0; i < 200; i++ {
		src = append(src, fmt.Sprintf("import { a%d } from './mod%d';\nconst s%d = `x ${a%d / 2} y`; // c\nif (/re[/]x/.test(s%d)) require('./r%d');\n", i, i, i, i, i, i)...)
	}
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scanSource(src)
	}
}

func TestHashTSPaths(t *testing.T) {
	h := newHost(t, map[string]string{
		"tsconfig.json":    `{"compilerOptions":{"paths":{"#lib/*":["./src/lib/*"]}}}`,
		"package.json":     `{"name":"app","imports":{"#db":"./src/db.ts"}}`,
		"src/a.test.ts":    "import x from \"#lib/util\"\nimport db from \"#db\"\nconst m = await import(`#lib/${n}`)",
		"src/lib/util.ts":  ``,
		"src/lib/other.ts": ``,
		"src/db.ts":        ``,
	})
	assertDeps(t, deps(t, h, "src/a.test.ts"), []string{"src/db.ts", "src/lib/other.ts", "src/lib/util.ts"})
}

// TestConfigDirTemplate checks that ${configDir} expands to the directory of the config
// that uses it, not the one that declares it.
func TestConfigDirTemplate(t *testing.T) {
	h := newHost(t, map[string]string{
		"tsconfig.base.json":  `{"compilerOptions":{"baseUrl":"${configDir}/src","paths":{"~lib/*":["${configDir}/src/lib/*"]}}}`,
		"app/tsconfig.json":   `{"extends":"../tsconfig.base.json"}`,
		"app/src/a.test.ts":   "import x from \"~lib/util\"\nconst m = await import(`~lib/${n}`)",
		"app/src/lib/util.ts": ``,
		"web/tsconfig.json":   `{"extends":"../tsconfig.base.json"}`,
		"web/src/b.test.ts":   `import x from "lib/util"`,
		"web/src/lib/util.ts": ``,
	})
	assertDeps(t, deps(t, h, "app/src/a.test.ts"), []string{"app/src/lib/util.ts"})
	assertDeps(t, deps(t, h, "web/src/b.test.ts"), []string{"web/src/lib/util.ts"})
}

func TestTemplateQuerySuffix(t *testing.T) {
	h := newHost(t, map[string]string{
		"src/a.test.ts":   "const m = await import(`./icons/${n}.svg?raw`)\nconst u = await import(`./img/${n}.png#hash`)",
		"src/icons/x.svg": ``,
		"src/icons/y.ts":  ``,
		"src/img/p.png":   ``,
	})
	assertDeps(t, deps(t, h, "src/a.test.ts"), []string{"src/icons/x.svg", "src/img/p.png"})
}

func TestDirnameTemplate(t *testing.T) {
	h := newHost(t, map[string]string{
		"src/a.test.ts": "require(`${__dirname}/fixtures/${n}.json`)\n" +
			"await import(`${import.meta.dirname}/cases/${n}.ts`)\n" +
			"require(__dirname + \"/data/\" + n + \".js\")",
		"src/fixtures/f.json": ``,
		"src/cases/c.ts":      ``,
		"src/data/d.js":       ``,
		"src/other.ts":        ``,
	})
	assertDeps(t, deps(t, h, "src/a.test.ts"), []string{"src/cases/c.ts", "src/data/d.js", "src/fixtures/f.json"})
}

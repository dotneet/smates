package js

import (
	"reflect"
	"testing"
	"time"
)

func obj(fields map[string]any) *Object { return &Object{Fields: fields} }

func TestConfigObject(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want any
		ok   bool
	}{
		{
			name: "module.exports literal",
			src: `'use strict';
/** @type {import('jest').Config} */
module.exports = {
  testEnvironment: 'node', // comment
  testMatch: ["**/?(*.)+(spec|test).[jt]s?(x)", ` + "`<rootDir>/e2e/**/*.e2e.js`" + `],
  testRegex: /\.spec\.ts$/i,
  'roots': ['<rootDir>/src'],
  maxWorkers: 3,
  verbose: true,
  bail: null,
};`,
			want: obj(map[string]any{
				"testEnvironment": "node",
				"testMatch":       []any{"**/?(*.)+(spec|test).[jt]s?(x)", "<rootDir>/e2e/**/*.e2e.js"},
				"testRegex":       Regex{Source: `\.spec\.ts$`, Flags: "i"},
				"roots":           []any{"<rootDir>/src"},
				"maxWorkers":      3.0,
				"verbose":         true,
				"bail":            nil,
			}),
			ok: true,
		},
		{
			name: "export default defineConfig",
			src: `import { defineConfig, configDefaults } from 'vitest/config';
export default defineConfig({
  plugins: [react()],
  test: {
    include: ['src/**/*.test.ts'],
    exclude: [...configDefaults.exclude, 'e2e/**'],
    setup() { return 1 },
  },
});`,
			want: obj(map[string]any{
				"plugins": []any{Unknown("react()")},
				"test": obj(map[string]any{
					"include": []any{"src/**/*.test.ts"},
					"exclude": []any{Unknown("...configDefaults.exclude"), "e2e/**"},
					"setup":   Unknown("setup"),
				}),
			}),
			ok: true,
		},
		{
			name: "binding with type annotation",
			src: `import type { Config } from 'jest';
const config: Config = { testMatch: ['**/*.spec.ts'] } satisfies Config;
export default config;`,
			want: obj(map[string]any{"testMatch": []any{"**/*.spec.ts"}}),
			ok:   true,
		},
		{
			name: "binding without semicolons",
			src: `const config: Config = {
  stories: ['../src/**/*.stories.tsx'],
}
export default config`,
			want: obj(map[string]any{"stories": []any{"../src/**/*.stories.tsx"}}),
			ok:   true,
		},
		{
			name: "an expression continued on the next line is not a literal",
			src: `const config = { a: 1 }
  .concat(x)
export default config`,
			want: Unknown("{ a: 1 }\n  .concat(x)\nexport default config"),
			ok:   true,
		},
		{
			name: "arrow returning an object",
			src:  `export default defineConfig(() => ({ test: { dir: 'src' } }))`,
			want: obj(map[string]any{"test": obj(map[string]any{"dir": "src"})}),
			ok:   true,
		},
		{
			name: "workspace array",
			src:  `export default defineWorkspace(['packages/*', { test: { name: 'unit', include: ['a/**'] } }])`,
			want: []any{"packages/*", obj(map[string]any{"test": obj(map[string]any{"name": "unit", "include": []any{"a/**"}})})},
			ok:   true,
		},
		{
			name: "spread and computed values",
			src:  `module.exports = { ...base, testMatch: dirs.map(d => d + '/**'), roots: ['src', ` + "`${x}`" + `] }`,
			want: &Object{Fields: map[string]any{
				"testMatch": Unknown("dirs.map(d => d + '/**')"),
				"roots":     []any{"src", Unknown("`${x}`")},
			}, Spread: true},
			ok: true,
		},
		{
			name: "wrapper with a trailing comma; member callee",
			src:  `export default vitest.defineProject({ test: { dir: 'src' } },)`,
			want: obj(map[string]any{"test": obj(map[string]any{"dir": "src"})}),
			ok:   true,
		},
		{
			name: "mergeConfig is not unwrapped",
			src:  `export default mergeConfig(base, defineConfig({ test: { include: ['a/**'] } }))`,
			want: Unknown("mergeConfig(base, defineConfig({ test: { include: ['a/**'] } }))"),
			ok:   true,
		},
		{
			name: "unknown single-argument call is not unwrapped",
			src:  `module.exports = withPreset({ testMatch: ['x'] })`,
			want: Unknown("withPreset({ testMatch: ['x'] })"),
			ok:   true,
		},
		{
			name: "wrapper with two arguments is not unwrapped",
			src:  `export default defineConfig({ test: {} }, extra)`,
			want: Unknown("defineConfig({ test: {} }, extra)"),
			ok:   true,
		},
		{
			name: "binding read but not changed",
			src: `const config = { testMatch: ['**/*.spec.ts'] };
if (config.testMatch.length === 0 || config.verbose == null) console.log(config['roots']);
const n = config.testMatch.length <= 1 ? config.x : config.y;
module.exports = config;`,
			want: obj(map[string]any{"testMatch": []any{"**/*.spec.ts"}}),
			ok:   true,
		},
		{
			name: "nested declaration is not the binding",
			src: `function f() { const config = { testMatch: ['nested'] }; return config }
export default config;`,
			want: Unknown("config"),
			ok:   true,
		},
		{
			name: "module.exports inside a block is not the export",
			src:  `if (process.env.CI) { module.exports = { testMatch: ['ci'] } }`,
			ok:   false,
		},
		{name: "no export", src: `const a = { testMatch: [] }`, ok: false},
		{
			name: "function body is not followed",
			src:  `export default defineConfig(() => { return { test: {} } })`,
			want: Unknown("defineConfig(() => { return { test: {} } })"),
			ok:   true,
		},
		{
			name: "async function",
			src:  `module.exports = async () => ({ testMatch: ['x'] });`,
			want: Unknown("async () => ({ testMatch: ['x'] })"),
			ok:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ConfigObject([]byte(c.src))
			if ok != c.ok || !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %#v, %v\nwant %#v, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestObjectGet(t *testing.T) {
	o := &Object{Fields: map[string]any{"a": 1.0}}
	if v, known := o.Get("a"); v != 1.0 || !known {
		t.Errorf("a: %v %v", v, known)
	}
	if v, known := o.Get("b"); v != nil || !known {
		t.Errorf("b: %v %v", v, known)
	}
	o.Spread = true
	if _, known := o.Get("b"); known {
		t.Error("a key missing from an object with a spread is unknown")
	}
}

func TestDecodeJSONObject(t *testing.T) {
	o, err := DecodeJSONObject([]byte(`{ "testMatch": ["a/**"], // c
	"roots": ["src",], "nested": {"x": 1} }`))
	if err != nil {
		t.Fatal(err)
	}
	want := obj(map[string]any{"testMatch": []any{"a/**"}, "roots": []any{"src"}, "nested": obj(map[string]any{"x": 1.0})})
	if !reflect.DeepEqual(o, want) {
		t.Errorf("got %#v", o)
	}
}

// TestConfigObjectMutated checks that a binding or module.exports changed after its
// declaration is not read as its literal.
func TestConfigObjectMutated(t *testing.T) {
	decl := "const config = { testMatch: ['**/*.spec.ts'], roots: ['src'] };\n"
	for _, c := range []struct{ name, src string }{
		{"member assignment", decl + "config.testMatch = ['x'];\nmodule.exports = config;"},
		{"nested member assignment", decl + "config.a.b = 1;\nexport default config;"},
		{"computed member", decl + "config['testMatch'] = ['x'];\nexport default config;"},
		{"computed member with a variable", decl + "const k = 'roots'; config[k] = [];\nexport default config;"},
		{"push", decl + "config.roots.push('lib');\nexport default config;"},
		{"optional chaining push", decl + "config.roots?.push('lib');\nexport default config;"},
		{"splice", decl + "config.testMatch.splice(0, 1);\nexport default config;"},
		{"Object.assign", decl + "Object.assign(config, { testMatch: [] });\nexport default config;"},
		{"compound assignment", decl + "config.verbose ||= true;\nexport default config;"},
		{"delete", decl + "delete config.roots;\nexport default config;"},
		{"reassignment", "let config = { testMatch: ['a'] };\nconfig = { testMatch: ['b'] };\nexport default config;"},
		{"inside a function", decl + "if (process.env.CI) { config.testMatch = ['ci'] }\nexport default defineConfig(config);"},
		{"after the export", decl + "export default config;\nconfig.testMatch = ['x'];"},
		{"module.exports member", "module.exports = { testMatch: ['a'] };\nmodule.exports.testMatch = ['b'];"},
		{"module.exports computed member", "module.exports = { testMatch: ['a'] };\nmodule.exports['roots'] = ['b'];"},
		{"exports member", "module.exports = { testMatch: ['a'] };\nexports.roots = ['b'];"},
		{"module.exports push", "module.exports = { testMatch: ['a'] };\nmodule.exports.testMatch.push('b');"},
		{"module.exports reassigned", "module.exports = { testMatch: ['a'] };\nmodule.exports = require('./other');"},
		{"Object.assign module.exports", "module.exports = { testMatch: ['a'] };\nObject.assign(module.exports, other);"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v, ok := ConfigObject([]byte(c.src))
			if _, unknown := v.(Unknown); !ok || !unknown {
				t.Errorf("got %#v, %v; want Unknown", v, ok)
			}
		})
	}
}

// TestConfigObjectTerminates guards against parsing loops: a method body with several
// statements, and stray ';' or ')' inside an object or array, must neither hang nor
// end the object early.
func TestConfigObjectTerminates(t *testing.T) {
	srcs := []string{
		"module.exports = {\n  resolver(p, o) { const r = o.defaultResolver(p, o); return r; },\n  testMatch: [\"**/*.e2e.js\"],\n};",
		"module.exports = {\n  resolver(p, o) { return o.defaultResolver(p, o) },\n  testMatch: [\"**/*.e2e.js\"],\n};",
		"export default { a: 1; b: 2 }",
		"module.exports = { testMatch: ['x'; ] }",
		"export default { ) }",
		"export default [1; 2)",
	}
	for _, src := range srcs {
		done := make(chan any, 1)
		go func() {
			v, _ := ConfigObject([]byte(src))
			done <- v
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("ConfigObject(%q) does not terminate", src)
		}
	}
	for _, src := range srcs[:2] {
		v, ok := ConfigObject([]byte(src))
		o, _ := v.(*Object)
		if !ok || o == nil {
			t.Fatalf("ConfigObject(%q) = %#v, %v", src, v, ok)
		}
		if tm, known := o.Get("testMatch"); !known || !reflect.DeepEqual(tm, []any{"**/*.e2e.js"}) {
			t.Errorf("%q: testMatch = %#v, %v", src, tm, known)
		}
		if _, known := o.Get("roots"); !known {
			t.Errorf("%q: roots should be known absent", src)
		}
	}
	// After an unexpected token, later keys may have been missed.
	v, _ := ConfigObject([]byte("export default { a: 1; b: 2 }"))
	if o, _ := v.(*Object); o == nil || !o.Spread {
		t.Errorf("malformed object: got %#v, want Spread", v)
	}
}

func TestStringLiterals(t *testing.T) {
	cases := []struct {
		src  string
		want []string
	}{
		{`export default { test: { setupFiles: ["./a.ts", './b.ts'] } }`, []string{"./a.ts", "./b.ts"}},
		{"const x = `./plain.ts`; const y = `./${n}.ts`", []string{"./plain.ts"}},
		{"// setupFiles: [\"./commented.ts\"]\n/* \"./block.ts\" */ const s = \"./kept.ts\"", []string{"./kept.ts"}},
		{`const s = "a\"b"`, []string{`a"b`}},
		{`const r = /"not-a-string"/`, nil},
	}
	for _, c := range cases {
		if got := StringLiterals([]byte(c.src)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("StringLiterals(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

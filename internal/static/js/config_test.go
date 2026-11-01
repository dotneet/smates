package js

import (
	"reflect"
	"testing"
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
		{name: "no export", src: `const a = { testMatch: [] }`, ok: false},
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

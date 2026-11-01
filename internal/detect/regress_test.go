package detect

import (
	"reflect"
	"sort"
	"testing"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
	"github.com/dotneet/smates/internal/testutil"
)

func TestDetectRegressions(t *testing.T) {
	pyTest := "import pytest\ndef test_a():\n    pass\n"
	type want struct {
		suites  []got
		command string            // of the first suite
		env     map[string]string // of the first suite
		tests   []string          // test files of the first suite
	}
	cases := []struct {
		name  string
		files map[string]string
		want  want
	}{
		{
			// Python project files in testdata/vendor are data, as go.mod there is.
			name: "pyproject in testdata",
			files: map[string]string{
				"go.mod": "module m", "a_test.go": "",
				"internal/py/testdata/proj/pyproject.toml": "[project]\nname='x'\n", "internal/py/testdata/proj/test_x.py": pyTest,
				"vendor/lib/setup.py": "", "vendor/lib/test_v.py": pyTest,
			},
			want: want{suites: []got{{"go", ".", "go"}}},
		},
		{
			// Fixture projects still get suites, as Go modules and JS packages there do.
			name: "pyproject in fixtures",
			files: map[string]string{
				"package.json": vitestPkg, "a.test.ts": "",
				"test/fixtures/proj/pyproject.toml": "[project]\nname='x'\n", "test/fixtures/proj/test_x.py": pyTest,
			},
			want: want{suites: []got{{"vitest", ".", "vitest"}, {"pytest", "test/fixtures/proj", "pytest"}}},
		},
		{
			name: "craco",
			files: map[string]string{
				"package.json":    `{"name":"x","scripts":{"test":"craco test"},"dependencies":{"@craco/craco":"7"}}`,
				"src/App.test.js": "",
			},
			want: want{suites: []got{{"jest", ".", "jest"}}, command: "npx --no-install craco test --watchAll=false", tests: []string{"src/App.test.js"}},
		},
		{
			name:  "react-app-rewired",
			files: map[string]string{"package.json": `{"name":"x","scripts":{"test":"react-app-rewired test --env=jsdom"}}`, "src/App.test.js": ""},
			want:  want{suites: []got{{"jest", ".", "jest"}}, command: "npx --no-install react-app-rewired test --env=jsdom --watchAll=false", tests: []string{"src/App.test.js"}},
		},
		{
			name:  "react-scripts with two spaces",
			files: map[string]string{"package.json": `{"name":"x","scripts":{"test":"react-scripts  test"}}`, "src/App.test.js": ""},
			want:  want{suites: []got{{"jest", ".", "jest"}}, command: "npx --no-install react-scripts test --watchAll=false", tests: []string{"src/App.test.js"}},
		},
		{
			// react-scripts only searches src/.
			name: "CRA tests outside src",
			files: map[string]string{
				"package.json":    `{"name":"x","scripts":{"test":"react-scripts test"}}`,
				"src/App.test.js": "", "src/__tests__/b.tsx": "", "e2e/login.spec.ts": "", "scripts/build.test.js": "",
			},
			want: want{suites: []got{{"jest", ".", "jest"}}, command: "npx --no-install react-scripts test --watchAll=false", tests: []string{"src/App.test.js", "src/__tests__/b.tsx"}},
		},
		{
			// A testMatch override in package.json is read from there.
			name: "CRA testMatch override",
			files: map[string]string{
				"package.json":   `{"name":"x","scripts":{"test":"react-scripts test"},"jest":{"testMatch":["<rootDir>/src/**/*.check.js"]}}`,
				"src/a.check.js": "", "src/App.test.js": "",
			},
			want: want{suites: []got{{"jest", ".", "jest"}}, command: "npx --no-install react-scripts test --watchAll=false", tests: []string{"src/a.check.js"}},
		},
		{
			name:  "node --import tsx --test",
			files: map[string]string{"package.json": `{"name":"x","scripts":{"test":"node --import tsx --test src/**/*.test.ts"}}`, "src/a.test.ts": ""},
			want:  want{suites: []got{{"node", ".", "node"}}, command: "node --import tsx --test", tests: []string{"src/a.test.ts"}},
		},
		{
			name:  "node --experimental-strip-types --test",
			files: map[string]string{"package.json": `{"name":"x","scripts":{"test":"TZ=UTC node --experimental-strip-types --test"}}`, "test/a.test.ts": ""},
			want:  want{suites: []got{{"node", ".", "node"}}, command: "node --experimental-strip-types --test", env: map[string]string{"TZ": "UTC"}, tests: []string{"test/a.test.ts"}},
		},
		{
			name:  "node --loader --test",
			files: map[string]string{"package.json": `{"name":"x","scripts":{"test":"node --loader ts-node/esm --test"}}`, "test/a.test.ts": ""},
			want:  want{suites: []got{{"node", ".", "node"}}, command: "node --loader ts-node/esm --test", tests: []string{"test/a.test.ts"}},
		},
		{
			// Runner calls inside quoted arguments.
			name:  "concurrently react-scripts test",
			files: map[string]string{"package.json": `{"name":"x","scripts":{"test":"concurrently \"react-scripts test\" \"echo hi\""}}`, "src/App.test.js": ""},
			want:  want{suites: []got{{"jest", ".", "jest"}}, command: "npx --no-install react-scripts test --watchAll=false", tests: []string{"src/App.test.js"}},
		},
		{
			name:  "sh -c react-scripts test",
			files: map[string]string{"package.json": `{"name":"x","scripts":{"test":"sh -c 'CI=true react-scripts test'"}}`, "src/App.test.js": ""},
			want:  want{suites: []got{{"jest", ".", "jest"}}, command: "npx --no-install react-scripts test --watchAll=false", env: map[string]string{"CI": "true"}, tests: []string{"src/App.test.js"}},
		},
		{
			name:  "sh -c node --test",
			files: map[string]string{"package.json": `{"name":"x","scripts":{"test":"sh -c \"node --test\""}}`, "test/a.test.js": ""},
			want:  want{suites: []got{{"node", ".", "node"}}, tests: []string{"test/a.test.js"}},
		},
		{
			// A wrapper's configuration may change roots/testMatch: jest's own patterns.
			name: "craco with config",
			files: map[string]string{
				"package.json":    `{"name":"x","scripts":{"test":"craco test"}}`,
				"craco.config.js": "module.exports = {}\n", "src/App.test.js": "", "test/b.test.js": "",
			},
			want: want{suites: []got{{"jest", ".", "jest"}}, command: "npx --no-install craco test --watchAll=false", tests: []string{"src/App.test.js", "test/b.test.js"}},
		},
		{
			// The suite must exit: watch options are dropped.
			name:  "node --watch --test",
			files: map[string]string{"package.json": `{"name":"x","scripts":{"test":"node --watch --watch-path src --test-reporter spec --test"}}`, "test/a.test.js": ""},
			want:  want{suites: []got{{"node", ".", "node"}}, command: "node --test-reporter spec --test", tests: []string{"test/a.test.js"}},
		},
		{
			name:  "plain node --test",
			files: map[string]string{"package.json": `{"name":"x","scripts":{"test":"node --test"}}`, "test/a.test.js": ""},
			want:  want{suites: []got{{"node", ".", "node"}}, tests: []string{"test/a.test.js"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, s := detect(t, c.files)
			if !reflect.DeepEqual(g, c.want.suites) {
				t.Fatalf("suites %v, want %v", g, c.want.suites)
			}
			if s[0].Command != c.want.command || !reflect.DeepEqual(s[0].Env, c.want.env) {
				t.Errorf("command %q env %v, want %q %v", s[0].Command, s[0].Env, c.want.command, c.want.env)
			}
			if c.want.tests == nil {
				return
			}
			dir := t.TempDir()
			var list []string
			for f, content := range c.files {
				testutil.Write(t, dir, f, content)
				list = append(list, f)
			}
			rs, err := runner.NewSuite(&gitx.Repo{Root: dir}, s[0])
			if err != nil {
				t.Fatal(err)
			}
			got := rs.TestFiles(list)
			sort.Strings(got)
			if !reflect.DeepEqual(got, c.want.tests) {
				t.Errorf("tests %v, want %v", got, c.want.tests)
			}
		})
	}
}

func TestDetectNodeScriptIsNotATestRun(t *testing.T) {
	for _, script := range []string{"node scripts/run.js --test", "node -r ./setup.js scripts/run.js --test"} {
		g, _ := detect(t, map[string]string{
			"package.json":   `{"name":"x","scripts":{"test":"` + script + `"}}`,
			"test/a.test.js": "",
		})
		if len(g) != 0 {
			t.Errorf("%q: suites %v, want none", script, g)
		}
	}
}

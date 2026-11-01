package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/trace"
)

// The node runner collects the files node --test's default patterns name, TypeScript
// variants of all of them included.
func TestNodeDefaultPatterns(t *testing.T) {
	s, err := NewSuite(tempRepo(t), config.Suite{Name: "n", Runner: "node", Root: "."})
	if err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]bool{
		"test.ts": true, "test/math.ts": true, "test/unit/a.mts": true, "pkg/test/b.cts": true, "a.test.ts": true,
		"test/a.js": true, "test-x.mjs": true, "src/build/compile.test.js": true,
		"test/fixtures/x.ts": false, "test/helpers/h.ts": false, "test/_util.ts": false, "test/a.tsx": false,
		"dist/a.test.js": false, "node_modules/p/test.js": false, "src/math.ts": false,
	} {
		if got := s.IsTestFile(f); got != want {
			t.Errorf("IsTestFile(%q) = %v", f, got)
		}
	}
}

// vitest, jest and node --test collect tests under build/, out/ and coverage/ (only
// node_modules, and compiled copies in dist/, are left out), with or without a config.
func TestJSRunnersExcludeLikeTheRunner(t *testing.T) {
	cases := []struct {
		runner string
		files  []string
	}{
		{"vitest", nil},
		{"vitest", []string{"vitest.config.ts"}},
		{"jest", nil},
		{"jest", []string{"jest.config.js"}},
		{"node", nil},
	}
	configs := map[string]string{
		"vitest.config.ts": "export default { test: { include: ['**/*.test.ts'] } }\n",
		"jest.config.js":   "module.exports = { testMatch: ['**/*.test.ts'] }\n",
	}
	for _, c := range cases {
		repo := tempRepo(t)
		for _, f := range c.files {
			os.WriteFile(filepath.Join(repo.Root, f), []byte(configs[f]), 0o644)
		}
		s, err := NewSuite(repo, config.Suite{Name: "s", Runner: c.runner, Root: "."})
		if err != nil {
			t.Fatal(err)
		}
		if len(c.files) > 0 && len(s.Discovered) == 0 {
			t.Fatalf("%s %v: config not read (%s)", c.runner, c.files, s.DiscoveryWarning)
		}
		for f, want := range map[string]bool{
			"src/build/compile.test.ts": true, "build/a.test.ts": true, "out/a.test.ts": true, "coverage/a.test.ts": true,
			"dist/a.test.ts": false, "node_modules/p/a.test.ts": false,
		} {
			if got := s.IsTestFile(f); got != want {
				t.Errorf("%s %v: IsTestFile(%q) = %v", c.runner, c.files, f, got)
			}
		}
	}
}

func TestEscapeNodeGlob(t *testing.T) {
	cases := map[string]string{
		"a.test.js":                   "a.test.js",
		"app/[slug]/page.test.js":     "app/[[]slug[]]/page.test.js",
		"[id].test.js":                "[[]id[]].test.js",
		"x(y)/r.test.js":              "x[(]y[)]/r.test.js",
		"star*.test.js":               "star[*].test.js",
		"q?.test.js":                  "q[?].test.js",
		"+(p).test.js":                "[+][(]p[)].test.js",
		"@(a).test.js":                "[@][(]a[)].test.js",
		"!bang.test.js":               "!bang.test.js",
		"a{b,c}/q.test.js":            "a[{]b?c[}]/q.test.js",
		"v{1..3}.test.js":             "v[{]1.[.]3[}].test.js",
		"../up/{a}/../[x].test.js":    "../up/[{]a[}]/../[[]x[]].test.js",
		"(group)/[...all]/x.test.js":  "[(]group[)]/[[]...all[]]/x.test.js",
		"{a}/[...all]/x.test.js":      "[{]a[}]/[[].[.][.]all[]]/x.test.js",
		"a,b.test.js":                 "a,b.test.js",
		"comma,{x}/and..dots.test.js": "comma?[{]x[}]/and.[.]dots.test.js",
	}
	if runtime.GOOS != "windows" {
		cases[`back\slash.test.js`] = "back?slash.test.js"
	}
	for in, want := range cases {
		if got := escapeNodeGlob(in); got != want {
			t.Errorf("escapeNodeGlob(%q) = %q, want %q", in, got, want)
		}
	}
}

// Test files whose paths are globs to Node 21+ are escaped there, and only there; the
// version is not asked when no path needs it.
func TestNodeArgsEscapedByVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	t.Setenv("NODE_OPTIONS", "")
	t.Setenv("NODE_V8_COVERAGE", "")
	bin := t.TempDir()
	// A node stand-in: prints $VERSION for --version (and leaves a mark), otherwise
	// writes its arguments to $ARGS.
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then : > \"$ASKED\"; echo \"$VERSION\"; exit 0; fi\nprintf '%s\\n' \"$@\" > \"$ARGS\"\n"
	if err := os.WriteFile(filepath.Join(bin, "node"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, version string, units []string) ([]string, bool) {
		repo := tempRepo(t, units...)
		dir := t.TempDir()
		args, asked := filepath.Join(dir, "args"), filepath.Join(dir, "asked")
		var out bytes.Buffer
		rc := &RunContext{
			Repo: repo, Units: units, Stdout: &out, Stderr: &out,
			Suite: &Suite{Suite: config.Suite{Root: ".", Command: filepath.Join(bin, "node") + " --test",
				Env: map[string]string{"VERSION": version, "ARGS": args, "ASKED": asked}}},
			UnitOf: func(f string) string { return f },
		}
		if _, err := (nodeTest{}).Run(context.Background(), rc); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(args)
		_, err := os.Stat(asked)
		return strings.Fields(string(data)), err == nil
	}
	units := []string{"a.test.js", "app/[slug]/page.test.js"}
	if got, _ := run(t, "v24.1.0", units); !slices.Equal(got, []string{"--test", "a.test.js", "app/[[]slug[]]/page.test.js"}) {
		t.Errorf("node 24: %v", got)
	}
	if got, _ := run(t, "v20.9.0", units); !slices.Equal(got, []string{"--test", "a.test.js", "app/[slug]/page.test.js"}) {
		t.Errorf("node 20: %v", got)
	}
	if got, asked := run(t, "v24.1.0", []string{"a.test.js"}); !slices.Equal(got, []string{"--test", "a.test.js"}) || asked {
		t.Errorf("plain paths: %v (version asked: %v)", got, asked)
	}
}

// nodeTestFile writes a node:test file that leaves <name>.ran in dir and fails when fail
// is set.
func nodeTestFile(t *testing.T, root, rel string, fail bool) {
	t.Helper()
	body := "require('node:test')('t', () => { require('node:fs').writeFileSync(" + jsString(filepath.Join(root, filepath.Base(rel)+".ran")) + ", ''); "
	if fail {
		body += "throw new Error('boom'); "
	}
	body += "});\n"
	p := filepath.Join(root, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsString(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

// With the node on PATH, a selected test file whose path is a glob runs, and only it.
func TestNodeRunsGlobLikePaths(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH")
	}
	t.Setenv("NODE_OPTIONS", "")
	t.Setenv("NODE_V8_COVERAGE", "")
	repo := tempRepo(t)
	nodeTestFile(t, repo.Root, "[id].test.js", false)
	nodeTestFile(t, repo.Root, "i.test.js", false)
	nodeTestFile(t, repo.Root, "app/[slug]/page.test.js", false)
	var out bytes.Buffer
	rc := &RunContext{
		Repo: repo, Units: []string{"[id].test.js", "app/[slug]/page.test.js"}, Stdout: &out, Stderr: &out,
		Suite: &Suite{Suite: config.Suite{Root: ".", Command: "node --test"}}, UnitOf: func(f string) string { return f },
	}
	res, err := (nodeTest{}).Run(context.Background(), rc)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("run: %+v %v\n%s", res, err, out.String())
	}
	for f, want := range map[string]bool{"[id].test.js.ran": true, "page.test.js.ran": true, "i.test.js.ran": false} {
		if _, err := os.Stat(filepath.Join(repo.Root, f)); (err == nil) != want {
			t.Errorf("%s exists: %v, want %v\n%s", f, err == nil, want, out.String())
		}
	}
}

// node --test --test-isolation=none runs every file in one process: a failing run says
// nothing definite about each file, and isolate: true records each file on its own.
func TestNodeIsolationNone(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	t.Setenv("NODE_OPTIONS", "")
	t.Setenv("NODE_V8_COVERAGE", "")
	repo := tempRepo(t)
	nodeTestFile(t, repo.Root, "a.test.js", false)
	nodeTestFile(t, repo.Root, "b.test.js", true)
	probe := exec.Command(node, "--test", "--test-isolation=none", "a.test.js")
	probe.Dir = repo.Root
	if probe.Run() != nil {
		t.Skip("node has no --test-isolation=none")
	}
	units := []string{"a.test.js", "b.test.js"}
	run := func(t *testing.T, isolate bool) *RunResult {
		sh, err := trace.ExtractShims(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		rc := &RunContext{
			Repo: repo, Units: units, Record: true, TraceDir: t.TempDir(), ShimDir: sh.Dir, Stdout: &out, Stderr: &out,
			Suite:  &Suite{Suite: config.Suite{Root: ".", Command: "node --test --test-isolation=none", Isolate: &isolate}},
			UnitOf: func(f string) string { return map[bool]string{true: f}[slices.Contains(units, f)] },
		}
		res, err := (nodeTest{}).Run(context.Background(), rc)
		if err != nil || res.ExitCode == 0 {
			t.Fatalf("run: %+v %v\n%s", res, err, out.String())
		}
		return res
	}
	res := run(t, false)
	for _, u := range units {
		if o := res.Observations[u]; o == nil || o.Status != state.Fail || !o.Inferred {
			t.Errorf("shared process, %s: %+v", u, o)
		}
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "isolate: true") }) {
		t.Errorf("warnings %v", res.Warnings)
	}
	res = run(t, true)
	for u, want := range map[string]string{"a.test.js": state.Pass, "b.test.js": state.Fail} {
		if o := res.Observations[u]; o == nil || o.Status != want || o.Inferred || !o.Files[u] {
			t.Errorf("isolated, %s: %+v", u, o)
		}
	}
}

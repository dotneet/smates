package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/state"
	"github.com/dotneet/smates/internal/testutil"
	"github.com/dotneet/smates/internal/trace"
)

// The recording bunfig puts the shim first, keeps project preloads (absolute) and
// disables coverage thresholds, which per-file coverage runs would otherwise fail.
func TestBunConfigWithShim(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"none", ""},
		{"preload and threshold", "[test]\npreload = [\"./setup.ts\"]\ncoverageThreshold = { lines = 0.9, functions = 0.9 }\n"},
		{"no test section", "smol = true\n"},
		// [test] keys set at the top level move into the [test] table (defined once).
		{"dotted keys", "smol = true\ntest.preload = [\n  \"./setup.ts\", # setup\n]\ntest.coverageThreshold = 0.9\n[install]\nexact = true\n"},
		{"inline table", "test = { preload = [\"./setup.ts\"], coverageThreshold = { lines = 0.9 }, root = \"./src\" }\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.in != "" {
				if err := os.WriteFile(filepath.Join(dir, "bunfig.toml"), []byte(tc.in), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			rc := &RunContext{Suite: &Suite{Suite: config.Suite{Root: "."}}, TraceDir: t.TempDir()}
			rc.Repo = &gitx.Repo{Root: dir}
			path, cleanup, err := bunConfigWithShim(rc, "/shim/node.cjs")
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			data, _ := os.ReadFile(path)
			out := string(data)
			if !strings.Contains(out, `preload = ["/shim/node.cjs"`) || !strings.Contains(out, "coverageThreshold = 0") || strings.Contains(out, "0.9") {
				t.Errorf("bunfig:\n%s", out)
			}
			if strings.Contains(tc.in, "setup.ts") && !strings.Contains(out, filepath.Join(dir, "setup.ts")) {
				t.Errorf("project preload lost:\n%s", out)
			}
			if strings.Count(out, "[test]") != 1 || strings.Contains(out, "test.") || strings.Contains(out, "test =") {
				t.Errorf("[test] keys not in one table:\n%s", out)
			}
			for _, keep := range []string{"smol = true", "exact = true", `root = "./src"`} {
				if strings.Contains(tc.in, keep) && !strings.Contains(out, keep) {
					t.Errorf("%s lost:\n%s", keep, out)
				}
			}
		})
	}
}

// bun itself reads the recording bunfig of a project setting [test] keys at the top
// level: the shim and the project's preload both run.
func TestBunConfigTopLevelTestKeys(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun not on PATH")
	}
	for name, cfg := range map[string]string{
		"dotted": "test.preload = [\"./setup.ts\"]\n",
		"inline": "test = { preload = [\"./setup.ts\"] }\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			testutil.Write(t, dir, "bunfig.toml", cfg)
			testutil.Write(t, dir, "setup.ts", "globalThis.SETUP = 1;\n")
			testutil.Write(t, dir, "shim.cjs", "globalThis.SHIM = 1;\n")
			testutil.Write(t, dir, "x.test.ts", "import { test, expect } from 'bun:test';\ntest('x', () => { expect(globalThis.SETUP).toBe(1); expect(globalThis.SHIM).toBe(1); });\n")
			rc := &RunContext{Suite: &Suite{Suite: config.Suite{Root: "."}}, TraceDir: t.TempDir()}
			rc.Repo = &gitx.Repo{Root: dir}
			path, cleanup, err := bunConfigWithShim(rc, filepath.Join(dir, "shim.cjs"))
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			data, _ := os.ReadFile(path)
			// From another directory: the project's preload path must be absolute.
			cmd := exec.Command(bun, "test", "--config="+path, filepath.Join(dir, "x.test.ts"))
			cmd.Dir = t.TempDir()
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%v\n%s\nbunfig:\n%s", err, out, data)
			}
		})
	}
}

// With bun itself: a self-contained test and one whose import fails write no lcov
// report, and are still recorded (pass and fail); one importing a source file depends
// on it.
func TestBunRecordWithoutCoverage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix paths")
	}
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not on PATH")
	}
	repo := tempRepo(t)
	write := func(rel, content string) { testutil.Write(t, repo.Root, rel, content) }
	write("package.json", `{"name": "x", "private": true}`)
	write("ok.test.ts", "import { test, expect } from \"bun:test\";\ntest(\"ok\", () => { expect(1).toBe(1); });\n")
	write("bad.test.ts", "import { test } from \"bun:test\";\nimport { x } from \"./missing.ts\";\ntest(\"bad\", () => { x(); });\n")
	write("lib.ts", "export function one() {\n  return 1;\n}\n")
	write("uses.test.ts", "import { test, expect } from \"bun:test\";\nimport { one } from \"./lib.ts\";\ntest(\"uses\", () => { expect(one()).toBe(1); });\n")
	shimDir := t.TempDir()
	if _, err := trace.ExtractShims(shimDir); err != nil {
		t.Fatal(err)
	}
	s := &Suite{Suite: config.Suite{Name: "bun", Root: ".", Runner: "bun", Command: "bun test"}, Adapter: bun{}}
	s.Tests = []string{"**/*.test.ts"}
	var out bytes.Buffer
	rc := &RunContext{
		Repo: repo, Suite: s, Units: []string{"bad.test.ts", "ok.test.ts", "uses.test.ts"}, Record: true,
		TraceDir: t.TempDir(), ShimDir: shimDir, Stdout: &out, Stderr: &out,
		IsTest: s.IsTestFile,
		UnitOf: func(f string) string {
			if s.IsTestFile(f) {
				return f
			}
			return ""
		},
		Known: func(u string) []string {
			if u == "uses.test.ts" {
				return []string{u, "lib.ts"}
			}
			return []string{u}
		},
	}
	res, err := bun{}.Run(context.Background(), rc)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for u, want := range map[string]string{"ok.test.ts": state.Pass, "bad.test.ts": state.Fail, "uses.test.ts": state.Pass} {
		if o := res.Observations[u]; o == nil || o.Status != want || o.Inferred || !o.Files[u] {
			t.Errorf("%s: %+v, want %s\n%s", u, o, want, out.String())
		}
	}
	if o := res.Observations["uses.test.ts"]; o != nil && !o.Files["lib.ts"] {
		t.Errorf("uses: %v", o.Files)
	}
}

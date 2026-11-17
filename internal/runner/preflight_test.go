package runner

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
)

func TestJSLauncher(t *testing.T) {
	cases := []struct{ cmd, launcher, bin string }{
		{"npx --no-install vitest run", "npx", "vitest"},
		{"CI=1 npx jest", "npx", "jest"},
		{"bunx vitest run", "bunx", "vitest"},
		{"bun x --bun vitest", "bun x", "vitest"},
		{"pnpm exec vitest run", "pnpm exec", "vitest"},
		{"npx -p vitest@1 vitest", "npx", ""},
		{"./node_modules/.bin/vitest run", "", ""},
		{"bun test", "", ""},
		{"go test", "", ""},
	}
	for _, c := range cases {
		w, _ := SplitWords(c.cmd)
		if l, b := JSLauncher(w); l != c.launcher || b != c.bin {
			t.Errorf("%q: %q %q, want %q %q", c.cmd, l, b, c.launcher, c.bin)
		}
	}
}

func TestPreflight(t *testing.T) {
	cases := []struct {
		name    string
		files   []string
		root    string
		command string
		err     string // substring; "" = no error
	}{
		{"installed bin", []string{"node_modules/.bin/vitest", "pkg/package.json"}, "pkg", "npx --no-install vitest run", ""},
		{"npx would download", []string{"package-lock.json"}, ".", "npx vitest run", "smates does not let npx download it"},
		{"bunx would download", []string{"bun.lock"}, ".", "bunx vitest run", "run `bun install`"},
		{"pnpm exec never downloads", []string{"pnpm-lock.yaml"}, ".", "pnpm exec jest", ""},
		{"tool on PATH", nil, ".", "npx sh -c true", ""},
		{"explicit version", []string{"node_modules/.bin/vitest"}, ".", "npx vitest@2 run", "download vitest@2"},
		{"package flag", nil, ".", "npx -p jest jest", "download a package"},
		{"direct command", nil, ".", "go test", ""},
		{"scoped package bin", []string{"node_modules/.bin/cli"}, ".", "npx @acme/cli", ""},
	}
	for _, c := range cases {
		repo := tempRepo(t, c.files...)
		s := &Suite{Suite: config.Suite{Name: "s", Root: c.root, Command: c.command}}
		err := Preflight(repo.Root, s)
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: %v, want %q", c.name, err, c.err)
		}
	}
	// The tool is looked up on the PATH the suite's env: sets, which the launcher gets.
	if runtime.GOOS != "windows" {
		repo := tempRepo(t, "tools/smates-only-tool", "package-lock.json")
		tools := filepath.Join(repo.Root, "tools")
		for _, c := range []struct {
			name string
			path string // the suite's PATH ("" = none)
			want bool   // a hard problem
		}{
			{"on the suite's PATH", tools + string(os.PathListSeparator) + os.Getenv("PATH"), false},
			{"not on smates' PATH, no suite PATH", "", true},
			{"suite PATH without it", os.Getenv("PATH"), true},
		} {
			s := &Suite{Suite: config.Suite{Name: "s", Root: ".", Command: "npx smates-only-tool"}}
			if c.path != "" {
				s.Env = map[string]string{"PATH": c.path}
			}
			if hard, err := LauncherProblem(repo.Root, s); hard != c.want {
				t.Errorf("%s: hard %v (%v), want %v", c.name, hard, err, c.want)
			}
		}
	}
	repo := tempRepo(t, "pkg/node_modules/.bin/x")
	if !HasNodeModules(repo.Root, "pkg") || HasNodeModules(tempRepo(t, "pkg/package.json").Root, "pkg") {
		t.Error("HasNodeModules")
	}
	if !HasNodeModules(tempRepo(t, ".pnp.cjs", "pkg/package.json").Root, "pkg") {
		t.Error("Yarn PnP counts as installed")
	}
}

func TestLauncherProblem(t *testing.T) {
	cases := []struct {
		name, command string
		hard, problem bool
	}{
		{"pnpm exec missing tool: advisory", "pnpm exec jest", false, true},
		{"pnpm exec tool on PATH", "pnpm exec sh -c true", false, false},
		{"pnpm exec without a tool", "pnpm exec", false, false},
		{"npx missing tool: hard", "npx jest", true, true},
		{"bun x missing tool: hard", "bun x vitest", true, true},
	}
	for _, c := range cases {
		repo := tempRepo(t, "pnpm-lock.yaml")
		s := &Suite{Suite: config.Suite{Name: "s", Root: ".", Command: c.command}}
		hard, err := LauncherProblem(repo.Root, s)
		if hard != c.hard || (err != nil) != c.problem {
			t.Errorf("%s: hard %t, err %v", c.name, hard, err)
		}
		if perr := Preflight(repo.Root, s); (perr != nil) != c.hard {
			t.Errorf("%s: Preflight %v", c.name, perr)
		}
	}
}

package runner

import (
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
		{"pnpm exec without install", []string{"pnpm-lock.yaml"}, ".", "pnpm exec jest", "jest is not installed"},
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
	repo := tempRepo(t, "pkg/node_modules/.bin/x")
	if !HasNodeModules(repo.Root, "pkg") || HasNodeModules(tempRepo(t, "pkg/package.json").Root, "pkg") {
		t.Error("HasNodeModules")
	}
}

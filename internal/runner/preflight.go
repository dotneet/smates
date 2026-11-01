package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Preflight checks, before a suite runs, that its command will not fail confusingly or
// download code: a tool launched through npx or bunx (`bun x`) must be installed in a
// node_modules/.bin between the suite directory and the repository root, because both
// would otherwise fetch the package from the registry. A command launched through
// `pnpm exec` gets the same check (it fails without node_modules, with a less clear
// message). It returns nil for every other command.
func Preflight(repoRoot string, s *Suite) error {
	words, err := SplitWords(s.Command)
	if err != nil {
		return nil // reported when the command line is built
	}
	launcher, bin := JSLauncher(words)
	if launcher == "" {
		return nil
	}
	dir := filepath.Join(repoRoot, filepath.FromSlash(s.Root))
	name := bin
	if name == "" || strings.Contains(strings.TrimPrefix(name, "@"), "@") {
		return fmt.Errorf("command %q would let %s download %s from the registry; install it as a dependency and run it from node_modules/.bin (or set `command:` in smates.yml)", s.Command, launcher, orUnknown(bin))
	}
	if i := strings.LastIndex(name, "/"); i >= 0 { // @scope/pkg: the bin is usually the package name
		name = name[i+1:]
	}
	if findBin(repoRoot, dir, name) != "" {
		return nil
	}
	var install string
	switch packageManager(repoRoot, dir) {
	case "pnpm":
		install = "run `pnpm install`"
	case "bun":
		install = "run `bun install`"
	case "yarn":
		install = "run `yarn install`"
	default:
		install = "run `npm ci` (or `npm install`)"
	}
	why := "is not installed"
	if launcher != "pnpm exec" {
		why += fmt.Sprintf(", and smates does not let %s download it implicitly", launcher)
	}
	return fmt.Errorf("%s %s (no node_modules/.bin/%s from %s up to the repository root): %s first", name, why, name, orDot(s.Root), install)
}

// JSLauncher reports whether argv (a command line, optionally led by VAR=value
// assignments) runs a JS tool through npx, bunx, `bun x` or `pnpm exec`, and which tool.
// bin is "" when it cannot be told (e.g. npx --package).
func JSLauncher(words []string) (launcher, bin string) {
	for len(words) > 0 && isAssignment(words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return "", ""
	}
	var rest []string
	switch base := filepath.Base(words[0]); {
	case base == "npx" || base == "bunx":
		launcher, rest = base, words[1:]
	case base == "bun" && len(words) > 1 && words[1] == "x":
		launcher, rest = "bun x", words[2:]
	case base == "pnpm" && len(words) > 1 && words[1] == "exec":
		launcher, rest = "pnpm exec", words[2:]
	default:
		return "", ""
	}
	for _, w := range rest {
		switch {
		case w == "-p" || w == "--package" || strings.HasPrefix(w, "--package="):
			return launcher, ""
		case w == "--":
			continue
		case strings.HasPrefix(w, "-"):
			continue
		default:
			return launcher, w
		}
	}
	return launcher, ""
}

func isAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	if i <= 0 {
		return false
	}
	for j, r := range w[:i] {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || j > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// HasNodeModules reports whether a node_modules directory exists between the suite
// directory and the repository root (the dependencies were installed).
func HasNodeModules(repoRoot, root string) bool {
	d := filepath.Join(repoRoot, filepath.FromSlash(root))
	for {
		if fi, err := os.Stat(filepath.Join(d, "node_modules")); err == nil && fi.IsDir() {
			return true
		}
		if d == repoRoot || len(d) <= len(repoRoot) {
			return false
		}
		d = filepath.Dir(d)
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "a package"
	}
	return s
}

func orDot(s string) string {
	if s == "" {
		return "."
	}
	return s
}

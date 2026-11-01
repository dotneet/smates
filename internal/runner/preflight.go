package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Preflight checks, before a suite runs, that its command will not download code: a
// tool launched through npx or bunx (`bun x`) must be installed in a node_modules/.bin
// between the suite directory and the repository root (or be on PATH), because both
// would otherwise fetch the package from the registry. It returns nil for every other
// command; `pnpm exec`, which never downloads, is only reported by LauncherProblem.
func Preflight(repoRoot string, s *Suite) error {
	if hard, err := LauncherProblem(repoRoot, s); hard {
		return err
	}
	return nil
}

// LauncherProblem reports a suite command that launches a JS tool which is not
// installed: hard is set when the launcher (npx, bunx, bun x) would download it, and
// the run must not start; for `pnpm exec` the error is advisory (pnpm fails on its own).
func LauncherProblem(repoRoot string, s *Suite) (hard bool, err error) {
	words, werr := SplitWords(s.Command)
	if werr != nil {
		return false, nil // reported when the command line is built
	}
	launcher, bin := JSLauncher(words)
	if launcher == "" {
		return false, nil
	}
	downloads := launcher != "pnpm exec"
	dir := filepath.Join(repoRoot, filepath.FromSlash(s.Root))
	name := bin
	if name == "" || strings.Contains(strings.TrimPrefix(name, "@"), "@") {
		if !downloads {
			return false, nil
		}
		return true, fmt.Errorf("command %q would let %s download %s from the registry; install it as a dependency and run it from node_modules/.bin (or set `command:` in smates.yml)", s.Command, launcher, orUnknown(bin))
	}
	if i := strings.LastIndex(name, "/"); i >= 0 { // @scope/pkg: the bin is usually the package name
		name = name[i+1:]
	}
	if findBin(repoRoot, dir, name) != "" {
		return false, nil
	}
	if _, lerr := exec.LookPath(name); lerr == nil { // node, a globally installed tool
		return false, nil
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
	if downloads {
		why += fmt.Sprintf(", and smates does not let %s download it implicitly", launcher)
	}
	return downloads, fmt.Errorf("%s %s (no node_modules/.bin/%s from %s up to the repository root, nor on PATH): %s first", name, why, name, orDot(s.Root), install)
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

// HasNodeModules reports whether the dependencies were installed: a node_modules
// directory, or a Yarn Plug'n'Play loader (.pnp.cjs/.pnp.js), between the suite
// directory and the repository root.
func HasNodeModules(repoRoot, root string) bool {
	d := filepath.Join(repoRoot, filepath.FromSlash(root))
	for {
		if fi, err := os.Stat(filepath.Join(d, "node_modules")); err == nil && fi.IsDir() {
			return true
		}
		for _, pnp := range []string{".pnp.cjs", ".pnp.js"} { // Yarn Plug'n'Play: no node_modules
			if _, err := os.Stat(filepath.Join(d, pnp)); err == nil {
				return true
			}
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

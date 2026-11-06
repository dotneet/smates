package runner

import (
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/dotneet/smates/internal/static/js"
)

// ConfigLoader is implemented by adapters whose runner configuration is code that
// loads other files by name: setup files, plugins, alias targets. No test imports
// them, so no static closure reaches them, and what the runner's own process runs
// from them may not be traced at all (Vite bundles the configuration before running
// it).
type ConfigLoader interface {
	// RunnerConfig describes the suite's runner configuration among src.Files.
	RunnerConfig(s *Suite, src Sources) RunnerConfig
}

// RunnerConfig is what selection must know about a suite's runner configuration.
type RunnerConfig struct {
	// Files are the configuration files, the repository files they name by a string
	// literal (setupFiles: ["./test/setup.ts"]) and the static import closure of both,
	// sorted: the runner loads them without a test importing them.
	Files []string
	// Browser is set when a configuration may run tests in a browser (Vitest's browser
	// mode, Storybook's Vitest plugin), where no trace sees what they load.
	Browser bool
}

// ConfigOf returns the runner configuration of suite s (zero for adapters that do not
// implement ConfigLoader).
func ConfigOf(s *Suite, src Sources) RunnerConfig {
	cl, ok := s.Adapter.(ConfigLoader)
	if !ok {
		return RunnerConfig{}
	}
	return cl.RunnerConfig(s, src)
}

// vitestConfigFile matches the base names of Vitest and Vite configuration files,
// including the per-project ones (vitest.browser.config.ts, vite.config.e2e.mts) and
// workspace files.
var vitestConfigFile = regexp.MustCompile(`^(?:vitest|vite)(?:\.[\w-]+)?\.config(?:\.[\w-]+)?\.[cm]?[jt]s$|^vitest\.(?:workspace|projects)\.(?:[cm]?[jt]s|json)$`)

// jestConfigFile matches the base names of Jest configuration files.
var jestConfigFile = regexp.MustCompile(`^jest\.config(?:\.[\w-]+)?\.(?:[cm]?[jt]s|json)$`)

// RunnerConfig implements ConfigLoader.
func (vitest) RunnerConfig(s *Suite, src Sources) RunnerConfig {
	return scanRunnerConfig(s, src, vitestConfigFile, false)
}

// RunnerConfig implements ConfigLoader.
func (jest) RunnerConfig(s *Suite, src Sources) RunnerConfig {
	return scanRunnerConfig(s, src, jestConfigFile, true)
}

// scanRunnerConfig finds the configuration files of suite s: the suite command's
// --config, the files discovery read (Suite.ConfigFiles), and the files whose base name
// matches name at or below the suite root or in a directory above it. Every string
// literal in them, and in what they import, that names a repository file (relative to
// the file's directory, after a leading <rootDir>/ with rootDir) adds that file and its
// closure, until nothing new is found.
func scanRunnerConfig(s *Suite, src Sources, name *regexp.Regexp, rootDir bool) RunnerConfig {
	root := path.Clean(s.Root)
	var configs []string
	if c, ok := commandFlag(s, "--config", "-c"); ok {
		if f, inRepo := repoRel(root, c); inRepo && c != "" {
			configs = append(configs, f)
		}
	}
	configs = append(configs, s.ConfigFiles...)
	for _, f := range src.Files {
		if !hasNodeModules(f) && name.MatchString(path.Base(f)) && (underRoot(root, f) || aboveRoot(root, path.Dir(f))) {
			configs = append(configs, f)
		}
	}
	slices.Sort(configs)
	configs = slices.Compact(configs)
	var rc RunnerConfig
	for _, f := range configs {
		if mentionsBrowser(src.Read(f)) {
			rc.Browser = true
		}
	}
	if len(configs) == 0 {
		return rc
	}
	has := func(p string) bool {
		_, found := slices.BinarySearch(src.Files, p)
		return found
	}
	seen := map[string]bool{}
	scanned := map[string]bool{}
	starts := configs
	for len(starts) > 0 {
		var next []string
		for _, f := range src.Closure(starts...) {
			seen[f] = true
			if scanned[f] || !isJSSource(f) {
				continue
			}
			scanned[f] = true
			for _, lit := range js.StringLiterals(src.Read(f)) {
				if p, ok := literalFile(path.Dir(f), lit, rootDir, has); ok && !seen[p] {
					seen[p] = true
					next = append(next, p)
				}
			}
		}
		starts = next
	}
	for f := range seen {
		rc.Files = append(rc.Files, f)
	}
	slices.Sort(rc.Files)
	return rc
}

// literalFile returns the repository file a string literal of a file in dir names, if
// any: the path relative to dir, as is or with a JS/TS extension added. With rootDir a
// leading <rootDir>/ (Jest) is relative to dir too.
func literalFile(dir, lit string, rootDir bool, has func(string) bool) (string, bool) {
	lit = strings.TrimSpace(lit)
	if rootDir {
		lit = strings.TrimPrefix(lit, "<rootDir>/")
	}
	if lit == "" || strings.ContainsAny(lit, "*?{}\n") || strings.Contains(lit, "://") {
		return "", false
	}
	p, ok := repoRel(dir, lit)
	if !ok || p == "." {
		return "", false
	}
	if has(p) {
		return p, true
	}
	for _, ext := range []string{".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs"} {
		if has(p + ext) {
			return p + ext, true
		}
	}
	return "", false
}

// isJSSource reports whether a path is a JavaScript or TypeScript source file.
func isJSSource(f string) bool {
	switch path.Ext(f) {
	case ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts":
		return true
	}
	return false
}

// browserRe matches what makes a Vitest configuration run tests in a browser: a
// browser option, the browser provider packages and Storybook's Vitest plugin.
var browserRe = regexp.MustCompile(`\bbrowser\s*:|@vitest/browser|@storybook/addon-vitest|@storybook/experimental-addon-test|\bstorybookTest\s*\(`)

// browserOffRe matches a browser option that is turned off outright.
var browserOffRe = regexp.MustCompile(`^\bbrowser\s*:\s*false\b`)

// mentionsBrowser reports whether configuration source may run tests in a browser
// (see browserRe); `browser: false` does not count.
func mentionsBrowser(b []byte) bool {
	for _, m := range browserRe.FindAllIndex(b, -1) {
		if !browserOffRe.Match(b[m[0]:]) {
			return true
		}
	}
	return false
}

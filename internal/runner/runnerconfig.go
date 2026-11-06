package runner

import (
	"encoding/json"
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
	// Unsure says why the files the runner loads for every test may be more than
	// Files ("" when they are not): a configuration smates cannot read (a JSON file
	// that does not parse, a bunfig preload or --preload it cannot read).
	Unsure string
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
// including the per-project ones (vitest.e2e.config.ts, vite.config.e2e.mts) and
// workspace files.
var vitestConfigFile = regexp.MustCompile(`^(?:vitest|vite)(?:\.[\w-]+)?\.config(?:\.[\w-]+)?\.[cm]?[jt]s$|^vitest\.(?:workspace|projects)\.(?:[cm]?[jt]s|json)$`)

// jestConfigFile matches the base names of Jest configuration files.
var jestConfigFile = regexp.MustCompile(`^jest\.config(?:\.[\w-]+)?\.(?:[cm]?[jt]s|json)$`)

// RunnerConfig implements ConfigLoader.
func (vitest) RunnerConfig(s *Suite, src Sources) RunnerConfig {
	return scanRunnerConfig(s, src, vitestConfigFile, false, "")
}

// RunnerConfig implements ConfigLoader. Jest also reads its configuration from the
// "jest" key of the package.json at the suite root.
func (jest) RunnerConfig(s *Suite, src Sources) RunnerConfig {
	return scanRunnerConfig(s, src, jestConfigFile, true, "jest")
}

// RunnerConfig implements ConfigLoader: bun test loads the preload files of
// bunfig.toml ([test] preload, in the suite root or the file --config names) and of
// --preload (-r) in the command, and what they import, before every test file.
func (bun) RunnerConfig(s *Suite, src Sources) RunnerConfig {
	root := path.Clean(s.Root)
	var rc RunnerConfig
	has := func(p string) bool {
		_, found := slices.BinarySearch(src.Files, p)
		return found
	}
	cfg := path.Join(root, "bunfig.toml")
	if c, ok := commandFlag(s, "--config", "-c"); ok {
		f, inRepo := repoRel(root, c)
		switch {
		case strings.ContainsAny(c, "$`") || c == "":
			rc.Unsure = "its --config " + c + " is not a path smates can read"
		case inRepo:
			cfg = f
		default:
			cfg = ""
		}
	}
	var starts []string
	if cfg != "" && has(cfg) {
		starts = append(starts, cfg)
		paths, ok := bunfigPreloads(string(src.Read(cfg)))
		if !ok {
			rc.Unsure = cfg + " has a preload smates cannot read"
		}
		for _, p := range paths {
			if f, ok := literalFile(path.Dir(cfg), p, false, has); ok {
				starts = append(starts, f)
			}
		}
	}
	for _, p := range append(commandFlags(s, "--preload"), commandFlags(s, "-r")...) {
		if strings.ContainsAny(p, "$`") {
			rc.Unsure = "its --preload " + p + " is not a path smates can read"
			continue
		}
		if f, ok := literalFile(root, p, false, has); ok {
			starts = append(starts, f)
		}
	}
	if len(starts) == 0 {
		return rc
	}
	rc.Files = slices.Clone(src.Closure(starts...))
	slices.Sort(rc.Files)
	rc.Files = slices.Compact(rc.Files)
	return rc
}

// bunfigPreloads returns the preload paths of a bunfig.toml: the [test] table's
// preload, a top-level preload, test.preload as a dotted key, or preload in an inline
// test table. ok is false when a preload value holds no string smates can read.
func bunfigPreloads(text string) (paths []string, ok bool) {
	ok = true
	add := func(value string) {
		got := tomlStrings(value)
		v := value
		if i := strings.IndexByte(v, '#'); i >= 0 {
			v = v[:i] // no string in it: a comment
		}
		if len(got) == 0 && strings.Trim(strings.Join(strings.Fields(v), ""), "[]") != "" {
			ok = false // not a string or an array of strings
		}
		paths = append(paths, got...)
	}
	text = strings.TrimPrefix(text, "\ufeff")
	var table []string // the current table's key (nil: the top level)
	for _, st := range tomlStatements(text) {
		if st.header {
			table = st.key
			if st.array {
				table = []string{"\x00"} // an array of tables: no preload of bun's
			}
			continue
		}
		if st.key == nil {
			continue
		}
		key := append(slices.Clone(table), st.key...)
		value := text[st.value:st.end]
		switch {
		case slices.Equal(key, []string{"preload"}), slices.Equal(key, []string{"test", "preload"}):
			add(value)
		case slices.Equal(key, []string{"test"}):
			for _, kv := range tomlInlineTable(value) {
				k, v, _ := strings.Cut(kv, "=")
				if strings.Trim(strings.TrimSpace(k), `"'`) == "preload" {
					add(v)
				}
			}
		}
	}
	return paths, ok
}

// scanRunnerConfig finds the configuration files of suite s: the suite command's
// --config, the files discovery read (Suite.ConfigFiles), and the files whose base name
// matches name at or below the suite root or in a directory above it. Every string
// literal in them, and in what they import, that names a repository file (relative to
// the file's directory, after a leading <rootDir>/ with rootDir) adds that file and its
// closure, until nothing new is found.
//
// A JSON configuration (jest.config.json) is read for its string values too, and so is
// the pkgKey key of the package.json at the suite root ("" for none); one that does not
// parse makes the result unsure.
func scanRunnerConfig(s *Suite, src Sources, name *regexp.Regexp, rootDir bool, pkgKey string) RunnerConfig {
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
	has := func(p string) bool {
		_, found := slices.BinarySearch(src.Files, p)
		return found
	}
	// The package.json key is read for its literals; the file itself is no
	// configuration file of the runner's.
	var pkgLits []string
	if pkg := path.Join(root, "package.json"); pkgKey != "" && has(pkg) {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(src.Read(pkg), &doc); err != nil {
			rc.Unsure = pkg + " does not parse"
		} else if raw, ok := doc[pkgKey]; ok {
			pkgLits, _ = jsonStrings(raw)
		}
	}
	if len(configs) == 0 && len(pkgLits) == 0 {
		return rc
	}
	seen := map[string]bool{}
	scanned := map[string]bool{}
	literals := func(f string) []string {
		switch {
		case isJSSource(f):
			return js.StringLiterals(src.Read(f))
		case path.Ext(f) == ".json" && path.Base(f) != "package.json" && slices.Contains(configs, f):
			lits, ok := jsonStrings(src.Read(f))
			if !ok {
				rc.Unsure = f + " does not parse"
			}
			return lits
		}
		return nil
	}
	starts := configs
	for _, lit := range pkgLits {
		if p, ok := literalFile(root, lit, rootDir, has); ok && !seen[p] {
			seen[p] = true
			starts = append(starts, p)
		}
	}
	for len(starts) > 0 {
		var next []string
		for _, f := range src.Closure(starts...) {
			seen[f] = true
			if scanned[f] {
				continue
			}
			scanned[f] = true
			for _, lit := range literals(f) {
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

// jsonStrings returns every string value of a JSON document (in objects and arrays,
// not keys); ok is false when it does not parse.
func jsonStrings(data []byte) (out []string, ok bool) {
	var v any
	if json.Unmarshal(data, &v) != nil {
		return nil, false
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return out, true
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

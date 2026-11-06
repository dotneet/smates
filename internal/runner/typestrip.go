package runner

import (
	"bytes"
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dotneet/smates/internal/static/js"
)

// TypeEraser is implemented by adapters whose runner may execute TypeScript with its
// types erased and never checked (vitest, bun). For such a suite a change to a module
// its tests reach only through type-only imports (import type, see
// static.Graph.RuntimeClosure) cannot change their outcome, so selection leaves those
// imports out of their static closure.
type TypeEraser interface {
	// ErasesTypes reports whether the suite's tests are known to run with types
	// erased and unchecked. It must answer false whenever it cannot tell.
	ErasesTypes(s *Suite, src Sources) bool
}

// Sources is the view of the repository a TypeEraser decides from.
type Sources struct {
	// Files are the repo-relative paths of the working tree, sorted.
	Files []string
	// Read returns a file's content (nil if unreadable).
	Read func(string) []byte
	// Closure returns the static import closure of files, including them.
	Closure func(...string) []string
	// Resolve returns the repository files a module specifier imported from a file
	// resolves to (nil: not known).
	Resolve func(from, spec string) []string
}

// ErasesTypes reports whether s's tests are known to run with TypeScript types erased
// and unchecked (see TypeEraser); false for runners that may check types.
func ErasesTypes(s *Suite, src Sources) bool {
	te, ok := s.Adapter.(TypeEraser)
	return ok && te.ErasesTypes(s, src)
}

// typeCheckRe matches what may make a run check types, or compile them differently
// than by erasing type-only imports: vitest's typecheck mode, a tsc / vue-tsc / tsd /
// ts-node invocation, and the compiler options and transform settings that keep
// imports of types as side-effect imports. It is matched against commands, the suite's
// environment and runner configuration files (comments included): any mention keeps
// type-only imports, which costs selection precision, never correctness.
var typeCheckRe = regexp.MustCompile(`(?i)type-?check|\btsc\b|\btsd\b|ts-node|verbatimModuleSyntax|preserveValueImports|importsNotUsedAsValues|onlyRemoveTypeImports|tsconfigRaw|STORYBOOK_COMPONENT_PATHS`)

// runsDirectly reports whether the suite's command runs the runner itself (bin(words,
// i) matches at some word) as one plain command: not a package script or a shell
// pipeline, which may check types before or after the tests, nor with anything in the
// command, the arguments passed through or the suite's environment (names and values)
// that typeCheckRe matches. A --tsconfig option (bun's --tsconfig-override) makes the
// compiler options unknown too.
func runsDirectly(s *Suite, bin func(words []string, i int) bool) bool {
	cmd := s.Command
	if strings.ContainsAny(cmd, ";|&`\n") || strings.Contains(cmd, "$(") {
		return false
	}
	all := append([]string{cmd}, s.ExtraArgs...)
	for k, v := range s.Env {
		all = append(all, k+"="+v)
	}
	for _, a := range all {
		if typeCheckRe.MatchString(a) || strings.Contains(a, "--tsconfig") {
			return false
		}
	}
	words, err := SplitWords(cmd)
	if err != nil {
		return false
	}
	for i := range words {
		if bin(words, i) {
			return true
		}
	}
	return false
}

// runsScript reports whether words[i] may name a package.json script rather than a
// program. After a package manager (npm, yarn, pnpm, bun) the word is the program
// only right after `exec`, `dlx` or `x`, or right after the manager itself when the
// package.json nearest the suite root has no script of that name (yarn vitest, pnpm
// vitest and bun vitest run such a script first); any other word in between (run,
// --filter web, workspace web, -C web, --cwd web) may select a script, possibly of
// another package. Without a package manager, a word after run / run-script is one.
func runsScript(words []string, i int, root string, src Sources) bool {
	for m := 0; m < i; m++ {
		mgr := binName(words[m])
		if !slices.Contains([]string{"npm", "yarn", "pnpm", "bun"}, mgr) {
			continue
		}
		switch between := words[m+1 : i]; {
		case len(between) == 0:
			return mgr == "npm" || hasScript(root, words[i], src)
		case len(between) == 1 && slices.Contains([]string{"exec", "dlx", "x"}, between[0]):
			return false
		default:
			return true
		}
	}
	return i > 0 && (words[i-1] == "run" || words[i-1] == "run-script")
}

// hasScript reports whether the package.json nearest dir (at or above it) declares a
// script named name, or cannot be read as one.
func hasScript(dir, name string, src Sources) bool {
	for d := dir; ; d = path.Dir(d) {
		f := path.Join(d, "package.json")
		if b := src.Read(f); b != nil {
			var pkg struct {
				Scripts map[string]json.RawMessage `json:"scripts"`
			}
			if json.Unmarshal(b, &pkg) != nil {
				return true
			}
			_, ok := pkg.Scripts[name]
			return ok
		}
		if d == "." || d == "/" || d == "" {
			return false
		}
	}
}

// binName returns the program name of a command word: its base name without a
// script extension (./node_modules/.bin/vitest, vitest.mjs -> vitest).
func binName(w string) string {
	b := path.Base(strings.ReplaceAll(w, `\`, "/"))
	for _, ext := range []string{".mjs", ".cjs", ".js", ".cmd", ".exe"} {
		b = strings.TrimSuffix(b, ext)
	}
	return b
}

// underRoot reports whether the repo-relative path f is in the suite root (or is it).
func underRoot(root, f string) bool {
	return root == "." || root == "" || f == root || strings.HasPrefix(f, root+"/")
}

// aboveRoot reports whether the repo-relative directory d is the suite root or one of
// its ancestors.
func aboveRoot(root, d string) bool {
	return d == "." || underRoot(d, root)
}

// ErasesTypes implements TypeEraser. Vitest transforms TypeScript with esbuild (or
// oxc), which erases types without checking them, unless typecheck mode is on (it then
// also runs *.test-d.ts files through tsc). Types are known to be erased when the
// command runs vitest directly (runsDirectly), no vitest or vite configuration file at
// or below the suite root or in a directory above it, nor any file they import, may
// enable typecheck or change how imports of types compile (typeCheckRe), and the suite
// has no type test files (*.test-d.* / *.spec-d.*), which only typecheck mode runs.
// This is deliberately coarse: a mention anywhere keeps type-only imports.
func (vitest) ErasesTypes(s *Suite, src Sources) bool {
	root := path.Clean(s.Root)
	if !runsDirectly(s, func(w []string, i int) bool { return binName(w[i]) == "vitest" && !runsScript(w, i, root, src) }) {
		return false
	}
	var configs []string
	if c, ok := commandFlag(s, "--config", "-c"); ok {
		f, inRepo := repoRel(root, c)
		if !inRepo {
			return false
		}
		configs = append(configs, f)
	}
	names := append(vitestConfigNames(), vitestWorkspaceNames()...)
	for _, e := range jsConfigExts {
		names = append(names, "vitest.projects."+e)
	}
	for _, f := range src.Files {
		if hasNodeModules(f) {
			continue
		}
		base := path.Base(f)
		if underRoot(root, f) && (strings.Contains(base, ".test-d.") || strings.Contains(base, ".spec-d.")) {
			return false
		}
		if slices.Contains(names, base) && (underRoot(root, f) || aboveRoot(root, path.Dir(f))) {
			configs = append(configs, f)
		}
	}
	return !mentionsTypeCheck(src, configs, root)
}

// ErasesTypes implements TypeEraser. bun test transpiles TypeScript itself and never
// checks types; they are known to be erased when the command is `bun test` itself
// (runsDirectly).
func (bun) ErasesTypes(s *Suite, src Sources) bool {
	return runsDirectly(s, func(w []string, i int) bool {
		return binName(w[i]) == "bun" && i+1 < len(w) && w[i+1] == "test"
	})
}

// configRefRe matches what makes a vitest configuration name other configuration
// files by path (a workspace's or a config's projects, a project that extends a shared
// config): those are not imports, so the static closure does not reach them.
var configRefRe = regexp.MustCompile(`\b(?:projects|workspace|extends)\s*:`)

// moduleRe matches the specifier of an import, export from, dynamic import or require.
var moduleRe = regexp.MustCompile(`(?:\bfrom|\bimport\s*\(|\bimport|\brequire\s*\()\s*["']([^"']+)["']`)

// knownConfigModule reports whether a vitest configuration importing module m is
// still fully seen: a relative path (the static closure follows it) or a package
// that does not configure vitest's type checking (vitest and vite themselves, their
// official plugins, Tailwind's Vite plugin, Node's builtins). Any other package may: a
// preset, or a plugin whose config hook returns test settings. Storybook's Vitest
// plugin is known only where storybookKnown says so.
func knownConfigModule(m string) bool {
	switch {
	case strings.HasPrefix(m, "."), strings.HasPrefix(m, "/"), strings.HasPrefix(m, "node:"):
		return true
	case m == "vitest", m == "vite", strings.HasPrefix(m, "vitest/"), strings.HasPrefix(m, "vite/"),
		strings.HasPrefix(m, "@vitest/"), strings.HasPrefix(m, "@vitejs/"), m == "vite-tsconfig-paths",
		m == "@tailwindcss/vite":
		return true
	}
	switch m {
	case "path", "url", "fs", "os", "module", "process", "fs/promises":
		return true
	}
	return false
}

// mentionsTypeCheck reports whether the vitest configuration may check types, or is not
// fully seen: a configuration file, or a source file in its static import closure,
// matches typeCheckRe, names other configuration files by path (configRefRe, except
// the projects of an aggregator vitestAggregator reads in full, which are scanned in
// turn), or imports at run time a package that may configure vitest (see
// knownConfigModule; an import of types only, or of a package the static analysis
// resolves inside the repository, whose files the closure holds, is seen); or a
// configuration file cannot be read. Storybook's Vitest plugin counts as seen in a
// configuration storybookKnown accepts, whose main.* files are scanned in turn.
// cwd is the directory vitest runs in (the suite root).
func mentionsTypeCheck(src Sources, configs []string, cwd string) bool {
	if len(configs) == 0 {
		return false
	}
	// refs holds, by configuration file, how many configRefRe matches are known to
	// name only files that are scanned too.
	refs := map[string]int{}
	storybook := map[string]bool{}
	seen := map[string]bool{}
	// projectFiles are the files aggregators name: a project file is no aggregator.
	projectFiles := map[string]bool{}
	for i := 0; i < len(configs); i++ {
		f := configs[i]
		if seen[f] {
			continue
		}
		seen[f] = true
		b := src.Read(f)
		if b == nil {
			return true
		}
		if base := path.Base(f); strings.HasPrefix(base, "vitest.workspace.") || strings.HasPrefix(base, "vitest.projects.") {
			return true
		}
		if configRefRe.Match(b) {
			projects, n, ok := vitestAggregator(f, b, src)
			if !ok || projectFiles[f] {
				return true
			}
			refs[f] = n
			for _, p := range projects {
				projectFiles[p] = true
			}
			configs = append(configs, projects...)
		}
		if usesStorybookPlugin(b) {
			mains, ok := storybookKnown(f, b, src, cwd)
			if !ok {
				return true
			}
			storybook[f] = true
			configs = append(configs, mains...)
		}
	}
	for _, f := range src.Closure(configs...) {
		switch path.Ext(f) {
		case ".json":
			if typeCheckRe.Match(src.Read(f)) {
				return true
			}
		case ".js", ".mjs", ".cjs", ".ts", ".mts", ".cts":
			b := src.Read(f)
			if typeCheckRe.Match(b) || len(configRefRe.FindAllIndex(b, -1)) > refs[f] {
				return true
			}
			typeOnly := typeOnlyImports(f, b)
			for _, m := range moduleRe.FindAllSubmatch(b, -1) {
				spec := string(m[1])
				switch {
				case knownConfigModule(spec), typeOnly[spec], storybook[f] && storybookModule(spec):
				case src.Resolve != nil && slices.ContainsFunc(src.Resolve(f, spec), func(h string) bool { return packageSource(src, spec, h) }):
				default:
					return true
				}
			}
		}
	}
	return false
}

// packageSource reports whether file h, which the bare specifier spec resolves to, is
// what loading the package runs: a source file (not a declaration file, not a manual
// mock under __mocks__) of a workspace package of that name, the package.json nearest
// h naming it. A tsconfig path or a mock resolves the specifier for the static
// analysis only; at run time the configuration loads the installed package, whose
// files smates never scans.
func packageSource(src Sources, spec, h string) bool {
	if strings.HasSuffix(h, ".d.ts") || strings.HasSuffix(h, ".d.mts") || strings.HasSuffix(h, ".d.cts") ||
		slices.Contains(strings.Split(h, "/"), "__mocks__") {
		return false
	}
	name := spec
	if parts := strings.SplitN(spec, "/", 3); strings.HasPrefix(spec, "@") && len(parts) >= 2 {
		name = parts[0] + "/" + parts[1]
	} else {
		name = parts[0]
	}
	for d := path.Dir(h); ; d = path.Dir(d) {
		if b := src.Read(path.Join(d, "package.json")); b != nil {
			var pkg struct{ Name string }
			return json.Unmarshal(b, &pkg) == nil && pkg.Name == name
		}
		if d == "." || d == "/" {
			return false
		}
	}
}

// typeOnlyImports returns the specifiers source b (of file f) imports only in
// type-only declarations, which never load the module.
func typeOnlyImports(f string, b []byte) map[string]bool {
	out := map[string]bool{}
	for _, imp := range js.Imports(f, b) {
		if v, ok := out[imp.Spec]; ok {
			out[imp.Spec] = v && imp.TypeOnly
		} else {
			out[imp.Spec] = imp.TypeOnly
		}
	}
	return out
}

// vitestAggregator reads the projects of a vitest configuration file f (source b)
// that names other configuration files by path (configRefRe) and returns them, with
// how many configRefRe matches of b they account for. ok is false unless every match
// is accounted for: a single `projects:`, the test.projects of a configuration
// ConfigObject reads (no mergeConfig, no spread, no root), whose entries are paths of
// repository JS/TS files (no glob, no directory, no negation) or inline projects that
// extend nothing but the root configuration (`extends: true`, as is every other
// `extends:`). A `workspace:` is never accounted for.
func vitestAggregator(f string, b []byte, src Sources) (projects []string, n int, ok bool) {
	matches := configRefRe.FindAllIndex(b, -1)
	nProjects := 0
	for _, m := range matches {
		switch key := strings.TrimRight(string(b[m[0]:m[1]]), " \t\r\n:"); key {
		case "projects":
			nProjects++
		case "extends":
			if !extendsTrueRe.Match(b[m[1]:]) {
				return nil, 0, false
			}
		default:
			return nil, 0, false
		}
	}
	if nProjects != 1 {
		return nil, 0, false
	}
	v, found := js.ConfigObject(b)
	obj, isObj := v.(*js.Object)
	if !found || !isObj {
		return nil, 0, false
	}
	if r, known := obj.Get("root"); !known || r != nil {
		return nil, 0, false
	}
	tv, _ := obj.Get("test")
	test, isObj := tv.(*js.Object)
	if !isObj {
		return nil, 0, false
	}
	pv, _ := test.Get("projects")
	list, isList := pv.([]any)
	if !isList {
		return nil, 0, false
	}
	dir := path.Dir(f)
	for _, e := range list {
		switch x := e.(type) {
		case string:
			p, inRepo := repoRel(dir, x)
			if !inRepo || strings.ContainsAny(x, "*?[]{}()!") || !isJSSource(p) {
				return nil, 0, false
			}
			if _, exists := slices.BinarySearch(src.Files, p); !exists {
				return nil, 0, false
			}
			projects = append(projects, p)
		case *js.Object:
			if ext, known := x.Get("extends"); !known || ext != nil && ext != true {
				return nil, 0, false
			}
		default:
			return nil, 0, false
		}
	}
	return projects, len(matches), true
}

// extendsTrueRe matches what follows `extends:` in an inline project that inherits
// the root configuration.
var extendsTrueRe = regexp.MustCompile(`^\s*true\b`)

// usesStorybookPlugin reports whether configuration source b uses Storybook's Vitest
// plugin: imports its package (or the experimental one) or calls storybookTest. A
// main.* naming the addon among its addons does not.
func usesStorybookPlugin(b []byte) bool {
	if storybookCallRe.Match(b) {
		return true
	}
	for _, m := range moduleRe.FindAllSubmatch(b, -1) {
		if spec := string(m[1]); storybookModule(spec) || strings.HasPrefix(spec, "@storybook/experimental-addon-test") {
			return true
		}
	}
	return false
}

var storybookCallRe = regexp.MustCompile(`\bstorybookTest\s*\(`)

// storybookModule reports whether m is the package of Storybook's Vitest plugin.
func storybookModule(m string) bool {
	return m == "@storybook/addon-vitest" || strings.HasPrefix(m, "@storybook/addon-vitest/")
}

// storybookAddons and storybookFrameworks are the Storybook addons and frameworks a
// configuration using Storybook's Vitest plugin may name while types stay erased:
// none sets test options or adds a Vite plugin that reads types (react-docgen, which
// the plugin itself leaves out from Storybook 10 on, or vue-component-meta).
var (
	storybookAddons = []string{"@storybook/addon-vitest", "@storybook/addon-docs", "@storybook/addon-a11y",
		"@storybook/addon-links", "@storybook/addon-themes", "@storybook/addon-onboarding"}
	storybookFrameworks = []string{"@storybook/react-vite", "@storybook/html-vite", "@storybook/web-components-vite"}
)

// storybookKnown reports whether Storybook's Vitest plugin in configuration file f
// (source b) is fully seen, and returns the main.* files of its configuration
// directories, to be scanned like configuration: the plugin loads them and applies
// the Vite settings of main.* and of every addon and framework it names, which are
// strings, not imports. It is when the plugin is @storybook/addon-vitest of
// Storybook 10 or later (installed in a node_modules at or above f), smates can tell
// each call's configuration directory, and the main.* there can be read, sets no
// viteFinal and no typescript options (react-docgen), and names only the addons and
// framework of storybookAddons and storybookFrameworks.
func storybookKnown(f string, b []byte, src Sources, cwd string) ([]string, bool) {
	if bytes.Contains(b, []byte("@storybook/experimental-addon-test")) || storybookMajor(path.Dir(f), src) < 10 {
		return nil, false
	}
	names, imported := storybookNames(b)
	dirs, found, err := storybookDirs(f, path.Dir(f), cwd, b, names, imported)
	if err != nil || !found || len(dirs) == 0 {
		return nil, false
	}
	var mains []string
	for _, d := range dirs {
		if d.unsure != "" {
			return nil, false // which directory Storybook reads is not certain
		}
		main := readStorybookMain(d.dir, src.Read)
		// The source too: a method (async viteFinal(config) {...}) may not read as a key.
		if main.err != nil || mainLoadsRe.Match(main.src) {
			return nil, false
		}
		for _, k := range []string{"viteFinal", "typescript"} {
			if v, known := main.obj.Get(k); !known || v != nil {
				return nil, false
			}
		}
		if !storybookNamesKnown(main.obj, "addons", storybookAddons, true) ||
			!storybookNamesKnown(main.obj, "framework", storybookFrameworks, false) {
			return nil, false
		}
		mains = append(mains, main.file)
	}
	return mains, true
}

// storybookNamesKnown reports whether key of main.* names only packages among
// allowed: a string or {name} object, or (list) a list of them, which may be absent.
func storybookNamesKnown(obj *js.Object, key string, allowed []string, list bool) bool {
	v, known := obj.Get(key)
	if !known {
		return false
	}
	entries := []any{v}
	if list {
		if v == nil {
			return true
		}
		l, ok := v.([]any)
		if !ok {
			return false
		}
		entries = l
	}
	for _, e := range entries {
		if o, ok := e.(*js.Object); ok {
			e, _ = o.Get("name")
		}
		name, ok := e.(string)
		if !ok || !slices.Contains(allowed, name) {
			return false
		}
	}
	return true
}

// storybookMajor returns the major version of the @storybook/addon-vitest installed in
// the node_modules nearest dir (at or above it), 0 if none or unknown.
func storybookMajor(dir string, src Sources) int {
	for d := dir; ; d = path.Dir(d) {
		if b := src.Read(path.Join(d, "node_modules/@storybook/addon-vitest/package.json")); b != nil {
			var pkg struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(b, &pkg) != nil {
				return 0
			}
			major, _, _ := strings.Cut(pkg.Version, ".")
			n, _ := strconv.Atoi(major)
			return n
		}
		if d == "." || d == "/" || d == "" {
			return 0
		}
	}
}

// hasNodeModules reports whether a repo-relative path is inside a node_modules
// directory.
func hasNodeModules(f string) bool {
	return strings.HasPrefix(f, "node_modules/") || strings.Contains(f, "/node_modules/")
}

package runner

import (
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"
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
	// Files are the repo-relative paths of the working tree.
	Files []string
	// Read returns a file's content (nil if unreadable).
	Read func(string) []byte
	// Closure returns the static import closure of files, including them.
	Closure func(...string) []string
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
var typeCheckRe = regexp.MustCompile(`(?i)type-?check|\btsc\b|\btsd\b|ts-node|verbatimModuleSyntax|preserveValueImports|importsNotUsedAsValues|onlyRemoveTypeImports|tsconfigRaw`)

// runsDirectly reports whether the suite's command runs the runner itself (bin(words,
// i) matches at some word) as one plain command: not a package script or a shell
// pipeline, which may check types before or after the tests, nor with anything in the
// command, the arguments passed through or the suite's environment that typeCheckRe
// matches. A --tsconfig option (bun's --tsconfig-override) makes the compiler options
// unknown too.
func runsDirectly(s *Suite, bin func(words []string, i int) bool) bool {
	cmd := s.Command
	if strings.ContainsAny(cmd, ";|&`\n") || strings.Contains(cmd, "$(") {
		return false
	}
	all := append([]string{cmd}, s.ExtraArgs...)
	for _, v := range s.Env {
		all = append(all, v)
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
	return !mentionsTypeCheck(src, configs)
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
// official plugins, Node's builtins). Any other package may: a preset, or a plugin
// whose config hook returns test settings.
func knownConfigModule(m string) bool {
	switch {
	case strings.HasPrefix(m, "."), strings.HasPrefix(m, "/"), strings.HasPrefix(m, "node:"):
		return true
	case m == "vitest", m == "vite", strings.HasPrefix(m, "vitest/"), strings.HasPrefix(m, "vite/"),
		strings.HasPrefix(m, "@vitest/"), strings.HasPrefix(m, "@vitejs/"), m == "vite-tsconfig-paths":
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
// matches typeCheckRe, names other configuration files by path (a workspace file,
// configRefRe), or imports a package that may configure vitest (see
// knownConfigModule); or a configuration file cannot be read.
func mentionsTypeCheck(src Sources, configs []string) bool {
	if len(configs) == 0 {
		return false
	}
	for _, f := range configs {
		if src.Read(f) == nil {
			return true
		}
		if b := path.Base(f); strings.HasPrefix(b, "vitest.workspace.") || strings.HasPrefix(b, "vitest.projects.") {
			return true
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
			if typeCheckRe.Match(b) || configRefRe.Match(b) {
				return true
			}
			for _, m := range moduleRe.FindAllSubmatch(b, -1) {
				if !knownConfigModule(string(m[1])) {
					return true
				}
			}
		}
	}
	return false
}

// hasNodeModules reports whether a repo-relative path is inside a node_modules
// directory.
func hasNodeModules(f string) bool {
	return strings.HasPrefix(f, "node_modules/") || strings.Contains(f, "/node_modules/")
}

package js

import (
	"encoding/json"
	"path"
	"sort"
	"strings"
)

// rawTSConfig is the part of one tsconfig.json/jsconfig.json file the resolver uses,
// before "extends" is applied.
type rawTSConfig struct {
	Extends         json.RawMessage `json:"extends"`
	CompilerOptions struct {
		BaseURL *string             `json:"baseUrl"`
		Paths   map[string][]string `json:"paths"`
		// The options that keep an import whose specifiers are all types as a
		// side-effect import (see typeImportOpts). Raw, so that an unexpected value
		// does not make the whole file unreadable.
		VerbatimModuleSyntax   json.RawMessage `json:"verbatimModuleSyntax"`
		PreserveValueImports   json.RawMessage `json:"preserveValueImports"`
		ImportsNotUsedAsValues json.RawMessage `json:"importsNotUsedAsValues"`
	} `json:"compilerOptions"`
	References []struct {
		Path string `json:"path"`
	} `json:"references"`
}

// tsConfig is a tsconfig with its "extends" chain applied.
type tsConfig struct {
	baseURL  string // repo-relative directory; valid if hasBase
	hasBase  bool
	pathsDir string // directory of the config that defined "paths"
	paths    []pathRule
	// configDir is the directory of the config the chain was loaded for, which
	// ${configDir} (TypeScript 5.5) stands for in "paths" targets and "baseUrl", also
	// when an extended config declares them.
	configDir string
}

// configDirVar is the template TypeScript expands to the using config's directory.
const configDirVar = "${configDir}"

// pathRule is one compiled "paths" entry.
type pathRule struct {
	prefix, suffix string // pattern split at its '*' (prefix is the whole key if !star)
	star           bool
	targets        []string
}

// match reports whether spec matches the rule and returns the text matched by '*'.
func (r pathRule) match(spec string) (string, bool) {
	if !r.star {
		return "", spec == r.prefix
	}
	if len(spec) < len(r.prefix)+len(r.suffix) || !strings.HasPrefix(spec, r.prefix) || !strings.HasSuffix(spec, r.suffix) {
		return "", false
	}
	return spec[len(r.prefix) : len(spec)-len(r.suffix)], true
}

// pathsBase is the directory "paths" targets are relative to: baseUrl if set, else the
// directory of the config that declared "paths" (TypeScript 4.1+).
func (c *tsConfig) pathsBase() string {
	if c.hasBase {
		return c.baseURL
	}
	return c.pathsDir
}

// target returns the directory a "paths" target is relative to and the target relative
// to it: the paths base, or configDir for a target starting with ${configDir}.
func (c *tsConfig) target(t string) (base, rel string) {
	if rest, ok := strings.CutPrefix(t, configDirVar); ok {
		return c.configDir, "./" + strings.TrimLeft(rest, "/")
	}
	return c.pathsBase(), t
}

// resolve resolves a bare specifier through "paths" and then "baseUrl" and reports
// whether anything was found. Rules are tried most specific first; the first rule whose
// targets resolve wins, as in TypeScript.
func (c *tsConfig) resolve(a *Analyzer, spec string, add func(string)) bool {
	for _, r := range c.paths {
		star, ok := r.match(spec)
		if !ok {
			continue
		}
		found := false
		for _, t := range r.targets {
			base, t := c.target(t)
			if r.star {
				t = strings.Replace(t, "*", star, 1)
			}
			if a.resolvePath(path.Join(base, t), add) {
				found = true
			}
		}
		if found {
			return true
		}
	}
	return c.hasBase && a.resolvePath(path.Join(c.baseURL, spec), add)
}

// configDirs returns the directories holding a tsconfig.json or jsconfig.json at or
// above dir, nearest first.
func (a *Analyzer) configDirs(dir string) []string {
	return a.tsDirs.get(dir, func() []string {
		var out []string
		if a.h.Has(path.Join(dir, "tsconfig.json")) || a.h.Has(path.Join(dir, "jsconfig.json")) {
			out = append(out, dir)
		}
		if dir != "." && dir != "" && dir != "/" {
			out = append(out, a.configDirs(parentDir(dir))...)
		}
		return out
	})
}

// dirConfigs returns the configs that may govern files under a config directory:
// tsconfig.json, jsconfig.json, sibling tsconfig.*.json (e.g. Vite's tsconfig.app.json)
// and project references, transitively.
func (a *Analyzer) dirConfigs(dir string) []*tsConfig {
	return a.tsConfigs.get(dir, func() []*tsConfig {
		var out []*tsConfig
		for _, p := range a.dirConfigFiles(dir) {
			if c := a.tsconfig(p); c != nil {
				out = append(out, c)
			}
		}
		return out
	})
}

// dirConfigFiles returns the files of dirConfigs(dir), readable or not.
func (a *Analyzer) dirConfigFiles(dir string) []string {
	return a.tsPaths.get(dir, func() []string {
		var paths []string
		seen := map[string]bool{}
		var visit func(p string)
		visit = func(p string) {
			if seen[p] || !a.h.Has(p) {
				return
			}
			seen[p] = true
			paths = append(paths, p)
			raw := a.rawConfig(p)
			if raw == nil {
				return
			}
			for _, r := range raw.References {
				rp := path.Join(path.Dir(p), r.Path)
				if !strings.HasSuffix(rp, ".json") {
					rp = path.Join(rp, "tsconfig.json")
				}
				visit(rp)
			}
		}
		visit(path.Join(dir, "tsconfig.json"))
		visit(path.Join(dir, "jsconfig.json"))
		prefix := path.Join(dir, "tsconfig.")
		if dir == "." {
			prefix = "tsconfig."
		}
		for _, f := range a.under(prefix) {
			if rest := f[len(prefix):]; strings.HasSuffix(rest, ".json") && !strings.Contains(rest, "/") {
				visit(f)
			}
		}
		return paths
	})
}

// tsconfig returns the config at p with its extends chain applied (nil if unreadable).
func (a *Analyzer) tsconfig(p string) *tsConfig {
	return a.tsByPath.get(p, func() *tsConfig {
		c, ok := a.loadConfig(p, path.Dir(p), map[string]bool{p: true})
		if !ok {
			return nil
		}
		c.configDir = path.Dir(p)
		return &c
	})
}

// loadConfig applies the extends chain of the config at p, loaded for a config in
// leafDir (the expansion of ${configDir}). visited guards against cycles.
func (a *Analyzer) loadConfig(p, leafDir string, visited map[string]bool) (tsConfig, bool) {
	raw := a.rawConfig(p)
	if raw == nil {
		return tsConfig{}, false
	}
	dir := path.Dir(p)
	var c tsConfig
	for _, ext := range extendsList(raw.Extends) {
		ep := a.resolveExtends(dir, ext)
		if ep == "" || visited[ep] {
			continue
		}
		visited[ep] = true
		parent, ok := a.loadConfig(ep, leafDir, visited)
		if !ok {
			continue
		}
		if parent.hasBase {
			c.baseURL, c.hasBase = parent.baseURL, true
		}
		if parent.paths != nil {
			c.paths, c.pathsDir = parent.paths, parent.pathsDir
		}
	}
	if b := raw.CompilerOptions.BaseURL; b != nil {
		if rest, ok := strings.CutPrefix(*b, configDirVar); ok {
			c.baseURL = path.Join(leafDir, rest)
		} else {
			c.baseURL = path.Join(dir, *b)
		}
		c.hasBase = true
	}
	if raw.CompilerOptions.Paths != nil {
		c.paths, c.pathsDir = compilePaths(raw.CompilerOptions.Paths), dir
	}
	return c, true
}

// rawConfig reads and parses one config file (nil if missing or invalid).
func (a *Analyzer) rawConfig(p string) *rawTSConfig {
	return a.rawTS.get(p, func() *rawTSConfig {
		b := a.h.Read(p)
		if b == nil {
			return nil
		}
		var raw rawTSConfig
		if err := parseJSONC(b, &raw); err != nil {
			return nil
		}
		return &raw
	})
}

// resolveExtends resolves an "extends" entry of a config in dir to a repo file. Bare
// entries are looked up in workspace packages (configs in node_modules are not part
// of the working tree and rarely declare paths).
func (a *Analyzer) resolveExtends(dir, ext string) string {
	var cands []string
	if isRelative(ext) || strings.HasPrefix(ext, "/") {
		p := path.Join(dir, ext)
		cands = []string{p, p + ".json", path.Join(p, "tsconfig.json")}
	} else {
		name, sub := splitPackage(ext)
		for _, d := range a.workspaces()[name] {
			if sub == "" {
				cands = append(cands, path.Join(d, "tsconfig.json"))
				continue
			}
			p := path.Join(d, sub)
			cands = append(cands, p, p+".json", path.Join(p, "tsconfig.json"))
		}
	}
	for _, c := range cands {
		if !outside(c) && a.h.Has(c) {
			return c
		}
	}
	return ""
}

// extendsList decodes "extends", which is a string or (TypeScript 5) an array.
func extendsList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	var list []string
	_ = json.Unmarshal(raw, &list)
	return list
}

// compilePaths compiles "paths" into rules ordered most specific first: exact keys,
// then patterns by decreasing prefix length.
func compilePaths(m map[string][]string) []pathRule {
	rules := make([]pathRule, 0, len(m))
	for k, targets := range m {
		r := pathRule{prefix: k, targets: targets}
		if i := strings.IndexByte(k, '*'); i >= 0 {
			r.prefix, r.suffix, r.star = k[:i], k[i+1:], true
		}
		rules = append(rules, r)
	}
	sort.Slice(rules, func(i, j int) bool {
		ri, rj := rules[i], rules[j]
		if ri.star != rj.star {
			return !ri.star
		}
		if len(ri.prefix) != len(rj.prefix) {
			return len(ri.prefix) > len(rj.prefix)
		}
		return ri.prefix+"*"+ri.suffix < rj.prefix+"*"+rj.suffix
	})
	return rules
}

// typeImportOpts are the settings of the options that keep an import declaration whose
// specifiers are all types (import { type A } from "./x") as a side-effect import
// (import {} from "./x"), which loads the module: verbatimModuleSyntax,
// preserveValueImports and importsNotUsedAsValues, in that order.
type typeImportOpts struct {
	set  [3]bool // the option is set in the config or a config it extends
	keep [3]bool // ... to a value that keeps the import
	// known is false when the config cannot be read, or extends one that cannot be
	// while it leaves some option unset.
	known bool
}

// keeps reports whether the options keep such imports, or may (unknown).
func (o typeImportOpts) keeps() bool {
	return !o.known || o.keep[0] || o.keep[1] || o.keep[2]
}

// elidesTypeImports reports whether TypeScript compilers remove an import or export
// declaration whose specifiers all carry the type modifier from the files in dir: no
// tsconfig.json or jsconfig.json that may govern them (every config of every config
// directory at or above dir, see configDirs and dirConfigs) keeps it or may, which
// includes a config that cannot be read. Compilers and type strippers (tsc, esbuild,
// Bun, swc) without such a config remove it.
func (a *Analyzer) elidesTypeImports(dir string) bool {
	return a.elides.get(dir, func() bool {
		for _, cd := range a.configDirs(dir) {
			for _, p := range a.dirConfigFiles(cd) {
				if a.typeImportOptions(p).keeps() {
					return false
				}
			}
		}
		return true
	})
}

// typeImportOptions returns the type import options of the config at p with its
// extends chain applied.
func (a *Analyzer) typeImportOptions(p string) typeImportOpts {
	return a.typeOpts.get(p, func() typeImportOpts {
		return a.loadTypeImportOpts(p, map[string]bool{p: true})
	})
}

// loadTypeImportOpts applies the extends chain of the config at p (later entries
// override earlier ones, the config itself all of them). Configs extended from
// installed packages (node_modules) are read too: they are not part of the working
// tree, but some set verbatimModuleSyntax. visited guards against cycles.
func (a *Analyzer) loadTypeImportOpts(p string, visited map[string]bool) typeImportOpts {
	raw := a.rawConfig(p)
	if raw == nil {
		return typeImportOpts{}
	}
	dir := path.Dir(p)
	out := typeImportOpts{known: true}
	unknownParent := false
	for _, ext := range extendsList(raw.Extends) {
		ep := a.extendsFile(dir, ext)
		if ep == "" {
			unknownParent = true
			continue
		}
		if visited[ep] {
			continue
		}
		visited[ep] = true
		parent := a.loadTypeImportOpts(ep, visited)
		if !parent.known {
			unknownParent = true
			continue
		}
		for i := range parent.set {
			if parent.set[i] {
				out.set[i], out.keep[i] = true, parent.keep[i]
			}
		}
	}
	co := raw.CompilerOptions
	for i, v := range []json.RawMessage{co.VerbatimModuleSyntax, co.PreserveValueImports, co.ImportsNotUsedAsValues} {
		if len(v) == 0 {
			continue
		}
		// Only false (and "remove", importsNotUsedAsValues' default) is known not to
		// keep the import; null resets the option to its default.
		s := strings.TrimSpace(string(v))
		out.set[i], out.keep[i] = true, s != "false" && s != "null" && s != `"remove"`
	}
	if unknownParent && (!out.set[0] || !out.set[1] || !out.set[2]) {
		out.known = false
	}
	return out
}

// extendsFile resolves an "extends" entry of a config in dir to a readable file: a
// repo file (resolveExtends), else a file under a node_modules directory at or above
// dir, or one relative to a config that is itself installed. "" when none is found.
func (a *Analyzer) extendsFile(dir, ext string) string {
	if p := a.resolveExtends(dir, ext); p != "" {
		return p
	}
	try := func(p string) string {
		for _, c := range []string{p, p + ".json", path.Join(p, "tsconfig.json")} {
			if !outside(c) && a.h.Read(c) != nil {
				return c
			}
		}
		return ""
	}
	if isRelative(ext) {
		return try(path.Join(dir, ext))
	}
	if ext == "" || strings.HasPrefix(ext, "/") {
		return ""
	}
	for d := dir; ; d = parentDir(d) {
		if p := try(path.Join(d, "node_modules", ext)); p != "" {
			return p
		}
		if d == "." || d == "" || d == "/" {
			return ""
		}
	}
}

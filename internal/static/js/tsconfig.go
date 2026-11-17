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

// configDirs returns the directories holding a tsconfig.json, jsconfig.json or
// tsconfig.*.json at or above dir, nearest first. A directory with only per-target
// configs (Vite's tsconfig.app.json and tsconfig.node.json, without a tsconfig.json
// referencing them) is one too: their paths apply to the files below it.
func (a *Analyzer) configDirs(dir string) []string {
	return a.tsDirs.get(dir, func() []string {
		var out []string
		if len(a.dirConfigFiles(dir)) > 0 {
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

// chainLink is one config of an extends chain (see configChain): its path and parsed
// content, nil when it cannot be read (an "extends" entry that resolves to no readable
// config has path "").
type chainLink struct {
	path string
	raw  *rawTSConfig
}

// maxChain bounds the length of an extends chain: past it the rest is unknown.
const maxChain = 256

// configChain returns the configs whose settings make up the config at p, in the
// order they apply (a later one overrides an earlier one): the chain of each "extends"
// entry, in order, then p itself. Like tsc, every entry is resolved on its own, so a
// base two entries share (a diamond) appears in both of their chains and its settings
// apply again where the second one starts. An entry already being resolved (a cycle)
// is skipped. Entries are also looked up in node_modules: a shared config may declare
// paths with ${configDir}, or set verbatimModuleSyntax.
func (a *Analyzer) configChain(p string) []chainLink {
	var out []chainLink
	stack := map[string]bool{}
	var walk func(p string)
	walk = func(p string) {
		if len(out) >= maxChain {
			out = append(out, chainLink{})
			return
		}
		raw := a.rawConfig(p)
		if raw == nil {
			out = append(out, chainLink{path: p})
			return
		}
		stack[p] = true
		for _, ext := range extendsList(raw.Extends) {
			ep := a.extendsFile(path.Dir(p), ext)
			switch {
			case ep == "":
				out = append(out, chainLink{})
			case !stack[ep]:
				walk(ep)
			}
		}
		delete(stack, p)
		out = append(out, chainLink{path: p, raw: raw})
	}
	walk(p)
	return out
}

// tsconfig returns the config at p with its extends chain applied (nil if unreadable).
// An "extends" entry that cannot be read contributes nothing.
func (a *Analyzer) tsconfig(p string) *tsConfig {
	return a.tsByPath.get(p, func() *tsConfig {
		if a.rawConfig(p) == nil {
			return nil
		}
		c := tsConfig{configDir: path.Dir(p)}
		for _, link := range a.configChain(p) {
			if link.raw == nil {
				continue
			}
			dir, co := path.Dir(link.path), link.raw.CompilerOptions
			if b := co.BaseURL; b != nil {
				if rest, ok := strings.CutPrefix(*b, configDirVar); ok {
					c.baseURL = path.Join(c.configDir, rest)
				} else {
					c.baseURL = path.Join(dir, *b)
				}
				c.hasBase = true
			}
			if co.Paths != nil {
				c.paths, c.pathsDir = compilePaths(co.Paths), dir
			}
		}
		return &c
	})
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
// entries are looked up in workspace packages; extendsFile also looks in node_modules.
func (a *Analyzer) resolveExtends(dir, ext string) string {
	if isRelative(ext) || strings.HasPrefix(ext, "/") {
		p := path.Join(dir, ext)
		return firstConfig(a.h.Has, p, p+".json", path.Join(p, "tsconfig.json"))
	}
	name, sub := splitPackage(ext)
	for _, d := range a.workspaces()[name] {
		if c := firstConfig(a.h.Has, a.packageConfigs(d, sub)...); c != "" {
			return c
		}
	}
	return ""
}

// firstConfig returns the first of cands inside the repository that exists ("" when
// none does).
func firstConfig(exists func(string) bool, cands ...string) string {
	for _, c := range cands {
		if !outside(c) && exists(c) {
			return c
		}
	}
	return ""
}

// packageConfigs returns the files an "extends" of subpath sub ("" for the package
// itself) of the package in dir may name, in the order TypeScript tries them: what the
// package's "exports" maps the subpath (or the subpath with ".json") to, its "tsconfig"
// field, then the subpath as a file, with ".json", or as a directory with a
// tsconfig.json (the package's tsconfig.json for "").
func (a *Analyzer) packageConfigs(dir, sub string) []string {
	var out []string
	file := func(p string) {
		out = append(out, p, p+".json", path.Join(p, "tsconfig.json"))
	}
	var p *pkgJSON
	if b := a.h.Read(path.Join(dir, "package.json")); b != nil {
		p = parsePkgJSON(b)
	}
	if p != nil && p.hasExports {
		keys := []string{"."}
		if sub != "" {
			keys = []string{"./" + sub, "./" + sub + ".json"}
		}
		for _, k := range keys {
			for _, t := range subpathTargets(p.exports, k) {
				if strings.HasPrefix(t.path, "./") {
					file(path.Join(dir, t.path))
				}
			}
		}
	}
	if sub == "" {
		if p != nil && p.tsconfig != "" {
			file(path.Join(dir, p.tsconfig))
		}
		return append(out, path.Join(dir, "tsconfig.json"))
	}
	file(path.Join(dir, sub))
	return out
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
	// and some option is set by no config applied after it.
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
// extends chain (see configChain) applied, later configs overriding earlier ones. A
// config of the chain that cannot be read (or found) may set any option: an option no
// later config sets is then unknown.
func (a *Analyzer) typeImportOptions(p string) typeImportOpts {
	return a.typeOpts.get(p, func() typeImportOpts {
		if a.rawConfig(p) == nil {
			return typeImportOpts{}
		}
		var unknown [3]bool
		var out typeImportOpts
		for _, link := range a.configChain(p) {
			if link.raw == nil {
				unknown = [3]bool{true, true, true}
				continue
			}
			co := link.raw.CompilerOptions
			for i, v := range []json.RawMessage{co.VerbatimModuleSyntax, co.PreserveValueImports, co.ImportsNotUsedAsValues} {
				if len(v) == 0 {
					continue
				}
				// Only false (and "remove", importsNotUsedAsValues' default) is known
				// not to keep the import; null resets the option to its default.
				s := strings.TrimSpace(string(v))
				out.set[i], out.keep[i], unknown[i] = true, s != "false" && s != "null" && s != `"remove"`, false
			}
		}
		out.known = !unknown[0] && !unknown[1] && !unknown[2]
		return out
	})
}

// extendsFile resolves an "extends" entry of a config in dir to a readable file: a
// repo file (resolveExtends), else a file under a node_modules directory at or above
// dir, or one relative to a config that is itself installed. "" when none is found.
func (a *Analyzer) extendsFile(dir, ext string) string {
	if p := a.resolveExtends(dir, ext); p != "" {
		return p
	}
	readable := func(p string) bool { return a.h.Read(p) != nil }
	if isRelative(ext) {
		p := path.Join(dir, ext)
		return firstConfig(readable, p, p+".json", path.Join(p, "tsconfig.json"))
	}
	if ext == "" || strings.HasPrefix(ext, "/") {
		return ""
	}
	name, sub := splitPackage(ext)
	for d := dir; ; d = parentDir(d) {
		if p := firstConfig(readable, a.packageConfigs(path.Join(d, "node_modules", name), sub)...); p != "" {
			return p
		}
		if d == "." || d == "" || d == "/" {
			return ""
		}
	}
}

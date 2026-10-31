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
}

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

// resolve resolves a bare specifier through "paths" and then "baseUrl" and reports
// whether anything was found. Rules are tried most specific first; the first rule whose
// targets resolve wins, as in TypeScript.
func (c *tsConfig) resolve(a *Analyzer, spec string, add func(string)) bool {
	base := c.pathsBase()
	for _, r := range c.paths {
		star, ok := r.match(spec)
		if !ok {
			continue
		}
		found := false
		for _, t := range r.targets {
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
		var out []*tsConfig
		for _, p := range paths {
			if c := a.tsconfig(p); c != nil {
				out = append(out, c)
			}
		}
		return out
	})
}

// tsconfig returns the config at p with its extends chain applied (nil if unreadable).
func (a *Analyzer) tsconfig(p string) *tsConfig {
	return a.tsByPath.get(p, func() *tsConfig {
		c, ok := a.loadConfig(p, map[string]bool{p: true})
		if !ok {
			return nil
		}
		return &c
	})
}

// loadConfig applies the extends chain of the config at p. visited guards against
// cycles.
func (a *Analyzer) loadConfig(p string, visited map[string]bool) (tsConfig, bool) {
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
		parent, ok := a.loadConfig(ep, visited)
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
		c.baseURL, c.hasBase = path.Join(dir, *b), true
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

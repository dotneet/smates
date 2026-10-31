package js

import (
	"encoding/json"
	"path"
	"strings"
)

// pkgJSON is the part of a package.json the resolver uses.
type pkgJSON struct {
	name       string
	fields     map[string]string // string-valued entry fields (main, module, ...)
	exports    any
	hasExports bool
	imports    any
}

// entryFields are package.json fields naming an entry point, in preference order.
var entryFields = []string{"source", "module", "main", "browser", "svelte", "jsnext:main", "types", "typings"}

// typeFields are entry fields that only name type declarations.
var typeFields = map[string]bool{"types": true, "typings": true}

// pkg returns the parsed package.json in dir, or nil if there is none or it does not
// parse.
func (a *Analyzer) pkg(dir string) *pkgJSON {
	return a.pkgs.get(dir, func() *pkgJSON {
		f := path.Join(dir, "package.json")
		if !a.h.Has(f) {
			return nil
		}
		return parsePkgJSON(a.h.Read(f))
	})
}

func parsePkgJSON(b []byte) *pkgJSON {
	var m map[string]json.RawMessage
	if err := parseJSONC(b, &m); err != nil {
		return nil
	}
	p := &pkgJSON{fields: map[string]string{}}
	_ = json.Unmarshal(m["name"], &p.name)
	for _, f := range entryFields {
		var v string
		if json.Unmarshal(m[f], &v) == nil && v != "" {
			p.fields[f] = v
		}
	}
	if raw, ok := m["exports"]; ok {
		p.hasExports = true
		_ = json.Unmarshal(raw, &p.exports)
	}
	if raw, ok := m["imports"]; ok {
		_ = json.Unmarshal(raw, &p.imports)
	}
	return p
}

// workspaces returns the map from package name to package directories, built from
// every package.json in the repository outside node_modules. This covers npm, yarn,
// pnpm and bun workspaces without reading their workspace configuration.
func (a *Analyzer) workspaces() map[string][]string {
	a.wsOnce.Do(func() {
		ws := map[string][]string{}
		dirs := map[string]bool{}
		for _, f := range a.files() {
			if path.Base(f) != "package.json" || hasSegment(f, "node_modules") {
				continue
			}
			d := path.Dir(f)
			dirs[d] = true
			if p := a.pkg(d); p != nil && p.name != "" {
				ws[p.name] = append(ws[p.name], d)
			}
		}
		a.ws, a.pkgDirs = ws, dirs
	})
	return a.ws
}

// nearestPkgDir returns the directory of the nearest package.json at or above dir.
func (a *Analyzer) nearestPkgDir(dir string) (string, bool) {
	for d := dir; ; d = parentDir(d) {
		if a.h.Has(path.Join(d, "package.json")) {
			return d, true
		}
		if d == "." || d == "/" || d == "" {
			return ".", false
		}
	}
}

// wsResult is the resolution of a bare specifier against workspace packages.
type wsResult struct {
	files    []string // resolved existing files
	found    bool     // a runtime entry point was resolved in every matching package
	fallback []string // source files of matching packages whose entry was not found
}

// resolveWorkspace resolves a bare specifier ("pkg" or "pkg/sub") against workspace
// packages. Results are cached per specifier.
func (a *Analyzer) resolveWorkspace(spec string) wsResult {
	return a.wsSpecs.get(spec, func() wsResult {
		name, sub := splitPackage(spec)
		var res wsResult
		dirs := a.workspaces()[name]
		if len(dirs) == 0 {
			return res
		}
		res.found = true
		add := func(f string) { res.files = append(res.files, f) }
		for _, d := range dirs {
			if !a.resolveInPackage(d, a.pkg(d), sub, add) {
				res.found = false
				res.fallback = append(res.fallback, a.packageFiles(d)...)
			}
		}
		return res
	})
}

// resolveInPackage resolves subpath sub ("" for the package root) of the package in dir
// and reports whether a runtime (not types-only) entry point exists.
func (a *Analyzer) resolveInPackage(dir string, p *pkgJSON, sub string, add func(string)) bool {
	found := false
	if p != nil && p.hasExports {
		key := "."
		if sub != "" {
			key = "./" + sub
		}
		for _, t := range subpathTargets(p.exports, key) {
			if !strings.HasPrefix(t.path, "./") {
				continue // exports targets must be package-relative
			}
			if a.resolvePath(path.Join(dir, t.path), add) && !t.typesOnly {
				found = true
			}
		}
		return found
	}
	if sub != "" {
		return a.resolvePath(path.Join(dir, sub), add) || a.resolvePath(path.Join(dir, "src", sub), add)
	}
	if p != nil {
		for _, f := range entryFields {
			if v := p.fields[f]; v != "" && a.resolvePath(path.Join(dir, v), add) && !typeFields[f] {
				found = true
			}
		}
	}
	if found {
		return true
	}
	for _, base := range []string{"index", "src/index"} {
		for _, e := range resolveExts {
			if c := path.Join(dir, base+e); a.h.Has(c) {
				add(c)
				found = found || e != ".d.ts" // declarations alone are not an entry point
			}
		}
		if found {
			return true
		}
	}
	return false
}

// resolveHashImport resolves a package.json "imports" specifier (#internal/x) against
// the nearest package.json that declares "imports".
func (a *Analyzer) resolveHashImport(dir, spec string, add func(string)) {
	for d := dir; ; d = parentDir(d) {
		if p := a.pkg(d); p != nil && p.imports != nil {
			for _, t := range subpathTargets(p.imports, spec) {
				switch {
				case isRelative(t.path):
					a.resolvePath(path.Join(d, t.path), add)
				case t.path != "" && t.path[0] != '#' && t.path[0] != '/':
					a.resolveBare(d, t.path, add)
				}
			}
			return
		}
		if d == "." || d == "/" || d == "" {
			return
		}
	}
}

// packageFiles returns the source files of the package in dir: JS/TS-family and JSON
// files, excluding build output, node_modules and nested packages.
func (a *Analyzer) packageFiles(dir string) []string {
	return a.pkgFiles.get(dir, func() []string {
		a.workspaces() // populates pkgDirs
		prefix := dir + "/"
		if dir == "." {
			prefix = ""
		}
		var out []string
	files:
		for _, f := range a.under(prefix) {
			if !sourceExts[path.Ext(f)] {
				continue
			}
			rel := f[len(prefix):]
			for i := 0; i < len(rel); i++ {
				if rel[i] != '/' {
					continue
				}
				// rel[:i] is an intermediate directory.
				seg := rel[strings.LastIndexByte(rel[:i], '/')+1 : i]
				if outputDirs[seg] || a.pkgDirs[prefix+rel[:i]] {
					continue files
				}
			}
			out = append(out, f)
		}
		return out
	})
}

// splitPackage splits a bare specifier into the package name and the subpath:
// "@s/p/x/y" -> ("@s/p", "x/y"), "p/x" -> ("p", "x").
func splitPackage(spec string) (name, sub string) {
	i := strings.IndexByte(spec, '/')
	if strings.HasPrefix(spec, "@") && i >= 0 {
		j := strings.IndexByte(spec[i+1:], '/')
		if j < 0 {
			return spec, ""
		}
		i += 1 + j
	}
	if i < 0 {
		return spec, ""
	}
	return spec[:i], spec[i+1:]
}

// target is a path an "exports"/"imports" entry maps to.
type target struct {
	path      string
	typesOnly bool // reached only through a "types"/"typings" condition
}

// subpathTargets returns the targets an "exports" or "imports" field maps key ("." or
// "./sub" or "#x") to. Exact keys win over "*" patterns (longest prefix first) and
// legacy folder mappings ("./dir/"). Every condition is followed, not just one set:
// over-approximating is safe, and targets that do not exist are dropped by the caller.
func subpathTargets(field any, key string) []target {
	m, ok := field.(map[string]any)
	if !ok || !isSubpathMap(m) {
		if key == "." {
			return collectTargets(field, "", false, nil)
		}
		return nil
	}
	if v, ok := m[key]; ok {
		return collectTargets(v, "", false, nil)
	}
	bestKey, bestPre, bestStar, bestFolder := "", "", "", false
	for k := range m {
		var pre, star string
		folder := false
		if i := strings.IndexByte(k, '*'); i >= 0 {
			pre, suf := k[:i], k[i+1:]
			if len(key) < len(pre)+len(suf) || !strings.HasPrefix(key, pre) || !strings.HasSuffix(key, suf) {
				continue
			}
			star = key[len(pre) : len(key)-len(suf)]
		} else if strings.HasSuffix(k, "/") && strings.HasPrefix(key, k) {
			pre, star, folder = k, key[len(k):], true
		} else {
			continue
		}
		if bestKey == "" || len(pre) > len(bestPre) || (len(pre) == len(bestPre) && len(k) > len(bestKey)) ||
			(len(pre) == len(bestPre) && len(k) == len(bestKey) && k < bestKey) {
			bestKey, bestPre, bestStar, bestFolder = k, pre, star, folder
		}
	}
	if bestKey == "" {
		return nil
	}
	if bestFolder {
		return collectTargets(m[bestKey], bestStar, true, nil)
	}
	return collectTargets(m[bestKey], bestStar, false, nil)
}

// collectTargets flattens a target value (string, condition object, fallback array)
// substituting star for "*" (or appending it for folder mappings).
func collectTargets(v any, star string, folder bool, out []target) []target {
	return collectTargetsTyped(v, star, folder, false, out)
}

func collectTargetsTyped(v any, star string, folder, typesOnly bool, out []target) []target {
	switch v := v.(type) {
	case string:
		p := v
		if folder {
			p += star
		} else {
			p = strings.ReplaceAll(p, "*", star)
		}
		out = append(out, target{path: p, typesOnly: typesOnly})
	case []any:
		for _, e := range v {
			out = collectTargetsTyped(e, star, folder, typesOnly, out)
		}
	case map[string]any:
		for cond, e := range v {
			out = collectTargetsTyped(e, star, folder, typesOnly || typeFields[cond], out)
		}
	}
	return out
}

// isSubpathMap reports whether an exports/imports object is keyed by subpaths rather
// than conditions.
func isSubpathMap(m map[string]any) bool {
	for k := range m {
		if strings.HasPrefix(k, ".") || strings.HasPrefix(k, "#") {
			return true
		}
	}
	return false
}

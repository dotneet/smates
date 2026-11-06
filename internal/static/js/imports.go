package js

import "path"

// Import is a module specifier a JavaScript or TypeScript source imports, exports
// from, requires or imports dynamically.
type Import struct {
	Spec string
	// TypeOnly is set for a type-only declaration (import type, export type ... from),
	// which every TypeScript compiler removes: the module is never loaded.
	TypeOnly bool
}

// Imports returns the module specifiers of the source of file p, in order. Specifiers
// in comments and strings are not imports. A specifier list whose specifiers all carry
// the type modifier (import { type A } from) counts as loading the module: compiler
// options may keep it.
func Imports(p string, src []byte) []Import {
	var refs []ref
	switch path.Ext(p) {
	case ".vue", ".svelte":
		refs = scanSFC(src, false)
	case ".astro":
		refs = scanSFC(src, true)
	default:
		refs = scanSource(src)
	}
	ts := typeScriptExts[path.Ext(p)]
	var out []Import
	for _, r := range refs {
		if r.kind == refModule {
			out = append(out, Import{Spec: r.spec, TypeOnly: ts && r.erase == eraseAlways})
		}
	}
	return out
}

// Resolve implements host.Resolver: the repository files a module specifier imported
// from file p resolves to (none for an installed package or a builtin).
func (a *Analyzer) Resolve(p, spec string) []string {
	var out []string
	seen := map[string]bool{}
	a.resolveRef(path.Dir(p), ref{kind: refModule, spec: spec}, func(f string) {
		if !seen[f] && !outside(f) && !hasSegment(f, "node_modules") && a.h.Has(f) {
			seen[f] = true
			out = append(out, f)
		}
	})
	return out
}

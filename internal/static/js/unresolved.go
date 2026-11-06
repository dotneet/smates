package js

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Unresolved implements host.ResolveReporter: the module specifiers p imports at run
// time (type-only declarations left out) that resolve to no file of the repository,
// name no Node builtin and no package installed in a node_modules directory at or
// above p: an alias only a bundler's configuration knows (Vite's resolve.alias), a
// virtual module a plugin serves, a file that does not exist. Patterns (template
// literals, import.meta.glob) are not checked.
func (a *Analyzer) Unresolved(p string, src []byte) []string {
	var refs []ref
	switch path.Ext(p) {
	case ".vue", ".svelte":
		refs = scanSFC(src, false)
	case ".astro":
		refs = scanSFC(src, true)
	default:
		refs = scanSource(src)
	}
	dir := path.Dir(p)
	var out []string
	for _, r := range refs {
		if r.kind != refModule || r.erase == eraseAlways {
			continue
		}
		spec := cleanSpec(r.spec)
		if spec == "" || strings.HasPrefix(spec, "data:") {
			continue
		}
		found := false
		a.resolveRef(dir, r, func(f string) {
			if !outside(f) && !hasSegment(f, "node_modules") && a.h.Has(f) {
				found = true
			}
		})
		if found {
			continue
		}
		if isExternalScheme(spec) {
			if strings.HasPrefix(spec, "node:") {
				continue
			}
		} else if isBare(spec) && (nodeBuiltins[strings.SplitN(spec, "/", 2)[0]] || a.installed(dir, spec)) {
			continue
		}
		out = append(out, r.spec)
	}
	return out
}

// installed reports whether the package a bare specifier names has a directory in
// node_modules at or above dir (up to the repository root).
func (a *Analyzer) installed(dir, spec string) bool {
	parts := strings.SplitN(spec, "/", 3)
	name := parts[0]
	if strings.HasPrefix(name, "@") && len(parts) > 1 {
		name += "/" + parts[1]
	}
	return a.installs.get(dir+"\x00"+name, func() bool {
		if fi, err := os.Stat(filepath.Join(a.h.Root(), filepath.FromSlash(dir), "node_modules", filepath.FromSlash(name))); err == nil && fi.IsDir() {
			return true
		}
		if dir == "." || dir == "" || dir == "/" {
			return false
		}
		return a.installed(parentDir(dir), spec)
	})
}

// nodeBuiltins are Node's builtin modules, imported without the node: prefix.
var nodeBuiltins = map[string]bool{
	"assert": true, "async_hooks": true, "buffer": true, "child_process": true, "cluster": true, "console": true,
	"constants": true, "crypto": true, "dgram": true, "diagnostics_channel": true, "dns": true, "domain": true,
	"events": true, "fs": true, "http": true, "http2": true, "https": true, "inspector": true, "module": true,
	"net": true, "os": true, "path": true, "perf_hooks": true, "process": true, "punycode": true, "querystring": true,
	"readline": true, "repl": true, "stream": true, "string_decoder": true, "sys": true, "timers": true, "tls": true,
	"trace_events": true, "tty": true, "url": true, "util": true, "v8": true, "vm": true, "wasi": true,
	"worker_threads": true, "zlib": true,
}

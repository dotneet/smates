package js

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/dotneet/smates/internal/static/host"
)

// Unresolved implements host.ResolveReporter: the module specifiers p imports at run
// time (type-only declarations left out) that resolve to no file of the repository,
// name no Node builtin and no package installed in a node_modules directory at or
// above p: an alias only a bundler's configuration knows (Vite's resolve.alias), a
// virtual module a plugin serves, a file that does not exist. An import() or require()
// of a computed name, or of a template literal without a static prefix
// (import(`${dir}/x.js`)), is reported once as host.DynamicSpec: it may load any file.
// Other patterns (templates with a prefix, import.meta.glob) are not checked, nor are
// specifiers only the coarse scan found (see withCoarse), which may be a comment's.
func (a *Analyzer) Unresolved(p string, src []byte) []string {
	dir := path.Dir(p)
	var out []string
	dynamic := false
	for _, r := range a.analyze(p, src).unfound {
		switch {
		case r.kind == refDynamic, r.kind == refTemplate && r.spec == "":
			dynamic = true
			continue
		case r.kind != refModule || r.erase == eraseAlways || r.coarse:
			// A specifier only the coarse scan found may be a comment's or a
			// string's; it adds dependencies but does not make the file unknown.
			continue
		}
		spec := cleanSpec(r.spec)
		if spec == "" || strings.HasPrefix(spec, "data:") {
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
	if dynamic {
		out = append(out, host.DynamicSpec)
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

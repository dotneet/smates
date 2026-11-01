// Package js resolves direct dependencies of JavaScript and TypeScript source files
// (including <script> blocks of .vue, .svelte and .astro components).
//
// Specifiers are extracted with a small lexer (see lexer.go) and resolved the way
// TypeScript, Node and bundlers do: relative paths with TypeScript extension rules,
// tsconfig/jsconfig "paths" and "baseUrl", package.json "imports", and workspace
// packages via "exports"/"main"/"module". The analyzer is a safety net under runtime
// traces, so whenever resolution is ambiguous it over-approximates: it returns every
// plausible candidate, and a workspace package whose entry point does not exist in the
// working tree (e.g. an unbuilt dist/) contributes all of its source files.
package js

import (
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/dotneet/smates/internal/static/host"
)

// Analyzer implements host.Analyzer for JavaScript and TypeScript. It caches
// repository configuration (tsconfig files, package.json files, the workspace package
// map) and is safe for concurrent use.
type Analyzer struct {
	h    host.Host
	opts Options

	filesOnce sync.Once
	sorted    []string // all repo files, sorted

	wsOnce  sync.Once
	ws      map[string][]string // workspace package name -> package dirs
	pkgDirs map[string]bool     // every dir with a package.json (outside node_modules)

	pkgs      memo[*pkgJSON] // dir -> parsed package.json (nil if none)
	rawTS     memo[*rawTSConfig]
	tsByPath  memo[*tsConfig]
	tsDirs    memo[[]string]    // dir -> config dirs from nearest to root
	tsConfigs memo[[]*tsConfig] // config dir -> configs that apply there
	wsSpecs   memo[wsResult]    // bare specifier -> workspace resolution
	pkgFiles  memo[[]string]    // package dir -> its source files
}

// Options configure the analyzer.
type Options struct {
	// Roots are extra repo-relative directories bare specifiers are resolved against
	// (like NODE_PATH or a bundler's resolve.modules).
	Roots []string
}

// New creates an analyzer bound to a repository.
func New(h host.Host, opts Options) *Analyzer { return &Analyzer{h: h, opts: opts} }

// handledExts are the file extensions the analyzer understands.
var handledExts = map[string]bool{
	".js": true, ".mjs": true, ".cjs": true, ".jsx": true,
	".ts": true, ".mts": true, ".cts": true, ".tsx": true,
	".vue": true, ".svelte": true, ".astro": true,
}

// resolveExts are the extensions tried, in order, for an extensionless specifier.
var resolveExts = []string{
	".ts", ".tsx", ".mts", ".cts", ".d.ts", ".js", ".jsx", ".mjs", ".cjs",
	".json", ".vue", ".svelte",
}

// sourceExts are the extensions of files that make up a package's source.
var sourceExts = map[string]bool{
	".ts": true, ".tsx": true, ".mts": true, ".cts": true,
	".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".json": true, ".vue": true, ".svelte": true, ".astro": true,
}

// outputDirs are directory names whose files are build output or caches, not
// package source.
var outputDirs = map[string]bool{
	"node_modules": true, "dist": true, "build": true, "coverage": true,
	".next": true, ".nuxt": true, ".svelte-kit": true, ".turbo": true, ".output": true,
}

// Handles implements host.Analyzer.
func (a *Analyzer) Handles(p string) bool { return handledExts[path.Ext(p)] }

// Deps implements host.Analyzer.
func (a *Analyzer) Deps(p string, src []byte) []string {
	var refs []ref
	switch path.Ext(p) {
	case ".vue", ".svelte":
		refs = scanSFC(src, false)
	case ".astro":
		refs = scanSFC(src, true)
	default:
		refs = scanSource(src)
	}
	if len(refs) == 0 {
		return nil
	}
	dir := path.Dir(p)
	seen := map[string]bool{p: true}
	var out []string
	add := func(f string) {
		if seen[f] || outside(f) || hasSegment(f, "node_modules") || !a.h.Has(f) {
			return
		}
		seen[f] = true
		out = append(out, f)
	}
	for _, r := range refs {
		a.resolveRef(dir, r, add)
	}
	sort.Strings(out)
	return out
}

// resolveRef resolves one reference made from a file in dir.
func (a *Analyzer) resolveRef(dir string, r ref, add func(string)) {
	switch r.kind {
	case refModule:
		a.resolveModule(dir, r.spec, add)
	case refFile:
		spec := cleanSpec(r.spec)
		if spec == "" || strings.Contains(spec, "://") || strings.HasPrefix(spec, "data:") {
			return
		}
		if spec[0] == '/' {
			a.resolveModule(dir, spec, add)
			return
		}
		a.resolvePath(path.Join(dir, spec), add)
	case refTemplate:
		// Like cleanSpec for the prefix: `./icons/${n}.svg?raw` loads ./icons/*.svg.
		suffix := r.suffix
		if k := strings.IndexAny(suffix, "?#"); k >= 0 {
			suffix = suffix[:k]
		}
		for _, pre := range a.expandPrefix(dir, r.spec, add) {
			a.matchTemplate(pre, suffix, add)
		}
	case refGlob:
		a.matchGlob(dir, r.spec, add)
	case refContext:
		head := strings.TrimSuffix(r.spec, "/") + "/"
		for _, pre := range a.expandPrefix(dir, head, add) {
			for _, f := range a.under(pre) {
				if r.recursive || !strings.Contains(f[len(pre):], "/") {
					add(f)
				}
			}
		}
	}
}

// resolveModule resolves a module specifier imported from a file in dir.
func (a *Analyzer) resolveModule(dir, spec string, add func(string)) {
	spec = cleanSpec(spec)
	switch {
	case spec == "":
	case isRelative(spec):
		a.resolvePath(path.Join(dir, spec), add)
	case spec[0] == '/':
		// Root-relative (Vite): relative to the project root, which is the nearest
		// package directory or the repository root.
		rel := strings.TrimLeft(spec, "/")
		a.resolvePath(rel, add)
		if pd, ok := a.nearestPkgDir(dir); ok && pd != "." {
			a.resolvePath(path.Join(pd, rel), add)
		}
	case spec[0] == '#':
		// package.json "imports" (Node) and tsconfig "paths" (TypeScript and bundlers
		// check them first; "#lib/*" is a valid key): keep both.
		a.resolveHashImport(dir, spec, add)
		a.resolveTSConfig(dir, spec, add)
	case isExternalScheme(spec):
	default:
		a.resolveBare(dir, spec, add)
	}
}

// resolveBare resolves a bare specifier through every mechanism that may apply:
// tsconfig paths/baseUrl, configured roots and workspace packages. A workspace
// package's whole-source fallback is used only if nothing else resolved the specifier.
func (a *Analyzer) resolveBare(dir, spec string, add func(string)) {
	found := a.resolveTSConfig(dir, spec, add)
	for _, root := range a.opts.Roots {
		if a.resolvePath(path.Join(root, spec), add) {
			found = true
		}
	}
	ws := a.resolveWorkspace(spec)
	for _, f := range ws.files {
		add(f)
	}
	if ws.found {
		return
	}
	if !found {
		for _, f := range ws.fallback {
			add(f)
		}
		if len(ws.fallback) == 0 {
			a.resolveAliasHeuristic(dir, spec, add)
		}
	}
}

// resolveTSConfig resolves a specifier through the tsconfig paths/baseUrl of the
// nearest config directory that resolves it, and reports whether anything was found.
func (a *Analyzer) resolveTSConfig(dir, spec string, add func(string)) bool {
	for _, cd := range a.configDirs(dir) {
		found := false
		for _, c := range a.dirConfigs(cd) {
			if c.resolve(a, spec, add) {
				found = true
			}
		}
		if found {
			return true
		}
	}
	return false
}

// aliasHeuristics are bundler aliases conventionally configured outside tsconfig
// (vite/webpack config), mapped to a directory of the nearest package.
var aliasHeuristics = []struct{ prefix, dir string }{
	{"@/", "src"}, {"~/", "src"}, {"$lib/", "src/lib"},
}

// resolveAliasHeuristic resolves conventional aliases like "@/x" to src/x of the
// nearest package (and to x at the package root for "@/" and "~/").
func (a *Analyzer) resolveAliasHeuristic(dir, spec string, add func(string)) {
	for _, h := range aliasHeuristics {
		rest, ok := strings.CutPrefix(spec, h.prefix)
		if !ok {
			continue
		}
		pd, _ := a.nearestPkgDir(dir)
		if !a.resolvePath(path.Join(pd, h.dir, rest), add) && h.dir == "src" {
			a.resolvePath(path.Join(pd, rest), add)
		}
	}
}

// resolvePath resolves a repo-relative path with TypeScript's rules and reports
// whether anything was found.
func (a *Analyzer) resolvePath(p string, add func(string)) bool {
	return a.resolvePathDepth(p, add, 0)
}

func (a *Analyzer) resolvePathDepth(p string, add func(string), depth int) bool {
	if outside(p) {
		return false
	}
	found := false
	try := func(c string) {
		if a.h.Has(c) {
			add(c)
			found = true
		}
	}
	try(p)
	// TypeScript (and bundlers in TS projects) map an emitted extension back to its
	// source. Over-approximate: keep the exact file too if both exist.
	ext := path.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	switch ext {
	case ".js":
		try(stem + ".ts")
		try(stem + ".tsx")
		try(stem + ".d.ts")
	case ".jsx":
		try(stem + ".tsx")
	case ".mjs":
		try(stem + ".mts")
		try(stem + ".d.mts")
	case ".cjs":
		try(stem + ".cts")
		try(stem + ".d.cts")
	}
	if p != "." {
		for _, e := range resolveExts {
			try(p + e)
		}
	}
	if found || depth > 2 {
		return found
	}
	return a.resolveDir(p, add, depth)
}

// resolveDir resolves a directory import: package.json entry fields, then index files.
func (a *Analyzer) resolveDir(dir string, add func(string), depth int) bool {
	found := false
	if pkg := a.pkg(dir); pkg != nil {
		for _, f := range entryFields {
			if v := pkg.fields[f]; v != "" && a.resolvePathDepth(path.Join(dir, v), add, depth+1) {
				found = true
			}
		}
	}
	if found {
		return true
	}
	for _, e := range resolveExts {
		if c := path.Join(dir, "index"+e); a.h.Has(c) {
			add(c)
			found = true
		}
	}
	return found
}

// expandPrefix maps the static prefix of a dynamic specifier (template literal head,
// glob directory, require.context directory) to repo-relative path prefixes. Bare
// prefixes naming a workspace package add the whole package via add.
func (a *Analyzer) expandPrefix(dir, prefix string, add func(string)) []string {
	prefix = cleanSpec(prefix)
	var out []string
	push := func(base, p string) {
		if s, ok := joinPrefix(base, p); ok {
			out = append(out, s)
		}
	}
	switch {
	case prefix == "":
		// Nothing static to go on (`${dir}/x.js`).
	case isRelative(prefix):
		push(dir, prefix)
	case prefix[0] == '/':
		rel := "./" + strings.TrimLeft(prefix, "/")
		push(".", rel)
		if pd, ok := a.nearestPkgDir(dir); ok && pd != "." {
			push(pd, rel)
		}
	case isExternalScheme(prefix):
	case prefix[0] == '#':
		// package.json "imports" patterns are not expanded; tsconfig "paths" are.
		a.expandTSConfig(dir, prefix, push)
	default:
		a.expandTSConfig(dir, prefix, push)
		if name, _ := splitPackage(prefix); len(prefix) > len(name) {
			for _, d := range a.workspaces()[name] {
				for _, f := range a.packageFiles(d) {
					add(f)
				}
			}
		}
		pd, _ := a.nearestPkgDir(dir)
		for _, h := range aliasHeuristics {
			if rest, ok := strings.CutPrefix(prefix, h.prefix); ok {
				push(path.Join(pd, h.dir), "./"+rest)
			}
		}
	}
	return out
}

// expandTSConfig maps a dynamic specifier prefix through tsconfig "paths" patterns and
// "baseUrl" of every config at or above dir, passing each (base, relative prefix) to push.
func (a *Analyzer) expandTSConfig(dir, prefix string, push func(base, p string)) {
	for _, cd := range a.configDirs(dir) {
		for _, c := range a.dirConfigs(cd) {
			for _, r := range c.paths {
				if !r.star || !strings.HasPrefix(prefix, r.prefix) {
					continue
				}
				for _, t := range r.targets {
					base, t := c.target(t)
					if k := strings.IndexByte(t, '*'); k >= 0 {
						push(base, "./"+t[:k]+prefix[len(r.prefix):])
					}
				}
			}
			if c.hasBase {
				push(c.baseURL, "./"+prefix)
			}
		}
	}
}

// matchTemplate adds files whose path is pre + anything + suffix. The suffix also
// matches with TypeScript's extension mapping and extension probing.
func (a *Analyzer) matchTemplate(pre, suffix string, add func(string)) {
	sufExt := path.Ext(suffix)
	if strings.Contains(sufExt, "/") {
		sufExt = ""
	}
	sufStem := strings.TrimSuffix(suffix, sufExt)
	jsFamily := sourceExts[sufExt] && sufExt != ".json"
	for _, f := range a.under(pre) {
		rest := f[len(pre):]
		if suffix == "" || strings.HasSuffix(rest, suffix) {
			add(f)
			continue
		}
		ext := path.Ext(rest)
		if !sourceExts[ext] {
			continue
		}
		stem := strings.TrimSuffix(rest, ext)
		switch {
		case jsFamily && ext != ".json" && strings.HasSuffix(stem, sufStem):
			add(f) // ./x/${n}.js -> ./x/foo.ts
		case sufExt == "" && (strings.HasSuffix(stem, suffix) || strings.HasSuffix(stem, suffix+"/index")):
			add(f) // ./x/${n}/index -> ./x/foo/index.ts
		}
	}
}

// matchGlob adds files matching an import.meta.glob pattern. Negated patterns are
// ignored (over-approximation).
func (a *Analyzer) matchGlob(dir, pattern string, add func(string)) {
	pattern = cleanSpec(pattern)
	if pattern == "" || pattern[0] == '!' {
		return
	}
	static := pattern
	if i := strings.IndexAny(pattern, "*?[{"); i >= 0 {
		static = pattern[:i]
	}
	j := strings.LastIndexByte(static, '/')
	if j < 0 {
		return // Vite requires globs to start with ./, / or an alias
	}
	head, rest := pattern[:j+1], pattern[j+1:]
	for _, pre := range a.expandPrefix(dir, head, add) {
		full := escapeGlob(pre) + rest
		if !doublestar.ValidatePattern(full) {
			continue
		}
		for _, f := range a.under(pre) {
			if ok, _ := doublestar.Match(full, f); ok {
				add(f)
			}
		}
	}
}

// escapeGlob escapes glob metacharacters in a literal path (e.g. Next.js "[id]" dirs).
func escapeGlob(s string) string {
	if !strings.ContainsAny(s, "*?[]{}\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '*', '?', '[', ']', '{', '}', '\\':
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// under returns the sorted repo files starting with prefix.
func (a *Analyzer) under(prefix string) []string {
	fs := a.files()
	i := sort.SearchStrings(fs, prefix)
	j := i
	for j < len(fs) && strings.HasPrefix(fs[j], prefix) {
		j++
	}
	return fs[i:j]
}

// files returns all repo files, sorted.
func (a *Analyzer) files() []string {
	a.filesOnce.Do(func() {
		m := a.h.Files()
		a.sorted = make([]string, 0, len(m))
		for f := range m {
			a.sorted = append(a.sorted, f)
		}
		sort.Strings(a.sorted)
	})
	return a.sorted
}

// cleanSpec strips what bundlers allow around a specifier: webpack loader prefixes
// ("raw-loader!./x") and query strings ("./x.svg?raw").
func cleanSpec(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '!'); i >= 0 && !strings.HasPrefix(s, "!") {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	return s
}

// isRelative reports whether a specifier is relative to the importing file.
func isRelative(s string) bool {
	return s == "." || s == ".." || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

// isExternalScheme reports whether a specifier names something outside the repository
// (node:fs, bun:test, https://..., virtual modules).
func isExternalScheme(s string) bool {
	i := strings.IndexByte(s, ':')
	if i <= 0 {
		return false
	}
	scheme := s[:i]
	for j := 0; j < len(scheme); j++ {
		c := scheme[j]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

// outside reports whether a cleaned path escapes the repository.
func outside(p string) bool {
	return p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/")
}

// joinPrefix joins a relative path prefix (which may end in a partial file name) to
// base, keeping the partial name: joinPrefix("src", "./plugins/p-") = "src/plugins/p-".
func joinPrefix(base, prefix string) (string, bool) {
	i := strings.LastIndexByte(prefix, '/')
	d, name := prefix[:i+1], prefix[i+1:]
	jd := path.Join(base, d)
	if outside(jd) {
		return "", false
	}
	if jd == "." {
		return name, true
	}
	return jd + "/" + name, true
}

// hasSegment reports whether seg is one of the slash-separated segments of p.
func hasSegment(p, seg string) bool {
	for p != "" {
		var s string
		if i := strings.IndexByte(p, '/'); i >= 0 {
			s, p = p[:i], p[i+1:]
		} else {
			s, p = p, ""
		}
		if s == seg {
			return true
		}
	}
	return false
}

// parentDir returns the parent of a repo-relative directory ("." for top level).
func parentDir(dir string) string { return path.Dir(dir) }

// memo is a concurrency-safe string-keyed cache. Values are computed outside the lock,
// so computations may recurse into other memos (or other keys of the same one); a
// value computed twice concurrently is computed identically, and the first stored wins.
type memo[V any] struct {
	mu sync.Mutex
	m  map[string]V
}

func (c *memo[V]) get(key string, compute func() V) V {
	c.mu.Lock()
	v, ok := c.m[key]
	c.mu.Unlock()
	if ok {
		return v
	}
	v = compute()
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, ok := c.m[key]; ok {
		return prev
	}
	if c.m == nil {
		c.m = map[string]V{}
	}
	c.m[key] = v
	return v
}

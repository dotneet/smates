// Package host defines what language analyzers can see of the repository.
package host

// Host gives analyzers read access to the working tree. All paths are repo-relative
// with forward slashes.
type Host interface {
	// Root is the absolute repository root.
	Root() string
	// Has reports whether a file exists in the working tree (tracked or untracked,
	// not ignored).
	Has(path string) bool
	// Files returns every file in the working tree.
	Files() map[string]bool
	// Read returns a file's content (nil if unreadable).
	Read(path string) []byte
}

// Analyzer extracts direct dependencies of source files of one language.
type Analyzer interface {
	// Handles reports whether the analyzer understands this file.
	Handles(path string) bool
	// Deps returns repo-relative files that path directly depends on. It must be safe
	// for concurrent use.
	Deps(path string, src []byte) []string
}

// TypeAnalyzer is implemented by analyzers of languages whose type-only imports are
// erased before the code runs (TypeScript).
type TypeAnalyzer interface {
	Analyzer
	// SplitDeps returns Deps(path, src) and the subset of them that path still
	// depends on at run time, once its type-only imports are erased.
	SplitDeps(path string, src []byte) (all, runtime []string)
}

// ResolveReporter is implemented by analyzers that can tell which imports of a file
// resolve to nothing they know of.
type ResolveReporter interface {
	// Unresolved returns the specifiers path imports at run time that resolve to no
	// file of the repository nor to anything installed: what they load is unknown.
	// An import of a computed name is reported as DynamicSpec.
	Unresolved(path string, src []byte) []string
}

// Markers that stand for a specifier in Unresolved results: what the file loads is
// unknown, though no specifier names it.
const (
	// DynamicSpec: the file imports a module whose name is computed at run time
	// (import(name), require(p), a template literal without a static prefix).
	DynamicSpec = "<dynamic>"
	// UnreadableSpec: the file was not analyzed (too large, not a regular file, or
	// unreadable), so its imports are unknown.
	UnreadableSpec = "<unreadable>"
)

// Resolver is implemented by analyzers that resolve a module specifier on its own.
type Resolver interface {
	// Resolve returns the repository files spec, imported from path, resolves to.
	Resolve(path, spec string) []string
}

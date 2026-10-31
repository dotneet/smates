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

// Package depkey defines the dependency keys recorded for a test. A key is one of:
//
//	path      the content of a file
//	dir/      the entry names of a directory (it was listed)
//	path?     whether a path exists (it was probed, possibly before it existed)
//	dir/**    every file below a directory (a command smates cannot see into ran there)
//
// Paths are repo-relative slash paths; the repository root is ".".
package depkey

import "strings"

// Kind is what a key stands for.
type Kind int

const (
	KindContent   Kind = iota // path
	KindListing               // dir/
	KindExistence             // path?
	KindTree                  // dir/**
)

const (
	listingSuffix   = "/"
	existenceSuffix = "?"
	treeSuffix      = "/**"
)

// Listing is the key of a directory listing.
func Listing(dir string) string { return strings.TrimSuffix(dir, "/") + listingSuffix }

// Existence is the key of an existence check.
func Existence(p string) string { return strings.TrimSuffix(p, "/") + existenceSuffix }

// Tree is the key of every file below dir.
func Tree(dir string) string { return strings.TrimSuffix(dir, "/") + treeSuffix }

// Make is the key of kind for path p.
func Make(k Kind, p string) string {
	switch k {
	case KindListing:
		return Listing(p)
	case KindExistence:
		return Existence(p)
	case KindTree:
		return Tree(p)
	}
	return p
}

// Parse returns the kind of a key and the path it names (the directory of a listing
// or tree, the probed path of an existence check).
func Parse(key string) (Kind, string) {
	if dir, ok := strings.CutSuffix(key, treeSuffix); ok {
		return KindTree, dir
	}
	if p, ok := strings.CutSuffix(key, existenceSuffix); ok {
		return KindExistence, p
	}
	if dir, ok := strings.CutSuffix(key, listingSuffix); ok {
		return KindListing, dir
	}
	return KindContent, key
}

// KindOf returns the kind of a key.
func KindOf(key string) Kind {
	k, _ := Parse(key)
	return k
}

// IsPseudo reports whether a key stands for something other than a file's content (a
// listing, an existence check or a tree).
func IsPseudo(key string) bool { return KindOf(key) != KindContent }

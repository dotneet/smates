// Package depkey defines the dependency keys recorded for a test. A key is one of:
//
//	path      the content of a file
//	dir/      the entry names of a directory (it was listed)
//	path?     whether a path exists (it was probed, possibly before it existed)
//	dir/**    every file below a directory (a command smates cannot see into ran there)
//	$NAME     the value of an environment variable (it was read)
//	f.go#decls the declarations of a Go file: its source without the bodies of its
//	          functions and methods (Go per-function selection: a test depends on the
//	          bodies it executed and on the declarations of everything it could reach)
//
// Paths are repo-relative slash paths; the repository root is ".". A path at the
// repository root whose name starts with "$" cannot be told from an environment
// variable; such files are not expected in practice.
package depkey

import "strings"

// Kind is what a key stands for.
type Kind int

const (
	KindContent   Kind = iota // path
	KindListing               // dir/
	KindExistence             // path?
	KindTree                  // dir/**
	KindEnv                   // $NAME
	KindDecls                 // file.go#decls
)

const (
	listingSuffix   = "/"
	existenceSuffix = "?"
	treeSuffix      = "/**"
	envPrefix       = "$"
	declsSuffix     = "#decls"
)

// Decls is the key of the declarations of a Go file.
func Decls(p string) string { return p + declsSuffix }

// File returns the file a content or declarations key names, and false for any other
// kind of key.
func File(key string) (string, bool) {
	switch k, p := Parse(key); k {
	case KindContent, KindDecls:
		return p, true
	}
	return "", false
}

// Listing is the key of a directory listing.
func Listing(dir string) string { return strings.TrimSuffix(dir, "/") + listingSuffix }

// Existence is the key of an existence check.
func Existence(p string) string { return strings.TrimSuffix(p, "/") + existenceSuffix }

// Tree is the key of every file below dir.
func Tree(dir string) string { return strings.TrimSuffix(dir, "/") + treeSuffix }

// Env is the key of an environment variable.
func Env(name string) string { return envPrefix + name }

// Make is the key of kind for path p (the variable name for KindEnv).
func Make(k Kind, p string) string {
	switch k {
	case KindListing:
		return Listing(p)
	case KindExistence:
		return Existence(p)
	case KindTree:
		return Tree(p)
	case KindEnv:
		return Env(p)
	case KindDecls:
		return Decls(p)
	}
	return p
}

// Parse returns the kind of a key and the path it names (the directory of a listing
// or tree, the probed path of an existence check, the name of a variable).
func Parse(key string) (Kind, string) {
	if name, ok := strings.CutPrefix(key, envPrefix); ok {
		return KindEnv, name
	}
	if p, ok := strings.CutSuffix(key, declsSuffix); ok {
		return KindDecls, p
	}
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
// listing, an existence check, a tree, an environment variable or a Go file's
// declarations).
func IsPseudo(key string) bool { return KindOf(key) != KindContent }

// IsEnv reports whether a key is an environment variable.
func IsEnv(key string) bool { return strings.HasPrefix(key, envPrefix) }

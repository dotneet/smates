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
//	f.go#decl:N one top-level declaration of a Go file, body included: function or
//	          variable N, method T.N, type N (Go per-function selection: the bodies a
//	          test executed, the test-file declarations it reaches)
//	@set:ID   a set of files declared in smates.yml (depends_on/serves), hashed as one
//	f#suite   the settings of a suite in the configuration file f (smates.yml): what in
//	          it can change how the suite's tests run, not its whole content
//
// Paths are repo-relative slash paths; the repository root is ".". A path at the
// repository root whose name starts with "$" cannot be told from an environment
// variable, nor a file whose name ends in "#decls" or "#suite" from those keys; such
// files are not expected in practice. A file whose name ends in "?" parses as an
// existence check; the hasher (selector.Hasher) hashes such a key as both when the
// snapshot holds the file, so that editing it still selects the tests that read it.
//
// This package owns the syntax: other packages build keys with the constructors
// (Listing, Env, Config, ...), take them apart with Parse and ask for their properties
// (File, Display, Kind.ContentLike, Kind.Cover, Covers) instead of matching prefixes
// and suffixes themselves.
package depkey

import (
	"path"
	"strings"
)

// Kind is what a key stands for.
type Kind int

const (
	KindContent   Kind = iota // path
	KindListing               // dir/
	KindExistence             // path?
	KindTree                  // dir/**
	KindEnv                   // $NAME
	KindDecls                 // file.go#decls
	KindSet                   // @set:ID
	KindConfig                // smates.yml#suite
	KindDecl                  // file.go#decl:Name
)

var kindNames = [...]string{"content", "listing", "existence", "tree", "env", "decls", "set", "config", "decl"}

func (k Kind) String() string {
	if k >= 0 && int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "unknown"
}

// ContentLike reports whether a key of kind k stands for a file's content as far as
// users and comparisons go: the file itself, or a suite's settings in a configuration
// file (shown as that file, see Display). Every other kind is a pseudo key (IsPseudo).
func (k Kind) ContentLike() bool { return k == KindContent || k == KindConfig }

const (
	listingSuffix   = "/"
	existenceSuffix = "?"
	treeSuffix      = "/**"
	envPrefix       = "$"
	declsSuffix     = "#decls"
	declInfix       = "#decl:"
	setPrefix       = "@set:"
	configSuffix    = "#suite"
)

// Decls is the key of the declarations of a Go file.
func Decls(p string) string { return p + declsSuffix }

// Decl is the key of the top-level declaration name of Go file p ("T.M" for method M
// of type T).
func Decl(p, name string) string { return p + declInfix + name }

// DeclName splits a declaration key (Decl) into its file and declaration name.
func DeclName(key string) (file, name string, ok bool) {
	i := strings.LastIndex(key, declInfix)
	if i < 0 {
		return "", "", false
	}
	return key[:i], key[i+len(declInfix):], true
}

// File returns the file a content or declarations key names, and false for any other
// kind of key.
func File(key string) (string, bool) {
	switch k, p := Parse(key); k {
	case KindContent, KindDecls, KindDecl:
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

// Config is the key of a suite's settings in the configuration file at file (hashed
// as a fingerprint of those settings, not of the file). Reading the file itself
// records its content key.
func Config(file string) string { return file + configSuffix }

// Display is what a key is shown (and diffed) as: the configuration file for a
// settings key (Config), the key itself otherwise.
func Display(key string) string {
	if k, p := Parse(key); k == KindConfig {
		return p
	}
	return key
}

// Set is the key of a declared set of files (id names the declaration).
func Set(id string) string { return setPrefix + id }

// IsSet reports whether a key is a declared set of files.
func IsSet(key string) bool { return strings.HasPrefix(key, setPrefix) }

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
	case KindSet:
		return Set(p)
	case KindConfig:
		return Config(p)
	}
	return p
}

// Parse returns the kind of a key and the path it names (the directory of a listing
// or tree, the probed path of an existence check, the name of a variable, the id of a
// set, the configuration file of a settings key).
func Parse(key string) (Kind, string) {
	if id, ok := strings.CutPrefix(key, setPrefix); ok {
		return KindSet, id
	}
	if name, ok := strings.CutPrefix(key, envPrefix); ok {
		return KindEnv, name
	}
	if p, ok := strings.CutSuffix(key, declsSuffix); ok {
		return KindDecls, p
	}
	if p, _, ok := DeclName(key); ok {
		return KindDecl, p
	}
	if p, ok := strings.CutSuffix(key, configSuffix); ok {
		return KindConfig, p
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
// listing, an existence check, a tree, an environment variable, a declared set or a
// Go file's declarations or one of them); see Kind.ContentLike.
func IsPseudo(key string) bool { return !KindOf(key).ContentLike() }

// IsEnv reports whether a key is an environment variable.
func IsEnv(key string) bool { return strings.HasPrefix(key, envPrefix) }

// CoverKind is how a dependency key explains a change to a file (see Covers). Its
// values are the "kind" smates why reports.
type CoverKind string

const (
	CoverNone      CoverKind = ""
	CoverContent   CoverKind = "content"      // the file itself (or its suite settings)
	CoverDecls     CoverKind = "declarations" // the declarations of a Go file, or one of them
	CoverListing   CoverKind = "listing"      // the listing of the file's directory
	CoverExistence CoverKind = "existence"    // a check whether the file exists
	CoverTree      CoverKind = "tree"         // every file below one of its directories
)

// Cover is how a key of kind k explains a change to the file it is about: CoverNone
// for environment variables and declared sets (a set's files are matched through its
// declaration, not its key).
func (k Kind) Cover() CoverKind {
	switch k {
	case KindContent, KindConfig:
		return CoverContent
	case KindDecls, KindDecl:
		return CoverDecls
	case KindListing:
		return CoverListing
	case KindExistence:
		return CoverExistence
	case KindTree:
		return CoverTree
	}
	return CoverNone
}

// Covers reports how a dependency set explains a change to file f (CoverNone when it
// does not), trying the keys about f strongest first: its content or its suite
// settings; the declarations of Go file f or one of them (edits inside function
// bodies the test did not execute do not select it); a listing of its directory or an
// existence check, only when existenceChanged (they explain f appearing or
// disappearing but not edits: an edited file only listed or probed may still be read
// by something tracing cannot see); a tree (a command smates cannot see into ran below
// a directory containing f; a tree at the repository root explains nothing: it would
// hide every change from the orphan check). A key below f counts as content: a
// submodule, recorded as one entry for its commit, is explained by tests that read
// files inside it.
func Covers(deps map[string]bool, f string, existenceChanged bool) CoverKind {
	for _, k := range []Kind{KindContent, KindConfig, KindDecls} {
		if deps[Make(k, f)] {
			return k.Cover()
		}
	}
	prefix, decl := f+"/", f+declInfix
	below := false
	for d := range deps {
		if strings.HasPrefix(d, decl) {
			return CoverDecls
		}
		below = below || strings.HasPrefix(d, prefix)
	}
	if existenceChanged {
		if deps[Listing(path.Dir(f))] {
			return CoverListing
		}
		if deps[Existence(f)] {
			return CoverExistence
		}
	}
	for d := path.Dir(f); d != "." && d != "/"; d = path.Dir(d) {
		if deps[Tree(d)] {
			return CoverTree
		}
	}
	if below {
		return CoverContent
	}
	return CoverNone
}

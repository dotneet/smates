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
//	f.go#sig:N the same declaration without function bodies (Go per-function
//	          selection: a declaration of the package a test may reach by name)
//	dir/#shape what the Go files of a package directory contribute to every test of
//	          the package (init functions, initializers that call code, build
//	          constraints, blank imports: golang.PackageParts)
//	dir/#methods:T the method set of type T in a package directory
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
	"slices"
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
	KindSig                   // file.go#sig:Name
	KindShape                 // dir/#shape
	KindMethods               // dir/#methods:Type
)

var kindNames = [...]string{"content", "listing", "existence", "tree", "env", "decls", "set", "config", "decl", "sig", "shape", "methods"}

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
	sigInfix        = "#sig:"
	shapeSuffix     = "/#shape"
	methodsInfix    = "/#methods:"
	setPrefix       = "@set:"
	configSuffix    = "#suite"
)

// Decls is the key of the declarations of a Go file.
func Decls(p string) string { return p + declsSuffix }

// Decl is the key of the top-level declaration name of Go file p ("T.M" for method M
// of type T).
func Decl(p, name string) string { return p + declInfix + name }

// DeclName splits a declaration key (Decl or Sig) into its file and declaration name.
func DeclName(key string) (file, name string, ok bool) {
	for _, infix := range []string{declInfix, sigInfix} {
		if i := strings.LastIndex(key, infix); i >= 0 {
			return key[:i], key[i+len(infix):], true
		}
	}
	return "", "", false
}

// Sig is the key of the top-level declaration name of Go file p without the bodies of
// its functions.
func Sig(p, name string) string { return p + sigInfix + name }

// Shape is the key of what the Go files of package directory dir contribute to every
// test of the package.
func Shape(dir string) string { return Listing(dir) + "#shape" }

// Methods is the key of the method set of type typ in package directory dir.
func Methods(dir, typ string) string {
	return strings.TrimSuffix(Listing(dir), "/") + methodsInfix + typ
}

// MethodsType splits a method set key (Methods) into its directory and type.
func MethodsType(key string) (dir, typ string, ok bool) {
	i := strings.LastIndex(key, methodsInfix)
	if i < 0 {
		return "", "", false
	}
	return key[:i], key[i+len(methodsInfix):], true
}

// File returns the file a content or declarations key names, and false for any other
// kind of key.
func File(key string) (string, bool) {
	switch k, p := Parse(key); k {
	case KindContent, KindDecls, KindDecl, KindSig:
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
	// Every kind below up to KindTree has a "#" in its key; most keys are plain paths.
	if strings.IndexByte(key, '#') < 0 {
		return parsePath(key)
	}
	if p, ok := strings.CutSuffix(key, declsSuffix); ok {
		return KindDecls, p
	}
	if dir, ok := strings.CutSuffix(key, shapeSuffix); ok {
		return KindShape, dir
	}
	if dir, _, ok := MethodsType(key); ok {
		return KindMethods, dir
	}
	if i := strings.LastIndex(key, declInfix); i >= 0 {
		return KindDecl, key[:i]
	}
	if i := strings.LastIndex(key, sigInfix); i >= 0 {
		return KindSig, key[:i]
	}
	if p, ok := strings.CutSuffix(key, configSuffix); ok {
		return KindConfig, p
	}
	return parsePath(key)
}

// parsePath is Parse for a key that is no set, variable, Go declaration or settings
// key.
func parsePath(key string) (Kind, string) {
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
	case KindDecls, KindDecl, KindSig, KindShape, KindMethods:
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
	return covers(deps, f, existenceChanged, func(decl, sig, prefix string) (bool, bool) {
		below := false
		for d := range deps {
			if strings.HasPrefix(d, decl) || strings.HasPrefix(d, sig) {
				return true, below
			}
			below = below || strings.HasPrefix(d, prefix)
		}
		return false, below
	})
}

// CoverIndex answers Covers for one dependency set, many times: the keys that start
// with a prefix are found by binary search instead of a scan of the set.
type CoverIndex struct {
	deps map[string]bool
	keys []string // sorted
}

// NewCoverIndex indexes deps, which must not change while the index is used.
func NewCoverIndex(deps map[string]bool) *CoverIndex {
	keys := make([]string, 0, len(deps))
	for k := range deps {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return &CoverIndex{deps: deps, keys: keys}
}

// Len returns the number of keys indexed.
func (x *CoverIndex) Len() int { return len(x.keys) }

// Covers is Covers over the indexed set.
func (x *CoverIndex) Covers(f string, existenceChanged bool) CoverKind {
	return covers(x.deps, f, existenceChanged, func(decl, sig, prefix string) (bool, bool) {
		return x.hasPrefix(decl) || x.hasPrefix(sig), x.hasPrefix(prefix)
	})
}

// hasPrefix reports whether some key starts with p: the first key not before p does.
func (x *CoverIndex) hasPrefix(p string) bool {
	i, _ := slices.BinarySearch(x.keys, p)
	return i < len(x.keys) && strings.HasPrefix(x.keys[i], p)
}

// covers is Covers, with scan telling whether some key starts with a declaration
// prefix of f (decl or sig) and whether one is below f (prefix).
func covers(deps map[string]bool, f string, existenceChanged bool, scan func(decl, sig, prefix string) (declared, below bool)) CoverKind {
	for _, k := range []Kind{KindContent, KindConfig, KindDecls} {
		if deps[Make(k, f)] {
			return k.Cover()
		}
	}
	if strings.HasSuffix(f, ".go") && deps[Shape(path.Dir(f))] {
		return CoverDecls // a Go file of a package whose shape the test depends on
	}
	declared, below := scan(f+declInfix, f+sigInfix, f+"/")
	if declared {
		return CoverDecls
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

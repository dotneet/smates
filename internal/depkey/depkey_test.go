package depkey

import (
	"math/rand"
	"strings"
	"testing"
)

func TestKinds(t *testing.T) {
	for _, c := range []struct {
		key     string
		kind    Kind
		subject string
		build   string // the key built with the kind's constructor ("" = key)
		file    string // File ("" = none)
		display string // Display ("" = key)
		cover   CoverKind
	}{
		{key: "src/a.ts", kind: KindContent, subject: "src/a.ts", file: "src/a.ts", cover: CoverContent},
		{key: "fixtures/", kind: KindListing, subject: "fixtures", build: Listing("fixtures"), cover: CoverListing},
		{key: "./", kind: KindListing, subject: ".", build: Listing("."), cover: CoverListing},
		{key: "config/local.json?", kind: KindExistence, subject: "config/local.json", build: Existence("config/local.json"), cover: CoverExistence},
		{key: "scripts/**", kind: KindTree, subject: "scripts", build: Tree("scripts"), cover: CoverTree},
		{key: "./**", kind: KindTree, subject: ".", build: Tree("."), cover: CoverTree},
		{key: "$TZ", kind: KindEnv, subject: "TZ", build: Env("TZ"), cover: CoverNone},
		{key: "$WEIRD/NAME?", kind: KindEnv, subject: "WEIRD/NAME?", build: Env("WEIRD/NAME?"), cover: CoverNone},
		{key: "pkg/a.go#decls", kind: KindDecls, subject: "pkg/a.go", build: Decls("pkg/a.go"), file: "pkg/a.go", cover: CoverDecls},
		{key: "@set:suite:api", kind: KindSet, subject: "suite:api", build: Set("suite:api"), cover: CoverNone},
		// A suite's settings in smates.yml: not the file's content, shown as the file.
		{key: "smates.yml#suite", kind: KindConfig, subject: "smates.yml", build: Config("smates.yml"), display: "smates.yml", cover: CoverContent},
		{key: "sub/smates.yml#suite", kind: KindConfig, subject: "sub/smates.yml", build: Config("sub/smates.yml"), display: "sub/smates.yml", cover: CoverContent},
		// Only the exact suffixes make a declarations or settings key.
		{key: "pkg/a.go#declsx", kind: KindContent, subject: "pkg/a.go#declsx", file: "pkg/a.go#declsx", cover: CoverContent},
		{key: "smates.yml#suites", kind: KindContent, subject: "smates.yml#suites", file: "smates.yml#suites", cover: CoverContent},
	} {
		k, p := Parse(c.key)
		if k != c.kind || p != c.subject {
			t.Errorf("Parse(%q) = %v %q, want %v %q", c.key, k, p, c.kind, c.subject)
		}
		if KindOf(c.key) != c.kind {
			t.Errorf("KindOf(%q)", c.key)
		}
		if got := Make(k, p); got != c.key {
			t.Errorf("Make(%v, %q) = %q, want %q", k, p, got, c.key)
		}
		if c.build != "" && c.build != c.key {
			t.Errorf("constructor for %v: %q, want %q", c.kind, c.build, c.key)
		}
		f, ok := File(c.key)
		if f != c.file || ok != (c.file != "") {
			t.Errorf("File(%q) = %q %v, want %q", c.key, f, ok, c.file)
		}
		want := c.display
		if want == "" {
			want = c.key
		}
		if got := Display(c.key); got != want {
			t.Errorf("Display(%q) = %q, want %q", c.key, got, want)
		}
		contentLike := c.kind == KindContent || c.kind == KindConfig
		if k.ContentLike() != contentLike || IsPseudo(c.key) == contentLike {
			t.Errorf("ContentLike/IsPseudo(%q)", c.key)
		}
		if IsEnv(c.key) != (c.kind == KindEnv) || IsSet(c.key) != (c.kind == KindSet) {
			t.Errorf("IsEnv/IsSet(%q)", c.key)
		}
		if k.Cover() != c.cover {
			t.Errorf("Cover(%v) = %q, want %q", k, k.Cover(), c.cover)
		}
		if k.String() == "unknown" {
			t.Errorf("%v has no name", k)
		}
	}
	// A trailing slash on the path is not doubled.
	if Listing("dir/") != "dir/" || Existence("dir/") != "dir?" || Tree("dir/") != "dir/**" {
		t.Error("trailing slash")
	}
	if Kind(99).String() != "unknown" {
		t.Error("unknown kind")
	}
}

func TestCovers(t *testing.T) {
	const f = "a/b/c.go"
	for _, c := range []struct {
		key              string
		existenceChanged bool
		want             CoverKind
	}{
		{"a/b/c.go", false, CoverContent},
		{Config(f), false, CoverContent},
		{Decls(f), false, CoverDecls},
		{Listing("a/b"), false, CoverNone},
		{Listing("a/b"), true, CoverListing},
		{Existence(f), false, CoverNone},
		{Existence(f), true, CoverExistence},
		{Tree("a/b"), false, CoverTree},
		{Tree("a"), false, CoverTree},
		{Tree("."), false, CoverNone}, // a tree at the root explains nothing
		{Listing("a"), true, CoverNone},
		{Env("A"), true, CoverNone},
		{Set("suite:api"), true, CoverNone},
		{"a/b/c.go/x", false, CoverContent}, // inside a submodule at f
		{"a/b/c.gox", false, CoverNone},     // a prefix ends at a path boundary
	} {
		deps := map[string]bool{c.key: true}
		if got := Covers(deps, f, c.existenceChanged); got != c.want {
			t.Errorf("Covers(%q, %v) = %q, want %q", c.key, c.existenceChanged, got, c.want)
		}
		if got := NewCoverIndex(deps).Covers(f, c.existenceChanged); got != c.want {
			t.Errorf("CoverIndex.Covers(%q, %v) = %q, want %q", c.key, c.existenceChanged, got, c.want)
		}
	}
	// The strongest explanation wins.
	deps := map[string]bool{Tree("a"): true, Existence(f): true, Decls(f): true}
	if got := Covers(deps, f, true); got != CoverDecls {
		t.Errorf("strongest = %q", got)
	}
	// A file at the root is listed in "./".
	if got := Covers(map[string]bool{Listing("."): true}, "go.mod", true); got != CoverListing {
		t.Errorf("root listing = %q", got)
	}
}

func TestGoDeclarationKeys(t *testing.T) {
	for key, want := range map[string]struct {
		kind Kind
		p    string
	}{
		Decl("p/a.go", "T.M"):  {KindDecl, "p/a.go"},
		Sig("p/a.go", "F"):     {KindSig, "p/a.go"},
		Shape("p"):             {KindShape, "p"},
		Shape("."):             {KindShape, "."},
		Methods("p", "Server"): {KindMethods, "p"},
		Methods(".", "Server"): {KindMethods, "."},
		Decls("p/a.go"):        {KindDecls, "p/a.go"},
	} {
		if k, p := Parse(key); k != want.kind || p != want.p {
			t.Errorf("Parse(%q) = %v %q, want %v %q", key, k, p, want.kind, want.p)
		}
		if !IsPseudo(key) {
			t.Errorf("%q is not a file's content", key)
		}
	}
	if f, n, ok := DeclName(Sig("p/a.go", "F")); !ok || f != "p/a.go" || n != "F" {
		t.Errorf("DeclName(sig) = %q %q %v", f, n, ok)
	}
	if d, typ, ok := MethodsType(Methods("p", "T")); !ok || d != "p" || typ != "T" {
		t.Errorf("MethodsType = %q %q %v", d, typ, ok)
	}
	deps := map[string]bool{Shape("p"): true}
	if got := Covers(deps, "p/new.go", true); got != CoverDecls {
		t.Errorf("a Go file of a package with a shape dependency: %q", got)
	}
	if got := Covers(deps, "p/data.txt", true); got != CoverNone {
		t.Errorf("a data file of that package: %q", got)
	}
	if got := Covers(map[string]bool{Sig("p/a.go", "F"): true}, "p/a.go", false); got != CoverDecls {
		t.Errorf("a signature dependency: %q", got)
	}
}

// parseReference is Parse without its fast path for keys with no "#": every check in
// order.
func parseReference(key string) (Kind, string) {
	if id, ok := strings.CutPrefix(key, setPrefix); ok {
		return KindSet, id
	}
	if name, ok := strings.CutPrefix(key, envPrefix); ok {
		return KindEnv, name
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

// TestParseFastPath checks Parse against every check in order over keys built from
// every pair of the syntax's parts (with and without "#").
func TestParseFastPath(t *testing.T) {
	parts := []string{"", "a", "a/b.go", "#", "#decls", "/#shape", "/#methods:T", "#decl:F", "#sig:F", "#suite", "/**", "?", "/", "$", "@set:", "x#y", "#/"}
	for _, a := range parts {
		for _, b := range parts {
			for _, c := range parts {
				key := a + b + c
				k, p := Parse(key)
				wk, wp := parseReference(key)
				if k != wk || p != wp {
					t.Errorf("Parse(%q) = %v %q, want %v %q", key, k, p, wk, wp)
				}
			}
		}
	}
}

// TestCoverIndexMatchesCovers checks the index against the scan over sets mixing keys
// that sort around the prefixes of f (a sibling with a longer name, "-" and "." which
// sort before "/" and "#").
func TestCoverIndexMatchesCovers(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	files := []string{"a/b/c.go", "a/b/c.go.bak", "a/b/c-go", "a/b/c.gox", "a/b/c", "a/b", "a", "go.mod", "a/b/c.go/sub"}
	var pool []string
	for _, f := range files {
		pool = append(pool, f, Decls(f), Decl(f, "F"), Sig(f, "T.M"), Config(f), Listing(f), Existence(f), Tree(f), Shape(f), Methods(f, "T"), f+"-x", f+".x", f+"/x/y")
	}
	pool = append(pool, Env("A"), Set("suite:a"), Tree("."), Listing("."))
	for i := 0; i < 3000; i++ {
		deps := map[string]bool{}
		for k := r.Intn(6); k >= 0; k-- {
			deps[pool[r.Intn(len(pool))]] = true
		}
		x := NewCoverIndex(deps)
		for _, f := range files {
			for _, ex := range []bool{false, true} {
				if got, want := x.Covers(f, ex), Covers(deps, f, ex); got != want {
					t.Fatalf("CoverIndex.Covers(%q, %v) over %v = %q, want %q", f, ex, deps, got, want)
				}
			}
		}
	}
}

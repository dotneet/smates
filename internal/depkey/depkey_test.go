package depkey

import "testing"

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
		if got := Covers(map[string]bool{c.key: true}, f, c.existenceChanged); got != c.want {
			t.Errorf("Covers(%q, %v) = %q, want %q", c.key, c.existenceChanged, got, c.want)
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

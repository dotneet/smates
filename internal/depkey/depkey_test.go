package depkey

import "testing"

func TestParseAndMake(t *testing.T) {
	for _, c := range []struct {
		key  string
		kind Kind
		path string
	}{
		{"src/a.ts", KindContent, "src/a.ts"},
		{"fixtures/", KindListing, "fixtures"},
		{"./", KindListing, "."},
		{"config/local.json?", KindExistence, "config/local.json"},
		{"scripts/**", KindTree, "scripts"},
		{"./**", KindTree, "."},
		{"$TZ", KindEnv, "TZ"},
		{"$WEIRD/NAME?", KindEnv, "WEIRD/NAME?"},
		{"pkg/a.go#decls", KindDecls, "pkg/a.go"},
	} {
		k, p := Parse(c.key)
		if k != c.kind || p != c.path {
			t.Errorf("Parse(%q) = %v %q, want %v %q", c.key, k, p, c.kind, c.path)
		}
		if got := Make(k, p); got != c.key {
			t.Errorf("Make(%v, %q) = %q, want %q", k, p, got, c.key)
		}
		if IsPseudo(c.key) != (c.kind != KindContent) {
			t.Errorf("IsPseudo(%q)", c.key)
		}
		if IsEnv(c.key) != (c.kind == KindEnv) {
			t.Errorf("IsEnv(%q)", c.key)
		}
	}
	if f, ok := File("pkg/a.go#decls"); !ok || f != "pkg/a.go" {
		t.Errorf("File(decls) = %q %v", f, ok)
	}
	if f, ok := File("pkg/a.go"); !ok || f != "pkg/a.go" {
		t.Errorf("File(content) = %q %v", f, ok)
	}
	if _, ok := File("pkg/"); ok {
		t.Error("File(listing)")
	}
	// A trailing slash on the path is not doubled.
	if Listing("dir/") != "dir/" || Existence("dir/") != "dir?" || Tree("dir/") != "dir/**" {
		t.Error("trailing slash")
	}
}

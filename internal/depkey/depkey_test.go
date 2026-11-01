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
	}
	// A trailing slash on the path is not doubled.
	if Listing("dir/") != "dir/" || Existence("dir/") != "dir?" || Tree("dir/") != "dir/**" {
		t.Error("trailing slash")
	}
}

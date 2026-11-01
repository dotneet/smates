package depkey

import "testing"

func TestSetKey(t *testing.T) {
	// A set id may end like a tree or a listing; the prefix decides.
	for _, id := range []string{"suite:api", "paths:backend/**", "paths:a/,b?"} {
		k := Set(id)
		if kind, got := Parse(k); kind != KindSet || got != id || !IsSet(k) || !IsPseudo(k) || Make(KindSet, id) != k {
			t.Errorf("%q: %v %q", k, kind, got)
		}
	}
	if IsSet("src/@set.ts") || KindOf("@scope/pkg/a.ts") != KindContent {
		t.Error("paths are not sets")
	}
}

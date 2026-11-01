package state

import (
	"reflect"
	"testing"
)

func TestParseSnap(t *testing.T) {
	cases := []struct {
		in         string
		out        string // String() after parsing ("" = in)
		hash       string
		hasHash    bool
		unrecorded bool
		absent     bool
		seenHash   string
		seen       []string
	}{
		{in: "h1", hash: "h1", hasHash: true},
		{in: "", hasHash: true, absent: true},
		{in: "?", unrecorded: true},
		{in: "?h1@a,b", unrecorded: true, seenHash: "h1", seen: []string{"a", "b"}},
		{in: "?@a", unrecorded: true, seen: []string{"a"}},
		{in: "?@", unrecorded: true},
		{in: "?h1@", unrecorded: true, seenHash: "h1"},
		// Malformed markers read like before: no "@" means no suite saw it; names are
		// kept as written.
		{in: "?h1", out: "?h1@", unrecorded: true, seenHash: "h1"},
		{in: "?h1@b,a,a", unrecorded: true, seenHash: "h1", seen: []string{"b", "a", "a"}},
		{in: "?h1@a,,b", unrecorded: true, seenHash: "h1", seen: []string{"a", "", "b"}},
		{in: "?h@1@a", unrecorded: true, seenHash: "h", seen: []string{"1@a"}},
		{in: "??", out: "??@", unrecorded: true, seenHash: "?"},
		{in: "h?@a", hash: "h?@a", hasHash: true},
	}
	for _, c := range cases {
		v := ParseSnap(c.in)
		want := c.out
		if want == "" {
			want = c.in
		}
		if got := v.String(); got != want {
			t.Errorf("%q: String() = %q, want %q", c.in, got, want)
		}
		if again := ParseSnap(v.String()); !reflect.DeepEqual(again, v) && c.out == "" {
			t.Errorf("%q: round trip %#v != %#v", c.in, again, v)
		}
		if h, ok := v.Hash(); h != c.hash || ok != c.hasHash {
			t.Errorf("%q: Hash() = %q %v", c.in, h, ok)
		}
		if v.Unrecorded() != c.unrecorded || v.Absent() != c.absent {
			t.Errorf("%q: Unrecorded %v Absent %v", c.in, v.Unrecorded(), v.Absent())
		}
		if h, ok := v.SeenHash(); h != c.seenHash || ok != c.unrecorded {
			t.Errorf("%q: SeenHash() = %q %v", c.in, h, ok)
		}
		if got := v.SeenBy(); !reflect.DeepEqual(got, c.seen) {
			t.Errorf("%q: SeenBy() = %#v, want %#v", c.in, got, c.seen)
		}
		for _, s := range c.seen {
			if !v.Seen(s) {
				t.Errorf("%q: not Seen(%q)", c.in, s)
			}
		}
		if v.Seen("zz") || (len(c.seen) > 0 && v.SeenByAll([]string{c.seen[0], "zz"})) || !v.SeenByAll(c.seen) {
			t.Errorf("%q: Seen/SeenByAll", c.in)
		}
	}
}

func TestSnapConstructors(t *testing.T) {
	for _, c := range []struct {
		v    SnapValue
		want string
	}{
		{HashSnap("h1"), "h1"},
		{HashSnap(""), ""},
		{SnapValue{}, ""},
		{UnrecordedSnap(), "?"},
		{MarkerSnap("h1", []string{"b", "a", "a"}), "?h1@a,b"},
		{MarkerSnap("", []string{"a"}), "?@a"},
		{MarkerSnap("h1", nil), "?h1@"},
	} {
		if got := c.v.String(); got != c.want {
			t.Errorf("%#v: %q, want %q", c.v, got, c.want)
		}
		if got := ParseSnap(c.want); got.String() != c.want {
			t.Errorf("%q: reparsed as %q", c.want, got.String())
		}
	}
}

func TestSnapMarkSeen(t *testing.T) {
	m := MarkerSnap("h1", []string{"b", "a"})
	cases := []struct {
		v      SnapValue
		hash   string
		suites []string
		want   string
	}{
		{m, "h1", []string{"c"}, "?h1@a,b,c"},               // same hash: suites add up
		{m, "h1", []string{"a"}, "?h1@a,b"},                 // already seen
		{m, "h2", []string{"c"}, "?h2@c"},                   // new hash: start over
		{HashSnap("h0"), "", []string{"a"}, "?@a"},          // a content hash: start over
		{HashSnap("h1"), "h1", []string{"a"}, "?h1@a"},      // even for the same hash
		{UnrecordedSnap(), "", []string{"b", "a"}, "?@a,b"}, // "?" saw the absent file
		{UnrecordedSnap(), "h1", []string{"a"}, "?h1@a"},
		{ParseSnap("?h1@b,a"), "h1", []string{"c"}, "?h1@a,b,c"}, // unsorted on disk
	}
	for _, c := range cases {
		if got := c.v.MarkSeen(c.hash, c.suites).String(); got != c.want {
			t.Errorf("%q.MarkSeen(%q, %v) = %q, want %q", c.v, c.hash, c.suites, got, c.want)
		}
	}
	if m.String() != "?h1@a,b" {
		t.Errorf("MarkSeen changed its receiver: %q", m)
	}
}

func TestSnapCollapseIfComplete(t *testing.T) {
	cases := []struct {
		v      SnapValue
		owners []string
		want   string
		absent bool
	}{
		{MarkerSnap("h1", []string{"a", "b"}), []string{"a", "b"}, "h1", false},
		{MarkerSnap("h1", []string{"a", "b"}), []string{"b"}, "h1", false},
		{MarkerSnap("h1", []string{"a"}), []string{"a", "b"}, "?h1@a", false},
		{MarkerSnap("", []string{"a"}), []string{"a"}, "", true}, // absent everywhere
		{HashSnap("h1"), []string{"a"}, "h1", false},
		{UnrecordedSnap(), nil, "?", false},
	}
	for _, c := range cases {
		got := c.v.CollapseIfComplete(c.owners)
		if got.String() != c.want || got.Absent() != c.absent {
			t.Errorf("%q.CollapseIfComplete(%v) = %q (absent %v)", c.v, c.owners, got, got.Absent())
		}
	}
}

func TestStateSnap(t *testing.T) {
	s := &State{Files: map[string]string{"a": "h1", "b": "?h2@x", "c": "?", "e": ""}}
	for f, want := range map[string]struct {
		h  string
		ok bool
	}{"a": {"h1", true}, "b": {"", false}, "c": {"", false}, "d": {"", false}, "e": {"", true}} {
		if h, ok := s.FileHash(f); h != want.h || ok != want.ok {
			t.Errorf("FileHash(%s) = %q %v", f, h, ok)
		}
	}
	if v, ok := s.Snap("b"); !ok || !v.Seen("x") {
		t.Errorf("Snap(b) = %q %v", v, ok)
	}
	if _, ok := s.Snap("d"); ok {
		t.Error("Snap(d)")
	}
	s.SetSnap("a", MarkerSnap("h3", []string{"y"}))
	s.SetSnap("b", HashSnap(""))
	s.SetSnap("d", HashSnap("h4"))
	if want := map[string]string{"a": "?h3@y", "c": "?", "d": "h4", "e": ""}; !reflect.DeepEqual(s.Files, want) {
		t.Errorf("Files = %v", s.Files)
	}
	var empty State
	empty.SetSnap("a", HashSnap("h"))
	if empty.Files["a"] != "h" {
		t.Errorf("SetSnap on a nil map: %v", empty.Files)
	}
}

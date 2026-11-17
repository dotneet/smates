package agent

import "testing"

// modeChange names a change of the executable bit alone; the off-disk prefix of a
// skip-worktree file is not the executable marker.
func TestModeChange(t *testing.T) {
	const b = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name, old, cur, want string
	}{
		{"same", b, b, ""},
		{"x added", b, b + "+x", "mode changed (+x added)"},
		{"x removed", b + "+x", b, "mode changed (+x removed)"},
		{"both x", b + "+x", b + "+x", ""},
		{"off-disk now", b, "offdisk:" + b, ""},
		{"off-disk before", "offdisk:" + b, b, ""},
		{"off-disk x added", "offdisk:" + b, b + "+x", "mode changed (+x added)"},
		{"off-disk x kept", b + "+x", "offdisk:" + b + "+x", ""},
		{"content changed", b, "f" + b[1:] + "+x", ""},
		{"no old", "", b, ""},
	}
	for _, c := range cases {
		tb := &toolbox{root: t.TempDir(), orphans: map[string]Orphan{"f": {Path: "f", OldBlob: c.old, NewBlob: c.cur}}}
		if got := tb.modeChange("f"); got != c.want {
			t.Errorf("%s: modeChange(%q -> %q) = %q, want %q", c.name, c.old, c.cur, got, c.want)
		}
	}
}

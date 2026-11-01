package selector

import (
	"reflect"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
)

// A state written by a smates without the executable marker (or by an older smates
// still recording on CI) holds the plain blob of executable files: they, the trees and
// sets covering them, are unchanged while the blob is. Losing the bit is a change.
func TestSelectLegacyExecutableHashes(t *testing.T) {
	f := newFixture(t, map[string]string{
		"test/exec.test.js":  `run("scripts/run.sh")`,
		"test/tree.test.js":  `sh("scripts")`,
		"test/set.test.js":   `fetch("/api")`,
		"test/plain.test.js": `1`,
		"scripts/run.sh":     "#!/bin/sh\necho hi\n",
		"scripts/lib.sh":     "x=1\n",
	})
	su := suite(t, config.Suite{Name: "node", Runner: "node", Tests: []string{"test/*.test.js"}})
	su.DependsOn = []config.Dependency{{Paths: []string{"scripts/**"}, Tests: []string{"test/set.test.js"}}}
	f.suites = []*runner.Suite{su}
	tree := depkey.Tree("scripts")
	legacy := f.record(map[string][]string{"test/exec.test.js": {"scripts/run.sh"}, "test/tree.test.js": {tree}}, nil)
	var setKey string
	for k := range legacy.Suites["node"].Tests["test/set.test.js"].Deps {
		if depkey.IsSet(k) {
			setKey = k
		}
	}
	if setKey == "" {
		t.Fatalf("no set key recorded: %v", legacy.Suites["node"].Tests["test/set.test.js"].Deps)
	}
	blob := gitx.HashBytes([]byte(f.files["scripts/run.sh"]))
	withExec := func(in *Input) { in.Snap["scripts/run.sh"] = blob + gitx.ExecSuffix }
	all := map[string]string{"test/exec.test.js": "-unaffected", "test/tree.test.js": "-unaffected", "test/set.test.js": "-unaffected", "test/plain.test.js": "-unaffected"}

	// The same files, now hashed with the marker: nothing changed.
	res := f.sel(legacy, withExec)
	if d := decisions(res); !reflect.DeepEqual(d, all) || len(res.Changed) != 0 || len(res.Orphans) != 0 {
		t.Errorf("legacy state: %v changed %v orphans %v", d, res.Changed, res.Orphans)
	}

	// A state recorded with the marker; then the bit is removed.
	st := f.record(map[string][]string{"test/exec.test.js": {"scripts/run.sh"}, "test/tree.test.js": {tree}}, nil)
	snapX := f.snap()
	snapX["scripts/run.sh"] = blob + gitx.ExecSuffix
	h := NewHasher(snapX).WithConfig(f.cfg)
	st.Files["scripts/run.sh"] = blob + gitx.ExecSuffix
	st.Suites["node"].Tests["test/exec.test.js"].Deps["scripts/run.sh"] = blob + gitx.ExecSuffix
	st.Suites["node"].Tests["test/tree.test.js"].Deps[tree] = h.Hash(tree)
	if d := decisions(f.sel(st, withExec)); !reflect.DeepEqual(d, all) {
		t.Errorf("recorded with the marker: %v", d)
	}
	res = f.sel(st, nil) // chmod -x
	want := map[string]string{"test/exec.test.js": ReasonChanged, "test/tree.test.js": ReasonChanged, "test/set.test.js": "-unaffected", "test/plain.test.js": "-unaffected"}
	if d := decisions(res); !reflect.DeepEqual(d, want) || !reflect.DeepEqual(res.Changed, []string{"scripts/run.sh"}) {
		t.Errorf("chmod -x: %v changed %v", d, res.Changed)
	}

	// Content changes are changes whatever the state recorded.
	f.write("scripts/run.sh", "#!/bin/sh\necho bye\n")
	res = f.sel(legacy, func(in *Input) { in.Snap["scripts/run.sh"] += gitx.ExecSuffix })
	want["test/set.test.js"] = ReasonChanged
	if d := decisions(res); !reflect.DeepEqual(d, want) {
		t.Errorf("edited: %v", d)
	}
}

func TestHasherUnchangedLegacy(t *testing.T) {
	snap := gitx.Snapshot{"s/a.sh": "aa" + gitx.ExecSuffix, "s/b.txt": "bb"}
	h := NewHasher(snap)
	legacyTree := NewHasher(gitx.Snapshot{"s/a.sh": "aa", "s/b.txt": "bb"}).Hash(depkey.Tree("s"))
	for _, tc := range []struct {
		key, recorded string
		want          bool
	}{
		{"s/a.sh", "aa" + gitx.ExecSuffix, true},
		{"s/a.sh", "aa", true}, // recorded before the marker
		{"s/a.sh", "ab", false},
		{"s/b.txt", "bb" + gitx.ExecSuffix, false}, // lost the bit
		{"s/b.txt", "", false},
		{depkey.Tree("s"), h.Hash(depkey.Tree("s")), true},
		{depkey.Tree("s"), legacyTree, true},
		{depkey.Tree("s"), "tree:00", false},
	} {
		if got := h.Unchanged(tc.key, tc.recorded); got != tc.want {
			t.Errorf("Unchanged(%q, %q) = %v, want %v", tc.key, tc.recorded, got, tc.want)
		}
	}
}

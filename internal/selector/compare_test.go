package selector

import (
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/depkey"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/runner"
)

// TestDepUnchanged pins how a dependency key of an entry is compared with its current
// hash (Hasher.DepUnchanged, the one comparison decide makes): the hash itself
// (gitx.ByKey: also one recorded without the executable marker, not the index hash of
// a skip-worktree file now off disk), a tree or set an older smates summed without the
// marker, and the settings key an older smates recorded as the configuration file's
// hash or over the suite with its defaults.
func TestDepUnchanged(t *testing.T) {
	snap := gitx.Snapshot{
		"s/a.sh":      "aa" + gitx.ExecSuffix,
		"s/b.txt":     "bb",
		"fx/off.json": gitx.OffDisk("cc"),
		"smates.yml":  "yy",
	}
	cfg := &config.Config{Path: "smates.yml"}
	su := &runner.Suite{
		Suite:      config.Suite{Name: "py", Runner: "pytest", Command: ".venv/bin/python -m pytest"},
		Configured: config.Suite{Name: "py", Runner: "pytest"},
	}
	setKey := depkey.Set("paths:s/**")
	sets := []*ServiceDeps{{Key: setKey, Files: []string{"s/a.sh", "s/b.txt"}}}
	h := NewHasher(snap).WithConfig(cfg).ForSuite(su).WithSets(sets)
	// What a smates from before the executable marker recorded.
	old := NewHasher(gitx.Snapshot{"s/a.sh": "aa", "s/b.txt": "bb"}).WithSets(sets)
	// What a smates from before the off-disk hash recorded for a tree over it.
	oldOff := NewHasher(gitx.Snapshot{"fx/off.json": "cc"})
	settings := depkey.Config("smates.yml")
	for _, tc := range []struct {
		name     string
		key      string
		recorded map[string]string
		want     bool
	}{
		{"same hash", "s/b.txt", map[string]string{"s/b.txt": "bb"}, true},
		{"other hash", "s/b.txt", map[string]string{"s/b.txt": "b2"}, false},
		{"not recorded", "s/b.txt", map[string]string{}, false},
		{"recorded absent", "s/b.txt", map[string]string{"s/b.txt": ""}, false},
		{"absent, recorded absent", "s/gone.txt", map[string]string{"s/gone.txt": ""}, true},
		{"executable, recorded without the marker", "s/a.sh", map[string]string{"s/a.sh": "aa"}, true},
		{"lost the executable bit", "s/b.txt", map[string]string{"s/b.txt": "bb" + gitx.ExecSuffix}, false},
		{"off disk, recorded on disk", "fx/off.json", map[string]string{"fx/off.json": "cc"}, false},
		{"off disk, recorded off disk", "fx/off.json", map[string]string{"fx/off.json": gitx.OffDisk("cc")}, true},
		{"probe off disk, recorded present", depkey.Existence("fx/off.json"), map[string]string{depkey.Existence("fx/off.json"): "exists"}, false},
		{"tree", depkey.Tree("s"), map[string]string{depkey.Tree("s"): h.Hash(depkey.Tree("s"))}, true},
		{"tree without the marker", depkey.Tree("s"), map[string]string{depkey.Tree("s"): old.Hash(depkey.Tree("s"))}, true},
		{"tree changed", depkey.Tree("s"), map[string]string{depkey.Tree("s"): "tree:00"}, false},
		{"tree off disk, recorded by an older smates", depkey.Tree("fx"), map[string]string{depkey.Tree("fx"): oldOff.Hash(depkey.Tree("fx"))}, false},
		{"set", setKey, map[string]string{setKey: h.Hash(setKey)}, true},
		{"set without the marker", setKey, map[string]string{setKey: old.Hash(setKey)}, true},
		{"set changed", setKey, map[string]string{setKey: "set:00"}, false},
		{"settings", settings, map[string]string{settings: ConfigHash(cfg, &su.Configured)}, true},
		{"settings with the defaults (older smates)", settings, map[string]string{settings: ConfigHash(cfg, &su.Suite)}, true},
		{"settings changed", settings, map[string]string{settings: "config:00"}, false},
		{"settings recorded empty", settings, map[string]string{settings: ""}, false},
		{"settings as the file's hash (older smates)", settings, map[string]string{"smates.yml": "yy"}, true},
		{"settings as the file's old hash", settings, map[string]string{"smates.yml": "y0"}, false},
		{"settings as an absent file", settings, map[string]string{"smates.yml": ""}, false},
		{"settings not recorded", settings, map[string]string{}, false},
		{"variable unset, recorded unset", depkey.Env("SMATES_TEST_UNSET_VARIABLE"), map[string]string{depkey.Env("SMATES_TEST_UNSET_VARIABLE"): ""}, true},
	} {
		if got := h.DepUnchanged(tc.key, tc.recorded); got != tc.want {
			t.Errorf("%s: DepUnchanged(%q, %v) = %v, want %v", tc.name, tc.key, tc.recorded, got, tc.want)
		}
	}
}

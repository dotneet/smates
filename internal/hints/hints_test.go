package hints

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/state"
)

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func paths(cs []Candidate) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Path)
	}
	return out
}

// baseInput is a unit tests/cart.test.ts that imports src/cart.ts (which imports
// src/price.ts), reads fixtures/prices.json and depends on package.json.
func baseInput(cur map[string]string) Input {
	recorded := map[string]string{
		"tests/cart.test.ts":   "t1",
		"src/cart.ts":          "c1",
		"src/price.ts":         "p1",
		"src/util/fmt.ts":      "f1",
		"fixtures/prices.json": "j1",
		"package.json":         "k1",
		"src/":                 "dir:1",
	}
	hash := func(k string) string {
		if v, ok := cur[k]; ok {
			return v
		}
		return recorded[k]
	}
	return Input{
		Suite: "web", Unit: "tests/cart.test.ts",
		TestFiles: []string{"tests/cart.test.ts"},
		Deps:      []string{"fixtures/prices.json", "package.json", "src/", "src/cart.ts", "src/price.ts", "src/util/fmt.ts", "tests/cart.test.ts"},
		Static:    set("tests/cart.test.ts", "src/cart.ts", "src/price.ts", "src/util/fmt.ts"),
		Inputs:    set("package.json"),
		Entry:     &state.Entry{Deps: recorded, Status: state.Pass, Commit: "abcdef123456"},
		Hash:      hash,
		Direct: func(f string) []string {
			if f == "tests/cart.test.ts" {
				return []string{"src/cart.ts"}
			}
			return nil
		},
	}
}

func TestRankingByTier(t *testing.T) {
	in := baseInput(map[string]string{
		"tests/cart.test.ts":   "t2",
		"src/cart.ts":          "c2",
		"src/price.ts":         "p2",
		"fixtures/prices.json": "j2",
		"package.json":         "k2",
		"src/":                 "dir:2",
	})
	h := Compute(in)
	if h.Verdict != VerdictChanged || h.Basis != BasisLastRun || h.Since != "abcdef123456" || h.Changed != 6 || h.Inputs != 7 {
		t.Fatalf("hint %+v", h)
	}
	want := []string{"src/cart.ts", "src/price.ts", "tests/cart.test.ts", "fixtures/prices.json", "package.json", "src/"}
	if got := paths(h.Candidates); !reflect.DeepEqual(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
	kinds := map[string]string{}
	links := map[string]string{}
	for _, c := range h.Candidates {
		kinds[c.Path], links[c.Path] = c.Kind, c.Link
	}
	if kinds["src/cart.ts"] != KindSource || links["src/cart.ts"] != "direct import" || links["src/price.ts"] != "static import" ||
		kinds["tests/cart.test.ts"] != KindTest || kinds["fixtures/prices.json"] != KindData || links["fixtures/prices.json"] != "traced" ||
		kinds["package.json"] != KindConfig || kinds["src/"] != KindListing {
		t.Errorf("kinds %v links %v", kinds, links)
	}
}

func TestOnlyChangedInputs(t *testing.T) {
	in := baseInput(map[string]string{"src/util/fmt.ts": "f2"})
	h := Compute(in)
	if got := paths(h.Candidates); !reflect.DeepEqual(got, []string{"src/util/fmt.ts"}) || h.Candidates[0].Change != "modified" {
		t.Errorf("candidates %+v", h.Candidates)
	}
}

func TestAddedAndDeleted(t *testing.T) {
	in := baseInput(map[string]string{"src/price.ts": ""})
	in.Entry.Deps["src/new.ts"] = ""
	in.Deps = append(in.Deps, "src/new.ts")
	in.Hash = func(k string) string {
		switch k {
		case "src/price.ts":
			return ""
		case "src/new.ts":
			return "n1"
		}
		return in.Entry.Deps[k]
	}
	h := Compute(in)
	changes := map[string]string{}
	for _, c := range h.Candidates {
		changes[c.Path] = c.Change
	}
	if !reflect.DeepEqual(changes, map[string]string{"src/price.ts": "deleted", "src/new.ts": "added"}) {
		t.Errorf("changes %v", changes)
	}
}

func TestNewInputCountsOnlyWhenChanged(t *testing.T) {
	// The failing run traced a file the recorded entry did not have: it is a candidate
	// only when it changed in the diff (otherwise the edit that made the test use it is).
	in := baseInput(map[string]string{"src/cart.ts": "c2"})
	in.Observed = set("src/extra.ts", "src/other.ts")
	in.Changed = set("src/other.ts")
	h := Compute(in)
	if got := paths(h.Candidates); !reflect.DeepEqual(got, []string{"src/cart.ts", "src/other.ts"}) {
		t.Errorf("candidates %v", got)
	}
	if h.Inputs != 9 {
		t.Errorf("inputs %d", h.Inputs)
	}
}

func TestNoChangedInputs(t *testing.T) {
	h := Compute(baseInput(nil))
	if h.Verdict != VerdictUnchanged || len(h.Candidates) != 0 || h.LastStatus != state.Pass {
		t.Errorf("hint %+v", h)
	}
}

func TestDiffBasisWithoutEntry(t *testing.T) {
	in := baseInput(nil)
	in.Entry = nil
	in.Changed = set("src/price.ts", "README.md")
	h := Compute(in)
	if h.Basis != BasisDiff || h.Verdict != VerdictChanged || !reflect.DeepEqual(paths(h.Candidates), []string{"src/price.ts"}) {
		t.Errorf("hint %+v", h)
	}
}

func TestNoRecord(t *testing.T) {
	in := baseInput(nil)
	in.Entry = nil
	h := Compute(in)
	if h.Verdict != VerdictNoRecord || h.Basis != BasisNone || h.Candidates == nil {
		t.Errorf("hint %+v", h)
	}
}

func TestTruncation(t *testing.T) {
	in := baseInput(nil)
	in.Entry.Deps = map[string]string{}
	in.Deps = nil
	for i := range 25 {
		f := filepath.ToSlash(filepath.Join("src", "m", string(rune('a'+i))+".ts"))
		in.Deps = append(in.Deps, f)
		in.Entry.Deps[f] = "old"
	}
	in.Hash = func(string) string { return "new" }
	h := Compute(in)
	if h.Changed != 25 || len(h.Candidates) != MaxCandidates || h.Candidates[0].Path != "src/m/a.ts" {
		t.Errorf("changed %d, candidates %v", h.Changed, paths(h.Candidates))
	}
}

func TestRecencyWithinTier(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"src/cart.ts", "src/price.ts", "src/util/fmt.ts"} {
		p := filepath.Join(root, filepath.FromSlash(f))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(root, "src/price.ts"), old, old)
	in := baseInput(map[string]string{"src/price.ts": "p2", "src/util/fmt.ts": "f2"})
	in.Root = root
	h := Compute(in)
	// Both are transitive sources: the more recently modified one ranks first.
	if got := paths(h.Candidates); !reflect.DeepEqual(got, []string{"src/util/fmt.ts", "src/price.ts"}) {
		t.Errorf("order %v", got)
	}
}

func TestClassification(t *testing.T) {
	for p, want := range map[string]bool{
		"vitest.config.ts": true, "web/tsconfig.app.json": true, "go.sum": true, ".env.local": true,
		"src/config.ts": false, "fixtures/a.json": false,
	} {
		if isConfig(p) != want {
			t.Errorf("isConfig(%s) = %v", p, !want)
		}
	}
	for p, want := range map[string]bool{"a.py": true, "b.GO": true, "c.json": false, "d.sql": true} {
		if isCode(p) != want {
			t.Errorf("isCode(%s) = %v", p, !want)
		}
	}
}

func TestUnattributedChanges(t *testing.T) {
	in := baseInput(nil)
	in.Orphans = map[string]string{"scripts/seed.sh": "s1", "db/schema.sql": "q1"}
	in.Hash = func(k string) string {
		switch k {
		case "scripts/seed.sh":
			return "s2"
		case "db/schema.sql":
			return ""
		}
		return in.Entry.Deps[k]
	}
	h := Compute(in)
	if h.Verdict != VerdictUnattributed || h.Changed != 0 || len(h.Candidates) != 2 {
		t.Fatalf("hint %+v", h)
	}
	for _, c := range h.Candidates {
		if c.Link != "unattributed" || (c.Path == "db/schema.sql") != (c.Change == "deleted") {
			t.Errorf("candidate %+v", c)
		}
	}
	// Known inputs that changed take precedence over unattributed files.
	in.Hash = func(k string) string {
		if k == "src/price.ts" {
			return "p2"
		}
		return in.Entry.Deps[k]
	}
	if h := Compute(in); h.Verdict != VerdictChanged || !reflect.DeepEqual(paths(h.Candidates), []string{"src/price.ts"}) {
		t.Errorf("hint %+v", h)
	}
}

func TestCleanOutput(t *testing.T) {
	if got := CleanOutput("\n \x1b[31mFAIL\x1b[39m x\n"); got != "FAIL x" {
		t.Errorf("%q", got)
	}
}

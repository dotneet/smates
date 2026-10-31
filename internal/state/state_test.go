package state

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func sample() *State {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := New()
	s.Tool, s.Commit, s.Created = "smates/test", "abc123", t0
	s.Files = map[string]string{"src/a.ts": "h1", "src/b.ts": "h2", "data/x.json": "h1"}
	su := s.Suite("web")
	su.Tests["src/a.test.ts"] = &Entry{Deps: map[string]string{"src/a.ts": "h1", "src/a.test.ts": "h3", "data/": "dir:abc"}, Status: Pass, DurationMs: 12, Commit: "abc123", At: t0}
	su.Tests["src/b.test.ts"] = &Entry{Deps: map[string]string{"src/b.ts": "h2", "gone.txt": ""}, Status: Fail, At: t0.Add(time.Second)}
	s.Suite("empty")
	s.AgentCache["k1"] = json.RawMessage(`{"decision":"x"}`)
	return s
}

func TestRoundTrip(t *testing.T) {
	for _, gz := range []bool{false, true} {
		var buf bytes.Buffer
		in := sample()
		if err := in.Write(&buf, gz); err != nil {
			t.Fatal(err)
		}
		if gz {
			if _, err := gzip.NewReader(bytes.NewReader(buf.Bytes())); err != nil {
				t.Fatalf("gz output is not gzip: %v", err)
			}
		} else if !json.Valid(buf.Bytes()) {
			t.Fatalf("plain output is not JSON")
		}
		out, err := Read(&buf, gz)
		if err != nil {
			t.Fatalf("gz=%v: %v", gz, err)
		}
		if !reflect.DeepEqual(in.Files, out.Files) {
			t.Errorf("gz=%v files: %v != %v", gz, out.Files, in.Files)
		}
		if out.Tool != in.Tool || out.Commit != in.Commit || !out.Created.Equal(in.Created) {
			t.Errorf("gz=%v header mismatch: %+v", gz, out)
		}
		for name, su := range in.Suites {
			osu := out.Suites[name]
			if osu == nil {
				t.Fatalf("gz=%v suite %s lost", gz, name)
			}
			if len(osu.Tests) != len(su.Tests) {
				t.Fatalf("gz=%v suite %s: %d tests, want %d", gz, name, len(osu.Tests), len(su.Tests))
			}
			for id, e := range su.Tests {
				oe := osu.Tests[id]
				if oe == nil || !reflect.DeepEqual(e.Deps, oe.Deps) || e.Status != oe.Status || e.DurationMs != oe.DurationMs || e.Commit != oe.Commit || !e.At.Equal(oe.At) {
					t.Errorf("gz=%v %s/%s: got %+v want %+v", gz, name, id, oe, e)
				}
			}
		}
		if string(out.AgentCache["k1"]) != `{"decision":"x"}` {
			t.Errorf("agent cache: %s", out.AgentCache["k1"])
		}
	}
}

func TestInterning(t *testing.T) {
	var buf bytes.Buffer
	if err := sample().Write(&buf, false); err != nil {
		t.Fatal(err)
	}
	var d diskState
	if err := json.Unmarshal(buf.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Version != Version {
		t.Errorf("version %d", d.Version)
	}
	seen := map[string]bool{}
	for _, p := range d.Paths {
		if seen[p] {
			t.Errorf("path %q interned twice", p)
		}
		seen[p] = true
	}
	seenB := map[string]bool{}
	for _, b := range d.Blobs {
		if seenB[b] {
			t.Errorf("blob %q interned twice", b)
		}
		seenB[b] = true
	}
	// src/a.ts appears in the snapshot and in deps: one path; h1 used three times: one blob.
	if !seen["src/a.ts"] || !seenB["h1"] {
		t.Fatalf("missing interned values: %v %v", d.Paths, d.Blobs)
	}
	// Snapshot paths are interned first, in sorted order.
	want := []string{"data/x.json", "src/a.ts", "src/b.ts"}
	if !reflect.DeepEqual(d.Paths[:3], want) {
		t.Errorf("snapshot paths first: %v", d.Paths)
	}
}

func TestEmptyStateWritesArrays(t *testing.T) {
	var buf bytes.Buffer
	if err := New().Write(&buf, false); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if !strings.Contains(s, `"paths":[]`) || !strings.Contains(s, `"blobs":[]`) {
		t.Errorf("empty arrays expected: %s", s)
	}
	st, err := Read(strings.NewReader(s), false)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Empty() || st.Files == nil || st.Suites == nil || st.AgentCache == nil {
		t.Errorf("empty state round trip: %+v", st)
	}
}

func TestReadErrors(t *testing.T) {
	cases := []struct {
		name string
		data string
		gz   bool
		want string
	}{
		{"not json", "{not json", false, "unreadable state"},
		{"not gzip", `{"version":1}`, true, "unreadable state"},
		{"version mismatch", `{"version":2,"paths":[],"blobs":[]}`, false, "unsupported state version 2"},
		{"version missing", `{"paths":[],"blobs":[]}`, false, "unsupported state version 0"},
		{"files path index out of range", `{"version":1,"paths":["a"],"blobs":["b"],"files":[[1,0]]}`, false, "index out of range"},
		{"files blob index out of range", `{"version":1,"paths":["a"],"blobs":["b"],"files":[[0,1]]}`, false, "index out of range"},
		{"negative index", `{"version":1,"paths":["a"],"blobs":["b"],"files":[[-1,0]]}`, false, "index out of range"},
		{"dep index out of range", `{"version":1,"paths":["a"],"blobs":["b"],"files":[],"suites":{"s":{"tests":{"t":{"deps":[[0,5]],"status":"pass"}}}}}`, false, "index out of range"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Read(strings.NewReader(c.data), c.gz)
			if err == nil || !strings.Contains(err.Error(), c.want) || !errors.Is(err, ErrUnreadable) {
				t.Fatalf("err = %v, want %q (ErrUnreadable)", err, c.want)
			}
		})
	}
}

func TestTruncatedGzip(t *testing.T) {
	var buf bytes.Buffer
	if err := sample().Write(&buf, true); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()[:buf.Len()/2]
	if _, err := Read(bytes.NewReader(data), true); err == nil {
		t.Fatal("truncated gzip state must not load")
	}
}

func TestLoadSave(t *testing.T) {
	dir := t.TempDir()
	// Missing file: empty state, no error.
	st, err := Load(filepath.Join(dir, "nope", "state.json.gz"))
	if err != nil || !st.Empty() {
		t.Fatalf("missing file: %v %v", st, err)
	}
	for _, name := range []string{"sub/dir/state.json.gz", "plain/state.json"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		in := sample()
		if err := in.Save(p); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(p)
		isGz := len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b
		if isGz != strings.HasSuffix(name, ".gz") {
			t.Errorf("%s: gzip=%v", name, isGz)
		}
		out, err := Load(p)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(out.Suites["web"].Tests["src/a.test.ts"].Deps, in.Suites["web"].Tests["src/a.test.ts"].Deps) {
			t.Errorf("%s: round trip mismatch", name)
		}
		// No temp files left behind.
		ents, _ := os.ReadDir(filepath.Dir(p))
		if len(ents) != 1 {
			t.Errorf("%s: leftover files %v", name, ents)
		}
	}
	// Corrupt file: error (not silently empty).
	bad := filepath.Join(dir, "bad.json.gz")
	os.WriteFile(bad, []byte("garbage"), 0o644)
	if _, err := Load(bad); err == nil {
		t.Error("corrupt file must fail to load")
	}
}

func TestEmpty(t *testing.T) {
	s := New()
	if !s.Empty() {
		t.Fatal("new state not empty")
	}
	s.Suite("a")
	s.Files["x"] = "y"
	if !s.Empty() {
		t.Fatal("suites without tests count as empty")
	}
	s.Suite("a").Tests["t"] = &Entry{}
	if s.Empty() {
		t.Fatal("state with a test is not empty")
	}
}

func TestSuiteCreates(t *testing.T) {
	s := New()
	a := s.Suite("a")
	if a == nil || a.Tests == nil || s.Suite("a") != a {
		t.Fatal("Suite must create once and return the same suite")
	}
}

func TestMerge(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	mk := func(created time.Time, commit string) *State {
		s := New()
		s.Created, s.Commit = created, commit
		s.Files = map[string]string{"f": commit}
		return s
	}

	t.Run("newer other replaces snapshot", func(t *testing.T) {
		a, b := mk(t0, "old"), mk(t1, "new")
		a.Merge(b)
		if a.Commit != "new" || a.Files["f"] != "new" || !a.Created.Equal(t1) {
			t.Errorf("got %+v", a)
		}
	})
	t.Run("older other keeps snapshot", func(t *testing.T) {
		a, b := mk(t1, "new"), mk(t0, "old")
		a.Merge(b)
		if a.Commit != "new" || a.Files["f"] != "new" || !a.Created.Equal(t1) {
			t.Errorf("got %+v", a)
		}
	})
	t.Run("entries newest wins", func(t *testing.T) {
		a, b := mk(t0, "a"), mk(t0, "b")
		a.Suite("s").Tests["keep-ours"] = &Entry{Status: Pass, At: t1}
		b.Suite("s").Tests["keep-ours"] = &Entry{Status: Fail, At: t0}
		a.Suite("s").Tests["take-theirs"] = &Entry{Status: Pass, At: t0}
		b.Suite("s").Tests["take-theirs"] = &Entry{Status: Fail, At: t1}
		a.Suite("s").Tests["tie"] = &Entry{Status: Pass, At: t0}
		b.Suite("s").Tests["tie"] = &Entry{Status: Fail, At: t0}
		b.Suite("s").Tests["only-theirs"] = &Entry{Status: Pass, At: t0}
		a.Suite("s").Tests["only-ours"] = &Entry{Status: Pass, At: t0}
		b.Suite("other").Tests["x"] = &Entry{Status: Pass, At: t0}
		a.AgentCache["k"] = json.RawMessage(`1`)
		b.AgentCache["k"] = json.RawMessage(`2`)
		b.AgentCache["k2"] = json.RawMessage(`3`)
		a.Merge(b)
		st := a.Suites["s"].Tests
		check := map[string]string{"keep-ours": Pass, "take-theirs": Fail, "tie": Pass, "only-theirs": Pass, "only-ours": Pass}
		for id, want := range check {
			if st[id] == nil || st[id].Status != want {
				t.Errorf("%s: %+v want %s", id, st[id], want)
			}
		}
		if a.Suites["other"] == nil || a.Suites["other"].Tests["x"] == nil {
			t.Error("suite from other not merged")
		}
		if string(a.AgentCache["k"]) != "2" || string(a.AgentCache["k2"]) != "3" {
			t.Errorf("agent cache %v", a.AgentCache)
		}
	})
	t.Run("merge into loaded state", func(t *testing.T) {
		var buf bytes.Buffer
		mk(t0, "a").Write(&buf, false)
		a, err := Read(&buf, false)
		if err != nil {
			t.Fatal(err)
		}
		b := mk(t1, "b")
		b.AgentCache["k"] = json.RawMessage(`1`)
		a.Merge(b) // must not panic on maps restored from disk
		if a.Commit != "b" || string(a.AgentCache["k"]) != "1" {
			t.Errorf("got %+v", a)
		}
	})
}

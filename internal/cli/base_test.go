package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/state"
)

// TestSelectBaseWithState checks which revision the tests a recorded state lacks are
// judged against: locally the base revision, in CI none (they are new) unless --base
// names one.
func TestSelectBaseWithState(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	g := newGitRepo(t)
	write := func(rel, content string) {
		p := filepath.Join(g.dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("smates.yml", "version: 1\norphans: all\nsuites:\n  - name: go\n    runner: go\n    command: \"true\"\n") // runs nothing: only the selection matters
	write("go.mod", "module example.com/m\n\ngo 1.22\n")
	for _, p := range []string{"rec", "unrec", "edited"} {
		write(p+"/"+p+".go", "package "+p+"\n\nfunc F() int { return 1 }\n")
		write(p+"/"+p+"_test.go", "package "+p+"\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {}\n")
	}
	g.git("add", "-A")
	base := g.commit("fixture")
	// A feature branch: on the default branch a CI build runs (and selects) everything.
	g.git("checkout", "-q", "-b", "topic")
	write("edited/edited.go", "package edited\n\nfunc F() int { return 2 }\n")
	g.git("add", "-A")
	g.commit("edit")

	// Record the snapshot (the command runs nothing), then give only rec an entry.
	var out, errb bytes.Buffer
	if code := Main([]string{"run", "-C", g.dir, "--no-agent", "--all"}, &out, &errb); code != 0 {
		t.Fatalf("run --all: %d %s", code, errb.String())
	}
	statePath := filepath.Join(g.dir, ".smates", "state.json.gz")
	st, err := state.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	deps := map[string]string{}
	for _, f := range []string{"rec/rec.go", "rec/rec_test.go", "go.mod", "smates.yml"} {
		deps[f] = st.Files[f]
	}
	st.Suites["go"] = &state.Suite{Tests: map[string]*state.Entry{"rec": {Deps: deps, Status: state.Pass}}}
	if err := st.Save(statePath); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		ci   bool
		base string
		want map[string]string // unit -> reason ("-" prefix: not selected)
	}{
		// Must keep: in CI with no --base, a test the state lacks runs as new.
		{"ci without --base", true, "", map[string]string{"rec": "-unaffected", "unrec": "new", "edited": "new"}},
		{"ci with --base", true, base, map[string]string{"rec": "-unaffected", "unrec": "-unaffected", "edited": "diff"}},
		{"local with --base", false, base, map[string]string{"rec": "-unaffected", "unrec": "-unaffected", "edited": "diff"}},
		// --base HEAD: nothing changed since then, so no unrecorded test is affected.
		{"ci with --base HEAD", true, "HEAD", map[string]string{"rec": "-unaffected", "unrec": "-unaffected", "edited": "-unaffected"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.ci {
				t.Setenv("CI", "true")
			} else {
				t.Setenv("CI", "")
			}
			args := []string{"select", "-C", g.dir, "--no-agent", "--format", "json"}
			if c.base != "" {
				args = append(args, "--base", c.base)
			}
			var out, errb bytes.Buffer
			if code := Main(args, &out, &errb); code != 0 {
				t.Fatalf("select: %d %s", code, errb.String())
			}
			var js struct {
				Suites []struct {
					Tests []struct {
						ID       string `json:"id"`
						Selected bool   `json:"selected"`
						Reason   string `json:"reason"`
					} `json:"tests"`
				} `json:"suites"`
			}
			if err := json.Unmarshal(out.Bytes(), &js); err != nil {
				t.Fatalf("%v\n%s", err, out.String())
			}
			got := map[string]string{}
			for _, tt := range js.Suites[0].Tests {
				r := tt.Reason
				if !tt.Selected {
					r = "-" + r
				}
				got[tt.ID] = r
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestSelectUnknownBase: an explicit --base that names no commit is a usage error (2),
// or, in a shallow clone that lacks it, an environment error (4) saying to fetch it;
// never "tests failed" (1), in CI or locally, with a recorded state or without.
func TestSelectUnknownBase(t *testing.T) {
	clearCI(t)
	noAgentCredentials(t)
	g := newGitRepo(t)
	writeTree(t, g.dir, map[string]string{
		"smates.yml":  "version: 1\norphans: all\nsuites:\n  - name: go\n    runner: go\n    command: \"true\"\n",
		"go.mod":      "module example.com/m\n\ngo 1.22\n",
		"a/a.go":      "package a\n",
		"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
	})
	g.git("add", "-A")
	g.commit("fixture")
	g.git("checkout", "-q", "-b", "topic") // not the default branch, where CI runs everything
	writeTree(t, g.dir, map[string]string{"a/a.go": "package a\n\n// edited\n"})
	g.git("add", "-A")
	g.commit("edit")
	// A shallow clone of the last commit.
	shallow := filepath.Join(t.TempDir(), "shallow")
	if out, err := exec.Command("git", "clone", "-q", "--depth", "1", "--branch", "topic", "file://"+g.dir, shallow).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	recorded := func(dir string) {
		var out, errb bytes.Buffer
		if code := Main([]string{"run", "-C", dir, "--no-agent", "--all"}, &out, &errb); code != 0 {
			t.Fatalf("run --all: %d %s", code, errb.String())
		}
	}
	cases := []struct {
		name  string
		dir   string
		ci    bool
		state bool
		base  string
		code  int
		msg   string
	}{
		{"ci with state", g.dir, true, true, "no-such-ref", ExitUsage, "--base no-such-ref: no such revision"},
		{"local with state", g.dir, false, true, "no-such-ref", ExitUsage, "no such revision"},
		{"no state", g.dir, false, false, "origin/nope", ExitUsage, "no such revision"},
		{"shallow clone in ci", shallow, true, true, "HEAD~1", ExitEnv, "fetch it"},
		{"shallow clone, base present", shallow, true, true, "HEAD", ExitOK, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			os.RemoveAll(filepath.Join(c.dir, ".smates"))
			if c.state {
				recorded(c.dir)
			}
			if c.ci {
				t.Setenv("CI", "true")
			} else {
				t.Setenv("CI", "")
			}
			var out, errb bytes.Buffer
			code := Main([]string{"select", "-C", c.dir, "--no-agent", "--base", c.base}, &out, &errb)
			if code != c.code || !strings.Contains(errb.String(), c.msg) {
				t.Errorf("exit %d, want %d; stderr:\n%s", code, c.code, errb.String())
			}
		})
	}
}

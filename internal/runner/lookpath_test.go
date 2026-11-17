//go:build !windows

package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/config"
	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/testutil"
)

// The suite command is found through the PATH of the suite's env:, which its processes
// get, not smates' own: a command only the suite's PATH has runs, and of two commands
// of the same name the suite's runs.
func TestExecUsesSuitePath(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	suiteBin, ownBin := filepath.Join(root, "suite-bin"), filepath.Join(root, "own-bin")
	tool := func(dir, says string) {
		testutil.WriteMode(t, dir, "smates-tool", "#!/bin/sh\necho "+says+"\n", 0o755)
	}
	tool(suiteBin, "suite")
	sys := os.Getenv("PATH")
	cases := []struct {
		name      string
		ownPath   string // smates' PATH
		suitePath string // "" when the suite sets none
		argv0     string
		want      string // output; "" when the command must not be found
	}{
		{"only on the suite's PATH", sys, suiteBin + ":" + sys, "smates-tool", "suite"},
		{"on both: the suite's wins", ownBin + ":" + sys, suiteBin + ":" + sys, "smates-tool", "suite"},
		{"no suite PATH: smates' own", ownBin + ":" + sys, "", "smates-tool", "own"},
		{"suite's PATH lacks it", ownBin + ":" + sys, sys, "smates-tool", ""},
		{"a path is not looked up", sys, sys, filepath.Join(suiteBin, "smates-tool"), "suite"},
		{"a relative path is the suite directory's", sys, sys, "./suite-bin/smates-tool", "suite"},
	}
	tool(ownBin, "own")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PATH", c.ownPath)
			s := &Suite{Suite: config.Suite{Name: "s", Root: "."}}
			if c.suitePath != "" {
				s.Env = map[string]string{"PATH": c.suitePath}
			}
			rc := &RunContext{Repo: &gitx.Repo{Root: root}, Suite: s}
			var out bytes.Buffer
			code, err := rc.ExecIn(context.Background(), root, []string{c.argv0}, nil, &out, io.Discard)
			if c.want == "" {
				if err == nil || !errors.Is(err, exec.ErrNotFound) {
					t.Errorf("found a command the suite's PATH does not have: exit %d, err %v, out %q", code, err, out.String())
				}
				return
			}
			if err != nil || code != 0 || strings.TrimSpace(out.String()) != c.want {
				t.Errorf("exit %d, err %v, out %q, want %q", code, err, out.String(), c.want)
			}
		})
	}
}

// node --test's version check asks the node of the suite's PATH.
func TestNodeMajorUsesSuitePath(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	testutil.WriteMode(t, bin, "node", "#!/bin/sh\necho v19.4.0\n", 0o755)
	t.Setenv("PATH", "/nonexistent")
	rc := &RunContext{Repo: &gitx.Repo{Root: root}, Suite: &Suite{Suite: config.Suite{Name: "s", Root: ".", Command: "node --test",
		Env: map[string]string{"PATH": bin + ":/usr/bin:/bin"}}}}
	if got := nodeMajor(context.Background(), rc); got != 19 {
		t.Errorf("nodeMajor = %d, want 19 (the suite's node)", got)
	}
}

func TestLookPathIn(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteMode(t, root, "a/tool", "#!/bin/sh\n", 0o755)
	testutil.WriteMode(t, root, "b/tool", "#!/bin/sh\n", 0o755)
	testutil.WriteMode(t, root, "c/tool", "not executable\n", 0o644)
	a, b, c := filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "c")
	for _, tc := range []struct {
		name, path string
		want       string
		err        error
	}{
		{"first match", a + ":" + b, filepath.Join(a, "tool"), nil},
		{"order", b + ":" + a, filepath.Join(b, "tool"), nil},
		{"not executable is skipped", c + ":" + b, filepath.Join(b, "tool"), nil},
		{"missing", c, "", exec.ErrNotFound},
		{"empty PATH", "", "", exec.ErrNotFound},
		// A relative directory of the suite's PATH counts from the suite directory, as
		// in a shell started there (the empty entry is the current directory).
		{"relative directory", "a", filepath.Join(a, "tool"), nil},
		{"relative directory with dot", "./b:a", filepath.Join(b, "tool"), nil},
		{"relative directory not executable", "c:b", filepath.Join(b, "tool"), nil},
		{"empty entry", ":" + c, "", exec.ErrNotFound},
	} {
		got, err := lookPathIn("tool", tc.path, root)
		if got != tc.want || !errors.Is(err, tc.err) && !(err == nil && tc.err == nil) {
			t.Errorf("%s: %q, %v; want %q, %v", tc.name, got, err, tc.want, tc.err)
		}
	}
}

package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/gitx"
	"github.com/dotneet/smates/internal/testutil"
)

// A recorded hash of an executable file carries the executable marker: the diff still
// starts from the recorded blob, and a change of the bit alone is named as one.
func TestGitDiffExecutableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no executable bit")
	}
	g := testutil.NewGitRepo(t, nil)
	testutil.WriteMode(t, g.Dir, "run.sh", "#!/bin/sh\necho one\n", 0o755)
	g.Commit("base")
	repo, err := gitx.Open(g.Dir)
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(repo.Root, "run.sh")
	old, _ := gitx.HashFile(abs)
	if !strings.HasSuffix(old, gitx.ExecSuffix) {
		t.Fatalf("recorded hash %q", old)
	}
	// HEAD moves on: a fallback to HEAD would diff against the wrong content.
	testutil.WriteMode(t, g.Dir, "run.sh", "#!/bin/sh\necho two\n", 0o755)
	g.Commit("two")
	diff := func() string {
		t.Helper()
		cur, _ := gitx.HashFile(abs)
		tb := &toolbox{root: repo.Root, orphans: map[string]Orphan{"run.sh": {Path: "run.sh", OldBlob: old, NewBlob: cur}}}
		out, err := tb.gitDiff(context.Background(), "run.sh")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := diff(); !strings.Contains(out, "recorded blob "+gitx.Blob(old)[:12]) || !strings.Contains(out, "-echo one") || !strings.Contains(out, "+echo two") {
		t.Errorf("diff against the recorded blob:\n%s", out)
	}
	testutil.WriteMode(t, g.Dir, "run.sh", "#!/bin/sh\necho one\n", 0o755)
	if err := os.Chmod(abs, 0o644); err != nil {
		t.Fatal(err)
	}
	if out := diff(); !strings.Contains(out, "run.sh: mode changed (+x removed), no content change against recorded blob") {
		t.Errorf("chmod -x: %s", out)
	}
	if err := os.Chmod(abs, 0o755); err != nil {
		t.Fatal(err)
	}
	if out := diff(); !strings.HasPrefix(out, "run.sh: no content change") {
		t.Errorf("unchanged: %s", out)
	}
	old = gitx.Blob(old) // recorded without the bit
	if out := diff(); !strings.Contains(out, "mode changed (+x added)") {
		t.Errorf("chmod +x: %s", out)
	}
}

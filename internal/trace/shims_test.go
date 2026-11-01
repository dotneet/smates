package trace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/testutil"
)

// These tests run the real shims in node and python3 against a temporary repository and
// check the dependencies Collect derives from what they recorded.

// shimRepo returns a temporary repository (symlinks resolved, as Collect expects) with
// the given files; contents default to the file name.
func shimRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		if body == "" {
			body = rel
		}
		testutil.Write(t, root, rel, body)
	}
	return root
}

// runTraced runs argv in root with env added and returns the dependencies recorded by
// processes of language lang.
func runTraced(t *testing.T, root, lang string, env []string, traceDir string, argv ...string) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
	procs, err := Collect(traceDir, root)
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]bool{}
	for _, p := range procs {
		if p.Lang != lang {
			continue
		}
		for k := range p.Reads {
			reads[k] = true
		}
	}
	if len(reads) == 0 {
		t.Fatalf("no %s process traced (procs: %d)", lang, len(procs))
	}
	return reads
}

func checkDeps(t *testing.T, reads map[string]bool, want, notWant []string) {
	t.Helper()
	keys := make([]string, 0, len(reads))
	for k := range reads {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, w := range want {
		if !reads[w] {
			t.Errorf("missing dependency %q; recorded %v", w, keys)
		}
	}
	for _, n := range notWant {
		if reads[n] {
			t.Errorf("unexpected dependency %q; recorded %v", n, keys)
		}
	}
}

func extractShims(t *testing.T) (*Shims, string) {
	t.Helper()
	sh, err := ExtractShims(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return sh, t.TempDir()
}

const nodeCopyScript = `
const fs = require('fs');
const path = require('path');
const cp = require('child_process');
const { pathToFileURL } = require('url');
const tmp = process.env.OUT_DIR;
const here = pathToFileURL(__dirname + '/');
(async () => {
  fs.copyFileSync('fixtures/a.txt', path.join(tmp, 'a.txt'));
  await new Promise((res, rej) => fs.copyFile(new URL('./fixtures/b.txt', here), path.join(tmp, 'b.txt'), (e) => (e ? rej(e) : res())));
  await fs.promises.copyFile(Buffer.from(path.join(__dirname, 'fixtures/c.txt')), path.join(tmp, 'c.txt'));
  // A copy into the repository depends on the source only.
  fs.copyFileSync('fixtures/a.txt', 'out/copy.txt');
  if (typeof fs.cpSync === 'function') {
    fs.cpSync('fixtures/tree', path.join(tmp, 'tree'), { recursive: true });
    await fs.promises.cp('fixtures/d.txt', path.join(tmp, 'd.txt'));
    await new Promise((res, rej) =>
      fs.cp(new URL('./fixtures/tree2', here), path.join(tmp, 'tree2'), { recursive: true }, (e) => (e ? rej(e) : res())),
    );
  }
  // cwd given as a file: URL object.
  cp.execFileSync('cat', ['data.txt'], { cwd: new URL('./sub/', here) });
})().catch((e) => {
  console.error(e);
  process.exit(1);
});
`

func TestNodeShimCopyAndURLCwd(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not on PATH")
	}
	root := shimRepo(t, map[string]string{
		"test.cjs":                nodeCopyScript,
		"fixtures/a.txt":          "",
		"fixtures/b.txt":          "",
		"fixtures/c.txt":          "",
		"fixtures/d.txt":          "",
		"fixtures/tree/x.txt":     "",
		"fixtures/tree/sub/y.txt": "",
		"fixtures/tree2/z.txt":    "",
		"sub/data.txt":            "",
	})
	if err := os.MkdirAll(filepath.Join(root, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	sh, traceDir := extractShims(t)
	env := append(NodeEnv(traceDir, root, sh, os.Environ()), "OUT_DIR="+t.TempDir())
	reads := runTraced(t, root, "node", env, traceDir, node, "test.cjs")

	ver, _ := exec.Command(node, "--version").Output()
	t.Logf("node %s", strings.TrimSpace(string(ver)))
	want := []string{"fixtures/a.txt", "fixtures/b.txt", "fixtures/c.txt", "sub/data.txt"}
	if out, _ := exec.Command(node, "-p", "typeof require('fs').cpSync").Output(); strings.TrimSpace(string(out)) == "function" {
		want = append(want, "fixtures/d.txt", "fixtures/tree/**", "fixtures/tree2/**")
	}
	checkDeps(t, reads, want, []string{"out/copy.txt"})
	for k := range reads {
		if strings.Contains(k, "file:") {
			t.Errorf("dependency from an unconverted file: URL: %q", k)
		}
	}
}

const pyModule = `import os

# Module-level listings run below importlib frames while the module is imported.
CASES = sorted(os.listdir("data"))
ENTRIES = [e.name for e in os.scandir(os.path.join(os.path.dirname(__file__), "..", "data2"))]


def later():
    return os.listdir("data3"), list(os.scandir("data4"))


fd = os.open("out.txt", os.O_WRONLY | os.O_CREAT, 0o644)
os.close(fd)
fd = os.open("data/a", os.O_RDONLY)
os.close(fd)
fd = os.open("rw.txt", os.O_RDWR | os.O_CREAT, 0o644)
os.close(fd)
with open("written.txt", "w") as f:
    f.write("x")
with open("data/b") as f:
    f.read()
`

func TestPythonShimListingsAndOpenFlags(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	root := shimRepo(t, map[string]string{
		"main.py":         "import pkg.mod\npkg.mod.later()\n",
		"pkg/__init__.py": "\n",
		"pkg/mod.py":      pyModule,
		"data/a":          "",
		"data/b":          "",
		"data2/c":         "",
		"data3/d":         "",
		"data4/e":         "",
	})
	sh, traceDir := extractShims(t)
	env := append(PythonEnv(traceDir, root, sh, os.Environ()), "PYTHONDONTWRITEBYTECODE=1")
	reads := runTraced(t, root, "python", env, traceDir, py, "main.py")

	ver, _ := exec.Command(py, "--version").CombinedOutput()
	t.Logf("%s", strings.TrimSpace(string(ver)))
	checkDeps(t, reads,
		[]string{"data/", "data2/", "data3/", "data4/", "data/a", "data/b", "rw.txt"},
		// pkg/ is listed by the import system to find pkg.mod; out.txt and written.txt
		// are opened write-only.
		[]string{"pkg/", "out.txt", "written.txt"})
}

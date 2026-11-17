package trace

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/dotneet/smates/internal/testutil"
)

// A variable a library reads over and over is still recorded when repository code
// reads it later: every read is checked until one by repository code is seen.
func TestNodeShimEnvAfterLibraryReads(t *testing.T) {
	root := repoDir(t)
	testutil.Write(t, root, "node_modules/lib/index.js", "exports.read = () => process.env.APP_LATE;\n")
	got := runShim(t, root, "t.js", `
const lib = require('lib');
for (let i = 0; i < 1000; i++) lib.read();
void process.env.APP_LATE;
for (let i = 0; i < 1000; i++) lib.read();
void process.env.APP_REGION;
process.stdout.write('done\n');
`, "node", "--require={node}")
	if want := []string{"APP_LATE", "APP_REGION"}; !slices.Equal(got, want) {
		t.Errorf("read %v, want %v", got, want)
	}
}

// Streams are named with a nonce (a reused pid gets a stream of its own); a Node process
// started in a context of a Python test records it (pctx) and passes it on, also to a
// child given an environment of its own.
func TestNodeShimNonceAndSpawnContext(t *testing.T) {
	root := shimRepo(t, nil)
	sh, traceDir := extractShims(t)
	writeFiles(t, root, map[string]string{
		"test.cjs":  "require('child_process').execFileSync(process.execPath, ['child.cjs'], { env: { PATH: process.env.PATH } });\n",
		"child.cjs": "require('fs').readFileSync(__dirname + '/data.txt');\n",
		"data.txt":  "",
		"test_a.py": "",
	})
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	ctx := "77-0:test:" + filepath.Join(root, "test_a.py")
	env := append(Env(traceDir, root, sh, os.Environ()), "SMATES_TRACE_CTX="+ctx)
	procs := runCollect(t, root, traceDir, env, node, "test.cjs")
	entries, _ := os.ReadDir(traceDir)
	named := regexp.MustCompile(`^proc-\d+-\d+-[0-9a-z]+\.jsonl$`)
	streams := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "proc-") {
			streams++
			if !named.MatchString(e.Name()) {
				t.Errorf("stream %s has no nonce", e.Name())
			}
		}
	}
	if streams != 2 {
		t.Errorf("%d streams, want 2", streams)
	}
	var child *Proc
	for _, p := range procs {
		if p.SpawnKey != "77-0" || p.SpawnCtx != "test:test_a.py" {
			t.Errorf("%v: spawn %q %q", p.Argv, p.SpawnKey, p.SpawnCtx)
		}
		if len(p.Argv) > 1 && strings.HasSuffix(p.Argv[1], "child.cjs") {
			child = p
		}
	}
	if child == nil || !child.SpawnFiles["test:test_a.py"]["data.txt"] {
		t.Errorf("child %+v", child)
	}
}

// nodeHas reports whether node runs code without failing (a feature of its version).
func nodeHas(t *testing.T, code string) bool {
	t.Helper()
	return exec.Command("node", "--no-warnings", "-e", code).Run() == nil
}

// fs.openAsBlob and node:sqlite's DatabaseSync read the files they open (an in-memory
// database is no file).
func TestNodeShimBlobAndSqlite(t *testing.T) {
	root := shimRepo(t, nil)
	procs := traceNode(t, root, map[string]string{"data.bin": "", "db.sqlite": ""}, `
const fs = require('fs');
let sqlite = null;
try {
  process.removeAllListeners('warning');
  sqlite = require('node:sqlite');
} catch (_) {}
if (sqlite) {
  new sqlite.DatabaseSync('db.sqlite').close();
  new sqlite.DatabaseSync(':memory:').close();
}
(fs.openAsBlob ? fs.openAsBlob('data.bin') : Promise.resolve()).then(() => {
  console.log(JSON.stringify({ sqlite: Boolean(sqlite), blob: Boolean(fs.openAsBlob) }));
});
`)
	reads := readsOf(procs, "node")
	if reads[":memory:"] {
		t.Errorf("in-memory database recorded: %v", keys(reads))
	}
	// Recent Node versions have both; older ones record neither.
	if _, ok := reads["db.sqlite"]; !ok && nodeHas(t, "require('node:sqlite')") {
		t.Errorf("database not recorded: %v", keys(reads))
	}
	if _, ok := reads["data.bin"]; !ok && nodeHas(t, "if (!require('fs').openAsBlob) process.exit(1)") {
		t.Errorf("blob not recorded: %v", keys(reads))
	}
}

// A process stopped with a signal it handles itself flushes its coverage first; the
// default action comes back with the program's last handler (after a once() handler
// ran, the next signal kills it), and a process without a handler dies at once.
func TestNodeShimSignalFlush(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGTERM")
	}
	root := shimRepo(t, nil)
	procs := traceNode(t, root, map[string]string{
		"handled.cjs": "require('./lib.cjs');\nprocess.on('SIGTERM', () => process.send('got'));\nprocess.send('ready');\nsetInterval(() => {}, 1000);\n",
		"once.cjs":    "require('./lib2.cjs');\nprocess.once('SIGTERM', () => process.send('got'));\nprocess.send('ready');\nsetInterval(() => {}, 1000);\n",
		"plain.cjs":   "require('./lib3.cjs');\nprocess.send('ready');\nsetInterval(() => {}, 1000);\n",
		"lib.cjs":     "exports.x = 1;\n",
		"lib2.cjs":    "exports.x = 2;\n",
		"lib3.cjs":    "exports.x = 3;\n",
	}, `
const cp = require('child_process');
const run = (file, after) => new Promise((resolve) => {
  const c = cp.fork(file);
  c.on('message', (m) => {
    if (m === 'ready') c.kill('SIGTERM');
    else after(c);
  });
  c.on('exit', (code, signal) => resolve({ file, code, signal }));
});
(async () => {
  const results = [
    await run('handled.cjs', (c) => c.kill('SIGKILL')),
    await run('once.cjs', (c) => setTimeout(() => c.kill('SIGTERM'), 50)),
    await run('plain.cjs', () => {}),
  ];
  for (const r of results) {
    const want = r.file === 'handled.cjs' ? 'SIGKILL' : 'SIGTERM';
    if (r.signal !== want) {
      console.error('unexpected exit', r);
      process.exit(1);
    }
  }
})();
`)
	code := map[string]bool{}
	for _, p := range procs {
		if len(p.Argv) < 2 || p.Lang != "node" || p.Exited {
			continue
		}
		name := filepath.Base(p.Argv[1])
		code[name] = p.Coverage
		if want := name != "plain.cjs"; p.Coverage != want {
			t.Errorf("%s: coverage %v, want %v (code %v)", name, p.Coverage, want, keys(p.Code))
		}
		if p.Coverage && !p.Code["lib.cjs"] && !p.Code["lib2.cjs"] {
			t.Errorf("%s: code %v", name, keys(p.Code))
		}
	}
	if len(code) != 3 {
		t.Errorf("killed processes %v", code)
	}
}

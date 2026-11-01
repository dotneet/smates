package trace

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// traceNode runs a script of root with node, traced, and returns the processes recorded.
func traceNode(t *testing.T, root string, files map[string]string, script string, extraEnv ...string) []*Proc {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	writeFiles(t, root, files)
	writeFiles(t, root, map[string]string{"test.cjs": script})
	sh, traceDir := extractShims(t)
	env := append(NodeEnv(traceDir, root, sh, os.Environ()), extraEnv...)
	env = append(env, PythonEnv(traceDir, root, sh, os.Environ())...)
	return runCollect(t, root, traceDir, env, node, "test.cjs")
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		if body == "" {
			body = rel
		}
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runCollect(t *testing.T, root, traceDir string, env []string, argv ...string) []*Proc {
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
	return procs
}

func readsOf(procs []*Proc, lang string) map[string]bool {
	reads := map[string]bool{}
	for _, p := range procs {
		if lang != "" && p.Lang != lang {
			continue
		}
		for k := range p.Reads {
			reads[k] = true
		}
	}
	return reads
}

func nodeVersion(t *testing.T) {
	t.Helper()
	if ver, err := exec.Command("node", "--version").Output(); err == nil {
		t.Logf("node %s", strings.TrimSpace(string(ver)))
	}
}

// A file read before a context and again within it is a dependency of both.
func TestNodeShimReadsPerContext(t *testing.T) {
	root := shimRepo(t, nil)
	procs := traceNode(t, root, map[string]string{"a.txt": "", "b.txt": ""}, `
const fs = require('fs');
fs.readFileSync('a.txt');
fs.existsSync('b.txt');
globalThis.__smatesTrace.context('global:');
fs.readFileSync('a.txt');
fs.existsSync('b.txt');
globalThis.__smatesTrace.context('');
fs.readFileSync('a.txt');
`)
	nodeVersion(t)
	for _, p := range procs {
		if p.Lang != "node" {
			continue
		}
		for _, ctx := range []string{"", GlobalContext} {
			for _, k := range []string{"a.txt", "b.txt?"} {
				if !p.Ctx[ctx][k] {
					t.Errorf("context %q: missing %q; recorded %v", ctx, k, p.Ctx[ctx])
				}
			}
		}
	}
}

// util.promisify(child_process.execFile/exec) uses their promisify.custom, which calls
// the internal functions.
func TestNodeShimPromisifiedExec(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not on PATH")
	}
	root := shimRepo(t, nil)
	procs := traceNode(t, root, map[string]string{"execfile.txt": "", "exec.txt": "", "exists.txt": ""}, `
const { promisify } = require('util');
const cp = require('child_process');
const fs = require('fs');
(async () => {
  await promisify(cp.execFile)('cat', ['execfile.txt']);
  await promisify(cp.exec)('cat exec.txt');
  await promisify(fs.exists)('exists.txt');
})().catch((e) => { console.error(e); process.exit(1); });
`)
	nodeVersion(t)
	checkDeps(t, readsOf(procs, "node"), []string{"execfile.txt", "exec.txt", "exists.txt?"}, nil)
}

// Children given an environment of their own are still traced, with the caller's
// variables kept.
func TestNodeShimChildWithOwnEnv(t *testing.T) {
	root := shimRepo(t, nil)
	files := map[string]string{
		"child.cjs":  "require('fs').readFileSync('child.txt');\n",
		"child2.cjs": "require('fs').readFileSync('child2.txt');\nif (process.env.KEEP !== 'yes' || !process.env.NODE_OPTIONS.includes('--no-deprecation')) process.exit(3);\n",
		"fork.cjs":   "require('fs').readFileSync('fork.txt');\n",
		"shell.cjs":  "require('fs').readFileSync('shell.txt');\n",
		"worker.cjs": "require('fs').readFileSync('worker.txt');\n",
		"child.py":   "open('py.txt').read()\n",
		"child.txt":  "", "child2.txt": "", "fork.txt": "", "shell.txt": "", "worker.txt": "", "py.txt": "",
	}
	_, pyErr := exec.LookPath("python3")
	script := `
const cp = require('child_process');
const { Worker } = require('worker_threads');
const node = process.execPath;
const env = { PATH: process.env.PATH };
cp.execFileSync(node, ['child.cjs'], { env });
const r = cp.spawnSync(node, ['child2.cjs'], { env: { PATH: process.env.PATH, KEEP: 'yes', NODE_OPTIONS: '--no-deprecation' }, stdio: 'inherit' });
if (r.status !== 0) throw new Error('child2: ' + r.status);
cp.execSync(JSON.stringify(node) + ' shell.cjs', { env });
` + map[bool]string{true: "cp.execFileSync('python3', ['child.py'], { env });\n"}[pyErr == nil] + `
const f = cp.fork('fork.cjs', { env });
f.on('exit', (code) => {
  if (code !== 0) process.exit(4);
  const w = new Worker('./worker.cjs', { env: {} });
  w.on('exit', (code) => process.exit(code));
});
`
	procs := traceNode(t, root, files, script)
	nodeVersion(t)
	checkDeps(t, readsOf(procs, "node"), []string{"child.txt", "child2.txt", "fork.txt", "shell.txt", "worker.txt"}, nil)
	if pyErr == nil {
		checkDeps(t, readsOf(procs, "python"), []string{"py.txt"}, nil)
	}
	n := 0
	for _, p := range procs {
		if p.Lang != "node" {
			continue
		}
		n++
		// SMATES_TRACE_PARENT links the children, threads inherit it.
		if main := len(p.Argv) > 1 && filepath.Base(p.Argv[1]) == "test.cjs"; !main && p.Anc == "" {
			t.Errorf("%v: not linked to its parent", p.Argv)
		}
	}
	if n != 6 {
		t.Errorf("%d node processes and threads traced, want 6", n)
	}
}

// Listings of the repository root itself, realpath/readlink probes, recursive listings.
func TestNodeShimRootListingsAndProbes(t *testing.T) {
	root := shimRepo(t, nil)
	procs := traceNode(t, root, map[string]string{
		"a.txt": "", "lib.cjs": "module.exports = 1;\n",
		"tree/x/y.txt": "", "tree2/x/y.txt": "", "tree3/x/y.txt": "", "tree4/x/y.txt": "", "flat/x/y.txt": "",
	}, `
const fs = require('fs');
(async () => {
  fs.readdirSync('.');
  try { fs.realpathSync('maybe.txt'); } catch (_) {}
  fs.realpathSync.native('a.txt');
  await fs.promises.realpath('tree');
  await new Promise((res) => fs.realpath('maybe2.txt', () => res()));
  await new Promise((res) => fs.realpath.native('tree2', () => res()));
  try { fs.readlinkSync('link'); } catch (_) {}
  require('./lib.cjs');
  fs.readdirSync('tree', { recursive: true });
  await fs.promises.readdir('tree2', { recursive: true });
  fs.opendirSync('tree3', { recursive: true }).closeSync();
  await new Promise((res) => fs.readdir('tree4', { recursive: true }, () => res()));
  fs.readdirSync('flat', { withFileTypes: true });
})().catch((e) => { console.error(e); process.exit(1); });
`)
	nodeVersion(t)
	checkDeps(t, readsOf(procs, "node"),
		[]string{"./", "maybe.txt?", "a.txt?", "tree?", "maybe2.txt?", "tree2?", "link?",
			"tree/**", "tree2/**", "tree3/**", "tree4/**", "flat/"},
		// The module loader resolves modules with realpathSync: that is no probe.
		[]string{"lib.cjs?", "test.cjs?", "flat/**", "a.txt"})
}

// Reads through workspace packages linked into node_modules are reads of the packages.
func TestNodeShimWorkspaceLinks(t *testing.T) {
	root := shimRepo(t, nil)
	files := map[string]string{
		"packages/shared/data.json":                               "{}",
		"packages/shared2/data.json":                              "{}",
		"node_modules/real/data.json":                             "{}",
		"node_modules/.pnpm/dep@1.0.0/node_modules/dep/data.json": "{}",
	}
	writeFiles(t, root, files)
	for link, target := range map[string]string{
		"node_modules/@me/shared":                               "../../packages/shared",
		"node_modules/.pnpm/dep@1.0.0/node_modules/@me/shared2": "../../../../../packages/shared2",
		"node_modules/dep":                                      ".pnpm/dep@1.0.0/node_modules/dep",
	} {
		p := filepath.Join(root, filepath.FromSlash(link))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	procs := traceNode(t, root, nil, `
const fs = require('fs');
fs.readFileSync('node_modules/@me/shared/data.json');
fs.readdirSync('node_modules/@me/shared');
fs.readFileSync('node_modules/.pnpm/dep@1.0.0/node_modules/@me/shared2/data.json');
fs.readFileSync('node_modules/real/data.json');
fs.readFileSync('node_modules/dep/data.json');
`)
	nodeVersion(t)
	reads := readsOf(procs, "node")
	checkDeps(t, reads, []string{"packages/shared/data.json", "packages/shared/", "packages/shared2/data.json"}, nil)
	for k := range reads {
		if strings.Contains(k, "node_modules") {
			t.Errorf("dependency in node_modules: %q", k)
		}
	}
}

// Bun.spawn and Bun.spawnSync record exec events, once for child_process calls (which
// Bun implements with them).
func TestBunShimSpawn(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun not on PATH")
	}
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not on PATH")
	}
	root := shimRepo(t, map[string]string{"a.txt": "", "b.txt": "", "c.txt": "", "sub/d.txt": ""})
	writeFiles(t, root, map[string]string{"test.js": `
const cp = require('child_process');
Bun.spawnSync(['cat', 'a.txt']);
await Bun.spawn({ cmd: ['cat', 'b.txt'] }).exited;
cp.execFileSync('cat', ['c.txt']);
await Bun.spawn(['cat', 'd.txt'], { cwd: 'sub', env: { PATH: process.env.PATH } }).exited;
`})
	sh, traceDir := extractShims(t)
	env := NodeEnv(traceDir, root, sh, os.Environ())
	procs := runCollect(t, root, traceDir, env, bun, "--preload", sh.Node, "test.js")
	if ver, err := exec.Command(bun, "--version").Output(); err == nil {
		t.Logf("bun %s", strings.TrimSpace(string(ver)))
	}
	checkDeps(t, readsOf(procs, "bun"), []string{"a.txt", "b.txt", "c.txt", "sub/d.txt"}, nil)

	execs := map[string]int{}
	ents, _ := os.ReadDir(traceDir)
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "proc-") {
			continue
		}
		f, err := os.Open(filepath.Join(traceDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var ev event
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.K == "exec" {
				execs[strings.Join(ev.Args, " ")]++
			}
		}
		f.Close()
	}
	if execs["cat c.txt"] != 1 {
		t.Errorf("child_process.execFileSync recorded %d times, want 1 (%v)", execs["cat c.txt"], execs)
	}
}

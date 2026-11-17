package trace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Bun.$ runs a shell of Bun's own (builtins such as cat read in process): each command is
// recorded as a shell exec event, with interpolated values rendered as the words Bun
// makes of them, in the directory the command runs in. A value the shim cannot render
// leaves the command depending on everything below its directory, and a command of a
// shell new $.Shell() made (which the shim does not wrap) on the whole repository.
func TestBunShellCommands(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun not on PATH")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH")
	}
	for _, tc := range []struct {
		name, script  string
		want, notWant []string
	}{
		{name: "imported $", script: "import { $ } from 'bun';\nawait $`cat a.txt`;", want: []string{"a.txt"}, notWant: []string{"./**"}},
		{name: "Bun.$", script: "await Bun.$`cat a.txt`;", want: []string{"a.txt"}, notWant: []string{"./**"}},
		{name: "text()", script: "await Bun.$`cat a.txt`.text();", want: []string{"a.txt"}},
		{name: "quiet()", script: "await Bun.$`cat a.txt`.quiet();", want: []string{"a.txt"}},
		{name: "builtin in a pipeline", script: "await Bun.$`cat a.txt | wc -c`;", want: []string{"a.txt"}},
		{name: "interpolated", script: "const f = 'a.txt';\nawait Bun.$`cat ${f}`;", want: []string{"a.txt"}, notWant: []string{"./**"}},
		{name: "interpolated with a space", script: "await Bun.$`cat ${'sp ace.txt'}`;", want: []string{"sp ace.txt"}, notWant: []string{"sp", "ace.txt"}},
		{name: "interpolated with quotes", script: "await Bun.$`cat ${`it's \"q\".txt`}`;", want: []string{`it's "q".txt`}},
		{name: "in double quotes", script: "await Bun.$`cat \"${'sp ace.txt'}\"`;", want: []string{"sp ace.txt"}},
		{name: "in single quotes", script: "await Bun.$`cat '${'sp ace.txt'}'`;", want: []string{"sp ace.txt"}},
		{name: "joined to text", script: "await Bun.$`cat sub/${'g'}.txt`;", want: []string{"sub/g.txt"}},
		{name: "array", script: "await Bun.$`cat ${['a.txt', 'sp ace.txt']}`;", want: []string{"a.txt", "sp ace.txt"}},
		{name: "raw", script: "await Bun.$`cat ${{ raw: 'a.txt sub/g.txt' }}`;", want: []string{"a.txt", "sub/g.txt"}},
		{name: "cwd of the command", script: "await Bun.$`cat g.txt`.cwd('sub');", want: []string{"sub/g.txt"}},
		{name: "cwd of a command of a shell", script: "const sh = new Bun.$.Shell();\nawait sh`cat g.txt`.cwd('sub');", want: []string{"./**"}},
		{name: "cwd of $", script: "Bun.$.cwd('sub');\nawait Bun.$`cat g.txt`;", want: []string{"sub/g.txt"}, notWant: []string{"./**"}},
		{name: "cwd of $, reset", script: "Bun.$.cwd('sub');\nBun.$.cwd();\nawait Bun.$`cat a.txt`;", want: []string{"a.txt"}, notWant: []string{"./**"}},
		{name: "Bun.file redirect", script: "await Bun.$`cat < ${Bun.file('a.txt')}`;", want: []string{"a.txt"}},
		{name: "data redirect", script: "await Bun.$`cat < ${new Response('x')}`;", notWant: []string{"./**"}},
		// A Node program the shell starts traces itself, also given an environment of
		// its own.
		{name: "node child", script: "await Bun.$`node child.js`;", want: []string{"b.txt"}, notWant: []string{"./**"}},
		{name: "node child, own env", script: "await Bun.$`node child.js`.env({ PATH: process.env.PATH });", want: []string{"b.txt"}, notWant: []string{"./**"}},
		{name: "node child, env of $", script: "Bun.$.env({ PATH: process.env.PATH });\nawait Bun.$`node child.js`;", want: []string{"b.txt"}, notWant: []string{"./**"}},
		// Must keep: what a value Bun cannot take stands for is not known.
		{name: "unknown value (must keep)", script: "try { await Bun.$`cat ${{ a: 1 }}`; } catch (_) {}", want: []string{"./**"}},
		{name: "shell made with new (must keep)", script: "const sh = new Bun.$.Shell();\nawait sh`cat a.txt`;", want: []string{"./**"}},
		{name: "subclass of Shell (must keep)", script: "class My extends Bun.$.Shell {}\nawait new My()`cat a.txt`.text();", want: []string{"./**"}},
		{name: "node child of a shell made with new (must keep)", script: "const sh = new Bun.$.Shell();\nsh.env({ PATH: process.env.PATH });\nawait sh`node child.js`;", want: []string{"./**"}},
		// Bun refuses it: nothing runs.
		{name: "not a template", script: "try { await Bun.$('cat a.txt'); } catch (_) {}", notWant: []string{"a.txt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := shimRepo(t, map[string]string{
				"a.txt": "", "b.txt": "", "sp ace.txt": "", `it's "q".txt`: "", "sub/g.txt": "",
				"child.js": "require('fs').readFileSync('b.txt');\n",
				"test.js":  tc.script + "\n",
			})
			sh, traceDir := extractShims(t)
			procs := runCollect(t, root, traceDir, NodeEnv(traceDir, root, sh, os.Environ()), bun, "test.js")
			checkDeps(t, readsOf(procs, ""), tc.want, tc.notWant)
		})
	}
}

// bunShellProbe prints what a program can observe of Bun.$, its Shell class, the shells
// and commands they make, and of ShellPromise: the shim's hook must not change any of it.
const bunShellProbe = `
const desc = (o, k) => {
  const d = Object.getOwnPropertyDescriptor(o, k);
  return d && [d.writable, d.enumerable, d.configurable, typeof d.value, typeof d.get];
};
const keys = (o) => Reflect.ownKeys(o).map(String);
const descs = (o) => keys(o).map((k) => [k, desc(o, k)]);
const fn = (f) => [typeof f, f && f.name, f && f.length, f && keys(f)];
const attempt = (f) => {
  try {
    const r = f();
    return ['ok', typeof r];
  } catch (e) {
    return ['throws', e && e.constructor && e.constructor.name];
  }
};
const $ = Bun.$;
const S = $.Shell;
const P = $.ShellPromise;
const sh = new S();
class My extends S {
  hello() {
    return 'hi';
  }
}
const my = new My();
const p = $` + "`echo x`" + `;
const out = {
  $: fn($),
  $descs: descs($),
  $keys: Object.keys($),
  $proto: Object.getPrototypeOf($) === S.prototype,
  $instanceof: $ instanceof S,
  $apply: typeof $.apply,
  $new: attempt(() => new $(['x'])),
  shell: fn(S),
  shellDescs: descs(S),
  shellProto: descs(S.prototype),
  shellCall: attempt(() => S()),
  inst: [typeof sh, keys(sh), sh instanceof S, Object.getPrototypeOf(sh) === S.prototype],
  sub: [typeof my.hello, attempt(() => my.hello()), my.hello(), my instanceof My, my instanceof S, my.cwd('.') === my],
  methods: ['cwd', 'env', 'nothrow', 'throws'].map((m) => [m, fn(S.prototype[m])]),
  returns: [$.cwd(process.cwd()) === $, $.nothrow() === $, $.throws(true) === $, $.env(undefined) === $, sh.cwd('.') === sh],
  promise: [keys(p), p instanceof P, p instanceof Promise, Object.getPrototypeOf(p) === P.prototype],
  promiseProto: descs(P.prototype),
  promiseMethods: Reflect.ownKeys(P.prototype).filter((k) => typeof P.prototype[k] === 'function').map((k) => [String(k), fn(P.prototype[k])]),
  promiseReturns: [p.cwd('.') === p, p.env(process.env) === p, p.quiet() === p, p.nothrow() === p],
  promiseKeysAfter: keys(p),
};
await p;
console.log(JSON.stringify(out));
`

// The hook on Bun.$ is invisible to the program: what it can observe of Bun.$, its Shell
// class (also subclassed), the shells and commands they make, is the same with the shim
// preloaded as without it.
func TestBunShellHookInvisible(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun not on PATH")
	}
	root := shimRepo(t, map[string]string{"probe.js": bunShellProbe})
	run := func(env []string, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bun, append(args, "probe.js")...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), env...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, stderr.String())
		}
		return out
	}
	plain := run([]string{"BUN_OPTIONS=", "SMATES_TRACE_DIR=", "SMATES_REPO_ROOT="})
	sh, traceDir := extractShims(t)
	traced := run(NodeEnv(traceDir, root, sh, os.Environ()), "--preload", sh.Node)
	if !bytes.Equal(plain, traced) {
		t.Errorf("Bun.$ differs under the shim:\nwithout %s\nwith    %s", plain, traced)
	}
	// And the command was recorded.
	procs, err := Collect(traceDir, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) == 0 {
		t.Fatal("not traced")
	}
	if m, _ := filepath.Glob(filepath.Join(traceDir, "proc-*.jsonl")); len(m) == 0 {
		t.Fatal("no stream")
	}
}

// When the exit event itself cannot be written, the Node shim tries once more with the
// file opened again, so that the stream still says events were lost.
func TestNodeShimLostAtExit(t *testing.T) {
	root := shimRepo(t, nil)
	procs := traceNode(t, root, map[string]string{"a.txt": ""}, `
const fs = require('fs');
const path = require('path');
fs.readFileSync('a.txt');
const dir = process.env.SMATES_TRACE_DIR;
const name = fs.readdirSync(dir).find((n) => n.startsWith('proc-' + process.pid + '-0-'));
const ino = fs.statSync(path.join(dir, name)).ino;
// Before the shim's own exit listener: its exit event goes to a closed descriptor.
process.prependListener('exit', () => {
  for (let fd = 3; fd < 256; fd++) {
    try {
      if (fs.fstatSync(fd).ino === ino) fs.closeSync(fd);
    } catch (_) {}
  }
});
`)
	var p *Proc
	for _, q := range procs {
		if q.Lang == "node" {
			p = q
		}
	}
	if p == nil {
		t.Fatalf("procs %v", procs)
	}
	if !p.Lost || !p.Exited || !p.Reads["a.txt"] {
		t.Errorf("lost %v, exited %v, reads %v", p.Lost, p.Exited, keys(p.Reads))
	}
}

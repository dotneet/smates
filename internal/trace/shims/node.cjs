'use strict';
// smates trace shim for Node.js (and Bun via --preload).
//
// Loaded into every Node process of a test run through NODE_OPTIONS=--require (Bun:
// BUN_OPTIONS=--preload, and bun test --preload), it appends
// one JSON object per line to $SMATES_TRACE_DIR/proc-<pid>-<threadId>.jsonl:
//   {"k":"start", pid, ppid, tid, argv, cwd, t, lang}
//   {"k":"read","p":"/abs/path"}   a file under the repository was opened for reading
//   {"k":"dir","p":"/abs/dir"}     a directory under the repository was listed
//   {"k":"probe","p":"/abs/path"}  existence of a path was checked (exists/stat/realpath...)
//   {"k":"exec","args":[...],"shell":true,"cwd":"/abs"}  a child process was started
//   {"k":"tree","p":"/abs/dir"}    everything below a directory may have been read
//   {"k":"code","p":"/abs/file"}   a module was loaded (Bun, which has no V8 coverage)
//   {"k":"env","p":"NAME"}         an environment variable was read (never its value)
//   {"k":"ctx","id":"test:/abs/file","t":ms}  attribution context switch (runner hooks)
//   {"k":"exit","code":0,"t":ms}
// Executed code is not recorded here: it comes from V8 coverage (NODE_V8_COVERAGE),
// which also covers worker threads, vm-evaluated modules and child processes.
(function smatesTrace() {
  const traceDir = process.env.SMATES_TRACE_DIR;
  const repoRoot = process.env.SMATES_REPO_ROOT;
  if (!traceDir || !repoRoot || globalThis.__smatesTrace) return;
  // The rest of the tracing environment, for children given an environment of their own.
  const env0 = {
    NODE_OPTIONS: process.env.NODE_OPTIONS || '',
    NODE_V8_COVERAGE: process.env.NODE_V8_COVERAGE || '',
    PYTHONPATH: process.env.PYTHONPATH || '',
    PYTEST_PLUGINS: process.env.PYTEST_PLUGINS || '',
  };

  const fs = require('fs');
  const path = require('path');
  const { fileURLToPath } = require('url');
  let threadId = 0;
  let isMainThread = true;
  try {
    ({ threadId, isMainThread } = require('worker_threads'));
  } catch (_) {}

  // One descriptor, one write per event: unbuffered, so a worker that is killed
  // loses nothing, without reopening the file for every event.
  const openSync0 = fs.openSync;
  const writeSync0 = fs.writeSync;
  const statSync0 = fs.statSync;
  const realpath0 = fs.realpathSync.native || fs.realpathSync;
  const out = path.join(traceDir, `proc-${process.pid}-${threadId}.jsonl`);
  let fd = -1;
  const write = (obj) => {
    try {
      if (fd < 0) fd = openSync0(out, 'a');
      writeSync0(fd, JSON.stringify(obj) + '\n');
    } catch (_) {}
  };
  const rootPrefix = repoRoot.endsWith(path.sep) ? repoRoot : repoRoot + path.sep;
  const rootDir = rootPrefix.slice(0, -1);
  const nm = path.sep + 'node_modules' + path.sep;
  const seen = new Set(); // context + kind + path
  let ctx = ''; // the current attribution context (see __smatesTrace.context)

  const toPath = (p) => {
    if (typeof p === 'string') return p.startsWith('file:') ? safeFileURL(p) : p;
    if (p && typeof p === 'object') {
      if (typeof p.href === 'string' && p.protocol === 'file:') return safeFileURL(p);
      if (Buffer.isBuffer(p)) return p.toString();
    }
    return null;
  };
  const safeFileURL = (u) => {
    try {
      return fileURLToPath(u);
    } catch (_) {
      return null;
    }
  };
  // Workspace packages are linked into node_modules (npm, yarn and pnpm workspaces): a
  // path through such a link is a path of the repository. Only the package directory
  // after the last node_modules is resolved, once per package; real packages stay out.
  const links = new Map(); // package directory -> its path in the repository, or null
  const linked = (s) => {
    const i = s.lastIndexOf(nm) + nm.length;
    const parts = s.slice(i).split(path.sep);
    const n = parts[0].startsWith('@') ? 2 : 1;
    if (parts.length < n) return null;
    const pkg = s.slice(0, i) + parts.slice(0, n).join(path.sep);
    let real = links.get(pkg);
    if (real === undefined) {
      try {
        real = realpath0(pkg);
        if (!real.startsWith(rootPrefix) || real.includes(nm)) real = null;
      } catch (_) {
        real = null;
      }
      links.set(pkg, real);
    }
    return real && path.join(real, ...parts.slice(n));
  };
  // Tracing must never throw into user code (a deleted cwd, odd path objects...).
  const record = (kind, p) => {
    try {
      let s = toPath(p);
      if (!s) return;
      if (!path.isAbsolute(s)) s = path.resolve(s);
      if (s.includes(nm) && !(s = linked(s))) return;
      // The repository root itself can be listed (readdirSync('.')), not read.
      if (!s.startsWith(rootPrefix) && !(s === rootDir && (kind === 'dir' || kind === 'tree'))) return;
      // Per context: a file read before a context (vitest's config reading .env) and
      // again within it (global setup) is a dependency of both. Not executed code: a
      // coverage result may list scripts that ran in earlier contexts.
      const key = (kind === 'code' ? '' : ctx) + '\0' + kind + '\0' + s;
      if (seen.has(key)) return;
      seen.add(key);
      write({ k: kind, p: s });
    } catch (_) {}
  };

  // wrapFn returns a replacement for orig that first calls before with the arguments
  // (it may return other arguments to call orig with) and has orig's properties. A
  // util.promisify.custom implementation is wrapped the same way: those of
  // child_process.exec and execFile call the internal functions, not the exports.
  // A depth counter ({n}) given is above zero while orig runs: spawning, so that the
  // functions a spawn function calls (exec calls execFile, Bun's child_process calls
  // Bun.spawn) record nothing; cloning, likewise for the cloning functions.
  let promisifyCustom = null;
  try {
    promisifyCustom = require('util').promisify.custom;
  } catch (_) {}
  const spawning = { n: 0 };
  const wrapFn = (orig, before, depth) => {
    const wrapped = function () {
      let args = arguments;
      try {
        args = before.apply(null, arguments) || arguments;
      } catch (_) {}
      if (!depth) return orig.apply(this, args);
      depth.n++;
      try {
        return orig.apply(this, args);
      } finally {
        depth.n--;
      }
    };
    Object.defineProperty(wrapped, '__smates', { value: true });
    Object.defineProperty(wrapped, 'name', { value: orig.name });
    Object.defineProperty(wrapped, 'length', { value: orig.length });
    for (const k of Reflect.ownKeys(orig)) {
      if (k === 'length' || k === 'name' || k === 'prototype') continue;
      try {
        const d = Object.getOwnPropertyDescriptor(orig, k);
        if (k === promisifyCustom && typeof d.value === 'function') d.value = wrapFn(d.value, before, depth);
        Object.defineProperty(wrapped, k, d);
      } catch (_) {}
    }
    return wrapped;
  };
  const wrap = (obj, name, kind, isOpen) => {
    const orig = obj && obj[name];
    if (typeof orig !== 'function' || orig.__smates) return;
    const wrapped = wrapFn(orig, function (p, flags) {
      if (typeof p !== 'number' && (!isOpen || isReadFlags(flags))) {
        if (typeof kind === 'function') kind.apply(null, arguments);
        else record(kind, p);
      }
    });
    try {
      obj[name] = wrapped;
    } catch (_) {}
  };
  // Opening for writing only is not a dependency; reading or read-write is.
  const isReadFlags = (flags) => {
    if (flags === undefined || flags === null || typeof flags === 'function') return true;
    if (typeof flags === 'object') return isReadFlags(flags.flag);
    if (typeof flags === 'number') return (flags & 3) !== 1; // O_WRONLY
    return /r|\+/.test(String(flags));
  };

  for (const name of ['readFileSync', 'readFile']) wrap(fs, name, 'read');
  for (const name of ['openSync', 'open']) wrap(fs, name, 'read', true);
  wrap(fs, 'createReadStream', 'read');
  // A recursive listing (Node 18.17+/20.1+) lists everything below the directory.
  const recordDir = (p, opts) => record(opts && typeof opts === 'object' && opts.recursive ? 'tree' : 'dir', p);
  for (const name of ['readdirSync', 'readdir', 'opendirSync', 'opendir']) wrap(fs, name, recordDir);
  // A test whose outcome depends on whether a file exists depends on that file's
  // existence (even when it does not exist yet).
  for (const name of ['existsSync', 'exists', 'statSync', 'stat', 'lstatSync', 'lstat', 'accessSync', 'access']) wrap(fs, name, 'probe');
  // Resolving a path checks that it exists too. Node's module loaders resolve every
  // module they load, passing their cache under a symbol key: that is no probe.
  const recordRealpath = (p, opts) => {
    if (!(opts && typeof opts === 'object' && Object.getOwnPropertySymbols(opts).length)) record('probe', p);
  };
  for (const name of ['realpathSync', 'realpath']) {
    wrap(fs[name], 'native', 'probe'); // before its parent, which copies it
    wrap(fs, name, recordRealpath);
  }
  for (const name of ['readlinkSync', 'readlink']) wrap(fs, name, 'probe');
  // Copying reads the source: a test that copies a fixture into a temporary directory
  // depends on the fixture. A recursive cp of a directory may read everything below it
  // (cp is native from Node 22 on, so nothing else would be seen). statSync0 is the
  // un-wrapped function, so this records no probe of its own.
  for (const name of ['copyFileSync', 'copyFile']) wrap(fs, name, 'read');
  const recordCp = (src, _dest, opts) => {
    let isDir = false;
    try {
      const s = toPath(src);
      isDir = Boolean(s) && statSync0(s).isDirectory();
    } catch (_) {}
    // A non-recursive cp of a directory fails: only the directory's existence matters.
    if (isDir) record(opts && typeof opts === 'object' && opts.recursive ? 'tree' : 'probe', src);
    else record('read', src);
  };
  for (const name of ['cpSync', 'cp']) wrap(fs, name, recordCp);
  const fsp = fs.promises;
  if (fsp) {
    wrap(fsp, 'readFile', 'read');
    wrap(fsp, 'open', 'read', true);
    wrap(fsp, 'readdir', recordDir);
    wrap(fsp, 'opendir', recordDir);
    for (const name of ['stat', 'lstat', 'access', 'realpath', 'readlink']) wrap(fsp, name, 'probe');
    wrap(fsp, 'copyFile', 'read');
    wrap(fsp, 'cp', recordCp);
  }
  // Child processes: Node and Python children trace themselves; for anything else
  // (shell scripts, grep, make...) smates infers dependencies from the command line.
  // cwd may be a string, a file: URL (string or object) or a Buffer.
  const cwdOf = (o) => {
    const c = o && typeof o === 'object' && o.cwd;
    return path.resolve((c && (toPath(c) || String(c))) || process.cwd());
  };
  const execEv = (args, shell, opts) => {
    try {
      write({ k: 'exec', args: args.map(String), shell: Boolean(shell), cwd: cwdOf(opts) });
    } catch (_) {}
  };
  // A child given an environment of its own ({env: {PATH}}) would start untraced, and
  // smates trusts Node, Bun and Python children to trace themselves: the tracing
  // variables (as NodeEnv and PythonEnv set them) are added to a copy of it. Values the
  // caller set are kept; NODE_OPTIONS, BUN_OPTIONS, PYTHONPATH and PYTEST_PLUGINS get the
  // shims' entries added.
  const optionValue = (s) => (s.startsWith('"') ? JSON.parse(s) : s);
  let shimPath = __filename; // as NODE_OPTIONS names it, if it does
  for (const m of env0.NODE_OPTIONS.matchAll(/--(?:require|preload)[= ]("(?:[^"\\]|\\.)*"|\S+)/g)) {
    try {
      const p = optionValue(m[1]);
      if (p === __filename || realpath0(p) === __filename) shimPath = p;
    } catch (_) {}
  }
  const shimOption = '--require=' + (/[\s"'\\]/.test(shimPath) ? JSON.stringify(shimPath) : shimPath);
  // Bun ignores NODE_OPTIONS --require; it reads BUN_OPTIONS, which has no escapes but
  // takes a whole argument in quotes (as NodeEnv quotes it).
  const bunShimOption = /\s/.test(shimPath) ? `"--preload=${shimPath}"` : '--preload=' + shimPath;
  // The Python shims are extracted next to the Node one (shims/node_modules/smates).
  let pyEntry;
  const pythonEntry = () => {
    if (pyEntry === undefined) {
      pyEntry = '';
      try {
        const dir = realpath0(path.join(__dirname, '..', '..', 'py'));
        for (const e of env0.PYTHONPATH.split(path.delimiter)) {
          try {
            if (e && realpath0(e) === dir) pyEntry = e;
          } catch (_) {}
        }
      } catch (_) {}
    }
    return pyEntry;
  };
  // Ways NODE_OPTIONS may name the shim already: Go quotes paths with backslashes
  // (Windows), escaping them.
  const shimNames = [shimPath, __filename, JSON.stringify(shimPath).slice(1, -1), JSON.stringify(__filename).slice(1, -1)];
  // inherited: child_process takes the environment's inherited variables too (for...in:
  // env: Object.create(process.env)); Worker and Bun.spawn take its own ones only.
  const tracedEnv = (env, inherited) => {
    const e = inherited ? {} : Object.assign({}, env);
    if (inherited) for (const k in env) e[k] = env[k];
    let changed = false;
    // Windows variable names are case-insensitive: Path, Node_Options...
    const keyOf = (k) => {
      if (process.platform !== 'win32') return k;
      const u = k.toUpperCase();
      return Object.keys(e).find((x) => x.toUpperCase() === u) || k;
    };
    const get = (k) => (e[k] === undefined || e[k] === null ? '' : String(e[k]));
    const set = (k, v) => {
      k = keyOf(k);
      if (v && e[k] === undefined) {
        e[k] = v;
        changed = true;
      }
    };
    const add = (k, entry, sep, first) => {
      k = keyOf(k);
      const cur = get(k);
      if (cur.split(sep).some((s) => s.trim() === entry)) return;
      e[k] = !cur ? entry : first ? entry + sep + cur : cur + sep + entry;
      changed = true;
    };
    set('SMATES_TRACE_DIR', traceDir);
    set('SMATES_REPO_ROOT', repoRoot);
    set('SMATES_TRACE_PARENT', `${process.pid}-${threadId}`);
    set('NODE_V8_COVERAGE', env0.NODE_V8_COVERAGE);
    const nodeOptions = keyOf('NODE_OPTIONS');
    const opts = get(nodeOptions);
    if (!shimNames.some((n) => opts.includes(n))) {
      e[nodeOptions] = (shimOption + ' ' + opts).trim();
      changed = true;
    }
    const bunOptions = keyOf('BUN_OPTIONS');
    const bunOpts = get(bunOptions);
    if (!shimNames.some((n) => bunOpts.includes(n))) {
      e[bunOptions] = (bunOpts + ' ' + bunShimOption).trim();
      changed = true;
    }
    if (pythonEntry()) {
      add('PYTHONPATH', pyEntry, path.delimiter, true);
      if (env0.PYTEST_PLUGINS.split(',').includes('smates_pytest')) add('PYTEST_PLUGINS', 'smates_pytest', ',', false);
    }
    return changed ? e : env;
  };
  // withEnv returns args with the options at index i given a traced environment, or
  // nothing when they need none.
  const withEnv = (args, i, inherited) => {
    const o = args[i];
    if (!o || typeof o !== 'object' || !o.env || typeof o.env !== 'object') return;
    const env = tracedEnv(o.env, inherited);
    if (env === o.env) return;
    const a = Array.prototype.slice.call(args);
    a[i] = Object.assign(Object.create(Object.getPrototypeOf(o)), o, { env });
    return a;
  };
  try {
    const cp = require('child_process');
    const wrapCP = (name, ev, at) => {
      const orig = cp[name];
      if (typeof orig !== 'function' || orig.__smates) return;
      cp[name] = wrapFn(
        orig,
        function () {
          if (spawning.n) return;
          const i = at(arguments);
          if (ev) ev(arguments, i);
          return withEnv(arguments, i, true);
        },
        spawning,
      );
    };
    // (file, args?, options?, callback?): args may be omitted or null.
    const fileOpts = (a) => (Array.isArray(a[1]) || a[1] === null || a[1] === undefined ? 2 : 1);
    const fileForm = (a, i) => {
      const file = a[0];
      const args = Array.isArray(a[1]) ? a[1] : [];
      const opts = a[i];
      const shell = opts && typeof opts === 'object' && opts.shell;
      execEv(shell ? [[file].concat(args).join(' ')] : [file].concat(args), shell, opts);
    };
    for (const name of ['spawn', 'spawnSync', 'execFile', 'execFileSync']) wrapCP(name, fileForm, fileOpts);
    for (const name of ['exec', 'execSync']) wrapCP(name, (a, i) => execEv([a[0]], true, a[i]), () => 1);
    // fork starts a Node child, which traces itself.
    wrapCP('fork', null, fileOpts);
  } catch (_) {}
  // process.env is traced through a proxy (below), and a proxy cannot be cloned: what
  // clones its argument (structuredClone, postMessage, workerData, v8.serialize) gets
  // the environment object itself instead, also one level down ({env: process.env}).
  const envTargets = new WeakMap(); // proxy -> the environment object it traces
  const unproxy = (v) => {
    if (!v || typeof v !== 'object') return v;
    const t = envTargets.get(v);
    if (t) return t;
    const proto = Object.getPrototypeOf(v);
    const arr = Array.isArray(v);
    if (arr ? v.length > 64 : proto !== Object.prototype && proto !== null) return v;
    // Data properties only: a getter runs once, when the value is cloned.
    let copy = null;
    for (const k of Object.keys(v)) {
      const d = Object.getOwnPropertyDescriptor(v, k);
      const tx = d && 'value' in d && d.value && typeof d.value === 'object' && envTargets.get(d.value);
      if (!tx) continue;
      if (!copy) copy = arr ? v.slice() : Object.assign(Object.create(proto), v);
      copy[k] = tx;
    }
    return copy || v;
  };
  // cloneArg returns a wrapFn hook that unproxies argument i. cloning is above zero
  // while a wrapped function runs: Node 18's structuredClone posts its value through
  // MessagePort.prototype.postMessage, which then has nothing to do.
  const cloning = { n: 0 };
  const cloneArg = (i) =>
    function () {
      if (cloning.n) return;
      const v = arguments[i];
      const u = unproxy(v);
      if (u === v) return;
      const a = Array.prototype.slice.call(arguments);
      a[i] = u;
      return a;
    };
  const wrapClone = (obj, name) => {
    const orig = obj && obj[name];
    if (typeof orig !== 'function' || orig.__smates) return;
    try {
      obj[name] = wrapFn(orig, cloneArg(0), cloning);
    } catch (_) {}
  };
  wrapClone(globalThis, 'structuredClone');
  try {
    const wt = require('worker_threads');
    for (const C of [wt.MessagePort, wt.BroadcastChannel]) wrapClone(C && C.prototype, 'postMessage');
  } catch (_) {}
  try {
    const v8 = require('v8');
    wrapClone(v8, 'serialize');
    wrapClone(v8.Serializer && v8.Serializer.prototype, 'writeValue');
  } catch (_) {}
  // Worker threads trace themselves too, given an environment that enables it.
  try {
    const wt = require('worker_threads');
    const Orig = wt.Worker;
    if (typeof Orig === 'function' && !Orig.__smates) {
      const Worker = class Worker extends Orig {
        constructor() {
          let args = arguments;
          try {
            args = withEnv(arguments, 1) || arguments;
            const o = args[1];
            const data = o && typeof o === 'object' && unproxy(o.workerData);
            if (data && data !== o.workerData) {
              args = Array.prototype.slice.call(args);
              args[1] = Object.assign(Object.create(Object.getPrototypeOf(o)), o, { workerData: data });
            }
          } catch (_) {}
          super(...args);
        }
      };
      Object.defineProperty(Worker, '__smates', { value: true });
      Object.defineProperty(Worker, 'length', { value: Orig.length });
      wt.Worker = Worker;
    }
  } catch (_) {}
  try {
    require('module').syncBuiltinESMExports();
  } catch (_) {}

  // Bun's own file APIs.
  if (typeof Bun !== 'undefined') {
    try {
      const origFile = Bun.file;
      if (typeof origFile === 'function' && !origFile.__smates) {
        const file = function (p) {
          if (typeof p === 'string' || (p && typeof p === 'object' && typeof p.href === 'string')) record('read', p);
          return origFile.apply(this, arguments);
        };
        Object.defineProperty(file, '__smates', { value: true });
        Bun.file = file;
      }
    } catch (_) {}
    try {
      const proto = Bun.Glob && Bun.Glob.prototype;
      const OrigGlob = Bun.Glob;
      if (proto && !proto.__smates) {
        // The pattern is not exposed on instances: remember it at construction.
        const patterns = new WeakMap();
        const Glob = function (pattern) {
          const g = new OrigGlob(pattern);
          patterns.set(g, String(pattern));
          return g;
        };
        Glob.prototype = proto;
        for (const name of ['scan', 'scanSync']) {
          const orig = proto[name];
          if (typeof orig !== 'function') continue;
          proto[name] = function (opts) {
            try {
              const cwd = path.resolve((opts && typeof opts === 'object' && opts.cwd) || (typeof opts === 'string' ? opts : process.cwd()));
              // The literal directories before the first glob segment are where the scan
              // starts: it lists that directory when only the last segment is a glob
              // (data/*.json), and may read anything below it when a directory segment
              // is (data/*/x.txt, data/**/fixtures/*.json).
              const segs = (patterns.get(this) || '**').split('/');
              let g = segs.findIndex((s) => /[*?[{]/.test(s));
              if (g < 0) g = segs.length - 1;
              const dir = path.resolve(cwd, segs.slice(0, g).join('/') || '.');
              if (g < segs.length - 1 || segs[g].includes('**')) write({ k: 'tree', p: dir });
              else record('dir', dir);
            } catch (_) {}
            return orig.apply(this, arguments);
          };
        }
        Object.defineProperty(proto, '__smates', { value: true });
        Bun.Glob = Glob;
      }
    } catch (_) {}
    // Bun.spawn(cmd, options?) and Bun.spawn({cmd, ...options}), like child_process
    // (whose Bun implementation calls them: those children are recorded already). Bun.$
    // runs its own shell, with builtins such as cat, and is not traced.
    for (const name of ['spawn', 'spawnSync']) {
      try {
        const orig = Bun[name];
        if (typeof orig !== 'function' || orig.__smates) continue;
        Bun[name] = wrapFn(orig, function (cmd, opts) {
          if (spawning.n) return;
          const i = Array.isArray(cmd) ? 1 : 0;
          const argv = i ? cmd : cmd && cmd.cmd;
          if (Array.isArray(argv)) execEv(argv, false, arguments[i]);
          return withEnv(arguments, i);
        });
      } catch (_) {}
    }
  }

  globalThis.__smatesTrace = {
    // Runner integrations call this to attribute subsequent activity to a test file.
    context(id) {
      ctx = String(id);
      write({ k: 'ctx', id: ctx, t: Date.now() });
    },
  };

  // The nearest traced ancestor, inherited through the environment so that processes
  // started via a shell (sh -c "node x | tr ...") are still linked to their test.
  const anc = process.env.SMATES_TRACE_PARENT || '';
  try {
    process.env.SMATES_TRACE_PARENT = `${process.pid}-${threadId}`;
  } catch (_) {}
  write({
    k: 'start',
    pid: process.pid,
    ppid: process.ppid,
    anc,
    tid: threadId,
    argv: process.argv,
    cwd: process.cwd(),
    t: Date.now(),
    lang: typeof Bun !== 'undefined' ? 'bun' : 'node',
  });

  // Environment variables: process.env becomes a proxy that records the names of the
  // variables read. Only reads with repository code on the stack count (not the
  // runner's own bookkeeping), and only of values this process did not set itself (a
  // test that sets TZ and reads it back does not depend on the outer TZ). Copying the
  // whole environment ({...process.env}, Object.keys, for...in, spawning a child) is
  // not a read of every variable: traced children record their own reads.
  try {
    const hasOwn = Object.prototype.hasOwnProperty;
    // Names that are looked up on any object (JSON.stringify, await, matchers...).
    const protocol = new Set(['toJSON', 'then', 'asymmetricMatch', '$$typeof', 'nodeType', 'inspect', 'length']);
    const frameRe = /(file:\/\/[^\s)]+?|\/[^\s()]+?|[A-Za-z]:\\[^\s()]+?):\d+:\d+/g;
    const self = __filename;
    const runnerRe = /[\\/]node_modules[\\/](?:vitest|@vitest[\\/][^\\/]+|jest-[^\\/]+|@jest[\\/][^\\/]+|expect)[\\/]/;
    const fromRepo = () => {
      const limit = Error.stackTraceLimit;
      let stack = '';
      try {
        Error.stackTraceLimit = 64;
        stack = String(new Error().stack || '');
      } finally {
        Error.stackTraceLimit = limit;
      }
      let first = true;
      for (const m of stack.matchAll(frameRe)) {
        const f = m[1].startsWith('file:') ? safeFileURL(m[1]) : m[1];
        if (!f || f === self) continue;
        // The test framework itself, e.g. vi.stubEnv saving the value it restores later.
        if (first && runnerRe.test(f)) return false;
        first = false;
        if (f.startsWith(rootPrefix) && !f.includes(nm)) return true;
      }
      return false;
    };
    const recorded = new Set(); // context + name
    const tries = new Map(); // context + name -> reads without repository code on the stack
    // envProxy traces an environment object (initial: its values now).
    const envProxy = (target) => {
      const initial = Object.assign(Object.create(null), target);
      const note = (env, name) => {
        try {
          if (typeof name !== 'string') return;
          const own = hasOwn.call(env, name);
          if (!own && (name in Object.prototype || protocol.has(name))) return;
          const key = ctx + '\0' + name;
          if (recorded.has(key)) return;
          if ((own ? env[name] : undefined) !== initial[name]) return; // set by this process
          const n = tries.get(key) || 0;
          if (n >= 32) return; // read over and over by code outside the repository
          tries.set(key, n + 1);
          if (!fromRepo()) return;
          recorded.add(key);
          write({ k: 'env', p: name });
        } catch (_) {}
      };
      // An enumeration (ownKeys) is followed by lookups of the keys in order, within
      // the same tick: those lookups are the copy, not reads of single variables.
      let keys = null;
      let descAt = 0;
      let getAt = 0;
      const enumerated = (name, get) => {
        if (!keys) return false;
        if (get ? keys[getAt] !== name : keys[descAt] !== name) return false;
        if (get) getAt++;
        else descAt++;
        return true;
      };
      const p = new Proxy(target, {
        get(env, name) {
          if (!enumerated(name, true)) note(env, name);
          return env[name];
        },
        has(env, name) {
          note(env, name);
          return name in env;
        },
        getOwnPropertyDescriptor(env, name) {
          if (!enumerated(name, false)) note(env, name);
          return Reflect.getOwnPropertyDescriptor(env, name);
        },
        ownKeys(env) {
          const list = Reflect.ownKeys(env);
          keys = list;
          descAt = getAt = 0;
          queueMicrotask(() => {
            if (keys === list) keys = null;
          });
          return list;
        },
        set(env, name, value) {
          env[name] = value;
          return true;
        },
        deleteProperty(env, name) {
          delete env[name];
          return true;
        },
      });
      envTargets.set(p, target);
      return p;
    };
    let proxy = envProxy(process.env);
    Object.defineProperty(process, 'env', {
      configurable: true,
      enumerable: true,
      get() {
        return proxy;
      },
      set(v) {
        // A test replaced the environment: keep tracing the new one.
        proxy = v && typeof v === 'object' ? envProxy(v) : v;
      },
    });
    // Jest gives every test file a copy of process.env (jest-util's
    // installCommonGlobals): trace the copy too.
    const Module = require('module');
    const load = Module._load;
    const jestUtil = new WeakMap();
    Module._load = function (request) {
      const exp = load.apply(this, arguments);
      if (request !== 'jest-util' || !exp || typeof exp.installCommonGlobals !== 'function') return exp;
      let w = jestUtil.get(exp);
      if (!w) {
        const install = exp.installCommonGlobals;
        // Its exports are getters that cannot be redefined: shadow the one we need.
        w = Object.create(exp, {
          installCommonGlobals: {
            enumerable: true,
            value: function (g) {
              const r = install.apply(this, arguments);
              try {
                if (g && g.process && g.process.env) g.process.env = envProxy(g.process.env);
              } catch (_) {}
              return r;
            },
          },
        });
        jestUtil.set(exp, w);
      }
      return w;
    };
  } catch (_) {}

  // Coverage tools that drive the V8 profiler themselves (vitest's v8 provider, jest
  // with coverageProvider "v8", through node:inspector) take and then stop V8's precise
  // coverage in the worker. That also ends the collection behind NODE_V8_COVERAGE, so
  // the worker's coverage file no longer lists what it executed: the scripts in each
  // coverage they take are recorded here instead (their results are left untouched).
  if (typeof Bun === 'undefined') {
    try {
      const Session = require('inspector').Session;
      const post = Session && Session.prototype.post;
      if (typeof post === 'function' && !post.__smates) {
        const noteCoverage = (res) => {
          try {
            for (const s of (res && res.result) || []) {
              if (s && typeof s.url === 'string' && s.url.startsWith('file:')) record('code', s.url);
            }
          } catch (_) {}
        };
        const wrapped = function (method, params, cb) {
          if (method === 'Profiler.takePreciseCoverage') {
            const args = Array.prototype.slice.call(arguments);
            const i = typeof params === 'function' ? 1 : 2;
            const done = args[i];
            if (typeof done === 'function') {
              args[i] = function (err, res) {
                if (!err) noteCoverage(res);
                return done.apply(this, arguments);
              };
              return post.apply(this, args);
            }
          }
          return post.apply(this, arguments);
        };
        Object.defineProperty(wrapped, '__smates', { value: true });
        Object.defineProperty(wrapped, 'name', { value: post.name });
        Session.prototype.post = wrapped;
      }
    } catch (_) {}
  }

  // Bun has no V8 coverage: the module cache tells which modules were loaded (lcov
  // leaves out files it has no lines for, such as JSON required at run time).
  const loadedModules = () => {
    if (typeof Bun === 'undefined') return;
    try {
      for (const k of Object.keys(require.cache)) record('code', k);
    } catch (_) {}
  };
  // bun test emits no exit event: a global afterAll hook (registered from a preload)
  // runs after the last file instead. Outside the test runner afterAll throws.
  if (typeof Bun !== 'undefined' && isMainThread) {
    try {
      require('bun:test').afterAll(loadedModules);
    } catch (_) {}
  }
  process.on('exit', (code) => {
    loadedModules();
    write({ k: 'exit', code, t: Date.now() });
  });

  // Pool workers are often stopped with a signal; V8 coverage is only written on a
  // normal exit, so flush it first, then let the default action (or other handlers) run.
  // Only pool workers (forked with an IPC channel, stopped with SIGTERM) need this; a
  // listener on other processes would change how they react to signals.
  if (isMainThread && process.channel) {
    // A vitest fork worker reports "stopped" and then exits by itself while the pool
    // SIGTERMs it and SIGKILLs it 500ms later: on a busy machine Node's exit-time
    // coverage write loses that race. The pool's timer only starts on "stopped", so
    // flush coverage just before sending it.
    if (typeof process.send === 'function') {
      const send = process.send;
      process.send = function (msg) {
        try {
          if (msg && typeof msg === 'object' && msg.type === 'stopped' && msg.__vitest_worker_response__) {
            require('v8').takeCoverage();
          }
        } catch (_) {}
        return send.apply(this, arguments);
      };
    }
    for (const sig of ['SIGTERM']) {
      const handler = () => {
        try {
          require('v8').takeCoverage();
        } catch (_) {}
        write({ k: 'exit', code: null, signal: sig, t: Date.now() });
        process.removeListener(sig, handler);
        if (process.listenerCount(sig) === 0) process.kill(process.pid, sig);
      };
      try {
        process.on(sig, handler);
      } catch (_) {}
    }
  }
})();

'use strict';
// smates trace shim for Node.js (and Bun via --preload).
//
// Loaded into every Node process of a test run through NODE_OPTIONS=--require (Bun:
// BUN_OPTIONS=--preload, and bun test --preload), it appends
// one JSON object per line to $SMATES_TRACE_DIR/proc-<pid>-<threadId>-<nonce>.jsonl (the
// nonce keeps a later process that gets a reused pid out of an earlier one's stream):
//   {"k":"start", pid, ppid, tid, argv, cwd, t, lang, pctx}  (pctx: the SMATES_TRACE_CTX
//                                  inherited, the context of a Python test that started it)
//   {"k":"read","p":"/abs/path"}   a file under the repository was opened for reading
//   {"k":"dir","p":"/abs/dir"}     a directory under the repository was listed
//   {"k":"probe","p":"/abs/path"}  existence of a path was checked (exists/stat/realpath...)
//   {"k":"exec","args":[...],"shell":true,"cwd":"/abs","t":ms}  a child process was started
//                                  ("trunc":true: args cut short, see MAX_ARGV; "rt": the
//                                  runtimes the child's environment traces, runtimesOf)
//   {"k":"tree","p":"/abs/dir"}    everything below a directory may have been read
//   {"k":"code","p":"/abs/file"}   a module was loaded (Bun, which has no V8 coverage)
//   {"k":"env","p":"NAME"}         an environment variable was read (never its value)
//   {"k":"ctx","id":"test:/abs/file","t":ms}  attribution context switch (runner hooks)
//   {"k":"lost","n":2}             that many events before this one could not be written
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
    // The context of the (Python) test this process was started in, "<key>:<context>":
    // the start event records it, and children inherit it unchanged.
    SMATES_TRACE_CTX: process.env.SMATES_TRACE_CTX || '',
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
  const nonce = Date.now().toString(36) + Math.floor(Math.random() * 0x7fffffff).toString(36);
  const out = path.join(traceDir, `proc-${process.pid}-${threadId}-${nonce}.jsonl`);
  let fd = -1;
  // mock-fs swaps the fs binding's functions for its fake file system's while it is
  // active (_mockedBinding), which would take the trace with them: the shim writes with
  // the real ones. process.binding warns under --pending-deprecation only (also given as
  // NODE_PENDING_DEPRECATION=1), which the shim then does without; the warning (fatal
  // with --throw-deprecation) is also kept off while it is called.
  let fsBinding = null;
  if (
    process.env.NODE_PENDING_DEPRECATION !== '1' &&
    !/--pending-deprecation/.test(env0.NODE_OPTIONS + ' ' + process.execArgv.join(' '))
  ) {
    // --no-deprecation makes process.noDeprecation read-only (and true).
    const quiet = process.noDeprecation;
    const setQuiet = (v) => {
      try {
        if (!quiet) process.noDeprecation = v;
      } catch (_) {}
    };
    setQuiet(true);
    try {
      fsBinding = process.binding('fs');
    } catch (_) {}
    setQuiet(quiet);
  }
  // An event that cannot be written (a full disk, a descriptor the program closed) is
  // counted, and the next one written is preceded by {"k":"lost","n":count}: smates then
  // takes the process for one that may have read anything. A write cut short leaves part
  // of a line, which the next write ends first (smates counts it as lost too). Events
  // are written as bytes: writeSync may write less than it was given.
  let lost = 0;
  let partial = false;
  const write = (obj) => {
    const mocked = fsBinding && fsBinding._mockedBinding;
    try {
      if (mocked) fsBinding._mockedBinding = undefined;
      if (fd < 0) fd = openSync0(out, 'a');
      let line = JSON.stringify(obj) + '\n';
      if (lost) line = JSON.stringify({ k: 'lost', n: lost }) + '\n' + line;
      if (partial) line = '\n' + line;
      const buf = Buffer.from(line);
      for (let off = 0; off < buf.length; ) {
        const n = writeSync0(fd, buf, off, buf.length - off);
        if (!(n > 0)) throw new Error('short write');
        off += n;
        partial = off < buf.length;
      }
      lost = 0;
    } catch (e) {
      lost++;
      // The program closed the descriptor (a test closing every fd): open the file
      // again for the next event, which then says what was lost.
      if (e && e.code === 'EBADF') fd = -1;
    } finally {
      if (mocked) fsBinding._mockedBinding = mocked;
    }
  };
  // Command lines are cut to this many characters in all (a test may pass megabytes of
  // arguments, to provoke E2BIG say); exec events mark it ("trunc") and smates then
  // takes the command for one that may read anything below its working directory.
  const MAX_ARGV = 256 * 1024;
  const cutArgv = (argv) => {
    let n = 0;
    for (let i = 0; i < argv.length; i++) {
      n += String(argv[i]).length;
      if (n > MAX_ARGV) {
        const a = String(argv[i]);
        return [argv.slice(0, i).concat(a.slice(0, a.length - (n - MAX_ARGV))), true];
      }
    }
    return [argv, false];
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
      // Module ids that name no file: Node's builtins (node:fs, also joined to a
      // directory by resolvers probing them) and bundlers' virtual modules ("\0vite/...").
      if (!s || s.includes('\0') || s.startsWith('node:') || s.includes(path.sep + 'node:')) return;
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
  wrap(fs, 'openAsBlob', 'read'); // Node 19.8+
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
  // opts are the options the child is started with, its environment traced (withEnv).
  const execEv = (args, shell, opts) => {
    try {
      const [cut, trunc] = cutArgv(args.map(String));
      const ev = { k: 'exec', args: cut, shell: Boolean(shell), cwd: cwdOf(opts), rt: runtimesOf(opts), t: Date.now() };
      if (trunc) ev.trunc = true;
      write(ev);
    } catch (_) {}
  };
  // runtimesOf lists the runtimes whose shim the environment of a child started with
  // opts loads (its own env, or this process's): smates takes a Node, Bun or Python
  // child whose environment does not trace it for an opaque command.
  const runtimesOf = (opts) => {
    let env = opts && typeof opts === 'object' && opts.env && typeof opts.env === 'object' ? opts.env : null;
    env = env ? envTargets.get(env) || env : envTargets.get(process.env) || process.env;
    const get = (k) => {
      if (process.platform === 'win32') k = Object.keys(env).find((x) => x.toUpperCase() === k) || k;
      const v = env[k];
      return v === undefined || v === null ? '' : String(v);
    };
    if (!get('SMATES_TRACE_DIR') || !get('SMATES_REPO_ROOT')) return [];
    const rt = [];
    if (shimNames.some((n) => get('NODE_OPTIONS').includes(n))) rt.push('node');
    if (shimNames.some((n) => get('BUN_OPTIONS').includes(n))) rt.push('bun');
    if (pythonEntry() && get('PYTHONPATH').split(path.delimiter).includes(pyEntry)) rt.push('python');
    return rt;
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
    set('SMATES_TRACE_CTX', env0.SMATES_TRACE_CTX);
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
  // Bun.$ runs a shell of Bun's own: its builtins (cat, ls...) read files in process, and
  // the programs it starts do not go through Bun.spawn. Each command is recorded as a
  // shell exec event, which smates analyzes like any shell command: the template, with
  // the values interpolated rendered as the words (or, inside quotes, the text) Bun
  // makes of them, run in the shell's directory (.cwd() on the shell or the command).
  // A value the shim cannot render becomes an unset variable, which smates takes for an
  // unknown path.
  const UNKNOWN_VALUE = '$SMATES_UNKNOWN_VALUE';
  const shQuote = (s) => "'" + String(s).replace(/'/g, "'\\''") + "'";
  // The words of a value, null when unknown: a Bun.file stands for its path, other data
  // a redirect reads or writes (Response, Blob, buffers, streams) for no file.
  const shellWords = (v) => {
    if (typeof v === 'string' || typeof v === 'number' || typeof v === 'boolean' || typeof v === 'bigint') return [String(v)];
    if (Array.isArray(v)) {
      const out = [];
      for (const x of v) {
        const w = shellWords(x);
        if (!w) return null;
        out.push(...w);
      }
      return out;
    }
    if (!v || typeof v !== 'object') return null;
    if (typeof Blob === 'function' && v instanceof Blob) return [typeof v.name === 'string' && v.name ? path.resolve(v.name) : '/dev/null'];
    const data = [globalThis.Response, globalThis.ReadableStream, ArrayBuffer, globalThis.SharedArrayBuffer];
    if (ArrayBuffer.isView(v) || data.some((C) => typeof C === 'function' && v instanceof C)) return ['/dev/null'];
    return null;
  };
  // q: the quote the value is in ("", "'" or '"').
  const valueText = (v, q) => {
    const words = shellWords(v);
    if (!words) return q === '"' ? UNKNOWN_VALUE : q === "'" ? `'"${UNKNOWN_VALUE}"'` : `"${UNKNOWN_VALUE}"`;
    if (q === '"') return words.join(' ').replace(/[\\"$`]/g, '\\$&');
    if (q === "'") return "'" + shQuote(words.join(' ')) + "'";
    return words.map(shQuote).join(' ');
  };
  // scanQuotes follows the quotes of shell text (st: {q, esc}).
  const scanQuotes = (text, st) => {
    for (const c of text) {
      if (st.esc) st.esc = false;
      else if (c === '\\' && st.q !== "'") st.esc = true;
      else if (st.q === '' && (c === "'" || c === '"')) st.q = c;
      else if (c === st.q) st.q = '';
    }
  };
  const shellCommand = (strings, values) => {
    const parts = Array.isArray(strings.raw) ? strings.raw : strings;
    const st = { q: '', esc: false };
    let cmd = '';
    for (let i = 0; i < parts.length; i++) {
      const part = String(parts[i]);
      cmd += part;
      scanQuotes(part, st);
      if (i >= parts.length - 1 || i >= values.length) continue;
      const v = values[i];
      if (v && typeof v === 'object' && !Array.isArray(v) && typeof v.raw === 'string') {
        cmd += v.raw;
        scanQuotes(v.raw, st);
      } else {
        cmd += valueText(v, st.q);
      }
    }
    return cmd;
  };
  // traceShell returns Bun.$ traced, invisibly to the program: a Proxy of it (typeof,
  // name, length, own keys, prototype and instanceof stay those of Bun.$; its methods,
  // called on the proxy, keep their state on Bun.$ itself). A call records the command
  // in the shell's directory and environment, read from the state Bun keeps on the shell
  // (Symbol(cwd), Symbol(env)); an environment set with $.env() gets the tracing
  // variables added. Commands are recorded when they are made, before they run.
  //
  // Shells made with new $.Shell() are not wrapped ($.Shell cannot be replaced: Bun
  // freezes it, and wrapping the shells it makes shows). Every command of every shell
  // runs through ShellPromise.prototype.then (await, .text(), .run()...): one of a shell
  // not traced is recorded there as an unknown command, which may read anything in the
  // repository. ShellPromise.prototype.cwd records a command again in the directory
  // given, and .env() adds the tracing variables, both with their descriptors, names and
  // lengths kept.
  const tracedShells = new WeakSet();
  const shellCommands = new WeakMap(); // ShellPromise -> records its command in a directory
  const ranPromises = new WeakSet();
  const shellState = (shell, name) => {
    for (const s of Object.getOwnPropertySymbols(shell)) {
      if (s.description === name) return { sym: s, value: shell[s] };
    }
    return null;
  };
  // replaceMethod replaces the function proto[name] with make(orig), keeping the
  // property's attributes and the function's name and length. make returns a method
  // (shorthand syntax), which like a class method has no prototype and is no constructor.
  const patched = new WeakSet();
  const replaceMethod = (proto, name, make) => {
    const d = Object.getOwnPropertyDescriptor(proto, name);
    if (!d || typeof d.value !== 'function' || patched.has(d.value)) return;
    const orig = d.value;
    const f = make(orig);
    Object.defineProperty(f, 'name', Object.getOwnPropertyDescriptor(orig, 'name') || { value: orig.name });
    Object.defineProperty(f, 'length', Object.getOwnPropertyDescriptor(orig, 'length') || { value: orig.length });
    patched.add(f);
    Object.defineProperty(proto, name, Object.assign({}, d, { value: f }));
  };
  const unknownCommand = () => execEv([`"${UNKNOWN_VALUE}"`], true, { cwd: path.sep });
  const patchShellPromise = (SP) => {
    const proto = SP && SP.prototype;
    if (!proto || typeof proto !== 'object') return;
    replaceMethod(
      proto,
      'then',
      (orig) =>
        ({
          then() {
            try {
              if (this && typeof this === 'object' && !ranPromises.has(this)) {
                ranPromises.add(this);
                if (!shellCommands.has(this)) unknownCommand();
              }
            } catch (_) {}
            return Reflect.apply(orig, this, arguments);
          },
        }).then,
    );
    replaceMethod(
      proto,
      'cwd',
      (orig) =>
        ({
          cwd(dir) {
            try {
              const ev = shellCommands.get(this);
              const d = dir === undefined || dir === null ? '.' : toPath(dir);
              if (ev && d !== null) ev(path.resolve(d || '.'));
            } catch (_) {}
            return Reflect.apply(orig, this, arguments);
          },
        }).cwd,
    );
    replaceMethod(
      proto,
      'env',
      (orig) =>
        ({
          env(e) {
            let args = arguments;
            try {
              if (e && typeof e === 'object') args = [tracedEnv(e, false)].concat(Array.prototype.slice.call(arguments, 1));
            } catch (_) {}
            return Reflect.apply(orig, this, args);
          },
        }).env,
    );
  };
  const traceShell = (orig) => {
    try {
      patchShellPromise(orig.ShellPromise);
    } catch (_) {}
    const proxy = new Proxy(orig, {
      apply(target, thisArg, args) {
        let ev = null;
        try {
          const strings = args[0];
          if (strings && typeof strings === 'object' && Array.isArray(strings.raw)) {
            const cmd = shellCommand(strings, Array.prototype.slice.call(args, 1));
            const cwd = shellState(target, 'cwd');
            const env = shellState(target, 'env');
            const opts = { env: env && env.value && typeof env.value === 'object' ? env.value : undefined };
            ev = (dir) => execEv([cmd], true, Object.assign({ cwd: dir }, opts));
            ev(cwd && typeof cwd.value === 'string' && cwd.value ? path.resolve(cwd.value) : undefined);
          }
        } catch (_) {}
        const p = Reflect.apply(target, thisArg, args);
        try {
          if (ev && p && typeof p === 'object') shellCommands.set(p, ev);
        } catch (_) {}
        return p;
      },
      set(target, key, value, receiver) {
        try {
          // $.env(obj): the shell's children get the tracing variables too.
          if (typeof key === 'symbol' && key.description === 'env' && value && typeof value === 'object') value = tracedEnv(value, false);
        } catch (_) {}
        return Reflect.set(target, key, value, receiver);
      },
    });
    tracedShells.add(proxy);
    return proxy;
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
          const traced = withEnv(arguments, i, true);
          if (ev) ev(traced || arguments, i);
          return traced;
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

  // node:sqlite's DatabaseSync(path) opens a database file. Loading node:sqlite prints
  // an ExperimentalWarning, so the shim patches it when the program loads it: with
  // require or process.getBuiltinModule (an ESM import made before either is not seen).
  const sqlitePatched = new WeakSet();
  const patchSqlite = (exp) => {
    try {
      if (!exp || typeof exp !== 'object' || sqlitePatched.has(exp)) return exp;
      sqlitePatched.add(exp);
      const Orig = exp.DatabaseSync;
      if (typeof Orig !== 'function' || Orig.__smates) return exp;
      const DatabaseSync = class DatabaseSync extends Orig {
        constructor(p) {
          try {
            if (toPath(p) !== ':memory:') record('read', p);
          } catch (_) {}
          super(...arguments);
        }
      };
      Object.defineProperty(DatabaseSync, '__smates', { value: true });
      Object.defineProperty(DatabaseSync, 'length', { value: Orig.length });
      exp.DatabaseSync = DatabaseSync;
      require('module').syncBuiltinESMExports();
    } catch (_) {}
    return exp;
  };
  try {
    const Module = require('module');
    const load = Module._load;
    if (typeof load === 'function' && !load.__smatesSqlite) {
      const patched = function (request) {
        const exp = load.apply(this, arguments);
        return request === 'node:sqlite' ? patchSqlite(exp) : exp;
      };
      Object.defineProperty(patched, '__smatesSqlite', { value: true });
      Module._load = patched;
    }
    const getBuiltin = process.getBuiltinModule;
    if (typeof getBuiltin === 'function' && !getBuiltin.__smates) {
      const getBuiltinModule = function getBuiltinModule(id) {
        const exp = getBuiltin.apply(this, arguments);
        return id === 'node:sqlite' ? patchSqlite(exp) : exp;
      };
      Object.defineProperty(getBuiltinModule, '__smates', { value: true });
      process.getBuiltinModule = getBuiltinModule;
    }
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
    // (below) starts its programs itself.
    for (const name of ['spawn', 'spawnSync']) {
      try {
        const orig = Bun[name];
        if (typeof orig !== 'function' || orig.__smates) continue;
        Bun[name] = wrapFn(orig, function (cmd, opts) {
          if (spawning.n) return;
          const i = Array.isArray(cmd) ? 1 : 0;
          const argv = i ? cmd : cmd && cmd.cmd;
          const traced = withEnv(arguments, i);
          if (Array.isArray(argv)) execEv(argv, false, (traced || arguments)[i]);
          return traced;
        });
      } catch (_) {}
    }
    try {
      if (typeof Bun.$ === 'function' && !tracedShells.has(Bun.$)) Bun.$ = traceShell(Bun.$);
    } catch (_) {}
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
    argv: cutArgv(process.argv)[0],
    cwd: process.cwd(),
    t: Date.now(),
    lang: typeof Bun !== 'undefined' ? 'bun' : 'node',
    pctx: env0.SMATES_TRACE_CTX,
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
    // The file names of the frames on the stack: V8's call sites (no stack text is
    // formatted, nor a prepareStackTrace of the program's called), else the stack text.
    const callSites = (_, sites) => sites;
    const stackFiles = () => {
      const limit = Error.stackTraceLimit;
      const prepare = Error.prepareStackTrace;
      let sites = null;
      try {
        Error.stackTraceLimit = 64;
        Error.prepareStackTrace = callSites;
        const holder = {};
        Error.captureStackTrace(holder, stackFiles);
        sites = holder.stack;
      } catch (_) {
      } finally {
        Error.prepareStackTrace = prepare;
        Error.stackTraceLimit = limit;
      }
      if (Array.isArray(sites)) {
        const out = [];
        for (const s of sites) {
          let f = null;
          try {
            f = s && typeof s.getFileName === 'function' ? s.getFileName() : null;
          } catch (_) {}
          if (typeof f === 'string' && f) out.push(f);
        }
        return out;
      }
      let stack = '';
      try {
        Error.stackTraceLimit = 64;
        stack = String(new Error().stack || '');
      } finally {
        Error.stackTraceLimit = limit;
      }
      return Array.from(stack.matchAll(frameRe), (m) => m[1]);
    };
    const fromRepo = () => {
      let first = true;
      for (const name of stackFiles()) {
        const f = name.startsWith('file:') ? safeFileURL(name) : name;
        if (!f || f === self || !path.isAbsolute(f)) continue;
        // The test framework itself, e.g. vi.stubEnv saving the value it restores later.
        if (first && runnerRe.test(f)) return false;
        first = false;
        if (f.startsWith(rootPrefix) && !f.includes(nm)) return true;
      }
      return false;
    };
    const recorded = new Set(); // context + name
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
          // Every read is checked until one by repository code is seen: a library
          // reading a variable over and over must not hide a later read by a test.
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
    // The last chance to say that events were lost (the exit event among them): once
    // more, with the file opened again.
    if (lost) {
      fd = -1;
      write({ k: 'exit', code, t: Date.now() });
    }
    // A pool worker that exits by itself may still get the pool's SIGTERM (jest-worker
    // restarting a worker over its idle memory limit) before Node writes the coverage
    // at exit, and no handler runs after "exit": write it now.
    if (pooled) {
      try {
        require('v8').takeCoverage();
      } catch (_) {}
    }
  });

  // Pool workers are often stopped with a signal; V8 coverage is only written on a
  // normal exit, so flush it first, then let the default action (or other handlers) run.
  // Only the process pool workers of known runners (forked with an IPC channel, stopped
  // with SIGTERM) get the listener: it defers the signal until the event loop is free,
  // so any other forked child that is busy would no longer be killed by it.
  const poolWorker =
    /[\\/](?:tinypool[\\/]dist[\\/](?:esm[\\/])?entry[\\/]process|vitest[\\/]dist[\\/]workers[\\/](?:vmF|f)orks|jest-worker[\\/]build[\\/](?:workers[\\/])?processChild)\.[cm]?js$/;
  const pooled = Boolean(isMainThread && process.channel && poolWorker.test(process.argv[1] || ''));
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
    for (const sig of poolWorker.test(process.argv[1] || '') ? ['SIGTERM'] : []) {
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

  // Any other process stopped with a signal (a server global setup starts and teardown
  // kills) leaves no coverage either (Bun: no loaded modules). Where the program handles
  // the signal itself, the signal waits for the event loop anyway: a listener of the
  // shim's own, put first, records what ran so far and changes nothing else. A process
  // without a handler keeps the default action, which kills it at once even while it is
  // busy; smates then takes its code for unknown (a Node or Bun process without exit
  // event or coverage counts as running repository code).
  // The shim's listener goes away with the program's last one, so the default action
  // comes back (also after a once() handler ran).
  if (isMainThread && !pooled) {
    const ours = new Map(); // signal -> the shim's listener
    const alone = (sig, f) => process.listeners(sig).every((l) => l === f);
    const drop = (sig, f) => {
      ours.delete(sig);
      process.removeListener(sig, f);
    };
    const flushOn = (sig) => {
      const f = () => {
        loadedModules();
        if (typeof Bun === 'undefined') {
          try {
            require('v8').takeCoverage();
          } catch (_) {}
        }
        if (alone(sig, f)) {
          drop(sig, f);
          process.kill(process.pid, sig);
        }
      };
      ours.set(sig, f);
      process.prependListener(sig, f);
    };
    const signals = new Set(['SIGTERM', 'SIGINT', 'SIGHUP']);
    try {
      process.on('newListener', (name) => {
        try {
          if (signals.has(name) && !ours.has(name)) flushOn(name);
        } catch (_) {}
      });
      process.on('removeListener', (name) => {
        try {
          const f = ours.get(name);
          if (!f) return;
          if (!process.listeners(name).includes(f)) ours.delete(name); // removeAllListeners
          else if (alone(name, f)) drop(name, f);
        } catch (_) {}
      });
    } catch (_) {}
  }
})();

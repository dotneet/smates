'use strict';
// smates trace shim for Node.js (and Bun via --preload).
//
// Loaded into every Node process of a test run through NODE_OPTIONS=--require, it appends
// one JSON object per line to $SMATES_TRACE_DIR/proc-<pid>-<threadId>.jsonl:
//   {"k":"start", pid, ppid, tid, argv, cwd, t, lang}
//   {"k":"read","p":"/abs/path"}   a file under the repository was opened for reading
//   {"k":"dir","p":"/abs/dir"}     a directory under the repository was listed
//   {"k":"probe","p":"/abs/path"}  existence of a path was checked (exists/stat/access)
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
  const out = path.join(traceDir, `proc-${process.pid}-${threadId}.jsonl`);
  let fd = -1;
  const write = (obj) => {
    try {
      if (fd < 0) fd = openSync0(out, 'a');
      writeSync0(fd, JSON.stringify(obj) + '\n');
    } catch (_) {}
  };
  const rootPrefix = repoRoot.endsWith(path.sep) ? repoRoot : repoRoot + path.sep;
  const nm = path.sep + 'node_modules' + path.sep;
  const seen = new Set();

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
  // Tracing must never throw into user code (a deleted cwd, odd path objects...).
  const record = (kind, p) => {
    try {
      let s = toPath(p);
      if (!s) return;
      if (!path.isAbsolute(s)) s = path.resolve(s);
      if (!s.startsWith(rootPrefix) || s.includes(nm)) return;
      const key = kind + '\0' + s;
      if (seen.has(key)) return;
      seen.add(key);
      write({ k: kind, p: s });
    } catch (_) {}
  };

  const wrap = (obj, name, kind, isOpen) => {
    const orig = obj && obj[name];
    if (typeof orig !== 'function' || orig.__smates) return;
    const wrapped = function (p, flags) {
      try {
        if (typeof p !== 'number' && (!isOpen || isReadFlags(flags))) {
          if (typeof kind === 'function') kind.apply(null, arguments);
          else record(kind, p);
        }
      } catch (_) {}
      return orig.apply(this, arguments);
    };
    Object.defineProperty(wrapped, '__smates', { value: true });
    Object.defineProperty(wrapped, 'name', { value: orig.name });
    for (const k of Reflect.ownKeys(orig)) {
      if (k === 'length' || k === 'name' || k === 'prototype') continue;
      try {
        Object.defineProperty(wrapped, k, Object.getOwnPropertyDescriptor(orig, k));
      } catch (_) {}
    }
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
  for (const name of ['readdirSync', 'readdir', 'opendirSync', 'opendir']) wrap(fs, name, 'dir');
  // A test whose outcome depends on whether a file exists depends on that file's
  // existence (even when it does not exist yet).
  for (const name of ['existsSync', 'exists', 'statSync', 'stat', 'lstatSync', 'lstat', 'accessSync', 'access']) wrap(fs, name, 'probe');
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
    wrap(fsp, 'readdir', 'dir');
    wrap(fsp, 'opendir', 'dir');
    for (const name of ['stat', 'lstat', 'access']) wrap(fsp, name, 'probe');
    wrap(fsp, 'copyFile', 'read');
    wrap(fsp, 'cp', recordCp);
  }
  // Child processes: Node and Python children trace themselves; for anything else
  // (shell scripts, grep, make...) smates infers dependencies from the command line.
  try {
    const cp = require('child_process');
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
    const wrapCP = (name, fn) => {
      const orig = cp[name];
      if (typeof orig !== 'function' || orig.__smates) return;
      const wrapped = function () {
        try {
          fn.apply(null, arguments);
        } catch (_) {}
        return orig.apply(this, arguments);
      };
      Object.defineProperty(wrapped, '__smates', { value: true });
      for (const k of Reflect.ownKeys(orig)) {
        if (k === 'length' || k === 'name' || k === 'prototype') continue;
        try {
          Object.defineProperty(wrapped, k, Object.getOwnPropertyDescriptor(orig, k));
        } catch (_) {}
      }
      cp[name] = wrapped;
    };
    const fileForm = (file, args, opts) => {
      if (!Array.isArray(args)) {
        opts = args;
        args = [];
      }
      const shell = opts && typeof opts === 'object' && opts.shell;
      execEv(shell ? [[file].concat(args).join(' ')] : [file].concat(args), shell, opts);
    };
    for (const name of ['spawn', 'spawnSync', 'execFile', 'execFileSync']) wrapCP(name, fileForm);
    for (const name of ['exec', 'execSync']) wrapCP(name, (cmd, opts) => execEv([cmd], true, opts));
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
              const pat = patterns.get(this) || '**';
              const fixed = pat.split('/').filter((s, i, a) => i < a.length - 1 && !/[*?[{]/.test(s));
              const dir = path.join(cwd, ...fixed);
              if (pat.includes('**')) write({ k: 'tree', p: dir });
              else record('dir', path.join(cwd, ...pat.split('/').slice(0, -1)));
            } catch (_) {}
            return orig.apply(this, arguments);
          };
        }
        Object.defineProperty(proto, '__smates', { value: true });
        Bun.Glob = Glob;
      }
    } catch (_) {}
  }

  globalThis.__smatesTrace = {
    // Runner integrations call this to attribute subsequent activity to a test file.
    context(id) {
      write({ k: 'ctx', id: String(id), t: Date.now() });
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
    let envCtx = '';
    // envProxy traces an environment object (initial: its values now).
    const envProxy = (target) => {
      const initial = Object.assign(Object.create(null), target);
      const note = (env, name) => {
        try {
          if (typeof name !== 'string') return;
          const own = hasOwn.call(env, name);
          if (!own && (name in Object.prototype || protocol.has(name))) return;
          const key = envCtx + '\0' + name;
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
      return new Proxy(target, {
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
    const context = globalThis.__smatesTrace.context;
    globalThis.__smatesTrace.context = function (id) {
      envCtx = String(id);
      return context.apply(this, arguments);
    };
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

  process.on('exit', (code) => {
    // Bun has no V8 coverage: the module cache tells which modules were loaded.
    if (typeof Bun !== 'undefined') {
      try {
        for (const k of Object.keys(require.cache)) record('code', k);
      } catch (_) {}
    }
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

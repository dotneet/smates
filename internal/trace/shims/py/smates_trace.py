"""smates trace shim for Python.

Records which repository files a process executes or reads and appends JSON lines to
$SMATES_TRACE_DIR/proc-<pid>-0.jsonl, using the same protocol as the Node shim:

    {"k": "start", "pid", "ppid", "tid", "argv", "cwd", "t", "lang": "python", "pctx"}
    {"k": "code", "p": "/abs/file.py"}     a code object from this file started executing
    {"k": "read", "p": "/abs/file"}        a file was opened for reading
    {"k": "dir", "p": "/abs/dir"}          a directory was listed
    {"k": "probe", "p": "/abs/path"}       existence of a path was checked (stat/access)
    {"k": "exec", "args": [...], "shell": false, "cwd": "/abs"}  a child process started
    {"k": "env", "p": "NAME"}              an environment variable was read (never its value)
    {"k": "ctx", "id": "test:/abs/file", "t": ms}   attribution context switch
    {"k": "uses", "id": "/abs/test_file", "ctx": ["fixture:..."]}
    {"k": "result", "id": "/abs/test_file", "status": "pass|fail", "ms": 12}
    {"k": "exit", "code": 0, "t": ms}

Code execution is observed with sys.monitoring (Python 3.12+; each code object reports
once per context, then is disabled until the next context switch) or sys.setprofile on
older versions. Reads come from the "open"/"os.listdir"/"os.scandir"/"sqlite3.connect"
audit events; environment variables from os.environ (os.getenv, os.environ.get, `in`,
indexing).

A process switching contexts exports the current one to its children as
SMATES_TRACE_CTX="<pid>-0:<context>", and a child records the value it inherited as
"pctx": smates attributes the child to the context it was started in, even when the child
starts up later (a server a session fixture starts without waiting for it). A forked child
(multiprocessing's fork start method) records into a file of its own, as a new process
(once it does more than run code before an exec), and follows the contexts of the process
that forked it through shared memory: {"k": "pctx", "id": "<pid>-0:<context>"} starts the
events recorded while the parent was in that context.

Children started with an environment of their own (subprocess's env=, os.execve,
os.posix_spawn) get the tracing variables (SMATES_*, the shim directory on PYTHONPATH and,
when Node is traced, NODE_OPTIONS) added where missing, so they see them in their
environment.
"""

import functools
import os
import sys
import threading
import time

_state = None
_os_open = os.open  # unwrapped (the shim writes its own files with it)


class _Tracer:
    def __init__(self, trace_dir, repo_root):
        self.trace_dir = trace_dir
        self.repo_root = repo_root
        self.root = os.path.join(os.path.realpath(repo_root), "")
        self.out = os.path.join(trace_dir, "proc-%d-0.jsonl" % os.getpid())
        # Re-entrant: write() raises audit events and a signal handler may record while
        # the lock is held.
        self.lock = threading.RLock()
        self.seen = set()
        self.ctx = ""
        self.stack = []
        self.enabled = True
        self.tool = None
        self.exported = None  # the SMATES_TRACE_CTX value children get
        self._shared = None  # memory forked children read the current context from
        self._follow = None  # a forked child: its parent's shared memory
        self._gen = 0  # the generation of the parent's context last seen in _follow
        self._pending = None  # a forked child that has not written its start event yet
        self._real = {}
        self._code_real = {}
        self._seen_codes = {}  # sys.setprofile: code object -> context it was recorded in
        self._probes = {}  # the original os.stat/lstat/access
        self._cwd = None
        self._local = threading.local()  # re-entrancy guard for the recording path
        self._json = __import__("json")
        importlib = __import__("importlib")
        self._importlib = os.path.join(os.path.dirname(os.path.abspath(importlib.__file__)), "")

    # -- output -------------------------------------------------------------
    def write(self, obj):
        pending = self._pending
        if pending is not None:
            # A forked child that only ran code so far may be about to exec (preexec_fn,
            # os.spawn* on POSIX, pty.fork): that code is the parent's business.
            if obj["k"] in ("code", "pctx") and len(pending[1]) < 1000:
                pending[1].append(obj)
                return
            self._begin()
        self._write(obj)

    def _begin(self, argv=None, own=True):
        """Writes a forked child's start event and what it buffered. argv: the program
        the child execs; own: whether its children have it as their traced ancestor."""
        pending, self._pending = self._pending, None
        if pending is None:
            return
        start, buffered = pending
        if argv is not None:
            start["argv"] = argv
        self._write(start)
        for obj in buffered:
            self._write(obj)
        if own:
            os.environ["SMATES_TRACE_PARENT"] = "%d-0" % os.getpid()

    def _write(self, obj):
        line = self._json.dumps(obj) + "\n"
        try:
            # os.open/os.write do not raise "open" audit events for our own file.
            fd = _os_open(self.out, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o644)
            try:
                os.write(fd, line.encode("utf-8", "surrogateescape"))
            finally:
                os.close(fd)
        except OSError:
            pass

    def _norm(self, p, root=False):
        if isinstance(p, bytes):
            p = os.fsdecode(p)
        if not isinstance(p, str) or not p or p.startswith("<"):
            return None
        if not os.path.isabs(p):
            cwd = self._cwd
            if cwd is None:
                try:
                    cwd = self._cwd = os.getcwd()
                except OSError:
                    return None
            p = os.path.join(cwd, p)
        real = self._real.get(p)
        if real is None:
            try:
                real = os.path.realpath(p)
            except (OSError, ValueError):
                real = p
            self._real[p] = real
        if not real.startswith(self.root) and not (root and real == self.root[:-1]):
            return None
        if "/site-packages/" in real or "/node_modules/" in real or "/__pycache__/" in real or "/.venv/" in real:
            return None
        return real

    def record(self, kind, p):
        # Anything recording does (path resolution, writing the trace) may raise
        # audit events or call wrapped functions again: never recurse.
        local = self._local
        if not self.enabled or getattr(local, "busy", False):
            return
        local.busy = True
        try:
            self._record(kind, p)
        except Exception:
            pass
        finally:
            local.busy = False

    def _record(self, kind, p):
        # The repository root itself is listed too (os.listdir(".") run at the root).
        real = self._norm(p, kind == "dir")
        if real is None:
            return
        if self._follow is not None:
            self._follow_parent()
        # Which repository source file did this? Modules read data (and import other
        # modules) once per process, at import time or behind a cache; smates gives
        # those reads to every test that depends on the reading module.
        # Fixtures are attributed precisely through their own contexts.
        by = self._by() if kind != "code" and not self.ctx.startswith("fixture:") else None
        key = (self.ctx, kind, real, by)
        if key in self.seen:
            return
        with self.lock:
            if key in self.seen:
                return
            self.seen.add(key)
            ev = {"k": kind, "p": real}
            if by is not None and by != real:
                ev["by"] = by
            self.write(ev)

    def _by(self):
        try:
            f = sys._getframe(2)
        except ValueError:
            return None
        n = 0
        cache = self._code_real
        while f is not None and n < 50:
            code = f.f_code
            real = cache.get(code, False)
            if real is False:
                fn = code.co_filename
                real = None if fn.startswith("<") else self._norm(fn)
                cache[code] = real
            if real is not None:
                return real
            f = f.f_back
            n += 1
        return None

    # -- contexts -----------------------------------------------------------
    def switch(self, ctx):
        with self.lock:
            self.ctx = ctx
            self.write({"k": "ctx", "id": ctx, "t": int(time.time() * 1000)})
            # Children inherit the context they are started in. putenv, not os.environ:
            # tests do not see the variable change when they compare, or save and
            # restore, their environment. Children given an environment of their own
            # get it from child_env.
            self.exported = "%d-0:%s" % (os.getpid(), ctx)
            try:
                os.putenv("SMATES_TRACE_CTX", self.exported)
            except (OSError, ValueError):
                pass
            self._publish(self.exported)
        mon = getattr(sys, "monitoring", None)
        if mon is not None and self.tool is not None:
            mon.restart_events()

    # Forked children (a pool of workers some test or fixture starts, then used by later
    # tests) keep following the context of the process that forked them: it is published
    # in an anonymous shared mapping (inherited across fork) as a generation counter
    # (8 bytes, odd while being written), a length (4 bytes) and the value.
    _SHARED = 4096

    def _publish(self, value):
        m = self._shared
        if m is None:
            if not hasattr(os, "fork"):
                return
            try:
                import mmap

                m = self._shared = mmap.mmap(-1, self._SHARED)
            except Exception:
                self._shared = False
                return
        elif m is False:
            return
        data = value.encode("utf-8", "surrogateescape")[: self._SHARED - 12]
        gen = int.from_bytes(m[0:8], "little")
        m[0:8] = (gen + 1).to_bytes(8, "little")
        m[8:12] = len(data).to_bytes(4, "little")
        m[12 : 12 + len(data)] = data
        m[0:8] = (gen + 2).to_bytes(8, "little")

    def _follow_parent(self):
        m = self._follow
        gen = int.from_bytes(m[0:8], "little")
        if gen == self._gen or gen & 1:
            return
        n = int.from_bytes(m[8:12], "little")
        value = bytes(m[12 : 12 + n]).decode("utf-8", "surrogateescape")
        if int.from_bytes(m[0:8], "little") != gen:
            return  # changing: next time
        self._gen = gen
        # The parent is in another context: what this child does now belongs to that one.
        with self.lock:
            self.seen = set()
            self._seen_codes.clear()
            self.write({"k": "pctx", "id": value})
        mon = getattr(sys, "monitoring", None)
        if mon is not None and self.tool is not None:
            mon.restart_events()

    def push(self, ctx):
        self.stack.append(self.ctx)
        self.switch(ctx)

    def pop(self):
        self.switch(self.stack.pop() if self.stack else "")

    # -- hooks --------------------------------------------------------------
    def _start_event(self, anc, pctx):
        return {
            "k": "start",
            "pid": os.getpid(),
            "ppid": os.getppid(),
            "anc": anc,
            "tid": 0,
            "argv": list(sys.argv) if hasattr(sys, "argv") else [],
            "cwd": _safe_cwd(),
            "t": int(time.time() * 1000),
            "lang": "python",
            "pctx": pctx,
        }

    def install(self):
        env = os.environ
        # Children of a process that does not switch contexts belong to the context the
        # process itself was started in: they inherit its SMATES_TRACE_CTX unchanged.
        self.exported = env.get("SMATES_TRACE_CTX") or None
        self.write(self._start_event(env.get("SMATES_TRACE_PARENT", ""), self.exported or ""))
        # Children started through a shell still find their traced ancestor.
        env["SMATES_TRACE_PARENT"] = "%d-0" % os.getpid()
        # What a child given an environment of its own needs to be traced (child_env).
        here = os.path.dirname(os.path.abspath(__file__))
        self._child_vars = [("SMATES_TRACE_DIR", self.trace_dir), ("SMATES_REPO_ROOT", self.repo_root)]
        self._pythonpath = here
        self._node_shim = os.path.join(os.path.dirname(here), "node_modules", "smates", "node.cjs")
        if self._node_shim not in env.get("NODE_OPTIONS", ""):
            self._node_shim = None  # Node is not traced
        elif env.get("NODE_V8_COVERAGE"):
            self._child_vars.append(("NODE_V8_COVERAGE", env["NODE_V8_COVERAGE"]))
        sys.addaudithook(self._audit)
        self._wrap_probes()
        self._wrap_environ()
        self._wrap_spawn()
        if not self._monitor():
            self._profile()
        if hasattr(os, "register_at_fork"):
            os.register_at_fork(after_in_child=self._forked)
            # Children forked before the first context switch follow from no context.
            self._publish("%d-0:" % os.getpid())
        import atexit

        atexit.register(self._exit)

    def _forked(self):
        # A forked child (multiprocessing's fork start method, os.fork) is a process of
        # its own: it records into its own file, from scratch (what the parent recorded
        # does not cover what the child does later, for a later test), and belongs to
        # the contexts its parent is in while it works (_follow_parent); its children
        # inherit SMATES_TRACE_CTX as the parent exported it. The context stays the
        # child's current one, so a child that switches contexts itself (pytest-forked
        # runs each test's hooks in one) returns to it. The start event waits for the
        # first event other than executed code (see write).
        try:
            self.lock = threading.RLock()  # another thread may have held it
            self.out = os.path.join(self.trace_dir, "proc-%d-0.jsonl" % os.getpid())
            self.seen = set()
            self._seen_codes.clear()
            if self._shared is not None and self._shared is not False:
                self._follow, self._shared = self._shared, None
            if self._follow is not None:
                self._gen = int.from_bytes(self._follow[0:8], "little")
            self._local.busy = True  # not a read of the program
            try:
                anc = os.environ.get("SMATES_TRACE_PARENT", "")
            finally:
                self._local.busy = False
            # The parent's context: one it exported (or inherited), else none of its own
            # yet (a pool a conftest creates at import), followed from there.
            pctx = self.exported or "%d-0:%s" % (os.getppid(), self.ctx)
            self._pending = (self._start_event(anc, pctx), [])
            mon = getattr(sys, "monitoring", None)
            if mon is not None and self.tool is not None:
                mon.restart_events()  # code the parent ran is disabled
        except Exception:
            pass

    # Tool ids smates tries, in order. 0 (debugger), 1 (coverage) and 2 (profiler) are
    # the ids reserved for those tools (coverage.py starts at 1 and moves up when it is
    # taken), so smates prefers the unassigned ones.
    TOOL_IDS = (3, 4, 2, 5)

    def _monitor(self):
        """Observes executed code with sys.monitoring (3.12+), as a tool of its own, so
        that coverage.py (sysmon core), profilers and debuggers keep theirs. Returns
        False when sys.monitoring is missing or every candidate id is taken."""
        mon = getattr(sys, "monitoring", None)
        if mon is None:
            return False
        for tool in self.TOOL_IDS:
            try:
                if mon.get_tool(tool) is not None:
                    continue
                mon.use_tool_id(tool, "smates")
            except ValueError:  # taken in between
                continue
            try:
                disable = mon.DISABLE

                def on_start(code, offset):
                    self.record("code", code.co_filename)
                    return disable

                mon.register_callback(tool, mon.events.PY_START, on_start)
                mon.set_events(tool, mon.events.PY_START)
            except Exception:
                try:
                    mon.free_tool_id(tool)
                except Exception:
                    pass
                return False
            self.tool = tool
            return True
        return False

    def _profile(self):
        """Observes executed code with sys.setprofile (before 3.12, or without a free
        sys.monitoring id). A profile function installed before keeps being called;
        coverage.py's tracers use sys.settrace, which is independent."""
        seen_codes = self._seen_codes
        tracer = self

        def chain(prev):
            def profile(frame, event, arg):
                if event == "call":
                    code = frame.f_code
                    if seen_codes.get(code) != tracer.ctx:
                        seen_codes[code] = tracer.ctx
                        tracer.record("code", code.co_filename)
                if prev is not None:
                    prev(frame, event, arg)

            return profile

        sys.setprofile(chain(sys.getprofile()))
        getprofile = getattr(threading, "getprofile", None)  # 3.10+
        threading.setprofile(chain(getprofile() if getprofile is not None else None))

    def _wrap_probes(self):
        # os.path.exists/isfile/isdir and pathlib call these at run time; the import
        # system uses its own bound copies, so imports are not affected.
        tracer = self
        local = self._local

        def wrap(name):
            orig = getattr(os, name, None)
            if orig is None or getattr(orig, "__smates__", False):
                return

            def probe(path, *args, **kwargs):
                # record() guards against re-entry (realpath calls os.lstat). A path
                # relative to dir_fd (shutil.rmtree) is not one relative to the cwd.
                if not isinstance(path, int) and (kwargs.get("dir_fd") is None or os.path.isabs(path)):
                    try:
                        tracer.record("probe", os.fspath(path))
                    except TypeError:
                        pass
                return orig(path, *args, **kwargs)

            tracer._probes[name] = orig
            _replace_os(name, orig, probe)

        for name in ("stat", "lstat", "access"):
            wrap(name)

        orig_open = os.open
        if not getattr(orig_open, "__smates__", False):

            def open_(path, *args, **kwargs):
                # The "open" audit event does not tell a path relative to dir_fd.
                if kwargs.get("dir_fd") is None or isinstance(path, int) or os.path.isabs(path):
                    return orig_open(path, *args, **kwargs)
                outer = getattr(local, "dir_fd", False)
                local.dir_fd = True
                try:
                    return orig_open(path, *args, **kwargs)
                finally:
                    local.dir_fd = outer

            _replace_os("open", orig_open, open_)

    def _wrap_spawn(self):
        # A child given an environment of its own (subprocess's env=, os.execve,
        # os.posix_spawn) without the tracing variables would go untraced, while smates
        # takes what traced programs (Python, Node) use from their own trace.
        tracer = self
        local = self._local

        def with_env(orig, i):
            def spawn(*args, **kwargs):
                if len(args) > i:
                    args = args[:i] + (tracer.child_env(args[i]),) + args[i + 1 :]
                elif "env" in kwargs:
                    kwargs["env"] = tracer.child_env(kwargs["env"])
                return orig(*args, **kwargs)

            return spawn

        # os.execle/execlpe/execvpe and, on POSIX, os.spawn*e call os.execve.
        for name, i in (("execve", 2), ("posix_spawn", 2), ("posix_spawnp", 2), ("spawnve", 3)):
            orig = getattr(os, name, None)
            if orig is None or getattr(orig, "__smates__", False):
                continue
            if name == "spawnve" and not isinstance(orig, type(os.getpid)):
                continue  # Python code calling os.execve (POSIX)
            _replace_os(name, orig, with_env(orig, i))

        orig_exit = os._exit
        if not getattr(orig_exit, "__smates__", False):

            def _exit(*args, **kwargs):
                # Forked workers (multiprocessing's) end with os._exit: one that only ran
                # code reports it still.
                if tracer._pending is not None and tracer._pending[1]:
                    tracer._begin()
                return orig_exit(*args, **kwargs)

            _replace_os("_exit", orig_exit, _exit)

        def patch(subprocess):
            cls = getattr(subprocess, "Popen", None)
            orig = getattr(cls, "_execute_child", None)
            code = getattr(orig, "__code__", None)
            if code is None or getattr(orig, "__smates__", False):
                return
            names = code.co_varnames[: code.co_argcount]
            if "env" not in names:
                return
            spawn = with_env(orig, names.index("env"))

            def _execute_child(*args, **kwargs):
                # Popen records the child itself, not the os.posix_spawn it may use.
                outer = getattr(local, "popen", False)
                local.popen = True
                try:
                    return spawn(*args, **kwargs)
                finally:
                    local.popen = outer

            _execute_child.__smates__ = True
            _execute_child.__wrapped__ = orig
            cls._execute_child = _execute_child

        # Not imported here: that would slow down the start of every traced process.
        _on_import("subprocess", patch)

        def patch_mp(process):
            # A forked multiprocessing worker is a process of its own from the start
            # (one that only runs code may be terminated, ending with no os._exit).
            cls = getattr(process, "BaseProcess", None)
            orig = getattr(cls, "_bootstrap", None)
            if orig is None or getattr(orig, "__smates__", False):
                return

            def _bootstrap(*args, **kwargs):
                if tracer._pending is not None:
                    tracer._begin()
                return orig(*args, **kwargs)

            _bootstrap.__smates__ = True
            _bootstrap.__wrapped__ = orig
            cls._bootstrap = _bootstrap

        _on_import("multiprocessing.process", patch_mp)

    def child_env(self, env):
        """Returns env, a child's own environment, with the variables that trace the
        child added where missing (as a copy; env itself when nothing is missing). The
        child sees them: SMATES_TRACE_DIR, SMATES_REPO_ROOT, SMATES_TRACE_PARENT,
        SMATES_TRACE_CTX, the shim directory first on PYTHONPATH and, when Node is
        traced, the shim's --require in NODE_OPTIONS (and NODE_V8_COVERAGE)."""
        local = self._local
        outer = getattr(local, "busy", False)
        local.busy = True  # these lookups (env may be os.environ) are no reads of the program
        try:
            return self._child_env(env)
        finally:
            local.busy = outer

    def _child_env(self, env):
        try:
            if env is None or not self.enabled:
                return env
            keys = list(env.keys())
            enc = os.fsencode if keys and isinstance(keys[0], bytes) else str
            # Names are case-insensitive on Windows.
            fold = (lambda k: os.fsdecode(k).upper()) if os.name == "nt" else os.fsdecode
            actual = {fold(k): k for k in keys}

            def get(name):
                k = actual.get(fold(name))
                return None if k is None else os.fsdecode(env[k])

            add = {}
            for name, value in self._child_vars + [("SMATES_TRACE_PARENT", "%d-0" % os.getpid())]:
                if fold(name) not in actual:
                    add[name] = value
            if self.exported is not None and get("SMATES_TRACE_CTX") != self.exported:
                add["SMATES_TRACE_CTX"] = self.exported
            cur = get("PYTHONPATH") or ""
            if self._pythonpath not in cur.split(os.pathsep):
                add["PYTHONPATH"] = self._pythonpath + (os.pathsep + cur if cur else "")
            if self._node_shim is not None:
                cur = get("NODE_OPTIONS") or ""
                if self._node_shim not in cur:
                    shim = self._node_shim
                    if any(c in shim for c in " \t\"'\\"):
                        shim = self._json.dumps(shim)  # as smates quotes it (NodeEnv)
                    add["NODE_OPTIONS"] = ("--require=" + shim + " " + cur).strip()
            if not add:
                return env
            out = dict(env)
            for name, value in add.items():
                out[actual.get(fold(name), enc(name))] = enc(value)
            return out
        except Exception:
            return env

    def _wrap_environ(self):
        # Names of the variables read with repository code on the stack (not the
        # runner's or a library's own lookups at their import), and only of values this
        # process did not set itself. Copying the whole environment (dict(os.environ),
        # os.environ.copy(), items()) is not a read of every variable: traced children
        # record their own reads.
        cls = type(os.environ)
        if getattr(cls, "__smates__", False):
            return
        tracer = self
        initial = dict(os.environ)
        local = self._local
        skip = ("os", "_collections_abc", "collections.abc")

        def caller(f):
            # The first frame outside os and the Mapping mixins (frozen modules, so
            # recognized by name).
            while f is not None and f.f_globals.get("__name__") in skip:
                f = f.f_back
            return f

        orig_iter = cls.__iter__
        orig_getitem = cls.__getitem__

        def __iter__(self_):
            # Remembers where the enumeration happens: lookups made there (at the same
            # instruction) are the copy, not reads of single variables.
            f = caller(sys._getframe(1))
            if f is not None:
                local.enum = (id(f), f.f_code, f.f_lasti)
            return orig_iter(self_)

        def __getitem__(self_, key):
            # A missing variable (KeyError) is a read too; other errors (a key that is
            # not a string) are the caller's and propagate unchanged.
            try:
                value = orig_getitem(self_, key)
            except KeyError:
                if tracer.enabled and not getattr(local, "busy", False):
                    tracer._env_read(self_, key, None, initial, caller)
                raise
            if tracer.enabled and not getattr(local, "busy", False):
                tracer._env_read(self_, key, value, initial, caller)
            return value

        cls.__iter__ = __iter__
        cls.__getitem__ = __getitem__
        cls.__smates__ = True

    def _env_read(self, env, key, value, initial, caller):
        local = self._local
        local.busy = True
        try:
            if isinstance(key, bytes):
                name, value = os.fsdecode(key), None if value is None else os.fsdecode(value)
            else:
                name = key
            if not isinstance(name, str) or initial.get(name) != value:
                return  # set by this process
            f = caller(sys._getframe(2))
            if f is None or getattr(local, "enum", None) == (id(f), f.f_code, f.f_lasti):
                return
            module = f.f_globals.get("__name__") or ""
            if module.split(".")[0] in ("_pytest", "pytest", "pluggy"):
                return  # pytest itself, e.g. monkeypatch saving the value it restores later
            # The repository file nearest on the stack: a module reading the variable at
            # import (once per process) passes it on to every test depending on it.
            by = None
            n = 0
            while f is not None and n < 50:
                fn = f.f_code.co_filename
                real = self._code_real.get(f.f_code, False)
                if real is False:
                    real = self._code_real[f.f_code] = None if fn.startswith("<") else self._norm(fn)
                if real is not None:
                    by = real
                    break
                f = f.f_back
                n += 1
            if by is None:
                return
            if self.ctx.startswith("fixture:"):
                by = None
            if self._follow is not None:
                self._follow_parent()
            k = (self.ctx, "env", name, by)
            if k in self.seen:
                return
            with self.lock:
                if k in self.seen:
                    return
                self.seen.add(k)
                ev = {"k": "env", "p": name}
                if by is not None:
                    ev["by"] = by
                self.write(ev)
        except Exception:
            pass
        finally:
            local.busy = False

    def _audit(self, event, args):
        if event in ("os.chdir", "os.fchdir"):
            self._cwd = None
            return
        if event == "open":
            path, mode, flags = args[0], args[1], args[2]
            if isinstance(path, int) or getattr(self._local, "dir_fd", False):
                return
            # os.open reports no mode, only flags: opening write-only is not a read.
            if isinstance(flags, int) and (flags & 3) == os.O_WRONLY:
                if mode is None or not ("r" in mode or "+" in mode):
                    return
            self.record("read", path)
        elif event == "subprocess.Popen":
            _exe, cmd, cwd, _env = args
            shell = isinstance(cmd, (str, bytes))
            argv = [cmd] if shell else list(cmd)
            self.exec_event(argv, shell, cwd)
        elif event == "os.system":
            self.exec_event([args[0]], True, None)
        elif event in ("os.exec", "os.posix_spawn", "os.spawn"):
            # os.exec* and os.posix_spawn[p] report (path, argv, env), os.spawn* (on
            # Windows; on POSIX they fork and exec) the mode first. subprocess records the
            # children it starts with os.posix_spawn itself.
            if event == "os.posix_spawn" and getattr(self._local, "popen", False):
                return
            try:
                argv = list(args[2] if event == "os.spawn" else args[1])
            except (TypeError, IndexError):
                return
            self.exec_event(argv, False, None, event == "os.exec")
        elif event == "sqlite3.connect":
            self._sqlite(args[0] if args else None)
        elif event in ("os.listdir", "os.scandir"):
            # The import system lists directories to find modules; that is covered by
            # the import itself, not a dependency on the directory's contents.
            # listdir/scandir are C functions without a frame: frame 1 (this hook's
            # caller) is the code that called them, e.g. importlib's FileFinder (frozen,
            # or the stdlib's importlib package without frozen modules).
            try:
                caller = sys._getframe(1).f_code.co_filename
            except ValueError:
                caller = ""
            if caller.startswith("<frozen importlib") or caller.startswith(self._importlib):
                return
            path = args[0] if args and args[0] is not None else "."
            # Libraries scanning sys.path for distributions or modules (pkg_resources,
            # the importlib_metadata backport) list it the same way; `python -m pytest`
            # puts the repository root on it.
            if ("/site-packages/" in caller or "/dist-packages/" in caller) and self._on_sys_path(path):
                return
            self.record("dir", path)

    def _on_sys_path(self, path):
        local = self._local
        if isinstance(path, int) or getattr(local, "busy", False):
            return False
        local.busy = True  # realpath calls the wrapped os.lstat
        try:
            real = self._norm(path, True)
            return real is not None and any(self._norm(e or ".", True) == real for e in list(sys.path))
        except Exception:
            return False
        finally:
            local.busy = False

    def _sqlite(self, db):
        # A database file that exists when connected to is an input; one the connection
        # creates is the test's own output. The event does not tell whether db is a URI
        # (uri=True); a path starting with "file:" practically always is one.
        try:
            db = os.fsdecode(os.fspath(db))
        except TypeError:
            return
        if db.startswith("file:"):
            db, _, query = db[5:].partition("?")
            if "mode=memory" in query:
                return
            if db.startswith("//"):  # file://host/path
                db = db[2:]
                db = db[db.find("/") :] if "/" in db else ""
            if "%" in db:
                from urllib.parse import unquote

                db = unquote(db)
        if not db or db == ":memory:":
            return
        try:
            self._probes.get("stat", os.stat)(db)
        except (OSError, ValueError):
            return
        self.record("read", db)

    def exec_event(self, argv, shell, cwd, replace=False):
        """Records a child process; replace: this process execs it (os.exec*)."""
        if not self.enabled:
            return
        try:
            argv = [os.fsdecode(a) if isinstance(a, bytes) else str(a) for a in argv]
            cwd = os.path.abspath(os.fsdecode(cwd)) if cwd else _safe_cwd()
        except Exception:
            return
        if self._follow is not None:
            self._follow_parent()
        if replace and self._pending is not None:
            # A forked child that execs (os.spawn* on POSIX, pty.fork) is that program;
            # a traced one records itself, with the same traced ancestor.
            self._begin(argv, own=False)
        self.write({"k": "exec", "args": argv, "shell": shell, "cwd": cwd})

    def _exit(self):
        self.write({"k": "exit", "code": None, "t": int(time.time() * 1000)})
        self.enabled = False


def _replace_os(name, orig, fn):
    """Replaces os.<name> with fn, as a callable that does not bind as a method like the
    builtin it replaces (pathlib keeps os.stat and others as class attributes before
    3.11), also in the os.supports_* sets listing the original (shutil picks its rmtree
    implementation from them)."""
    new = functools.partial(fn)
    for attr in ("__name__", "__qualname__", "__doc__", "__module__"):
        try:
            setattr(new, attr, getattr(orig, attr))
        except (AttributeError, TypeError):
            pass
    new.__wrapped__ = orig
    new.__smates__ = True
    setattr(os, name, new)
    for s in ("supports_dir_fd", "supports_fd", "supports_follow_symlinks", "supports_effective_ids"):
        funcs = getattr(os, s, None)
        if isinstance(funcs, set) and orig in funcs:
            funcs.add(new)


class _PatchingLoader:
    """Runs a module with its own loader, then patches it."""

    def __init__(self, loader, patch):
        self._loader = loader
        self._patch = patch

    def create_module(self, spec):
        create = getattr(self._loader, "create_module", None)
        return create(spec) if create is not None else None

    def exec_module(self, module):
        self._loader.exec_module(module)
        try:
            self._patch(module)
        except Exception:
            pass

    def __getattr__(self, name):  # get_source, get_code, ...
        return getattr(self._loader, name)


class _ImportPatcher:
    """A meta path finder that finds nothing itself: it has the other finders find one
    module and returns that spec with a loader that patches the module once it has run
    (again when it is reloaded or imported anew)."""

    def __init__(self, name, patch):
        self.name = name
        self.patch = patch

    def find_spec(self, fullname, path=None, target=None):
        if fullname != self.name:
            return None
        for finder in list(sys.meta_path):
            find = getattr(finder, "find_spec", None)
            if finder is self or find is None:
                continue
            spec = find(fullname, path, target)
            if spec is not None:
                break
        else:
            return None
        if not hasattr(spec.loader, "exec_module"):
            return spec
        import copy

        spec = copy.copy(spec)
        spec.loader = _PatchingLoader(spec.loader, self.patch)
        return spec


def _on_import(name, patch):
    """Calls patch(module) whenever module name is imported (now, if it already is)."""
    sys.meta_path.insert(0, _ImportPatcher(name, patch))
    mod = sys.modules.get(name)
    if mod is not None:
        try:
            patch(mod)
        except Exception:
            pass


def _safe_cwd():
    try:
        return os.getcwd()
    except OSError:
        return ""


def install():
    """Start tracing this process (idempotent). Returns the tracer or None."""
    global _state
    if _state is not None:
        return _state
    trace_dir = os.environ.get("SMATES_TRACE_DIR")
    repo_root = os.environ.get("SMATES_REPO_ROOT")
    if not trace_dir or not repo_root:
        return None
    _state = _Tracer(trace_dir, repo_root)
    _state.install()
    return _state


def tracer():
    return _state

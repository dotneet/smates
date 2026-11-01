"""smates trace shim for Python.

Records which repository files a process executes or reads and appends JSON lines to
$SMATES_TRACE_DIR/proc-<pid>-0.jsonl, using the same protocol as the Node shim:

    {"k": "start", "pid", "ppid", "tid", "argv", "cwd", "t", "lang": "python"}
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
older versions. Reads come from the "open"/"os.listdir"/"os.scandir" audit events;
environment variables from os.environ (os.getenv, os.environ.get, `in`, indexing).
"""

import os
import sys
import threading
import time

_state = None


class _Tracer:
    def __init__(self, trace_dir, repo_root):
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
        self._real = {}
        self._code_real = {}
        self._cwd = None
        self._local = threading.local()  # re-entrancy guard for the recording path
        self._json = __import__("json")

    # -- output -------------------------------------------------------------
    def write(self, obj):
        line = self._json.dumps(obj) + "\n"
        try:
            # os.open/os.write do not raise "open" audit events for our own file.
            fd = os.open(self.out, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o644)
            try:
                os.write(fd, line.encode("utf-8", "surrogateescape"))
            finally:
                os.close(fd)
        except OSError:
            pass

    def _norm(self, p):
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
        if not real.startswith(self.root):
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
        real = self._norm(p)
        if real is None:
            return
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
        mon = getattr(sys, "monitoring", None)
        if mon is not None and self.tool is not None:
            mon.restart_events()

    def push(self, ctx):
        self.stack.append(self.ctx)
        self.switch(ctx)

    def pop(self):
        self.switch(self.stack.pop() if self.stack else "")

    # -- hooks --------------------------------------------------------------
    def install(self):
        self.write(
            {
                "k": "start",
                "pid": os.getpid(),
                "ppid": os.getppid(),
                "anc": os.environ.get("SMATES_TRACE_PARENT", ""),
                "tid": 0,
                "argv": list(sys.argv) if hasattr(sys, "argv") else [],
                "cwd": _safe_cwd(),
                "t": int(time.time() * 1000),
                "lang": "python",
            }
        )
        # Children started through a shell still find their traced ancestor.
        os.environ["SMATES_TRACE_PARENT"] = "%d-0" % os.getpid()
        sys.addaudithook(self._audit)
        self._wrap_probes()
        self._wrap_environ()
        mon = getattr(sys, "monitoring", None)
        if mon is not None:
            for tool in (3, 4, 2, 5):
                try:
                    if mon.get_tool(tool) is None:
                        mon.use_tool_id(tool, "smates")
                        self.tool = tool
                        break
                except ValueError:
                    continue
        if self.tool is not None:
            disable = mon.DISABLE

            def on_start(code, offset):
                self.record("code", code.co_filename)
                return disable

            mon.register_callback(self.tool, mon.events.PY_START, on_start)
            mon.set_events(self.tool, mon.events.PY_START)
        else:
            seen_codes = {}

            def profile(frame, event, arg):
                if event == "call":
                    code = frame.f_code
                    if seen_codes.get(code) != self.ctx:
                        seen_codes[code] = self.ctx
                        self.record("code", code.co_filename)

            sys.setprofile(profile)
            threading.setprofile(profile)
        import atexit

        atexit.register(self._exit)

    def _wrap_probes(self):
        # os.path.exists/isfile/isdir and pathlib call these at run time; the import
        # system uses its own bound copies, so imports are not affected.
        tracer = self

        def wrap(name):
            orig = getattr(os, name, None)
            if orig is None or getattr(orig, "__smates__", False):
                return

            def probe(path, *args, **kwargs):
                # record() guards against re-entry (realpath calls os.lstat).
                if not isinstance(path, int):
                    try:
                        tracer.record("probe", os.fspath(path))
                    except TypeError:
                        pass
                return orig(path, *args, **kwargs)

            probe.__smates__ = True
            probe.__wrapped__ = orig
            setattr(os, name, probe)

        for name in ("stat", "lstat", "access"):
            wrap(name)

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
            if isinstance(path, int):
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
        elif event in ("os.listdir", "os.scandir"):
            # The import system lists directories to find modules; that is covered by
            # the import itself, not a dependency on the directory's contents.
            # listdir/scandir are C functions without a frame: frame 1 (this hook's
            # caller) is the code that called them, e.g. importlib's FileFinder or a
            # module body, which itself runs below importlib frames while imported.
            try:
                caller = sys._getframe(1).f_code.co_filename
            except ValueError:
                caller = ""
            if "importlib" in caller:
                return
            self.record("dir", args[0] if args and args[0] is not None else ".")

    def exec_event(self, argv, shell, cwd):
        if not self.enabled:
            return
        try:
            argv = [os.fsdecode(a) if isinstance(a, bytes) else str(a) for a in argv]
            cwd = os.path.abspath(os.fsdecode(cwd)) if cwd else _safe_cwd()
        except Exception:
            return
        self.write({"k": "exec", "args": argv, "shell": shell, "cwd": cwd})

    def _exit(self):
        self.write({"k": "exit", "code": None, "t": int(time.time() * 1000)})
        self.enabled = False


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

"""smates pytest plugin: attributes traced activity to test files.

Loaded through PYTEST_PLUGINS while smates records. Contexts:
    collect:<file>   importing a test module (module-level code, imports)
    test:<file>      running a test item of that file (setup, call, teardown)
    fixture:<name>@<baseid>   setting up a fixture (shared fixtures are set up once,
                              so their dependencies are added to every test using them)
"""

import os
import time

import pytest

import smates_trace

_tracer = smates_trace.install()
_results = {}


def _file_of(node):
    path = getattr(node, "path", None)
    if path is None:
        path = getattr(node, "fspath", None)
    return os.path.realpath(str(path)) if path is not None else None


if _tracer is not None:

    @pytest.hookimpl(hookwrapper=True)
    def pytest_make_collect_report(collector):
        f = _file_of(collector) if isinstance(collector, pytest.File) else None
        if f is None:
            yield
            return
        _tracer.push("collect:" + f)
        try:
            yield
        finally:
            _tracer.pop()

    @pytest.hookimpl(hookwrapper=True)
    def pytest_runtest_protocol(item, nextitem):
        f = _file_of(item)
        start = time.time()
        _tracer.push("test:" + f)
        try:
            yield
        finally:
            _tracer.pop()
            r = _results.setdefault(f, {"ms": 0, "failed": False})
            r["ms"] += int((time.time() - start) * 1000)
            names = getattr(item, "fixturenames", ()) or ()
            defs = getattr(getattr(item, "_fixtureinfo", None), "name2fixturedefs", {}) or {}
            ctx = []
            for name in names:
                for fd in defs.get(name, ()):
                    ctx.append("fixture:%s@%s" % (fd.argname, fd.baseid))
            if ctx:
                _tracer.write({"k": "uses", "id": f, "ctx": sorted(set(ctx))})

    @pytest.hookimpl(hookwrapper=True)
    def pytest_fixture_setup(fixturedef, request):
        _tracer.push("fixture:%s@%s" % (fixturedef.argname, fixturedef.baseid))
        try:
            yield
        finally:
            _tracer.pop()

    _failed_nodeids = set()
    # Failure output per test file (bounded: it only feeds smates' failure hints).
    _messages = {}
    _MAX_MSG = 4000

    def _note_failure(report):
        f = report.nodeid.split("::", 1)[0]
        _failed_nodeids.add(f)
        prev = _messages.get(f, "")
        if len(prev) >= _MAX_MSG:
            return
        try:
            text = report.longreprtext or ""
        except Exception:
            text = ""
        if len(text) > 1500:
            text = text[:200] + "\n[…]\n" + text[-1300:]  # the assertion is at the end
        msg = (prev + "\n\n" if prev else "") + report.nodeid + ": " + text
        _messages[f] = msg[:_MAX_MSG]

    def pytest_runtest_logreport(report):
        if report.failed:
            _note_failure(report)

    def pytest_collectreport(report):
        if report.failed:
            _note_failure(report)

    @pytest.hookimpl(trylast=True)
    def pytest_sessionfinish(session, exitstatus):
        rootdir = str(session.config.rootpath)
        failed = {}
        for n in _failed_nodeids:
            if n:
                failed[os.path.realpath(os.path.join(rootdir, n))] = _messages.get(n, "")
        for f, r in _results.items():
            ev = {"k": "result", "id": f, "status": "fail" if f in failed else "pass", "ms": r["ms"]}
            if failed.get(f):
                ev["msg"] = failed[f]
            _tracer.write(ev)
        for f, msg in failed.items():
            if f not in _results:
                ev = {"k": "result", "id": f, "status": "fail", "ms": 0}
                if msg:
                    ev["msg"] = msg
                _tracer.write(ev)

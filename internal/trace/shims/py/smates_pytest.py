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

    def pytest_runtest_logreport(report):
        if report.failed:
            _failed_nodeids.add(report.nodeid.split("::", 1)[0])

    def pytest_collectreport(report):
        if report.failed:
            _failed_nodeids.add(report.nodeid.split("::", 1)[0])

    @pytest.hookimpl(trylast=True)
    def pytest_sessionfinish(session, exitstatus):
        rootdir = str(session.config.rootpath)
        failed = {os.path.realpath(os.path.join(rootdir, n)) for n in _failed_nodeids if n}
        for f, r in _results.items():
            status = "fail" if f in failed else "pass"
            _tracer.write({"k": "result", "id": f, "status": status, "ms": r["ms"]})
        for f in failed:
            if f not in _results:
                _tracer.write({"k": "result", "id": f, "status": "fail", "ms": 0})

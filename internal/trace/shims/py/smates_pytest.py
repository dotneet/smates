"""smates pytest plugin: attributes traced activity to test files.

Loaded through PYTEST_PLUGINS while smates records, and without tracing (smates run
--no-record) to write per-file results only. Contexts:
    collect:<file>   importing a test module (module-level code, imports)
    test:<file>      running a test item of that file (setup, call, teardown)
    fixture:<name>@<baseid>   setting up a fixture (shared fixtures are set up once,
                              so their dependencies are added to every test using them)
    session:end      the end of a session that ran tests (reports such as pytest-cov's),
                     attributed to no test

A session a test runs in-process (pytester's runpytest()) is part of that test: the
plugin, loaded into it too, attributes nothing of its own there and reports no results.
"""

import json
import os
import time

import pytest

import smates_trace

_tracer = smates_trace.install()
_results = {}
# Without tracing, results go to a file of their own in $SMATES_TRACE_DIR.
_results_dir = os.environ.get("SMATES_TRACE_DIR") if _tracer is None else None
# Sessions running in this process, the outermost first.
_sessions = []
# Contexts of the fixtures the running test requested beyond its declared ones.
_requested = set()
_running = []  # the test file running (outside nested sessions)
_ended = False  # session:end is active


def _nested():
    return len(_sessions) > 1


def _file_of(node):
    path = getattr(node, "path", None)
    if path is None:
        path = getattr(node, "fspath", None)
    return os.path.realpath(str(path)) if path is not None else None


def _fixture_ctx(fixturedef):
    return "fixture:%s@%s" % (fixturedef.argname, fixturedef.baseid)


def _write_result(ev):
    if _tracer is not None:
        _tracer.write(ev)
        return
    try:
        with open(os.path.join(_results_dir, "results-pytest-%d.jsonl" % os.getpid()), "a") as f:
            f.write(json.dumps(ev) + "\n")
    except OSError:
        pass


if _tracer is not None:

    @pytest.hookimpl(hookwrapper=True)
    def pytest_make_collect_report(collector):
        f = _file_of(collector) if isinstance(collector, pytest.File) and not _nested() else None
        if f is None:
            yield
            return
        _tracer.push("collect:" + f)
        try:
            yield
        finally:
            _tracer.pop()

    @pytest.hookimpl(hookwrapper=True)
    def pytest_fixture_setup(fixturedef, request):
        if _nested():
            yield
            return
        ctx = _fixture_ctx(fixturedef)
        if _running:
            _requested.add(ctx)  # also when requested dynamically
        _tracer.push(ctx)
        try:
            yield
        finally:
            _tracer.pop()

    @pytest.hookimpl(hookwrapper=True)
    def pytest_runtest_teardown(item, nextitem):
        yield
        if _nested():
            return
        # Fixtures resolved by the test's request, those requested dynamically
        # (request.getfixturevalue) included: one set up for an earlier test is cached,
        # so pytest_fixture_setup does not see it again. The request is gone afterwards.
        defs = getattr(getattr(item, "_request", None), "_fixture_defs", None)
        if isinstance(defs, dict):
            for fd in defs.values():
                _requested.add(_fixture_ctx(fd))


if _tracer is not None or _results_dir:

    @pytest.hookimpl(tryfirst=True)
    def pytest_sessionstart(session):
        global _ended
        if not _sessions and _ended:
            # An earlier session of this process (pytest.main called again) is over.
            _ended = False
            _tracer.pop()
        _sessions.append(session)

    @pytest.hookimpl(hookwrapper=True)
    def pytest_runtest_protocol(item, nextitem):
        if _nested():
            yield
            return
        f = _file_of(item)
        start = time.time()
        _running.append(f)
        _requested.clear()
        if _tracer is not None:
            _tracer.push("test:" + f)
        try:
            yield
        finally:
            _running.pop()
            r = _results.setdefault(f, {"ms": 0, "failed": False})
            r["ms"] += int((time.time() - start) * 1000)
            if _tracer is not None:
                _tracer.pop()
                names = getattr(item, "fixturenames", ()) or ()
                defs = getattr(getattr(item, "_fixtureinfo", None), "name2fixturedefs", {}) or {}
                ctx = set(_requested)
                for name in names:
                    for fd in defs.get(name, ()):
                        ctx.add(_fixture_ctx(fd))
                if ctx:
                    _tracer.write({"k": "uses", "id": f, "ctx": sorted(ctx)})

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
        if report.failed and not _nested():
            _note_failure(report)

    def pytest_collectreport(report):
        if report.failed and not _nested():
            _note_failure(report)

    @pytest.hookimpl(hookwrapper=True, trylast=True)
    def pytest_runtestloop(session):
        # Every test has run: what the session does afterwards (pytest-cov reading every
        # source file under --cov for its report, then the terminal summary) cannot
        # change an outcome, so it is no dependency of any test. trylast: this wrapper
        # resumes before the other plugins' (pytest-cov reports in its own wrapper).
        # Only in a process that ran tests: a pytest-xdist controller has no test
        # contexts of its own.
        global _ended
        yield
        if _results and _tracer is not None and not _nested() and not _ended:
            _ended = True
            _tracer.push("session:end")

    @pytest.hookimpl(trylast=True)
    def pytest_sessionfinish(session, exitstatus):
        if _sessions and session is not _sessions[0]:
            if session in _sessions:
                _sessions.remove(session)
            return
        rootdir = str(session.config.rootpath)
        failed = {}
        for n in _failed_nodeids:
            if n:
                failed[os.path.realpath(os.path.join(rootdir, n))] = _messages.get(n, "")
        for f, r in _results.items():
            ev = {"k": "result", "id": f, "status": "fail" if f in failed else "pass", "ms": r["ms"]}
            if failed.get(f):
                ev["msg"] = failed[f]
            _write_result(ev)
        for f, msg in failed.items():
            if f not in _results:
                ev = {"k": "result", "id": f, "status": "fail", "ms": 0}
                if msg:
                    ev["msg"] = msg
                _write_result(ev)
        _results.clear()
        _failed_nodeids.clear()
        _messages.clear()
        del _sessions[:]

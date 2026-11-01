"""smates pytest plugin: attributes traced activity to test files.

Loaded through PYTEST_PLUGINS while smates records, and without tracing (smates run
--no-record) to write per-file results only. Contexts:
    collect:<file>   importing a test module (module-level code, imports)
    test:<file>      running a test item of that file (setup, call, teardown)
    fixture:<name>@<baseid>   setting up a fixture (shared fixtures are set up once,
                              so their dependencies are added to every test using them)
    session:end      the end of a session that ran tests (reports such as pytest-cov's),
                     attributed to no test

A session a test runs, in-process (pytester's runpytest()) or in a child process
(runpytest_subprocess(), subprocess.run([sys.executable, "-m", "pytest", ...]), which
inherits the test's context), is part of that test: the plugin, loaded into it too,
attributes nothing of its own there and reports no results.

Results are per test file, keyed by the file of the item (a node id names files outside
the rootdir relative to the arguments, if at all). A file some of whose collected tests
did not run (-x, --maxfail, pytest.exit(), an interrupt) did not pass: it is reported
"unknown", also by a pytest-xdist controller (its workers each run some of the tests).
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


def _outer_test():
    # The context this process was started in (SMATES_TRACE_CTX as inherited): one of
    # a test of another session, whose children are its helpers.
    ctx = (getattr(_tracer, "exported", None) or "").partition(":")[2]
    return ctx.partition(":")[0] in ("test", "collect", "fixture")


_outer = _outer_test()
# Sessions running in this process, the outermost first.
_sessions = []
# Contexts of the fixtures the running test requested beyond its declared ones.
_requested = set()
_running = []  # the test file running (outside nested sessions)
_ended = False  # session:end is active
_expected = {}  # test file -> items collected
_done = {}  # test file -> items whose protocol completed
_root = [""]  # the session's rootpath
_real = {}

_STOPPED = (
    "smates: the run stopped (-x, --maxfail, pytest.exit() or an interrupt) before every "
    "test of this file ran; not recorded as passed."
)


def _nested():
    return _outer or len(_sessions) > 1


def _realpath(p):
    real = _real.get(p)
    if real is None:
        real = _real[p] = os.path.realpath(p)
    return real


def _file_of(node):
    path = getattr(node, "path", None)
    if path is None:
        path = getattr(node, "fspath", None)
    return _realpath(str(path)) if path is not None else None


def _fixture_ctx(fixturedef):
    return "fixture:%s@%s" % (fixturedef.argname, fixturedef.baseid)


def _write_result(ev, final=False):
    """final: a status the process trace cannot carry (smates combines one recorded
    there with the run's exit code), written to a results file even when tracing."""
    if _tracer is not None and not final:
        _tracer.write(ev)
        return
    out = os.path.join(os.environ.get("SMATES_TRACE_DIR", ""), "results-pytest-%d.jsonl" % os.getpid())
    try:
        with open(out, "a") as f:
            f.write(json.dumps(ev) + "\n")
    except OSError:
        pass


if _tracer is not None:

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
    # Failure output per test file (bounded: it only feeds smates' failure hints).
    _failed = {}
    _MAX_MSG = 4000
    # Collect reports noted in this process (pytest_collectreport sees them again).
    _noted = {}
    # A pytest-xdist controller: node ids its workers collected and finished, and the
    # test files of the reports it got per node id prefix.
    _xdist_ids = {}
    _xdist_done = {}
    _xdist_files = {}

    @pytest.hookimpl(tryfirst=True)
    def pytest_sessionstart(session):
        global _ended
        if not _sessions and _ended:
            # An earlier session of this process (pytest.main called again) is over.
            _ended = False
            _tracer.pop()
        if not _sessions:
            config = session.config
            _root[0] = str(getattr(config, "rootpath", None) or config.rootdir)
        _sessions.append(session)

    @pytest.hookimpl(hookwrapper=True)
    def pytest_make_collect_report(collector):
        if _nested():
            yield
            return
        f = _file_of(collector)
        push = _tracer is not None and f is not None and isinstance(collector, pytest.File)
        if push:
            _tracer.push("collect:" + f)
        try:
            outcome = yield
        finally:
            if push:
                _tracer.pop()
        if outcome.excinfo is None and f is not None:
            rep = outcome.get_result()
            if rep.failed:
                _noted[id(rep)] = rep
                _note_failure(f, rep)

    def pytest_collection_finish(session):
        if _nested():
            return
        for item in session.items:
            f = _file_of(item)
            _expected[f] = _expected.get(f, 0) + 1

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
        outcome = None
        try:
            outcome = yield
        finally:
            _running.pop()
            r = _results.setdefault(f, {"ms": 0})
            r["ms"] += int((time.time() - start) * 1000)
            if outcome is not None and outcome.excinfo is None:
                _done[f] = _done.get(f, 0) + 1  # not stopped by pytest.exit() or ^C
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

    def _note_failure(f, report):
        prev = _failed.get(f, "")
        if len(prev) >= _MAX_MSG:
            return
        try:
            text = report.longreprtext or ""
        except Exception:
            text = ""
        if len(text) > 1500:
            text = text[:200] + "\n[…]\n" + text[-1300:]  # the assertion is at the end
        msg = (prev + "\n\n" if prev else "") + report.nodeid + ": " + text
        _failed[f] = msg[:_MAX_MSG]

    def _report_file(report):
        """The test file of a report made in another process (a pytest-xdist worker's):
        its node id names it relative to the rootdir, unless it is outside of it; the
        location names the file defining the test, relative to the rootdir."""
        root = _root[0]
        head = report.nodeid.split("::", 1)[0]
        if head:
            p = _realpath(os.path.join(root, head))
            if os.path.isfile(p):
                return p
        loc = getattr(report, "location", None)
        if loc and loc[0]:
            return _realpath(os.path.join(root, str(loc[0])))
        return _realpath(os.path.join(root, head)) if head else None

    @pytest.hookimpl(optionalhook=True)
    def pytest_xdist_node_collection_finished(node, ids):
        if _nested() or _xdist_ids:
            return
        for nodeid in ids:
            _xdist_ids[nodeid] = _xdist_ids.get(nodeid, 0) + 1

    def pytest_runtest_logreport(report):
        if _nested():
            return
        if _running:
            f = _running[-1]
        else:
            # On a pytest-xdist controller.
            f = _report_file(report)
            if f is None:
                return
            nodeid = report.nodeid
            _xdist_files.setdefault(nodeid.split("::", 1)[0], set()).add(f)
            if report.when == "teardown":
                _xdist_done[nodeid] = _xdist_done.get(nodeid, 0) + 1
        if report.failed:
            _note_failure(f, report)

    def pytest_collectreport(report):
        if report.failed and not _nested() and id(report) not in _noted:
            f = _report_file(report)  # a pytest-xdist worker's
            if f is not None:
                _note_failure(f, report)

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

    def _stopped(session):
        """Test files some of whose collected tests did not run."""
        out = set()
        if not hasattr(session.config, "workerinput"):  # not a pytest-xdist worker
            for f in _results:
                if _done.get(f, 0) < _expected.get(f, 0):
                    out.add(f)
        for nodeid, n in _xdist_ids.items():
            if _xdist_done.get(nodeid, 0) < n:
                out.update(_xdist_files.get(nodeid.split("::", 1)[0], ()))
        return out

    @pytest.hookimpl(trylast=True)
    def pytest_sessionfinish(session, exitstatus):
        if _sessions and session is not _sessions[0]:
            if session in _sessions:
                _sessions.remove(session)
            return
        if not _outer:
            stopped = _stopped(session) - set(_failed)
            for f, r in _results.items():
                ev = {"k": "result", "id": f, "status": "pass", "ms": r["ms"]}
                if f in _failed:
                    ev["status"] = "fail"
                    if _failed[f]:
                        ev["msg"] = _failed[f]
                elif f in stopped:
                    ev["status"], ev["msg"] = "unknown", _STOPPED
                _write_result(ev, ev["status"] == "unknown")
            for f in sorted(stopped - set(_results)):
                _write_result({"k": "result", "id": f, "status": "unknown", "ms": 0, "msg": _STOPPED}, True)
            for f, msg in _failed.items():
                if f not in _results:
                    ev = {"k": "result", "id": f, "status": "fail", "ms": 0}
                    if msg:
                        ev["msg"] = msg
                    _write_result(ev)
        for m in (_results, _failed, _noted, _expected, _done, _xdist_ids, _xdist_done, _xdist_files):
            m.clear()
        del _sessions[:]

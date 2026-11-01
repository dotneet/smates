"""smates unittest wrapper: `python -m unittest` with attribution per test module.

While smates records a unittest suite it runs `python -m smates_unittest <arguments>`
instead of `python -m unittest <arguments>` (this directory is on PYTHONPATH). It runs
unittest's own main, adding the contexts the pytest plugin uses:

    collect:<file>   importing a test module (module-level code, imports)
    test:<file>      running its tests, including setUpModule/setUpClass, their
                     teardowns, and the test methods themselves

and per-file results ({"k": "result"}): a failure, an error (also in a fixture or while
importing the module) or an unexpected success fails the file; skipped tests and
expected failures pass. A module that loaded but ran no test at all is "unknown" (never
"pass": it runs again next time) when it looks written for pytest (module-level test
functions or `import pytest`, which unittest cannot run), and "pass" with a warning
otherwise (a helper module matching the test pattern). Both are explained on stderr.
"""

import json
import os
import re
import sys
import time
import unittest
import unittest.suite

import smates_trace

_tracer = smates_trace.install()
# File -> {"ms": total test time, "failed": bool, "msg": failure output}.
_results = {}
_MAX_MSG = 4000


def _module_file(name):
    if not name or name == "unittest" or name.startswith("unittest."):
        return None
    f = getattr(sys.modules.get(name), "__file__", None)
    return os.path.realpath(f) if f else None


def _file_of(test):
    f = getattr(test, "_smates_file", None)
    if f:
        return f
    return _module_file(type(test).__module__)


def _class_file(cls):
    return _module_file(getattr(cls, "__module__", None)) if cls is not None else None


def _name_file(name):
    """The file a dotted module name loads from (without importing it)."""
    parts = name.split(".")
    if not all(parts):
        return None
    for entry in sys.path:
        base = os.path.join(entry or os.getcwd(), *parts)
        if os.path.isfile(base + ".py"):
            return os.path.realpath(base + ".py")
    return None


def _tag(test, f):
    tests = getattr(test, "_tests", None)
    if tests is not None:
        for t in tests:
            _tag(t, f)
        return
    try:
        test._smates_file = f
    except Exception:
        pass


def _current_file():
    ctx = _tracer.ctx if _tracer is not None else ""
    return ctx[len("test:"):] if ctx.startswith("test:") else None


def _enter(f):
    """Make test:<f> the current context (no-op without a file)."""
    if f is None or _tracer is None:
        return
    ctx = "test:" + f
    if _tracer.ctx != ctx:
        _tracer.switch(ctx)


def _result(f):
    r = _results.get(f)
    if r is None:
        r = _results[f] = {"ms": 0, "failed": False, "msg": "", "ran": 0}
    return r


_NO_TESTS = (
    "smates: unittest ran no test from this module. unittest only runs unittest.TestCase "
    "methods: if the module holds pytest-style tests (plain `def test_...` functions), "
    "set `runner: pytest` on the suite in smates.yml; if it is a helper matching the test "
    "pattern, add it to the suite's `exclude`."
)
_NO_TESTS_HELPER = (
    "smates: unittest ran no test from this module (a helper matching the test pattern?); "
    "recorded as passed. Add it to the suite's `exclude` in smates.yml, or set "
    "`runner: pytest` if it holds pytest tests."
)
# A test file written for pytest (as runner.PytestStyle in smates): it imports pytest or
# defines module-level test functions other than unittest's `test_suite()` convention.
_PYTEST_STYLE = re.compile(r"^(?:import pytest\b|from pytest import|(?:async )?def (?!test_suite\b)test_\w*)", re.M)


def _pytest_style(f):
    try:
        with open(f, "rb") as fh:
            src = fh.read(256 << 10).decode("utf-8", "replace")
    except OSError:
        return False
    return _PYTEST_STYLE.search(src) is not None


def _fail(f, text):
    if f is None:
        return
    r = _result(f)
    r["failed"] = True
    if len(r["msg"]) >= _MAX_MSG:
        return
    if len(text) > 1500:
        text = text[:200] + "\n[…]\n" + text[-1300:]  # the assertion is at the end
    r["msg"] = ((r["msg"] + "\n\n") if r["msg"] else "") + text
    r["msg"] = r["msg"][:_MAX_MSG]


class _Result(unittest.TextTestResult):
    def _outcome_file(self, test):
        # Errors in class and module fixtures are reported with an _ErrorHolder: they
        # belong to the module whose context is current.
        return _file_of(test) or _current_file()

    def _note(self, test, err=None, text=None):
        if text is None:
            try:
                text = self._exc_info_to_string(err, test)
            except Exception:
                text = repr(err)
        try:
            name = test.id()
        except Exception:
            name = str(test)
        _fail(self._outcome_file(test), name + ": " + text)

    def startTest(self, test):
        f = _file_of(test)
        _enter(f)
        if f is not None:
            _result(f)["ran"] += 1
        self._smates_start = time.time()
        super().startTest(test)

    def stopTest(self, test):
        super().stopTest(test)
        f = _file_of(test)
        start = getattr(self, "_smates_start", None)
        if f is not None and start is not None:
            _result(f)["ms"] += int((time.time() - start) * 1000)

    def addError(self, test, err):
        super().addError(test, err)
        self._note(test, err)

    def addFailure(self, test, err):
        super().addFailure(test, err)
        self._note(test, err)

    def addSubTest(self, test, subtest, err):
        super().addSubTest(test, subtest, err)
        if err is not None:
            self._note(subtest, err)

    def addUnexpectedSuccess(self, test):
        super().addUnexpectedSuccess(test)
        self._note(test, text="unexpected success")

    def addSuccess(self, test):
        super().addSuccess(test)
        f = _file_of(test)
        if f is not None:
            _result(f)

    def addSkip(self, test, reason):
        super().addSkip(test, reason)
        f = self._outcome_file(test)
        if f is not None:
            # A class skipped in setUpClass reports its skip without starting a test.
            _result(f)["ran"] += 1


class _Runner(unittest.TextTestRunner):
    resultclass = _Result

    def run(self, test):
        try:
            return super().run(test)
        finally:
            _finish()


def _finish():
    if _tracer is None:
        return
    _tracer.switch("")
    for f, r in sorted(_results.items()):
        status = "fail" if r["failed"] else "pass"
        if not r["failed"] and r["ran"] == 0:
            if _pytest_style(f):
                # Tests unittest cannot run: never a pass, so the module runs again.
                status, r["msg"] = "unknown", _NO_TESTS
                sys.stderr.write("%s: %s\n" % (os.path.relpath(f), _NO_TESTS))
            else:
                # A helper module matching the test pattern: nothing to fail (its
                # dependencies are still recorded).
                sys.stderr.write("%s: %s\n" % (os.path.relpath(f), _NO_TESTS_HELPER))
        ev = {"k": "result", "id": f, "status": status, "ms": r["ms"]}
        if r["msg"]:
            ev["msg"] = r["msg"]
        if status == "unknown":
            # Results files (results-*.jsonl) are final: a status in the process trace
            # is combined with the run's exit code, which would make this a pass.
            _write_result_file(ev)
        else:
            _tracer.write(ev)


def _write_result_file(ev):
    out = os.path.join(os.environ.get("SMATES_TRACE_DIR", ""), "results-unittest-%d.jsonl" % os.getpid())
    try:
        fd = os.open(out, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o644)
        try:
            os.write(fd, (json.dumps(ev) + "\n").encode("utf-8", "surrogateescape"))
        finally:
            os.close(fd)
    except OSError:
        pass


def _loaded(loader, make, name, *exc):
    """The suite unittest's private helper builds for a module that failed to load."""
    if make is None:
        return None
    out = make(name, *exc, loader.suiteClass) if exc else make(name, loader.suiteClass)
    if isinstance(out, tuple):  # (suite, message) on Python 3.6+
        out, message = out
        if not exc:
            loader.errors.append(message)
    return out


def _patch():
    loader = unittest.TestLoader
    suite = unittest.suite.TestSuite

    orig_name = loader.loadTestsFromName

    def loadTestsFromName(self, name, module=None):
        f = _name_file(name) if module is None else None
        if f is None:
            return orig_name(self, name, module)
        _result(f)
        _tracer.push("collect:" + f)
        try:
            tests = orig_name(self, name, module)
        except unittest.SkipTest as exc:
            tests = _loaded(self, getattr(unittest.loader, "_make_skipped_test", None), name, exc)
        except Exception:
            # unittest only survives an ImportError here: any other exception raised
            # while importing one module would end the run, leaving every other module
            # without a result. Report it as a failed import, as discovery does.
            tests = _loaded(self, getattr(unittest.loader, "_make_failed_import_test", None), name)
            if tests is None:
                raise
        finally:
            _tracer.pop()
        _tag(tests, f)
        return tests

    loader.loadTestsFromName = loadTestsFromName

    # unittest's own discovery (no module names given).
    orig_find = getattr(loader, "_find_test_path", None)
    if orig_find is not None:

        def _find_test_path(self, full_path, *args, **kwargs):
            if not (full_path.endswith(".py") and os.path.isfile(full_path)):
                return orig_find(self, full_path, *args, **kwargs)
            f = os.path.realpath(full_path)
            _tracer.push("collect:" + f)
            try:
                tests, recurse = orig_find(self, full_path, *args, **kwargs)
            finally:
                _tracer.pop()
            if tests is not None:
                _result(f)
                _tag(tests, f)
            return tests, recurse

        loader._find_test_path = _find_test_path

    # Fixtures: setUpModule/setUpClass of the next test run in its module's context,
    # tearDownClass/tearDownModule of the previous one in that module's context.
    def entering(name):
        orig = getattr(suite, name, None)
        if orig is None:
            return

        def wrapper(self, test, result, *args, **kwargs):
            _enter(_file_of(test))
            return orig(self, test, result, *args, **kwargs)

        setattr(suite, name, wrapper)

    def leaving(name):
        orig = getattr(suite, name, None)
        if orig is None:
            return

        def wrapper(self, *args, **kwargs):
            result = args[-1] if args else kwargs.get("result")
            f = _class_file(getattr(result, "_previousTestClass", None))
            saved = _tracer.ctx
            _enter(f)
            try:
                return orig(self, *args, **kwargs)
            finally:
                if _tracer.ctx != saved:
                    _tracer.switch(saved)

        setattr(suite, name, wrapper)

    entering("_handleModuleFixture")
    entering("_handleClassSetUp")
    leaving("_tearDownPreviousClass")
    leaving("_handleModuleTearDown")


def main():
    if _tracer is not None:
        _patch()
    sys.argv[0] = "python -m unittest"
    unittest.main(module=None, testRunner=_Runner)


if __name__ == "__main__":
    main()

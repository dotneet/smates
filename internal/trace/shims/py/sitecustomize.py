"""smates: start tracing in every Python process of a recorded test run.

This directory is prepended to PYTHONPATH only while smates records, so Python imports
this module at startup. It then hands over to any other sitecustomize on sys.path so
existing project or virtualenv customizations keep working.
"""


def _smates_bootstrap():
    import os
    import sys

    here = os.path.dirname(os.path.abspath(__file__))
    try:
        import smates_trace

        smates_trace.install()
    except Exception as exc:  # never break the user's program
        sys.stderr.write("smates: tracing disabled: %r\n" % (exc,))

    # Chain to the next sitecustomize, if any.
    import importlib.machinery
    import importlib.util

    rest = [p for p in sys.path if os.path.abspath(p or ".") != here]
    spec = importlib.machinery.PathFinder.find_spec("sitecustomize", rest)
    if spec is not None and spec.loader is not None:
        mod = importlib.util.module_from_spec(spec)
        try:
            spec.loader.exec_module(mod)
        except Exception as exc:
            sys.stderr.write("smates: error in chained sitecustomize: %r\n" % (exc,))


_smates_bootstrap()
del _smates_bootstrap

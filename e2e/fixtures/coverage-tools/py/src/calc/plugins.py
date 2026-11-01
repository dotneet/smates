import importlib


def load(name):
    # The module name is computed: only tracing sees which plugin a test loads.
    return importlib.import_module(".".join([__package__, name.replace("-", "")]))

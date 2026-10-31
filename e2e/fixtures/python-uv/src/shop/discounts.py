"""Discount plugins. The active plugin is named in settings.toml and imported by name."""

import functools
import importlib
import tomllib
from pathlib import Path

SETTINGS = Path(__file__).parent / "settings.toml"


def load_plugin(module_name: str):
    module = importlib.import_module(module_name)
    if not hasattr(module, "apply"):
        raise TypeError(f"{module_name} is not a discount plugin")
    return module


@functools.cache
def active_plugin():
    with SETTINGS.open("rb") as f:
        settings = tomllib.load(f)
    return load_plugin(settings["discounts"]["plugin"])


def apply(lines: list[tuple[str, int, int]]) -> int:
    """Discount in cents for cart lines (sku, qty, unit_cents)."""
    return active_plugin().apply(lines)

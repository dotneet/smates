"""Sales reports in pluggable output formats (polytools.formats.<name>_out)."""

import importlib


def render(rows: list[dict], fmt: str) -> str:
    module = importlib.import_module(f"polytools.formats.{fmt}_out")
    return module.render(rows)


def totals(rows: list[dict]) -> dict[str, int]:
    out: dict[str, int] = {}
    for r in rows:
        out[r["sku"]] = out.get(r["sku"], 0) + int(r["quantity"]) * int(r["unit_cents"])
    return out

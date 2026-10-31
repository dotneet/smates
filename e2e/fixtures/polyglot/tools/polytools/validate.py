"""Checks order exports against the contract shared with the backend and the web app."""

import json
import re

from polytools import REPO_ROOT

SCHEMA_PATH = REPO_ROOT / "shared" / "schema.json"


def load_schema() -> dict:
    with open(SCHEMA_PATH, encoding="utf-8") as f:
        return json.load(f)


def validate(schema: dict, order: dict) -> list[str]:
    errors = [f"missing {k}" for k in schema["required"] if k not in order]
    for k, v in order.items():
        prop = schema["properties"].get(k)
        if prop is None:
            errors.append(f"unknown {k}")
            continue
        if prop["type"] == "integer" and not (isinstance(v, int) and not isinstance(v, bool)):
            errors.append(f"{k}: want integer")
        if prop["type"] == "string" and not isinstance(v, str):
            errors.append(f"{k}: want string")
        if "pattern" in prop and isinstance(v, str) and not re.search(prop["pattern"], v):
            errors.append(f"{k}: bad format")
        if "minimum" in prop and isinstance(v, int) and v < prop["minimum"]:
            errors.append(f"{k}: too small")
    return errors

"""Lint rules for product names, one TOML file per rule in tools/rules/."""

import re
import tomllib

from polytools import TOOLS_ROOT

RULES_DIR = TOOLS_ROOT / "rules"


def load_rules() -> list[dict]:
    rules = []
    for path in sorted(RULES_DIR.glob("*.toml")):
        with open(path, "rb") as f:
            rule = tomllib.load(f)
        rule["id"] = path.stem
        rules.append(rule)
    return rules


def check(name: str, rules: list[dict]) -> list[str]:
    problems = []
    for r in rules:
        if "max_len" in r and len(name) > r["max_len"]:
            problems.append(r["id"])
        if "forbid" in r and re.search(r["forbid"], name):
            problems.append(r["id"])
    return problems

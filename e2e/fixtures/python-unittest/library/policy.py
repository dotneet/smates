"""Lending policy, read from policy.json at import."""

import json
import os

with open(os.path.join(os.path.dirname(__file__), "policy.json")) as _f:
    _policy = json.load(_f)

MAX_LOANS = _policy["max_loans"]
LATE_FEE_CENTS = _policy["late_fee_cents"]

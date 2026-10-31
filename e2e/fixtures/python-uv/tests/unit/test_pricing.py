from pathlib import Path

import pytest
import yaml

from shop import pricing

CASES = yaml.safe_load((Path(__file__).parent.parent / "data" / "tax_cases.yaml").read_text(encoding="utf-8"))


@pytest.mark.parametrize("case", CASES["with_tax"], ids=lambda c: c["name"])
def test_with_tax(case):
    assert pricing.with_tax(case["cents"]) == case["expected"]


def test_tax_can_be_overridden(monkeypatch):
    monkeypatch.setattr("shop.pricing.TAX", 0.2)
    assert pricing.with_tax(1000) == 1200

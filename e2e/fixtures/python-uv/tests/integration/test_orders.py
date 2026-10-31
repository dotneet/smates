import json
from pathlib import Path

import pytest

from shop import orders

ORDERS = Path(__file__).parent.parent / "fixtures" / "orders"
FILES = sorted(ORDERS.glob("*.json"))


@pytest.mark.parametrize("path", FILES, ids=lambda p: p.stem)
def test_order_fixture_is_valid(path):
    order = json.loads(path.read_text(encoding="utf-8"))
    assert orders.item_count(order) > 0


def test_there_are_order_fixtures():
    assert len(FILES) >= 3

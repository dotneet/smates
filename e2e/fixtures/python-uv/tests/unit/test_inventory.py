import pytest

from shop.inventory import OutOfStock, Stock


def test_add_and_take():
    s = Stock()
    s.add("APL-001", 5)
    s.take("APL-001", 2)
    assert s.available("APL-001") == 3


def test_out_of_stock():
    s = Stock({"BAN-002": 1})
    with pytest.raises(OutOfStock):
        s.take("BAN-002", 2)


def test_rejects_non_positive():
    with pytest.raises(ValueError):
        Stock().add("X", 0)

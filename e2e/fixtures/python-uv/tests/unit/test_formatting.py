from shop.formatting import format_money


def test_usd():
    assert format_money(123456) == "$1,234.56"


def test_cents_are_padded():
    assert format_money(5) == "$0.05"


def test_jpy_has_no_minor_unit():
    assert format_money(1500, "JPY") == "¥1,500"

import pytest

from shop import catalog


def test_skus_match_reference(catalog_data):
    assert catalog.all_skus() == sorted(catalog_data["prices"])


def test_prices_match_reference(catalog_data):
    for sku, price in catalog_data["prices"].items():
        assert catalog.get(sku).price_cents == price, sku


def test_unknown_sku():
    with pytest.raises(KeyError):
        catalog.get("NOPE-999")

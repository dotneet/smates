from shop import discounts


def test_configured_plugin_is_bulk():
    assert discounts.active_plugin().NAME == "bulk"


def test_bulk_threshold():
    plugin = discounts.active_plugin()
    assert plugin.apply([("A", 2, 1000)]) == 0
    assert plugin.apply([("A", 3, 1000)]) == 300


def test_load_by_name():
    plugin = discounts.load_plugin("shop.plugins.seasonal")
    assert plugin.apply([("A", 1, 1000)]) == 50

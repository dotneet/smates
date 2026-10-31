from shop.cart import Cart


def test_sample_cart_total(catalog_data):
    cart = Cart()
    for sku, qty in catalog_data["sample_cart"].items():
        cart.add(sku, qty)
    assert cart.total() == catalog_data["sample_cart_total"]


def test_no_discount_below_threshold():
    cart = Cart()
    cart.add("TEA-004", 2)
    assert cart.discount() == 0
    assert cart.total() == 1836

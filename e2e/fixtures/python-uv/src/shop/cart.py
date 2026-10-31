"""Shopping cart."""

from shop import catalog, discounts, pricing


class Cart:
    def __init__(self) -> None:
        self.items: dict[str, int] = {}

    def add(self, sku: str, qty: int = 1) -> None:
        catalog.get(sku)  # validates the sku
        self.items[sku] = self.items.get(sku, 0) + qty

    def lines(self) -> list[tuple[str, int, int]]:
        return [(sku, qty, catalog.get(sku).price_cents) for sku, qty in sorted(self.items.items())]

    def subtotal(self) -> int:
        return sum(qty * unit for _, qty, unit in self.lines())

    def discount(self) -> int:
        return discounts.apply(self.lines())

    def total(self) -> int:
        return pricing.with_tax(self.subtotal() - self.discount())

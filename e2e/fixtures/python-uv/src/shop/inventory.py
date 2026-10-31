"""Stock keeping, independent of the catalog."""

from dataclasses import dataclass, field


class OutOfStock(Exception):
    pass


@dataclass
class Stock:
    levels: dict[str, int] = field(default_factory=dict)

    def add(self, sku: str, qty: int) -> None:
        if qty <= 0:
            raise ValueError("qty must be positive")
        self.levels[sku] = self.levels.get(sku, 0) + qty

    def take(self, sku: str, qty: int) -> None:
        have = self.levels.get(sku, 0)
        if qty > have:
            raise OutOfStock(sku)
        self.levels[sku] = have - qty

    def available(self, sku: str) -> int:
        return self.levels.get(sku, 0)

"""Product catalog, loaded once from data/products.csv when the module is imported."""

import csv
from dataclasses import dataclass
from pathlib import Path

DATA = Path(__file__).parent / "data" / "products.csv"


@dataclass(frozen=True)
class Product:
    sku: str
    name: str
    price_cents: int
    category: str


def _load() -> dict[str, Product]:
    with DATA.open(newline="", encoding="utf-8") as f:
        return {
            row["sku"]: Product(row["sku"], row["name"], int(row["price_cents"]), row["category"])
            for row in csv.DictReader(f)
        }


PRODUCTS = _load()


def get(sku: str) -> Product:
    try:
        return PRODUCTS[sku]
    except KeyError:
        raise KeyError(f"unknown sku {sku!r}") from None


def all_skus() -> list[str]:
    return sorted(PRODUCTS)

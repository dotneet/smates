"""Tax computation."""

TAX = 0.08


def tax_for(cents: int) -> int:
    return round(cents * TAX)


def with_tax(cents: int) -> int:
    return cents + tax_for(cents)

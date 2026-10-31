"""Loyalty points."""

from shop.config import RATE


def points_for(amount_cents: int) -> int:
    """Points earned for a purchase (one point per RATE of a currency unit)."""
    return int(amount_cents / 100 * RATE * 10)

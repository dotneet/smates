from shop.config import RATE
from shop.loyalty import points_for


def test_rate_is_five_percent():
    assert RATE == 0.05


def test_points_for_one_hundred_dollars():
    assert points_for(10_000) == 50

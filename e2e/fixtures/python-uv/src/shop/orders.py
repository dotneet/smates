"""Order validation (orders arrive as JSON documents)."""

REQUIRED = ("id", "customer", "lines")


class InvalidOrder(ValueError):
    pass


def validate(order: dict) -> dict:
    for key in REQUIRED:
        if key not in order:
            raise InvalidOrder(f"missing {key}")
    if not order["lines"]:
        raise InvalidOrder("empty order")
    for line in order["lines"]:
        if line.get("qty", 0) <= 0:
            raise InvalidOrder(f"bad qty in {order['id']}")
    return order


def item_count(order: dict) -> int:
    return sum(line["qty"] for line in validate(order)["lines"])

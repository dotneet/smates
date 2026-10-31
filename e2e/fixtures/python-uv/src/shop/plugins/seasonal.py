"""5% off everything."""

NAME = "seasonal"


def apply(lines):
    return sum(qty * unit for _, qty, unit in lines) // 20

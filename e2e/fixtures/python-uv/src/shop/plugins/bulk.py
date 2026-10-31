"""10% off any line with at least THRESHOLD items."""

NAME = "bulk"
THRESHOLD = 3


def apply(lines):
    return sum(qty * unit // 10 for _, qty, unit in lines if qty >= THRESHOLD)

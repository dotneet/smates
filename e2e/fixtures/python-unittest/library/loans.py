"""Members borrowing books."""

from library import policy
from library.books import find


class Member:
    def __init__(self, name):
        self.name = name
        self.loans = []

    def borrow(self, isbn):
        if find(isbn) is None:
            raise KeyError(isbn)
        if len(self.loans) >= policy.MAX_LOANS:
            raise ValueError("loan limit reached")
        self.loans.append(isbn)


def late_fee(days):
    return max(0, days) * policy.LATE_FEE_CENTS

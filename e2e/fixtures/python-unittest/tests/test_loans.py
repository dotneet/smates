import unittest

from library.loans import Member, late_fee


class LoansTest(unittest.TestCase):
    def test_limit(self):
        m = Member("ada")
        for isbn in ("111", "222", "333"):
            m.borrow(isbn)
        with self.assertRaises(ValueError):
            m.borrow("111")

    def test_unknown_book(self):
        with self.assertRaises(KeyError):
            Member("bob").borrow("999")

    def test_late_fee(self):
        self.assertEqual(late_fee(4), 100)
        self.assertEqual(late_fee(-1), 0)

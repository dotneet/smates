import unittest

from library.fmt import money, title


class MoneyTest(unittest.TestCase):
    def test_money(self):
        self.assertEqual(money(1250), "$12.50")
        self.assertEqual(money(5), "$0.05")

    def test_title(self):
        for raw, want in [("the hobbit", "The Hobbit"), ("  dune ", "Dune")]:
            with self.subTest(raw=raw):
                self.assertEqual(title(raw), want)

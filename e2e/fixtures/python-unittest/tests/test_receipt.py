import unittest

from library.receipt import render


class ReceiptTest(unittest.TestCase):
    def test_render(self):
        self.assertEqual(render("Ada", 1250), "Member: Ada\nTotal due: $12.50")

import unittest

from library import books


class BooksTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.catalog = books.catalog()

    def test_titles(self):
        self.assertEqual(sorted(b["title"] for b in self.catalog.values()), ["Dune", "Emma", "The Hobbit"])

    def test_find(self):
        self.assertEqual(books.find("222")["price"], 999)
        self.assertIsNone(books.find("999"))

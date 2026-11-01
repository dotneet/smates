"""The catalog, loaded once from data/books.csv."""

import csv
import functools
import os

from library.fmt import title

DATA = os.path.join(os.path.dirname(__file__), "data", "books.csv")


@functools.lru_cache(maxsize=None)
def catalog():
    with open(DATA, newline="") as f:
        return {r["isbn"]: {"title": title(r["title"]), "price": int(r["price"])} for r in csv.DictReader(f)}


def find(isbn):
    return catalog().get(isbn)

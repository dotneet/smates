"""Receipts rendered from templates/receipt.txt."""

import os

from library.fmt import money

TEMPLATE = os.path.join(os.path.dirname(__file__), "templates", "receipt.txt")


def render(name, cents):
    with open(TEMPLATE) as f:
        return f.read().format(name=name, total=money(cents)).strip()

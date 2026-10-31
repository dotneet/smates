"""Command line: `python -m shop.cli receipt SKU QTY` or `python -m shop.cli summary`."""

import argparse
import sys
from pathlib import Path
from string import Template

from shop import catalog
from shop.formatting import format_money

TEMPLATES = Path(__file__).parent / "templates"


def receipt(sku: str, qty: int) -> str:
    product = catalog.get(sku)
    template = Template((TEMPLATES / "receipt.txt").read_text(encoding="utf-8"))
    return template.substitute(
        name=product.name, qty=qty, unit=format_money(product.price_cents), total=format_money(product.price_cents * qty)
    )


def summary() -> str:
    rows = [f"{p.sku}\t{p.category}\t{format_money(p.price_cents)}" for p in map(catalog.get, catalog.all_skus())]
    return "\n".join(rows)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="shop")
    sub = parser.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("receipt")
    r.add_argument("sku")
    r.add_argument("qty", type=int)
    sub.add_parser("summary")
    args = parser.parse_args(argv)
    if args.cmd == "receipt":
        print(receipt(args.sku, args.qty))
    else:
        print(summary())
    return 0


if __name__ == "__main__":
    sys.exit(main())

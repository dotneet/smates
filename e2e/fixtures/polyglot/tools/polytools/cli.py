"""polytools command line: `python -m polytools.cli slug <name>...`."""

import sys

from polytools import TOOLS_ROOT
from polytools.slug import slugify


def stopwords() -> frozenset[str]:
    text = (TOOLS_ROOT / "data" / "stopwords.txt").read_text(encoding="utf-8")
    return frozenset(w.strip() for w in text.splitlines() if w.strip() and not w.startswith("#"))


def main(argv: list[str]) -> int:
    if len(argv) < 2 or argv[0] != "slug":
        print("usage: polytools.cli slug <name>...", file=sys.stderr)
        return 2
    sw = stopwords()
    for name in argv[1:]:
        print(slugify(name, sw))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

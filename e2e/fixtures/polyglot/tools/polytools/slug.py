"""URL slugs for product names."""

import re
import unicodedata


def slugify(text: str, stopwords: frozenset[str] = frozenset()) -> str:
    text = unicodedata.normalize("NFKD", text).encode("ascii", "ignore").decode()
    words = [w for w in re.split(r"[^a-z0-9]+", text.lower()) if w and w not in stopwords]
    return "-".join(words)

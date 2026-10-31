from polytools.slug import slugify


def test_slugify_basic():
    assert slugify("Crème Brûlée Deluxe") == "creme-brulee-deluxe"


def test_slugify_stopwords():
    assert slugify("The Art of Tea", frozenset({"the", "of"})) == "art-tea"

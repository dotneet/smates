from polytools.rules import check, load_rules


def test_rule_set():
    assert [r["id"] for r in load_rules()] == ["max_len", "no_shouting"]


def test_check():
    rules = load_rules()
    assert check("Green Apple", rules) == []
    assert check("HUGE Watermelon Family Pack", rules) == ["max_len", "no_shouting"]

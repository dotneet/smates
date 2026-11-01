from calc.arith import add, sign


def test_add():
    assert add(1, 2) == 3


def test_sign():
    assert sign(-3) == -1

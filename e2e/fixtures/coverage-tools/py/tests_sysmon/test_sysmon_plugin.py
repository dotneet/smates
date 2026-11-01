from calc.plugins import load


def test_plugin():
    assert load("ans-wer").answer() == 42

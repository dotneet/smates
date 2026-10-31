import pytest


@pytest.mark.parametrize("kind", ["sales"])
def test_report(run_script, kind):
    out = run_script(f"{kind}_report").stdout.splitlines()
    assert out[0] == f"# {kind} report"
    assert "TEA-004\tdrinks\t$8.50" in out

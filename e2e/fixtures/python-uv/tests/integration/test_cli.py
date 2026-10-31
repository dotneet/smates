import subprocess
import sys


def shop(*args: str) -> str:
    return subprocess.run(
        [sys.executable, "-m", "shop.cli", *args], capture_output=True, text=True, check=True
    ).stdout


def test_receipt():
    out = shop("receipt", "TEA-004", "2")
    assert out.splitlines()[0] == "RECEIPT"
    assert "Green Tea x 2 @ $8.50" in out
    assert "TOTAL $17.00" in out


def test_summary_lists_every_product():
    lines = shop("summary").splitlines()
    assert len(lines) == 4
    assert lines[0] == "APL-001\tfruit\t$1.20"

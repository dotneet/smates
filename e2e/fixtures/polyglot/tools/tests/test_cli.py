import subprocess
import sys

from polytools import TOOLS_ROOT


def test_cli_slug():
    r = subprocess.run(
        [sys.executable, "-m", "polytools.cli", "slug", "The Joy of Pears", "Apples and Oranges"],
        cwd=TOOLS_ROOT, capture_output=True, text=True, check=False,
    )
    assert r.returncode == 0, r.stderr
    assert r.stdout.splitlines() == ["joy-pears", "apples-oranges"]

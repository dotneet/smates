import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent
DATA = Path(__file__).parent / "data"


@pytest.fixture(scope="session")
def catalog_data():
    """Reference catalog the domain must agree with."""
    with (DATA / "catalog.json").open(encoding="utf-8") as f:
        return json.load(f)


@pytest.fixture
def run_script():
    """Run one of the operational shell scripts in scripts/ by name."""

    def run(name: str, *args: str) -> subprocess.CompletedProcess:
        script = ROOT / "scripts" / f"{name}.sh"
        env = {**os.environ, "PYTHON": sys.executable}
        return subprocess.run(
            ["sh", str(script), *args], cwd=ROOT, env=env, capture_output=True, text=True, check=True
        )

    return run

import csv
from pathlib import Path

import pytest

DATA = Path(__file__).parent / "data"


@pytest.fixture
def sales_rows() -> list[dict]:
    with open(DATA / "sales.csv", newline="", encoding="utf-8") as f:
        return list(csv.DictReader(f))

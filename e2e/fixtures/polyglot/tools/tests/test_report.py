from polytools.report import render, totals


def test_totals(sales_rows):
    assert totals(sales_rows) == {"FRU-001": 480, "FRU-002": 160}


def test_markdown(sales_rows):
    out = render(sales_rows, "markdown")
    assert out.splitlines()[2] == "| FRU-001 | 3 | 1.20 |"


def test_csv_roundtrip(sales_rows):
    assert render(sales_rows, "csv").splitlines()[1] == "FRU-001,3,120"

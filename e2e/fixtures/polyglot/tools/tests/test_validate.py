from polytools.validate import load_schema, validate


def test_contract_version():
    s = load_schema()
    assert (s["title"], s["version"]) == ("Order", 3)


def test_validate_export_rows():
    s = load_schema()
    assert validate(s, {"id": "o1", "sku": "ABC-123", "quantity": 2, "unit_cents": 499}) == []
    assert len(validate(s, {"id": "o1", "sku": "abc", "quantity": 0, "unit_cents": 499})) == 2

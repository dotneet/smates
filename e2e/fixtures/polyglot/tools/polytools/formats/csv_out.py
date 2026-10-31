def render(rows: list[dict]) -> str:
    lines = ["sku,quantity,unit_cents"]
    lines += [f"{r['sku']},{r['quantity']},{r['unit_cents']}" for r in rows]
    return "\n".join(lines) + "\n"

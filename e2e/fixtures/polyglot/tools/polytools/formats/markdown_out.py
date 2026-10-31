def render(rows: list[dict]) -> str:
    lines = ["| SKU | Qty | Unit |", "|---|---:|---:|"]
    lines += [f"| {r['sku']} | {r['quantity']} | {int(r['unit_cents']) / 100:.2f} |" for r in rows]
    return "\n".join(lines) + "\n"

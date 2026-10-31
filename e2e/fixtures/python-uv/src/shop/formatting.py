"""Money formatting."""

SYMBOLS = {"USD": "$", "EUR": "€", "JPY": "¥"}


def format_money(cents: int, currency: str = "USD") -> str:
    symbol = SYMBOLS.get(currency, currency + " ")
    if currency == "JPY":
        return f"{symbol}{cents:,}"
    return f"{symbol}{cents // 100:,}.{cents % 100:02d}"

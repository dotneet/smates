"""Formatting helpers."""


def money(cents):
    return "$%d.%02d" % (cents // 100, cents % 100)


def title(s):
    return " ".join(w.capitalize() for w in s.split())

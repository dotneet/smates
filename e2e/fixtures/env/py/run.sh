#!/bin/sh
# Runs each test file given as an argument (all of them when none is given).
set -e
[ $# -gt 0 ] || set -- py/test_*.py
for f in "$@"; do python3 "$f"; done

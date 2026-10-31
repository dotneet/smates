#!/bin/sh
# Normalise a SKU read on stdin: upper case, separators collapsed to "-".
tr '[:lower:]' '[:upper:]' | tr -s ' _.' '---' | sed -e 's/^-//' -e 's/-$//'

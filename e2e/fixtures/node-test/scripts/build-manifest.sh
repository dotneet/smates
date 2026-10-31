#!/bin/sh
# Builds a release manifest from the header template and the package name.
set -e
cd "$(dirname "$0")/.."
cat templates/header.txt
grep '"name"' package.json | sed 's/.*: "\(.*\)".*/name=\1/'

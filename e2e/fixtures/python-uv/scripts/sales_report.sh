#!/bin/sh
# Nightly sales report: a header followed by the catalog summary.
set -eu
echo "# sales report"
"${PYTHON:-python}" -m shop.cli summary

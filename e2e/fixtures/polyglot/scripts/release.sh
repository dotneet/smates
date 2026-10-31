#!/bin/sh
# Tags a release. Not used by any test.
set -eu
version=$(sh backend/scripts/stamp.sh)
git tag "$version"
echo "tagged $version"

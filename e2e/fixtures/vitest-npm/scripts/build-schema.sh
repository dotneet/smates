#!/bin/sh
# Concatenates the migrations into one schema file: sh scripts/build-schema.sh > schema.sql
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
. "$here/lib/schema-header.sh"
print_header
for f in "$here/../db/migrations/"*.sql; do
  echo "-- $(basename "$f")"
  cat "$f"
done

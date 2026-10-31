#!/bin/sh
# Fails when a content page uses a banned word (case-insensitive).
cd "$(dirname "$0")/.."
if grep -i -n -w -f content/banned-words.txt content/*.md; then
  echo "banned words found" >&2
  exit 1
fi
echo "copy ok"

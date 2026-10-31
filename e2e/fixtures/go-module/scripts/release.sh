#!/bin/sh
# Builds release binaries. Not used by tests.
set -eu
for os in linux darwin; do
  GOOS=$os GOARCH=amd64 go build -o "bin/tool-$os-amd64" ./cmd/tool
done

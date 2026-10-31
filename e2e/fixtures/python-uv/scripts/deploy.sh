#!/bin/sh
# Builds and pushes the container image. Not used by tests.
set -eu
docker build -t shop:latest .
docker push shop:latest

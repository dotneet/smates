#!/bin/sh
# Bumps versions and publishes the packages. Not used by tests.
set -e
pnpm -r exec npm version patch --no-git-tag-version

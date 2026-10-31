#!/bin/sh
# Seeds the staging database (run by the deploy pipeline, not by tests).
set -eu
psql "$DATABASE_URL" -c "insert into skus(code) values ('ABC-1') on conflict do nothing"

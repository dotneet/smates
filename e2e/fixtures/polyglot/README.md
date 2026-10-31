# polyglot

Order tooling in three languages that share one contract, `shared/schema.json`:

- `backend/` — Go service code (pricing, catalog, receipts).
- `web/` — TypeScript storefront helpers, tested with vitest.
- `tools/` — Python data tools (uv + pytest).

Run `make -C backend test`, `npm test --prefix web`, `uv run --project tools pytest`.

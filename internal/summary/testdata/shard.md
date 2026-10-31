### smates (shard 2/2): 4 of 7 test units selected

This change affects **4 test units in 2 of 3 suites**; 3 skipped, saving about **2m 4s** of recorded test time.

_Sharded 2 ways: this summary covers the 7 test units shard 2 owns._

| suite | runner | selected | skipped | time saved |
|---|---|---:|---:|---:|
| web | vitest | 3 / 4 | 1 | 4s |
| api | go | 1 / 2 | 1 | 2m 0s |
| py | pytest | 0 / 1 | 1 | 0.5s |

<details open><summary>Why these 4 test units (4 changed files, 1 other reason)</summary>

**`db/schema.sql`** → 1 test unit
- `web/src/db.test.ts` (web) — names it

**`scripts/gen/a.go`** → 1 test unit
- `api/server` (api) — an untraced command ran under `scripts/gen/`

**`scripts/gen/b.go`** → 1 test unit
- `api/server` (api) — an untraced command ran under `scripts/gen/`

**`web/src/math.ts`** → 1 test unit
- `web/src/stats.test.ts` (web) — static dependency

**Never recorded** → 1 test unit
- `web/src/new.test.ts` (web) — no recorded run

</details>

**Changes no test depends on (3):**
- `Makefile` — left to the agent (below)
- `db/schema.sql` — named by tests: selected 1 test unit
- `docs/notes.md` — the agent found no affected test

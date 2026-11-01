### smates: 8 of 14 test units selected

This change affects **8 test units in 2 of 3 suites**; 6 skipped, saving about **3m 10s** of recorded test time (1 skipped without a recorded duration).

| suite | runner | selected | skipped | time saved |
|---|---|---:|---:|---:|
| web | vitest | 6 / 9 | 3 | 1m 9s |
| api | go | 2 / 3 | 1 | 2m 0s |
| py | pytest | 0 / 2 | 2 | 1s |

**Result:** ❌ exit code 1 after 1m 23s; 1 test unit failed:
- `web: web/src/math.test.ts`

<details open><summary>Why these 8 test units (7 changed files, 3 other reasons)</summary>

**`db/schema.sql`** → 1 test unit
- `web/src/db.test.ts` (web) — names it

**`scripts/gen/a.go`** → 1 test unit
- `api/server` (api) — an untraced command ran under `scripts/gen/`

**`scripts/gen/b.go`** → 1 test unit
- `api/server` (api) — an untraced command ran under `scripts/gen/`

**`web/fixtures/` (files added or removed)** → 1 test unit
- `web/src/report.test.ts` (web) — lists the directory

**`web/fixtures/report.json`** → 1 test unit
- `web/src/report.test.ts` (web) — read it in its last run (traced)

**`web/src/math.test.ts`** → 1 test unit
- `web/src/math.test.ts` (web) — the test itself

**`web/src/math.ts`** → 2 test units
- `web/src/math.test.ts` (web) — static dependency
- `web/src/stats.test.ts` (web) — static dependency

**Failed or unfinished last time** → 1 test unit
- `web/src/flaky.test.ts` (web) — last run: fail

**Never recorded** → 1 test unit
- `web/src/new.test.ts` (web) — no recorded run

**Selected by the agent** → 1 test unit
- `api/client` (api) — agent: @​octocat says &lt;b&gt;client&lt;/b&gt; reads the generated \| table

</details>

**Changes no test depends on (3):**
- `Makefile` — left to the agent (below)
- `db/schema.sql` — named by tests: selected 1 test unit
- `docs/notes.md` — the agent found no affected test

**Flaky tests**

- `web/src/flaky.test.ts` (2 of the last 5 runs)

<details><summary>Failure hints</summary>

`web/src/math.ts` changed in this PR.

</details>

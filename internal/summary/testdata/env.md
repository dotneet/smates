### smates: 2 of 3 test units selected

This change affects **2 test units in 1 suite**; 1 skipped, saving about **0.1s** of recorded test time.

| suite | runner | selected | skipped | time saved |
|---|---|---:|---:|---:|
| web | vitest | 2 / 3 | 1 | 0.1s |

<details open><summary>Why these 2 test units (1 changed file, 1 changed environment variable)</summary>

**`web/src/a.ts`** → 1 test unit
- `web/both.test.ts` — static dependency

**environment variable `TZ`** → 2 test units
- `web/both.test.ts` — read it in its last run (traced)
- `web/tz.test.ts` — read it in its last run (traced)

</details>

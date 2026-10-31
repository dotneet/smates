### smates: 4 of 5 test units selected

This change affects **4 test units in 1 of 2 suites**; 1 skipped, saving about **2s** of recorded test time.

| suite | runner | selected | skipped | time saved |
|---|---|---:|---:|---:|
| web | vitest | 4 / 4 | 0 | – |
| api | go | 0 / 1 | 1 | 2s |

<details open><summary>Why these 4 test units (1 changed file)</summary>

**`web/vite.config.ts`** → all of `web` (orphan policy; 4 test units not selected for other reasons)

</details>

**Changes no test depends on (2):**
- `README.md` — ignored (`orphans: ignore`)
- `web/vite.config.ts` — orphan policy: runs `web` in full

_agent unavailable (no API key); running affected suites in full_

> ignored 1 unattributed change(s) (orphans: ignore)

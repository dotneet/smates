# Parallel worktrees

Coding agents often work in several `git worktree`s at once. This page explains how each
worktree gets a useful state without a full run, and how worktrees can share what they
learn.

## A new worktree starts warm

```text
$ git worktree add ../shop-fix-cart -b fix-cart
$ cd ../shop-fix-cart
$ smates run
smates: seeded this worktree from the main worktree (/home/me/src/shop/.smates/state.json.gz)
smates: selected 3 of 412 test unit(s) across 3 suite(s) (99% skipped)
...
```

Each worktree has its own state, `.smates/state.json.gz`, so a fresh worktree would
start with no recording, and select every test. Instead, the first command that needs the
state in a linked worktree that has none *seeds* it, and prints one line saying where the
seed came from.

### Where the seed comes from

1. The main worktree's state, if it has one.
2. Otherwise, the remote state (pushed with `smates state push`). smates fetches it at
   most once an hour, as the [git hook](git-hooks.md) does. A state ref that another
   worktree already fetched is used without fetching.

### When the seed is kept

The seed brings two things: per-test entries (what each test depends on, and its last
result) and a full-tree snapshot (the file hashes that changes are compared against).

The snapshot is kept only when it was recorded on a commit in this worktree's history.
Otherwise, it would make the other branch's changes look like this one's. The worktree
then keeps the per-test entries and judges changed files against the default branch, as
it would without a snapshot.

### Turn seeding off

- A worktree is seeded once. `smates state clear` keeps it from being seeded again.
- `state_seed: false` in `smates.yml` turns seeding off for everyone.
- `SMATES_NO_STATE_SEED=1` turns it off in one environment.
- An explicit `--state FILE` never seeds and never shares.

## Share one state between worktrees

Seeding copies a state once. To share what *any* worktree learns with all of them, set:

```yaml
state_shared: true
```

| what | where it lives |
|---|---|
| per-test entries, agent answers, run records | one file under the git common directory: `.git/smates/state.json.gz` |
| the full-tree snapshot | per worktree: `.smates/state.snapshot.json.gz` |

Each worktree keeps its own snapshot, so a run on one branch never makes another
branch's changes look like this one's.

### Concurrent runs

smates processes in several worktrees can save at the same time without losing each
other's results:

- Saving takes a lock file next to the shared state, and merges this run's changes into
  what is on disk.
- When two runs re-ran the same test, the newer result wins, and the outcome histories
  are combined.
- `smates flaky --clear` and `smates state clear` are not undone by a run that started
  earlier.
- A lock left behind by a crashed process is taken over after two minutes.

If another run holds the lock, saving waits up to a minute. If it is still held then,
the run exits 4 after its summary (or 1 when tests failed); run it again.

!!! note
    `smates state clear` in shared mode clears the shared state for **every** worktree.

## Inspect the state

```bash
smates state show                 # where the state lives, whether it is shared, where it was seeded from
smates state show --format json
smates state path                 # the path of the state file (the shared file, in shared mode)
```

## See also

- [Git hooks and Claude Code](git-hooks.md): the hook pulls the remote state too
- [Other CI systems](../ci/other-ci.md): `smates state push` and `pull`
- [Commands](../reference/cli.md#smates-state): every `smates state` subcommand
- [smates.yml reference](../reference/configuration.md): `state`, `state_seed`, `state_shared`

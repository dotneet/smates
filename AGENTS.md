# AGENTS.md — working on smates

Rules for coding agents that change smates itself. (docs/AGENTS.md is different: it is
the guide for agents that *use* smates, embedded in the binary as `smates guide`.)

- Never put details of external projects you evaluate smates on (names, paths,
  identifiers, code) into code, tests, fixtures, docs, commits or PRs. Rebuild repro
  cases with generic names.
- Fail toward running more. A change that narrows selection must keep a dependency
  whenever it cannot tell, and add a test row for that "must keep" case.
- Fix a review finding for the whole class of inputs, not the quoted example: list the
  variants and cover each with a table test.
- Follow the exit-code table in internal/cli/cli.go. A non-test error must not exit 1
  ("tests failed"): wrap it in usageError, configError or envError, because Main sends
  an unwrapped error to ExitFailed.

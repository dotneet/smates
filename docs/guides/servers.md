# Tests that talk to a server

End-to-end tests often reach a server in another process or container: Playwright
against a backend started by docker compose, pytest against an HTTP API. Tracing cannot
follow a request into that server, so tell smates which code the tests exercise with
`depends_on` or `serves`.

```yaml
suites:
  - name: api
    runner: go
    root: services/api
  - name: e2e
    runner: custom
    root: e2e
    tests: ["tests/**/*.spec.ts"]
    command: "npx playwright test"
    depends_on: [api]          # the code of suite api
```

Now a change to the server selects the e2e tests:

```text
+ e2e/tests/login.spec.ts  [changed: services/api/user.go (depends_on suite api)]
```

## The problem

smates traces the test process and its children: the files they execute and read.
A server started separately (by docker compose, a CI service, or a script before the
tests) is not a child of the test. Without a declaration:

- a change to the server's code selects none of the e2e tests;
- the changed file is an *orphan*, a change no test is known to depend on (see
  [Changes no test explains](orphans.md)), unless another suite's tests cover it.

## Declare the dependency: `depends_on`

`depends_on` is a list. Each entry is either another suite's code or a set of paths.

```yaml
    depends_on:
      - api                              # same as {suite: api}
      - suite: worker
      - paths: ["docker-compose.yml", "services/api/migrations/**"]
        tests: ["admin/**"]              # optional: only these tests get this entry
```

| entry | means |
|---|---|
| `api` or `{suite: api}` | the files under suite `api`'s root, minus test files (of any suite), ignored and vendored files, and files of suites rooted below it |
| `{paths: [globs]}` | repository-relative globs (a leading `./` is stripped) |
| `tests: [globs]` | narrows the entry to some of the suite's tests; globs relative to the suite root (default: all tests) |

An entry needs either `suite` or `paths`, not both, and a suite cannot depend on itself.
The depending suite's own test files are never added.

### `serves`: the short form

`serves` is shorthand for one `paths` entry. Use it when the server is not a suite of its
own:

```yaml
    serves: ["backend/**", "docker-compose.yml"]
    # same as: depends_on: [{paths: ["backend/**", "docker-compose.yml"]}]
```

## How it is tracked

Each declaration is one dependency of each test it applies to. Its hash covers all of
its files at the current commit, so the recorded state does not grow with the size of
the server. A change to any of those files:

- selects the tests with that declaration;
- is no longer an orphan, also when you run one suite with `--suite e2e`.

## Where it shows

`smates run --explain` and `smates select --explain` name the changed file and the
declaration:

```text
  e2e (custom): 1/2
    + e2e/admin/users.spec.ts  [changed: docker-compose.yml (depends_on docker-compose.yml)]
```

`smates why` lists the tests that depend on a file through a declaration:

```text
$ smates why services/api/user.go
e2e	e2e/tests/login.spec.ts	(depends_on suite api)
e2e	e2e/admin/users.spec.ts	(depends_on suite api)
```

The pull request summary (`--markdown`, `--format markdown`) groups such tests under the
changed file as "server code its tests exercise". Failure hints for a failing e2e test
expand the declaration into the files that changed.

## Example: docker compose and Playwright

A Go API and a worker run in containers; Playwright tests in `e2e/` call the API on
`http://localhost:8080`.

```text
.
├── docker-compose.yml
├── services/
│   ├── api/        # Go, has its own unit tests
│   └── worker/     # Python, no tests of its own
└── e2e/
    ├── playwright.config.ts
    └── tests/
        ├── login.spec.ts
        └── admin/users.spec.ts
```

```yaml
version: 1
suites:
  - name: api
    runner: go
    root: services/api
  - name: e2e
    runner: custom
    root: e2e
    tests: ["tests/**/*.spec.ts"]
    command: "npx playwright test"
    depends_on:
      - api                                   # API code, without its tests
      - paths: ["docker-compose.yml"]         # how the stack is started
    serves: ["services/worker/**"]            # worker code (not a suite)
```

Start the stack before the tests, then run smates as usual:

```bash
docker compose up -d --wait
smates run
```

With this config:

| change | runs |
|---|---|
| `services/api/handlers/user.go` | the `api` package tests that reach it, and every e2e test |
| `services/api/handlers/user_test.go` | that package's tests only (test files are left out of `{suite: api}`) |
| `services/worker/jobs.py` | every e2e test |
| `e2e/tests/login.spec.ts` | `login.spec.ts` |
| `README.md` | nothing |

If only some e2e tests touch a part of the server, narrow the entry with `tests:`.
For example, `{paths: ["services/api/admin/**"], tests: ["tests/admin/**"]}` gives the
admin API only to the admin tests.

## smates doctor points at missing declarations

`smates doctor` warns about suites that look like end-to-end tests and declare none of
`depends_on`, `serves` or `rules`. A suite looks like one when its name or root mentions
e2e, its command runs Playwright or Cypress, or a test file calls `http://localhost` (one
that starts the server it calls itself, an `httptest` server or `http.server` on port 0,
or intercepts the requests with nock, msw or `responses`, does not count):

```text
suite e2e looks like end-to-end tests (runs playwright) but declares no depends_on, serves or rules: tracing cannot follow requests into a server process, so changes to the server's code select none of its tests. Declare it, e.g. depends_on: [<server suite>] or serves: ["backend/**"]
```

`smates init --format json` reports the same finding as a `needs_review` item.

## See also

- [Configure suites](configuration.md): `inputs` and `rules` for other dependencies tracing cannot see
- [smates.yml reference](../reference/configuration.md#depends_on): every `depends_on` key
- [Changes no test explains](orphans.md)
- [Limitations](../reference/limitations.md)

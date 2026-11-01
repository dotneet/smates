# env fixture

Tests in three runners read the environment variable `APP_REGION` (`node/region.test.js`
through `node/lib/region.js`, the Go package `go/region`, `py/test_region.py`); the
`plain` tests read nothing. `node/labels.test.js` reads `BUILD_LABEL` (listed in
`env_ignore`) and `GITHUB_RUN_ID` (ignored by default). `e2e/env_test.go` changes the
variables between runs and checks that only the tests reading a tracked variable are
selected. `APP_REGION=invalid` makes `node/region.test.js` fail.

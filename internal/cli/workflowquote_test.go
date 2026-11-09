package cli

import "testing"

// A test command counts where the shell runs it, not where a line only names it in a
// string.
func TestRunTestCmd(t *testing.T) {
	for line, want := range map[string]string{
		"go test ./...":                                              "go test",
		"cd web && pnpm run test --shard=1/2":                        "pnpm run test",
		"pnpm test || exit 1":                                        "pnpm test",
		`expected='pnpm run lint && pnpm run test'`:                  "",
		`expected="pnpm run lint && pnpm run test"`:                  "",
		`echo "run pnpm run test to check" && go vet ./...`:          "",
		`if [ "$cmd" != "go test ./..." ]; then exit 1; fi`:          "",
		`grep -q 'pytest ' Makefile`:                                 "",
		`echo 'see "go test"' && go test ./...`:                      "go test",
		`sh -c "cd api && go test ./..."`:                            "go test",
		`bash -euc 'cd api && pytest -q'`:                            "pytest",
		`docker compose run app sh -c 'cd /app && npm test -- --ci'`: "npm test",
		`eval "cd web && pnpm run test --silent"`:                    "pnpm run test",
		`echo \"x && go test ./...`:                                  "go test",
		`msg='it'\''s fine' && pytest`:                               "pytest",
		`expected='pnpm run test &&`:                                 "",
	} {
		if got := runTestCmd(line); got != want {
			t.Errorf("runTestCmd(%q) = %q, want %q", line, got, want)
		}
	}
}

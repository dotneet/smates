package cli

import (
	"testing"

	"github.com/dotneet/smates/internal/config"
)

func TestCIOnlyValue(t *testing.T) {
	for v, want := range map[string]bool{
		"${{ matrix.shard }}": true,
		"${{ github.event_name == 'pull_request' && needs.plan.outputs.base || '' }}": true,
		"${{ steps.select.outputs.base }}":                                            true,
		"${{ strategy.job-index }}/${{ strategy.job-total }}":                         true,
		"${{ runner.temp }}/reports":                                                  true,
		"${{ matrix['node'] }}":                                                       true,
		"${{ secrets.DATABASE_URL }}":                                                 false,
		"${{ vars.API_ORIGIN }}":                                                      false,
		"${{ inputs.level }}":                                                         false,
		"${{ env.BASE }}/x":                                                           false,
		"${{ matrix.db == 'pg' && secrets.PG_URL || secrets.MY_URL }}":                false,
		"${{ true }}":                                                                 false,
		"postgres://localhost/x":                                                      false,
		"1":                                                                           false,
	} {
		if got := ciOnlyValue(v); got != want {
			t.Errorf("ciOnlyValue(%q) = %v, want %v", v, got, want)
		}
	}
}

// The env: hints keep what the tests may need: constants, secrets, variables, and
// orchestration values the repository's code reads.
func TestWorkflowEnvHintsOrchestration(t *testing.T) {
	suites := []config.Suite{{Name: "web", Runner: "vitest", Root: "."}}
	steps := []testStep{{File: ".github/workflows/ci.yml", Job: "test", Command: "npx vitest run", Runners: []string{"vitest"}, Env: [][2]string{
		{"SHARD", "${{ matrix.shard }}"},
		{"CHANGED_BASE", "${{ needs.plan.outputs.base }}"},
		{"TEST_PARTITION", "${{ matrix.part }}"},
		{"TZ", "UTC"},
		{"API_KEY", "${{ secrets.API_KEY }}"},
	}}}
	read := func(name string) bool { return name == "TEST_PARTITION" }
	got := map[string]bool{}
	for _, kv := range workflowEnvHints(steps, suites, read)["web"].Env {
		got[kv[0]] = true
	}
	for name, want := range map[string]bool{"SHARD": false, "CHANGED_BASE": false, "TEST_PARTITION": true, "TZ": true, "API_KEY": true} {
		if got[name] != want {
			t.Errorf("%s kept %v, want %v (hints %v)", name, got[name], want, got)
		}
	}
}

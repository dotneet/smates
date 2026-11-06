package selector

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dotneet/smates/internal/config"
)

// Suite keys added after the settings hash was first recorded: unset, none of them is
// hashed (an upgrade does not rerun every test). Set, those that change how the tests
// are selected or run change the hash, like any edit of the suite's entry; volatile,
// max_age and requires only decide when a test runs, so editing them reruns nothing.
func TestConfigHashKeysAddedLater(t *testing.T) {
	base := func() *config.Suite {
		return &config.Suite{Name: "go", Runner: "go", Rules: []config.Rule{{Tests: []string{"**"}, Deps: []string{"m/**"}}}}
	}
	cases := []struct {
		name   string
		field  string // the name in the hashed JSON
		set    func(*config.Suite)
		hashed bool
	}{
		{"reaches", "Reaches", func(s *config.Suite) { s.Rules[0].Reaches = []string{"pkg/shared/testkit/**"} }, true},
		{"requires", "Requires", func(s *config.Suite) { s.Requires = []config.Requirement{{TCP: "127.0.0.1:5432"}} }, false},
		{"requires timeout", "Requires", func(s *config.Suite) {
			s.Requires = []config.Requirement{{TCP: "127.0.0.1:5432", Timeout: 5 * time.Second}}
		}, false},
		{"trust_browser_passes", "TrustBrowserPasses", func(s *config.Suite) { s.TrustBrowserPasses = new(bool) }, true},
		{"volatile", "Volatile", func(s *config.Suite) { s.Volatile = []string{"pkg/clock"} }, false},
		{"max_age", "MaxAge", func(s *config.Suite) { s.MaxAge = time.Hour }, false},
	}
	c := &config.Config{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			data, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), `"`+tc.field+`"`) {
				t.Errorf("an unset %s is hashed: %s", tc.name, data)
			}
			before := ConfigHash(c, s)
			tc.set(s)
			if changed := ConfigHash(c, s) != before; changed != tc.hashed {
				t.Errorf("setting %s changes the hash: %v, want %v", tc.name, changed, tc.hashed)
			}
		})
	}
}

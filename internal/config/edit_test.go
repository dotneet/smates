package config

import (
	"reflect"
	"strings"
	"testing"
)

const commented = `# smates configuration (top comment)
version: 1

suites:
  - name: go   # the unit tests
    runner: go
    root: .

# Changed files no test depends on.
orphans: agent # policy

ignore:
  # docs
  - "docs/**"
  - design/**   # images

inputs: [".nvmrc"]   # pins

agent:
  # provider: auto
  model: claude-haiku   # cheap
`

func TestAddToListBlockKeepsComments(t *testing.T) {
	out, added, present, err := AddToList([]byte(commented), "ignore", []string{"design/**", "tools/**", "tools/**", "app/\\[id\\]/**"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, []string{"tools/**", "app/\\[id\\]/**"}) || !reflect.DeepEqual(present, []string{"design/**"}) {
		t.Errorf("added %v present %v", added, present)
	}
	want := strings.Replace(commented, "  - design/**   # images\n", "  - design/**   # images\n  - \"tools/**\"\n  - \"app/\\\\[id\\\\]/**\"\n", 1)
	if string(out) != want {
		t.Errorf("got:\n%s\nwant:\n%s", out, want)
	}
	c, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Ignore, []string{"docs/**", "design/**", "tools/**", "app/\\[id\\]/**"}) {
		t.Errorf("ignore = %v", c.Ignore)
	}
}

func TestAddToListFlowAndMissing(t *testing.T) {
	out, added, _, err := AddToList([]byte(commented), "inputs", []string{".nvmrc", "go.sum"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(added, []string{"go.sum"}) || !strings.Contains(string(out), `inputs: [".nvmrc", "go.sum"]   # pins`) {
		t.Errorf("added %v:\n%s", added, out)
	}
	cases := map[string]string{
		"version: 1\n":                     "version: 1\nignore:\n  - \"a/**\"\n",
		"version: 1":                       "version: 1\nignore:\n  - \"a/**\"\n",
		"":                                 "ignore:\n  - \"a/**\"\n",
		"ignore: []\n":                     "ignore: [\"a/**\"]\n",
		"ignore: [\n  \"x\",\n]\n":         "ignore: [\n  \"x\", \"a/**\"\n]\n",
		"ignore:\n# end\n":                 "ignore:\n  - \"a/**\"\n# end\n",
		"ignore: # none yet\nversion: 1\n": "ignore: # none yet\n  - \"a/**\"\nversion: 1\n",
		"ignore: ~\n":                      "ignore:\n  - \"a/**\"\n",
		"ignore:\n- x\n- y\n# tail\norphans: all\n": "ignore:\n- x\n- y\n- \"a/**\"\n# tail\norphans: all\n",
		"ignore:\n  - x\n\n  - y\nstate: s\n":       "ignore:\n  - x\n\n  - y\n  - \"a/**\"\nstate: s\n",
	}
	for in, want := range cases {
		out, _, _, err := AddToList([]byte(in), "ignore", []string{"a/**"})
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if string(out) != want {
			t.Errorf("%q:\ngot  %q\nwant %q", in, out, want)
		}
	}
}

func TestAddToListRejects(t *testing.T) {
	for _, c := range []struct {
		doc, key string
		vals     []string
	}{
		{"version: 1\n", "ignore", []string{"/abs/**"}},
		{"version: 1\n", "ignore", []string{"../up"}},
		{"version: 1\n", "ignore", []string{"a/[b"}},
		{"version: 1\n", "ignore", []string{""}},
		{"version: 1\n", "suites", []string{"x"}},
		{"ignore: docs\n", "ignore", []string{"x"}},
		{"orphans: bogus\n", "ignore", []string{"x"}}, // the current config is invalid
		{"{ignore: []}\n", "ignore", []string{"x"}},
	} {
		if out, _, _, err := AddToList([]byte(c.doc), c.key, c.vals); err == nil {
			t.Errorf("%q %s %v accepted:\n%s", c.doc, c.key, c.vals, out)
		}
	}
}

func TestSetScalar(t *testing.T) {
	out, prev, err := SetScalar([]byte(commented), "orphans", "all")
	if err != nil || prev != "agent" {
		t.Fatal(prev, err)
	}
	if want := strings.Replace(commented, "orphans: agent # policy", "orphans: all # policy", 1); string(out) != want {
		t.Errorf("got:\n%s", out)
	}
	out, prev, err = SetScalar([]byte(commented), "agent.model", "gpt-x")
	if err != nil || prev != "claude-haiku" || !strings.Contains(string(out), "  model: gpt-x   # cheap\n") {
		t.Errorf("%q %v:\n%s", prev, err, out)
	}
	out, _, err = SetScalar([]byte(commented), "agent.provider", "openai")
	if err != nil || !strings.HasSuffix(string(out), "  model: claude-haiku   # cheap\n  provider: openai\n") {
		t.Errorf("%v:\n%s", err, out)
	}
	out, _, err = SetScalar([]byte(commented), "state", "1")
	if err != nil || !strings.HasSuffix(string(out), "\nstate: \"1\"\n") {
		t.Errorf("%v:\n%s", err, out)
	}
	if c, err := Parse(out); err != nil || c.State != "1" {
		t.Errorf("%v %+v", err, c)
	}
	cases := map[string][3]string{ // doc -> key, value, want
		"flaky:\n  retry: 1\n":       {"flaky.retry", "2", "flaky:\n  retry: 2\n"},
		"version: 1\n":               {"flaky.retry", "2", "version: 1\nflaky:\n  retry: 2\n"},
		"flaky:\nstate: x\n":         {"flaky.retry", "3", "flaky:\n  retry: 3\nstate: x\n"},
		"state: 'a''b' # c\n":        {"state", "s", "state: s # c\n"},
		"state: \"a\\\"b\"\n":        {"state", "s", "state: s\n"},
		"state:\n":                   {"state", "s", "state: s\n"},
		"agent:\n  timeout: 2m\n":    {"agent.timeout", "90s", "agent:\n  timeout: 90s\n"},
		"agent:\n  model: x\n":       {"agent.explain_failures", "false", "agent:\n  model: x\n  explain_failures: false\n"},
		"orphans: all\n":             {"orphans", "ignore", "orphans: ignore\n"},
		"state: .smates/s.json.gz\n": {"state", "a: b", "state: 'a: b'\n"},
	}
	for doc, c := range cases {
		out, _, err := SetScalar([]byte(doc), c[0], c[1])
		if err != nil {
			t.Errorf("%q %s=%s: %v", doc, c[0], c[1], err)
		} else if string(out) != c[2] {
			t.Errorf("%q %s=%s:\ngot  %q\nwant %q", doc, c[0], c[1], out, c[2])
		}
	}
}

func TestSetScalarRejects(t *testing.T) {
	for _, c := range [][3]string{
		{commented, "orphans", "sometimes"},  // Normalize rejects it
		{commented, "flaky.retry", "x"},      // not an integer
		{commented, "flaky.retry", "9"},      // out of range
		{commented, "ignore", "x"},           // a list
		{commented, "suites", "x"},           // a list
		{commented, "agent", "x"},            // a mapping
		{commented, "nope", "x"},             // unknown
		{commented, "agent.nope", "x"},       // unknown
		{commented, "agent.timeout", "soon"}, // not a duration
		{commented, "agent.explain_failures", "yes"},
		{"agent: {model: x}\n", "agent.effort", "high"}, // flow mapping
		{"state: |\n  x\n", "state", "y"},               // block scalar
		{"version: 1\n", "version", "2"},
	} {
		if out, _, err := SetScalar([]byte(c[0]), c[1], c[2]); err == nil {
			t.Errorf("%s=%s accepted:\n%s", c[1], c[2], out)
		}
	}
	if keys := ScalarKeys(); !contains(keys, "orphans") || !contains(keys, "agent.model") || contains(keys, "ignore") {
		t.Errorf("ScalarKeys = %v", keys)
	}
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

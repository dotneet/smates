package config

import (
	"strings"
	"testing"
)

func TestEditCRLF(t *testing.T) {
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	lists := map[string]string{
		"version: 1\nignore:\norphans: all\n":            "version: 1\nignore:\n  - \"a/**\"\norphans: all\n",
		"ignore: # none\norphans: all\n":                 "ignore: # none\n  - \"a/**\"\norphans: all\n",
		"ignore:\n  - x   # c\norphans: all\n":           "ignore:\n  - x   # c\n  - \"a/**\"\norphans: all\n",
		"ignore:\n  - x\n":                               "ignore:\n  - x\n  - \"a/**\"\n",
		"version: 1\n":                                   "version: 1\nignore:\n  - \"a/**\"\n",
		"ignore: [x]   # c\norphans: all\n":              "ignore: [x, \"a/**\"]   # c\norphans: all\n",
		"# top\nversion: 1\n\nignore:\n  - x\n\n# end\n": "# top\nversion: 1\n\nignore:\n  - x\n  - \"a/**\"\n\n# end\n",
	}
	for in, want := range lists {
		out, _, _, err := AddToList([]byte(crlf(in)), "ignore", []string{"a/**"})
		if err != nil {
			t.Errorf("%q: %v", in, err)
		} else if string(out) != crlf(want) {
			t.Errorf("%q:\ngot  %q\nwant %q", in, out, crlf(want))
		}
	}
	scalars := map[string][3]string{
		"orphans: agent   # c\nstate: s\n": {"orphans", "all", "orphans: all   # c\nstate: s\n"},
		"state: x\nversion: 1\n":           {"state", "z", "state: z\nversion: 1\n"},
		"state:\nversion: 1\n":             {"state", "z", "state: z\nversion: 1\n"},
		"agent:\n  model: x\n":             {"agent.effort", "high", "agent:\n  model: x\n  effort: high\n"},
		"agent:\nversion: 1\n":             {"agent.effort", "high", "agent:\n  effort: high\nversion: 1\n"},
	}
	for in, c := range scalars {
		out, _, err := SetScalar([]byte(crlf(in)), c[0], c[1])
		if err != nil {
			t.Errorf("%q: %v", in, err)
		} else if string(out) != crlf(c[2]) {
			t.Errorf("%q:\ngot  %q\nwant %q", in, out, crlf(c[2]))
		}
	}
}

// A comment after a tab is a comment too.
func TestSetScalarTabComment(t *testing.T) {
	for in, want := range map[string]string{
		"state: x\t# keep\n":    "state: z\t# keep\n",
		"state: x \t # keep\n":  "state: z \t # keep\n",
		"state: a#b\t# keep\n":  "state: z\t# keep\n",
		"orphans: all\t#k\n":    "orphans: all\t#k\n",
		"state: x\t#keep\t#2\n": "state: z\t#keep\t#2\n",
	} {
		key, val := "state", "z"
		if strings.HasPrefix(in, "orphans") {
			key, val = "orphans", "all"
		}
		out, _, err := SetScalar([]byte(in), key, val)
		if err != nil {
			t.Errorf("%q: %v", in, err)
		} else if string(out) != want {
			t.Errorf("%q: got %q, want %q", in, out, want)
		}
	}
}

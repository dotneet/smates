// Package configlint validates the flat "key: value" files under config/.
package configlint

import (
	"fmt"
	"strings"
)

// Lint returns the problems found in one config file.
func Lint(name string, data []byte) []string {
	var probs []string
	seen := map[string]bool{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case !ok || k == "" || v == "":
			probs = append(probs, fmt.Sprintf("%s:%d: expected \"key: value\"", name, i+1))
		case seen[k]:
			probs = append(probs, fmt.Sprintf("%s:%d: duplicate key %q", name, i+1, k))
		}
		seen[k] = true
	}
	return probs
}

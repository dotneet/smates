// Package schema loads the order contract shared with the web and Python tools
// (shared/schema.json at the repository root) and checks orders against it.
package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Schema is the subset of JSON Schema the backend enforces.
type Schema struct {
	Title      string                    `json:"title"`
	Version    int                       `json:"version"`
	Required   []string                  `json:"required"`
	Properties map[string]map[string]any `json:"properties"`
}

// Locate finds shared/schema.json by walking up from dir.
func Locate(dir string) (string, error) {
	for d := dir; ; d = filepath.Dir(d) {
		p := filepath.Join(d, "shared", "schema.json")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		if filepath.Dir(d) == d {
			return "", errors.New("shared/schema.json not found")
		}
	}
}

// Load reads and decodes a schema file.
func Load(path string) (*Schema, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// Validate checks an order (decoded JSON) against the schema.
func (s *Schema) Validate(order map[string]any) error {
	for _, k := range s.Required {
		if _, ok := order[k]; !ok {
			return fmt.Errorf("missing required field %q", k)
		}
	}
	for k, v := range order {
		p, ok := s.Properties[k]
		if !ok {
			return fmt.Errorf("unknown field %q", k)
		}
		switch p["type"] {
		case "string":
			str, ok := v.(string)
			if !ok {
				return fmt.Errorf("%s: want string", k)
			}
			if pat, ok := p["pattern"].(string); ok && !regexp.MustCompile(pat).MatchString(str) {
				return fmt.Errorf("%s: %q does not match %s", k, str, pat)
			}
		case "integer":
			n, ok := v.(float64)
			if !ok || n != float64(int64(n)) {
				return fmt.Errorf("%s: want integer", k)
			}
			if m, ok := p["minimum"].(float64); ok && n < m {
				return fmt.Errorf("%s: %v below minimum %v", k, n, m)
			}
		}
	}
	return nil
}

// Package settings loads service settings: defaults.json, overlaid by an optional
// per-environment file defaults.<env>.json next to it.
package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Settings are the service settings.
type Settings struct {
	Port        int    `json:"port"`
	Currency    string `json:"currency"`
	MaxQuantity int    `json:"max_quantity"`
}

// Load reads dir/defaults.json and overlays dir/defaults.<env>.json when it exists.
func Load(dir, env string) (Settings, error) {
	var s Settings
	if err := readInto(filepath.Join(dir, "defaults.json"), &s); err != nil {
		return s, err
	}
	overlay := filepath.Join(dir, fmt.Sprintf("defaults.%s.json", env))
	if _, err := os.Stat(overlay); err == nil {
		if err := readInto(overlay, &s); err != nil {
			return s, err
		}
	}
	return s, nil
}

func readInto(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

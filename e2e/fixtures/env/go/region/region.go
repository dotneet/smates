// Package region reads the deployment region from the environment.
package region

import "os"

// Name is the configured region.
func Name() string {
	if v, ok := os.LookupEnv("APP_REGION"); ok {
		return v
	}
	return "default"
}

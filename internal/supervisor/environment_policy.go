package supervisor

import (
	"strings"
)

func mergeProviderEnvironment(merged, providerEnvironment []string) []string {
	positions := make(map[string]int, len(merged)+len(providerEnvironment))
	for index, entry := range merged {
		key, _, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			positions[key] = index
		}
	}
	for _, entry := range providerEnvironment {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		// Provider profiles may isolate their own configuration with
		// XDG_CONFIG_HOME. Supervisor commands must retain the caller's
		// control configuration so their saved endpoint and identity remain
		// resolvable.
		if key == "XDG_CONFIG_HOME" {
			continue
		}
		if index, exists := positions[key]; exists {
			merged[index] = entry
			continue
		}
		positions[key] = len(merged)
		merged = append(merged, entry)
	}
	return merged
}

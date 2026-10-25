package verifiedexec

import "strings"

// EnvironmentValue keeps the first matching entry, preserving interpreter
// lookup's recorded-snapshot convention. This differs from exec.Cmd.Env's
// last-value-wins convention; persisted task environments reject duplicates.
// Entries without '=' are ignored. Missing values never fall back to the
// process environment.
func EnvironmentValue(environment []string, key string) string {
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if found && name == key {
			return value
		}
	}
	return ""
}

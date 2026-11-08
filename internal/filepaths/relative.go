// Package filepaths owns the pure wire contract for declared relative files.
package filepaths

import (
	"fmt"
	"path"
	"strings"
)

const MaxEntries = 10000

func ValidatePath(name string) error {
	if len(name) > 4096 || name == "" || name == "." || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00") {
		return fmt.Errorf("invalid relative path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || strings.EqualFold(part, ".git") || strings.HasPrefix(strings.ToLower(part), ".delegate-") {
			return fmt.Errorf("reserved path %q", name)
		}
	}
	return nil
}

func ValidateSelectors(selectors []string) error {
	if len(selectors) > MaxEntries {
		return fmt.Errorf("selectors exceed %d entries", MaxEntries)
	}
	seen := make(map[string]bool, len(selectors))
	spelling := make(map[string]string)
	for _, name := range selectors {
		if err := ValidatePath(name); err != nil {
			return err
		}
		if err := validatePathSpelling(name, spelling); err != nil {
			return err
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("duplicate selector %q", name)
		}
		seen[key] = true
	}
	for name := range seen {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return fmt.Errorf("file/directory collision %q", parent)
			}
		}
	}
	return nil
}

func validatePathSpelling(name string, spellings map[string]string) error {
	for current := name; current != "."; current = path.Dir(current) {
		key := strings.ToLower(current)
		if old, ok := spellings[key]; ok && old != current {
			return fmt.Errorf("case-colliding directory or file %q", current)
		}
		spellings[key] = current
	}
	return nil
}

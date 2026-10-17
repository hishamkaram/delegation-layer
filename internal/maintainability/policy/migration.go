package policy

import "fmt"

// ApplyMigration rejects changed, stale, duplicate, or unowned baseline entries.
func ApplyMigration(findings []Finding, sources []Source, entries []Migration, strict bool) ([]Finding, int) {
	if strict {
		if len(entries) == 0 {
			return findings, 0
		}
		unresolved := append([]Finding(nil), findings...)
		return append(unresolved, Finding{Path: "agents/_data/subsystems.json", Kind: "migration", Detail: "strict audit requires an empty migration inventory"}), 0
	}
	digests := make(map[string]string, len(sources))
	for _, source := range sources {
		digests[source.Path] = source.SHA256
	}
	permitted := make(map[string]Migration, len(entries))
	var unresolved []Finding
	for _, entry := range entries {
		key := findingKey(Finding{Path: entry.Path, Kind: entry.Kind, Symbol: entry.Symbol, Detail: entry.Detail})
		if _, exists := permitted[key]; exists || entry.Owner == "" || entry.Stage == "" || len(entry.SHA256) != 64 {
			unresolved = append(unresolved, Finding{Path: entry.Path, Kind: "migration", Detail: "duplicate or invalid migration entry"})
			continue
		}
		permitted[key] = entry
	}
	used := make(map[string]bool)
	deferred := 0
	for _, finding := range findings {
		key := findingKey(finding)
		entry, exists := permitted[key]
		if exists && digests[finding.Path] == entry.SHA256 {
			used[key] = true
			deferred++
			continue
		}
		unresolved = append(unresolved, finding)
	}
	for key, entry := range permitted {
		if !used[key] {
			unresolved = append(unresolved, Finding{Path: entry.Path, Kind: "migration", Detail: fmt.Sprintf("stale or changed baseline entry for %s", entry.Kind)})
		}
	}
	return unresolved, deferred
}

package policy

import (
	"sort"
	"strings"
)

func checkCycles(sources []Source, modules map[string]Module) []Finding {
	edges := make(map[string][]string)
	for _, source := range sources {
		for _, dependency := range source.Imports {
			if _, exists := modules[dependency.Target]; exists {
				edges[source.Module] = append(edges[source.Module], dependency.Target)
			}
		}
	}
	state := make(map[string]uint8)
	var findings []Finding
	var visit func(string, []string)
	visit = func(name string, path []string) {
		if state[name] == 2 {
			return
		}
		if state[name] == 1 {
			findings = append(findings, Finding{Path: "agents/_data/subsystems.json", Kind: "cycle", Detail: strings.Join(append(path, name), " -> ")})
			return
		}
		state[name] = 1
		for _, next := range edges[name] {
			visit(next, append(append([]string(nil), path...), name))
		}
		state[name] = 2
	}
	names := make([]string, 0, len(modules))
	for name := range modules {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		visit(name, nil)
	}
	return findings
}

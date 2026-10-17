package policy

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Check validates facts against registered boundaries. It never learns new edges.
func Check(manifest Manifest, sources []Source) []Finding {
	modules := make(map[string]Module, len(manifest.Modules))
	for _, module := range manifest.Modules {
		modules[module.Name] = module
	}
	used := make(map[string]map[string]bool)
	var findings []Finding
	for _, source := range sources {
		findings = append(findings, checkSize(manifest.Limits, source)...)
		if source.Module == "" {
			continue
		}
		module, exists := modules[source.Module]
		if !exists {
			findings = append(findings, Finding{Path: source.Path, Kind: "module", Detail: "unclassified module " + source.Module})
			continue
		}
		if used[source.Module] == nil {
			used[source.Module] = make(map[string]bool)
		}
		for _, dependency := range source.Imports {
			used[source.Module][dependency.Target] = true
		}
		findings = append(findings, checkImports(module, source)...)
		findings = append(findings, checkImportLayers(module, source, modules)...)
		if module.Layer == "pure" {
			for _, effect := range source.Effects {
				findings = append(findings, Finding{Path: source.Path, Line: effect.Line, Kind: "purity", Detail: effect.Name})
			}
		}
	}
	findings = append(findings, checkRegistryUse(modules, used)...)
	findings = append(findings, checkCycles(sources, modules)...)
	sort.Slice(findings, func(i, j int) bool { return findingKey(findings[i]) < findingKey(findings[j]) })
	return findings
}

func checkRegistryUse(modules map[string]Module, used map[string]map[string]bool) []Finding {
	var findings []Finding
	for name, module := range modules {
		if used[name] == nil {
			findings = append(findings, Finding{Path: "agents/_data/subsystems.json", Kind: "registry", Detail: "unused module " + name})
			continue
		}
		for _, dependency := range module.Imports {
			if !used[name][dependency] {
				findings = append(findings, Finding{Path: "agents/_data/subsystems.json", Kind: "registry", Detail: "unused allowed dependency " + name + " -> " + dependency})
			}
		}
	}
	return findings
}

func checkImportLayers(module Module, source Source, modules map[string]Module) []Finding {
	if module.Layer != "pure" {
		return nil
	}
	var findings []Finding
	for _, dependency := range source.Imports {
		target, registered := modules[dependency.Target]
		if registered && target.Layer != "pure" {
			findings = append(findings, Finding{
				Path: source.Path, Line: dependency.Line, Kind: "dependency",
				Detail: "pure module imports non-pure boundary " + dependency.Target,
			})
		}
	}
	return findings
}

func checkSize(limits Limits, source Source) []Finding {
	if source.Kind == "asset" || source.Kind == "generated" {
		return nil
	}
	limit := limits.Code
	switch source.Kind {
	case "test":
		limit = limits.Tests
	case "documentation":
		limit = limits.Documentation
	}
	var findings []Finding
	if source.Lines > limit {
		findings = append(findings, Finding{Path: source.Path, Kind: "size", Detail: "file exceeds physical line budget", Actual: source.Lines, Limit: limit})
	}
	for _, function := range source.Functions {
		if function.Length > limits.Function {
			findings = append(findings, Finding{Path: source.Path, Line: function.Line, Kind: "function", Symbol: function.Name, Detail: "function exceeds AST span budget", Actual: function.Length, Limit: limits.Function})
		}
	}
	return findings
}

func checkImports(module Module, source Source) []Finding {
	var findings []Finding
	for _, dependency := range source.Imports {
		if !slices.Contains(module.Imports, dependency.Target) {
			findings = append(findings, Finding{Path: source.Path, Line: dependency.Line, Kind: "dependency", Detail: module.Name + " -> " + dependency.Target})
		}
		if module.Layer == "pure" && forbiddenPureImport(dependency.Target) {
			findings = append(findings, Finding{Path: source.Path, Line: dependency.Line, Kind: "purity", Detail: "effectful import " + dependency.Target})
		}
	}
	return findings
}

func forbiddenPureImport(target string) bool {
	for _, name := range []string{"go:os", "go:os/exec", "go:syscall", "go:unsafe", "go:net", "go:net/http", "go:crypto/rand", "go:math/rand", "go:C", "py:os", "py:subprocess", "py:socket", "py:random", "py:time", "py:importlib", "py:ctypes", "js:node:fs", "js:node:child_process", "js:node:http", "js:node:https", "js:node:net", "js:node:vm"} {
		if target == name || strings.HasPrefix(target, name+"/") {
			return true
		}
	}
	return strings.HasPrefix(target, "go:golang.org/x/sys/")
}

func findingKey(finding Finding) string {
	return fmt.Sprintf("%s:%s:%s:%s", finding.Path, finding.Kind, finding.Symbol, finding.Detail)
}

package maintainability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

func collectOther(root string, sources []policy.Source) error {
	for _, language := range []string{"python", "javascript"} {
		paths := pathsFor(language, sources)
		facts, err := languageFacts(root, language, paths)
		if err != nil {
			return err
		}
		if err = mergeFacts(language, paths, facts, sources); err != nil {
			return err
		}
	}
	return nil
}

func mergeFacts(language string, paths []string, facts, sources []policy.Source) error {
	prefix := "py:"
	if language == "javascript" {
		prefix = "js:"
	}
	byPath := make(map[string]policy.Source, len(facts))
	for _, fact := range facts {
		if !strings.HasPrefix(fact.Module, prefix) || len(fact.Module) <= len(prefix) {
			return fmt.Errorf("missing %s module identity for %s", language, fact.Path)
		}
		if _, exists := byPath[fact.Path]; exists {
			return fmt.Errorf("duplicate %s source fact %s", language, fact.Path)
		}
		byPath[fact.Path] = fact
	}
	for index := range sources {
		if fact, exists := byPath[sources[index].Path]; exists {
			sources[index].Module = fact.Module
			sources[index].Imports = fact.Imports
			sources[index].Functions = fact.Functions
			sources[index].Effects = fact.Effects
			delete(byPath, fact.Path)
		}
	}
	if len(facts) != len(paths) || len(byPath) != 0 {
		return fmt.Errorf("incomplete %s source observations", language)
	}
	return nil
}

func pathsFor(language string, sources []policy.Source) []string {
	extension := ".py"
	if language == "javascript" {
		extension = ".mjs"
	}
	var paths []string
	for _, source := range sources {
		if strings.HasSuffix(source.Path, extension) && source.Kind != "generated" {
			paths = append(paths, source.Path)
		}
	}
	return paths
}

func languageFacts(root, language string, paths []string) ([]policy.Source, error) {
	input, err := json.Marshal(paths)
	if err != nil {
		return nil, fmt.Errorf("encode source paths: %w", err)
	}
	command := exec.Command("python3", "-B", "scripts/maintainability/python_facts.py")
	if language == "javascript" {
		command = exec.Command("node", "--experimental-vm-modules", "scripts/maintainability/javascript-facts.mjs")
	}
	command.Dir = root
	command.Stdin = bytes.NewReader(input)
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("collect %s facts: %w: %s", language, err, strings.TrimSpace(diagnostic.String()))
	}
	var facts []policy.Source
	if err = json.Unmarshal(output, &facts); err != nil {
		return nil, fmt.Errorf("decode %s facts from %s: %w", language, filepath.Base(command.Path), err)
	}
	return facts, nil
}

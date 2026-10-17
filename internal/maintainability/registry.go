package maintainability

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

// Load reads one manifest and its owned fragments; no collector edits these rules.
func Load(root string) (policy.Manifest, []policy.Migration, error) {
	var manifest policy.Manifest
	if err := decodeFile(filepath.Join(root, "agents/_data/subsystems.json"), &manifest); err != nil {
		return manifest, nil, err
	}
	if manifest.Version != 1 || manifest.Module == "" || manifest.Limits.Code <= 0 || manifest.Limits.Tests <= 0 || manifest.Limits.Documentation <= 0 || manifest.Limits.Function <= 0 {
		return manifest, nil, errors.New("invalid maintainability manifest")
	}
	for _, name := range manifest.ModuleFiles {
		if !validRelativePath(name) {
			return manifest, nil, errors.New("invalid module manifest path")
		}
		var modules []policy.Module
		if err := decodeFile(filepath.Join(root, name), &modules); err != nil {
			return manifest, nil, err
		}
		manifest.Modules = append(manifest.Modules, modules...)
	}
	if err := validateModules(manifest.Modules); err != nil {
		return manifest, nil, err
	}
	if err := validateGenerated(root, manifest.Generated); err != nil {
		return manifest, nil, err
	}
	entries, err := readMigration(root, manifest.Migration)
	return manifest, entries, err
}

func validateGenerated(root string, artifacts []policy.Generated) error {
	seen := make(map[string]bool)
	for _, artifact := range artifacts {
		if artifact.Generator == "" || !validRelativePath(artifact.Path) || !validRelativePath(artifact.Source) || seen[artifact.Path] {
			return errors.New("invalid generated-artifact provenance")
		}
		for _, path := range []string{artifact.Path, artifact.Source} {
			info, err := os.Lstat(filepath.Join(root, path))
			if err != nil {
				return fmt.Errorf("read generated-artifact provenance: %w", err)
			}
			if !info.Mode().IsRegular() {
				return errors.New("generated-artifact provenance must name regular files")
			}
		}
		seen[artifact.Path] = true
	}
	return nil
}

// decodeFile accepts only the finite schemas used by this contributor tool.
func decodeFile[T policy.Manifest | []policy.Module](path string, target *T) (resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open maintainability policy: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(target); err != nil {
		return fmt.Errorf("decode maintainability policy %s: %w", path, err)
	}
	var trailing json.RawMessage
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("maintainability policy contains trailing data")
	}
	return nil
}

func validateModules(modules []policy.Module) error {
	seen := make(map[string]bool)
	for _, module := range modules {
		if module.Name == "" || module.Owner == "" || seen[module.Name] {
			return fmt.Errorf("missing or duplicate subsystem identity: %s", module.Name)
		}
		if !policy.KnownLayer(module.Layer) {
			return fmt.Errorf("unknown layer for %s", module.Name)
		}
		seen[module.Name] = true
	}
	return nil
}

func readMigration(root, name string) (entries []policy.Migration, resultErr error) {
	if !validRelativePath(name) {
		return nil, errors.New("invalid migration inventory path")
	}
	file, err := os.Open(filepath.Join(root, name))
	if err != nil {
		return nil, fmt.Errorf("open migration inventory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry policy.Migration
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&entry); err != nil {
			return nil, fmt.Errorf("decode migration inventory: %w", err)
		}
		entries = append(entries, entry)
	}
	return entries, scanner.Err()
}

func validRelativePath(name string) bool {
	return name != "" && !filepath.IsAbs(name) && filepath.Clean(name) == name && name != ".." && !strings.HasPrefix(name, ".."+string(filepath.Separator))
}

// Package maintainability collects source observations for the pure policy checker.
package maintainability

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/maintainability/policy"
)

// Inventory includes maintained additions before staging, but never ignored scratch files.
func Inventory(root string, generated []policy.Generated) ([]policy.Source, error) {
	command := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("inventory maintained paths: %w", err)
	}
	names := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	slices.Sort(names)
	names = slices.Compact(names)
	var sources []policy.Source
	for _, name := range names {
		source, exists, readErr := readSource(root, name, generated)
		if readErr != nil {
			return nil, readErr
		}
		if exists {
			sources = append(sources, source)
		}
	}
	return sources, nil
}

// GoSourcePaths gives formatting the same maintained-file boundary as the audit.
// Generated classification does not change inclusion in this inventory.
func GoSourcePaths(root string) ([]string, error) {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	sources, err := Inventory(canonical, nil)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, source := range sources {
		if filepath.Ext(source.Path) == ".go" {
			paths = append(paths, filepath.Join(canonical, source.Path))
		}
	}
	return paths, nil
}

func readSource(root, name string, generated []policy.Generated) (policy.Source, bool, error) {
	data, err := os.ReadFile(filepath.Join(root, name))
	if os.IsNotExist(err) {
		return policy.Source{}, false, nil // Tracked removals belong to the candidate snapshot.
	}
	if err != nil {
		return policy.Source{}, false, fmt.Errorf("read maintained path %s: %w", name, err)
	}
	digest := sha256.Sum256(data)
	source := policy.Source{Path: name, Kind: sourceKind(name), SHA256: hex.EncodeToString(digest[:]), Lines: physicalLines(data)}
	for _, artifact := range generated {
		if artifact.Path == name {
			source.Kind = "generated"
		}
	}
	return source, true, nil
}

func physicalLines(data []byte) int {
	count := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		count++
	}
	return count
}

func sourceKind(name string) string {
	extension := filepath.Ext(name)
	if slices.Contains([]string{".png", ".jpg", ".jpeg", ".webp", ".ico", ".woff", ".woff2"}, extension) {
		return "asset"
	}
	if extension == ".md" || isDiagramSource(name) {
		return "documentation"
	}
	if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(filepath.Base(name), "test_") || strings.HasPrefix(filepath.Base(name), "test-") || strings.HasPrefix(name, "scripts/acceptance_tests/") {
		return "test"
	}
	return "code"
}

func isDiagramSource(name string) bool {
	return filepath.Dir(name) == "diagrams" && (strings.HasSuffix(name, ".architecture.json") || strings.HasSuffix(name, ".sequence.json"))
}

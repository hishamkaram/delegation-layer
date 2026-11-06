package provider

import (
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

// CLIInfo describes the installed executable selected for one task. Current
// discovery records only the resolved path. SHA256 is an optional historical
// observation: definitions reconstructed from records admitted before direct
// launch may carry it, and current discovery always leaves it empty.
type CLIInfo struct {
	Path   string
	SHA256 string
}

// LocateCLI resolves a provider executable in the selected lookup environment
// without starting or reading it. Help output is observed later by the
// supervised inspection worker immediately before the provider launch.
func LocateCLI(name string, environment []string) (CLIInfo, error) {
	path, err := verifiedexec.LookPath(name, environment)
	if err != nil {
		return CLIInfo{}, fmt.Errorf("provider executable %q is unavailable: %w", name, err)
	}
	return LocateCLIPath(path)
}

// LocateCLIPath applies the static availability checks used before a
// supervised capability probe: it resolves the canonical path and requires a
// regular file with an execute bit. It never reads or fingerprints the
// provider's bytes, because the user owns provider installations and updates.
func LocateCLIPath(path string) (CLIInfo, error) {
	canonical, err := verifiedexec.LocatePath(path)
	if err != nil {
		return CLIInfo{}, fmt.Errorf("resolve provider executable: %w", err)
	}
	return CLIInfo{Path: canonical}, nil
}

// ValidateOptionalExecutableSHA256 applies the provider executable digest rule
// shared by observations, inspection definitions, and policy details: an
// empty digest means the bytes were not observed; a present digest must be
// valid.
func ValidateOptionalExecutableSHA256(digest string) error {
	if digest == "" {
		return nil
	}
	return task.ValidateSHA256(digest)
}

func containsCLIFlag(output []byte, flag string) bool {
	if flag == "" {
		return false
	}
	text := string(output)
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], flag)
		if index < 0 {
			return false
		}
		index += offset
		end := index + len(flag)
		if (index == 0 || !isCLIFlagRune(text[index-1])) && (end == len(text) || !isCLIFlagRune(text[end])) {
			return true
		}
		offset = end
	}
	return false
}

// ContainsCLIFlag reports whether a help stream advertises one complete flag
// token. It deliberately accepts output from either stdout or stderr because
// CLIs differ in where they print help diagnostics.
func ContainsCLIFlag(output []byte, flag string) bool { return containsCLIFlag(output, flag) }

func isCLIFlagRune(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == '-' || value == '_'
}

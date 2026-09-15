package claude

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

type profileEnvironment struct {
	Home          string
	ClaudeHome    string
	RuntimeSHA256 string
	Values        []string
	WritableRoots []string
}

const storageBackendPin = "CLAUDE_CODE_HOVER_REST=0"

// prepareEnvironment preserves the default native login context. Provider,
// credential, session, and config selectors are rejected instead of silently
// switching an account; unrelated caller environment is not inherited.
func prepareEnvironment(values []string) (profileEnvironment, error) {
	entries, err := parseEnvironment(values)
	if err != nil {
		return profileEnvironment{}, err
	}
	home, err := config.CanonicalizePath(entries["HOME"])
	if err != nil {
		return profileEnvironment{}, fmt.Errorf("%w: invalid HOME", ErrUnsupportedProfile)
	}
	claudeHome, err := config.CanonicalizePath(filepath.Join(home, ".claude"))
	if err != nil {
		return profileEnvironment{}, fmt.Errorf("%w: invalid Claude home", ErrUnsupportedProfile)
	}
	result := profileEnvironment{Home: home, ClaudeHome: claudeHome}
	// The restricted runtime processes this explicit false value before its
	// first-pin-wins storage latch. Remote feature evaluation cannot repin it.
	// OAuth retains the same native Keychain service/account.
	result.Values = append(result.Values, storageBackendPin)
	for _, key := range []string{"HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING"} {
		if value, exists := entries[key]; exists {
			result.Values = append(result.Values, key+"="+value)
		}
	}
	result.WritableRoots = []string{claudeHome, filepath.Join(home, ".claude.json"), filepath.Join(home, ".cache"), filepath.Join(home, "Library/Application Support/Claude"), filepath.Join(home, "Library/Caches"), filepath.Join(home, "Library/Logs"), filepath.Join(home, "Library/Keychains"), "/tmp", "/var/tmp", "/var/folders", "/dev"}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if entries[key] != "" {
			result.WritableRoots = append(result.WritableRoots, entries[key])
		}
	}
	for index, path := range result.WritableRoots {
		canonical, pathErr := config.CanonicalizePath(path)
		if pathErr != nil {
			return profileEnvironment{}, fmt.Errorf("%w: invalid runtime writable root", ErrUnsupportedProfile)
		}
		result.WritableRoots[index] = canonical
	}
	slices.Sort(result.Values)
	slices.Sort(result.WritableRoots)
	result.WritableRoots = slices.Compact(result.WritableRoots)
	return result, nil
}

func parseEnvironment(values []string) (map[string]string, error) {
	entries := make(map[string]string, len(values))
	for _, entry := range values {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("%w: malformed environment entry", ErrUnsupportedProfile)
		}
		if _, exists := entries[key]; exists {
			return nil, fmt.Errorf("%w: duplicate environment key", ErrUnsupportedProfile)
		}
		if strings.HasPrefix(key, "CLAUDE") || strings.HasPrefix(key, "ANTHROPIC") {
			if key+"="+value != storageBackendPin {
				return nil, fmt.Errorf("%w: alternate Claude environment selectors are unsupported", ErrUnsupportedProfile)
			}
		}
		entries[key] = value
	}
	return entries, nil
}

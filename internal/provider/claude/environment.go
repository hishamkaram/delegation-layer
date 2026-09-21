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

// prepareEnvironment preserves the historical isolated environment used by
// old Claude profiles. New admission uses prepareNativeEnvironment so the
// Claude CLI can resolve its own configuration and login context.
func prepareEnvironment(values []string) (profileEnvironment, error) {
	return prepareProfileEnvironment(values, false)
}

func prepareNativeEnvironment(values []string) (profileEnvironment, error) {
	return prepareProfileEnvironment(values, true)
}

func prepareProfileEnvironment(values []string, native bool) (profileEnvironment, error) {
	entries, err := parseEnvironmentForProfile(values, native)
	if err != nil {
		return profileEnvironment{}, err
	}
	home, claudeHome, err := profileHomes(entries)
	if err != nil {
		return profileEnvironment{}, err
	}
	result := profileEnvironment{Home: home, ClaudeHome: claudeHome}
	result.Values = profileEnvironmentValues(entries, native)
	result.WritableRoots, err = profileWritableRoots(home, claudeHome, entries, native)
	if err != nil {
		return profileEnvironment{}, err
	}
	slices.Sort(result.Values)
	return result, nil
}

func profileHomes(entries map[string]string) (string, string, error) {
	home, err := config.CanonicalizePath(entries["HOME"])
	if err != nil {
		return "", "", fmt.Errorf("%w: invalid HOME", ErrUnsupportedProfile)
	}
	claudeHome, err := config.CanonicalizePath(filepath.Join(home, ".claude"))
	if err != nil {
		return "", "", fmt.Errorf("%w: invalid Claude home", ErrUnsupportedProfile)
	}
	return home, claudeHome, nil
}

func profileEnvironmentValues(entries map[string]string, native bool) []string {
	values := make([]string, 0, len(entries))
	if !native {
		// The historical restricted runtime processes this explicit false value
		// before its first-pin-wins storage latch. It remains part of old
		// launch reconstruction only.
		values = append(values, storageBackendPin)
	}
	for _, key := range []string{"HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING"} {
		if value, exists := entries[key]; exists {
			values = append(values, key+"="+value)
		}
	}
	if native {
		for key := range entries {
			if nativeDiscoveryKey(key) {
				values = append(values, key+"="+entries[key])
			}
		}
	}
	return values
}

func profileWritableRoots(home, claudeHome string, entries map[string]string, native bool) ([]string, error) {
	roots := []string{claudeHome, filepath.Join(home, ".claude.json"), filepath.Join(home, ".cache"), filepath.Join(home, "Library/Application Support/Claude"), filepath.Join(home, "Library/Caches"), filepath.Join(home, "Library/Logs"), filepath.Join(home, "Library/Keychains"), "/tmp", "/var/tmp", "/var/folders", "/dev"}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if entries[key] != "" {
			roots = append(roots, entries[key])
		}
	}
	if native {
		roots = appendNativeDiscoveryRoots(roots, entries)
	}
	return canonicalizeWritableRoots(roots)
}

func appendNativeDiscoveryRoots(roots []string, entries map[string]string) []string {
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_DIRS"} {
		values := []string{entries[key]}
		if strings.HasSuffix(key, "_DIRS") {
			values = filepath.SplitList(entries[key])
		}
		for _, value := range values {
			if filepath.IsAbs(value) {
				roots = append(roots, filepath.Join(value, "claude"))
			}
		}
	}
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "ANTHROPIC_CONFIG_DIR"} {
		if value := entries[key]; filepath.IsAbs(value) {
			roots = append(roots, value)
		}
	}
	return roots
}

func canonicalizeWritableRoots(roots []string) ([]string, error) {
	for index, path := range roots {
		canonical, err := config.CanonicalizePath(path)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid runtime writable root", ErrUnsupportedProfile)
		}
		roots[index] = canonical
	}
	slices.Sort(roots)
	return slices.Compact(roots), nil
}

func parseEnvironment(values []string) (map[string]string, error) {
	return parseEnvironmentForProfile(values, false)
}

func parseEnvironmentForProfile(values []string, native bool) (map[string]string, error) {
	entries := make(map[string]string, len(values))
	for _, entry := range values {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("%w: malformed environment entry", ErrUnsupportedProfile)
		}
		if _, exists := entries[key]; exists {
			return nil, fmt.Errorf("%w: duplicate environment key", ErrUnsupportedProfile)
		}
		if !native && (strings.HasPrefix(key, "CLAUDE") || strings.HasPrefix(key, "ANTHROPIC")) {
			if key+"="+value != storageBackendPin {
				return nil, fmt.Errorf("%w: alternate Claude environment selectors are unsupported", ErrUnsupportedProfile)
			}
		}
		entries[key] = value
	}
	return entries, nil
}

// nativeDiscoveryKey keeps nonsecret provider configuration and login-session
// locations available to Claude without copying credential or token values.
func nativeDiscoveryKey(key string) bool {
	switch key {
	case "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
		"CLAUDE_CONFIG_DIR", "ANTHROPIC_CONFIG_DIR":
		return true
	default:
		return false
	}
}

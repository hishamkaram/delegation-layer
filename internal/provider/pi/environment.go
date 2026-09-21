package pi

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

type profileEnvironment struct {
	Home          string
	PiRoot        string
	AgentDir      string
	SessionDir    string
	Values        []string
	WritableRoots []string
}

const (
	piAgentDirEnv   = "PI_CODING_AGENT_DIR"
	piSessionDirEnv = "PI_CODING_AGENT_SESSION_DIR"
)

var nativeRuntimePathKeys = []string{
	"TMPDIR", "TMP", "TEMP", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
	"GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME", "GRADLE_USER_HOME",
	"NPM_CONFIG_CACHE", "GOPATH",
}

// prepareEnvironment retains the historical isolated environment contract.
func prepareEnvironment(values []string) (profileEnvironment, error) {
	return prepareProfileEnvironment(values, false)
}

// prepareProfileEnvironment retains native nonsecret discovery and session
// selectors for new tasks while keeping the historical environment contract
// available to reconstruction of older tasks.
func prepareProfileEnvironment(values []string, native bool) (profileEnvironment, error) {
	entries, err := parseEnvironment(values)
	if err != nil {
		return profileEnvironment{}, err
	}
	home, err := config.CanonicalizePath(entries["HOME"])
	if err != nil {
		return profileEnvironment{}, fmt.Errorf("%w: invalid HOME", ErrUnsupportedProfile)
	}
	piRoot, err := config.CanonicalizePath(filepath.Join(home, ".pi"))
	if err != nil {
		return profileEnvironment{}, fmt.Errorf("%w: invalid Pi home", ErrUnsupportedProfile)
	}
	agentDir, err := resolveNativeDirectory(entries, piAgentDirEnv, filepath.Join(piRoot, "agent"), "config")
	if err != nil {
		return profileEnvironment{}, err
	}
	sessionDir, err := resolveNativeDirectory(entries, piSessionDirEnv, filepath.Join(agentDir, "sessions"), "session")
	if err != nil {
		return profileEnvironment{}, err
	}
	environmentValues := profileEnvironmentValues(entries, native, agentDir, sessionDir)
	writableRoots, err := profileWritableRoots(home, piRoot, agentDir, sessionDir, entries, native)
	if err != nil {
		return profileEnvironment{}, err
	}
	return profileEnvironment{Home: home, PiRoot: piRoot, AgentDir: agentDir, SessionDir: sessionDir, Values: environmentValues, WritableRoots: writableRoots}, nil
}

func profileEnvironmentValues(entries map[string]string, native bool, agentDir, sessionDir string) []string {
	values := make([]string, 0, len(entries))
	for _, key := range []string{
		"HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
		"TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING",
	} {
		if value, exists := entries[key]; exists {
			values = append(values, key+"="+value)
		}
	}
	if _, exists := entries[piAgentDirEnv]; exists {
		values = append(values, piAgentDirEnv+"="+agentDir)
	}
	if native {
		if _, exists := entries[piSessionDirEnv]; exists {
			values = append(values, piSessionDirEnv+"="+sessionDir)
		}
		values = appendNativeDiscoveryValues(values, entries)
	} else {
		// Pin the resolved session directory for historical read-only tasks.
		values = append(values, piSessionDirEnv+"="+sessionDir)
	}
	slices.Sort(values)
	return values
}

func appendNativeDiscoveryValues(values []string, entries map[string]string) []string {
	for key, value := range entries {
		if nativeDiscoveryKey(key) {
			values = append(values, key+"="+value)
		}
	}
	return values
}

func profileWritableRoots(home, piRoot, agentDir, sessionDir string, entries map[string]string, native bool) ([]string, error) {
	roots := []string{
		piRoot, agentDir, sessionDir,
		"/tmp", "/var/tmp", "/var/folders", "/dev",
		filepath.Join(home, "Library/Caches"), filepath.Join(home, "Library/Logs"),
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if entries[key] != "" {
			roots = append(roots, entries[key])
		}
	}
	if native {
		roots = appendNativeRuntimeRoots(roots, entries)
	}
	return canonicalWritableRoots(roots)
}

func appendNativeRuntimeRoots(roots []string, entries map[string]string) []string {
	for _, key := range nativeRuntimePathKeys {
		values := []string{entries[key]}
		if key == "GOPATH" {
			values = filepath.SplitList(entries[key])
		}
		for _, value := range values {
			if value == "" || (key == "GOCACHE" && value == "off") {
				continue
			}
			roots = append(roots, value)
		}
	}
	return roots
}

func canonicalWritableRoots(roots []string) ([]string, error) {
	for index, path := range roots {
		canonical, pathErr := config.CanonicalizePath(path)
		if pathErr != nil {
			return nil, fmt.Errorf("%w: invalid runtime writable root", ErrUnsupportedProfile)
		}
		roots[index] = canonical
	}
	slices.Sort(roots)
	return slices.Compact(roots), nil
}

func nativeDiscoveryKey(key string) bool {
	switch key {
	case "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
		"GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME", "GRADLE_USER_HOME", "NPM_CONFIG_CACHE", "GOPATH":
		return true
	default:
		return false
	}
}

func resolveNativeDirectory(entries map[string]string, key, fallback, label string) (string, error) {
	value, exists := entries[key]
	if !exists {
		value = fallback
	}
	if value == "" {
		return "", fmt.Errorf("%w: empty Pi %s directory", ErrUnsupportedProfile, label)
	}
	canonical, err := config.CanonicalizePath(value)
	if err != nil {
		return "", fmt.Errorf("%w: invalid Pi %s directory", ErrUnsupportedProfile, label)
	}
	return canonical, nil
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
		entries[key] = value
	}
	return entries, nil
}

package codex

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

type profileEnvironment struct {
	Home          string
	CodexHome     string
	Values        []string
	WritableRoots []string
}

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
	if authErr := rejectAlternateAuthentication(entries, native); authErr != nil {
		return profileEnvironment{}, authErr
	}
	home, err := config.CanonicalizePath(entries["HOME"])
	if err != nil {
		return profileEnvironment{}, fmt.Errorf("%w: invalid HOME", ErrUnsupportedProfile)
	}
	codexHome := entries["CODEX_HOME"]
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	codexHome, err = config.CanonicalizePath(codexHome)
	if err != nil {
		return profileEnvironment{}, fmt.Errorf("%w: invalid CODEX_HOME", ErrUnsupportedProfile)
	}
	filteredValues := environmentValues(entries, native)
	roots, err := writableRoots(home, codexHome, entries, native)
	if err != nil {
		return profileEnvironment{}, err
	}
	return profileEnvironment{Home: home, CodexHome: codexHome, Values: filteredValues, WritableRoots: roots}, nil
}

func rejectAlternateAuthentication(entries map[string]string, native bool) error {
	if native {
		return nil
	}
	for _, key := range []string{"CODEX_API_KEY", "OPENAI_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE", "OPENAI_WORKLOAD_IDENTITY_CONTEXT", "CODEX_REFRESH_TOKEN_URL_OVERRIDE", "CODEX_REVOKE_TOKEN_URL_OVERRIDE", "CODEX_APP_SERVER_LOGIN_CLIENT_ID"} {
		if _, present := entries[key]; present {
			return fmt.Errorf("%w: alternate authentication is outside the personal file profile", ErrUnsupportedProfile)
		}
	}
	return nil
}

func environmentValues(entries map[string]string, native bool) []string {
	values := make([]string, 0, len(entries))
	for _, key := range []string{"HOME", "CODEX_HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING"} {
		if value, exists := entries[key]; exists {
			values = append(values, key+"="+value)
		}
	}
	if native {
		values = appendNativeDiscoveryValues(values, entries)
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

func writableRoots(home, codexHome string, entries map[string]string, native bool) ([]string, error) {
	roots := []string{codexHome, "/tmp", "/var/tmp", "/var/folders", "/dev", filepath.Join(home, "Library/Caches"), filepath.Join(home, "Library/Logs")}
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

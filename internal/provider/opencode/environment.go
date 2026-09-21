package opencode

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

type profileEnvironment struct {
	Home          string
	Values        []string
	WritableRoots []string
}

// nativeEnvironmentKeys are the nonsecret process controls needed by both the
// historical and native OpenCode profiles. Provider credentials remain in
// OpenCode's own native store; arbitrary ambient variables are not copied into
// a queued task.
var nativeEnvironmentKeys = map[string]struct{}{
	"HOME": {}, "PATH": {}, "USER": {}, "LOGNAME": {}, "SHELL": {},
	"LANG": {}, "LC_ALL": {}, "LC_CTYPE": {}, "TZ": {},
	"TMPDIR": {}, "TMP": {}, "TEMP": {},
	"XDG_CONFIG_HOME": {}, "XDG_CONFIG_DIRS": {},
	"XDG_DATA_HOME": {}, "XDG_DATA_DIRS": {}, "XDG_STATE_HOME": {}, "XDG_CACHE_HOME": {},
	"__CF_USER_TEXT_ENCODING": {},
}

var nativeOpenCodeEnvironmentKeys = map[string]struct{}{
	"OPENCODE_CONFIG":     {},
	"OPENCODE_CONFIG_DIR": {},
	"OPENCODE_TUI_CONFIG": {},
	"OPENCODE_PERMISSION": {},
	"OPENCODE_AUTO_SHARE": {},
}

func prepareEnvironment(values []string) (profileEnvironment, error) {
	return prepareProfileEnvironment(values, false)
}

// prepareProfileEnvironment keeps the historical isolated environment for
// reconstruction while allowing new tasks to retain native OpenCode discovery
// and session selectors. Sensitive inline configuration and authentication
// values are deliberately excluded from both paths.
func prepareProfileEnvironment(values []string, native bool) (profileEnvironment, error) {
	entries := make(map[string]string, len(values))
	for _, entry := range values {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(key, '\x00') || strings.ContainsRune(value, '\x00') {
			return profileEnvironment{}, fmt.Errorf("%w: malformed environment entry", ErrUnsupportedProfile)
		}
		if _, exists := entries[key]; exists {
			return profileEnvironment{}, fmt.Errorf("%w: duplicate environment key", ErrUnsupportedProfile)
		}
		entries[key] = value
	}
	home, err := config.CanonicalizePath(entries["HOME"])
	if err != nil {
		return profileEnvironment{}, fmt.Errorf("%w: invalid HOME", ErrUnsupportedProfile)
	}
	result := profileEnvironment{Home: home}
	for key, value := range entries {
		if _, allowed := nativeEnvironmentKeys[key]; allowed || native && nativeDiscoveryKey(key) {
			result.Values = append(result.Values, key+"="+value)
		}
	}
	slices.Sort(result.Values)
	if native {
		result.WritableRoots, err = nativeRuntimeWritableRoots(home, entries)
	} else {
		result.WritableRoots, err = runtimeWritableRoots(home, entries)
	}
	if err != nil {
		return profileEnvironment{}, err
	}
	return result, nil
}

// nativeDiscoveryKey retains path and policy selectors that change where
// OpenCode finds user configuration, extensions, authentication, or sessions.
// Inline config/auth content is excluded because task metadata must remain
// nonsecret and the provider can load the native files directly.
func nativeDiscoveryKey(key string) bool {
	switch key {
	case "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK":
		return true
	}
	_, allowed := nativeOpenCodeEnvironmentKeys[key]
	return allowed
}

func runtimeWritableRoots(home string, entries map[string]string) ([]string, error) {
	delegationConfigHome, err := delegationConfigHome(home, entries["XDG_CONFIG_HOME"])
	if err != nil {
		return nil, err
	}
	paths := []string{
		"/tmp", "/var/tmp", "/var/folders", "/dev",
		delegationConfigHome,
		filepath.Join(home, ".opencode"),
		filepath.Join(home, ".config/opencode"),
		filepath.Join(home, ".local/share/opencode"),
		filepath.Join(home, ".local/state/opencode"),
		filepath.Join(home, ".cache/opencode"),
		filepath.Join(home, ".cache"),
		filepath.Join(home, "Library/Application Support/opencode"),
		filepath.Join(home, "Library/Caches/opencode"),
		filepath.Join(home, "Library/Logs/opencode"),
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if value := entries[name]; value != "" {
			paths = append(paths, value)
		}
	}
	// XDG *_HOME variables name shared roots. OpenCode owns only its
	// provider-specific child, so the entire shared directory must not be
	// classified as writable. This also keeps delegation-layer's default state
	// root ($XDG_CONFIG_HOME/delegation-layer on Linux) disjoint.
	// XDG_CONFIG_HOME is replaced by environmentForMode with the isolated
	// delegation config root above. Including its provider child here would
	// make the candidate depend on whether the caller already has that
	// replacement applied (admission versus runner reconstruction).
	for _, name := range []string{"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		if value := entries[name]; value != "" {
			paths = append(paths, filepath.Join(value, "opencode"))
		}
	}
	for index, path := range paths {
		canonical, err := config.CanonicalizePath(path)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid runtime writable root", ErrUnsupportedProfile)
		}
		paths[index] = canonical
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

func nativeRuntimeWritableRoots(home string, entries map[string]string) ([]string, error) {
	paths := nativeRuntimeRootPaths(home, entries)
	for index, path := range paths {
		canonical, err := config.CanonicalizePath(path)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid native runtime writable root", ErrUnsupportedProfile)
		}
		paths[index] = canonical
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

func nativeRuntimeRootPaths(home string, entries map[string]string) []string {
	paths := []string{
		"/tmp", "/var/tmp", "/var/folders", "/dev",
		filepath.Join(home, ".opencode"),
		filepath.Join(home, ".config/opencode"),
		filepath.Join(home, ".local/share/opencode"),
		filepath.Join(home, ".local/state/opencode"),
		filepath.Join(home, ".cache/opencode"),
		filepath.Join(home, ".cache"),
		filepath.Join(home, "Library/Application Support/opencode"),
		filepath.Join(home, "Library/Caches/opencode"),
		filepath.Join(home, "Library/Logs/opencode"),
	}
	paths = appendEnvironmentRoots(paths, entries, []string{"TMPDIR", "TMP", "TEMP"})
	paths = appendOpenCodeXDGHomeRoots(paths, entries)
	paths = appendOpenCodeXDGListRoots(paths, entries)
	paths = appendEnvironmentRoots(paths, entries, []string{"OPENCODE_CONFIG_DIR"})
	paths = appendConfigFileRoots(paths, entries, []string{"OPENCODE_CONFIG", "OPENCODE_TUI_CONFIG"})
	return paths
}

func appendEnvironmentRoots(paths []string, entries map[string]string, names []string) []string {
	for _, name := range names {
		if value := entries[name]; value != "" {
			paths = append(paths, value)
		}
	}
	return paths
}

func appendOpenCodeXDGHomeRoots(paths []string, entries map[string]string) []string {
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		if value := entries[name]; value != "" {
			paths = append(paths, filepath.Join(value, "opencode"))
		}
	}
	return paths
}

func appendOpenCodeXDGListRoots(paths []string, entries map[string]string) []string {
	for _, name := range []string{"XDG_CONFIG_DIRS", "XDG_DATA_DIRS"} {
		for _, value := range filepath.SplitList(entries[name]) {
			if value != "" {
				paths = append(paths, filepath.Join(value, "opencode"))
			}
		}
	}
	return paths
}

func appendConfigFileRoots(paths []string, entries map[string]string, names []string) []string {
	for _, name := range names {
		if value := entries[name]; value != "" {
			paths = append(paths, value)
		}
	}
	return paths
}

func delegationConfigHome(home, xdgConfigHome string) (string, error) {
	base := xdgConfigHome
	if base == "" {
		base = filepath.Join(home, ".config")
	}
	base = filepath.Clean(base)
	path := base
	if filepath.Base(base) != "delegation-layer-opencode" {
		path = filepath.Join(base, "delegation-layer-opencode")
	}
	path, err := config.CanonicalizePath(path)
	if err != nil {
		return "", fmt.Errorf("%w: invalid isolated config home", ErrUnsupportedProfile)
	}
	return path, nil
}

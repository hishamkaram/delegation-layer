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

// nativeEnvironmentKeys are the nonsecret process controls needed for native
// OpenCode discovery. Provider credentials remain in OpenCode's own native
// store; arbitrary ambient variables are not copied into a queued task.
var nativeEnvironmentKeys = map[string]struct{}{
	"HOME": {}, "PATH": {}, "USER": {}, "LOGNAME": {}, "SHELL": {},
	"LANG": {}, "LC_ALL": {}, "LC_CTYPE": {}, "TZ": {},
	"TMPDIR": {}, "TMP": {}, "TEMP": {},
	"XDG_CONFIG_HOME": {}, "XDG_CONFIG_DIRS": {},
	"XDG_DATA_HOME": {}, "XDG_DATA_DIRS": {}, "XDG_STATE_HOME": {}, "XDG_CACHE_HOME": {},
	"__CF_USER_TEXT_ENCODING": {},
}

func prepareEnvironment(values []string) (profileEnvironment, error) {
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
		if _, allowed := nativeEnvironmentKeys[key]; allowed {
			result.Values = append(result.Values, key+"="+value)
		}
	}
	slices.Sort(result.Values)
	result.WritableRoots, err = runtimeWritableRoots(home, entries)
	if err != nil {
		return profileEnvironment{}, err
	}
	return result, nil
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

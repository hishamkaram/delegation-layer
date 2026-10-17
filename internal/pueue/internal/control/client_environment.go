package control

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func canonicalExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: executable must be absolute", ErrConfiguration)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%w: executable unavailable: %w", ErrConfiguration, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: executable must be an executable regular file", ErrConfiguration)
	}
	return resolved, nil
}

func (c *Client) resolution() (ResolutionContext, error) {
	return c.resolutionForEnvironment(environmentForCommand(c.options.Environment))
}

func (c *Client) resolutionForEnvironment(environment []string) (ResolutionContext, error) {
	if c.options.resolutionPinned {
		if c.options.Resolution == nil {
			return ResolutionContext{}, ErrConfiguration
		}
		resolution := *c.options.Resolution
		if err := validateResolutionContext(resolution); err != nil {
			return ResolutionContext{}, err
		}
		return resolution, nil
	}
	current, err := resolutionContext(environment)
	if err != nil {
		return ResolutionContext{}, err
	}
	if c.options.Resolution == nil {
		return current, nil
	}
	resolution := *c.options.Resolution
	if err := validateResolutionContext(resolution); err != nil {
		return ResolutionContext{}, err
	}
	if !sameEnvironmentResolution(resolution, current) {
		return ResolutionContext{}, fmt.Errorf("%w: resolution does not match command environment", ErrConfiguration)
	}
	return resolution, nil
}

func environmentForResolution(environment []string, resolution ResolutionContext) []string {
	merged := append([]string(nil), environmentForCommand(environment)...)
	for _, value := range []struct {
		key   string
		value string
	}{
		{key: "HOME", value: resolution.Home},
		{key: "XDG_DATA_HOME", value: resolution.DataLocalDirectory},
		{key: "XDG_CONFIG_HOME", value: resolution.ConfigDirectory},
		{key: "XDG_RUNTIME_DIR", value: resolution.RuntimeDirectory},
	} {
		if resolution.OS != "linux" && value.key != "HOME" {
			continue
		}
		merged = replaceEnvironmentValue(merged, value.key, value.value)
	}
	return merged
}

func replaceEnvironmentValue(environment []string, key, value string) []string {
	entry := key + "=" + value
	for index, current := range environment {
		name, _, ok := strings.Cut(current, "=")
		if ok && name == key {
			environment[index] = entry
			return environment
		}
	}
	return append(environment, entry)
}

func environmentForCommand(environment []string) []string {
	if environment != nil {
		return environment
	}
	return defaultEnvironment()
}

// defaultEnvironment bounds the environment inherited by the pueue client.
// It is rebuilt for each invocation so HOME/XDG changes are observed by both
// resolution and the child process without carrying unrelated ambient state.
func defaultEnvironment() []string {
	ambient := os.Environ()
	filtered := make([]string, 0, len(ambient))
	for _, entry := range ambient {
		key, _, ok := strings.Cut(entry, "=")
		if ok && defaultEnvironmentKey(key) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// DefaultEnvironment exposes the same bounded control environment used by a
// client with nil Options.Environment. Composition roots can add a provider's
// explicitly declared nonsecret variables without dropping platform-specific
// values such as Linux XDG socket/configuration selectors.
func DefaultEnvironment() []string { return defaultEnvironment() }

func defaultEnvironmentKey(key string) bool {
	switch key {
	case "HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING":
		return true
	case "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR":
		return runtime.GOOS == "linux"
	default:
		return false
	}
}

func sameEnvironmentResolution(resolution, current ResolutionContext) bool {
	return resolution.OS == current.OS &&
		resolution.Home == current.Home &&
		resolution.DataLocalDirectory == current.DataLocalDirectory &&
		resolution.ConfigDirectory == current.ConfigDirectory &&
		resolution.RuntimeDirectory == current.RuntimeDirectory &&
		resolution.Username == current.Username
}

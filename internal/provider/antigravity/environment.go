package antigravity

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/config"
)

type profileEnvironment struct {
	Home          string
	WritableRoots []string
	Values        []string
}

// prepareEnvironment preserves the caller's authentication and environment.
// Only path decisions enter the policy snapshot; credential values never do.
// Alternate provider/config discovery roots need a separately verified profile.
func prepareEnvironment(values []string) (profileEnvironment, error) {
	environment := make(map[string]string, len(values))
	for _, entry := range values {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return profileEnvironment{}, fmt.Errorf("%w: invalid environment entry", ErrUnsupportedProfile)
		}
		if _, duplicate := environment[key]; duplicate {
			return profileEnvironment{}, fmt.Errorf("%w: duplicate environment key", ErrUnsupportedProfile)
		}
		environment[key] = value
	}
	if err := rejectAlternateDiscovery(environment); err != nil {
		return profileEnvironment{}, err
	}
	home, err := config.CanonicalizePath(environment["HOME"])
	if err != nil {
		return profileEnvironment{}, fmt.Errorf("%w: HOME must resolve to an absolute directory", ErrUnsupportedProfile)
	}
	roots, err := runtimeStateExclusions(home, environment)
	if err != nil {
		return profileEnvironment{}, err
	}
	return profileEnvironment{Home: home, WritableRoots: roots, Values: slices.Clone(values)}, nil
}

func rejectAlternateDiscovery(environment map[string]string) error {
	for _, name := range []string{
		"XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS", "XDG_STATE_HOME",
		"GEMINI_HOME", "GEMINI_CLI_HOME", "ANTIGRAVITY_HOME", "ANTIGRAVITY_CONFIG_HOME", "AGY_HOME", "AGY_CONFIG_HOME",
	} {
		if environment[name] != "" {
			return fmt.Errorf("%w: alternate discovery variable %s is not supported", ErrUnsupportedProfile, name)
		}
	}
	return nil
}

// runtimeStateExclusions conservatively excludes the provider runtime, system
// temporary roots, and the documented build-cache classes. Some entries are
// broader than the provider's actual grants; this is a state-placement check,
// never an assertion that the agent is authorized to write every listed path.
func runtimeStateExclusions(home string, environment map[string]string) ([]string, error) {
	roots := []string{"/tmp", "/var/tmp", "/var/folders", "/dev"}
	for _, relative := range []string{
		".gemini", ".cache", ".cargo", ".rustup", ".npm", ".nvm", ".bun", ".gradle", ".m2",
		".dotnet", ".nuget", ".nimble", ".docker", ".local", "go",
		"Library/Caches", "Library/Logs", "Library/Application Support/Antigravity", "Library/Application Support/Google/Antigravity",
	} {
		roots = append(roots, filepath.Join(home, relative))
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP", "XDG_CACHE_HOME", "GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME", "GRADLE_USER_HOME", "NPM_CONFIG_CACHE"} {
		if value := environment[name]; value != "" {
			if name == "GOCACHE" && value == "off" {
				continue
			}
			roots = append(roots, value)
		}
	}
	if value := environment["GOPATH"]; value != "" {
		roots = append(roots, filepath.SplitList(value)...)
	}
	for index, path := range roots {
		canonical, err := config.CanonicalizePath(path)
		if err != nil {
			return nil, fmt.Errorf("%w: runtime temporary/cache root cannot be resolved", ErrUnsupportedProfile)
		}
		roots[index] = canonical
	}
	slices.Sort(roots)
	return slices.Compact(roots), nil
}

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

// nativeEnvironmentKeys is the same bounded, nonsecret control environment
// used by the supervisor client. Antigravity's supported login is native to
// its home directory; ambient credential values are deliberately not copied
// into a queued task record.
var nativeEnvironmentKeys = map[string]struct{}{
	"HOME": {}, "PATH": {}, "USER": {}, "LOGNAME": {}, "SHELL": {},
	"LANG": {}, "LC_ALL": {}, "LC_CTYPE": {}, "TZ": {},
	"TMPDIR": {}, "TMP": {}, "TEMP": {}, "__CF_USER_TEXT_ENCODING": {},
}

// prepareEnvironment preserves the native Antigravity login location and the
// bounded process-control values. Unrelated ambient variables, including
// credential values, never cross the supervisor boundary.
func prepareEnvironment(values []string) (profileEnvironment, error) {
	return prepareProfileEnvironment(values, false)
}

func prepareProfileEnvironment(values []string, native bool) (profileEnvironment, error) {
	environment := make(map[string]string, len(values))
	for _, entry := range values {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return profileEnvironment{}, fmt.Errorf("%w: invalid environment entry", ErrUnsupportedProfile)
		}
		// The pueue shell adds these bookkeeping values to queued workers and
		// may rewrite shell bookkeeping. They are not provider inputs and must
		// not change the immutable inspection definition between admission and
		// the supervised worker.
		if volatileSupervisorEnvironmentKey(key) {
			continue
		}
		if _, duplicate := environment[key]; duplicate {
			return profileEnvironment{}, fmt.Errorf("%w: duplicate environment key", ErrUnsupportedProfile)
		}
		environment[key] = value
	}
	if !native {
		if err := rejectAlternateDiscovery(environment); err != nil {
			return profileEnvironment{}, err
		}
	}
	home, err := config.CanonicalizePath(environment["HOME"])
	if err != nil {
		return profileEnvironment{}, fmt.Errorf("%w: HOME must resolve to an absolute directory", ErrUnsupportedProfile)
	}
	roots, err := runtimeStateExclusions(home, environment)
	if err != nil {
		return profileEnvironment{}, err
	}
	filtered := make([]string, 0, len(nativeEnvironmentKeys))
	for key, value := range environment {
		if _, allowed := nativeEnvironmentKeys[key]; !allowed && (!native || !nativeDiscoveryKey(key)) {
			continue
		}
		filtered = append(filtered, key+"="+value)
	}
	slices.Sort(filtered)
	return profileEnvironment{Home: home, WritableRoots: roots, Values: filtered}, nil
}

func volatileSupervisorEnvironmentKey(key string) bool {
	if strings.HasPrefix(key, "PUEUE_") {
		return true
	}
	switch key {
	case "_", "OLDPWD", "SHLVL":
		return true
	default:
		return false
	}
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

// nativeDiscoveryKey preserves nonsecret native configuration and session locations.
func nativeDiscoveryKey(key string) bool {
	switch key {
	case "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS", "XDG_DATA_HOME", "XDG_DATA_DIRS", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS",
		"GEMINI_HOME", "GEMINI_CLI_HOME", "ANTIGRAVITY_HOME", "ANTIGRAVITY_CONFIG_HOME", "AGY_HOME", "AGY_CONFIG_HOME",
		"GOCACHE", "GOMODCACHE", "CARGO_HOME", "RUSTUP_HOME", "GRADLE_USER_HOME", "NPM_CONFIG_CACHE", "GOPATH":
		return true
	default:
		return false
	}
}

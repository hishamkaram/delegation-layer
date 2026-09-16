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

// prepareEnvironment retains only nonsecret process settings. Pi resolves
// authentication from its native account store; credentials and arbitrary
// ambient variables must not enter persisted inspection metadata.
func prepareEnvironment(values []string) (profileEnvironment, error) {
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
	result := profileEnvironment{Home: home, PiRoot: piRoot, AgentDir: agentDir, SessionDir: sessionDir}
	for _, key := range []string{
		"HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
		"TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING",
	} {
		if value, exists := entries[key]; exists {
			result.Values = append(result.Values, key+"="+value)
		}
	}
	if _, exists := entries[piAgentDirEnv]; exists {
		result.Values = append(result.Values, piAgentDirEnv+"="+agentDir)
	}
	// Pin the resolved session directory even when the caller did not set the
	// selector. Pi reads settings.json before handling --help; without this
	// value a native sessionDir setting could make the capability probe create
	// files in the task workspace.
	result.Values = append(result.Values, piSessionDirEnv+"="+sessionDir)
	result.WritableRoots = []string{
		piRoot, agentDir, sessionDir,
		"/tmp", "/var/tmp", "/var/folders", "/dev",
		filepath.Join(home, "Library/Caches"), filepath.Join(home, "Library/Logs"),
	}
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

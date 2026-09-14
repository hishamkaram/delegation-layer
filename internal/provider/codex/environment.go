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

// prepareEnvironment preserves native credential locations. Alternate auth
// selectors are rejected rather than silently switching the native account.
func prepareEnvironment(values []string) (profileEnvironment, error) {
	entries, err := parseEnvironment(values)
	if err != nil {
		return profileEnvironment{}, err
	}
	for _, key := range []string{"CODEX_API_KEY", "OPENAI_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE", "OPENAI_WORKLOAD_IDENTITY_CONTEXT", "CODEX_REFRESH_TOKEN_URL_OVERRIDE", "CODEX_REVOKE_TOKEN_URL_OVERRIDE", "CODEX_APP_SERVER_LOGIN_CLIENT_ID"} {
		if _, present := entries[key]; present {
			return profileEnvironment{}, fmt.Errorf("%w: alternate authentication is outside the personal file profile", ErrUnsupportedProfile)
		}
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
	result := profileEnvironment{Home: home, CodexHome: codexHome}
	for _, key := range []string{"HOME", "CODEX_HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING"} {
		if value, exists := entries[key]; exists {
			result.Values = append(result.Values, key+"="+value)
		}
	}
	result.WritableRoots = []string{codexHome, "/tmp", "/var/tmp", "/var/folders", "/dev", filepath.Join(home, "Library/Caches"), filepath.Join(home, "Library/Logs")}
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
		entries[key] = value
	}
	return entries, nil
}

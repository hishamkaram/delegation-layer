package pueue

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
)

// ResolutionContext contains only inputs to pinned pueue path resolution.
// Empty platform directories retain the upstream fallback; unknown OS fails.
type ResolutionContext struct {
	OS                 string
	Home               string
	DataLocalDirectory string
	ConfigDirectory    string
	RuntimeDirectory   string
	Username           string
	// Cwd is the local process directory used for command execution. It is
	// intentionally not persisted as supervisor path-resolution state because
	// the original caller directory may be removed before recovery.
	Cwd string
}

type ResolvedConfig struct {
	Client            ClientConfig
	Daemon            DaemonConfig
	PueueDirectory    string
	RuntimeDirectory  string
	AliasFile         string
	SocketPath        string
	PIDPath           string
	SecretPath        string
	CertificatePath   string
	KeyPath           string
	SocketPermissions *uint32
	Host              string
	Port              string
}

// CurrentResolutionContext mirrors the supported Unix dirs defaults without
// changing HOME/XDG or consulting a developer's pueue configuration.
func CurrentResolutionContext() (ResolutionContext, error) {
	return resolutionContext(os.Environ())
}

func resolutionContext(environment []string) (ResolutionContext, error) {
	u, err := user.Current()
	if err != nil {
		return ResolutionContext{}, fmt.Errorf("%w: determine current user: %w", ErrConfiguration, err)
	}
	home := environmentValue(environment, "HOME")
	if home == "" {
		home = u.HomeDir
	}
	home = filepath.Clean(home)
	cwd := ""
	if current, cwdErr := os.Getwd(); cwdErr == nil {
		cwd = filepath.Clean(current)
	}
	r := ResolutionContext{OS: runtime.GOOS, Home: home, Username: u.Username, Cwd: cwd}
	switch runtime.GOOS {
	case "darwin":
		r.DataLocalDirectory = filepath.Join(home, "Library", "Application Support")
		r.ConfigDirectory = r.DataLocalDirectory
	case "linux":
		r.DataLocalDirectory = absoluteEnvironmentOr(environment, "XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
		r.ConfigDirectory = absoluteEnvironmentOr(environment, "XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		r.RuntimeDirectory = absoluteEnvironmentOr(environment, "XDG_RUNTIME_DIR", "")
	default:
		return ResolutionContext{}, fmt.Errorf("%w: unsupported platform", ErrConfiguration)
	}
	return r, nil
}

func resolutionFromBinding(binding task.SupervisorRef) (ResolutionContext, bool, error) {
	if binding.ResolutionOS == "" {
		return ResolutionContext{}, false, nil
	}
	resolution := ResolutionContext{
		OS:                 binding.ResolutionOS,
		Home:               binding.ResolutionHome,
		DataLocalDirectory: binding.ResolutionDataLocal,
		ConfigDirectory:    binding.ResolutionConfig,
		RuntimeDirectory:   binding.ResolutionRuntime,
		Username:           binding.ResolutionUsername,
	}
	if err := validateResolutionContext(resolution); err != nil {
		return ResolutionContext{}, false, err
	}
	return resolution, true, nil
}

func setBindingResolution(binding *task.SupervisorRef, resolution ResolutionContext) {
	binding.ResolutionOS = resolution.OS
	binding.ResolutionHome = cleanResolutionPath(resolution.Home)
	binding.ResolutionDataLocal = cleanResolutionPath(resolution.DataLocalDirectory)
	binding.ResolutionConfig = cleanResolutionPath(resolution.ConfigDirectory)
	binding.ResolutionRuntime = cleanResolutionPath(resolution.RuntimeDirectory)
	binding.ResolutionUsername = resolution.Username
}

func absoluteEnvironmentOr(environment []string, key, fallback string) string {
	if v := environmentValue(environment, key); filepath.IsAbs(v) {
		return filepath.Clean(v)
	}
	return cleanResolutionPath(fallback)
}

func cleanResolutionPath(value string) string {
	if value == "" {
		return ""
	}
	return filepath.Clean(value)
}

func environmentValue(environment []string, key string) string {
	for index := len(environment) - 1; index >= 0; index-- {
		name, value, ok := strings.Cut(environment[index], "=")
		if ok && name == key {
			return value
		}
	}
	return ""
}

// ResolveConfig is pure. Relative filesystem coordinates are rejected because
// an existing daemon's cwd cannot be inferred from a client config file.
func ResolveConfig(c *Config, r ResolutionContext) (ResolvedConfig, error) {
	if c == nil || (r.OS != "darwin" && r.OS != "linux") {
		return ResolvedConfig{}, ErrConfiguration
	}
	if !c.Shared.UseUnixSocket {
		return ResolvedConfig{}, fmt.Errorf("%w: only Unix socket endpoints are supported", ErrConfiguration)
	}
	if err := validateResolutionContext(r); err != nil {
		return ResolvedConfig{}, err
	}
	dataFallback := "./pueue"
	if r.DataLocalDirectory != "" {
		dataFallback = filepath.Join(r.DataLocalDirectory, "pueue")
	}
	data, err := resolvePath(c.Shared.PueueDirectory, dataFallback, r.Home)
	if err != nil {
		return ResolvedConfig{}, err
	}
	runtimeDir, err := resolvePath(c.Shared.RuntimeDirectory, valueOrNonempty(r.RuntimeDirectory, data), r.Home)
	if err != nil {
		return ResolvedConfig{}, err
	}
	return resolveRemaining(c, r, data, runtimeDir)
}

func validateResolutionContext(r ResolutionContext) error {
	for _, p := range []string{r.Home, r.DataLocalDirectory, r.ConfigDirectory, r.RuntimeDirectory, r.Cwd} {
		if p != "" && (!filepath.IsAbs(p) || strings.ContainsRune(p, 0)) {
			return fmt.Errorf("%w: resolution context path must be absolute", ErrConfiguration)
		}
	}
	return nil
}

func valueOrNonempty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func resolvePath(explicit *string, fallback, home string) (string, error) {
	p := valueOr(explicit, fallback)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("%w: home required for tilde expansion", ErrConfiguration)
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~/"))
		if valueOr(explicit, fallback) == "~" {
			p = home
		}
	}
	if !filepath.IsAbs(p) || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("%w: explicit absolute resolved paths required", ErrConfiguration)
	}
	return filepath.Clean(p), nil
}

func resolveRemaining(c *Config, r ResolutionContext, data, runtimeDir string) (ResolvedConfig, error) {
	if c.Shared.UnixSocketPath == nil && (r.Username == "" || strings.ContainsAny(r.Username, "/\\\x00")) {
		return ResolvedConfig{}, fmt.Errorf("%w: valid username required for default socket", ErrConfiguration)
	}
	aliasFallback := "pueue_aliases.yml"
	if r.ConfigDirectory != "" {
		aliasFallback = filepath.Join(r.ConfigDirectory, "pueue", "pueue_aliases.yml")
	}
	result := ResolvedConfig{Client: c.Client, Daemon: c.Daemon, PueueDirectory: data, RuntimeDirectory: runtimeDir, SocketPermissions: c.Shared.UnixSocketPermissions, Host: c.Shared.Host, Port: c.Shared.Port}
	paths := []struct {
		target   *string
		explicit *string
		fallback string
	}{
		{&result.SocketPath, c.Shared.UnixSocketPath, filepath.Join(runtimeDir, "pueue_"+r.Username+".socket")},
		{&result.AliasFile, c.Shared.AliasFile, aliasFallback},
		{&result.PIDPath, c.Shared.PIDPath, filepath.Join(runtimeDir, "pueue.pid")},
		{&result.SecretPath, c.Shared.SharedSecretPath, filepath.Join(data, "shared_secret")},
		{&result.CertificatePath, c.Shared.DaemonCert, filepath.Join(data, "certs", "daemon.cert")},
		{&result.KeyPath, c.Shared.DaemonKey, filepath.Join(data, "certs", "daemon.key")},
	}
	for _, p := range paths {
		v, err := resolvePath(p.explicit, p.fallback, r.Home)
		if err != nil {
			return ResolvedConfig{}, err
		}
		*p.target = v
	}
	limit := 103
	if r.OS == "linux" {
		limit = 107
	}
	if len(result.SocketPath) > limit {
		return ResolvedConfig{}, fmt.Errorf("%w: Unix socket path too long", ErrConfiguration)
	}
	return result, nil
}

func (r ResolvedConfig) Endpoint() string { return "unix:" + r.SocketPath }

// Digest binds effective configuration, excluding irrelevant process environment.
func (r ResolvedConfig) Digest() (string, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return task.ComputeSHA256(data), nil
}

// ValidateIsolation is a preflight for the private real-daemon acceptance owner.
// It performs no creation and refuses symlink traversal through existing parents.
func (r ResolvedConfig) ValidateIsolation(base string) error {
	if !filepath.IsAbs(base) || filepath.Clean(base) != base {
		return ErrConfiguration
	}
	paths := []string{r.PueueDirectory, r.RuntimeDirectory, r.AliasFile, r.SocketPath, r.PIDPath, r.SecretPath, r.CertificatePath, r.KeyPath}
	for _, path := range paths {
		rel, err := filepath.Rel(base, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: private path escapes base", ErrConfiguration)
		}
		if err := rejectSymlinkComponents(path); err != nil {
			return err
		}
	}
	if r.Client.ShowConfirmationQuestions || r.Daemon.Callback != nil || len(r.Daemon.EnvVars) != 0 {
		return fmt.Errorf("%w: private fixture must disable confirmation, callbacks and environment overrides", ErrConfiguration)
	}
	if r.Daemon.ShellCommand == nil || !slices.Equal(*r.Daemon.ShellCommand, []string{"/bin/sh", "-c", "{{ pueue_command_string }}"}) {
		return fmt.Errorf("%w: private fixture requires pinned shell command", ErrConfiguration)
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink path component", ErrConfiguration)
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

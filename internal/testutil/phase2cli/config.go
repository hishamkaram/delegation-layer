package phase2cli

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const (
	fixtureConfigName = "phase2-fixture.json"
	maxHookDelay      = 10 * time.Second
	maxConfigBytes    = task.MaxControlRecordSize
)

var errInvalidHarnessReference = errors.New("invalid phase2 harness reference")

// HookMode selects one finite, acceptance-only execution perturbation.
type HookMode string

const (
	HookNormal             HookMode = "normal"
	HookStartFailed        HookMode = "start-failed"
	HookWaitFailed         HookMode = "wait-failed"
	HookCaptureFailed      HookMode = "capture-failed"
	HookAbandonBeforeSeal  HookMode = "abandon-before-seal"
	HookStartDelayed       HookMode = "start-delayed"
	HookPublicationDelayed HookMode = "publication-delayed"
)

// HookConfig is the bounded hook portion of phase2-fixture.json.
type HookConfig struct {
	Mode        HookMode `json:"mode"`
	DelayMS     int64    `json:"delay_ms"`
	ReleasePath string   `json:"release_path,omitempty"`
}

// HarnessConfig is the strict sibling configuration consumed by acceptance
// wrappers. It carries only fixed, harness-owned paths and literal values.
type HarnessConfig struct {
	SchemaVersion        int        `json:"schema_version"`
	ProviderExecutable   string     `json:"provider_executable"`
	ProviderSHA256       string     `json:"provider_sha256"`
	ProviderConfig       string     `json:"provider_config"`
	ProviderConfigSHA256 string     `json:"provider_config_sha256"`
	SupervisorExecutable string     `json:"supervisor_executable"`
	EventsDirectory      string     `json:"events_directory"`
	Environment          []string   `json:"environment"`
	Hooks                HookConfig `json:"hooks"`
}

type loadedHarnessConfig struct {
	Config HarnessConfig
	Raw    []byte
}

func siblingConfigPath(executable string) (string, error) {
	canonical, err := canonicalExecutable(executable)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(canonical), fixtureConfigName), nil
}

func canonicalExecutable(path string) (string, error) {
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("resolving phase2 executable: %w", err)
		}
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("phase2 executable must be absolute: %w", errInvalidHarnessReference)
	}
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("resolving phase2 executable: %w", err)
	}
	if filepath.Clean(resolved) != clean {
		return "", fmt.Errorf("phase2 executable must be canonical: %w", errInvalidHarnessReference)
	}
	info, err := os.Stat(clean)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("phase2 executable is not executable: %w", errInvalidHarnessReference)
	}
	return clean, nil
}

func loadHarnessConfig(path string) (loadedHarnessConfig, error) {
	if err := validateCleanAbsolute(path); err != nil {
		return loadedHarnessConfig{}, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return loadedHarnessConfig{}, fmt.Errorf("resolving phase2 fixture config: %w", err)
	}
	if filepath.Clean(resolved) != path {
		return loadedHarnessConfig{}, fmt.Errorf("phase2 fixture config must be canonical: %w", errInvalidHarnessReference)
	}
	data, err := readRegular(path, maxConfigBytes)
	if err != nil {
		return loadedHarnessConfig{}, err
	}
	var cfg HarnessConfig
	if err = task.DecodeStrict(data, &cfg); err != nil {
		return loadedHarnessConfig{}, fmt.Errorf("decoding phase2 fixture config: %w", err)
	}
	if err = validateHarnessConfig(cfg); err != nil {
		return loadedHarnessConfig{}, err
	}
	return loadedHarnessConfig{Config: cfg, Raw: data}, nil
}

func validateHarnessConfig(cfg HarnessConfig) error {
	if cfg.SchemaVersion != 1 {
		return fmt.Errorf("unsupported phase2 fixture schema version %d", cfg.SchemaVersion)
	}
	for _, path := range []string{cfg.ProviderExecutable, cfg.ProviderConfig, cfg.SupervisorExecutable, cfg.EventsDirectory} {
		if err := validateCleanAbsolute(path); err != nil {
			return err
		}
	}
	for _, digest := range []string{cfg.ProviderSHA256, cfg.ProviderConfigSHA256} {
		if err := task.ValidateSHA256(digest); err != nil {
			return err
		}
	}
	if err := validateEnvironment(cfg.Environment); err != nil {
		return err
	}
	if cfg.Hooks.DelayMS < 0 || cfg.Hooks.DelayMS > maxHookDelay.Milliseconds() {
		return errors.New("phase2 fixture hook delay exceeds the finite bound")
	}
	switch cfg.Hooks.Mode {
	case HookNormal, HookStartFailed, HookWaitFailed, HookCaptureFailed, HookAbandonBeforeSeal, HookStartDelayed:
	case HookPublicationDelayed:
		if cfg.Hooks.ReleasePath == "" {
			return errors.New("publication-delayed hook requires an explicit release path")
		}
	default:
		return fmt.Errorf("unsupported phase2 fixture hook mode %q", cfg.Hooks.Mode)
	}
	if cfg.Hooks.ReleasePath != "" {
		if err := validateCleanAbsolute(cfg.Hooks.ReleasePath); err != nil {
			return err
		}
	}
	return nil
}

func validateEnvironment(environment []string) error {
	if environment == nil {
		return errors.New("phase2 fixture environment must be an explicit array")
	}
	seen := make(map[string]bool)
	for _, value := range environment {
		key, _, found := strings.Cut(value, "=")
		if !found || key == "" || strings.ContainsRune(value, '\x00') || seen[key] {
			return errors.New("phase2 fixture environment contains an invalid or duplicate entry")
		}
		seen[key] = true
	}
	return nil
}

func validateCleanAbsolute(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("phase2 fixture path must be clean and absolute: %w", errInvalidHarnessReference)
	}
	return nil
}

func readRegular(path string, limit int64) (data []byte, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("opening phase2 fixture file: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("phase2 fixture file must be regular: %w", errInvalidHarnessReference)
	}
	return task.ReadBounded(f, limit)
}

func hashRegular(path string) (digest string, err error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("opening phase2 provider executable: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("phase2 provider executable must be executable regular file: %w", errInvalidHarnessReference)
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

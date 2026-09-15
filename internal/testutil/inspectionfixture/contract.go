// Package inspectionfixture provides the hermetic native-inspection fixture
// used by the acceptance harness.  It composes the existing phase-two
// provider and command wrappers; the helper below is the only native command
// added by this fixture.
package inspectionfixture

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

const (
	// ConfigName is the fixed sibling configuration name consumed by the
	// delegate and delegate-run fixture wrappers.
	ConfigName = "inspection-fixture.json"

	// HelperRevision identifies the pure projector and helper output contract.
	HelperRevision = "inspection-fixture-v1"

	// SchemaVersion is the only supported fixture configuration schema.
	SchemaVersion = 1

	// MaxDelay bounds the finite helper lifetime.  The acceptance harness uses
	// zero for natural completion and 30 seconds for the running-expiry case.
	MaxDelay = 60 * time.Second

	maxSentinelBytes = 64 * 1024
)

var (
	// Errors crossing the fixture command boundary are deliberately fixed. In
	// particular, helper config and sentinel contents never appear in them.
	ErrInvalidConfig = errors.New("inspection fixture configuration unavailable")
	ErrInvalidHelper = errors.New("inspection fixture helper unavailable")
	ErrHelperOutput  = errors.New("inspection fixture helper output unavailable")
)

// Config is the sibling inspection-fixture.json record. All paths and
// digests are authenticated before a candidate is returned.
type Config struct {
	SchemaVersion      int      `json:"schema_version"`
	HelperExecutable   string   `json:"helper_executable"`
	HelperSHA256       string   `json:"helper_sha256"`
	HelperConfig       string   `json:"helper_config"`
	HelperConfigSHA256 string   `json:"helper_config_sha256"`
	Environment        []string `json:"environment"`
}

// HelperConfig is the separate, case-specific native helper record. The
// helper has no task, session, provider, or supervisor authority.
type HelperConfig struct {
	SchemaVersion int    `json:"schema_version"`
	ArtifactDir   string `json:"artifact_dir"`
	SentinelPath  string `json:"sentinel_path"`
	DelayMS       int64  `json:"delay_ms"`
	Eligible      bool   `json:"eligible"`
}

// loadedConfig retains the exact authenticated bytes for effective-policy
// binding. It is intentionally private so callers cannot mutate the digest
// source independently of validation.
type loadedConfig struct {
	Config Config
	Raw    []byte
	SHA256 string
}

type loadedHelperConfig struct {
	Config HelperConfig
	Raw    []byte
	SHA256 string
}

func loadConfig(path string) (loadedConfig, error) {
	data, err := readCanonicalRegular(path, task.MaxControlRecordSize, false)
	if err != nil {
		return loadedConfig{}, ErrInvalidConfig
	}
	var cfg Config
	if err = task.DecodeStrict(data, &cfg); err != nil {
		return loadedConfig{}, ErrInvalidConfig
	}
	if err = validateConfig(cfg); err != nil {
		return loadedConfig{}, ErrInvalidConfig
	}
	helper, err := loadHelperConfig(cfg.HelperConfig)
	if err != nil || helper.SHA256 != cfg.HelperConfigSHA256 {
		return loadedConfig{}, ErrInvalidConfig
	}
	if helper.Config.SchemaVersion != SchemaVersion {
		return loadedConfig{}, ErrInvalidConfig
	}
	return loadedConfig{Config: cfg, Raw: data, SHA256: task.ComputeSHA256(data)}, nil
}

// LoadConfig reads and strictly validates one sibling configuration. It is
// exported for process-free fixture tests and harness setup checks.
func LoadConfig(path string) (Config, error) {
	loaded, err := loadConfig(path)
	if err != nil {
		return Config{}, err
	}
	return loaded.Config, nil
}

func loadHelperConfig(path string) (loadedHelperConfig, error) {
	data, err := readCanonicalRegular(path, task.MaxControlRecordSize, false)
	if err != nil {
		return loadedHelperConfig{}, ErrInvalidHelper
	}
	var cfg HelperConfig
	if err = task.DecodeStrict(data, &cfg); err != nil {
		return loadedHelperConfig{}, ErrInvalidHelper
	}
	if err = validateHelperConfig(cfg); err != nil {
		return loadedHelperConfig{}, ErrInvalidHelper
	}
	return loadedHelperConfig{Config: cfg, Raw: data, SHA256: task.ComputeSHA256(data)}, nil
}

// LoadHelperConfig reads and strictly validates a case-specific helper
// configuration. The caller must separately bind its bytes to Config.
func LoadHelperConfig(path string) (HelperConfig, error) {
	loaded, err := loadHelperConfig(path)
	if err != nil {
		return HelperConfig{}, err
	}
	return loaded.Config, nil
}

func validateConfig(cfg Config) error {
	if cfg.SchemaVersion != SchemaVersion || task.ValidateSHA256(cfg.HelperSHA256) != nil || task.ValidateSHA256(cfg.HelperConfigSHA256) != nil {
		return ErrInvalidConfig
	}
	if err := validateEnvironment(cfg.Environment); err != nil {
		return err
	}
	if err := validateExistingFile(cfg.HelperExecutable, true); err != nil {
		return err
	}
	if err := validateExistingFile(cfg.HelperConfig, false); err != nil {
		return err
	}
	digest, err := hashRegular(cfg.HelperExecutable)
	if err != nil || digest != cfg.HelperSHA256 {
		return ErrInvalidConfig
	}
	return nil
}

func validateHelperConfig(cfg HelperConfig) error {
	if cfg.SchemaVersion != SchemaVersion || !cfg.Eligible || cfg.DelayMS < 0 || cfg.DelayMS > MaxDelay.Milliseconds() {
		return ErrInvalidHelper
	}
	if err := validateExistingDirectory(cfg.ArtifactDir); err != nil {
		return err
	}
	if err := validateExistingFile(cfg.SentinelPath, false); err != nil {
		return err
	}
	info, err := os.Stat(cfg.SentinelPath)
	if err != nil || info.Size() <= 0 || info.Size() > maxSentinelBytes {
		return ErrInvalidHelper
	}
	return nil
}

func validateEnvironment(environment []string) error {
	if environment == nil {
		return ErrInvalidConfig
	}
	seen := make(map[string]struct{}, len(environment))
	for _, entry := range environment {
		if !utf8.ValidString(entry) || strings.ContainsRune(entry, '\x00') {
			return ErrInvalidConfig
		}
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return ErrInvalidConfig
		}
		if _, exists := seen[key]; exists {
			return ErrInvalidConfig
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateExistingFile(path string, executable bool) error {
	if err := validateCleanAbsolute(path); err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Clean(canonical) != path {
		return ErrInvalidConfig
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ErrInvalidConfig
	}
	if executable && info.Mode().Perm()&0o111 == 0 {
		return ErrInvalidConfig
	}
	return nil
}

func validateExistingDirectory(path string) error {
	if err := validateCleanAbsolute(path); err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || filepath.Clean(canonical) != path {
		return ErrInvalidHelper
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return ErrInvalidHelper
	}
	return nil
}

func validateCleanAbsolute(path string) error {
	if path == "" || !utf8.ValidString(path) || strings.ContainsRune(path, '\x00') || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrInvalidConfig
	}
	return nil
}

func readCanonicalRegular(path string, limit int64, executable bool) ([]byte, error) {
	if err := validateExistingFile(path, executable); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, err := task.ReadBounded(file, limit)
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

func hashRegular(path string) (string, error) {
	data, err := readCanonicalRegular(path, task.MaxControlRecordSize*64, false)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func writeAll(writer io.Writer, data []byte) error {
	if writer == nil {
		return ErrHelperOutput
	}
	for len(data) > 0 {
		n, err := writer.Write(data)
		if n < 0 || n > len(data) {
			return ErrHelperOutput
		}
		data = data[n:]
		if err != nil {
			return ErrHelperOutput
		}
		if n == 0 {
			return ErrHelperOutput
		}
	}
	return nil
}

// ConfigPathForExecutable returns the canonical sibling inspection config
// path. Empty executable selects the current process, matching phase2cli.
func ConfigPathForExecutable(executable string) (string, error) {
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return "", ErrInvalidConfig
		}
	}
	if err := validateExistingFile(executable, true); err != nil {
		return "", ErrInvalidConfig
	}
	return filepath.Join(filepath.Dir(executable), ConfigName), nil
}

// inspectionOutputLimit is deliberately below the shared one-megabyte
// capture ceiling only by schema validation; the core still owns the bound.
const inspectionOutputLimit = commonprovider.MaxInspectionOutput

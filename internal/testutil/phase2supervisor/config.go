package phase2supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// DefaultConfig returns a complete configuration for a finite 4.0.4 client.
// The caller supplies every filesystem path so the fake never consults an
// ambient environment variable or PATH lookup.
func DefaultConfig(configPath, artifactDir, statusPath string) Config {
	return Config{
		SchemaVersion:      SchemaVersion,
		ConfigPath:         configPath,
		ExpectedConfigPath: configPath,
		ArtifactDir:        artifactDir,
		StatusPath:         statusPath,
		Version:            DefaultVersion,
		AddID:              7,
	}
}

// ConfigPathForExecutable returns the fixed config sibling required by the
// compiled fake client.
func ConfigPathForExecutable(executable string) (string, error) {
	canonical, err := canonicalExecutable(executable)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(canonical), ConfigName), nil
}

// WriteConfig validates and atomically writes one explicit fake configuration.
func WriteConfig(path string, cfg Config) error {
	path, err := cleanAbsolute(path)
	if err != nil {
		return err
	}
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = SchemaVersion
	}
	if cfg.ConfigPath == "" {
		cfg.ConfigPath = path
	}
	if cfg.ExpectedConfigPath == "" {
		cfg.ExpectedConfigPath = path
	}
	if cfg.Version == "" {
		cfg.Version = DefaultVersion
	}
	if err = validateConfig(cfg, path); err != nil {
		return err
	}
	data, err := task.MarshalCanonical(cfg)
	if err != nil {
		return fmt.Errorf("marshal fake supervisor config: %w", err)
	}
	if len(data) > MaxConfigBytes {
		return ErrConfigTooBig
	}
	return writeAtomic(path, data, 0o600, MaxConfigBytes)
}

// LoadConfig reads one strict, bounded JSON configuration.
func LoadConfig(path string) (Config, error) {
	path, err := cleanAbsolute(path)
	if err != nil {
		return Config{}, err
	}
	data, err := readBounded(path, MaxConfigBytes)
	if err != nil {
		if errors.Is(err, task.ErrControlRecordTooBig) {
			return Config{}, ErrConfigTooBig
		}
		return Config{}, fmt.Errorf("read fake supervisor config: %w", err)
	}
	if err = task.ValidateJSONStructure(data); err != nil {
		return Config{}, fmt.Errorf("%w: malformed JSON: %w", ErrInvalidConfig, err)
	}
	var cfg Config
	if err = task.DecodeStrict(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("%w: decode JSON: %w", ErrInvalidConfig, err)
	}
	if err = validateConfig(cfg, path); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateConfig(cfg Config, path string) error {
	if cfg.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: unsupported schema version", ErrInvalidConfig)
	}
	if cfg.ConfigPath != path {
		return fmt.Errorf("%w: config_path must equal %s", ErrInvalidConfig, path)
	}
	if err := validateAbsolutePath(cfg.ConfigPath); err != nil {
		return fmt.Errorf("%w: config_path: %w", ErrInvalidConfig, err)
	}
	if err := validateAbsolutePath(cfg.ExpectedConfigPath); err != nil {
		return fmt.Errorf("%w: expected_config_path: %w", ErrInvalidConfig, err)
	}
	if err := validateAbsolutePath(cfg.ArtifactDir); err != nil {
		return fmt.Errorf("%w: artifact_dir: %w", ErrInvalidConfig, err)
	}
	if err := validateAbsolutePath(cfg.StatusPath); err != nil {
		return fmt.Errorf("%w: status_path: %w", ErrInvalidConfig, err)
	}
	if cfg.Version == "" {
		return fmt.Errorf("%w: version must be nonempty", ErrInvalidConfig)
	}
	if cfg.AddID < 0 {
		return fmt.Errorf("%w: add_id must be nonnegative", ErrInvalidConfig)
	}
	for name, verb := range map[string]VerbConfig{
		"status": cfg.Status,
		"add":    cfg.Add,
		"kill":   cfg.Kill,
		"remove": cfg.Remove,
	} {
		if err := validateVerb(name, verb); err != nil {
			return err
		}
	}
	return nil
}

func validateVerb(name string, verb VerbConfig) error {
	if verb.Delay < 0 || verb.Delay > MaxRunDuration {
		return fmt.Errorf("%w: %s delay is outside finite limit", ErrInvalidConfig, name)
	}
	if verb.ExitCode < 0 || verb.ExitCode > 255 {
		return fmt.Errorf("%w: %s exit_code is outside process range", ErrInvalidConfig, name)
	}
	for field, path := range map[string]string{
		"release_path": verb.ReleasePath,
		"stdout_path":  verb.StdoutPath,
		"stderr_path":  verb.StderrPath,
	} {
		if path != "" {
			if err := validateAbsolutePath(path); err != nil {
				return fmt.Errorf("%w: %s %s: %w", ErrInvalidConfig, name, field, err)
			}
		}
	}
	return nil
}

func cleanAbsolute(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("%w: path must be absolute and clean", ErrInvalidConfig)
	}
	return path, nil
}

func validateAbsolutePath(path string) error {
	_, err := cleanAbsolute(path)
	return err
}

func canonicalExecutable(path string) (string, error) {
	path, err := cleanAbsolute(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%w: executable cannot be resolved: %w", ErrInvalidConfig, err)
	}
	if canonical != path {
		return "", fmt.Errorf("%w: executable must be canonical", ErrInvalidConfig)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%w: executable cannot be statted: %w", ErrInvalidConfig, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: executable must be a regular executable", ErrInvalidConfig)
	}
	return path, nil
}

func readBounded(path string, max int64) ([]byte, error) {
	f, err := openBounded(path)
	if err != nil {
		return nil, err
	}
	data, readErr := task.ReadBounded(f, max)
	return data, errors.Join(readErr, f.Close())
}

func openBounded(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return nil, errors.Join(fmt.Errorf("%w: failed to wrap bounded input", ErrInvalidConfig), unix.Close(fd))
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("%w: bounded input must be a regular file", ErrInvalidConfig), file.Close())
	}
	return file, nil
}

func readOutput(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	data, err := readBounded(path, MaxOutputBytes)
	if errors.Is(err, task.ErrControlRecordTooBig) {
		return nil, ErrOutputTooBig
	}
	return data, err
}

func waitForInput(path string, delay, max time.Duration) error {
	deadline := time.Now().Add(max)
	if delay > 0 {
		timer := time.NewTimer(delay)
		<-timer.C
	}
	if path == "" {
		return nil
	}
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ErrReleaseTimeout
		}
		interval := 10 * time.Millisecond
		if remaining < interval {
			interval = remaining
		}
		timer := time.NewTimer(interval)
		<-timer.C
	}
}

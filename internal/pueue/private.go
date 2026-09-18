package pueue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const privateSupervisorDirectory = ".supervisor"

// BindPrivate creates or reuses a supervisor owned by one Delegation Layer
// state root. The daemon is started only when the private endpoint is not
// ready; the caller never owns provider process lifetime.
func BindPrivate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options) (client *Client, resultErr error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return nil, fmt.Errorf("%w: private supervisor root must be canonical and absolute", ErrConfiguration)
	}
	options, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	resolution, err := privateResolution(options)
	if err != nil {
		return nil, err
	}
	clientPath, err := canonicalExecutable(clientExecutable)
	if err != nil {
		return nil, err
	}
	daemonPath, err := canonicalExecutable(daemonExecutable)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(stateRoot, privateSupervisorDirectory)
	if err = ensurePrivateBase(base); err != nil {
		return nil, err
	}
	lock, err := acquireBootstrapLock(ctx, filepath.Join(base, "bootstrap.lock"))
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeBootstrapLock(lock)) }()
	configPath, base, err := ensurePrivateConfig(stateRoot, resolution)
	if err != nil {
		return nil, err
	}
	client, err = Bind(ctx, clientPath, configPath, options)
	if err != nil {
		return nil, err
	}
	if err = client.Ready(ctx); err == nil {
		return client, nil
	}
	if err = startDaemon(daemonPath, configPath, base, options); err != nil {
		return nil, err
	}
	if err = waitReady(ctx, client, options.ObservationTimeout); err != nil {
		return nil, fmt.Errorf("%w: private pueued did not become ready: %w", ErrBinding, err)
	}
	return client, nil
}

func privateResolution(options Options) (ResolutionContext, error) {
	current, err := resolutionContext(environmentForCommand(options.Environment))
	if err != nil {
		return ResolutionContext{}, err
	}
	if options.Resolution == nil {
		return current, nil
	}
	resolution := *options.Resolution
	if err := validateResolutionContext(resolution); err != nil {
		return ResolutionContext{}, err
	}
	if !sameEnvironmentResolution(resolution, current) {
		return ResolutionContext{}, fmt.Errorf("%w: resolution does not match private supervisor environment", ErrConfiguration)
	}
	return resolution, nil
}

func ensurePrivateConfig(stateRoot string, resolution ResolutionContext) (configPath, base string, resultErr error) {
	base = filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateBase(base); err != nil {
		return "", "", err
	}
	configPath = filepath.Join(base, "pueue.yml")
	if err := rejectSymlinkComponents(configPath); err != nil {
		return "", "", err
	}
	if _, err := os.Stat(configPath); err == nil {
		if validateErr := validatePrivateConfig(configPath, base, resolution); validateErr != nil {
			return "", "", validateErr
		}
		return configPath, base, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("%w: inspect private supervisor config: %w", ErrConfiguration, err)
	}

	data := privateConfigYAML(base)
	file, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			if validateErr := validatePrivateConfig(configPath, base, resolution); validateErr != nil {
				return "", "", validateErr
			}
			return configPath, base, nil
		}
		return "", "", fmt.Errorf("%w: create private supervisor config: %w", ErrConfiguration, err)
	}
	if _, err = file.WriteString(data); err != nil {
		return "", "", errors.Join(
			fmt.Errorf("%w: write private supervisor config: %w", ErrConfiguration, err),
			file.Close(),
		)
	}
	if err = file.Sync(); err != nil {
		return "", "", errors.Join(
			fmt.Errorf("%w: sync private supervisor config: %w", ErrConfiguration, err),
			file.Close(),
		)
	}
	if err = file.Close(); err != nil {
		return "", "", fmt.Errorf("%w: close private supervisor config: %w", ErrConfiguration, err)
	}
	if err = syncPrivateDirectory(base); err != nil {
		return "", "", err
	}
	if err = validatePrivateConfig(configPath, base, resolution); err != nil {
		return "", "", err
	}
	return configPath, base, nil
}

func ensurePrivateBase(base string) error {
	if err := rejectSymlinkComponents(base); err != nil {
		return err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return fmt.Errorf("%w: create private supervisor directory: %w", ErrConfiguration, err)
	}
	if err := rejectSymlinkComponents(base); err != nil {
		return err
	}
	return nil
}

func syncPrivateDirectory(path string) (resultErr error) {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open private supervisor directory barrier: %w", ErrConfiguration, err)
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("%w: sync private supervisor directory: %w", ErrConfiguration, err)
	}
	return nil
}

func validatePrivateConfig(configPath, base string, resolution ResolutionContext) error {
	data, err := readRegular(configPath, MaxControlBytes)
	if err != nil {
		return fmt.Errorf("%w: read private supervisor config: %w", ErrConfiguration, err)
	}
	config, err := ParseConfig(data)
	if err != nil {
		return err
	}
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		return err
	}
	if err = resolved.ValidateIsolation(base); err != nil {
		return err
	}
	for _, directory := range []string{resolved.PueueDirectory, resolved.RuntimeDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("%w: create private supervisor data: %w", ErrConfiguration, err)
		}
		if err := rejectSymlinkComponents(directory); err != nil {
			return err
		}
	}
	return nil
}

func privateConfigYAML(base string) string {
	quote := strconv.Quote
	data := filepath.Join(base, "data")
	runtimeDirectory := filepath.Join(base, "run")
	return strings.Join([]string{
		"client:",
		"  show_confirmation_questions: false",
		"shared:",
		"  pueue_directory: " + quote(data),
		"  runtime_directory: " + quote(runtimeDirectory),
		"  alias_file: " + quote(filepath.Join(base, "aliases.yml")),
		"  use_unix_socket: true",
		"  unix_socket_path: " + quote(filepath.Join(runtimeDirectory, "pueue.sock")),
		"  unix_socket_permissions: 448",
		"  pid_path: " + quote(filepath.Join(runtimeDirectory, "pueue.pid")),
		"  shared_secret_path: " + quote(filepath.Join(data, "shared_secret")),
		"daemon:",
		"  shell_command:",
		"    - \"/bin/sh\"",
		"    - \"-c\"",
		"    - \"{{ pueue_command_string }}\"",
		"",
	}, "\n")
}

func startDaemon(executable, configPath, base string, options Options) error {
	cmd := exec.Command(executable, "-c", configPath)
	cmd.Dir = base
	cmd.Env = environmentForCommand(options.Environment)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: start private pueued: %w", ErrBinding, err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("%w: release private pueued process: %w", ErrBinding, err)
	}
	return nil
}

func waitReady(ctx context.Context, client *Client, timeout time.Duration) error {
	if timeout <= 0 {
		return ErrConfiguration
	}
	readyContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := client.Ready(readyContext); err == nil {
			return nil
		} else {
			lastErr = err
			if pending := pendingFrom(err); pending != nil {
				select {
				case <-pending.Done():
				case <-readyContext.Done():
					return errors.Join(lastErr, readyContext.Err())
				}
			}
		}
		select {
		case <-readyContext.Done():
			return errors.Join(lastErr, readyContext.Err())
		case <-ticker.C:
		}
	}
}

type bootstrapLock struct {
	file *os.File
}

func acquireBootstrapLock(ctx context.Context, path string) (*bootstrapLock, error) {
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: open supervisor bootstrap lock: %w", ErrBinding, err)
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &bootstrapLock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, errors.Join(fmt.Errorf("%w: acquire supervisor bootstrap lock: %w", ErrBinding, err), file.Close())
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), file.Close())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func closeBootstrapLock(lock *bootstrapLock) error {
	if lock == nil || lock.file == nil {
		return nil
	}
	return errors.Join(
		unix.Flock(int(lock.file.Fd()), unix.LOCK_UN),
		lock.file.Close(),
	)
}

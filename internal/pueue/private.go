package pueue

import (
	"bytes"
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

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

const privateSupervisorDirectory = ".supervisor"

const privateSupervisorPendingGrace = time.Second

// PrivateConfigPath returns the canonical config path owned by a state root.
func PrivateConfigPath(stateRoot string) string {
	return filepath.Join(stateRoot, privateSupervisorDirectory, "pueue.yml")
}

// IsPrivateConfig reports whether configPath is the state-rooted private
// supervisor config used by the released CLI.
func IsPrivateConfig(stateRoot, configPath string) bool {
	return filepath.IsAbs(stateRoot) && filepath.Clean(stateRoot) == stateRoot && configPath == PrivateConfigPath(stateRoot)
}

// BindPrivate creates or reuses a supervisor owned by one Delegation Layer
// state root. The daemon is started only when the private endpoint is not
// ready; the caller never owns provider process lifetime.
func BindPrivate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options) (client *Client, resultErr error) {
	return bindPrivate(ctx, clientExecutable, daemonExecutable, stateRoot, options, nil)
}

func bindPrivate(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options, expected *task.SupervisorRef) (client *Client, resultErr error) {
	options, resolution, clientPath, daemonPath, base, lock, err := preparePrivateBootstrap(ctx, clientExecutable, daemonExecutable, stateRoot, options)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeBootstrapLock(lock)) }()
	configPath, base, err := privateBindingConfig(stateRoot, base, resolution, expected)
	if err != nil {
		return nil, err
	}
	if err = verifyExpectedPrivateBinding(ctx, clientPath, daemonPath, configPath, options, expected); err != nil {
		return nil, err
	}
	client, ready, err := bindPrivateClient(ctx, clientPath, daemonPath, configPath, options)
	if err != nil {
		return nil, err
	}
	if err = matchExpectedPrivateBinding(client, expected); err != nil {
		return nil, err
	}
	if ready {
		if err = ensurePrivateDaemonIdentity(base, client.Binding(), expected); err != nil {
			return nil, err
		}
		return client, nil
	}
	if err = verifyExpectedPrivateBinding(ctx, clientPath, daemonPath, configPath, options, expected); err != nil {
		return nil, err
	}
	if identityErr := checkPrivateDaemonIdentity(base, client.Binding()); identityErr != nil && !errors.Is(identityErr, os.ErrNotExist) {
		return nil, identityErr
	}
	if err = startDaemon(daemonPath, configPath, base, options); err != nil {
		return nil, err
	}
	if err = waitReady(ctx, client, options.ObservationTimeout); err != nil {
		return nil, fmt.Errorf("%w: private pueued did not become ready: %w", ErrBinding, err)
	}
	if err = publishPrivateDaemonIdentity(base, client.Binding()); err != nil {
		return nil, err
	}
	return client, nil
}

func preparePrivateBootstrap(ctx context.Context, clientExecutable, daemonExecutable, stateRoot string, options Options) (normalized Options, resolution ResolutionContext, clientPath, daemonPath, base string, lock *bootstrapLock, resultErr error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return Options{}, ResolutionContext{}, "", "", "", nil, fmt.Errorf("%w: private supervisor root must be canonical and absolute", ErrConfiguration)
	}
	normalized, err := normalizeOptions(options)
	if err != nil {
		return Options{}, ResolutionContext{}, "", "", "", nil, err
	}
	resolution, err = privateResolution(normalized)
	if err != nil {
		return Options{}, ResolutionContext{}, "", "", "", nil, err
	}
	clientPath, err = canonicalExecutable(clientExecutable)
	if err != nil {
		return Options{}, ResolutionContext{}, "", "", "", nil, err
	}
	daemonPath, err = canonicalExecutable(daemonExecutable)
	if err != nil {
		return Options{}, ResolutionContext{}, "", "", "", nil, err
	}
	base = filepath.Join(stateRoot, privateSupervisorDirectory)
	if err = ensurePrivateBase(base); err != nil {
		return Options{}, ResolutionContext{}, "", "", "", nil, err
	}
	lock, err = acquireBootstrapLock(ctx, filepath.Join(base, "bootstrap.lock"))
	if err != nil {
		return Options{}, ResolutionContext{}, "", "", "", nil, err
	}
	return normalized, resolution, clientPath, daemonPath, base, lock, nil
}

func privateBindingConfig(stateRoot, base string, resolution ResolutionContext, expected *task.SupervisorRef) (configPath, privateBase string, resultErr error) {
	if expected == nil {
		return ensurePrivateConfig(stateRoot, resolution)
	}
	if !IsPrivateConfig(stateRoot, expected.ConfigPath) {
		return "", "", fmt.Errorf("%w: saved supervisor config is not private to the state root", ErrConfiguration)
	}
	// Recovery must fail closed when the saved config is absent or changed; it
	// must never recreate a default config before checking the saved binding,
	// because that could start a different queue.
	configPath = expected.ConfigPath
	if err := rejectSymlinkComponents(configPath); err != nil {
		return "", "", err
	}
	if _, err := os.Stat(configPath); err != nil {
		return "", "", fmt.Errorf("%w: saved private supervisor config is unavailable: %w", ErrBinding, err)
	}
	if err := validatePrivateConfig(configPath, base, resolution); err != nil {
		return "", "", err
	}
	return configPath, base, nil
}

func verifyExpectedPrivateBinding(ctx context.Context, clientPath, daemonPath, configPath string, options Options, expected *task.SupervisorRef) error {
	if expected == nil {
		return nil
	}
	bound, err := Bind(ctx, clientPath, configPath, options)
	if err != nil {
		joinPendingWithin(pendingFrom(err), privateSupervisorPendingGrace)
		return err
	}
	if err := attachPrivateDaemon(bound, daemonPath); err != nil {
		return err
	}
	return matchExpectedPrivateBinding(bound, expected)
}

func matchExpectedPrivateBinding(client *Client, expected *task.SupervisorRef) error {
	if expected != nil && client.Binding() != *expected {
		return ErrBinding
	}
	return nil
}

// RecoverPrivate verifies the saved private binding and restarts its daemon
// when the private endpoint is unavailable. It returns a client retaining the
// saved binding so subsequent operations continue to verify fresh authority.
func RecoverPrivate(ctx context.Context, stateRoot string, saved task.SupervisorRef, options Options) (*Client, error) {
	if !IsPrivateConfig(stateRoot, saved.ConfigPath) {
		return nil, fmt.Errorf("%w: supervisor config is not private to the state root", ErrConfiguration)
	}
	if err := validatePrivateSupervisorRef(saved); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	bound, err := bindPrivate(ctx, saved.ClientExecutable, saved.DaemonExecutable, stateRoot, options, &saved)
	if err != nil {
		return nil, err
	}
	if bound.Binding() != saved {
		return nil, ErrBinding
	}
	return NewClient(saved, options)
}

func bindPrivateClient(ctx context.Context, clientPath, daemonPath, configPath string, options Options) (*Client, bool, error) {
	client, err := Bind(ctx, clientPath, configPath, options)
	if err != nil {
		joinPendingWithin(pendingFrom(err), privateSupervisorPendingGrace)
		return nil, false, err
	}
	if err = attachPrivateDaemon(client, daemonPath); err != nil {
		return nil, false, err
	}
	ready, err := privateClientReady(ctx, client)
	if err != nil {
		return nil, false, err
	}
	return client, ready, nil
}

func validatePrivateSupervisorRef(ref task.SupervisorRef) error {
	if err := task.ValidateFreshSupervisorRef(ref); err != nil {
		return err
	}
	if ref.DaemonExecutable == "" {
		return errors.New("missing saved private supervisor daemon executable")
	}
	if err := task.ValidateSHA256(ref.DaemonSHA256); err != nil {
		return err
	}
	return nil
}

func attachPrivateDaemon(client *Client, daemonPath string) error {
	if client == nil {
		return ErrBinding
	}
	resolved, err := canonicalExecutable(daemonPath)
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	digest, err := hashExecutable(resolved)
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	client.binding.DaemonExecutable = resolved
	client.binding.DaemonSHA256 = digest
	return nil
}

type privateDaemonIdentity struct {
	Executable string `json:"executable"`
	SHA256     string `json:"sha256"`
}

func privateDaemonIdentityPath(base string) string {
	return filepath.Join(base, "daemon.identity.json")
}

func checkPrivateDaemonIdentity(base string, binding task.SupervisorRef) error {
	data, err := readRegular(privateDaemonIdentityPath(base), MaxControlBytes)
	if err != nil {
		return err
	}
	var identity privateDaemonIdentity
	if err = task.DecodeStrict(data, &identity); err != nil {
		return errors.Join(ErrBinding, err)
	}
	if identity.Executable != binding.DaemonExecutable || identity.SHA256 != binding.DaemonSHA256 {
		return ErrBinding
	}
	return nil
}

func ensurePrivateDaemonIdentity(base string, binding task.SupervisorRef, expected *task.SupervisorRef) error {
	err := checkPrivateDaemonIdentity(base, binding)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if expected == nil || expected.DaemonExecutable != binding.DaemonExecutable || expected.DaemonSHA256 != binding.DaemonSHA256 {
		return fmt.Errorf("%w: private daemon identity is not persisted", ErrBinding)
	}
	return publishPrivateDaemonIdentity(base, binding)
}

func publishPrivateDaemonIdentity(base string, binding task.SupervisorRef) error {
	data, err := task.MarshalCanonical(privateDaemonIdentity{Executable: binding.DaemonExecutable, SHA256: binding.DaemonSHA256})
	if err != nil {
		return fmt.Errorf("%w: marshal private daemon identity: %w", ErrConfiguration, err)
	}
	return publishPrivateRecord(base, privateDaemonIdentityPath(base), data)
}

func publishPrivateRecord(base, destination string, data []byte) (resultErr error) {
	file, stagePath, err := createPrivateStage(base, ".private-record-")
	if err != nil {
		return err
	}
	defer func() {
		if stagePath != "" {
			resultErr = errors.Join(resultErr, os.Remove(stagePath))
		}
	}()
	written, writeErr := file.Write(data)
	if writeErr != nil || written != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return errors.Join(fmt.Errorf("%w: write private record: %w", ErrConfiguration, writeErr), file.Close())
	}
	if err = privateBarrierFile(file); err != nil {
		return errors.Join(fmt.Errorf("%w: sync private record: %w", ErrConfiguration, err), file.Close())
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("%w: close private record: %w", ErrConfiguration, err)
	}
	if err = os.Link(stagePath, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: publish private record: %w", ErrConfiguration, err)
		}
		existing, readErr := readRegular(destination, MaxControlBytes)
		if readErr != nil {
			return errors.Join(ErrBinding, readErr)
		}
		if !bytes.Equal(existing, data) {
			return ErrBinding
		}
		if removeErr := os.Remove(stagePath); removeErr != nil {
			return fmt.Errorf("%w: discard private record stage: %w", ErrConfiguration, removeErr)
		}
		stagePath = ""
		return syncPrivateDirectory(base)
	}
	if err = syncPrivateDirectory(base); err != nil {
		return err
	}
	if err = os.Remove(stagePath); err != nil {
		return fmt.Errorf("%w: remove private record stage: %w", ErrConfiguration, err)
	}
	stagePath = ""
	return syncPrivateDirectory(base)
}

func privateClientReady(ctx context.Context, client *Client) (bool, error) {
	result, err := client.command(ctx, nil, "status", "--json")
	if pending := pendingFrom(err); pending != nil {
		if !joinPendingWithin(pending, privateSupervisorPendingGrace) {
			return false, err
		}
		if ctx.Err() != nil {
			return false, errors.Join(err, ctx.Err())
		}
		var done bool
		result, done = pending.Result()
		if !done {
			return false, err
		}
		err = result.Err
	}
	if err != nil {
		// A normally completed nonzero status command means the endpoint is
		// unavailable and can be bootstrapped. A started command that returned
		// success but failed schema validation is a reachable incompatible
		// supervisor and must be surfaced without starting a second daemon.
		if result.Started && result.ExitCode != 0 {
			return false, nil
		}
		return false, err
	}
	if err = client.validateReadyResult(result); err != nil {
		return false, err
	}
	return true, nil
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

	if err := createPrivateConfig(configPath, base, privateConfigYAML(base)); err != nil {
		return "", "", err
	}
	if err := validatePrivateConfig(configPath, base, resolution); err != nil {
		return "", "", err
	}
	return configPath, base, nil
}

func createPrivateConfig(configPath, base, data string) (resultErr error) {
	file, stagePath, err := createPrivateConfigStage(base)
	if err != nil {
		return err
	}
	defer func() {
		if stagePath != "" {
			resultErr = errors.Join(resultErr, os.Remove(stagePath))
		}
	}()

	written, writeErr := file.WriteString(data)
	if writeErr != nil || written != len(data) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return errors.Join(
			fmt.Errorf("%w: write staged private supervisor config: %w", ErrConfiguration, writeErr),
			file.Close(),
		)
	}
	if err = privateBarrierFile(file); err != nil {
		return errors.Join(
			fmt.Errorf("%w: sync staged private supervisor config: %w", ErrConfiguration, err),
			file.Close(),
		)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("%w: close staged private supervisor config: %w", ErrConfiguration, err)
	}

	if err = os.Link(stagePath, configPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: publish private supervisor config: %w", ErrConfiguration, err)
		}
		if removeErr := os.Remove(stagePath); removeErr != nil {
			return errors.Join(
				fmt.Errorf("%w: discard competing private supervisor config: %w", ErrConfiguration, removeErr),
				err,
			)
		}
		stagePath = ""
		return syncPrivateDirectory(base)
	}
	if err = syncPrivateDirectory(base); err != nil {
		return err
	}
	if err = os.Remove(stagePath); err != nil {
		return fmt.Errorf("%w: remove staged private supervisor config: %w", ErrConfiguration, err)
	}
	stagePath = ""
	return syncPrivateDirectory(base)
}

func createPrivateConfigStage(base string) (*os.File, string, error) {
	return createPrivateStage(base, ".pueue.yml-")
}

func createPrivateStage(base, prefix string) (*os.File, string, error) {
	for range 4 {
		id, err := task.NewRandomID()
		if err != nil {
			return nil, "", fmt.Errorf("%w: create staged private supervisor config: %w", ErrConfiguration, err)
		}
		stagePath := filepath.Join(base, prefix+id)
		file, err := os.OpenFile(stagePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("%w: create staged private supervisor config: %w", ErrConfiguration, err)
		}
		return file, stagePath, nil
	}
	return nil, "", fmt.Errorf("%w: create unique staged private supervisor config: %w", ErrConfiguration, os.ErrExist)
}

func ensurePrivateBase(base string) error {
	if err := rejectSymlinkComponents(base); err != nil {
		return err
	}
	missing, err := missingPrivateDirectories(base)
	if err != nil {
		return err
	}
	for _, directory := range missing {
		if err := createPrivateDirectory(directory); err != nil {
			return err
		}
	}
	if err := rejectSymlinkComponents(base); err != nil {
		return err
	}
	return nil
}

func missingPrivateDirectories(base string) ([]string, error) {
	var missing []string
	current := base
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return nil, fmt.Errorf("%w: private supervisor path is not a directory", ErrConfiguration)
			}
			return missing, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: inspect private supervisor directory: %w", ErrConfiguration, err)
		}
		missing = append([]string{current}, missing...)
		parent := filepath.Dir(current)
		if parent == current {
			return missing, nil
		}
		current = parent
	}
}

func createPrivateDirectory(directory string) error {
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: create private supervisor directory: %w", ErrConfiguration, err)
	}
	if err := privateBarrierDir(directory); err != nil {
		return fmt.Errorf("%w: barrier private supervisor directory: %w", ErrConfiguration, err)
	}
	if err := privateBarrierDir(filepath.Dir(directory)); err != nil {
		return fmt.Errorf("%w: barrier private supervisor parent: %w", ErrConfiguration, err)
	}
	return nil
}

func syncPrivateDirectory(path string) error {
	if err := privateBarrierDir(path); err != nil {
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
				if !joinPendingWithin(pending, privateSupervisorPendingGrace) {
					return errors.Join(lastErr, readyContext.Err())
				}
				result, _ := pending.Result()
				return errors.Join(lastErr, readyContext.Err(), result.Err)
			}
		}
		select {
		case <-readyContext.Done():
			return errors.Join(lastErr, readyContext.Err())
		case <-ticker.C:
		}
	}
}

func joinPendingWithin(pending *Pending, grace time.Duration) bool {
	if pending == nil {
		return true
	}
	if grace <= 0 {
		select {
		case <-pending.Done():
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-pending.Done():
		return true
	case <-timer.C:
		return false
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

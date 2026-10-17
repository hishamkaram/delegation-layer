package control

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

func ensurePrivateConfig(stateRoot string, resolution ResolutionContext) (configPath, base string, resultErr error) {
	base = filepath.Join(stateRoot, privateSupervisorDirectory)
	if err := ensurePrivateBase(base); err != nil {
		return "", "", err
	}
	if err := ensurePrivateLockFile(base, privateTaskObservationLockName); err != nil {
		return "", "", err
	}
	configPath = filepath.Join(base, "pueue.yml")
	if err := rejectSymlinkComponents(configPath); err != nil {
		return "", "", err
	}
	if _, err := os.Stat(configPath); err == nil {
		if err = validateExistingPrivateConfig(stateRoot, configPath, base, resolution); err != nil {
			return "", "", err
		}
		return configPath, base, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("%w: inspect private supervisor config: %w", ErrConfiguration, err)
	}
	configSource, err := privateConfigYAMLForResolution(base, resolution)
	if err != nil {
		return "", "", err
	}

	// Record bootstrap intent before exposing the config. If the process dies
	// after this point, the next private bind can distinguish its own partial
	// bootstrap from an unowned config-only state root.
	if err := markPrivateBootstrapPending(stateRoot); err != nil {
		return "", "", err
	}
	if err := createPrivateConfig(configPath, base, configSource); err != nil {
		return "", "", err
	}
	if err := validatePrivateConfig(configPath, base, resolution); err != nil {
		return "", "", err
	}
	return configPath, base, nil
}

func validateExistingPrivateConfig(stateRoot, configPath, base string, resolution ResolutionContext) error {
	if validateErr := validatePrivateConfig(configPath, base, resolution); validateErr != nil {
		repaired, repairErr := repairIncompletePrivateConfig(stateRoot, configPath, base, resolution, validateErr)
		if repairErr != nil {
			return repairErr
		}
		if !repaired {
			return validateErr
		}
		if validateErr = validatePrivateConfig(configPath, base, resolution); validateErr != nil {
			return validateErr
		}
	}
	pending, err := PrivateBootstrapPending(stateRoot)
	if err != nil || !pending {
		return err
	}
	// A replacement rename may have succeeded before its final directory
	// barrier reported failure. Re-establish durability before launch.
	return syncPrivateDirectory(base)
}

func repairIncompletePrivateConfig(stateRoot, configPath, base string, resolution ResolutionContext, validationErr error) (bool, error) {
	if !errors.Is(validationErr, errUnixSocketPathTooLong) {
		return false, nil
	}
	pending, err := PrivateBootstrapPending(stateRoot)
	if err != nil || !pending {
		return false, err
	}
	current, err := readRegular(configPath, MaxControlBytes)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(current, []byte(privateConfigYAML(base))) {
		return false, nil
	}
	authorized, err := incompletePrivateConfigRepairAuthorized(stateRoot, base)
	if err != nil || !authorized {
		return false, err
	}
	replacement, err := privateConfigYAMLForResolution(base, resolution)
	if err != nil {
		return false, err
	}
	if bytes.Equal(current, []byte(replacement)) {
		return false, nil
	}
	if err = replacePrivateRecordAtomic(
		base,
		configPath,
		".private-config-",
		"incomplete private supervisor config",
		[]byte(replacement),
		true,
	); err != nil {
		return false, err
	}
	return true, nil
}

func incompletePrivateConfigRepairAuthorized(stateRoot, base string) (bool, error) {
	generationAbsent, err := incompletePrivateConfigGenerationAbsent(stateRoot, base)
	if err != nil || !generationAbsent {
		return false, err
	}
	recoveryAbsent, err := incompletePrivateConfigRecoveryAbsent(stateRoot)
	if err != nil || !recoveryAbsent {
		return false, err
	}
	return incompletePrivateConfigRuntimeAbsent(base)
}

func incompletePrivateConfigGenerationAbsent(stateRoot, base string) (bool, error) {
	if _, found, err := loadPrivateDaemonIdentity(base); err != nil || found {
		return false, err
	}
	if _, found, err := loadPrivateSupervisorBinding(stateRoot); err != nil || found {
		return false, err
	}
	if pending, err := PrivateSupervisorMigrationPending(stateRoot); err != nil || pending {
		return false, err
	}
	if _, present, err := readPrivateIdentityPublication(stateRoot); err != nil || present {
		return false, err
	}
	if _, present, err := loadPrivateMigrationHistory(base); err != nil || present {
		return false, err
	}
	return true, nil
}

func incompletePrivateConfigRecoveryAbsent(stateRoot string) (bool, error) {
	if present, err := PrivateRecoveryBindingPresent(stateRoot); err != nil || present {
		return false, err
	}
	if present, err := privateRecoveryAliasArtifactsPresent(stateRoot); err != nil || present {
		return false, err
	}
	if _, err := readRegular(privateSchedulingEvidencePath(stateRoot), MaxControlBytes); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

func incompletePrivateConfigRuntimeAbsent(base string) (bool, error) {
	// The legacy endpoint and PID both live under run. Requiring that whole
	// directory to be absent also rejects unrecognized runtime residue without
	// following a symlinked component. Data presence likewise proves that setup
	// advanced beyond the pre-start config publication repaired here.
	for _, path := range []string{
		filepath.Join(base, "s"),
		filepath.Join(base, "run"),
		filepath.Join(base, "data"),
	} {
		if _, err := os.Lstat(path); err == nil {
			return false, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return true, nil
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
		id, err := randomid.NewRandomID()
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
	// Auxiliary locks belong to their own mutation paths. A shared probe must
	// never create them: the transition election and task-status observation
	// lock are provisioned after lifecycle ownership is elected, and terminal
	// event state only when a ticket or event is written.
	if err := ensurePrivateLockFile(base, "bootstrap.lock"); err != nil {
		return err
	}
	return nil
}

func ensurePrivateLockFile(base, name string) (resultErr error) {
	path := filepath.Join(base, name)
	for {
		present, err := privateLockFilePresent(base, name)
		if err != nil {
			return err
		}
		if present {
			return nil
		}
		file, createErr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(createErr, os.ErrExist) {
			continue
		}
		if createErr != nil {
			return fmt.Errorf("%w: create private supervisor lock: %w", ErrConfiguration, createErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return errors.Join(fmt.Errorf("%w: close private supervisor lock: %w", ErrConfiguration, closeErr), os.Remove(path))
		}
		return syncPrivateDirectory(base)
	}
}

func privateLockFilePresent(base, name string) (bool, error) {
	if name == "" || filepath.Base(name) != name {
		return false, ErrConfiguration
	}
	path := filepath.Join(base, name)
	if err := rejectSymlinkComponents(path); err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect private supervisor lock: %w", ErrConfiguration, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return false, fmt.Errorf("%w: private supervisor lock is not a protected regular file", ErrConfiguration)
	}
	return true, nil
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

func readPrivateConfig(configPath, base string, resolution ResolutionContext) (ResolvedConfig, error) {
	data, err := readRegular(configPath, MaxControlBytes)
	if err != nil {
		return ResolvedConfig{}, fmt.Errorf("%w: read private supervisor config: %w", ErrConfiguration, err)
	}
	return resolvePrivateConfigSource(data, base, resolution)
}

func resolvePrivateConfigSource(data []byte, base string, resolution ResolutionContext) (ResolvedConfig, error) {
	config, err := ParseConfig(data)
	if err != nil {
		return ResolvedConfig{}, err
	}
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		return ResolvedConfig{}, err
	}
	if err = resolved.ValidateIsolation(base); err != nil {
		return ResolvedConfig{}, err
	}
	return resolved, nil
}

func validatePrivateConfig(configPath, base string, resolution ResolutionContext) error {
	resolved, err := readPrivateConfig(configPath, base, resolution)
	if err != nil {
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
	return privateConfigYAMLWithSocket(base, filepath.Join(base, "run", "pueue.sock"))
}

func privateConfigYAMLForResolution(base string, resolution ResolutionContext) (string, error) {
	limit, ok := unixSocketPathLimit(resolution.OS)
	if !ok {
		return "", fmt.Errorf("%w: unsupported platform", ErrConfiguration)
	}
	socketPath := filepath.Join(base, "run", "pueue.sock")
	if len(socketPath) > limit {
		socketPath = filepath.Join(base, "s")
	}
	data := privateConfigYAMLWithSocket(base, socketPath)
	if _, err := resolvePrivateConfigSource([]byte(data), base, resolution); err != nil {
		return "", err
	}
	return data, nil
}

func privateConfigYAMLWithSocket(base, socketPath string) string {
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
		"  unix_socket_path: " + quote(socketPath),
		"  unix_socket_permissions: 448",
		"  pid_path: " + quote(filepath.Join(runtimeDirectory, "pueue.pid")),
		"  shared_secret_path: " + quote(filepath.Join(data, "shared_secret")),
		"daemon:",
		"  callback: " + quote(privateTerminalEventCallback(base)),
		"  shell_command:",
		"    - \"/bin/sh\"",
		"    - \"-c\"",
		"    - \"{{ pueue_command_string }}\"",
		"",
	}, "\n")
}

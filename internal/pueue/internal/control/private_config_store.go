package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/task/randomid"
)

func ensurePrivateConfigContext(ctx context.Context, stateRoot string, resolution ResolutionContext, guard PrivateGenerationGuard, callbackRunner ...string) (configPath, base string, resultErr error) {
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
		if err = validateExistingPrivateConfig(ctx, stateRoot, configPath, base, resolution, guard); err != nil {
			return "", "", err
		}
		return configPath, base, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("%w: inspect private supervisor config: %w", ErrConfiguration, err)
	}
	configSource, err := privateConfigYAMLForResolution(base, resolution, callbackRunner...)
	if err != nil {
		return "", "", err
	}

	// Record bootstrap intent before exposing the config. If the process dies
	// after this point, the next private bind can distinguish its own partial
	// bootstrap from an unowned config-only state root.
	if err := guardPrivateGeneration(ctx, stateRoot, guard, func() error {
		absent, err := incompletePrivateConfigGenerationAbsent(stateRoot, base)
		if err != nil {
			return privateRecoveryFailure("state_unreadable", errors.Join(ErrBinding, err))
		}
		if !absent {
			return privateRecoveryFailure("state_unreadable", ErrBinding)
		}
		if err := markPrivateBootstrapPending(stateRoot); err != nil {
			return err
		}
		return createPrivateConfig(configPath, base, configSource)
	}); err != nil {
		return "", "", err
	}
	if err := validatePrivateConfig(configPath, base, resolution); err != nil {
		return "", "", err
	}
	return configPath, base, nil
}

func validateExistingPrivateConfig(ctx context.Context, stateRoot, configPath, base string, resolution ResolutionContext, guard PrivateGenerationGuard) error {
	if validateErr := validatePrivateConfig(configPath, base, resolution); validateErr != nil {
		repaired, repairErr := repairIncompletePrivateConfig(ctx, stateRoot, configPath, base, resolution, validateErr, guard)
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

func repairIncompletePrivateConfig(ctx context.Context, stateRoot, configPath, base string, resolution ResolutionContext, validationErr error, guard PrivateGenerationGuard) (bool, error) {
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
	config, parseErr := ParseConfig(current)
	if parseErr != nil {
		return false, parseErr
	}
	callbackRunner := []string{""}
	if config.Daemon.Callback != nil {
		runner, configured := privateTerminalCallbackRunner(base, config.Daemon.Callback)
		if !configured {
			return false, nil
		}
		callbackRunner[0] = runner
	}
	if !bytes.Equal(current, []byte(privateConfigYAMLWithSocket(base, filepath.Join(base, "run", "pueue.sock"), callbackRunner...))) {
		return false, nil
	}
	repaired := false
	err = guardPrivateGeneration(ctx, stateRoot, guard, func() error {
		authorized, authorizationErr := incompletePrivateConfigRepairAuthorized(stateRoot, base)
		if authorizationErr != nil || !authorized {
			return authorizationErr
		}
		replacement, replacementErr := privateConfigYAMLForResolution(base, resolution, callbackRunner...)
		if replacementErr != nil {
			return replacementErr
		}
		if bytes.Equal(current, []byte(replacement)) {
			return nil
		}
		if replaceErr := replacePrivateRecordAtomic(base, configPath, ".private-config-", "incomplete private supervisor config", []byte(replacement), true); replaceErr != nil {
			return replaceErr
		}
		repaired = true
		return nil
	})
	return repaired, err
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
		privateCompactSocketPath(base),
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

func privateConfigYAMLForResolution(base string, resolution ResolutionContext, callbackRunner ...string) (string, error) {
	if len(callbackRunner) > 1 {
		return "", ErrConfiguration
	}
	if len(callbackRunner) == 1 && callbackRunner[0] != "" {
		if _, err := privateInstalledTerminalEventCallback(filepath.Dir(base), callbackRunner[0]); err != nil {
			return "", err
		}
	}
	limit, ok := unixSocketPathLimit(resolution.OS)
	if !ok {
		return "", fmt.Errorf("%w: unsupported platform", ErrConfiguration)
	}
	socketPath := filepath.Join(base, "run", "pueue.sock")
	if len(socketPath) > limit {
		socketPath = privateCompactSocketPath(base)
	}
	data := privateConfigYAMLWithSocket(base, socketPath, callbackRunner...)
	if _, err := resolvePrivateConfigSource([]byte(data), base, resolution); err != nil {
		return "", err
	}
	return data, nil
}

// privateCompactSocketPath is the one definition of the short endpoint the
// config selects when the run-directory socket would exceed the platform limit.
// Bootstrap residue detection and stale endpoint cleanup recognize the same path.
func privateCompactSocketPath(base string) string {
	return filepath.Join(base, "s")
}

func privateConfigYAMLWithSocket(base, socketPath string, callbackRunner ...string) string {
	quote := strconv.Quote
	data := filepath.Join(base, "data")
	runtimeDirectory := filepath.Join(base, "run")
	callback := privateTerminalEventCallback(base)
	if len(callbackRunner) > 0 {
		callback = ""
		if callbackRunner[0] != "" {
			callback = privateTerminalEventCallbackForRunner(filepath.Dir(base), callbackRunner[0])
		}
	}
	lines := []string{
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
		"  callback: " + quote(callback),
		"  shell_command:",
		"    - \"/bin/sh\"",
		"    - \"-c\"",
		"    - \"{{ pueue_command_string }}\"",
		"",
	}
	if callback == "" {
		lines = slices.DeleteFunc(lines, func(line string) bool { return strings.HasPrefix(line, "  callback:") })
	}
	return strings.Join(lines, "\n")
}

// privateInstalledCallbackConfig edits the one owned callback row, retaining
// every other byte of the managed config. Nonstandard source is never guessed.
func privateInstalledCallbackConfig(data []byte, base, runner string) ([]byte, bool, error) {
	if runner == "" {
		return data, false, nil
	}
	callback, err := privateInstalledTerminalEventCallback(filepath.Dir(base), runner)
	if err != nil {
		return nil, false, err
	}
	config, err := ParseConfig(data)
	if err != nil {
		return nil, false, err
	}
	if config.Daemon.Callback != nil && *config.Daemon.Callback == callback {
		return data, false, nil
	}
	source := string(data)
	replacement := "  callback: " + strconv.Quote(callback)
	if config.Daemon.Callback == nil {
		const daemonHeader = "\ndaemon:\n"
		if strings.Count(source, daemonHeader) != 1 {
			return nil, false, fmt.Errorf("%w: managed daemon callback section is unavailable", ErrPrivateSupervisorMigrationRequired)
		}
		source = strings.Replace(source, daemonHeader, daemonHeader+replacement+"\n", 1)
	} else {
		if !privateTerminalEventsConfigured(base, config.Daemon.Callback) {
			return nil, false, fmt.Errorf("%w: private callback is outside the supported terminal protocol", ErrPrivateSupervisorMigrationRequired)
		}
		previous := "  callback: " + strconv.Quote(*config.Daemon.Callback)
		if strings.Count(source, "\n"+previous+"\n") != 1 {
			return nil, false, fmt.Errorf("%w: managed callback source is unavailable", ErrPrivateSupervisorMigrationRequired)
		}
		source = strings.Replace(source, "\n"+previous+"\n", "\n"+replacement+"\n", 1)
	}
	if _, err = ParseConfig([]byte(source)); err != nil {
		return nil, false, err
	}
	return []byte(source), true, nil
}

// Explicit migration owns this config transition before any target is
// accepted. The existing recovery alias retains the exact predecessor bytes;
// interrupted preparation therefore resumes through the normal alias path.
func preparePrivateInstalledCallback(ctx context.Context, root string, candidate *Client) (*Client, error) {
	if candidate == nil {
		return nil, ErrConfiguration
	}
	if pending, err := PrivateSupervisorMigrationPending(root); err != nil || pending {
		return candidate, err
	}
	data, err := readRegular(candidate.binding.ConfigPath, MaxControlBytes)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(root, privateSupervisorDirectory)
	replacement, changed, err := privateInstalledCallbackConfig(data, base, candidate.options.PrivateTerminalEventRunner)
	if err != nil {
		return candidate, err
	}
	if !changed {
		// A previous config rename may have committed before its directory
		// barrier failed. Its durable predecessor alias owns that retry.
		if syncErr := syncPreparedPrivateCallback(root, base); syncErr != nil {
			return nil, syncErr
		}
		return candidate, nil
	}
	published, found, err := loadPrivateSupervisorBinding(root)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: complete canonical predecessor is required for callback migration", ErrPrivateSupervisorMigrationRequired)
	}
	return candidate.preparePrivateCallbackTransition(ctx, root, published, data, replacement)
}

func (c *Client) preparePrivateCallbackTransition(ctx context.Context, root string, published task.SupervisorRef, data, replacement []byte) (_ *Client, resultErr error) {
	options := c.options
	options.RuntimeClientExecutable = c.runtimeBinding.ClientExecutable
	previous, err := BindSaved(ctx, published, options)
	recoveryOwned, stopRecoveryOnFailure := false, false
	if previous != nil {
		defer func() {
			if resultErr != nil && stopRecoveryOnFailure {
				resultErr = errors.Join(resultErr, previous.StopRecovered(ctx, options.ObservationTimeout, resultErr))
			}
			resultErr = errors.Join(resultErr, previous.CloseAfterPending(resultErr))
		}()
	}
	if err != nil {
		return nil, err
	}
	recovered, started, err := c.recoverStoppedPrivateCallback(ctx, root, published, previous)
	if err != nil {
		return nil, err
	}
	if recovered != previous {
		closeErr := previous.CloseAfterPending(nil)
		previous, recoveryOwned = recovered, started
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err = c.authorizePrivateMigrationQueue(ctx, root, previous); err != nil {
		return nil, err
	}
	stopRecoveryOnFailure = recoveryOwned
	if err = previous.PersistPrivateRecoveryBindingContext(ctx, root); err != nil {
		return nil, err
	}
	if recoveryOwned {
		// A later config rename invalidates the primary transport. Preserve the
		// recovery owner's stop path through its already-persisted exact alias.
		transport, bindErr := c.bindPreparedPrivateCallbackAlias(ctx, root, published)
		if bindErr != nil {
			return nil, bindErr
		}
		closeErr := previous.CloseAfterPending(nil)
		previous = transport
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err = c.replacePrivateInstalledCallback(ctx, root, data, replacement); err != nil {
		return nil, err
	}
	return c.bindPrivateInstalledCallback(ctx)
}

func (c *Client) bindPreparedPrivateCallbackAlias(ctx context.Context, root string, published task.SupervisorRef) (*Client, error) {
	alias, present, err := readPrivateRecoveryBinding(root)
	if err != nil || !present {
		return nil, errors.Join(ErrBinding, err)
	}
	return bindPrivateRecoveryTransport(ctx, alias, published, c.runtimeBinding.ClientExecutable, c.runtimeBinding.DaemonExecutable, c.options)
}

// Offline state can refuse recovery, never authorize migration writes. Only
// the live queue check after the existing recovery owner returns does that.
func (c *Client) recoverStoppedPrivateCallback(ctx context.Context, root string, published task.SupervisorRef, previous *Client) (*Client, bool, error) {
	if published.ExecutableIdentity != task.ExecutableIdentityInstalledPathV1 {
		return previous, false, nil
	}
	facts, err := loadPrivateGenerationFacts(root)
	if err != nil {
		return nil, false, err
	}
	complete, err := facts.complete(published, facts.aliasPresent && facts.alias.matches(published))
	if err != nil || !complete || facts.aliasPublishing || facts.aliasPublicationErr != nil {
		return previous, false, errors.Join(err, facts.aliasPublicationErr)
	}
	if err = refusePendingPrivateRestart(root); err != nil {
		return nil, false, err
	}
	missing, err := PrivateSupervisorPairMissing(published)
	if err != nil || !missing {
		return previous, false, err
	}
	observation, err := previous.availableForTransition(ctx, true)
	if err != nil || observation.ready() {
		return previous, false, err
	}
	if err = previous.requireStoppedPrivateCallback(observation); err != nil {
		return nil, false, err
	}
	if err = c.checkStoppedPrivateCallbackRestore(ctx, root, published, previous); err != nil {
		return nil, false, err
	}
	return RecoverSavedWithPair(ctx, root, published, c.runtimeBinding.ClientExecutable, c.runtimeBinding.DaemonExecutable, c.options)
}

func (c *Client) requireStoppedPrivateCallback(observation PrivateEndpointObservation) error {
	if observation.State() != PrivateEndpointAbsent && observation.State() != PrivateEndpointRefused {
		return privateRecoveryFailure("predecessor_pair_unavailable", ErrBinding)
	}
	pid, recorded, err := c.privateDaemonPID()
	if err != nil {
		return privateRecoveryFailure("predecessor_pair_unavailable", err)
	}
	if !recorded {
		if observation.State() == PrivateEndpointAbsent {
			return nil
		}
		return privateRecoveryFailure("predecessor_pair_unavailable", observation.projectedError())
	}
	_, present, err := c.privateProcessIdentity(pid)
	if err != nil || present {
		return privateRecoveryFailure("predecessor_pair_unavailable", errors.Join(ErrBinding, err))
	}
	return nil
}

func (c *Client) checkStoppedPrivateCallbackRestore(ctx context.Context, root string, published task.SupervisorRef, previous *Client) error {
	pin, err := readControlRootPin(ctx, root)
	if err != nil {
		return err
	}
	resolution, err := previous.resolution()
	if err != nil {
		return err
	}
	restore, err := ObservePrivateRestore(ctx, pin, published, resolution)
	if err != nil {
		return err
	}
	if restore.Presence() != PrivateRestorePresent {
		return privateRecoveryFailure("predecessor_pair_unavailable", ErrPrivateRestoreLayout)
	}
	if _, err = c.authorizePrivateMigrationJobs(ctx, root, published, restore.Snapshot().Jobs); err != nil {
		if task.AllErrorCauses(err, func(leaf error) bool { return errors.Is(leaf, ErrPrivateSupervisorMigrationBusy) }) &&
			!task.AnyErrorNode(err, func(node error) bool {
				_, retained := node.(interface{ RetainsResidentCleanup() })
				return retained
			}) {
			// A stopped raw queue cannot finish work. Do not retain Busy as a
			// leaf, project wait_for_legacy_work, or publish a recovery alias.
			return privateRecoveryFailure("predecessor_pair_unavailable", ErrBinding)
		}
		return err
	}
	rechecked, err := ObservePrivateRestore(ctx, pin, published, resolution)
	if err != nil {
		return err
	}
	if rechecked.Presence() != PrivateRestorePresent || rechecked.Source() != restore.Source() {
		return privateRecoveryFailure("predecessor_pair_unavailable", ErrPrivateRestoreLayout)
	}
	observation, err := previous.availableForTransition(ctx, true)
	if err != nil {
		return err
	}
	return previous.requireStoppedPrivateCallback(observation)
}

func (c *Client) bindPrivateInstalledCallback(ctx context.Context) (*Client, error) {
	prepared, err := Bind(ctx, c.runtimeBinding.ClientExecutable, c.binding.ConfigPath, c.options)
	if err == nil {
		err = attachPrivateDaemon(prepared, c.runtimeBinding.DaemonExecutable)
	}
	if err != nil {
		return nil, errors.Join(err, prepared.CloseAfterPending(err))
	}
	return prepared, nil
}

func syncPreparedPrivateCallback(root, base string) error {
	if _, present, err := readPrivateRecoveryBinding(root); err != nil {
		return err
	} else if present {
		return syncPrivateDirectory(base)
	}
	return nil
}

func (c *Client) replacePrivateInstalledCallback(ctx context.Context, root string, expected, replacement []byte) error {
	base := filepath.Join(root, privateSupervisorDirectory)
	return guardPrivateGeneration(ctx, root, c.options.PrivateGenerationGuard, func() error {
		current, err := readRegular(c.binding.ConfigPath, MaxControlBytes)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, expected) {
			return fmt.Errorf("%w: private config changed during callback preparation", ErrBinding)
		}
		if _, present, err := loadPrivateMigrationRecord(base); err != nil || present {
			return errors.Join(ErrPrivateSupervisorMigrationRequired, err)
		}
		return replacePrivateRecordAtomic(base, c.binding.ConfigPath, ".private-config-", "private callback migration config", replacement, true)
	})
}

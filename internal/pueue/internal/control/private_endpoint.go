package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Shutdown requests graceful termination through the exact bound endpoint.
// It never falls back to a process signal or a different supervisor.
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.ShutdownDispatched(ctx)
	return err
}

// ShutdownDispatched reports whether the shutdown command was handed to the
// process observer. A false result means verification or command construction
// failed before the command could be dispatched, so a recovery owner may still
// perform its bounded cleanup. A true result transfers command ownership to
// the observer even when the command later fails or remains in flight.
func (c *Client) ShutdownDispatched(ctx context.Context) (dispatched bool, resultErr error) {
	if c == nil {
		return false, ErrConfiguration
	}
	if err := c.verify(ctx); err != nil {
		return false, err
	}
	result, err := c.command(ctx, nil, "shutdown")
	if pending := pendingFrom(err); pending != nil {
		dispatched = true
		result, err = awaitPending(ctx, pending)
	}
	dispatched = dispatched || result.Started
	if err != nil {
		return dispatched, errors.Join(ErrBinding, err)
	}
	if result.ExitCode != 0 {
		return dispatched, fmt.Errorf("%w: supervisor shutdown was rejected", ErrBinding)
	}
	return dispatched, nil
}

// Reachable performs a bounded, read-only readiness probe and reports whether
// the bound endpoint answered a valid status request. A normal endpoint
// absence is represented by (false, nil); malformed or changed bindings remain
// errors so callers cannot mistake incompatibility for a stopped daemon.
func (c *Client) Reachable(ctx context.Context) (bool, error) {
	return c.available(ctx)
}

// VerifyPrivateMigrationEndpoint checks the endpoint that an explicit
// migration would replace before any successor can be started. A stopped
// daemon with the same persisted launch identity is an ordinary restart, so it
// remains safe to start. A stopped daemon whose identity differs from the
// current launch pair is an upgrade migration; without a durable predecessor
// journal, migration must stop rather than risk running two daemons for one
// queue.
func (c *Client) VerifyPrivateMigrationEndpoint(ctx context.Context, stateRoot string) error {
	if c == nil || !IsPrivateConfig(stateRoot, c.binding.ConfigPath) {
		return ErrConfiguration
	}
	// Migration verification is a lifecycle decision. It requires the full
	// status protocol so a malformed task response cannot be mistaken for a
	// recoverable private supervisor. Admission reuses a separately established
	// bounded group proof only for its already-live scheduling path.
	ready, err := c.availableForBootstrap(ctx)
	if err != nil {
		return err
	}
	if ready {
		return nil
	}
	_, identityPresent, err := loadPrivateDaemonIdentity(filepath.Join(stateRoot, privateSupervisorDirectory))
	if err != nil {
		return err
	}
	if identityPresent {
		_, bindingPresent, bindingErr := loadPrivateSupervisorBinding(stateRoot)
		if bindingErr != nil {
			return bindingErr
		}
		if bindingPresent {
			return nil
		}
		return fmt.Errorf("%w: private supervisor endpoint is unreachable and its logical predecessor binding is unavailable", ErrPrivateSupervisorMigrationRequired)
	}
	bootstrapPending, bootstrapErr := PrivateBootstrapPending(stateRoot)
	if bootstrapErr != nil {
		return bootstrapErr
	}
	if bootstrapPending {
		return nil
	}
	return fmt.Errorf("%w: private supervisor endpoint is unreachable and its predecessor identity is unavailable", ErrPrivateSupervisorMigrationRequired)
}

// WaitUnavailable waits until the exact endpoint no longer answers a valid
// status request and, when the private supervisor's PID is recorded, until
// that process has exited. It is used only after a successful Delegate-owned
// shutdown. Verification errors are returned immediately because they cannot
// prove that the endpoint has disappeared.
func (c *Client) WaitUnavailable(ctx context.Context) error {
	if c == nil {
		return ErrConfiguration
	}
	if ctx == nil {
		return context.Canceled
	}
	pid, pidRecorded, err := c.privateDaemonPID()
	if err != nil {
		return err
	}
	process := privateProcessIdentity{}
	processPresent := false
	if pidRecorded {
		process, processPresent, err = c.privateProcessIdentity(pid)
		if err != nil {
			return err
		}
	}
	return c.waitUnavailable(ctx, process, pidRecorded, processPresent)
}

// WaitUnavailableWithPID waits for a previously captured private daemon PID
// as well as the exact endpoint. Migration captures the PID before sending
// shutdown, because a graceful daemon may remove its PID file before the
// caller starts waiting.
func (c *Client) WaitUnavailableWithPID(ctx context.Context, pid int) error {
	if c == nil {
		return ErrConfiguration
	}
	if pid <= 0 {
		return fmt.Errorf("%w: private supervisor PID is invalid", ErrBinding)
	}
	process, present, err := c.privateProcessIdentity(pid)
	if err != nil {
		return err
	}
	return c.waitUnavailable(ctx, process, true, present)
}

func (c *Client) waitUnavailableWithProcess(ctx context.Context, process privateProcessIdentity) error {
	if process.PID <= 0 || process.StartToken == "" {
		return fmt.Errorf("%w: private supervisor process identity is incomplete", ErrBinding)
	}
	return c.waitUnavailable(ctx, process, true, true)
}

func (c *Client) waitUnavailable(ctx context.Context, process privateProcessIdentity, pidRecorded, processPresent bool) error {
	if ctx == nil {
		return context.Canceled
	}
	path, err := privateEndpointSocketPath(c.runtimeBinding.Endpoint)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		unavailable, err := c.privateEndpointUnavailable(ctx, path, process, pidRecorded, processPresent)
		if err != nil {
			return err
		}
		if unavailable {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Client) privateEndpointUnavailable(ctx context.Context, path string, process privateProcessIdentity, pidRecorded, processPresent bool) (bool, error) {
	unavailable, refused, err := privateEndpointProbe(ctx, path)
	if err != nil {
		return false, err
	}
	processUnavailable := true
	if processPresent {
		processUnavailable, err = c.privateProcessUnavailable(process)
		if err != nil {
			return false, err
		}
	}
	if refused && pidRecorded && processUnavailable {
		unavailable, err = c.removeStalePrivateEndpoint(ctx, path, process)
		if err != nil {
			return false, err
		}
	}
	return unavailable && processUnavailable, nil
}

// removeStalePrivateEndpoint removes only a state-rooted Unix socket whose
// recorded daemon is already gone. Callers reach this through the bootstrap
// transition lock, so a Delegate-owned successor cannot race the ownership
// check and lose its live endpoint.
func (c *Client) removeStalePrivateEndpoint(ctx context.Context, path string, process privateProcessIdentity) (bool, error) {
	if ctx == nil {
		return false, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !isPrivateClient(c) {
		return false, nil
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return false, nil
	}
	runDirectory := filepath.Join(stateRoot, privateSupervisorDirectory, "run")
	if err := pathWithin(runDirectory, path); err != nil {
		return false, fmt.Errorf("%w: private supervisor socket is outside its runtime directory", ErrBinding)
	}
	if unavailable, err := c.privateProcessUnavailable(process); err != nil {
		return false, err
	} else if !unavailable {
		return false, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect stale private supervisor socket: %w", ErrBinding, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return false, fmt.Errorf("%w: private supervisor endpoint is not a Unix socket", ErrBinding)
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("%w: remove stale private supervisor socket: %w", ErrBinding, err)
	}
	return true, syncPrivateDirectory(filepath.Join(stateRoot, privateSupervisorDirectory))
}

func (c *Client) privateDaemonPID() (pid int, present bool, resultErr error) {
	if c == nil {
		return 0, false, ErrConfiguration
	}
	if !isPrivateClient(c) {
		return 0, false, nil
	}
	configData, err := readRegular(c.runtimeBinding.ConfigPath, MaxControlBytes)
	if err != nil {
		return 0, false, fmt.Errorf("%w: read private supervisor config for PID: %w", ErrConfiguration, err)
	}
	config, err := ParseConfig(configData)
	if err != nil {
		return 0, false, err
	}
	resolution, err := c.resolution()
	if err != nil {
		return 0, false, err
	}
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		return 0, false, err
	}
	stateRoot, _ := privateStateRoot(c.binding.ConfigPath)
	if err = resolved.ValidateIsolation(filepath.Join(stateRoot, privateSupervisorDirectory)); err != nil {
		return 0, false, err
	}
	data, err := readRegular(resolved.PIDPath, 128)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("%w: read private supervisor PID: %w", ErrBinding, err)
	}
	text := strings.TrimSpace(string(data))
	pid, err = strconv.Atoi(text)
	if err != nil || pid <= 0 || strconv.Itoa(pid) != text {
		return 0, false, fmt.Errorf("%w: private supervisor PID is invalid", ErrBinding)
	}
	return pid, true, nil
}

// RestartPrivate starts the already-bound private daemon after the caller has
// shut down and drained the old endpoint. It refuses a reachable endpoint and
// publishes the new daemon identity only after readiness succeeds.
func (c *Client) RestartPrivate(ctx context.Context, stateRoot string) error {
	if c == nil {
		return ErrConfiguration
	}
	if !IsPrivateConfig(stateRoot, c.binding.ConfigPath) {
		return ErrConfiguration
	}
	if err := c.verifyBinding(); err != nil {
		return err
	}
	if c.runtimeBinding.DaemonExecutable == "" || c.runtimeBinding.DaemonSHA256 == "" {
		return ErrBinding
	}
	if err := c.WaitUnavailable(ctx); err != nil {
		return err
	}
	return c.startPrivate(ctx)
}

package pueue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"golang.org/x/sys/unix"
)

// Client keeps immutable supervisor identity and owns only the bounded
// background maintenance it admits. Observations synchronize through their
// own command completion handles.
type Client struct {
	// binding is the durable supervisor reference used by task records and
	// permits. runtimeBinding is the executable pair currently used to reach it.
	binding        task.SupervisorRef
	runtimeBinding task.SupervisorRef
	options        Options
	cleanup        func() error
	workOwner      *clientWorkOwner
	ownsWorkOwner  bool
}

type clientWorkOwner struct {
	mu                     sync.Mutex
	closing                bool
	privateMaintenance     bool
	privateMaintenanceDone chan struct{}
	work                   sync.WaitGroup
}

// ErrStatusOperational identifies a completed status command that could not
// produce a valid queue snapshot. ErrUnknown may still be joined for callers
// that treat every non-match as uncertainty, but this sentinel distinguishes
// malformed output, schema failures, and nonzero exits from a valid no-match.
var ErrStatusOperational = errors.New("pueue status observation failed")

// ErrStatusTransient marks a completed status command that failed before it
// produced a usable snapshot for a reason that may clear on a later bounded
// observation. Parser, schema, identity, binding, and output-limit failures do
// not carry this sentinel.
var ErrStatusTransient = errors.New("pueue status observation temporarily unavailable")

// Bind validates an explicit config and executable before recording a binding.
func Bind(ctx context.Context, executable, configPath string, options Options) (*Client, error) {
	options, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	path, err := canonicalExecutable(executable)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(configPath) || filepath.Clean(configPath) != configPath {
		return nil, ErrConfiguration
	}
	c := &Client{
		options:       options,
		binding:       task.SupervisorRef{ClientExecutable: path, ConfigPath: configPath},
		workOwner:     &clientWorkOwner{},
		ownsWorkOwner: true,
	}
	c.runtimeBinding = c.binding
	current, err := c.readBinding()
	if err != nil {
		return nil, err
	}
	version, err := c.checkVersion(ctx)
	if err != nil {
		return c, err
	}
	current.ObservedVersion = version
	c.binding = current
	c.runtimeBinding = current
	return c, nil
}

// BindSaved reopens a previously recorded supervisor for bounded, read-only
// reconciliation. Its saved path-resolution context is pinned before the
// executable is inspected, so migration does not silently bind a different
// user configuration. When inspection remains in flight, the returned client
// owns the pending command and must be closed with CloseAfterPending.
func BindSaved(ctx context.Context, saved task.SupervisorRef, options Options) (*Client, error) {
	if err := task.ValidateFreshSupervisorRef(saved); err != nil {
		return nil, err
	}
	resolved, err := optionsForSavedBinding(saved, options)
	if err != nil {
		return nil, err
	}
	client, err := Bind(ctx, saved.ClientExecutable, saved.ConfigPath, resolved)
	if err != nil {
		return client, err
	}
	if !task.SameSupervisorIdentity(client.Binding(), saved) {
		return client, ErrBinding
	}
	return client, nil
}

// BindSavedForMigration reopens a saved supervisor while permitting a logical
// private-config drift to be inspected by the migration owner. It retains the
// saved binding as durable authority and uses a separately verified runtime
// binding for the bounded status/shutdown commands.
func BindSavedForMigration(ctx context.Context, saved task.SupervisorRef, options Options) (*Client, error) {
	return BindSavedForMigrationWithClient(ctx, saved, saved.ClientExecutable, options)
}

// BindSavedForMigrationWithClient is the migration form of BindSaved with an
// explicit verified client executable. It is used when an upgrade removed the
// executable recorded in the legacy binding while the legacy endpoint remains
// reachable.
func BindSavedForMigrationWithClient(ctx context.Context, saved task.SupervisorRef, clientExecutable string, options Options) (*Client, error) {
	if err := validateSavedSupervisorRef(saved); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	resolved, err := optionsForSavedBinding(saved, options)
	if err != nil {
		return nil, err
	}
	if clientExecutable == "" {
		clientExecutable = saved.ClientExecutable
	}
	return bindSavedForMigrationWithClient(ctx, saved, clientExecutable, resolved)
}

func bindSavedForMigrationWithClient(ctx context.Context, saved task.SupervisorRef, clientExecutable string, options Options) (*Client, error) {
	client, err := Bind(ctx, clientExecutable, saved.ConfigPath, options)
	if err != nil {
		return client, err
	}
	migration, migrationErr := bindSavedMigrationClient(ctx, saved, client)
	if migration == nil && migrationErr != nil {
		// Bind succeeded, so this client remains the stable owner for every
		// later validation failure. Endpoint-migration staging owns its own
		// temporary config cleanup and retains any pending command in the error.
		return client, migrationErr
	}
	return migration, migrationErr
}

// NewClient constructs from saved identity without consulting external files.
// Each operation performs its own fresh validation before execution.
func NewClient(saved task.SupervisorRef, options Options) (*Client, error) {
	if err := task.ValidateFreshSupervisorRef(saved); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBinding, err)
	}
	options, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	if resolution, present, resolutionErr := resolutionFromBinding(saved); resolutionErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrBinding, resolutionErr)
	} else if present {
		if options.Resolution != nil && !sameEnvironmentResolution(*options.Resolution, resolution) {
			return nil, ErrConfiguration
		}
		options.Resolution = &resolution
		options.resolutionPinned = true
		if options.Environment == nil || len(options.Environment) > 0 {
			options.Environment = environmentForResolution(options.Environment, resolution)
		}
	}
	return &Client{
		binding: saved, runtimeBinding: saved, options: options,
		workOwner: &clientWorkOwner{}, ownsWorkOwner: true,
	}, nil
}

func normalizeOptions(options Options) (Options, error) {
	if options.ObservationTimeout == 0 {
		options.ObservationTimeout = DefaultObservationTimeout
	}
	if options.ObservationTimeout < 0 {
		return Options{}, ErrConfiguration
	}
	if options.Resolution != nil {
		copy := *options.Resolution
		options.Resolution = &copy
	}
	if options.Environment != nil {
		options.Environment = append([]string{}, options.Environment...)
	}
	return options, nil
}

func (c *Client) Binding() task.SupervisorRef { return c.binding }

// RuntimeBinding returns the verified executable/configuration generation
// currently used for control commands. It can differ from Binding during a
// saved-task recovery, where the durable task reference remains unchanged.
func (c *Client) RuntimeBinding() task.SupervisorRef { return c.runtimeBinding }

// Close waits for admitted background maintenance, then removes any short-lived
// migration configuration owned by this client.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.closeBackgroundWork()
	if c.cleanup == nil {
		return nil
	}
	cleanup := c.cleanup
	c.cleanup = nil
	return cleanup()
}

// CloseAfterPending closes synchronously when no command is pending. When err
// retains unfinished command ownership, it prevents new work and transfers
// final cleanup to that existing work owner so the finite caller can return.
// A timed-out command owns its process through Pending; removing a short-lived
// migration config before that ownership ends can change the supervisor it
// observes.
func (c *Client) CloseAfterPending(err error) error {
	if c == nil {
		return nil
	}
	c.beginBackgroundClose()
	cleanup := c.takeCleanup()
	if len(pendingHandles(err)) == 0 {
		c.waitBackgroundWork()
		if cleanup == nil {
			return nil
		}
		return cleanup()
	}
	if cleanup == nil {
		return nil
	}
	owner := c.workOwner
	owned := c.ownsWorkOwner
	go func() {
		if owned && owner != nil {
			owner.work.Wait()
		}
		if cleanupErr := cleanup(); cleanupErr != nil {
			log.Printf("delegation-layer: deferred private supervisor cleanup failed: %v", cleanupErr)
		}
	}()
	return nil
}

// WithObservationTimeout returns an independent client with the same bound
// supervisor identity and a different finite command observation timeout. The
// binding and runtime executable pair remain unchanged; this is used when a
// bounded inspection needs a longer observation window without changing the
// timeout for ordinary supervisor controls.
func (c *Client) WithObservationTimeout(timeout time.Duration) (*Client, error) {
	if c == nil {
		return nil, ErrConfiguration
	}
	options := c.options
	options.ObservationTimeout = timeout
	options, err := normalizeOptions(options)
	if err != nil {
		return nil, err
	}
	return &Client{
		binding: c.binding, runtimeBinding: c.runtimeBinding, options: options,
		workOwner: c.workOwner,
	}, nil
}

func (c *Client) closeBackgroundWork() {
	c.beginBackgroundClose()
	c.waitBackgroundWork()
}

func (c *Client) beginBackgroundClose() {
	if c == nil || !c.ownsWorkOwner || c.workOwner == nil {
		return
	}
	c.workOwner.mu.Lock()
	c.workOwner.closing = true
	c.workOwner.mu.Unlock()
}

func (c *Client) waitBackgroundWork() {
	if c == nil || !c.ownsWorkOwner || c.workOwner == nil {
		return
	}
	c.workOwner.work.Wait()
}

func (c *Client) takeCleanup() func() error {
	if c == nil || c.cleanup == nil {
		return nil
	}
	cleanup := c.cleanup
	c.cleanup = nil
	return cleanup
}

// backgroundClient runs commands covered by an already-admitted outer work
// token. It must never be returned to a caller or closed independently.
func (c *Client) backgroundClient() *Client {
	if c == nil {
		return nil
	}
	worker := *c
	worker.cleanup = nil
	worker.workOwner = nil
	worker.ownsWorkOwner = false
	return &worker
}

func (c *Client) beginCommandWork() (func(), error) {
	if c == nil {
		return nil, ErrConfiguration
	}
	if c.workOwner == nil {
		return func() {}, nil
	}
	c.workOwner.mu.Lock()
	defer c.workOwner.mu.Unlock()
	if c.workOwner.closing {
		return nil, ErrConfiguration
	}
	c.workOwner.work.Add(1)
	return c.workOwner.work.Done, nil
}

func (c *Client) beginPrivateMaintenance() (finish func(), done <-chan struct{}, available bool) {
	if c == nil || c.workOwner == nil {
		return nil, nil, false
	}
	c.workOwner.mu.Lock()
	if c.workOwner.closing {
		c.workOwner.mu.Unlock()
		return nil, nil, false
	}
	if c.workOwner.privateMaintenance {
		done = c.workOwner.privateMaintenanceDone
		c.workOwner.mu.Unlock()
		return nil, done, true
	}
	doneChannel := make(chan struct{})
	c.workOwner.privateMaintenance = true
	c.workOwner.privateMaintenanceDone = doneChannel
	c.workOwner.work.Add(1)
	c.workOwner.mu.Unlock()
	return func() {
		c.workOwner.mu.Lock()
		c.workOwner.privateMaintenance = false
		c.workOwner.privateMaintenanceDone = nil
		close(doneChannel)
		c.workOwner.mu.Unlock()
		c.workOwner.work.Done()
	}, doneChannel, true
}

func canonicalExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: executable must be absolute", ErrConfiguration)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("%w: executable unavailable: %w", ErrConfiguration, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: executable must be an executable regular file", ErrConfiguration)
	}
	return resolved, nil
}

func (c *Client) resolution() (ResolutionContext, error) {
	return c.resolutionForEnvironment(environmentForCommand(c.options.Environment))
}

func (c *Client) resolutionForEnvironment(environment []string) (ResolutionContext, error) {
	if c.options.resolutionPinned {
		if c.options.Resolution == nil {
			return ResolutionContext{}, ErrConfiguration
		}
		resolution := *c.options.Resolution
		if err := validateResolutionContext(resolution); err != nil {
			return ResolutionContext{}, err
		}
		return resolution, nil
	}
	current, err := resolutionContext(environment)
	if err != nil {
		return ResolutionContext{}, err
	}
	if c.options.Resolution == nil {
		return current, nil
	}
	resolution := *c.options.Resolution
	if err := validateResolutionContext(resolution); err != nil {
		return ResolutionContext{}, err
	}
	if !sameEnvironmentResolution(resolution, current) {
		return ResolutionContext{}, fmt.Errorf("%w: resolution does not match command environment", ErrConfiguration)
	}
	return resolution, nil
}

func environmentForResolution(environment []string, resolution ResolutionContext) []string {
	merged := append([]string(nil), environmentForCommand(environment)...)
	for _, value := range []struct {
		key   string
		value string
	}{
		{key: "HOME", value: resolution.Home},
		{key: "XDG_DATA_HOME", value: resolution.DataLocalDirectory},
		{key: "XDG_CONFIG_HOME", value: resolution.ConfigDirectory},
		{key: "XDG_RUNTIME_DIR", value: resolution.RuntimeDirectory},
	} {
		if resolution.OS != "linux" && value.key != "HOME" {
			continue
		}
		merged = replaceEnvironmentValue(merged, value.key, value.value)
	}
	return merged
}

func replaceEnvironmentValue(environment []string, key, value string) []string {
	entry := key + "=" + value
	for index, current := range environment {
		name, _, ok := strings.Cut(current, "=")
		if ok && name == key {
			environment[index] = entry
			return environment
		}
	}
	return append(environment, entry)
}

func environmentForCommand(environment []string) []string {
	if environment != nil {
		return environment
	}
	return defaultEnvironment()
}

// defaultEnvironment bounds the environment inherited by the pueue client.
// It is rebuilt for each invocation so HOME/XDG changes are observed by both
// resolution and the child process without carrying unrelated ambient state.
func defaultEnvironment() []string {
	ambient := os.Environ()
	filtered := make([]string, 0, len(ambient))
	for _, entry := range ambient {
		key, _, ok := strings.Cut(entry, "=")
		if ok && defaultEnvironmentKey(key) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// DefaultEnvironment exposes the same bounded control environment used by a
// client with nil Options.Environment. Composition roots can add a provider's
// explicitly declared nonsecret variables without dropping platform-specific
// values such as Linux XDG socket/configuration selectors.
func DefaultEnvironment() []string { return defaultEnvironment() }

func defaultEnvironmentKey(key string) bool {
	switch key {
	case "HOME", "PATH", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "TMP", "TEMP", "__CF_USER_TEXT_ENCODING":
		return true
	case "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR":
		return runtime.GOOS == "linux"
	default:
		return false
	}
}

func sameEnvironmentResolution(resolution, current ResolutionContext) bool {
	return resolution.OS == current.OS &&
		resolution.Home == current.Home &&
		resolution.DataLocalDirectory == current.DataLocalDirectory &&
		resolution.ConfigDirectory == current.ConfigDirectory &&
		resolution.RuntimeDirectory == current.RuntimeDirectory &&
		resolution.Username == current.Username
}

func (c *Client) readBinding() (task.SupervisorRef, error) {
	runtimeBinding := c.runtimeBinding
	if err := rejectSymlinkComponents(runtimeBinding.ConfigPath); err != nil {
		return task.SupervisorRef{}, err
	}
	data, err := readRegular(runtimeBinding.ConfigPath, MaxControlBytes)
	if err != nil {
		return task.SupervisorRef{}, fmt.Errorf("%w: read explicit config: %w", ErrConfiguration, err)
	}
	config, err := ParseConfig(data)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	resolution, err := c.resolution()
	if err != nil {
		return task.SupervisorRef{}, err
	}
	resolved, err := ResolveConfig(config, resolution)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	fingerprint, err := resolved.Digest()
	if err != nil {
		return task.SupervisorRef{}, err
	}
	executable, err := canonicalExecutable(runtimeBinding.ClientExecutable)
	if err != nil || executable != runtimeBinding.ClientExecutable {
		return task.SupervisorRef{}, errors.Join(ErrBinding, err)
	}
	digest, err := hashExecutable(executable)
	if err != nil {
		return task.SupervisorRef{}, err
	}
	binding := task.SupervisorRef{ClientExecutable: executable, ClientSHA256: digest, DaemonExecutable: runtimeBinding.DaemonExecutable, DaemonSHA256: runtimeBinding.DaemonSHA256, ResolutionCwd: runtimeBinding.ResolutionCwd, ConfigPath: runtimeBinding.ConfigPath, ConfigDigest: task.ComputeSHA256(data), Endpoint: resolved.Endpoint(), ResolvedConfigSHA256: fingerprint, ObservedVersion: runtimeBinding.ObservedVersion}
	if runtimeBinding.Endpoint == "" || runtimeBinding.ResolutionOS != "" {
		setBindingResolution(&binding, resolution)
	}
	return binding, nil
}

func readRegular(path string, limit int64) (data []byte, err error) {
	f, err := openRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return task.ReadBounded(f, limit)
}

func hashExecutable(path string) (digest string, err error) {
	f, err := openRegular(path)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func openRegular(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open regular file: %w", ErrConfiguration, err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		return nil, errors.Join(ErrConfiguration, unix.Close(fd))
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(ErrConfiguration, f.Close())
	}
	return f, nil
}

func (c *Client) verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.verifyBinding(); err != nil {
		return err
	}
	return ctx.Err()
}

func (c *Client) verifyBinding() error {
	current, err := c.readBinding()
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	current.ObservedVersion = c.runtimeBinding.ObservedVersion
	if current != c.runtimeBinding {
		return ErrBinding
	}
	return nil
}

func (c *Client) checkVersion(ctx context.Context) (string, error) {
	result, err := c.command(ctx, nil, "--version")
	if err != nil {
		return "", bindingProbeError(err)
	}
	return validateVersionResult(result)
}

// An unfinished probe has no result that can establish binding drift. Retain
// its command owner and any independently established failure unchanged.
func bindingProbeError(err error) error {
	if err == nil || pendingFrom(err) != nil {
		return err
	}
	return errors.Join(ErrBinding, err)
}

func validateVersionResult(result CommandResult) (string, error) {
	if result.Err != nil {
		return "", errors.Join(ErrBinding, result.Err)
	}
	if len(result.Stdout) == 0 || len(result.Stdout) > MaxControlBytes || !utf8.Valid(result.Stdout) {
		return "", fmt.Errorf("%w: supervisor did not report a usable version", ErrBinding)
	}
	version := strings.TrimSpace(string(result.Stdout))
	if version == "" {
		return "", fmt.Errorf("%w: supervisor did not report a usable version", ErrBinding)
	}
	for _, character := range version {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("%w: supervisor did not report a usable version", ErrBinding)
		}
	}
	return version, nil
}

// Ready verifies that the configured daemon accepts a bounded status request
// with the queue schema required by this client. An empty queue is healthy.
func (c *Client) Ready(ctx context.Context) error {
	result, err := c.readyCommand(ctx)
	if err != nil {
		return bindingProbeError(err)
	}
	return c.validateReadyResult(result)
}

// readyCommand validates the binding immediately before the status probe.
// Version discovery belongs to Bind; unchanged executable bytes do not need a
// second version subprocess before the actual status command.
func (c *Client) readyCommand(ctx context.Context) (CommandResult, error) {
	return c.statusAfterVerifiedVersion(ctx)
}

func (c *Client) statusAfterVerifiedVersion(ctx context.Context) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	if err := c.verifyBinding(); err != nil {
		return CommandResult{}, err
	}
	result, err := c.command(ctx, nil, "status", "--json")
	return classifyReadyStatus(result, err)
}

// groupAfterVerifiedBinding observes only Pueue's bounded group state after
// verifying the saved executable and configuration binding. Private supervisor
// lifecycle controls use this instead of a queue-wide status response when
// they need liveness and ordinary-group scheduling facts, not task rows.
func (c *Client) groupAfterVerifiedBinding(ctx context.Context) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	if err := c.verifyBinding(); err != nil {
		return CommandResult{}, err
	}
	return c.command(ctx, nil, "group", "--json")
}

func classifyReadyStatus(result CommandResult, err error) (CommandResult, error) {
	if errors.Is(err, ErrControlLimit) {
		return result, errors.Join(err, errStatusControlLimit)
	}
	if result.snapshot != nil && err != nil {
		return result, errors.Join(ErrBinding, err)
	}
	return result, err
}

func (c *Client) validateReadyResult(result CommandResult) error {
	if result.Err != nil {
		return errors.Join(ErrBinding, result.Err)
	}
	if _, err := snapshotFromResult(result, c.runtimeBinding.ObservedVersion); err != nil {
		return errors.Join(ErrBinding, err)
	}
	return nil
}

func (c *Client) command(ctx context.Context, consume func() error, args ...string) (CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	cmd, err := c.prepareCommand(args...)
	if err != nil {
		return CommandResult{}, err
	}
	commandID := ""
	if c.options.Observer != nil || c.options.ControlOwnerExecutable != "" {
		commandID, err = task.NewRandomID()
		if err != nil {
			return CommandResult{}, err
		}
	}
	finishWork, err := c.beginCommandWork()
	if err != nil {
		return CommandResult{}, err
	}
	defer func() {
		if finishWork != nil {
			finishWork()
		}
	}()
	if consume != nil {
		if err := consume(); err != nil {
			return CommandResult{}, err
		}
	}
	statusVersion := ""
	if isJSONStatusCommand(args) {
		statusVersion = c.runtimeBinding.ObservedVersion
	}
	var pending *Pending
	if c.options.ControlOwnerExecutable == "" {
		pending = startOwnedCaptured(cmd, commandID, c.options.Observer, statusVersion)
	} else {
		pending = startControlOwnedCaptured(ctx, c.options.ControlOwnerExecutable, cmd, commandID, c.options.Observer, statusVersion)
	}
	finishPending := finishWork
	finishWork = nil
	go func() {
		<-pending.Done()
		finishPending()
	}()
	if statusVersion != "" {
		pending.status = &pendingStatusIdentity{binding: c.runtimeBinding, version: statusVersion}
	}
	result, observeErr := observeCommand(ctx, pending, c.options.ObservationTimeout)
	if statusVersion == "" {
		return result, observeErr
	}
	return result, statusObservationError(observeErr)
}

func isJSONStatusCommand(args []string) bool {
	if len(args) < 2 || args[0] != "status" {
		return false
	}
	for _, arg := range args[1:] {
		if arg == "--json" {
			return true
		}
	}
	return false
}

// prepareCommand constructs the fully bound supervisor command without
// starting a process. It is kept separate so command construction can be
// inspected independently from observer setup, permit consumption, and
// process ownership.
func (c *Client) prepareCommand(args ...string) (*exec.Cmd, error) {
	environment := environmentForCommand(c.options.Environment)
	resolution, err := c.resolutionForEnvironment(environment)
	if err != nil {
		return nil, err
	}
	commandArgs := make([]string, 0, len(args)+2)
	commandArgs = append(commandArgs, "-c", c.runtimeBinding.ConfigPath)
	commandArgs = append(commandArgs, args...)
	cmd := exec.Command(c.runtimeBinding.ClientExecutable, commandArgs...)
	cmd.Dir = resolution.Cwd
	if cmd.Dir == "" {
		// Saved bindings intentionally omit the caller's working directory. Use
		// the existing config directory for recovery so a removed ambient cwd
		// cannot prevent a bound supervisor command from starting.
		cmd.Dir = filepath.Dir(c.runtimeBinding.ConfigPath)
	}
	cmd.Env = environment
	return cmd, nil
}

func validateIdentity(i Identity) error {
	for _, err := range []error{task.ValidateRootID(i.RootID), task.ValidateTaskID(i.TaskID), task.ValidateSHA256(i.SpecSHA256), task.ValidateSHA256(i.MetaSHA256)} {
		if err != nil {
			return err
		}
	}
	if i.NumericTaskID != nil && *i.NumericTaskID < 0 {
		return ErrUnknown
	}
	return nil
}

// Reconcile never adds work. Empty, duplicate, malformed and mismatched rows
// remain unknown, including when a reachable queue is empty.
func (c *Client) Reconcile(ctx context.Context, identity Identity) (Observation, error) {
	identity = snapshotIdentity(identity)
	unknown := Observation{State: StateUnknown, Identity: identity, Binding: c.binding}
	if err := validateIdentity(identity); err != nil {
		return unknown, err
	}
	if err := c.verify(ctx); err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	snapshot, err := c.snapshotAfterVerify(ctx, "label="+identity.Label())
	if err != nil {
		unknown.Pending = pendingFrom(err)
		return unknown, err
	}
	return matchingObservation(snapshot.Jobs, identity, c.binding)
}

func matchingObservation(jobs []Job, identity Identity, binding task.SupervisorRef) (Observation, error) {
	identity = snapshotIdentity(identity)
	result := Observation{State: StateUnknown, Identity: identity, Binding: binding}
	matches := 0
	for _, job := range jobs {
		if job.Label == nil || *job.Label != identity.Label() {
			continue
		}
		matches++
		result.NumericTaskID, result.State, result.Group = job.ID, job.State, job.Group
	}
	if matches != 1 || (identity.NumericTaskID != nil && *identity.NumericTaskID != result.NumericTaskID) {
		result.State = StateUnknown
		return result, ErrUnknown
	}
	result.Matched = true
	return result, nil
}

func snapshotIdentity(identity Identity) Identity {
	if identity.NumericTaskID != nil {
		id := *identity.NumericTaskID
		identity.NumericTaskID = &id
	}
	return identity
}

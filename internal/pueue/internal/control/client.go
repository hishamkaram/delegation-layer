package control

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
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
	return c.withCommandOptions(options)
}

// WithCommandEnvironment derives one command client without changing daemon
// startup options, supervisor identity, or ownership of pending commands.
func (c *Client) WithCommandEnvironment(environment []string) (*Client, error) {
	if c == nil {
		return nil, ErrConfiguration
	}
	options := c.options
	options.Environment = environment
	return c.withCommandOptions(options)
}

func (c *Client) withCommandOptions(options Options) (*Client, error) {
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

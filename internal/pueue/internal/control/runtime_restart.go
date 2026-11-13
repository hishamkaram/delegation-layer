package control

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// RuntimeRestartReference is bounded correlation data, never Start authority.
// Infrastructure owns its canonical startup-token parser and immutable intent.
type RuntimeRestartReference string

type RuntimeRestartPhase string

const (
	RuntimeRestartRequested         RuntimeRestartPhase = "requested"
	RuntimeRestartShutdownPending   RuntimeRestartPhase = "shutdown_pending"
	RuntimeRestartDaemonClosed      RuntimeRestartPhase = "daemon_closed"
	RuntimeRestartSuccessorAccepted RuntimeRestartPhase = "successor_accepted"
	RuntimeRestartReady             RuntimeRestartPhase = "ready"
)

var (
	ErrPrivateRuntimeRestartPending              = errors.New("private runtime restart is pending")
	ErrPrivateRuntimeRestartAuthorization        = errors.New("private runtime restart authorization is unavailable")
	ErrPrivateRuntimeRestartReferenceNotRetained = errors.New("private runtime restart reference is not retained")
)

// PrivateRuntimeRestartError preserves the cause and a path-free classification.
type PrivateRuntimeRestartError struct {
	ReasonCode string
	Err        error
	diagnostic error
}

func (e *PrivateRuntimeRestartError) Error() string { return e.Err.Error() }

func (e *PrivateRuntimeRestartError) Unwrap() error { return e.Err }

// DiagnosticCause preserves a handled ordinary command failure separately
// from its semantic pending observation, as PrivateEndpointObservation does.
// Independent execution/storage/identity/cleanup faults are never projected.
func (e *PrivateRuntimeRestartError) DiagnosticCause() error { return e.diagnostic }

// PrivateRuntimeRestartObservation is a copy of one retained lifecycle record.
// Ready records describe completed handoffs, not current provider authentication.
type PrivateRuntimeRestartObservation struct {
	From      RuntimeRestartReference `json:"from"`
	Binding   task.SupervisorRef      `json:"binding"`
	Phase     RuntimeRestartPhase     `json:"phase"`
	Successor RuntimeRestartReference `json:"successor,omitempty"`
}

// The existing bootstrap journal owns this record. All mutations require the
// actual root bootstrap EX and the current resident generation commit guard.
// Original owner closure stays with infrastructure; successor acceptance is
// permitted only after its operation-specific start guard verifies that fact.
type privateRuntimeRestartRecord struct {
	From            RuntimeRestartReference `json:"from"`
	Binding         task.SupervisorRef      `json:"binding"`
	Phase           RuntimeRestartPhase     `json:"phase"`
	OldPID          int                     `json:"old_pid"`
	OldProcessStart string                  `json:"old_process_start"`
	Successor       RuntimeRestartReference `json:"successor,omitempty"`
	NewPID          int                     `json:"new_pid,omitempty"`
	NewProcessStart string                  `json:"new_process_start,omitempty"`
}

func (record privateRuntimeRestartRecord) observation() PrivateRuntimeRestartObservation {
	return PrivateRuntimeRestartObservation{From: record.From, Binding: record.Binding, Phase: record.Phase, Successor: record.Successor}
}

func validateRuntimeRestartReference(reference RuntimeRestartReference) error {
	const maxReferenceBytes = 256
	if len(reference) == 0 || len(reference) > maxReferenceBytes || !utf8.ValidString(string(reference)) ||
		strings.IndexFunc(string(reference), func(r rune) bool { return r <= ' ' || r > '~' }) >= 0 {
		return fmt.Errorf("%w: invalid runtime restart correlation", ErrBinding)
	}
	return nil
}

func validatePrivateRuntimeRestartRecord(root string, record privateRuntimeRestartRecord) error {
	if err := validateRuntimeRestartReference(record.From); err != nil {
		return err
	}
	if err := validatePrivateSupervisorRef(record.Binding); err != nil || !IsPrivateConfig(root, record.Binding.ConfigPath) {
		return errors.Join(ErrBinding, err)
	}
	if _, err := lifetime.NewProcessRef(record.OldPID, record.OldProcessStart); err != nil {
		return errors.Join(ErrBinding, err)
	}
	switch record.Phase {
	case RuntimeRestartRequested, RuntimeRestartShutdownPending, RuntimeRestartDaemonClosed:
		if record.Successor != "" || record.NewPID != 0 || record.NewProcessStart != "" {
			return ErrBinding
		}
	case RuntimeRestartSuccessorAccepted, RuntimeRestartReady:
		if err := validateRuntimeRestartReference(record.Successor); err != nil || record.Successor == record.From {
			return errors.Join(ErrBinding, err)
		}
		if err := validateRuntimeRestartSuccessor(record); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: invalid runtime restart phase", ErrBinding)
	}
	return nil
}

func validateRuntimeRestartSuccessor(record privateRuntimeRestartRecord) error {
	if record.NewPID == 0 && record.NewProcessStart == "" && record.Phase == RuntimeRestartSuccessorAccepted {
		return nil
	}
	newProcess, err := lifetime.NewProcessRef(record.NewPID, record.NewProcessStart)
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	oldProcess, err := lifetime.NewProcessRef(record.OldPID, record.OldProcessStart)
	if err != nil {
		return errors.Join(ErrBinding, err)
	}
	same, err := lifetime.SameProcess(newProcess, oldProcess)
	if err != nil || same {
		return errors.Join(ErrBinding, err)
	}
	return nil
}

// ObservePrivateRuntimeRestart only reads the exact retained reference. Missing
// proof is not proof of an earlier failure/success and never authorizes retry.
func ObservePrivateRuntimeRestart(ctx context.Context, root string, from RuntimeRestartReference) (PrivateRuntimeRestartObservation, error) {
	if err := runtimeRestartReadContext(ctx); err != nil {
		return PrivateRuntimeRestartObservation{}, err
	}
	if err := validateRuntimeRestartReference(from); err != nil {
		return PrivateRuntimeRestartObservation{}, err
	}
	bootstrap, found, err := readPrivateBootstrapRecord(root)
	if err != nil {
		return PrivateRuntimeRestartObservation{}, err
	}
	if err = ctx.Err(); err != nil {
		return PrivateRuntimeRestartObservation{}, err
	}
	if !found || bootstrap.RuntimeRestart == nil || bootstrap.RuntimeRestart.From != from {
		return PrivateRuntimeRestartObservation{}, &PrivateRuntimeRestartError{
			ReasonCode: "restart_reference_not_retained", Err: ErrPrivateRuntimeRestartReferenceNotRetained,
		}
	}
	return bootstrap.RuntimeRestart.observation(), nil
}

func runtimeRestartReadContext(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	return ctx.Err()
}

// PrivateRuntimeRestartPending derives the runtime-purpose admission fence.
// Unlike ordinary bootstrap, it never grants initial configuration repair.
func PrivateRuntimeRestartPending(ctx context.Context, root string) (bool, error) {
	if err := runtimeRestartReadContext(ctx); err != nil {
		return false, err
	}
	record, _, err := readPrivateBootstrapRecord(root)
	if err != nil {
		return false, err
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	return runtimeRestartPending(record), nil
}

func runtimeRestartPending(record privateBootstrapRecord) bool {
	return record.RuntimeRestart != nil && record.RuntimeRestart.Phase != RuntimeRestartReady
}

func requireRuntimeRestartBootstrapEX(ctx context.Context, root string) error {
	if err := runtimeRestartReadContext(ctx); err != nil {
		return err
	}
	bootstrap, tagged := ctx.Value(privateBootstrapEXKey{}).(*privateBootstrapEX)
	if !tagged || bootstrap == nil || bootstrap.root != root {
		return fmt.Errorf("%w: runtime restart requires the actual root bootstrap EX", ErrBinding)
	}
	if err := bootstrap.consume(nil); err != nil {
		return err
	}
	identity, err := checkedBootstrapIdentity(root, bootstrap.lock.file)
	if err != nil || identity != bootstrap.identity {
		return errors.Join(ErrBinding, err)
	}
	return nil
}

func (c *Client) acceptPrivateRuntimeRestart(ctx context.Context, root string, from RuntimeRestartReference, process privateProcessIdentity, jobs []Job) (privateRuntimeRestartRecord, error) {
	if c == nil || c.options.PrivateRuntimeRestartAuthorizer == nil {
		return privateRuntimeRestartRecord{}, ErrPrivateRuntimeRestartAuthorization
	}
	if err := requireRuntimeRestartBootstrapEX(ctx, root); err != nil {
		return privateRuntimeRestartRecord{}, err
	}
	candidate := privateRuntimeRestartRecord{From: from, Binding: c.binding, Phase: RuntimeRestartRequested, OldPID: process.PID, OldProcessStart: process.StartToken}
	if err := validatePrivateRuntimeRestartRecord(root, candidate); err != nil {
		return privateRuntimeRestartRecord{}, err
	}
	if err := c.options.PrivateRuntimeRestartAuthorizer(ctx, root, from, c.binding, append([]Job(nil), jobs...)); err != nil {
		return privateRuntimeRestartRecord{}, err
	}
	var accepted privateRuntimeRestartRecord
	err := guardPrivateGeneration(ctx, root, c.options.PrivateGenerationGuard, func() error {
		var err error
		accepted, err = acceptPrivateRuntimeRestartRecord(ctx, root, candidate)
		return err
	})
	return accepted, err
}

func acceptPrivateRuntimeRestartRecord(ctx context.Context, root string, candidate privateRuntimeRestartRecord) (privateRuntimeRestartRecord, error) {
	if err := requireRuntimeRestartBootstrapEX(ctx, root); err != nil {
		return privateRuntimeRestartRecord{}, err
	}
	published, found, err := loadPrivateSupervisorBinding(root)
	if err != nil || !found || published != candidate.Binding {
		return privateRuntimeRestartRecord{}, errors.Join(ErrBinding, err)
	}
	current, found, err := readPrivateBootstrapRecord(root)
	if err != nil {
		return privateRuntimeRestartRecord{}, err
	}
	if found && current.RuntimeRestart != nil && current.RuntimeRestart.From == candidate.From {
		return replaceRuntimeRestartAcceptance(root, current, candidate)
	}
	if err = runtimeRestartAcceptanceAvailable(ctx, root, candidate.From, candidate.Binding); err != nil {
		return privateRuntimeRestartRecord{}, err
	}
	if found {
		return replaceRuntimeRestartAcceptance(root, current, candidate)
	}
	data, err := task.MarshalCanonical(privateBootstrapRecord{SchemaVersion: task.SchemaVersion, RuntimeRestart: &candidate})
	if err == nil {
		err = publishPrivateRecord(filepath.Join(root, privateSupervisorDirectory), privateBootstrapRecordPath(root), data)
	}
	return candidate, err
}

func runtimeRestartAcceptanceAvailable(ctx context.Context, root string, from RuntimeRestartReference, binding task.SupervisorRef) error {
	migration, err := PrivateSupervisorMigrationPending(root)
	if err != nil {
		return err
	}
	if migration {
		return &PrivateRuntimeRestartError{ReasonCode: "competing_transition", Err: ErrPrivateSupervisorTransitionIncomplete}
	}
	if err = runtimeRestartRecoveryAvailable(ctx, root, from, binding); err != nil {
		return err
	}
	_, publishing, err := readPrivateIdentityPublication(root)
	if err != nil {
		return err
	}
	if publishing {
		return &PrivateRuntimeRestartError{ReasonCode: "competing_transition", Err: ErrPrivateSupervisorTransitionIncomplete}
	}
	return nil
}

func runtimeRestartRecoveryAvailable(ctx context.Context, root string, from RuntimeRestartReference, binding task.SupervisorRef) error {
	alias, present, err := readValidatedPrivateRecoveryAlias(root)
	if err != nil {
		return err
	}
	publishing, err := privateRecoveryObservationConflict(root, present)
	if err != nil {
		return err
	}
	if publishing {
		return &PrivateRuntimeRestartError{ReasonCode: "competing_transition", Err: ErrPrivateSupervisorTransitionIncomplete}
	}
	if !present {
		return nil
	}
	// The mandatory root-wide authorizer has already succeeded while this EX
	// remains held. Reuse the alias owner's read-only retirement predicate; this
	// callback revalidates that scope, never supplies default quiescence policy.
	facts, err := runtimeRestartAliasFacts(ctx, root, from, binding)
	if err != nil {
		return err
	}
	return authorizeObservedPrivateRecoveryAliasRetirement(ctx, root, binding, &alias.record, func(observation context.Context) error {
		return requireRuntimeRestartBootstrapEX(observation, root)
	}, facts)
}

func replaceRuntimeRestartAcceptance(root string, current privateBootstrapRecord, candidate privateRuntimeRestartRecord) (privateRuntimeRestartRecord, error) {
	if current.RuntimeRestart == nil {
		return privateRuntimeRestartRecord{}, ErrPrivateSupervisorTransitionIncomplete
	}
	previous := *current.RuntimeRestart
	if previous.From == candidate.From {
		if previous.Binding != candidate.Binding || previous.OldPID != candidate.OldPID || previous.OldProcessStart != candidate.OldProcessStart {
			return privateRuntimeRestartRecord{}, ErrBinding
		}
		return previous, syncPrivateDirectory(filepath.Join(root, privateSupervisorDirectory))
	}
	if previous.Phase != RuntimeRestartReady {
		return privateRuntimeRestartRecord{}, ErrPrivateRuntimeRestartPending
	}
	if err := writeRuntimeRestartReplacement(root, candidate); err != nil {
		return privateRuntimeRestartRecord{}, err
	}
	return candidate, nil
}

func writeRuntimeRestartReplacement(root string, candidate privateRuntimeRestartRecord) error {
	data, err := task.MarshalCanonical(privateBootstrapRecord{SchemaVersion: task.SchemaVersion, RuntimeRestart: &candidate})
	if err != nil {
		return err
	}
	return replacePrivateRecordAtomic(filepath.Join(root, privateSupervisorDirectory), privateBootstrapRecordPath(root),
		".private-runtime-restart-", "private runtime restart journal", data, true)
}

// RequireRuntimeRestartBootstrap validates the borrowed live bootstrap EX.
// It neither acquires a lock nor grants startup or generation authority.
func RequireRuntimeRestartBootstrap(ctx context.Context, root string) error {
	return requireRuntimeRestartBootstrapEX(ctx, root)
}

// PrepareRuntimeRestartWithLock opens only the exact published installed pair.
// The caller retains the actual existing bootstrap EX. Unlike ordinary
// preparation this permits its matching runtime fence, never config repair,
// provisioning, identity replay, daemon startup, or another transition.
func PrepareRuntimeRestartWithLock(ctx context.Context, root string, from RuntimeRestartReference, options Options) (*Client, error) {
	if err := requireRuntimeRestartBootstrapEX(ctx, root); err != nil {
		return nil, err
	}
	if err := validateRuntimeRestartReference(from); err != nil {
		return nil, err
	}
	bootstrap, found, err := readPrivateBootstrapRecord(root)
	if err != nil {
		return nil, err
	}
	if found && bootstrap.RuntimeRestart == nil {
		return nil, ErrPrivateSupervisorTransitionIncomplete
	}
	if runtimeRestartPending(bootstrap) && bootstrap.RuntimeRestart.From != from {
		return nil, runtimeRestartReferenceNotRetained()
	}
	binding, present, err := loadPrivateSupervisorBinding(root)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, ErrPrivateSupervisorNotInitialized
	}
	if bootstrap.RuntimeRestart != nil && bootstrap.RuntimeRestart.From == from && bootstrap.RuntimeRestart.Binding != binding {
		return nil, ErrBinding
	}
	client, pairErr, err := preparePublishedPrivatePair(ctx, binding, options)
	return client, errors.Join(pairErr, err)
}

func runtimeRestartReferenceNotRetained() error {
	return &PrivateRuntimeRestartError{ReasonCode: "restart_reference_not_retained", Err: ErrPrivateRuntimeRestartReferenceNotRetained}
}

// QuiesceForRuntimeRestart accepts or reconciles exactly one restart intent.
// Infrastructure supplies the current startup authority in the required
// authorizer and retains the daemon's actual Wait. This method proves only the
// recorded process/endpoint closure; it never retires or joins a resident.
func (c *Client) QuiesceForRuntimeRestart(ctx context.Context, root string, from RuntimeRestartReference) (PrivateRuntimeRestartObservation, error) {
	if err := c.validateRuntimeRestartClient(ctx, root, from); err != nil {
		return PrivateRuntimeRestartObservation{}, err
	}
	bootstrap, found, err := readPrivateBootstrapRecord(root)
	if err != nil {
		return PrivateRuntimeRestartObservation{}, err
	}
	if found && bootstrap.RuntimeRestart == nil {
		return PrivateRuntimeRestartObservation{}, ErrPrivateSupervisorTransitionIncomplete
	}
	if bootstrap.RuntimeRestart != nil && bootstrap.RuntimeRestart.From == from {
		return c.reconcileRuntimeRestartQuiescence(ctx, root, *bootstrap.RuntimeRestart)
	}
	if runtimeRestartPending(bootstrap) {
		return PrivateRuntimeRestartObservation{}, runtimeRestartReferenceNotRetained()
	}
	snapshot, observation, err := privateClientObserveSnapshot(ctx, c, false)
	if err != nil {
		return PrivateRuntimeRestartObservation{}, err
	}
	if !observation.ready() {
		return PrivateRuntimeRestartObservation{}, errors.Join(ErrBinding, observation.projectedError())
	}
	process, err := c.captureRuntimeRestartPredecessor(ctx, root)
	if err != nil {
		return PrivateRuntimeRestartObservation{}, err
	}
	record, err := c.acceptPrivateRuntimeRestart(ctx, root, from, process, snapshot.Jobs)
	if err != nil {
		return PrivateRuntimeRestartObservation{}, err
	}
	return c.quiesceRuntimeRestartDaemon(ctx, root, record, true)
}

func (c *Client) validateRuntimeRestartClient(ctx context.Context, root string, from RuntimeRestartReference) error {
	if err := requireRuntimeRestartBootstrapEX(ctx, root); err != nil {
		return err
	}
	if err := validateRuntimeRestartReference(from); err != nil {
		return err
	}
	if c == nil || c.options.PrivateRuntimeRestartAuthorizer == nil {
		return ErrPrivateRuntimeRestartAuthorization
	}
	if c.options.ControlOwnerExecutable == "" {
		return ErrInfrastructureOwnerRequired
	}
	if !IsPrivateConfig(root, c.binding.ConfigPath) || c.runtimeBinding != c.binding {
		return ErrBinding
	}
	if err := c.verify(ctx); err != nil {
		return err
	}
	published, found, err := loadPrivateSupervisorBinding(root)
	if err != nil || !found || published != c.binding {
		return errors.Join(ErrBinding, err)
	}
	identity, found, err := loadPrivateDaemonIdentity(filepath.Join(root, privateSupervisorDirectory))
	if err != nil || !found || !privateDaemonIdentityMatchesBinding(identity, c.binding) {
		return errors.Join(ErrBinding, err)
	}
	return nil
}

func (c *Client) captureRuntimeRestartPredecessor(ctx context.Context, root string) (privateProcessIdentity, error) {
	owner, ok := c.options.DaemonWaitOwner.(DaemonWaitCustodyOwner)
	if !ok {
		return privateProcessIdentity{}, ErrInfrastructureOwnerRequired
	}
	pid, found, err := c.privateDaemonPID()
	if err != nil || !found {
		return privateProcessIdentity{}, errors.Join(ErrBinding, err)
	}
	// Use the canonical native identity, including Darwin's stable uniqueid.
	// The PID file alone cannot identify a child or establish Wait custody.
	birth, err := lifetime.ObserveProcess(pid)
	if err != nil {
		return privateProcessIdentity{}, errors.Join(ErrBinding, err)
	}
	// This registry port acquires the resident mutex itself. It must run before
	// acceptance, outside the non-reentrant generation commit callback.
	if err = owner.RequireDaemonWaitCustody(ctx, root, birth); err != nil {
		return privateProcessIdentity{}, err
	}
	return privateProcessIdentity{PID: birth.PID(), StartToken: birth.StartToken()}, nil
}

func (c *Client) reconcileRuntimeRestartQuiescence(ctx context.Context, root string, record privateRuntimeRestartRecord) (PrivateRuntimeRestartObservation, error) {
	if record.Binding != c.binding {
		return record.observation(), ErrBinding
	}
	switch record.Phase {
	case RuntimeRestartDaemonClosed, RuntimeRestartSuccessorAccepted, RuntimeRestartReady:
		// Reconciliation never turns the successor into another predecessor.
		return record.observation(), nil
	case RuntimeRestartRequested, RuntimeRestartShutdownPending:
		snapshot, observation, err := privateClientObserveSnapshot(ctx, c, false)
		if err != nil {
			return record.observation(), err
		}
		if err = c.options.PrivateRuntimeRestartAuthorizer(ctx, root, record.From, c.binding, append([]Job(nil), snapshot.Jobs...)); err != nil {
			return record.observation(), err
		}
		if err = runtimeRestartAcceptanceAvailable(ctx, root, record.From, c.binding); err != nil {
			return record.observation(), err
		}
		return c.quiesceRuntimeRestartDaemon(ctx, root, record, observation.ready())
	default:
		return record.observation(), ErrBinding
	}
}

func (c *Client) quiesceRuntimeRestartDaemon(ctx context.Context, root string, record privateRuntimeRestartRecord, reachable bool) (PrivateRuntimeRestartObservation, error) {
	process := privateProcessIdentity{PID: record.OldPID, StartToken: record.OldProcessStart}
	absent, err := c.privateProcessUnavailable(process)
	if err != nil {
		return record.observation(), err
	}
	if !absent {
		if !reachable {
			return record.observation(), runtimeRestartPendingError("daemon_shutdown_pending", nil)
		}
		if err = c.requireRuntimeRestartPredecessor(process); err != nil {
			return record.observation(), err
		}
		if err = guardPrivateGeneration(ctx, root, c.options.PrivateGenerationGuard, func() error {
			return claimRuntimeRestartShutdown(ctx, root)
		}); err != nil {
			return record.observation(), err
		}
		command, shutdownErr := c.runtimeRestartShutdown(ctx)
		if command.invoked && record.Phase != RuntimeRestartShutdownPending {
			pending := record
			pending.Phase = RuntimeRestartShutdownPending
			if err = c.replaceRuntimeRestartRecord(ctx, root, record, pending); err != nil {
				return record.observation(), errors.Join(err, shutdownErr)
			}
			record = pending
		}
		if shutdownErr != nil {
			return record.observation(), shutdownErr
		}
		if !command.dispatched {
			return record.observation(), errors.Join(ErrUnknown, ErrPrivateRuntimeRestartPending)
		}
	}
	if err = c.waitUnavailableWithProcess(ctx, process); err != nil {
		return record.observation(), runtimeRestartPendingError("daemon_shutdown_pending", err)
	}
	closed := record
	closed.Phase = RuntimeRestartDaemonClosed
	if err = c.replaceRuntimeRestartRecord(ctx, root, record, closed); err != nil {
		return record.observation(), err
	}
	return closed.observation(), nil
}

func claimRuntimeRestartShutdown(ctx context.Context, root string) error {
	if err := requireRuntimeRestartBootstrapEX(ctx, root); err != nil {
		return err
	}
	bootstrap, ok := ctx.Value(privateBootstrapEXKey{}).(*privateBootstrapEX)
	if !ok || bootstrap == nil {
		return ErrBinding
	}
	return bootstrap.consume(func() error {
		if bootstrap.runtimeShutdownAttempted {
			return runtimeRestartPendingError("daemon_shutdown_pending", nil)
		}
		// This scope owns one attempt, including a proven pre-Start failure.
		// Only another independent EX acquisition can admit another attempt.
		bootstrap.runtimeShutdownAttempted = true
		return nil
	})
}

func (c *Client) runtimeRestartShutdown(ctx context.Context) (privateShutdownCommand, error) {
	proof := &controlCommandSettlement{}
	command, err := c.shutdownCommand(ctx, proof)
	if err == nil {
		if !command.dispatched || command.result.ExitCode != 0 {
			return command, ErrUnknown
		}
		return command, nil
	}
	return command, runtimeRestartShutdownFailure(err, proof)
}

func runtimeRestartShutdownFailure(err error, proof *controlCommandSettlement) error {
	if pendingFrom(err) != nil {
		return runtimeRestartPendingError("daemon_shutdown_pending", err)
	}
	if runtimeRestartOrdinaryShutdownFailure(err, proof) {
		return &PrivateRuntimeRestartError{ReasonCode: "daemon_shutdown_retry", Err: ErrPrivateRuntimeRestartPending, diagnostic: err}
	}
	if task.AllErrorCauses(err, func(cause error) bool {
		return exactRuntimeShutdownCancellation(cause)
	}) {
		return runtimeRestartPendingError("daemon_shutdown_pending", err)
	}
	return err
}

func exactRuntimeShutdownCancellation(cause error) bool {
	return runtimeRestartReadinessObservationCause(cause) && !errors.Is(cause, ErrInFlight)
}

func runtimeRestartOrdinaryShutdownFailure(err error, proof *controlCommandSettlement) bool {
	if pendingFrom(err) != nil || errors.Is(err, ErrControlLimit) || task.AnyErrorNode(err, func(node error) bool {
		_, retained := node.(interface{ RetainsResidentCleanup() })
		return retained
	}) {
		return false
	}
	return proof.permitsOrdinaryFailure(err)
}

func (c *Client) requireRuntimeRestartPredecessor(process privateProcessIdentity) error {
	pid, found, err := c.privateDaemonPID()
	if err != nil || !found || pid != process.PID {
		return errors.Join(ErrBinding, err)
	}
	absent, err := c.privateProcessUnavailable(process)
	if err != nil || absent {
		return errors.Join(ErrBinding, err)
	}
	return nil
}

func runtimeRestartPendingError(reason string, cause error) error {
	return &PrivateRuntimeRestartError{ReasonCode: reason, Err: errors.Join(ErrPrivateRuntimeRestartPending, cause)}
}

func (c *Client) replaceRuntimeRestartRecord(ctx context.Context, root string, expected, replacement privateRuntimeRestartRecord) error {
	if err := validatePrivateRuntimeRestartRecord(root, replacement); err != nil {
		return err
	}
	return guardPrivateGeneration(ctx, root, c.options.PrivateGenerationGuard, func() error {
		if err := requireRuntimeRestartBootstrapEX(ctx, root); err != nil {
			return err
		}
		current, found, err := readPrivateBootstrapRecord(root)
		if err != nil || !found || current.RuntimeRestart == nil || *current.RuntimeRestart != expected {
			return errors.Join(ErrBinding, err)
		}
		published, found, err := loadPrivateSupervisorBinding(root)
		if err != nil || !found || published != expected.Binding || replacement.Binding != expected.Binding || replacement.From != expected.From {
			return errors.Join(ErrBinding, err)
		}
		return writeRuntimeRestartReplacement(root, replacement)
	})
}

// StartRuntimeRestartSuccessor completes only this accepted handoff. Required
// infrastructure authorization proves exact original-owner closure and outside
// provenance; a correlation token can never provide either fact. The actual
// Start still uses the canonical observed resident start guard and sole Wait.
func (c *Client) StartRuntimeRestartSuccessor(ctx context.Context, root string, from, successor RuntimeRestartReference) (PrivateRuntimeRestartObservation, error) {
	record, err := c.runtimeRestartSuccessorRecord(ctx, root, from, successor)
	if err != nil {
		return record.observation(), err
	}
	if record.Phase == RuntimeRestartReady {
		return record.observation(), nil
	}
	if record.Phase != RuntimeRestartDaemonClosed && record.Phase != RuntimeRestartSuccessorAccepted {
		return record.observation(), runtimeRestartPendingError("daemon_shutdown_pending", nil)
	}
	if err = c.authorizeRuntimeRestartSuccessor(ctx, root, record, successor); err != nil {
		return record.observation(), err
	}
	scope := &privateRuntimeRestartStart{root: root, record: record}
	if record.Phase == RuntimeRestartSuccessorAccepted {
		if record.NewPID == 0 {
			// Acceptance/Start ambiguity survives a caller or owner crash. A
			// fresh PID-file read cannot recover canonical birth/Wait custody.
			return record.observation(), runtimeRestartPendingError("successor_birth_unknown", ErrUnknown)
		}
		err = c.completeRuntimeRestartReady(ctx, root, scope)
		return scope.record.observation(), err
	}
	if err = c.requireRuntimeRestartSuccessorPorts(); err != nil {
		return record.observation(), err
	}
	if err = c.requireRuntimeRestartOldClosed(ctx, record); err != nil {
		return record.observation(), err
	}
	accepted := record
	accepted.Phase, accepted.Successor = RuntimeRestartSuccessorAccepted, successor
	if err = c.replaceRuntimeRestartRecord(ctx, root, record, accepted); err != nil {
		return record.observation(), err
	}
	scope.record = accepted
	err = c.startPrivateForRuntime(ctx, root, scope)
	return scope.record.observation(), err
}

func (c *Client) runtimeRestartSuccessorRecord(ctx context.Context, root string, from, successor RuntimeRestartReference) (privateRuntimeRestartRecord, error) {
	if err := c.validateRuntimeRestartClient(ctx, root, from); err != nil {
		return privateRuntimeRestartRecord{}, err
	}
	if err := validateRuntimeRestartReference(successor); err != nil || successor == from {
		return privateRuntimeRestartRecord{}, errors.Join(ErrBinding, err)
	}
	bootstrap, found, err := readPrivateBootstrapRecord(root)
	if err != nil {
		return privateRuntimeRestartRecord{}, err
	}
	if !found || bootstrap.RuntimeRestart == nil || bootstrap.RuntimeRestart.From != from {
		return privateRuntimeRestartRecord{}, runtimeRestartReferenceNotRetained()
	}
	record := *bootstrap.RuntimeRestart
	if record.Binding != c.binding {
		return record, ErrBinding
	}
	if record.Successor != "" && record.Successor != successor {
		return record, runtimeRestartReferenceNotRetained()
	}
	return record, nil
}

func (c *Client) authorizeRuntimeRestartSuccessor(ctx context.Context, root string, record privateRuntimeRestartRecord, successor RuntimeRestartReference) error {
	if c.options.PrivateRuntimeRestartSuccessorAuthorizer == nil {
		return ErrPrivateRuntimeRestartAuthorization
	}
	if err := c.options.PrivateRuntimeRestartSuccessorAuthorizer(ctx, root, record.From, successor, record.Binding); err != nil {
		return err
	}
	if err := c.options.PrivateRuntimeRestartAuthorizer(ctx, root, record.From, record.Binding, nil); err != nil {
		return err
	}
	return runtimeRestartAcceptanceAvailable(ctx, root, record.From, record.Binding)
}

// This owner-local scope is not a public resume permit. Every use checks the
// same immutable accepted record under the still-live actual bootstrap EX.
type privateRuntimeRestartStart struct {
	root   string
	record privateRuntimeRestartRecord
}

func (scope *privateRuntimeRestartStart) authorizeStart(ctx context.Context, root string) error {
	if scope == nil {
		return refusePendingPrivateRestart(root)
	}
	if scope.root != root || scope.record.Phase != RuntimeRestartSuccessorAccepted || scope.record.NewPID != 0 {
		return ErrBinding
	}
	if err := requireRuntimeRestartBootstrapEX(ctx, root); err != nil {
		return err
	}
	bootstrap, found, err := readPrivateBootstrapRecord(root)
	if err != nil || !found || bootstrap.RuntimeRestart == nil || *bootstrap.RuntimeRestart != scope.record {
		return errors.Join(ErrBinding, err)
	}
	return nil
}

func (scope *privateRuntimeRestartStart) recordBirth(ctx context.Context, client *Client, started privateDaemonStart) error {
	if started.birthReady == nil || started.birth == nil {
		return runtimeRestartPendingError("successor_birth_unknown", ErrUnknown)
	}
	select {
	case <-started.birthReady:
	case <-ctx.Done():
		return runtimeRestartPendingError("successor_birth_unknown", ctx.Err())
	}
	birth, ready, err := started.birth()
	if err != nil || !ready {
		return runtimeRestartPendingError("successor_birth_unknown", errors.Join(ErrUnknown, err))
	}
	updated := scope.record
	updated.NewPID, updated.NewProcessStart = birth.PID(), birth.StartToken()
	if err = client.replaceRuntimeRestartRecord(ctx, scope.root, scope.record, updated); err != nil {
		return err
	}
	scope.record = updated
	return nil
}

func (c *Client) requireRuntimeRestartOldClosed(ctx context.Context, record privateRuntimeRestartRecord) error {
	process := privateProcessIdentity{PID: record.OldPID, StartToken: record.OldProcessStart}
	absent, err := c.privateProcessUnavailable(process)
	if err != nil {
		return err
	}
	if !absent {
		return runtimeRestartPendingError("daemon_shutdown_pending", nil)
	}
	path, err := privateEndpointSocketPath(c.runtimeBinding.Endpoint)
	if err != nil {
		return err
	}
	unavailable, err := c.privateEndpointUnavailable(ctx, path, process, true, true)
	if err != nil {
		return err
	}
	if !unavailable {
		return runtimeRestartPendingError("endpoint_still_available", nil)
	}
	return nil
}

func (c *Client) completeRuntimeRestartReady(ctx context.Context, root string, scope *privateRuntimeRestartStart) error {
	record := scope.record
	if record.Phase != RuntimeRestartSuccessorAccepted || record.NewPID == 0 {
		return ErrBinding
	}
	if err := c.authorizeRuntimeRestartSuccessor(ctx, root, record, record.Successor); err != nil {
		return err
	}
	birth, err := lifetime.NewProcessRef(record.NewPID, record.NewProcessStart)
	if err != nil {
		return err
	}
	owner, ok := c.options.DaemonWaitOwner.(DaemonWaitCustodyOwner)
	if !ok {
		return ErrInfrastructureOwnerRequired
	}
	if err = owner.RequireDaemonWaitCustody(ctx, root, birth); err != nil {
		return err
	}
	present, err := lifetime.ProcessPresent(birth)
	if err != nil || !present {
		return runtimeRestartPendingError("successor_not_ready", errors.Join(ErrUnknown, err))
	}
	pid, found, err := c.privateDaemonPID()
	if err != nil || !found || pid != birth.PID() {
		return errors.Join(ErrBinding, err)
	}
	if err = c.runtimeRestartReadiness(ctx); err != nil {
		return runtimeRestartReadinessFailure(err)
	}
	completed := record
	completed.Phase = RuntimeRestartReady
	if err = c.replaceRuntimeRestartRecord(ctx, root, record, completed); err != nil {
		return err
	}
	scope.record = completed
	return nil
}

func (c *Client) requireRuntimeRestartSuccessorPorts() error {
	if c.options.DaemonStartGuard == nil || c.options.PrivateGenerationGuard == nil {
		return ErrInfrastructureOwnerRequired
	}
	if _, ok := c.options.DaemonWaitOwner.(DaemonWaitBirthOwner); !ok {
		return ErrInfrastructureOwnerRequired
	}
	if _, ok := c.options.DaemonWaitOwner.(DaemonWaitCustodyOwner); !ok {
		return ErrInfrastructureOwnerRequired
	}
	return nil
}

func (scope *privateRuntimeRestartStart) releaseUnstarted(ctx context.Context, client *Client) error {
	if scope.record.Phase != RuntimeRestartSuccessorAccepted || scope.record.NewPID != 0 {
		return ErrBinding
	}
	released := scope.record
	released.Phase, released.Successor = RuntimeRestartDaemonClosed, ""
	if err := client.replaceRuntimeRestartRecord(ctx, scope.root, scope.record, released); err != nil {
		return err
	}
	scope.record = released
	return nil
}

// Only this exact accepted runtime purpose can observe its alias through its
// own admission fence. Other consumers and every independent transition keep
// their original facts. This grants no mutation or generic resume permission.
func runtimeRestartAliasFacts(ctx context.Context, root string, from RuntimeRestartReference, binding task.SupervisorRef) (privateGenerationFacts, error) {
	if err := requireRuntimeRestartBootstrapEX(ctx, root); err != nil {
		return privateGenerationFacts{}, err
	}
	facts, err := loadPrivateGenerationFacts(root)
	if err != nil {
		return privateGenerationFacts{}, err
	}
	bootstrap, _, err := readPrivateBootstrapRecord(root)
	if err != nil {
		return privateGenerationFacts{}, err
	}
	if !runtimeRestartPending(bootstrap) {
		return facts, nil
	}
	if bootstrap.RuntimeRestart.From != from || bootstrap.RuntimeRestart.Binding != binding {
		return privateGenerationFacts{}, runtimeRestartReferenceNotRetained()
	}
	_, publishing, err := readPrivateIdentityPublication(root)
	if err != nil || publishing {
		return privateGenerationFacts{}, errors.Join(ErrPrivateSupervisorTransitionIncomplete, err)
	}
	// The actual EX, matching authoritative journal and independent publication
	// check prove the sole missing settled fact is this operation's own fence.
	facts.runtimeRestartPending = false
	facts.settled = true
	return facts, nil
}

// A born successor's observation can expire while its actual Wait and birth
// remain owned. Never reclassify that expiry as a changed binding. Ordinary
// Ready keeps its historical binding projection; this purpose uses the same
// fresh binding check, command and schema validator without that broad wrapper.
func (c *Client) runtimeRestartReadiness(ctx context.Context) error {
	result, err := c.readyCommand(ctx)
	if err != nil {
		return err
	}
	return c.validateReadyResult(result)
}

func (scope *privateRuntimeRestartStart) waitReady(ctx context.Context, client *Client) error {
	if scope.record.Phase != RuntimeRestartSuccessorAccepted || scope.record.NewPID == 0 {
		return ErrBinding
	}
	err := waitPrivateReadyWithProbe(ctx, client, client.options.ObservationTimeout, false, client.runtimeRestartReadiness)
	return runtimeRestartReadinessFailure(err)
}

func runtimeRestartReadinessFailure(err error) error {
	if task.AnyErrorNode(err, func(node error) bool {
		_, retained := node.(interface{ RetainsResidentCleanup() })
		return retained
	}) {
		return err
	}
	// Only these exact observation leaves are safe. A deadline joined with a
	// binding, schema, storage, guard or unknown fault retains that whole fault.
	if task.AllErrorCauses(err, runtimeRestartReadinessObservationCause) {
		return runtimeRestartPendingError("successor_not_ready", err)
	}
	return err
}

func runtimeRestartReadinessObservationCause(cause error) bool {
	value := reflect.ValueOf(cause)
	if !value.IsValid() || !value.Comparable() {
		return false
	}
	for _, allowed := range []error{context.DeadlineExceeded, context.Canceled, ErrInFlight} {
		candidate := reflect.ValueOf(allowed)
		if value.Type() == candidate.Type() && value.Equal(candidate) {
			return true
		}
	}
	return false
}

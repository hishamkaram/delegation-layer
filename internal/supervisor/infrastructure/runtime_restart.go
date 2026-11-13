package infrastructure

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const runtimeRestartProtocolVersion = 1

// RuntimeRestartReference identifies the original startup before restart is
// requested. It is correlation and a precondition, never a native grant.
type RuntimeRestartReference struct {
	RootID  string           `json:"root_id"`
	Startup StartupReference `json:"startup"`
}

func (r RuntimeRestartReference) Validate() error {
	return errors.Join(task.ValidateRootID(r.RootID), r.Startup.Validate())
}

// FormatRuntimeRestartReference produces the sole public token representation.
func FormatRuntimeRestartReference(reference RuntimeRestartReference) (string, error) {
	if err := reference.Validate(); err != nil {
		return "", err
	}
	startup := reference.Startup
	return strings.Join([]string{"v1", reference.RootID, strconv.FormatUint(startup.Sequence, 10), startup.Epoch, startup.IntentSHA256}, ":"), nil
}

func ParseRuntimeRestartReference(value string) (RuntimeRestartReference, error) {
	// Five canonical fields, including at most 20 decimal uint64 digits.
	if len(value) > 154 {
		return RuntimeRestartReference{}, ErrIdentity
	}
	parts := strings.Split(value, ":")
	if len(parts) != 5 || parts[0] != "v1" {
		return RuntimeRestartReference{}, ErrIdentity
	}
	sequence, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil || sequence == 0 || strconv.FormatUint(sequence, 10) != parts[2] {
		return RuntimeRestartReference{}, ErrIdentity
	}
	reference := RuntimeRestartReference{RootID: parts[1], Startup: StartupReference{Sequence: sequence, Epoch: parts[3], IntentSHA256: parts[4]}}
	if err = reference.Validate(); err != nil {
		return RuntimeRestartReference{}, ErrIdentity
	}
	return reference, nil
}

func validateRuntimeRestartBirth(intent StartupIntent, environment []string) error {
	if intent.Inputs.Initial.Kind != InitialRuntimeRestart {
		return nil
	}
	if intent.Inputs.RuntimeProducer != producerOutside {
		return ErrOwnerRequest
	}
	_, found, err := ReadOrigin(environment)
	if err != nil || found {
		return ErrOwnerRequest
	}
	return nil
}

func validateRuntimeRestartRequest(operation Operation, request ownerRequest) error {
	if request.Action != ownerRuntimeRestart {
		return nil
	}
	current, err := ReferenceStartup(operation.Intent())
	if err != nil {
		return err
	}
	if request.RestartFrom == current {
		return nil
	}
	inputs := operation.Intent().Inputs
	if inputs.Initial.Kind != InitialRuntimeRestart || inputs.Initial.RestartFrom != request.RestartFrom || inputs.RuntimeProducer != producerOutside {
		return ErrOwnerRequest
	}
	return nil
}

// ValidateRuntimeRestartReference reads only the named immutable startup. A
// newer head cannot change its subject or turn a stale reference into a restart.
func ValidateRuntimeRestartReference(ctx context.Context, reader *taskdir.ReadOnlyStore, reference RuntimeRestartReference) error {
	if ctx == nil || reader == nil {
		return os.ErrInvalid
	}
	if err := reference.Validate(); err != nil {
		return err
	}
	pin, err := reader.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if pin.RootID() != reference.RootID {
		return ErrIdentity
	}
	intent, err := (passiveIntentReader{store: reader}).read(ctx, intentName(reference.Startup.Sequence))
	if err != nil {
		return err
	}
	if err = validateHistoricalIntent(intent, pin); err != nil {
		return err
	}
	if intent.Inputs.RootDevice != pin.Device() || intent.Inputs.RootInode != pin.Inode() {
		return ErrIdentity
	}
	if err = reference.Startup.validateIntent(intent); err != nil {
		return err
	}
	after, err := reader.PinRootContext(ctx)
	if err != nil {
		return err
	}
	if !pin.Equal(after) {
		return ErrIdentity
	}
	return ctx.Err()
}

// ReadRuntimeRestartSuccessorContext validates the named historical successor's
// immutable purpose and context. It grants no live native or Start authority.
func ReadRuntimeRestartSuccessorContext(ctx context.Context, reader *taskdir.ReadOnlyStore, from, successor RuntimeRestartReference) (LaunchContextReference, error) {
	if from.RootID != successor.RootID || successor.Startup.Sequence <= from.Startup.Sequence {
		return LaunchContextReference{}, ErrIdentity
	}
	if err := ValidateRuntimeRestartReference(ctx, reader, from); err != nil {
		return LaunchContextReference{}, err
	}
	if err := ValidateRuntimeRestartReference(ctx, reader, successor); err != nil {
		return LaunchContextReference{}, err
	}
	intent, err := (passiveIntentReader{store: reader}).read(ctx, intentName(successor.Startup.Sequence))
	if err != nil {
		return LaunchContextReference{}, err
	}
	if err = successor.Startup.validateIntent(intent); err != nil {
		return LaunchContextReference{}, err
	}
	if intent.Inputs.Initial.Kind != InitialRuntimeRestart || intent.Inputs.Initial.RestartFrom != from.Startup {
		return LaunchContextReference{}, ErrIdentity
	}
	return intent.Inputs.Context, nil
}

type RuntimeRestartPhase string

const (
	RuntimeRestartRequested         RuntimeRestartPhase = "requested"
	RuntimeRestartShutdownPending   RuntimeRestartPhase = "shutdown_pending"
	RuntimeRestartDaemonClosed      RuntimeRestartPhase = "daemon_closed"
	RuntimeRestartSuccessorAccepted RuntimeRestartPhase = "successor_accepted"
	RuntimeRestartReady             RuntimeRestartPhase = "ready"
)

// RuntimeRestartReport is a validated observation of the control journal.
// Neither this projection nor a ready phase establishes provider authentication.
type RuntimeRestartReport struct {
	From      StartupReference    `json:"from"`
	Phase     RuntimeRestartPhase `json:"phase"`
	Successor StartupReference    `json:"successor,omitzero"`
}

func (r RuntimeRestartReport) Validate() error {
	if r == (RuntimeRestartReport{}) {
		return nil
	}
	if err := r.From.Validate(); err != nil {
		return err
	}
	switch r.Phase {
	case RuntimeRestartRequested, RuntimeRestartShutdownPending, RuntimeRestartDaemonClosed:
		if r.Successor != (StartupReference{}) {
			return ErrIdentity
		}
	case RuntimeRestartSuccessorAccepted, RuntimeRestartReady:
		if err := r.Successor.Validate(); err != nil {
			return err
		}
		if r.Successor.Sequence <= r.From.Sequence {
			return ErrIdentity
		}
	default:
		return ErrIdentity
	}
	return nil
}

func (r RuntimeRestartReport) ValidateCondition(condition PreparationCondition) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r != (RuntimeRestartReport{}) && condition == PreparationOK && r.Phase != RuntimeRestartDaemonClosed && r.Phase != RuntimeRestartReady {
		return ErrIdentity
	}
	return nil
}

// A disposition reports supervisor readiness. Quiescence, shutdown and an
// accepted successor do not establish it, including in partial error results.
func (r RuntimeRestartReport) ValidateDisposition(disposition Disposition) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if disposition != (Disposition{}) && r.Phase != RuntimeRestartReady {
		return ErrIdentity
	}
	return nil
}

func (r ownerReply) validateRuntimeRestartResult() error {
	if err := r.RuntimeRestart.ValidateCondition(PreparationCondition(r.Code)); err != nil {
		return err
	}
	if r.Action != ownerRuntimeRestart {
		if r.RuntimeRestart != (RuntimeRestartReport{}) {
			return ErrIdentity
		}
		return nil
	}
	if err := r.RuntimeRestart.ValidateDisposition(r.Disposition); err != nil {
		return err
	}
	if r.Code == ownerOK && r.RuntimeRestart == (RuntimeRestartReport{}) {
		return ErrIdentity
	}
	if r.RuntimeRestart == (RuntimeRestartReport{}) {
		return nil
	}
	if r.RuntimeRestart.From != r.RestartFrom {
		return ErrIdentity
	}
	if r.RuntimeRestart.Successor != (StartupReference{}) && r.RuntimeRestart.Successor != r.OwnerRef {
		return ErrIdentity
	}
	return nil
}

type RuntimeRestartCustody string

const (
	RuntimeRestartCustodyAvailable   RuntimeRestartCustody = "available"
	RuntimeRestartCustodyUnavailable RuntimeRestartCustody = "unavailable"
	RuntimeRestartCustodyUnknown     RuntimeRestartCustody = "unknown"
)

func (c RuntimeRestartCustody) Validate() error {
	switch c {
	case RuntimeRestartCustodyAvailable, RuntimeRestartCustodyUnavailable, RuntimeRestartCustodyUnknown:
		return nil
	default:
		return ErrIdentity
	}
}

// RuntimeRestartPreflight is a dated custody observation, never permission to
// stop/start, root-wide quiescence, admission readiness, or provider login.
type RuntimeRestartPreflight struct {
	Protocol   int                     `json:"protocol"`
	From       RuntimeRestartReference `json:"from"`
	Custody    RuntimeRestartCustody   `json:"custody"`
	ObservedAt string                  `json:"observed_at"`
}

func (p RuntimeRestartPreflight) Validate() error {
	if p.Protocol != runtimeRestartProtocolVersion {
		return ErrOwnerIncompatible
	}
	_, stampErr := canonicalStartupTime(p.ObservedAt)
	return errors.Join(p.From.Validate(), p.Custody.Validate(), stampErr)
}

// ReadRuntimeRestartPreflight contacts the selected current resident once.
// Expected zero selects the current startup; a nonzero value never falls back.
func ReadRuntimeRestartPreflight(ctx context.Context, reader *taskdir.ReadOnlyStore, expected StartupReference) (RuntimeRestartPreflight, error) {
	request, pin, err := currentOwnerRequestAtStartup(ctx, reader, ownerRuntimeRestartPreflight, expected)
	if err != nil {
		return RuntimeRestartPreflight{}, err
	}
	reply, err := classifiedOwnerContact(ctx, reader, pin, request)
	return reply.RestartPreflight, err
}

func (s *ownerServer) executeRuntimeRestartPreflight(ctx context.Context, work acceptedOwnerWork, reply ownerReply) ownerReply {
	custody, err := s.host.ObserveRuntimeRestartCustody(ctx, work.operation, work.selected.Clone(), s.waits)
	if invalid := custody.Validate(); invalid != nil {
		err = errors.Join(err, invalid)
	}
	if err != nil {
		custody = RuntimeRestartCustodyUnknown
		reply.Code = ownerFault
		s.connectionFault(err)
	}
	reply.RestartPreflight = RuntimeRestartPreflight{
		Protocol: runtimeRestartProtocolVersion,
		From:     RuntimeRestartReference{RootID: work.request.RootID, Startup: work.request.Owner},
		Custody:  custody, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	return reply
}

func (r ownerReply) validateRuntimeRestartPreflight() error {
	if r.Action != ownerRuntimeRestartPreflight {
		if r.RestartPreflight != (RuntimeRestartPreflight{}) {
			return ErrIdentity
		}
		return nil
	}
	if r.RestartPreflight == (RuntimeRestartPreflight{}) {
		if r.Code == ownerOK {
			return ErrIdentity
		}
		return nil
	}
	if err := r.RestartPreflight.Validate(); err != nil {
		return err
	}
	if r.RestartPreflight.From != (RuntimeRestartReference{RootID: r.RootID, Startup: r.OwnerRef}) || r.Code != ownerOK && r.RestartPreflight.Custody != RuntimeRestartCustodyUnknown {
		return ErrIdentity
	}
	return nil
}

// The control journal remains authoritative. This server-owned gate is derived
// only from its validated runtime observations, and is cleared by a verified
// Ready result. It admits same-From reconciliation, never ordinary mutations.
func (s *ownerServer) observeRuntimePreparation(from StartupReference, outcome PreparationOutcome, cause error) {
	failure := outcome.Failure
	if outcome == (PreparationOutcome{}) && cause != nil {
		failure = s.host.ClassifyPreparation(cause)
	}
	safe := runtimeObservationFailure(failure) && !retainsResidentCleanup(cause)
	s.mu.Lock()
	stop := false
	if !safe && (s.runtimeFrom != (StartupReference{}) || outcome.RuntimeRestart != (RuntimeRestartReport{})) {
		s.closing, stop = true, true
	} else if safe && outcome.RuntimeRestart != (RuntimeRestartReport{}) {
		if outcome.RuntimeRestart.From != from || s.runtimeFrom != (StartupReference{}) && s.runtimeFrom != from {
			s.closing, stop = true, true
		} else if outcome.RuntimeRestart.Phase == RuntimeRestartReady {
			s.runtimeFrom = StartupReference{}
		} else {
			s.runtimeFrom = from
		}
	}
	s.mu.Unlock()
	if stop {
		s.stopAccepting()
	}
}

func runtimeObservationFailure(failure PreparationFailure) bool {
	const observations = PreparationCauseRuntimeRestartPending | PreparationCauseRuntimeQuiescenceBusy |
		PreparationCauseLockBusy | PreparationCauseDeadline | PreparationCauseCanceled |
		PreparationCauseUnknown | PreparationCauseInFlight | PreparationCauseStatusOperational | PreparationCauseControlLimit
	return failure.Validate() == nil && !failure.IndependentFault && failure.Causes & ^observations == 0
}

func (s *ownerServer) keepsRuntimeReconciliation() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closing && s.runtimeFrom != (StartupReference{})
}

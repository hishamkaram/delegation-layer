package infrastructure

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

type epochLifecycleKind string

const (
	epochLifecycleStop         epochLifecycleKind = "stop"
	epochLifecycleOwnerUpgrade epochLifecycleKind = "owner-upgrade"
)

// The target root's infrastructure journal owns immutable lifecycle intents
// and create-once progress. Its future writer serializes Stop and OwnerUpgrade
// under one authority, validates the latest startup owner, and proves native
// transitions. These comparable values validate data only; they grant no lease,
// executable provenance, native permission, or evidence of owner absence.
type epochLifecycleIntent struct {
	Protocol           int                    `json:"protocol"`
	Sequence           uint64                 `json:"sequence"`
	ID                 string                 `json:"id"`
	Kind               epochLifecycleKind     `json:"kind"`
	Root               string                 `json:"root"`
	RootID             string                 `json:"root_id"`
	BootID             string                 `json:"boot_id"`
	RootDevice         uint64                 `json:"root_device"`
	RootInode          uint64                 `json:"root_inode"`
	Owner              StartupReference       `json:"owner"`
	AcceptedAt         string                 `json:"accepted_at"`
	Deadline           string                 `json:"deadline"`
	SuccessorArtifacts Artifacts              `json:"successor_artifacts"`
	SuccessorContext   LaunchContextReference `json:"successor_context"`
}

func (i epochLifecycleIntent) Validate() error {
	if i.Protocol != ProtocolVersion || i.Sequence == 0 || task.ValidateTaskID(i.ID) != nil {
		return ErrIdentity
	}
	if _, err := NewOrigin(i.Root, i.RootID); err != nil {
		return err
	}
	if i.RootInode == 0 || lifetime.CompareBootIDs(i.BootID, i.BootID) != lifetime.BootSame {
		return fmt.Errorf("%w: lifecycle requires stable boot and directory identities", ErrIdentity)
	}
	// Zero names the logical predecessor when no startup exists at acceptance,
	// or when inherited verbatim from the immediately preceding Abandoned body.
	// It never waives startup history, settlement, native-absence or daemon-capture checks.
	if i.Owner != (StartupReference{}) {
		if err := i.Owner.Validate(); err != nil {
			return err
		}
	}
	accepted, acceptedErr := canonicalStartupTime(i.AcceptedAt)
	deadline, deadlineErr := canonicalStartupTime(i.Deadline)
	if acceptedErr != nil || deadlineErr != nil || !deadline.After(accepted) {
		return errors.Join(ErrIdentity, acceptedErr, deadlineErr)
	}
	return i.validateSuccessorChoice()
}

func (i epochLifecycleIntent) validateSuccessorChoice() error {
	switch i.Kind {
	case epochLifecycleStop:
		if i.SuccessorArtifacts != (Artifacts{}) || i.SuccessorContext != (LaunchContextReference{}) {
			return fmt.Errorf("%w: stop cannot select successor artifacts or context", ErrIdentity)
		}
		return nil
	case epochLifecycleOwnerUpgrade:
		return errors.Join(i.SuccessorArtifacts.validate(), i.SuccessorContext.Validate())
	default:
		return fmt.Errorf("%w: unknown epoch lifecycle operation", ErrIdentity)
	}
}

func (i epochLifecycleIntent) Digest() (string, error) {
	if err := i.Validate(); err != nil {
		return "", err
	}
	return digestStartupRecord(i)
}

// ID permits direct body lookup; its digest binds the sequence and every other
// immutable field. No second sequence, generation ledger, or mutable head is
// needed in a reference.
type epochLifecycleReference struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

func referenceEpochLifecycle(intent epochLifecycleIntent) (epochLifecycleReference, error) {
	digest, err := intent.Digest()
	if err != nil {
		return epochLifecycleReference{}, err
	}
	return epochLifecycleReference{ID: intent.ID, SHA256: digest}, nil
}

func (r epochLifecycleReference) Validate() error {
	if task.ValidateTaskID(r.ID) != nil || task.ValidateSHA256(r.SHA256) != nil {
		return ErrIdentity
	}
	return nil
}

func (r epochLifecycleReference) validateIntent(intent epochLifecycleIntent) error {
	if err := r.Validate(); err != nil {
		return err
	}
	expected, err := referenceEpochLifecycle(intent)
	if err != nil {
		return err
	}
	if r != expected {
		return fmt.Errorf("%w: epoch lifecycle reference changed", ErrIdentity)
	}
	return nil
}

type epochLifecyclePhase string

const (
	epochLifecycleAccepted  epochLifecyclePhase = "accepted"
	epochLifecycleClosing   epochLifecyclePhase = "closing"
	epochLifecycleStopping  epochLifecyclePhase = "stopping"
	epochLifecycleExiting   epochLifecyclePhase = "exiting"
	epochLifecycleComplete  epochLifecyclePhase = "complete"
	epochLifecycleAborted   epochLifecyclePhase = "aborted"
	epochLifecycleAbandoned epochLifecyclePhase = "abandoned"
)

// Progress records observations of authorized transitions, never authorization
// themselves. The writer must guard Closing under Generation, persist Stopping
// before ShutdownDispatched, and prove old-owner absence, lease freedom and any
// successor publication before Complete. Successor validates shape only here;
// the writer must verify its root, kind, lifecycle ID/hash and publication.
// Abandoned requires a new explicit Stop/Upgrade, old-owner absence and every
// successor attempt settled under the actual exclusive instance lease and
// Generation. The same writer fences continuation before accepting a new body.
// Neither expiry nor this data validator proves those conditions. Ordinary
// Ensure cannot reopen an abandoned upgrade or select the old runtime.
// epochStopTarget is a durable projection of the control owner's checked
// capture. The app maps values; infrastructure never parses Pueue's opaque
// process token or turns this data back into native shutdown permission.
type epochStopTargetKind string

const (
	epochStopAbsent   epochStopTargetKind = "absent"
	epochStopCaptured epochStopTargetKind = "captured"
)

type epochNativeFileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}
type epochStopTarget struct {
	Kind       epochStopTargetKind     `json:"kind"`
	Binding    task.SupervisorRef      `json:"binding"`
	BootID     string                  `json:"boot_id"`
	Directory  epochNativeFileIdentity `json:"directory"`
	Bootstrap  epochNativeFileIdentity `json:"bootstrap"`
	PID        int                     `json:"pid"`
	StartToken string                  `json:"start_token"`
}

func (t epochStopTarget) Validate() error {
	if lifetime.CompareBootIDs(t.BootID, t.BootID) != lifetime.BootSame || t.Directory.Inode == 0 || t.Bootstrap.Inode == 0 {
		return ErrIdentity
	}
	if t.Binding != (task.SupervisorRef{}) {
		if err := task.ValidateSupervisorRef(t.Binding); err != nil {
			return err
		}
	}
	switch t.Kind {
	case epochStopAbsent:
		// Zero binding represents only control-verified canonical never-started
		// state. Missing PID/binding alone never establishes that observation.
		if t.PID != 0 || t.StartToken != "" {
			return ErrIdentity
		}
	case epochStopCaptured:
		if t.Binding == (task.SupervisorRef{}) || t.PID <= 0 || t.StartToken == "" || strings.ContainsRune(t.StartToken, '\x00') {
			return ErrIdentity
		}
	default:
		return ErrIdentity
	}
	return nil
}

type epochLifecycleProgress struct {
	Protocol     int                     `json:"protocol"`
	OperationRef epochLifecycleReference `json:"operation"`
	Phase        epochLifecyclePhase     `json:"phase"`
	RecordedAt   string                  `json:"recorded_at"`
	Successor    StartupReference        `json:"successor"`
	StopTarget   epochStopTarget         `json:"stop_target"`
}

func (p epochLifecycleProgress) Validate(intent epochLifecycleIntent) error {
	if p.Protocol != ProtocolVersion {
		return ErrIdentity
	}
	if err := p.OperationRef.validateIntent(intent); err != nil {
		return err
	}
	recorded, err := canonicalStartupTime(p.RecordedAt)
	if err != nil {
		return err
	}
	accepted, err := canonicalStartupTime(intent.AcceptedAt)
	if err != nil || recorded.Before(accepted) {
		return errors.Join(ErrIdentity, err)
	}
	if err = p.validatePayload(intent); err != nil {
		return err
	}
	return p.validatePhaseKind(intent.Kind)
}

func (p epochLifecycleProgress) validatePayload(intent epochLifecycleIntent) error {
	if p.Phase != epochLifecycleStopping && p.StopTarget != (epochStopTarget{}) {
		return ErrIdentity
	}
	if p.Phase == epochLifecycleComplete && intent.Kind == epochLifecycleOwnerUpgrade {
		return p.Successor.Validate()
	}
	if p.Successor != (StartupReference{}) {
		return fmt.Errorf("%w: successor belongs only to completed owner upgrade", ErrIdentity)
	}
	return nil
}

func (p epochLifecycleProgress) validatePhaseKind(kind epochLifecycleKind) error {
	switch p.Phase {
	case epochLifecycleClosing, epochLifecycleAborted, epochLifecycleComplete:
		return nil
	case epochLifecycleStopping:
		if kind != epochLifecycleStop {
			return ErrIdentity
		}
		return p.StopTarget.Validate()
	case epochLifecycleExiting, epochLifecycleAbandoned:
		if kind != epochLifecycleOwnerUpgrade {
			return ErrIdentity
		}
		return nil
	case epochLifecycleAccepted:
		return ErrIdentity
	default:
		return ErrIdentity
	}
}

// deriveEpochLifecyclePhase reads an ordered immutable history without mutating
// it. Intent alone stays accepted and unresolved, even beyond its original
// deadline. Later explicit reconciliation may record completion or pre-dispatch
// abort; neither expiry nor a missing record implicitly completes or reopens it.
func deriveEpochLifecyclePhase(intent epochLifecycleIntent, records []epochLifecycleProgress) (epochLifecyclePhase, error) {
	if err := intent.Validate(); err != nil {
		return "", err
	}
	phase := epochLifecycleAccepted
	for _, record := range records {
		if err := record.Validate(intent); err != nil {
			return "", err
		}
		if !epochLifecycleTransitionAllowed(phase, record.Phase) {
			return "", fmt.Errorf("%w: invalid epoch lifecycle progress order", ErrIdentity)
		}
		phase = record.Phase
	}
	return phase, nil
}

func epochLifecycleTransitionAllowed(current, next epochLifecyclePhase) bool {
	switch current {
	case epochLifecycleAccepted:
		return next == epochLifecycleClosing
	case epochLifecycleClosing:
		return next == epochLifecycleStopping || next == epochLifecycleExiting || next == epochLifecycleAborted
	case epochLifecycleStopping:
		return next == epochLifecycleComplete
	case epochLifecycleExiting:
		return next == epochLifecycleComplete || next == epochLifecycleAbandoned
	case epochLifecycleComplete, epochLifecycleAborted, epochLifecycleAbandoned:
		return false
	}
	return false
}

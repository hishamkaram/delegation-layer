package infrastructure

import (
	"context"
	"fmt"
	"os"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// PrivateGenerationGuard guards only the exact canonical binding/generation
// commit callback. The control owner retains bootstrap before invoking it.
// It is structural to avoid a Pueue implementation dependency in this package.
type PrivateGenerationGuard interface {
	GuardPrivateGenerationCommit(context.Context, string, func() error) error
}

// OwnerContactRequiredError is an observation, not a joined startup or a
// native grant. The caller must authenticate the referenced existing owner.
type OwnerContactRequiredError struct{ Startup StartupReference }

func (e *OwnerContactRequiredError) Error() string {
	return "current infrastructure owner requires authenticated contact"
}

type lifecycleAction uint8

const (
	lifecycleOrdinaryBirth lifecycleAction = iota + 1
	lifecycleGenerationMutation
	lifecyclePrivateTaskAdmission
)

// One policy decision distinguishes warm task admission from changing the
// resident/daemon generation. In particular an abandoned owner Upgrade never
// authorizes an old-image birth or an old lifecycle continuation.
func checkLifecycleAction(snapshot epochLifecycleSnapshot, found bool, action lifecycleAction) error {
	switch action {
	case lifecycleOrdinaryBirth, lifecycleGenerationMutation, lifecyclePrivateTaskAdmission:
	default:
		return os.ErrInvalid
	}
	if !found {
		return nil
	}
	if action == lifecyclePrivateTaskAdmission && snapshot.intent.Kind == epochLifecycleOwnerUpgrade {
		return nil
	}
	if snapshot.phase == epochLifecycleComplete || snapshot.phase == epochLifecycleAborted {
		return nil
	}
	return fmt.Errorf("%w: infrastructure lifecycle requires explicit reconciliation", task.ErrLockBusy)
}

func requireOrdinaryLifecycle(snapshot epochLifecycleSnapshot, found bool) error {
	return checkLifecycleAction(snapshot, found, lifecycleOrdinaryBirth)
}

// CheckPrivateTaskAdmissionContext reads lifecycle data only. The consumer
// must retain RootAdmission SH and Generation plus a freshly verified warm
// canonical daemon. This grants no native Start, generation/binding mutation
// or retired lifecycle continuation. It creates no journal and takes no
// instance lease. Owner-Upgrade warm admission includes Abandoned; that edge
// remains unactivated pending the independent integrated policy review.
func CheckPrivateTaskAdmissionContext(ctx context.Context, borrowed *taskdir.ReadOnlyStore) error {
	snapshot, found, err := readLatestLifecycle(ctx, borrowed)
	if err != nil {
		return err
	}
	return checkLifecycleAction(snapshot, found, lifecyclePrivateTaskAdmission)
}

func requireLifecycleStartup(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, intent StartupIntent) error {
	snapshot, found, err := latestLifecycle(ctx, reader, pin)
	if err != nil {
		return err
	}
	if intent.Inputs.Kind == StartupRootOwner {
		if err = requireOrdinaryLifecycle(snapshot, found); err != nil {
			return err
		}
		return requireUnretiredStartup(snapshot, found, intent)
	}
	if intent.Inputs.Kind != StartupUpgrade || !found {
		return ErrIdentity
	}
	reference := epochLifecycleReference{ID: intent.Inputs.UpgradeID, SHA256: intent.Inputs.UpgradeSHA256}
	if err = reference.validateIntent(snapshot.intent); err != nil {
		return err
	}
	if snapshot.intent.Kind != epochLifecycleOwnerUpgrade || snapshot.phase != epochLifecycleExiting {
		return task.ErrLockBusy
	}
	current, referenceErr := ReferenceStartup(intent)
	if referenceErr != nil {
		return referenceErr
	}
	if err = requireLifecycleSettlements(ctx, reader, pin, intent.Inputs.BootID, current); err != nil {
		return err
	}
	return nil
}

// Complete retires the prior owner's continuation. Aborted instead represents
// the coordinator's durably reopened pre-dispatch fence for that same owner.
func requireUnretiredStartup(snapshot epochLifecycleSnapshot, found bool, intent StartupIntent) error {
	if !found || snapshot.phase == epochLifecycleAborted {
		return nil
	}
	last := snapshot.intent.Owner.Sequence
	if snapshot.phase == epochLifecycleComplete && snapshot.intent.Kind == epochLifecycleOwnerUpgrade {
		last = lifecycleSuccessor(snapshot).Sequence
	}
	if intent.Sequence <= last {
		return ErrIdentity
	}
	return nil
}

func lifecycleSuccessor(snapshot epochLifecycleSnapshot) StartupReference {
	for _, progress := range snapshot.records {
		if progress.Phase == epochLifecycleComplete {
			return progress.Successor
		}
	}
	return StartupReference{}
}

func validateLifecycleStartupHistory(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, intent StartupIntent) error {
	if intent.Inputs.Kind == StartupRootOwner {
		return nil
	}
	if intent.Inputs.Kind != StartupUpgrade {
		return ErrIdentity
	}
	reference := epochLifecycleReference{ID: intent.Inputs.UpgradeID, SHA256: intent.Inputs.UpgradeSHA256}
	snapshot, err := readReferencedLifecycle(ctx, reader, pin, reference)
	if err != nil {
		return err
	}
	return validateUpgradeLifecycleBody(intent, snapshot.intent)
}

func lifecycleStartupSelection(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, intent StartupIntent) (Artifacts, LaunchContextReference, error) {
	if intent.Inputs.Kind == StartupRootOwner {
		return intent.Inputs.Artifacts, intent.Inputs.Context, nil
	}
	reference := epochLifecycleReference{ID: intent.Inputs.UpgradeID, SHA256: intent.Inputs.UpgradeSHA256}
	snapshot, err := readReferencedLifecycle(ctx, reader, pin, reference)
	if err != nil {
		return Artifacts{}, LaunchContextReference{}, err
	}
	if err = validateUpgradeLifecycleBody(intent, snapshot.intent); err != nil {
		return Artifacts{}, LaunchContextReference{}, err
	}
	return snapshot.intent.SuccessorArtifacts, snapshot.intent.SuccessorContext, nil
}

func validateLifecycleRuntime(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, intent StartupIntent, digest string) error {
	artifacts, _, err := lifecycleStartupSelection(ctx, reader, pin, intent)
	if err != nil {
		return err
	}
	return validateSelectedRuntime(artifacts, digest)
}

func validateSelectedRuntime(artifacts Artifacts, digest string) error {
	if digest != artifacts.RunnerSHA256 {
		return ErrIdentity
	}
	return nil
}

func requireResidentLifecycle(ctx context.Context, reader lifecycleRecordReader, pin taskdir.RootPin, intent StartupIntent) error {
	snapshot, found, err := latestLifecycle(ctx, reader, pin)
	if err != nil {
		return err
	}
	if err = checkLifecycleAction(snapshot, found, lifecycleGenerationMutation); err != nil {
		return err
	}
	if err = validateLifecycleStartupHistory(ctx, reader, pin, intent); err != nil {
		return err
	}
	if found && snapshot.phase == epochLifecycleComplete && snapshot.intent.Kind == epochLifecycleOwnerUpgrade {
		reference, referenceErr := ReferenceStartup(intent)
		if referenceErr != nil {
			return referenceErr
		}
		if reference == lifecycleSuccessor(snapshot) {
			return nil
		}
	}
	return requireUnretiredStartup(snapshot, found, intent)
}

func (a *StartupAttempt) currentLifecycleReader() controlLifecycleReader {
	return controlLifecycleReader{controlOwnerReader{controlIntentReader{a.journal.control}}}
}

func (a *StartupAttempt) requireOrdinaryBirth(ctx context.Context, reader lifecycleRecordReader) error {
	snapshot, found, err := latestLifecycle(ctx, reader, a.pin)
	if err != nil {
		return err
	}
	return requireOrdinaryLifecycle(snapshot, found)
}

func (a *StartupAttempt) requireOwnerContact(ctx context.Context, reader lifecycleRecordReader, intent StartupIntent, requirements ArtifactRequirements, evidence settlementEvidence) error {
	reference, err := ReferenceStartup(intent)
	if err != nil {
		return err
	}
	if evidence.assumption == nil || evidence.publication == nil {
		return task.ErrLockBusy
	}
	artifacts, _, err := lifecycleStartupSelection(ctx, reader, a.pin, intent)
	if err != nil {
		return err
	}
	if err = validateSelectedRuntime(artifacts, evidence.publication.RuntimeSHA256); err != nil {
		return err
	}
	if err = requirements.ValidateArtifacts(artifacts); err != nil {
		return err
	}
	process, err := evidence.assumption.Process.Reference()
	if err != nil {
		return err
	}
	present, err := lifetime.ProcessPresent(process)
	if err != nil {
		return err
	}
	if !present {
		return task.ErrLockBusy
	}
	if err = a.checkRoot(ctx); err != nil {
		return err
	}
	return &OwnerContactRequiredError{Startup: reference}
}

func (g *residentGrant) GuardPrivateGenerationCommit(ctx context.Context, operationRoot string, commit func() error) error {
	if g == nil || g.entry == nil || ctx == nil || commit == nil {
		return os.ErrInvalid
	}
	entry := g.entry
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.closed {
		return os.ErrClosed
	}
	if operationRoot != entry.operation.Intent().Inputs.Root {
		return ErrIdentity
	}
	return entry.journal.withOpen(ctx, func() error {
		return entry.lease.WithHeld(ctx, func() error {
			if err := entry.validateResidentHeld(ctx); err != nil {
				return err
			}
			reader := controlLifecycleReader{controlOwnerReader{controlIntentReader{entry.journal.control}}}
			if err := requireResidentLifecycle(ctx, reader, entry.journal.pin, entry.operation.Intent()); err != nil {
				return err
			}
			return commit()
		})
	})
}

// validateUpgradeLifecycleBody binds immutable data only. Current lifecycle
// phase, native lease and process absence remain the journal/coordinator's job.
func validateUpgradeLifecycleBody(intent StartupIntent, body epochLifecycleIntent) error {
	if err := intent.Validate(); err != nil {
		return err
	}
	if err := body.Validate(); err != nil {
		return err
	}
	if intent.Inputs.Kind != StartupUpgrade || body.Kind != epochLifecycleOwnerUpgrade {
		return ErrIdentity
	}
	reference := epochLifecycleReference{ID: intent.Inputs.UpgradeID, SHA256: intent.Inputs.UpgradeSHA256}
	if err := reference.validateIntent(body); err != nil {
		return err
	}
	if intent.Inputs.Root != body.Root || intent.Inputs.RootID != body.RootID {
		return ErrIdentity
	}
	switch lifetime.CompareBootIDs(intent.Inputs.BootID, body.BootID) {
	case lifetime.BootSame:
		if intent.Inputs.RootDevice != body.RootDevice || intent.Inputs.RootInode != body.RootInode {
			return ErrIdentity
		}
	case lifetime.BootDifferent:
		// Prior-boot directory coordinates remain historical data.
	case lifetime.BootUnknown:
		return ErrIdentity
	}
	return nil
}

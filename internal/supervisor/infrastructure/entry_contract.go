package infrastructure

import (
	"context"
	"runtime"
	"runtime/debug"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Operation exposes immutable, verified startup records. Only the fixed entry
// constructs it; its artifacts are derived from the intent, never a second head.
// Its private context validates birth only; the request handler separately
// resolves the context selected for each preparation. It is data, not Start permission.
type Operation struct {
	intent    StartupIntent
	context   LaunchContext
	lifecycle epochLifecycleIntent
}

func newRootOwnerOperation(intent StartupIntent, launchContext LaunchContext) (Operation, error) {
	if intent.Inputs.Kind != StartupRootOwner {
		return Operation{}, ErrIdentity
	}
	operation := Operation{intent: intent, context: launchContext.Clone()}
	if err := operation.Validate(); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

// The caller reads body through the canonical referenced lifecycle reader.
// This constructor checks immutable data only. The journal separately requires
// the latest exact Exiting body before successor birth, assumption or publication.
func newUpgradeOperation(intent StartupIntent, launchContext LaunchContext, body epochLifecycleIntent) (Operation, error) {
	if intent.Inputs.Kind != StartupUpgrade {
		return Operation{}, ErrIdentity
	}
	operation := Operation{intent: intent, context: launchContext.Clone(), lifecycle: body}
	if err := operation.Validate(); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

func (o Operation) Intent() StartupIntent { return o.intent }

func (o Operation) Artifacts() Artifacts {
	if o.intent.Inputs.Kind == StartupUpgrade {
		return o.lifecycle.SuccessorArtifacts
	}
	return o.intent.Inputs.Artifacts
}

func (o Operation) defaultContextReference() LaunchContextReference {
	if o.intent.Inputs.Kind == StartupUpgrade {
		return o.lifecycle.SuccessorContext
	}
	return o.intent.Inputs.Context
}

func (o Operation) Validate() error {
	if o.context.OS != runtime.GOOS {
		return ErrIdentity
	}
	switch o.intent.Inputs.Kind {
	case StartupRootOwner:
		if o.lifecycle != (epochLifecycleIntent{}) {
			return ErrIdentity
		}
		if err := o.intent.Validate(); err != nil {
			return err
		}
	case StartupUpgrade:
		if err := validateUpgradeLifecycleBody(o.intent, o.lifecycle); err != nil {
			return err
		}
	default:
		return ErrIdentity
	}
	return o.defaultContextReference().ValidateContext(o.context, o.intent.Inputs.Root, o.intent.Inputs.RootID)
}

// DaemonStartGuard protects only the canonical supervisor's native Start edge.
// The canonical supervisor still owns daemon selection, hashing and recovery.
type DaemonStartGuard interface {
	GuardDaemonStart(context.Context, string, func() error) error
}

// ResidentGuards is borrowed from the sole successfully published entry owner.
// Both mutation edges validate that same resident and its lifecycle state.
type ResidentGuards interface {
	DaemonStartGuard
	PrivateGenerationGuard
}

// DaemonWaitOwner retains the canonical daemon completion handle for resident
// shutdown. Pueue retains the sole Wait; PID alone is no mutation authority.
type DaemonWaitOwner interface {
	RegisterDaemonWait(root string, pid int, done <-chan struct{}) error
}

// EntryHost is implemented by trusted app composition, never caller-supplied
// dependencies. The child owns native birth checks; the parent owns managed
// build eligibility, custody policy and canonical supervisor preparation.
// Failed resource cleanup must retain the actual owners in an error node with
// RetainsResidentCleanup(). The server stops admission and retains every such
// fault through disposal; neither that marker nor a reply certifies closure.
type EntryHost interface {
	ValidateArtifacts(context.Context, string, Artifacts, *debug.BuildInfo) error
	EnsureOwnRoot(context.Context, Operation, LaunchContext, ResidentGuards, DaemonWaitOwner) (Disposition, error)
	RecoverSaved(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error)
	PrepareContinuation(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error)
	MigrateReferenced(context.Context, Operation, LaunchContext, MigrationReference, ResidentGuards, DaemonWaitOwner) (Disposition, MigrationResult, error)
	MigrateOwnRoot(context.Context, Operation, LaunchContext, ResidentGuards, DaemonWaitOwner) (Disposition, ManualMigrationReport, error)
	ClassifyPreparation(error) PreparationFailure
	StageTargetArtifacts(context.Context, Operation, ArtifactRequirements, *taskdir.Store) (Artifacts, error)
}

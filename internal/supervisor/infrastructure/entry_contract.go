package infrastructure

import (
	"context"
	"runtime"
	"runtime/debug"

	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

// Operation exposes immutable, verified startup records. Only the fixed entry
// constructs it; its artifacts are derived from the intent, never a second head.
// Construction validates the birth context; the request handler separately
// resolves the context selected for each preparation. It is data, not Start permission.
type Operation struct {
	intent StartupIntent
}

func newRootOwnerOperation(intent StartupIntent, launchContext LaunchContext) (Operation, error) {
	if intent.Inputs.Kind != StartupRootOwner || launchContext.OS != runtime.GOOS {
		return Operation{}, ErrIdentity
	}
	operation := Operation{intent: intent}
	if err := operation.Validate(); err != nil {
		return Operation{}, err
	}
	if err := intent.Inputs.Context.ValidateContext(launchContext, intent.Inputs.Root, intent.Inputs.RootID); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

func (o Operation) Intent() StartupIntent { return o.intent }

func (o Operation) Artifacts() Artifacts { return o.intent.Inputs.Artifacts }

func (o Operation) defaultContextReference() LaunchContextReference { return o.intent.Inputs.Context }

func (o Operation) Validate() error { return o.intent.Validate() }

// DaemonStartGuard protects only the canonical supervisor's native Start edge.
// The canonical supervisor still owns daemon selection and recovery.
type DaemonStartGuard interface {
	GuardDaemonStart(context.Context, string, func() error) error
}

// PrivateGenerationGuard guards only the exact canonical binding/generation
// commit callback. The control owner retains bootstrap before invoking it.
// It is structural to avoid a Pueue implementation dependency in this package.
type PrivateGenerationGuard interface {
	GuardPrivateGenerationCommit(context.Context, string, func() error) error
}

// ResidentGuards is borrowed from the sole successfully published entry owner.
// Both mutation edges validate the same held instance, root and publication.
type ResidentGuards interface {
	DaemonStartGuard
	PrivateGenerationGuard
}

// DaemonWaitOwner retains the canonical daemon completion handle for resident
// shutdown. Pueue retains the sole Wait; PID alone is no mutation authority.
type DaemonWaitOwner interface {
	RegisterDaemonWait(root string, pid int, done <-chan struct{}) error
}

// ResidentCleanupCompletion is the fixed host's cleanup-attempt observation.
// The producer owns the completion signal and retained resource graph. Done
// means all borrowers settled and cleanup was attempted; it does not establish
// successful release. This port grants no native operation or closure authority.
type ResidentCleanupCompletion interface {
	error
	RetainsResidentCleanup()
	CleanupDone() <-chan struct{}
	CleanupErr() error
	CleanupPending() bool
}

// EntryHost is implemented by trusted app composition, never caller-supplied
// dependencies. The child owns native birth checks; the parent owns managed
// build eligibility, custody policy and canonical supervisor preparation.
// Failed resource cleanup must retain actual owners in an error node implementing
// ResidentCleanupCompletion. The server stops admission and retains every such
// fault through disposal; neither that marker nor a reply certifies closure.
type EntryHost interface {
	ValidateRunner(context.Context, string, string, *debug.BuildInfo) error
	ValidateEstablishment(context.Context, *taskdir.Store, InitialOperation) error
	ObserveRuntimeRestartCustody(context.Context, Operation, LaunchContext, DaemonWaitOwner) (RuntimeRestartCustody, error)
	EnsureOwnRoot(context.Context, Operation, LaunchContext, ResidentGuards, DaemonWaitOwner) (Disposition, error)
	RecoverSaved(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error)
	PrepareContinuation(context.Context, Operation, LaunchContext, SavedBindingSource, ResidentGuards, DaemonWaitOwner) (Disposition, error)
	MigrateOwnRoot(context.Context, Operation, LaunchContext, ResidentGuards, DaemonWaitOwner) (Disposition, ManualMigrationReport, error)
	PrepareRuntimeRestart(context.Context, Operation, LaunchContext, StartupReference, ResidentGuards, DaemonWaitOwner) (Disposition, RuntimeRestartReport, error)
	ClassifyPreparation(error) PreparationFailure
}

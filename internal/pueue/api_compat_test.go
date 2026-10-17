package pueue_test

import (
	"context"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// These references make facade symbol removal or renaming a compile failure.
// Application packages separately compile against the full method signatures.
var (
	_ = pueue.FixtureVersion
	_ = pueue.DefaultObservationTimeout
	_ = pueue.PrivateStatusObservationTimeout
	_ = pueue.RunnerCommandUpgradeTimeout
	_ = pueue.MaxControlBytes
	_ = pueue.StateUnknown
	_ = pueue.StateQueued
	_ = pueue.StateStashed
	_ = pueue.StateRunning
	_ = pueue.StateEnded

	_ = pueue.ErrConfiguration
	_ = pueue.ErrBinding
	_ = pueue.ErrUnknown
	_ = pueue.ErrSubmissionUncertain
	_ = pueue.ErrEnqueueUncertain
	_ = pueue.ErrStagedCapability
	_ = pueue.ErrInFlight
	_ = pueue.ErrControlLimit
	_ = pueue.ErrCleanupDeferred
	_ = pueue.ErrCleanupAuthorization
	_ = pueue.ErrMigrationAuthorization
	_ = pueue.ErrStatusOperational
	_ = pueue.ErrStatusTransient
	_ = pueue.ErrPrivateSupervisorMigrationBusy
	_ = pueue.ErrPrivateSupervisorMigrationRequired
	_ = pueue.ErrPrivateSupervisorNotInitialized

	_ = pueue.Bind
	_ = pueue.BindPrivate
	_ = pueue.BindSaved
	_ = pueue.BindSavedForMigration
	_ = pueue.BindSavedForMigrationWithClient
	_ = pueue.NewClient
	_ = pueue.PreparePrivate
	_ = pueue.PreparePrivateForMigrationWithLock
	_ = pueue.PreparePrivateWithLock
	_ = pueue.PreparePublishedPrivateWithLock
	_ = pueue.PrepareRecordedPrivateMigrationWithLock
	_ = pueue.ProbePrivate
	_ = pueue.ProbePrivateEndpointWithGate
	_ = pueue.ProbePrivateGroupsWithBootstrapLock
	_ = pueue.ProbePrivateGroupsWithGate
	_ = pueue.ProbePrivateWithGate
	_ = pueue.RecoverPrivate
	_ = pueue.RecoverSaved
	_ = pueue.RecoverSavedWithPair
	_ = pueue.ParseConfig
	_ = pueue.ParseStatus
	_ = pueue.ParseQueueSnapshot
	_ = pueue.CurrentResolutionContext
	_ = pueue.ResolveConfig
	_ = pueue.ClearPrivateRecoveryBinding
	_ = pueue.ClearPrivateRecoveryBindingIfPresent
	_ = pueue.DefaultEnvironment
	_ = pueue.EnsurePrivateTaskObservationWithBootstrapLock
	_ = pueue.IsPrivateConfig
	_ = pueue.PrivateBootstrapPending
	_ = pueue.PrivateConfigPath
	_ = pueue.PrivateControlTimeout
	_ = pueue.PrivateDaemonIdentityDrift
	_ = pueue.PrivateDaemonIdentityPresent
	_ = pueue.PrivateMigrationRetargetableWithLock
	_ = pueue.PrivateMigrationTargetsWithLock
	_ = pueue.PrivatePairRestartPending
	_ = pueue.PrivateQueueMaintenanceRetryNeeded
	_ = pueue.PrivateQueueMaintenanceRetryNeededContext
	_ = pueue.PrivateRecoveryBindingMatches
	_ = pueue.PrivateRecoveryBindingPresent
	_ = pueue.PrivateSupervisorGenerationMatches
	_ = pueue.PrivateSupervisorLogicalDrift
	_ = pueue.PrivateSupervisorMigrationComplete
	_ = pueue.PrivateSupervisorMigrationEdgeCompleteWithLock
	_ = pueue.PrivateSupervisorMigrationEdgePublishedWithLock
	_ = pueue.PrivateSupervisorMigrationPending
	_ = pueue.PrivateTaskObservationReady
	_ = pueue.PrivateTerminalEventCallback
	_ = pueue.PrivateTerminalEventRunnerPath
	_ = pueue.PublishPrivateSupervisorBindingIfAbsent
	_ = pueue.PublishPrivateSupervisorBindingIfAbsentContext
	_ = pueue.PublishVerifiedPrivatePredecessorWithLock
	_ = pueue.ReadPrivateRecoveryBinding
	_ = pueue.ReadPrivateSchedulingEvidence
	_ = pueue.ReadPrivateSupervisorBinding
	_ = pueue.ReconcilePublishedPrivatePairWithLock
	_ = pueue.RecordPrivateTerminalEvent
	_ = pueue.RecordPrivateTerminalEventForMaintenance
	_ = pueue.RefreshPrivateSupervisorBinding
	_ = pueue.ReplacePrivateSchedulingEvidence
	_ = pueue.RequestPrivateCandidateQueueMaintenance
	_ = pueue.RequestPrivateInspectionQueueMaintenance
	_ = pueue.RequestPrivateQueueMaintenance
	_ = pueue.ResolvePrivateSupervisorSuccessor
	_ = pueue.ResolvePrivateSupervisorSuccessorWithLock
	_ = pueue.RetirePrivateRecoveryBinding
	_ = pueue.RetirePrivateRecoveryBindingContext
	_ = pueue.RunControlOwnerMode
	_ = pueue.TryWithPrivateTransition
	_ = pueue.WaitForPrivateQueueMaintenanceIdle
	_ = pueue.WaitForPrivateTransition
	_ = pueue.WithBootstrapLock
	_ = pueue.WithBootstrapLockContext
	_ = pueue.WithControlLease
	_ = pueue.WithSupervisorLease
	_ = pueue.WithSupervisorLeaseContext

	_ = (*pueue.Client).AwaitInspectionObservation
	_ = (*pueue.Client).AwaitInspectionObservationWithLease
	_ = (*pueue.Client).Binding
	_ = (*pueue.Client).ClearPrivateTerminalEvents
	_ = (*pueue.Client).Close
	_ = (*pueue.Client).CloseAfterPending
	_ = (*pueue.Client).CommitInspectionObservation
	_ = (*pueue.Client).CommitInspectionStaged
	_ = (*pueue.Client).CommitInspectionStagedWithGate
	_ = (*pueue.Client).CommitStaged
	_ = (*pueue.Client).CommitStagedWithGate
	_ = (*pueue.Client).CreateInspectionGroup
	_ = (*pueue.Client).EnsurePrivateSupervisorBindingWithLock
	_ = (*pueue.Client).MaintainPrivateQueue
	_ = (*pueue.Client).MigratePrivateSupervisorWithLock
	_ = (*pueue.Client).PersistPrivateRecoveryBinding
	_ = (*pueue.Client).PrivateTerminalEventsEnabled
	_ = (*pueue.Client).Reachable
	_ = (*pueue.Client).ReadPrivateTerminalEvent
	_ = (*pueue.Client).Ready
	_ = (*pueue.Client).Reconcile
	_ = (*pueue.Client).ReconcileInspection
	_ = (*pueue.Client).ReconcileInspectionWithLease
	_ = (*pueue.Client).ReconcileStatus
	_ = (*pueue.Client).RecordPrivateTerminalObservation
	_ = (*pueue.Client).RecoverInspectionStaged
	_ = (*pueue.Client).RecoverInspectionStagedIdentity
	_ = (*pueue.Client).RecoverInspectionStagedObservation
	_ = (*pueue.Client).RecoverInspectionStagedWithGate
	_ = (*pueue.Client).RecoverStaged
	_ = (*pueue.Client).RecoverStagedIdentity
	_ = (*pueue.Client).RecoverStagedObservation
	_ = (*pueue.Client).RecoverStagedWithGate
	_ = (*pueue.Client).RegisterPrivateOrdinaryTerminalEvent
	_ = (*pueue.Client).RegisterPrivateTerminalEvent
	_ = (*pueue.Client).RemoveCompletedTask
	_ = (*pueue.Client).ReplaceQueuedRunnerCommands
	_ = (*pueue.Client).RestartPrivate
	_ = (*pueue.Client).RunRequestedPrivateQueueMaintenance
	_ = (*pueue.Client).RuntimeBinding
	_ = (*pueue.Client).SchedulePrivateQueueMaintenance
	_ = (*pueue.Client).ScheduleRequestedPrivateQueueMaintenance
	_ = (*pueue.Client).ScopePrivateTask
	_ = (*pueue.Client).SetPrivateDefaultParallelTasksWithBootstrapLock
	_ = (*pueue.Client).Shutdown
	_ = (*pueue.Client).ShutdownDispatched
	_ = (*pueue.Client).Snapshot
	_ = (*pueue.Client).StagedLaunchCapability
	_ = (*pueue.Client).StagedLaunchCapabilityWithBootstrapLock
	_ = (*pueue.Client).StartPrivate
	_ = (*pueue.Client).Stop
	_ = (*pueue.Client).StopInspection
	_ = (*pueue.Client).StopRecovered
	_ = (*pueue.Client).Submit
	_ = (*pueue.Client).SubmitConcurrentInspection
	_ = (*pueue.Client).SubmitConcurrentInspectionStaged
	_ = (*pueue.Client).SubmitConcurrentInspectionStagedWithCommit
	_ = (*pueue.Client).SubmitConcurrentInspectionStagedWithGate
	_ = (*pueue.Client).SubmitConcurrentInspectionWithGate
	_ = (*pueue.Client).SubmitInspection
	_ = (*pueue.Client).SubmitInspectionStaged
	_ = (*pueue.Client).SubmitInspectionStagedWithCommit
	_ = (*pueue.Client).SubmitInspectionStagedWithGate
	_ = (*pueue.Client).SubmitInspectionWithGate
	_ = (*pueue.Client).SubmitStaged
	_ = (*pueue.Client).SubmitStagedWithCommit
	_ = (*pueue.Client).SubmitStagedWithGate
	_ = (*pueue.Client).SubmitWithGate
	_ = (*pueue.Client).SupportsStagedLaunch
	_ = (*pueue.Client).VerifyPrivateMigrationEndpoint
	_ = (*pueue.Client).WaitUnavailable
	_ = (*pueue.Client).WaitUnavailableWithPID
	_ = (*pueue.Client).WithCommandEnvironment
	_ = (*pueue.Client).WithObservationTimeout
)

var _ = pueue.Options{
	PrivateMaintenanceLauncher: func(context.Context, string, string) error { return nil },
	SuccessfulCleanupAuthorizer: func(context.Context, string, task.SupervisorRef, []pueue.Job) (pueue.PrivateQueueCleanupPlan, error) {
		return pueue.PrivateQueueCleanupPlan{}, nil
	},
	PrivateTerminalEvidencePreparer: func(context.Context, string, task.SupervisorRef, []pueue.Job) error {
		return nil
	},
	PrivateMigrationAuthorizer: func(context.Context, string, task.SupervisorRef, []pueue.Job) error {
		return nil
	},
}

var (
	_ pueue.Client
	_ pueue.ClientConfig
	_ pueue.CommandEvent
	_ pueue.CommandResult
	_ pueue.Config
	_ pueue.ControlLease
	_ pueue.DaemonConfig
	_ pueue.Group
	_ pueue.Identity
	_ pueue.InFlightError
	_ pueue.InspectionGroupPermit
	_ pueue.InspectionIdentity
	_ pueue.InspectionObservation
	_ pueue.InspectionObservationCommit
	_ pueue.InspectionStopPermit
	_ pueue.InspectionSubmissionCommit
	_ pueue.InspectionSubmissionPermit
	_ pueue.Job
	_ pueue.Launch
	_ pueue.Observation
	_ pueue.Options
	_ pueue.Pending
	_ pueue.PrivateQueueCleanupPlan
	_ pueue.PrivateRecoveryError
	_ pueue.QueueSnapshot
	_ pueue.ResolutionContext
	_ pueue.ResolvedConfig
	_ pueue.RunnerCommandReplacement
	_ pueue.SharedConfig
	_ pueue.StagedInspectionCommit
	_ pueue.StagedLaunchCapability
	_ pueue.StagedSubmissionCommit
	_ pueue.State
	_ pueue.StopResult
	_ pueue.SubmissionAuthorization
	_ pueue.SubmissionCommit
	_ pueue.SubmissionGate
	_ pueue.TerminalEvent
)

var (
	_ pueue.SubmissionCommit            = func(pueue.Observation) error { return nil }
	_ pueue.InspectionObservationCommit = func(pueue.InspectionObservation) error { return nil }
	_ pueue.SubmissionGate              = func(context.Context, task.SupervisorRef) (pueue.SubmissionAuthorization, error) {
		return pueue.SubmissionAuthorization{}, nil
	}
)

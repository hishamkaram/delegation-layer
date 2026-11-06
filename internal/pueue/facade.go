// Package pueue preserves the supervisor API while runtime ownership lives in internal/control.
package pueue

import (
	"context"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/pueue/internal/control"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type Client = control.Client

// BootstrapCleanupError exposes cleanup ownership to fixed resident hosts.
// Its completion and diagnostics confer no permission to start native work.
type BootstrapCleanupError = control.BootstrapCleanupError

type CompatiblePairSelector = control.CompatiblePairSelector

var ErrCompatiblePairUnavailable = control.ErrCompatiblePairUnavailable

func ProbeSavedPrivateCompatible(ctx context.Context, stateRoot string, saved task.SupervisorRef, options Options, selectPair CompatiblePairSelector) (*Client, error) {
	return control.ProbeSavedPrivateCompatible(ctx, stateRoot, saved, options, selectPair)
}

func ProbeSavedPrivateTaskObservation(ctx context.Context, stateRoot string, saved task.SupervisorRef, options Options, selectPair CompatiblePairSelector) (*Client, error) {
	return control.ProbeSavedPrivateTaskObservation(ctx, stateRoot, saved, options, selectPair)
}

func CheckPrivateAdmissionRuntime(ctx context.Context, root string, runtime task.SupervisorRef) error {
	return control.CheckPrivateAdmissionRuntime(ctx, root, runtime)
}

type (
	PrivateRestorePresence    = control.PrivateRestorePresence
	PrivateRestoreSource      = control.PrivateRestoreSource
	PrivateRestoreObservation = control.PrivateRestoreObservation
)

const (
	PrivateRestoreMissing = control.PrivateRestoreMissing
	PrivateRestorePresent = control.PrivateRestorePresent
)

var (
	ErrPrivateRestorePartial = control.ErrPrivateRestorePartial
	ErrPrivateRestoreLayout  = control.ErrPrivateRestoreLayout
	ErrPrivateRestoreCorrupt = control.ErrPrivateRestoreCorrupt
)

func ObservePrivateRestore(ctx context.Context, root taskdir.RootPin, expectedGeneration task.SupervisorRef, resolution ResolutionContext) (PrivateRestoreObservation, error) {
	return control.ObservePrivateRestore(ctx, root, expectedGeneration, resolution)
}

var (
	ErrStatusOperational = control.ErrStatusOperational
	ErrStatusTransient   = control.ErrStatusTransient
)

func Bind(ctx context.Context, executable string, configPath string, options Options) (*Client, error) {
	return control.Bind(ctx, executable, configPath, options)
}

func BindSaved(ctx context.Context, saved task.SupervisorRef, options Options) (*Client, error) {
	return control.BindSaved(ctx, saved, options)
}

func BindSavedForMigrationWithClient(ctx context.Context, saved task.SupervisorRef, clientExecutable string, options Options) (*Client, error) {
	return control.BindSavedForMigrationWithClient(ctx, saved, clientExecutable, options)
}

// IsLegacyStateSupervisorClient recognizes the reserved historical copy layout.
// This pure observation grants no launch or filesystem cleanup authority.
func IsLegacyStateSupervisorClient(root string, saved task.SupervisorRef) bool {
	return control.IsLegacyStateSupervisorClient(root, saved)
}

func IsLegacyStateRunnerPath(root, path string) bool {
	return control.IsLegacyStateRunnerPath(root, path)
}

func NewClient(saved task.SupervisorRef, options Options) (*Client, error) {
	return control.NewClient(saved, options)
}

func DefaultEnvironment() []string {
	return control.DefaultEnvironment()
}

func DefaultEnvironmentFrom(environment []string) []string {
	return control.DefaultEnvironmentFrom(environment)
}

func ParseConfig(data []byte) (*Config, error) {
	return control.ParseConfig(data)
}

type (
	ResolutionContext = control.ResolutionContext
	ResolvedConfig    = control.ResolvedConfig
)

func CurrentResolutionContext() (ResolutionContext, error) {
	return control.CurrentResolutionContext()
}

func ResolveConfig(c *Config, r ResolutionContext) (ResolvedConfig, error) {
	return control.ResolveConfig(c, r)
}

func ValidateUnixSocketPathBytes(goos string, pathBytes int) error {
	return control.ValidateUnixSocketPathBytes(goos, pathBytes)
}

type (
	Config       = control.Config
	ClientConfig = control.ClientConfig
	DaemonConfig = control.DaemonConfig
	SharedConfig = control.SharedConfig
)

func RunControlOwnerMode(args []string) (int, bool) {
	return control.RunControlOwnerMode(args)
}

func RunRepairEditorMode(args []string) (int, bool) {
	return control.RunRepairEditorMode(args)
}

type ControlLease = control.ControlLease

func WithControlLease(ctx context.Context, lease ControlLease) context.Context {
	return control.WithControlLease(ctx, lease)
}

type (
	InspectionGroupPermit       = control.InspectionGroupPermit
	InspectionSubmissionPermit  = control.InspectionSubmissionPermit
	InspectionStopPermit        = control.InspectionStopPermit
	InspectionIdentity          = control.InspectionIdentity
	InspectionObservation       = control.InspectionObservation
	InspectionObservationCommit = control.InspectionObservationCommit
)

func RequestPrivateQueueMaintenance(ctx context.Context, stateRoot string) error {
	return control.RequestPrivateQueueMaintenance(ctx, stateRoot)
}

func RequestPrivateCandidateQueueMaintenance(ctx context.Context, stateRoot string) error {
	return control.RequestPrivateCandidateQueueMaintenance(ctx, stateRoot)
}

func PrivateQueueMaintenanceRetryNeeded(stateRoot string) (bool, error) {
	return control.PrivateQueueMaintenanceRetryNeeded(stateRoot)
}

func PrivateQueueMaintenanceRetryNeededContext(ctx context.Context, stateRoot string) (bool, error) {
	return control.PrivateQueueMaintenanceRetryNeededContext(ctx, stateRoot)
}

func WaitForPrivateQueueMaintenanceIdle(ctx context.Context, stateRoot string) error {
	return control.WaitForPrivateQueueMaintenanceIdle(ctx, stateRoot)
}

type (
	SubmissionAuthorization     = control.SubmissionAuthorization
	SubmissionGate              = control.SubmissionGate
	PrivateEndpointState        = control.PrivateEndpointState
	PrivateEndpointObservation  = control.PrivateEndpointObservation
	PrivateRecovery             = control.PrivateRecovery
	PublishedPairAvailability   = control.PublishedPairAvailability
	PublishedPairReconciliation = control.PublishedPairReconciliation
)

const (
	PrivateEndpointReady   = control.PrivateEndpointReady
	PrivateEndpointAbsent  = control.PrivateEndpointAbsent
	PrivateEndpointRefused = control.PrivateEndpointRefused

	PublishedPairReady         = control.PublishedPairReady
	PublishedPairNotPublished  = control.PublishedPairNotPublished
	PublishedPairIncomplete    = control.PublishedPairIncomplete
	PublishedPairMemberMissing = control.PublishedPairMemberMissing
	PublishedPairConfigChanged = control.PublishedPairConfigChanged

	PublishedPairReconciled        = control.PublishedPairReconciled
	PublishedPairTransitionPending = control.PublishedPairTransitionPending
	PublishedPairLiveUnavailable   = control.PublishedPairLiveUnavailable
)

var (
	ErrPrivateSupervisorMigrationBusy        = control.ErrPrivateSupervisorMigrationBusy
	ErrPrivateSupervisorMigrationRequired    = control.ErrPrivateSupervisorMigrationRequired
	ErrPrivateSupervisorTransitionIncomplete = control.ErrPrivateSupervisorTransitionIncomplete
	ErrPrivateSupervisorSubmissionIncomplete = control.ErrPrivateSupervisorSubmissionIncomplete
	ErrPrivateSupervisorNotInitialized       = control.ErrPrivateSupervisorNotInitialized
	ErrPrivateEndpointRefused                = control.ErrPrivateEndpointRefused
)

func PrivateConfigPath(stateRoot string) string {
	return control.PrivateConfigPath(stateRoot)
}

func IsPrivateConfig(stateRoot string, configPath string) bool {
	return control.IsPrivateConfig(stateRoot, configPath)
}

func BindPrivate(ctx context.Context, clientExecutable string, daemonExecutable string, stateRoot string, options Options) (*Client, error) {
	return control.BindPrivate(ctx, clientExecutable, daemonExecutable, stateRoot, options)
}

func PreparePrivate(ctx context.Context, clientExecutable string, daemonExecutable string, stateRoot string, options Options) (*Client, error) {
	return control.PreparePrivate(ctx, clientExecutable, daemonExecutable, stateRoot, options)
}

func ProbePrivate(ctx context.Context, clientExecutable string, daemonExecutable string, stateRoot string, options Options) (*Client, bool, error) {
	return control.ProbePrivate(ctx, clientExecutable, daemonExecutable, stateRoot, options)
}

func ProbePrivateWithGate(ctx context.Context, clientExecutable string, daemonExecutable string, stateRoot string, options Options, gate func(context.Context, *Client, QueueSnapshot) (bool, error)) (*Client, bool, error) {
	return control.ProbePrivateWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, gate)
}

func ProbePrivateGroupsWithGate(ctx context.Context, clientExecutable string, daemonExecutable string, stateRoot string, options Options, gate func(context.Context, *Client, QueueSnapshot) (bool, error)) (*Client, bool, error) {
	return control.ProbePrivateGroupsWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, gate)
}

func ProbePrivateEndpointWithGate(ctx context.Context, clientExecutable string, daemonExecutable string, stateRoot string, options Options, gate func(context.Context, *Client) (bool, error)) (*Client, bool, error) {
	return control.ProbePrivateEndpointWithGate(ctx, clientExecutable, daemonExecutable, stateRoot, options, gate)
}

func ProbePrivateGroupsWithBootstrapLock(ctx context.Context, clientExecutable string, daemonExecutable string, stateRoot string, options Options) (*Client, map[string]Group, bool, error) {
	return control.ProbePrivateGroupsWithBootstrapLock(ctx, clientExecutable, daemonExecutable, stateRoot, options)
}

func WithBootstrapLock(ctx context.Context, stateRoot string, callback func() error) error {
	return control.WithBootstrapLock(ctx, stateRoot, callback)
}

func WithBootstrapLockContext(ctx context.Context, stateRoot string, callback func(context.Context) error) error {
	return control.WithBootstrapLockContext(ctx, stateRoot, callback)
}

func WithExistingBootstrapLockContext(ctx context.Context, stateRoot string, callback func(context.Context) error) error {
	return control.WithExistingBootstrapLockContext(ctx, stateRoot, callback)
}

func TryWithPrivateTransition(ctx context.Context, stateRoot string, callback func() error) (bool, error) {
	return control.TryWithPrivateTransition(ctx, stateRoot, callback)
}

func WaitForPrivateTransition(ctx context.Context, stateRoot string) error {
	return control.WaitForPrivateTransition(ctx, stateRoot)
}

func WithSupervisorLease(ctx context.Context, stateRoot string, callback func() error) error {
	return control.WithSupervisorLease(ctx, stateRoot, callback)
}

func WithSupervisorLeaseContext(ctx context.Context, stateRoot string, callback func(context.Context) error) error {
	return control.WithSupervisorLeaseContext(ctx, stateRoot, callback)
}

func PrivateControlTimeout(options Options) time.Duration {
	return control.PrivateControlTimeout(options)
}

func PrivateSupervisorPairMissing(binding task.SupervisorRef) (bool, error) {
	return control.PrivateSupervisorPairMissing(binding)
}

func AttemptPrivateRecovery(ctx context.Context, stateRoot string, saved task.SupervisorRef, clientExecutable string, daemonExecutable string, options Options) (PrivateRecovery, error) {
	return control.AttemptPrivateRecovery(ctx, stateRoot, saved, clientExecutable, daemonExecutable, options)
}

func RecoverSaved(ctx context.Context, operationRoot string, saved task.SupervisorRef, options Options) (*Client, bool, error) {
	return control.RecoverSaved(ctx, operationRoot, saved, options)
}

func RecoverSavedWithPair(ctx context.Context, operationRoot string, saved task.SupervisorRef, clientExecutable string, daemonExecutable string, options Options) (*Client, bool, error) {
	return control.RecoverSavedWithPair(ctx, operationRoot, saved, clientExecutable, daemonExecutable, options)
}

func PrivateSupervisorLogicalDrift(stateRoot string, binding task.SupervisorRef) (bool, error) {
	return control.PrivateSupervisorLogicalDrift(stateRoot, binding)
}

func PrivateDaemonIdentityPresent(stateRoot string) (bool, error) {
	return control.PrivateDaemonIdentityPresent(stateRoot)
}

func PrivateSupervisorMigrationPending(stateRoot string) (bool, error) {
	return control.PrivateSupervisorMigrationPending(stateRoot)
}

func PrivateSupervisorMigrationComplete(stateRoot string, binding task.SupervisorRef) (bool, error) {
	return control.PrivateSupervisorMigrationComplete(stateRoot, binding)
}

func PreparePrivateWithLock(ctx context.Context, clientExecutable string, daemonExecutable string, stateRoot string, options Options) (*Client, error) {
	return control.PreparePrivateWithLock(ctx, clientExecutable, daemonExecutable, stateRoot, options)
}

func PreparePrivateForMigrationWithLock(ctx context.Context, clientExecutable string, daemonExecutable string, stateRoot string, options Options) (*Client, error) {
	return control.PreparePrivateForMigrationWithLock(ctx, clientExecutable, daemonExecutable, stateRoot, options)
}

func PreparePublishedPrivateWithLock(ctx context.Context, stateRoot string, options Options) (*Client, bool, error) {
	return control.PreparePublishedPrivateWithLock(ctx, stateRoot, options)
}

func ObservePublishedPrivatePairWithLock(ctx context.Context, stateRoot string, options Options) (PublishedPairAvailability, error) {
	return control.ObservePublishedPrivatePairWithLock(ctx, stateRoot, options)
}

func ReadPrivateSchedulingEvidence(stateRoot string) ([]byte, bool, error) {
	return control.ReadPrivateSchedulingEvidence(stateRoot)
}

func ReplacePrivateSchedulingEvidence(stateRoot string, data []byte) error {
	return control.ReplacePrivateSchedulingEvidence(stateRoot, data)
}

func ReadPrivateSupervisorBinding(stateRoot string) (task.SupervisorRef, bool, error) {
	return control.ReadPrivateSupervisorBinding(stateRoot)
}

type (
	ExecutionTarget      = control.ExecutionTarget
	ExecutionTargetState = control.ExecutionTargetState
)

const (
	ExecutionTargetUnknown = control.ExecutionTargetUnknown
	ExecutionTargetAbsent  = control.ExecutionTargetAbsent
	ExecutionTargetRunning = control.ExecutionTargetRunning
)

var ErrExecutionTargetUnverified = control.ErrExecutionTargetUnverified

func ObservePrivateExecutionTarget(ctx context.Context, stateRoot string, options Options) (ExecutionTarget, error) {
	return control.ObservePrivateExecutionTarget(ctx, stateRoot, options)
}

func PrivateBootstrapPending(stateRoot string) (bool, error) {
	return control.PrivateBootstrapPending(stateRoot)
}

func PrivateSupervisorGenerationMatches(stateRoot string, binding task.SupervisorRef) (bool, error) {
	return control.PrivateSupervisorGenerationMatches(stateRoot, binding)
}

type PrivateRecoveryError = control.PrivateRecoveryError

func PrivatePairRestartPending(root string) (bool, error) {
	return control.PrivatePairRestartPending(root)
}

func PrivateMigrationTargetsWithLock(root string, target task.SupervisorRef) (bool, error) {
	return control.PrivateMigrationTargetsWithLock(root, target)
}

func PrivateMigrationRetargetableWithLock(root string) (bool, error) {
	return control.PrivateMigrationRetargetableWithLock(root)
}

func PrepareRecordedPrivateMigrationWithLock(ctx context.Context, root string, target task.SupervisorRef, options Options) (*Client, error) {
	return control.PrepareRecordedPrivateMigrationWithLock(ctx, root, target, options)
}

func ReconcilePublishedPrivatePairAttemptWithLock(ctx context.Context, stateRoot string, selectPair CompatiblePairSelector, options Options) (PublishedPairReconciliation, error) {
	return control.ReconcilePublishedPrivatePairAttemptWithLock(ctx, stateRoot, selectPair, options)
}

func PublishVerifiedPrivatePredecessorWithLock(ctx context.Context, root string, predecessor task.SupervisorRef) error {
	return control.PublishVerifiedPrivatePredecessorWithLock(ctx, root, predecessor)
}

func PrivateRecoveryBindingMatches(stateRoot string, supervisor task.SupervisorRef) (bool, error) {
	return control.PrivateRecoveryBindingMatches(stateRoot, supervisor)
}

func PrivateRecoveryBindingPresent(stateRoot string) (bool, error) {
	return control.PrivateRecoveryBindingPresent(stateRoot)
}

func ReadPrivateRecoveryBinding(stateRoot string) (task.SupervisorRef, bool, error) {
	return control.ReadPrivateRecoveryBinding(stateRoot)
}

func ResolvePrivateSupervisorSuccessor(ctx context.Context, stateRoot string, saved task.SupervisorRef) (task.SupervisorRef, bool, error) {
	return control.ResolvePrivateSupervisorSuccessor(ctx, stateRoot, saved)
}

func ResolvePrivateSupervisorSuccessorWithLock(ctx context.Context, stateRoot string, saved task.SupervisorRef) (task.SupervisorRef, bool, error) {
	return control.ResolvePrivateSupervisorSuccessorWithLock(ctx, stateRoot, saved)
}

func PrivateSupervisorMigrationEdgePublishedWithLock(ctx context.Context, stateRoot string, predecessor task.SupervisorRef, successor task.SupervisorRef) (bool, error) {
	return control.PrivateSupervisorMigrationEdgePublishedWithLock(ctx, stateRoot, predecessor, successor)
}

func PrivateSupervisorMigrationEdgeCompleteWithLock(ctx context.Context, stateRoot string, predecessor task.SupervisorRef, successor task.SupervisorRef) (bool, error) {
	return control.PrivateSupervisorMigrationEdgeCompleteWithLock(ctx, stateRoot, predecessor, successor)
}

type (
	InFlightError              = control.InFlightError
	RunnerCommandReplacement   = control.RunnerCommandReplacement
	SubmissionCommit           = control.SubmissionCommit
	StagedSubmissionCommit     = control.StagedSubmissionCommit
	InspectionSubmissionCommit = control.InspectionSubmissionCommit
	StagedInspectionCommit     = control.StagedInspectionCommit
	StagedLaunchCapability     = control.StagedLaunchCapability
	Job                        = control.Job
	Group                      = control.Group
	QueueSnapshot              = control.QueueSnapshot
)

func ParseStatus(data []byte, version string) ([]Job, error) {
	return control.ParseStatus(data, version)
}

func ParseQueueSnapshot(data []byte, version string) (QueueSnapshot, error) {
	return control.ParseQueueSnapshot(data, version)
}

func PrivateTaskObservationReady(stateRoot string) (bool, error) {
	return control.PrivateTaskObservationReady(stateRoot)
}

func EnsurePrivateTaskObservationWithBootstrapLock(stateRoot string) error {
	return control.EnsurePrivateTaskObservationWithBootstrapLock(stateRoot)
}

type TerminalEvent = control.TerminalEvent

func PrivateTerminalEventRunnerPath(stateRoot string) string {
	return control.PrivateTerminalEventRunnerPath(stateRoot)
}

func RecordPrivateTerminalEvent(ctx context.Context, stateRoot string, id int64, exitCode string) error {
	return control.RecordPrivateTerminalEvent(ctx, stateRoot, id, exitCode)
}

func RecordPrivateTerminalEventForMaintenance(ctx context.Context, stateRoot string, id int64, exitCode string) (bool, error) {
	return control.RecordPrivateTerminalEventForMaintenance(ctx, stateRoot, id, exitCode)
}

const (
	FixtureVersion                  = control.FixtureVersion
	DefaultObservationTimeout       = control.DefaultObservationTimeout
	PrivateStatusObservationTimeout = control.PrivateStatusObservationTimeout
	RunnerCommandUpgradeTimeout     = control.RunnerCommandUpgradeTimeout
	MaxControlBytes                 = control.MaxControlBytes
)

var (
	ErrConfiguration               = control.ErrConfiguration
	ErrBinding                     = control.ErrBinding
	ErrUnknown                     = control.ErrUnknown
	ErrSubmissionUncertain         = control.ErrSubmissionUncertain
	ErrEnqueueUncertain            = control.ErrEnqueueUncertain
	ErrStagedCapability            = control.ErrStagedCapability
	ErrManagedAdmissionUnsupported = control.ErrManagedAdmissionUnsupported
	ErrInFlight                    = control.ErrInFlight
	ErrControlLimit                = control.ErrControlLimit
	ErrCleanupAuthorization        = control.ErrCleanupAuthorization
	ErrMigrationAuthorization      = control.ErrMigrationAuthorization
	ErrInfrastructureOwnerRequired = control.ErrInfrastructureOwnerRequired
)

type (
	PrivateGenerationGuard      = control.PrivateGenerationGuard
	DaemonStartGuard            = control.DaemonStartGuard
	DaemonWaitOwner             = control.DaemonWaitOwner
	Options                     = control.Options
	PrivateQueueCleanupPlan     = control.PrivateQueueCleanupPlan
	PrivateTerminalCustodyScope = control.PrivateTerminalCustodyScope
	CommandEvent                = control.CommandEvent
	Identity                    = control.Identity
	Launch                      = control.Launch
	State                       = control.State
)

const (
	StateUnknown = control.StateUnknown
	StateQueued  = control.StateQueued
	StateStashed = control.StateStashed
	StateRunning = control.StateRunning
	StateEnded   = control.StateEnded
)

type (
	Observation   = control.Observation
	StopResult    = control.StopResult
	CommandResult = control.CommandResult
	Pending       = control.Pending
)

// Historical target classification remains owned by canonical Pueue recovery.
var (
	ErrHistoricalTargetUnsupported = control.ErrHistoricalTargetUnsupported
	ErrHistoricalTargetNotReady    = control.ErrHistoricalTargetNotReady
)

func PublishVerifiedPrivatePredecessorWithOptionsAndLock(ctx context.Context, root string, predecessor task.SupervisorRef, options Options) error {
	return control.PublishVerifiedPrivatePredecessorWithOptionsAndLock(ctx, root, predecessor, options)
}

func RetirePrivateRecoveryBindingWithOptionsContext(ctx context.Context, stateRoot string, successor task.SupervisorRef, options Options) error {
	return control.RetirePrivateRecoveryBindingWithOptionsContext(ctx, stateRoot, successor, options)
}

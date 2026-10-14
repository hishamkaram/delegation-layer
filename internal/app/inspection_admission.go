package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	commonprovider "github.com/hishamkaram/delegation-layer/internal/provider"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type admissionPreparation struct {
	Profile          PreparedProfile
	Supervisor       *pueue.Client
	RunnerExecutable string
	RunnerOwnership  string
	Capability       CapabilityReport
}

const (
	privateOrdinarySchedulingRevision               = "unlimited-v4"
	privateOrdinarySchedulingCapabilitylessRevision = "unlimited-v3"
	privateOrdinarySchedulingDaemonRevision         = "unlimited-v2"
	privateOrdinarySchedulingLegacyRevision         = "unlimited-v1"
)

type privateOrdinarySchedulingRecord struct {
	SchemaVersion int                          `json:"schema_version"`
	Revision      string                       `json:"revision"`
	Supervisor    task.SupervisorRef           `json:"supervisor"`
	StagedLaunch  pueue.StagedLaunchCapability `json:"staged_launch"`
}

type privateOrdinarySchedulingDaemon struct {
	Executable           string `json:"executable"`
	SHA256               string `json:"sha256"`
	ConfigPath           string `json:"config_path,omitempty"`
	ConfigDigest         string `json:"config_digest,omitempty"`
	ResolvedConfigSHA256 string `json:"resolved_config_sha256,omitempty"`
	Endpoint             string `json:"endpoint,omitempty"`
}

// supervisorOptionsForCandidate carries the provider's bounded, nonsecret
// launch environment into the ordinary supervisor client. The inspection
// worker is started by that supervisor and reconstructs the candidate from its
// process environment; using the same values here keeps its immutable
// definition identical to admission. Inspection commands receive a separate
// timeout-scoped client so ordinary supervisor controls keep their normal
// finite observation boundary.
func supervisorOptionsForCandidate(base pueue.Options, candidate commonprovider.ProfileCandidate) (pueue.Options, error) {
	if base.ObservationTimeout < 0 {
		return pueue.Options{}, pueue.ErrConfiguration
	}
	if candidate.Inspection == nil {
		return base, nil
	}
	definition, _, err := candidate.Inspection.Snapshot()
	if err != nil {
		return pueue.Options{}, err
	}
	return supervisorOptionsWithEnvironment(base, definition.Environment), nil
}

// supervisorOptionsForProfile is the retry/recovery counterpart of
// supervisorOptionsForCandidate. A task that was admitted but whose ordinary
// submission must be retried still needs the exact provider environment that
// was used to construct its immutable plan.
func supervisorOptionsForProfile(base pueue.Options, profile PreparedProfile) pueue.Options {
	return supervisorOptionsWithEnvironment(base, profile.Plan.Environment)
}

func supervisorOptionsForMeta(base pueue.Options, meta task.MetaRecord) pueue.Options {
	return supervisorOptionsWithEnvironment(base, meta.Environment)
}

// supervisorOptionsForCurrentEnvironment snapshots the bounded supervisor
// control environment before runner reconstruction applies a task's provider
// environment to the process. This preserves Linux XDG socket/configuration
// selectors for external supervisors while keeping task credentials out of
// the supervisor context.
func supervisorOptionsForCurrentEnvironment(base pueue.Options) pueue.Options {
	if base.Environment != nil {
		base.Environment = append([]string(nil), base.Environment...)
		return base
	}
	base.Environment = pueue.DefaultEnvironment()
	return base
}

func supervisorOptionsWithEnvironment(base pueue.Options, providerEnvironment []string) pueue.Options {
	if len(providerEnvironment) == 0 {
		return base
	}
	var merged []string
	if base.Environment == nil {
		merged = pueue.DefaultEnvironment()
	} else {
		// Preserve the caller's explicit empty environment. A nil slice means
		// "use the client default"; a non-nil empty slice intentionally means
		// "launch without inherited variables".
		merged = make([]string, len(base.Environment))
		copy(merged, base.Environment)
	}
	positions := make(map[string]int, len(merged)+len(providerEnvironment))
	for index, entry := range merged {
		key, _, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			positions[key] = index
		}
	}
	for _, entry := range providerEnvironment {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		// Provider profiles may isolate their own configuration with
		// XDG_CONFIG_HOME. Supervisor commands must retain the caller's
		// control configuration so their saved endpoint and identity remain
		// resolvable.
		if key == "XDG_CONFIG_HOME" {
			continue
		}
		if index, exists := positions[key]; exists {
			merged[index] = entry
			continue
		}
		positions[key] = len(merged)
		merged = append(merged, entry)
	}
	base.Environment = merged
	return base
}

func prepareAdmission(a Arguments, deps Dependencies, store *taskdir.Store, req task.TaskRecord) (prepared admissionPreparation, resultErr error) {
	var supervisorOwner *supervisorClientOwner
	defer func() {
		if resultErr != nil {
			closeSupervisorOwnerMaintenance(supervisorOwner, resultErr)
		}
	}()
	candidate, err := prepareCandidate(deps, store.Root, req)
	if err != nil {
		return admissionPreparation{}, withDispatchStage("prepare-provider", err)
	}
	profile, err := initialAdmissionProfile(candidate, req)
	if err != nil {
		return admissionPreparation{}, withDispatchStage("finalize-provider", err)
	}
	runnerOwnership := requestedRunnerOwnership(a)
	a.runnerOwnership = runnerOwnership
	runner, err := prepareAdmissionExecutables(a, deps, store.Root)
	if err != nil {
		return admissionPreparation{}, err
	}
	a.Runner = runner
	supervisorOptions, err := supervisorOptionsForCandidate(deps.SupervisorOptions, candidate)
	if err != nil {
		return admissionPreparation{}, withDispatchStage("prepare-supervisor-options", err)
	}
	privateSupervisor, err := admissionUsesPrivateSupervisor(a, store.Root)
	if err != nil {
		return admissionPreparation{}, withDispatchStage("prepare-supervisor-options", err)
	}
	supervisorOwner, err = acquireSupervisorClient(prepareAdmissionSupervisor(a, deps, store, candidate, runner, supervisorOptions, privateSupervisor))
	if err != nil {
		return admissionPreparation{}, withDispatchStage("start-supervisor", err)
	}
	supervisor := supervisorOwner.Client()
	prepared = admissionPreparation{Profile: profile, Supervisor: supervisor, RunnerExecutable: runner, RunnerOwnership: runnerOwnership}
	if candidate.Inspection != nil {
		prepared.Profile, candidate, err = finalizeAdmissionInspection(a, deps, store, req, candidate, supervisor)
		if err != nil {
			return admissionPreparation{}, err
		}
	}
	resumeRetainedPrivateQueueMaintenance(store.Root, supervisor)
	prepared.Capability, err = admissionRuntimeCapability(deps, req, candidate, prepared.Profile)
	if err != nil {
		return admissionPreparation{}, err
	}
	if err = prepared.Profile.ValidateStatePlacement(store.Root); err != nil {
		return admissionPreparation{}, withDispatchStage("validate-state-placement", err)
	}
	supervisorOwner.Release()
	return prepared, nil
}

func resumeRetainedPrivateQueueMaintenance(stateRoot string, supervisor *pueue.Client) {
	if supervisor == nil || !pueue.IsPrivateConfig(stateRoot, supervisor.Binding().ConfigPath) {
		return
	}
	pending, err := pueue.PrivateQueueMaintenanceRetryNeeded(stateRoot)
	if err != nil {
		log.Printf("delegation-layer: retained terminal cleanup probe deferred: %v", err)
		return
	}
	if pending && !supervisor.ScheduleRequestedPrivateQueueMaintenance(context.Background()) {
		log.Printf("delegation-layer: retained terminal cleanup owner deferred")
	}
}

func prepareAdmissionSupervisor(a Arguments, deps Dependencies, store *taskdir.Store, candidate commonprovider.ProfileCandidate, runner string, options pueue.Options, privateSupervisor bool) (*pueue.Client, error) {
	var err error
	var supervisor *pueue.Client
	if privateSupervisor && a.savedSupervisor == nil {
		supervisor, err = preparePrivateAdmissionSupervisor(deps, store, options)
	} else {
		supervisor, err = bindInitialForAdmission(a, deps, store.Root, options, candidate.Inspection != nil)
	}
	if err != nil {
		return supervisor, err
	}
	if !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return supervisor, nil
	}
	if a.savedSupervisor != nil {
		if err = ensurePrivateAdmissionScheduling(store, supervisor, options); err != nil {
			return supervisor, err
		}
	}
	queueRepairRunner, err := admissionQueueRepairRunner(a, deps, runner)
	if err != nil {
		return supervisor, err
	}
	if queueRepairRunner == "" {
		return supervisor, nil
	}
	if err = ensurePrivateQueuedRunnerRepair(store, supervisor, queueRepairRunner); err != nil {
		return supervisor, fmt.Errorf("repair queued task runner paths: %w", err)
	}
	return supervisor, nil
}

// ensurePrivateAdmissionScheduling routes resumed tasks through the same
// private-supervisor scheduling policy as a fresh admission. Recovery may
// reuse an already-running generation, so it cannot rely on the fresh-start
// transition to have published the receipt. The shared probe is no-change
// only; one elected transition owner rechecks and repairs under the bootstrap
// lock when the receipt or observed group state is stale.
func ensurePrivateAdmissionScheduling(store *taskdir.Store, supervisor *pueue.Client, options pueue.Options) error {
	if store == nil || supervisor == nil {
		return errors.New("missing private admission supervisor")
	}
	controlContext, cancel := supervisorControlContext(options)
	defer cancel()
	return ensurePrivateAdmissionSchedulingContext(controlContext, store.Root, supervisor)
}

func ensurePrivateAdmissionSchedulingContext(controlContext context.Context, stateRoot string, supervisor *pueue.Client) error {
	if controlContext == nil {
		return context.Canceled
	}
	if supervisor == nil || !pueue.IsPrivateConfig(stateRoot, supervisor.Binding().ConfigPath) {
		return errors.New("missing private admission supervisor")
	}
	for {
		recorded, err := privateOrdinarySchedulingRecorded(stateRoot, supervisor.RuntimeBinding())
		if err != nil {
			return err
		}
		if recorded {
			return nil
		}
		acquired, transitionErr := pueue.TryWithPrivateTransition(controlContext, stateRoot, func() error {
			return pueue.WithBootstrapLockContext(controlContext, stateRoot, func(lockedContext context.Context) error {
				return ensurePrivateOrdinarySchedulingWithBootstrapLock(lockedContext, stateRoot, supervisor)
			})
		})
		if transitionErr != nil {
			return transitionErr
		}
		if acquired {
			return nil
		}
		if transitionErr = pueue.WaitForPrivateTransition(controlContext, stateRoot); transitionErr != nil {
			return transitionErr
		}
	}
}

func privateOrdinarySchedulingRecorded(stateRoot string, binding task.SupervisorRef) (bool, error) {
	_, recorded, err := privateOrdinarySchedulingAuthorization(stateRoot, binding)
	return recorded, err
}

func privateOrdinarySchedulingAuthorization(stateRoot string, binding task.SupervisorRef) (pueue.SubmissionAuthorization, bool, error) {
	if !pueue.IsPrivateConfig(stateRoot, binding.ConfigPath) {
		return pueue.SubmissionAuthorization{}, false, pueue.ErrConfiguration
	}
	data, found, err := pueue.ReadPrivateSchedulingEvidence(stateRoot)
	if err != nil || !found {
		return pueue.SubmissionAuthorization{}, false, err
	}
	authorization, recorded, err := decodePrivateOrdinaryScheduling(data, binding)
	if err != nil {
		return pueue.SubmissionAuthorization{}, false, errors.Join(pueue.ErrBinding, err)
	}
	return authorization, recorded, nil
}

func privateSubmissionGate(stateRoot string) pueue.SubmissionGate {
	return func(_ context.Context, binding task.SupervisorRef) (pueue.SubmissionAuthorization, error) {
		if !pueue.IsPrivateConfig(stateRoot, binding.ConfigPath) {
			return pueue.SubmissionAuthorization{}, nil
		}
		complete, err := pueue.PrivateSupervisorMigrationComplete(stateRoot, binding)
		if err != nil {
			return pueue.SubmissionAuthorization{}, err
		}
		if !complete {
			return pueue.SubmissionAuthorization{}, pueue.ErrBinding
		}
		authorization, recorded, err := privateOrdinarySchedulingAuthorization(stateRoot, binding)
		if err != nil {
			return pueue.SubmissionAuthorization{}, err
		}
		if !recorded {
			return pueue.SubmissionAuthorization{}, pueue.ErrBinding
		}
		return authorization, nil
	}
}

func decodePrivateOrdinaryScheduling(data []byte, binding task.SupervisorRef) (pueue.SubmissionAuthorization, bool, error) {
	var fields map[string]json.RawMessage
	if err := task.DecodeStrict(data, &fields); err != nil {
		return pueue.SubmissionAuthorization{}, false, err
	}
	if err := validatePrivateOrdinarySchedulingFields(fields); err != nil {
		return pueue.SubmissionAuthorization{}, false, err
	}
	var schemaVersion int
	if err := json.Unmarshal(fields["schema_version"], &schemaVersion); err != nil {
		return pueue.SubmissionAuthorization{}, false, err
	}
	if schemaVersion != task.SchemaVersion {
		return pueue.SubmissionAuthorization{}, false, task.ErrUnsupportedSchema
	}
	var revision string
	if err := json.Unmarshal(fields["revision"], &revision); err != nil {
		return pueue.SubmissionAuthorization{}, false, err
	}
	switch revision {
	case privateOrdinarySchedulingLegacyRevision:
		recorded, err := decodeLegacyPrivateOrdinaryScheduling(fields)
		return pueue.SubmissionAuthorization{}, recorded, err
	case privateOrdinarySchedulingDaemonRevision:
		recorded, err := decodeDaemonPrivateOrdinaryScheduling(fields)
		return pueue.SubmissionAuthorization{}, recorded, err
	case privateOrdinarySchedulingCapabilitylessRevision:
		recorded, err := decodeCapabilitylessPrivateOrdinaryScheduling(fields, binding)
		return pueue.SubmissionAuthorization{}, recorded, err
	case privateOrdinarySchedulingRevision:
		return decodeCurrentPrivateOrdinaryScheduling(fields, binding)
	default:
		return pueue.SubmissionAuthorization{}, false, fmt.Errorf("unsupported private ordinary scheduling revision %q", revision)
	}
}

func validatePrivateOrdinarySchedulingFields(fields map[string]json.RawMessage) error {
	for field := range fields {
		switch field {
		case "schema_version", "revision", "supervisor", "config_digest", "staged_launch":
		default:
			return fmt.Errorf("unknown private ordinary scheduling field %q", field)
		}
	}
	if len(fields["schema_version"]) == 0 || len(fields["revision"]) == 0 {
		return task.ErrEvidenceFault
	}
	return nil
}

func decodeLegacyPrivateOrdinaryScheduling(fields map[string]json.RawMessage) (bool, error) {
	configDigest, err := privateOrdinarySchedulingString(fields, "config_digest")
	if err != nil || len(fields["supervisor"]) != 0 || len(fields["staged_launch"]) != 0 || task.ValidateSHA256(configDigest) != nil {
		return false, task.ErrEvidenceFault
	}
	return false, nil
}

func decodeDaemonPrivateOrdinaryScheduling(fields map[string]json.RawMessage) (bool, error) {
	configDigest, err := privateOrdinarySchedulingString(fields, "config_digest")
	if err != nil || configDigest != "" || len(fields["supervisor"]) == 0 || len(fields["staged_launch"]) != 0 {
		return false, task.ErrEvidenceFault
	}
	var daemon privateOrdinarySchedulingDaemon
	if err := task.DecodeStrict(fields["supervisor"], &daemon); err != nil {
		return false, err
	}
	if err := validatePrivateOrdinarySchedulingDaemon(daemon); err != nil {
		return false, err
	}
	// v2 did not bind the client provenance, so even a complete daemon record
	// is stale for the full-generation contract.
	return false, nil
}

func privateOrdinarySchedulingString(fields map[string]json.RawMessage, field string) (string, error) {
	data := fields[field]
	if len(data) == 0 {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return "", err
	}
	return value, nil
}

func validatePrivateOrdinarySchedulingDaemon(daemon privateOrdinarySchedulingDaemon) error {
	if !canonicalAbsolutePath(daemon.Executable) || task.ValidateSHA256(daemon.SHA256) != nil {
		return task.ErrEvidenceFault
	}
	logical := []string{daemon.ConfigPath, daemon.ConfigDigest, daemon.ResolvedConfigSHA256, daemon.Endpoint}
	present := 0
	for _, value := range logical {
		if value != "" {
			present++
		}
	}
	if present == 0 {
		return nil
	}
	if present != len(logical) || !canonicalAbsolutePath(daemon.ConfigPath) {
		return task.ErrEvidenceFault
	}
	if task.ValidateSHA256(daemon.ConfigDigest) != nil || task.ValidateSHA256(daemon.ResolvedConfigSHA256) != nil {
		return task.ErrEvidenceFault
	}
	if !strings.HasPrefix(daemon.Endpoint, "unix:") || !canonicalAbsolutePath(strings.TrimPrefix(daemon.Endpoint, "unix:")) {
		return task.ErrEvidenceFault
	}
	return nil
}

func canonicalAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, 0)
}

func decodeCapabilitylessPrivateOrdinaryScheduling(fields map[string]json.RawMessage, binding task.SupervisorRef) (bool, error) {
	_, err := decodePrivateOrdinarySchedulingBinding(fields, binding)
	if err != nil || len(fields["staged_launch"]) != 0 {
		if err == nil {
			err = task.ErrEvidenceFault
		}
		return false, err
	}
	// v3 proves the exact generation and scheduling policy but contains no
	// staged command-surface evidence, so it is a valid stale receipt.
	return false, nil
}

func decodeCurrentPrivateOrdinaryScheduling(fields map[string]json.RawMessage, binding task.SupervisorRef) (pueue.SubmissionAuthorization, bool, error) {
	recorded, err := decodePrivateOrdinarySchedulingBinding(fields, binding)
	if err != nil || !recorded {
		return pueue.SubmissionAuthorization{}, recorded, err
	}
	if len(fields["staged_launch"]) == 0 {
		return pueue.SubmissionAuthorization{}, false, task.ErrEvidenceFault
	}
	var capability pueue.StagedLaunchCapability
	if err := task.DecodeStrict(fields["staged_launch"], &capability); err != nil {
		return pueue.SubmissionAuthorization{}, false, err
	}
	if !capability.AddStashed || !capability.EnqueueByID {
		return pueue.SubmissionAuthorization{}, false, task.ErrEvidenceFault
	}
	return pueue.SubmissionAuthorization{StagedLaunchCapability: &capability}, true, nil
}

func decodePrivateOrdinarySchedulingBinding(fields map[string]json.RawMessage, binding task.SupervisorRef) (bool, error) {
	configDigest, err := privateOrdinarySchedulingString(fields, "config_digest")
	if err != nil || configDigest != "" || len(fields["supervisor"]) == 0 {
		return false, task.ErrEvidenceFault
	}
	var recorded task.SupervisorRef
	if err := task.DecodeStrict(fields["supervisor"], &recorded); err != nil {
		return false, err
	}
	if err := validatePrivateOrdinarySchedulingBinding(recorded); err != nil {
		return false, err
	}
	return recorded == binding, nil
}

func validatePrivateOrdinarySchedulingBinding(binding task.SupervisorRef) error {
	if err := task.ValidateFreshSupervisorRef(binding); err != nil {
		return err
	}
	if !canonicalAbsolutePath(binding.DaemonExecutable) {
		return task.ErrEvidenceFault
	}
	return task.ValidateSHA256(binding.DaemonSHA256)
}

func privateOrdinarySchedulingConfiguredGroups(stateRoot string, binding task.SupervisorRef, groups map[string]pueue.Group) (bool, error) {
	recorded, err := privateOrdinarySchedulingRecorded(stateRoot, binding)
	if err != nil || !recorded {
		return recorded, err
	}
	group, found := groups["default"]
	return found && group.ParallelTasks == 0, nil
}

func ensurePrivateOrdinarySchedulingWithBootstrapLock(ctx context.Context, stateRoot string, supervisor *pueue.Client) error {
	recorded, err := privateOrdinarySchedulingRecorded(stateRoot, supervisor.RuntimeBinding())
	if err != nil || recorded {
		return err
	}
	return repairPrivateOrdinarySchedulingWithBootstrapLock(ctx, stateRoot, supervisor)
}

func repairPrivateOrdinarySchedulingWithBootstrapLock(ctx context.Context, stateRoot string, supervisor *pueue.Client) error {
	groups, err := supervisor.SetPrivateDefaultParallelTasksWithBootstrapLock(ctx, stateRoot, 0)
	if err != nil {
		return err
	}
	group, found := groups["default"]
	if !found || group.ParallelTasks != 0 {
		return fmt.Errorf("%w: private supervisor did not retain unlimited ordinary task parallelism", pueue.ErrBinding)
	}
	capability, err := supervisor.StagedLaunchCapabilityWithBootstrapLock(ctx, stateRoot)
	if err != nil {
		return err
	}
	return publishPrivateOrdinaryScheduling(stateRoot, supervisor.RuntimeBinding(), capability)
}

func publishPrivateOrdinaryScheduling(stateRoot string, binding task.SupervisorRef, capability pueue.StagedLaunchCapability) error {
	if !pueue.IsPrivateConfig(stateRoot, binding.ConfigPath) {
		return pueue.ErrConfiguration
	}
	if err := validatePrivateOrdinarySchedulingBinding(binding); err != nil {
		return errors.Join(pueue.ErrConfiguration, err)
	}
	if !capability.AddStashed || !capability.EnqueueByID {
		return errors.Join(pueue.ErrConfiguration, pueue.ErrStagedCapability)
	}
	data, err := task.MarshalCanonical(privateOrdinarySchedulingRecord{
		SchemaVersion: task.SchemaVersion,
		Revision:      privateOrdinarySchedulingRevision,
		Supervisor:    binding,
		StagedLaunch:  capability,
	})
	if err != nil {
		return errors.Join(pueue.ErrConfiguration, err)
	}
	return pueue.ReplacePrivateSchedulingEvidence(stateRoot, data)
}

func initialAdmissionProfile(candidate commonprovider.ProfileCandidate, req task.TaskRecord) (PreparedProfile, error) {
	if candidate.Inspection != nil {
		return PreparedProfile{}, nil
	}
	return finalizeCandidate(candidate, req, nil)
}

func prepareAdmissionExecutables(a Arguments, deps Dependencies, root string) (string, error) {
	runnerOwnership := requestedRunnerOwnership(a)
	privateSupervisor, err := admissionUsesPrivateSupervisor(a, root)
	if err != nil {
		return "", withDispatchStage("prepare-supervisor-executables", err)
	}
	runner, err := resolveAdmissionRunner(a.Runner, runnerOwnership, deps)
	if err != nil {
		return "", withDispatchStage("resolve-runner", err)
	}
	if !privateSupervisor {
		return runner, nil
	}
	return preparePrivateAdmissionExecutables(a, deps, root, runner, runnerOwnership)
}

func resolveAdmissionRunner(argument, ownership string, deps Dependencies) (string, error) {
	if ownership == task.RunnerOwnershipManaged {
		argument = ""
	} else if argument == "" {
		return "", task.ErrEvidenceFault
	}
	return resolveRunner(argument, deps)
}

func preparePrivateAdmissionExecutables(a Arguments, deps Dependencies, root, runner, runnerOwnership string) (string, error) {
	controlContext, cancel := supervisorControlContext(deps.SupervisorOptions)
	defer cancel()
	terminalRunner, err := resolveRunner("", deps)
	if err != nil {
		return "", withDispatchStage("resolve-runner", err)
	}
	if _, err = prepareStateTerminalEventRunner(controlContext, root, terminalRunner); err != nil {
		return "", withDispatchStage("prepare-runner", err)
	}
	if runnerOwnership == task.RunnerOwnershipManaged {
		runner, err = installStateRunner(root, runner)
		if err != nil {
			return "", withDispatchStage("prepare-runner", err)
		}
	}
	clientExecutable, daemonExecutable, err := admissionSupervisorExecutablePair(a, root, deps)
	if err != nil {
		return "", withDispatchStage("prepare-supervisor-executables", err)
	}
	if err = installStateSupervisorPairForAdmission(controlContext, root, clientExecutable, daemonExecutable); err != nil {
		return "", withDispatchStage("prepare-supervisor-executables", err)
	}
	return runner, nil
}

func admissionSupervisorExecutablePair(a Arguments, root string, deps Dependencies) (string, string, error) {
	if a.savedSupervisor != nil && privateSupervisorExecutablesAvailable(a.savedSupervisor.ClientExecutable, a.savedSupervisor.DaemonExecutable) {
		return a.savedSupervisor.ClientExecutable, a.savedSupervisor.DaemonExecutable, nil
	}
	if a.savedSupervisor != nil {
		return resolvePrivateSupervisorExecutables(root, deps)
	}
	// Fresh admission stages the active install as successor inventory. The
	// completed publication remains the ordinary active-generation authority.
	return resolveCurrentPrivateSupervisorExecutables(root, deps)
}

func installStateSupervisorPairForAdmission(ctx context.Context, root, clientExecutable, daemonExecutable string) error {
	return prepareStateSupervisorPair(ctx, root, clientExecutable, daemonExecutable)
}

func requestedRunnerOwnership(a Arguments) string {
	if a.runnerOwnership != "" {
		return a.runnerOwnership
	}
	if a.Runner != "" {
		return task.RunnerOwnershipCustom
	}
	return task.RunnerOwnershipManaged
}

func admissionQueueRepairRunner(a Arguments, deps Dependencies, taskRunner string) (string, error) {
	if requestedRunnerOwnership(a) == task.RunnerOwnershipManaged {
		return taskRunner, nil
	}
	runner, err := resolveRunner("", deps)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return runner, nil
}

func admissionUsesPrivateSupervisor(a Arguments, root string) (bool, error) {
	if a.savedSupervisor != nil {
		return pueue.IsPrivateConfig(root, a.savedSupervisor.ConfigPath), nil
	}
	configPath, err := resolveInitialConfig(a)
	if err != nil {
		return false, err
	}
	if configPath != "" && pueue.IsPrivateConfig(root, configPath) {
		return false, fmt.Errorf("%w: explicit supervisor config collides with the private state-rooted supervisor", pueue.ErrConfiguration)
	}
	return configPath == "", nil
}

func finalizeAdmissionInspection(a Arguments, deps Dependencies, store *taskdir.Store, req task.TaskRecord, candidate commonprovider.ProfileCandidate, supervisor *pueue.Client) (PreparedProfile, commonprovider.ProfileCandidate, error) {
	var err error
	candidate, err = refreshAdmissionCandidate(deps, store.Root, req, candidate)
	if err != nil {
		return PreparedProfile{}, commonprovider.ProfileCandidate{}, withDispatchStage("finalize-provider", err)
	}
	facts, _, err := admissionInspectionFacts(a, deps, store, req, candidate, supervisor, false)
	if err != nil {
		return PreparedProfile{}, commonprovider.ProfileCandidate{}, withDispatchStage("inspect-provider", err)
	}
	// The inspection worker reconstructs the candidate when it actually runs.
	// Refresh once more after its proof is complete so finalization uses the
	// same current provider identity that the worker inspected. The finalizer
	// still binds the returned facts to this candidate; a replacement during
	// this last handoff fails closed instead of relabeling the proof.
	candidate, err = refreshAdmissionCandidate(deps, store.Root, req, candidate)
	if err != nil {
		return PreparedProfile{}, commonprovider.ProfileCandidate{}, withDispatchStage("finalize-provider", err)
	}
	profile, err := finalizeCandidate(candidate, req, facts)
	if err != nil {
		return PreparedProfile{}, commonprovider.ProfileCandidate{}, withDispatchStage("finalize-provider", err)
	}
	// The operation deadline bounds provider work and its evidence. Once that
	// evidence is timely and the provider identity is refreshed above, local
	// task creation must not race the already-expired provider clock.
	return profile, candidate, nil
}

func admissionRuntimeCapability(deps Dependencies, req task.TaskRecord, candidate commonprovider.ProfileCandidate, profile PreparedProfile) (CapabilityReport, error) {
	registration, err := deps.normalized().Catalog.Lookup(req.Provider)
	if err != nil {
		return CapabilityReport{}, withDispatchStage("select-provider", err)
	}
	capability, err := runtimeCapability(req.Provider, candidate, profile, registration.Description)
	if err != nil {
		return CapabilityReport{}, withDispatchStage("record-runtime-capability", err)
	}
	return capability, nil
}

func ensureInspectionGroup(store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration) (resultErr error) {
	return ensureInspectionGroupWithMigration(store, supervisor, timeout, false)
}

// ensureInspectionGroupWithoutMigration binds a historical continuation to
// the exact inspection generation it recorded. A continuation must never
// rewrite the root journal backwards to its predecessor supervisor.
func ensureInspectionGroupWithoutMigration(store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration) (resultErr error) {
	if store == nil || supervisor == nil {
		return errors.New("missing inspection supervisor")
	}
	if timeout <= 0 {
		return pueue.ErrConfiguration
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return ensureInspectionGroupHistorical(ctx, store, supervisor)
	}
	if ready, probeErr := probeEstablishedInspectionGroup(ctx, store, supervisor); probeErr != nil || ready {
		return probeErr
	}
	return pueue.WithBootstrapLockContext(ctx, store.Root, func(lockedContext context.Context) error {
		return ensureInspectionGroupHistorical(lockedContext, store, supervisor)
	})
}

func ensureInspectionGroupHistorical(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client) (resultErr error) {
	group, err := inspection.OpenGroupHistorical(ctx, store, supervisor.Binding())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, group.Close()) }()
	return ensureInspectionGroupReady(ctx, store, supervisor, group)
}

func ensureInspectionGroupWithMigration(store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration, allowMigration bool) (resultErr error) {
	return ensureInspectionGroupWithMigrationContext(context.Background(), store, supervisor, timeout, allowMigration)
}

func ensureInspectionGroupWithMigrationContext(parent context.Context, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration, allowMigration bool) (resultErr error) {
	ctx, cancel, err := inspectionGroupContext(parent, store, supervisor, timeout)
	if err != nil {
		return err
	}
	defer cancel()
	// Ordinary admission never stops or rebinds a reachable supervisor. The
	// private bootstrap path already adopted the endpoint under its bootstrap
	// lock; the group journal has its own rooted transaction lock. Migration is
	// an explicit supervisor repair operation.
	ensure := func(ensureContext context.Context) (ensureErr error) {
		if !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
			if err := rejectExternalInspectionMigration(ensureContext, store, supervisor); err != nil {
				return err
			}
		} else if err := recoverInspectionGroupGenerationWithLock(ensureContext, store, supervisor); err != nil {
			return err
		}
		migrationCheck := inspectionGroupMigrationCheck(store, supervisor, timeout, allowMigration)
		group, err := inspection.OpenGroupWithMigration(ensureContext, store, supervisor.Binding(), migrationCheck)
		if err != nil {
			return err
		}
		defer func() { ensureErr = errors.Join(ensureErr, group.Close()) }()
		if err := ensureInspectionGroupReady(ensureContext, store, supervisor, group); err != nil {
			return err
		}
		if pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
			if err := clearCompletedInspectionRecovery(ensureContext, store, supervisor); err != nil {
				return err
			}
		}
		return nil
	}
	if !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return ensure(ctx)
	}
	if ready, probeErr := probeEstablishedInspectionGroup(ctx, store, supervisor); probeErr != nil || ready {
		return probeErr
	}
	return pueue.WithBootstrapLockContext(ctx, store.Root, ensure)
}

func inspectionGroupContext(parent context.Context, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if parent == nil {
		return nil, nil, context.Canceled
	}
	if store == nil || supervisor == nil {
		return nil, nil, errors.New("missing inspection supervisor")
	}
	if timeout <= 0 {
		return nil, nil, pueue.ErrConfiguration
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	return ctx, cancel, nil
}

func probeEstablishedInspectionGroup(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client) (bool, error) {
	ready := false
	err := pueue.WithSupervisorLeaseContext(ctx, store.Root, func(lockedContext context.Context) error {
		complete, completeErr := pueue.PrivateSupervisorMigrationComplete(store.Root, supervisor.Binding())
		if completeErr != nil || !complete {
			return completeErr
		}
		snapshot, snapshotErr := supervisor.Snapshot(lockedContext)
		if snapshotErr != nil {
			return snapshotErr
		}
		var probeErr error
		ready, probeErr = inspection.ProbeGroupContext(lockedContext, store, supervisor.Binding(), snapshot)
		return probeErr
	})
	if errors.Is(err, pueue.ErrPrivateSupervisorNotInitialized) {
		// This root has no published lifecycle boundary to observe. Let the
		// caller elect the exclusive bootstrap transition; a shared probe must
		// not create the missing lock merely to report it absent.
		return false, nil
	}
	return ready, err
}

func rejectExternalInspectionMigration(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client) error {
	legacy, _, err := inspection.ReadGroupHead(ctx, store)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if task.SameSupervisorIdentity(legacy.Supervisor, supervisor.Binding()) {
		return nil
	}
	if !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return fmt.Errorf("%w: Delegate does not migrate an existing external supervisor", inspection.ErrGroupMigrationUnsupported)
	}
	if !pueue.IsPrivateConfig(store.Root, legacy.Supervisor.ConfigPath) {
		return fmt.Errorf("%w: Delegate does not migrate an existing external supervisor", inspection.ErrGroupMigrationUnsupported)
	}
	return nil
}

func preparePrivateAdmissionSupervisor(deps Dependencies, store *taskdir.Store, options pueue.Options) (*pueue.Client, error) {
	controlContext, cancel := supervisorControlContext(options)
	defer cancel()
	return preparePrivateAdmissionSupervisorContext(controlContext, deps, store, options)
}

func preparePrivateAdmissionSupervisorContext(ctx context.Context, deps Dependencies, store *taskdir.Store, options pueue.Options) (*pueue.Client, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if store == nil {
		return nil, errors.New("missing inspection state store")
	}
	for {
		if supervisor, err := probePrivateAdmissionSupervisor(ctx, store, "", "", options); err != nil || supervisor != nil {
			return supervisor, err
		}
		supervisor, acquired, transitionErr := electPrivateAdmissionTransition(ctx, deps, store, options)
		if transitionErr != nil {
			return nil, closePrivateAdmissionTransition(supervisor, transitionErr)
		}
		if acquired {
			if supervisor == nil {
				return nil, pueue.ErrConfiguration
			}
			return supervisor, nil
		}
		if err := pueue.WaitForPrivateTransition(ctx, store.Root); err != nil {
			return nil, err
		}
	}
}

func electPrivateAdmissionTransition(ctx context.Context, deps Dependencies, store *taskdir.Store, options pueue.Options) (*pueue.Client, bool, error) {
	var supervisor *pueue.Client
	acquired, err := pueue.TryWithPrivateTransition(ctx, store.Root, func() error {
		var probeErr error
		supervisor, probeErr = probePrivateAdmissionSupervisor(ctx, store, "", "", options)
		if probeErr != nil || supervisor != nil {
			return probeErr
		}
		return pueue.WithBootstrapLockContext(ctx, store.Root, func(lockedContext context.Context) error {
			var prepareErr error
			supervisor, prepareErr = preparePrivateAdmissionTransition(lockedContext, deps, store, options)
			return prepareErr
		})
	})
	return supervisor, acquired, err
}

func preparePrivateAdmissionTransition(ctx context.Context, deps Dependencies, store *taskdir.Store, options pueue.Options) (*pueue.Client, error) {
	configExisted, err := privateSupervisorConfigExisted(store.Root)
	if err != nil {
		return nil, err
	}
	if err = pueue.EnsurePrivateTaskObservationWithBootstrapLock(store.Root); err != nil {
		return nil, err
	}
	supervisor, ready, err := repairLivePrivateAdmissionTransition(ctx, store, options, configExisted)
	if err != nil || ready {
		return supervisor, err
	}
	supervisor, published, err := pueue.PreparePublishedPrivateWithLock(ctx, store.Root, options)
	if err != nil {
		return nil, err
	}
	if published {
		return restartPublishedPrivateAdmission(ctx, store, supervisor, configExisted)
	}
	return bootstrapPrivateAdmission(ctx, deps, store, options, configExisted)
}

func repairLivePrivateAdmissionTransition(ctx context.Context, store *taskdir.Store, options pueue.Options, configExisted bool) (*pueue.Client, bool, error) {
	supervisor, groups, ready, err := pueue.ProbePrivateGroupsWithBootstrapLock(ctx, "", "", store.Root, options)
	if err != nil || !ready {
		return supervisor, ready, err
	}
	if err = rejectPrivateAdmissionMigrationAfterEndpointProbe(ctx, store, supervisor, configExisted); err != nil {
		return supervisor, true, err
	}
	configured, err := privateOrdinarySchedulingConfiguredGroups(store.Root, supervisor.Binding(), groups)
	if err != nil || configured {
		return supervisor, true, err
	}
	err = repairPrivateOrdinarySchedulingWithBootstrapLock(ctx, store.Root, supervisor)
	return supervisor, true, err
}

func restartPublishedPrivateAdmission(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, configExisted bool) (*pueue.Client, error) {
	if supervisor == nil {
		return nil, pueue.ErrConfiguration
	}
	if err := rejectPrivateAdmissionMigration(ctx, store, supervisor, configExisted); err != nil {
		return supervisor, err
	}
	if err := supervisor.StartPrivate(ctx, store.Root); err != nil {
		return supervisor, err
	}
	return supervisor, ensurePrivateOrdinarySchedulingWithBootstrapLock(ctx, store.Root, supervisor)
}

func bootstrapPrivateAdmission(ctx context.Context, deps Dependencies, store *taskdir.Store, options pueue.Options, configExisted bool) (*pueue.Client, error) {
	clientExecutable, daemonExecutable, err := resolvePrivateSupervisorExecutables(store.Root, deps)
	if err != nil {
		return nil, err
	}
	supervisor, err := pueue.PreparePrivateWithLock(ctx, clientExecutable, daemonExecutable, store.Root, options)
	if err != nil {
		return nil, err
	}
	if err = rejectPrivateAdmissionMigration(ctx, store, supervisor, configExisted); err != nil {
		return supervisor, err
	}
	if err = supervisor.StartPrivate(ctx, store.Root); err != nil {
		return supervisor, err
	}
	return supervisor, ensurePrivateOrdinarySchedulingWithBootstrapLock(ctx, store.Root, supervisor)
}

func closePrivateAdmissionTransition(supervisor *pueue.Client, err error) error {
	if supervisor == nil {
		return err
	}
	return errors.Join(err, supervisor.CloseAfterPending(err))
}

// Healthy admission observes a published generation under a shared lease.
// Missing or incomplete bootstrap falls through to the exclusive transition,
// which rechecks all state before any start or identity publication.
func probePrivateAdmissionSupervisor(ctx context.Context, store *taskdir.Store, clientExecutable, daemonExecutable string, options pueue.Options) (*pueue.Client, error) {
	client, ready, err := pueue.ProbePrivateEndpointWithGate(ctx, clientExecutable, daemonExecutable, store.Root, options, func(checkContext context.Context, current *pueue.Client) (bool, error) {
		if err := rejectPrivateAdmissionMigrationAfterEndpointProbe(checkContext, store, current, true); err != nil {
			return false, err
		}
		observationReady, err := pueue.PrivateTaskObservationReady(store.Root)
		if err != nil || !observationReady {
			return false, err
		}
		return privateOrdinarySchedulingRecorded(store.Root, current.Binding())
	})
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, client.Close()
	}
	return client, nil
}

func privateSupervisorConfigExisted(root string) (bool, error) {
	_, err := os.Stat(pueue.PrivateConfigPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func rejectPrivateAdmissionMigration(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, configExisted bool) error {
	return rejectPrivateAdmissionMigrationWithEndpointProof(ctx, store, supervisor, configExisted, false)
}

// rejectPrivateAdmissionMigrationAfterEndpointProbe reuses the liveness proof
// already established by the warm endpoint probe or a protocol-level group
// probe. It avoids an identical full status observation solely to decide
// whether a published live endpoint may remain in place.
func rejectPrivateAdmissionMigrationAfterEndpointProbe(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, configExisted bool) error {
	return rejectPrivateAdmissionMigrationWithEndpointProof(ctx, store, supervisor, configExisted, true)
}

func rejectPrivateAdmissionMigrationWithEndpointProof(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, configExisted, endpointProvenLive bool) error {
	if store == nil || supervisor == nil {
		return errors.New("missing private admission supervisor")
	}
	resolution, err := resolvePrivateSupervisorGenerationWithLock(ctx, store, supervisor.Binding())
	if err != nil {
		return err
	}
	if resolution.PrivateAuthority {
		if resolution.Active == supervisor.Binding() {
			return nil
		}
		return fmt.Errorf("%w: admission supervisor is not the active private generation", inspection.ErrGroupMigrationRequired)
	}
	if !resolution.GroupPresent {
		if !configExisted {
			return nil
		}
		bootstrapPending, bootstrapErr := pueue.PrivateBootstrapPending(store.Root)
		if bootstrapErr != nil {
			return bootstrapErr
		}
		if bootstrapPending {
			// A Delegate-owned bootstrap marker proves that this config was
			// created by a startup interrupted before identity publication.
			// It is safe to retry that same bootstrap; an unmarked config-only
			// root remains an unverified predecessor and fails closed below.
			return nil
		}
		if endpointProvenLive {
			return nil
		}
		return supervisor.VerifyPrivateMigrationEndpoint(ctx, store.Root)
	}
	if task.SameSupervisorIdentity(resolution.Active, supervisor.Binding()) {
		return nil
	}
	if !pueue.IsPrivateConfig(store.Root, resolution.GroupHead.Supervisor.ConfigPath) {
		return fmt.Errorf("%w: Delegate does not migrate an existing external supervisor", inspection.ErrGroupMigrationUnsupported)
	}
	return fmt.Errorf("%w: existing private inspection supervisor requires explicit migration", inspection.ErrGroupMigrationRequired)
}

func migrateInspectionSupervisorLocked(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration) error {
	if store == nil || supervisor == nil {
		return errors.New("missing inspection supervisor")
	}
	present, matches, err := inspection.GroupHeadMatchesBinding(ctx, store, supervisor.Binding())
	if err != nil {
		return err
	}
	if !present || matches {
		if migrationErr := supervisor.MigratePrivateSupervisorWithLock(ctx, store.Root); migrationErr != nil {
			return migrationErr
		}
	}
	if !present {
		return nil
	}
	group, err := inspection.OpenGroupWithMigration(ctx, store, supervisor.Binding(), func(checkCtx context.Context, legacy inspection.GroupRequestRecord) error {
		return migrateInspectionGroup(checkCtx, store, supervisor, timeout, legacy)
	})
	if err != nil {
		return err
	}
	return group.Close()
}

func ensureInspectionGroupLocked(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration, allowMigration bool) (resultErr error) {
	if pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		if err := recoverInspectionGroupGenerationWithLock(ctx, store, supervisor); err != nil {
			return err
		}
	}
	if err := prepareInspectionGroupSupervisor(ctx, store, supervisor, allowMigration); err != nil {
		return err
	}
	migrationCheck := inspectionGroupMigrationCheck(store, supervisor, timeout, allowMigration)
	group, err := inspection.OpenGroupWithMigration(ctx, store, supervisor.Binding(), migrationCheck)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, group.Close()) }()
	if pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		if err := supervisor.StartPrivate(ctx, store.Root); err != nil {
			return err
		}
		if err := clearCompletedInspectionRecovery(ctx, store, supervisor); err != nil {
			return err
		}
	}
	return ensureInspectionGroupReady(ctx, store, supervisor, group)
}

func prepareInspectionGroupSupervisor(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, allowMigration bool) error {
	if !allowMigration || !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return nil
	}
	present, matches, err := inspection.GroupHeadMatchesBinding(ctx, store, supervisor.Binding())
	if err != nil {
		return err
	}
	if present && matches {
		if err := pueue.PublishPrivateSupervisorBindingIfAbsent(store.Root, supervisor.Binding()); err != nil {
			return err
		}
		// A completed logical migration is the durable proof that the
		// predecessor endpoint has already been retired. Reconcile its
		// recovery alias before daemon-provenance repair so a retry cannot
		// mistake the completed transition for a fresh legacy binding.
		if err := clearCompletedInspectionRecovery(ctx, store, supervisor); err != nil {
			return err
		}
	}
	if !present {
		return supervisor.MigratePrivateSupervisorWithLock(ctx, store.Root)
	}
	// A stale inspection head is the authority for the legacy transition. Its
	// queue must be inspected before private migration starts a successor; the
	// migration callback performs that ordering while this supervisor remains
	// prepared but stopped.
	return nil
}

func inspectionGroupMigrationCheck(store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration, allowMigration bool) func(context.Context, inspection.GroupRequestRecord) error {
	if !allowMigration {
		return nil
	}
	return func(ctx context.Context, legacy inspection.GroupRequestRecord) error {
		return migrateInspectionGroup(ctx, store, supervisor, timeout, legacy)
	}
}

func clearCompletedInspectionRecovery(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client) error {
	if store == nil || supervisor == nil || !pueue.IsPrivateConfig(store.Root, supervisor.Binding().ConfigPath) {
		return nil
	}
	head, migrated, err := inspection.ReadGroupHead(ctx, store)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !migrated || !task.SameSupervisorIdentity(head.Supervisor, supervisor.Binding()) {
		return nil
	}
	return pueue.RetirePrivateRecoveryBindingContext(ctx, store.Root, supervisor.Binding())
}

func migrateInspectionGroup(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration, legacy inspection.GroupRequestRecord) error {
	successor := privateInspectionGroup(store.RootID, supervisor.Binding())
	complete, err := inspection.GroupMigrationComplete(ctx, store, legacy, successor)
	if err != nil {
		return fmt.Errorf("%w: read supervisor migration journal: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if complete {
		return nil
	}
	if !pueue.IsPrivateConfig(store.Root, legacy.Supervisor.ConfigPath) || !pueue.IsPrivateConfig(store.Root, successor.Supervisor.ConfigPath) {
		return fmt.Errorf("%w: only Delegate private supervisors can be migrated", inspection.ErrGroupMigrationUnsupported)
	}
	if err = reconcileStaleInspectionRecovery(ctx, store, legacy); err != nil {
		return err
	}
	if err = inspection.PrepareGroupMigration(ctx, store, legacy, successor); err != nil {
		return fmt.Errorf("%w: prepare supervisor migration journal: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if err = ensureInspectionPrivateMigration(ctx, store.Root, supervisor, legacy.Supervisor, successor.Supervisor); err != nil {
		return err
	}
	if err := inspection.CompleteGroupMigration(ctx, store, legacy, successor); err != nil {
		return fmt.Errorf("%w: publish supervisor migration journal: %w", inspection.ErrGroupMigrationRequired, err)
	}
	return nil
}

func ensureInspectionPrivateMigration(ctx context.Context, stateRoot string, supervisor *pueue.Client, predecessor, successor task.SupervisorRef) error {
	complete, err := pueue.PrivateSupervisorMigrationEdgeCompleteWithLock(ctx, stateRoot, predecessor, successor)
	if err != nil {
		return fmt.Errorf("%w: verify completed private supervisor migration: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if complete {
		return nil
	}
	published, err := pueue.PrivateSupervisorMigrationEdgePublishedWithLock(ctx, stateRoot, predecessor, successor)
	if err != nil {
		return fmt.Errorf("%w: verify published private supervisor migration: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if !published {
		if err = pueue.PublishPrivateSupervisorBindingIfAbsent(stateRoot, predecessor); err != nil {
			return fmt.Errorf("%w: preserve legacy supervisor binding: %w", inspection.ErrGroupMigrationRequired, err)
		}
	}
	if err = supervisor.MigratePrivateSupervisorWithLock(ctx, stateRoot); err != nil {
		return fmt.Errorf("%w: migrate private supervisor with legacy inspection evidence: %w", inspection.ErrGroupMigrationRequired, err)
	}
	complete, err = pueue.PrivateSupervisorMigrationEdgeCompleteWithLock(ctx, stateRoot, predecessor, successor)
	if err != nil {
		return fmt.Errorf("%w: verify reconciled private supervisor migration: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if !complete {
		return fmt.Errorf("%w: reconciled private supervisor migration remains incomplete", inspection.ErrGroupMigrationRequired)
	}
	return nil
}

func reconcileStaleInspectionRecovery(ctx context.Context, store *taskdir.Store, legacy inspection.GroupRequestRecord) error {
	if store == nil {
		return fmt.Errorf("%w: inspection state store is unavailable", inspection.ErrGroupMigrationRequired)
	}
	saved, found, err := pueue.ReadPrivateRecoveryBinding(store.Root)
	if err != nil || !found {
		return err
	}
	if task.SameSupervisorIdentity(saved, legacy.Supervisor) {
		return nil
	}
	historical, err := inspection.OpenGroupHistorical(ctx, store, saved)
	if err != nil {
		return fmt.Errorf("%w: stale private supervisor recovery is not a published predecessor: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if err = historical.Close(); err != nil {
		return fmt.Errorf("%w: close stale private supervisor history: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if err = pueue.ClearPrivateRecoveryBinding(store.Root, saved); err != nil {
		return fmt.Errorf("%w: retire stale private supervisor recovery: %w", inspection.ErrGroupMigrationRequired, err)
	}
	return nil
}

func ensureInspectionGroupReady(ctx context.Context, store *taskdir.Store, supervisor *pueue.Client, group *inspection.GroupJournal) error {
	snapshot, err := supervisor.Snapshot(ctx)
	if err != nil {
		return err
	}
	if _, exists := snapshot.Groups[group.Name()]; exists {
		return group.ObserveContext(ctx, snapshot)
	}
	permit, err := group.ClaimCreateContext(ctx, time.Now())
	if err != nil {
		return err
	}
	_, createErr := supervisor.CreateInspectionGroup(ctx, permit, store.RootID)
	if pending := inFlightInspectionError(createErr); pending != nil {
		// The create command retains the private supervisor lease until it
		// naturally completes. Returning its pending owner preserves that
		// serialization without extending this caller past its finite deadline.
		return createErr
	}
	// A failed or lost response never permits another create. A fresh exact
	// snapshot can resolve the saved intent, including after an uncertain reply.
	if err = ctx.Err(); err != nil {
		return errors.Join(createErr, err)
	}
	snapshot, observeErr := supervisor.Snapshot(ctx)
	if observeErr != nil {
		return errors.Join(createErr, observeErr)
	}
	if observeErr = group.ObserveContext(ctx, snapshot); observeErr != nil {
		return errors.Join(createErr, observeErr)
	}
	return nil
}

func inspectLegacyEndedJob(ctx context.Context, store *taskdir.Store, legacy inspection.GroupRequestRecord, job pueue.Job) (resultErr error) {
	if store == nil {
		return fmt.Errorf("%w: inspection state store is unavailable", inspection.ErrGroupMigrationRequired)
	}
	if legacy.RootID != store.RootID || legacy.Name != (pueue.InspectionIdentity{RootID: store.RootID}).Group() || legacy.ParallelTasks != 1 || task.ValidateFreshSupervisorRef(legacy.Supervisor) != nil {
		return fmt.Errorf("%w: invalid legacy inspection group binding", inspection.ErrGroupMigrationRequired)
	}
	if job.Label == nil {
		return nil
	}
	taskID, managed, inspectionWorker, err := legacyJobTaskID(*job.Label, store.RootID)
	if err != nil {
		return fmt.Errorf("%w: ended Delegate job label is invalid: %w", inspection.ErrGroupMigrationRequired, err)
	}
	if !managed {
		return nil
	}
	if inspectionWorker {
		return inspectLegacyEndedInspection(ctx, store, legacy, taskID, job)
	}
	return inspectLegacyEndedTask(ctx, store, legacy, taskID, job)
}

func inspectLegacyEndedTask(ctx context.Context, store *taskdir.Store, legacy inspection.GroupRequestRecord, taskID string, job pueue.Job) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	td, err := store.OpenTask(taskID)
	if err != nil {
		return fmt.Errorf("%w: ended Delegate task %s could not be reopened: %w", inspection.ErrGroupMigrationRequired, taskID, err)
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	receipt, err := td.ReadSupervisorReceipt()
	if err != nil {
		return fmt.Errorf("%w: ended Delegate task %s has no valid supervisor receipt: %w", inspection.ErrGroupMigrationRequired, taskID, err)
	}
	belongsToGeneration, generationErr := privateMigrationSupervisorMatches(store.Root, legacy.Supervisor, receipt.SupervisorRef())
	if receipt.NumericTaskID != job.ID || receipt.RootID != store.RootID || generationErr != nil || !belongsToGeneration {
		return fmt.Errorf("%w: ended Delegate task %s supervisor receipt does not match the legacy job", inspection.ErrGroupMigrationRequired, taskID)
	}
	result, err := td.Inspect()
	if err != nil {
		return fmt.Errorf("%w: ended Delegate task %s could not be inspected: %w", inspection.ErrGroupMigrationRequired, taskID, err)
	}
	if !isTerminal(result) {
		return fmt.Errorf("%w: ended Delegate task %s is awaiting publication", inspection.ErrGroupMigrationBusy, taskID)
	}
	return nil
}

func validateLegacyEndedInspectionJob(taskID string, job pueue.Job) error {
	if job.State != pueue.StateEnded {
		return fmt.Errorf("%w: Delegate inspection %s has not ended", inspection.ErrGroupMigrationBusy, taskID)
	}
	return nil
}

func validateLegacyEndedInspectionEvidence(ctx context.Context, operation *inspection.Operation, taskID string, job pueue.Job) error {
	result, succeeded, err := operation.ReadWorkerResultContext(ctx)
	managed := operation.Request().Binding.WorkerExitProof == inspection.WorkerExitProofGuardianWaitV1
	if managed && errors.Is(err, inspection.ErrAdmissionExpired) {
		return nil
	}
	if err != nil || (!managed && succeeded != job.Succeeded) || (succeeded && (result == nil || result.Reason != inspection.ResultEligible)) {
		return fmt.Errorf("%w: ended Delegate inspection %s has no valid terminal evidence: %w", inspection.ErrGroupMigrationRequired, taskID, errors.Join(task.ErrEvidenceFault, err))
	}
	return nil
}

func validateLegacyEndedInspectionBinding(ctx context.Context, store *taskdir.Store, legacy inspection.GroupRequestRecord, operation *inspection.Operation, taskID string, job pueue.Job) error {
	request := operation.Request()
	receipt, err := operation.ReceiptContext(ctx)
	if err != nil {
		return fmt.Errorf("%w: ended Delegate inspection %s has no valid supervisor receipt: %w", inspection.ErrGroupMigrationRequired, taskID, err)
	}
	belongsToGeneration, generationErr := privateMigrationSupervisorMatches(store.Root, legacy.Supervisor, request.Binding.Supervisor)
	if receipt.NumericTaskID == nil || *receipt.NumericTaskID != job.ID || job.Group != request.Group || (request.Group != "default" && request.Group != legacy.Name) || generationErr != nil || !belongsToGeneration {
		return fmt.Errorf("%w: ended Delegate inspection %s supervisor receipt does not match the legacy job", inspection.ErrGroupMigrationRequired, taskID)
	}
	return nil
}

func validateLegacyEndedInspectionLifetime(ctx context.Context, operation *inspection.Operation, taskID string, job pueue.Job) error {
	if !job.Succeeded {
		if err := recoverInspectionLifetime(ctx, operation); err != nil {
			return fmt.Errorf("%w: ended Delegate inspection %s lifetime termination is unproven: %w", inspection.ErrGroupMigrationRequired, taskID, err)
		}
		return nil
	}
	ready, err := successfulInspectionLifetimeEvidence(ctx, operation)
	if err != nil {
		return fmt.Errorf("%w: ended Delegate inspection %s lifetime evidence is invalid: %w", inspection.ErrGroupMigrationRequired, taskID, err)
	}
	if !ready {
		if err = recoverInspectionLifetime(ctx, operation); err != nil {
			return fmt.Errorf("%w: ended Delegate inspection %s lifetime termination is unproven: %w", inspection.ErrGroupMigrationRequired, taskID, err)
		}
		ready, err = successfulInspectionLifetimeEvidence(ctx, operation)
		if err != nil {
			return fmt.Errorf("%w: ended Delegate inspection %s lifetime evidence is invalid after recovery: %w", inspection.ErrGroupMigrationRequired, taskID, err)
		}
	}
	if !ready {
		return fmt.Errorf("%w: ended Delegate inspection %s is awaiting lifetime termination", inspection.ErrGroupMigrationBusy, taskID)
	}
	return nil
}

func recordLegacyEndedInspectionExit(ctx context.Context, operation *inspection.Operation, taskID string, job pueue.Job) error {
	if err := operation.RecordWorkerExitContext(ctx, job.ID, job.Succeeded); err != nil {
		return fmt.Errorf("%w: ended Delegate inspection %s worker observation could not be retained: %w", inspection.ErrGroupMigrationBusy, taskID, err)
	}
	return nil
}

func inspectLegacyEndedInspection(ctx context.Context, store *taskdir.Store, legacy inspection.GroupRequestRecord, taskID string, job pueue.Job) (resultErr error) {
	if err := validateLegacyEndedInspectionJob(taskID, job); err != nil {
		return err
	}
	operation, err := inspection.LoadOperationContext(ctx, store, taskID)
	if err != nil {
		return fmt.Errorf("%w: ended Delegate inspection %s could not be reopened: %w", inspection.ErrGroupMigrationRequired, taskID, err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	if err := validateLegacyEndedInspectionBinding(ctx, store, legacy, operation, taskID, job); err != nil {
		return err
	}
	if err := validateLegacyEndedInspectionLifetime(ctx, operation, taskID, job); err != nil {
		return err
	}
	// Preserve this independent exit observation before cleanup or migration
	// removes the row. Admission can then finish from durable evidence even if
	// its own next queue snapshot no longer contains the worker.
	if err := recordLegacyEndedInspectionExit(ctx, operation, taskID, job); err != nil {
		return err
	}
	return validateLegacyEndedInspectionEvidence(ctx, operation, taskID, job)
}

func privateMigrationSupervisorMatches(stateRoot string, current, candidate task.SupervisorRef) (bool, error) {
	if task.SameSupervisorIdentity(current, candidate) {
		return true, nil
	}
	return pueue.PrivateSupervisorGenerationMatches(stateRoot, candidate)
}

func legacyJobTaskID(label, rootID string) (taskID string, managed, inspectionWorker bool, resultErr error) {
	ordinaryPrefix := "delegate:" + rootID + ":"
	inspectionPrefix := "delegation-inspection-" + rootID + "-"
	switch {
	case strings.HasPrefix(label, ordinaryPrefix):
		taskID = strings.TrimPrefix(label, ordinaryPrefix)
	case strings.HasPrefix(label, inspectionPrefix):
		taskID = strings.TrimPrefix(label, inspectionPrefix)
		inspectionWorker = true
	default:
		return "", false, false, nil
	}
	if err := task.ValidateTaskID(taskID); err != nil {
		return "", true, inspectionWorker, err
	}
	return taskID, true, inspectionWorker, nil
}

func admissionInspectionFacts(a Arguments, deps Dependencies, store *taskdir.Store, req task.TaskRecord, candidate commonprovider.ProfileCandidate, supervisor *pueue.Client, inspectionGroupReady bool) (facts json.RawMessage, deadline time.Time, resultErr error) {
	runner, inspectionSupervisor, binding, err := prepareAdmissionInspectionBinding(a, deps, candidate, supervisor)
	if err != nil {
		return nil, time.Time{}, err
	}
	inspectionTimeout := inspection.TimeoutForRevision(binding.DefinitionRevision)
	concurrent := concurrentAdmissionInspection(store.Root, binding)
	if !concurrent && !inspectionGroupReady {
		err = ensureAdmissionInspectionGroup(a, store, inspectionSupervisor, inspectionTimeout)
	}
	if err != nil {
		return nil, time.Time{}, annotateMissingInspectionStage("ensuring inspection group", err)
	}

	openedAt := time.Now()
	setupContext, cancelSetup := context.WithDeadline(context.Background(), openedAt.Add(inspectionTimeout))
	defer cancelSetup()
	operation, err := openAdmissionInspectionOperationContext(setupContext, store, req, binding, openedAt)
	if err != nil {
		return nil, time.Time{}, annotateMissingInspectionStage("opening inspection operation", err)
	}
	defer func() { resultErr = errors.Join(resultErr, operation.Close()) }()
	// Replays retain their saved group, including legacy runtime probes. A
	// current capability marker must never rewrite an existing request.
	if concurrent && operation.Request().Group != "default" && !inspectionGroupReady {
		err = ensureAdmissionInspectionGroup(a, store, inspectionSupervisor, inspectionTimeout)
	}
	if err != nil {
		return nil, operation.Deadline(), annotateMissingInspectionStage("ensuring saved inspection group", err)
	}
	facts, deadline, err = runAdmissionInspection(operation, inspectionSupervisor, runner, store.Root)
	if err != nil {
		err = withInspectionFailureContext(err, req.TaskID, deadline)
	}
	return facts, deadline, err
}

func prepareAdmissionInspectionBinding(a Arguments, deps Dependencies, candidate commonprovider.ProfileCandidate, supervisor *pueue.Client) (string, *pueue.Client, inspection.Binding, error) {
	runner, err := resolveRunner(a.Runner, deps)
	if err != nil {
		return "", nil, inspection.Binding{}, annotateMissingInspectionStage("resolving worker", err)
	}
	workerSHA, err := commonprovider.FingerprintExecutable(runner)
	if err != nil {
		return "", nil, inspection.Binding{}, annotateMissingInspectionStage("fingerprinting worker", err)
	}
	definition, definitionSHA, err := candidate.Inspection.Snapshot()
	if err != nil {
		return "", nil, inspection.Binding{}, annotateMissingInspectionStage("snapshotting definition", err)
	}
	capabilitySHA := ""
	if runtimeOnlyInspectionDefinition(definition) {
		capabilitySHA, err = commonprovider.RuntimeCapabilityDigest(definition)
		if err != nil {
			return "", nil, inspection.Binding{}, annotateMissingInspectionStage("snapshotting capability", err)
		}
	}
	inspectionTimeout := inspection.TimeoutForRevision(definition.Revision)
	inspectionSupervisor, err := supervisor.WithObservationTimeout(inspectionTimeout)
	if err != nil {
		return "", nil, inspection.Binding{}, annotateMissingInspectionStage("preparing inspection supervisor", err)
	}
	binding := inspection.Binding{
		DefinitionRevision: definition.Revision, DefinitionSHA256: definitionSHA, CapabilitySHA256: capabilitySHA,
		HelperExecutable: definition.Executable, HelperSHA256: definition.ExecutableSHA256,
		WorkerExecutable: runner, WorkerSHA256: workerSHA,
		RunnerOwnership: requestedRunnerOwnership(a),
		WorkerExitProof: inspection.WorkerExitProofGuardianWaitV1,
		Environment:     append([]string(nil), definition.Environment...), EnvironmentRecorded: true,
		Supervisor: supervisor.Binding(),
	}
	return runner, inspectionSupervisor, binding, nil
}

func runAdmissionInspection(operation *inspection.Operation, supervisor *pueue.Client, runner, root string) (json.RawMessage, time.Time, error) {
	deadline := operation.Deadline()
	var ctx context.Context
	var cancel context.CancelFunc
	if time.Now().Before(deadline) {
		ctx, cancel = context.WithDeadline(context.Background(), deadline)
	} else {
		// A completed operation remains replayable after its immutable deadline.
		// Give that local proof read one bounded recovery window without renewing
		// the operation or permitting new external work.
		ctx, cancel = context.WithTimeout(context.Background(), inspectionStatusWatchdog)
	}
	defer cancel()
	recordedFacts, _, terminal, recordedErr := recordedInspectionFacts(ctx, operation)
	if recordedErr != nil {
		if errors.Is(recordedErr, context.DeadlineExceeded) && operation.Expired(time.Now()) {
			return nil, deadline, inspection.ErrAdmissionExpired
		}
		return nil, deadline, recordedErr
	}
	if terminal {
		// A validated worker result and independent empty-scope termination are
		// durable admission facts. They remain replayable after a crash even
		// when ordinary task creation had not happened before the inspection
		// deadline.
		return recordedFacts, deadline, nil
	}
	if operation.Expired(time.Now()) {
		return nil, deadline, inspection.ErrAdmissionExpired
	}
	submission, err := submitInspectionOnceWithState(ctx, operation, supervisor, runner, root)
	if err != nil {
		return nil, deadline, annotateMissingInspectionStage("submitting inspection", err)
	}
	facts, err := awaitInspectionWithSubmissionState(ctx, operation, supervisor, submission)
	if err != nil {
		facts, err = awaitCompletedInspectionObservation(operation, supervisor, err)
		if err != nil {
			return nil, deadline, annotateMissingInspectionStage("awaiting inspection", err)
		}
		return facts, deadline, nil
	}
	return facts, deadline, nil
}

func ensureAdmissionInspectionGroup(a Arguments, store *taskdir.Store, supervisor *pueue.Client, timeout time.Duration) error {
	if a.savedSupervisor != nil {
		return ensureInspectionGroupWithoutMigration(store, supervisor, timeout)
	}
	return ensureInspectionGroup(store, supervisor, timeout)
}

func openAdmissionInspectionOperation(store *taskdir.Store, request task.TaskRecord, binding inspection.Binding, now time.Time) (operation *inspection.Operation, resultErr error) {
	return openAdmissionInspectionOperationContext(context.Background(), store, request, binding, now)
}

func openAdmissionInspectionOperationContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding inspection.Binding, now time.Time) (operation *inspection.Operation, resultErr error) {
	operation, err := inspection.LoadOperationContext(ctx, store, request.TaskID)
	if errors.Is(err, os.ErrNotExist) {
		return createAdmissionInspectionOperationContext(ctx, store, request, binding, now)
	}
	if err != nil {
		return nil, err
	}
	keepOperation := false
	defer func() {
		if !keepOperation {
			resultErr = errors.Join(resultErr, operation.Close())
		}
	}()

	if err = validateAdmissionInspectionReplay(ctx, store, request, binding, operation); err != nil {
		return nil, err
	}
	keepOperation = true
	return operation, nil
}

func validateAdmissionInspectionReplay(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding inspection.Binding, operation *inspection.Operation) error {
	record := operation.Request()
	requestBytes, err := task.MarshalCanonical(request)
	if err != nil {
		return task.ErrEvidenceFault
	}
	if record.RootID != store.RootID || record.TaskID != request.TaskID || record.TaskSHA256 != task.ComputeSHA256(requestBytes) {
		return task.ErrEvidenceFault
	}
	if err = rejectCompletedRuntimeIdentityDrift(ctx, operation, record.Binding, binding); err != nil {
		return err
	}
	allowMarkerlessReplay, err := allowIncompleteMarkerlessRuntimeReplay(ctx, operation, record.Binding, binding)
	if err != nil || allowMarkerlessReplay {
		// A permitted markerless replay keeps its original create-once request.
		// The worker establishes the refreshed capability contract in new facts.
		return err
	}
	if sameInspectionBinding(record.Binding, binding) {
		return nil
	}
	if !sameInspectionBindingExceptManagedWorker(record.Binding, binding) {
		return task.ErrEvidenceFault
	}
	if record.Binding.WorkerExecutable == binding.WorkerExecutable && record.Binding.WorkerSHA256 == binding.WorkerSHA256 {
		return nil
	}
	return validateManagedInspectionReplay(ctx, store.Root, operation, binding)
}

func createAdmissionInspectionOperation(store *taskdir.Store, request task.TaskRecord, binding inspection.Binding, now time.Time) (*inspection.Operation, error) {
	return createAdmissionInspectionOperationContext(context.Background(), store, request, binding, now)
}

func createAdmissionInspectionOperationContext(ctx context.Context, store *taskdir.Store, request task.TaskRecord, binding inspection.Binding, now time.Time) (*inspection.Operation, error) {
	if concurrentAdmissionInspection(store.Root, binding) {
		return inspection.OpenConcurrentOperationContext(ctx, store, request, binding, now)
	}
	return inspection.OpenOperationContext(ctx, store, request, binding, now)
}

func concurrentAdmissionInspection(root string, binding inspection.Binding) bool {
	return binding.DefinitionRevision == commonprovider.RuntimeInspectionRevision && binding.CapabilitySHA256 != "" && pueue.IsPrivateConfig(root, binding.Supervisor.ConfigPath)
}

// allowIncompleteMarkerlessRuntimeReplay lets a pre-capability-marker runtime
// journal survive a provider or probe-contract refresh while it is still
// incomplete. Its immutable worker, supervisor, and environment bindings must
// remain unchanged; only the runtime identity/definition may be re-probed.
// Completed markerless journals remain subject to the conservative identity
// drift rejection above because their proof cannot be relabeled in place.
func allowIncompleteMarkerlessRuntimeReplay(ctx context.Context, operation *inspection.Operation, saved, current inspection.Binding) (bool, error) {
	if operation == nil || !markerlessRuntimeReplayBindingMatches(saved, current) {
		return false, nil
	}
	savedEnvironmentRecorded := saved.EnvironmentRecorded || len(saved.Environment) > 0
	currentEnvironmentRecorded := current.EnvironmentRecorded || len(current.Environment) > 0
	if savedEnvironmentRecorded != currentEnvironmentRecorded || !slices.Equal(saved.Environment, current.Environment) {
		return false, nil
	}
	_, _, err := operation.ReadCompletedContext(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func markerlessRuntimeReplayBindingMatches(saved, current inspection.Binding) bool {
	return saved.DefinitionRevision == commonprovider.RuntimeInspectionRevision &&
		current.DefinitionRevision == commonprovider.RuntimeInspectionRevision &&
		saved.CapabilitySHA256 == "" && current.CapabilitySHA256 != "" &&
		(saved.RunnerOwnership == "" || saved.RunnerOwnership == current.RunnerOwnership) &&
		saved.WorkerExitProof == current.WorkerExitProof &&
		saved.WorkerExecutable == current.WorkerExecutable &&
		saved.WorkerSHA256 == current.WorkerSHA256 &&
		task.SameSupervisorIdentity(saved.Supervisor, current.Supervisor)
}

// rejectCompletedRuntimeIdentityDrift prevents a completed capability proof
// from being relabeled as proof for a different provider executable. A queued
// or incomplete operation can still run its one worker against the current
// candidate; a completed operation has no safe way to rerun that worker under
// the same create-once journal.
func rejectCompletedRuntimeIdentityDrift(ctx context.Context, operation *inspection.Operation, saved, current inspection.Binding) error {
	if operation == nil ||
		(saved.CapabilitySHA256 == "" && current.CapabilitySHA256 == "") ||
		saved.DefinitionRevision != commonprovider.RuntimeInspectionRevision ||
		current.DefinitionRevision != commonprovider.RuntimeInspectionRevision ||
		saved.DefinitionSHA256 == current.DefinitionSHA256 {
		return nil
	}
	if _, _, err := operation.ReadCompletedContext(ctx); err == nil {
		return task.ErrEvidenceFault
	} else if errors.Is(err, os.ErrNotExist) {
		return nil
	} else {
		return err
	}
}

func validateManagedInspectionReplay(ctx context.Context, root string, operation *inspection.Operation, binding inspection.Binding) error {
	if !managedInspectionWorkerExecutable(root, binding) {
		return task.ErrEvidenceFault
	}
	authorized, err := operation.ManagedWorkerUpgradeAuthorizedContext(ctx)
	if err != nil {
		return err
	}
	if authorized {
		return nil
	}
	// A retry may reopen a completed operation after its queued worker is gone.
	// The later supervisor reconciliation still verifies the matching
	// successful job before these facts can admit the request.
	result, _, err := operation.ReadCompletedContext(ctx)
	if err != nil || result.Reason != inspection.ResultEligible {
		return task.ErrEvidenceFault
	}
	return nil
}

func sameInspectionBinding(left, right inspection.Binding) bool {
	return sameInspectionBindingBase(left, right) &&
		(left.RunnerOwnership == right.RunnerOwnership || left.RunnerOwnership == "") &&
		left.WorkerExitProof == right.WorkerExitProof &&
		left.WorkerExecutable == right.WorkerExecutable &&
		left.WorkerSHA256 == right.WorkerSHA256
}

func sameInspectionBindingBase(left, right inspection.Binding) bool {
	return sameInspectionEnvironment(left, right) &&
		task.SameSupervisorIdentity(left.Supervisor, right.Supervisor) &&
		sameInspectionDefinition(left, right)
}

func sameInspectionEnvironment(left, right inspection.Binding) bool {
	leftEnvironmentRecorded := left.EnvironmentRecorded || len(left.Environment) > 0
	rightEnvironmentRecorded := right.EnvironmentRecorded || len(right.Environment) > 0
	return leftEnvironmentRecorded == rightEnvironmentRecorded && slices.Equal(left.Environment, right.Environment)
}

func sameInspectionDefinition(left, right inspection.Binding) bool {
	if left.DefinitionRevision == commonprovider.RuntimeInspectionRevision && right.DefinitionRevision == commonprovider.RuntimeInspectionRevision {
		return sameRuntimeInspectionDefinition(left, right)
	}
	return sameExactInspectionDefinition(left, right)
}

func sameRuntimeInspectionDefinition(left, right inspection.Binding) bool {
	// Only bindings carrying the runtime-only capability contract may relax
	// executable identity. A mixed native/runtime binding without that marker
	// remains exact so native command and helper changes cannot be silently
	// reused.
	if left.CapabilitySHA256 != "" && right.CapabilitySHA256 != "" {
		return left.CapabilitySHA256 == right.CapabilitySHA256
	}
	return sameExactInspectionDefinition(left, right)
}

func sameExactInspectionDefinition(left, right inspection.Binding) bool {
	return left.DefinitionRevision == right.DefinitionRevision &&
		left.DefinitionSHA256 == right.DefinitionSHA256 &&
		left.HelperExecutable == right.HelperExecutable &&
		left.HelperSHA256 == right.HelperSHA256
}

func sameInspectionBindingExceptManagedWorker(saved, current inspection.Binding) bool {
	return saved.RunnerOwnership == task.RunnerOwnershipManaged &&
		current.RunnerOwnership == task.RunnerOwnershipManaged &&
		saved.WorkerExitProof == current.WorkerExitProof &&
		sameInspectionBindingBase(saved, current)
}

// annotateMissingInspectionStage gives a missing-file failure a fixed stage
// without exposing paths, records, or native diagnostics. The original error
// remains wrapped so callers can still classify errors.Is(os.ErrNotExist).
func annotateMissingInspectionStage(stage string, err error) error {
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fmt.Errorf("inspection %s: %w", stage, err)
}

type inspectionSubmissionState struct {
	terminalEventRegistrationKnown bool
	terminalEventRegistered        bool
}

func submitInspectionOnce(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, runner, root string) error {
	_, err := submitInspectionOnceWithState(ctx, operation, supervisor, runner, root)
	return err
}

func submitInspectionOnceWithState(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, runner, root string) (inspectionSubmissionState, error) {
	permit, err := operation.ClaimSubmissionContext(ctx, time.Now())
	if errors.Is(err, task.ErrAlreadySubmitted) {
		return inspectionSubmissionState{}, nil
	}
	if err != nil {
		return inspectionSubmissionState{}, err
	}
	record := operation.Request()
	// The worker executable is part of the immutable inspection binding. It was
	// fingerprinted before the durable claim, so revalidate the exact path and
	// bytes immediately before handing the launch to the supervisor. A changed
	// worker consumes the create-once claim but can never reach an external add.
	if err = validateInspectionRunner(operation, runner); err != nil {
		return inspectionSubmissionState{}, err
	}
	launch := guardedInspectionLaunch(runner, root)
	identity := record.SupervisorIdentity()
	var observation pueue.InspectionObservation
	var submitErr error
	commit := func(observation pueue.InspectionObservation) error {
		return operation.RecordReceiptContext(ctx, observation.Job.ID)
	}
	if concurrentAdmissionInspection(root, record.Binding) && record.Group == "default" {
		observation, submitErr = supervisor.SubmitConcurrentInspectionStagedWithGate(ctx, permit, identity, launch, privateSubmissionGate(root), commit)
	} else {
		observation, submitErr = supervisor.SubmitInspectionStagedWithGate(ctx, permit, identity, launch, privateSubmissionGate(root), commit)
	}
	if pending := inFlightInspectionError(submitErr); pending != nil {
		// SubmitInspection may have timed out in its verification, add, or
		// reconciliation command. Returning its pending owner keeps the
		// supervisor lease until completion without extending admission.
		return inspectionSubmissionState{}, submitErr
	}
	submission := inspectionSubmissionState{
		terminalEventRegistrationKnown: observation.TerminalEventRegistrationKnown,
		terminalEventRegistered:        observation.TerminalEventRegistered,
	}
	if observation.Pending != nil {
		if inFlightInspectionError(submitErr) == nil {
			submitErr = errors.Join(submitErr, &pueue.InFlightError{Pending: observation.Pending})
		}
		return submission, submitErr
	}
	if observation.Matched {
		return submission, nil
	}
	if ctx.Err() != nil {
		return submission, errors.Join(submitErr, ctx.Err())
	}
	// The durable guard is consumed even if add completion is unknown. Only
	// observational reconciliation follows; no failure path resubmits.
	return submission, nil
}

func guardedInspectionLaunch(runner, root string) pueue.Launch {
	return pueue.Launch{
		RunnerExecutable: runner,
		RootPath:         root,
		// Every inspection worker needs the guardian's independent empty-scope
		// proof. The immutable runner ownership still controls whether the child
		// may consume the saved add receipt; custom runners reconcile the exact
		// queue row instead.
		Supervised: true,
	}
}

func validateInspectionRunner(operation *inspection.Operation, runner string) error {
	if operation == nil || runner == "" {
		return task.ErrEvidenceFault
	}
	binding := operation.Request().Binding
	if runner != binding.WorkerExecutable {
		return task.ErrIdentityMismatch
	}
	digest, err := commonprovider.FingerprintExecutable(runner)
	if err != nil || digest != binding.WorkerSHA256 {
		return task.ErrEvidenceFault
	}
	return nil
}

func inspectionIdentity(operation *inspection.Operation) (pueue.InspectionIdentity, error) {
	return inspectionIdentityContext(context.Background(), operation)
}

func inspectionIdentityContext(ctx context.Context, operation *inspection.Operation) (pueue.InspectionIdentity, error) {
	record := operation.Request()
	identity := record.SupervisorIdentity()
	receipt, err := operation.ReceiptContext(ctx)
	if errors.Is(err, os.ErrNotExist) {
		return identity, nil
	}
	if err != nil {
		return identity, err
	}
	identity.NumericTaskID = receipt.NumericTaskID
	return identity, nil
}

type inspectionAwaitState struct {
	identity                       pueue.InspectionIdentity
	polls                          inspectionPollSchedule
	lastStatusErr                  error
	terminalEvents                 bool
	terminalEventSeen              bool
	terminalEventRegistrationKnown bool
	terminalEventRegistered        bool
}

func awaitInspection(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client) (facts json.RawMessage, resultErr error) {
	return awaitInspectionWithSubmissionState(ctx, operation, supervisor, inspectionSubmissionState{})
}

func awaitInspectionWithSubmissionState(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, submission inspectionSubmissionState) (facts json.RawMessage, resultErr error) {
	defer func() {
		facts, resultErr = finalizeInspectionAwait(ctx, operation, facts, resultErr)
	}()
	state, err := newInspectionAwaitStateWithRetry(ctx, operation, supervisor, submission)
	if err != nil {
		return nil, err
	}
	return observeInspectionUntilTerminal(ctx, operation, supervisor, &state)
}

func finalizeInspectionAwait(ctx context.Context, operation *inspection.Operation, facts json.RawMessage, resultErr error) (json.RawMessage, error) {
	resultErr = normalizeInspectionAwaitError(ctx, operation, resultErr)
	if resultErr == nil {
		return facts, nil
	}
	// Durable inspection proof is the admission authority. An observer can
	// fail after the guardian has already published valid result, lifetime,
	// and Wait evidence, so every error path gets one final local decision
	// before it can reject the dispatch.
	recordedFacts, _, terminal, recordedErr := recordedInspectionFactsAfterObservation(ctx, operation)
	if terminal || recordedErr != nil {
		return recordedFacts, recordedErr
	}
	return facts, resultErr
}

func newInspectionAwaitStateWithRetry(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, submission inspectionSubmissionState) (inspectionAwaitState, error) {
	for {
		state, err := newInspectionAwaitState(ctx, operation, supervisor, submission)
		if err == nil {
			return state, nil
		}
		if !retryInspectionJournalContention(ctx, operation, err) {
			return inspectionAwaitState{}, err
		}
		if err = waitInspectionObservation(ctx); err != nil {
			return inspectionAwaitState{}, err
		}
	}
}

func observeInspectionUntilTerminal(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, state *inspectionAwaitState) (json.RawMessage, error) {
	for {
		facts, ended, observeErr := state.observe(ctx, operation, supervisor)
		if ended || observeErr != nil {
			if retryInspectionJournalContention(ctx, operation, observeErr) {
				if err := waitInspectionObservation(ctx); err != nil {
					return nil, err
				}
				continue
			}
			return facts, observeErr
		}
		if err := waitInspectionObservation(ctx); err != nil {
			return nil, err
		}
	}
}

func retryInspectionJournalContention(ctx context.Context, operation *inspection.Operation, err error) bool {
	return errors.Is(err, task.ErrLockBusy) && operation != nil &&
		!inspectionDeadlineReached(ctx, operation)
}

func (state *inspectionAwaitState) observe(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client) (json.RawMessage, bool, error) {
	if inspectionDeadlineReached(ctx, operation) {
		if facts, _, terminal, err := recordedInspectionFactsAfterObservation(ctx, operation); terminal || err != nil {
			return facts, true, err
		}
		return nil, true, errors.Join(inspection.ErrAdmissionExpired, state.lastStatusErr)
	}
	if facts, recorded, terminal, err := recordedInspectionFacts(ctx, operation); terminal || err != nil {
		return facts, true, err
	} else if recorded {
		return nil, false, nil
	}
	completed, err := inspectionCompletionHint(ctx, operation)
	if err != nil {
		return nil, true, err
	}
	facts, ended, handled, err := state.observeTerminalEvent(ctx, operation, supervisor, completed)
	if state.rememberRetryableStatus(err) {
		return nil, false, nil
	}
	if ended || err != nil || handled {
		return facts, ended, err
	}
	now := time.Now()
	if !state.polls.due(now, completed) {
		return nil, false, nil
	}
	facts, ended, err = pollInspectionStatus(ctx, operation, supervisor, &state.identity)
	if state.rememberRetryableStatus(err) {
		return nil, false, nil
	}
	return facts, ended, err
}

func (state *inspectionAwaitState) rememberRetryableStatus(err error) bool {
	retryErr := inspectionStatusRetryError(err)
	if retryErr == nil {
		return false
	}
	state.lastStatusErr = retryErr
	return true
}

func newInspectionAwaitState(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, submission inspectionSubmissionState) (inspectionAwaitState, error) {
	identity, err := inspectionIdentityContext(ctx, operation)
	if err != nil {
		return inspectionAwaitState{}, err
	}
	state := inspectionAwaitState{
		identity:                       identity,
		terminalEventRegistrationKnown: submission.terminalEventRegistrationKnown,
		terminalEventRegistered:        submission.terminalEventRegistered,
	}
	if supervisor == nil || identity.NumericTaskID == nil {
		return state, nil
	}
	state.terminalEvents, err = supervisor.PrivateTerminalEventsEnabled(ctx)
	state.polls.callbackExpected = state.terminalEvents && state.terminalEventRegistrationKnown && state.terminalEventRegistered
	return state, err
}

func (state *inspectionAwaitState) observeTerminalEvent(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, completed bool) (json.RawMessage, bool, bool, error) {
	if !state.terminalEvents || state.identity.NumericTaskID == nil {
		return nil, false, false, nil
	}
	if state.terminalEventRegistrationKnown && !state.terminalEventRegistered {
		return nil, false, false, nil
	}
	if state.terminalEventSeen {
		return nil, false, false, nil
	}
	facts, observed, handled, err := acceptInspectionTerminalEventState(ctx, operation, supervisor, *state.identity.NumericTaskID)
	if inspectionStatusRetryError(err) != nil {
		return nil, false, false, err
	}
	if handled {
		state.terminalEventSeen = true
		state.polls.observed(time.Now(), completed)
	}
	if observed || err != nil {
		return facts, true, false, err
	}
	return nil, false, handled, nil
}

func acceptInspectionTerminalEvent(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, numericTaskID int64) (json.RawMessage, bool, error) {
	facts, terminal, _, err := acceptInspectionTerminalEventState(ctx, operation, supervisor, numericTaskID)
	return facts, terminal, err
}

func acceptInspectionTerminalEventState(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, numericTaskID int64) (json.RawMessage, bool, bool, error) {
	if operation == nil || supervisor == nil || numericTaskID < 0 {
		return nil, false, false, task.ErrEvidenceFault
	}
	event, found, err := supervisor.ReadPrivateTerminalEvent(ctx, numericTaskID)
	if err != nil || !found {
		return nil, false, false, err
	}
	if event.NumericTaskID != numericTaskID {
		return nil, false, true, task.ErrEvidenceFault
	}
	// The callback is only a wake-up hint. Reconcile the exact immutable label
	// so a delayed callback cannot authorize a reused numeric ID and admission
	// never depends on a root-wide queue snapshot.
	job, current, reconcileErr := currentInspectionTerminalJob(ctx, operation, supervisor, numericTaskID)
	if reconcileErr != nil || !current {
		if facts, _, terminal, decisionErr := recordedInspectionFactsAfterObservation(ctx, operation); terminal || decisionErr != nil {
			return facts, terminal, true, decisionErr
		}
		return nil, false, true, reconcileErr
	}
	identity := operation.Request().SupervisorIdentity()
	identity.NumericTaskID = &numericTaskID
	observation := pueue.InspectionObservation{
		Binding:  supervisor.Binding(),
		Identity: identity,
		Job:      job,
		Matched:  true,
	}
	var facts json.RawMessage
	var terminal bool
	acceptErr := supervisor.CommitInspectionObservation(ctx, observation, func(observation pueue.InspectionObservation) error {
		var err error
		facts, terminal, err = acceptCompletedInspectionObservation(ctx, operation, supervisor, observation)
		return err
	})
	return facts, terminal, true, acceptErr
}

func currentInspectionTerminalJob(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, numericTaskID int64) (pueue.Job, bool, error) {
	identity := operation.Request().SupervisorIdentity()
	identity.NumericTaskID = &numericTaskID
	observation, reconcileErr := supervisor.ReconcileInspection(ctx, identity)
	retry, classifiedErr := classifyInspectionStatusObservation(observation, reconcileErr)
	if retry {
		return pueue.Job{}, false, classifiedInspectionStatusError(true, classifiedErr)
	}
	if classifiedErr != nil {
		return pueue.Job{}, false, classifiedErr
	}
	if !observation.Matched || observation.Job.ID != numericTaskID || observation.Job.State != pueue.StateEnded {
		return pueue.Job{}, false, nil
	}
	return observation.Job, true, nil
}

func normalizeInspectionAwaitError(ctx context.Context, operation *inspection.Operation, err error) error {
	if err == nil || errors.Is(err, inspection.ErrAdmissionExpired) || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) && !operation.Expired(time.Now()) {
		return err
	}
	return errors.Join(inspection.ErrAdmissionExpired, err)
}

func acceptInspectionObservation(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, identity *pueue.InspectionIdentity, job pueue.Job) (json.RawMessage, bool, error) {
	if err := requireInspectionReceipt(ctx, operation, job.ID); err != nil {
		return nil, false, err
	}
	id := job.ID
	identity.NumericTaskID = &id
	if facts, _, terminal, err := recordedInspectionFacts(ctx, operation); terminal || err != nil {
		return facts, terminal, err
	}
	if job.State != pueue.StateEnded {
		return nil, false, nil
	}
	if operation.Request().Binding.WorkerExitProof == inspection.WorkerExitProofGuardianWaitV1 {
		// Pueue reports the guardian process, not the exact inspection child.
		// Managed exit authority is published only by the guardian after Wait.
		return nil, false, nil
	}
	if !job.Succeeded {
		if err := operation.RecordWorkerExitContext(ctx, job.ID, false); errors.Is(err, os.ErrNotExist) {
			return nil, true, errInspectionWorkerUnavailable
		} else if err != nil {
			return nil, true, err
		}
		return nil, true, errInspectionWorkerUnavailable
	}
	recordErr := operation.RecordWorkerExitContext(ctx, job.ID, job.Succeeded)
	if errors.Is(recordErr, os.ErrNotExist) {
		// Pueue can report the row just before the worker's final control record
		// becomes observable. Preserve the row and retry the same observation.
		return nil, false, nil
	} else if recordErr != nil {
		return nil, true, recordErr
	}
	facts, _, terminal, err := recordedInspectionFacts(ctx, operation)
	return facts, terminal, err
}

// requireInspectionReceipt keeps the staged submission commit as the sole
// creator of inspection admission authority. Later queue observations only
// validate the exact numeric task they were already authorized to observe.
func requireInspectionReceipt(ctx context.Context, operation *inspection.Operation, numericTaskID int64) error {
	if operation == nil || numericTaskID < 0 {
		return task.ErrEvidenceFault
	}
	receipt, err := operation.ReceiptContext(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.Join(task.ErrEvidenceFault, err)
		}
		return err
	}
	if receipt.NumericTaskID == nil || *receipt.NumericTaskID != numericTaskID {
		return task.ErrIdentityMismatch
	}
	return nil
}

func reconcileInspectionObservation(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, identity pueue.InspectionIdentity, commit pueue.InspectionObservationCommit) (pueue.InspectionObservation, error) {
	if inspectionDeadlineReached(ctx, operation) {
		return pueue.InspectionObservation{}, inspection.ErrAdmissionExpired
	}
	committed := commit
	if commit != nil {
		committed = func(observation pueue.InspectionObservation) error {
			if observation.Job.State == pueue.StateStashed {
				return nil
			}
			return commit(observation)
		}
	}
	observation, observeErr := supervisor.ReconcileInspectionWithLease(ctx, identity, committed)
	if inspectionDeadlineReached(ctx, operation) {
		// The supervisor observation may reach the deadline with either an
		// in-flight status command or a terminal failed worker. The persisted
		// admission deadline is authoritative in both cases. Preserve a pending
		// command in the error so its supervisor lease outlives this caller.
		return observation, errors.Join(inspection.ErrAdmissionExpired, observeErr)
	}
	if observeErr == nil && observation.Matched && observation.Job.State == pueue.StateStashed {
		return recoverStagedInspectionObservation(ctx, operation, supervisor, observation, commit)
	}
	return observation, observeErr
}

func recoverStagedInspectionObservation(ctx context.Context, operation *inspection.Operation, supervisor *pueue.Client, observation pueue.InspectionObservation, commit pueue.InspectionObservationCommit) (pueue.InspectionObservation, error) {
	receiptCommit := func(current pueue.InspectionObservation) error {
		return operation.RecordReceiptContext(ctx, current.Job.ID)
	}
	root := filepath.Dir(filepath.Dir(observation.Binding.ConfigPath))
	recovered, recoverErr := supervisor.RecoverInspectionStagedWithGate(ctx, observation.Identity, observation.Job.ID, privateSubmissionGate(root), receiptCommit)
	if recoverErr == nil || !errors.Is(recoverErr, pueue.ErrUnknown) {
		return recovered, recoverErr
	}
	receipt, receiptErr := operation.ReceiptContext(ctx)
	if receiptErr != nil || receipt.NumericTaskID == nil || *receipt.NumericTaskID != observation.Job.ID {
		return recovered, errors.Join(recoverErr, receiptErr)
	}
	// Another observer may have enqueued the exact receipted row between
	// the first Stashed observation and recovery. Reobserve under the
	// supervisor lease and run the original acceptance callback there so a
	// Running or Ended race cannot lose its application-side evidence.
	current, currentErr := supervisor.ReconcileInspectionWithLease(ctx, observation.Identity, commit)
	if currentErr != nil || !current.Matched || current.Job.ID != observation.Job.ID {
		return current, errors.Join(recoverErr, currentErr)
	}
	switch current.Job.State {
	case pueue.StateQueued, pueue.StateRunning, pueue.StateEnded:
		return current, nil
	case pueue.StateUnknown, pueue.StateStashed:
		return current, recoverErr
	}
	return current, recoverErr
}

func inspectionDeadlineReached(ctx context.Context, operation *inspection.Operation) bool {
	return ctx.Err() != nil || operation.Expired(time.Now())
}

func shouldStopInspectionPolling(err error) bool {
	return errors.Is(err, inspection.ErrAdmissionExpired) || inFlightInspectionError(err) != nil
}

func inFlightInspectionError(err error) *pueue.Pending {
	var inFlight *pueue.InFlightError
	if !errors.As(err, &inFlight) || inFlight == nil {
		return nil
	}
	return inFlight.Pending
}

func completedInspectionFacts(ctx context.Context, operation *inspection.Operation, job pueue.Job) (json.RawMessage, error) {
	if operation == nil {
		if !job.Succeeded {
			return nil, errInspectionWorkerUnavailable
		}
		return nil, task.ErrEvidenceFault
	}
	if !job.Succeeded {
		return nil, errInspectionWorkerUnavailable
	}
	if err := operation.RecordWorkerExitContext(ctx, job.ID, true); err != nil {
		return nil, err
	}
	facts, _, terminal, err := recordedInspectionFacts(ctx, operation)
	if err != nil || terminal {
		return facts, err
	}
	return nil, os.ErrNotExist
}

func waitInspectionObservation(ctx context.Context) error {
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return inspection.ErrAdmissionExpired
	case <-timer.C:
		return nil
	}
}

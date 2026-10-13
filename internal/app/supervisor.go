package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/inspection"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

const supervisorMigrationTimeout = 30 * time.Second

// privateQueueAuthorizationWorkers bounds only the filesystem work used to
// publish queue observations. It does not limit admitted or running tasks.
const privateQueueAuthorizationWorkers = 32

var errSupervisorCleanup = errors.New("supervisor migration client cleanup failed")

// supervisorClientOwner is the application boundary for every acquired Pueue
// client. Bind may return both a client and an error when a version probe is
// still in flight, so ownership is installed before callers inspect the error.
type supervisorClientOwner struct {
	client *pueue.Client
}

func acquireSupervisorClient(client *pueue.Client, err error) (*supervisorClientOwner, error) {
	return &supervisorClientOwner{client: client}, err
}

func (owner *supervisorClientOwner) Client() *pueue.Client {
	if owner == nil {
		return nil
	}
	return owner.client
}

func (owner *supervisorClientOwner) Release() *pueue.Client {
	if owner == nil {
		return nil
	}
	client := owner.client
	owner.client = nil
	return client
}

func (owner *supervisorClientOwner) CloseAfterPending(cause error) error {
	client := owner.Release()
	if client == nil {
		return nil
	}
	return client.CloseAfterPending(cause)
}

func closeSupervisorOwnerMaintenance(owner *supervisorClientOwner, cause error) {
	if owner == nil {
		return
	}
	closeSupervisorMaintenance(owner.Release(), cause)
}

// closeSupervisorMaintenance releases a recovered supervisor client without
// changing the command's authority result. A temporary migration config may
// still be owned by an in-flight pueue command; CloseAfterPending records that
// cleanup boundary and the next lifecycle pass can finish it. Cleanup failure
// is maintenance telemetry, never evidence that changes a successful task.
func closeSupervisorMaintenance(client *pueue.Client, cause error) {
	if client == nil {
		return
	}
	if err := client.CloseAfterPending(cause); err != nil {
		log.Printf("delegate: supervisor cleanup deferred: %v", err)
	}
}

func privateSuccessfulCleanupCandidates(rootID string, jobs []pueue.Job) []pueue.Job {
	labelCounts := privateQueueLabelCounts(jobs)
	candidates := make([]pueue.Job, 0, len(jobs))
	for _, job := range jobs {
		if job.State != pueue.StateEnded || job.Label == nil || labelCounts[*job.Label] != 1 {
			continue
		}
		if _, managed, _, labelErr := legacyJobTaskID(*job.Label, rootID); labelErr != nil || !managed {
			continue
		}
		candidates = append(candidates, job)
	}
	return candidates
}

func authorizePrivateSuccessfulCleanupCandidates(ctx context.Context, store *taskdir.Store, group inspection.GroupRequestRecord, candidates []pueue.Job) (ids []int64, resultErr error) {
	authorized := make([]bool, len(candidates))
	work := make(chan int, len(candidates))
	for index := range candidates {
		work <- index
	}
	close(work)
	workers := min(len(candidates), privateQueueAuthorizationWorkers)
	var groupWorkers sync.WaitGroup
	groupWorkers.Add(workers)
	for range workers {
		go func() {
			defer groupWorkers.Done()
			for index := range work {
				if ctx.Err() != nil {
					return
				}
				authorized[index] = inspectLegacyEndedJob(ctx, store, group, candidates[index]) == nil
			}
		}()
	}
	groupWorkers.Wait()
	for index, job := range candidates {
		if authorized[index] {
			ids = append(ids, job.ID)
		}
	}
	return ids, ctx.Err()
}

// authorizePrivateSuccessfulCleanup selects exact terminal rows using the same
// evidence validator as migration. Active or incomplete records are preserved
// individually; they cannot veto cleanup of an unrelated validated result.
func authorizePrivateSuccessfulCleanup(ctx context.Context, stateRoot string, supervisor task.SupervisorRef, jobs []pueue.Job) (ids []int64, resultErr error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, stateRoot, predicate.Default())
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	group := privateInspectionGroup(store.RootID, supervisor)
	candidates := privateSuccessfulCleanupCandidates(store.RootID, jobs)
	ids, err = authorizePrivateSuccessfulCleanupCandidates(ctx, store, group, candidates)
	return ids, err
}

func privateInspectionGroup(rootID string, supervisor task.SupervisorRef) inspection.GroupRequestRecord {
	return inspection.GroupRequestRecord{
		SchemaVersion: task.SchemaVersion,
		RootID:        rootID,
		Supervisor:    supervisor,
		Name:          (pueue.InspectionIdentity{RootID: rootID}).Group(), ParallelTasks: 1,
	}
}

func privateQueueLabelCounts(jobs []pueue.Job) map[string]int {
	counts := make(map[string]int)
	for _, job := range jobs {
		if job.Label != nil {
			counts[*job.Label]++
		}
	}
	return counts
}

type privateTaskEvidence struct {
	submission *task.SubmitRecord
	receipt    *task.SupervisorReceipt
	inspection *taskdir.TaskInspection
}

func readPrivateTaskEvidence(ctx context.Context, store *taskdir.Store, taskID string) (evidence privateTaskEvidence, resultErr error) {
	if ctx == nil {
		return privateTaskEvidence{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return privateTaskEvidence{}, err
	}
	td, err := store.OpenTask(taskID)
	if err != nil {
		return privateTaskEvidence{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	evidence.submission, err = td.ReadSubmission()
	if err != nil {
		return privateTaskEvidence{}, err
	}
	evidence.receipt, err = td.ReadSupervisorReceipt()
	if err != nil {
		return privateTaskEvidence{}, err
	}
	evidence.inspection, err = td.Inspect()
	if err != nil {
		return privateTaskEvidence{}, err
	}
	if err = ctx.Err(); err != nil {
		return privateTaskEvidence{}, err
	}
	return evidence, nil
}

// authorizePrivateMigrationQueue is the application-owned migration policy.
// Pueue supplies the snapshot and performs only the already-authorized queue
// transition; all Delegate labels and terminal evidence stay here.
func authorizePrivateMigrationQueue(ctx context.Context, stateRoot string, legacy task.SupervisorRef, jobs []pueue.Job) (resultErr error) {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var store *taskdir.Store
	defer func() {
		if store != nil {
			resultErr = errors.Join(resultErr, store.Close())
		}
	}()
	if err := rejectAmbiguousPrivateMigrationLabels(ctx, stateRoot, &store, privateQueueLabelCounts(jobs)); err != nil {
		return err
	}
	for _, job := range jobs {
		if err := authorizePrivateMigrationJob(ctx, stateRoot, legacy, &store, job); err != nil {
			return err
		}
	}
	return nil
}

func rejectAmbiguousPrivateMigrationLabels(ctx context.Context, stateRoot string, store **taskdir.Store, counts map[string]int) error {
	for label, count := range counts {
		if count <= 1 || (!strings.HasPrefix(label, "delegate:") && !strings.HasPrefix(label, "delegation-inspection-")) {
			continue
		}
		if err := openPrivateMigrationStore(ctx, stateRoot, store); err != nil {
			return err
		}
		_, managed, _, labelErr := legacyJobTaskID(label, (*store).RootID)
		if labelErr != nil || managed {
			return fmt.Errorf("%w: duplicate Delegate supervisor label", pueue.ErrPrivateSupervisorMigrationRequired)
		}
	}
	return nil
}

func authorizePrivateMigrationJob(ctx context.Context, stateRoot string, legacy task.SupervisorRef, store **taskdir.Store, job pueue.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if privateMigrationJobActive(job) {
		return pueue.ErrPrivateSupervisorMigrationBusy
	}
	if job.State != pueue.StateEnded {
		return nil
	}
	return authorizeEndedPrivateMigrationJob(ctx, stateRoot, legacy, store, job)
}

func privateMigrationJobActive(job pueue.Job) bool {
	switch job.State {
	case pueue.StateQueued, pueue.StateStashed, pueue.StateRunning, pueue.StateUnknown:
		return true
	case pueue.StateEnded:
		return false
	default:
		return false
	}
}

func authorizeEndedPrivateMigrationJob(ctx context.Context, stateRoot string, legacy task.SupervisorRef, store **taskdir.Store, job pueue.Job) error {
	if job.Label == nil || (!strings.HasPrefix(*job.Label, "delegate:") && !strings.HasPrefix(*job.Label, "delegation-inspection-")) {
		return nil
	}
	if err := openPrivateMigrationStore(ctx, stateRoot, store); err != nil {
		return err
	}
	if err := inspectLegacyEndedJob(ctx, *store, privateInspectionGroup((*store).RootID, legacy), job); err != nil {
		return fmt.Errorf("%w: %w", pueue.ErrPrivateSupervisorMigrationRequired, err)
	}
	return nil
}

func openPrivateMigrationStore(ctx context.Context, stateRoot string, store **taskdir.Store) error {
	if *store != nil {
		return nil
	}
	var err error
	*store, err = taskdir.OpenStoreWithPredicatesContext(ctx, stateRoot, predicate.Default())
	if err != nil {
		return fmt.Errorf("%w: inspect ended Delegate task state: %w", pueue.ErrPrivateSupervisorMigrationRequired, err)
	}
	return nil
}

func supervisorCommand(a Arguments, deps Dependencies) (result commandResult) {
	response := newResponse("supervisor")
	response.SupervisorControl = &SupervisorControlResponse{
		Action:             a.SupervisorAction,
		State:              "unknown",
		LegacyReachability: "unknown",
		ActiveWork:         "unknown",
		NextAction:         "stop_and_report",
	}
	root, err := resolveRoot(a.Root)
	if err != nil {
		return failedSupervisor(response, "supervisor state root is unavailable", 1)
	}
	if a.SupervisorAction == "status" {
		return supervisorStatus(root, deps, response)
	}
	return supervisorMigrate(root, deps, response)
}

func supervisorStatus(root string, deps Dependencies, response Response) (result commandResult) {
	options := supervisorControlOptions(deps)
	controlContext, cancel := supervisorControlContext(options)
	defer cancel()
	store, err := openStoreContext(controlContext, root, deps.storeDependencies(), false)
	if errors.Is(err, os.ErrNotExist) {
		response.SupervisorControl.State = "not_initialized"
		response.SupervisorControl.LegacyReachability = "not_present"
		response.SupervisorControl.ActiveWork = "none"
		response.SupervisorControl.NextAction = "none"
		return commandResult{response: response, code: 0}
	}
	if err != nil {
		return failedSupervisor(response, "supervisor state root is unavailable", 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	response.RootID = store.RootID
	return supervisorStatusFromStore(controlContext, root, deps, options, response, store)
}

func supervisorStatusFromStore(ctx context.Context, root string, deps Dependencies, options pueue.Options, response Response, store *taskdir.Store) commandResult {
	controlContext := ctx
	seed, err := inspection.ReadGroupRequest(controlContext, store)
	if errors.Is(err, os.ErrNotExist) {
		return supervisorStatusWithoutInspectionGroup(controlContext, root, deps, options, response)
	}
	if err != nil {
		return failedSupervisor(response, "supervisor migration journal could not be read", 1)
	}
	if !pueue.IsPrivateConfig(root, seed.Supervisor.ConfigPath) {
		response.SupervisorControl.State = "external"
		response.SupervisorControl.LegacyReachability = "not_managed"
		response.SupervisorControl.ActiveWork = "unknown"
		response.SupervisorControl.NextAction = "stop_and_report"
		return commandResult{response: response, code: 1, err: errors.New("inspection group uses an external supervisor")}
	}
	resolution, err := resolvePrivateSupervisorGeneration(controlContext, store, seed.Supervisor)
	if err != nil {
		return failedSupervisor(response, "supervisor migration journal could not be resolved", 1)
	}
	active := resolution.GroupHead
	if !resolution.GroupPresent || active.Supervisor != resolution.Active {
		active = privateInspectionGroup(store.RootID, resolution.Active)
	}
	migrated := resolution.GroupMigrated || (resolution.PrivateAuthority && seed.Supervisor != resolution.Active)
	return supervisorLegacyStatus(controlContext, root, deps, options, response, active, migrated)
}

func supervisorStatusWithoutInspectionGroup(ctx context.Context, root string, deps Dependencies, options pueue.Options, response Response) (result commandResult) {
	if _, err := os.Stat(pueue.PrivateConfigPath(root)); errors.Is(err, os.ErrNotExist) {
		response.SupervisorControl.State = "not_initialized"
		response.SupervisorControl.LegacyReachability = "not_present"
		response.SupervisorControl.ActiveWork = "none"
		response.SupervisorControl.NextAction = "none"
		return commandResult{response: response, code: 0}
	} else if err != nil {
		return failedSupervisor(response, "private supervisor state could not be inspected", 1)
	}
	currentOwner, currentErr := acquireSupervisorClient(currentPrivateSupervisor(ctx, root, deps, options))
	cleanupCause := currentErr
	defer func() { closeSupervisorOwnerMaintenance(currentOwner, cleanupCause) }()
	if currentErr != nil {
		response.SupervisorControl.State = supervisorStatusState(true, false)
		response.SupervisorControl.LegacyReachability = "incompatible"
		response.SupervisorControl.ActiveWork = "unknown"
		response.SupervisorControl.NextAction = "run_supervisor_migrate"
		return commandResult{response: response, code: 1, err: errors.New("private supervisor could not be verified")}
	}
	current := currentOwner.Client()
	logicalDrift, err := pueue.PrivateSupervisorLogicalDrift(root, current.Binding())
	if err != nil {
		return failedSupervisor(response, "private supervisor identity could not be inspected", 1)
	}
	daemonDrift, err := privateSupervisorDaemonDrift(root, deps)
	if err != nil {
		return failedSupervisor(response, "private supervisor identity could not be inspected", 1)
	}
	drift := logicalDrift || daemonDrift
	ready, readyErr := current.Reachable(ctx)
	if readyErr != nil {
		cleanupCause = readyErr
		response.SupervisorControl.State = supervisorStatusState(drift, false)
		response.SupervisorControl.LegacyReachability = "incompatible"
		response.SupervisorControl.ActiveWork = "unknown"
		response.SupervisorControl.NextAction = "run_supervisor_migrate"
		return commandResult{response: response, code: 1, err: errors.New("private supervisor could not be verified")}
	}
	if !ready {
		response.SupervisorControl.State = supervisorStatusState(drift, false)
		response.SupervisorControl.LegacyReachability = "unreachable"
		response.SupervisorControl.ActiveWork = "unknown"
		response.SupervisorControl.NextAction = "run_supervisor_migrate"
		return commandResult{response: response, code: 1, err: errors.New("private supervisor is unreachable")}
	}
	response.SupervisorControl.State = supervisorStatusState(logicalDrift, true)
	response.SupervisorControl.LegacyReachability = "ready"
	snapshot, snapshotErr := current.Snapshot(ctx)
	if snapshotErr != nil {
		cleanupCause = snapshotErr
		return failedSupervisor(response, "private supervisor status could not be read", 1)
	}
	response.SupervisorControl.ActiveWork = legacyActiveWork(snapshot)
	if response.SupervisorControl.ActiveWork == "present" && logicalDrift {
		response.SupervisorControl.NextAction = "wait_for_legacy_work"
	} else if logicalDrift {
		response.SupervisorControl.NextAction = "run_supervisor_migrate"
	} else {
		response.SupervisorControl.NextAction = "none"
	}
	if daemonDrift && !logicalDrift {
		response.SupervisorControl.MigrationPhase = "adopted"
	}
	return commandResult{response: response, code: 0}
}

func supervisorStatusState(drift, ready bool) string {
	if drift {
		return "legacy_present"
	}
	if ready {
		return "ready"
	}
	return "unavailable"
}

func supervisorLegacyStatus(ctx context.Context, root string, deps Dependencies, options pueue.Options, response Response, legacy inspection.GroupRequestRecord, migrated bool) (result commandResult) {
	currentOwner, currentErr := acquireSupervisorClient(currentPrivateSupervisor(ctx, root, deps, options))
	cleanupCause := currentErr
	defer func() { closeSupervisorOwnerMaintenance(currentOwner, cleanupCause) }()
	current := currentOwner.Client()
	if currentErr != nil && current != nil {
		response.SupervisorControl.State = "unavailable"
		response.SupervisorControl.LegacyReachability = "incompatible"
		response.SupervisorControl.ActiveWork = "unknown"
		response.SupervisorControl.NextAction = "stop_and_report"
		return commandResult{
			response: response,
			code:     1,
			err:      errors.Join(errors.New("private supervisor could not be verified"), currentErr),
		}
	}
	currentMatches := currentErr == nil && task.SameSupervisorIdentity(current.Binding(), legacy.Supervisor)
	currentReady := false
	if currentMatches {
		currentReady, currentErr = current.Reachable(ctx)
		cleanupCause = currentErr
	}
	daemonDrift := false
	if currentMatches {
		var daemonErr error
		daemonDrift, daemonErr = privateSupervisorDaemonDrift(root, deps)
		if daemonErr != nil {
			return failedSupervisor(response, "private supervisor identity could not be inspected", 1)
		}
	}
	if currentMatches && currentReady {
		snapshot, snapshotErr := current.Snapshot(ctx)
		if snapshotErr != nil {
			cleanupCause = snapshotErr
			return failedSupervisor(response, "private supervisor status could not be read", 1)
		}
		response.SupervisorControl.ActiveWork = legacyActiveWork(snapshot)
	}
	if result, done := legacySupervisorStateResult(response, legacy, migrated, currentMatches, currentReady, currentErr, daemonDrift); done {
		return result
	}
	if currentMatches {
		response.SupervisorControl.LegacyReachability = "unreachable"
		return commandResult{response: response, code: 1, err: errors.New("legacy supervisor requires managed migration")}
	}
	fallbackClient := ""
	if current != nil {
		fallbackClient = current.Binding().ClientExecutable
	}
	return legacySupervisorWorkStatus(ctx, options, response, legacy, fallbackClient)
}

func legacySupervisorStateResult(response Response, legacy inspection.GroupRequestRecord, migrated, currentMatches, currentReady bool, currentErr error, daemonDrift bool) (commandResult, bool) {
	if currentMatches && currentErr != nil {
		response.SupervisorControl.State = "unavailable"
		response.SupervisorControl.LegacyReachability = "incompatible"
		response.SupervisorControl.ActiveWork = "unknown"
		response.SupervisorControl.NextAction = "stop_and_report"
		return commandResult{
			response: response,
			code:     1,
			err:      errors.New("private supervisor could not be verified"),
		}, true
	}
	if currentMatches && currentReady && daemonDrift {
		response.SupervisorControl.State = "ready"
		response.SupervisorControl.LegacyReachability = "ready"
		response.SupervisorControl.MigrationPhase = "adopted"
		response.SupervisorControl.NextAction = "none"
		return commandResult{response: response, code: 0}, true
	}
	if migrated && currentMatches && currentReady && !daemonDrift {
		response.SupervisorControl.State = "migrated"
		response.SupervisorControl.LegacyReachability = "completed"
		response.SupervisorControl.MigrationPhase = "complete"
		response.SupervisorControl.NextAction = "none"
		return commandResult{response: response, code: 0}, true
	}
	if currentReady && !daemonDrift {
		response.SupervisorControl.State = "ready"
		response.SupervisorControl.LegacyReachability = "ready"
		response.SupervisorControl.NextAction = "none"
		if migrated {
			response.SupervisorControl.State = "migrated"
			response.SupervisorControl.LegacyReachability = "completed"
			response.SupervisorControl.MigrationPhase = "complete"
			return commandResult{response: response, code: 0}, true
		}
		return commandResult{response: response, code: 0}, true
	} else {
		response.SupervisorControl.State = "legacy_present"
		response.SupervisorControl.NextAction = "run_supervisor_migrate"
		if daemonDrift {
			response.SupervisorControl.MigrationPhase = "pending"
		}
	}
	return commandResult{}, false
}

func legacySupervisorWorkStatus(ctx context.Context, options pueue.Options, response Response, legacy inspection.GroupRequestRecord, fallbackClient string) (result commandResult) {
	clientOwner, err := acquireSupervisorClient(pueue.BindSavedForMigrationWithClient(ctx, legacy.Supervisor, fallbackClient, options))
	cleanupCause := err
	defer func() {
		if cleanupErr := clientOwner.CloseAfterPending(cleanupCause); cleanupErr != nil {
			// Supervisor JSON is a stable control surface. Keep cleanup details,
			// including temporary config paths, out of it.
			result.err = errSupervisorCleanup
			result.code = 1
			result.response.setError(result.err)
		}
	}()
	if err != nil {
		response.SupervisorControl.LegacyReachability = "unreachable"
		return commandResult{response: response, code: 1, err: errors.Join(errors.New("legacy supervisor requires managed migration"), err)}
	}
	client := clientOwner.Client()
	ready, probeErr := client.Reachable(ctx)
	if probeErr != nil {
		cleanupCause = probeErr
		response.SupervisorControl.LegacyReachability = "incompatible"
		return commandResult{response: response, code: 1, err: errors.New("legacy supervisor could not be verified")}
	}
	if !ready {
		response.SupervisorControl.LegacyReachability = "unreachable"
		return commandResult{response: response, code: 1, err: errors.New("legacy supervisor requires managed migration")}
	}
	response.SupervisorControl.LegacyReachability = "ready"
	snapshot, snapshotErr := client.Snapshot(ctx)
	if snapshotErr != nil {
		cleanupCause = snapshotErr
		return failedSupervisor(response, "legacy supervisor status could not be read", 1)
	}
	response.SupervisorControl.ActiveWork = legacyActiveWork(snapshot)
	if response.SupervisorControl.ActiveWork == "none" {
		response.SupervisorControl.NextAction = "run_supervisor_migrate"
	} else {
		response.SupervisorControl.NextAction = "wait_for_legacy_work"
	}
	return commandResult{response: response, code: 0}
}

func privateSupervisorDaemonDrift(root string, deps Dependencies) (bool, error) {
	_, daemonExecutable, err := resolveCurrentPrivateSupervisorExecutables(root, deps)
	if err != nil {
		return false, err
	}
	return pueue.PrivateDaemonIdentityDrift(root, daemonExecutable)
}

func supervisorControlContext(options pueue.Options) (context.Context, context.CancelFunc) {
	timeout := pueue.PrivateControlTimeout(options)
	if timeout <= 0 {
		timeout = supervisorMigrationTimeout
	}
	return context.WithTimeout(context.Background(), timeout)
}

func supervisorControlOptions(deps Dependencies) pueue.Options {
	options := deps.SupervisorOptions
	if options.ObservationTimeout == 0 {
		options.ObservationTimeout = pueue.DefaultObservationTimeout
	}
	return options
}

func currentPrivateSupervisor(ctx context.Context, root string, deps Dependencies, options pueue.Options) (*pueue.Client, error) {
	clientExecutable, _, err := resolveCurrentPrivateSupervisorExecutables(root, deps)
	if err != nil {
		return nil, err
	}
	return pueue.Bind(ctx, clientExecutable, pueue.PrivateConfigPath(root), options)
}

func prepareCurrentPrivateSupervisor(ctx context.Context, root string, deps Dependencies, options pueue.Options) (*pueue.Client, error) {
	clientExecutable, daemonExecutable, err := resolveCurrentPrivateSupervisorExecutables(root, deps)
	if err != nil {
		return nil, err
	}
	if err = installStateSupervisorPair(ctx, root, clientExecutable, daemonExecutable); err != nil {
		return nil, err
	}
	return pueue.PreparePrivateForMigrationWithLock(ctx, clientExecutable, daemonExecutable, root, options)
}

type supervisorMigrationState struct {
	current                 *pueue.Client
	noInspectionGroup       bool
	notInitialized          bool
	externalInspectionGroup bool
	activeWork              string
}

func supervisorMigrate(root string, deps Dependencies, response Response) (result commandResult) {
	options := supervisorControlOptions(deps)
	controlContext, cancel := supervisorControlContext(options)
	defer cancel()
	store, err := openStoreContext(controlContext, root, deps.storeDependencies(), false)
	if errors.Is(err, os.ErrNotExist) {
		return notInitializedSupervisor(response)
	}
	if err != nil {
		return failedSupervisor(response, "supervisor state root could not be opened", 1)
	}
	defer func() { mergeCommandClose(&result, store.Close) }()
	response.RootID = store.RootID
	state, err := inspectSupervisorMigration(controlContext, root, deps, options, store)
	if state.current != nil {
		defer closeSupervisorMaintenance(state.current, err)
	}
	if err == nil && state.notInitialized {
		return notInitializedSupervisor(response)
	}
	return finishSupervisorMigration(response, state, err)
}

func notInitializedSupervisor(response Response) commandResult {
	response.SupervisorControl.State = "not_initialized"
	response.SupervisorControl.LegacyReachability = "not_present"
	response.SupervisorControl.ActiveWork = "none"
	response.SupervisorControl.NextAction = "none"
	return commandResult{response: response, code: 0}
}

func inspectSupervisorMigration(ctx context.Context, root string, deps Dependencies, options pueue.Options, store *taskdir.Store) (state supervisorMigrationState, resultErr error) {
	state.activeWork = "none"
	present, err := privateSupervisorStatePresent(root)
	if err != nil {
		return state, fmt.Errorf("private supervisor state could not be inspected: %w", err)
	}
	if !present {
		state.notInitialized = true
		return state, nil
	}
	observationTimeout := options.ObservationTimeout
	if observationTimeout <= 0 {
		observationTimeout = pueue.DefaultObservationTimeout
	}
	resultErr = pueue.WithBootstrapLockContext(ctx, root, func(lockedContext context.Context) error {
		return inspectSupervisorMigrationLocked(lockedContext, root, deps, options, store, observationTimeout, &state)
	})
	return state, resultErr
}

func privateSupervisorStatePresent(root string) (bool, error) {
	configPath := pueue.PrivateConfigPath(root)
	if _, err := os.Stat(configPath); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Stat(filepath.Dir(configPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	_, err := os.Stat(filepath.Dir(configPath))
	return err == nil, nil
}

func inspectSupervisorMigrationLocked(ctx context.Context, root string, deps Dependencies, options pueue.Options, store *taskdir.Store, observationTimeout time.Duration, state *supervisorMigrationState) error {
	legacy, _, journalErr := inspection.ReadGroupHead(ctx, store)
	if errors.Is(journalErr, os.ErrNotExist) {
		return inspectSupervisorWithoutGroup(ctx, root, deps, options, state)
	}
	if journalErr != nil {
		return fmt.Errorf("supervisor migration journal could not be read: %w", journalErr)
	}
	if !pueue.IsPrivateConfig(root, legacy.Supervisor.ConfigPath) {
		state.externalInspectionGroup = true
		return nil
	}
	current, err := prepareCurrentPrivateSupervisor(ctx, root, deps, options)
	if err != nil {
		return err
	}
	state.current = current
	return ensureInspectionGroupLocked(ctx, store, current, observationTimeout, true)
}

func inspectSupervisorWithoutGroup(ctx context.Context, root string, deps Dependencies, options pueue.Options, state *supervisorMigrationState) error {
	state.noInspectionGroup = true
	if _, err := os.Stat(pueue.PrivateConfigPath(root)); errors.Is(err, os.ErrNotExist) {
		state.notInitialized = true
		return nil
	} else if err != nil {
		return fmt.Errorf("private supervisor state could not be inspected: %w", err)
	}
	current, err := supervisorMigrateWithoutInspectionGroupLocked(ctx, root, deps, options)
	if err != nil {
		state.current = current
		return err
	}
	state.current = current
	snapshot, err := current.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("private supervisor status could not be read: %w", err)
	}
	state.activeWork = legacyActiveWork(snapshot)
	return nil
}

func finishSupervisorMigration(response Response, state supervisorMigrationState, err error) commandResult {
	if state.externalInspectionGroup {
		response.SupervisorControl.State = "external"
		response.SupervisorControl.LegacyReachability = "not_managed"
		response.SupervisorControl.ActiveWork = "unknown"
		response.SupervisorControl.NextAction = "stop_and_report"
		return commandResult{response: response, code: 1, err: errors.New("inspection group uses an external supervisor")}
	}
	if err != nil {
		return supervisorMigrationFailure(response, err)
	}
	response.SupervisorControl.State = "migrated"
	if state.noInspectionGroup {
		response.SupervisorControl.LegacyReachability = "not_present"
		response.SupervisorControl.ActiveWork = state.activeWork
	} else {
		response.SupervisorControl.LegacyReachability = "recovered"
		response.SupervisorControl.ActiveWork = "none"
	}
	response.SupervisorControl.MigrationPhase = "complete"
	response.SupervisorControl.NextAction = "retry_dispatch"
	return commandResult{response: response, code: 0}
}

func supervisorMigrationFailure(response Response, err error) commandResult {
	if errors.Is(err, inspection.ErrGroupMigrationBusy) || errors.Is(err, pueue.ErrPrivateSupervisorMigrationBusy) {
		response.SupervisorControl.State = "blocked"
		response.SupervisorControl.ActiveWork = "present"
		response.SupervisorControl.NextAction = "wait_for_legacy_work"
		return commandResult{response: response, code: 1, err: errors.New("legacy supervisor has active inspection work")}
	}
	response.SupervisorControl.State = "unavailable"
	response.SupervisorControl.NextAction = "stop_and_report"
	return commandResult{response: response, code: 1, err: errors.New("delegate could not safely migrate the legacy supervisor")}
}

func supervisorMigrateWithoutInspectionGroupLocked(ctx context.Context, root string, deps Dependencies, options pueue.Options) (*pueue.Client, error) {
	current, err := prepareCurrentPrivateSupervisor(ctx, root, deps, options)
	if err != nil {
		return nil, err
	}
	pending, err := pueue.PrivateSupervisorMigrationPending(root)
	if err != nil {
		return current, err
	}
	if !pending {
		if err = current.VerifyPrivateMigrationEndpoint(ctx, root); err != nil {
			return current, err
		}
	}
	if err = current.MigratePrivateSupervisorWithLock(ctx, root); err != nil {
		return current, err
	}
	if err = current.StartPrivate(ctx, root); err != nil {
		return current, err
	}
	return current, nil
}

func failedSupervisor(response Response, message string, code int) commandResult {
	response.Error = message
	if response.SupervisorControl != nil && response.SupervisorControl.State == "unknown" {
		response.SupervisorControl.State = "unavailable"
	}
	return commandResult{response: response, code: code, err: errors.New(message)}
}

func legacyActiveWork(snapshot pueue.QueueSnapshot) string {
	for _, job := range snapshot.Jobs {
		if job.State == pueue.StateQueued || job.State == pueue.StateStashed || job.State == pueue.StateRunning || job.State == pueue.StateUnknown {
			return "present"
		}
	}
	return "none"
}

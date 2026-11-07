package app

// Terminal task maintenance composition. Ownership:
//   - The elected private queue-maintenance owner owns optional row removal and
//     custody retry for a private supervisor. Pueue hands each capture to
//     releasePrivateTerminalCustody outside bootstrap SH and queue identity.
//   - guardian.ReadClosureProof and the exact recorded placement remain the
//     only release authority. Queue candidates, task-directory names, outcomes
//     and wake generations are discovery, never authority.
//   - A public status or collect of a private task never removes a row, binds
//     the supervisor for mutation, or releases custody. It reads its own task
//     and, for a positive remaining-work hint, persists one wake and starts the
//     existing independent owner.
//   - A validated saved external binding has no elected owner, so its public
//     observer keeps the synchronous custody retry.
//   - An ordinary private guardian removes no view after its run. It hands a
//     closed lifetime's custody to the elected owner through one durable full
//     wake and the existing launcher. Inspection and external guardians keep
//     their direct post-run release.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"

	"github.com/hishamkaram/delegation-layer/internal/guardian"
	"github.com/hishamkaram/delegation-layer/internal/predicate"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
	supervisorcore "github.com/hishamkaram/delegation-layer/internal/supervisor"
	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/tasklifecycle"
)

// releasePrivateTerminalCustody is the application custody owner of one
// elected maintenance phase. Captured inspection candidates use the canonical
// inspection helper. The full-recovery phase has no candidates and reconciles
// ordinary task custody independently of queue rows, because a removed row, a
// lost callback or an unavailable queue must not strand a closed lifetime's
// views. Every task is attempted; faults are joined so the pass stays failed
// and a later full wake retries it.
func releasePrivateTerminalCustody(ctx context.Context, root string, supervisor task.SupervisorRef, scope pueue.PrivateTerminalCustodyScope) error {
	err := privateCleanupOwner().ReleaseInspectionCustody(ctx, root, supervisor, scope.Candidates)
	if !scope.Full {
		return err
	}
	return errors.Join(err, releaseClosedTaskCustody(ctx, root))
}

// closedTaskCustodyWorkers bounds the ordinary-task custody releases one full
// pass runs at once. Each release may wait on a pool's non-interruptible view
// removal, so a pass at its deadline can still hold up to this many.
const closedTaskCustodyWorkers = 4

func releaseClosedTaskCustody(ctx context.Context, root string) (resultErr error) {
	store, err := taskdir.OpenStoreWithPredicatesContext(ctx, root, predicate.Default())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, store.Close()) }()
	ids, err := store.TaskIDs(ctx)
	if err != nil {
		return err
	}
	return releaseTaskCustodyInOrder(ctx, ids, func(ctx context.Context, id string) error {
		return releaseStoredTaskCustody(ctx, store, id)
	})
}

// releaseTaskCustodyInOrder is the coordinator of one pass's task releases. It
// hands the unique ids out in inventory order to at most
// closedTaskCustodyWorkers workers; a worker checks ctx after receiving an id
// and before calling release, and stops once it observes cancellation. A
// release fault never skips an independent task. Every started release is
// joined before the coordinator returns, so the caller's store outlives it.
// Faults keep inventory order and their task ids; cancellation that skipped
// any id is joined once after them.
func releaseTaskCustodyInOrder(ctx context.Context, ids []string, release func(context.Context, string) error) error {
	faults := make([]error, len(ids))
	work := make(chan int)
	var workers sync.WaitGroup
	var workerStopped atomic.Bool
	for range min(closedTaskCustodyWorkers, len(ids)) {
		workers.Go(func() {
			for index := range work {
				if ctx.Err() != nil {
					workerStopped.Store(true)
					return
				}
				if err := release(ctx, ids[index]); err != nil {
					faults[index] = fmt.Errorf("task %s custody release: %w", ids[index], err)
				}
			}
		})
	}
	handedOut := handOutTaskCustody(ctx, len(ids), work)
	close(work)
	workers.Wait()
	if !handedOut || workerStopped.Load() {
		faults = append(faults, ctx.Err())
	}
	return errors.Join(faults...)
}

// handOutTaskCustody sends each inventory index in order and reports whether
// every index was handed out before ctx was done.
func handOutTaskCustody(ctx context.Context, count int, work chan<- int) bool {
	for index := range count {
		select {
		case work <- index:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func releaseStoredTaskCustody(ctx context.Context, store *taskdir.Store, id string) (resultErr error) {
	td, err := store.OpenTask(id)
	if task.RecordAbsent(err) {
		// Task creation opens its locks before any lifetime record, so a
		// partially created directory holds no custody.
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, td.Close()) }()
	releaseErr := releaseLifetimeCustody(ctx, td)
	request, readErr := td.RecordedRequest()
	if readErr != nil {
		return errors.Join(releaseErr, readErr)
	}
	if request.Remote == nil || len(request.Remote.OutputPaths) == 0 {
		return releaseErr
	}
	_, closureErr := guardian.ReadClosureProof(ctx, td)
	if errors.Is(closureErr, guardian.ErrProofPending) {
		return releaseErr
	}
	if closureErr != nil {
		return errors.Join(releaseErr, closureErr)
	}
	_, captureErr := captureRemoteOutputs(ctx, store, td)
	return errors.Join(releaseErr, captureErr)
}

// taskMaintenanceObserver is the public status and collect maintenance step.
// It runs once, before the task handle closes, and never changes the command
// result: every fault is a logged warning.
type taskMaintenanceObserver struct {
	root    string
	options pueue.Options
	lookup  supervisorcore.ExecutableLookup
}

func (o taskMaintenanceObserver) observeTask(ctx context.Context, td *taskdir.TaskDir) error {
	req, meta, spec, metaHash, err := td.PreparedIdentity()
	if err != nil {
		// A malformed saved binding classifies nothing, so it never falls back
		// to the external synchronous release.
		return err
	}
	if !pueue.IsPrivateConfig(o.root, meta.SupervisorConfig.ConfigPath) {
		return releaseLifetimeCustody(ctx, td)
	}
	custodyWork, custodyErr := closedLifetimeCustodyReleasePending(ctx, td)
	rowWork, client, rowErr := o.observeEndedRow(ctx, td, req, meta, spec, metaHash)
	captureWork, captureErr := remoteTaskCapturePending(ctx, o.root, td, req)
	err = errors.Join(custodyErr, rowErr, captureErr)
	if custodyWork || rowWork || captureWork {
		err = errors.Join(err, o.requestMaintenance(ctx, client))
	}
	supervisorcore.CloseMaintenance(client, err)
	return err
}

// closedLifetimeCustodyReleasePending reports positive remaining custody work
// for a canonically closed lifetime. Closure is proven before the pool is
// asked, because a pending answer also covers a lifetime that is still open.
func closedLifetimeCustodyReleasePending(ctx context.Context, records guardian.ClosureRecords) (bool, error) {
	custody, closed, err := closedLifetimeCustody(ctx, records)
	if err != nil || !closed {
		return false, err
	}
	return custody.pool.ReleasePending(custody.namespace)
}

// observeEndedRow observes, read-only, whether this task's exact ended queue
// row is still present after its outcome was published. Only a successful
// exact match is positive; an unknown observation proves nothing. The
// returned client is structural and owned by the caller.
func (o taskMaintenanceObserver) observeEndedRow(ctx context.Context, td *taskdir.TaskDir, req *task.TaskRecord, meta *task.MetaRecord, spec, metaHash string) (bool, *pueue.Client, error) {
	submission, receipt, err := publishedTaskSubmission(td)
	if err != nil || submission == nil {
		return false, nil, err
	}
	saved := submission.Supervisor
	current, found, err := pueue.ResolvePrivateSupervisorSuccessor(ctx, o.root, saved)
	if err != nil || !found {
		return false, nil, err
	}
	client, err := supervisorcore.NewClient(ctx, o.root, current, supervisorcore.OptionsWithEnvironment(o.options, meta.Environment), o.lookup)
	if err != nil {
		return false, nil, err
	}
	if err = client.ScopePrivateTask(ctx, o.root, saved); err != nil {
		return false, client, err
	}
	observer, err := client.WithObservationTimeout(pueue.PrivateStatusObservationTimeout)
	if err != nil {
		return false, client, err
	}
	observation, err := observer.ReconcileStatus(ctx, tasklifecycle.TaskIdentity(req, meta, spec, metaHash, receipt))
	if !observation.Matched && task.AllErrorCauses(err, isUnknownObservation) {
		// Absence, ambiguity and a numeric mismatch are all unknown: no work is
		// proven. A concrete fault joined beside it stays a warning.
		return false, client, nil
	}
	return err == nil && observation.Matched && observation.State == pueue.StateEnded, client, err
}

// publishedTaskSubmission returns the saved submission and receipt of a task
// whose outcome is published, or nil when it has none: a row of an unadmitted
// or unpublished task proves no removable work.
func publishedTaskSubmission(td *taskdir.TaskDir) (*task.SubmitRecord, *task.SupervisorReceipt, error) {
	inspection, err := td.Inspect()
	if err != nil || inspection.Outcome == nil {
		return nil, nil, err
	}
	submission, err := td.ReadSubmission()
	if err != nil {
		return nil, nil, absentRecordIsNone(err)
	}
	receipt, err := td.ReadSupervisorReceipt()
	if err != nil {
		return nil, nil, absentRecordIsNone(err)
	}
	return submission, receipt, nil
}

func absentRecordIsNone(err error) error {
	if task.RecordAbsent(err) {
		return nil
	}
	return err
}

// requestMaintenance persists one full wake for an accepted positive hint and
// starts the existing independent owner. Each accepted hint is a new
// generation, so a hint observed after a failed pass is runnable again. The
// row observation may have exhausted ctx, so the wake is published in its own
// bounded phase. The wake stays durable when no owner can be started here.
func (o taskMaintenanceObserver) requestMaintenance(ctx context.Context, client *pueue.Client) error {
	if err := requestPrivateFullMaintenanceWake(ctx, o.root); err != nil {
		return err
	}
	if client == nil || o.options.PrivateMaintenanceLauncher == nil {
		// Without a launcher the scheduler would run the owner inside this
		// command, so the durable wake waits for the next callback or admission.
		return nil
	}
	if !client.ScheduleRequestedPrivateQueueMaintenance(ctx) {
		return errPrivateMaintenanceNotStarted
	}
	return nil
}

func isUnknownObservation(err error) bool { return errors.Is(err, pueue.ErrUnknown) }

var errPrivateMaintenanceNotStarted = errors.New("private queue maintenance owner was not started; the wake remains durable")

// releaseGuardianLifetimeCustody is the post-run release of an inspection or
// external guardian. A private inspection's release fault persists a full
// wake before the guardian returns, so the Done callback's elected pass
// retries the exact recorded pool. The guardian's own result never changes.
func releaseGuardianLifetimeCustody(root string, closure guardian.ClosureRecords, private bool) {
	ctx, cancel := context.WithTimeout(context.Background(), lifetimeCustodyReleaseTimeout)
	defer cancel()
	err := releaseLifetimeCustody(ctx, closure)
	if err == nil {
		return
	}
	log.Printf("delegate: lifetime custody release deferred: %v", err)
	if !private {
		return
	}
	if wakeErr := requestPrivateFullMaintenanceWake(ctx, root); wakeErr != nil {
		log.Printf("delegate: lifetime custody recovery request deferred: %v", wakeErr)
	}
}

// handOffGuardianLifetimeCustody is an ordinary private guardian's post-run
// custody step. It never removes a view. A closed lifetime with remaining
// views, or a pending check that faulted, gets one durable full wake before
// the existing independent owner is launched; that owner's elected pass
// releases through the canonical closure policy. Pending closure or released
// custody requests nothing. A fault keeps the views, and the wake once
// published, and is returned beside any check fault.
func handOffGuardianLifetimeCustody(root string, closure guardian.ClosureRecords, options pueue.Options) error {
	ctx, cancel := context.WithTimeout(context.Background(), lifetimeCustodyReleaseTimeout)
	defer cancel()
	pending, err := closedLifetimeCustodyReleasePending(ctx, closure)
	if err == nil && !pending {
		return nil
	}
	if wakeErr := requestPrivateFullMaintenanceWake(ctx, root); wakeErr != nil {
		return errors.Join(err, wakeErr)
	}
	if options.PrivateMaintenanceLauncher == nil || options.ControlOwnerExecutable == "" {
		return errors.Join(err, errPrivateMaintenanceNotStarted)
	}
	// The wake is durable, so launch has its own bound, as the scheduled
	// handoff does, even after the pending check exhausted ctx.
	launchCtx, cancelLaunch := context.WithTimeout(context.WithoutCancel(ctx), pueue.DefaultObservationTimeout)
	defer cancelLaunch()
	if launchErr := options.PrivateMaintenanceLauncher(launchCtx, options.ControlOwnerExecutable, root, options.PrivateMaintenanceEnvironment); launchErr != nil {
		return errors.Join(err, launchErr, errPrivateMaintenanceNotStarted)
	}
	return err
}

// requestPrivateFullMaintenanceWake publishes one full wake in its own bounded
// phase. The caller's release or observation may have exhausted or cancelled
// ctx; the wake must still be durable, under the same bound the terminal
// callback uses for its journal publication.
func requestPrivateFullMaintenanceWake(ctx context.Context, root string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pueue.DefaultObservationTimeout)
	defer cancel()
	return pueue.RequestPrivateQueueMaintenance(ctx, root)
}

// Remote capture recovery is requested through the elected maintenance owner;
// observers never reread mutable workspace bytes or perform capture themselves.
func remoteTaskCapturePending(ctx context.Context, root string, td *taskdir.TaskDir, request *task.TaskRecord) (pending bool, resultErr error) {
	if request.Remote == nil || len(request.Remote.OutputPaths) == 0 {
		return false, nil
	}
	if _, err := guardian.ReadClosureProof(ctx, td); err != nil {
		if errors.Is(err, guardian.ErrProofPending) {
			return false, nil
		}
		return false, err
	}
	directory, err := os.OpenRoot(root)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	_, err = readRemoteCapturePassive(ctx, directory, request.RootID, request.TaskID)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	return false, err
}

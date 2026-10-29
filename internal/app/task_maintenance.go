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

import (
	"context"
	"errors"
	"fmt"
	"log"

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
	var faults []error
	for _, id := range ids {
		if err = ctx.Err(); err != nil {
			return errors.Join(append(faults, err)...)
		}
		if err = releaseStoredTaskCustody(ctx, store, id); err != nil {
			faults = append(faults, fmt.Errorf("task %s custody release: %w", id, err))
		}
	}
	return errors.Join(faults...)
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
	return releaseLifetimeCustody(ctx, td)
}

// taskMaintenanceObserver is the public status and collect maintenance step.
// It runs once, before the task handle closes, and never changes the command
// result: every fault is a logged warning.
type taskMaintenanceObserver struct {
	root    string
	options pueue.Options
}

func (o taskMaintenanceObserver) observe(td *taskdir.TaskDir) {
	ctx, cancel := context.WithTimeout(context.Background(), lifetimeCustodyReleaseTimeout)
	defer cancel()
	if err := o.observeTask(ctx, td); err != nil {
		log.Printf("delegate: task maintenance observation deferred: %v", err)
	}
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
	err = errors.Join(custodyErr, rowErr)
	if custodyWork || rowWork {
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
	client, err := pueue.NewClient(current, supervisorcore.OptionsWithEnvironment(o.options, meta.Environment))
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

// releaseGuardianLifetimeCustody is the guardian's post-run release. A private
// task's release fault persists a full wake before the guardian returns, so
// the Done callback's elected pass retries the exact recorded pool. The
// guardian's own result never changes.
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

// requestPrivateFullMaintenanceWake publishes one full wake in its own bounded
// phase. The caller's release or observation may have exhausted or cancelled
// ctx; the wake must still be durable, under the same bound the terminal
// callback uses for its journal publication.
func requestPrivateFullMaintenanceWake(ctx context.Context, root string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pueue.DefaultObservationTimeout)
	defer cancel()
	return pueue.RequestPrivateQueueMaintenance(ctx, root)
}

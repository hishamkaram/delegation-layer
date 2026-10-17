package control

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

func privateClientReady(ctx context.Context, client *Client) (bool, error) {
	return privateClientReadyWithCleanup(ctx, client, true)
}

func privateClientReadyWithCleanup(ctx context.Context, client *Client, cleanupSuccessful bool) (bool, error) {
	_, ready, err := privateClientReadySnapshotWithCleanup(ctx, client, cleanupSuccessful)
	return ready, err
}

func privateClientReadySnapshotWithCleanup(ctx context.Context, client *Client, cleanupSuccessful bool) (QueueSnapshot, bool, error) {
	if client == nil {
		return QueueSnapshot{}, false, ErrConfiguration
	}
	cleaned := false
	for {
		result, err := privateReadyAttempt(ctx, client)
		if err != nil {
			if handled, unavailable, endpointErr := privateReadyEndpointFailure(ctx, client, result, err); handled {
				if endpointErr != nil {
					return QueueSnapshot{}, false, endpointErr
				}
				return QueueSnapshot{}, !unavailable, nil
			}
			if retry, cleanErr := client.cleanupStatusOverflow(ctx, err, cleanupSuccessful, cleaned); cleanErr != nil {
				return QueueSnapshot{}, false, errors.Join(err, cleanErr)
			} else if retry {
				cleaned = true
				continue
			}
			return QueueSnapshot{}, false, errors.Join(ErrBinding, err)
		}
		if err = client.validateReadyResult(result); err != nil {
			return QueueSnapshot{}, false, errors.Join(ErrBinding, err)
		}
		snapshot, err := snapshotFromResult(result, client.runtimeBinding.ObservedVersion)
		if err != nil {
			return QueueSnapshot{}, false, errors.Join(ErrBinding, err)
		}
		return snapshot, true, nil
	}
}

func privateReadyAttempt(ctx context.Context, client *Client) (CommandResult, error) {
	result, err := client.readyCommand(ctx)
	if pending := pendingFrom(err); pending != nil {
		return awaitReadyPending(ctx, client, pending)
	}
	return result, err
}

// privateClientReadyGroups verifies the private endpoint through Pueue's
// bounded group response. Lifecycle admission needs only daemon liveness and
// group scheduling facts; it must not deserialize every task row while a large
// private queue is active.
func privateClientReadyGroups(ctx context.Context, client *Client) (map[string]Group, bool, error) {
	if client == nil {
		return nil, false, ErrConfiguration
	}
	groups, result, err := client.privateGroups(ctx)
	if err == nil {
		return groups, true, nil
	}
	if pendingFrom(err) != nil {
		return nil, false, err
	}
	if handled, unavailable, endpointErr := privateReadyEndpointFailure(ctx, client, result, err); handled {
		if endpointErr != nil {
			return nil, false, endpointErr
		}
		return nil, !unavailable, nil
	}
	return nil, false, errors.Join(ErrBinding, err)
}

func (c *Client) privateGroups(ctx context.Context) (map[string]Group, CommandResult, error) {
	result, err := c.groupAfterVerifiedBinding(ctx)
	if pending := pendingFrom(err); pending != nil {
		result, err = awaitPending(ctx, pending)
	}
	if err != nil {
		return nil, result, err
	}
	groups, err := groupsFromResult(result)
	return groups, result, err
}

func privateReadyEndpointFailure(ctx context.Context, client *Client, result CommandResult, err error) (handled, unavailable bool, resultErr error) {
	// A nonzero liveness command is only an absent endpoint when the exact Unix
	// socket also proves absent. A live endpoint with an incompatible response
	// must never be mistaken for a stopped daemon during migration.
	if !result.Started || result.ExitCode == 0 {
		return false, false, nil
	}
	unavailable, probeErr := client.endpointUnavailable(ctx)
	if probeErr != nil {
		return true, false, errors.Join(err, probeErr)
	}
	if unavailable {
		return true, true, nil
	}
	return true, false, errors.Join(ErrBinding, err)
}

// cleanSuccessfulTasks removes only exact terminal rows authorized by the
// application. It never issues broad clean, including after status overflow.
func (c *Client) cleanSuccessfulTasks(ctx context.Context, eventOnly bool) error {
	for {
		settling, err := c.cleanSuccessfulTasksPass(ctx, eventOnly)
		if err != nil {
			return err
		}
		if !settling {
			return nil
		}
		// A settling hint grants no removal authority. Release queue identity
		// while the daemon advances its row, then reacquire it for a fresh
		// snapshot and application decision. This keeps admission available
		// without weakening the snapshot-to-removal identity invariant.
		if err = waitPrivateQueueSettlement(ctx); err != nil {
			return err
		}
	}
}

func (c *Client) cleanSuccessfulTasksPass(ctx context.Context, eventOnly bool) (bool, error) {
	snapshot, plan, err := c.authorizePrivateSuccessfulTasks(ctx, eventOnly)
	if err != nil {
		return false, err
	}
	if len(plan.RemoveIDs) != 0 {
		err = c.withPrivateQueueIdentityMutation(ctx, func(lockedContext context.Context) error {
			current, snapshotErr := c.privateCleanupSnapshot(lockedContext)
			if snapshotErr != nil {
				return snapshotErr
			}
			if validateAuthorizedTerminalRows(snapshot, current, plan.RemoveIDs) != nil {
				return ErrCleanupAuthorization
			}
			return c.removeAuthorizedTerminalRows(lockedContext, current, plan.RemoveIDs)
		})
		if err != nil {
			return false, err
		}
	}
	if err = c.clearStalePrivateTerminalEvents(ctx); err != nil {
		return false, err
	}
	return plan.Settling, nil
}

// clearStalePrivateTerminalEvents repairs a callback candidate left behind
// after its queue row was removed. The fresh full snapshot and the event
// retirement must share the queue-identity mutation lease so a new submission
// cannot reuse an ID between those operations.
func (c *Client) clearStalePrivateTerminalEvents(ctx context.Context) error {
	if c == nil {
		return ErrConfiguration
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return nil
	}
	enabled, err := c.PrivateTerminalEventsEnabled(ctx)
	if err != nil || !enabled {
		return err
	}
	pending, err := privateTerminalEventWakePending(ctx, stateRoot)
	if err != nil || !pending {
		return err
	}
	return c.withPrivateQueueIdentityMutation(ctx, func(lockedContext context.Context) error {
		snapshot, snapshotErr := c.privateCleanupSnapshot(lockedContext)
		if snapshotErr != nil {
			return snapshotErr
		}
		return c.clearPrivateTerminalEventsForAbsentRows(lockedContext, snapshot)
	})
}

func validateAuthorizedTerminalRows(authorized, current QueueSnapshot, ids []int64) error {
	authorizedLabels := queueJobLabelCounts(authorized.Jobs)
	currentLabels := queueJobLabelCounts(current.Jobs)
	for _, id := range ids {
		before, beforeCount := uniqueQueueJob(authorized.Jobs, id)
		after, afterCount := uniqueQueueJob(current.Jobs, id)
		if beforeCount != 1 || afterCount != 1 || !sameTerminalQueueJob(before, after) {
			return ErrCleanupAuthorization
		}
		if before.Label == nil || authorizedLabels[*before.Label] != 1 || currentLabels[*before.Label] != 1 {
			return ErrCleanupAuthorization
		}
	}
	return nil
}

func queueJobLabelCounts(jobs []Job) map[string]int {
	counts := make(map[string]int, len(jobs))
	for _, job := range jobs {
		if job.Label != nil {
			counts[*job.Label]++
		}
	}
	return counts
}

func uniqueQueueJob(jobs []Job, id int64) (Job, int) {
	var matched Job
	count := 0
	for _, job := range jobs {
		if job.ID == id {
			matched = job
			count++
		}
	}
	return matched, count
}

func sameTerminalQueueJob(left, right Job) bool {
	if left.ID != right.ID || left.State != StateEnded || right.State != StateEnded ||
		left.Group != right.Group || left.Succeeded != right.Succeeded {
		return false
	}
	if left.Label == nil || right.Label == nil {
		return left.Label == nil && right.Label == nil
	}
	return *left.Label == *right.Label
}

// authorizePrivateSuccessfulTasks takes one queue snapshot and asks the
// application-owned policy to validate its terminal rows. Evidence preparation
// consumes the identity-protected snapshot; authorization itself is read-only
// and runs outside that mutation lease.
func (c *Client) authorizePrivateSuccessfulTasks(ctx context.Context, eventOnly bool) (QueueSnapshot, PrivateQueueCleanupPlan, error) {
	if c == nil {
		return QueueSnapshot{}, PrivateQueueCleanupPlan{}, ErrConfiguration
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return QueueSnapshot{}, PrivateQueueCleanupPlan{}, ErrConfiguration
	}
	if c.options.SuccessfulCleanupAuthorizer == nil {
		return QueueSnapshot{}, PrivateQueueCleanupPlan{}, ErrCleanupAuthorization
	}
	var snapshot QueueSnapshot
	var candidates []Job
	err := c.withPrivateQueueIdentityMutation(ctx, func(lockedContext context.Context) error {
		var readErr error
		snapshot, readErr = c.privateCleanupSnapshot(lockedContext)
		if readErr != nil {
			return readErr
		}
		candidates, readErr = c.privateTerminalEventJobs(lockedContext, stateRoot, cloneQueueSnapshot(snapshot).Jobs, eventOnly)
		if readErr != nil || c.options.PrivateTerminalEvidencePreparer == nil {
			return readErr
		}
		return c.options.PrivateTerminalEvidencePreparer(lockedContext, stateRoot, c.binding, candidates)
	})
	if err != nil {
		return QueueSnapshot{}, PrivateQueueCleanupPlan{}, err
	}
	plan, err := c.options.SuccessfulCleanupAuthorizer(ctx, stateRoot, c.binding, candidates)
	if err != nil {
		return QueueSnapshot{}, PrivateQueueCleanupPlan{}, err
	}
	return snapshot, plan, nil
}

func (c *Client) privateCleanupSnapshot(ctx context.Context) (QueueSnapshot, error) {
	if err := c.verify(ctx); err != nil {
		return QueueSnapshot{}, err
	}
	return c.snapshotAfterVerifySettled(ctx)
}

func waitPrivateQueueSettlement(ctx context.Context) error {
	timer := time.NewTimer(privateQueueSettlementPoll)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) removeAuthorizedTerminalRows(ctx context.Context, snapshot QueueSnapshot, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	ended := make(map[int64]int, len(snapshot.Jobs))
	for _, job := range snapshot.Jobs {
		if job.State == StateEnded {
			ended[job.ID]++
		}
	}
	args := make([]string, 1, len(ids)+1)
	args[0] = "remove"
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if ended[id] != 1 {
			return ErrCleanupAuthorization
		}
		if _, duplicate := seen[id]; duplicate {
			return ErrCleanupAuthorization
		}
		seen[id] = struct{}{}
		args = append(args, strconv.FormatInt(id, 10))
	}
	// Pueue accepts multiple explicit IDs in one remove command. Every ID still
	// names the authorized row here because it remains present from the snapshot
	// through this command; a concurrent add therefore cannot reuse it.
	result, err := c.command(ctx, nil, args...)
	if pending := pendingFrom(err); pending != nil {
		settled, waitErr := awaitPending(ctx, pending)
		result, err = settled, waitErr
	}
	if err = errors.Join(err, result.Err); err != nil {
		return err
	}
	// Durable evidence authorized every row, including rows recovered without
	// an event. Retire callback state after successful removal; failed removals
	// keep it so the next maintenance generation can retry the exact row. A new
	// submission also replaces classification before enqueue, covering a crash
	// between successful removal and this bounded cleanup.
	return c.ClearPrivateTerminalEvents(ctx, ids...)
}

// MaintainPrivateQueue opportunistically reaps terminal private-supervisor
// rows that the application has authorized from durable evidence. It returns
// true when this private client can perform or coalesce that cleanup. It never
// waits behind an active lifecycle transition and it never changes admission
// or inspection authority when maintenance cannot run.
func (c *Client) MaintainPrivateQueue(ctx context.Context) bool {
	if ctx == nil || c == nil || !isPrivateClient(c) || c.options.SuccessfulCleanupAuthorizer == nil {
		return false
	}
	finish, _, available := c.beginPrivateMaintenance()
	if finish == nil {
		return available
	}
	c.runPrivateQueueMaintenance(ctx, finish, false)
	return true
}

func newPrivateQueueMaintenanceRun(client *Client, scheduled bool) privateQueueMaintenanceRun {
	lockMode := unix.LOCK_SH | unix.LOCK_NB
	if scheduled {
		lockMode = unix.LOCK_SH
	}
	worker := client.backgroundClient()
	stateRoot, private := privateStateRoot(worker.binding.ConfigPath)
	return privateQueueMaintenanceRun{
		worker:      worker,
		stateRoot:   stateRoot,
		private:     private,
		scheduled:   scheduled,
		lockMode:    lockMode,
		passTimeout: privateQueueMaintenanceTimeout(client.options.ObservationTimeout),
	}
}

func (c *Client) runPrivateQueueMaintenance(ctx context.Context, finish func(), waitForLease bool) {
	finishOnReturn := true
	defer func() {
		if finishOnReturn {
			finish()
		}
	}()
	ownerContext := ctx
	if waitForLease {
		// A scheduled owner has already left the admission critical path. Its
		// journal generation and election define lifetime; caller cancellation
		// cannot revoke work that was durably requested.
		ownerContext = context.WithoutCancel(ctx)
	}
	run := newPrivateQueueMaintenanceRun(c, waitForLease)
	for {
		skipped, err := run.attempt(ownerContext)
		if skipped {
			return
		}
		if waitForLease {
			resume, waitErr := waitPrivateQueueMaintenancePending(ownerContext, run.stateRoot, run.private, err)
			if waitErr != nil {
				log.Printf("delegate: terminal supervisor cleanup deferred: %v", errors.Join(err, waitErr))
				return
			}
			if resume {
				// Pending completion is visible before a retained election lock is
				// necessarily released. Re-enter only after observing that release,
				// otherwise this owner can mistake itself for a contender and leave
				// its durable wake unattempted.
				continue
			}
		}
		if err != nil {
			finishOnReturn = !retainPrivateQueueWorkUntilPending(err, finish)
			log.Printf("delegate: terminal supervisor cleanup deferred: %v", err)
		}
		return
	}
}

func (run privateQueueMaintenanceRun) attempt(ctx context.Context) (bool, error) {
	if run.scheduled {
		if run.private {
			owned, err := runElectedPrivateQueueMaintenance(ctx, run.stateRoot, func() error { return run.maintain(ctx) })
			return err == nil && !owned, err
		}
		return false, run.maintain(ctx)
	}
	if !run.private {
		return false, run.maintain(ctx)
	}
	acquired, err := tryWithPrivateQueueMaintenance(ctx, run.stateRoot, func() error { return run.maintain(ctx) })
	return err == nil && !acquired, err
}

func (run privateQueueMaintenanceRun) maintain(ctx context.Context) error {
	maintenanceContext, cancel := run.maintenanceContext(ctx)
	defer cancel()
	return run.worker.withPrivateSupervisorLock(maintenanceContext, run.maintainLocked, run.acquireGenerationLock)
}

func (run privateQueueMaintenanceRun) maintenanceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if run.scheduled {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, run.passTimeout)
}

func (run privateQueueMaintenanceRun) acquireGenerationLock(ctx context.Context, root string, callback func(context.Context) error) error {
	return withPublishedPrivateLockContext(ctx, root, "bootstrap.lock", run.lockMode, callback)
}

func (run privateQueueMaintenanceRun) maintainLocked(ctx context.Context) error {
	complete, err := PrivateSupervisorMigrationComplete(run.stateRoot, run.worker.binding)
	if err != nil {
		return err
	}
	if !complete {
		return ErrPrivateSupervisorMigrationRequired
	}
	for {
		pending, err := run.maintainGeneration(ctx)
		if err != nil || !pending {
			return err
		}
		// Requests published after this pass's target need a fresh queue
		// observation. Give the completion wave one more bounded quiet window so
		// the next pass captures the latest generation instead of scanning once
		// per callback in a burst.
		if err = settlePrivateQueueMaintenanceWake(ctx, run.stateRoot); err != nil {
			return err
		}
	}
}

func (run privateQueueMaintenanceRun) maintainGeneration(ctx context.Context) (bool, error) {
	target, full, needed, err := beginPrivateQueueMaintenancePass(ctx, run.stateRoot, !run.scheduled)
	if err != nil || !needed {
		return false, err
	}
	cleanErr := run.worker.cleanSuccessfulTasks(ctx, !full)
	if len(pendingHandles(cleanErr)) != 0 {
		// An observer deadline does not settle the pass: the command still owns
		// its result and may still mutate the queue. Keep this generation
		// unattempted so the scheduled owner resumes it after natural completion.
		return false, cleanErr
	}
	pending, stateErr := finishPrivateQueueMaintenancePass(ctx, run.stateRoot, target, full, cleanErr == nil)
	if stateErr != nil {
		return false, errors.Join(cleanErr, stateErr)
	}
	if cleanErr != nil {
		return false, cleanErr
	}
	return pending, nil
}

func waitPrivateQueueMaintenancePending(ctx context.Context, stateRoot string, private bool, err error) (bool, error) {
	pending := pendingHandles(err)
	if len(pending) == 0 {
		return false, nil
	}
	for _, handle := range pending {
		select {
		case <-handle.Done():
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if private {
		if waitErr := WaitForPrivateQueueMaintenanceIdle(ctx, stateRoot); waitErr != nil {
			return false, waitErr
		}
	}
	return true, nil
}

func retainPrivateQueueWorkUntilPending(err error, finish func()) bool {
	pending := pendingHandles(err)
	if len(pending) == 0 {
		return false
	}
	// The work token was admitted before Close could begin. Transfer it to the
	// sole pending-command waiter so client-owned resources outlive every
	// control process started by this background operation.
	go func() {
		for _, handle := range pending {
			<-handle.Done()
		}
		finish()
	}()
	return true
}

func privateQueueMaintenanceTimeout(observationTimeout time.Duration) time.Duration {
	if observationTimeout <= 0 || observationTimeout > DefaultObservationTimeout {
		observationTimeout = DefaultObservationTimeout
	}
	return observationTimeout * privateQueueMaintenanceObservationPhases
}

// SchedulePrivateQueueMaintenance admits one bounded maintenance attempt under
// the client's lifecycle owner. Close waits for admitted work, so maintenance
// cannot outlive the state and configuration resources used by the client.
func (c *Client) SchedulePrivateQueueMaintenance(ctx context.Context) bool {
	if ctx == nil || c == nil || !isPrivateClient(c) || c.options.SuccessfulCleanupAuthorizer == nil {
		return false
	}
	stateRoot, _ := privateStateRoot(c.binding.ConfigPath)
	if _, err := requestPrivateQueueMaintenance(ctx, stateRoot); err != nil {
		log.Printf("delegate: terminal supervisor cleanup request deferred: %v", err)
		return false
	}
	return c.ScheduleRequestedPrivateQueueMaintenance(ctx)
}

// ScheduleRequestedPrivateQueueMaintenance starts a best-effort owner for a
// request already persisted by the terminal callback. It never consumes or
// duplicates that request when binding the active supervisor fails.
func (c *Client) ScheduleRequestedPrivateQueueMaintenance(ctx context.Context) bool {
	if ctx == nil || c == nil || !isPrivateClient(c) || c.options.SuccessfulCleanupAuthorizer == nil {
		return false
	}
	if c.options.PrivateMaintenanceLauncher != nil {
		stateRoot, _ := privateStateRoot(c.binding.ConfigPath)
		// The wake is already durable. Launch has its own bounded lifetime so an
		// inspection poll deadline cannot revoke the maintenance handoff after
		// publication succeeded.
		launchContext, cancelLaunch := context.WithTimeout(context.WithoutCancel(ctx), DefaultObservationTimeout)
		err := c.options.PrivateMaintenanceLauncher(launchContext, c.options.ControlOwnerExecutable, stateRoot)
		cancelLaunch()
		if err != nil {
			log.Printf("delegate: terminal supervisor cleanup owner deferred: %v", err)
			return false
		}
		return true
	}
	finish, err := c.beginCommandWork()
	if err != nil {
		return false
	}
	maintenanceContext := context.WithoutCancel(ctx)
	go c.runPrivateQueueMaintenance(maintenanceContext, finish, true)
	return true
}

// RunRequestedPrivateQueueMaintenance owns already-persisted maintenance in
// the current process. delegate-run uses it after becoming the independent
// process owner; focused package tests use it through the scheduler above.
func (c *Client) RunRequestedPrivateQueueMaintenance(ctx context.Context) bool {
	if ctx == nil || c == nil || !isPrivateClient(c) || c.options.SuccessfulCleanupAuthorizer == nil {
		return false
	}
	finish, err := c.beginCommandWork()
	if err != nil {
		return false
	}
	c.runPrivateQueueMaintenance(context.WithoutCancel(ctx), finish, true)
	return true
}

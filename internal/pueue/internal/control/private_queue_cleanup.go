package control

import (
	"context"
	"errors"
	"fmt"
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

// privateClientReadySnapshotWithCleanup projects the endpoint observation onto
// the existing observer contract: absent is (false, nil) and a refusal is its
// typed ErrBinding error.
func privateClientReadySnapshotWithCleanup(ctx context.Context, client *Client, cleanupSuccessful bool) (QueueSnapshot, bool, error) {
	snapshot, observation, err := privateClientObserveSnapshot(ctx, client, cleanupSuccessful)
	if err != nil {
		return QueueSnapshot{}, false, err
	}
	return snapshot, observation.ready(), observation.projectedError()
}

func privateClientObserveSnapshot(ctx context.Context, client *Client, cleanupSuccessful bool) (QueueSnapshot, PrivateEndpointObservation, error) {
	if client == nil {
		return QueueSnapshot{}, PrivateEndpointObservation{}, ErrConfiguration
	}
	cleaned := false
	for {
		result, err := privateReadyAttempt(ctx, client)
		if err != nil {
			if observation, handled, endpointErr := privateReadyEndpointFailure(ctx, client, result, err); handled {
				return QueueSnapshot{}, observation, endpointErr
			}
			if retry, cleanErr := client.cleanupStatusOverflow(ctx, err, cleanupSuccessful, cleaned); cleanErr != nil {
				return QueueSnapshot{}, PrivateEndpointObservation{}, errors.Join(err, cleanErr)
			} else if retry {
				cleaned = true
				continue
			}
			return QueueSnapshot{}, PrivateEndpointObservation{}, errors.Join(ErrBinding, err)
		}
		if err = client.validateReadyResult(result); err != nil {
			return QueueSnapshot{}, PrivateEndpointObservation{}, errors.Join(ErrBinding, err)
		}
		snapshot, err := snapshotFromResult(result, client.runtimeBinding.ObservedVersion)
		if err != nil {
			return QueueSnapshot{}, PrivateEndpointObservation{}, errors.Join(ErrBinding, err)
		}
		return snapshot, PrivateEndpointObservation{state: PrivateEndpointReady}, nil
	}
}

func privateReadyAttempt(ctx context.Context, client *Client) (CommandResult, error) {
	result, err := client.readyCommand(ctx)
	if pending := pendingFrom(err); pending != nil {
		return awaitReadyPending(ctx, client, pending)
	}
	return result, err
}

// privateClientObserveGroups verifies the private endpoint through Pueue's
// bounded group response. Lifecycle admission needs only daemon liveness and
// group scheduling facts; it must not deserialize every task row while a large
// private queue is active.
func privateClientObserveGroups(ctx context.Context, client *Client) (map[string]Group, PrivateEndpointObservation, error) {
	if client == nil {
		return nil, PrivateEndpointObservation{}, ErrConfiguration
	}
	groups, result, err := client.privateGroups(ctx)
	if err == nil {
		return groups, PrivateEndpointObservation{state: PrivateEndpointReady}, nil
	}
	if pendingFrom(err) != nil {
		return nil, PrivateEndpointObservation{}, err
	}
	if observation, handled, endpointErr := privateReadyEndpointFailure(ctx, client, result, err); handled {
		return nil, observation, endpointErr
	}
	return nil, PrivateEndpointObservation{}, errors.Join(ErrBinding, err)
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

// privateReadyEndpointFailure is the only producer of absent and refused
// observations. A nonzero liveness command is absent or refused only when it
// is a clean ordinary completion and the exact Unix socket proves that state.
// Any other command, capture, parse, or probe fault stays an operational error
// even when the socket is absent, and a live endpoint with an incompatible
// response is never mistaken for a stopped daemon.
func privateReadyEndpointFailure(ctx context.Context, client *Client, result CommandResult, err error) (observation PrivateEndpointObservation, handled bool, resultErr error) {
	if !result.Started || result.ExitCode == 0 {
		return PrivateEndpointObservation{}, false, nil
	}
	unavailable, refused, probeErr := client.endpointProbe(ctx)
	if probeErr != nil {
		return PrivateEndpointObservation{}, true, errors.Join(err, probeErr)
	}
	if (unavailable || refused) && privateRefusalCompletion(result, err) {
		state := PrivateEndpointRefused
		if unavailable {
			state = PrivateEndpointAbsent
		}
		return PrivateEndpointObservation{state: state, cause: err}, true, nil
	}
	return PrivateEndpointObservation{}, true, errors.Join(ErrBinding, err)
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

// privateCleanupCapture is one identity-protected cleanup observation: the
// queue snapshot that removal must later revalidate and the terminal
// candidates whose evidence was prepared from that same snapshot.
type privateCleanupCapture struct {
	snapshot   QueueSnapshot
	candidates []Job
}

func (c *Client) cleanSuccessfulTasksPass(ctx context.Context, eventOnly bool) (bool, error) {
	capture, err := c.capturePrivateCleanup(ctx, eventOnly)
	if err != nil {
		return false, err
	}
	return c.removeCapturedPrivateCleanup(ctx, capture)
}

// removeCapturedPrivateCleanup asks the application to authorize one capture
// and removes only the exact rows that a fresh identity-protected snapshot
// still shows unchanged. It reports whether the plan asked to settle.
func (c *Client) removeCapturedPrivateCleanup(ctx context.Context, capture privateCleanupCapture) (bool, error) {
	plan, err := c.authorizePrivateCleanupCapture(ctx, capture)
	if err != nil {
		return false, err
	}
	if len(plan.RemoveIDs) != 0 {
		err = c.withPrivateQueueIdentityMutation(ctx, func(lockedContext context.Context) error {
			current, snapshotErr := c.privateCleanupSnapshot(lockedContext)
			if snapshotErr != nil {
				return snapshotErr
			}
			if validateAuthorizedTerminalRows(capture.snapshot, current, plan.RemoveIDs) != nil {
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

// capturePrivateCleanup takes one queue snapshot and prepares the evidence of
// its terminal candidates while queue identity protects that snapshot, so
// worker observations are published only from rows that are still current.
func (c *Client) capturePrivateCleanup(ctx context.Context, eventOnly bool) (privateCleanupCapture, error) {
	if c == nil {
		return privateCleanupCapture{}, ErrConfiguration
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return privateCleanupCapture{}, ErrConfiguration
	}
	if c.options.SuccessfulCleanupAuthorizer == nil {
		return privateCleanupCapture{}, ErrCleanupAuthorization
	}
	var capture privateCleanupCapture
	err := c.withPrivateQueueIdentityMutation(ctx, func(lockedContext context.Context) error {
		var readErr error
		capture.snapshot, readErr = c.privateCleanupSnapshot(lockedContext)
		if readErr != nil {
			return readErr
		}
		capture.candidates, readErr = c.privateTerminalEventJobs(lockedContext, stateRoot, cloneQueueSnapshot(capture.snapshot).Jobs, eventOnly)
		if readErr != nil || c.options.PrivateTerminalEvidencePreparer == nil {
			return readErr
		}
		return c.options.PrivateTerminalEvidencePreparer(lockedContext, stateRoot, c.binding, capture.candidates)
	})
	if err != nil {
		return privateCleanupCapture{}, err
	}
	return capture, nil
}

// authorizePrivateCleanupCapture asks the application-owned policy to validate
// the captured terminal rows. Authorization is read-only and runs outside the
// queue identity mutation lease.
func (c *Client) authorizePrivateCleanupCapture(ctx context.Context, capture privateCleanupCapture) (PrivateQueueCleanupPlan, error) {
	if c == nil {
		return PrivateQueueCleanupPlan{}, ErrConfiguration
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return PrivateQueueCleanupPlan{}, ErrConfiguration
	}
	if c.options.SuccessfulCleanupAuthorizer == nil {
		return PrivateQueueCleanupPlan{}, ErrCleanupAuthorization
	}
	return c.options.SuccessfulCleanupAuthorizer(ctx, stateRoot, c.binding, capture.candidates)
}

// releasePrivateTerminalCustody hands one capture to the application custody
// owner. Its caller holds neither the generation lease nor queue identity.
func (c *Client) releasePrivateTerminalCustody(ctx context.Context, capture privateCleanupCapture, full bool) error {
	if c == nil {
		return ErrConfiguration
	}
	if c.options.PrivateTerminalCustodyReleaser == nil || (!full && len(capture.candidates) == 0) {
		return nil
	}
	stateRoot, private := privateStateRoot(c.binding.ConfigPath)
	if !private {
		return ErrConfiguration
	}
	// The consumer gets its own deep copy, so it cannot rewrite the capture
	// that authorization and removal revalidation consume afterwards.
	candidates := cloneQueueSnapshot(QueueSnapshot{Jobs: capture.candidates}).Jobs
	scope := PrivateTerminalCustodyScope{Candidates: candidates, Full: full}
	if err := c.options.PrivateTerminalCustodyReleaser(ctx, stateRoot, c.binding, scope); err != nil {
		return fmt.Errorf("private terminal custody release: %w", err)
	}
	return nil
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
	for {
		pending, err := run.maintainGeneration(maintenanceContext)
		if err != nil || !pending {
			return err
		}
		// Requests published after this pass's target need a fresh queue
		// observation. Give the completion wave one more bounded quiet window so
		// the next pass captures the latest generation instead of scanning once
		// per callback in a burst. The window holds no generation lease, so a
		// waiting lifecycle transition can enter between generations.
		if err = settlePrivateQueueMaintenanceWake(maintenanceContext, run.stateRoot); err != nil {
			return err
		}
	}
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

// withGeneration runs one protected phase of a pass under the shared
// generation lease and revalidates the canonical generation inside it. A pass
// releases the lease between phases, so each phase repeats this check. entered
// is false only when a non-blocking pass found a lifecycle transition active.
func (run privateQueueMaintenanceRun) withGeneration(ctx context.Context, phase func(context.Context) error) (entered bool, resultErr error) {
	resultErr = run.worker.withPrivateSupervisorLock(ctx, func(lockedContext context.Context) error {
		entered = true
		complete, err := PrivateSupervisorMigrationComplete(run.stateRoot, run.worker.binding)
		if err != nil {
			return err
		}
		if !complete {
			return ErrPrivateSupervisorMigrationRequired
		}
		return phase(lockedContext)
	}, run.acquireGenerationLock)
	return entered, resultErr
}

// withBegunGeneration is withGeneration for a later phase of a begun pass.
// That pass must settle, so a skipped non-blocking acquisition is a fault
// rather than a silent success.
func (run privateQueueMaintenanceRun) withBegunGeneration(ctx context.Context, phase func(context.Context) error) error {
	entered, err := run.withGeneration(ctx, phase)
	if err == nil && !entered {
		return errSupervisorLockBusy
	}
	return err
}

// maintainGeneration begins a pass under one generation lease. A refused,
// skipped, or unneeded generation stays unattempted; a begun pass settles
// unless a command is still pending.
func (run privateQueueMaintenanceRun) maintainGeneration(ctx context.Context) (bool, error) {
	var target uint64
	var full, begun bool
	_, beginErr := run.withGeneration(ctx, func(lockedContext context.Context) error {
		var err error
		target, full, begun, err = beginPrivateQueueMaintenancePass(lockedContext, run.stateRoot, !run.scheduled)
		return err
	})
	if beginErr != nil || !begun {
		return false, beginErr
	}
	cleanErr := run.cleanBegunGeneration(ctx, full)
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

// cleanBegunGeneration runs the phases of one begun pass and joins their
// faults. A full-recovery generation first releases custody that needs no
// queue observation, with neither lease held and before the endpoint is
// verified, so a stopped supervisor cannot strand closed custody. The queue
// capture then prepares evidence under the protected phase; a failed capture
// hands no candidate to custody and publishes no evidence.
func (run privateQueueMaintenanceRun) cleanBegunGeneration(ctx context.Context, full bool) error {
	var custodyErr error
	if full {
		custodyErr = run.worker.releasePrivateTerminalCustody(ctx, privateCleanupCapture{}, true)
	}
	var capture privateCleanupCapture
	err := run.withBegunGeneration(ctx, func(lockedContext context.Context) error {
		var captureErr error
		capture, captureErr = run.worker.capturePrivateCleanup(lockedContext, !full)
		return captureErr
	})
	if err == nil {
		err = run.cleanCapturedGeneration(ctx, capture, !full)
	}
	return errors.Join(custodyErr, err)
}

// cleanCapturedGeneration finishes the queue work of a begun pass. Custody of
// the captured candidates runs with neither the generation lease nor queue
// identity; removal then reacquires both and revalidates the capture, so a
// custody fault never withholds a row that the application still authorizes.
// A settling plan recaptures the same generation after a quiet poll that also
// holds neither lease.
func (run privateQueueMaintenanceRun) cleanCapturedGeneration(ctx context.Context, capture privateCleanupCapture, eventOnly bool) error {
	for {
		custodyErr := run.worker.releasePrivateTerminalCustody(ctx, capture, false)
		settling := false
		err := run.withBegunGeneration(ctx, func(lockedContext context.Context) error {
			var removeErr error
			settling, removeErr = run.worker.removeCapturedPrivateCleanup(lockedContext, capture)
			return removeErr
		})
		if err = errors.Join(custodyErr, err); err != nil || !settling {
			return err
		}
		if err = waitPrivateQueueSettlement(ctx); err != nil {
			return err
		}
		err = run.withBegunGeneration(ctx, func(lockedContext context.Context) error {
			var captureErr error
			capture, captureErr = run.worker.capturePrivateCleanup(lockedContext, eventOnly)
			return captureErr
		})
		if err != nil {
			return err
		}
	}
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
		environment := c.options.PrivateMaintenanceEnvironment
		if environment == nil {
			environment = defaultEnvironment()
		}
		err := c.options.PrivateMaintenanceLauncher(launchContext, c.options.ControlOwnerExecutable, stateRoot, append([]string{}, environment...))
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

package execution

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type invocation struct {
	interaction         *interactiveInvocation
	td                  *taskdir.TaskDir
	cmd                 *exec.Cmd
	stdin               *os.File
	stdout              *capture
	stderr              *capture
	launchFilesPrepared bool
	launchDeclaration   []string
	launchArguments     []string
	inherited           []string
	budget              *budgetOwner
	started             time.Time
	wait                chan error
}

// Run consumes one real permit, owns all capture/Wait/deadline workers, and
// finalizes under the same runner lease. The caller retains task/store handles
// until Run returns. Setup errors preserve the start guard without a new Start.
func Run(td *taskdir.TaskDir, permit *taskdir.StartPermit, plan Plan, opts Options) (result Result) {
	inherited := opts.inheritedEnvironment()
	if permit != nil {
		defer func() { result.Error = errors.Join(result.Error, permit.Release()) }()
	}
	allowRuntimeRefresh := opts.PreflightPlan != nil
	if err := validatePlan(td, permit, plan, allowRuntimeRefresh); err != nil {
		result.Error = err
		return result
	}
	if err := permit.Consume(); err != nil {
		result.Error = err
		return result
	}
	opts.emit("start-permit-consumed")
	i, err := prepareInvocation(td)
	if err != nil {
		result.Error = err
		return result
	}
	i.inherited = inherited
	request, _, err := td.PreparedRecords()
	if err != nil {
		result.Error = errors.Join(err, i.discard())
		return result
	}
	if err = validateProtocolOptions(request.RequestedConfig.Interactive, opts); err != nil {
		result.Error = errors.Join(err, i.discard())
		return result
	}
	if protocol, user := opts.selectedProtocol(); protocol != nil {
		opts.Identity, err = i.prepareInteraction(protocol, user)
		if err != nil {
			result.Error = errors.Join(err, i.discard())
			return result
		}
	}
	return runPreparedInvocation(i, request.BudgetNanos, plan, opts, allowRuntimeRefresh)
}

func runPreparedInvocation(i *invocation, budgetNanos int64, plan Plan, opts Options, allowRuntimeRefresh bool) (result Result) {
	go i.stdout.run(opts)
	go i.stderr.run(opts)
	launchPlan, startErr := prepareLaunchPlan(i, budgetNanos, plan, opts, allowRuntimeRefresh)
	if startErr == nil {
		startErr = i.start(opts)
	} else {
		opts.emit("start-failed")
	}
	return finishInvocation(i, launchPlan, startErr, opts)
}

// prepareLaunchPlan runs the fresh preflight inside the armed budget and then
// constructs the one unstarted provider command from the plan it returns.
// Nothing is bound before preflight, so a provider update between admission
// and launch is resolved by the preflight rather than by a stale binding.
func prepareLaunchPlan(i *invocation, budgetNanos int64, plan Plan, opts Options, allowRuntimeRefresh bool) (Plan, error) {
	launchPlan := plan
	_, startErr := i.prepareLaunchFiles(plan)
	clock := opts.clock()
	i.started = clock.Now()
	deadline := i.started.Add(time.Duration(budgetNanos))
	if !opts.Deadline.IsZero() {
		if opts.Deadline.After(deadline) {
			// Keep the admitted deadline and arm its owner before finalizing
			// the refusal; every finalization path requires that owner.
			startErr = errors.Join(startErr, task.ErrIdentityMismatch)
		} else {
			deadline = opts.Deadline
		}
	}
	i.budget = armBudgetUntil(clock, deadline, opts)
	opts.emit("start-entry")
	if startErr != nil {
		return launchPlan, startErr
	}
	refreshedPlan, preflightErr := opts.preflight(i.budget, plan)
	if preflightErr != nil {
		return launchPlan, preflightErr
	}
	if err := validatePreflightPlan(plan, refreshedPlan); err != nil {
		return launchPlan, err
	}
	if err := i.prepareCommand(refreshedPlan, allowRuntimeRefresh); err != nil {
		return launchPlan, err
	}
	// The constructed command makes the refreshed plan authoritative. A
	// refusal keeps the admitted plan available for sealing.
	return refreshedPlan, nil
}

func finishInvocation(i *invocation, plan Plan, startErr error, opts Options) (result Result) {
	parentErr := i.closeParents(startErr, opts)
	if startErr == nil {
		result.ReceiptError = recordStart(i.td, i, plan, opts)
	}
	if i.interaction != nil {
		i.interaction.start(i.budget, opts.clock(), startErr)
	}
	result.Error = errors.Join(parentErr, i.finishCaptures(startErr, opts))
	if i.interaction == nil {
		i.budget.complete(opts)
	}
	if opts.Identity != nil {
		result.ReceiptError = errors.Join(result.ReceiptError, opts.Identity.Complete())
	}
	result.Error = errors.Join(result.Error, i.stdout.closeObserver())
	i.budget.complete(opts)
	if opts.Lifetime != nil {
		// A lifetime-backed budget stop must finish its termination handshake
		// before CloseTask can close the same private control channel.
		result.StopError = i.budget.await()
		result.Error = errors.Join(result.Error, opts.Lifetime.CloseTask(context.Background()))
	}
	if i.interaction.userChannel() {
		result.Error = errors.Join(result.Error, i.td.SealInteraction())
	}
	if result.Error == nil {
		result.Outcome, result.CleanupError, result.Error = sealAndPublish(i.td, i, plan, startErr, opts)
	}
	if opts.Lifetime == nil {
		// Historical supervisor-only stopping retains its independent observation
		// path and does not delay publication.
		result.StopError = i.budget.await()
	}
	return result
}

func validatePlan(td *taskdir.TaskDir, permit *taskdir.StartPermit, p Plan, allowRuntimeRefresh bool) error {
	if td == nil || permit == nil || permit.TaskID() != td.TaskID {
		return task.ErrInvalidPermit
	}
	if !filepath.IsAbs(p.Executable) || !filepath.IsAbs(p.Directory) {
		return errors.New("launch executable and cwd must be absolute")
	}
	if err := validatePlanRuntimeIdentity(p); err != nil {
		return err
	}
	req, meta, err := td.PreparedRecords()
	if err != nil {
		return err
	}
	// The caller cannot bypass a pending admission by clearing its plan marker.
	if p.RuntimePreflightSHA256 != meta.RuntimePreflightSHA256 {
		return task.ErrIdentityMismatch
	}
	guard, err := permit.Record()
	if err != nil {
		return err
	}
	if guard.RootID != meta.RootID || guard.TaskID != td.TaskID || guard.SpecSHA256 != meta.SpecSHA256 || guard.BudgetNanos != req.BudgetNanos {
		return task.ErrInvalidPermit
	}
	return matchLaunchPlan(p, req, meta, allowRuntimeRefresh)
}

func matchLaunchPlan(p Plan, req *task.TaskRecord, meta *task.MetaRecord, allowRuntimeRefresh bool) error {
	if !task.CompareInputFiles(p.InputFiles, meta.InputFiles) || !task.CompareOutputArtifacts(p.OutputArtifacts, meta.OutputArtifacts) || p.OutputWriterContract != meta.OutputWriterContract {
		return task.ErrIdentityMismatch
	}
	if (!allowRuntimeRefresh && p.Executable != meta.ProviderExecutable) || p.Directory != req.CanonicalCwd || !p.Predicate.Equal(meta.Predicate) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func prepareInvocation(td *taskdir.TaskDir) (*invocation, error) {
	stdin, err := td.OpenBriefForExecution()
	if err != nil {
		return nil, err
	}
	stdout, err := newCapture(td, "stdout")
	if err != nil {
		return nil, errors.Join(err, stdin.Close())
	}
	stderr, err := newCapture(td, "stderr")
	if err != nil {
		return nil, errors.Join(err, stdin.Close(), stdout.discard())
	}
	return &invocation{td: td, stdin: stdin, stdout: stdout, stderr: stderr, wait: make(chan error, 1)}, nil
}

func (i *invocation) discard() error {
	return errors.Join(i.stdin.Close(), i.stdout.discard(), i.stderr.discard())
}

// prepareCommand constructs the one unstarted provider command for the plan
// returned by fresh preflight. The installed executable runs directly at its
// resolved absolute path; the OS performs any shebang handling. There is no
// copy, view, byte verification, or fallback.
func (i *invocation) prepareCommand(plan Plan, allowRuntimeRefresh bool) error {
	request, meta, err := i.td.PreparedRecords()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(plan.Executable) || !filepath.IsAbs(plan.Directory) {
		return errors.New("launch executable and cwd must be absolute")
	}
	if err = validatePlanRuntimeIdentity(plan); err != nil {
		return err
	}
	if err = matchLaunchPlan(plan, request, meta, allowRuntimeRefresh); err != nil {
		return err
	}
	arguments, err := i.launchArgumentsFor(plan)
	if err != nil {
		return err
	}
	cmd := exec.Command(plan.Executable, arguments...)
	cmd.Dir, cmd.Env = plan.Directory, i.launchEnvironment(plan)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = i.stdin, i.stdout.writer, i.stderr.writer
	i.cmd = cmd
	return nil
}

// launchEnvironment is the single environment of the child. An explicit plan environment wins, including
// a non-nil empty one, and is never extended. A nil plan environment selects
// the snapshot captured at Run entry plus the host's implicit working
// directory entries, so the launch matches the historical implicit inheritance
// of exec.Cmd. The result is never nil.
func (i *invocation) launchEnvironment(plan Plan) []string {
	if plan.Environment != nil {
		return append([]string{}, plan.Environment...)
	}
	return InheritedCommandEnvironment(i.inherited, plan.Directory)
}

func (i *invocation) launchArgumentsFor(plan Plan) ([]string, error) {
	if !i.launchFilesPrepared {
		if _, err := i.prepareLaunchFiles(plan); err != nil {
			return nil, err
		}
	}
	if !slices.Equal(plan.Arguments, i.launchDeclaration) {
		return nil, task.ErrIdentityMismatch
	}
	return append([]string(nil), i.launchArguments...), nil
}

func validatePlanRuntimeIdentity(plan Plan) error {
	identity := plan.ProviderRuntime
	if plan.RuntimePreflightSHA256 != "" && identity != (task.ProviderRuntimeIdentity{}) {
		return task.ErrIdentityMismatch
	}
	if err := task.ValidateProviderRuntimeIdentity(identity); err != nil {
		return task.ErrIdentityMismatch
	}
	if identity.Executable == "" {
		return nil
	}
	if identity.Executable != plan.Executable || identity.SHA256 != plan.ExecutableSHA256 {
		return task.ErrIdentityMismatch
	}
	return nil
}

func validatePlanRuntimeEvidence(plan Plan) error {
	if err := task.ValidateProviderRuntimeEvidenceIdentity(plan.ProviderRuntime); err != nil {
		return task.ErrIdentityMismatch
	}
	return validatePlanRuntimeIdentity(plan)
}

func validatePreflightPlan(admitted, refreshed Plan) error {
	if refreshed.RuntimePreflightSHA256 != "" {
		return fmt.Errorf("%w: required runtime preflight is still pending", task.ErrIdentityMismatch)
	}
	if admitted.RuntimePreflightSHA256 != "" && refreshed.ProviderRuntime.Executable == "" {
		return fmt.Errorf("%w: runtime preflight did not return an observed provider identity", task.ErrIdentityMismatch)
	}
	return validatePlanRuntimeEvidence(refreshed)
}

func (i *invocation) prepareLaunchFiles(plan Plan) ([]string, error) {
	if i.launchFilesPrepared {
		return nil, task.ErrEvidenceFault
	}
	i.launchFilesPrepared = true
	i.launchDeclaration = slices.Clone(plan.Arguments)
	arguments, err := i.td.PrepareLaunchFiles(plan.Arguments)
	if err != nil {
		return nil, err
	}
	i.launchArguments = append([]string(nil), arguments...)
	return arguments, nil
}

func (i *invocation) start(opts Options) error {
	start := func() error { return opts.start(i.cmd) }
	if opts.Lifetime != nil {
		start = func() error { return opts.Lifetime.StartCommand(i.cmd, func() error { return opts.start(i.cmd) }) }
	}
	err := i.budget.authorizeAndStart(opts, start)
	if err == nil {
		opts.emit("started")
		go func() {
			waitErr := opts.wait(i.cmd)
			opts.emit("wait-completed")
			i.wait <- waitErr
		}()
	} else {
		opts.emit("start-failed")
	}
	return err
}

func recordStart(td *taskdir.TaskDir, i *invocation, plan Plan, opts Options) error {
	now := opts.clock().Now()
	err := td.RecordStartedWithIdentity(now.Sub(i.started).Nanoseconds(), plan.ProviderRuntime)
	if err == nil {
		opts.emit("started-receipt")
	}
	return err
}

func (i *invocation) closeParents(startErr error, opts Options) error {
	var diagnosticErr error
	if startErr != nil {
		_, diagnosticErr = fmt.Fprintln(i.stderr.writer, startErr.Error())
	}
	parentErr := errors.Join(i.stdin.Close(), i.stdout.writer.Close(), i.stderr.writer.Close())
	opts.emit("parent-fds-closed")
	return errors.Join(diagnosticErr, parentErr)
}

func (i *invocation) finishCaptures(startErr error, opts Options) error {
	var waitErr error
	if startErr == nil {
		waitErr = <-i.wait
	}
	var lifetimeErr error
	if startErr == nil && opts.Lifetime != nil {
		lifetimeErr = opts.Lifetime.FinishCommand(context.Background(), i.cmd)
	}
	stdoutErr, stderrErr := <-i.stdout.done, <-i.stderr.done
	return errors.Join(observedWaitError(waitErr), lifetimeErr, stdoutErr, stderrErr)
}

func observedWaitError(err error) error {
	if err == nil {
		return nil
	}
	// A joined infrastructure fault must not be hidden by an ExitError elsewhere
	// in the error tree. exec.Cmd.Wait's ordinary nonzero exit is a single chain.
	for current := err; current != nil; current = errors.Unwrap(current) {
		if _, multiple := current.(interface{ Unwrap() []error }); multiple {
			return err
		}
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) && exited.ProcessState != nil {
		return nil
	}
	return err
}

func sealAndPublish(td *taskdir.TaskDir, i *invocation, plan Plan, startErr error, opts Options) (*task.OutcomeRecord, error, error) {
	invocationState, exitCode, diagnostic := task.InvocationStarted, 0, ""
	if i.interaction != nil && opts.Identity != nil {
		diagnostic = i.interaction.completionDiagnostic()
	}
	if startErr != nil {
		invocationState, exitCode, diagnostic = task.InvocationStartFailed, 1, startErr.Error()
	} else if i.cmd.ProcessState != nil {
		exitCode = i.cmd.ProcessState.ExitCode()
	} else {
		return nil, nil, errors.New("successful Start has no observed process state")
	}
	if err := td.ImportOutputArtifacts(); err != nil {
		return nil, nil, err
	}
	identity := plan.ProviderRuntime
	if startErr != nil {
		// A failed preflight, command construction, or Start has no confirmed
		// provider launch.
		// Do not carry an untrusted historical identity into terminal evidence.
		identity = task.ProviderRuntimeIdentity{}
	}
	if _, err := td.SealWithIdentity(invocationState, exitCode, diagnostic, plan.Predicate, identity); err != nil {
		return nil, nil, err
	}
	opts.emit("sealed")
	out, cleanup, err := td.Finalize(plan.Predicate)
	if out != nil {
		opts.emit("published")
	}
	return out, cleanup, err
}

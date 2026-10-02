package execution

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
	"github.com/hishamkaram/delegation-layer/internal/verifiedexec"
)

type invocation struct {
	td                  *taskdir.TaskDir
	cmd                 *exec.Cmd
	stdin               *os.File
	stdout              *capture
	stderr              *capture
	verified            *verifiedexec.Command
	launchFilesPrepared bool
	launchDeclaration   []string
	launchArguments     []string
	boundPlan           *Plan
	budget              *budgetOwner
	started             time.Time
	wait                chan error
}

// Run consumes one real permit, owns all capture/Wait/deadline workers, and
// finalizes under the same runner lease. The caller retains task/store handles
// until Run returns. Setup errors preserve the start guard without a new Start.
func Run(td *taskdir.TaskDir, permit *taskdir.StartPermit, plan Plan, opts Options) (result Result) {
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
	request, _, err := td.PreparedRecords()
	if err != nil {
		result.Error = errors.Join(err, i.discard())
		return result
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

func prepareLaunchPlan(i *invocation, budgetNanos int64, plan Plan, opts Options, allowRuntimeRefresh bool) (Plan, error) {
	launchPlan := plan
	_, startErr := i.prepareLaunchFiles(plan)
	if startErr == nil {
		deferred, bindErr := i.bindInitial(plan, allowRuntimeRefresh)
		if bindErr != nil {
			startErr = bindErr
		} else if deferred {
			// The historical provider executable is no longer the current
			// candidate. Fresh preflight will select and verify the replacement.
			startErr = nil
		}
	}
	i.budget, i.started = armBudget(opts.clock(), time.Duration(budgetNanos), opts)
	opts.emit("start-entry")
	if startErr != nil {
		return launchPlan, startErr
	}
	refreshedPlan, preflightErr := opts.preflight(i.budget, plan)
	if preflightErr != nil {
		return launchPlan, preflightErr
	}
	if err := validatePlanRuntimeEvidence(refreshedPlan); err != nil {
		return launchPlan, err
	}
	if i.boundPlan == nil || !sameLaunchPlan(*i.boundPlan, refreshedPlan) {
		if err := i.unbind(); err != nil {
			return launchPlan, err
		}
		if err := i.bind(refreshedPlan, allowRuntimeRefresh); err != nil {
			return launchPlan, err
		}
	}
	// A successful bind makes the refreshed plan authoritative. A bind
	// refusal keeps the admitted plan available for sealing.
	return refreshedPlan, nil
}

func finishInvocation(i *invocation, plan Plan, startErr error, opts Options) (result Result) {
	parentErr := i.closeParents(startErr, opts)
	if startErr == nil {
		result.ReceiptError = recordStart(i.td, i, plan, opts)
	}
	result.Error = errors.Join(parentErr, i.finishCaptures(startErr))
	i.budget.complete(opts)
	if opts.Identity != nil {
		result.ReceiptError = errors.Join(result.ReceiptError, opts.Identity.Complete())
	}
	if result.Error == nil {
		result.Outcome, result.CleanupError, result.Error = sealAndPublish(i.td, i, plan, startErr, opts)
	}
	// Publication does not wait for a delayed supervisor reply. The callback's
	// observation context has ended; its independent in-flight client keeps Wait.
	result.StopError = i.budget.await()
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
	guard, err := permit.Record()
	if err != nil {
		return err
	}
	if guard.RootID != meta.RootID || guard.TaskID != td.TaskID || guard.SpecSHA256 != meta.SpecSHA256 || guard.BudgetNanos != req.BudgetNanos {
		return task.ErrInvalidPermit
	}
	if matchErr := matchLaunchPlan(p, req, meta, allowRuntimeRefresh); matchErr != nil {
		return matchErr
	}
	_, err = executableDigest(p, *meta, allowRuntimeRefresh)
	return err
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

// executableDigest selects the identity carried by the launch plan. New
// profiles refresh it during preflight; older records may carry it in
// persisted native policy details. A pathname without a validated digest is
// never launched.
func executableDigest(plan Plan, meta task.MetaRecord, allowRuntimeRefresh bool) (string, error) {
	planDigest := plan.ExecutableSHA256
	if planDigest != "" && task.ValidateSHA256(planDigest) != nil {
		return "", task.ErrIdentityMismatch
	}
	policyDigest := ""
	if meta.EffectiveConfig.Policy != nil {
		policyDigest = meta.EffectiveConfig.Policy.RuntimeSHA256
		if task.ValidateSHA256(policyDigest) != nil {
			return "", task.ErrIdentityMismatch
		}
	}
	if !allowRuntimeRefresh && planDigest != "" && policyDigest != "" && planDigest != policyDigest {
		return "", task.ErrIdentityMismatch
	}
	if planDigest != "" {
		return planDigest, nil
	}
	if policyDigest != "" {
		return policyDigest, nil
	}
	return "", task.ErrIdentityMismatch
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
	var verifiedErr error
	if i.verified != nil {
		verifiedErr = i.verified.Close()
	}
	return errors.Join(i.stdin.Close(), i.stdout.discard(), i.stderr.discard(), verifiedErr)
}

func (i *invocation) bindInitial(plan Plan, allowRuntimeRefresh bool) (bool, error) {
	err := i.bind(plan, allowRuntimeRefresh)
	if err == nil || !allowRuntimeRefresh {
		return false, err
	}
	// Only an executable that no longer matches its recorded bytes may be
	// repaired by the fresh runtime preflight. A valid executable with another
	// bind failure remains a definite setup error.
	_, meta, metaErr := i.td.PreparedRecords()
	if metaErr != nil {
		return false, err
	}
	expected, digestErr := executableDigest(plan, *meta, allowRuntimeRefresh)
	if digestErr == nil {
		observed, fingerprintErr := verifiedexec.Fingerprint(plan.Executable)
		if fingerprintErr == nil && observed == expected {
			return false, err
		}
	}
	return true, nil
}

func (i *invocation) bind(plan Plan, allowRuntimeRefresh bool) error {
	_, meta, err := i.td.PreparedRecords()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(plan.Executable) || !filepath.IsAbs(plan.Directory) {
		return errors.New("launch executable and cwd must be absolute")
	}
	if err = validatePlanRuntimeIdentity(plan); err != nil {
		return err
	}
	request, _, err := i.td.PreparedRecords()
	if err != nil {
		return err
	}
	if err = matchLaunchPlan(plan, request, meta, allowRuntimeRefresh); err != nil {
		return err
	}
	digest, err := executableDigest(plan, *meta, allowRuntimeRefresh)
	if err != nil {
		return err
	}
	arguments, err := i.launchArgumentsFor(plan)
	if err != nil {
		return err
	}
	verified, err := verifiedexec.NewCommandInDirectory(plan.Executable, digest, plan.Directory, plan.Environment, arguments...)
	if err != nil {
		return err
	}
	cmd := verified.Cmd
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = plan.Directory, i.stdin, i.stdout.writer, i.stderr.writer
	if plan.Environment != nil {
		cmd.Env = append([]string{}, plan.Environment...)
	}
	i.cmd, i.verified = cmd, verified
	i.boundPlan = clonePlan(plan)
	return nil
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

func (i *invocation) unbind() error {
	if i.verified == nil {
		i.cmd, i.boundPlan = nil, nil
		return nil
	}
	err := i.verified.Close()
	i.cmd, i.verified, i.boundPlan = nil, nil, nil
	return err
}

func sameLaunchPlan(left, right Plan) bool {
	return left.Executable == right.Executable && left.ExecutableSHA256 == right.ExecutableSHA256 &&
		left.ProviderRuntime == right.ProviderRuntime &&
		left.Directory == right.Directory && sameLaunchEnvironment(left.Environment, right.Environment) &&
		slices.Equal(left.Arguments, right.Arguments) && left.Predicate.Equal(right.Predicate) &&
		task.CompareInputFiles(left.InputFiles, right.InputFiles) &&
		task.CompareOutputArtifacts(left.OutputArtifacts, right.OutputArtifacts) &&
		left.OutputWriterContract == right.OutputWriterContract
}

func sameLaunchEnvironment(left, right []string) bool {
	return (left == nil) == (right == nil) && slices.Equal(left, right)
}

func clonePlan(plan Plan) *Plan {
	clone := plan
	clone.Arguments = slices.Clone(plan.Arguments)
	clone.Environment = slices.Clone(plan.Environment)
	clone.InputFiles = slices.Clone(plan.InputFiles)
	clone.OutputArtifacts = slices.Clone(plan.OutputArtifacts)
	return &clone
}

func validatePlanRuntimeIdentity(plan Plan) error {
	identity := plan.ProviderRuntime
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
	err := i.budget.authorizeAndStart(opts, func() error { return opts.start(i.cmd) })
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
	if startErr != nil {
		if i.verified != nil {
			parentErr = errors.Join(parentErr, i.verified.Close())
		}
	} else if i.verified != nil {
		parentErr = errors.Join(parentErr, i.verified.ReleaseDescriptors())
	}
	opts.emit("parent-fds-closed")
	return errors.Join(diagnosticErr, parentErr)
}

func (i *invocation) finishCaptures(startErr error) error {
	var waitErr error
	if startErr == nil {
		waitErr = <-i.wait
	}
	stdoutErr, stderrErr := <-i.stdout.done, <-i.stderr.done
	var verifiedErr error
	if startErr == nil && i.verified != nil {
		verifiedErr = i.verified.Close()
	}
	return errors.Join(observedWaitError(waitErr), stdoutErr, stderrErr, verifiedErr)
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
		// A failed preflight, bind, or Start has no confirmed provider launch.
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

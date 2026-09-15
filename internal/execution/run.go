package execution

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/task"
	"github.com/hishamkaram/delegation-layer/internal/taskdir"
)

type invocation struct {
	cmd     *exec.Cmd
	stdin   *os.File
	stdout  *capture
	stderr  *capture
	budget  *budgetOwner
	started time.Time
	wait    chan error
}

// Run consumes one real permit, owns all capture/Wait/deadline workers, and
// finalizes under the same runner lease. The caller retains task/store handles
// until Run returns. Setup errors preserve the start guard without a new Start.
func Run(td *taskdir.TaskDir, permit *taskdir.StartPermit, plan Plan, opts Options) (result Result) {
	if permit != nil {
		defer func() { result.Error = errors.Join(result.Error, permit.Release()) }()
	}
	if err := validatePlan(td, permit, plan); err != nil {
		result.Error = err
		return result
	}
	if err := permit.Consume(); err != nil {
		result.Error = err
		return result
	}
	opts.emit("start-permit-consumed")
	i, err := prepareInvocation(td, plan)
	if err != nil {
		result.Error = err
		return result
	}
	request, _, err := td.PreparedRecords()
	if err != nil {
		result.Error = errors.Join(err, i.discard())
		return result
	}
	go i.stdout.run(opts)
	go i.stderr.run(opts)
	i.budget, i.started = armBudget(opts.clock(), time.Duration(request.BudgetNanos), opts)
	startErr := i.start(opts)
	parentErr := i.closeParents(startErr, opts)
	if startErr == nil {
		result.ReceiptError = recordStart(td, i, opts)
	}
	result.Error = errors.Join(parentErr, i.finishCaptures(startErr))
	i.budget.complete(opts)
	if opts.Identity != nil {
		result.ReceiptError = errors.Join(result.ReceiptError, opts.Identity.Complete())
	}
	if result.Error == nil {
		result.Outcome, result.CleanupError, result.Error = sealAndPublish(td, i, plan, startErr, opts)
	}
	// Publication does not wait for a delayed supervisor reply. The callback's
	// observation context has ended; its independent in-flight client keeps Wait.
	result.StopError = i.budget.await()
	return result
}

func validatePlan(td *taskdir.TaskDir, permit *taskdir.StartPermit, p Plan) error {
	if td == nil || permit == nil || permit.TaskID() != td.TaskID {
		return task.ErrInvalidPermit
	}
	if !filepath.IsAbs(p.Executable) || !filepath.IsAbs(p.Directory) {
		return errors.New("launch executable and cwd must be absolute")
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
	return matchLaunchPlan(p, req, meta)
}

func matchLaunchPlan(p Plan, req *task.TaskRecord, meta *task.MetaRecord) error {
	if !task.CompareInputFiles(p.InputFiles, meta.InputFiles) || !task.CompareOutputArtifacts(p.OutputArtifacts, meta.OutputArtifacts) || p.OutputWriterContract != meta.OutputWriterContract {
		return task.ErrIdentityMismatch
	}
	if p.Executable != meta.ProviderExecutable || p.Directory != req.CanonicalCwd || !p.Predicate.Equal(meta.Predicate) {
		return task.ErrIdentityMismatch
	}
	return nil
}

func prepareInvocation(td *taskdir.TaskDir, plan Plan) (*invocation, error) {
	arguments, err := td.PrepareLaunchFiles(plan.Arguments)
	if err != nil {
		return nil, err
	}
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
	cmd := exec.Command(plan.Executable, arguments...)
	cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = plan.Directory, stdin, stdout.writer, stderr.writer
	if plan.Environment != nil {
		cmd.Env = append([]string{}, plan.Environment...)
	}
	return &invocation{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr, wait: make(chan error, 1)}, nil
}

func (i *invocation) discard() error {
	return errors.Join(i.stdin.Close(), i.stdout.discard(), i.stderr.discard())
}

func (i *invocation) start(opts Options) error {
	opts.emit("start-entry")
	err := opts.preflight(i.budget)
	if err == nil {
		err = i.budget.authorizeStart(opts)
	}
	if err == nil {
		err = opts.start(i.cmd)
	}
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

func recordStart(td *taskdir.TaskDir, i *invocation, opts Options) error {
	now := opts.clock().Now()
	err := td.RecordStarted(now.Sub(i.started).Nanoseconds())
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

func (i *invocation) finishCaptures(startErr error) error {
	var waitErr error
	if startErr == nil {
		waitErr = <-i.wait
	}
	stdoutErr, stderrErr := <-i.stdout.done, <-i.stderr.done
	return errors.Join(observedWaitError(waitErr), stdoutErr, stderrErr)
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
	if _, err := td.Seal(invocationState, exitCode, diagnostic, plan.Predicate); err != nil {
		return nil, nil, err
	}
	opts.emit("sealed")
	out, cleanup, err := td.Finalize(plan.Predicate)
	if out != nil {
		opts.emit("published")
	}
	return out, cleanup, err
}

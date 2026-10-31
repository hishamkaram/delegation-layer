package verifiedexec

// Owned worker processes for the custody tests. The test binary re-executes
// itself as a finite worker that has no descendants and uses the production
// Pool. A crash is the worker calling os.Exit at a named pool point; the
// parent's Wait on that exact process is the oracle that it is gone. No signal
// is ever sent.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	exitWorkerFailed = 1
	// exitWorkerCrash is the status of a worker that reached its crash point.
	exitWorkerCrash = 97

	modeObserve      = "observe"
	modeCrashRelease = "crash-release:"
)

func runProofWorker(mode string, args []string) int {
	code, err := dispatchWorker(mode, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "custody worker %s: %v\n", mode, err)
		return exitWorkerFailed
	}
	return code
}

func dispatchWorker(mode string, args []string) (int, error) {
	switch {
	case mode == modeObserve:
		return 0, workerObserve(args)
	case strings.HasPrefix(mode, modeCrashRelease):
		return 0, workerCrash(poolPoint(strings.TrimPrefix(mode, modeCrashRelease)), args)
	default:
		return 0, fmt.Errorf("unknown worker mode %q", mode)
	}
}

// workerTarget names the recorded pool and owner for observation or release.
type workerTarget struct {
	pool *Pool
	ns   Namespace
}

func parseWorkerTarget(args []string) (workerTarget, error) {
	if len(args) != 3 {
		return workerTarget{}, errors.New("usage: poolRoot poolID owner")
	}
	pool, err := NewPoolWithIdentity(args[0], args[1])
	if err != nil {
		return workerTarget{}, err
	}
	ns, err := NewNamespace(args[2])
	if err != nil {
		return workerTarget{}, err
	}
	return workerTarget{pool: pool, ns: ns}, nil
}

func workerObserve(args []string) error {
	target, err := parseWorkerTarget(args)
	if err != nil {
		return err
	}
	_, err = target.pool.ReleasePending(target.ns)
	return err
}

// workerCrash exits during closure-authorized release of a seeded legacy view.
func workerCrash(point poolPoint, args []string) error {
	target, err := parseWorkerTarget(args)
	if err != nil {
		return err
	}
	target.pool.faults.at = func(got poolPoint, _ string) {
		if got == point {
			os.Exit(exitWorkerCrash)
		}
	}
	if err = target.pool.ReleaseOwner(context.Background(), target.ns); err != nil {
		return err
	}
	return fmt.Errorf("the crash point %s never fired", point)
}

// workerProcess is one started worker and its captured output.
type workerProcess struct {
	cmd            *exec.Cmd
	stdout, stderr bytes.Buffer
	reaped         bool
	outcome        workerOutcome
	reapErr        error
}

type workerOutcome struct {
	Exit   int
	Stdout []byte
	Stderr string
}

func startWorker(t *testing.T, mode string, args ...string) *workerProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	worker := &workerProcess{cmd: exec.Command(executable, args...)}
	// A minimal environment: the worker needs no inherited variables.
	worker.cmd.Env = []string{proofWorkerEnv + "=" + mode}
	worker.cmd.Stdout, worker.cmd.Stderr = &worker.stdout, &worker.stderr
	if err = worker.cmd.Start(); err != nil {
		t.Fatalf("start worker %s: %v", mode, err)
	}
	// The worker is finite and has no stdin to close; if the test leaves early,
	// this cleanup still Waits for it exactly once.
	t.Cleanup(func() {
		if _, reapErr := worker.reap(); reapErr != nil {
			t.Errorf("cleanup of worker %s: %v", mode, reapErr)
		}
	})
	return worker
}

// reap Waits for the worker once and later returns the same result. A nonzero
// exit status is an outcome, not an error.
func (w *workerProcess) reap() (workerOutcome, error) {
	if w.reaped {
		return w.outcome, w.reapErr
	}
	w.reaped = true
	err := w.cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
		w.outcome = workerOutcome{Stdout: w.stdout.Bytes(), Stderr: w.stderr.String()}
	case errors.As(err, &exit):
		w.outcome = workerOutcome{Exit: exit.ExitCode(), Stdout: w.stdout.Bytes(), Stderr: w.stderr.String()}
	default:
		w.reapErr = err
	}
	return w.outcome, w.reapErr
}

// wait reports the worker's exit status, failing the test on a Wait error.
func (w *workerProcess) wait(t *testing.T) workerOutcome {
	t.Helper()
	outcome, err := w.reap()
	if err != nil {
		t.Fatalf("wait for worker: %v", err)
	}
	return outcome
}

func runWorker(t *testing.T, mode string, args ...string) workerOutcome {
	t.Helper()
	return startWorker(t, mode, args...).wait(t)
}

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
	"strconv"
	"strings"
	"testing"
)

const (
	exitWorkerFailed = 1
	// exitWorkerCrash is the status of a worker that reached its crash point.
	exitWorkerCrash = 97

	modeBind         = "bind"
	modeChurn        = "churn"
	modeCrashBind    = "crash-bind:"
	modeCrashRelease = "crash-release:"
	// modePrepare prints the identity an unbound pool at args[0] prepares.
	modePrepare = "prepare"
	// modeCrashPrepare exits at the named point while preparing args[0].
	modeCrashPrepare = "crash-prepare:"
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
	case mode == modeBind:
		return 0, workerBind(args)
	case mode == modeChurn:
		return 0, workerChurn(args)
	case mode == modePrepare:
		return 0, workerPrepare(poolPoint(""), args)
	case strings.HasPrefix(mode, modeCrashPrepare):
		return 0, workerPrepare(poolPoint(strings.TrimPrefix(mode, modeCrashPrepare)), args)
	case strings.HasPrefix(mode, modeCrashBind):
		return 0, workerCrash(poolPoint(strings.TrimPrefix(mode, modeCrashBind)), false, args)
	case strings.HasPrefix(mode, modeCrashRelease):
		return 0, workerCrash(poolPoint(strings.TrimPrefix(mode, modeCrashRelease)), true, args)
	default:
		return 0, fmt.Errorf("unknown worker mode %q", mode)
	}
}

// workerTarget is the parsed common prefix of every worker's arguments: a pool
// root, the identity its marker holds, an owner namespace and (entry, digest)
// pairs to bind.
type workerTarget struct {
	pool     *Pool
	ns       Namespace
	requests []CommandRequest
}

func parseWorkerTarget(args []string) (workerTarget, error) {
	if len(args) < 5 || len(args)%2 != 1 {
		return workerTarget{}, errors.New("usage: poolRoot poolID owner (entry digest)...")
	}
	pool, err := NewPoolWithIdentity(args[0], args[1])
	if err != nil {
		return workerTarget{}, err
	}
	ns, err := NewNamespace(args[2])
	if err != nil {
		return workerTarget{}, err
	}
	target := workerTarget{pool: pool, ns: ns}
	for i := 3; i < len(args); i += 2 {
		target.requests = append(target.requests, CommandRequest{Executable: args[i], SHA256: args[i+1]})
	}
	return target, nil
}

// bindAll binds every request for the owner and returns the commands. They are
// not closed: view removal belongs to the release path, as in production.
func (w workerTarget) bindAll(ctx context.Context) ([]*Command, error) {
	var commands []*Command
	for _, request := range w.requests {
		command, err := w.pool.Bind(ctx, w.ns, request)
		if err != nil {
			return commands, err
		}
		commands = append(commands, command)
	}
	return commands, nil
}

func workerBind(args []string) error {
	target, err := parseWorkerTarget(args)
	if err != nil {
		return err
	}
	_, err = target.bindAll(context.Background())
	return err
}

// workerCrash exits with exitWorkerCrash when the pool reaches point. With
// release set it first binds, then releases, so release points can fire.
func workerCrash(point poolPoint, release bool, args []string) error {
	target, err := parseWorkerTarget(args)
	if err != nil {
		return err
	}
	target.pool.faults.at = func(got poolPoint, _ string) {
		if got == point {
			os.Exit(exitWorkerCrash)
		}
	}
	ctx := context.Background()
	if _, err = target.bindAll(ctx); err != nil {
		return err
	}
	if release {
		if err = target.pool.ReleaseOwner(ctx, target.ns); err != nil {
			return err
		}
	}
	return fmt.Errorf("the crash point %s never fired", point)
}

// workerPrepare prepares the pool at args[0] and prints the identity. With a
// crash point it exits with exitWorkerCrash when the pool reaches it.
func workerPrepare(crash poolPoint, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: poolRoot")
	}
	pool, err := NewPool(args[0])
	if err != nil {
		return err
	}
	if crash != "" {
		pool.faults.at = func(got poolPoint, _ string) {
			if got == crash {
				os.Exit(exitWorkerCrash)
			}
		}
	}
	id, err := pool.Prepare(context.Background())
	if err != nil {
		return err
	}
	if crash != "" {
		return fmt.Errorf("the crash point %s never fired", crash)
	}
	_, err = fmt.Println(id)
	return err
}

// workerChurn binds and releases its own owners in a loop against a shared
// pool and checks every view it was handed: args are poolRoot poolID
// ownerPrefix iterations (entry digest)...
func workerChurn(args []string) error {
	if len(args) < 6 {
		return errors.New("usage: poolRoot poolID ownerPrefix iterations (entry digest)...")
	}
	iterations, err := strconv.Atoi(args[3])
	if err != nil {
		return err
	}
	for i := range iterations {
		target, parseErr := parseWorkerTarget(append([]string{args[0], args[1], args[2] + strconv.Itoa(i)}, args[4:]...))
		if parseErr != nil {
			return parseErr
		}
		if err = churnOnce(target); err != nil {
			return fmt.Errorf("iteration %d: %w", i, err)
		}
	}
	return nil
}

func churnOnce(target workerTarget) error {
	ctx := context.Background()
	commands, err := target.bindAll(ctx)
	if err != nil {
		return err
	}
	for i, command := range commands {
		if err = requireViewHoldsDigest(command, target.requests[i].SHA256); err != nil {
			return err
		}
	}
	return target.pool.ReleaseOwner(ctx, target.ns)
}

// requireViewHoldsDigest re-reads the view's entry through the command path and
// requires the admitted bytes.
func requireViewHoldsDigest(command *Command, digest string) error {
	content, err := os.ReadFile(command.Cmd.Path)
	if err != nil {
		return fmt.Errorf("read bound view: %w", err)
	}
	if got := digestOf(content); got != digest {
		return fmt.Errorf("bound view holds %s, want %s", got, digest)
	}
	return nil
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

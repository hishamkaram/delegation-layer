package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"

	"golang.org/x/sys/unix"
)

func startDaemon(ctx context.Context, operationRoot, executable, configPath, base string, options Options) error {
	_, err := startDaemonForRuntime(ctx, operationRoot, executable, configPath, base, options, nil)
	return err
}

// The optional scope is minted only by the matching restart owner after its
// durable successor acceptance. It bypasses only that exact runtime fence.
func startDaemonForRuntime(ctx context.Context, operationRoot, executable, configPath, base string, options Options, restart *privateRuntimeRestartStart) (privateDaemonStart, error) {
	if err := restart.authorizeStart(ctx, operationRoot); err != nil {
		return privateDaemonStart{}, err
	}
	selected, err := canonicalExecutable(executable)
	if err != nil || selected != executable {
		return privateDaemonStart{}, errors.Join(ErrBinding, err)
	}
	cmd := exec.Command(executable, "-c", configPath)
	cmd.Dir = base
	cmd.Env = environmentForCommand(options.Environment)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err = configurePrivateDaemonCommand(cmd); err != nil {
		return privateDaemonStart{}, fmt.Errorf("%w: prepare private pueued launch: %w", ErrBinding, err)
	}
	started, guardErr := guardedDaemonStart(ctx, operationRoot, configPath, cmd, options.DaemonStartGuard, options.ReportDiagnostic)
	if started.done == nil {
		return started, guardErr
	}
	if owner, ok := options.DaemonWaitOwner.(DaemonWaitBirthOwner); ok {
		return started, errors.Join(guardErr, owner.RegisterDaemonWaitBirth(operationRoot, cmd.Process.Pid, started.done, started.birth))
	}
	if options.DaemonWaitOwner != nil {
		return started, errors.Join(guardErr, options.DaemonWaitOwner.RegisterDaemonWait(operationRoot, cmd.Process.Pid, started.done))
	}
	return started, guardErr
}

type privateDaemonStart struct {
	attempted  bool
	done       <-chan struct{}
	birthReady <-chan struct{}
	birth      func() (lifetime.ProcessRef, bool, error)
}

// guardedDaemonStart owns callback lifetime and the sole Wait after successful Start.
func guardedDaemonStart(ctx context.Context, operationRoot, configPath string, cmd *exec.Cmd, guard DaemonStartGuard, report func(error)) (privateDaemonStart, error) {
	if ctx == nil {
		return privateDaemonStart{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return privateDaemonStart{}, err
	}
	if !filepath.IsAbs(operationRoot) || filepath.Clean(operationRoot) != operationRoot {
		return privateDaemonStart{}, ErrConfiguration
	}
	if _, private := privateStateRoot(configPath); private && !IsPrivateConfig(operationRoot, configPath) {
		return privateDaemonStart{}, ErrConfiguration
	}
	if filepath.Base(configPath) == privateRecoveryConfigName && configPath != privateRecoveryConfigPath(operationRoot) {
		return privateDaemonStart{}, ErrConfiguration
	}
	if guard == nil {
		return privateDaemonStart{}, ErrInfrastructureOwnerRequired
	}
	var mu sync.Mutex
	called, active := false, true
	var started privateDaemonStart
	var startErr, contractErr error
	start := func() error {
		mu.Lock()
		defer mu.Unlock()
		if !active || called {
			contractErr = ErrInfrastructureOwnerRequired
			return ErrInfrastructureOwnerRequired
		}
		called = true
		startErr = ctx.Err()
		if startErr == nil {
			if err := cmd.Start(); err != nil {
				startErr = fmt.Errorf("%w: start private pueued: %w", ErrBinding, err)
			} else {
				// A guard may report a post-Start fault; it cannot delay reaping.
				started = startDaemonWait(cmd, report, lifetime.ObserveStartedProcess)
			}
		}
		return startErr
	}
	guardErr := guard.GuardDaemonStart(ctx, operationRoot, start)
	mu.Lock()
	defer mu.Unlock()
	active = false
	if !called {
		return privateDaemonStart{}, errors.Join(guardErr, ErrInfrastructureOwnerRequired)
	}
	started.attempted = called
	return started, errors.Join(guardErr, startErr, contractErr)
}

// Called immediately after successful Start, this owner launches the sole Wait.
// Birth is captured before that first Wait, including a fast unreaped exit.
// Both futures are independent of consumers and diagnostic log delivery.
func startDaemonWait(cmd *exec.Cmd, report func(error), observeBirth func(int) (lifetime.ProcessRef, error)) privateDaemonStart {
	done := make(chan struct{})
	birthReady := make(chan struct{})
	var birth lifetime.ProcessRef
	var birthErr error
	go func() {
		if observeBirth == nil {
			birthErr = errors.New("private daemon birth observer is unavailable")
		} else {
			birth, birthErr = observeBirth(cmd.Process.Pid)
		}
		close(birthReady)
		waitErr := cmd.Wait()
		close(done)
		if birthErr != nil {
			reportDiagnostic(report, fmt.Errorf("private daemon birth observation failed: %w", birthErr))
		}
		if waitErr != nil {
			reportDiagnostic(report, fmt.Errorf("private daemon exited: %w", waitErr))
		}
	}()
	return privateDaemonStart{done: done, birthReady: birthReady, birth: func() (lifetime.ProcessRef, bool, error) {
		select {
		case <-birthReady:
			return birth, true, birthErr
		default:
			return lifetime.ProcessRef{}, false, nil
		}
	}}
}

func waitReady(ctx context.Context, client *Client, timeout time.Duration) error {
	return waitPrivateReady(ctx, client, timeout, true)
}

func waitPrivateReady(ctx context.Context, client *Client, timeout time.Duration, cleanupSuccessful bool) error {
	return waitPrivateReadyWithProbe(ctx, client, timeout, cleanupSuccessful, client.Ready)
}

// Both purposes share command completion, natural pending ownership and polls.
// The caller selects only its error projection at the verified readiness edge.
func waitPrivateReadyWithProbe(ctx context.Context, client *Client, timeout time.Duration, cleanupSuccessful bool, probe func(context.Context) error) error {
	if ctx == nil {
		return context.Canceled
	}
	if timeout <= 0 {
		return ErrConfiguration
	}
	var lastErr error
	cleaned := false
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		attemptErr, done := waitReadyAttempt(ctx, client, timeout, probe)
		if attemptErr == nil {
			return nil
		}
		lastErr = attemptErr
		if retry, cleanErr := client.cleanupStatusOverflow(ctx, lastErr, cleanupSuccessful, cleaned); cleanErr != nil {
			return errors.Join(lastErr, cleanErr)
		} else if retry {
			cleaned = true
			continue
		}
		if done {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return errors.Join(lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitReadyAttempt(ctx context.Context, client *Client, timeout time.Duration, probe func(context.Context) error) (error, bool) {
	readyContext, cancel := context.WithTimeout(ctx, timeout)
	err := probe(readyContext)
	cancel()
	if err == nil {
		return nil, true
	}
	if pending := pendingFrom(err); pending != nil {
		return processReadyPending(ctx, client, err, pending)
	}
	return err, false
}

func (l *bootstrapLock) duplicateForControlOwner() (*os.File, error) {
	if l == nil {
		return nil, os.ErrInvalid
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.retired || l.file == nil {
		return nil, os.ErrClosed
	}
	duplicate, err := duplicateCloseOnExec(l.file, "delegate-supervisor-generation-lease")
	if err != nil {
		return nil, err
	}
	// flock is attached to the shared open-file description. Once inherited,
	// the caller must close its descriptor without LOCK_UN; the one-shot owner
	// releases the generation lease after its exact child Wait completes.
	l.transferred = true
	return duplicate, nil
}

func acquireBootstrapLock(ctx context.Context, path string) (*bootstrapLock, error) {
	return acquireSupervisorLock(ctx, path, unix.LOCK_EX)
}

func acquireSupervisorLock(ctx context.Context, path string, mode int) (*bootstrapLock, error) {
	return acquireSupervisorLockMode(ctx, path, mode, true)
}

func acquireExistingSupervisorLock(ctx context.Context, path string, mode int) (*bootstrapLock, error) {
	return acquireSupervisorLockMode(ctx, path, mode, false)
}

func acquireSupervisorLockMode(ctx context.Context, path string, mode int, create bool) (*bootstrapLock, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w: open supervisor bootstrap lock: %w", ErrBinding, err)
	}
	for {
		err = unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return &bootstrapLock{file: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, errors.Join(fmt.Errorf("%w: acquire supervisor bootstrap lock: %w", ErrBinding, err), file.Close())
		}
		if mode&unix.LOCK_NB != 0 {
			if closeErr := file.Close(); closeErr != nil {
				return nil, errors.Join(err, closeErr)
			}
			return nil, errSupervisorLockBusy
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), file.Close())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func closeBootstrapLock(lock *bootstrapLock) error {
	if lock == nil || lock.file == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.closed {
		return nil
	}
	lock.retired = true
	var unlockErr error
	if !lock.transferred {
		unlockErr = unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	}
	closeErr := lock.file.Close()
	// os.File.Close retires the Go handle even when native release reports an
	// error. Preserve the fault; a second attempt cannot retry native custody.
	lock.closed = true
	return errors.Join(unlockErr, closeErr)
}

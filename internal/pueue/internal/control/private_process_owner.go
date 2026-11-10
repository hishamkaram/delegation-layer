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

	"golang.org/x/sys/unix"
)

func startDaemon(ctx context.Context, operationRoot, executable, configPath, base string, options Options) error {
	if err := refusePendingPrivateRestart(operationRoot); err != nil {
		return err
	}
	selected, err := canonicalExecutable(executable)
	if err != nil || selected != executable {
		return errors.Join(ErrBinding, err)
	}
	cmd := exec.Command(executable, "-c", configPath)
	cmd.Dir = base
	cmd.Env = environmentForCommand(options.Environment)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err = configurePrivateDaemonCommand(cmd); err != nil {
		return fmt.Errorf("%w: prepare private pueued launch: %w", ErrBinding, err)
	}
	done, guardErr := guardedDaemonStart(ctx, operationRoot, configPath, cmd, options.DaemonStartGuard, options.ReportDiagnostic)
	if done == nil {
		return guardErr
	}
	if options.DaemonWaitOwner != nil {
		return errors.Join(guardErr, options.DaemonWaitOwner.RegisterDaemonWait(operationRoot, cmd.Process.Pid, done))
	}
	return guardErr
}

// guardedDaemonStart owns callback lifetime and the sole Wait after successful Start.
func guardedDaemonStart(ctx context.Context, operationRoot, configPath string, cmd *exec.Cmd, guard DaemonStartGuard, report func(error)) (<-chan struct{}, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(operationRoot) || filepath.Clean(operationRoot) != operationRoot {
		return nil, ErrConfiguration
	}
	if _, private := privateStateRoot(configPath); private && !IsPrivateConfig(operationRoot, configPath) {
		return nil, ErrConfiguration
	}
	if filepath.Base(configPath) == privateRecoveryConfigName && configPath != privateRecoveryConfigPath(operationRoot) {
		return nil, ErrConfiguration
	}
	if guard == nil {
		return nil, ErrInfrastructureOwnerRequired
	}
	var mu sync.Mutex
	called, active := false, true
	var done <-chan struct{}
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
				done = startDaemonWait(cmd, report)
			}
		}
		return startErr
	}
	guardErr := guard.GuardDaemonStart(ctx, operationRoot, start)
	mu.Lock()
	defer mu.Unlock()
	active = false
	if !called {
		return nil, errors.Join(guardErr, ErrInfrastructureOwnerRequired)
	}
	return done, errors.Join(guardErr, startErr, contractErr)
}

// Called immediately after successful Start, this owner launches the sole Wait.
// Completion denotes Wait's return; diagnostic log I/O cannot delay it.
func startDaemonWait(cmd *exec.Cmd, report func(error)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		waitErr := cmd.Wait()
		close(done)
		if waitErr != nil {
			reportDiagnostic(report, fmt.Errorf("private daemon exited: %w", waitErr))
		}
	}()
	return done
}

func waitReady(ctx context.Context, client *Client, timeout time.Duration) error {
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
		attemptErr, done := waitReadyAttempt(ctx, client, timeout)
		if attemptErr == nil {
			return nil
		}
		lastErr = attemptErr
		if retry, cleanErr := client.cleanupStatusOverflow(ctx, lastErr, true, cleaned); cleanErr != nil {
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

func waitReadyAttempt(ctx context.Context, client *Client, timeout time.Duration) (error, bool) {
	readyContext, cancel := context.WithTimeout(ctx, timeout)
	err := client.Ready(readyContext)
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

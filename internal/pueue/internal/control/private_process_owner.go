package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

func startDaemon(executable, expectedSHA256, configPath, base string, options Options) error {
	digest, err := hashExecutable(executable)
	if err != nil {
		return fmt.Errorf("%w: verify private pueued identity before start: %w", ErrBinding, err)
	}
	if digest != expectedSHA256 {
		return fmt.Errorf("%w: private pueued identity changed before start", ErrBinding)
	}
	cmd := exec.Command(executable, "-c", configPath)
	cmd.Dir = base
	cmd.Env = environmentForCommand(options.Environment)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err = configurePrivateDaemonCommand(cmd); err != nil {
		return fmt.Errorf("%w: prepare private pueued launch: %w", ErrBinding, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: start private pueued: %w", ErrBinding, err)
	}
	// The spawning process owns exactly one waiter. Release does not reap a
	// Unix child; an unreaped daemon can keep migration waiting on a zombie.
	// Waiting asynchronously preserves the daemon lifetime beyond this caller.
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("private daemon exited: %v", err)
		}
	}()
	return nil
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
	if l.closed || l.file == nil {
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

func retainBootstrapLock(lock **bootstrapLock, pending []*Pending, report func(error)) {
	if lock == nil || *lock == nil || len(pending) == 0 {
		return
	}
	retained := *lock
	*lock = nil
	go func() {
		for _, handle := range pending {
			if handle != nil {
				<-handle.Done()
			}
		}
		if err := closeBootstrapLock(retained); err != nil && report != nil {
			report(err)
		}
	}()
}

func finishPrivateBootstrap(lock **bootstrapLock, resultErr *error) {
	retainBootstrapLock(lock, pendingHandles(*resultErr), func(err error) {
		log.Printf("delegate: retained supervisor lock cleanup failed: %v", err)
	})
	*resultErr = errors.Join(*resultErr, closeBootstrapLock(*lock))
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
	var unlockErr error
	if !lock.transferred {
		unlockErr = unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	}
	closeErr := lock.file.Close()
	if closeErr == nil {
		lock.closed = true
	}
	return errors.Join(unlockErr, closeErr)
}

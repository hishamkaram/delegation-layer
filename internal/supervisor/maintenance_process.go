package supervisor

import (
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/lifetime"
	"github.com/hishamkaram/delegation-layer/internal/pueue"
)

func LaunchQueueMaintenanceOwner(ctx context.Context, executable, root string) error {
	_, err := StartQueueMaintenanceOwner(ctx, executable, root)
	return err
}

func StartQueueMaintenanceOwner(ctx context.Context, executable, root string) (<-chan error, error) {
	if ctx == nil {
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(executable) || filepath.Clean(executable) != executable ||
		!filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, pueue.ErrConfiguration
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil || resolved != executable {
		return nil, errors.Join(pueue.ErrConfiguration, err)
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, errors.Join(pueue.ErrConfiguration, err)
	}
	command := exec.Command(executable, "--queue-maintenance", "--root", root)
	command.Dir = filepath.Dir(executable)
	command.Env = pueue.DefaultEnvironment()
	if err = lifetime.ConfigureIndependentSession(command); err != nil {
		return nil, err
	}
	if err = command.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	// Start transfers durable ownership to the independent process. Keep one
	// asynchronous Wait while this caller lives so an early helper exit cannot
	// become a zombie. If the caller exits first, the OS adopts the independent
	// session and reaps it after natural completion.
	go func() {
		waitErr := command.Wait()
		if waitErr != nil {
			log.Printf("delegate: maintenance process deferred: %v", waitErr)
		}
		done <- waitErr
		close(done)
	}()
	return done, nil
}

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

func LaunchQueueMaintenanceOwnerWithEnvironment(ctx context.Context, executable, root string, environment []string) error {
	_, err := StartQueueMaintenanceOwnerWithEnvironment(ctx, executable, root, environment)
	return err
}

// StartQueueMaintenanceOwnerWithEnvironment retains the selected bounded
// environment through Start and the exact child's natural Wait.
func StartQueueMaintenanceOwnerWithEnvironment(ctx context.Context, executable, root string, environment []string) (<-chan error, error) {
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
	command.Env = pueue.DefaultEnvironmentFrom(environment)
	if err = lifetime.ConfigureIndependentSession(command); err != nil {
		return nil, err
	}
	if err = command.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	// The caller has already persisted the maintenance wake. This helper is a
	// best-effort attempt: a new session alone cannot outlive every finite task
	// scope, and interruption leaves the wake available for later root activity.
	// Keep one Wait while this caller lives so an early helper exit is reaped.
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

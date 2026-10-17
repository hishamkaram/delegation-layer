package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

func InstallStateSupervisorPairForRecovery(ctx context.Context, root, initialSupervisorExecutable string, binding task.SupervisorRef) error {
	client, daemon, err := resolveBundledExecutables(initialSupervisorExecutable)
	if err == nil {
		return PrepareStateSupervisorPair(ctx, root, client, daemon)
	}
	found := false
	stateErr := pueue.WithSupervisorLeaseContext(ctx, root, func(lockedContext context.Context) error {
		var probeErr error
		found, probeErr = probeStateSupervisorPair(lockedContext, root, nil)
		return probeErr
	})
	if errors.Is(stateErr, pueue.ErrPrivateSupervisorNotInitialized) {
		stateErr = pueue.WithBootstrapLockContext(ctx, root, func(lockedContext context.Context) error {
			var probeErr error
			found, probeErr = probeStateSupervisorPair(lockedContext, root, nil)
			if errors.Is(probeErr, os.ErrNotExist) {
				return nil
			}
			return probeErr
		})
	}
	if stateErr != nil {
		return errors.Join(err, stateErr)
	}
	if found {
		return nil
	}
	if !PrivateSupervisorExecutablesAvailable(binding.ClientExecutable, binding.DaemonExecutable) {
		return err
	}
	clientDigest, clientErr := stateExecutableSourceDigest(binding.ClientExecutable)
	daemonDigest, daemonErr := stateExecutableSourceDigest(binding.DaemonExecutable)
	if clientErr != nil || daemonErr != nil || clientDigest != binding.ClientSHA256 || daemonDigest != binding.DaemonSHA256 {
		return errors.Join(err, clientErr, daemonErr, pueue.ErrBinding)
	}
	return PrepareStateSupervisorPair(ctx, root, binding.ClientExecutable, binding.DaemonExecutable)
}

func ResolveRecoveryPrivateSupervisorExecutables(root string, saved task.SupervisorRef, initialSupervisorExecutable string) (client, daemon string, resultErr error) {
	if PrivateSupervisorExecutablesAvailable(saved.ClientExecutable, saved.DaemonExecutable) {
		if err := ValidateSavedStateSupervisorPair(root, saved); err != nil {
			return "", "", fmt.Errorf("%w: saved private supervisor pair failed integrity validation: %w", pueue.ErrBinding, err)
		}
		return saved.ClientExecutable, saved.DaemonExecutable, nil
	}
	client, daemon, resolveErr := ResolvePrivateSupervisorExecutables(root, initialSupervisorExecutable)
	if resolveErr == nil {
		return client, daemon, nil
	}
	return "", "", fmt.Errorf("%w: resolve private supervisor executables: %w", pueue.ErrBinding, resolveErr)
}

func PrivateSupervisorExecutablesAvailable(clientExecutable, daemonExecutable string) bool {
	if clientExecutable == "" || daemonExecutable == "" {
		return false
	}
	if _, err := exec.LookPath(clientExecutable); err != nil {
		return false
	}
	if _, err := exec.LookPath(daemonExecutable); err != nil {
		return false
	}
	return true
}

func ResolveRetainedSupervisorPair(root string, published task.SupervisorRef) (string, string, error) {
	base := filepath.Join(root, ".supervisor")
	pair := filepath.Join(base, stateSupervisorPairDirectoryPrefix+stateSupervisorPairDigest(published.ClientSHA256, published.DaemonSHA256))
	candidate := published
	candidate.ClientExecutable = filepath.Join(pair, "pueue")
	candidate.DaemonExecutable = filepath.Join(pair, "pueued")
	if err := validateSavedStateSupervisorPairContents(base, pair, candidate); err != nil {
		return "", "", err
	}
	return candidate.ClientExecutable, candidate.DaemonExecutable, nil
}

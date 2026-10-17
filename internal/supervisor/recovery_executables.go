package supervisor

import (
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

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

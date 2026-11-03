package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hishamkaram/delegation-layer/internal/pueue"
	"github.com/hishamkaram/delegation-layer/internal/task"
)

// infrastructurePairSource is an immutable operation snapshot, not another
// generation head. Pueue's published private binding alone selects the source.
// Existing content-addressed copies outlive the short source generation lease.
type infrastructurePairSource struct {
	root   string
	saved  task.SupervisorRef
	record stateSupervisorPairRecord
}

// Construct before taking source or target exclusive generation/bootstrap
// locks. No lock-order enforcement follows from transferring an instance lease.
func readInfrastructurePairSource(ctx context.Context, root string) (infrastructurePairSource, error) {
	var source infrastructurePairSource
	err := pueue.WithSupervisorLeaseContext(ctx, root, func(lockedContext context.Context) error {
		saved, found, err := pueue.ReadPrivateSupervisorBinding(root)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: published private supervisor binding missing: %w", pueue.ErrBinding, os.ErrNotExist)
		}
		snapshot := infrastructurePairSource{root: root, saved: saved, record: stateSupervisorPairRecord{
			SchemaVersion: 1, PairSHA256: stateSupervisorPairDigest(saved.ClientSHA256, saved.DaemonSHA256),
			ClientSHA256: saved.ClientSHA256, DaemonSHA256: saved.DaemonSHA256,
		}}
		if err := snapshot.validate(lockedContext); err != nil {
			return err
		}
		source = snapshot
		return nil
	})
	if err != nil {
		return infrastructurePairSource{}, err
	}
	return source, nil
}

func (s infrastructurePairSource) validate(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.root == "" || !filepath.IsAbs(s.root) || filepath.Clean(s.root) != s.root {
		return fmt.Errorf("%w: infrastructure pair source has no canonical root", pueue.ErrBinding)
	}
	if err := requireInfrastructureStatePair(s.root, s.saved); err != nil {
		return err
	}
	if err := validateStateSupervisorPairRecord(s.record); err != nil {
		return err
	}
	if s.record.ClientSHA256 != s.saved.ClientSHA256 || s.record.DaemonSHA256 != s.saved.DaemonSHA256 {
		return pueue.ErrBinding
	}
	return ValidateSavedStateSupervisorPair(s.root, s.saved)
}

// stage validates frozen source bytes without reacquiring the source generation
// lease, then adds immutable target members. The target's canonical transaction
// owns selection; this operation never changes its inventory or binding.
// Trusted composition must retain a validated, pinned target through staging;
// the target path alone is not root identity or mutation authority.
func (s infrastructurePairSource) stage(ctx context.Context, targetRoot string) (string, string, error) {
	if err := s.validate(ctx); err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(targetRoot) || filepath.Clean(targetRoot) != targetRoot {
		return "", "", pueue.ErrConfiguration
	}
	base := filepath.Join(targetRoot, ".supervisor")
	if err := ensureSupervisorDirectory(targetRoot, base); err != nil {
		return "", "", err
	}
	if err := stageStateSupervisorPairContentAddressed(ctx, base, s.saved.ClientExecutable, s.saved.DaemonExecutable, s.record); err != nil {
		return "", "", err
	}
	directory := filepath.Join(base, stateSupervisorPairDirectoryPrefix+s.record.PairSHA256)
	return filepath.Join(directory, "pueue"), filepath.Join(directory, "pueued"), nil
}

func requireInfrastructureStatePair(root string, saved task.SupervisorRef) error {
	base := filepath.Join(root, ".supervisor")
	clientDirectory, clientManaged := stateSupervisorPairDirectoryForExecutable(base, saved.ClientExecutable)
	daemonDirectory, daemonManaged := stateSupervisorPairDirectoryForExecutable(base, saved.DaemonExecutable)
	// The legacy validator intentionally accepts two external paths. Infrastructure custody
	// requires a positive managed-pair check before invoking that validator.
	if !clientManaged || !daemonManaged || clientDirectory != daemonDirectory {
		return fmt.Errorf("%w: infrastructure custody requires a state-managed pair", pueue.ErrBinding)
	}
	return nil
}
